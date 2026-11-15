package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

func newVoxKubeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add clientgo scheme: %v", err)
	}
	if err := openvoxv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add voxkube scheme: %v", err)
	}
	return s
}

func TestOpenShiftPodSecurityContext_DefaultsAreNilUIDGID(t *testing.T) {
	psc := buildOpenShiftPodSecurityContext(nil)

	if psc.RunAsUser != nil {
		t.Errorf("expected RunAsUser to be nil for OpenShift restricted-v2 SCC, got %v", *psc.RunAsUser)
	}
	if psc.RunAsGroup != nil {
		t.Errorf("expected RunAsGroup to be nil for OpenShift restricted-v2 SCC, got %v", *psc.RunAsGroup)
	}
	if psc.FSGroup != nil {
		t.Errorf("expected FSGroup to be nil for OpenShift restricted-v2 SCC, got %v", *psc.FSGroup)
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("expected RunAsNonRoot=true")
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("expected SeccompProfile=RuntimeDefault")
	}
}

func TestVoxClusterReconciler_FullStackAndOpenShiftCompliance(t *testing.T) {
	s := newVoxKubeScheme(t)
	ctx := context.Background()

	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "openvox",
		},
		Spec: openvoxv1alpha1.VoxClusterSpec{
			Code: openvoxv1alpha1.ClusterCodeSpec{
				Mode: "git",
				Git: &openvoxv1alpha1.GitCodeSpec{
					Remote:       "https://github.com/myorg/puppet-control-repo.git",
					PollInterval: "60s",
				},
			},
			Server: openvoxv1alpha1.ClusterServerSpec{
				Replicas: new(int32(3)),
			},
			Database: openvoxv1alpha1.ClusterDatabaseSpec{
				Housekeeping: openvoxv1alpha1.DatabaseHousekeepingSpec{
					NodeTTL:      "7d",
					NodePurgeTTL: "14d",
					ReportTTL:    "14d",
					GCInterval:   60,
				},
			},
			Networking: openvoxv1alpha1.ClusterNetworkingSpec{
				ExternalHostname: "puppet.apps.ocp.example.com",
				OpenShiftRoute: &openvoxv1alpha1.OpenShiftRouteSpec{
					Enabled: true,
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster).
		WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).
		Build()

	reconciler := &VoxClusterReconciler{
		Client:    k8sClient,
		Scheme:    s,
		GitPoller: &mockGitPoller{sha: "a1b2c3d4e5f6"},
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "openvox", Name: "production"},
	})
	if err != nil {
		t.Fatalf("VoxCluster Reconcile failed: %v", err)
	}

	// 1. Verify CA PKI Secret was created
	caSec := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "production-ca-tls"}, caSec); err != nil {
		t.Fatalf("expected CA secret production-ca-tls: %v", err)
	}
	if len(caSec.Data["ca.crt"]) == 0 || len(caSec.Data["tls.crt"]) == 0 {
		t.Error("expected CA secret to contain ca.crt and tls.crt")
	}

	// 2. Verify OpenVox DB ConfigMap contains housekeeping TTLs (node-ttl, node-purge-ttl, report-ttl)
	dbCM := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "production-db-config"}, dbCM); err != nil {
		t.Fatalf("expected DB ConfigMap production-db-config: %v", err)
	}
	dbIni := dbCM.Data["database.ini"]
	for _, expected := range []string{"node-ttl = 7d", "node-purge-ttl = 14d", "report-ttl = 14d"} {
		if !strings.Contains(dbIni, expected) {
			t.Errorf("expected database.ini to contain %q, got:\n%s", expected, dbIni)
		}
	}

	// 3. Verify Server Deployment is 100% OpenShift restricted-v2 SCC compliant & includes g10k sidecar + observed commit
	srvDeploy := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "production-server"}, srvDeploy); err != nil {
		t.Fatalf("expected Server Deployment production-server: %v", err)
	}
	if *srvDeploy.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", *srvDeploy.Spec.Replicas)
	}
	psc := srvDeploy.Spec.Template.Spec.SecurityContext
	if psc.RunAsUser != nil || psc.FSGroup != nil {
		t.Errorf("OpenShift restricted-v2 violation: RunAsUser (%v) and FSGroup (%v) must be nil", psc.RunAsUser, psc.FSGroup)
	}
	if srvDeploy.Spec.Template.Annotations[AnnotationObservedCommit] != "a1b2c3d4e5f6" {
		t.Errorf("expected observed-commit annotation a1b2c3d4e5f6, got %q", srvDeploy.Spec.Template.Annotations[AnnotationObservedCommit])
	}
	if len(srvDeploy.Spec.Template.Spec.InitContainers) == 0 || srvDeploy.Spec.Template.Spec.InitContainers[0].Name != "g10k-init" {
		t.Error("expected g10k-init initContainer on Server Deployment")
	}

	// 4. Verify VoxCluster status was updated
	updated := &openvoxv1alpha1.VoxCluster{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "production"}, updated); err != nil {
		t.Fatalf("failed to get updated VoxCluster: %v", err)
	}
	if updated.Status.Phase != openvoxv1alpha1.ClusterPhaseRunning {
		t.Errorf("expected Phase=Running, got %s", updated.Status.Phase)
	}
	if updated.Status.ObservedCommit != "a1b2c3d4e5f6" {
		t.Errorf("expected Status.ObservedCommit=a1b2c3d4e5f6, got %s", updated.Status.ObservedCommit)
	}
}

