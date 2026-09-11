# Loadgen baseline — 2026-09-11 — ramp 50 → 100 → 200 rps

Raw evidence: [`baseline-20260911-ramp50-100-200.json`](./baseline-20260911-ramp50-100-200.json)
(self-describing: meta carries machine context, warmup, seed and limiter posture).
Reproduce with [`run-baseline.sh`](./run-baseline.sh).

## Machine / environment context (honest, nothing tuned)

| Item | Value |
| --- | --- |
| Commit | `91813de` (perf/load-slo; includes #104 NULL provider_ref fix and tenant-isolation merge) |
| CPU | 2 vCPU (`nproc=2`, GOMAXPROCS=2) — shared CI sandbox |
| Memory | 4.0 GiB total |
| Kernel | 5.10.134-013.8.3.kangaroo.al8.x86_64 |
| Go | go1.27.1 linux/amd64 |
| PostgreSQL | 16.4.0 (devstack Zonky bundle, `fsync=off` dev datadir, loopback) |
| Topology | loadgen → loopback → api (`127.0.0.1:18080`, single process) → loopback → PG (port 55444) |
| Providers | simulator planes (no external comms), search degraded (OpenSearch absent) |
| Worker | **not running** — outbox rows accumulate for the run's duration (drained only by a worker process; none attached here) |
| Limiter posture | per-key 600/min burst 120 × 40 keys; webhook ingress 120/min per source IP × 64 loopback IPs |
| DB state | fresh devstack datadir, migrations 0001–0012 applied, **fresh tenant** per run (empty identifier space) |
| Scenario mix | `all`: webhook ingest + interaction create + customer-create + customer-list, round-robin equal weight |
| Profile | stepped ramp 50/100/200 rps × 60 s per step + 10 s unmeasured warmup; open-loop at fixed rate |

## Results (client-observed loopback latency, ms)

Aggregate per step — 0 errors, 0 non-2xx, 0 rate-limited anywhere:

| Step | Target | Achieved | Issued | OK | p50 | p95 | p99 | max |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 50 rps | 50.0 rps | 3,000 | 3,000 | 1.08 | 1.74 | 5.17 | 28.7 |
| 2 | 100 rps | 100.0 rps | 6,000 | 6,000 | 0.99 | 1.65 | 4.76 | 55.1 |
| 3 | 200 rps | 199.8 rps | 11,990 | 11,990 | 0.90 | 1.54 | 5.35 | 44.7 |

Per scenario, per step (p50 / p95 / p99):

| Scenario | 50 rps | 100 rps | 200 rps |
| --- | --- | --- | --- |
| webhook ingest | 0.75 / 1.01 / 3.27 | 0.68 / 0.90 / 3.99 | 0.61 / 0.90 / 5.05 |
| interaction create | 1.47 / 2.07 / 7.34 | 1.39 / 1.88 / 5.74 | 1.21 / 1.78 / 6.71 |
| customer create | 1.27 / 1.75 / 7.55 | 1.16 / 1.58 / 3.91 | 1.05 / 1.56 / 6.05 |
| customer list | 0.81 / 1.20 / 2.36 | 0.80 / 1.33 / 5.43 | 0.72 / 1.09 / 4.48 |

Status distribution at every step: `200` (list) / `201` (creates) / `202` (webhook ingest accepted), no other codes.

## Read of the data

- **No saturation signal anywhere in the measured envelope.** Throughput tracks
  target at every step; p95 *decreased* slightly as the ramp climbed (2 vCPU
  warm caches, pool warm-up); p99 stays under 8 ms — roughly 12–18× inside the
  proposed 100 ms webhook-ingest p95 SLO even at p99.
- **Webhook ingest is the cheapest scenario measured** (p95 ≈ 0.9 ms at 200 rps):
  HMAC verify + ledger insert + same-state no-op processor. Interaction create
  is the heaviest (p95 ≈ 1.8 ms): customer FK + conversation find-or-open +
  interaction row + outbox write.
- **No bottleneck issues filed from this run — the data exposes none.** Issue
  #42 asks for bottleneck issues with numbers, not guesses; at ≤200 rps
  aggregate on loopback the system shows no failure mode (0 errors, 0 429s,
  0 5xx). Filing speculative bottlenecks would be fabrication. The honest
  follow-up is a saturation run (see caveats below).
- **#104 fix evidence under load:** the measured mix includes 5,248 API
  interaction creates (64 seeded + 5,184 measured, all `201`, zero `409`s) in
  one tenant — before d621c17 the *second* such create per tenant always 409'd
  on the `(tenant_id, provider, provider_ref)` unique index (empty-string
  provider_ref). The fix holds across the full envelope.

## Caveats (read before quoting these numbers)

1. **Local loopback, no network.** Client→api and api→PG are both loopback;
   real deployments add network RTT and provider-side variance. These numbers
   are a floor, not a forecast.
2. **2 vCPU shared sandbox, fsync=off dev datadir.** PG durability settings
   (fsync on, real disks) will shift write-path numbers; nobody should quote
   ms-scale absolutes from this environment for capacity planning without a
   like-for-like re-run.
3. **Worker not attached.** Interaction creates and webhook ingests append to
   `outbox_events`; nothing drained them during the run. A production-shaped
   run needs `cmd/worker` under the same load — that is unmeasured here.
4. **Limiter headroom was engineered into the run** (40 keys at ≤5 rps/key
   sustained vs 600/min cap; 64 source IPs at <1 rps/IP vs 120/min cap), so the
   **rate limiters are never the measured path**. A single-key or single-IP
   deployment would hit the shipped limiter long before these rates — that is
   the intended protective behavior, not a defect.
5. **200 rps is the end of the measured envelope, not the system's ceiling.**
   The ramp was designed per issue #42's 50/100/200 scope; saturation, and any
   bottleneck ranking, is future work beyond the issue's scope.
6. **Seeding (64 customers/interactions) is outside the measured window**; the
   10 s warmup is likewise unmeasured and excluded from every figure above.
