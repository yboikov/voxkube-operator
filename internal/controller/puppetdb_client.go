package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// ErrNodeNotFound is returned when PuppetDB has no record of a certname.
var ErrNodeNotFound = errors.New("node not found in PuppetDB")

// DiscoveredPuppetNode represents an active node and its facts returned from OpenVox DB (PuppetDB).
type DiscoveredPuppetNode struct {
	Certname    string                 `json:"certname"`
	Environment string                 `json:"environment"`
	Deactivated *string                `json:"deactivated"`
	Facts       map[string]interface{} `json:"facts"`
	Trusted     map[string]interface{} `json:"trusted"`
}

// Fact resolves a dot-separated fact path. "trusted.*" paths fall back to the node's trusted data, which
// PuppetDB's inventory returns separately from facts.
func (n DiscoveredPuppetNode) Fact(path string) string {
	if v := LookupFactString(n.Facts, path); v != "" {
		return v
	}
	if rest, ok := strings.CutPrefix(path, "trusted."); ok {
		return LookupFactString(n.Trusted, rest)
	}
	return ""
}

// PuppetDBNode is the subset of /pdb/query/v4/nodes/<certname> used for VoxNode status.
type PuppetDBNode struct {
	Certname           string     `json:"certname"`
	Deactivated        *time.Time `json:"deactivated"`
	Expired            *time.Time `json:"expired"`
	ReportTimestamp    *time.Time `json:"report_timestamp"`
	FactsTimestamp     *time.Time `json:"facts_timestamp"`
	CatalogTimestamp   *time.Time `json:"catalog_timestamp"`
	LatestReportStatus string     `json:"latest_report_status"`
}

// LastSeen returns the most recent of the node's report, facts and catalog timestamps.
func (n *PuppetDBNode) LastSeen() *time.Time {
	var latest *time.Time
	for _, t := range []*time.Time{n.ReportTimestamp, n.FactsTimestamp, n.CatalogTimestamp} {
		if t != nil && (latest == nil || t.After(*latest)) {
			latest = t
		}
	}
	return latest
}

// OpenVoxDBClient interacts with OpenVox DB (PuppetDB).
type OpenVoxDBClient interface {
	// DeactivateNode sends the "deactivate node" v3 command to OpenVox DB (POST /pdb/cmd/v1).
	DeactivateNode(ctx context.Context, dbBaseURL, certname string) error
	// QueryActiveNodes returns active nodes matching the selector (PQL inventory query + fact filters).
	QueryActiveNodes(ctx context.Context, dbBaseURL string, selector openvoxv1alpha1.DiscoverySelectorSpec) ([]DiscoveredPuppetNode, error)
	// GetNode returns PuppetDB's record of a node, or ErrNodeNotFound.
	GetNode(ctx context.Context, dbBaseURL, certname string) (*PuppetDBNode, error)
}

// HTTPPuppetDBClient implements OpenVoxDBClient over HTTP/HTTPS.
type HTTPPuppetDBClient struct {
	HTTPClient *http.Client
}

// NewHTTPPuppetDBClient creates a new HTTPPuppetDBClient with a default timeout.
func NewHTTPPuppetDBClient(httpClient *http.Client) *HTTPPuppetDBClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPPuppetDBClient{HTTPClient: httpClient}
}

type pdbCommandEnvelope struct {
	Command string               `json:"command"`
	Version int                  `json:"version"`
	Payload pdbDeactivatePayload `json:"payload"`
}

type pdbDeactivatePayload struct {
	Certname          string `json:"certname"`
	ProducerTimestamp string `json:"producer_timestamp"`
}

// DeactivateNode sends POST /pdb/cmd/v1 with {"command": "deactivate node", "version": 3, ...}.
func (c *HTTPPuppetDBClient) DeactivateNode(ctx context.Context, dbBaseURL, certname string) error {
	if dbBaseURL == "" {
		return nil
	}
	u := fmt.Sprintf("%s/pdb/cmd/v1", strings.TrimRight(dbBaseURL, "/"))
	envelope := pdbCommandEnvelope{
		Command: "deactivate node",
		Version: 3,
		Payload: pdbDeactivatePayload{
			Certname:          certname,
			ProducerTimestamp: time.Now().UTC().Format(time.RFC3339),
		},
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PuppetDB deactivate node %s returned HTTP %d: %s", certname, resp.StatusCode, string(msg))
	}
	return nil
}

