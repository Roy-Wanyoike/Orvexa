package contract

// Route enumeration (issue #41): the assembled router — built exactly the
// way cmd/api/main.go builds it, minus the database — and the OpenAPI
// document must describe the SAME /api/v1 surface, in both directions.
//
// Why a nil pool is safe here: every service constructor is a struct fill
// (verified by construction) and route REGISTRATION never touches storage;
// handlers only reach the pool on real business calls, which these probes
// never trigger (authentication answers 401 before any service runs, and
// the search plane degrades to a typed 503 without a backend).
//
// 404/405 semantics: chi's transport defaults (404 plain text, 405 empty)
// are asserted — a documented path must accept its documented methods, must
// reject methods the spec never declares, and unknown paths must not be
// silently absorbed. Resource-level 404/422/409 envelopes (the spec's
// Error404/Error422/Error409) are produced inside handlers and are covered
// by the envelope conformance suite.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/agents"
	"github.com/Roy-Wanyoike/orvexa/internal/ai"
	"github.com/Roy-Wanyoike/orvexa/internal/analytics"
	"github.com/Roy-Wanyoike/orvexa/internal/cases"
	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/conversations"
	"github.com/Roy-Wanyoike/orvexa/internal/customers"
	"github.com/Roy-Wanyoike/orvexa/internal/httpserver"
	"github.com/Roy-Wanyoike/orvexa/internal/identity"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	"github.com/Roy-Wanyoike/orvexa/internal/queues"
	"github.com/Roy-Wanyoike/orvexa/internal/routing"
	"github.com/Roy-Wanyoike/orvexa/internal/search"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
	"github.com/Roy-Wanyoike/orvexa/internal/tools"
	"github.com/Roy-Wanyoike/orvexa/internal/webhooks"
	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
)

// contractRouter assembles the FULL production route tree the way
// cmd/api/main.go does (pool==nil branch + ORVEXA_OIDC_* configured):
// every domain service wired, search mounted (O-27 — main.go always wires
// it; unconfigured ⇒ honest typed 503), identity plane mounted (O-29 —
// the conditional public login endpoints are part of the documented
// surface, so the router under test must include them).
func contractRouter() http.Handler {
	writer := outbox.NewWriter(nil)
	interactionSvc := interactions.NewService(nil, writer, "contract-test")

	deps := httpserver.DomainDeps{
		Tenancy: tenancy.NewService(nil),
		// [O-27] main.go: `deps.Search = search.NewService(search.NewServiceFromEnv())`
		// — constructible without a backend; route stays mounted and serves
		// the typed 503 (search.not_configured) when unset.
		Search:         search.NewService(search.Config{}),
		Webhooks:       webhooks.NewGateway(nil, "contract-test-hmac-secret"),
		Customers:      customers.NewService(nil),
		Conversations:  conversations.NewService(nil, writer, "contract-test"),
		Interactions:   interactionSvc,
		Agents:         agents.NewService(nil),
		Queues:         queues.NewService(nil),
		Cases:          cases.NewService(nil, writer, "contract-test"),
		CommsProcessor: &comms.Processor{Interactions: interactionSvc},
		Calls:          telephony.NewService(interactionSvc, nil),
		Messages:       messaging.NewService(interactionSvc, nil),
		Routing:        routing.NewService(nil, routing.NewPresenceStore(nil, 0), writer, "contract-test"),
		AI: ai.NewRuntime(nil,
			ai.NewGateway(ai.NewRulesProvider("contract-test"), 0, 0),
			tools.NewGateway(stubExecutor{}, stubPolicies{}, nil),
			writer, "contract-test"),
		Workflows: workflows.NewEngine(nil, writer, stubWorkflowServices{}, "contract-test"),
		Analytics: analytics.NewService(nil),
		Writer:    writer,
	}

	// [O-29] identity plane: constructed directly — the env equivalent of
	// ORVEXA_OIDC_* being configured. The failing IdP client makes any
	// accidental network access immediate and deterministic, so probes of
	// the public login routes never leave the process.
	idSvc := identity.NewService(identity.Config{
		Issuer:       "https://idp.orvexa-contract.invalid",
		ClientID:     "orvexa-contract-test",
		ClientSecret: "contract-test-only",
		HTTPClient:   &http.Client{Timeout: 250 * time.Millisecond, Transport: failingTransport{}},
	})

	// Same construction as cmd/api/main.go (only the pool is nil).
	return httpserver.New(httpserver.Deps{
		Domain:   deps,
		Limiter:  httpx.NewRateLimit(600, 120, 10_000),
		Identity: idSvc,
	})
}

// ---- stubs standing in for main-package internals (platformToolExecutor,
// dbToolPolicy, apiWorkflowServices are unexported types of cmd/api) ----

type stubExecutor struct{}

func (stubExecutor) Execute(_ context.Context, _ *tools.Call) (map[string]any, error) {
	return nil, nil
}

type stubPolicies struct{}

