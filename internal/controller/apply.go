package controller

import (
	"encoding/json"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// AnnotationAppliedHash records the hash of what the operator last applied to an unstructured object (Route,
// cert-manager Certificate, ServiceMonitor). Comparing against it, instead of against the live object, keeps
// fields the API server or another controller fills in from causing an update on every reconcile.
const AnnotationAppliedHash = "voxkube.io/applied-hash"

// AnnotationAppliedLabels and AnnotationAppliedAnnotations list the label and annotation keys the operator set last
// time, so a key removed from the spec (e.g. from openShiftRoute.annotations) is removed from the object, while keys
// set by others are kept.
const (
	AnnotationAppliedLabels      = "voxkube.io/applied-labels"
	AnnotationAppliedAnnotations = "voxkube.io/applied-annotations"
)

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

// stampAppliedHash marks desired with its applied hash and owned keys before it is created.
func stampAppliedHash(desired *unstructured.Unstructured) {
	desired.SetAnnotations(mergeStringMaps(desired.GetAnnotations(), appliedBookkeeping(desired)))
}

// appliedBookkeeping returns the bookkeeping annotations for desired (computed before they are added).
func appliedBookkeeping(desired *unstructured.Unstructured) map[string]string {
	return map[string]string{
		AnnotationAppliedHash:        appliedHash(desired),
		AnnotationAppliedLabels:      joinKeys(desired.GetLabels()),
		AnnotationAppliedAnnotations: joinKeys(desired.GetAnnotations()),
	}
}

func joinKeys(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// mergeOwned returns live with the keys the operator set last time (previous, comma-separated) removed, overlaid
// with desired: keys dropped from desired disappear, keys set by others survive.
func mergeOwned(live, desired map[string]string, previous string) map[string]string {
	out := make(map[string]string, len(live)+len(desired))
	for k, v := range live {
		out[k] = v
	}
	if previous != "" {
		for _, k := range strings.Split(previous, ",") {
			delete(out, k)
		}
	}
	for k, v := range desired {
		out[k] = v
	}
	return out
}

// prepareUnstructuredUpdate readies desired to replace existing and reports whether an update is needed.
// It returns false when existing already carries what desired describes. Otherwise it keeps the labels,
// annotations and finalizers others set (ours win on conflicts; keys we set before and no longer want are
// removed), keeps a Route's generated spec.host when desired sets none, and copies the resourceVersion.
func prepareUnstructuredUpdate(existing, desired *unstructured.Unstructured) bool {
	bookkeeping := appliedBookkeeping(desired)
	if existing.GetAnnotations()[AnnotationAppliedHash] == bookkeeping[AnnotationAppliedHash] {
		return false
	}
	if desired.GetKind() == "Route" {
		if host, _, _ := unstructured.NestedString(desired.Object, "spec", "host"); host == "" {
			if live, _, _ := unstructured.NestedString(existing.Object, "spec", "host"); live != "" {
				_ = unstructured.SetNestedField(desired.Object, live, "spec", "host")
			}
		}
	}
	prev := existing.GetAnnotations()
	desired.SetLabels(mergeOwned(existing.GetLabels(), desired.GetLabels(), prev[AnnotationAppliedLabels]))
	desired.SetAnnotations(mergeStringMaps(
		mergeOwned(existing.GetAnnotations(), desired.GetAnnotations(), prev[AnnotationAppliedAnnotations]), bookkeeping))
	desired.SetFinalizers(existing.GetFinalizers())
	owners := desired.GetOwnerReferences()
	for _, ref := range existing.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			continue // ours is in desired
		}
		owners = append(owners, ref)
	}
	desired.SetOwnerReferences(owners)
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