// GetNode queries GET /pdb/query/v4/nodes/<certname>.
func (c *HTTPPuppetDBClient) GetNode(ctx context.Context, dbBaseURL, certname string) (*PuppetDBNode, error) {
	if dbBaseURL == "" {
		return nil, fmt.Errorf("database URL is not known yet")
	}
	u := fmt.Sprintf("%s/pdb/query/v4/nodes/%s", strings.TrimRight(dbBaseURL, "/"), url.PathEscape(certname))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNodeNotFound
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("PuppetDB node query %s returned HTTP %d: %s", certname, resp.StatusCode, string(msg))
	}
	var n PuppetDBNode
	if err := json.NewDecoder(resp.Body).Decode(&n); err != nil {
		return nil, err
	}
	return &n, nil
}

// pqlString quotes s as a PQL string literal.
func pqlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// puppetClassTitle converts "profile::node_exporter" to the catalog resource title "Profile::Node_exporter".
func puppetClassTitle(class string) string {
	parts := strings.Split(strings.TrimPrefix(class, "::"), "::")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "::")
}

// BuildDiscoveryPQL renders the PQL inventory query for a VoxDiscovery selector.
// Environment, classes and the raw PQL expression are evaluated by PuppetDB; fact matches are applied client-side
// so that booleans and numbers compare as strings.
func BuildDiscoveryPQL(selector openvoxv1alpha1.DiscoverySelectorSpec) string {
	conds := []string{`certname in nodes[certname] { node_state = "active" }`}
	if selector.Environment != "" {
		conds = append(conds, "environment = "+pqlString(selector.Environment))
	}
	for _, cls := range selector.Classes {
		if strings.TrimSpace(cls) == "" {
			continue
		}
		conds = append(conds, fmt.Sprintf(`certname in resources[certname] { type = "Class" and title = %s }`, pqlString(puppetClassTitle(cls))))
	}
	if pql := strings.TrimSpace(selector.PQL); pql != "" {
		conds = append(conds, "("+pql+")")
	}
	return "inventory[certname, environment, facts, trusted] { " + strings.Join(conds, " and ") + " }"
}

// QueryActiveNodes runs the selector's PQL query against GET /pdb/query/v4 and applies fact filters.
func (c *HTTPPuppetDBClient) QueryActiveNodes(ctx context.Context, dbBaseURL string, selector openvoxv1alpha1.DiscoverySelectorSpec) ([]DiscoveredPuppetNode, error) {
	if dbBaseURL == "" {
		return nil, nil
	}
	u := fmt.Sprintf("%s/pdb/query/v4?query=%s", strings.TrimRight(dbBaseURL, "/"), url.QueryEscape(BuildDiscoveryPQL(selector)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("PuppetDB inventory query returned HTTP %d: %s", resp.StatusCode, string(msg))
	}

	var allNodes []DiscoveredPuppetNode
	if err := json.NewDecoder(resp.Body).Decode(&allNodes); err != nil {
		return nil, err
	}

	var matched []DiscoveredPuppetNode
	for _, node := range allNodes {
		if node.Deactivated != nil && *node.Deactivated != "" {
			continue
		}
		if !matchesDiscoveryFacts(node, selector) {
			continue
		}
		matched = append(matched, node)
	}
	return matched, nil
}

func matchesDiscoveryFacts(node DiscoveredPuppetNode, selector openvoxv1alpha1.DiscoverySelectorSpec) bool {
	if selector.Environment != "" && node.Environment != "" && node.Environment != selector.Environment {
		return false
	}
	for k, expected := range selector.Facts {
		if node.Fact(k) != expected {
			return false
		}
	}
	return true
}

// LookupFactString resolves a dot-separated fact path (e.g. "networking.ip" or "os.family") in a nested facts map.
func LookupFactString(facts map[string]interface{}, factPath string) string {
	if facts == nil || factPath == "" {
		return ""
	}
	if val, ok := facts[factPath]; ok {
		return fmt.Sprintf("%v", val)
	}
	parts := strings.Split(factPath, ".")
	var curr interface{} = facts
	for _, part := range parts {
		m, ok := curr.(map[string]interface{})
		if !ok {
			return ""
		}
		curr, ok = m[part]
		if !ok {
			return ""
		}
	}
	if curr == nil {
		return ""
	}
	return fmt.Sprintf("%v", curr)
}
