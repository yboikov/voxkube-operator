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
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		`inventory[certname, environment, facts, trusted] {`,
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
	if !strings.Contains(script, `curl -sS --max-time 10 -o "$BODY" -w '%{http_code}' 'https://enc.example.com/nodes'"/${CERTNAME}"`) {
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
func (m *mockCA) GetCertificatePEM(context.Context, string, string) ([]byte, error) {
	return nil, ErrCertNotFound
}
func (m *mockCA) GetCertificateRequestPEM(context.Context, string, string) ([]byte, error) {
	return nil, ErrCertNotFound
}

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
	hasRemote := false
	for _, e := range sidecar.Env {
		if e.Name == "VOXKUBE_GIT_REMOTE" && e.Value == "git@github.com:myorg/control.git" {
			hasRemote = true
		}
	}
	if !hasRemote {
		t.Error("the sidecar needs VOXKUBE_GIT_REMOTE to check for moved branches")
	}
	g10kCM := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "prod-g10k"}, g10kCM); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g10kCM.Data["sidecar.sh"], "deploy.sh --if-changed") {
		t.Error("the sidecar must redeploy only when a branch moved (deploy.sh --if-changed)")
	}
	if !strings.Contains(g10kCM.Data["deploy.sh"], "git ls-remote --heads") || !strings.Contains(g10kCM.Data["deploy.sh"], ".voxkube-deployed-refs") {
		t.Error("deploy.sh must record the deployed branch heads")
	}
	for _, want := range []string{"not deploying", "exit 1", "g10k deployed no environments", `rm -rf "$CACHE"`} {
		if !strings.Contains(g10kCM.Data["deploy.sh"], want) {
			t.Errorf("deploy.sh must fail on an unreachable remote or an empty deployment and reset the g10k cache (missing %q)", want)
		}
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

type countingDB struct{ queries int }

func (c *countingDB) DeactivateNode(context.Context, string, string) error { return nil }
func (c *countingDB) QueryActiveNodes(context.Context, string, openvoxv1alpha1.DiscoverySelectorSpec) ([]DiscoveredPuppetNode, error) {
	c.queries++
	return nil, nil
}
func (c *countingDB) GetNode(context.Context, string, string) (*PuppetDBNode, error) {
	return nil, ErrNodeNotFound
}

func TestVoxDiscovery_DatabaseDisabledRemovesSlices(t *testing.T) {
	s := newVoxKubeScheme(t)
	ctx := context.Background()

	disabled := false
	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "ovx"},
		Spec:       openvoxv1alpha1.VoxClusterSpec{Database: openvoxv1alpha1.ClusterDatabaseSpec{Enabled: &disabled}},
	}
	disc := &openvoxv1alpha1.VoxDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "node-exporter", Namespace: "ovx", UID: "disc-uid"},
		Spec: openvoxv1alpha1.VoxDiscoverySpec{
			ClusterRef: "dev",
			Ports:      []openvoxv1alpha1.DiscoveryPort{{Name: "metrics", Port: 9100}},
		},
	}
	owner := []metav1.OwnerReference{{
		APIVersion: openvoxv1alpha1.GroupVersion.String(), Kind: "VoxDiscovery", Name: disc.Name, UID: disc.UID,
		Controller: new(true),
	}}
	staleSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "node-exporter", Namespace: "ovx", OwnerReferences: owner}}
	staleSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-exporter-0", Namespace: "ovx", OwnerReferences: owner,
			Labels: map[string]string{discoveryv1.LabelServiceName: "node-exporter"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}

	k8sClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cluster, disc, staleSvc, staleSlice).
		WithStatusSubresource(&openvoxv1alpha1.VoxDiscovery{}).
		Build()
	db := &countingDB{}
	r := &VoxDiscoveryReconciler{Client: k8sClient, Scheme: s, DBClient: db}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ovx", Name: "node-exporter"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no periodic requeue without a database, got %s", res.RequeueAfter)
	}
	if db.queries != 0 {
		t.Errorf("expected no PuppetDB queries, got %d", db.queries)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "node-exporter-0"}, &discoveryv1.EndpointSlice{}); err == nil {
		t.Error("expected the EndpointSlice to be removed")
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "node-exporter"}, &corev1.Service{}); err == nil {
		t.Error("expected the headless Service to be removed")
	}
	got := &openvoxv1alpha1.VoxDiscovery{}
	_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: "ovx", Name: "node-exporter"}, got)
	c := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	if c == nil || c.Reason != "DatabaseDisabled" || got.Status.EndpointSliceCount != 0 {
		t.Errorf("expected Ready=False/DatabaseDisabled and no slices, got %+v / %+v", c, got.Status)
	}
}

