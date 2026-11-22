package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	// VoxNodeFinalizer ensures CA clean and PuppetDB node deactivation before a VoxNode is deleted.
	VoxNodeFinalizer = "voxkube.io/node-cleanup"
	// AnnotationSkipCleanup ("true") lets a VoxNode be deleted without cleaning the CA and OpenVox DB, e.g. when
	// the CA is permanently lost. Without it the finalizer stays until the cleanup succeeds.
	AnnotationSkipCleanup = "voxkube.io/skip-cleanup"

	nodeRequeueWaiting = 30 * time.Second
	nodeRequeueActive  = 5 * time.Minute
	nodeRequeuePending = time.Minute
)

// VoxNodeReconciler reconciles a VoxNode object.
type VoxNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	CAClient OpenVoxCAClient
	DBClient OpenVoxDBClient
}

// +kubebuilder:rbac:groups=voxkube.io,resources=voxnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=voxkube.io,resources=voxnodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=voxkube.io,resources=voxnodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// Reconcile manages CSR signing, ENC, cert-manager certificates, Hiera data, PuppetDB status
// and CA + PuppetDB cleanup on decommission or deletion.
func (r *VoxNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := &openvoxv1alpha1.VoxNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	cluster := &openvoxv1alpha1.VoxCluster{}
	clusterFound := true
	if err := r.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: node.Spec.ClusterRef}, cluster); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		clusterFound = false
	}

	// Two VoxNodes for the same (cluster, certname): the oldest owns the certname, the others are inert.
	owner, err := r.certnameOwner(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	duplicateOf := ""
	if owner != nil && owner.Name != node.Name {
		duplicateOf = owner.Name
	}

	// Paused (own annotation or the VoxCluster's): no CA/OpenVox DB calls and no writes beyond the condition.
	paused := pauseReason(node, cluster, clusterFound)

	// Deletion: clean the CA and OpenVox DB, then release the finalizer. The finalizer stays until both
	// succeed, unless the VoxCluster is gone (or going), the node is a duplicate or skip-cleanup is set.
	// While paused the cleanup waits (it would call the CA and OpenVox DB); skip-cleanup still releases at once.
	if !node.DeletionTimestamp.IsZero() {
		cleanupNeeded := node.Annotations[AnnotationSkipCleanup] != "true" && duplicateOf == "" &&
			clusterFound && cluster.DeletionTimestamp.IsZero()
		if paused != "" && cleanupNeeded && controllerutil.ContainsFinalizer(node, VoxNodeFinalizer) {
			return ctrl.Result{}, r.markNodePaused(ctx, node, paused,
				pausedMessage(paused, "VoxNode")+"; deletion waits for that (or set "+AnnotationSkipCleanup+"=\"true\" to delete without cleanup)")
		}
		return r.finalizeVoxNode(ctx, node, cluster, clusterFound, duplicateOf)
	}
	if paused != "" {
		return ctrl.Result{}, r.markNodePaused(ctx, node, paused, pausedMessage(paused, "VoxNode"))
	}

	if !controllerutil.ContainsFinalizer(node, VoxNodeFinalizer) {
		controllerutil.AddFinalizer(node, VoxNodeFinalizer)
		if err := r.Update(ctx, node); err != nil {
			return ctrl.Result{}, err
		}
	}

	status := node.Status.DeepCopy()
	meta.RemoveStatusCondition(&status.Conditions, ConditionPaused)
	setReady := func(ok bool, reason, msg string) {
		s := metav1.ConditionFalse
		if ok {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: "Ready", Status: s, ObservedGeneration: node.Generation, Reason: reason, Message: msg,
		})
	}

	if !clusterFound {
		status.Phase = openvoxv1alpha1.NodePhaseError
		setReady(false, "ClusterNotFound", fmt.Sprintf("VoxCluster %q not found", node.Spec.ClusterRef))
		return ctrl.Result{}, r.updateVoxNodeStatus(ctx, req.NamespacedName, *status)
	}
	if duplicateOf != "" {
		// Never sign, revoke, clean or classify for a duplicate: that would act on the machine the owner manages.
		status.Phase = openvoxv1alpha1.NodePhaseError
		setReady(false, "DuplicateCertname", fmt.Sprintf("certname %q is already managed by VoxNode %q in VoxCluster %q; "+
			"this VoxNode is ignored (delete it, or delete %q to hand the certname over)", node.Spec.Certname, duplicateOf,
			node.Spec.ClusterRef, duplicateOf))
		// Re-check periodically: the owner may be deleted, and then this VoxNode takes over.
		return ctrl.Result{RequeueAfter: nodeRequeuePending}, r.updateVoxNodeStatus(ctx, req.NamespacedName, *status)
	}
	if reservedCertname(cluster, node.Spec.Certname) {
		status.Phase = openvoxv1alpha1.NodePhaseError
		setReady(false, "ReservedCertname", fmt.Sprintf("certname %q is reserved for the cluster's own services and is never signed",
			node.Spec.Certname))
		return ctrl.Result{}, r.updateVoxNodeStatus(ctx, req.NamespacedName, *status)
	}

	var result ctrl.Result
	switch node.Spec.State {
	case openvoxv1alpha1.NodeStateDecommissioned:
		status.Phase = openvoxv1alpha1.NodePhaseDecommissioned
		status.CertificateSigned = false
		status.ReadyCertificates = 0
		// A decommissioned machine has no current check-in, report or certificate.
		status.LastSeenTime = nil
		status.LastReportStatus = ""
		status.CertificateFingerprint = ""
		status.CertificateExtensions = nil
		if !status.CACleaned || !status.PuppetDBDeactivated {
			status.CACleaned, status.PuppetDBDeactivated = r.cleanupNode(ctx, node, cluster, status.CACleaned, status.PuppetDBDeactivated)
		}
		if err := r.deleteNodeCertificates(ctx, node, nil); err != nil {
			return ctrl.Result{}, err
		}
		if status.CACleaned && status.PuppetDBDeactivated {
			setReady(true, "NodeDecommissioned", "Certificate cleaned from the CA and node deactivated in OpenVox DB")
		} else {
			setReady(false, "CleanupPending", cleanupPendingMessage(status.CACleaned, status.PuppetDBDeactivated))
			result = ctrl.Result{RequeueAfter: nodeRequeueWaiting}
		}

	case openvoxv1alpha1.NodeStateRevoked:
		caURL := cluster.Status.CAURL
		if caClient := resolveCAClient(ctx, r.Client, r.CAClient, cluster); caClient != nil && caURL != "" {
			if err := caClient.RevokeCert(ctx, caURL, node.Spec.Certname); err != nil {
				return ctrl.Result{}, err
			}
		}
		status.Phase = openvoxv1alpha1.NodePhaseRevoked
		status.CertificateSigned = false
		status.CACleaned, status.PuppetDBDeactivated = false, false
		setReady(true, "CertificateRevoked", "Certificate revoked on the CA; compilers reject it once the CRL is synced")

	case openvoxv1alpha1.NodeStatePendingApproval:
		status.Phase = openvoxv1alpha1.NodePhasePendingCSR
		status.CACleaned, status.PuppetDBDeactivated = false, false
		msg := "Awaiting approval: set spec.state to Active to sign the CSR"
		if cs, err := r.caStatus(ctx, cluster, node); err == nil && cs.State == "requested" {
			msg = "CSR received; set spec.state to Active to sign it"
			r.refreshCertificateExtensions(ctx, cluster, node, status, false)
			if role := status.CertificateExtensions[ppRoleExtensionName]; role != "" {
				msg = fmt.Sprintf("CSR received (pp_role %q); set spec.state to Active to sign it", role)
			}
		}
		setReady(false, "PendingApproval", msg)
		// Keep the CSR's extensions (pp_role) current while waiting for approval.
		result = ctrl.Result{RequeueAfter: nodeRequeuePending}

	default: // Active
		var err error
		result, err = r.reconcileActive(ctx, node, cluster, status, setReady)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	status.Role = effectiveRoleFor(node, status)
	if err := r.updateVoxNodeStatus(ctx, req.NamespacedName, *status); err != nil {
		return ctrl.Result{}, err
	}
	// The VoxCluster controller, the only writer of the ENC and Hiera data, re-renders them when the spec,
	// the role or the certificate extensions in this status change (it watches VoxNodes for exactly that).
	return result, nil
}

