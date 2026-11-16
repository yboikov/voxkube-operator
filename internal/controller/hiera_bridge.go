package controller

import (
	"context"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	// LabelHieraSecret marks a Secret (e.g. from ESO) for automatic Hiera injection.
	LabelHieraSecret = "voxkube.io/hiera-secret"
	// LabelHieraScope specifies "common", "role", or "node" scope for a Hiera Secret.
	LabelHieraScope = "voxkube.io/hiera-scope"
	// LabelHieraRole specifies the target role when LabelHieraScope is "role".
	LabelHieraRole = "voxkube.io/hiera-role"
	// LabelHieraNode specifies the target node certname when LabelHieraScope is "node".
	LabelHieraNode = "voxkube.io/hiera-node"
	// AnnotationHieraPrefix optionally prefixes all keys in a labeled Secret when rendered into Hiera.
	AnnotationHieraPrefix = "voxkube.io/hiera-prefix"
)

// RenderHieraSecretsBundle collects ESO and cert-manager Secrets for a VoxCluster and its VoxNodes,
// and returns a map of filename -> YAML content to be stored in the cluster's Hiera Secret.
// Every injected key (including the openvox::certs hash) is added to lookup_options with convert_to: "Sensitive"
// so secrets and TLS private keys never leak in Puppet logs or PuppetDB reports.
func RenderHieraSecretsBundle(
	ctx context.Context,
	k8sClient client.Reader,
	cluster *openvoxv1alpha1.VoxCluster,
	nodes []openvoxv1alpha1.VoxNode,
) (map[string][]byte, error) {
	commonData := make(map[string]interface{})
	roleData := make(map[string]map[string]interface{})
	nodeData := make(map[string]map[string]interface{})

	// 1. Collect explicit CommonSecrets from VoxCluster.Spec.HieraSecrets.CommonSecrets
	for _, secName := range cluster.Spec.HieraSecrets.CommonSecrets {
		sec := &corev1.Secret{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: secName}, sec); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		prefix := sec.Annotations[AnnotationHieraPrefix]
		mergeSecretKeys(commonData, sec, prefix)
	}

	// 2. Collect labeled Secrets in the namespace (ESO ExternalSecrets, etc.)
	secretList := &corev1.SecretList{}
	if err := k8sClient.List(ctx, secretList, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}

	for i := range secretList.Items {
		sec := &secretList.Items[i]
		if !matchesHieraLabelSelector(sec, cluster.Spec.HieraSecrets.LabelSelector) {
			continue
		}
		scope := sec.Labels[LabelHieraScope]
		prefix := sec.Annotations[AnnotationHieraPrefix]

		switch scope {
		case "role":
			role := sec.Labels[LabelHieraRole]
			if role == "" {
				continue
			}
			if roleData[role] == nil {
				roleData[role] = make(map[string]interface{})
			}
			mergeSecretKeys(roleData[role], sec, prefix)
		case "node":
			certname := sec.Labels[LabelHieraNode]
			if certname == "" {
				continue
			}
			if nodeData[certname] == nil {
				nodeData[certname] = make(map[string]interface{})
			}
			mergeSecretKeys(nodeData[certname], sec, prefix)
		default:
			mergeSecretKeys(commonData, sec, prefix)
		}
	}

	// 3. Collect per-VoxNode explicit Secrets and multi-certificate cert-manager TLS Secrets
	for _, node := range nodes {
		if node.Spec.ClusterRef != cluster.Name || node.Spec.State == openvoxv1alpha1.NodeStateDecommissioned {
			continue
		}
		certname := node.Spec.Certname
		if certname == "" {
			continue
		}
		if nodeData[certname] == nil {
			nodeData[certname] = make(map[string]interface{})
		}

		// Explicit ESO / K8s Secrets referenced on the VoxNode
		for _, secRef := range node.Spec.Secrets {
			sec := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: secRef.SecretName}, sec); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, err
			}
			mergeSecretKeys(nodeData[certname], sec, secRef.HieraPrefix)
		}

		// Multiple cert-manager certificates per VM
		if len(node.Spec.Certificates) > 0 {
			certsMap := make(map[string]interface{})
			for _, certReq := range node.Spec.Certificates {
				secName := CertManagerSecretName(&node, certReq)
				sec := &corev1.Secret{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: secName}, sec); err != nil {
					if apierrors.IsNotFound(err) {
						continue
					}
					return nil, err
				}

				tlsCrt := string(sec.Data["tls.crt"])
				tlsKey := string(sec.Data["tls.key"])
				caCrt := string(sec.Data["ca.crt"])

				certEntry := map[string]interface{}{
					"tls_crt": tlsCrt,
					"tls_key": tlsKey,
				}
				if caCrt != "" {
					certEntry["ca_crt"] = caCrt
				}
				if certReq.TargetCertPath != "" {
					certEntry["target_cert_path"] = certReq.TargetCertPath
				}
				if certReq.TargetKeyPath != "" {
					certEntry["target_key_path"] = certReq.TargetKeyPath
				}
				certsMap[certReq.Name] = certEntry

				// Also inject flat Hiera keys for direct lookup('certs::<name>::tls_crt')
				nodeData[certname][fmt.Sprintf("certs::%s::tls_crt", certReq.Name)] = tlsCrt
				nodeData[certname][fmt.Sprintf("certs::%s::tls_key", certReq.Name)] = tlsKey
				if caCrt != "" {
					nodeData[certname][fmt.Sprintf("certs::%s::ca_crt", certReq.Name)] = caCrt
				}
			}
			if len(certsMap) > 0 {
				nodeData[certname]["openvox::certs"] = certsMap
			}
		}
	}

	// Render YAML files with lookup_options marking all secret keys Sensitive
	bundle := make(map[string][]byte)

	commonYAML, err := marshalHieraYAMLWithSensitive(commonData)
	if err != nil {
		return nil, err
	}
	bundle["common.yaml"] = commonYAML

	for role, data := range roleData {
		roleYAML, err := marshalHieraYAMLWithSensitive(data)
		if err != nil {
			return nil, err
		}
		bundle[fmt.Sprintf("roles_%s.yaml", role)] = roleYAML
	}

	for certname, data := range nodeData {
		nodeYAML, err := marshalHieraYAMLWithSensitive(data)
		if err != nil {
			return nil, err
		}
		bundle[fmt.Sprintf("nodes_%s.yaml", certname)] = nodeYAML
	}

	return bundle, nil
}

