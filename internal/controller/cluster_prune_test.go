package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func TestPruneDisabledComponents(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	off := false
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox", UID: "c1"},
		Spec: openvoxv1alpha1.VoxClusterSpec{
			Database:     openvoxv1alpha1.ClusterDatabaseSpec{Enabled: &off},
			CA:           openvoxv1alpha1.ClusterCASpec{Enabled: &off},
			HieraSecrets: openvoxv1alpha1.HieraSecretsSpec{Enabled: &off},
		},
	}
	owned := func(o client.Object) client.Object {
		if err := controllerutil.SetControllerReference(cluster, o, s); err != nil {
			t.Fatal(err)
		}
		return o
	}
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "openvox"} }
	objs := []client.Object{
		cluster,
		owned(&appsv1.Deployment{ObjectMeta: meta("openvox-db")}),
		owned(&corev1.Secret{ObjectMeta: meta("openvox-db-database")}),
		owned(&appsv1.StatefulSet{ObjectMeta: meta("openvox-postgres")}),
		owned(&appsv1.StatefulSet{ObjectMeta: meta("openvox-ca")}),
		owned(&corev1.ConfigMap{ObjectMeta: meta("openvox-ca-config")}),
		owned(&corev1.Secret{ObjectMeta: meta("openvox-hiera-secrets")}),
		// Not ours: must survive.
		&corev1.Service{ObjectMeta: meta("openvox-db")},
		// The CA itself is never pruned.
		&corev1.Secret{ObjectMeta: meta("openvox-ca-tls")},
		&corev1.PersistentVolumeClaim{ObjectMeta: meta("openvox-ca-data")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s}
	if err := r.pruneDisabledComponents(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	gone := map[string]client.Object{
		"openvox-db": &appsv1.Deployment{}, "openvox-db-database": &corev1.Secret{}, "openvox-postgres": &appsv1.StatefulSet{},
		"openvox-ca": &appsv1.StatefulSet{}, "openvox-ca-config": &corev1.ConfigMap{}, "openvox-hiera-secrets": &corev1.Secret{},
	}
	for name, obj := range gone {
		if err := c.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: name}, obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s should have been pruned, got %v", obj, name, err)
		}
	}
	kept := map[string]client.Object{
		"openvox-db": &corev1.Service{}, "openvox-ca-tls": &corev1.Secret{}, "openvox-ca-data": &corev1.PersistentVolumeClaim{},
	}
	for name, obj := range kept {
		if err := c.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: name}, obj); err != nil {
			t.Errorf("%T %s must be kept, got %v", obj, name, err)
		}
	}
}

func TestPruneBuiltinPostgresWhenExternal(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox", UID: "c1"},
		Spec: openvoxv1alpha1.VoxClusterSpec{Database: openvoxv1alpha1.ClusterDatabaseSpec{
			Postgres: openvoxv1alpha1.ClusterPostgresSpec{Mode: "external"},
		}},
	}
	pg := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "openvox-postgres", Namespace: "openvox"}}
	db := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "openvox-db", Namespace: "openvox"}}
	for _, o := range []client.Object{pg, db} {
		if err := controllerutil.SetControllerReference(cluster, o, s); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, pg, db).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s}
	if err := r.pruneDisabledComponents(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pg), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Errorf("the built-in PostgreSQL must go when mode is external, got %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(db), &appsv1.Deployment{}); err != nil {
		t.Errorf("OpenVox DB must stay, got %v", err)
	}
}
