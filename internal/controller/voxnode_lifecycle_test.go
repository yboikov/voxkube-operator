package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// fakeVoxServer is a CA + OpenVox DB mock whose clean/deactivate calls can be made to fail.
type fakeVoxServer struct {
	mu           sync.Mutex
	caDown       bool
	dbDown       bool
	cleans       int
	deactivates  int
	calls        int
	nodeResponse string
}

func (f *fakeVoxServer) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	switch {
	case r.URL.Path == "/puppet-ca/v1/clean":
		f.cleans++
		if f.caDown {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/pdb/cmd/v1":
		f.deactivates++
		if f.dbDown {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"uuid":"x"}`))
	case strings.HasPrefix(r.URL.Path, "/puppet-ca/v1/certificate_status/"):
		_, _ = w.Write([]byte(`{"name":"web01.example.com","state":"signed","fingerprint":"AA:BB","not_after":"2031-02-23T21:06:52UTC"}`))
	case strings.HasPrefix(r.URL.Path, "/pdb/query/v4/nodes/") && f.nodeResponse != "":
		_, _ = w.Write([]byte(f.nodeResponse))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newLifecycleFixture(t *testing.T, node *openvoxv1alpha1.VoxNode, withCluster bool) (*VoxNodeReconciler, client.Client, *fakeVoxServer) {
	t.Helper()
	f := &fakeVoxServer{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	s := newVoxKubeScheme(t)
	objs := []client.Object{node}
	if withCluster {
		objs = append(objs, &openvoxv1alpha1.VoxCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox"},
			Status:     openvoxv1alpha1.VoxClusterStatus{ServerURL: srv.URL, CAURL: srv.URL, DatabaseURL: srv.URL},
		})
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&openvoxv1alpha1.VoxNode{}).Build()
	r := &VoxNodeReconciler{Client: c, Scheme: s, CAClient: NewHTTPCAClient(srv.Client()), DBClient: NewHTTPPuppetDBClient(srv.Client())}
	return r, c, f
}

func testNode(state openvoxv1alpha1.VoxNodeState) *openvoxv1alpha1.VoxNode {
	return &openvoxv1alpha1.VoxNode{
		ObjectMeta: metav1.ObjectMeta{Name: "web01", Namespace: "openvox"},
		Spec:       openvoxv1alpha1.VoxNodeSpec{ClusterRef: "openvox", Certname: "web01.example.com", State: state},
	}
}

var web01Key = types.NamespacedName{Namespace: "openvox", Name: "web01"}

func TestVoxNodeFinalizerWaitsForCleanup(t *testing.T) {
	ctx := context.Background()
	node := testNode(openvoxv1alpha1.NodeStateActive)
	now := metav1.Now()
	node.DeletionTimestamp = &now
	node.Finalizers = []string{VoxNodeFinalizer}
	r, c, f := newLifecycleFixture(t, node, true)
	f.caDown = true

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Error("a failed CA clean must be retried")
	}
	n := &openvoxv1alpha1.VoxNode{}
	if err := c.Get(ctx, web01Key, n); err != nil {
		t.Fatalf("the VoxNode must stay while the CA clean fails: %v", err)
	}
	if n.Status.CACleaned || !n.Status.PuppetDBDeactivated {
		t.Errorf("expected only the DB step to be recorded as done, got %+v", n.Status)
	}
	if cond := n.Status.Conditions; len(cond) == 0 || cond[0].Reason != "CleanupPending" ||
		!strings.Contains(cond[0].Message, AnnotationSkipCleanup) {
		t.Errorf("expected a CleanupPending condition naming the escape hatch, got %+v", cond)
	}

	f.caDown = false
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, web01Key, n); !apierrors.IsNotFound(err) {
		t.Fatalf("expected the VoxNode to be gone after a successful cleanup, got %v", err)
	}
	if f.deactivates != 1 {
		t.Errorf("a step that already succeeded must not be repeated, got %d deactivations", f.deactivates)
	}
}

func TestVoxNodeFinalizerEscapeHatches(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	node := testNode(openvoxv1alpha1.NodeStateActive)
	node.DeletionTimestamp = &now
	node.Finalizers = []string{VoxNodeFinalizer}
	node.Annotations = map[string]string{AnnotationSkipCleanup: "true"}
	r, c, f := newLifecycleFixture(t, node, true)
	f.caDown, f.dbDown = true, true
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, web01Key, &openvoxv1alpha1.VoxNode{}); !apierrors.IsNotFound(err) {
		t.Errorf("skip-cleanup must release the VoxNode, got %v", err)
	}
	if f.cleans != 0 || f.deactivates != 0 {
		t.Errorf("skip-cleanup must not call the CA or OpenVox DB, got %d/%d", f.cleans, f.deactivates)
	}

	orphan := testNode(openvoxv1alpha1.NodeStateActive)
	orphan.DeletionTimestamp = &now
	orphan.Finalizers = []string{VoxNodeFinalizer}
	r, c, _ = newLifecycleFixture(t, orphan, false)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, web01Key, &openvoxv1alpha1.VoxNode{}); !apierrors.IsNotFound(err) {
		t.Errorf("a VoxNode whose VoxCluster is gone must be released, got %v", err)
	}
}

func TestVoxNodeDecommissionRecordsOnlySuccess(t *testing.T) {
	ctx := context.Background()
	node := testNode(openvoxv1alpha1.NodeStateDecommissioned)
	seen := metav1.Now()
	node.Status = openvoxv1alpha1.VoxNodeStatus{
		LastSeenTime: &seen, LastReportStatus: "failed", CertificateFingerprint: "AA:BB",
		CertificateExtensions: map[string]string{"pp_role": "web"},
	}
	r, c, f := newLifecycleFixture(t, node, true)
	f.dbDown = true

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key})
	if err != nil {
		t.Fatal(err)
	}
	n := &openvoxv1alpha1.VoxNode{}
	if err := c.Get(ctx, web01Key, n); err != nil {
		t.Fatal(err)
	}
	if !n.Status.CACleaned || n.Status.PuppetDBDeactivated {
		t.Errorf("only the CA clean succeeded, got CACleaned=%v PuppetDBDeactivated=%v", n.Status.CACleaned, n.Status.PuppetDBDeactivated)
	}
	if n.Status.Conditions[0].Reason != "CleanupPending" || res.RequeueAfter == 0 {
		t.Errorf("expected CleanupPending with a retry, got %s / %s", n.Status.Conditions[0].Reason, res.RequeueAfter)
	}
	if n.Status.LastSeenTime != nil || n.Status.LastReportStatus != "" || n.Status.CertificateFingerprint != "" ||
		n.Status.CertificateExtensions != nil || n.Status.Role != "" {
		t.Errorf("a decommissioned node must not keep stale check-in or certificate data, got %+v", n.Status)
	}

	f.dbDown = false
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Get(ctx, web01Key, n); err != nil {
		t.Fatal(err)
	}
	if !n.Status.PuppetDBDeactivated || n.Status.Conditions[0].Reason != "NodeDecommissioned" {
		t.Errorf("expected NodeDecommissioned, got %+v", n.Status)
	}
	if f.cleans != 1 || f.deactivates != 2 {
		t.Errorf("finished steps must not be repeated: cleans=%d deactivates=%d", f.cleans, f.deactivates)
	}
}

func TestVoxNodeDeactivatedInDatabase(t *testing.T) {
	ctx := context.Background()
	r, c, f := newLifecycleFixture(t, testNode(openvoxv1alpha1.NodeStateActive), true)
	f.nodeResponse = `{"certname":"web01.example.com","deactivated":"2026-11-01T10:00:00Z",` +
		`"report_timestamp":"2026-10-31T10:00:00Z","latest_report_status":"changed"}`
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	n := &openvoxv1alpha1.VoxNode{}
	if err := c.Get(ctx, web01Key, n); err != nil {
		t.Fatal(err)
	}
	if n.Status.Phase == openvoxv1alpha1.NodePhaseActive || n.Status.Conditions[0].Reason != "NodeDeactivated" {
		t.Errorf("a deactivated node must not be reported Active, got %s / %s", n.Status.Phase, n.Status.Conditions[0].Reason)
	}
}

func TestVoxNodePendingApprovalRequeues(t *testing.T) {
	r, _, _ := newLifecycleFixture(t, testNode(openvoxv1alpha1.NodeStatePendingApproval), true)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: web01Key})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != nodeRequeuePending {
		t.Errorf("expected a %s poll for pending approval, got %s", nodeRequeuePending, res.RequeueAfter)
	}
}

func TestNodeServedByExcludesRevokedAndDeleting(t *testing.T) {
	cluster := &openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "openvox"}}
	now := metav1.Now()
	deleting := testNode(openvoxv1alpha1.NodeStateActive)
	deleting.DeletionTimestamp = &now
	other := testNode(openvoxv1alpha1.NodeStateActive)
	other.Spec.ClusterRef = "other"
	for name, tc := range map[string]struct {
		node *openvoxv1alpha1.VoxNode
		want bool
	}{
		"active":         {testNode(openvoxv1alpha1.NodeStateActive), true},
		"empty state":    {testNode(""), true},
		"pending":        {testNode(openvoxv1alpha1.NodeStatePendingApproval), true},
		"revoked":        {testNode(openvoxv1alpha1.NodeStateRevoked), false},
		"decommissioned": {testNode(openvoxv1alpha1.NodeStateDecommissioned), false},
		"deleting":       {deleting, false},
		"other cluster":  {other, false},
	} {
		if got := nodeServedBy(tc.node, cluster); got != tc.want {
			t.Errorf("%s: nodeServedBy = %v, want %v", name, got, tc.want)
		}
	}
}

func TestPrepareUnstructuredUpdate(t *testing.T) {
	desired := func() *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "route.openshift.io/v1", "kind": "Route",
			"metadata": map[string]interface{}{"name": "r", "namespace": "ns"},
			"spec":     map[string]interface{}{"to": map[string]interface{}{"kind": "Service", "name": "svc"}},
		}}
		return u
	}
	created := desired()
	stampAppliedHash(created)
	live := created.DeepCopy()
	_ = unstructured.SetNestedField(live.Object, "r-ns.apps.example.com", "spec", "host")
	live.SetAnnotations(mergeStringMaps(live.GetAnnotations(), map[string]string{"haproxy.router.openshift.io/timeout": "5m"}))
	live.SetResourceVersion("7")

	if prepareUnstructuredUpdate(live, desired()) {
		t.Error("an unchanged object must not be updated")
	}

	changed := desired()
	_ = unstructured.SetNestedField(changed.Object, "other", "spec", "to", "name")
	if !prepareUnstructuredUpdate(live, changed) {
		t.Fatal("a changed spec must be updated")
	}
	if host, _, _ := unstructured.NestedString(changed.Object, "spec", "host"); host != "r-ns.apps.example.com" {
		t.Errorf("the generated Route host must be kept, got %q", host)
	}
	if changed.GetAnnotations()["haproxy.router.openshift.io/timeout"] != "5m" || changed.GetResourceVersion() != "7" {
		t.Errorf("foreign annotations and the resourceVersion must be kept, got %v / %s", changed.GetAnnotations(), changed.GetResourceVersion())
	}
}

// A VoxNode with a reserved certname (ReservedCertname) was never signed: deleting it must not clean the cluster's
// own service certname on the CA or deactivate it in OpenVox DB, paused or not (#67).
func TestDeletingReservedCertnameVoxNodeMakesNoCalls(t *testing.T) {
	ctx := context.Background()
	for _, paused := range []bool{false, true} {
		for _, certname := range []string{"openvox.openvox.svc", "openvox-db.openvox.svc", "x.default.svc.cluster.local"} {
			node := testNode(openvoxv1alpha1.NodeStateActive)
			node.Spec.Certname = certname
			now := metav1.Now()
			node.DeletionTimestamp = &now
			node.Finalizers = []string{VoxNodeFinalizer}
			if paused {
				node.Annotations = map[string]string{AnnotationPaused: "true"}
			}
			r, c, f := newLifecycleFixture(t, node, true)
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
				t.Fatal(err)
			}
			if f.cleans != 0 || f.deactivates != 0 || f.calls != 0 {
				t.Errorf("%s (paused %v): %d CA/DB calls (clean %d, deactivate %d), want none", certname, paused, f.calls, f.cleans, f.deactivates)
			}
			if err := c.Get(ctx, web01Key, &openvoxv1alpha1.VoxNode{}); !apierrors.IsNotFound(err) {
				t.Errorf("%s (paused %v): the finalizer must be released at once, got %v", certname, paused, err)
			}
		}
	}
}