// finalizeVoxNode runs the deletion cleanup and releases the finalizer once it has succeeded.
func (r *VoxNodeReconciler) finalizeVoxNode(ctx context.Context, node *openvoxv1alpha1.VoxNode,
	cluster *openvoxv1alpha1.VoxCluster, clusterFound bool, duplicateOf string) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(node, VoxNodeFinalizer) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)
	// The node leaves the ENC and Hiera as soon as deletion starts: the VoxCluster controller reacts to the
	// deletion timestamp and nodeServedBy skips deleting VoxNodes.
	clusterLive := clusterFound && cluster.DeletionTimestamp.IsZero()
	switch {
	case node.Annotations[AnnotationSkipCleanup] == "true":
		logger.Info("skip-cleanup annotation set; releasing the VoxNode without CA/OpenVox DB cleanup", "certname", node.Spec.Certname)
	case duplicateOf != "":
		// The certificate belongs to the machine the owning VoxNode manages: cleaning it would revoke that machine.
		logger.Info("duplicate VoxNode; releasing it without CA/OpenVox DB cleanup", "certname", node.Spec.Certname, "owner", duplicateOf)
	case !clusterLive:
		// Nothing left to clean against (or it is being torn down): don't block deletion forever.
		logger.Info("VoxCluster gone; releasing the VoxNode without CA/OpenVox DB cleanup", "certname", node.Spec.Certname)
	default:
		status := node.Status.DeepCopy()
		status.CACleaned, status.PuppetDBDeactivated = r.cleanupNode(ctx, node, cluster, status.CACleaned, status.PuppetDBDeactivated)
		if !status.CACleaned || !status.PuppetDBDeactivated {
			msg := cleanupPendingMessage(status.CACleaned, status.PuppetDBDeactivated) +
				". Set the annotation " + AnnotationSkipCleanup + "=\"true\" to delete without cleanup"
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: node.Generation,
				Reason: "CleanupPending", Message: msg,
			})
			if err := r.updateVoxNodeStatus(ctx, client.ObjectKeyFromObject(node), *status); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			r.event(node, corev1.EventTypeWarning, "CleanupPending", msg)
			return ctrl.Result{RequeueAfter: nodeRequeueWaiting}, nil
		}
	}
	controllerutil.RemoveFinalizer(node, VoxNodeFinalizer)
	if err := r.Update(ctx, node); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func cleanupPendingMessage(caCleaned, dbDeactivated bool) string {
	var pending []string
	if !caCleaned {
		pending = append(pending, "cleaning the certificate on the CA")
	}
	if !dbDeactivated {
		pending = append(pending, "deactivating the node in OpenVox DB")
	}
	return "Retrying " + strings.Join(pending, " and ") + " (see the operator log)"
}

