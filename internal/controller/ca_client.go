package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// OpenVoxCAClient interacts with the OpenVox CA HTTP API for signing CSRs and cleaning/revoking certificates.
type OpenVoxCAClient interface {
	// SignCSR signs a pending CSR for certname on the OpenVox CA.
	SignCSR(ctx context.Context, caBaseURL, certname string) (fingerprint string, err error)
	// RevokeCert revokes a node's certificate on the OpenVox CA without deleting it.
	RevokeCert(ctx context.Context, caBaseURL, certname string) error
	// CleanNode revokes and removes a node's certificate from the OpenVox CA (PUT /puppet-ca/v1/clean).
	CleanNode(ctx context.Context, caBaseURL, certname string) error
}

// HTTPCAClient implements OpenVoxCAClient over HTTP/HTTPS.
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

type caCertStatusResponse struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Fingerprint string `json:"fingerprint"`
}

// SignCSR checks certificate status or signs a pending CSR via PUT /puppet-ca/v1/certificate_status/<certname>.
func (c *HTTPCAClient) SignCSR(ctx context.Context, caBaseURL, certname string) (string, error) {
	if caBaseURL == "" {
		return "sha256:local-" + certname, nil
	}
	url := fmt.Sprintf("%s/puppet-ca/v1/certificate_status/%s", strings.TrimRight(caBaseURL, "/"), certname)

	// First check if already signed or pending
	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	getResp, err := c.HTTPClient.Do(getReq)
	if err == nil {
		defer func() { _ = getResp.Body.Close() }()
		if getResp.StatusCode == http.StatusOK {
			var status caCertStatusResponse
			if err := json.NewDecoder(getResp.Body).Decode(&status); err == nil {
				if status.State == "signed" {
					return status.Fingerprint, nil
				}
			}
		}
	}

	// Sign the CSR
	body, err := json.Marshal(caStatusUpdate{DesiredState: "signed"})
	if err != nil {
		return "", err
	}
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	putReq.Header.Set("Content-Type", "application/json")
	putResp, err := c.HTTPClient.Do(putReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = putResp.Body.Close() }()

	if putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(putResp.Body)
		return "", fmt.Errorf("CA sign CSR for %s returned HTTP %d: %s", certname, putResp.StatusCode, string(msg))
	}
	return "sha256:signed-" + certname, nil
}

// RevokeCert revokes a node certificate via PUT /puppet-ca/v1/certificate_status/<certname> {"desired_state":"revoked"}.
func (c *HTTPCAClient) RevokeCert(ctx context.Context, caBaseURL, certname string) error {
	if caBaseURL == "" {
		return nil
	}
	url := fmt.Sprintf("%s/puppet-ca/v1/certificate_status/%s", strings.TrimRight(caBaseURL, "/"), certname)
	body, err := json.Marshal(caStatusUpdate{DesiredState: "revoked"})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("CA revoke cert for %s returned HTTP %d: %s", certname, resp.StatusCode, string(msg))
	}
	return nil
}

// CleanNode revokes and cleans a node certificate via PUT /puppet-ca/v1/clean.
func (c *HTTPCAClient) CleanNode(ctx context.Context, caBaseURL, certname string) error {
	if caBaseURL == "" {
		return nil
	}
	url := fmt.Sprintf("%s/puppet-ca/v1/clean", strings.TrimRight(caBaseURL, "/"))
	body, err := json.Marshal(caCleanRequest{Certnames: []string{certname}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("CA clean for %s returned HTTP %d: %s", certname, resp.StatusCode, string(msg))
	}
	return nil
}

// GenerateSelfSignedCA generates a root X.509 CA certificate and private key PEM for bootstrapping VoxCluster.
func GenerateSelfSignedCA(commonName string, duration time.Duration) (certPEM, keyPEM []byte, err error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"VoxKube OpenVox CA"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(duration),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM, nil
}

// IssueClientCertificate signs a leaf certificate (e.g., prometheus-scraper or compiler TLS) using the cluster CA.
func IssueClientCertificate(caCertPEM, caKeyPEM []byte, commonName string, dnsNames []string, duration time.Duration) (certPEM, keyPEM []byte, err error) {
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

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"VoxKube"},
		},
		DNSNames:    dnsNames,
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(duration),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &priv.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM, nil
}
