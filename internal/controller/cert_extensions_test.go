package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func utf8Ext(t *testing.T, oid asn1.ObjectIdentifier, v string) pkix.Extension {
	t.Helper()
	b, err := asn1.MarshalWithParams(v, "utf8")
	if err != nil {
		t.Fatal(err)
	}
	return pkix.Extension{Id: oid, Value: b}
}

// testPuppetExtensions are what an agent with this csr_attributes.yaml would request:
// pp_role, pp_datacenter, pp_preshared_key, one site-specific extension, plus a non-Puppet extension.
func testPuppetExtensions(t *testing.T, role string) []pkix.Extension {
	return []pkix.Extension{
		utf8Ext(t, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 34380, 1, 1, 13}, role),
		utf8Ext(t, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 34380, 1, 1, 19}, "sofia"),
		utf8Ext(t, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 34380, 1, 1, 4}, "s3cr3t"),
		{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 34380, 1, 2, 7}, Value: []byte("raw-team")},
		utf8Ext(t, asn1.ObjectIdentifier{1, 2, 3, 4}, "not-puppet"),
	}
}

func testCSRAndCertPEM(t *testing.T, certname, csrRole, certRole string) (csrPEM, certPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:         pkix.Name{CommonName: certname},
		ExtraExtensions: testPuppetExtensions(t, csrRole),
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:    big.NewInt(7),
		Subject:         pkix.Name{CommonName: certname},
		NotBefore:       time.Now().Add(-time.Hour),
		NotAfter:        time.Now().Add(time.Hour),
		ExtraExtensions: testPuppetExtensions(t, certRole),
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Puppet CA"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

func TestPuppetExtensionsFromPEM(t *testing.T) {
	csrPEM, certPEM := testCSRAndCertPEM(t, "web01.example.com", "jumphost", "jumphost")
	for name, p := range map[string][]byte{"csr": csrPEM, "certificate": certPEM} {
		exts, err := PuppetExtensionsFromPEM(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := map[string]string{
			"pp_role":                 "jumphost",
			"pp_datacenter":           "sofia",
			"1.3.6.1.4.1.34380.1.2.7": "raw-team",
		}
		if len(exts) != len(want) {
			t.Errorf("%s: got %v, want %v (pp_preshared_key and non-Puppet extensions must be left out)", name, exts, want)
		}
		for k, v := range want {
			if exts[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, exts[k], v)
			}
		}
	}
	if _, err := PuppetExtensionsFromPEM([]byte("garbage")); err == nil {
		t.Error("expected an error for non-PEM input")
	}
}

func TestEffectiveRoleAndENC(t *testing.T) {
	node := &openvoxv1alpha1.VoxNode{
		ObjectMeta: metav1.ObjectMeta{Name: "web01"},
		Spec:       openvoxv1alpha1.VoxNodeSpec{Certname: "web01.example.com"},
		Status:     openvoxv1alpha1.VoxNodeStatus{CertificateExtensions: map[string]string{"pp_role": "jumphost"}},
	}
	cluster := &openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox"}}

	if got := EffectiveRole(node); got != "jumphost" {
		t.Errorf("without classification.role the certificate's pp_role must be used, got %q", got)
	}
	enc, err := RenderVoxNodeENCYAML(node, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(enc), "role: jumphost") {
		t.Errorf("ENC must pass pp_role as $role:\n%s", enc)
	}

	node.Spec.Classification.Role = "bastion"
	if got := EffectiveRole(node); got != "bastion" {
		t.Errorf("classification.role must override pp_role, got %q", got)
	}

	node.Spec.Classification.Role = ""
	node.Status.CertificateExtensions = nil
	enc, _ = RenderVoxNodeENCYAML(node, cluster)
	if strings.Contains(string(enc), "role:") {
		t.Errorf("no role at all must not set $role:\n%s", enc)
	}
}

// End to end against a mock CA and OpenVox DB: extensions shown while pending, read again after signing,
// status.role filled, fast polling while waiting for the first run, and a visible OpenVox DB error.
func TestVoxNodeCertificateExtensionsAndDatabaseStatus(t *testing.T) {
	s := newVoxKubeScheme(t)
	ctx := context.Background()
	csrPEM, certPEM := testCSRAndCertPEM(t, "web01.example.com", "jumphost", "jumphost")

	certState := "requested"
	dbDown := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/puppet-ca/v1/certificate_status/"):
			fp := ""
			if certState == "signed" {
				fp = "AA:BB"
			}
			_, _ = w.Write([]byte(`{"name":"web01.example.com","state":"` + certState + `","fingerprint":"` + fp + `"}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/puppet-ca/v1/certificate_status/"):
			certState = "signed"
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/puppet-ca/v1/certificate_request/web01.example.com" && certState == "requested":
			_, _ = w.Write(csrPEM)
		case r.URL.Path == "/puppet-ca/v1/certificate/web01.example.com" && certState == "signed":
			_, _ = w.Write(certPEM)
		case strings.HasPrefix(r.URL.Path, "/pdb/query/v4/nodes/") && dbDown:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("database is starting"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "openvox", Namespace: "openvox"},
		Status:     openvoxv1alpha1.VoxClusterStatus{ServerURL: srv.URL, CAURL: srv.URL, DatabaseURL: srv.URL},
	}
	node := &openvoxv1alpha1.VoxNode{
		ObjectMeta: metav1.ObjectMeta{Name: "web01.example.com", Namespace: "openvox"},
		Spec: openvoxv1alpha1.VoxNodeSpec{
			ClusterRef: "openvox", Certname: "web01.example.com", State: openvoxv1alpha1.NodeStatePendingApproval,
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, node).
		WithStatusSubresource(&openvoxv1alpha1.VoxNode{}).Build()
	r := &VoxNodeReconciler{Client: k8sClient, Scheme: s, CAClient: NewHTTPCAClient(srv.Client()), DBClient: NewHTTPPuppetDBClient(srv.Client())}
	key := types.NamespacedName{Namespace: "openvox", Name: "web01.example.com"}
	get := func() *openvoxv1alpha1.VoxNode {
		n := &openvoxv1alpha1.VoxNode{}
		if err := k8sClient.Get(ctx, key, n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 1. Pending: the requested pp_role is visible before approval.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	n := get()
	if n.Status.CertificateExtensions["pp_role"] != "jumphost" || n.Status.Role != "jumphost" {
		t.Errorf("pending node must show the CSR's extensions and role, got %+v", n.Status)
	}
	if !strings.Contains(n.Status.Conditions[0].Message, `pp_role "jumphost"`) {
		t.Errorf("approval message should name the requested role, got %q", n.Status.Conditions[0].Message)
	}

	// 2. Approved and signed while OpenVox DB is down: the error is visible and polling is fast.
	n.Spec.State = openvoxv1alpha1.NodeStateActive
	if err := k8sClient.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	n = get()
	if certState != "signed" || n.Status.CertificateFingerprint != "AA:BB" || n.Status.CertificateExtensions["pp_datacenter"] != "sofia" {
		t.Errorf("expected the signed certificate's extensions, got %+v", n.Status)
	}
	ready := n.Status.Conditions[0]
	if ready.Reason != "DatabaseUnreachable" || !strings.Contains(ready.Message, "503") {
		t.Errorf("an OpenVox DB failure must be visible, got %s: %s", ready.Reason, ready.Message)
	}
	if res.RequeueAfter != nodeRequeueWaiting {
		t.Errorf("expected a %s retry while OpenVox DB fails, got %s", nodeRequeueWaiting, res.RequeueAfter)
	}

	// 3. OpenVox DB up, no run yet: waiting for the first run is polled every 30s, not every 5 minutes.
	dbDown = false
	res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	n = get()
	if n.Status.Phase != openvoxv1alpha1.NodePhaseSigned || n.Status.Conditions[0].Reason != "CertificateSigned" {
		t.Errorf("expected Signed/CertificateSigned, got %s/%s", n.Status.Phase, n.Status.Conditions[0].Reason)
	}
	if res.RequeueAfter != nodeRequeueWaiting {
		t.Errorf("expected a %s poll while waiting for the first run, got %s", nodeRequeueWaiting, res.RequeueAfter)
	}
}
