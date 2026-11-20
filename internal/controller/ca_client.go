package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrCertNotFound is returned when the CA has neither a CSR nor a certificate for a certname.
var ErrCertNotFound = errors.New("certificate or CSR not found on CA")

// CertStatus is the subset of the Puppet CA certificate_status response used by the operator.
type CertStatus struct {
	Name        string `json:"name"`
	State       string `json:"state"` // requested | signed | revoked
	Fingerprint string `json:"fingerprint"`
}

// OpenVoxCAClient interacts with the OpenVox CA HTTP API.
type OpenVoxCAClient interface {
	// GetCertStatus returns the CA state of certname, or ErrCertNotFound.
	GetCertStatus(ctx context.Context, caBaseURL, certname string) (*CertStatus, error)
	// ListCertStatuses lists all CSRs and certificates known to the CA.
	ListCertStatuses(ctx context.Context, caBaseURL string) ([]CertStatus, error)
	// SignCSR signs a pending CSR for certname and returns the resulting certificate status.
	SignCSR(ctx context.Context, caBaseURL, certname string) (*CertStatus, error)
	// RevokeCert revokes a node's certificate on the OpenVox CA without deleting it.
	RevokeCert(ctx context.Context, caBaseURL, certname string) error
	// CleanNode revokes and removes a node's certificate from the OpenVox CA (PUT /puppet-ca/v1/clean).
	CleanNode(ctx context.Context, caBaseURL, certname string) error
	// FetchCRL downloads the current CA certificate revocation list (PEM).
	FetchCRL(ctx context.Context, caBaseURL string) ([]byte, error)
	// GetCertificatePEM downloads certname's signed certificate (PEM), or returns ErrCertNotFound.
	GetCertificatePEM(ctx context.Context, caBaseURL, certname string) ([]byte, error)
	// GetCertificateRequestPEM downloads certname's pending CSR (PEM), or returns ErrCertNotFound.
	GetCertificateRequestPEM(ctx context.Context, caBaseURL, certname string) ([]byte, error)
}

// HTTPCAClient implements OpenVoxCAClient over HTTPS.
type HTTPCAClient struct {
	HTTPClient *http.Client
}

// NewHTTPCAClient creates a new HTTPCAClient with a default timeout.
func NewHTTPCAClient(httpClient *http.Client) *HTTPCAClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPCAClient{HTTPClient: httpClient}
}