// CertManagerSecretName returns the deterministic RFC 1123 compliant TLS Secret name for a VoxNode certificate request.
func CertManagerSecretName(node *openvoxv1alpha1.VoxNode, req openvoxv1alpha1.CertManagerCertRequest) string {
	if req.SecretName != "" {
		return req.SecretName
	}
	return fmt.Sprintf("voxnode-cert-%s-%s", sanitizeDNSLabel(node.Name), sanitizeDNSLabel(req.Name))
}

func matchesHieraLabelSelector(sec *corev1.Secret, customSelector map[string]string) bool {
	if len(sec.Labels) == 0 {
		return false
	}
	if len(customSelector) > 0 {
		for k, v := range customSelector {
			if sec.Labels[k] != v {
				return false
			}
		}
		return true
	}
	return sec.Labels[LabelHieraSecret] == "true"
}

func mergeSecretKeys(dst map[string]interface{}, sec *corev1.Secret, prefix string) {
	keys := make([]string, 0, len(sec.Data))
	for k := range sec.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		hieraKey := k
		if prefix != "" {
			hieraKey = fmt.Sprintf("%s::%s", prefix, k)
		}
		dst[hieraKey] = string(sec.Data[k])
	}
}

func marshalHieraYAMLWithSensitive(data map[string]interface{}) ([]byte, error) {
	out := make(map[string]interface{}, len(data)+1)
	lookupOpts := make(map[string]interface{}, len(data))

	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		out[k] = data[k]
		// Every key is wrapped in Sensitive, including the openvox::certs hash (which carries private keys);
		// Puppet code must call .unwrap to read the values.
		lookupOpts[k] = map[string]interface{}{
			"convert_to": "Sensitive",
		}
	}

	if len(lookupOpts) > 0 {
		out["lookup_options"] = lookupOpts
	}

	return yaml.Marshal(out)
}
