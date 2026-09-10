# Orvexa — Security Posture (independent sweep)

**Task:** issue [#39](https://github.com/Roy-Wanyoike/Orvexa/issues/39) ([O-30]) · **Agent:** D1 (Senior Security Engineer, independent reviewer — separate from all implementing agents)
**Base:** main @ `6459bc5` (glue-wave merge), sweep branch `security/sweep` @ `197c04e` · **Date:** 2026-09-10
**Method:** adversarial, evidence-first, read-only outside the owned file set (`scripts/security-scan.sh`, `docs/security/`, `internal/platform/httpx/` header+limiter, `.env.example` security vars). Cross-surface defects are filed as issues, never patched here.
**Prior baseline:** A1 hygiene audit ([#20], `docs/audit/repository-audit.md` §5 found zero secrets pre-waves A–C).

---

## 1. Secret scan — `scripts/security-scan.sh` (new, shipped with this PR)

**Properties:** self-contained (bash + git + python3 stdlib — zero installs, zero network); scans every git-TRACKED file (`git ls-files`, 261/261 text); exit 0 clean / 1 findings / 2 env error; output is a summary table plus per-hit detail with **secrets redacted (first 10 chars)**; every allowlist exemption is an explicit, justified rule — no blanket file skips for pattern checks.

| Check | Pattern family | Hits | Allowlisted (justified) | Actionable |
|---|---|---:|---:|---:|
| C1 | AWS access keys (`AKIA…16`) | 0 | 0 | 0 |
| C2 | AWS secret-style assigns | 0 | 0 | 0 |
| C3 | Slack tokens (`xox[baprs]-…`) | 0 | 0 | 0 |
| C4 | OpenAI keys (`sk-…`, `sk-proj-…`) | 0 | 0 | 0 |
| C5 | Google API keys (`AIza…35`) | 0 | 0 | 0 |
| C6 | GitHub tokens (`ghp_…`, `github_pat_…`) | 0 | 0 | 0 |
| C7 | Private-key blocks (`-----BEGIN … PRIVATE KEY-----`) | 0 | 0 | 0 |
| C8 | Credential URLs (`scheme://user:pass@host`) | 19 | 19 | 0 |
| C9 | High-entropy strings (≥40 chars, Shannon ≥ 4.5 bits/char) | 8 | 8 | 0 |
| C10 | Tracked `.env` files (except `.env.example`) | 0 | 0 | 0 |
| C11 | Secret-named artifacts (`*.pem`, `*.key`, `id_rsa*`, `credentials*.json`, …) | 0 | 0 | 0 |
| C12 | Generic credential assigns (`password/secret/token/api_key = "8+ char literal"`) | 4 | 4 | 0 |

**Result: CLEAN (exit 0) — zero actionable secret findings.** Full table output is attached to the PR.

Allowlist policy (each reviewed by hand, printed per-hit by the scanner):

- **Loopback/dev-host credential URLs** (12 hits): `postgres://postgres:postgres@127.0.0.1:55432/…`, `postgres://orvexa:orvexa@localhost:5432/…`, `clickhouse://default:orvexa@127.0.0.1:9000/…` in `.env.example`, `docker-compose.dev.yml`, `docs/devstack.md`, docs/audit quotations, and ClickHouse test fixtures. Local-only dummies behind the documented devstack; production values arrive only via environment.
- **Placeholder userinfo** (7 hits): `redis://[user:pass@]host:port[/db]` format documentation in limiter/presence error strings; `clickhouse://user:pass@host:9000/db` format docs; `https://user:pass@cdn.example.test/…` — a **negative test case** asserting credentials-in-URL are rejected; shell `${VAR}` interpolations in `devstack.sh` (no literal credential exists).
- **Test-fixture constants** (4 hits C12 / 5 hits C9): `cfg.APIKey = "wrong-key"`, `cfg.password = "wrong-password"` (adversarial reject-cases), self-described `EAAG-test-fixture-token-…`, golden-fixture sequential hex `AC0123456789abcdef…`, Go test identifiers, `ORVEXA_*` env-var references. Mirrors A1's audit §5 classifications.
- **go.sum / go.mod**: exempt from the ENTROPY check only (public module checksums); still scanned by every token-pattern check.

**Calibration:** the entropy threshold (≥40 chars, Shannon ≥ 4.5 bits/char) was tuned against this tree: at 4.5 the residual set is exactly the 8 allowlisted shapes above; lower thresholds drown the signal in import paths and test identifiers (742 candidates at 3.8). Structural filters: URLs (cred-URLs remain covered by C8), import/doc references, `ORVEXA_*` env references, Go test identifiers, golden-fixture dummy hex.

**Adversarial canary validation:** a file planted (then removed) with one sample of each family — AWS key, Slack token, OpenAI key, Google key, GitHub token, private-key block, credential URL with real-looking password, `password = "…"`, and a 48-char high-entropy base64 string — produced **11/11 expected findings, exit 1**. The scanner is not a no-op.

Corroboration: `.gitignore` covers `.env`/`.env.*` with `!.env.example` (verified tracked-tree census: no `.env` files tracked).

## 2. Dependency audit — govulncheck triage (every finding)

`govulncheck ./...` (symbol level, `golang.org/x/vuln/cmd/govulncheck@latest`) — **9 findings: 1 symbol-level (reachable), 2 package-level, 6 module-level.**

| # | ID | Description | Module | Found | Fixed in | Level | Reachable in Orvexa? | Action |
|---|---|---|---|---|---|---|---|---|
| 1 | **GO-2026-5004** | **SQL injection** — placeholder confusion with dollar-quoted string literals | `jackc/pgx/v5` | v5.7.2 | v5.9.2 | **symbol** | **YES (call-graph)**: `internal/cases/service.go:335 ListNotes → pgxpool.Pool.Query → sanitize.SanitizeSQL` | **REQUIRED — bump pgx ≥ v5.9.2 → [#81]** |
| 2 | GO-2026-4772 (CVE-2026-33816) | pgx | `jackc/pgx/v5` | v5.7.2 | v5.9.0 | package | vulnerable symbols not called | cleared by the same bump |
| 3 | GO-2026-4771 (CVE-2026-33815) | pgx | `jackc/pgx/v5` | v5.7.2 | v5.9.0 | package | vulnerable symbols not called | cleared by the same bump |
| 4 | GO-2026-6355 | DoS on deadlocked established channel (`x/crypto/ssh`) | `golang.org/x/crypto` | v0.54.0 | v0.56.0 | module | indirect dep; `ssh` not imported anywhere in `internal/`, `cmd/`, `pkg/` | cleared via transitive bump; monitor |
| 5 | GO-2026-6354 | `x/crypto/ssh` | `golang.org/x/crypto` | v0.54.0 | v0.56.0 | module | not imported | same |
| 6 | GO-2026-6303 | `x/crypto` | `golang.org/x/crypto` | v0.54.0 | v0.55.0 | module | not imported | same |
| 7 | GO-2026-5932 | `x/crypto/openpgp` unmaintained/unsafe-by-design | `golang.org/x/crypto` | v0.54.0 | **N/A** | module | `openpgp` not imported | **accepted residual** (no upstream fix exists; unreachable) — re-check if an import ever appears |
| 8 | GO-2026-5777 | IP spoofing via chi **RealIP** middleware (`X-Forwarded-For`) | `go-chi/chi/v5` | v5.1.0 | v5.3.0 | module | **RealIP is not used** (grep-verified). `httpserver/v1.go clientIP()` deliberately keys on `r.RemoteAddr` | bump chi ≥ v5.3.0 → [#81] |
| 9 | GO-2026-5775 | IP spoofing via chi middleware | `go-chi/chi/v5` | v5.1.0 | v5.3.0 | module | same | same |

**Triage of the reachable finding (GO-2026-5004), stated honestly:** all Orvexa SQL uses the extended protocol with parameterized placeholders (`$1…$n` bound args — verified at the cited call site); the vulnerable `sanitize.SanitizeSQL` path is exercised only by query-string interpolation, which this codebase never performs. govulncheck reachability is static call-graph reachability, not proven exploitability. **Nevertheless, a SQL-injection-class finding with a same-major patch available does not ride a public repo.** go.mod is outside D1's exclusive file set, so the bump is filed as [#81] with the exact change and the verdict below is BLOCKED until it lands.

## 3. Security headers — every route (fixed in this PR)

**Proven gap on `6459bc5`:** `httpx.SecurityHeaders` set `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, `Permissions-Policy`, `Cache-Control` — but **no `Content-Security-Policy` and no `Strict-Transport-Security`**. Fixed in `internal/platform/httpx/middleware.go` (owned file) and pinned by tests.

| Header | Value | Status before | Status after |
|---|---|---|---|
| `Content-Security-Policy` | `default-src 'none'; frame-ancestors 'none'; base-uri 'none'` | **MISSING** | ✅ set |
| `Strict-Transport-Security` | `max-age=31536000; includeSubDomains` | **MISSING** | ✅ set |
| `X-Content-Type-Options` | `nosniff` | ✅ | ✅ |
| `X-Frame-Options` | `DENY` | ✅ | ✅ |
| `Referrer-Policy` | `strict-origin-when-cross-origin` | ✅ | ✅ |
| `Permissions-Policy` | `camera=(), microphone=(), geolocation=()` | ✅ | ✅ |
| `Cache-Control` | `no-store` | ✅ | ✅ |

- **Route coverage:** the middleware is mounted globally (`internal/httpserver/server.go`: `r.Use(httpx.RequestID)`, `r.Use(httpx.SecurityHeaders)`, `r.Use(httpx.Recoverer)`) — every route class inherits it: `/healthz`, `/readyz`, `POST /api/v1/webhooks/{provider}`, `/api/v1/identity/*` (public), the authenticated `/api/v1/*` group, 404s, and panic-derived 500s.
- **Why this CSP:** the API surface is JSON-only (repo-wide grep: zero `text/html` responses), so `default-src 'none'` with no allowances costs nothing. `frame-ancestors 'none'` is the CSP complement to `X-Frame-Options: DENY` (both sent — legacy clients honor only the latter).
- **HSTS unconditionally:** RFC 6797 §6.1 makes clients ignore the header over non-secure transport (harmless in local dev); in the documented production topology TLS terminates at the trusted edge proxy where `r.TLS` is nil at this handler — gating on `r.TLS != nil` would silently disable HSTS exactly where it matters.
- **Tests (new):** `TestSecurityHeadersOnEveryResponse` (exact-value matrix), `TestSecurityHeadersOnErrorAndPanicPaths` (headers survive 4xx `WriteError` and the `Recoverer` panic path). `go test -race ./internal/platform/httpx/...` green.

## 4. Rate-limit coverage matrix — every mutation route

**Route census method:** full grep of `r.Get/Post/Put/Patch/Delete` over `internal/httpserver/` (chi tree). The tree contains **24 mutation routes** (23 POST, 1 PUT; zero PATCH/DELETE). Group wiring: everything except the three public routes below sits inside the `authed` group guarded by `tenancy.AuthMiddleware` (or `identity.DualAuth` wrapping it), which applies the limiter **per request, before any handler runs**.

| # | Route | Method | Mounted at | Limiter | Key (server-side) | Covered |
|---|---|---|---|---|---|---|
| 0 | `/api/v1/webhooks/{provider}` | POST | v1.go:72 | `webhookLimiter` 120/min burst 60 | `clientIP(r)` = **socket `RemoteAddr`** (not `X-Forwarded-For`) | ✅ |
| 1 | `/api/v1/customers/` | POST | handlers.go:78 | `limiter` 600/min burst 120 | `principal.APIKeyID` | ✅ |
| 2 | `/api/v1/customers/resolve` | POST | handlers.go:81 | 〃 | 〃 | ✅ |
| 3 | `/api/v1/conversations/{id}/close` | POST | handlers.go:145 | 〃 | 〃 | ✅ |
| 4 | `/api/v1/conversations/{id}/assign` | POST | handlers.go:146 | 〃 | 〃 | ✅ |
| 5 | `/api/v1/interactions/` | POST | handlers.go:226 | 〃 | 〃 | ✅ |
| 6 | `/api/v1/interactions/{id}/transition` | POST | handlers.go:228 | 〃 | 〃 | ✅ |
| 7 | `/api/v1/interactions/{id}/assign` | POST | handlers.go:229 | 〃 | 〃 | ✅ |
| 8 | `/api/v1/agents/` | POST | handlers.go:299 | 〃 | 〃 | ✅ |
| 9 | `/api/v1/queues/` | POST | handlers.go:346 | 〃 | 〃 | ✅ |
| 10 | `/api/v1/cases/` | POST | handlers.go:393 | 〃 | 〃 | ✅ |
| 11 | `/api/v1/cases/{id}/transition` | POST | handlers.go:396 | 〃 | 〃 | ✅ |
| 12 | `/api/v1/cases/{id}/notes` | POST | handlers.go:397 | 〃 | 〃 | ✅ |
| 13 | `/api/v1/cases/{id}/interactions/{interactionId}/link` | POST | handlers.go:398 | 〃 | 〃 | ✅ |
| 14 | `/api/v1/calls/` | POST | comms_handlers.go:22 | 〃 | 〃 | ✅ |
| 15 | `/api/v1/calls/{id}/actions` | POST | comms_handlers.go:23 | 〃 | 〃 | ✅ |
| 16 | `/api/v1/messages/` | POST | comms_handlers.go:67 | 〃 | 〃 | ✅ |
| 17 | `/api/v1/routing/interactions/{id}` | POST | routing_handlers.go:18 | 〃 | 〃 | ✅ |
| 18 | `/api/v1/routing/agents/{id}/presence` | PUT | routing_handlers.go:22 | 〃 | 〃 | ✅ |
| 19 | `/api/v1/ai/agents/{id}/invoke` | POST | ai_handlers.go:20 | 〃 | 〃 | ✅ |
| 20 | `/api/v1/ai/agents/{id}/tools` | POST | ai_handlers.go:21 | 〃 | 〃 | ✅ |
| 21 | `/api/v1/workflows/callbacks` | POST | workflow_handlers.go:20 | 〃 | 〃 | ✅ |
| 22 | `/api/v1/workflows/collections` | POST | workflow_handlers.go:21 | 〃 | 〃 | ✅ |
| 23 | `/api/v1/workflows/{id}/cancel` | POST | workflow_handlers.go:23 | 〃 | 〃 | ✅ |

RBAC-gated subtrees (`interactions`, `cases`, `workflows` under `identity.RequireMethodCapability` / `RequireCapability`) run the limiter **before** the capability gate (limiter lives in the auth layer) — denied-by-RBAC requests still consume bucket tokens, which is the fail-safe direction.

**Key-spoofability analysis (server-side keying verified):**

- API-key path: key = `principal.APIKeyID` — resolved server-side from the credential row; **no client input reaches the limiter key**.
- OIDC path ([#38] `DualAuth`): key = `"oidc:" + claims.Subject` — the `sub` of a **signature-verified** JWT (forged tokens are rejected before the limiter; a failed verification is final, never retried as an API key).
- Webhook ingress: key = parsed socket `RemoteAddr`. The code **deliberately does not** read `X-Forwarded-For` for the limiter (and chi `RealIP` is not used — also the subject of GO-2026-5777/5775, unreachable here for exactly that reason). Trade-off documented in v1.go: behind the trusted proxy all providers share one bucket — collateral limiting is fail-safe.
- **Limiter drivers:** in-process token bucket (default; bounded at 10k buckets, oldest-half eviction) or Redis fixed-window (`ORVEXA_REDIS_URL`, [#37]) with per-scope keys `orvexa:rl:v1:<scope>:<key>:<bucket>`; fail-open except auth-critical surfaces which fail closed (429 + Retry-After). No silent fallback hybrid exists (documented design).

**Limiter deficiency found & fixed in httpx (owned file, proven by measurement):** `(*RateLimit).evictLocked` used a hand-rolled insertion sort — **O(n²)** per eviction. Benchmark at the 10k-bucket cap under a unique-key flood with distinct timestamps: **~150 µs/op sustained** (vs ~0.4 µs steady state) — a self-inflicted CPU-amplification vector on precisely the flood path the bucket bound was meant to defend. Replaced with `slices.SortFunc` (pdqsort, O(n log n)); policy unchanged (oldest half by last touch). Post-fix: **~1.0 µs/op**. Committed benchmark `BenchmarkRateLimitUniqueKeysDistinctTimes` + policy test `TestRateLimitEvictionDropsOldestKeys` pin the behavior.

**Observations (documented, not blockers):**

- `GET /api/v1/identity/authorize-url` and `GET /api/v1/identity/callback` are public and un-limited. They are read-only (not in the mutation matrix): `authorize-url` mints a signed state; `callback` validates the state MAC locally **before** any IdP exchange, so unauthenticated amplification requires a validly-signed state plus a valid IdP code. Acceptable residual; revisit if these endpoints ever mutate.
- Health endpoints are public and un-limited (process/db liveness only) — standard practice.

## 5. Webhook ingress security review (read-only; evidence from targeted test runs)

Architecture contract: authentication → signature validation → deduplication → rate limiting → persistence; no business logic in the HTTP handler; accepted events land in the `provider_events` ledger.

**Verified fail-closed paths** (existing tests run green under `-race`; package coverage 87.7%):

| Path | Behavior | Evidence |
|---|---|---|
| Empty HMAC secret | `ValidateSignature` rejects EVERYTHING (no unsigned ingestion) | `TestSignatureRejectsWrongSecretAndEmptySecret` ✅ |
| Tampered body | signature bound to exact bytes → reject | `TestSignatureRejectsTamperedBody` ✅ |
| Registered provider + valid legacy signature | **does NOT fall back** to legacy check — verifier is authoritative | `TestGatewayVerifierRoutingAndLegacyFallback` ✅ |
| Registered provider + nil/missing carrier headers | reject (`Unauth`-class) | `TestIngestHeadersGuardrailsBeforeAuth` ✅ |
| Provider path guardrails (empty, >64 chars, empty body, >256 KiB) | rejected **before** auth (DoS guard) | `TestIngestHeadersGuardrailsBeforeAuth` ✅ |
| Verifier credentials empty (Twilio tokens / WA secret) | reject (fail-closed, per provider) | `TestAttackMatrix/tw-empty-token-list`, `/wa-empty-secret` ✅ |
| AT allowlist unconfigured | allow + structured WARN on EVERY acceptance (honest residual — AT signs nothing) | `TestAttackMatrix/at-unconfigured-allowlist` ✅ + `TestNoSecretLeakage` |
| Secret leakage in errors/logs | fixed-string rejections only; adversarially asserted | `TestNoSecretLeakage` ✅ |
| Registry normalization | `"Twilio"` ≡ `"twilio"`; nil verifier entries dropped (can never route to a zero verifier) | `TestSetVerifiersNormalizesAndDropsNil` ✅ |
| Legacy behavior with no verifiers installed | byte-identical fallback | `TestGatewayVerifierRoutingAndLegacyFallback` ✅ |

**Dedupe:** idempotency key = `Derive("webhook", provider, body)` → `INSERT … ON CONFLICT (provider, provider_event_id) DO NOTHING` → replays return `duplicate=true`, single effect. Known contract defect **[#59]** (replay returns the stored row UUID instead of the derived event id — response-field mismatch; dedupe itself remains single-effect; not a security hole) — pre-tracked, cross-surface, referenced here.

**Per-provider threat notes:**

- **Twilio** (`verify_twilio.go`): validates `X-Twilio-Signature` (HMAC-SHA1 over URL+params) against an allowlist of auth tokens (rotation keeps two live); the external URL MUST come from configuration, never reconstructed from the request (proxy-rewrite pitfall — tested: `tw-internal-url` rejects). Residual: HMAC-SHA1 is Twilio's scheme, not our choice; 20+ byte token list required else fail-closed.
- **WhatsApp Cloud** (`verify_whatsappcloud.go`): `X-Hub-Signature-256` HMAC-SHA256 over raw body vs app secret; hex-case and truncation attacks covered (`wa-uppercase-hex`, `wa-truncated-digest`, `wa-first/last-byte-flipped`).
- **Africa's Talking** (`verify_africastalking.go`): AT signs nothing — authenticity rests on the peer-IP allowlist over a header the trusted edge MUST overwrite (`X-Forwarded-For` leftmost; header is configurable). Unconfigured allowlist = allow + WARN (documented residual, honest posture). Forged XFF from a non-compliant edge is explicitly tested (`at-forged-xff-uncompliant-edge`).
- **Simulator / platform** (legacy path): `X-Orvexa-Signature` HMAC-SHA256 hex over raw body, timing-safe `hmac.Equal`, empty-secret rejection.

## 6. `.env.example` (security-config delta)

- **Fixed in this PR:** the OIDC identity block (`ORVEXA_OIDC_ISSUER/_CLIENT_ID/_CLIENT_SECRET/_REDIRECT_URL/_SCOPES`, issue #38) was consumed by code but absent from `.env.example` — `CLIENT_SECRET` doubles as the OAuth-state HMAC key, i.e. a rotating credential; now documented with rotation guidance.
- **Filed, not fixed:** non-security drift (`ORVEXA_CLICKHOUSE_*`, `ORVEXA_MESSAGING_PROVIDER`, `ORVEXA_TELEPHONY_PROVIDER`) → [#82] (outside D1's file set).

## 7. Residual risks & standing watch items

| # | Item | Class | Disposition |
|---|---|---|---|
| R1 | GO-2026-5004 pgx bump pending | dependency (reachable) | **[#81] — the BLOCKED item** |
| R2 | GO-2026-5932 (`x/crypto/openpgp` unmaintained) | dependency (unreachable, no upstream fix) | accepted residual; re-triage on any new import |
| R3 | Webhook limiter single bucket per edge-proxy IP | design trade-off (fail-safe) | documented in v1.go; revisit when per-route limiter keys land ([#37] notes) |
| R4 | Public identity GETs un-limited | hardening backlog | documented §4; not a mutation surface |
| R5 | Replay response contract (`duplicate` event id) | correctness | [#59] pre-tracked |
| R6 | 15/46 packages without test files (incl. httpserver, workflows, outbox) | coverage | [#57] pre-tracked |
| R7 | Env-contract drift (7 non-security vars) | ops hygiene | [#82] |

## 8. Verification matrix (this sweep)

| Gate | Command | Result |
|---|---|---|
| Formatting | `gofmt -l .` | **PASS** — empty |
| Static analysis | `go vet ./...` | **PASS** — clean |
| Build | `go build ./...` | **PASS** — clean |
| Marker guard | `make lint-todos` | **PASS** — clean |
| Focused security tests | `go test -race ./internal/platform/httpx/...` | **PASS** |
| Full matrix | `go test -race ./...` | **PASS** — 31 packages ok, 0 failures (15 packages have no test files, [#57]) |
| Secret scan | `bash scripts/security-scan.sh` | **CLEAN** (exit 0; canary-validated detector, exit 1 on planted secrets) |
| Dependency audit | `govulncheck ./...` | 1 reachable finding → triaged → [#81] |

---

## VERDICT: BLOCKED — GO-2026-5004 (pgx v5.7.2 < v5.9.2, symbol-level reachable via cases.Service.ListNotes): land dependency bump [#81] + re-run govulncheck, then flip to PUBLIC-READY

Everything else the gate covers is green and evidenced: zero actionable secrets (scanner shipped, canary-validated), headers complete with tests, rate-limit matrix complete with server-side keying proven, webhook ingress fail-closed with adversarial test evidence. The single blocking item is a routine, same-major dependency bump outside this agent's file ownership.
