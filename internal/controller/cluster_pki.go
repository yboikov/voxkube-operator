package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

const (
	defaultCATTL       = 10 * 365 * 24 * time.Hour
	serviceCertTTL     = 365 * 24 * time.Hour
	leafRenewBefore    = 30 * 24 * time.Hour
	caSecretRetainNote = "voxkube.io/retained"
)

// caSecretName returns the name of the cluster's CA/TLS Secret.
func caSecretName(cluster *openvoxv1alpha1.VoxCluster) string {
	if cluster.Status.CASecretName != "" {
		return cluster.Status.CASecretName
	}
	return fmt.Sprintf("%s-ca-tls", cluster.Name)
}

// serviceCertname is the Puppet certname used by the CA and compiler pods.
func serviceCertname(cluster *openvoxv1alpha1.VoxCluster) string {
	return fmt.Sprintf("%s.%s.svc", cluster.Name, cluster.Namespace)
}

// serviceDNSNames lists every DNS SAN of the shared service certificate: the compiler, CA and DB Services in all
// in-cluster forms, plus the configured external names.
func serviceDNSNames(cluster *openvoxv1alpha1.VoxCluster) []string {
	ns := cluster.Namespace
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, svc := range []string{cluster.Name, cluster.Name + "-ca", cluster.Name + "-db"} {
		add(svc)
		add(svc + "." + ns)
		add(svc + "." + ns + ".svc")
		add(svc + "." + ns + ".svc.cluster.local")
	}
	for _, ep := range []openvoxv1alpha1.EndpointNetworkingSpec{cluster.Spec.Networking.Server, cluster.Spec.Networking.CA} {
		add(ep.ExternalHostname)
		if rt := ep.OpenShiftRoute; rt != nil && rt.Enabled {
			add(rt.Host)
		}
		for _, n := range ep.AdditionalDNSNames {
			add(n)
		}
	}
	return names
}

// reconcileCAPKI makes sure <cluster>-ca-tls holds the CA (generated once, kept forever) and a valid service
// certificate whose SANs match the spec (renewed 30 days before expiry or when SANs change).
// The Secret is deliberately NOT owned by the VoxCluster so that deleting the VoxCluster does not destroy the CA.
func (r *VoxClusterReconciler) reconcileCAPKI(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) (string, string, error) {
	secName := fmt.Sprintf("%s-ca-tls", cluster.Name)
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: secName}, sec)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return "", "", err
	}

	data := map[string][]byte{}
	if exists {
		for k, v := range sec.Data {
			data[k] = v
		}
	}
	changed := !exists

	if len(data["ca.crt"]) == 0 || len(data["ca.key"]) == 0 {
		ttl := defaultCATTL
		if cluster.Spec.CA.TTL != "" {
			d, err := ParseDurationWithDays(cluster.Spec.CA.TTL)
			if err != nil {
				return "", "", fmt.Errorf("invalid spec.ca.ttl: %w", err)
			}
			ttl = d
		}
		caCert, caKey, err := GenerateSelfSignedCA(fmt.Sprintf("VoxKube Puppet CA - %s", cluster.Name), ttl)
		if err != nil {
			return "", "", err
		}
		data["ca.crt"], data["ca.key"] = caCert, caKey
		delete(data, "ca.pub")
		delete(data, "crl.pem")
		delete(data, "tls.crt")
		changed = true
	}
	if len(data["ca.pub"]) == 0 || len(data["crl.pem"]) == 0 {
		caPub, crlPEM, err := GenerateCAArtifacts(data["ca.crt"], data["ca.key"])
		if err != nil {
			return "", "", err
		}
		if len(data["ca.pub"]) == 0 {
			data["ca.pub"] = caPub
		}
		if len(data["crl.pem"]) == 0 {
			data["crl.pem"] = crlPEM
		}
		changed = true
	}

	if renew, why := LeafNeedsRenewal(data["tls.crt"], data["ca.crt"], serviceDNSNames(cluster), leafRenewBefore); renew {
		certPEM, keyPEM, err := IssueLeafCertificate(data["ca.crt"], data["ca.key"], LeafCertOptions{
			CommonName: serviceCertname(cluster),
			DNSNames:   serviceDNSNames(cluster),
			Duration:   serviceCertTTL,
			CLIAuth:    true, // the operator uses this certificate for the CA admin API
		})
		if err != nil {
			return "", "", err
		}
		pub, err := ExtractPublicKeyPEM(keyPEM)
		if err != nil {
			return "", "", err
		}
		data["tls.crt"], data["tls.key"], data["tls.pub"] = certPEM, keyPEM, pub
		changed = true
		if exists {
			r.event(cluster, corev1.EventTypeNormal, "ServiceCertificateIssued", "Issued a new service certificate: "+why)
		}
	}

	if !exists {
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        secName,
				Namespace:   cluster.Namespace,
				Labels:      voxClusterLabels(cluster.Name, "ca"),
				Annotations: map[string]string{caSecretRetainNote: "true"},
			},
			Type: corev1.SecretTypeTLS,
			Data: data,
		}
		if err := r.Create(ctx, sec); err != nil {
			return "", "", err
		}
	} else {
		// Older versions set an owner reference; drop it so the CA survives VoxCluster deletion.
		refs := sec.OwnerReferences[:0]
		for _, ref := range sec.OwnerReferences {
			if ref.UID != cluster.UID {
				refs = append(refs, ref)
			}
		}
		if len(refs) != len(sec.OwnerReferences) {
			sec.OwnerReferences = refs
			changed = true
		}
		if changed {
			sec.Data = data
			if err := r.Update(ctx, sec); err != nil {
				return "", "", err
			}
		}
	}
	return secName, hashBytes(data["tls.crt"]), nil
}

