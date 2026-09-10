//go:build authz

package authz

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Defect ledger — isolation weaknesses OBSERVED by this matrix and filed as
// type/bug issues (never fixed inside this package; module owners fix, this
// suite re-evidences). Issue numbers are updated once filed on GitHub.
// ---------------------------------------------------------------------------

type defectInfo struct {
	id    string
	title string
	owner string
	issue string // "#NN" once filed; "pending" until then
	repro string
}

// defectRegistry is the single source of truth for the MATRIX.md defects
// section and the ratchet rows below.
var defectRegistry = []defectInfo{
	{
		id:    "D1",
		title: "Cross-tenant customer_id accepted by interaction-plane creates: POST /api/v1/interactions, /calls, /messages return 201 and anchor tenant-B rows to a tenant-A customer UUID (conversation auto-open + interactions/calls/messages FKs are unscoped)",
		owner: "internal/interactions (+internal/conversations auto-open)",
		issue: "pending",
		repro: "POST /api/v1/interactions with tenant-B key and body {\"customer_id\":\"<tenant-A customer UUID>\",\"channel\":\"chat\",...} → 201 (expect 404/422: customer does not exist for tenant B)",
	},
	{
		id:    "D2",
		title: "Cross-tenant customer_id accepted by POST /api/v1/cases → 201; cases.customer_id FK is unscoped and the service never verifies the customer belongs to the caller's tenant",
		owner: "internal/cases",
		issue: "pending",
		repro: "POST /api/v1/cases with tenant-B key and body {\"customer_id\":\"<tenant-A customer UUID>\",\"subject\":\"x\"} → 201",
	},
	{
		id:    "D3",
		title: "POST /api/v1/cases/{id}/notes does not verify the case belongs to the caller's tenant → 201 note row written against a foreign case id (case_notes.case_id FK is unscoped)",
		owner: "internal/cases",
		issue: "pending",
		repro: "POST /api/v1/cases/<tenant-A case UUID>/notes with tenant-B key → 201 (expect 404)",
	},
	{
		id:    "D4",
		title: "POST /api/v1/cases/{id}/interactions/{iid}/link verifies the interaction tenant but not the CASE tenant → 204 linking a tenant-B interaction into a tenant-A case",
		owner: "internal/cases",
		issue: "pending",
		repro: "POST /api/v1/cases/<tenant-A case UUID>/interactions/<tenant-B interaction UUID>/link with tenant-B key → 204 (expect 404)",
	},
	{
		id:    "D5",
		title: "POST /api/v1/routing/interactions/{id} accepts a foreign-tenant interaction_id and persists a routing_decision under the caller's tenant embedding the foreign UUID (echoed in the response and visible in GET /routing/decisions) — cross-tenant identifier disclosure + referential pollution",
		owner: "internal/routing",
		issue: "pending",
		repro: "POST /api/v1/routing/interactions/<tenant-A interaction UUID> with tenant-B key → 200 decision whose interaction_id is the foreign UUID; GET /routing/decisions (tenant-B key) then lists it",
	},
	{
		id:    "D7",
		title: "Second+ interaction-plane create WITHOUT provider_ref returns 409 interaction.duplicate: the service binds an empty ProviderRef as '' instead of NULL, so UNIQUE (tenant_id, provider, provider_ref) collapses every ref-less create per (tenant, provider) — POST /api/v1/interactions, /calls and /messages all refuse a legitimate second create (calls/messages take no provider_ref at all)",
		owner: "internal/interactions (+internal/telephony, internal/messaging call paths)",
		issue: "pending",
		repro: "POST /api/v1/interactions/ twice for one tenant with bodies lacking ProviderRef — the 2nd returns 409 interaction.duplicate although nothing was duplicated (no idempotency key, no provider ref, fresh UUID). Secure behavior: 201 — dedupe must key on a real idempotency key, never on the absence of one",
	},
	{
		id:    "D8",
		title: "Sub-resource LIST routes do not verify the parent resource's tenancy: GET /api/v1/conversations/{foreign-id}/interactions and GET /api/v1/cases/{foreign-id}/notes return 200 with an empty list instead of the canonical 404 (GET of the parent itself correctly 404s). No data leaks and no enumeration oracle exists (foreign and missing ids behave identically), but the responses are inconsistent with the 404-canonical contract the rest of the API enforces",
		owner: "internal/interactions (ListByConversation) + internal/cases (ListNotes)",
		issue: "pending",
		repro: "With tenant-B's key, GET /api/v1/conversations/<tenant-A conversation UUID>/interactions → 200 {data:[]} (expect 404); same for GET /api/v1/cases/<tenant-A case UUID>/notes. Secure behavior: 404 like every other foreign-id route",
	},
	{
		id:    "D9",
		title: "POST /api/v1/calls/{id}/actions with action hold|resume on a voice interaction that never went through POST /api/v1/calls/ (e.g. a voice interaction created via the generic interactions plane, or an inbound call opened by a provider webhook) returns 500 internal.error: telephony.ApplyAction propagates the provider's unknown-leg error unwrapped instead of mapping it to a typed app error",
		owner: "internal/telephony (ApplyAction → provider error mapping)",
		issue: "pending",
		repro: "Create a voice interaction via POST /api/v1/interactions/ (channel=voice, any customer), then POST /api/v1/calls/{that id}/actions with {\"action\":\"hold\"} → 500 {internal.error} (expect a typed 4xx, e.g. 409 call.unknown_leg / 422; 200 after a fix that makes hold a no-op on unplaced legs)",
	},
	{
		id:    "D6",
		title: "Cross-tenant customer_id accepted by POST /api/v1/workflows/callbacks and /collections → 201; workflow_instances carries no customer FK and the engine never verifies the customer belongs to the caller's tenant",
		owner: "internal/workflows",
		issue: "pending",
		repro: "POST /api/v1/workflows/callbacks with tenant-B key and body {\"customer_id\":\"<tenant-A customer UUID>\",\"phone\":\"...\"} → 201",
	},
}

