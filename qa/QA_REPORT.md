# Orvexa — Final Production Readiness Report

**Date:** 2026-09-11 · **Tree:** `main` @ `494d354` (post-#114; security re-verification ran on `283aed5`, same content for every gate here)
**Scope:** Orvexa platform v0.10.x — interaction, intelligence, execution, control planes + production adapter push
**Method:** evidence-first. Every quantitative claim links a relative-path artifact or a merged PR. Issues #1–#115 / PRs #11–#115 cross-checked against the GitHub API (see ledger, §3).
**Author:** QA Lead (agent E3, issue #50 [O-41]). Exclusive file ownership: `qa/QA_REPORT.md` (+ evidence index).

**Verified live by the author for this report (2026-09-11, on `qa/final-report` @ `main` content):**
`gofmt -l .` empty · `go vet ./...` clean · `go build ./...` clean · `make lint-todos` clean ·
`go test -race -count=1 ./...` exit 0 with **35 packages ok / 0 FAIL / 13 `[no test files]`** ·
**40 paths** counted in `api/openapi/orvexa-v1.yaml`. Everything else cites the committed evidence pack ([INDEX](evidence/2026-09-11/INDEX.md)).

---

## 1. Executive summary & VERDICT

Orvexa is a four-plane, event-driven Go platform (customers → conversations → interactions → cases → workflows) with carrier, LLM and datastore adapters behind conformance-tested ports. Since the last QA report (wave-8, 2026-09-10) the repository absorbed: the full production adapter push (7 carrier adapters, NATS/Redis/ClickHouse/OpenSearch drivers, OIDC+capability RBAC — umbrella #10, closed with per-item evidence), an independent security sweep plus a post-integration adversarial re-verification, a 153-probe cross-tenant authorization matrix, OpenAPI⇄router contract tests, a load-generator baseline with proposed SLOs, an e2e customer-journey suite, and remediation of every defect the new harnesses surfaced (10 filed defects, all fixed and re-proven secured).

| Headline claim | Number | Evidence |
|---|---|---|
| Race matrix (default tags) | 35 pkgs ok / 0 FAIL — re-verified live 2026-09-11 | [04-unit-race.txt](evidence/2026-09-11/04-unit-race.txt) |
| Race across all gated suites (default + `integration` + `e2e`) | 37 distinct packages ok | [04](evidence/2026-09-11/04-unit-race.txt) + [06](evidence/2026-09-11/06-integration.txt) + [08](evidence/2026-09-11/08-e2e-journeys.txt) |
| Cross-tenant authz matrix | 153 probes · 0 DEFECT · 0 FAIL · 14 PASS-SECURED | [tests/authz/MATRIX.md](../tests/authz/MATRIX.md), [07-authz-matrix.txt](evidence/2026-09-11/07-authz-matrix.txt) |
| Security verdict | PUBLIC-READY — re-proven on final main | [security-posture.md §9](../docs/security/security-posture.md) |
| E2E journeys + demo | J1/J2/J3 GREEN · demo 12/12 ALL GREEN | [08](evidence/2026-09-11/08-e2e-journeys.txt), [09](evidence/2026-09-11/09-e2e-demo.txt), [journeys/](evidence/2026-09-11/journeys/) |
| Performance baseline | 20,990 requests, 0 errors / 429s / 5xx, p95 ≤ 1.74 ms | [perf summary](../docs/perf/baseline-20260911-summary.md) |
| Carrier conformance re-run | 12 packages ok, 0 fail; 8 kit runs PASS | [12-carrier-conformance.txt](evidence/2026-09-11/12-carrier-conformance.txt), [closure table](evidence/2026-09-11/umbrella-10-closure.md) |

## **VERDICT: READY WITH APPROVED RISKS — GO for pilot onboarding and public visibility.**

Every P0/P1/P2 defect is fixed and re-proven secured ([matrix](../tests/authz/MATRIX.md): 0 DEFECT; [posture §9.6](../docs/security/security-posture.md): blockers list **empty**); the security verdict is PUBLIC-READY re-proven on final main; e2e journeys and the demo loop are green with first-failure artifacts retained; performance baselines and SLO proposals are recorded with honest caveats. The word "approved" in the verdict carries exactly three material residuals, quoted here and dispositioned in §5:

1. **Multi-node wiring gap (P3, [#113](https://github.com/Roy-Wanyoike/Orvexa/issues/113)):** *"wiring: connect NATS bus + Redis presence/limiter drivers into cmd binaries (swap points shipped, binaries still InProc)"* — the [umbrella closure](evidence/2026-09-11/umbrella-10-closure.md) states: *"the cmd binaries still boot the InProc bus and in-process presence/limiter. This is stated honestly in the runbook degradation table and architecture doc — not a silent gap."* Pilots run single-node; multi-node deployments wait on #113.
2. **Temporal driver is build-tag-gated:** the [closure evidence](evidence/2026-09-11/umbrella-10-closure.md) records: *"Temporal is build-tagged and constructed per deployment by design (ADR-0009); not counted as a gap"* — the shipped default is the durable-Postgres workflow engine ([ADR-0006](../docs/adr/0006-durable-workflows.md)).
3. **Performance floors are loopback:** the [baseline caveats](../docs/perf/baseline-20260911-summary.md) state: *"These numbers are a floor, not a forecast"*, and *"A production-shaped run needs `cmd/worker` under the same load — that is unmeasured here."* No capacity-planning claims are made from them.

What would move the verdict to unqualified READY: #113 wired + a production-shaped (worker attached, real disks, network RTT) re-measurement with SLOs adopted. What would flip it to NOT READY: any authz-matrix ratchet row regressing from SECURED, a reachable govulncheck finding, or a red full-matrix gate — all are continuously checkable from the committed harnesses ([ADR-0003](../docs/adr/0003-verification-without-hosted-actions.md) method).

---

## 2. Per-area assessments

**Architecture** — PASS. The four-plane model, critical-path rule and hexagonal carrier ports are realized in code and documented ([ADR-0001](../docs/adr/0001-modular-distributed-architecture.md), [ADR-0002](../docs/adr/0002-deployable-topology-and-ownership.md), [architecture.md](../docs/architecture.md), ownership map [domains.md](../docs/domains.md)). The transactional outbox (commit-atomic events, SKIP-LOCKED leasing, at-least-once + idempotent consumers) is integration-proven against real PostgreSQL ([06-integration.txt](evidence/2026-09-11/06-integration.txt), [ADR-0004](../docs/adr/0004-event-delivery-and-outbox.md)). Degradation is designed, not accidental: typed search 503s, honest bus-fallback logging, fail-closed defaults ([runbook §Degradation](../docs/runbooks/operations.md)).

**Backend** — PASS. 35 packages race-green by default, 37 across gated suites (§1 table); the tenancy defects the matrix found (D1–D9) were fixed in [PR #105](https://github.com/Roy-Wanyoike/Orvexa/pull/105) + [PR #111](https://github.com/Roy-Wanyoike/Orvexa/pull/111) and re-proven by the untouched harness ([posture §9.3](../docs/security/security-posture.md)). Wire-contract drift (#101) was fixed in [PR #110](https://github.com/Roy-Wanyoike/Orvexa/pull/110) and the e2e suite re-aligned with it ([08 first-failure note](evidence/2026-09-11/08-e2e-journeys.txt)). Interactions/cases/workflows state machines are matrix-tested in-package; provider_ref uniqueness false-409s (#90/#104) were fixed and hold under load ([perf §#104 evidence](../docs/perf/baseline-20260911-summary.md)).

**Data** — PASS. 12 ordered forward-only migrations (11 PostgreSQL + 1 ClickHouse-engine) applied cleanly on a fresh datadir ([05-devstack.txt](evidence/2026-09-11/05-devstack.txt)); RBAC seeds pinned Go↔SQL by tests ([0012_rbac.sql](../migrations/0012_rbac.sql), [docs/rbac.md](../docs/rbac.md)); ClickHouse facts DDL + env-gated sink selection shipped and wired in `cmd/worker` ([closure item 11](evidence/2026-09-11/umbrella-10-closure.md)). Schema-level uniqueness (tenant/provider/provider_ref) behaves correctly post-fix.

**API** — PASS. OpenAPI v1 has 40 paths (counted 2026-09-11) guarded by bidirectional spec⇄router contract tests ([tests/contract](../tests/contract), [PR #86](https://github.com/Roy-Wanyoike/Orvexa/pull/86)); uniform `{data,meta}` / typed error envelope; every `MountV1` registration is probed by the authz matrix ("100% route accounting", [MATRIX notes](../tests/authz/MATRIX.md)). Runnable client surface: Postman collection + .http examples ([PR #102](https://github.com/Roy-Wanyoike/Orvexa/pull/102)).

**Security** — PASS (PUBLIC-READY, re-proven). Independent sweep ([PR #85](https://github.com/Roy-Wanyoike/Orvexa/pull/85)) + adversarial re-verification on final main ([PR #115](https://github.com/Roy-Wanyoike/Orvexa/pull/115)): secret scan CLEAN with canary validation ([10-security-scan.txt](evidence/2026-09-11/10-security-scan.txt)), govulncheck 0 reachable, 7/7 security headers on 200/404/405/429 paths, all 24 mutation routes limiter-covered, per-provider webhook attack matrices + 10-shape OIDC forgery matrix all fail-closed ([posture §9.4 A1–A13](../docs/security/security-posture.md)). Residual risk table has **no blockers** ([§9.6](../docs/security/security-posture.md)).

**AI** — PASS. The AI→side-effects boundary is deny-by-default: allowlist + schema + per-agent rate + audited refusals, AI holds no credentials ([tool gateway](../internal/tools), [architecture §AI boundary](../docs/architecture.md)); token metering clamped at the gateway. J3 proves the full chain live — AI suggestion → tool call → refusal → audit rows → usage-recorded fact ([journey transcript](evidence/2026-09-11/journeys/go-journeys-023541.txt)). LLM adapter wiring behind the gateway remains roadmap ([README roadmap](../README.md)), honestly gated.

**Observability** — PASS. Truthful health (`/readyz` component truth, 503 when degraded — [release boot smoke](evidence/2026-09-11/11-release.txt)), structured logging with redaction guarantees ([registry redaction tests](../internal/comms/registry/redaction_test.go)), request-id propagation, per-dependency health checks ([closure: umbrella acceptance ✅](evidence/2026-09-11/umbrella-10-closure.md)). The loadgen emits per-status telemetry designed to double as SLO error-budget input ([docs/slo.md](../docs/slo.md)).

**Docs** — PASS with one drift flag. The doc tree is code-synced and evidence-linked ([PR #83](https://github.com/Roy-Wanyoike/Orvexa/pull/83), [PR #91](https://github.com/Roy-Wanyoike/Orvexa/pull/91), ADRs 0001–0009, [runbook](../docs/runbooks/operations.md), [devstack](../docs/devstack.md)); `docs/rbac.md` is test-pinned against drift. Flagged, non-blocking: README still claims "32 test packages" ([README.md](../README.md) quality table) and lists fixed issues #89/#90 as open limitations — see §5 KL-2.

**Git hygiene** — PASS. Every issue→PR pairing in §3 is a squash/merge with test evidence in-body; zero TODO/FIXME markers (`make lint-todos` clean, re-verified 2026-09-11; audit census [repository-audit.md §1](../docs/audit/repository-audit.md)); no secrets committed (scan CLEAN, [§4](#4-matrices-journeys-tests-performance-security)); first-failure artifacts retained rather than rewritten — the history is auditable.

---

## 3. Issue → PR ledger (state, PR, outcome)

States cross-checked via GitHub API on 2026-09-11. "Closes" references read from PR bodies; #54/#57 closures confirmed from issue-thread comments (no closing PR existed).

### 3.1 Foundation waves ([#1]–[#9] → PRs #11–#19)

| Issue | Wave | PR | Outcome |
|---|---|---|---|
| #1 | Foundation: kernels, deployables, control-plane schema, CI | [#11](https://github.com/Roy-Wanyoike/Orvexa/pull/11) | merged, closed |
| #2 | Domain core + REST v1 + tenant auth | [#12](https://github.com/Roy-Wanyoike/Orvexa/pull/12) | merged, closed |
| #3 | Event backbone: outbox, bus, webhook gateway | [#13](https://github.com/Roy-Wanyoike/Orvexa/pull/13) | merged, closed |
| #4 | Communications: ports, simulator, lifecycle | [#14](https://github.com/Roy-Wanyoike/Orvexa/pull/14) | merged, closed |
| #5 | Routing engine + presence | [#15](https://github.com/Roy-Wanyoike/Orvexa/pull/15) | merged, closed |
| #6 | Realtime WS hub | [#16](https://github.com/Roy-Wanyoike/Orvexa/pull/16) | merged, closed |
| #7 | Intelligence: AI gateway, tool boundary | [#17](https://github.com/Roy-Wanyoike/Orvexa/pull/17) | merged, closed |
| #8 | Execution: workflows + analytics | [#18](https://github.com/Roy-Wanyoike/Orvexa/pull/18) | merged, closed |
| #9 | Release gate: OpenAPI, docs, QA report | [#19](https://github.com/Roy-Wanyoike/Orvexa/pull/19) | merged, closed |

### 3.2 Production push ([#20]–[#52] → PRs #53–#115)

| Issue | Item | PR(s) | Outcome |
|---|---|---|---|
| #20 | Repository audit & hygiene | [#58](https://github.com/Roy-Wanyoike/Orvexa/pull/58) | merged ([audit doc](../docs/audit/repository-audit.md)) |
| #21 | Provider conformance test-kit | [#60](https://github.com/Roy-Wanyoike/Orvexa/pull/60) | merged |
| #22 | Provider registry + credentials | [#61](https://github.com/Roy-Wanyoike/Orvexa/pull/61) | merged |
| #23 | Portable devstack + integration | [#62](https://github.com/Roy-Wanyoike/Orvexa/pull/62) | merged |
| #24 | Real-provider webhook verifiers | [#63](https://github.com/Roy-Wanyoike/Orvexa/pull/63) | merged |
| #25–#31 | 7 carrier adapters (Twilio voice/SMS, WhatsApp, AT voice/SMS+USSD, FreeSWITCH, Asterisk) | [#64](https://github.com/Roy-Wanyoike/Orvexa/pull/64)–[#70](https://github.com/Roy-Wanyoike/Orvexa/pull/70) | merged, each with conformance evidence |
| #32 | Provider factory wiring → cmd/api | [#71](https://github.com/Roy-Wanyoike/Orvexa/pull/71) | merged |
| #33 | NATS JetStream bus driver | [#72](https://github.com/Roy-Wanyoike/Orvexa/pull/72) | merged (library; cmd wiring → #113) |
| #34 | Temporal workflow driver (build-tagged) | [#73](https://github.com/Roy-Wanyoike/Orvexa/pull/73), [#78](https://github.com/Roy-Wanyoike/Orvexa/pull/78), [#79](https://github.com/Roy-Wanyoike/Orvexa/pull/79) | merged |
| #35 | ClickHouse facts writer | [#76](https://github.com/Roy-Wanyoike/Orvexa/pull/76), [#80](https://github.com/Roy-Wanyoike/Orvexa/pull/80) | merged + wired in cmd/worker |
| #36 | OpenSearch search plane | [#74](https://github.com/Roy-Wanyoike/Orvexa/pull/74), [#80](https://github.com/Roy-Wanyoike/Orvexa/pull/80) | merged + routes mounted |
| #37 | Redis presence + rate-limit drivers | [#75](https://github.com/Roy-Wanyoike/Orvexa/pull/75) | merged (library; cmd wiring → #113) |
| #38 | OIDC + capability RBAC | [#77](https://github.com/Roy-Wanyoike/Orvexa/pull/77) | merged |
| #39 | Independent security sweep | [#85](https://github.com/Roy-Wanyoike/Orvexa/pull/85) | merged |
| #40 | Tenancy/authz matrix | [#100](https://github.com/Roy-Wanyoike/Orvexa/pull/100) | merged |
| #41 | OpenAPI contract tests | [#86](https://github.com/Roy-Wanyoike/Orvexa/pull/86) | merged |
| #42 | Load harness + SLO doc | [#107](https://github.com/Roy-Wanyoike/Orvexa/pull/107) | merged |
| #43 | E2E journey suite + demo upgrade | [#108](https://github.com/Roy-Wanyoike/Orvexa/pull/108) | merged |
| #44 | Docs refresh + ADR-0007/0008 | [#83](https://github.com/Roy-Wanyoike/Orvexa/pull/83) | merged |
| #45 | Flagship README | [#91](https://github.com/Roy-Wanyoike/Orvexa/pull/91) | merged |
| #46 | Release engineering | [#88](https://github.com/Roy-Wanyoike/Orvexa/pull/88) | merged |
| #47 | API client collection | [#102](https://github.com/Roy-Wanyoike/Orvexa/pull/102) | merged |
| #48 | Full-matrix verification + evidence pack | [#112](https://github.com/Roy-Wanyoike/Orvexa/pull/112) | merged |
| #49 | Security re-verification (adversarial) | [#115](https://github.com/Roy-Wanyoike/Orvexa/pull/115) | merged |
| #51 | Umbrella #10 closure evidence | [#114](https://github.com/Roy-Wanyoike/Orvexa/pull/114) | merged; #10 closed |
| #52 | Wave-C dependency pre-provisioning | [#53](https://github.com/Roy-Wanyoike/Orvexa/pull/53) | merged |

### 3.3 Audit findings (#54–#57)

| Issue | Finding | Resolution | Evidence |
|---|---|---|---|
| #54 | Doc-vs-code drift (README cmd/, domains map, ADR refs) | fixed via docs refresh + README rewrite | [PR #83](https://github.com/Roy-Wanyoike/Orvexa/pull/83), [PR #91](https://github.com/Roy-Wanyoike/Orvexa/pull/91); closure per issue-thread comment |
| #55 | CI pinned go 1.23 vs go.mod 1.26 | fixed in release-gate fixes | [PR #87](https://github.com/Roy-Wanyoike/Orvexa/pull/87) |
| #56 | `ORVEXA_BOOTSTRAP_API_KEY` dead knob | knob removed; admin-API decision stays roadmap | [PR #87](https://github.com/Roy-Wanyoike/Orvexa/pull/87), [runbook bootstrap](../docs/runbooks/operations.md) |
| #57 | 16/32 packages without tests | closed on evidence: 35 default + 2 gated = 37 pkgs with race-green tests | [04](evidence/2026-09-11/04-unit-race.txt)/[06](evidence/2026-09-11/06-integration.txt)/[08](evidence/2026-09-11/08-e2e-journeys.txt); closure per issue-thread comment |

### 3.4 Defects found by harnesses → fixed & re-proven

| Defect issue | What the harness caught | Fixed in | Re-proven by |
|---|---|---|---|
| #59 | Webhook replay returned stored row UUID, not derived event id | [PR #87](https://github.com/Roy-Wanyoike/Orvexa/pull/87) | adversarial A1 replay ([posture §9.4](../docs/security/security-posture.md)) |
| #81 | pgx/chi reachable CVEs (GO-2026-5004/5777/5775) | [PR #87](https://github.com/Roy-Wanyoike/Orvexa/pull/87) | govulncheck 0 reachable ([posture verdict](../docs/security/security-posture.md)) |
| #82 | `.env.example` drift (3 vars missing) | [PR #111](https://github.com/Roy-Wanyoike/Orvexa/pull/111) | env tables code-verified ([audit §2](../docs/audit/repository-audit.md)) |
| #84 | Merge-conflict block stranded in worklog | [PR #87](https://github.com/Roy-Wanyoike/Orvexa/pull/87) | clean tree at HEAD |
| #89 | e2e-demo payload predates strict processor vocabulary | [PR #108](https://github.com/Roy-Wanyoike/Orvexa/pull/108) | demo 12/12 GREEN ([09](evidence/2026-09-11/09-e2e-demo.txt)) |
| #90, #104 | Empty `provider_ref` false-409 on 2nd create per tenant | `d621c17`, landed to main with [PR #107](https://github.com/Roy-Wanyoike/Orvexa/pull/107) | matrix DEF-1 row SECURED ([MATRIX](../tests/authz/MATRIX.md)); 5,248 creates / zero 409 under load ([perf](../docs/perf/baseline-20260911-summary.md)) |
| #92–#97 | Cross-tenant reference acceptance (D1–D6, P0) | [PR #105](https://github.com/Roy-Wanyoike/Orvexa/pull/105) | 14/14 ratchet rows SECURED ([posture §9.3](../docs/security/security-posture.md)) |
| #98 | Sub-resource LIST skips parent-tenancy (D8, 200+empty vs 404) | [PR #111](https://github.com/Roy-Wanyoike/Orvexa/pull/111) | 404-consistency re-observed ([posture §9.3](../docs/security/security-posture.md)) |
| #99 | Unmapped provider error surfaced as 500 on call hold/resume (D9) | [PR #111](https://github.com/Roy-Wanyoike/Orvexa/pull/111) | typed 409 re-observed ([posture §9.3](../docs/security/security-posture.md)) |
| #101 | Interactions-plane wire drift (Go-cased JSON, rejected documented payloads) | [PR #110](https://github.com/Roy-Wanyoike/Orvexa/pull/110) | journeys re-aligned + GREEN ([08 note](evidence/2026-09-11/08-e2e-journeys.txt)) |
| #103 | Simulator receipts ledger-only; outbound voice stuck pending | [PR #109](https://github.com/Roy-Wanyoike/Orvexa/pull/109) | provider-events consumer; e2e evidence ([posture R8](../docs/security/security-posture.md)) |
| #106 | Analytics counted lifecycle events, not distinct interactions | [PR #111](https://github.com/Roy-Wanyoike/Orvexa/pull/111) | demo step 12 distinct counts ([09](evidence/2026-09-11/09-e2e-demo.txt)) |

### 3.5 Open issues (residuals)

| Issue | State | Disposition |
|---|---|---|
| #50 (this report) | OPEN | closed by this PR |
| [#113](https://github.com/Roy-Wanyoike/Orvexa/issues/113) | OPEN | P3 approved risk AR-1 (§5): wire NATS/Redis drivers into cmd binaries — swap points shipped, integration-tested, honestly documented ([closure residual notes](evidence/2026-09-11/umbrella-10-closure.md)) |

No other open issues exist in the repository (checked 2026-09-11). Umbrella [#10] is CLOSED with per-item evidence ([closure doc](evidence/2026-09-11/umbrella-10-closure.md)).

---

## 4. Matrices, journeys, tests, performance, security

### 4.1 Roles & permissions

Contract: **IdP owns identity, Orvexa owns authorization**; closed role set `viewer ⊂ agent ⊂ supervisor ⊂ admin` over a capability catalog, pinned three ways (code comments, migration seeds, [docs/rbac.md](../docs/rbac.md)) with tests asserting the renderings equal ([internal/identity](../internal/identity), [ADR-0008](../docs/adr/0008-oidc-identity-capability-rbac.md), migration [0012_rbac.sql](../migrations/0012_rbac.sql)). RS256-only JWT verification, kid rotation + amplification floor, revocation inside the token's valid lifetime. Live evidence in this run's posture: API-key scope `{api}` passes capability middleware; foreign-tenant tool calls are policy-refused 403 ([MATRIX notes](../tests/authz/MATRIX.md)); the identity plane is absent (404) when unconfigured — fail-closed by design (matrix "Conditional" rows).

### 4.2 Tenancy & authorization matrix

Harness `tests/authz` (issue #40, defects never fixed in-package — ratchet rows auto-re-verify): **153 probes · 135 PASS · 2 PASS(degraded: typed search 503) · 14 PASS(secured) · 0 DEFECT · 2 Conditional · 0 FAIL · 0 hard-assert failures** ([result table](../tests/authz/MATRIX.md); raw gate [07](evidence/2026-09-11/07-authz-matrix.txt)). 100% route accounting of `MountV1`; cross-tenant identifiers read as 404; tenant resolved only from the authenticated credential; webhook ingress proves API keys confer nothing there. The 14 SECURED rows are the D1–D6 + #98/#99/#90 ratchets re-observed post-fix ([posture §9.3](../docs/security/security-posture.md)).

### 4.3 Customer journeys (e2e, real devstack PostgreSQL)

| Journey | Scenario | Result |
|---|---|---|
| J1 | Inbound WhatsApp → conversation/interaction → routing decision → agent assignment → wrap-up → case close | PASS 0.04s ([transcript](evidence/2026-09-11/journeys/go-journeys-023541.txt)) |
| J2 | Outbound SMS → signed receipts → delivery lifecycle → analytics facts | PASS 0.21s (same transcript) |
| J3 | AI suggest → tool call executed + refusal → audit trail + usage metering | PASS 0.21s (same transcript) |
| Demo | `scripts/e2e-demo.sh` 12-step loop incl. workflow start, conversation close, analytics summary | **12/12 ALL GREEN** ([transcript](evidence/2026-09-11/journeys/demo-loop-023541.txt)) |

**Failure injections & negative evidence kept:** journeys run1 RED (422 `request.body_malformed` — wire drift, root-caused to #101, payloads aligned, re-run GREEN: [run1](evidence/2026-09-11/08-e2e-journeys.failed-run1.txt) → [run2](evidence/2026-09-11/08-e2e-journeys.txt)); demo run1 FAIL at step 4/12 → re-run ALL GREEN ([run1](evidence/2026-09-11/09-e2e-demo.failed-run1.txt) → [run2](evidence/2026-09-11/09-e2e-demo.txt)); webhook tamper/replay/missing-signature and OIDC forgery injections all fail-closed ([posture §9.4 A1–A13](../docs/security/security-posture.md)); per-provider signature attack matrices pinned by `TestAttackMatrix` ([webhooks tests](../internal/webhooks)).

### 4.4 Automated test results

| Suite | Command | Result | Evidence |
|---|---|---|---|
| Default race matrix | `go test -race -count=1 ./...` | 35 ok / 0 FAIL — **re-verified live 2026-09-11** | [04](evidence/2026-09-11/04-unit-race.txt) |
| + integration (`-tags=integration`) | real PostgreSQL | ok, incl. `tests/integration` → 37 distinct pkgs across gates | [06](evidence/2026-09-11/06-integration.txt) |
| + e2e (`-tags=e2e`) | real devstack cycle | ok 34.5s | [08](evidence/2026-09-11/08-e2e-journeys.txt) |
| Authz matrix (`-tags=authz`) | 153-probe harness | ok 2.6s | [07](evidence/2026-09-11/07-authz-matrix.txt) |
| Carrier conformance | telephony/messaging/comms re-run | 12 ok / 0 fail; 8 kit runs PASS incl. negative controls | [12](evidence/2026-09-11/12-carrier-conformance.txt) |
| Driver planes | bus/CH/search/routing/httpx/identity/workflows (+`-tags=temporal`) | green | [13](evidence/2026-09-11/13-driver-planes.txt) |
| Contract | OpenAPI ⇄ router bidirectional | ok | [04](evidence/2026-09-11/04-unit-race.txt) (`tests/contract`) |
| Static gates | gofmt / vet (+4 tag sets) / build / lint-todos / migrations | all clean — **re-verified live 2026-09-11** | [01](evidence/2026-09-11/01-gofmt.txt)–[03](evidence/2026-09-11/03-build.txt), [05](evidence/2026-09-11/05-devstack.txt) |

### 4.5 Performance baseline (2026-09-11, loopback — a floor, not a forecast)

From [baseline-20260911-summary.md](../docs/perf/baseline-20260911-summary.md) (commit `91813de`, ramp 50→100→200 rps, 2 vCPU sandbox, simulator planes, fresh tenant; raw JSON in-repo):

| Step | Achieved | Issued | Errors / 429 / 5xx | p50 | p95 | p99 |
|---|---|---|---|---|---|---|
| 50 rps | 50.0 | 3,000 | 0 / 0 / 0 | 1.08 ms | 1.74 ms | 5.17 ms |
| 100 rps | 100.0 | 6,000 | 0 / 0 / 0 | 0.99 ms | 1.65 ms | 4.76 ms |
| 200 rps | 199.8 | 11,990 | 0 / 0 / 0 | 0.90 ms | 1.54 ms | 5.35 ms |

Total **20,990 requests, 0 errors**. Webhook ingest p95 ≈0.90 ms and interaction create p95 ≈1.78 ms at 200 rps — ~110×/140× inside the proposed S2/S3 ([docs/slo.md](../docs/slo.md)); SLOs are **proposed**, binding only after a production-shaped re-measurement. Full caveats (loopback, fsync=off, worker not attached, limiter headroom engineered, 200 rps ≠ ceiling) are part of the quoted doc and approved-risk AR-3.

### 4.6 Security scans

| Scan | Result | Evidence |
|---|---|---|
| Secret scan (12 pattern families, all tracked files) | CLEAN, zero actionable; canary-validated exit 1 on planted secrets | [10-security-scan.txt](evidence/2026-09-11/10-security-scan.txt), [posture §9.2](../docs/security/security-posture.md) |
| govulncheck | 0 reachable vulnerabilities | [posture verdict](../docs/security/security-posture.md) |
| Security headers | 7/7 present on 200/404/405/429 paths (CSP, HSTS, XCTO, XFO, Referrer-Policy, Permissions-Policy, no-store) | [posture §9.4 A9–A10](../docs/security/security-posture.md) |
| Rate-limit coverage | all 24 mutation routes (23 POST + 1 PUT) limiter-covered; webhook ingress 429 + Retry-After under burst; keys unspoofable (socket RemoteAddr) | [posture §9.5, A12–A13](../docs/security/security-posture.md) |
| Webhook/OIDC attack matrices | tamper/replay/empty-secret/hex-case/alg-none/HS256-confusion — all fail-closed | [posture §9.4 A1–A8](../docs/security/security-posture.md) |
| Verdict | **PUBLIC-READY — re-proven on final main (`283aed5`); blockers list: empty** | [posture §9](../docs/security/security-posture.md) |

---

## 5. Approved risks, known limitations, deployment & rollback

### 5.1 Approved risks (each: what / evidence / mitigation / owner)

| ID | Risk | Evidence (quoted where material) | Mitigation | Owner |
|---|---|---|---|---|
| AR-1 | NATS/Redis drivers are library swap points; cmd binaries boot InProc bus + in-process presence/limiter — multi-node presence sharing and global rate limits not available until wired | [#113](https://github.com/Roy-Wanyoike/Orvexa/issues/113): *"connect NATS bus + Redis presence/limiter drivers into cmd binaries (swap points shipped, binaries still InProc)"*; [closure](evidence/2026-09-11/umbrella-10-closure.md): *"not a silent gap"* | Single-node pilot posture; drivers are integration-tested against real NATS/Redis and swap via documented constructors; runbook degradation table states it ([operations.md](../docs/runbooks/operations.md)); NTP requirement for wired multi-node already documented | Orchestrator (P3 follow-up, tracked) |
| AR-2 | Temporal workflow driver is build-tag-gated; default is the durable-Postgres engine | [closure](evidence/2026-09-11/umbrella-10-closure.md): *"Temporal is build-tagged and constructed per deployment by design (ADR-0009); not counted as a gap"* | Default engine is race-tested and e2e-proven; Temporal adoption is a per-deployment decision recorded in [ADR-0009](../docs/adr/0009-temporal-driver.md); tag-built tests green ([13](evidence/2026-09-11/13-driver-planes.txt)) | Platform |
| AR-3 | Performance numbers are loopback floors; worker not attached; no saturation run | [perf caveats](../docs/perf/baseline-20260911-summary.md): *"These numbers are a floor, not a forecast"*; *"A production-shaped run needs `cmd/worker` under the same load — that is unmeasured here."* | No capacity claims are made; SLOs proposed not binding ([slo.md](../docs/slo.md)); saturation run is the honest follow-up before any sizing statement | Perf/QA |
| AR-4 | Dependency hygiene: `x/crypto` module-level advisories (unreachable; no fix for openpgp) | [posture §9.6 R2](../docs/security/security-posture.md): *"accepted residual; a routine x/crypto bump is hygiene, not a blocker"* | Re-triage on any new import; govulncheck in the gate routine | Security |
| AR-5 | Config-dependent production posture: AT allowlist unconfigured = allow+WARN; three public read-only GETs un-limited; per-provider verifiers opt-in | [posture §9.6 R9/R4/R10](../docs/security/security-posture.md) | Fail-closed defaults everywhere (no verifier → legacy fail-closed; no OIDC env → plane absent); documented deployment contract in runbook | Deployer (documented contract) |
| AR-6 | Tenant bootstrap is SQL until the admin/onboarding API lands | [runbook §First tenant bootstrap](../docs/runbooks/operations.md); roadmap item in [README](../README.md) | Documented SQL path (hashed keys); keys stored SHA-256 only; admin API tracked on roadmap | Product |

### 5.2 Known limitations (non-risk, factual)

- **KL-1 No hosted CI on this account** — the authoritative gate is the local full matrix recorded in-repo ([ADR-0003](../docs/adr/0003-verification-without-hosted-actions.md)); `.github/workflows/ci.yml` is armed and will enforce if Actions become available (go-pin fixed by [PR #87](https://github.com/Roy-Wanyoike/Orvexa/pull/87)).
- **KL-2 README staleness (flagged, non-blocking):** README claims "32 test packages" under race ([README quality table](../README.md)) — measured reality is 35 default / 37 across gates ([04](evidence/2026-09-11/04-unit-race.txt)); README "Honest limitations" still lists #89/#90 as open — both fixed ([§3.4](#34-defects-found-by-harnesses--fixed--re-proven)); runbook "Deployment posture" points Redis wiring at closed umbrella [#10] instead of [#113] ([operations.md](../docs/runbooks/operations.md)). One docs PR should sync all three; nothing else in the doc tree drifted (contract tests + test-pinned rbac doc guard the rest).
- **KL-3 Single-region, single-Postgres posture** — region-isolation design is recorded but intentionally unimplemented ([ADR-0007](../docs/adr/0007-multi-region-readiness-region-isolation.md), design-only by title).
- **KL-4 `e2e-demo.sh`/journeys run against simulator planes** — real-carrier production runs follow per-adapter acceptance criteria tracked from the [closure table](evidence/2026-09-11/umbrella-10-closure.md).

### 5.3 Deployment & rollback verification statements

- **Deployment:** `make release` produced all three stamped deployables (`orvexa-api`, `orvexa-worker`, `orvexa-realtime`) plus a deterministic CycloneDX 1.5 SBOM, version `v0.10.1-rc1` @ `35de2a0` ([11-release.txt](evidence/2026-09-11/11-release.txt)); boot smoke shows truthful degradation (`WARN … running without durable storage`, then `listening` on :8080). Production deployment posture (TLS at edge, XFF overwrite contract, HMAC rotation window, worker scale-out via SKIP-LOCKED, per-dependency health) is specified and code-verified in the [runbook §Deployment posture](../docs/runbooks/operations.md). Migrations are ordered, forward-only, applied idempotently by the devstack runner ([05](evidence/2026-09-11/05-devstack.txt)).
- **Rollback:** every change in §3 shipped as a single squash PR, so platform rollback = revert one PR (the [issue #50 contract](https://github.com/Roy-Wanyoike/Orvexa/issues/50): *"Revert the single squash PR"* — same protocol every wave, [git log](https://github.com/Roy-Wanyoike/Orvexa/commits/main)); migrations are forward-only by design, so schema rollback is restore-from-backup, documented with backup guidance in the [runbook §Backups & recovery](../docs/runbooks/operations.md). Devstack rollback is fully additive (delete `.devstack/` + scripts — [devstack §Rollback](../docs/devstack.md)). This PR touches only `qa/` + worklog: rollback is reverting this PR; no code, schema, or event-surface impact.

---

## 6. Final recommendation

**Pilot onboarding: GO.** Onboard single-tenant pilots in the documented posture: single-node default profile (simulator or configured carriers), TLS-terminating edge per the runbook contract, `ORVEXA_WEBHOOK_HMAC_SECRET` set, SQL bootstrap per the runbook. The loop is complete end-to-end (journeys §4.3), the security boundary is enforced by construction (§4.6), and every known gap is approved + tracked (§5.1) rather than silent.

**Public visibility: GO.** The independent posture verdict is PUBLIC-READY re-proven on final main with an empty blockers list ([posture §9](../docs/security/security-posture.md)); the secret scan is CLEAN and canary-validated; no credential material exists in the repo. The ledger (§3) is public and every claim above links evidence — trust is checkable, not asserted.

**Before multi-node or capacity-sensitive production** (the two gates between READY-WITH-RISKS and unqualified READY): land [#113] (AR-1), then re-run the loadgen production-shaped (worker attached, real disks, network RTT) and adopt the SLOs ([slo.md](../docs/slo.md), AR-3). Neither blocks pilots.

---

### Appendix — verification log for this report

| Checked | When | Command / method | Result |
|---|---|---|---|
| Formatting / vet / build / TODO gates | 2026-09-11 | `gofmt -l .` / `go vet ./...` / `go build ./...` / `make lint-todos` | all clean (exit 0) |
| Race matrix | 2026-09-11 | `go test -race -count=1 ./...` | exit 0 · 35 ok / 0 FAIL / 13 no-test-files |
| OpenAPI path count | 2026-09-11 | `rg -c '^  /' api/openapi/orvexa-v1.yaml` | 40 |
| Ledger states | 2026-09-11 | `gh issue list --state all` + `gh pr list --state all` + PR-body "Closes" + issue timelines | §3 complete; open: #50, #113 only |
| Evidence counts | 2026-09-11 | `^ok` line counts in [04](evidence/2026-09-11/04-unit-race.txt)=35, [12](evidence/2026-09-11/12-carrier-conformance.txt)=12; matrix summary row in [MATRIX.md](../tests/authz/MATRIX.md)=153/0 | matches §1/§4 claims |
| Link resolution | 2026-09-11 | scripted relative-path check over this report (see PR testing notes) | all resolve |