func (stubPolicies) Allowlist(context.Context, string, string) (map[string]int, error) {
	return map[string]int{}, nil
}

type stubWorkflowServices struct{}

func (stubWorkflowServices) QueueMessage(context.Context, string, string, string, string, string) error {
	return nil
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("contract test: no network")
}

// ---- route enumeration ----

type routeKey struct{ Method, Path string }

// specBase is the versioned mount the spec's servers[0].url declares.
const specBase = "/api/v1"

// normalizePattern aligns chi walk patterns with OpenAPI path templating:
// the mount prefix is stripped, the group-root "/" pattern survives as "/",
// and the trailing slash of Route-level leaves ("/customers/") is dropped —
// chi serves both spellings, the spec documents one.
func normalizePattern(pattern string) string {
	rel := strings.TrimPrefix(pattern, specBase)
	if rel == "" || rel == "/" {
		return "/"
	}
	return strings.TrimSuffix(rel, "/")
}

// walkedRoutes returns every registered route (method+normalized path) plus
// the subsets: versioned API surface and root-level infra endpoints.
func walkedRoutes(t *testing.T, h http.Handler) (api, infra map[routeKey]bool) {
	t.Helper()
	mux, ok := h.(chi.Router)
	if !ok {
		t.Fatalf("contract router is not a chi.Router (got %T)", h)
	}
	api = map[routeKey]bool{}
	infra = map[routeKey]bool{}
	err := chi.Walk(mux, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := routeKey{Method: method, Path: normalizePattern(pattern)}
		if strings.HasPrefix(pattern, specBase) {
			api[key] = true
		} else {
			infra[key] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk failed: %v", err)
	}
	return api, infra
}

// doRequest fires one probe at the router (no network: pure ServeHTTP).
func doRequest(h http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{"contract":"probe"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// fillParams replaces OpenAPI path templating with deterministic probes.
func wirePath(base, specPath string) string {
	p := specPath
	for _, param := range []string{"{id}", "{interactionId}", "{provider}"} {
		p = strings.ReplaceAll(p, param, "contract-probe")
	}
	if p == "/" {
		return base + "/"
	}
	return base + p
}

// TestSpecRoutesExistInRouter: direction 1 — every documented path+method is
// registered. A missing route means the implementation dropped contract.
func TestSpecRoutesExistInRouter(t *testing.T) {
	spec := MustLoadSpec(t)
	api, _ := walkedRoutes(t, contractRouter())

	var missing []string
	for path, methods := range spec.RouteSet() {
		for method := range methods {
			if !api[routeKey{Method: method, Path: path}] {
				missing = append(missing, fmt.Sprintf("%-6s %s", method, path))
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("spec routes absent from router (router drift — file an issue):\n%s", joinLines(missing))
	}
}

// TestRouterRoutesAreDocumented: direction 2 — every route the router
// registers under /api/v1 is documented. An extra route means the wire
// surface grew beyond the published contract.
func TestRouterRoutesAreDocumented(t *testing.T) {
	spec := MustLoadSpec(t)
	documented := spec.RouteSet()
	api, _ := walkedRoutes(t, contractRouter())

	var undocumented []string
	for key := range api {
		methods, ok := documented[key.Path]
		if !ok || !methods[key.Method] {
			undocumented = append(undocumented, fmt.Sprintf("%-6s %s", key.Method, key.Path))
		}
	}
	if len(undocumented) > 0 {
		sort.Strings(undocumented)
		t.Fatalf("router routes not documented in spec (router drift — file an issue):\n%s", joinLines(undocumented))
	}
}

// TestHealthEndpointsExistOutsideVersionedSurface: /healthz and /readyz are
// process endpoints, deliberately outside the /api/v1 base (spec servers[0]
// is /api/v1, so they are NOT spec paths — but they must keep existing).
func TestHealthEndpointsExistOutsideVersionedSurface(t *testing.T) {
	_, infra := walkedRoutes(t, contractRouter())
	for _, want := range []routeKey{{"GET", "/healthz"}, {"GET", "/readyz"}} {
		if !infra[want] {
			t.Fatalf("infra endpoint %s %s missing from the router", want.Method, want.Path)
		}
	}
	for key := range infra {
		if strings.HasPrefix(key.Path, specBase) {
			t.Fatalf("%s %s classified as infra but lives under %s", key.Method, key.Path, specBase)
		}
	}
}

// TestUnknownPathsAnswer404: paths with no mounted resource prefix must 404
// (chi transport default). An unknown SUB-path under an authed resource
// answers 401 instead — the auth gate wraps the resource subtree before its
// 404 branch, so route existence is never leaked to unauthenticated callers
// (the stronger, spec-conformant security posture).
func TestUnknownPathsAnswer404(t *testing.T) {
	h := contractRouter()
	for _, path := range []string{"/api/v1/definitely-not-a-resource", "/definitely/not"} {
		if code := doRequest(h, http.MethodGet, path).Code; code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, code)
		}
	}
	if code := doRequest(h, http.MethodGet, "/api/v1/customers/contract-probe/unknown-sub").Code; code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/customers/{id}/unknown-sub = %d, want 401 (auth gate precedes the resource 404)", code)
	}
}

// TestEverySpecRouteAcceptsItsDocumentedMethod: a documented method must
// reach a real handler — never the router's 404/405. Status expectations by
// surface class:
//
//   - authed resource routes → 401 auth.missing_key (nil tenancy service
//     answers before any storage access — the middleware contract)
//   - public webhook ingress → 401 signature validation (nil-pool gateway
//     rejects before persistence)
//   - identity callback without params → 422 (typed validation)
//   - identity authorize-url → 500 (discovery fails deterministically on
//     the injected no-network client — proves the handler ran)
func TestEverySpecRouteAcceptsItsDocumentedMethod(t *testing.T) {
	spec := MustLoadSpec(t)
	h := contractRouter()

	var failures []string
	for path, methods := range spec.RouteSet() {
		for method := range methods {
			rec := doRequest(h, method, wirePath("/api/v1", path))
			switch rec.Code {
			case http.StatusNotFound, http.StatusMethodNotAllowed:
				failures = append(failures, fmt.Sprintf("%-6s %-44s -> %d (route/method not registered)",
					method, path, rec.Code))
			}
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		t.Fatalf("documented methods not served:\n%s", joinLines(failures))
	}

	// Spot-check the class expectations above (exact statuses, not just
	// "not 404/405").
	cases := []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/v1/customers", http.StatusUnauthorized},
		{"POST", "/api/v1/conversations/x/close", http.StatusUnauthorized},
		{"GET", "/api/v1/identity/callback", http.StatusUnprocessableEntity},
		{"POST", "/api/v1/webhooks/voice", http.StatusUnauthorized},
	}
	for _, c := range cases {
		if got := doRequest(h, c.method, c.path).Code; got != c.want {
			t.Fatalf("%s %s = %d, want %d", c.method, c.path, got, c.want)
		}
	}
	if got := doRequest(h, "GET", "/api/v1/identity/authorize-url").Code; got < 500 {
		t.Fatalf("GET /api/v1/identity/authorize-url = %d, want a 5xx from the deterministic discovery failure", got)
	}
}

// TestEverySpecPathRejectsUndocumentedMethods: PATCH is declared nowhere in
// the spec, so no documented path may SERVE it. Wire status depends on chi's
// two-level dispatch, which the middleware order fixes deterministically:
//
//   - public paths (security: []) → exactly 405: dispatch rejects the method
//     before any endpoint handler exists.
//   - authed paths → 401 or 405, never a business outcome: Route-subtree
//     resources run the auth gate before their method dispatch (401), while
//     directly-registered endpoints 405 at dispatch. Either way an
//     unauthenticated caller can never invoke an undocumented method, and a
//     PATCH route actually REGISTERED anywhere is caught exactly by
//     TestRouterMethodSetsMatchExactly (middleware-independent).
func TestEverySpecPathRejectsUndocumentedMethods(t *testing.T) {
	spec := MustLoadSpec(t)
	h := contractRouter()

	var failures []string
	for path := range spec.RouteSet() {
		code := doRequest(h, http.MethodPatch, wirePath("/api/v1", path)).Code
		switch {
		case spec.IsPublic(path, "PATCH"):
			if code != http.StatusMethodNotAllowed {
				failures = append(failures, fmt.Sprintf("PATCH %-38s -> %d, want 405 (public path)", path, code))
			}
		case code != http.StatusUnauthorized && code != http.StatusMethodNotAllowed:
			failures = append(failures, fmt.Sprintf("PATCH %-38s -> %d, want 401/405 (must not be served)", path, code))
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		t.Fatalf("undocumented methods not rejected cleanly:\n%s", joinLines(failures))
	}
}

// TestRouterMethodSetsMatchExactly: per path, the SET of registered methods
// must equal the documented set — no method can hide behind middleware order.
func TestRouterMethodSetsMatchExactly(t *testing.T) {
	spec := MustLoadSpec(t)
	documented := spec.RouteSet()
	api, _ := walkedRoutes(t, contractRouter())

	registered := map[string]map[string]bool{}
	for key := range api {
		if registered[key.Path] == nil {
			registered[key.Path] = map[string]bool{}
		}
		registered[key.Path][key.Method] = true
	}

	paths := map[string]bool{}
	for p := range documented {
		paths[p] = true
	}
	for p := range registered {
		paths[p] = true
	}

	var failures []string
	for p := range paths {
		want, got := documented[p], registered[p]
		for m := range want {
			if got == nil || !got[m] {
				failures = append(failures, fmt.Sprintf("%-44s documented %s but not registered", p, m))
			}
		}
		for m := range got {
			if want == nil || !want[m] {
				failures = append(failures, fmt.Sprintf("%-44s registered %s but not documented", p, m))
			}
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		t.Fatalf("method-set drift:\n%s", joinLines(failures))
	}
}