// nodeServedBy reports whether cluster classifies n and renders its Hiera data: n belongs to cluster, has a
// certname, isn't being deleted and is neither Decommissioned nor Revoked.
func nodeServedBy(n *openvoxv1alpha1.VoxNode, cluster *openvoxv1alpha1.VoxCluster) bool {
	return n.Spec.ClusterRef == cluster.Name && n.Spec.Certname != "" && n.DeletionTimestamp.IsZero() &&
		n.Spec.State != openvoxv1alpha1.NodeStateDecommissioned && n.Spec.State != openvoxv1alpha1.NodeStateRevoked
}

// effectiveRoleFor is EffectiveRole evaluated against the status being written.
func effectiveRoleFor(node *openvoxv1alpha1.VoxNode, status *openvoxv1alpha1.VoxNodeStatus) string {
	n := node.DeepCopy()
	n.Status = *status
	return EffectiveRole(n)
}

// refreshCertificateExtensions reads the Puppet extensions of certname's pending CSR (signed=false) or signed
// certificate into status.CertificateExtensions. Failures keep the previous value.
func (r *VoxNodeReconciler) refreshCertificateExtensions(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster,
	node *openvoxv1alpha1.VoxNode, status *openvoxv1alpha1.VoxNodeStatus, signed bool) {
	caClient := resolveCAClient(ctx, r.Client, r.CAClient, cluster)
	if caClient == nil || cluster.Status.CAURL == "" {
		return
	}
	var pemBytes []byte
	var err error
	if signed {
		pemBytes, err = caClient.GetCertificatePEM(ctx, cluster.Status.CAURL, node.Spec.Certname)
	} else {
		pemBytes, err = caClient.GetCertificateRequestPEM(ctx, cluster.Status.CAURL, node.Spec.Certname)
	}
	if err == nil {
		var exts map[string]string
		if exts, err = PuppetExtensionsFromPEM(pemBytes); err == nil {
			status.CertificateExtensions = exts
			return
		}
	}
	log.FromContext(ctx).Info("could not read certificate extensions from the CA; keeping the previous ones",
		"certname", node.Spec.Certname, "signed", signed, "reason", err.Error())
}

