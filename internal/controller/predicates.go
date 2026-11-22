package controller

import (
	"reflect"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// The predicates below keep status writes from waking controllers up. Without them every status update
// (VoxDiscovery's lastSyncTime, a VoxNode's lastSeenTime, a VoxCluster's readyReplicas) re-triggered the
// object's own controller and every controller watching it, so a single Puppet run could reconcile the
// whole VoxCluster stack and VoxDiscovery ran in a loop.

// specOrMetaChanged passes creates, deletes and updates that change the spec (generation), the deletion
// timestamp, labels, annotations or finalizers; status-only updates are dropped.
func specOrMetaChanged() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		o, n := e.ObjectOld, e.ObjectNew
		return o.GetGeneration() != n.GetGeneration() ||
			!o.GetDeletionTimestamp().Equal(n.GetDeletionTimestamp()) ||
			!reflect.DeepEqual(o.GetLabels(), n.GetLabels()) ||
			!reflect.DeepEqual(o.GetAnnotations(), n.GetAnnotations()) ||
			!reflect.DeepEqual(o.GetFinalizers(), n.GetFinalizers())
	}}
}

// voxNodeChangedForCluster passes the VoxNode changes that alter what the VoxCluster renders (ENC, Hiera,
// auto-creation): spec, deletion, the role and certificate extensions in status. Check-in ticks
// (lastSeenTime, lastReportStatus) are dropped.
func voxNodeChangedForCluster() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok1 := e.ObjectOld.(*openvoxv1alpha1.VoxNode)
		n, ok2 := e.ObjectNew.(*openvoxv1alpha1.VoxNode)
		if !ok1 || !ok2 {
			return true
		}
		return o.Generation != n.Generation ||
			!o.DeletionTimestamp.Equal(n.DeletionTimestamp) ||
			o.Status.Role != n.Status.Role ||
			!reflect.DeepEqual(o.Status.CertificateExtensions, n.Status.CertificateExtensions)
	}}
}

// voxNodeChangedForDiscovery passes the VoxNode changes that alter discovery targets: spec (state, certname,
// clusterRef), deletion and monitoring.voxkube.io/* labels.
func voxNodeChangedForDiscovery() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		o, n := e.ObjectOld, e.ObjectNew
		return o.GetGeneration() != n.GetGeneration() ||
			!o.GetDeletionTimestamp().Equal(n.GetDeletionTimestamp()) ||
			!reflect.DeepEqual(monitoringLabels(o.GetLabels()), monitoringLabels(n.GetLabels()))
	}}
}

func monitoringLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if strings.HasPrefix(k, voxNodeMonitoringLabelPrefix) {
			out[k] = v
		}
	}
	return out
}

// voxClusterChangedForDependents passes the VoxCluster changes VoxNodes and VoxDiscoveries depend on: spec,
// deletion, pausing (voxkube.io/paused cascades to them), and the endpoints/Secret recorded in status.
// readyReplicas, observedCommit and conditions are dropped.
func voxClusterChangedForDependents() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok1 := e.ObjectOld.(*openvoxv1alpha1.VoxCluster)
		n, ok2 := e.ObjectNew.(*openvoxv1alpha1.VoxCluster)
		if !ok1 || !ok2 {
			return true
		}
		return o.Generation != n.Generation ||
			!o.DeletionTimestamp.Equal(n.DeletionTimestamp) ||
			annotatedPaused(o) != annotatedPaused(n) ||
			o.Status.CAURL != n.Status.CAURL ||
			o.Status.DatabaseURL != n.Status.DatabaseURL ||
			o.Status.ServerURL != n.Status.ServerURL ||
			o.Status.CASecretName != n.Status.CASecretName
	}}
}
