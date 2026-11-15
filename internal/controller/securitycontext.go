package controller

import (
	corev1 "k8s.io/api/core/v1"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// buildOpenShiftPodSecurityContext returns a 100% OpenShift restricted-v2 SCC
// compliant PodSecurityContext by default: RunAsUser, RunAsGroup, and FSGroup
// are left nil so OpenShift's admission controller automatically injects the
// namespace's assigned UID/GID range without rejecting the Pod.
func buildOpenShiftPodSecurityContext(override *openvoxv1alpha1.PodSecurityContextSpec) *corev1.PodSecurityContext {
	psc := &corev1.PodSecurityContext{
		RunAsNonRoot: new(true),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}

	if override == nil {
		return psc
	}
	if override.RunAsUser != nil {
		psc.RunAsUser = override.RunAsUser
	}
	if override.RunAsGroup != nil {
		psc.RunAsGroup = override.RunAsGroup
	}
	if override.RunAsNonRoot != nil {
		psc.RunAsNonRoot = override.RunAsNonRoot
	}
	if override.FSGroup != nil {
		psc.FSGroup = override.FSGroup
	}
	if override.FSGroupChangePolicy != nil {
		psc.FSGroupChangePolicy = override.FSGroupChangePolicy
	}
	return psc
}

// buildRestrictedContainerSecurityContext returns a hardened Container SecurityContext
// that satisfies both OpenShift restricted-v2 SCC and Kubernetes Pod Security Standards (restricted).
func buildRestrictedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		RunAsNonRoot:             new(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}
