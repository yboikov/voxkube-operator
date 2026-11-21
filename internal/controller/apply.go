package controller

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// AnnotationAppliedHash records the hash of what the operator last applied to an unstructured object (Route,
// cert-manager Certificate, ServiceMonitor). Comparing against it, instead of against the live object, keeps
// fields the API server or another controller fills in from causing an update on every reconcile.
const AnnotationAppliedHash = "voxkube.io/applied-hash"

// appliedHash hashes the parts of desired the operator owns.
func appliedHash(desired *unstructured.Unstructured) string {
	b, _ := json.Marshal(map[string]interface{}{
		"spec":        desired.Object["spec"],
		"labels":      desired.GetLabels(),
		"annotations": desired.GetAnnotations(),
		"owners":      desired.GetOwnerReferences(),
	})
	return hashBytes(b)
}

// stampAppliedHash marks desired with its applied hash before it is created.
func stampAppliedHash(desired *unstructured.Unstructured) {
	h := appliedHash(desired)
	desired.SetAnnotations(mergeStringMaps(desired.GetAnnotations(), map[string]string{AnnotationAppliedHash: h}))
}

// prepareUnstructuredUpdate readies desired to replace existing and reports whether an update is needed.
// It returns false when existing already carries what desired describes. Otherwise it keeps the labels and
// annotations other controllers set (ours win on conflicts), keeps a Route's generated spec.host when desired
// sets none, and copies the resourceVersion.
func prepareUnstructuredUpdate(existing, desired *unstructured.Unstructured) bool {
	h := appliedHash(desired)
	if existing.GetAnnotations()[AnnotationAppliedHash] == h {
		return false
	}
	if desired.GetKind() == "Route" {
		if host, _, _ := unstructured.NestedString(desired.Object, "spec", "host"); host == "" {
			if live, _, _ := unstructured.NestedString(existing.Object, "spec", "host"); live != "" {
				_ = unstructured.SetNestedField(desired.Object, live, "spec", "host")
			}
		}
	}
	desired.SetLabels(mergeStringMaps(existing.GetLabels(), desired.GetLabels()))
	annotations := mergeStringMaps(existing.GetAnnotations(), desired.GetAnnotations())
	annotations[AnnotationAppliedHash] = h
	desired.SetAnnotations(annotations)
	desired.SetResourceVersion(existing.GetResourceVersion())
	return true
}

// mergeStringMaps returns a new map with base's entries overridden by over's. It never returns nil.
func mergeStringMaps(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}
