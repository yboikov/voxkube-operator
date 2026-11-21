package controller

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// clustersForSecret enqueues the VoxClusters a changed Secret matters to. Most Secrets in a namespace (Helm
// releases, service account tokens, application credentials) don't, and enqueuing every VoxCluster for each of
// them would re-run the whole stack on every unrelated change. obj is metadata only.
func (r *VoxClusterReconciler) clustersForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	clusters := &openvoxv1alpha1.VoxClusterList{}
	if err := r.List(ctx, clusters, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var nodes *openvoxv1alpha1.VoxNodeList
	var reqs []reconcile.Request
	for i := range clusters.Items {
		c := &clusters.Items[i]
		relevant := secretRelevantToCluster(c, obj)
		if !relevant {
			// VoxNode secrets and cert-manager TLS Secrets end up in the Hiera Secret.
			if nodes == nil {
				nodes = &openvoxv1alpha1.VoxNodeList{}
				if err := r.List(ctx, nodes, client.InNamespace(obj.GetNamespace())); err != nil {
					return nil
				}
			}
			relevant = secretReferencedByNodes(c, nodes.Items, obj.GetName())
		}
		if relevant {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: c.Namespace, Name: c.Name}})
		}
	}
	return reqs
}

// secretRelevantToCluster reports whether a Secret feeds the VoxCluster: it matches the Hiera label selector, is
// named in the spec, or is one of the cluster's own Secrets (<cluster>-*, or controlled by it).
func secretRelevantToCluster(c *openvoxv1alpha1.VoxCluster, obj metav1.Object) bool {
	name := obj.GetName()
	if metav1.IsControlledBy(obj, c) || strings.HasPrefix(name, c.Name+"-") {
		return true
	}
	labels := obj.GetLabels()
	matches := len(labels) > 0
	for k, v := range hieraLabelSelector(c.Spec.HieraSecrets.LabelSelector) {
		if labels[k] != v {
			matches = false
		}
	}
	if matches {
		return true
	}
	for _, ref := range clusterSecretRefs(c) {
		if ref == name {
			return true
		}
	}
	return false
}

// clusterSecretRefs lists the Secret names a VoxCluster spec references.
func clusterSecretRefs(c *openvoxv1alpha1.VoxCluster) []string {
	refs := append([]string{}, c.Spec.HieraSecrets.CommonSecrets...)
	refs = append(refs, c.Spec.CA.PSKSecretRef, c.Spec.Database.Postgres.CredentialsSecretRef, caSecretName(c))
	if g := c.Spec.Code.Git; g != nil {
		refs = append(refs, g.CredentialsSecretRef)
		if g.GitHubApp != nil {
			refs = append(refs, g.GitHubApp.PrivateKeySecretRef.Name)
		}
	}
	return refs
}

// secretReferencedByNodes reports whether a VoxNode of c names the Secret (spec.secrets or a cert-manager
// certificate Secret).
func secretReferencedByNodes(c *openvoxv1alpha1.VoxCluster, nodes []openvoxv1alpha1.VoxNode, name string) bool {
	for i := range nodes {
		n := &nodes[i]
		if n.Spec.ClusterRef != c.Name {
			continue
		}
		for _, s := range n.Spec.Secrets {
			if s.SecretName == name {
				return true
			}
		}
		for _, cert := range n.Spec.Certificates {
			if CertManagerSecretName(n, cert) == name {
				return true
			}
		}
	}
	return false
}
