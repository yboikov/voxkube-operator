package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func setAnnotation(t *testing.T, c client.Client, obj client.Object, key types.NamespacedName, value string) {
	t.Helper()
	if err := c.Get(context.Background(), key, obj); err != nil {
		t.Fatal(err)
	}
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	if value == "" {
		delete(a, AnnotationPaused)
	} else {
		a[AnnotationPaused] = value
	}
	obj.SetAnnotations(a)
	if err := c.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}

func TestPausedVoxClusterWritesOnlyItsCondition(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(pkiCluster()).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
	r := &VoxClusterReconciler{Client: base, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
	reconcilePKICluster(t, base, r)
	key := types.NamespacedName{Namespace: "ovx", Name: "prod"}

	// Pause and change the spec in the same edit.
	cl := &openvoxv1alpha1.VoxCluster{}
	setAnnotation(t, base, cl, key, "true")
	three := int32(3)
	cl.Spec.Server.Replicas = &three
	if err := base.Update(ctx, cl); err != nil {
		t.Fatal(err)
	}

	var writes []string
	r.Client = interceptor.NewClient(base.(client.WithWatch), countingWrites(&writes))
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("a paused cluster must not requeue on a timer, got %v", res.RequeueAfter)
	}
	if len(writes) != 1 || !strings.HasPrefix(writes[0], "update/status") {
		t.Errorf("a paused cluster may only write its status, got %v", writes)
	}
	getObj(t, base, "prod", cl)
	if c := meta.FindStatusCondition(cl.Status.Conditions, ConditionPaused); c == nil || c.Status != metav1.ConditionTrue || c.Reason != pausedByAnnotation {
		t.Errorf("expected Paused=True, got %+v", c)
	}
	d := &appsv1.Deployment{}
	getObj(t, base, "prod-server", d)
	if *d.Spec.Replicas == 3 {
		t.Error("the spec change must not be applied while paused")
	}

	writes = nil
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 0 {
		t.Errorf("a repeated reconcile while paused must not write, got %v", writes)
	}

	r.Client = base
	setAnnotation(t, base, cl, key, "")
	reconcilePKICluster(t, base, r)
	getObj(t, base, "prod", cl)
	if meta.FindStatusCondition(cl.Status.Conditions, ConditionPaused) != nil {
		t.Error("the Paused condition must be removed on unpause")
	}
	getObj(t, base, "prod-server", d)
	if *d.Spec.Replicas != 3 {
		t.Errorf("the spec change must apply after unpausing, replicas %d", *d.Spec.Replicas)
	}
}

func TestVoxNodePausedByCluster(t *testing.T) {
	ctx := context.Background()
	r, c, f := newLifecycleFixture(t, testNode(openvoxv1alpha1.NodeStateActive), true)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	n := &openvoxv1alpha1.VoxNode{}
	if err := c.Get(ctx, web01Key, n); err != nil {
		t.Fatal(err)
	}
	if n.Status.CertificateNotAfter == nil || !n.Status.CertificateNotAfter.Time.Equal(time.Date(2031, 2, 23, 21, 6, 52, 0, time.UTC)) {
		t.Errorf("certificateNotAfter from the CA: %v", n.Status.CertificateNotAfter)
	}

	setAnnotation(t, c, &openvoxv1alpha1.VoxCluster{}, types.NamespacedName{Namespace: "openvox", Name: "openvox"}, "true")
	f.calls = 0
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 || res.RequeueAfter != 0 {
		t.Errorf("paused: %d CA/DB calls, requeue %v; want none", f.calls, res.RequeueAfter)
	}
	_ = c.Get(ctx, web01Key, n)
	if cond := meta.FindStatusCondition(n.Status.Conditions, ConditionPaused); cond == nil || cond.Reason != pausedByCluster {
		t.Errorf("expected Paused/ClusterPaused, got %+v", cond)
	}

	// Deleted while paused: the finalizer holds, nothing is cleaned.
	if err := c.Delete(ctx, n); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Errorf("no cleanup calls while paused, got %d", f.calls)
	}
	if err := c.Get(ctx, web01Key, n); err != nil {
		t.Fatalf("the VoxNode must wait for unpause before cleanup: %v", err)
	}
	if cond := meta.FindStatusCondition(n.Status.Conditions, ConditionPaused); cond == nil || !strings.Contains(cond.Message, AnnotationSkipCleanup) {
		t.Errorf("the Paused condition must name the skip-cleanup escape hatch, got %+v", cond)
	}

	// Unpause: cleanup runs and the VoxNode goes.
	setAnnotation(t, c, &openvoxv1alpha1.VoxCluster{}, types.NamespacedName{Namespace: "openvox", Name: "openvox"}, "")
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	if f.cleans == 0 || f.deactivates == 0 {
		t.Errorf("cleanup must run after unpause (cleans %d, deactivates %d)", f.cleans, f.deactivates)
	}
	if err := c.Get(ctx, web01Key, n); !apierrors.IsNotFound(err) {
		t.Errorf("expected the VoxNode to be gone, got %v", err)
	}
}

func TestPausedVoxNodeSkipCleanupReleases(t *testing.T) {
	ctx := context.Background()
	node := testNode(openvoxv1alpha1.NodeStateActive)
	now := metav1.Now()
	node.DeletionTimestamp = &now
	node.Finalizers = []string{VoxNodeFinalizer}
	node.Annotations = map[string]string{AnnotationPaused: "true", AnnotationSkipCleanup: "true"}
	r, c, f := newLifecycleFixture(t, node, true)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Errorf("skip-cleanup must not call the CA or DB, got %d calls", f.calls)
	}
	if err := c.Get(ctx, web01Key, &openvoxv1alpha1.VoxNode{}); !apierrors.IsNotFound(err) {
		t.Errorf("skip-cleanup must release a paused VoxNode at once, got %v", err)
	}
}

func TestVoxDiscoveryPausedByCluster(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	cluster := &openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx",
		Annotations: map[string]string{AnnotationPaused: "true"}}}
	disc := &openvoxv1alpha1.VoxDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "ovx"},
		Spec:       openvoxv1alpha1.VoxDiscoverySpec{ClusterRef: "prod"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, disc).WithStatusSubresource(&openvoxv1alpha1.VoxDiscovery{}).Build()
	db := &countingDB{}
	r := &VoxDiscoveryReconciler{Client: c, Scheme: s, DBClient: db}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(disc)})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 0 || db.queries != 0 {
		t.Errorf("paused discovery: requeue %v, DB calls %d; want none", res.RequeueAfter, db.queries)
	}
	got := &openvoxv1alpha1.VoxDiscovery{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(disc), got)
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionPaused); cond == nil || cond.Reason != pausedByCluster {
		t.Errorf("expected Paused/ClusterPaused, got %+v", got.Status.Conditions)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "nodes"}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Errorf("a paused discovery must not create its Service, got %v", err)
	}
}

func TestPauseCascadesThroughPredicate(t *testing.T) {
	o := &openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}
	n := o.DeepCopy()
	n.Annotations = map[string]string{AnnotationPaused: "true"}
	if !voxClusterChangedForDependents().Update(event.UpdateEvent{ObjectOld: o, ObjectNew: n}) {
		t.Error("pausing a VoxCluster must reach its VoxNodes and VoxDiscoveries")
	}
	if !voxClusterChangedForDependents().Update(event.UpdateEvent{ObjectOld: n, ObjectNew: o}) {
		t.Error("unpausing a VoxCluster must reach its VoxNodes and VoxDiscoveries")
	}
}
