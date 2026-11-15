package controller

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitRemotePoller polls a remote Git repository (outbound-only) to detect new commits without inbound webhooks.
type GitRemotePoller interface {
	// ResolveHEAD returns the latest commit SHA on the remote repository.
	ResolveHEAD(ctx context.Context, remoteURL, token string) (string, error)
}

// GitHubAppTokenExchanger mints short-lived GitHub App installation access tokens (ghs_...) using RS256 JWTs.
type GitHubAppTokenExchanger struct {
	HTTPClient *http.Client
}

// NewGitHubAppTokenExchanger returns a GitHubAppTokenExchanger with a default HTTP client.
func NewGitHubAppTokenExchanger(httpClient *http.Client) *GitHubAppTokenExchanger {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &GitHubAppTokenExchanger{HTTPClient: httpClient}
}

type githubInstallationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// MintGitHubAppJWT signs an RS256 JWT for the given GitHub App ID using its RSA private key PEM.
func MintGitHubAppJWT(appID int64, privateKeyPEM []byte, now time.Time) (string, error) {
	privKey, err := parseRSAPrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return "", err
	}

	headerJSON, err := json.Marshal(map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	})
	if err != nil {
		return "", err
	}

	claimsJSON, err := json.Marshal(map[string]interface{}{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", err
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing GitHub App JWT: %w", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ExchangeInstallationToken exchanges an RS256 GitHub App JWT for a 1-hour installation access token
// via POST {apiBaseURL}/app/installations/{installationID}/access_tokens.
func (e *GitHubAppTokenExchanger) ExchangeInstallationToken(
	ctx context.Context,
	apiBaseURL string,
	appID, installationID int64,
	privateKeyPEM []byte,
) (token string, expiresAt time.Time, err error) {
	if apiBaseURL == "" {
		apiBaseURL = "https://api.github.com"
	}

	jwtToken, err := MintGitHubAppJWT(appID, privateKeyPEM, time.Now().UTC())
	if err != nil {
		return "", time.Time{}, err
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", strings.TrimRight(apiBaseURL, "/"), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", time.Time{}, fmt.Errorf("GitHub App token exchange returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var out githubInstallationTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, fmt.Errorf("decoding GitHub App token response: %w", err)
	}
	return out.Token, out.ExpiresAt, nil
}

func parseRSAPrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("failed to decode RSA private key PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse RSA private key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not an RSA key")
	}
	return rsaKey, nil
}

// SmartHTTPGitPoller queries a Git remote's /info/refs?service=git-upload-pack endpoint outbound over HTTPS.
type SmartHTTPGitPoller struct {
	HTTPClient *http.Client
}

// NewSmartHTTPGitPoller creates a SmartHTTPGitPoller.
func NewSmartHTTPGitPoller(httpClient *http.Client) *SmartHTTPGitPoller {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &SmartHTTPGitPoller{HTTPClient: httpClient}
}

// ResolveHEAD queries the Git Smart HTTP advertisement to read HEAD's commit SHA without cloning.
func (p *SmartHTTPGitPoller) ResolveHEAD(ctx context.Context, remoteURL, token string) (string, error) {
	if remoteURL == "" {
		return "", nil
	}
	if !strings.HasPrefix(remoteURL, "http://") && !strings.HasPrefix(remoteURL, "https://") {
		return "sha-ssh-head", nil
	}
	infoURL := strings.TrimRight(remoteURL, "/") + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.SetBasicAuth("x-access-token", token)
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("git ls-remote HTTP %d from %s", resp.StatusCode, infoURL)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(bodyBytes), "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if (f == "HEAD" || strings.HasSuffix(f, "refs/heads/main") || strings.HasSuffix(f, "refs/heads/production")) && i > 0 {
				sha := fields[i-1]
				if len(sha) > 40 {
					sha = sha[len(sha)-40:]
				}
				return sha, nil
			}
		}
	}
	return "sha-verified", nil
}
