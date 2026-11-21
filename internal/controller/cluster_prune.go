package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// prunedObject names an object a VoxCluster creates for a component that can be switched off.
type prunedObject struct {
	name string
	obj  client.Object
}

func builtinPostgresObjects(c string) []prunedObject {
	return []prunedObject{
		{c + "-postgres", &appsv1.StatefulSet{}},
		{c + "-postgres", &corev1.Service{}},
		{c + "-postgres-init", &corev1.ConfigMap{}},
		{c + "-postgres-credentials", &corev1.Secret{}},
	}
}

func databaseObjects(c string) []prunedObject {
	return append([]prunedObject{
		{c + "-db", &appsv1.Deployment{}},
		{c + "-db", &corev1.Service{}},
		{c + "-db-config", &corev1.ConfigMap{}},
		{c + "-db-database", &corev1.Secret{}},
	}, builtinPostgresObjects(c)...)
}

// caObjects are the CA workload objects. The CA data PVC and the <cluster>-ca-tls Secret are deliberately absent:
// they hold the CA itself and must survive switching the CA off.
func caObjects(c string) []prunedObject {
	return []prunedObject{
		{c + "-ca", &appsv1.StatefulSet{}},
		{c + "-ca", &corev1.Service{}},
		{c + "-ca-config", &corev1.ConfigMap{}},
	}
}

// pruneDisabledComponents deletes what a component left behind after it was switched off: OpenVox DB
// (database.enabled=false), the built-in PostgreSQL (postgres.mode external), the CA workload (ca.enabled=false)
// and the Hiera Secret (hieraSecrets.enabled=false). Only objects controlled by this VoxCluster are deleted.
func (r *VoxClusterReconciler) pruneDisabledComponents(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) error {
	c := cluster.Name
	var prune []prunedObject
	switch {
	case !databaseEnabled(cluster):
		prune = append(prune, databaseObjects(c)...)
	case cluster.Spec.Database.Postgres.Mode != "" && cluster.Spec.Database.Postgres.Mode != "builtin":
		prune = append(prune, builtinPostgresObjects(c)...)
	}
	if !caEnabled(cluster) {
		prune = append(prune, caObjects(c)...)
	}
	if cluster.Spec.HieraSecrets.Enabled != nil && !*cluster.Spec.HieraSecrets.Enabled {
		prune = append(prune, prunedObject{c + "-hiera-secrets", &corev1.Secret{}})
	}
	for _, p := range prune {
		if err := r.deleteIfControlled(ctx, cluster, p.name, p.obj); err != nil {
			return err
		}
	}
	return nil
}

func (r *VoxClusterReconciler) deleteIfControlled(ctx context.Context, owner metav1.Object, name string, obj client.Object) error {
	if err := r.Get(ctx, types.NamespacedName{Namespace: owner.GetNamespace(), Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(obj, owner) {
		return nil
	}
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting %T %s: %w", obj, name, err)
	}
	log.FromContext(ctx).Info("deleted an object of a disabled component", "kind", fmt.Sprintf("%T", obj), "name", name)
	return nil
}
