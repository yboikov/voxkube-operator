package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

type gathered map[string][]map[string]string // metric -> series (labels plus "value")

func gatherFrom(t *testing.T, c prometheus.Collector) gathered {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather() // fails on duplicate series
	if err != nil {
		t.Fatal(err)
	}
	out := gathered{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			ls := map[string]string{}
			for _, lp := range m.GetLabel() {
				ls[lp.GetName()] = lp.GetValue()
			}
			ls["value"] = time.Unix(int64(m.GetGauge().GetValue()), 0).UTC().Format(time.RFC3339)
			out[f.GetName()] = append(out[f.GetName()], ls)
		}
	}
	return out
}

func testCA(t *testing.T) (caCrt, caKey, crl []byte) {
	t.Helper()
	caCrt, caKey, err := GenerateSelfSignedCA("test CA", 5*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, crl, err = GenerateCAArtifacts(caCrt, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return caCrt, caKey, crl
}

func leaf(t *testing.T, caCrt, caKey []byte, cn string, d time.Duration) []byte {
	t.Helper()
	c, _, err := IssueLeafCertificate(caCrt, caKey, LeafCertOptions{CommonName: cn, DNSNames: []string{cn}, Duration: d})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExpiryStoreMetrics(t *testing.T) {
	old := certExpiry
	certExpiry = newExpiryStore()
	defer func() { certExpiry = old }()

	caCrt, caKey, crl := testCA(t)
	caNotAfter, _ := pemNotAfter(caCrt)
	crlNext, ok := crlNextUpdate(crl)
	if !ok {
		t.Fatal("CRL nextUpdate not parsed")
	}
	certExpiry.recordCA("ovx", "prod", map[string][]byte{"ca.crt": caCrt, "crl.pem": crl, "tls.crt": leaf(t, caCrt, caKey, "prod.ovx.svc", 30*24*time.Hour)})
	srv := leaf(t, caCrt, caKey, "srv", 60*24*time.Hour)
	certExpiry.recordService("ovx", "prod", serviceComponentServer, srv)
	certExpiry.recordService("ovx", "prod", serviceComponentDatabase, leaf(t, caCrt, caKey, "db", 90*24*time.Hour))
	// Two VoxDiscoveries on one cluster: distinct series, no duplicate error.
	certExpiry.recordScraper("ovx", "nodes", "prod", leaf(t, caCrt, caKey, "prometheus-scraper", 365*24*time.Hour))
	certExpiry.recordScraper("ovx", "nodes-dc2", "prod", leaf(t, caCrt, caKey, "prometheus-scraper", 300*24*time.Hour))

	g := gatherFrom(t, PKIMetricsCollector{})
	if s := g[metricCACertExpiry]; len(s) != 1 || s[0]["value"] != caNotAfter.UTC().Format(time.RFC3339) || s[0]["cluster"] != "prod" {
		t.Errorf("CA expiry: %v", s)
	}
	if s := g[metricCRLNextUpdate]; len(s) != 1 || s[0]["value"] != crlNext.UTC().Format(time.RFC3339) {
		t.Errorf("CRL nextUpdate: %v", s)
	}
	comps := map[string]bool{}
	for _, s := range g[metricServiceCertExpiry] {
		comps[s["component"]] = true
	}
	if len(comps) != 3 || !comps["ca"] || !comps["server"] || !comps["db"] {
		t.Errorf("service components: %v", g[metricServiceCertExpiry])
	}
	if s := g[metricScraperCertExpiry]; len(s) != 2 || s[0]["discovery"] == s[1]["discovery"] || s[0]["cluster"] != "prod" {
		t.Errorf("scraper series: %v", s)
	}

	certExpiry.dropService("ovx", "prod", serviceComponentDatabase)
	certExpiry.forgetScraper("ovx", "nodes-dc2")
	g = gatherFrom(t, PKIMetricsCollector{})
	if len(g[metricServiceCertExpiry]) != 2 || len(g[metricScraperCertExpiry]) != 1 {
		t.Errorf("after drop/forget: %v / %v", g[metricServiceCertExpiry], g[metricScraperCertExpiry])
	}
	certExpiry.forgetCluster("ovx", "prod")
	if g = gatherFrom(t, PKIMetricsCollector{}); len(g[metricCACertExpiry]) != 0 || len(g[metricServiceCertExpiry]) != 0 {
		t.Errorf("forgotten cluster still exported: %v", g)
	}
}

// The reconcile records what it reads anyway; the collector itself has no client, so a scrape can't touch Secrets.
func TestReconcileRecordsClusterExpiry(t *testing.T) {
	old := certExpiry
	certExpiry = newExpiryStore()
	defer func() { certExpiry = old }()

	s := newVoxKubeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pkiCluster()).WithStatusSubresource(&openvoxv1alpha1.VoxCluster{}).Build()
	r := &VoxClusterReconciler{Client: c, Scheme: s, GitPoller: &mockGitPoller{sha: "x"}}
	reconcilePKICluster(t, c, r)

	g := gatherFrom(t, PKIMetricsCollector{})
	want := map[string]time.Time{}
	for comp, sec := range map[string]string{"ca": "prod-ca-tls", "server": "prod-server-tls", "db": "prod-db-tls"} {
		na, ok := pemNotAfter(getSecret(t, c, sec).Data["tls.crt"])
		if !ok {
			t.Fatalf("%s: no certificate", sec)
		}
		want[comp] = na
	}
	for _, series := range g[metricServiceCertExpiry] {
		if series["value"] != want[series["component"]].UTC().Format(time.RFC3339) {
			t.Errorf("%s expiry %s, want %s", series["component"], series["value"], want[series["component"]])
		}
		delete(want, series["component"])
	}
	if len(want) != 0 {
		t.Errorf("missing service certificate series: %v", want)
	}
	if len(g[metricCACertExpiry]) != 1 || len(g[metricCRLNextUpdate]) != 1 {
		t.Errorf("CA/CRL series: %v %v", g[metricCACertExpiry], g[metricCRLNextUpdate])
	}
}

func TestParseCATimeAndNodeExpiry(t *testing.T) {
	want := time.Date(2031, 2, 23, 21, 6, 52, 0, time.UTC)
	for _, s := range []string{"2031-02-23T21:06:52UTC", "2031-02-23T21:06:52Z", "2031-02-23T21:06:52+00:00"} {
		if got, ok := parseCATime(s); !ok || !got.Equal(want) {
			t.Errorf("parseCATime(%q) = %v %v", s, got, ok)
		}
	}
	if _, ok := parseCATime(""); ok {
		t.Error("empty not_after must not parse")
	}
	prev := metav1.NewTime(want)
	if got := caNotAfter("2031-02-23T21:06:52UTC", &prev); got != &prev {
		t.Error("an unchanged expiry must keep the previous value (no status churn)")
	}

	node := openvoxv1alpha1.VoxNode{
		ObjectMeta: metav1.ObjectMeta{Name: "web01", Namespace: "ovx"},
		Spec:       openvoxv1alpha1.VoxNodeSpec{ClusterRef: "prod", Certname: "web01.example.com"},
		Status:     openvoxv1alpha1.VoxNodeStatus{CertificateNotAfter: &prev},
	}
	var found bool
	for _, s := range nodeMetricSamples([]openvoxv1alpha1.VoxNode{node}, nil) {
		if s.metric == metricNodeCertExpiry {
			found = s.value == float64(want.Unix()) && s.labels["certname"] == "web01.example.com"
		}
	}
	if !found {
		t.Error("expected voxkube_node_certificate_expiry_timestamp_seconds for web01")
	}
	node.Status.CertificateNotAfter = nil
	for _, s := range nodeMetricSamples([]openvoxv1alpha1.VoxNode{node}, nil) {
		if s.metric == metricNodeCertExpiry {
			t.Error("no expiry series for a node without a certificate")
		}
	}
}
