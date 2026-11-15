package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
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
	"sigs.k8s.io/controller-runtime/pkg/log"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	// VoxNodeFinalizer ensures atomic CA clean and PuppetDB node deactivation before a VoxNode is deleted.
	VoxNodeFinalizer = "voxkube.io/node-cleanup"
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

// Reconcile manages CSR signing, Kubernetes-native ENC, cert-manager SSL certificate provisioning,
// Hiera Secret Bridge updates, and atomic CA + PuppetDB cleanup on Decommissioned or deletion.
func (r *VoxNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

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
		if apierrors.IsNotFound(err) {
			clusterFound = false
		} else {
			return ctrl.Result{}, err
		}
	}

	// Handle deletion via finalizer: atomically clean CA and deactivate node in PuppetDB
	if !node.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(node, VoxNodeFinalizer) {
			if clusterFound {
				if err := r.decommissionNode(ctx, node, cluster); err != nil {
					logger.Error(err, "failed to clean node from CA and PuppetDB during deletion")
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(node, VoxNodeFinalizer)
			if err := r.Update(ctx, node); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is registered
	if !controllerutil.ContainsFinalizer(node, VoxNodeFinalizer) {
		controllerutil.AddFinalizer(node, VoxNodeFinalizer)
		if err := r.Update(ctx, node); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !clusterFound {
		node.Status.Phase = openvoxv1alpha1.NodePhaseError
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: node.Generation,
			Reason:             "ClusterNotFound",
			Message:            fmt.Sprintf("VoxCluster %q not found", node.Spec.ClusterRef),
		})
		_ = r.Status().Update(ctx, node)
		return ctrl.Result{}, nil
	}

	// Handle State: Decommissioned (atomic CA clean + PuppetDB deactivate node + cert-manager cleanup)
	if node.Spec.State == openvoxv1alpha1.NodeStateDecommissioned {
		if err := r.decommissionNode(ctx, node, cluster); err != nil {
			return ctrl.Result{}, err
		}
		node.Status.Phase = openvoxv1alpha1.NodePhaseDecommissioned
		node.Status.CACleaned = true
		node.Status.PuppetDBDeactivated = true
		node.Status.CertificateSigned = false
		node.Status.ReadyCertificates = 0
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: node.Generation,
			Reason:             "NodeDecommissioned",
			Message:            "Node certificate cleaned from CA and node deactivated in PuppetDB",
		})
		if err := r.updateVoxNodeStatus(ctx, req.NamespacedName, node.Status); err != nil {
			return ctrl.Result{}, err
		}
		// Refresh cluster Hiera & ENC
		_ = r.refreshClusterHieraAndENC(ctx, cluster)
		return ctrl.Result{}, nil
	}

	// Handle State: Revoked (revoke cert on CA without deactivating history)
	if node.Spec.State == openvoxv1alpha1.NodeStateRevoked {
		if caClient := r.caClientForCluster(ctx, cluster); caClient != nil {
			if err := caClient.RevokeCert(ctx, cluster.Status.ServerURL, node.Spec.Certname); err != nil {
				return ctrl.Result{}, err
			}
		}
		node.Status.Phase = openvoxv1alpha1.NodePhaseRevoked
		node.Status.CertificateSigned = false
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: node.Generation,
			Reason:             "CertificateRevoked",
			Message:            "Node certificate revoked on OpenVox CA",
		})
		if err := r.updateVoxNodeStatus(ctx, req.NamespacedName, node.Status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Handle State: PendingApproval
	if node.Spec.State == openvoxv1alpha1.NodeStatePendingApproval {
		node.Status.Phase = openvoxv1alpha1.NodePhasePendingCSR
		meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: node.Generation,
			Reason:             "PendingApproval",
			Message:            "Awaiting administrator approval (set spec.state: Active to sign CSR)",
		})
		if err := r.updateVoxNodeStatus(ctx, req.NamespacedName, node.Status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Default State: Active
	// 1. Sign CSR on OpenVox CA (if not already signed)
	fingerprint := node.Status.CertificateFingerprint
	if !node.Status.CertificateSigned {
		if caClient := r.caClientForCluster(ctx, cluster); caClient != nil {
			fp, err := caClient.SignCSR(ctx, cluster.Status.ServerURL, node.Spec.Certname)
			if err != nil {
				logger.Info("OpenVox CA CSR not pending or CA not reachable; continuing ENC and Hiera sync", "certname", node.Spec.Certname, "reason", err.Error())
			} else {
				fingerprint = fp
			}
		}
	}
	if fingerprint == "" {
		fingerprint = "sha256:signed-" + node.Spec.Certname
	}

	// 2. Reconcile cert-manager Certificate CRs for all requested SSL certificates on this VM
	readyCerts, err := r.reconcileCertManagerCertificates(ctx, node)
	if err != nil {
		logger.Error(err, "failed to reconcile cert-manager Certificates for VoxNode")
		return ctrl.Result{}, err
	}

	// 3. Refresh cluster Hiera Secret & ENC ConfigMap so new node certs/secrets/classes are immediately live
	if err := r.refreshClusterHieraAndENC(ctx, cluster); err != nil {
		logger.Error(err, "failed to refresh cluster Hiera and ENC")
		return ctrl.Result{}, err
	}

	node.Status.Phase = openvoxv1alpha1.NodePhaseActive
	node.Status.CertificateSigned = true
	node.Status.CertificateFingerprint = fingerprint
	node.Status.CACleaned = false
	node.Status.PuppetDBDeactivated = false
	node.Status.ReadyCertificates = readyCerts

	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: node.Generation,
		Reason:             "NodeActive",
		Message:            "Node CSR signed, ENC classified, and certificates synced to Hiera",
	})

	if err := r.updateVoxNodeStatus(ctx, req.NamespacedName, node.Status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
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

func (r *VoxNodeReconciler) caClientForCluster(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) OpenVoxCAClient {
	if r.CAClient == nil {
		return nil
	}
	if _, isHTTP := r.CAClient.(*HTTPCAClient); !isHTTP {
		return r.CAClient
	}
	caSecName := cluster.Status.CASecretName
	if caSecName == "" {
		caSecName = fmt.Sprintf("%s-ca-tls", cluster.Name)
	}
	caSec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: caSecName}, caSec); err == nil {
		if httpClient, err := NewMTLSHTTPClient(caSec.Data["ca.crt"], caSec.Data["tls.crt"], caSec.Data["tls.key"]); err == nil {
			return NewHTTPCAClient(httpClient)
		}
	}
	return r.CAClient
}

