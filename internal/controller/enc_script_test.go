package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// TestExternalENCScriptStatusHandling runs the rendered enc.sh against a stub curl: 200 prints the answer,
// 404 falls back to the default ENC, anything else (and a failed connection) fails the classification.
func TestExternalENCScriptStatusHandling(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	cluster := &openvoxv1alpha1.VoxCluster{Spec: openvoxv1alpha1.VoxClusterSpec{
		Workflows: openvoxv1alpha1.ClusterWorkflowsSpec{
			NodeClassification: &openvoxv1alpha1.WorkflowNodeClassificationSpec{
				Mode: "externalHttp", ExternalURL: "https://enc.example.com/nodes",
			},
		},
	}}
	script := strings.ReplaceAll(renderENCScript(cluster), "/etc/puppetlabs/puppet/", dir+"/")
	if err := os.WriteFile(filepath.Join(dir, "enc.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "enc-default.yaml"), []byte("environment: production\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stub curl: writes $STUB_BODY to the -o file and prints $STUB_CODE; exits $STUB_EXIT.
	stub := `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in -o) OUTFILE="$2"; shift ;; esac
  shift
done
printf '%s' "$STUB_BODY" > "$OUTFILE"
printf '%s' "$STUB_CODE"
exit "${STUB_EXIT:-0}"
`
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		code, body, exit string
		wantOut          string
		wantFail         bool
	}{
		"classified":   {code: "200", body: "environment: web\n", wantOut: "environment: web"},
		"unknown node": {code: "404", body: "not found", wantOut: "environment: production"},
		"server error": {code: "503", body: "down", wantFail: true},
		"unreachable":  {code: "000", exit: "7", wantFail: true},
	} {
		cmd := exec.Command("sh", filepath.Join(dir, "enc.sh"), "web01.example.com")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"),
			"STUB_CODE="+tc.code, "STUB_BODY="+tc.body, "STUB_EXIT="+tc.exit)
		out, err := cmd.Output()
		if tc.wantFail {
			if err == nil {
				t.Errorf("%s: expected enc.sh to fail, got %q", name, out)
			}
			continue
		}
		if err != nil || !strings.Contains(string(out), tc.wantOut) {
			t.Errorf("%s: expected %q, got %q (err %v)", name, tc.wantOut, out, err)
		}
	}
}
