package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestPinnedOpenVoxVersionsAreConsistent fails when the OpenVox versions drift between the
// Containerfiles, the operator defaults and the Helm chart.
func TestPinnedOpenVoxVersionsAreConsistent(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		return string(b)
	}
	expectAll := func(rel, pattern, want string) {
		matches := regexp.MustCompile(pattern).FindAllStringSubmatch(read(rel), -1)
		if len(matches) == 0 {
			t.Errorf("%s: no match for %q", rel, pattern)
		}
		for _, m := range matches {
			if m[1] != want {
				t.Errorf("%s: %q pins %s, expected %s", rel, pattern, m[1], want)
			}
		}
	}

	server, db := DefaultOpenVoxServerVersion, DefaultOpenVoxDBVersion
	expectAll("images/openvox-server/Containerfile", `ARG OPENVOX_SERVER_VERSION=(\S+)`, server)
	expectAll("images/openvox-server/Containerfile", `ARG OPENVOXDB_VERSION=(\S+)`, db)
	expectAll("images/openvox-db/Containerfile", `ARG OPENVOXDB_VERSION=(\S+)`, db)
	for _, f := range []string{"charts/voxkube-operator/values.yaml", "charts/voxkube-operator/templates/voxcluster.yaml"} {
		expectAll(f, `ghcr\.io/yboikov/openvox-server:(\d+\.\d+\.\d+)`, server)
		expectAll(f, `ghcr\.io/yboikov/openvox-db:(\d+\.\d+\.\d+)`, db)
	}
	if server != db {
		t.Errorf("OpenVox Server (%s) and OpenVoxDB (%s) should be released together on the same version", server, db)
	}
}
