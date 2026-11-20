package controller

import (
	"context"
	"errors"
	"fmt"
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

	nodeRequeueWaiting = 30 * time.Second
	nodeRequeueActive  = 5 * time.Minute
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

	// Deletion: clean CA + PuppetDB, then release the finalizer.
	if !node.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(node, VoxNodeFinalizer) {
			if clusterFound {
				r.decommissionNode(ctx, node, cluster)
				_ = r.refreshClusterHieraAndENC(ctx, cluster)
			}
			controllerutil.RemoveFinalizer(node, VoxNodeFinalizer)
			if err := r.Update(ctx, node); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(node, VoxNodeFinalizer) {
		controllerutil.AddFinalizer(node, VoxNodeFinalizer)
		if err := r.Update(ctx, node); err != nil {
			return ctrl.Result{}, err
		}
	}

	status := node.Status.DeepCopy()
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

	var result ctrl.Result
	switch node.Spec.State {
	case openvoxv1alpha1.NodeStateDecommissioned:
		r.decommissionNode(ctx, node, cluster)
		status.Phase = openvoxv1alpha1.NodePhaseDecommissioned
		status.CACleaned = true
		status.PuppetDBDeactivated = true
		status.CertificateSigned = false
		status.ReadyCertificates = 0
		setReady(true, "NodeDecommissioned", "Certificate cleaned from the CA and node deactivated in OpenVox DB")

	case openvoxv1alpha1.NodeStateRevoked:
		caURL := cluster.Status.CAURL
		if caClient := resolveCAClient(ctx, r.Client, r.CAClient, cluster); caClient != nil && caURL != "" {
			if err := caClient.RevokeCert(ctx, caURL, node.Spec.Certname); err != nil {
				return ctrl.Result{}, err
			}
		}
		status.Phase = openvoxv1alpha1.NodePhaseRevoked
		status.CertificateSigned = false
		setReady(true, "CertificateRevoked", "Certificate revoked on the CA; compilers reject it once the CRL is synced")

	case openvoxv1alpha1.NodeStatePendingApproval:
		status.Phase = openvoxv1alpha1.NodePhasePendingCSR
		msg := "Awaiting approval: set spec.state to Active to sign the CSR"
		if cs, err := r.caStatus(ctx, cluster, node); err == nil && cs.State == "requested" {
			msg = "CSR received; set spec.state to Active to sign it"
			r.refreshCertificateExtensions(ctx, cluster, node, status, false)
			if role := status.CertificateExtensions[ppRoleExtensionName]; role != "" {
				msg = fmt.Sprintf("CSR received (pp_role %q); set spec.state to Active to sign it", role)
			}
		}
		setReady(false, "PendingApproval", msg)

	default: // Active
		var err error
		result, err = r.reconcileActive(ctx, node, cluster, status, setReady)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	if node.Spec.State != openvoxv1alpha1.NodeStateActive && node.Spec.State != "" {
		if err := r.refreshClusterHieraAndENC(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}
	status.Role = effectiveRoleFor(node, status)
	if err := r.updateVoxNodeStatus(ctx, req.NamespacedName, *status); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
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
			status.CertificateExtensions = nil
			waitingReason, waitingMsg = "AwaitingCSR", "No CSR from "+node.Spec.Certname+" yet; run 'puppet ssl bootstrap' (or 'puppet agent -t') on the node"
			requeue = nodeRequeueWaiting
		case err != nil:
			logger.Info("CA not reachable; will retry", "certname", node.Spec.Certname, "reason", err.Error())
			waitingReason, waitingMsg = "CAUnreachable", err.Error()
			requeue = nodeRequeueWaiting
		case cs.State == "revoked":
			status.CertificateSigned = false
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
		}
	}

	readyCerts, err := r.reconcileCertManagerCertificates(ctx, node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling cert-manager Certificates: %w", err)
	}
	status.ReadyCertificates = readyCerts
	status.CACleaned = false
	status.PuppetDBDeactivated = false

	if err := r.refreshClusterHieraAndENC(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// Last check-in and report status from OpenVox DB.
	seen := false
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
		latest.Status = desired
		return r.Status().Update(ctx, latest)
	})
}

// decommissionNode cleans the certificate on the CA, deactivates the node in OpenVox DB and deletes its
// cert-manager Certificates. CA/DB failures are logged and do not block local cleanup.
func (r *VoxNodeReconciler) decommissionNode(ctx context.Context, node *openvoxv1alpha1.VoxNode, cluster *openvoxv1alpha1.VoxCluster) {
	logger := log.FromContext(ctx)
	if caClient := resolveCAClient(ctx, r.Client, r.CAClient, cluster); caClient != nil && cluster.Status.CAURL != "" {
		if err := caClient.CleanNode(ctx, cluster.Status.CAURL, node.Spec.Certname); err != nil {
			logger.Info("CA not reachable during decommission; proceeding", "certname", node.Spec.Certname, "reason", err.Error())
		}
	}
	if dbClient := resolveDBClient(ctx, r.Client, r.DBClient, cluster); dbClient != nil && cluster.Status.DatabaseURL != "" {
		if err := dbClient.DeactivateNode(ctx, cluster.Status.DatabaseURL, node.Spec.Certname); err != nil {
			logger.Info("OpenVox DB not reachable during decommission; proceeding", "certname", node.Spec.Certname, "reason", err.Error())
		}
	}
	for _, certReq := range node.Spec.Certificates {
		certObj := &unstructured.Unstructured{}
		certObj.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
		certObj.SetNamespace(node.Namespace)
		certObj.SetName(CertManagerSecretName(node, certReq))
		_ = r.Delete(ctx, certObj)
	}
}

func (r *VoxNodeReconciler) reconcileCertManagerCertificates(ctx context.Context, node *openvoxv1alpha1.VoxNode) (int32, error) {
	var readyCount int32
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
			if createErr := r.Create(ctx, certObj); createErr != nil && !isNoMatch(createErr, "Certificate") {
				return readyCount, createErr
			}
		case err == nil:
			certObj.SetResourceVersion(existing.GetResourceVersion())
			if err := r.Update(ctx, certObj); err != nil {
				return readyCount, err
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

func (r *VoxNodeReconciler) refreshClusterHieraAndENC(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) error {
	clusterRec := &VoxClusterReconciler{Client: r.Client, Scheme: r.Scheme}
	if _, err := clusterRec.reconcileHieraBridge(ctx, cluster); err != nil {
		return err
	}
	return clusterRec.reconcileENCConfigMap(ctx, cluster)
}

// SetupWithManager registers VoxNodeReconciler with the controller manager.
func (r *VoxNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openvoxv1alpha1.VoxNode{}).
		Watches(&openvoxv1alpha1.VoxCluster{}, handler.EnqueueRequestsFromMapFunc(r.nodesForCluster)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.nodesForSecret)).
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
