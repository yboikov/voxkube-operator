package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	// AnnotationSyncNow can be set to any new value to force an immediate reconcile (and Git poll).
	AnnotationSyncNow = "voxkube.io/sync-now"
	// AnnotationObservedCommit records the control-repo HEAD commit on the compiler pod template.
	AnnotationObservedCommit = "voxkube.io/observed-commit"
	// AnnotationCodeRevision is a hash of all control-repo branch heads; changing it rolls the compilers.
	AnnotationCodeRevision = "voxkube.io/code-revision"
	// AnnotationConfigHash is a hash of the configuration files mounted into a pod via subPath.
	AnnotationConfigHash = "voxkube.io/config-hash"
	// AnnotationTLSHash is a hash of the service certificate; changing it restarts the pods that serve it.
	AnnotationTLSHash = "voxkube.io/tls-hash"
	// AnnotationSpecHash records the hash of the desired spec of operator-managed objects.
	AnnotationSpecHash = "voxkube.io/spec-hash"

	// Condition types reported on VoxCluster.
	ConditionReady         = "Ready"
	ConditionCAReady       = "CAReady"
	ConditionDatabaseReady = "DatabaseReady"
	ConditionCodeReady     = "CodeReady"
	ConditionServerReady   = "ServerReady"
	// ConditionHieraSecretsCapacity is defined in hiera_shards.go.

	defaultClusterResync = 60 * time.Second
)

// VoxClusterReconciler reconciles a VoxCluster object.
type VoxClusterReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       events.EventRecorder
	GitPoller      GitRemotePoller
	TokenExchanger *GitHubAppTokenExchanger
	CAClient       OpenVoxCAClient
}

// clusterState collects what one reconcile observed; it is turned into VoxCluster status.
type clusterState struct {
	caSecretName      string
	tlsHash           string // CA endpoint certificate (<cluster>-ca-tls)
	serverTLSHash     string // compiler certificate (<cluster>-server-tls)
	dbTLSHash         string // OpenVox DB certificate (<cluster>-db-tls)
	caURL             string
	caEnabled         bool
	caReady           bool
	caMessage         string
	dbEnabled         bool
	dbURL             string
	dbReady           bool
	dbMessage         string
	hiera             *hieraLayout // nil when the Hiera Secret Bridge is disabled
	hieraMeasured     *hieraLayout // sizes from step 0, also when writing failed
	code              *codePlan
	serverURL         string
	serverExternalURL string
	caExternalURL     string
	readyReplicas     int32
	desiredReplicas   int32
}

// +kubebuilder:rbac:groups=voxkube.io,resources=voxclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=voxkube.io,resources=voxclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=voxkube.io,resources=voxclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=voxkube.io,resources=voxnodes,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;configmaps;services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete

// Reconcile converges a VoxCluster into a complete OpenVox stack.
func (r *VoxClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &openvoxv1alpha1.VoxCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			mtlsClients.forget(req.Namespace + "/" + req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	st := &clusterState{}
	recErr := r.reconcileStack(ctx, cluster, st)
	if recErr != nil {
		logger.Error(recErr, "VoxCluster reconcile failed")
	}

	phase, statusErr := r.updateClusterStatus(ctx, req.NamespacedName, cluster, st, recErr)
	if recErr != nil {
		return ctrl.Result{}, recErr
	}
	if statusErr != nil {
		return ctrl.Result{}, statusErr
	}

	requeue := defaultClusterResync
	if phase != openvoxv1alpha1.ClusterPhaseRunning {
		requeue = 15 * time.Second
	}
	if st.code != nil && st.code.pollInterval > 0 && st.code.pollInterval < requeue {
		requeue = st.code.pollInterval
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *VoxClusterReconciler) reconcileStack(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, st *clusterState) error {
	logger := log.FromContext(ctx)

	// 0. ENC and Hiera data first, so that a VoxNode change (revoked, decommissioned, deleted, new role) reaches
	// the compilers even while a later step (CA, Git, OpenVox DB) is failing. This controller is their only writer.
	if err := r.reconcileENCConfigMap(ctx, cluster); err != nil {
		return fmt.Errorf("reconciling ENC ConfigMap: %w", err)
	}
	hiera, hieraErr := r.reconcileHieraBridge(ctx, cluster)
	st.hieraMeasured = hiera

	// 1. CA key material and the shared service certificate (<cluster>-ca-tls).
	secName, tlsHash, err := r.reconcileCAPKI(ctx, cluster)
	if err != nil {
		return fmt.Errorf("reconciling CA PKI: %w", err)
	}
	st.caSecretName, st.tlsHash = secName, tlsHash

	// 2. Dedicated CA StatefulSet.
	if err := r.reconcileCAWorkload(ctx, cluster, st); err != nil {
		return fmt.Errorf("reconciling CA workload: %w", err)
	}

	// 3. CA housekeeping that needs a running CA: CRL sync and VoxNode auto-creation (best effort).
	if st.caReady && st.caURL != "" {
		if caClient := r.caClientForCluster(ctx, cluster); caClient != nil {
			if err := r.syncCRL(ctx, cluster, caClient, st.caURL, secName); err != nil {
				logger.Info("CRL sync from CA failed; will retry", "reason", err.Error())
			}
			if autoCreateVoxNodesEnabled(cluster) {
				if err := r.autoCreateVoxNodes(ctx, cluster, caClient, st.caURL); err != nil {
					logger.Info("VoxNode auto-creation failed; will retry", "reason", err.Error())
				}
			}
		}
	}

	// 3b. Workload certificates for compilers and OpenVox DB (no CA key, no pp_cli_auth), carrying the CRL
	// synced above.
	if st.serverTLSHash, err = r.reconcileWorkloadTLS(ctx, cluster, "server", serverSecretName(cluster),
		serviceCertname(cluster), serverDNSNames(cluster)); err != nil {
		return fmt.Errorf("reconciling compiler TLS Secret: %w", err)
	}
	if databaseEnabled(cluster) {
		if st.dbTLSHash, err = r.reconcileWorkloadTLS(ctx, cluster, "db", dbSecretName(cluster),
			dbCertname(cluster), dbDNSNames(cluster)); err != nil {
			return fmt.Errorf("reconciling OpenVox DB TLS Secret: %w", err)
		}
	}

	// 4. Puppet code delivery (Git/g10k, PVC, image volume or demo code).
	plan, err := r.reconcileCode(ctx, cluster)
	if err != nil {
		return fmt.Errorf("reconciling Puppet code delivery: %w", err)
	}
	st.code = plan

	// 5. PostgreSQL + OpenVox DB.
	if err := r.reconcileDatabaseStack(ctx, cluster, st); err != nil {
		return fmt.Errorf("reconciling OpenVox DB: %w", err)
	}

	// 6. The Hiera Secret (rendered in step 0) must exist before the compilers mount it.
	if hieraErr != nil {
		return fmt.Errorf("reconciling Hiera Secret Bridge: %w", hieraErr)
	}
	st.hiera = hiera

	// 8. Compilers, autoscaling and networking.
	if err := r.reconcileServerDeployment(ctx, cluster, st); err != nil {
		return fmt.Errorf("reconciling compiler Deployment: %w", err)
	}
	if err := r.reconcileAutoscaling(ctx, cluster); err != nil {
		return fmt.Errorf("reconciling HorizontalPodAutoscaler: %w", err)
	}
	if err := r.reconcileNetworking(ctx, cluster, st); err != nil {
		return fmt.Errorf("reconciling networking: %w", err)
	}

	// 9. Remove what switched-off components left behind, last and only once the compilers have rolled out
	// without them, so no compiler pod still mounts (e.g.) a Hiera Secret that is about to disappear.
	if r.serverRolledOut(ctx, cluster) {
		if err := r.pruneDisabledComponents(ctx, cluster); err != nil {
			return fmt.Errorf("pruning disabled components: %w", err)
		}
	}
	return nil
}

// updateClusterStatus derives phase and conditions from what the reconcile observed and writes them.
func (r *VoxClusterReconciler) updateClusterStatus(
	ctx context.Context,
	key types.NamespacedName,
	cluster *openvoxv1alpha1.VoxCluster,
	st *clusterState,
	recErr error,
) (openvoxv1alpha1.VoxClusterPhase, error) {
	status := cluster.Status.DeepCopy()
	gen := cluster.Generation
	status.ObservedGeneration = gen

	if st.caSecretName != "" {
		status.CASecretName = st.caSecretName
	}
	// After a failed reconcile keep the last known endpoints so VoxNodes/VoxDiscoveries keep working.
	if recErr == nil {
		status.CAURL = st.caURL
		status.DatabaseURL = st.dbURL
		status.HieraSecretName = ""
		status.HieraSecrets = nil
		if st.hiera != nil {
			if len(st.hiera.names) == 1 {
				status.HieraSecretName = st.hiera.names[0]
			}
			status.HieraSecrets = &openvoxv1alpha1.HieraSecretsStatus{
				Shards:              int32(len(st.hiera.names)),
				SecretNames:         st.hiera.names,
				LargestShardBytes:   st.hiera.largestBytes,
				LargestShardPercent: st.hiera.largestPercent(),
			}
		}
		status.ReadyReplicas = st.readyReplicas
		status.ServerExternalURL = st.serverExternalURL
		status.CAExternalURL = st.caExternalURL
	}
	if st.serverURL != "" {
		status.ServerURL = st.serverURL
	}
	if st.code != nil && st.code.observedCommit != "" {
		status.ObservedCommit = st.code.observedCommit
	}

	setCond := func(t string, ok bool, reason, msg string) {
		s := metav1.ConditionFalse
		if ok {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: t, Status: s, ObservedGeneration: gen, Reason: reason, Message: msg,
		})
	}

	r.setHieraCapacity(cluster, status, st.hieraMeasured, setCond)

	switch {
	case !st.caEnabled:
		setCond(ConditionCAReady, true, "Disabled", "Built-in CA is disabled (spec.ca.enabled=false)")
	case st.caReady:
		setCond(ConditionCAReady, true, "Ready", "CA StatefulSet is ready")
	default:
		setCond(ConditionCAReady, false, "NotReady", firstNonEmpty(st.caMessage, "CA StatefulSet is not ready yet"))
	}

	switch {
	case !st.dbEnabled:
		setCond(ConditionDatabaseReady, true, "Disabled", "OpenVox DB is disabled (spec.database.enabled=false)")
	case st.dbReady:
		setCond(ConditionDatabaseReady, true, "Ready", "OpenVox DB is ready")
	default:
		setCond(ConditionDatabaseReady, false, "NotReady", firstNonEmpty(st.dbMessage, "OpenVox DB is not ready yet"))
	}

	if st.code != nil {
		setCond(ConditionCodeReady, st.code.ready, st.code.reason, st.code.message)
	}

	if st.desiredReplicas > 0 {
		serverOK := st.readyReplicas >= st.desiredReplicas
		reason := "NotReady"
		if serverOK {
			reason = "Ready"
		}
		setCond(ConditionServerReady, serverOK, reason,
			fmt.Sprintf("%d/%d compiler pods ready", st.readyReplicas, st.desiredReplicas))
	}

	var phase openvoxv1alpha1.VoxClusterPhase
	switch {
	case recErr != nil:
		phase = openvoxv1alpha1.ClusterPhaseError
		setCond(ConditionReady, false, "ReconcileError", recErr.Error())
	case st.caEnabled && !st.caReady:
		phase = openvoxv1alpha1.ClusterPhaseInitializingCA
		setCond(ConditionReady, false, "InitializingCA", "Waiting for the CA")
	case st.dbEnabled && !st.dbReady:
		phase = openvoxv1alpha1.ClusterPhaseDeployingDatabase
		setCond(ConditionReady, false, "DeployingDatabase", "Waiting for OpenVox DB")
	case st.readyReplicas == 0:
		phase = openvoxv1alpha1.ClusterPhaseDeployingServer
		setCond(ConditionReady, false, "DeployingServer", "Waiting for compiler pods")
	case st.readyReplicas < st.desiredReplicas || (st.code != nil && !st.code.ready):
		phase = openvoxv1alpha1.ClusterPhaseDegraded
		msg := fmt.Sprintf("%d/%d compiler pods ready", st.readyReplicas, st.desiredReplicas)
		if st.code != nil && !st.code.ready {
			msg = st.code.message
		}
		setCond(ConditionReady, false, "Degraded", msg)
	default:
		phase = openvoxv1alpha1.ClusterPhaseRunning
		setCond(ConditionReady, true, "Running", "CA, OpenVox DB and compilers are ready")
	}
	status.Phase = phase

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &openvoxv1alpha1.VoxCluster{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		if reflect.DeepEqual(latest.Status, *status) {
			return nil
		}
		latest.Status = *status
		return r.Status().Update(ctx, latest)
	})
	return phase, err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func voxClusterLabels(clusterName, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "voxkube",
		"app.kubernetes.io/instance":   clusterName,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "voxkube-operator",
	}
}