func (r *VoxNodeReconciler) caStatus(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, node *openvoxv1alpha1.VoxNode) (*CertStatus, error) {
	caClient := resolveCAClient(ctx, r.Client, r.CAClient, cluster)
	if caClient == nil || cluster.Status.CAURL == "" {
		return nil, fmt.Errorf("CA not available")
	}
	return caClient.GetCertStatus(ctx, cluster.Status.CAURL, node.Spec.Certname)
}

// reconcileActive signs the CSR when allowed, provisions certificates, refreshes ENC/Hiera and reads PuppetDB.
func (r *VoxNodeReconciler) reconcileActive(
	ctx context.Context,
	node *openvoxv1alpha1.VoxNode,
	cluster *openvoxv1alpha1.VoxCluster,
	status *openvoxv1alpha1.VoxNodeStatus,
	setReady func(bool, string, string),
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	requeue := nodeRequeueActive
	waitingReason, waitingMsg := "", ""

	caClient := resolveCAClient(ctx, r.Client, r.CAClient, cluster)
	caURL := cluster.Status.CAURL
	switch {
	case !caEnabled(cluster):
		// External CA: nothing to sign; treat the node as signed.
		status.CertificateSigned = true
	case caClient == nil:
		status.CertificateSigned = true
	case caURL == "":
		waitingReason, waitingMsg = "AwaitingCA", "Waiting for the VoxCluster CA to come up"
		requeue = nodeRequeueWaiting
	default:
		cs, err := caClient.GetCertStatus(ctx, caURL, node.Spec.Certname)
		switch {
		case errors.Is(err, ErrCertNotFound):
			status.CertificateSigned = false
			status.CertificateFingerprint = ""
			status.CertificateNotAfter = nil
			status.CertificateExtensions = nil
			waitingReason, waitingMsg = "AwaitingCSR", "No CSR from "+node.Spec.Certname+" yet; run 'puppet ssl bootstrap' (or 'puppet agent -t') on the node"
			requeue = nodeRequeueWaiting
		case err != nil:
			logger.Info("CA not reachable; will retry", "certname", node.Spec.Certname, "reason", err.Error())
			waitingReason, waitingMsg = "CAUnreachable", err.Error()
			requeue = nodeRequeueWaiting
		case cs.State == "revoked":
			status.CertificateSigned = false
			status.CertificateNotAfter = nil
			status.Phase = openvoxv1alpha1.NodePhaseError
			setReady(false, "CertificateRevokedOnCA",
				"The certificate is revoked on the CA. Set state Decommissioned (or delete and re-create the VoxNode) and let the agent submit a new CSR")
			return ctrl.Result{}, nil
		case cs.State == "requested" && autosignMode(cluster) == autosignOff:
			r.refreshCertificateExtensions(ctx, cluster, node, status, false)
			status.CertificateSigned = false
			waitingReason, waitingMsg = "AwaitingManualSigning", "autosignMode is off; sign the CSR with 'puppetserver ca sign' on the CA pod"
			requeue = nodeRequeueWaiting
		case cs.State == "requested":
			signed, err := caClient.SignCSR(ctx, caURL, node.Spec.Certname)
			if err != nil {
				waitingReason, waitingMsg = "SigningFailed", err.Error()
				requeue = nodeRequeueWaiting
			} else {
				status.CertificateSigned = signed.State == "signed"
				status.CertificateFingerprint = signed.Fingerprint
				status.CertificateNotAfter = caNotAfter(signed.NotAfter, status.CertificateNotAfter)
				r.event(node, corev1.EventTypeNormal, "CertificateSigned", "Signed CSR for "+node.Spec.Certname)
				r.refreshCertificateExtensions(ctx, cluster, node, status, true)
			}
		default: // signed
			// Read the extensions once per certificate: when first signed and whenever it was reissued.
			if status.CertificateExtensions == nil || status.CertificateFingerprint != cs.Fingerprint {
				r.refreshCertificateExtensions(ctx, cluster, node, status, true)
			}
			status.CertificateSigned = true
			status.CertificateFingerprint = cs.Fingerprint
			status.CertificateNotAfter = caNotAfter(cs.NotAfter, status.CertificateNotAfter)
		}
	}

	readyCerts, err := r.reconcileCertManagerCertificates(ctx, node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling cert-manager Certificates: %w", err)
	}
	status.ReadyCertificates = readyCerts
	status.CACleaned = false
	status.PuppetDBDeactivated = false

	// Last check-in and report status from OpenVox DB.
	seen, deactivated := false, false
	var dbErr error
	if dbClient := resolveDBClient(ctx, r.Client, r.DBClient, cluster); dbClient != nil && cluster.Status.DatabaseURL != "" {
		pn, err := dbClient.GetNode(ctx, cluster.Status.DatabaseURL, node.Spec.Certname)
		switch {
		case err == nil:
			if t := pn.LastSeen(); t != nil {
				mt := metav1.NewTime(*t)
				status.LastSeenTime = &mt
				seen = true
			}
			status.LastReportStatus = pn.LatestReportStatus
			// OpenVox DB keeps deactivated and expired nodes around; they are not checking in. Keep the last
			// check-in (so "not reporting" alerts keep firing for a machine that aged out), but not its report
			// status: when a certname is reused, the old machine's failed run must not show up as the new one's.
			deactivated = pn.Deactivated != nil || pn.Expired != nil
			if deactivated {
				status.LastReportStatus = ""
			}
		case !errors.Is(err, ErrNodeNotFound):
			// Keep the last known check-in: an OpenVox DB outage must not demote an Active node.
			dbErr = err
			seen = status.LastSeenTime != nil
			logger.Info("OpenVox DB query failed; will retry", "certname", node.Spec.Certname, "reason", err.Error())
			requeue = nodeRequeueWaiting
		}
	}

	switch {
	case waitingReason != "":
		status.Phase = openvoxv1alpha1.NodePhasePendingCSR
		if waitingReason == "CAUnreachable" && status.CertificateSigned {
			status.Phase = openvoxv1alpha1.NodePhaseSigned
		}
		setReady(false, waitingReason, waitingMsg)
	case deactivated:
		status.Phase = openvoxv1alpha1.NodePhaseSigned
		setReady(false, "NodeDeactivated",
			"The node is deactivated or expired in OpenVox DB; its next Puppet run reactivates it")
	case !seen && dbErr != nil:
		status.Phase = openvoxv1alpha1.NodePhaseSigned
		setReady(true, "DatabaseUnreachable", "Certificate signed, but OpenVox DB can't be queried: "+dbErr.Error())
	case !seen:
		// Poll quickly until the first run shows up; the agent usually runs right after signing.
		status.Phase = openvoxv1alpha1.NodePhaseSigned
		setReady(true, "CertificateSigned", "Certificate signed; waiting for the first Puppet run to reach OpenVox DB")
		requeue = nodeRequeueWaiting
	default:
		status.Phase = openvoxv1alpha1.NodePhaseActive
		msg := "Node is signed, classified and reporting"
		if status.LastReportStatus == "failed" {
			msg = "Node is reporting, but its last Puppet run failed"
		}
		if dbErr != nil {
			msg += " (last check-in from before the current OpenVox DB outage: " + dbErr.Error() + ")"
		}
		setReady(true, "NodeActive", msg)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *VoxNodeReconciler) event(obj runtime.Object, eventType, reason, msg string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, nil, eventType, reason, reason, "%s", msg)
}