func TestServiceMonitorUsesEndpointSliceRole(t *testing.T) {
	disc := &openvoxv1alpha1.VoxDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "node-exporter", Namespace: "openvox"},
		Spec: openvoxv1alpha1.VoxDiscoverySpec{
			Ports: []openvoxv1alpha1.DiscoveryPort{{Name: "metrics", Port: 9100}},
			ServiceMonitor: openvoxv1alpha1.DiscoveryServiceMonitorSpec{
				Enabled: true,
				TLS:     openvoxv1alpha1.DiscoveryTLSSpec{Enabled: true},
			},
		},
	}
	spec := buildServiceMonitorSpec(disc, "node-exporter-prometheus-scraper-tls")
	if spec["serviceDiscoveryRole"] != "EndpointSlice" {
		t.Fatalf("operator only writes EndpointSlices, ServiceMonitor must use that role, got %v", spec["serviceDiscoveryRole"])
	}
	eps := spec["endpoints"].([]interface{})
	ep := eps[0].(map[string]interface{})
	if ep["port"] != "metrics" || ep["interval"] != "30s" || ep["scheme"] != "https" {
		t.Errorf("unexpected endpoint: %v", ep)
	}
	rl := ep["relabelings"].([]interface{})[0].(map[string]interface{})
	src := rl["sourceLabels"].([]interface{})[0]
	if src != "__meta_kubernetes_endpointslice_endpoint_node_name" || rl["targetLabel"] != "instance" || rl["regex"] != "(.+)" {
		t.Errorf("instance relabeling must read the EndpointSlice node name, got %v", rl)
	}

	off := false
	disc.Spec.ServiceMonitor.PreserveNodeHostnameAsInstance = &off
	disc.Spec.ServiceMonitor.TLS.Enabled = false
	ep = buildServiceMonitorSpec(disc, "")["endpoints"].([]interface{})[0].(map[string]interface{})
	rls := ep["relabelings"].([]interface{})
	if len(rls) != 1 || rls[0].(map[string]interface{})["action"] != "labelmap" {
		t.Errorf("without preserveNodeHostnameAsInstance only the per-target labelmap must remain, got %v", rls)
	}
	if _, ok := ep["scheme"]; ok {
		t.Error("scheme must be omitted without TLS")
	}
}

func TestDiscoveryTargetLabels(t *testing.T) {
	disc := &openvoxv1alpha1.VoxDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "node-exporter-web", Namespace: "openvox"},
		Spec: openvoxv1alpha1.VoxDiscoverySpec{
			Ports: []openvoxv1alpha1.DiscoveryPort{{Name: "metrics", Port: 9119}},
			ServiceMonitor: openvoxv1alpha1.DiscoveryServiceMonitorSpec{
				Enabled:      true,
				TargetLabels: map[string]string{"role": "web", "alerts": "true"},
			},
		},
	}
	if err := validateTargetLabels(disc.Spec.ServiceMonitor.TargetLabels); err != nil {
		t.Fatalf("valid labels rejected: %v", err)
	}
	spec := buildServiceMonitorSpec(disc, "")
	got, _ := spec["targetLabels"].([]interface{})
	if len(got) != 2 || got[0] != "alerts" || got[1] != "role" {
		t.Errorf("expected sorted targetLabels [alerts role], got %v", spec["targetLabels"])
	}
	svcLabels := discoveryServiceLabels(disc)
	if svcLabels["role"] != "web" || svcLabels["alerts"] != "true" || svcLabels["app.kubernetes.io/instance"] != "node-exporter-web" {
		t.Errorf("Service must carry target labels and selector labels, got %v", svcLabels)
	}

	disc.Spec.ServiceMonitor.TargetLabels = nil
	if _, ok := buildServiceMonitorSpec(disc, "")["targetLabels"]; ok {
		t.Error("targetLabels must be omitted when empty")
	}

	for name, labels := range map[string]map[string]string{
		"reserved":      {"job": "x"},
		"dunder":        {"__meta": "x"},
		"dotted key":    {"app.role": "x"},
		"leading digit": {"1role": "x"},
		"bad value":     {"role": "web server"},
	} {
		if err := validateTargetLabels(labels); err == nil {
			t.Errorf("%s: expected %v to be rejected", name, labels)
		}
	}
}

