package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// DiscoveredPuppetNode represents an active node and its facts returned from OpenVox DB (PuppetDB).
type DiscoveredPuppetNode struct {
	Certname    string                 `json:"certname"`
	Environment string                 `json:"environment"`
	Deactivated *string                `json:"deactivated"`
	Facts       map[string]interface{} `json:"facts"`
	Classes     []string               `json:"classes,omitempty"`
}

// OpenVoxDBClient interacts with OpenVox DB (PuppetDB) for node deactivation and inventory discovery.
type OpenVoxDBClient interface {
	// DeactivateNode sends the "deactivate node" v3 command to OpenVox DB (POST /pdb/cmd/v1).
	DeactivateNode(ctx context.Context, dbBaseURL, certname string) error
	// QueryActiveNodes queries /pdb/query/v4/inventory for active nodes matching the selector.
	QueryActiveNodes(ctx context.Context, dbBaseURL string, selector openvoxv1alpha1.DiscoverySelectorSpec) ([]DiscoveredPuppetNode, error)
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
	url := fmt.Sprintf("%s/pdb/cmd/v1", strings.TrimRight(dbBaseURL, "/"))
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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

// QueryActiveNodes queries /pdb/query/v4/inventory and filters active nodes by environment, facts, and classes.
func (c *HTTPPuppetDBClient) QueryActiveNodes(ctx context.Context, dbBaseURL string, selector openvoxv1alpha1.DiscoverySelectorSpec) ([]DiscoveredPuppetNode, error) {
	if dbBaseURL == "" {
		return nil, nil
	}
	url := fmt.Sprintf("%s/pdb/query/v4/inventory", strings.TrimRight(dbBaseURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		if !matchesDiscoverySelector(node, selector) {
			continue
		}
		matched = append(matched, node)
	}
	return matched, nil
}

func matchesDiscoverySelector(node DiscoveredPuppetNode, selector openvoxv1alpha1.DiscoverySelectorSpec) bool {
	if selector.Environment != "" && node.Environment != selector.Environment {
		return false
	}
	for k, expected := range selector.Facts {
		actual := LookupFactString(node.Facts, k)
		if actual != expected {
			return false
		}
	}
	if len(selector.Classes) > 0 {
		classSet := make(map[string]bool, len(node.Classes))
		for _, cls := range node.Classes {
			classSet[cls] = true
		}
		for _, reqCls := range selector.Classes {
			if !classSet[reqCls] {
				return false
			}
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