func (r *VoxNodeReconciler) updateVoxNodeStatus(ctx context.Context, key types.NamespacedName, desired openvoxv1alpha1.VoxNodeStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &openvoxv1alpha1.VoxNode{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		if statusJSONEqual(latest.Status, desired) {
			return nil // nothing changed: no write, no watch event
		}
		latest.Status = desired
		return r.Status().Update(ctx, latest)
	})
}

// cleanupNode cleans the certificate on the CA and deactivates the node in OpenVox DB, skipping the steps
// already done. It reports which steps are done; failures are logged and retried by the caller.
// A cluster without its own CA or OpenVox DB has nothing to clean there.
func (r *VoxNodeReconciler) cleanupNode(ctx context.Context, node *openvoxv1alpha1.VoxNode,
	cluster *openvoxv1alpha1.VoxCluster, caCleaned, dbDeactivated bool) (bool, bool) {
	logger := log.FromContext(ctx)
	if !caCleaned {
		caClient := resolveCAClient(ctx, r.Client, r.CAClient, cluster)
		switch {
		case !caEnabled(cluster) || caClient == nil:
			caCleaned = true
		case cluster.Status.CAURL == "":
			logger.Info("CA not up yet; will retry the certificate clean", "certname", node.Spec.Certname)
		default:
			if err := caClient.CleanNode(ctx, cluster.Status.CAURL, node.Spec.Certname); err != nil {
				logger.Info("CA clean failed; will retry", "certname", node.Spec.Certname, "reason", err.Error())
			} else {
				caCleaned = true
				r.event(node, corev1.EventTypeNormal, "CertificateCleaned", "Cleaned "+node.Spec.Certname+" on the CA")
			}
		}
	}
	if !dbDeactivated {
		dbClient := resolveDBClient(ctx, r.Client, r.DBClient, cluster)
		switch {
		case !databaseEnabled(cluster) || dbClient == nil:
			dbDeactivated = true
		case cluster.Status.DatabaseURL == "":
			logger.Info("OpenVox DB not up yet; will retry the deactivation", "certname", node.Spec.Certname)
		default:
			if err := dbClient.DeactivateNode(ctx, cluster.Status.DatabaseURL, node.Spec.Certname); err != nil {
				logger.Info("OpenVox DB deactivation failed; will retry", "certname", node.Spec.Certname, "reason", err.Error())
			} else {
				dbDeactivated = true
				r.event(node, corev1.EventTypeNormal, "NodeDeactivated", "Deactivated "+node.Spec.Certname+" in OpenVox DB")
			}
		}
	}
	return caCleaned, dbDeactivated
}

