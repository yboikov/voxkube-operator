package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func TestPrepareUnstructuredUpdateRemovesDroppedKeys(t *testing.T) {
	desired := func(ann map[string]string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "route.openshift.io/v1", "kind": "Route",
			"metadata": map[string]interface{}{"name": "r", "namespace": "ns"},
			"spec":     map[string]interface{}{"to": map[string]interface{}{"kind": "Service", "name": "svc"}},
		}}
		u.SetAnnotations(ann)
		u.SetLabels(map[string]string{"app": "x", "tier": "edge"})
		return u
	}
	live := desired(map[string]string{"ours/timeout": "5m", "ours/balance": "roundrobin"})
	stampAppliedHash(live)
	live.SetAnnotations(mergeStringMaps(live.GetAnnotations(), map[string]string{"other/note": "keep"}))
	live.SetLabels(mergeStringMaps(live.GetLabels(), map[string]string{"other-label": "keep"}))
	live.SetFinalizers([]string{"other.io/protect"})
	live.SetResourceVersion("3")

	next := desired(map[string]string{"ours/timeout": "5m"})
	next.SetLabels(map[string]string{"app": "x"})
	if !prepareUnstructuredUpdate(live, next) {
		t.Fatal("dropping an annotation must trigger an update")
	}
	ann, labels := next.GetAnnotations(), next.GetLabels()
	if _, ok := ann["ours/balance"]; ok {
		t.Error("an annotation removed from the spec must be removed from the object")
	}
	if ann["ours/timeout"] != "5m" || ann["other/note"] != "keep" {
		t.Errorf("kept annotations wrong: %v", ann)
	}
	if _, ok := labels["tier"]; ok || labels["other-label"] != "keep" || labels["app"] != "x" {
		t.Errorf("labels wrong: %v", labels)
	}
	if f := next.GetFinalizers(); len(f) != 1 || f[0] != "other.io/protect" {
		t.Errorf("foreign finalizers must be kept, got %v", f)
	}
}

// A cached token about to expire is refreshed, but writing the new one conflicts.
func TestGitHubAppTokenWriteConflictKeepsSecretMounted(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	_, keyPEM, err := GenerateSelfSignedCA("github-app-test", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_new","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
	}))
	defer gh.Close()

	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx", UID: "u1"},
		Spec: openvoxv1alpha1.VoxClusterSpec{Code: openvoxv1alpha1.ClusterCodeSpec{Mode: "git", Git: &openvoxv1alpha1.GitCodeSpec{
			Remote: "https://github.com/o/control.git", AuthType: "githubApp",
			GitHubApp: &openvoxv1alpha1.GitHubAppAuthSpec{AppID: 1, InstallationID: 2, APIBaseURL: gh.URL,
				PrivateKeySecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "app-key"}, Key: "key.pem"}},
		}}},
	}
	cached := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-g10k-git-token", Namespace: "ovx",
			Annotations: map[string]string{tokenExpiryAnno: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}},
		Data: map[string][]byte{"token": []byte("ghs_old")},
	}
	appKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "app-key", Namespace: "ovx"}, Data: map[string][]byte{"key.pem": keyPEM}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, cached, appKey).
		WithInterceptorFuncs(interceptor.Funcs{Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == "prod-g10k-git-token" {
				return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), nil)
			}
			return cl.Update(ctx, obj, opts...)
		}}).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s, TokenExchanger: NewGitHubAppTokenExchanger(gh.Client())}
	_, secName, err := r.reconcileGitHubAppToken(ctx, cluster)
	if err == nil {
		t.Fatal("expected the conflict to be reported")
	}
	if secName != "prod-g10k-git-token" {
		t.Errorf("a failed token write must keep the Secret mounted, got %q", secName)
	}
}

type countingPoller struct{ calls int }

func (p *countingPoller) Resolve(context.Context, string, *GitAuth) (GitRevision, error) {
	p.calls++
	return GitRevision{Head: "x", RefsHash: "r"}, nil
}

func TestSidecarModeDoesNotPollWithoutToken(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	off := false
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx", UID: "u1"},
		Spec: openvoxv1alpha1.VoxClusterSpec{Code: openvoxv1alpha1.ClusterCodeSpec{Mode: "git", Git: &openvoxv1alpha1.GitCodeSpec{
			Remote: "https://github.com/o/control.git", AuthType: "githubApp", SynchronizedRollout: &off,
			GitHubApp: &openvoxv1alpha1.GitHubAppAuthSpec{AppID: 1, InstallationID: 2,
				PrivateKeySecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "k"}},
		}}},
	}
	poller := &countingPoller{}
	r := &VoxClusterReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).Build(), Scheme: s, GitPoller: poller}
	plan, err := r.reconcileCode(ctx, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if plan.reason != codeReasonAuthFail {
		t.Errorf("expected %s, got %s", codeReasonAuthFail, plan.reason)
	}
	if poller.calls != 0 {
		t.Errorf("the remote must not be polled without credentials, got %d polls", poller.calls)
	}
}

func TestPruneWaitsForCompilerRollout(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	cluster := &openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox"}}
	one := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "openvox-server", Namespace: "openvox", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, d).WithStatusSubresource(&appsv1.Deployment{}).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s}
	if r.serverRolledOut(ctx, cluster) {
		t.Error("an old compiler pod is still running: not rolled out")
	}
	d.Status.Replicas = 1
	if err := c.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if !r.serverRolledOut(ctx, cluster) {
		t.Error("expected the rollout to be complete")
	}
}

func TestDeletingVoxNodeLeavesENCWhileCleanupPending(t *testing.T) {
	ctx := context.Background()
	node := testNode(openvoxv1alpha1.NodeStateActive)
	now := metav1.Now()
	node.DeletionTimestamp = &now
	node.Finalizers = []string{VoxNodeFinalizer}
	r, c, f := newLifecycleFixture(t, node, true)
	f.caDown = true
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	// The cleanup is pending, but the VoxCluster controller (the ENC's only writer) already leaves the node out.
	cluster := &openvoxv1alpha1.VoxCluster{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "openvox"}, cluster); err != nil {
		t.Fatal(err)
	}
	if err := (&VoxClusterReconciler{Client: c, Scheme: r.Scheme}).reconcileENCConfigMap(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "openvox-enc-nodes"}, cm); err != nil {
		t.Fatal(err)
	}
	if _, ok := cm.Data["web01.example.com.yaml"]; ok {
		t.Error("a deleting VoxNode must leave the ENC even while its cleanup is pending")
	}
}