func TestHieraBridge_MultiCertAndESOSensitiveRendering(t *testing.T) {
	s := newVoxKubeScheme(t)
	ctx := context.Background()

	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "openvox",
		},
	}

	// ESO common Secret
	esoCommon := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eso-datadog",
			Namespace: "openvox",
			Labels: map[string]string{
				LabelHieraSecret: "true",
				LabelHieraScope:  "common",
			},
			Annotations: map[string]string{
				AnnotationHieraPrefix: "datadog_agent",
			},
		},
		Data: map[string][]byte{
			"api_key": []byte("dd-secret-key-999"),
		},
	}

	// ESO role Secret for webserver
	esoRole := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eso-webserver-secrets",
			Namespace: "openvox",
			Labels: map[string]string{
				LabelHieraSecret: "true",
				LabelHieraScope:  "role",
				LabelHieraRole:   "webserver",
			},
		},
		Data: map[string][]byte{
			"oauth::client_secret": []byte("super-secret-oauth"),
		},
	}

	// Two cert-manager TLS Secrets for web01.example.com
	cert1Sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "voxnode-cert-web01-public-web",
			Namespace: "openvox",
		},
		Data: map[string][]byte{
			"tls.crt": []byte("-----BEGIN CERTIFICATE-----\nPUBLIC\n-----END CERTIFICATE-----"),
			"tls.key": []byte("-----BEGIN PRIVATE KEY-----\nPUBLIC-KEY\n-----END PRIVATE KEY-----"),
		},
	}
	cert2Sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "voxnode-cert-web01-internal-api",
			Namespace: "openvox",
		},
		Data: map[string][]byte{
			"tls.crt": []byte("-----BEGIN CERTIFICATE-----\nINTERNAL\n-----END CERTIFICATE-----"),
			"tls.key": []byte("-----BEGIN PRIVATE KEY-----\nINTERNAL-KEY\n-----END PRIVATE KEY-----"),
			"ca.crt":  []byte("-----BEGIN CERTIFICATE-----\nINTERNAL-CA\n-----END CERTIFICATE-----"),
		},
	}

	node := openvoxv1alpha1.VoxNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web01",
			Namespace: "openvox",
		},
		Spec: openvoxv1alpha1.VoxNodeSpec{
			ClusterRef: "production",
			Certname:   "web01.example.com",
			State:      openvoxv1alpha1.NodeStateActive,
			Certificates: []openvoxv1alpha1.CertManagerCertRequest{
				{
					Name:           "public_web",
					IssuerRef:      openvoxv1alpha1.CertManagerIssuerRef{Name: "letsencrypt-prod"},
					TargetCertPath: "/etc/nginx/ssl/public.crt",
					TargetKeyPath:  "/etc/nginx/ssl/public.key",
				},
				{
					Name:           "internal_api",
					IssuerRef:      openvoxv1alpha1.CertManagerIssuerRef{Name: "corp-vault-issuer"},
					TargetCertPath: "/etc/nginx/ssl/internal.crt",
					TargetKeyPath:  "/etc/nginx/ssl/internal.key",
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster, esoCommon, esoRole, cert1Sec, cert2Sec).
		Build()

	bundle, err := RenderHieraSecretsBundle(ctx, k8sClient, cluster, []openvoxv1alpha1.VoxNode{node})
	if err != nil {
		t.Fatalf("RenderHieraSecretsBundle failed: %v", err)
	}

	// Verify common.yaml has datadog_agent::api_key and lookup_options Sensitive
	var commonParsed map[string]interface{}
	if err := yaml.Unmarshal(bundle["common.yaml"], &commonParsed); err != nil {
		t.Fatalf("failed to unmarshal common.yaml: %v", err)
	}
	if commonParsed["datadog_agent::api_key"] != "dd-secret-key-999" {
		t.Errorf("expected datadog_agent::api_key in common.yaml, got %v", commonParsed)
	}
	if !strings.Contains(string(bundle["common.yaml"]), "convert_to: Sensitive") {
		t.Errorf("expected lookup_options convert_to: Sensitive in common.yaml:\n%s", string(bundle["common.yaml"]))
	}

	// Verify roles_webserver.yaml
	if !strings.Contains(string(bundle["roles_webserver.yaml"]), "oauth::client_secret: super-secret-oauth") {
		t.Errorf("expected oauth::client_secret in roles_webserver.yaml:\n%s", string(bundle["roles_webserver.yaml"]))
	}

	// Verify nodes_web01.example.com.yaml contains BOTH certificates in openvox::certs map and flat keys
	nodeYAML := string(bundle["nodes_web01.example.com.yaml"])
	for _, expected := range []string{
		"openvox::certs:",
		"public_web:",
		"internal_api:",
		"/etc/nginx/ssl/public.crt",
		"/etc/nginx/ssl/internal.crt",
		"certs::public_web::tls_key:",
		"certs::internal_api::tls_key:",
		"convert_to: Sensitive",
	} {
		if !strings.Contains(nodeYAML, expected) {
			t.Errorf("expected nodes_web01.example.com.yaml to contain %q, got:\n%s", expected, nodeYAML)
		}
	}
}

func TestVoxNodeReconciler_SignENCAndAtomicDecommission(t *testing.T) {
	s := newVoxKubeScheme(t)
	ctx := context.Background()

	// Start mock OpenVox CA + OpenVox DB HTTP server to verify atomic API calls
	var caSigned, caCleaned, pdbDeactivated []string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/puppet-ca/v1/certificate_status/"):
			certname := strings.TrimPrefix(r.URL.Path, "/puppet-ca/v1/certificate_status/")
			caSigned = append(caSigned, certname)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && r.URL.Path == "/puppet-ca/v1/clean":
			caCleaned = append(caCleaned, "web01.example.com")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/pdb/cmd/v1":
			pdbDeactivated = append(pdbDeactivated, "web01.example.com")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"uuid":"1234"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockServer.Close()

	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "openvox",
		},
		Status: openvoxv1alpha1.VoxClusterStatus{
			ServerURL:   mockServer.URL,
			DatabaseURL: mockServer.URL,
		},
	}

	node := &openvoxv1alpha1.VoxNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web01",
			Namespace: "openvox",
		},
		Spec: openvoxv1alpha1.VoxNodeSpec{
			ClusterRef: "production",
			Certname:   "web01.example.com",
			State:      openvoxv1alpha1.NodeStateActive,
			Classification: openvoxv1alpha1.NodeClassificationSpec{
				Environment: "production",
				Role:        "webserver",
				Classes:     []string{"profile::base", "profile::nginx"},
				Parameters: map[string]apiextensionsv1.JSON{
					"nginx_worker_processes": {Raw: []byte(`4`)},
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster, node).
		WithStatusSubresource(&openvoxv1alpha1.VoxNode{}).
		Build()

	reconciler := &VoxNodeReconciler{
		Client:   k8sClient,
		Scheme:   s,
		CAClient: NewHTTPCAClient(mockServer.Client()),
		DBClient: NewHTTPPuppetDBClient(mockServer.Client()),
	}

	// Reconcile adds finalizer, signs CSR, and renders ENC
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "openvox", Name: "web01"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if len(caSigned) != 1 || caSigned[0] != "web01.example.com" {
		t.Errorf("expected CSR for web01.example.com to be signed on CA, got %v", caSigned)
	}

	// Verify ENC ConfigMap was rendered with role and parameters
	encCM := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "production-enc-nodes"}, encCM); err != nil {
		t.Fatalf("expected ENC ConfigMap production-enc-nodes: %v", err)
	}
	encYAML := encCM.Data["web01.example.com.yaml"]
	if !strings.Contains(encYAML, "role: webserver") || !strings.Contains(encYAML, "profile::nginx") {
		t.Errorf("unexpected ENC YAML:\n%s", encYAML)
	}

	// Now transition node to Decommissioned and verify atomic CA clean + PuppetDB deactivate node
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "web01"}, node); err != nil {
		t.Fatal(err)
	}
	node.Spec.State = openvoxv1alpha1.NodeStateDecommissioned
	if err := k8sClient.Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "openvox", Name: "web01"},
	})
	if err != nil {
		t.Fatalf("Decommission Reconcile failed: %v", err)
	}

	if len(caCleaned) != 1 {
		t.Errorf("expected CA clean API to be called once, got %d", len(caCleaned))
	}
	if len(pdbDeactivated) != 1 {
		t.Errorf("expected PuppetDB deactivate node API to be called once, got %d", len(pdbDeactivated))
	}

	updatedNode := &openvoxv1alpha1.VoxNode{}
	_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "web01"}, updatedNode)
	if !updatedNode.Status.CACleaned || !updatedNode.Status.PuppetDBDeactivated {
		t.Errorf("expected CACleaned=true and PuppetDBDeactivated=true, got %+v", updatedNode.Status)
	}
}