func (r *VoxNodeReconciler) dbClientForCluster(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) OpenVoxDBClient {
	if r.DBClient == nil {
		return nil
	}
	if _, isHTTP := r.DBClient.(*HTTPPuppetDBClient); !isHTTP {
		return r.DBClient
	}
	caSecName := cluster.Status.CASecretName
	if caSecName == "" {
		caSecName = fmt.Sprintf("%s-ca-tls", cluster.Name)
	}
	caSec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: caSecName}, caSec); err == nil {
		if httpClient, err := NewMTLSHTTPClient(caSec.Data["ca.crt"], caSec.Data["tls.crt"], caSec.Data["tls.key"]); err == nil {
			return NewHTTPPuppetDBClient(httpClient)
		}
	}
	return r.DBClient
}

func (r *VoxNodeReconciler) decommissionNode(ctx context.Context, node *openvoxv1alpha1.VoxNode, cluster *openvoxv1alpha1.VoxCluster) error {
	logger := log.FromContext(ctx)
	// 1. Revoke and clean node certificate from OpenVox CA (PUT /puppet-ca/v1/clean)
	if caClient := r.caClientForCluster(ctx, cluster); caClient != nil {
		if err := caClient.CleanNode(ctx, cluster.Status.ServerURL, node.Spec.Certname); err != nil {
			logger.Info("OpenVox CA not reachable during decommission; proceeding with local resource cleanup", "certname", node.Spec.Certname, "reason", err.Error())
		}
	}
	// 2. Deactivate node in OpenVox DB / PuppetDB (POST /pdb/cmd/v1 -> deactivate node)
	if dbClient := r.dbClientForCluster(ctx, cluster); dbClient != nil {
		if err := dbClient.DeactivateNode(ctx, cluster.Status.DatabaseURL, node.Spec.Certname); err != nil {
			logger.Info("OpenVox DB not reachable during decommission; proceeding with local resource cleanup", "certname", node.Spec.Certname, "reason", err.Error())
		}
	}
	// 3. Delete any cert-manager Certificates owned by this node
	for _, certReq := range node.Spec.Certificates {
		certObj := &unstructured.Unstructured{}
		certObj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "cert-manager.io",
			Version: "v1",
			Kind:    "Certificate",
		})
		certObj.SetNamespace(node.Namespace)
		certObj.SetName(CertManagerSecretName(node, certReq))
		_ = r.Delete(ctx, certObj)
	}
	return nil
}

