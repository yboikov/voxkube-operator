package controller

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Certificate expiry metrics (#56). Expiry times are parsed while reconciling, from Secrets the controllers read
// anyway, and kept in memory: a scrape never reads Secrets or parses PEM.
const (
	metricCACertExpiry       = "voxkube_ca_certificate_expiry_timestamp_seconds"
	metricServiceCertExpiry  = "voxkube_service_certificate_expiry_timestamp_seconds"
	metricScraperCertExpiry  = "voxkube_discovery_scraper_certificate_expiry_timestamp_seconds"
	metricCRLNextUpdate      = "voxkube_crl_next_update_timestamp_seconds"
	metricNodeCertExpiry     = "voxkube_node_certificate_expiry_timestamp_seconds"
	serviceComponentCA       = "ca"
	serviceComponentServer   = "server"
	serviceComponentDatabase = "db"
)

var (
	descCACertExpiry = prometheus.NewDesc(metricCACertExpiry,
		"Unix time at which the VoxCluster's CA certificate expires.", []string{"namespace", "cluster"}, nil)
	descServiceCertExpiry = prometheus.NewDesc(metricServiceCertExpiry,
		"Unix time at which a VoxCluster service certificate expires (component ca, server or db). The operator renews them 30 days before.",
		[]string{"namespace", "cluster", "component"}, nil)
	descScraperCertExpiry = prometheus.NewDesc(metricScraperCertExpiry,
		"Unix time at which a VoxDiscovery's operator-minted Prometheus scraper certificate expires. The operator renews it 30 days before.",
		[]string{"namespace", "discovery", "cluster"}, nil)
	descCRLNextUpdate = prometheus.NewDesc(metricCRLNextUpdate,
		"Unix time of the nextUpdate field of the VoxCluster's current CRL.", []string{"namespace", "cluster"}, nil)
)

type clusterExpiry struct {
	ca, crlNextUpdate time.Time
	services          map[string]time.Time
}

type scraperExpiry struct {
	cluster  string
	notAfter time.Time
}

// expiryStore holds the expiry times recorded by the controllers, keyed by namespace/name.
type expiryStore struct {
	mu       sync.RWMutex
	clusters map[string]*clusterExpiry
	scrapers map[string]scraperExpiry
}

func newExpiryStore() *expiryStore {
	return &expiryStore{clusters: map[string]*clusterExpiry{}, scrapers: map[string]scraperExpiry{}}
}

// certExpiry is shared by the controllers and PKIMetricsCollector (like mtlsClients, one per process).
var certExpiry = newExpiryStore()

func (s *expiryStore) cluster(key string) *clusterExpiry {
	c := s.clusters[key]
	if c == nil {
		c = &clusterExpiry{services: map[string]time.Time{}}
		s.clusters[key] = c
	}
	return c
}

// recordCA records the CA certificate, the CRL and the CA endpoint's service certificate from <cluster>-ca-tls data.
func (s *expiryStore) recordCA(namespace, name string, data map[string][]byte) {
	ca, caOK := pemNotAfter(data["ca.crt"])
	crl, crlOK := crlNextUpdate(data["crl.pem"])
	svc, svcOK := pemNotAfter(data["tls.crt"])
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cluster(namespace + "/" + name)
	if caOK {
		c.ca = ca
	}
	if crlOK {
		c.crlNextUpdate = crl
	}
	if svcOK {
		c.services[serviceComponentCA] = svc
	}
}

// recordService records a workload certificate (component server or db).
func (s *expiryStore) recordService(namespace, name, component string, certPEM []byte) {
	t, ok := pemNotAfter(certPEM)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cluster(namespace + "/" + name).services[component] = t
}

// dropService forgets a component whose certificate is gone (e.g. OpenVox DB disabled).
func (s *expiryStore) dropService(namespace, name, component string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.clusters[namespace+"/"+name]; c != nil {
		delete(c.services, component)
	}
}

func (s *expiryStore) forgetCluster(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clusters, namespace+"/"+name)
}

func (s *expiryStore) recordScraper(namespace, discovery, cluster string, certPEM []byte) {
	t, ok := pemNotAfter(certPEM)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scrapers[namespace+"/"+discovery] = scraperExpiry{cluster: cluster, notAfter: t}
}

func (s *expiryStore) forgetScraper(namespace, discovery string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.scrapers, namespace+"/"+discovery)
}

func (s *expiryStore) metrics() []prometheus.Metric {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []prometheus.Metric
	unix := func(t time.Time) float64 { return float64(t.Unix()) }
	for _, key := range sortedGroupKeys(s.clusters) {
		ns, name, _ := strings.Cut(key, "/")
		c := s.clusters[key]
		if !c.ca.IsZero() {
			out = append(out, prometheus.MustNewConstMetric(descCACertExpiry, prometheus.GaugeValue, unix(c.ca), ns, name))
		}
		if !c.crlNextUpdate.IsZero() {
			out = append(out, prometheus.MustNewConstMetric(descCRLNextUpdate, prometheus.GaugeValue, unix(c.crlNextUpdate), ns, name))
		}
		for _, comp := range sortedGroupKeys(c.services) {
			out = append(out, prometheus.MustNewConstMetric(descServiceCertExpiry, prometheus.GaugeValue, unix(c.services[comp]), ns, name, comp))
		}
	}
	for _, key := range sortedGroupKeys(s.scrapers) {
		ns, disc, _ := strings.Cut(key, "/")
		sc := s.scrapers[key]
		out = append(out, prometheus.MustNewConstMetric(descScraperCertExpiry, prometheus.GaugeValue, unix(sc.notAfter), ns, disc, sc.cluster))
	}
	return out
}

// PKIMetricsCollector exports the expiry times recorded in certExpiry. It reads only memory.
type PKIMetricsCollector struct{}

// Describe implements prometheus.Collector.
func (PKIMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descCACertExpiry, descServiceCertExpiry, descScraperCertExpiry, descCRLNextUpdate} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (PKIMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range certExpiry.metrics() {
		ch <- m
	}
}

// pemNotAfter returns the expiry of the first certificate in a PEM bundle (the leaf, or the CA itself).
func pemNotAfter(certPEM []byte) (time.Time, bool) {
	for rest := certPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return time.Time{}, false
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, false
		}
		return cert.NotAfter, true
	}
}

func crlNextUpdate(crlPEM []byte) (time.Time, bool) {
	block, _ := pem.Decode(crlPEM)
	if block == nil {
		return time.Time{}, false
	}
	crl, err := x509.ParseRevocationList(block.Bytes)
	if err != nil || crl.NextUpdate.IsZero() {
		return time.Time{}, false
	}
	return crl.NextUpdate, true
}

// parseCATime parses the not_after of a Puppet CA certificate_status response ("2031-02-23T21:06:52UTC").
func parseCATime(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05MST", time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05 MST"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