func TestDiscoveryEndpointLabels(t *testing.T) {
	disc := &openvoxv1alpha1.VoxDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "node-exporter", Namespace: "openvox"},
		Spec: openvoxv1alpha1.VoxDiscoverySpec{
			Ports:          []openvoxv1alpha1.DiscoveryPort{{Name: "metrics", Port: 9119}},
			EndpointLabels: map[string]string{"role": "trusted.extensions.pp_role", "os": "os.family", "kernel": "kernelrelease"},
			ServiceMonitor: openvoxv1alpha1.DiscoveryServiceMonitorSpec{
				Enabled:      true,
				TargetLabels: map[string]string{"env": "prod"},
			},
		},
	}
	if err := validateDiscoveryLabels(disc); err != nil {
		t.Fatalf("valid labels rejected: %v", err)
	}
	node := DiscoveredPuppetNode{
		Certname: "web01.example.com",
		Facts: map[string]interface{}{
			"trusted":       map[string]interface{}{"extensions": map[string]interface{}{"pp_role": "web"}},
			"os":            map[string]interface{}{"family": "RedHat"},
			"kernelrelease": "this is not a label value",
		},
	}
	vnLabels := voxNodeMonitoringLabels(map[string]string{
		"monitoring.voxkube.io/alerts": "false",
		"monitoring.voxkube.io/role":   "web-canary", // VoxNode overrides the fact
		"monitoring.voxkube.io/env":    "dev",        // owned by targetLabels: skipped
		"app.kubernetes.io/name":       "ignored",
	})
	topo, skipped := endpointTargetLabels(disc, node, vnLabels)
	want := map[string]string{
		"role":   "web-canary",
		"os":     "RedHat",
		"alerts": "false",
	}
	if len(topo) != len(want) {
		t.Fatalf("expected %v, got %v", want, topo)
	}
	for k, v := range want {
		if topo[k] != v {
			t.Errorf("labels[%s] = %q, want %q", k, topo[k], v)
		}
	}
	if skipped != 2 {
		t.Errorf("expected 2 skipped labels (invalid fact value, env owned by targetLabels), got %d", skipped)
	}

	if topo, _ := endpointTargetLabels(disc, DiscoveredPuppetNode{Certname: "bare"}, nil); topo != nil {
		t.Errorf("node without facts or VoxNode labels must have no labels, got %v", topo)
	}

	ep := buildServiceMonitorSpec(disc, "")["endpoints"].([]interface{})[0].(map[string]interface{})
	rls := ep["relabelings"].([]interface{})
	lm := rls[len(rls)-1].(map[string]interface{})
	if lm["action"] != "labelmap" || lm["regex"] != "__meta_kubernetes_endpointslice_label_label_voxkube_io_(.+)" {
		t.Errorf("expected labelmap for per-target labels, got %v", lm)
	}

	for name, spec := range map[string]map[string]string{
		"reserved":         {"instance": "certname"},
		"trailing _":       {"role_": "x"},
		"dash":             {"pp-role": "x"},
		"empty fact path":  {"role": " "},
		"also targetLabel": {"env": "x"},
	} {
		d := disc.DeepCopy()
		d.Spec.EndpointLabels = spec
		if err := validateDiscoveryLabels(d); err == nil {
			t.Errorf("%s: expected %v to be rejected", name, spec)
		}
	}
}

