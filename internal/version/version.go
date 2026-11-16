// Package version holds the operator build information, set at build time with -ldflags:
//
//	-X github.com/yboikov/voxkube-operator/internal/version.Version=0.1.0
//	-X github.com/yboikov/voxkube-operator/internal/version.Commit=<git sha>
//	-X github.com/yboikov/voxkube-operator/internal/version.BuildDate=<RFC3339>
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the operator release (SemVer, without the leading "v"), or "dev" for local builds.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// BuildDate is the UTC build time (RFC 3339).
	BuildDate = "unknown"
)

// String returns a one-line description of the build.
func String() string {
	return fmt.Sprintf("voxkube-operator %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
