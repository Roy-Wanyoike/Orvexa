# Orvexa — Repository Audit & Hygiene Sweep

**Task:** issue [#20](https://github.com/Roy-Wanyoike/Orvexa/issues/20) ([O-11]) · **Agent:** A1 (Release/QA engineering)
**Base:** main @ `b414357` (central-deps merge) · **Date:** 2026-09-10 · **Method:** read-only census over all 103 tracked files (`git grep -I` full tree, `.git` excluded), classification with file:line evidence, zero behavioral code changes.

---

## 1. Marker census — TODO / FIXME / HACK / XXX / placeholder / mock / stub

Full-tree case-insensitive scan (`git grep -nI -iE "TODO|FIXME|HACK|XXX|placeholder|mock|stub"` plus word-bounded per-token verification). **Result: 4 hits, all self-referential documentation/tooling. Zero markers in any `.go` file — zero obsolete, zero technical debt, zero required-functionality gaps, zero known bugs.**

| # | Evidence | Token | Classification | Disposition |
|---|---|---|---|---|
| M1 | `Makefile:4` — target name `lint-todos` (matches "todo" case-insensitively) | todo | Documentation (tooling target name) | None — not a marker |
| M2 | `Makefile:36` — `# Guard: TODO/FIXME markers must be intentional debt, not silent gaps.` | TODO/FIXME | Documentation (describes the guard) | None |
| M3 | `Makefile:38` — guard regex string `"TODO\|FIXME\|HACK\|XXX"` | TODO/FIXME/HACK/XXX | Documentation (the lint pattern itself) | None |
| M4 | `docs/adr/0005-provider-agnostic-communications.md:26` — "Test-only mock providers: mocks would bypass the signed-webhook entry…" | mock | Documentation (design rationale: explains why mocks are *not* used) | None |

Corroborating gates: `make lint-todos` clean; `git grep -nIiE "not implemented|unimplemented|not yet supported|coming soon|for now|temporary|workaround"` over `*.go`/`*.md` → **zero hits**.

Scope note (report-only, Makefile owned elsewhere): `lint-todos` scans `*.go` under `internal|cmd|pkg` only — docs/scripts/tests are outside the guard. Current state is clean everywhere anyway; extending the guard is optional hardening.

## 2. Runtime env-var table (code truth vs `.env.example`)

Single source of truth: `pkg/config/config.go` `Load()` (helpers `env`/`envInt` + direct `os.Getenv`); verified by repo-wide `git grep -nE "os\.Getenv|os\.LookupEnv|os\.Environ"` → config.go + `tests/integration/integration_test.go` only.

| # | Variable | Default | Consumed at | In `.env.example` before audit | Disposition |
|---|---|---|---|---|---|
| 1 | `ORVEXA_HTTP_ADDR` | `:8080` | config.go:76 | yes | — |
| 2 | `ORVEXA_REALTIME_ADDR` | `:8081` | config.go:77 | yes | — |
| 3 | `ORVEXA_DATABASE_URL` | *(empty)* | config.go:78 | yes | — |
| 4 | `ORVEXA_DB_MAX_CONNS` | `20` | config.go:79 | yes | — |
| 5 | `ORVEXA_BUS_DRIVER` | `inproc` | config.go:80 | yes | — |
| 6 | `ORVEXA_NATS_URL` | `nats://localhost:4222` | config.go:81 | yes | — |
| 7 | `ORVEXA_LOG_LEVEL` | `info` | config.go:82 | yes | — |
| 8 | `ORVEXA_ENV` | `development` | config.go:83 (`Production()` = `production`) | **NO** | **added** (this PR) |
| 9 | `ORVEXA_BOOTSTRAP_API_KEY` | *(empty, raw Getenv)* | config.go:85 — **loaded, zero consumers** (grep: only field + load) | yes — but comment claimed "shown once at first boot" | comment corrected; knob itself → [#56](https://github.com/Roy-Wanyoike/Orvexa/issues/56) |
| 10 | `ORVEXA_AI_PROVIDER` | `rules` | config.go:87 | yes | — |
| 11 | `ORVEXA_AI_LLM_BASE_URL` | *(empty)* | config.go:88 | yes | — |
| 12 | `ORVEXA_AI_LLM_API_KEY` | *(empty)* | config.go:89 | yes | — |
| 13 | `ORVEXA_AI_MAX_TOKENS` | `1024` | config.go:90 | yes | — |
| 14 | `ORVEXA_AI_TIMEOUT_SECONDS` | `30` | config.go:91 | yes | — |
| 15 | `ORVEXA_COMMS_PROVIDER` | `simulator` | config.go:93 | yes | — |
| 16 | `ORVEXA_WEBHOOK_HMAC_SECRET` | *(empty)* | config.go:94 | yes | — |
| 17 | `ORVEXA_WS_MAX_PER_PRINCIPAL` | `10` | config.go:96 | **NO** | **added** (this PR) |
| 18 | `ORVEXA_WS_MAX_TOTAL` | `10000` | config.go:97 | **NO** | **added** (this PR) |
| T1 | `ORVEXA_TEST_DATABASE_URL` | *(empty)* | tests/integration/integration_test.go:25 (build tag `integration`; skips when unset) | comment only (`.env.example:10`) | explicit commented entry **added** |
| S1 | `ORVEXA_URL` / `ORVEXA_KEY` / `ORVEXA_SECRET` | — | scripts/e2e-demo.sh:5-8 (operator-supplied shell vars; `:?=set` guards) | n/a | correctly absent — script scope, not process config; documented in the script header |

**Post-fix state: 18/18 runtime vars documented in `.env.example` (100%), plus the test-only var.** Drift item: `.env.example:21-22` previously overstated the bootstrap key's behavior (no first-boot consumption exists in code; runbook bootstrap is SQL) — comment corrected here, underlying dead knob tracked as [#56](https://github.com/Roy-Wanyoike/Orvexa/issues/56).

## 3. Ignore-files audit (report-only — `.gitignore` owned by A4 this wave)

Generated artifacts observed in the shared workspace (outside the repo): `~work/.gopath/` (GOPATH per `env.sh`), `~work/toolchain/` (Go 1.27.1 userland), scratch `tmp/`. `git status --porcelain --ignored` in the clone: clean — no artifact currently leaks into the worktree.

| Check | Result | Evidence |
|---|---|---|
| `.gopath` ignored? | **NOT-IGNORED** | `git check-ignore .gopath` fails |
| `toolchain/` ignored? | **NOT-IGNORED** | `git check-ignore toolchain` fails |
| `.devstack` ignored? | **NOT-IGNORED** | `git check-ignore .devstack` fails |
| `coverage.txt` | ignored | `.gitignore:8` |
| `*.log` (e.g. tmp/foo.log) | ignored | `.gitignore:17` |
| `.env.local` (via `.env.*`) | ignored | `.gitignore:11-13` |
| binaries (`/bin/`, `orvexa-*`) | ignored | `.gitignore:2-6` |

**Recommendation to A4 (do-not-edit honored here):** add `.gopath/`, `toolchain/`, `.devstack/` to `.gitignore`. These are workspace-convention directories (per `env.sh`); a clone created inside such a workspace would otherwise show them as untracked noise, and a future in-repo GOPATH/toolchain would risk accidental `git add`.
**`.dockerignore`: absent** — no `Dockerfile`/compose file is tracked (`git ls-files | grep -i docker` → none), so this is non-material today; bundle with containerization work adjacent to roadmap [#10](https://github.com/Roy-Wanyoike/Orvexa/issues/10).

## 4. Doc-vs-code drift inventory

| # | Claim (evidence) | Reality (evidence) | Severity | Tracked |
|---|---|---|---|---|
| D1 | README.md:34 — `cmd/` deployables: "api, worker, realtime, routing, ai-worker, analytics-worker" | `cmd/` contains only `api`, `worker`, `realtime` (`ls cmd/`); ADR-0002 marks the other three *(reserved)*; README omits the qualifier | Medium — user-facing miscount of deployables | [#54](https://github.com/Roy-Wanyoike/Orvexa/issues/54) |
| D2 | README.md:36 — pkg kernels list | omits existing, tested `pkg/config` | Low | [#54](https://github.com/Roy-Wanyoike/Orvexa/issues/54) |
| D3 | docs/domains.md — "Domain Ownership Map" (18 modules) | omits `internal/httpserver` (8 files, the REST v1 layer); tables `audit_events` (migrations/0001:45), `webhook_endpoints`/`webhook_deliveries` (migrations/0006:24,36) unattributed | Medium — the map is the dependency-rule reference | [#54](https://github.com/Roy-Wanyoike/Orvexa/issues/54) |
| D4 | docs/architecture.md:53 — AI boundary "Details: ADR-0006" | ADR-0006 is *durable workflows*; no AI/tool-gateway ADR exists (ADRs 0001–0006) | Low — wrong pointer | [#54](https://github.com/Roy-Wanyoike/Orvexa/issues/54) |
| D5 | docs/adr/0006:8 — "architecture doc §16" | architecture.md has no numbered sections (repo-wide `§16` grep → only that ref) | Low — broken cross-ref | [#54](https://github.com/Roy-Wanyoike/Orvexa/issues/54) |
| D6 | docs/adr/0002 ownership table — `telephony, messaging` own `calls, messages`; `customers` owns `consents` | no `calls`/`messages`/`consents` tables exist (CREATE TABLE census across migrations 0001–0010); shipped pattern = `interactions` | Low — accepted-ADR intent vs shipped reality; annotate | [#54](https://github.com/Roy-Wanyoike/Orvexa/issues/54) |
| D7 | `.github/workflows/ci.yml:19` — `go-version: "1.23"` | `go.mod:3` — `go 1.26.0`; dormant-but-armed CI would toolchain-download mid-run | Medium | [#55](https://github.com/Roy-Wanyoike/Orvexa/issues/55) |
| D8 | pre-audit `.env.example:21-22` — bootstrap key "shown once at first boot" | `BootstrapAPIKey` has zero consumers (config.go:41,85 only); runbook bootstrap is SQL | Medium — false operational claim | comment fixed in this PR; knob → [#56](https://github.com/Roy-Wanyoike/Orvexa/issues/56) |
| D9 | README.md:37 — migrations "reversible-where-practical" | operations.md:21 "Forward-only"; zero DOWN/REVOKE/DROP sections in migrations/ | Low — wording | [#54](https://github.com/Roy-Wanyoike/Orvexa/issues/54) |

Verified-true claims (no drift): architecture.md deployment table (3 deployables) matches `cmd/`; domains.md state machines match handler implementations; QA_REPORT "16 packages green" reproduced exactly (see §6); OpenAPI paths exist in the REST layer; `.env.example` claim "boots with zero external dependencies" matches default profile (inproc bus + simulator).

## 5. Secrets scan — tracked files (zero tolerance)

**Result: no secrets found. 0 credentials, 0 private key blocks, 0 live tokens.**

Method: `git grep -I` over the full tracked tree for — private key block headers (`BEGIN … PRIVATE KEY`), AWS (`AKIA…16`), GitHub (`ghp_…36`, `github_pat_…`), Slack (`xox[baprs]-`), OpenAI (`sk-…20+`), Google (`AIza…35`), GitLab (`glpat-`), Shopify (`shpat_`), npm (`npm_…36`), generic `password|secret|token|api_key = "<8+ char literal>"`, credential-bearing URLs (`scheme://user:pass@`), and ≥40-char high-entropy strings.

Hits reviewed and classified (all benign):

| Evidence | Content | Classification |
|---|---|---|
| internal/webhooks/gateway_test.go:10 | `const testSecret = "test-hmac-secret-key"` | Legitimate test fixture — self-described HMAC test constant, not a credential |
| .env.example:11 | `postgres://orvexa:orvexa@localhost:5432/...` | Dummy local-dev default (documented, empty-by-design secrets elsewhere) |
| entropy scan hits (cmd/api/main.go:20,23-30; api/openapi/orvexa-v1.yaml:46) | Go import paths; OpenAPI schema description text | Not secrets |

Corroboration: QA_REPORT §1 records GitGuardian-on-push active with no alerts; `.env` is untracked (`.gitignore:11-13`, `!.env.example`).

## 6. Quality gates (full matrix, main @ b414357 + audit changes)

| Gate | Command | Result |
|---|---|---|
| Formatting | `gofmt -l .` | **PASS** — empty output |
| Static analysis | `go vet ./...` | **PASS** — clean |
| Build | `go build ./...` | **PASS** — clean |
| Tests | `go test -race ./...` | **PASS** — 16 packages `ok`, 16 `[no test files]` (32 total) |
| Marker guard | `make lint-todos` | **PASS** — clean |

Coverage observation (report-only): the 16 untested packages include `internal/httpserver` (entire REST layer), `internal/workflows` (durable executor), `internal/platform/outbox` (sanctioned event write path) — tracked as [#57](https://github.com/Roy-Wanyoike/Orvexa/issues/57).

## 7. Findings summary

| # | Finding | Disposition |
|---|---|---|
| F1 | `.env.example` incomplete: 3 runtime vars undocumented (`ORVEXA_ENV`, `ORVEXA_WS_MAX_PER_PRINCIPAL`, `ORVEXA_WS_MAX_TOTAL`); test var comment-only; bootstrap-key comment overstated | **Fixed in this PR** (only file edit; `.env.example` is owned by A1) |
| F2 | Doc drift bundle: README cmd/pkg lists, domains.md map gaps (httpserver + 3 tables), architecture.md→ADR-0006 AI link, ADR-0006 §16 ref, ADR-0002 intent-vs-shipped tables, migration-reversibility wording | [#54](https://github.com/Roy-Wanyoike/Orvexa/issues/54) |
| F3 | CI pins go 1.23 vs go.mod 1.26.0 | [#55](https://github.com/Roy-Wanyoike/Orvexa/issues/55) |
| F4 | `ORVEXA_BOOTSTRAP_API_KEY` dead knob (loaded, never consumed) | [#56](https://github.com/Roy-Wanyoike/Orvexa/issues/56) |
| F5 | 16/32 packages without tests (httpserver, workflows, outbox notable) | [#57](https://github.com/Roy-Wanyoike/Orvexa/issues/57) |
| F6 | `.gitignore` gaps (`.gopath/`, `toolchain/`, `.devstack/`); `.dockerignore` absent (no Dockerfile yet) | Reported in §3 — `.gitignore` owned by A4 this wave |

**Verdict:** hygiene is strong — zero unexplained markers, zero secrets, env-var contract now fully documented. The drift inventory is documentation-level; nothing blocks the pilot-onboarding posture recorded in `qa/QA_REPORT.md`.