// deleteNodeCertificates deletes the cert-manager Certificates controlled by node whose name is not in keep
// (nil keep deletes all of them). Certificates someone else created under the same name are left alone.
func (r *VoxNodeReconciler) deleteNodeCertificates(ctx context.Context, node *openvoxv1alpha1.VoxNode, keep map[string]bool) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "CertificateList"})
	if err := r.List(ctx, list, client.InNamespace(node.Namespace),
		client.MatchingLabels{"voxkube.io/node": sanitizeLabelValue(node.Spec.Certname)}); err != nil {
		if isNoMatch(err, "Certificate") || isNoMatch(err, "CertificateList") {
			return nil
		}
		return err
	}
	for i := range list.Items {
		c := &list.Items[i]
		if keep[c.GetName()] || !metav1.IsControlledBy(c, node) {
			continue
		}
		if err := r.Delete(ctx, c); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *VoxNodeReconciler) reconcileCertManagerCertificates(ctx context.Context, node *openvoxv1alpha1.VoxNode) (int32, error) {
	var readyCount int32
	wanted := map[string]bool{}
	for _, req := range node.Spec.Certificates {
		wanted[CertManagerSecretName(node, req)] = true
	}
	// Drop Certificates whose entry was removed from spec.certificates.
	if err := r.deleteNodeCertificates(ctx, node, wanted); err != nil {
		return 0, err
	}
	for _, req := range node.Spec.Certificates {
		secName := CertManagerSecretName(node, req)
		issuerKind := firstNonEmpty(req.IssuerRef.Kind, "ClusterIssuer")
		issuerGroup := firstNonEmpty(req.IssuerRef.Group, "cert-manager.io")

		certObj := &unstructured.Unstructured{}
		certObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
		certObj.SetName(secName)
		certObj.SetNamespace(node.Namespace)
		certObj.SetLabels(map[string]string{
			"app.kubernetes.io/name":       "voxkube",
			"app.kubernetes.io/managed-by": "voxkube-operator",
			"voxkube.io/node":              sanitizeLabelValue(node.Spec.Certname),
			"voxkube.io/cert-name":         sanitizeLabelValue(req.Name),
		})

		spec := map[string]interface{}{
			"secretName": secName,
			"issuerRef": map[string]interface{}{
				"name":  req.IssuerRef.Name,
				"kind":  issuerKind,
				"group": issuerGroup,
			},
		}
		dnsNames := req.DNSNames
		if len(dnsNames) == 0 {
			dnsNames = []string{node.Spec.Certname}
		}
		spec["dnsNames"] = toInterfaceSlice(dnsNames)
		if len(req.IPAddresses) > 0 {
			spec["ipAddresses"] = toInterfaceSlice(req.IPAddresses)
		}
		if req.Duration != "" {
			spec["duration"] = req.Duration
		}
		if req.RenewBefore != "" {
			spec["renewBefore"] = req.RenewBefore
		}
		certObj.Object["spec"] = spec

		if err := controllerutil.SetControllerReference(node, certObj, r.Scheme); err != nil {
			return readyCount, err
		}

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(certObj.GroupVersionKind())
		err := r.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: secName}, existing)
		switch {
		case apierrors.IsNotFound(err):
			stampAppliedHash(certObj)
			if createErr := r.Create(ctx, certObj); createErr != nil && !isNoMatch(createErr, "Certificate") {
				return readyCount, createErr
			}
		case err == nil && !metav1.IsControlledBy(existing, node):
			return readyCount, fmt.Errorf("certificate %q: a cert-manager Certificate %s already exists and is not owned by this VoxNode; choose another secretName",
				req.Name, secName)
		case err == nil:
			if prepareUnstructuredUpdate(existing, certObj) {
				if err := r.Update(ctx, certObj); err != nil {
					return readyCount, err
				}
			}
		case !isNoMatch(err, "Certificate"):
			return readyCount, err
		}

		tlsSec := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: secName}, tlsSec); err == nil {
			if len(tlsSec.Data["tls.crt"]) > 0 && len(tlsSec.Data["tls.key"]) > 0 {
				readyCount++
			}
		}
	}
	return readyCount, nil
}

