package controller

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// countingWrites records every create, update, patch and status write made through the client.
func countingWrites(writes *[]string) interceptor.Funcs {
	rec := func(verb string, obj client.Object) {
		*writes = append(*writes, fmt.Sprintf("%s %T %s", verb, obj, obj.GetName()))
	}
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			rec("create", obj)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			rec("update", obj)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			rec("patch", obj)
			return c.Patch(ctx, obj, p, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			rec("update/"+sub, obj)
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
}

func TestSteadyVoxNodeWritesNothing(t *testing.T) {
	ctx := context.Background()
	r, c, f := newLifecycleFixture(t, testNode(openvoxv1alpha1.NodeStateActive), true)
	f.nodeResponse = `{"certname":"web01.example.com","report_timestamp":"2026-11-21T10:00:00.123456Z","latest_report_status":"changed"}`
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	var writes []string
	r.Client = interceptor.NewClient(c.(client.WithWatch), countingWrites(&writes))
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 0 {
		t.Errorf("a VoxNode whose state didn't change must not write anything (incl. ENC/Hiera), got %v", writes)
	}
}

func TestPredicatesDropStatusTicks(t *testing.T) {
	seen := metav1.Now()
	base := testNode(openvoxv1alpha1.NodeStateActive)
	base.Generation = 1
	tick := base.DeepCopy()
	tick.Status.LastSeenTime = &seen
	tick.Status.LastReportStatus = "changed"
	role := base.DeepCopy()
	role.Status.Role = "web"
	label := base.DeepCopy()
	label.Labels = map[string]string{"monitoring.voxkube.io/team": "x"}
	other := base.DeepCopy()
	other.Labels = map[string]string{"app": "x"}
	spec := base.DeepCopy()
	spec.Generation = 2

	upd := func(o, n client.Object) event.UpdateEvent { return event.UpdateEvent{ObjectOld: o, ObjectNew: n} }
	for name, tc := range map[string]struct {
		got, want bool
	}{
		"self: status tick ignored":         {specOrMetaChanged().Update(upd(base, tick)), false},
		"self: spec change passes":          {specOrMetaChanged().Update(upd(base, spec)), true},
		"cluster: check-in tick ignored":    {voxNodeChangedForCluster().Update(upd(base, tick)), false},
		"cluster: role change passes":       {voxNodeChangedForCluster().Update(upd(base, role)), true},
		"cluster: spec change passes":       {voxNodeChangedForCluster().Update(upd(base, spec)), true},
		"discovery: tick ignored":           {voxNodeChangedForDiscovery().Update(upd(base, tick)), false},
		"discovery: monitoring label":       {voxNodeChangedForDiscovery().Update(upd(base, label)), true},
		"discovery: unrelated label":        {voxNodeChangedForDiscovery().Update(upd(base, other)), false},
		"discovery: status role irrelevant": {voxNodeChangedForDiscovery().Update(upd(base, role)), false},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %v, want %v", name, tc.got, tc.want)
		}
	}

	c := &openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Generation: 1}}
	ready := c.DeepCopy()
	ready.Status.ReadyReplicas = 2
	ready.Status.ObservedCommit = "abc"
	ca := c.DeepCopy()
	ca.Status.CAURL = "https://ca"
	if voxClusterChangedForDependents().Update(upd(c, ready)) {
		t.Error("readyReplicas/observedCommit must not wake every VoxNode")
	}
	if !voxClusterChangedForDependents().Update(upd(c, ca)) {
		t.Error("a new CA URL must reach the VoxNodes")
	}
}

func TestMTLSClientCacheReusesClient(t *testing.T) {
	caCert, _, err := GenerateSelfSignedCA("cache test", 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	cache := &mtlsClientCache{entries: map[string]mtlsClientEntry{}}
	a, err := cache.get("ns/c", caCert, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := cache.get("ns/c", caCert, nil, nil)
	if a != b {
		t.Error("unchanged credentials must reuse the client")
	}
	other, _, _ := GenerateSelfSignedCA("rotated", 24*3600*1e9)
	c, _ := cache.get("ns/c", other, nil, nil)
	if c == a {
		t.Error("rotated credentials must build a new client")
	}
	if _, ok := c.Transport.(*http.Transport); !ok {
		t.Error("expected an *http.Transport")
	}
	cache.forget("ns/c")
	if len(cache.entries) != 0 {
		t.Error("forget must drop the entry")
	}
}

func TestHPAUnchangedIsNotUpdated(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	maxR := int32(4)
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox", UID: "u1"},
		Spec: openvoxv1alpha1.VoxClusterSpec{Server: openvoxv1alpha1.ClusterServerSpec{
			Autoscaling: &openvoxv1alpha1.ServerAutoscalingSpec{Enabled: true, MaxReplicas: maxR},
		}},
	}
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).Build()
	r := &VoxClusterReconciler{Client: base, Scheme: s}
	if err := r.reconcileAutoscaling(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	var writes []string
	r.Client = interceptor.NewClient(base, countingWrites(&writes))
	if err := r.reconcileAutoscaling(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 0 {
		t.Errorf("an unchanged HPA must not be updated, got %v", writes)
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := base.Get(ctx, client.ObjectKey{Namespace: "openvox", Name: "openvox-server"}, hpa); err != nil {
		t.Fatal(err)
	}
	if hpa.Spec.MaxReplicas != maxR {
		t.Errorf("unexpected HPA %+v", hpa.Spec)
	}
}

func TestDiscoveryStatusWriteRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	off := false
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx"},
		Spec:       openvoxv1alpha1.VoxClusterSpec{Database: openvoxv1alpha1.ClusterDatabaseSpec{Enabled: &off}},
	}
	disc := &openvoxv1alpha1.VoxDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "ovx"},
		Spec:       openvoxv1alpha1.VoxDiscoverySpec{ClusterRef: "prod"},
	}
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, disc).WithStatusSubresource(&openvoxv1alpha1.VoxDiscovery{}).Build()
	conflicts := 1
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(schema.GroupResource{Resource: "voxdiscoveries"}, obj.GetName(), nil)
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	r := &VoxDiscoveryReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(disc)}); err != nil {
		t.Fatalf("a conflict on the status write must be retried, got %v", err)
	}
	got := &openvoxv1alpha1.VoxDiscovery{}
	if err := base.Get(ctx, client.ObjectKeyFromObject(disc), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Conditions) == 0 || got.Status.Conditions[0].Reason != "DatabaseDisabled" {
		t.Errorf("expected DatabaseDisabled after the retry, got %+v", got.Status.Conditions)
	}
}