func TestVoxDiscoveryReconciler_EndpointSlicesAndOpenVoxCAMTLS(t *testing.T) {
	s := newVoxKubeScheme(t)
	ctx := context.Background()

	// Mock PuppetDB /pdb/query/v4/inventory endpoint returning 2 active Linux VMs and 1 deactivated VM
	deactivatedTime := "2026-11-10T00:00:00Z"
	mockPDB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{
				"certname": "web01.example.com",
				"environment": "production",
				"deactivated": null,
				"facts": {"kernel": "Linux", "networking": {"ip": "10.20.30.11"}},
				"classes": ["profile::node_exporter"]
			},
			{
				"certname": "db01.example.com",
				"environment": "production",
				"deactivated": null,
				"facts": {"kernel": "Linux", "networking": {"ip": "10.20.30.12"}},
				"classes": ["profile::node_exporter"]
			},
			{
				"certname": "old-vm.example.com",
				"environment": "production",
				"deactivated": "` + deactivatedTime + `",
				"facts": {"kernel": "Linux", "networking": {"ip": "10.20.30.99"}},
				"classes": ["profile::node_exporter"]
			}
		]`))
	}))
	defer mockPDB.Close()

	caCert, caKey, err := GenerateSelfSignedCA("Test OpenVox CA", 24*365*1000000000)
	if err != nil {
		t.Fatal(err)
	}
	caSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production-ca-tls",
			Namespace: "openvox",
		},
		Data: map[string][]byte{
			"ca.crt": caCert,
			"ca.key": caKey,
		},
	}

	cluster := &openvoxv1alpha1.VoxCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "openvox",
		},
		Status: openvoxv1alpha1.VoxClusterStatus{
			DatabaseURL:  mockPDB.URL,
			CASecretName: "production-ca-tls",
		},
	}

	disc := &openvoxv1alpha1.VoxDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "puppet-node-exporter",
			Namespace: "openvox",
		},
		Spec: openvoxv1alpha1.VoxDiscoverySpec{
			ClusterRef:  "production",
			AddressFact: "networking.ip",
			Selector: openvoxv1alpha1.DiscoverySelectorSpec{
				Environment: "production",
				Facts:       map[string]string{"kernel": "Linux"},
				Classes:     []string{"profile::node_exporter"},
			},
			Ports: []openvoxv1alpha1.DiscoveryPort{
				{Name: "metrics", Port: 9100, Protocol: corev1.ProtocolTCP},
			},
			ServiceMonitor: openvoxv1alpha1.DiscoveryServiceMonitorSpec{
				Enabled: true,
				TLS: openvoxv1alpha1.DiscoveryTLSSpec{
					Enabled: true,
					Mode:    "openvoxCA",
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(caSec, cluster, disc).
		WithStatusSubresource(&openvoxv1alpha1.VoxDiscovery{}).
		Build()

	reconciler := &VoxDiscoveryReconciler{
		Client:   k8sClient,
		Scheme:   s,
		DBClient: NewHTTPPuppetDBClient(mockPDB.Client()),
	}

	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "openvox", Name: "puppet-node-exporter"},
	})
	if err != nil {
		t.Fatalf("VoxDiscovery Reconcile failed: %v", err)
	}

	// 1. Verify EndpointSlice contains only the 2 active nodes (not old-vm.example.com)
	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "puppet-node-exporter-0"}, slice); err != nil {
		t.Fatalf("expected EndpointSlice puppet-node-exporter-0: %v", err)
	}
	if len(slice.Endpoints) != 2 {
		t.Fatalf("expected 2 active endpoints in EndpointSlice, got %d", len(slice.Endpoints))
	}
	if slice.Endpoints[0].Addresses[0] != "10.20.30.12" || *slice.Endpoints[0].NodeName != "db01.example.com" {
		t.Errorf("unexpected first endpoint: %+v", slice.Endpoints[0])
	}
	if slice.Endpoints[1].Addresses[0] != "10.20.30.11" || *slice.Endpoints[1].NodeName != "web01.example.com" {
		t.Errorf("unexpected second endpoint: %+v", slice.Endpoints[1])
	}

	// 2. Verify zero-touch prometheus-scraper mTLS Secret was minted from OpenVox CA
	scraperSec := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "openvox", Name: "puppet-node-exporter-prometheus-scraper-tls"}, scraperSec); err != nil {
		t.Fatalf("expected prometheus-scraper mTLS Secret: %v", err)
	}
	if len(scraperSec.Data["tls.crt"]) == 0 || len(scraperSec.Data["tls.key"]) == 0 || len(scraperSec.Data["ca.crt"]) == 0 {
		t.Error("expected prometheus-scraper Secret to contain ca.crt, tls.crt, and tls.key")
	}
}

func TestGitHubAppAuth_JWTAndTokenExchange(t *testing.T) {
	ctx := context.Background()
	_, keyPEM, err := GenerateSelfSignedCA("github-app-test", 24*365*1000000000)
	if err != nil {
		t.Fatal(err)
	}

	mockGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.Count(auth, ".") != 2 {
			t.Errorf("expected RS256 Bearer JWT, got %q", auth)
		}
		if r.URL.Path != "/app/installations/987654/access_tokens" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_mockInstallationToken123","expires_at":"2026-11-15T17:00:00Z"}`))
	}))
	defer mockGitHub.Close()

	exchanger := NewGitHubAppTokenExchanger(mockGitHub.Client())
	token, _, err := exchanger.ExchangeInstallationToken(ctx, mockGitHub.URL, 123456, 987654, keyPEM)
	if err != nil {
		t.Fatalf("ExchangeInstallationToken failed: %v", err)
	}
	if token != "ghs_mockInstallationToken123" {
		t.Errorf("expected token ghs_mockInstallationToken123, got %q", token)
	}
}

type mockGitPoller struct {
	sha string
}

func (m *mockGitPoller) ResolveHEAD(_ context.Context, _, _ string) (string, error) {
	return m.sha, nil
}
