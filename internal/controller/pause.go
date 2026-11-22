package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	// AnnotationPaused set to "true" on a VoxCluster, VoxNode or VoxDiscovery stops the operator from acting on it:
	// no writes to child objects, no CA or OpenVox DB calls, no periodic requeue. A paused VoxCluster pauses its
	// VoxNodes and VoxDiscoveries too. Removing the annotation resumes them.
	AnnotationPaused = "voxkube.io/paused"
	// ConditionPaused is True while an object is paused; it is removed when reconciling resumes.
	ConditionPaused = "Paused"

	pausedByAnnotation = "AnnotationSet"
	pausedByCluster    = "ClusterPaused"
)

func annotatedPaused(o metav1.Object) bool {
	return o.GetAnnotations()[AnnotationPaused] == "true"
}

// pauseReason reports why a VoxNode or VoxDiscovery is paused: its own annotation, or that of its VoxCluster.
// Empty means it isn't.
func pauseReason(own metav1.Object, cluster *openvoxv1alpha1.VoxCluster, clusterFound bool) string {
	switch {
	case annotatedPaused(own):
		return pausedByAnnotation
	case clusterFound && annotatedPaused(cluster):
		return pausedByCluster
	}
	return ""
}

// setPaused sets the Paused condition and reports whether the object was not paused before (for a single event).
func setPaused(conds *[]metav1.Condition, reason, msg string, generation int64) bool {
	prev := meta.FindStatusCondition(*conds, ConditionPaused)
	meta.SetStatusCondition(conds, metav1.Condition{
		Type: ConditionPaused, Status: metav1.ConditionTrue, ObservedGeneration: generation, Reason: reason, Message: msg,
	})
	return prev == nil || prev.Status != metav1.ConditionTrue
}

func pausedMessage(reason, kind string) string {
	if reason == pausedByCluster {
		return "The VoxCluster has " + AnnotationPaused + "=\"true\": this " + kind + " makes no changes and no CA or OpenVox DB calls until it is removed"
	}
	return AnnotationPaused + "=\"true\" is set: the operator makes no changes for this " + kind + " until it is removed"
}
