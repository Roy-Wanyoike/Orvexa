# Orvexa SLOs — proposed targets + measured baselines

Issue [#42](https://github.com/Roy-Wanyoike/Orvexa/issues/42) — [O-33].
Baselines measured with the self-contained load generator
([`cmd/loadgen`](../cmd/loadgen)) against the local devstack; raw evidence and
the reproducible procedure live in [`perf/`](./perf/). **SLOs below are
proposed** — they bind once adopted after a production-shaped re-measurement
(see caveats); until then they are the review bar for perf-relevant changes.

## Scope of the measured surface

The hot path: **signed webhook ingest** (WhatsApp-shaped ProviderEvent → HMAC
verify → ledger insert → processor) and **interaction lifecycle writes**
(create with conversation find-or-open + outbox append), plus customer
create/list as supporting read/write traffic. Every non-2xx counts as an SLO
error; 429s are additionally broken out because they are limiter policy, not
capacity.

## Proposed SLOs

| # | Objective | Target | Window |
| --- | --- | --- | --- |
| S1 | Webhook ingest availability (2xx/ingest received) | ≥ 99.9% | 30-day rolling |
| S2 | Webhook ingest latency | p95 < 100 ms | 30-day rolling |
| S3 | Interaction create latency | p95 < 250 ms | 30-day rolling |
| S4 | Interaction create availability (2xx) | ≥ 99.5% | 30-day rolling |
| S5 | Authenticated API (all routes) unauthenticated-rejection integrity | 100% (fail-closed; measured as 0 unsigned ingest accepted) | continuous |

Rationale: the 2026-09-11 local baseline puts webhook ingest p95 at **≈0.9 ms**
(200 rps aggregate, loopback) — 100× headroom over S2 — and interaction create
p95 at **≈1.8 ms** over S3's bar. The targets are deliberately loose relative
to loopback numbers so they remain meaningful once network RTT, real disks
(fsync on), and a running worker are in the path; they are still tight enough
to catch real regressions (an order-of-magnitude p95 jump trips them).

## Error budget policy

- Budget = 100% − SLO (S1: 0.1% ≈ 43 min/month of webhook-ingest errors; S4:
  0.5% ≈ 3.6 h/month of interaction-create errors).
- **Budget burn measured from the same telemetry the loadgen produces**
  (per-status counts; every non-2xx is an error, 429s tracked separately —
  sustained 429s indicate limiter misconfiguration, and are triaged as policy,
  not capacity).
- < 50% budget remaining in-window → freeze risky deploys to the ingest/write
  path, prioritize latency/error work.
- < 25% remaining → change freeze on the measured path except fixes; incident
  review publishes a budget-reset plan.
- Budget exhausted → revert the most recent ingest/write-path change and file
  a regression issue with the measured before/after.

## Measured baseline (2026-09-11, devstack, loopback)

Full context, raw JSON and caveats:
[`perf/baseline-20260911-summary.md`](./perf/baseline-20260911-summary.md).
Machine: 2 vCPU / 4 GiB shared sandbox, PG 16.4 devstack (fsync=off), simulator
provider planes, no worker attached, 40 API keys + 64 webhook source IPs so the
shipped limiters never gate the measured path.

| Step (target → achieved rps) | webhook p50/p95/p99 (ms) | interaction p50/p95/p99 (ms) | errors |
| --- | --- | --- | --- |
| 50 → 50.0 | 0.75 / 1.01 / 3.27 | 1.47 / 2.07 / 7.34 | 0 / 3,000 |
| 100 → 100.0 | 0.68 / 0.90 / 3.99 | 1.39 / 1.88 / 5.74 | 0 / 6,000 |
| 200 → 199.8 | 0.61 / 0.90 / 5.05 | 1.21 / 1.78 / 6.71 | 0 / 11,990 |

(customer-create / customer-list and full machine context in the summary doc.)

Against the proposed SLOs at every measured step: **S2 met with ≈110× headroom
(p95 0.90 ms vs 100 ms), S3 met with ≈140× headroom (p95 1.78 ms vs 250 ms),
S1/S4 error-rate term met (0 non-2xx across 20,990 measured requests)**. S5
holds structurally (fail-closed gateway, constant-time MAC, unsigned ingest is
rejected 401 and never persisted — verified by the webhook suite, not by this
load profile which sends correctly-signed traffic).

## Honest caveats

1. **Local loopback only** — no network RTT, no provider-side variance. These
   are floors, not production forecasts.
2. **Dev-grade durability** — PG devstack runs `fsync=off` on a shared 2-vCPU
   sandbox; a production-like re-measurement is required before these SLOs
   bind (the error-budget policy presumes real telemetry, not a loadgen).
3. **No worker attached** — outbox writes accumulated undrained during the
   run; end-to-end delivery latency behind the outbox is explicitly NOT
   measured here.
4. **Limiter headroom engineered in** (40 keys, 64 source IPs) — a deployment
   running one key/IP hits the shipped 600/min and 120/min limiters far below
   200 rps; that is protective behavior, not a defect. Sustained single-key
   capacity is unmeasured by design.
5. **200 rps is the measured envelope's end, not the system's ceiling** —
   saturation testing (and hence any bottleneck ranking) is out of #42's
   scope; the run exposed zero errors/429s/5xx, so no bottleneck issues were
   filed from it.
6. **Simulator planes** — real telephony/messaging providers will change the
   ingest mix and failure modes (timeouts, retries, replay).
