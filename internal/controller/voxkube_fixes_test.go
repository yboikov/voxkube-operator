package controller

import (
	"context"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func TestParseRefAdvertisement(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha2 := strings.Repeat("b", 40)
	body := pktLine("# service=git-upload-pack\n") + "0000" +
		pktLine(sha1+" HEAD\x00multi_ack symref=HEAD:refs/heads/main\n") +
		pktLine(sha1+" refs/heads/main\n") +
		pktLine(sha2+" refs/heads/feature\n") +
		pktLine(sha2+" refs/tags/v1\n") + "0000"

	rev, err := ParseRefAdvertisement([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if rev.Head != sha1 {
		t.Errorf("expected HEAD %s, got %s", sha1, rev.Head)
	}
	if rev.RefsHash == "" {
		t.Fatal("expected a refs hash")
	}

	// Moving a non-default branch must change the refs hash (so compilers roll).
	moved := strings.Replace(body, sha2+" refs/heads/feature", strings.Repeat("c", 40)+" refs/heads/feature", 1)
	rev2, err := ParseRefAdvertisement([]byte(moved))
	if err != nil {
		t.Fatal(err)
	}
	if rev2.RefsHash == rev.RefsHash || rev2.Head != rev.Head {
		t.Errorf("expected only the refs hash to change, got %+v vs %+v", rev2, rev)
	}
}

func TestBuildDiscoveryPQL(t *testing.T) {
	q := BuildDiscoveryPQL(openvoxv1alpha1.DiscoverySelectorSpec{
		Environment: "prod\"uction",
		Classes:     []string{"profile::node_exporter", "::role::web"},
		PQL:         `facts.os.family = "RedHat"`,
	})
	for _, want := range []string{
		`inventory[certname, environment, facts] {`,
		`certname in nodes[certname] { node_state = "active" }`,
		`environment = "prod\"uction"`,
		`title = "Profile::Node_exporter"`,
		`title = "Role::Web"`,
		`(facts.os.family = "RedHat")`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("expected PQL to contain %q, got %s", want, q)
		}
	}
}

func TestParseDurationWithDays(t *testing.T) {
	cases := map[string]time.Duration{
		"10y":   10 * 365 * 24 * time.Hour,
		"90d":   90 * 24 * time.Hour,
		"720h":  720 * time.Hour,
		"1.5d":  36 * time.Hour,
		"30m0s": 30 * time.Minute,
	}
	for in, want := range cases {
		got, err := ParseDurationWithDays(in)
		if err != nil || got != want {
			t.Errorf("ParseDurationWithDays(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "-1d", "0y"} {
		if _, err := ParseDurationWithDays(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestLeafCertificates_CLIAuthAndRenewal(t *testing.T) {
	caCert, caKey, err := GenerateSelfSignedCA("test", 24*time.Hour*365)
	if err != nil {
		t.Fatal(err)
	}
	admin, _, err := IssueLeafCertificate(caCert, caKey, LeafCertOptions{CommonName: "svc", DNSNames: []string{"a", "b"}, Duration: 365 * 24 * time.Hour, CLIAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	scraper, _, err := IssueLeafCertificate(caCert, caKey, LeafCertOptions{CommonName: "scraper", Duration: 365 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	hasCLIAuth := func(certPEM []byte) (bool, string) {
		block, _ := pem.Decode(certPEM)
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		for _, ext := range c.Extensions {
			if ext.Id.Equal(oidPuppetCLIAuth) {
				var v string
				rest, err := asn1.UnmarshalWithParams(ext.Value, &v, "utf8")
				if err != nil || len(rest) != 0 {
					t.Fatalf("pp_cli_auth is not a UTF8String: %v", err)
				}
				return true, v
			}
		}
		return false, ""
	}
	if ok, v := hasCLIAuth(admin); !ok || v != "true" {
		t.Errorf("expected service certificate to carry pp_cli_auth=true, got %v %q", ok, v)
	}
	if ok, _ := hasCLIAuth(scraper); ok {
		t.Error("scraper certificate must not carry pp_cli_auth")
	}

	if renew, why := LeafNeedsRenewal(admin, caCert, []string{"b", "a"}, 30*24*time.Hour); renew {
		t.Errorf("did not expect renewal, got %s", why)
	}
	if renew, _ := LeafNeedsRenewal(admin, caCert, []string{"a", "b", "c"}, 30*24*time.Hour); !renew {
		t.Error("expected renewal when SANs change")
	}
	if renew, _ := LeafNeedsRenewal(admin, caCert, nil, 400*24*time.Hour); !renew {
		t.Error("expected renewal when the certificate expires within the window")
	}
	otherCA, _, _ := GenerateSelfSignedCA("other", 24*time.Hour)
	if renew, _ := LeafNeedsRenewal(admin, otherCA, nil, time.Hour); !renew {
		t.Error("expected renewal when the certificate is not signed by the current CA")
	}
}

func TestENCModesMetadataAndReports(t *testing.T) {
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx"},
		Spec: openvoxv1alpha1.VoxClusterSpec{
			Workflows: openvoxv1alpha1.ClusterWorkflowsSpec{
				NodeClassification: &openvoxv1alpha1.WorkflowNodeClassificationSpec{
					Mode:               "externalHttp",
					ExternalURL:        "https://enc.example.com/nodes/",
					DefaultEnvironment: "staging",
				},
				Facts:   &openvoxv1alpha1.WorkflowFactsSpec{InjectKubernetesMetadata: false},
				Reports: &openvoxv1alpha1.WorkflowReportsSpec{WebhookURL: "https://reports.example.com/in"},
			},
		},
	}
	script := renderENCScript(cluster)
	if !strings.Contains(script, `curl -fsS --max-time 10 'https://enc.example.com/nodes'"/${CERTNAME}"`) {
		t.Errorf("expected external ENC fallback, got:\n%s", script)
	}
	def := string(renderDefaultENC(cluster))
	if !strings.Contains(def, "environment: staging") || strings.Contains(def, "voxkube_cluster") {
		t.Errorf("unexpected default ENC:\n%s", def)
	}
	node := &openvoxv1alpha1.VoxNode{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: openvoxv1alpha1.VoxNodeSpec{Certname: "web.example.com"}}
	enc, err := RenderVoxNodeENCYAML(node, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(enc), "environment: staging") || strings.Contains(string(enc), "voxkube_") {
		t.Errorf("expected cluster default environment and no metadata, got:\n%s", enc)
	}
	conf := renderServerPuppetConf(cluster)
	if !strings.Contains(conf, "reports = puppetdb,http") || !strings.Contains(conf, "reporturl = https://reports.example.com/in") {
		t.Errorf("unexpected report settings:\n%s", conf)
	}

	cluster.Spec.Workflows.NodeClassification.Mode = "none"
	if strings.Contains(renderServerPuppetConf(cluster), "node_terminus") {
		t.Error("ENC mode none must not configure an exec node terminus")
	}
}

type mockCA struct {
	statuses []CertStatus
	crl      []byte
}

func (m *mockCA) GetCertStatus(context.Context, string, string) (*CertStatus, error) {
	return nil, ErrCertNotFound
}
func (m *mockCA) ListCertStatuses(context.Context, string) ([]CertStatus, error) {
	return m.statuses, nil
}
func (m *mockCA) SignCSR(context.Context, string, string) (*CertStatus, error) { return nil, nil }
func (m *mockCA) RevokeCert(context.Context, string, string) error             { return nil }
func (m *mockCA) CleanNode(context.Context, string, string) error              { return nil }
func (m *mockCA) FetchCRL(context.Context, string) ([]byte, error)             { return m.crl, nil }

func TestVoxCluster_AutosignHPAExternalDBAndAutoCreate(t *testing.T) {
	s := newVoxKubeScheme(t)
	ctx := context.Background()

	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx"},
		Spec: openvoxv1alpha1.VoxClusterSpec{
			Code: openvoxv1alpha1.ClusterCodeSpec{Mode: "pvc", ClaimName: "puppet-code"},
			CA:   openvoxv1alpha1.ClusterCASpec{AutosignMode: "psk", PSKSecretRef: "autosign-psk"},
			Server: openvoxv1alpha1.ClusterServerSpec{
				Autoscaling: &openvoxv1alpha1.ServerAutoscalingSpec{Enabled: true, MinReplicas: new(int32(3)), MaxReplicas: 8},
			},
			Database: openvoxv1alpha1.ClusterDatabaseSpec{
				Postgres: openvoxv1alpha1.ClusterPostgresSpec{
					Mode:                 "external",
					Host:                 "openvoxdb-rw.ovx.svc",
					CredentialsSecretRef: "openvoxdb-app",
					SSLMode:              "verify-full",
				},
			},
		},
	}
	cnpgSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "openvoxdb-app", Namespace: "ovx"},
		Data:       map[string][]byte{"username": []byte("app"), "password": []byte("s3cret")},
	}
	// Pretend the CA StatefulSet is already ready so CA housekeeping runs.
	caSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-ca", Namespace: "ovx"},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
	existingNode := &openvoxv1alpha1.VoxNode{
		ObjectMeta: metav1.ObjectMeta{Name: "known", Namespace: "ovx"},
		Spec:       openvoxv1alpha1.VoxNodeSpec{ClusterRef: "prod", Certname: "known.example.com", State: openvoxv1alpha1.NodeStateActive},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster, cnpgSecret, caSTS, existingNode).
		WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}, &appsv1.StatefulSet{}).
		Build()

	ca := &mockCA{statuses: []CertStatus{
		{Name: "prod.ovx.svc", State: "signed"},
		{Name: "known.example.com", State: "signed"},
		{Name: "New_Host.example.com", State: "requested"},
		{Name: "db01.example.com", State: "signed"},
		{Name: "gone.example.com", State: "revoked"},
	}}
	reconciler := &VoxClusterReconciler{Client: k8sClient, Scheme: s, CAClient: ca}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ovx", Name: "prod"}}); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// autosign psk wiring
	caCM := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-ca-config"}, caCM); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(caCM.Data["puppet.conf"], "autosign = "+caScriptsPath+"/autosign-psk.sh") {
		t.Errorf("expected psk autosign executable in CA puppet.conf:\n%s", caCM.Data["puppet.conf"])
	}

	// external DB with CNPG credentials and sslmode
	dbSec := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-db-database"}, dbSec); err != nil {
		t.Fatal(err)
	}
	ini := string(dbSec.Data["database.ini"])
	for _, want := range []string{"subname = //openvoxdb-rw.ovx.svc:5432/openvoxdb?sslmode=verify-full", "username = app", "password = s3cret"} {
		if !strings.Contains(ini, want) {
			t.Errorf("expected database.ini to contain %q, got:\n%s", want, ini)
		}
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-postgres"}, &appsv1.StatefulSet{}); err == nil {
		t.Error("external mode must not deploy the built-in PostgreSQL")
	}

	// HPA owns replicas; PVC code is mounted read-only without demo seeding
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-server"}, hpa); err != nil {
		t.Fatalf("expected HPA: %v", err)
	}
	if *hpa.Spec.MinReplicas != 3 || hpa.Spec.MaxReplicas != 8 {
		t.Errorf("unexpected HPA bounds: %+v", hpa.Spec)
	}
	deploy := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-server"}, deploy); err != nil {
		t.Fatal(err)
	}
	if *deploy.Spec.Replicas != 3 {
		t.Errorf("expected initial replicas from minReplicas, got %d", *deploy.Spec.Replicas)
	}
	foundPVC := false
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == "puppet-code" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "puppet-code" && v.PersistentVolumeClaim.ReadOnly {
			foundPVC = true
		}
	}
	if !foundPVC {
		t.Error("expected the code PVC to be mounted read-only")
	}

	// auto-created VoxNodes: requested -> PendingApproval, signed -> Active; server, known and revoked skipped
	nodes := &openvoxv1alpha1.VoxNodeList{}
	if err := k8sClient.List(ctx, nodes); err != nil {
		t.Fatal(err)
	}
	byCert := map[string]openvoxv1alpha1.VoxNode{}
	for _, n := range nodes.Items {
		byCert[n.Spec.Certname] = n
	}
	if len(nodes.Items) != 3 {
		t.Errorf("expected 3 VoxNodes (1 existing + 2 auto-created), got %d", len(nodes.Items))
	}
	if n, ok := byCert["New_Host.example.com"]; !ok || n.Spec.State != openvoxv1alpha1.NodeStatePendingApproval || n.Name != "new-host.example.com" || n.Labels[LabelAutoCreated] != "true" {
		t.Errorf("unexpected auto-created pending node: %+v", n)
	}
	if n, ok := byCert["db01.example.com"]; !ok || n.Spec.State != openvoxv1alpha1.NodeStateActive {
		t.Errorf("unexpected auto-created signed node: %+v", n)
	}
	if _, ok := byCert["gone.example.com"]; ok {
		t.Error("revoked certificates must not get a VoxNode")
	}
}