func isNoMatch(err error, kind string) bool {
	return meta.IsNoMatchError(err) || strings.Contains(err.Error(), fmt.Sprintf("no matches for kind %q", kind))
}

func toInterfaceSlice(in []string) []interface{} {
	out := make([]interface{}, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// sanitizeLabelValue makes s a valid label value (<= 63 chars, alphanumerics plus -_.).
func sanitizeLabelValue(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			b[i] = '-'
		}
	}
	out := strings.Trim(string(b), "-_.")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-_.")
	}
	return out
}

// SetupWithManager registers VoxNodeReconciler with the controller manager.
func (r *VoxNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openvoxv1alpha1.VoxNode{}, builder.WithPredicates(specOrMetaChanged())).
		Watches(&openvoxv1alpha1.VoxCluster{}, handler.EnqueueRequestsFromMapFunc(r.nodesForCluster),
			builder.WithPredicates(voxClusterChangedForDependents())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.nodesForSecret), builder.OnlyMetadata).
		Complete(r)
}

// nodesForCluster enqueues every VoxNode that references the changed VoxCluster (e.g. when its CA becomes ready).
func (r *VoxNodeReconciler) nodesForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	nodes := &openvoxv1alpha1.VoxNodeList{}
	if err := r.List(ctx, nodes, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, n := range nodes.Items {
		if n.Spec.ClusterRef == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: n.Namespace, Name: n.Name}})
		}
	}
	return reqs
}

