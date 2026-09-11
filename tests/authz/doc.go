//go:build authz

// Package authz holds the tenancy & authorization matrix harness (issue #40).
//
// It boots a REAL Orvexa stack end to end and proves, with HTTP evidence,
// that the v1 API enforces authentication and tenant isolation across 100%
// of the route tree enumerated from internal/httpserver/v1.go:
//
//	devstack PostgreSQL (scripts/devstack.sh start, migrations applied)
//	  + two tenants + two API keys minted via the documented bootstrap path
//	    (docs/runbooks/operations.md § "First tenant bootstrap" — SQL, because
//	    no admin API exists; ORVEXA_BOOTSTRAP_API_KEY is a loaded-but-dead
//	    knob, tracked in issue #56)
//	  + `go run ./cmd/api` on a loopback test port (ORVEXA_* env, simulator
//	    comms providers, inproc bus)
//	  + seeded tenant-A/B fixtures created through the public API itself
//
// The matrix probes every route three ways and asserts:
//
//	no credentials        → 401 on every authenticated route
//	tenant-B key on a
//	tenant-A resource     → non-2xx (404 canonical; 403 where capability
//	                        middleware refuses), never tenant-A data
//	tenant-A key          → the method-appropriate success status
//
// Plus IDOR probes (real tenant-A UUIDs substituted into tenant-B requests
// across customers/conversations/interactions/cases + write variants), a
// no-enumeration-oracle check (foreign id ≡ missing id), cross-tenant
// reference-in-body probes, and public/conditional surfaces (health, webhook
// gateway, search degradation, unmounted identity routes).
//
// Assertion classes (honest ratchet — see MATRIX.md legend):
//
//	ASSERT   hard invariant; a violation fails the suite immediately.
//	DEFECT   behavior recorded as a filed isolation defect (type/bug issue).
//	         The row passes while the platform exhibits exactly the recorded
//	         defect status OR the secure status (after a fix lands, the row
//	         still passes and MATRIX.md shows the secured result). Any other
//	         status fails. Defects are never silently fixed in this package —
//	         module owners fix them; this suite re-evidences.
//
// INVOCATION (requires the devstack — this suite drives real processes):
//
//	scripts/devstack.sh start          # or: make devstack-up
//	go test -race -tags=authz ./tests/authz/
//
// Without the `authz` build tag the package is excluded and `go test ./...`
// (the unit/CI matrix) is byte-identical to the pre-#40 tree.
//
// Environment knobs (all optional):
//
//	ORVEXA_TEST_DATABASE_URL  external PG URL; skips the devstack autostart
//	                          (same override contract as `make integration`)
//	ORVEXA_DEVSTACK_PORT      devstack port when auto-starting (default
//	                          55439 — deliberately NOT the shared 55432)
//	ORVEXA_AUTHZ_REPORT       report path override (default MATRIX.md next
//	                          to this package, committed as evidence)
//
// Secrets posture: API keys are minted at runtime and never committed;
// reports render keys masked (first 10 bytes). The webhook HMAC secret is a
// fixed test fixture. No credential material appears in logs or errors.
package authz