// syncCRL copies the CA's current CRL into <cluster>-ca-tls (key crl.pem) so that compilers reject revoked agents.
func (r *VoxClusterReconciler) syncCRL(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster, caClient OpenVoxCAClient, caURL, secName string) error {
	crlPEM, err := caClient.FetchCRL(ctx, caURL)
	if err != nil {
		return err
	}
	sec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: secName}, sec); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(sec.Data["crl.pem"]), bytes.TrimSpace(crlPEM)) {
		return nil
	}
	if err := verifyCRL(crlPEM, sec.Data["ca.crt"]); err != nil {
		return fmt.Errorf("CRL from CA rejected: %w", err)
	}
	sec.Data["crl.pem"] = crlPEM
	return r.Update(ctx, sec)
}

func verifyCRL(crlPEM, caCertPEM []byte) error {
	crlBlock, _ := pem.Decode(crlPEM)
	if crlBlock == nil {
		return fmt.Errorf("no PEM CRL")
	}
	crl, err := x509.ParseRevocationList(crlBlock.Bytes)
	if err != nil {
		return err
	}
	caBlock, _ := pem.Decode(caCertPEM)
	if caBlock == nil {
		return fmt.Errorf("no CA certificate")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return err
	}
	return crl.CheckSignatureFrom(caCert)
}

// clusterMTLSClient builds an HTTP client that trusts the cluster CA and presents the service certificate.
func clusterMTLSClient(ctx context.Context, c client.Reader, cluster *openvoxv1alpha1.VoxCluster) (*http.Client, error) {
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: caSecretName(cluster)}, sec); err != nil {
		return nil, err
	}
	return mtlsClients.get(cluster.Namespace+"/"+cluster.Name, sec.Data["ca.crt"], sec.Data["tls.crt"], sec.Data["tls.key"])
}

// mtlsClientCache keeps one mTLS *http.Client per VoxCluster so connections are reused across reconciles.
// An entry is rebuilt when the credentials change; the old client's idle connections are closed.
type mtlsClientCache struct {
	mu      sync.Mutex
	entries map[string]mtlsClientEntry
}

type mtlsClientEntry struct {
	hash   string
	client *http.Client
}

var mtlsClients = &mtlsClientCache{entries: map[string]mtlsClientEntry{}}

func (c *mtlsClientCache) get(key string, caPEM, certPEM, keyPEM []byte) (*http.Client, error) {
	h := sha256.New()
	for _, b := range [][]byte{caPEM, certPEM, keyPEM} {
		h.Write(b)
		h.Write([]byte{0})
	}
	sum := hex.EncodeToString(h.Sum(nil))

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && e.hash == sum {
		return e.client, nil
	}
	cl, err := NewMTLSHTTPClient(caPEM, certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if old, ok := c.entries[key]; ok {
		old.client.CloseIdleConnections()
	}
	c.entries[key] = mtlsClientEntry{hash: sum, client: cl}
	return cl, nil
}

// forget drops a cluster's client (on VoxCluster deletion).
func (c *mtlsClientCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		e.client.CloseIdleConnections()
		delete(c.entries, key)
	}
}

// resolveCAClient returns base bound to the cluster's mTLS credentials when base is the HTTP implementation
// (test doubles are returned unchanged).
func resolveCAClient(ctx context.Context, c client.Reader, base OpenVoxCAClient, cluster *openvoxv1alpha1.VoxCluster) OpenVoxCAClient {
	if base == nil {
		return nil
	}
	if _, isHTTP := base.(*HTTPCAClient); !isHTTP {
		return base
	}
	if hc, err := clusterMTLSClient(ctx, c, cluster); err == nil {
		return NewHTTPCAClient(hc)
	}
	return base
}

// resolveDBClient is the OpenVox DB counterpart of resolveCAClient.
func resolveDBClient(ctx context.Context, c client.Reader, base OpenVoxDBClient, cluster *openvoxv1alpha1.VoxCluster) OpenVoxDBClient {
	if base == nil {
		return nil
	}
	if _, isHTTP := base.(*HTTPPuppetDBClient); !isHTTP {
		return base
	}
	if hc, err := clusterMTLSClient(ctx, c, cluster); err == nil {
		return NewHTTPPuppetDBClient(hc)
	}
	return base
}

func (r *VoxClusterReconciler) caClientForCluster(ctx context.Context, cluster *openvoxv1alpha1.VoxCluster) OpenVoxCAClient {
	return resolveCAClient(ctx, r.Client, r.CAClient, cluster)
}