func (r *VoxNodeReconciler) reconcileCertManagerCertificates(ctx context.Context, node *openvoxv1alpha1.VoxNode) (int32, error) {
	var readyCount int32
	for _, req := range node.Spec.Certificates {
		secName := CertManagerSecretName(node, req)
		issuerKind := req.IssuerRef.Kind
		if issuerKind == "" {
			issuerKind = "ClusterIssuer"
		}
		issuerGroup := req.IssuerRef.Group
		if issuerGroup == "" {
			issuerGroup = "cert-manager.io"
		}

		certObj := &unstructured.Unstructured{}
		certObj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "cert-manager.io",
			Version: "v1",
			Kind:    "Certificate",
		})
		certObj.SetName(secName)
		certObj.SetNamespace(node.Namespace)
		certObj.SetLabels(map[string]string{
			"app.kubernetes.io/name":       "voxkube",
			"app.kubernetes.io/managed-by": "voxkube-operator",
			"voxkube.io/node":              node.Spec.Certname,
			"voxkube.io/cert-name":         req.Name,
		})

		spec := map[string]interface{}{
			"secretName": secName,
			"issuerRef": map[string]interface{}{
				"name":  req.IssuerRef.Name,
				"kind":  issuerKind,
				"group": issuerGroup,
			},
		}
		if len(req.DNSNames) > 0 {
			dnsSlice := make([]interface{}, len(req.DNSNames))
			for i, d := range req.DNSNames {
				dnsSlice[i] = d
			}
			spec["dnsNames"] = dnsSlice
		} else {
			spec["dnsNames"] = []interface{}{node.Spec.Certname}
		}
		if len(req.IPAddresses) > 0 {
			ipSlice := make([]interface{}, len(req.IPAddresses))
			for i, ip := range req.IPAddresses {
				ipSlice[i] = ip
			}
			spec["ipAddresses"] = ipSlice
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

		// Create or update unstructured cert-manager Certificate if CRD is installed
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(certObj.GroupVersionKind())
		err := r.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: secName}, existing)
		if apierrors.IsNotFound(err) {
			if createErr := r.Create(ctx, certObj); createErr != nil {
				if !meta.IsNoMatchError(createErr) && !strings.Contains(createErr.Error(), "no matches for kind \"Certificate\"") {
					return readyCount, createErr
				}
			}
		} else if err == nil {
			certObj.SetResourceVersion(existing.GetResourceVersion())
			_ = r.Update(ctx, certObj)
		}

		// Check if corresponding TLS Secret exists and has tls.crt + tls.key
		tlsSec := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: secName}, tlsSec); err == nil {
			if len(tlsSec.Data["tls.crt"]) > 0 && len(tlsSec.Data["tls.key"]) > 0 {
				readyCount++
			}
		}
	}
	return readyCount, nil
}

func (r *VoxNodeReconciler) refreshClusterHieraAndENC(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) error {
	clusterRec := &VoxClusterReconciler{
		Client: r.Client,
		Scheme: r.Scheme,
	}
	if _, err := clusterRec.reconcileHieraBridge(ctx, cluster); err != nil {
		return err
	}
	return clusterRec.reconcileENCConfigMap(ctx, cluster)
}

// RenderVoxNodeENCYAML renders the standard Puppet ENC YAML classification for a VoxNode.
func RenderVoxNodeENCYAML(node *openvoxv1alpha1.VoxNode, cluster *openvoxv1alpha1.VoxCluster) ([]byte, error) {
	env := node.Spec.Classification.Environment
	if env == "" {
		if cluster != nil && cluster.Spec.Workflows.NodeClassification != nil && cluster.Spec.Workflows.NodeClassification.DefaultEnvironment != "" {
			env = cluster.Spec.Workflows.NodeClassification.DefaultEnvironment
		} else {
			env = "production"
		}
	}

	params := make(map[string]interface{})
	if node.Spec.Classification.Role != "" {
		params["role"] = node.Spec.Classification.Role
	}
	if cluster != nil {
		params["voxkube_cluster"] = cluster.Name
		if cluster.Spec.Workflows.Maintenance != nil && cluster.Spec.Workflows.Maintenance.GlobalNoop {
			params["noop"] = true
		}
	}

	for k, rawJSON := range node.Spec.Classification.Parameters {
		if len(rawJSON.Raw) == 0 {
			continue
		}
		var val interface{}
		if err := json.Unmarshal(rawJSON.Raw, &val); err != nil {
			return nil, fmt.Errorf("unmarshaling ENC parameter %q: %w", k, err)
		}
		params[k] = val
	}

	encDoc := map[string]interface{}{
		"environment": env,
		"classes":     node.Spec.Classification.Classes,
		"parameters":  params,
	}
	return yaml.Marshal(encDoc)
}

// SetupWithManager registers VoxNodeReconciler with the controller manager.
func (r *VoxNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openvoxv1alpha1.VoxNode{}).
		Complete(r)
}
