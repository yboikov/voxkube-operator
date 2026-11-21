package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func TestDuplicateCertnameIsInertAndDeletedWithoutCleanup(t *testing.T) {
	ctx := context.Background()
	owner := testNode(openvoxv1alpha1.NodeStateActive)
	owner.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	r, c, f := newLifecycleFixture(t, owner, true)

	dup := testNode(openvoxv1alpha1.NodeStateDecommissioned) // would clean the CA if it were the owner
	dup.Name = "web01-copy"
	dup.CreationTimestamp = metav1.Now()
	if err := c.Create(ctx, dup); err != nil {
		t.Fatal(err)
	}
	dupKey := types.NamespacedName{Namespace: "openvox", Name: "web01-copy"}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: dupKey}); err != nil {
		t.Fatal(err)
	}
	got := &openvoxv1alpha1.VoxNode{}
	if err := c.Get(ctx, dupKey, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != openvoxv1alpha1.NodePhaseError || got.Status.Conditions[0].Reason != "DuplicateCertname" {
		t.Errorf("expected DuplicateCertname, got %s / %+v", got.Status.Phase, got.Status.Conditions)
	}
	if f.cleans != 0 || f.deactivates != 0 {
		t.Fatalf("a duplicate must never touch the CA or OpenVox DB, got %d cleans / %d deactivations", f.cleans, f.deactivates)
	}

	// Deleting the duplicate releases it without revoking the owner's machine.
	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: dupKey}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, dupKey, &openvoxv1alpha1.VoxNode{}); !apierrors.IsNotFound(err) {
		t.Errorf("the duplicate should be gone, got %v", err)
	}
	if f.cleans != 0 || f.deactivates != 0 {
		t.Errorf("deleting a duplicate must not clean the certificate, got %d cleans / %d deactivations", f.cleans, f.deactivates)
	}
}

func TestServedVoxNodesPicksTheOldestPerCertname(t *testing.T) {
	cluster := &openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox"}}
	older := *testNode(openvoxv1alpha1.NodeStateActive)
	older.Name = "b-older"
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	older.Spec.Classification.Role = "web"
	newer := *testNode(openvoxv1alpha1.NodeStateActive)
	newer.Name = "a-newer"
	newer.CreationTimestamp = metav1.Now()
	newer.Spec.Classification.Role = "db"
	reserved := *testNode(openvoxv1alpha1.NodeStateActive)
	reserved.Name = "svc"
	reserved.Spec.Certname = "openvox.openvox.svc"

	for _, order := range [][]openvoxv1alpha1.VoxNode{{older, newer, reserved}, {reserved, newer, older}} {
		got := servedVoxNodes(order, cluster)
		if len(got) != 1 || got[0].Name != "b-older" {
			names := []string{}
			for _, n := range got {
				names = append(names, n.Name)
			}
			t.Errorf("expected only the oldest owner b-older, got %v", names)
		}
	}
}

func TestReservedCertnameIsNeverSigned(t *testing.T) {
	ctx := context.Background()
	node := testNode(openvoxv1alpha1.NodeStateActive)
	node.Spec.Certname = "openvox.openvox.svc"
	r, c, _ := newLifecycleFixture(t, node, true)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: web01Key}); err != nil {
		t.Fatal(err)
	}
	got := &openvoxv1alpha1.VoxNode{}
	if err := c.Get(ctx, web01Key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Conditions[0].Reason != "ReservedCertname" || got.Status.CertificateSigned {
		t.Errorf("expected ReservedCertname and no signing, got %+v", got.Status)
	}
	for _, name := range []string{"openvox.openvox.svc", "openvox-db.openvox.svc", "x.y.svc.cluster.local"} {
		if !reservedCertname(&openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox"}}, name) {
			t.Errorf("%s must be reserved", name)
		}
	}
	if reservedCertname(&openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox"}}, "web01.example.com") {
		t.Error("a normal FQDN is not reserved")
	}
}
