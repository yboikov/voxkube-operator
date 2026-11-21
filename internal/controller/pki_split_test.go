package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func pkiCluster() *openvoxv1alpha1.VoxCluster {
	return &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx", UID: "u1"},
		Spec: openvoxv1alpha1.VoxClusterSpec{
			Code:     openvoxv1alpha1.ClusterCodeSpec{Mode: "pvc", ClaimName: "code"},
			Database: openvoxv1alpha1.ClusterDatabaseSpec{CertificateAllowlist: []string{"report.example.com"}},
			Networking: openvoxv1alpha1.ClusterNetworkingSpec{
				Server: openvoxv1alpha1.EndpointNetworkingSpec{ExternalHostname: "puppet.example.com"},
				CA:     openvoxv1alpha1.EndpointNetworkingSpec{ExternalHostname: "puppet-ca.example.com"},
			},
		},
	}
}

func reconcilePKICluster(t *testing.T, c client.Client, r *VoxClusterReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ovx", Name: "prod"}}); err != nil {
		t.Fatal(err)
	}
}

func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ovx", Name: name}, s); err != nil {
		t.Fatalf("Secret %s: %v", name, err)
	}
	return s
}

func secretVolumes(spec corev1.PodSpec) []string {
	var out []string
	for _, v := range spec.Volumes {
		if v.Secret != nil {
			out = append(out, v.Secret.SecretName)
		}
	}
	return out
}

func TestWorkloadsNeverGetTheCAKey(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pkiCluster()).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
	reconcilePKICluster(t, c, r)

	ca := getSecret(t, c, "prod-ca-tls")
	if ext, _ := PuppetExtensionsFromPEM(ca.Data["tls.crt"]); ext["pp_cli_auth"] != "true" {
		t.Errorf("the operator's certificate must carry pp_cli_auth, got %v", ext)
	}
	for _, name := range []string{"prod-server-tls", "prod-db-tls"} {
		sec := getSecret(t, c, name)
		if _, ok := sec.Data["ca.key"]; ok {
			t.Errorf("%s must not contain the CA key", name)
		}
		if ext, _ := PuppetExtensionsFromPEM(sec.Data["tls.crt"]); ext["pp_cli_auth"] != "" {
			t.Errorf("%s must not carry pp_cli_auth", name)
		}
		for _, k := range []string{"ca.crt", "crl.pem", "tls.crt", "tls.key"} {
			if len(sec.Data[k]) == 0 {
				t.Errorf("%s is missing %s", name, k)
			}
		}
		if !metav1.IsControlledBy(sec, pkiCluster()) {
			t.Errorf("%s must be owned by the VoxCluster", name)
		}
	}

	for _, name := range []string{"prod-server", "prod-db"} {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: name}, d); err != nil {
			t.Fatal(err)
		}
		for _, sn := range secretVolumes(d.Spec.Template.Spec) {
			if sn == "prod-ca-tls" {
				t.Errorf("%s must not mount the CA Secret", name)
			}
		}
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-db-config"}, cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["jetty.ini"], "ssl-crl-path = /etc/puppetlabs/puppetdb/ssl/crl.pem") ||
		!strings.Contains(cm.Data["jetty.ini"], "certificate-allowlist = /etc/puppetlabs/puppetdb/certificate-allowlist") {
		t.Errorf("OpenVox DB must check the CRL and the allowlist:\n%s", cm.Data["jetty.ini"])
	}
	if cm.Data["certificate-allowlist"] != "prod.ovx.svc\nreport.example.com\n" {
		t.Errorf("unexpected allowlist %q", cm.Data["certificate-allowlist"])
	}
}

func TestCompilerHostnameChangeDoesNotRollDB(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	cluster := pkiCluster()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
	reconcilePKICluster(t, c, r)

	tlsHash := func(name string) string {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: name}, d); err != nil {
			t.Fatal(err)
		}
		return d.Spec.Template.Annotations[AnnotationTLSHash]
	}
	dbBefore, srvBefore := tlsHash("prod-db"), tlsHash("prod-server")
	caBefore := getSecret(t, c, "prod-ca-tls").Data["tls.crt"]

	live := &openvoxv1alpha1.VoxCluster{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod"}, live); err != nil {
		t.Fatal(err)
	}
	live.Spec.Networking.Server.ExternalHostname = "puppet2.example.com"
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	reconcilePKICluster(t, c, r)

	if tlsHash("prod-server") == srvBefore {
		t.Error("a new compiler hostname must re-issue the compiler certificate and roll the compilers")
	}
	if tlsHash("prod-db") != dbBefore {
		t.Error("a compiler hostname change must not roll OpenVox DB")
	}
	if string(getSecret(t, c, "prod-ca-tls").Data["tls.crt"]) != string(caBefore) {
		t.Error("a compiler hostname change must not re-issue the CA certificate")
	}
}

func TestCRLReachesWorkloadSecrets(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pkiCluster()).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
	reconcilePKICluster(t, c, r)

	ca := getSecret(t, c, "prod-ca-tls")
	ca.Data["crl.pem"] = []byte("-----BEGIN X509 CRL-----\nnew\n-----END X509 CRL-----\n")
	if err := c.Update(ctx, ca); err != nil {
		t.Fatal(err)
	}
	reconcilePKICluster(t, c, r)
	for _, name := range []string{"prod-server-tls", "prod-db-tls"} {
		if string(getSecret(t, c, name).Data["crl.pem"]) != string(ca.Data["crl.pem"]) {
			t.Errorf("%s must carry the CA's current CRL", name)
		}
	}
}