// nodesForSecret enqueues VoxNodes whose certificate or secret references match the changed Secret.
func (r *VoxNodeReconciler) nodesForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	nodes := &openvoxv1alpha1.VoxNodeList{}
	if err := r.List(ctx, nodes, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range nodes.Items {
		n := &nodes.Items[i]
		match := false
		for _, c := range n.Spec.Certificates {
			if CertManagerSecretName(n, c) == obj.GetName() {
				match = true
			}
		}
		for _, s := range n.Spec.Secrets {
			if s.SecretName == obj.GetName() {
				match = true
			}
		}
		if match {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: n.Namespace, Name: n.Name}})
		}
	}
	return reqs
}

// statusJSONEqual compares two statuses as the API server stores them (times at second precision), so a
// freshly computed status equal to the stored one is recognised as unchanged.
func statusJSONEqual(a, b interface{}) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// certnameOwner returns the VoxNode that owns node's (clusterRef, certname): the oldest by creation time, then
// name. Deleting VoxNodes still count, so a duplicate doesn't take over while the owner's cleanup runs.
func (r *VoxNodeReconciler) certnameOwner(ctx context.Context, node *openvoxv1alpha1.VoxNode) (*openvoxv1alpha1.VoxNode, error) {
	list := &openvoxv1alpha1.VoxNodeList{}
	if err := r.List(ctx, list, client.InNamespace(node.Namespace)); err != nil {
		return nil, err
	}
	var owner *openvoxv1alpha1.VoxNode
	for i := range list.Items {
		n := &list.Items[i]
		if n.Spec.ClusterRef != node.Spec.ClusterRef || n.Spec.Certname != node.Spec.Certname {
			continue
		}
		if owner == nil || olderVoxNode(n, owner) {
			owner = n
		}
	}
	return owner, nil
}

func olderVoxNode(a, b *openvoxv1alpha1.VoxNode) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// servedVoxNodes returns the VoxNodes cluster classifies and renders Hiera data for, oldest first: one per certname
// (the owner; duplicates are skipped), and only those nodeServedBy accepts.
func servedVoxNodes(nodes []openvoxv1alpha1.VoxNode, cluster *openvoxv1alpha1.VoxCluster) []*openvoxv1alpha1.VoxNode {
	sorted := make([]*openvoxv1alpha1.VoxNode, 0, len(nodes))
	for i := range nodes {
		if nodes[i].Spec.ClusterRef == cluster.Name {
			sorted = append(sorted, &nodes[i])
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return olderVoxNode(sorted[i], sorted[j]) })
	seen := map[string]bool{}
	var out []*openvoxv1alpha1.VoxNode
	for _, n := range sorted {
		if seen[n.Spec.Certname] {
			continue // a duplicate: the owner came first
		}
		seen[n.Spec.Certname] = true
		if nodeServedBy(n, cluster) && !reservedCertname(cluster, n.Spec.Certname) {
			out = append(out, n)
		}
	}
	return out
}

// reservedCertname reports certnames that belong to the cluster's own services: the compiler/operator and DB
// certnames, and any in-cluster service name. OpenVox DB's allowlist trusts the service certname, so an agent must
// never be able to get a certificate for it.
func reservedCertname(cluster *openvoxv1alpha1.VoxCluster, certname string) bool {
	return certname == serviceCertname(cluster) || certname == dbCertname(cluster) ||
		strings.HasSuffix(certname, ".svc") || strings.HasSuffix(certname, ".svc.cluster.local")
}

// caNotAfter converts a CA not_after into status, keeping prev when it is unchanged (no status churn) and nil when
// the CA didn't report one.
func caNotAfter(s string, prev *metav1.Time) *metav1.Time {
	t, ok := parseCATime(s)
	if !ok {
		return nil
	}
	if prev != nil && prev.Time.Equal(t) {
		return prev
	}
	mt := metav1.NewTime(t)
	return &mt
}

// markNodePaused records the Paused condition (only when it changes) and emits one event when pausing starts.
func (r *VoxNodeReconciler) markNodePaused(ctx context.Context, node *openvoxv1alpha1.VoxNode, reason, msg string) error {
	status := node.Status.DeepCopy()
	if setPaused(&status.Conditions, reason, msg, node.Generation) {
		r.event(node, corev1.EventTypeNormal, "Paused", msg)
	}
	if err := r.updateVoxNodeStatus(ctx, client.ObjectKeyFromObject(node), *status); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