// Per-target labels must end up as EndpointSlice labels: the API server silently drops endpoints' deprecatedTopology.
func TestDiscoveryLabelsGroupEndpointSlices(t *testing.T) {
	s := newVoxKubeScheme(t)
	ctx := context.Background()
	disc := &openvoxv1alpha1.VoxDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "node-exporter", Namespace: "openvox", UID: "disc-uid"},
		Spec: openvoxv1alpha1.VoxDiscoverySpec{
			ClusterRef:     "production",
			Ports:          []openvoxv1alpha1.DiscoveryPort{{Name: "metrics", Port: 9119}},
			EndpointLabels: map[string]string{"role": "trusted.extensions.pp_role"},
		},
	}
	node := func(name, ip, role string) DiscoveredPuppetNode {
		facts := map[string]interface{}{"networking": map[string]interface{}{"ip": ip}}
		if role != "" {
			facts["trusted"] = map[string]interface{}{"extensions": map[string]interface{}{"pp_role": role}}
		}
		return DiscoveredPuppetNode{Certname: name, Facts: facts}
	}
	nodes := []DiscoveredPuppetNode{
		node("web01.example.com", "10.0.0.11", "web"),
		node("web02.example.com", "10.0.0.12", "web"),
		node("db01.example.com", "10.0.0.21", "db"),
		node("bare.example.com", "10.0.0.31", ""),
		node("web03.example.com", "fd00::13", "web"),
	}
	vnLabels := map[string]map[string]string{"web02.example.com": {"alerts": "false"}}

	k8sClient := fake.NewClientBuilder().WithScheme(s).WithObjects(disc).Build()
	r := &VoxDiscoveryReconciler{Client: k8sClient, Scheme: s}
	count, slices, skipped, err := r.reconcileEndpointSlices(ctx, disc, "node-exporter", nodes, map[string]bool{}, vnLabels)
	if err != nil {
		t.Fatal(err)
	}
	// groups: {} → bare; {role=db}; {role=web} → web01 (v4) + web03 (v6); {role=web,alerts=false} → web02
	if count != 5 || slices != 5 || skipped != 0 {
		t.Fatalf("expected 5 endpoints in 5 slices, 0 skipped; got %d, %d, %d", count, slices, skipped)
	}

	list := &discoveryv1.EndpointSliceList{}
	if err := k8sClient.List(ctx, list, client.InNamespace("openvox")); err != nil {
		t.Fatal(err)
	}
	byNode := map[string]discoveryv1.EndpointSlice{}
	for _, sl := range list.Items {
		for _, ep := range sl.Endpoints {
			if ep.DeprecatedTopology != nil {
				t.Errorf("slice %s must not use deprecatedTopology", sl.Name)
			}
			byNode[*ep.NodeName] = sl
		}
	}
	check := func(certname string, want map[string]string, wantName string) {
		t.Helper()
		sl, ok := byNode[certname]
		if !ok {
			t.Fatalf("%s not exported", certname)
		}
		if wantName != "" && sl.Name != wantName {
			t.Errorf("%s: slice name %q, want %q", certname, sl.Name, wantName)
		}
		got := map[string]string{}
		for k, v := range sl.Labels {
			if name, ok := strings.CutPrefix(k, "label.voxkube.io/"); ok {
				got[name] = v
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: target labels %v, want %v", certname, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: label %s=%q, want %q", certname, k, got[k], v)
			}
		}
		if sl.Labels[discoveryv1.LabelServiceName] != "node-exporter" {
			t.Errorf("%s: slice %s lost its service-name label", certname, sl.Name)
		}
	}
	check("bare.example.com", map[string]string{}, "node-exporter-0")
	check("db01.example.com", map[string]string{"role": "db"}, "")
	check("web01.example.com", map[string]string{"role": "web"}, "")
	check("web02.example.com", map[string]string{"role": "web", "alerts": "false"}, "")
	check("web03.example.com", map[string]string{"role": "web"}, "")
	if byNode["web01.example.com"].Name == byNode["web02.example.com"].Name {
		t.Error("different label sets must not share a slice")
	}
	if byNode["web03.example.com"].AddressType != discoveryv1.AddressTypeIPv6 {
		t.Error("IPv6 endpoint must be in an IPv6 slice")
	}

	// Unmuting web02 moves it back to the role=web group and prunes its old slice.
	oldSlice := byNode["web02.example.com"].Name
	if _, slices, _, err = r.reconcileEndpointSlices(ctx, disc, "node-exporter", nodes, map[string]bool{}, nil); err != nil {
		t.Fatal(err)
	}
	if slices != 4 {
		t.Errorf("expected 4 slices after unmuting, got %d", slices)
	}
	gone := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: oldSlice}, gone); err == nil {
		t.Errorf("slice %s of the old label group must be pruned", oldSlice)
	}
}