func defectByID(id string) *defectInfo {
	for i := range defectRegistry {
		if defectRegistry[i].id == id {
			return &defectRegistry[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Result ledger
// ---------------------------------------------------------------------------

type mrow struct {
	RouteID   string
	Route     string
	Mount     string
	Probe     string
	Expect    string
	Got       int
	Result    string // PASS | PASS-SECURED | PASS-DEGRADED | DEFECT | CONDITIONAL | FAIL
	Note      string
	HardFails int // ASSERT violations (drive the suite verdict)
}

type ledger struct {
	t      *testing.T
	rows   []*mrow
	byID   map[string]*mrow
	defect map[string]int // defect id → observed status (0 if none)
}

func newLedger(t *testing.T) *ledger {
	return &ledger{t: t, byID: map[string]*mrow{}, defect: map[string]int{}}
}

func (l *ledger) row(routeID, route, mount, probe, expect string, got int, result, note string) *mrow {
	r := &mrow{
		RouteID: routeID, Route: route, Mount: mount, Probe: probe, Expect: expect,
		Got: got, Result: result, Note: note,
	}
	key := routeID + "|" + slotOf(probe)
	if prev, ok := l.byID[key]; ok {
		// same route+slot re-probed (regeneration) — replace in place
		r.HardFails = prev.HardFails
		*l.rows[l.indexOf(prev)] = *r
		return r
	}
	l.rows = append(l.rows, r)
	l.byID[key] = r
	return r
}

// slotOf normalizes the matrix column a row belongs to (the probe
// description prefixes are the contract).
func slotOf(probe string) string {
	switch {
	case strings.HasPrefix(probe, "no-auth"):
		return "no-auth"
	case strings.HasPrefix(probe, "tenant-B key"):
		return "foreign"
	case strings.HasPrefix(probe, "tenant-A key"):
		return "own"
	default:
		return "extra"
	}
}

func (l *ledger) indexOf(r *mrow) int {
	for i, x := range l.rows {
		if x == r {
			return i
		}
	}
	return len(l.rows) - 1
}

// probe executes one request and hard-asserts got ∈ expect. 2xx on a
// foreign-tenant row fails even when expect somehow allowed it (defense in
// depth is explicit in the callers, which never put 2xx in foreign expects).
func (l *ledger) probe(s *stack, routeID, route, mount, probeDesc, method, path, key, body string, hdr map[string]string, expect ...int) {
	l.t.Helper()
	code, respBody := s.do(l.t, method, path, key, body, hdr)
	got := fmt.Sprint(code)
	want := expectDesc(expect)
	if !containsInt(expect, code) {
		l.row(routeID, route, mount, probeDesc, want, code, "FAIL",
			snip(respBody)).HardFails++
		l.t.Errorf("[%s] %s (%s): got %s want %s\n%s", routeID, route, probeDesc, got, want, snip(respBody))
		return
	}
	l.row(routeID, route, mount, probeDesc, want, code, "PASS", snip(respBody))
}

// probeNoLeak additionally asserts the response body carries none of the
// tenant-A resource identifiers (the adversarial "prove it's not there" scan
// on every tenant-B 2xx row).
func (l *ledger) probeNoLeak(s *stack, routeID, route, mount, probeDesc, method, path, key, body string, needles []string, expect ...int) {
	l.t.Helper()
	code, respBody := s.do(l.t, method, path, key, body, nil)
	want := expectDesc(expect)
	if !containsInt(expect, code) {
		l.row(routeID, route, mount, probeDesc, want, code, "FAIL", snip(respBody)).HardFails++
		l.t.Errorf("[%s] %s (%s): got %d want %s\n%s", routeID, route, probeDesc, code, want, snip(respBody))
		return
	}
	if leaks := leakedNeedles(respBody, needles); len(leaks) > 0 {
		l.row(routeID, route, mount, probeDesc, want, code, "FAIL",
			"tenant-A identifiers present in tenant-B response: "+strings.Join(leaks, ", ")).HardFails++
		l.t.Errorf("[%s] %s (%s): 2xx response contains tenant-A identifiers: %v", routeID, route, probeDesc, leaks)
		return
	}
	l.row(routeID, route, mount, probeDesc, want, code, "PASS", "no tenant-A identifiers in response")
}

// probeContains asserts a positive-scope row: the response MUST carry the
// caller's own seeded resource (proves the list/get is real, not an empty shell).
func (l *ledger) probeContains(s *stack, routeID, route, mount, probeDesc, method, path, key, body, mustContain string, expect ...int) {
	l.t.Helper()
	code, respBody := s.do(l.t, method, path, key, body, nil)
	want := expectDesc(expect)
	if !containsInt(expect, code) {
		l.row(routeID, route, mount, probeDesc, want, code, "FAIL", snip(respBody)).HardFails++
		l.t.Errorf("[%s] %s (%s): got %d want %s\n%s", routeID, route, probeDesc, code, want, snip(respBody))
		return
	}
	if !strings.Contains(respBody, mustContain) {
		l.row(routeID, route, mount, probeDesc, want, code, "FAIL",
			"response does not contain own resource "+mustContain).HardFails++
		l.t.Errorf("[%s] %s (%s): own resource %s missing from response", routeID, route, probeDesc, mustContain)
		return
	}
	l.row(routeID, route, mount, probeDesc, want, code, "PASS", "own resource present: "+mustContain)
}

// probeDefect is the ratchet row for a filed isolation defect: the platform
// may exhibit the recorded defect status OR the secure status; anything else
// fails. This keeps the suite honest both before and after a fix lands.
func (l *ledger) probeDefect(s *stack, routeID, route, mount, probeDesc, method, path, key, body string, d *defectInfo, defectStatus int, secureStatuses ...int) {
	l.t.Helper()
	code, respBody := s.do(l.t, method, path, key, body, nil)
	secure := expectDesc(secureStatuses)
	switch {
	case code == defectStatus:
		l.defect[d.id] = code
		l.row(routeID, route, mount, probeDesc,
			fmt.Sprintf("%d (defect %s %s) OR %s (secured)", defectStatus, d.id, d.issue, secure),
			code, "DEFECT", d.id+" "+d.issue+" — recorded isolation defect; not fixed here").HardFails += 0
	case containsInt(secureStatuses, code):
		l.row(routeID, route, mount, probeDesc,
			fmt.Sprintf("%d (defect %s %s) OR %s (secured)", defectStatus, d.id, d.issue, secure),
			code, "PASS-SECURED", d.id+" behavior secured (defect no longer reproduces)")
	default:
		l.row(routeID, route, mount, probeDesc,
			fmt.Sprintf("%d (defect %s %s) OR %s (secured)", defectStatus, d.id, d.issue, secure),
			code, "FAIL", snip(respBody)).HardFails++
		l.t.Errorf("[%s] %s (%s): got %d — matches neither the recorded defect (%d) nor the secured behavior (%s)",
			routeID, route, probeDesc, code, defectStatus, secure)
	}
}

// probeForeignList is the D8-family ratchet for sub-resource LIST routes: a
// foreign parent id must leak nothing (hard needle scan) and should answer the
// canonical 404. While defect `defectID` is unfixed the platform answers
// 200 + empty list (recorded, no leak); after a fix it must answer secureStatus.
func (l *ledger) probeForeignList(s *stack, routeID, route, mount, probeDesc, method, path, key string, needles []string, defectID string, secureStatus ...int) {
	l.t.Helper()
	code, respBody := s.do(l.t, method, path, key, "", nil)
	d := defectByID(defectID)
	want := fmt.Sprintf("200+empty (defect %s %s) OR %s (secured)", defectID, d.issue, expectDesc(secureStatus))
	if leaks := leakedNeedles(respBody, needles); len(leaks) > 0 {
		l.row(routeID, route, mount, probeDesc, want, code, "FAIL",
			"tenant-A identifiers present in foreign-list response: "+strings.Join(leaks, ", ")).HardFails++
		l.t.Errorf("[%s] %s (%s): leak in foreign-list response: %v", routeID, route, probeDesc, leaks)
		return
	}
	switch {
	case code == http.StatusOK:
		l.defect[defectID] = code
		l.row(routeID, route, mount, probeDesc, want, code, "DEFECT",
			defectID+" "+d.issue+" — 200 with no tenant-A identifiers (no leak, no oracle); not fixed here")
	case containsInt(secureStatus, code):
		l.row(routeID, route, mount, probeDesc, want, code, "PASS-SECURED",
			defectID+" behavior secured (foreign parent id no longer answers 200)")
	default:
		l.row(routeID, route, mount, probeDesc, want, code, "FAIL", snip(respBody)).HardFails++
		l.t.Errorf("[%s] %s (%s): got %d want %s\n%s", routeID, route, probeDesc, code, want, snip(respBody))
	}
}

// probeDefectMasked is probeDefect plus a masking status: `maskID`/`maskStatus`
// records a SECOND filed defect whose observed behavior prevents the primary
// defect probe from reaching the code under test this run (e.g. D7's 409 on a
// ref-less create happens before the cross-tenant reference (D1) is read).
// Verdicts: primary defect status → DEFECT (primary reproduced); mask status →
// DEFECT recorded against the mask defect; secure statuses → PASS-SECURED.
func (l *ledger) probeDefectMasked(s *stack, routeID, route, mount, probeDesc, method, path, key, body string, d *defectInfo, defectStatus int, maskID string, maskStatus int, secureStatuses ...int) {
	l.t.Helper()
	code, respBody := s.do(l.t, method, path, key, body, nil)
	switch {
	case code == defectStatus:
		l.defect[d.id] = code
		l.row(routeID, route, mount, probeDesc,
			fmt.Sprintf("%d (defect %s %s) | %d (%s masks) | %s (secured)", defectStatus, d.id, d.issue, maskStatus, maskID, expectDesc(secureStatuses)),
			code, "DEFECT", d.id+" "+d.issue+" — recorded isolation defect; not fixed here")
	case code == maskStatus:
		l.defect[maskID] = code
		l.row(routeID, route, mount, probeDesc,
			fmt.Sprintf("%d (defect %s %s) | %d (%s masks) | %s (secured)", defectStatus, d.id, d.issue, maskStatus, maskID, expectDesc(secureStatuses)),
			code, "DEFECT", maskID+" masks "+d.id+" this run — "+maskID+" reproduced; see "+maskID)
	case containsInt(secureStatuses, code):
		l.row(routeID, route, mount, probeDesc,
			fmt.Sprintf("%d (defect %s %s) | %d (%s masks) | %s (secured)", defectStatus, d.id, d.issue, maskStatus, maskID, expectDesc(secureStatuses)),
			code, "PASS-SECURED", d.id+" behavior secured (defect no longer reproduces)")
	default:
		l.row(routeID, route, mount, probeDesc,
			fmt.Sprintf("%d (defect %s %s) | %d (%s masks) | %s (secured)", defectStatus, d.id, d.issue, maskStatus, maskID, expectDesc(secureStatuses)),
			code, "FAIL", snip(respBody)).HardFails++
		l.t.Errorf("[%s] %s (%s): got %d — matches neither the recorded defect (%d), the mask (%d) nor the secured behavior (%s)",
			routeID, route, probeDesc, code, defectStatus, maskStatus, expectDesc(secureStatuses))
	}
}

func expectDesc(xs []int) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, fmt.Sprint(x))
	}
	return strings.Join(parts, "|")
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// leakedNeedles returns the tenant-A identifiers found in a response body.
func leakedNeedles(body string, needles []string) []string {
	var out []string
	for _, n := range needles {
		if n != "" && strings.Contains(body, n) {
			out = append(out, n)
		}
	}
	return out
}

// snip keeps report bodies short and single-line.
func snip(body string) string {
	b := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, body)
	if len(b) > 120 {
		b = b[:120] + "…"
	}
	if b == "" {
		b = "∅"
	}
	return b
}

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

func TestAuthzMatrix(t *testing.T) {
	s := bootStack(t)
	l := newLedger(t)
	fx := &s.fx

	// needles: every tenant-A identifier tenant B must never see.
	needles := []string{
		fx.tenantA, fx.custA1, fx.custA2, fx.phoneA1, fx.phoneA2,
		fx.convA1, fx.convA2, fx.convA3, fx.convA4,
		fx.interA1, fx.interA2, fx.interA3, fx.interA4,
		fx.caseA1, fx.caseA2, fx.agentA1, fx.queueA1, fx.wfA1, fx.aiA1,
	}

	// ------------------------------------------------------------- platform
	l.probe(s, "PLAT-1", "GET /healthz", "server.go:64 (public liveness)",
		"no-auth", http.MethodGet, "/healthz", "", "", nil, 200)
	l.probe(s, "PLAT-2", "GET /readyz", "server.go:65 (public readiness)",
		"no-auth", http.MethodGet, "/readyz", "", "", nil, 200)

	// ------------------------------------------------------ credential forms
	l.probe(s, "AUTH-1", "GET /api/v1/customers/ (invalid key)", "tenancy.AuthMiddleware — middleware.go:16",
		"X-API-Key: orvx_<invalid>", http.MethodGet, "/api/v1/customers/", "orvx_"+strings.Repeat("0", 48), "", nil, 401)
	// Bearer-form authentication (tenancy.bearerOrHeader accepts both shapes):
	code, body := s.do(t, http.MethodGet, "/api/v1/customers/", "", "", map[string]string{"Authorization": "Bearer " + fx.keyA})
	if code != 200 {
		l.row("AUTH-2", "GET /api/v1/customers/ (Bearer form)", "tenancy.AuthMiddleware — middleware.go:43 bearerOrHeader",
			"Authorization: Bearer <tenant-A key>", "200", code, "FAIL", snip(body)).HardFails++
		t.Errorf("[AUTH-2] Bearer-form authentication failed: %d\n%s", code, snip(body))
	} else {
		l.row("AUTH-2", "GET /api/v1/customers/ (Bearer form)", "tenancy.AuthMiddleware — middleware.go:43 bearerOrHeader",
			"Authorization: Bearer <tenant-A key>", "200", code, "PASS", "both X-API-Key and Bearer forms authenticate")
	}

	// ------------------------------------------------------------- route tree
	// Enumerated from internal/httpserver/v1.go MountV1 (+ mount helpers).
	// Mount references are the authoritative source of each registration.

	// R0 — GET /api/v1/ (authenticated index)
	r0 := "GET /api/v1/ (index)"
	m0 := "v1.go:92 — authed.Get(\"/\")"
	l.probe(s, "R0", r0, m0, "no-auth", http.MethodGet, "/api/v1/", "", "", nil, 401)
	l.probeNoLeak(s, "R0", r0, m0, "tenant-B key", http.MethodGet, "/api/v1/", fx.keyB, "", needles, 200)
	l.probe(s, "R0", r0, m0, "tenant-A key", http.MethodGet, "/api/v1/", fx.keyA, "", nil, 200)

	// R1 — POST /api/v1/customers/
	r1 := "POST /api/v1/customers/"
	m1 := "handlers.go:75 MountCustomers"
	l.probe(s, "R1", r1, m1, "no-auth", http.MethodPost, "/api/v1/customers/", "", `{"display_name":"x"}`, nil, 401)
	l.probeNoLeak(s, "R1", r1, m1, "tenant-B key (own resource control)",
		http.MethodPost, "/api/v1/customers/", fx.keyB,
		fmt.Sprintf(`{"display_name":"B control","identifiers":[{"type":"phone","value":"2547%s"}]}`, fx.runID+"01"),
		needles, 201)
	l.probe(s, "R1", r1, m1, "tenant-A key", http.MethodPost, "/api/v1/customers/", fx.keyA,
		fmt.Sprintf(`{"display_name":"A create probe","identifiers":[{"type":"phone","value":"2547%s"}]}`, fx.runID+"02"), nil, 201)

	// R2 — GET /api/v1/customers/
	r2 := "GET /api/v1/customers/"
	m2 := "handlers.go:75 MountCustomers"
	l.probe(s, "R2", r2, m2, "no-auth", http.MethodGet, "/api/v1/customers/", "", "", nil, 401)
	l.probeNoLeak(s, "R2", r2, m2, "tenant-B key", http.MethodGet, "/api/v1/customers/", fx.keyB, "", needles, 200)
	l.probeContains(s, "R2", r2, m2, "tenant-A key", http.MethodGet, "/api/v1/customers/", fx.keyA, "", fx.custA1, 200)

	// R3 — GET /api/v1/customers/{id}
	r3 := "GET /api/v1/customers/{id}"
	m3 := "handlers.go:75 MountCustomers"
	l.probe(s, "R3", r3, m3, "no-auth", http.MethodGet, "/api/v1/customers/"+fx.custA1, "", "", nil, 401)
	l.probeNoLeak(s, "R3", r3, m3, "tenant-B key on tenant-A customer", http.MethodGet, "/api/v1/customers/"+fx.custA1, fx.keyB, "", needles, 404)
	l.probe(s, "R3", r3, m3, "tenant-A key", http.MethodGet, "/api/v1/customers/"+fx.custA1, fx.keyA, "", nil, 200)

	// R4 — POST /api/v1/customers/resolve
	r4 := "POST /api/v1/customers/resolve"
	m4 := "handlers.go:75 MountCustomers"
	resolveBody := fmt.Sprintf(`{"type":"phone","value":%q}`, fx.phoneA1)
	l.probe(s, "R4", r4, m4, "no-auth", http.MethodPost, "/api/v1/customers/resolve", "", resolveBody, nil, 401)
	l.probe(s, "R4", r4, m4, "tenant-B key resolving tenant-A identifier", http.MethodPost, "/api/v1/customers/resolve", fx.keyB, resolveBody, nil, 404)
	l.probe(s, "R4", r4, m4, "tenant-A key", http.MethodPost, "/api/v1/customers/resolve", fx.keyA, resolveBody, nil, 200)

	// R5 — GET /api/v1/conversations/
	r5 := "GET /api/v1/conversations/"
	m5 := "handlers.go:140 MountConversations (capability interaction.read/write)"
	l.probe(s, "R5", r5, m5, "no-auth", http.MethodGet, "/api/v1/conversations/", "", "", nil, 401)
	l.probeNoLeak(s, "R5", r5, m5, "tenant-B key", http.MethodGet, "/api/v1/conversations/", fx.keyB, "", needles, 200)
	l.probeContains(s, "R5", r5, m5, "tenant-A key", http.MethodGet, "/api/v1/conversations/", fx.keyA, "", fx.convA1, 200)

	// R6 — GET /api/v1/conversations/{id}
	r6 := "GET /api/v1/conversations/{id}"
	m6 := "handlers.go:140 MountConversations"
	l.probe(s, "R6", r6, m6, "no-auth", http.MethodGet, "/api/v1/conversations/"+fx.convA1, "", "", nil, 401)
	l.probeNoLeak(s, "R6", r6, m6, "tenant-B key on tenant-A conversation", http.MethodGet, "/api/v1/conversations/"+fx.convA1, fx.keyB, "", needles, 404)
	l.probe(s, "R6", r6, m6, "tenant-A key", http.MethodGet, "/api/v1/conversations/"+fx.convA1, fx.keyA, "", nil, 200)

	// R7 — POST /api/v1/conversations/{id}/close
	r7 := "POST /api/v1/conversations/{id}/close"
	m7 := "handlers.go:140 MountConversations"
	l.probe(s, "R7", r7, m7, "no-auth", http.MethodPost, "/api/v1/conversations/"+fx.convA1+"/close", "", "", nil, 401)
	l.probe(s, "R7", r7, m7, "tenant-B key on tenant-A conversation", http.MethodPost, "/api/v1/conversations/"+fx.convA1+"/close", fx.keyB, "", nil, 404)
	l.probe(s, "R7", r7, m7, "tenant-A key", http.MethodPost, "/api/v1/conversations/"+fx.convA3+"/close", fx.keyA, "", nil, 200)

	// R8 — POST /api/v1/conversations/{id}/assign
	r8 := "POST /api/v1/conversations/{id}/assign"
	m8 := "handlers.go:140 MountConversations"
	assignConv := fmt.Sprintf(`{"agent_id":%q}`, fx.agentA1)
	l.probe(s, "R8", r8, m8, "no-auth", http.MethodPost, "/api/v1/conversations/"+fx.convA1+"/assign", "", assignConv, nil, 401)
	l.probe(s, "R8", r8, m8, "tenant-B key on tenant-A conversation", http.MethodPost, "/api/v1/conversations/"+fx.convA1+"/assign", fx.keyB, assignConv, nil, 404)
	l.probe(s, "R8", r8, m8, "tenant-A key", http.MethodPost, "/api/v1/conversations/"+fx.convA1+"/assign", fx.keyA, assignConv, nil, 200)

	// R9 — GET /api/v1/conversations/{id}/interactions
	r9 := "GET /api/v1/conversations/{id}/interactions"
	m9 := "handlers.go:140 MountConversations"
	l.probe(s, "R9", r9, m9, "no-auth", http.MethodGet, "/api/v1/conversations/"+fx.convA1+"/interactions", "", "", nil, 401)
	l.probeForeignList(s, "R9", r9, m9, "tenant-B key on tenant-A conversation", http.MethodGet, "/api/v1/conversations/"+fx.convA1+"/interactions", fx.keyB, needles, "D8", 404)
	l.probe(s, "R9", r9, m9, "tenant-A key", http.MethodGet, "/api/v1/conversations/"+fx.convA1+"/interactions", fx.keyA, "", nil, 200)

	// R10 — POST /api/v1/interactions/
	r10 := "POST /api/v1/interactions/"
	m10 := "handlers.go:223 MountInteractions (capability interaction.read/write)"
	l.probe(s, "R10", r10, m10, "no-auth", http.MethodPost, "/api/v1/interactions/", "", `{"CustomerID":"x"}`, nil, 401)
	l.probeNoLeak(s, "R10", r10, m10, "tenant-B key (own resource control)",
		http.MethodPost, "/api/v1/interactions/", fx.keyB,
		fmt.Sprintf(`{"CustomerID":%q,"Channel":"chat","Direction":"inbound","Source":"matrix:b","Destination":"queue-b","Provider":"matrix","ProviderRef":"matrix:%s-b-r10"}`, fx.custB1, fx.runID),
		needles, 201)
	l.probe(s, "R10", r10, m10, "tenant-A key", http.MethodPost, "/api/v1/interactions/", fx.keyA,
		fmt.Sprintf(`{"CustomerID":%q,"Channel":"chat","Direction":"inbound","Source":"matrix:a","Destination":"queue-a","Provider":"matrix","ProviderRef":"matrix:%s-a-r10"}`, fx.custA1, fx.runID), nil, 201)

	// DEF-1 — D7 ratchet. Step 1: a ref-less create (R10's own probe carries a
	// ProviderRef, so the (internal,'') slot is still free) — hard-assert 201.
	// Step 2: a SECOND ref-less create — D7 answers 409 while the defect
	// stands; once fixed it must 201 (a fresh interaction, nothing duplicated).
	refless := fmt.Sprintf(`{"CustomerID":%q,"Channel":"chat","Direction":"inbound","Source":"matrix:def1","Destination":"queue-a"}`, fx.custA1)
	code, body = s.do(t, http.MethodPost, "/api/v1/interactions/", fx.keyA, refless, nil)
	if code != 201 {
		l.row("DEF-1", "POST /api/v1/interactions/ (1st ref-less create)", m10, "probe 1 (tenant-A key, no ProviderRef)", "201", code, "FAIL", snip(body)).HardFails++
		t.Errorf("[DEF-1] first ref-less create: got %d want 201\n%s", code, snip(body))
	} else {
		l.row("DEF-1", "POST /api/v1/interactions/ (1st ref-less create)", m10, "probe 1 (tenant-A key, no ProviderRef)", "201", code, "PASS", "first ref-less create accepted")
	}
	l.probeDefect(s, "DEF-1", "POST /api/v1/interactions/ (2nd ref-less create)", m10,
		"tenant-A key (no ProviderRef)", http.MethodPost, "/api/v1/interactions/", fx.keyA, refless,
		defectByID("D7"), 409, 201)

	// R11 — GET /api/v1/interactions/{id}
	r11 := "GET /api/v1/interactions/{id}"
	m11 := "handlers.go:223 MountInteractions"
	l.probe(s, "R11", r11, m11, "no-auth", http.MethodGet, "/api/v1/interactions/"+fx.interA1, "", "", nil, 401)
	l.probeNoLeak(s, "R11", r11, m11, "tenant-B key on tenant-A interaction", http.MethodGet, "/api/v1/interactions/"+fx.interA1, fx.keyB, "", needles, 404)
	l.probe(s, "R11", r11, m11, "tenant-A key", http.MethodGet, "/api/v1/interactions/"+fx.interA1, fx.keyA, "", nil, 200)

	// R12 — POST /api/v1/interactions/{id}/transition
	r12 := "POST /api/v1/interactions/{id}/transition"
	m12 := "handlers.go:223 MountInteractions"
	l.probe(s, "R12", r12, m12, "no-auth", http.MethodPost, "/api/v1/interactions/"+fx.interA1+"/transition", "", `{"to":"active"}`, nil, 401)
	l.probe(s, "R12", r12, m12, "tenant-B key on tenant-A interaction", http.MethodPost, "/api/v1/interactions/"+fx.interA1+"/transition", fx.keyB, `{"to":"active"}`, nil, 404)
	l.probe(s, "R12", r12, m12, "tenant-A key", http.MethodPost, "/api/v1/interactions/"+fx.interA1+"/transition", fx.keyA, `{"to":"active"}`, nil, 200)

	// R13 — POST /api/v1/interactions/{id}/assign
	r13 := "POST /api/v1/interactions/{id}/assign"
	m13 := "handlers.go:223 MountInteractions"
	assignInter := fmt.Sprintf(`{"agent_id":%q}`, fx.agentA1)
	l.probe(s, "R13", r13, m13, "no-auth", http.MethodPost, "/api/v1/interactions/"+fx.interA1+"/assign", "", assignInter, nil, 401)
	l.probe(s, "R13", r13, m13, "tenant-B key on tenant-A interaction", http.MethodPost, "/api/v1/interactions/"+fx.interA1+"/assign", fx.keyB, assignInter, nil, 404)
	l.probe(s, "R13", r13, m13, "tenant-A key", http.MethodPost, "/api/v1/interactions/"+fx.interA1+"/assign", fx.keyA, assignInter, nil, 200)

	// R14 — POST /api/v1/agents/
	r14 := "POST /api/v1/agents/"
	m14 := "handlers.go:296 MountAgents"
	l.probe(s, "R14", r14, m14, "no-auth", http.MethodPost, "/api/v1/agents/", "", `{"display_name":"x"}`, nil, 401)
	l.probeNoLeak(s, "R14", r14, m14, "tenant-B key (own resource control)",
		http.MethodPost, "/api/v1/agents/", fx.keyB,
		fmt.Sprintf(`{"external_identity":"authz-agent-b-%s","display_name":"Agent B1"}`, fx.runID), needles, 201)
	l.probe(s, "R14", r14, m14, "tenant-A key", http.MethodPost, "/api/v1/agents/", fx.keyA,
		fmt.Sprintf(`{"external_identity":"authz-agent-a2-%s","display_name":"Agent A2"}`, fx.runID), nil, 201)

	// R15 — GET /api/v1/agents/
	r15 := "GET /api/v1/agents/"
	m15 := "handlers.go:296 MountAgents"
	l.probe(s, "R15", r15, m15, "no-auth", http.MethodGet, "/api/v1/agents/", "", "", nil, 401)
	l.probeNoLeak(s, "R15", r15, m15, "tenant-B key", http.MethodGet, "/api/v1/agents/", fx.keyB, "", needles, 200)
	l.probeContains(s, "R15", r15, m15, "tenant-A key", http.MethodGet, "/api/v1/agents/", fx.keyA, "", fx.agentA1, 200)

	// R16 — GET /api/v1/agents/{id}
	r16 := "GET /api/v1/agents/{id}"
	m16 := "handlers.go:296 MountAgents"
	l.probe(s, "R16", r16, m16, "no-auth", http.MethodGet, "/api/v1/agents/"+fx.agentA1, "", "", nil, 401)
	l.probeNoLeak(s, "R16", r16, m16, "tenant-B key on tenant-A agent", http.MethodGet, "/api/v1/agents/"+fx.agentA1, fx.keyB, "", needles, 404)
	l.probe(s, "R16", r16, m16, "tenant-A key", http.MethodGet, "/api/v1/agents/"+fx.agentA1, fx.keyA, "", nil, 200)

	// R17 — POST /api/v1/queues/
	r17 := "POST /api/v1/queues/"
	m17 := "handlers.go:343 MountQueues"
	l.probe(s, "R17", r17, m17, "no-auth", http.MethodPost, "/api/v1/queues/", "", `{"name":"x"}`, nil, 401)
	l.probeNoLeak(s, "R17", r17, m17, "tenant-B key (own resource control)",
		http.MethodPost, "/api/v1/queues/", fx.keyB,
		fmt.Sprintf(`{"name":"authz-q-b-%s"}`, fx.runID), needles, 201)
	l.probe(s, "R17", r17, m17, "tenant-A key", http.MethodPost, "/api/v1/queues/", fx.keyA,
		fmt.Sprintf(`{"name":"authz-q-a2-%s"}`, fx.runID), nil, 201)

	// R18 — GET /api/v1/queues/
	r18 := "GET /api/v1/queues/"
	m18 := "handlers.go:343 MountQueues"
	l.probe(s, "R18", r18, m18, "no-auth", http.MethodGet, "/api/v1/queues/", "", "", nil, 401)
	l.probeNoLeak(s, "R18", r18, m18, "tenant-B key", http.MethodGet, "/api/v1/queues/", fx.keyB, "", needles, 200)
	l.probeContains(s, "R18", r18, m18, "tenant-A key", http.MethodGet, "/api/v1/queues/", fx.keyA, "", fx.queueA1, 200)

	// R19 — GET /api/v1/queues/{id}
	r19 := "GET /api/v1/queues/{id}"
	m19 := "handlers.go:343 MountQueues"
	l.probe(s, "R19", r19, m19, "no-auth", http.MethodGet, "/api/v1/queues/"+fx.queueA1, "", "", nil, 401)
	l.probeNoLeak(s, "R19", r19, m19, "tenant-B key on tenant-A queue", http.MethodGet, "/api/v1/queues/"+fx.queueA1, fx.keyB, "", needles, 404)
	l.probe(s, "R19", r19, m19, "tenant-A key", http.MethodGet, "/api/v1/queues/"+fx.queueA1, fx.keyA, "", nil, 200)

	// R20 — POST /api/v1/cases/
	r20 := "POST /api/v1/cases/"
	m20 := "handlers.go:390 MountCases (capability case.read/write)"
	l.probe(s, "R20", r20, m20, "no-auth", http.MethodPost, "/api/v1/cases/", "", `{"subject":"x"}`, nil, 401)
	l.probeNoLeak(s, "R20", r20, m20, "tenant-B key (own resource control)",
		http.MethodPost, "/api/v1/cases/", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"subject":"B control case"}`, fx.custB1), needles, 201)
	l.probe(s, "R20", r20, m20, "tenant-A key", http.MethodPost, "/api/v1/cases/", fx.keyA,
		fmt.Sprintf(`{"customer_id":%q,"subject":"A create probe case"}`, fx.custA1), nil, 201)

	// R21 — GET /api/v1/cases/
	r21 := "GET /api/v1/cases/"
	m21 := "handlers.go:390 MountCases"
	l.probe(s, "R21", r21, m21, "no-auth", http.MethodGet, "/api/v1/cases/", "", "", nil, 401)
	l.probeNoLeak(s, "R21", r21, m21, "tenant-B key", http.MethodGet, "/api/v1/cases/", fx.keyB, "", needles, 200)
	l.probeContains(s, "R21", r21, m21, "tenant-A key", http.MethodGet, "/api/v1/cases/", fx.keyA, "", fx.caseA1, 200)

	// R22 — GET /api/v1/cases/{id}
	r22 := "GET /api/v1/cases/{id}"
	m22 := "handlers.go:390 MountCases"
	l.probe(s, "R22", r22, m22, "no-auth", http.MethodGet, "/api/v1/cases/"+fx.caseA1, "", "", nil, 401)
	l.probeNoLeak(s, "R22", r22, m22, "tenant-B key on tenant-A case", http.MethodGet, "/api/v1/cases/"+fx.caseA1, fx.keyB, "", needles, 404)
	l.probe(s, "R22", r22, m22, "tenant-A key", http.MethodGet, "/api/v1/cases/"+fx.caseA1, fx.keyA, "", nil, 200)

	// R23 — POST /api/v1/cases/{id}/transition
	r23 := "POST /api/v1/cases/{id}/transition"
	m23 := "handlers.go:390 MountCases"
	l.probe(s, "R23", r23, m23, "no-auth", http.MethodPost, "/api/v1/cases/"+fx.caseA1+"/transition", "", `{"to":"in_progress"}`, nil, 401)
	l.probe(s, "R23", r23, m23, "tenant-B key on tenant-A case", http.MethodPost, "/api/v1/cases/"+fx.caseA1+"/transition", fx.keyB, `{"to":"in_progress"}`, nil, 404)
	l.probe(s, "R23", r23, m23, "tenant-A key", http.MethodPost, "/api/v1/cases/"+fx.caseA2+"/transition", fx.keyA, `{"to":"in_progress"}`, nil, 200)

	// R24 — POST /api/v1/cases/{id}/notes
	r24 := "POST /api/v1/cases/{id}/notes"
	m24 := "handlers.go:390 MountCases"
	noteA := `{"author_type":"agent","author_id":"matrix-a","body":"matrix-note-A2"}`
	noteDefect := `{"author_type":"agent","author_id":"matrix-b","body":"authz-defect-note-D3"}`
	l.probe(s, "R24", r24, m24, "no-auth", http.MethodPost, "/api/v1/cases/"+fx.caseA1+"/notes", "", noteA, nil, 401)
	l.probeDefect(s, "R24", r24, m24, "tenant-B key on tenant-A case (write-IDOR)",
		http.MethodPost, "/api/v1/cases/"+fx.caseA1+"/notes", fx.keyB, noteDefect,
		defectByID("D3"), 201, 404)
	l.probe(s, "R24", r24, m24, "tenant-A key", http.MethodPost, "/api/v1/cases/"+fx.caseA2+"/notes", fx.keyA, noteA, nil, 201)

	// R25 — GET /api/v1/cases/{id}/notes
	r25 := "GET /api/v1/cases/{id}/notes"
	m25 := "handlers.go:390 MountCases"
	l.probe(s, "R25", r25, m25, "no-auth", http.MethodGet, "/api/v1/cases/"+fx.caseA2+"/notes", "", "", nil, 401)
	l.probeForeignList(s, "R25", r25, m25, "tenant-B key on tenant-A case", http.MethodGet, "/api/v1/cases/"+fx.caseA1+"/notes", fx.keyB, append(needles, "matrix-note-A2"), "D8", 404)
	// positive + proof the D3 injected note is invisible to tenant A:
	l.probeContains(s, "R25", r25, m25, "tenant-A key", http.MethodGet, "/api/v1/cases/"+fx.caseA2+"/notes", fx.keyA, "", "matrix-note-A2", 200)
	code, body = s.do(t, http.MethodGet, "/api/v1/cases/"+fx.caseA2+"/notes", fx.keyA, "", nil)
	if strings.Contains(body, "authz-defect-note-D3") {
		l.row("R25", r25, m25, "D3 contamination check (tenant-A note thread)", "no D3 note", code, "FAIL",
			"D3 injected note visible to tenant A").HardFails++
		t.Errorf("[R25] D3 injected note leaked into tenant A's note thread")
	}

	// R26 — POST /api/v1/cases/{id}/interactions/{iid}/link
	r26 := "POST /api/v1/cases/{id}/interactions/{interactionId}/link"
	m26 := "handlers.go:390 MountCases"
	linkPath := func(caseID, interID string) string {
		return "/api/v1/cases/" + caseID + "/interactions/" + interID + "/link"
	}
	l.probe(s, "R26", r26, m26, "no-auth", http.MethodPost, linkPath(fx.caseA2, fx.interA1), "", "", nil, 401)
	l.probeDefect(s, "R26", r26, m26, "tenant-B key: tenant-A case + tenant-B interaction (write-IDOR)",
		http.MethodPost, linkPath(fx.caseA1, fx.interB1), fx.keyB, "",
		defectByID("D4"), http.StatusNoContent, 404)
	l.probe(s, "R26", r26, m26, "tenant-A key", http.MethodPost, linkPath(fx.caseA2, fx.interA1), fx.keyA, "", nil, 204)

	// R27 — POST /api/v1/calls/
	r27 := "POST /api/v1/calls/"
	m27 := "comms_handlers.go:16 MountCalls"
	l.probe(s, "R27", r27, m27, "no-auth", http.MethodPost, "/api/v1/calls/", "", `{"customer_id":"x"}`, nil, 401)
	l.probeNoLeak(s, "R27", r27, m27, "tenant-B key (own resource control)",
		http.MethodPost, "/api/v1/calls/", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"to":"+254711111111"}`, fx.custB1), needles, 201)
	code, body = s.do(t, http.MethodPost, "/api/v1/calls/", fx.keyA,
		fmt.Sprintf(`{"customer_id":%q,"to":"+254711111112"}`, fx.custA1), nil)
	if code != 201 {
		l.row("R27", r27, m27, "tenant-A key", "201", code, "FAIL", snip(body)).HardFails++
		t.Errorf("[R27] tenant-A placed call: got %d want 201\n%s", code, snip(body))
	} else {
		fx.callA1 = jsonField(t, body, "ID") // telephony.Rec ships untagged (wire key "ID")
		l.row("R27", r27, m27, "tenant-A key", "201", code, "PASS", "placed call "+fx.callA1)
	}

	// R28 — POST /api/v1/calls/{id}/actions
	r28 := "POST /api/v1/calls/{id}/actions"
	m28 := "comms_handlers.go:16 MountCalls"
	l.probe(s, "R28", r28, m28, "no-auth", http.MethodPost, "/api/v1/calls/"+fx.interA2+"/actions", "", `{"action":"hold"}`, nil, 401)
	l.probe(s, "R28", r28, m28, "tenant-B key on tenant-A call", http.MethodPost, "/api/v1/calls/"+fx.interA2+"/actions", fx.keyB, `{"action":"hold"}`, nil, 404)
	l.probe(s, "R28", r28, m28, "tenant-A key (placed call)", http.MethodPost, "/api/v1/calls/"+fx.callA1+"/actions", fx.keyA, `{"action":"hold"}`, nil, 200)
	// legs that never went through POST /calls/ hit D9 (provider unknown-leg
	// error surfaces as 500 instead of a typed status)
	l.probeDefect(s, "R28", r28, m28, "D9 leg: tenant-A key on own unplaced voice interaction", http.MethodPost, "/api/v1/calls/"+fx.interA2+"/actions", fx.keyA, `{"action":"hold"}`,
		defectByID("D9"), 500, 200, 409, 422)

	// R29 — POST /api/v1/messages/
	r29 := "POST /api/v1/messages/"
	m29 := "comms_handlers.go:61 MountMessages"
	l.probe(s, "R29", r29, m29, "no-auth", http.MethodPost, "/api/v1/messages/", "", `{"body":"x"}`, nil, 401)
	l.probeNoLeak(s, "R29", r29, m29, "tenant-B key (own resource control)",
		http.MethodPost, "/api/v1/messages/", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"channel":"whatsapp","from":"+254711111113","to":"+254711111114","body":"b"}`, fx.custB1), needles, 201)
	l.probe(s, "R29", r29, m29, "tenant-A key", http.MethodPost, "/api/v1/messages/", fx.keyA,
		fmt.Sprintf(`{"customer_id":%q,"channel":"whatsapp","from":"+254711111115","to":"+254711111116","body":"a"}`, fx.custA1), nil, 201)

	// R30 — POST /api/v1/routing/interactions/{id}
	r30 := "POST /api/v1/routing/interactions/{id}"
	m30 := "routing_handlers.go:13 MountRouting"
	l.probe(s, "R30", r30, m30, "no-auth", http.MethodPost, "/api/v1/routing/interactions/"+fx.interA1, "", `{"priority":5}`, nil, 401)
	l.probeDefect(s, "R30", r30, m30, "tenant-B key on tenant-A interaction (foreign reference)",
		http.MethodPost, "/api/v1/routing/interactions/"+fx.interA1, fx.keyB, `{"priority":5}`,
		defectByID("D5"), 200, 404, 422)
	l.probe(s, "R30", r30, m30, "tenant-A key", http.MethodPost, "/api/v1/routing/interactions/"+fx.interA3, fx.keyA, `{"priority":5}`, nil, 200)

	// R31 — GET /api/v1/routing/decisions
	r31 := "GET /api/v1/routing/decisions"
	m31 := "routing_handlers.go:13 MountRouting"
	l.probe(s, "R31", r31, m31, "no-auth", http.MethodGet, "/api/v1/routing/decisions", "", "", nil, 401)
	code, body = s.do(t, http.MethodGet, "/api/v1/routing/decisions", fx.keyB, "", nil)
	if code != 200 {
		l.row("R31", r31, m31, "tenant-B key", "200", code, "FAIL", snip(body)).HardFails++
		t.Errorf("[R31] tenant-B decisions list: got %d want 200", code)
	} else if leaks := leakedNeedles(body, needles); len(leaks) > 0 {
		// tenant-B's OWN decision rows embed foreign (tenant-A) ids — the read
		// model is scoped correctly; the foreign reference is D5's pollution.
		l.row("R31", r31, m31, "tenant-B key", "200", code, "PASS",
			"tenant-scoped list; D5 side-effect visible: foreign ids "+strings.Join(leaks, ", ")+" (see D5)")
	} else {
		l.row("R31", r31, m31, "tenant-B key", "200", code, "PASS", "no tenant-A identifiers in response")
	}
	code, body = s.do(t, http.MethodGet, "/api/v1/routing/decisions", fx.keyA, "", nil)
	if code != 200 {
		l.row("R31", r31, m31, "tenant-A key", "200", code, "FAIL", snip(body)).HardFails++
		t.Errorf("[R31] tenant-A decisions list: got %d want 200", code)
	} else {
		l.row("R31", r31, m31, "tenant-A key", "200", code, "PASS", snip(body))
	}

	// R32 — PUT /api/v1/agents/{id}/presence
	r32 := "PUT /api/v1/agents/{id}/presence"
	m32 := "routing_handlers.go:22 (mounted on authed, presence lives with agents)"
	l.probe(s, "R32", r32, m32, "no-auth", http.MethodPut, "/api/v1/agents/"+fx.agentA1+"/presence", "", `{"status":"available"}`, nil, 401)
	l.probe(s, "R32", r32, m32, "tenant-B key on tenant-A agent", http.MethodPut, "/api/v1/agents/"+fx.agentA1+"/presence", fx.keyB, `{"status":"available"}`, nil, 404)
	l.probe(s, "R32", r32, m32, "tenant-A key", http.MethodPut, "/api/v1/agents/"+fx.agentA1+"/presence", fx.keyA, `{"status":"available"}`, nil, 200)

	// R33 — GET /api/v1/agents/{id}/presence
	r33 := "GET /api/v1/agents/{id}/presence"
	m33 := "routing_handlers.go:23"
	l.probe(s, "R33", r33, m33, "no-auth", http.MethodGet, "/api/v1/agents/"+fx.agentA1+"/presence", "", "", nil, 401)
	l.probeNoLeak(s, "R33", r33, m33, "tenant-B key on tenant-A agent", http.MethodGet, "/api/v1/agents/"+fx.agentA1+"/presence", fx.keyB, "", needles, 404)
	l.probe(s, "R33", r33, m33, "tenant-A key", http.MethodGet, "/api/v1/agents/"+fx.agentA1+"/presence", fx.keyA, "", nil, 200)

	// R34 — GET /api/v1/ai/agents/{id}
	r34 := "GET /api/v1/ai/agents/{id}"
	m34 := "ai_handlers.go:14 MountAI"
	l.probe(s, "R34", r34, m34, "no-auth", http.MethodGet, "/api/v1/ai/agents/"+fx.aiA1, "", "", nil, 401)
	l.probeNoLeak(s, "R34", r34, m34, "tenant-B key on tenant-A AI agent", http.MethodGet, "/api/v1/ai/agents/"+fx.aiA1, fx.keyB, "", needles, 404)
	l.probe(s, "R34", r34, m34, "tenant-A key", http.MethodGet, "/api/v1/ai/agents/"+fx.aiA1, fx.keyA, "", nil, 200)

	// R35 — POST /api/v1/ai/agents/{id}/invoke
	r35 := "POST /api/v1/ai/agents/{id}/invoke"
	m35 := "ai_handlers.go:14 MountAI"
	invokeBody := fmt.Sprintf(`{"agent_id":%q,"user_message":"hello","facts":{"channel":"chat"}}`, fx.aiA1)
	l.probe(s, "R35", r35, m35, "no-auth", http.MethodPost, "/api/v1/ai/agents/"+fx.aiA1+"/invoke", "", invokeBody, nil, 401)
	l.probeNoLeak(s, "R35", r35, m35, "tenant-B key invoking tenant-A AI agent", http.MethodPost, "/api/v1/ai/agents/"+fx.aiA1+"/invoke", fx.keyB, invokeBody, needles, 404)
	l.probe(s, "R35", r35, m35, "tenant-A key", http.MethodPost, "/api/v1/ai/agents/"+fx.aiA1+"/invoke", fx.keyA, invokeBody, nil, 200)

	// R36 — POST /api/v1/ai/agents/{id}/tools
	r36 := "POST /api/v1/ai/agents/{id}/tools"
	m36 := "ai_handlers.go:14 MountAI (policy refusal → 403 for non-allowlisted agent)"
	toolBody := fmt.Sprintf(`{"tool":"get_customer","args":{"customer_id":%q}}`, fx.custA1)
	l.probe(s, "R36", r36, m36, "no-auth", http.MethodPost, "/api/v1/ai/agents/"+fx.aiA1+"/tools", "", toolBody, nil, 401)
	// tenant-B agent lookup is tenant-scoped → empty allowlist → policy refusal
	l.probe(s, "R36", r36, m36, "tenant-B key on tenant-A AI agent", http.MethodPost, "/api/v1/ai/agents/"+fx.aiA1+"/tools", fx.keyB, toolBody, nil, 403)
	l.probe(s, "R36", r36, m36, "tenant-A key", http.MethodPost, "/api/v1/ai/agents/"+fx.aiA1+"/tools", fx.keyA, toolBody, nil, 200)

	// R37 — POST /api/v1/workflows/callbacks
	r37 := "POST /api/v1/workflows/callbacks"
	m37 := "workflow_handlers.go:15 MountWorkflows (capability workflow.run)"
	l.probe(s, "R37", r37, m37, "no-auth", http.MethodPost, "/api/v1/workflows/callbacks", "", `{"customer_id":"x"}`, nil, 401)
	l.probeNoLeak(s, "R37", r37, m37, "tenant-B key (own resource control)",
		http.MethodPost, "/api/v1/workflows/callbacks", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"phone":"+254711111117"}`, fx.custB1), needles, 201)
	l.probe(s, "R37", r37, m37, "tenant-A key", http.MethodPost, "/api/v1/workflows/callbacks", fx.keyA,
		fmt.Sprintf(`{"customer_id":%q,"phone":"+254711111118"}`, fx.custA1), nil, 201)

	// R38 — POST /api/v1/workflows/collections
	r38 := "POST /api/v1/workflows/collections"
	m38 := "workflow_handlers.go:15 MountWorkflows"
	l.probe(s, "R38", r38, m38, "no-auth", http.MethodPost, "/api/v1/workflows/collections", "", `{"customer_id":"x"}`, nil, 401)
	l.probeNoLeak(s, "R38", r38, m38, "tenant-B key (own resource control)",
		http.MethodPost, "/api/v1/workflows/collections", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"invoice_ref":"INV-B-1","amount_due":100,"phone":"+254711111119"}`, fx.custB1), needles, 201)
	l.probe(s, "R38", r38, m38, "tenant-A key", http.MethodPost, "/api/v1/workflows/collections", fx.keyA,
		fmt.Sprintf(`{"customer_id":%q,"invoice_ref":"INV-A-1","amount_due":100,"phone":"+254711111120"}`, fx.custA1), nil, 201)

	// R39 — GET /api/v1/workflows/{id}
	r39 := "GET /api/v1/workflows/{id}"
	m39 := "workflow_handlers.go:15 MountWorkflows"
	l.probe(s, "R39", r39, m39, "no-auth", http.MethodGet, "/api/v1/workflows/"+fx.wfA1, "", "", nil, 401)
	l.probeNoLeak(s, "R39", r39, m39, "tenant-B key on tenant-A workflow", http.MethodGet, "/api/v1/workflows/"+fx.wfA1, fx.keyB, "", needles, 404)
	l.probe(s, "R39", r39, m39, "tenant-A key", http.MethodGet, "/api/v1/workflows/"+fx.wfA1, fx.keyA, "", nil, 200)

	// R40 — POST /api/v1/workflows/{id}/cancel
	r40 := "POST /api/v1/workflows/{id}/cancel"
	m40 := "workflow_handlers.go:15 MountWorkflows"
	l.probe(s, "R40", r40, m40, "no-auth", http.MethodPost, "/api/v1/workflows/"+fx.wfA1+"/cancel", "", "", nil, 401)
	l.probe(s, "R40", r40, m40, "tenant-B key on tenant-A workflow", http.MethodPost, "/api/v1/workflows/"+fx.wfA1+"/cancel", fx.keyB, "", nil, 404)
	l.probe(s, "R40", r40, m40, "tenant-A key", http.MethodPost, "/api/v1/workflows/"+fx.wfA1+"/cancel", fx.keyA, "", nil, 200)

	// R41 — GET /api/v1/analytics/summary
	r41 := "GET /api/v1/analytics/summary"
	m41 := "workflow_handlers.go:82 MountAnalytics"
	l.probe(s, "R41", r41, m41, "no-auth", http.MethodGet, "/api/v1/analytics/summary", "", "", nil, 401)
	l.probeNoLeak(s, "R41", r41, m41, "tenant-B key", http.MethodGet, "/api/v1/analytics/summary", fx.keyB, "", needles, 200)
	l.probe(s, "R41", r41, m41, "tenant-A key", http.MethodGet, "/api/v1/analytics/summary", fx.keyA, "", nil, 200)

	// R42 — GET /api/v1/search/conversations
	r42 := "GET /api/v1/search/conversations?q=matrix"
	m42 := "search_handlers.go:49 MountSearchRoutes (mounted; service degrades 503 without OpenSearch)"
	l.probe(s, "R42", r42, m42, "no-auth", http.MethodGet, "/api/v1/search/conversations?q=matrix", "", "", nil, 401)
	for _, who := range []struct{ name, key string }{{"tenant-B key", fx.keyB}, {"tenant-A key", fx.keyA}} {
		code, body = s.do(t, http.MethodGet, "/api/v1/search/conversations?q=matrix", who.key, "", nil)
		switch {
		case code == 503 && strings.Contains(body, "search.not_configured"):
			l.row("R42", r42, m42, who.name, "200 (configured) / 503 (not configured)", code, "PASS-DEGRADED",
				"authenticated; typed degradation search.not_configured (OpenSearch off in devstack)")
		case code == 200:
			l.row("R42", r42, m42, who.name, "200 (configured) / 503 (not configured)", code, "PASS",
				"search configured; tenant filter enforced server-side from the principal")
		default:
			l.row("R42", r42, m42, who.name, "200 (configured) / 503 (not configured)", code, "FAIL", snip(body)).HardFails++
			t.Errorf("[R42] search (%s): got %d want 200 or 503 not_configured\n%s", who.name, code, snip(body))
		}
	}

	// ------------------------------------------------- public webhook ingress
	w1 := "POST /api/v1/webhooks/{provider}"
	mw := "v1.go:72 webhookHandler (public; signature-gated, per-IP limits)"
	l.probe(s, "WEB-1", w1, mw, "no signature", http.MethodPost, "/api/v1/webhooks/simulator", "", `{"event":"call.ringing"}`, nil, 401)
	l.probe(s, "WEB-2", w1, mw, "tampered signature", http.MethodPost, "/api/v1/webhooks/simulator", "",
		`{"event":"call.ringing"}`, map[string]string{"X-Orvexa-Signature": "deadbeef"}, 401)
	// API keys confer NOTHING on webhook ingress (credential-shape separation):
	l.probe(s, "WEB-3", w1, mw, "tenant-B API key + bad signature", http.MethodPost, "/api/v1/webhooks/simulator", fx.keyB,
		`{"event":"call.ringing"}`, map[string]string{"X-Orvexa-Signature": "deadbeef"}, 401)
	// valid platform-signed simulator event targeting tenant-A's pending call:
	wb, whdr := signedWebhook(t, "call.ringing", fx.tenantA, fx.interA4)
	l.probe(s, "WEB-4", w1, mw, "valid signature (public by design)", http.MethodPost, "/api/v1/webhooks/simulator", "", wb, whdr, 202)

	// ------------------------------- conditional identity surface ([O-29])
	// identitySvc == nil in this run (ORVEXA_OIDC_* unset → OIDC disabled by
	// design; the API-key chain is byte-identical to the pre-[O-29] tree).
	l.probe(s, "ID-1", "GET /api/v1/identity/authorize-url", "v1.go:77 (mounted only when identity configured)",
		"probe", http.MethodGet, "/api/v1/identity/authorize-url", "", "", nil, 404)
	l.row("ID-1", "GET /api/v1/identity/authorize-url", "v1.go:77 (mounted only when identity configured)",
		"probe", "404 (unmounted)", 404, "CONDITIONAL", "identity plane disabled in this run (no ORVEXA_OIDC_*) — route absent, fail-closed 404")
	l.probe(s, "ID-2", "GET /api/v1/identity/callback", "v1.go:78 (mounted only when identity configured)",
		"probe", http.MethodGet, "/api/v1/identity/callback", "", "", nil, 404)
	l.row("ID-2", "GET /api/v1/identity/callback", "v1.go:78 (mounted only when identity configured)",
		"probe", "404 (unmounted)", 404, "CONDITIONAL", "identity plane disabled in this run (no ORVEXA_OIDC_*) — route absent, fail-closed 404")

	// ------------------------------------------------- IDOR write + ref-body
	l.probe(s, "IDW-1", "POST /api/v1/conversations/{A-id}/close (2nd A conversation)", "IDOR: real tenant-A UUID via tenant-B key",
		"tenant-B key", http.MethodPost, "/api/v1/conversations/"+fx.convA2+"/close", fx.keyB, "", nil, 404)
	l.probe(s, "IDW-2", "POST /api/v1/interactions/{A-id}/assign", "IDOR: real tenant-A UUID via tenant-B key",
		"tenant-B key", http.MethodPost, "/api/v1/interactions/"+fx.interA2+"/assign", fx.keyB, assignInter, nil, 404)
	l.probeForeignList(s, "IDW-3", "GET /api/v1/cases/{A-id}/notes", "IDOR: real tenant-A UUID via tenant-B key",
		"tenant-B key", http.MethodGet, "/api/v1/cases/"+fx.caseA2+"/notes", fx.keyB, needles, "D8", 404)

	// reference-in-body probes: cross-tenant UUIDs in BODIES of B-key creates
	refBody := func(custID string) string {
		return fmt.Sprintf(`{"CustomerID":%q,"Channel":"chat","Direction":"inbound","Source":"matrix:ref","Destination":"queue-b","Provider":"matrix","ProviderRef":"matrix:%s-ref-inter"}`, custID, fx.runID)
	}
	l.probeDefect(s, "REF-1", "POST /api/v1/interactions/ (tenant-A customer_id in body)", m10,
		"tenant-B key + tenant-A customer_id", http.MethodPost, "/api/v1/interactions/", fx.keyB, refBody(fx.custA1),
		defectByID("D1"), 201, 404, 422)
	// REF-2/REF-3 are masked-defect rows: calls/messages take no provider_ref,
	// so a SECOND ref-less create per provider can hit D7's 409 before the
	// cross-tenant reference (D1) is even evaluated. 409 = D7 masks D1 this
	// run; 201 = D1 reproduced; 404/422 = secured.
	l.probeDefectMasked(s, "REF-2", "POST /api/v1/calls/ (tenant-A customer_id in body)", m27,
		"tenant-B key + tenant-A customer_id", http.MethodPost, "/api/v1/calls/", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"to":"+254711111121"}`, fx.custA1),
		defectByID("D1"), 201, "D7", 409, 404, 422)
	l.probeDefectMasked(s, "REF-3", "POST /api/v1/messages/ (tenant-A customer_id in body)", m29,
		"tenant-B key + tenant-A customer_id", http.MethodPost, "/api/v1/messages/", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"channel":"sms","from":"+254711111122","to":"+254711111123","body":"ref"}`, fx.custA1),
		defectByID("D1"), 201, "D7", 409, 404, 422)
	l.probeDefect(s, "REF-4", "POST /api/v1/cases/ (tenant-A customer_id in body)", m20,
		"tenant-B key + tenant-A customer_id", http.MethodPost, "/api/v1/cases/", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"subject":"ref probe"}`, fx.custA1),
		defectByID("D2"), 201, 404, 422)
	l.probeDefect(s, "REF-5", "POST /api/v1/workflows/callbacks (tenant-A customer_id in body)", m37,
		"tenant-B key + tenant-A customer_id", http.MethodPost, "/api/v1/workflows/callbacks", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"phone":"+254711111124"}`, fx.custA1),
		defectByID("D6"), 201, 404, 422)
	l.probeDefect(s, "REF-6", "POST /api/v1/workflows/collections (tenant-A customer_id in body)", m38,
		"tenant-B key + tenant-A customer_id", http.MethodPost, "/api/v1/workflows/collections", fx.keyB,
		fmt.Sprintf(`{"customer_id":%q,"invoice_ref":"INV-REF","amount_due":1,"phone":"+254711111125"}`, fx.custA1),
		defectByID("D6"), 201, 404, 422)

	// enumeration-oracle check: foreign id must be indistinguishable from a
	// missing id (both 404, same shape).
	randUUID := uuid.NewString()
	l.probe(s, "ORACLE-1", "GET /api/v1/customers/<random valid UUID> (tenant-B key)", "no-enumeration-oracle",
		"tenant-B key", http.MethodGet, "/api/v1/customers/"+randUUID, fx.keyB, "", nil, 404)
	l.probe(s, "ORACLE-2", "GET /api/v1/customers/<tenant-A customer> (tenant-B key)", "no-enumeration-oracle",
		"tenant-B key", http.MethodGet, "/api/v1/customers/"+fx.custA1, fx.keyB, "", nil, 404)

	// -------------------------------------------------------------- report
	writeReport(t, s, l)

	if l.hardFails() > 0 {
		t.Fatalf("authz matrix: %d hard assertion failure(s) — see MATRIX.md", l.hardFails())
	}
}

func (l *ledger) hardFails() int {
	n := 0
	for _, r := range l.rows {
		n += r.HardFails
	}
	return n
}
