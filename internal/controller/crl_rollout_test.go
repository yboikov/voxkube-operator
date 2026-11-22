package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func podAnnotation(t *testing.T, r *VoxClusterReconciler, name, key string) (string, bool) {
	t.Helper()
	d := &appsv1.Deployment{}
	getObj(t, r.Client, name, d)
	v, ok := d.Spec.Template.Annotations[key]
	return v, ok
}

// Jetty reads ssl-crl-path only at startup, so a new CRL must roll the pods that check agents against it (#69).
func TestCRLChangeRollsCompilers(t *testing.T) {
	ctx := context.Background()
	for _, withAllowlist := range []bool{true, false} {
		s := newVoxKubeScheme(t)
		cluster := pkiCluster() // has database.certificateAllowlist
		if !withAllowlist {
			cluster.Spec.Database.CertificateAllowlist = nil
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
		r := &VoxClusterReconciler{Client: c, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
		reconcilePKICluster(t, c, r)

		srv1, ok := podAnnotation(t, r, "prod-server", AnnotationCRLHash)
		if !ok || srv1 == "" {
			t.Fatalf("compilers need %s", AnnotationCRLHash)
		}
		db1, dbOK := podAnnotation(t, r, "prod-db", AnnotationCRLHash)
		if dbOK != withAllowlist {
			t.Fatalf("allowlist=%v: OpenVox DB %s present=%v", withAllowlist, AnnotationCRLHash, dbOK)
		}
		tls1, _ := podAnnotation(t, r, "prod-server", AnnotationTLSHash)

		// Unchanged CRL: nothing moves.
		reconcilePKICluster(t, c, r)
		if v, _ := podAnnotation(t, r, "prod-server", AnnotationCRLHash); v != srv1 {
			t.Error("an unchanged CRL must not roll the compilers")
		}

		// A revocation: the CA's CRL changes (syncCRL writes it into <c>-ca-tls).
		ca := getSecret(t, c, "prod-ca-tls")
		ca.Data["crl.pem"] = append([]byte{}, ca.Data["crl.pem"]...)
		ca.Data["crl.pem"] = append(ca.Data["crl.pem"], []byte("\n# revoked serial 2\n")...)
		if err := c.Update(ctx, ca); err != nil {
			t.Fatal(err)
		}
		reconcilePKICluster(t, c, r)
		if v, _ := podAnnotation(t, r, "prod-server", AnnotationCRLHash); v == srv1 {
			t.Error("a new CRL must roll the compilers")
		}
		if v, _ := podAnnotation(t, r, "prod-server", AnnotationTLSHash); v != tls1 {
			t.Error("the certificate hash must not change with the CRL")
		}
		if withAllowlist {
			if v, _ := podAnnotation(t, r, "prod-db", AnnotationCRLHash); v == db1 {
				t.Error("with certificateAllowlist, a new CRL must roll OpenVox DB")
			}
		} else if _, ok := podAnnotation(t, r, "prod-db", AnnotationCRLHash); ok {
			t.Error("without certificateAllowlist OpenVox DB only trusts operator-issued certificates and must not roll on a CRL change")
		}
	}
}
