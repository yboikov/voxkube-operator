package controller

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GitAuth carries HTTP basic credentials for polling (GitHub App and token auth use
// username "x-access-token" and the token as password).
type GitAuth struct {
	Username string
	Password string
}

// GitRevision is the result of polling a remote.
type GitRevision struct {
	// Head is the commit SHA of HEAD (the default branch).
	Head string
	// RefsHash is a stable hash over every refs/heads/* entry; it changes whenever any branch moves.
	RefsHash string
	// Branches maps branch names (without refs/heads/) to their commit SHA.
	Branches map[string]string
}

// SelectBranches narrows rev to the given branches (all branches when the list is empty): RefsHash covers only
// those, and Head is the commit of the first listed branch that exists. It fails when none of them exist, so a
// typo or a deleted branch never looks like "no change".
func SelectBranches(rev GitRevision, branches []string) (GitRevision, error) {
	if len(branches) == 0 {
		return rev, nil
	}
	var heads []string
	head := ""
	for _, b := range branches {
		sha, ok := rev.Branches[b]
		if !ok {
			continue
		}
		if head == "" {
			head = sha
		}
		heads = append(heads, "refs/heads/"+b+" "+sha)
	}
	if len(heads) == 0 {
		return GitRevision{}, fmt.Errorf("none of the configured branches %v exist on the remote", branches)
	}
	selected := map[string]string{}
	for _, b := range branches {
		if sha, ok := rev.Branches[b]; ok {
			selected[b] = sha
		}
	}
	return GitRevision{Head: head, RefsHash: hashHeads(heads), Branches: selected}, nil
}

// hashHeads is the RefsHash of "refs/heads/<name> <sha>" entries.
func hashHeads(heads []string) string {
	sorted := append([]string{}, heads...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// GitRemotePoller polls a remote Git repository (outbound-only) to detect new commits without inbound webhooks.
type GitRemotePoller interface {
	// Resolve returns the current revision of the remote. Only http(s) remotes are supported.
	Resolve(ctx context.Context, remoteURL string, auth *GitAuth) (GitRevision, error)
}

// IsHTTPRemote reports whether the operator can poll remoteURL itself.
func IsHTTPRemote(remoteURL string) bool {
	return strings.HasPrefix(remoteURL, "http://") || strings.HasPrefix(remoteURL, "https://")
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

// Resolve queries the Git Smart HTTP ref advertisement (the equivalent of git ls-remote) without cloning.
func (p *SmartHTTPGitPoller) Resolve(ctx context.Context, remoteURL string, auth *GitAuth) (GitRevision, error) {
	if !IsHTTPRemote(remoteURL) {
		return GitRevision{}, fmt.Errorf("only http(s) remotes can be polled, got %q", remoteURL)
	}
	infoURL := strings.TrimRight(remoteURL, "/") + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return GitRevision{}, err
	}
	req.Header.Set("Git-Protocol", "version=1")
	if auth != nil && auth.Password != "" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return GitRevision{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return GitRevision{}, fmt.Errorf("git ref advertisement returned HTTP %d from %s", resp.StatusCode, infoURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return GitRevision{}, err
	}
	return ParseRefAdvertisement(body)
}

// ParseRefAdvertisement parses a git-upload-pack smart HTTP ref advertisement (pkt-line format).
func ParseRefAdvertisement(body []byte) (GitRevision, error) {
	refs := map[string]string{}
	for len(body) >= 4 {
		n, err := strconv.ParseUint(string(body[:4]), 16, 16)
		if err != nil {
			return GitRevision{}, fmt.Errorf("malformed pkt-line length %q", string(body[:4]))
		}
		if n == 0 { // flush-pkt
			body = body[4:]
			continue
		}
		if int(n) > len(body) || n < 4 {
			return GitRevision{}, fmt.Errorf("truncated pkt-line")
		}
		line := string(body[4:n])
		body = body[n:]
		line = strings.TrimRight(line, "\n")
		if i := strings.IndexByte(line, 0); i >= 0 { // strip capabilities
			line = line[:i]
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) < 40 {
			continue
		}
		refs[fields[1]] = fields[0]
	}

	var heads []string
	branches := map[string]string{}
	for ref, sha := range refs {
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			heads = append(heads, ref+" "+sha)
			branches[name] = sha
		}
	}
	if len(heads) == 0 && refs["HEAD"] == "" {
		return GitRevision{}, fmt.Errorf("remote advertised no branches")
	}

	head := refs["HEAD"]
	if head == "" {
		for _, b := range []string{"refs/heads/main", "refs/heads/master", "refs/heads/production"} {
			if refs[b] != "" {
				head = refs[b]
				break
			}
		}
	}
	return GitRevision{Head: head, RefsHash: hashHeads(heads), Branches: branches}, nil
}