func TestVoxCluster_GitSidecarModeAndPSKRequiresSecret(t *testing.T) {
	s := newVoxKubeScheme(t)
	ctx := context.Background()
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "ovx"},
		Spec: openvoxv1alpha1.VoxClusterSpec{
			Code: openvoxv1alpha1.ClusterCodeSpec{
				Mode: "git",
				Git: &openvoxv1alpha1.GitCodeSpec{
					Remote:               "git@github.com:myorg/control.git",
					AuthType:             "sshKey",
					CredentialsSecretRef: "deploy-key",
				},
			},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
	reconciler := &VoxClusterReconciler{Client: k8sClient, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ovx", Name: "prod"}}); err != nil {
		t.Fatal(err)
	}
	deploy := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-server"}, deploy); err != nil {
		t.Fatal(err)
	}
	var sidecar *corev1.Container
	for i := range deploy.Spec.Template.Spec.Containers {
		if deploy.Spec.Template.Spec.Containers[i].Name == "g10k-sync" {
			sidecar = &deploy.Spec.Template.Spec.Containers[i]
		}
	}
	if sidecar == nil {
		t.Fatal("SSH remotes must fall back to the g10k sidecar")
	}
	if _, ok := deploy.Spec.Template.Annotations[AnnotationCodeRevision]; ok {
		t.Error("sidecar mode must not roll compilers on commits")
	}
	hasSSH := false
	for _, m := range sidecar.VolumeMounts {
		if m.Name == "g10k-ssh" && m.MountPath == g10kSSHDir {
			hasSSH = true
		}
	}
	if !hasSSH {
		t.Error("expected the SSH deploy key to be mounted into the g10k sidecar")
	}

	// psk without a Secret is a spec error that must surface as Phase=Error
	latest := &openvoxv1alpha1.VoxCluster{}
	_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod"}, latest)
	latest.Spec.CA.AutosignMode = "psk"
	if err := k8sClient.Update(ctx, latest); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ovx", Name: "prod"}}); err == nil {
		t.Fatal("expected an error for autosignMode psk without pskSecretRef")
	}
	_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod"}, latest)
	if latest.Status.Phase != openvoxv1alpha1.ClusterPhaseError {
		t.Errorf("expected Phase=Error, got %s", latest.Status.Phase)
	}
}