// NewMTLSHTTPClient builds an *http.Client that trusts the provided OpenVox CA PEM and presents the client TLS certificate/key.
func NewMTLSHTTPClient(caCertPEM, clientCertPEM, clientKeyPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("failed to parse CA certificate PEM")
	}
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
	}
	if len(clientCertPEM) > 0 && len(clientKeyPEM) > 0 {
		cert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
		if err != nil {
			return nil, err
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

type caStatusUpdate struct {
	DesiredState string `json:"desired_state"`
}

type caCleanRequest struct {
	Certnames []string `json:"certnames"`
}

func caURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

func (c *HTTPCAClient) doJSON(ctx context.Context, method, u string, body interface{}) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.HTTPClient.Do(req)
}

// GetCertStatus implements OpenVoxCAClient (GET /puppet-ca/v1/certificate_status/<certname>).
func (c *HTTPCAClient) GetCertStatus(ctx context.Context, caBaseURL, certname string) (*CertStatus, error) {
	if caBaseURL == "" {
		return nil, fmt.Errorf("CA URL is not known yet")
	}
	resp, err := c.doJSON(ctx, http.MethodGet, caURL(caBaseURL, "/puppet-ca/v1/certificate_status/"+url.PathEscape(certname)), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrCertNotFound
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("CA certificate_status %s returned HTTP %d: %s", certname, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var st CertStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	if st.Name == "" {
		st.Name = certname
	}
	return &st, nil
}

// ListCertStatuses implements OpenVoxCAClient (GET /puppet-ca/v1/certificate_statuses/any_key).
func (c *HTTPCAClient) ListCertStatuses(ctx context.Context, caBaseURL string) ([]CertStatus, error) {
	if caBaseURL == "" {
		return nil, fmt.Errorf("CA URL is not known yet")
	}
	resp, err := c.doJSON(ctx, http.MethodGet, caURL(caBaseURL, "/puppet-ca/v1/certificate_statuses/any_key"), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("CA certificate_statuses returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out []CertStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// SignCSR implements OpenVoxCAClient. It is idempotent: an already signed certificate is returned as-is.
func (c *HTTPCAClient) SignCSR(ctx context.Context, caBaseURL, certname string) (*CertStatus, error) {
	st, err := c.GetCertStatus(ctx, caBaseURL, certname)
	if err != nil {
		return nil, err
	}
	switch st.State {
	case "signed":
		return st, nil
	case "revoked":
		return st, fmt.Errorf("certificate for %s is revoked on the CA; clean it and let the agent submit a new CSR", certname)
	}

	resp, err := c.doJSON(ctx, http.MethodPut, caURL(caBaseURL, "/puppet-ca/v1/certificate_status/"+url.PathEscape(certname)), caStatusUpdate{DesiredState: "signed"})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("CA sign CSR for %s returned HTTP %d: %s", certname, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return c.GetCertStatus(ctx, caBaseURL, certname)
}

// RevokeCert revokes a node certificate via PUT /puppet-ca/v1/certificate_status/<certname> {"desired_state":"revoked"}.
func (c *HTTPCAClient) RevokeCert(ctx context.Context, caBaseURL, certname string) error {
	if caBaseURL == "" {
		return nil
	}
	resp, err := c.doJSON(ctx, http.MethodPut, caURL(caBaseURL, "/puppet-ca/v1/certificate_status/"+url.PathEscape(certname)), caStatusUpdate{DesiredState: "revoked"})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("CA revoke cert for %s returned HTTP %d: %s", certname, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// CleanNode revokes and cleans a node certificate via PUT /puppet-ca/v1/clean.
func (c *HTTPCAClient) CleanNode(ctx context.Context, caBaseURL, certname string) error {
	if caBaseURL == "" {
		return nil
	}
	resp, err := c.doJSON(ctx, http.MethodPut, caURL(caBaseURL, "/puppet-ca/v1/clean"), caCleanRequest{Certnames: []string{certname}})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("CA clean for %s returned HTTP %d: %s", certname, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// FetchCRL implements OpenVoxCAClient (GET /puppet-ca/v1/certificate_revocation_list/ca).
func (c *HTTPCAClient) FetchCRL(ctx context.Context, caBaseURL string) ([]byte, error) {
	if caBaseURL == "" {
		return nil, fmt.Errorf("CA URL is not known yet")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, caURL(caBaseURL, "/puppet-ca/v1/certificate_revocation_list/ca"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CA CRL download returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !bytes.Contains(body, []byte("BEGIN X509 CRL")) {
		return nil, fmt.Errorf("CA CRL download returned no PEM CRL")
	}
	return body, nil
}

// GetCertificatePEM implements OpenVoxCAClient (GET /puppet-ca/v1/certificate/<certname>).
func (c *HTTPCAClient) GetCertificatePEM(ctx context.Context, caBaseURL, certname string) ([]byte, error) {
	return c.fetchPEM(ctx, caBaseURL, "/puppet-ca/v1/certificate/"+url.PathEscape(certname), "certificate "+certname)
}

// GetCertificateRequestPEM implements OpenVoxCAClient (GET /puppet-ca/v1/certificate_request/<certname>).
func (c *HTTPCAClient) GetCertificateRequestPEM(ctx context.Context, caBaseURL, certname string) ([]byte, error) {
	return c.fetchPEM(ctx, caBaseURL, "/puppet-ca/v1/certificate_request/"+url.PathEscape(certname), "certificate request "+certname)
}

func (c *HTTPCAClient) fetchPEM(ctx context.Context, caBaseURL, path, what string) ([]byte, error) {
	if caBaseURL == "" {
		return nil, fmt.Errorf("CA URL is not known yet")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, caURL(caBaseURL, path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrCertNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CA %s returned HTTP %d: %s", what, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// PKI helpers
// ---------------------------------------------------------------------------

// oidPuppetCLIAuth is the Puppet pp_cli_auth extension; certificates carrying it with value "true"
// may use the CA admin API (certificate_status, certificate_statuses, clean) under the default auth.conf.
var oidPuppetCLIAuth = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 34380, 1, 3, 39}

func randomSerial() (*big.Int, error) {
	// 127-bit random serials never collide with the small sequential serials issued by the Puppet CA.
	limit := new(big.Int).Lsh(big.NewInt(1), 127)
	return rand.Int(rand.Reader, limit)
}

// ParseDurationWithDays parses Go durations plus "d" (24h) and "y" (365d) suffixes, e.g. "10y", "90d", "720h".
func ParseDurationWithDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	unit := s[len(s)-1]
	if unit == 'y' || unit == 'd' {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		day := 24 * time.Hour
		if unit == 'y' {
			return time.Duration(n * float64(365*day)), nil
		}
		return time.Duration(n * float64(day)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive: %q", s)
	}
	return d, nil
}

// GenerateSelfSignedCA generates a root X.509 CA certificate and private key PEM for bootstrapping VoxCluster.
func GenerateSelfSignedCA(commonName string, duration time.Duration) (certPEM, keyPEM []byte, err error) {
	priv, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, nil, err
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	ski := sha1.Sum(pubDER)

	// Serial 1 matches what the Puppet CA uses for its own root.
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: commonName,
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(duration),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		SubjectKeyId:          ski[:],
		AuthorityKeyId:        ski[:],
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM, nil
}

// ExtractPublicKeyPEM extracts the PKIX PUBLIC KEY PEM from an RSA PRIVATE KEY PEM.
func ExtractPublicKeyPEM(keyPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode private key PEM")
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), nil
}

func parseCAPair(caCertPEM, caKeyPEM []byte) (*x509.Certificate, *rsa.PrivateKey, error) {
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode CA certificate PEM")
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode CA private key PEM")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return caCert, caKey, nil
}

// GenerateCAArtifacts derives the PEM-encoded public key (ca_pub.pem) and an empty X.509 v2 CRL (ca_crl.pem) from a CA cert/key pair.
func GenerateCAArtifacts(caCertPEM, caKeyPEM []byte) (pubPEM, crlPEM []byte, err error) {
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&caKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	pubPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	now := time.Now().UTC()
	crlTmpl := &x509.RevocationList{
		Number:     big.NewInt(1),
		ThisUpdate: now.Add(-5 * time.Minute),
		NextUpdate: now.Add(5 * 365 * 24 * time.Hour),
	}
	crlDER, err := x509.CreateRevocationList(rand.Reader, crlTmpl, caCert, caKey)
	if err != nil {
		return nil, nil, err
	}
	crlPEM = pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: crlDER})
	return pubPEM, crlPEM, nil
}

// LeafCertOptions describes a leaf certificate issued by the cluster CA.
type LeafCertOptions struct {
	CommonName string
	DNSNames   []string
	Duration   time.Duration
	// CLIAuth adds the Puppet pp_cli_auth extension, granting access to the CA admin API.
	CLIAuth bool
}

// IssueLeafCertificate signs a client/server leaf certificate with the cluster CA.
func IssueLeafCertificate(caCertPEM, caKeyPEM []byte, opts LeafCertOptions) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := parseCAPair(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	ski := sha1.Sum(pubDER)

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:   serial,
		Subject:        pkix.Name{CommonName: opts.CommonName},
		DNSNames:       opts.DNSNames,
		NotBefore:      now.Add(-5 * time.Minute),
		NotAfter:       now.Add(opts.Duration),
		KeyUsage:       x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		SubjectKeyId:   ski[:],
		AuthorityKeyId: caCert.SubjectKeyId,
	}
	if opts.CLIAuth {
		// Puppet encodes extension values as UTF8String.
		val, err := asn1.MarshalWithParams("true", "utf8")
		if err != nil {
			return nil, nil, err
		}
		tmpl.ExtraExtensions = []pkix.Extension{{Id: oidPuppetCLIAuth, Value: val}}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &priv.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM, nil
}

// IssueClientCertificate signs a leaf certificate without CA admin rights. Kept for API compatibility.
func IssueClientCertificate(caCertPEM, caKeyPEM []byte, commonName string, dnsNames []string, duration time.Duration) (certPEM, keyPEM []byte, err error) {
	return IssueLeafCertificate(caCertPEM, caKeyPEM, LeafCertOptions{CommonName: commonName, DNSNames: dnsNames, Duration: duration})
}

// LeafNeedsRenewal reports whether certPEM is missing, unparsable, not signed by caCertPEM,
// expires within renewBefore, or does not carry exactly the wanted DNS SANs.
func LeafNeedsRenewal(certPEM, caCertPEM []byte, wantDNS []string, renewBefore time.Duration) (bool, string) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return true, "certificate missing"
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true, "certificate unparsable"
	}
	if caBlock, _ := pem.Decode(caCertPEM); caBlock != nil {
		if caCert, err := x509.ParseCertificate(caBlock.Bytes); err == nil {
			if err := cert.CheckSignatureFrom(caCert); err != nil {
				return true, "certificate not signed by current CA"
			}
		}
	}
	if time.Until(cert.NotAfter) < renewBefore {
		return true, "certificate expires soon"
	}
	if wantDNS != nil && !sameStringSet(cert.DNSNames, wantDNS) {
		return true, "DNS SANs changed"
	}
	return false, ""
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
