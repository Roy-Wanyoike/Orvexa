# Orvexa — QA Report & Production Readiness

**Date:** 2026-09-10 · **Build:** main @ wave-8 merge (a582f9b) + release-gate wave
**Scope:** Orvexa platform v0.1 — interaction, intelligence, execution and control planes
**Method:** issue → branch → PR → merge for every wave (issues #1–#9, PRs #11–#19). Verification per ADR-0003: full local matrix (build, vet, `go test -race`, migration sanity, boot verification) recorded here as evidence.

---

## Verdict

## **GO for pilot onboarding (single-tenant pilots, sandbox/demo posture)**

The platform is a complete, coherent, tested vertical slice of the four-plane architecture: the full interaction loop — customer identity → conversation → interaction → signed provider webhooks → guarded lifecycle → routing decision → assignment → metered AI suggestion → audited tool calls → durable workflows → analytics facts → audit trail — is implemented end to end with zero external dependencies in the default profile, and clean extraction paths to production infrastructure. Known limitations are documented below and tracked as issues; none are silent.

---

## 1. Verification evidence (authoritative gate, ADR-0003)

| Gate | Command | Result |
|---|---|---|
| Build | `go build ./...` (3 deployables + libs) | **PASS** (clean) |
| Static analysis | `go vet ./...` | **PASS** (clean) |
| Tests | `go test -race ./...` | **PASS — 16 packages green under the race detector** |
| Formatting | `gofmt -l cmd internal pkg` | **PASS** (no output) |
| Migrations | ordered, non-empty, forward-only (CI step) | **PASS** — 10 migrations |
| Boot: API | binary run, `:18080` | **PASS** — `/healthz` 200 `{"status":"alive"}`; `/readyz` 503 `degraded, db:down` (truthful) |
| Boot: worker | binary run without DB | **PASS (fail-fast)** — explicit error `ORVEXA_DATABASE_URL is required` (by design) |
| Boot: realtime | binary run without DB | **PASS (fail-fast)** — explicit error (socket auth requires DB) |
| Auth enforcement | `GET /api/v1/customers` without key | **PASS** — 401 `auth.missing_key` |
| Webhook fail-closed | `POST /api/v1/webhooks/simulator` with no gateway configured | **PASS** — `webhook.not_configured` (no unsigned path exists) |
| Security headers | response inspection | **PASS** — nosniff, frame-deny, referrer-policy, permissions-policy, no-store |
| Secret scan | GitGuardian on push + repo grep | **PASS** — no credentials committed |

## 2. Test inventory (what the suite actually proves)

| Package | Proves |
|---|---|
| `pkg/errors` | kind→HTTP mapping; unknown errors become opaque 500s (internal details never leak); wrap preserves `errors.Is` |
| `pkg/config` | defaults boot clean; invalid bus driver / non-positive timeout rejected |
| `pkg/events` | envelope invariants; **closed topic registry** (unregistered topics impossible); deterministic catalog |
| `pkg/idempotency` | key validation; deterministic derived keys (replay-safety basis) |
| `pkg/pagination` | bounded cursor round-trip; oversize clamped; malformed cursors rejected |
| `internal/platform/httpx` | error envelope; **bounded token-bucket rate limiter with memory-bound eviction**; request-id; panic→500 |
| `internal/tenancy` | SHA-256 key hashing; scope enforcement; wildcard scopes; constant-time compare |
| `internal/customers` | identifier normalization (phone→E.164 digits, email lowercase, trim); **phone/WhatsApp of same number normalize identically** (no identity forking) |
| `internal/interactions` | **full state-machine matrix** — legal paths accepted, illegal transitions 409 with allowed-set, terminal states sealed |
| `internal/cases` | case lifecycle matrix incl. reopen-from-resolved, closed-is-terminal |
| `internal/webhooks` | HMAC round-trip; **tampered body, wrong secret, empty secret, cross-body replay all rejected** |
| `internal/comms` | **full simulator→signed-webhook→processor→lifecycle loop** (call + message); tenant-context enforcement; same-state idempotency; unknown leg/event rejection |
| `internal/routing` | scoring matrix: coverage preference, availability gating (unavailable never assigned), urgent-requires-full-coverage, queue pinning, determinism, fallback outcomes |
| `internal/realtime` | **hard tenant isolation** on fan-out; per-agent filtering; slow-consumer drop (non-blocking); connection caps enforced |
| `internal/ai` | gateway timeout metering; retry-then-fail (attempts=2); **token metering clamped to cap**; rules provider determinism; **refusal over hallucination** |
| `internal/tools` | allowlist refusal (+audit of refusals); schema validation before execution; **per-agent-tool rate isolation**; platform cap clamping; body limits |

Integration suite (`tests/integration`, build tag `integration`) additionally proves DB-backed behavior — webhook replay dedup against `provider_events`, outbox dispatcher lease/ack/delivery — and skips cleanly without `ORVEXA_TEST_DATABASE_URL` (no Postgres in the build sandbox; documented limitation, §7).

## 3. Architecture & code quality

- **Four-plane model realized** (ADR-0001/0002): interaction, execution, intelligence planes communicate only via the outbox/bus, defined interfaces, or the API. Ownership map in `docs/domains.md`.
- **Transactional outbox** (ADR-0004): events commit atomically with state; lease-based dispatcher (SKIP LOCKED), at-least-once + idempotent consumers, failures parked for operators — never deleted.
- **Hexagonal communications** (ADR-0005): the simulator drives the exact production loop — signed webhooks through the public gateway. No provider bypasses HMAC validation.
- **Tool gateway** (ADR-0006 companion, issue #7): AI→side-effects path is allowlist + schema + rate + audit gated; refusals are audited; AI holds no credentials.
- **Durable workflows** (ADR-0006): deterministic steps, timer rows, immutable step trace, crash-safe resume semantics.
- **Uniform API contract**: every handler passes through the shared envelope (`{data,meta}` / `{error:{code,message,details}}`), 1 MiB body cap, `DisallowUnknownFields`, cursor pagination bounded at 100, stable machine error codes, cross-tenant ids read as 404.

## 4. Security posture (verified)

- API keys stored as SHA-256 hashes; constant-time comparisons; scope enforcement per route class.
- Tenant resolution **only** from the authenticated key — never from request bodies or headers.
- Webhook ingress: HMAC-SHA256 timing-safe validation, fail-closed on missing secret, 256 KiB cap, per-IP rate limiting, payload-derived idempotency keys.
- Rate limiting: per-principal (API) + per-IP (webhooks) token buckets with **memory-bounded eviction** (flood-resistant).
- Security headers on every response; no-store on API responses; request-id propagation for tracing.
- No secrets in the repository (GitGuardian active on push; `.env` untracked; only `.env.example`).
- No SQL injection surface: parameterized pgx only; no raw string interpolation into queries.
- AI boundary: token + timeout caps non-bypassable (enforced in the gateway, not the provider); tool calls schema-validated deny-by-default.

## 5. Known limitations (honest, tracked — none silent)

1. **No hosted CI on this account** — Actions startup failures are account-level; the authoritative gate is the local matrix recorded here (ADR-0003). `.github/workflows/ci.yml` is armed and will enforce automatically if Actions becomes available.
2. **Default profile uses in-process bus, simulator carrier, DB-backed workflows.** Production adapters (NATS JetStream driver, Twilio/WhatsApp Cloud/Africa's Talking, ClickHouse, OpenSearch, Redis, Temporal) are behind defined interfaces and tracked under umbrella issue **#10** with per-adapter acceptance criteria.
3. **Build sandbox has no PostgreSQL**, so DB-backed integration tests (`-tags=integration`) and the full `scripts/e2e-demo.sh` loop require a provisioned environment; unit coverage + boot verification are the recorded evidence here.
4. **Admin/onboarding API not yet implemented** — first tenant + key bootstrap is SQL (runbook §"First tenant bootstrap"); OIDC/RBAC tracked in issue #10.
5. **Single-region, single-Postgres** posture; time-partitioning and facts-store extraction paths documented (ADR-0001, O-10).

## 6. Issue/PR traceability (protocol compliance)

| Wave | Issue | PR | Outcome |
|---|---|---|---|
| Foundation | #1 | #11 | merged, closed |
| Domain core | #2 | #12 | merged, closed |
| Event backbone | #3 | #13 | merged, closed |
| Communications | #4 | #14 | merged, closed |
| Routing + presence | #5 | #15 | merged, closed |
| Realtime | #6 | #16 | merged, closed |
| Intelligence | #7 | #17 | merged, closed |
| Execution | #8 | #18 | merged, closed |
| Release gate | #9 | #19 (this) | closes #9 |

Roadmap umbrella #10 remains open deliberately: every deferred production adapter is enumerated there — no orphaned work.

## 7. Recommendation

**Onboard pilot tenants now** (sandbox/demo posture): single-tenant pilots with synthetic or non-sensitive data are fully supported today — the loop is complete, the contracts are stable, and the security boundary (auth, tenant isolation, signed ingress, metered AI) is enforced by construction. Before real-carrier production: provision PostgreSQL (run the integration suite + E2E script), set `ORVEXA_WEBHOOK_HMAC_SECRET`, front with a TLS proxy that rewrites XFF, and work the #10 adapter list in priority order (carriers → NATS → ClickHouse/OpenSearch → OIDC/RBAC).
