package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestCertManagerNamesNeverCollide(t *testing.T) {
	us := certManagerDefaultName("web01.us.example.com", "public_web")
	eu := certManagerDefaultName("web01.eu.example.com", "public_web")
	if us == eu {
		t.Fatalf("FQDN VoxNodes sharing a short hostname must get distinct names, both got %q", us)
	}
	if got := certManagerDefaultName("web01", "public_web"); got != "voxnode-cert-web01-public-web" {
		t.Errorf("names without dots must keep the pre-0.2.8 form, got %q", got)
	}
	long := strings.Repeat("a", 70) + ".example.com"
	for _, n := range []string{us, eu, certManagerDefaultName(long, "x"), certManagerDefaultName(long+"z", "x"),
		certManagerDefaultName(strings.Repeat("b", 220), "cert")} {
		if len(n) > 63 || !dns1123Label.MatchString(n) {
			t.Errorf("%q is not a valid name of at most 63 characters", n)
		}
	}
	if certManagerDefaultName(long, "x") == certManagerDefaultName(long+"z", "x") {
		t.Error("truncated names must still differ")
	}
	if a, b := dnsLabel("web01.us.example.com", 63), dnsLabel("web01.eu.example.com", 63); a == b || a != "web01-us-example-com" {
		t.Errorf("endpoint hostnames must keep the whole FQDN, got %q and %q", a, b)
	}
	if h := dnsLabel(strings.Repeat("x", 100)+".example.com", 63); len(h) > 63 || !dns1123Label.MatchString(h) {
		t.Errorf("long hostname %q is not a valid label", h)
	}
}

func TestSecretMapperIgnoresUnrelatedSecrets(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx", UID: "u1"},
		Spec: openvoxv1alpha1.VoxClusterSpec{
			Code:         openvoxv1alpha1.ClusterCodeSpec{Git: &openvoxv1alpha1.GitCodeSpec{CredentialsSecretRef: "git-creds"}},
			HieraSecrets: openvoxv1alpha1.HieraSecretsSpec{CommonSecrets: []string{"common"}},
		},
	}
	node := &openvoxv1alpha1.VoxNode{
		ObjectMeta: metav1.ObjectMeta{Name: "web01", Namespace: "ovx"},
		Spec: openvoxv1alpha1.VoxNodeSpec{ClusterRef: "prod", Certname: "web01.example.com",
			Secrets:      []openvoxv1alpha1.NodeSecretRef{{SecretName: "web01-db"}},
			Certificates: []openvoxv1alpha1.CertManagerCertRequest{{Name: "tls", SecretName: "web01-tls"}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, node).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s}
	meta := func(name string, labels map[string]string) *metav1.PartialObjectMetadata {
		return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ovx", Labels: labels}}
	}
	for _, tc := range []struct {
		obj  *metav1.PartialObjectMetadata
		want bool
	}{
		{meta("sh.helm.release.v1.app.v3", map[string]string{"owner": "helm"}), false},
		{meta("app-credentials", nil), false},
		{meta("anything", map[string]string{LabelHieraSecret: "true"}), true},
		{meta("prod-ca-tls", nil), true},
		{meta("git-creds", nil), true},
		{meta("common", nil), true},
		{meta("web01-db", nil), true},
		{meta("web01-tls", nil), true},
	} {
		if got := len(r.clustersForSecret(ctx, tc.obj)) > 0; got != tc.want {
			t.Errorf("Secret %s: enqueued=%v, want %v", tc.obj.Name, got, tc.want)
		}
	}
}

func TestHieraSecretsAreListedWithALabelSelector(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).Build()
	selectors := []string{}
	c := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.SecretList); ok {
			lo := &client.ListOptions{}
			lo.ApplyOptions(opts)
			sel := ""
			if lo.LabelSelector != nil {
				sel = lo.LabelSelector.String()
			}
			selectors = append(selectors, sel)
		}
		return cl.List(ctx, list, opts...)
	}})
	cluster := &openvoxv1alpha1.VoxCluster{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx"}}
	if _, err := RenderHieraSecretsBundle(ctx, c, cluster, nil); err != nil {
		t.Fatal(err)
	}
	if len(selectors) != 1 || selectors[0] != LabelHieraSecret+"=true" {
		t.Errorf("Secrets must be filtered server-side by label, got selectors %q", selectors)
	}
}

func TestPruneGitLeftoversWhenCodeModeChanges(t *testing.T) {
	ctx := context.Background()
	s := newVoxKubeScheme(t)
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx", UID: "u1"},
		Spec:       openvoxv1alpha1.VoxClusterSpec{Code: openvoxv1alpha1.ClusterCodeSpec{Mode: "pvc", ClaimName: "code"}},
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "prod-g10k", Namespace: "ovx"}}
	tok := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "prod-g10k-git-token", Namespace: "ovx"}}
	for _, o := range []client.Object{cm, tok} {
		if err := controllerutil.SetControllerReference(cluster, o, s); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, cm, tok).Build()
	if err := (&VoxClusterReconciler{Client: c, Scheme: s}).pruneDisabledComponents(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	for _, o := range []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}} {
		name := "prod-g10k"
		if _, ok := o.(*corev1.Secret); ok {
			name = "prod-g10k-git-token"
		}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: name}, o); err == nil {
			t.Errorf("%s must be pruned when code.mode is no longer git", name)
		}
	}
}

func TestENCScriptRejectsOddCertnames(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "enc.sh")
	if err := os.WriteFile(script, []byte(renderENCScript(&openvoxv1alpha1.VoxCluster{})), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "../etc/passwd", "a/b", "x..y", "a b"} {
		if err := exec.Command("sh", script, bad).Run(); err == nil {
			t.Errorf("enc.sh must reject certname %q", bad)
		}
	}
}