func (r *VoxClusterReconciler) event(obj runtime.Object, eventType, reason, msg string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, nil, eventType, reason, reason, "%s", msg)
}

// hashStrings returns a short stable hash over a map of strings (used for config-hash annotations).
func hashStrings(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(m[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// ---------------------------------------------------------------------------
// create-or-update helpers
// ---------------------------------------------------------------------------

func (r *VoxClusterReconciler) createOrUpdateSecret(ctx context.Context, desired *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(existing.Data, desired.Data) || !reflect.DeepEqual(existing.Labels, desired.Labels) {
		existing.Data = desired.Data
		existing.Labels = desired.Labels
		return r.Update(ctx, existing)
	}
	return nil
}

func (r *VoxClusterReconciler) createOrUpdateConfigMap(ctx context.Context, desired *corev1.ConfigMap) error {
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(existing.Data, desired.Data) || !reflect.DeepEqual(existing.Labels, desired.Labels) {
		existing.Data = desired.Data
		existing.Labels = desired.Labels
		return r.Update(ctx, existing)
	}
	return nil
}

func specHash(v interface{}) string {
	b, _ := json.Marshal(v)
	return hashBytes(b)
}

func (r *VoxClusterReconciler) createOrUpdateDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	h := specHash(desired.Spec)
	if desired.Annotations == nil {
		desired.Annotations = make(map[string]string)
	}
	desired.Annotations[AnnotationSpecHash] = h

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Annotations[AnnotationSpecHash] == h && reflect.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	existing.Annotations[AnnotationSpecHash] = h
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *VoxClusterReconciler) createOrUpdateStatefulSet(ctx context.Context, desired *appsv1.StatefulSet) error {
	h := specHash(desired.Spec)
	if desired.Annotations == nil {
		desired.Annotations = make(map[string]string)
	}
	desired.Annotations[AnnotationSpecHash] = h

	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Annotations[AnnotationSpecHash] == h && reflect.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	existing.Annotations[AnnotationSpecHash] = h
	// StatefulSet volumeClaimTemplates, selector and serviceName are immutable; keep the existing ones.
	desired.Spec.VolumeClaimTemplates = existing.Spec.VolumeClaimTemplates
	desired.Spec.Selector = existing.Spec.Selector
	desired.Spec.ServiceName = existing.Spec.ServiceName
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *VoxClusterReconciler) createOrUpdateService(ctx context.Context, desired *corev1.Service) error {
	h := specHash(desired.Spec)
	if desired.Annotations == nil {
		desired.Annotations = make(map[string]string)
	}
	desired.Annotations[AnnotationSpecHash] = h

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Annotations[AnnotationSpecHash] == h && reflect.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	existing.Annotations[AnnotationSpecHash] = h
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	desired.Spec.ClusterIPs = existing.Spec.ClusterIPs
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *VoxClusterReconciler) createOrUpdateUnstructured(ctx context.Context, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.GetNamespace(), Name: desired.GetName()}, existing)
	if apierrors.IsNotFound(err) {
		stampAppliedHash(desired)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !prepareUnstructuredUpdate(existing, desired) {
		return nil
	}
	return r.Update(ctx, desired)
}

// SetupWithManager registers VoxClusterReconciler with the controller manager.
func (r *VoxClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openvoxv1alpha1.VoxCluster{}, builder.WithPredicates(specOrMetaChanged())).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		// Metadata only: Secrets are never cached in full (cmd/main.go disables the Secret cache).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.clustersForSecret),
			builder.OnlyMetadata,
		).
		Watches(
			&openvoxv1alpha1.VoxNode{},
			handler.EnqueueRequestsFromMapFunc(r.clusterForVoxNode),
			builder.WithPredicates(voxNodeChangedForCluster()),
		).
		Complete(r)
}

// clusterForVoxNode enqueues the VoxCluster a VoxNode belongs to.
func (r *VoxClusterReconciler) clusterForVoxNode(_ context.Context, obj client.Object) []reconcile.Request {
	n, ok := obj.(*openvoxv1alpha1.VoxNode)
	if !ok || n.Spec.ClusterRef == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: n.Namespace, Name: n.Spec.ClusterRef}}}
}
