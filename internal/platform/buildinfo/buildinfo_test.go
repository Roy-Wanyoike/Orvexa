package buildinfo

import (
	"strings"
	"testing"
)

// TestDefaultsAreHonest pins the unstamped defaults: a plain `go build`
// binary must never claim a release it was not.
func TestDefaultsAreHonest(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version default = %q, want %q", Version, "dev")
	}
	if Commit != "none" {
		t.Fatalf("Commit default = %q, want %q", Commit, "none")
	}
	if Date != "unknown" {
		t.Fatalf("Date default = %q, want %q", Date, "unknown")
	}
}

// TestStringContainsStamp overrides the vars exactly as -ldflags -X would
// (the test binary itself is unstamped) and asserts the rendered line
// carries all three components in order.
func TestStringContainsStamp(t *testing.T) {
	origV, origC, origD := Version, Commit, Date
	defer func() { Version, Commit, Date = origV, origC, origD }()

	Version, Commit, Date = "v0.10.0", "a76b462", "2026-09-10T00:00:00Z"
	got := String()

	for _, want := range []string{"v0.10.0", "a76b462", "2026-09-10T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() = %q, missing %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "v0.10.0 ") {
		t.Fatalf("String() = %q, version must lead the stamp", got)
	}
}

// TestStringUnstampedShape pins the exact unstamped rendering so health
// responses stay stable for tooling that greps them.
func TestStringUnstampedShape(t *testing.T) {
	if got, want := String(), "dev (commit=none, built=unknown)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
