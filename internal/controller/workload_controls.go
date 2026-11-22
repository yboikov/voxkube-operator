package controller

import (
	"context"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// withProbes adds a startup and a liveness probe to c, using the same check as its readiness probe.
//   - Startup: up to 5 minutes (30 × 10 s) for a cold JVM/JRuby boot before liveness starts counting.
//   - Liveness: restart only after 2 minutes of failures (6 × 20 s), so long compiles or GC pauses never trigger it.
func withProbes(c *corev1.Container) {
	if c.ReadinessProbe == nil {
		return
	}
	handler := *c.ReadinessProbe.ProbeHandler.DeepCopy()
	c.StartupProbe = &corev1.Probe{
		ProbeHandler: handler, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 30,
	}
	c.LivenessProbe = &corev1.Probe{
		ProbeHandler: *handler.DeepCopy(), PeriodSeconds: 20, TimeoutSeconds: 10, FailureThreshold: 6,
	}
}

// applyPlacement copies spec.placement into a pod spec. defaultAffinity applies only when the user sets no
// affinity of their own.
func applyPlacement(spec *corev1.PodSpec, cluster *openvoxv1alpha1.VoxCluster, defaultAffinity *corev1.Affinity) {
	p := cluster.Spec.Placement.DeepCopy()
	spec.NodeSelector = p.NodeSelector
	spec.Tolerations = p.Tolerations
	spec.ImagePullSecrets = p.ImagePullSecrets
	spec.Affinity = p.Affinity
	if spec.Affinity == nil {
		spec.Affinity = defaultAffinity
	}
}

// compilerAntiAffinity makes compilers prefer different nodes (soft), so a node failure or drain takes out at
// most one of them where the cluster has room.
func compilerAntiAffinity(cluster *openvoxv1alpha1.VoxCluster) *corev1.Affinity {
	return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
			Weight: 100,
			PodAffinityTerm: corev1.PodAffinityTerm{
				TopologyKey:   corev1.LabelHostname,
				LabelSelector: &metav1.LabelSelector{MatchLabels: voxClusterLabels(cluster.Name, "server")},
			},
		}},
	}}
}

// pdbEnabled is true unless the spec switches the PodDisruptionBudget off.
func pdbEnabled(spec *openvoxv1alpha1.PodDisruptionBudgetSpec) bool {
	return spec == nil || spec.Enabled == nil || *spec.Enabled
}

// reconcilePDB keeps a PodDisruptionBudget (maxUnavailable 1) for a component that runs at least two replicas,
// and deletes it otherwise: with a single replica it would block every node drain.
func (r *VoxClusterReconciler) reconcilePDB(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, component string,
	minReplicas int32, enabled bool) error {
	name := fmt.Sprintf("%s-%s", cluster.Name, component)
	existing := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, existing)
	found := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if !enabled || minReplicas < 2 {
		if found && metav1.IsControlledBy(existing, cluster) {
			return client0IgnoreNotFound(r.Delete(ctx, existing))
		}
		return nil
	}
	one := intstr.FromInt32(1)
	desired := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace, Labels: voxClusterLabels(cluster.Name, component)},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &one,
			Selector:       &metav1.LabelSelector{MatchLabels: voxClusterLabels(cluster.Name, component)},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	if !found {
		return r.Create(ctx, desired)
	}
	if reflect.DeepEqual(existing.Spec.MaxUnavailable, desired.Spec.MaxUnavailable) &&
		reflect.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) && reflect.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	existing.Spec.MaxUnavailable = desired.Spec.MaxUnavailable
	existing.Spec.MinAvailable = nil
	existing.Spec.Selector = desired.Spec.Selector
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func client0IgnoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
