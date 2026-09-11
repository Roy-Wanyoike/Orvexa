package contract

// Spec-level conformance: the document must parse, describe exactly the
// route set it claims, resolve every $ref internally and keep the named
// component schemas clients build against. Spec-internal drift fixed under
// issue #41 (duplicate components block) is what these tests keep dead.

import (
	"strings"
	"testing"
)

func TestSpecParses(t *testing.T) {
	spec := MustLoadSpec(t)
	if got := len(spec.RouteSet()); got < 30 {
		t.Fatalf("spec exposes %d paths; expected the full v1 surface (>=30)", got)
	}
}

func TestSpecServerBaseIsVersioned(t *testing.T) {
	if got := MustLoadSpec(t).ServerBase(); got != "/api/v1" {
		t.Fatalf("servers[0].url = %q, want /api/v1", got)
	}
}

func TestSpecRouteSetShape(t *testing.T) {
	spec := MustLoadSpec(t)
	routes := spec.RouteSet()

	wantMethods := map[string]map[string]bool{
		"/customers":                    {"POST": true, "GET": true},
		"/customers/{id}":               {"GET": true},
		"/conversations/{id}/close":     {"POST": true},
		"/search/conversations":         {"GET": true},
		"/interactions/{id}/transition": {"POST": true},
		"/agents/{id}/presence":         {"GET": true, "PUT": true},
		"/cases/{id}/notes":             {"POST": true, "GET": true},
		"/webhooks/{provider}":          {"POST": true},
		"/identity/authorize-url":       {"GET": true},
		"/identity/callback":            {"GET": true},
		"/":                             {"GET": true},
	}
	for path, want := range wantMethods {
		got, ok := routes[path]
		if !ok {
			t.Fatalf("spec lost path %q", path)
		}
		for m := range want {
			if !got[m] {
				t.Fatalf("path %q lost method %s (documented: %v)", path, m, got)
			}
		}
	}
}

func TestSpecRefsAllResolve(t *testing.T) {
	spec := MustLoadSpec(t)
	if bad := spec.DanglingRefs(); len(bad) > 0 {
		t.Fatalf("dangling $refs in spec:\n%s", joinLines(bad))
	}
}

func TestSpecNamedSchemasExist(t *testing.T) {
	spec := MustLoadSpec(t)
	names := map[string]bool{}
	for _, n := range spec.ComponentNames() {
		names[n] = true
	}
	for _, want := range []string{"CustomerCreate", "InteractionCreate", "AgentCreate", "QueueCreate", "CaseCreate"} {
		if !names[want] {
			t.Fatalf("component schema %q missing (have %v)", want, spec.ComponentNames())
		}
	}
}

func TestSpecOperationLookups(t *testing.T) {
	spec := MustLoadSpec(t)

	// /search/conversations documents the degradation trio: 200/422/503.
	if codes := spec.ResponseKeys("/search/conversations", "GET"); !codes["200"] || !codes["422"] || !codes["503"] {
		t.Fatalf("/search/conversations responses = %v, want 200+422+503", codes)
	}
	desc, ok := spec.ResponseDescription("/search/conversations", "GET", "503")
	if !ok {
		t.Fatal("/search/conversations has no 503 response documented")
	}
	for _, code := range []string{"search.unavailable", "search.not_configured"} {
		if !strings.Contains(desc, code) {
			t.Fatalf("503 description %q does not name %q", desc, code)
		}
	}

	// The index route is documented once, as the versioned root.
	if _, ok := spec.Operation("/", "GET"); !ok {
		t.Fatal("index path / (GET) missing from spec")
	}

	// Path-item metadata must not leak into the route set (parameters etc.).
	for path, methods := range spec.RouteSet() {
		for m := range methods {
			switch m {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE":
			default:
				t.Fatalf("path %q declares non-HTTP method key %q", path, m)
			}
		}
	}
}

func joinLines(items []string) string {
	out := ""
	for _, it := range items {
		out += "  - " + it + "\n"
	}
	return out
}
