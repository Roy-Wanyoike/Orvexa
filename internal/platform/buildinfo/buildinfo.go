// Package buildinfo carries the release stamp injected at link time:
//
//	go build -ldflags "\
//	  -X github.com/Roy-Wanyoike/orvexa/internal/platform/buildinfo.Version=v0.10.0 \
//	  -X github.com/Roy-Wanyoike/orvexa/internal/platform/buildinfo.Commit=$(git rev-parse --short HEAD) \
//	  -X github.com/Roy-Wanyoike/orvexa/internal/platform/buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
//	  ./cmd/api
//
// The Makefile `release` target applies exactly these flags to all three
// deployables (cmd/api, cmd/worker, cmd/realtime). The stamp is surfaced
// additively by the /healthz and /readyz response bodies
// (internal/httpserver) so operations can identify any running build.
//
// Defaults describe an unstamped development build — every binary reports
// something honest even when built with plain `go build`. The package is
// dependency-free: no configuration, no environment, no I/O.
package buildinfo

import "fmt"

var (
	// Version is the release identifier (e.g. "v0.10.0").
	// "dev" when the binary was built without stamping.
	Version = "dev"

	// Commit is the VCS revision the binary was built from
	// ("none" when unknown, e.g. an unstamped development build).
	Commit = "none"

	// Date is the UTC build time in RFC 3339 ("unknown" when unstamped).
	Date = "unknown"
)

// String renders the one-line stamp reported by the health endpoints:
//
//	v0.10.0 (commit=a76b462, built=2026-09-10T00:00:00Z)
//
// Unstamped builds render "dev (commit=none, built=unknown)".
func String() string {
	return fmt.Sprintf("%s (commit=%s, built=%s)", Version, Commit, Date)
}
