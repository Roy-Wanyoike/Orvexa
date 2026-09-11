#!/usr/bin/env bash
# Reproducible loadgen baseline (issue #42) — the exact procedure behind
# docs/perf/baseline-20260911-summary.md.
#
# Prerequisites: bash, curl, Go (module toolchain). No docker, no sudo.
# The script is self-contained: it boots the userland devstack PG on the
# harness-safe override port 55444 (see docs/devstack.md — ORVEXA_DEVSTACK_PORT),
# mints a FRESH tenant + 40 API keys (empty identifier space per run —
# identifier uniqueness is per tenant), starts the api binary on a test port,
# runs the stepped ramp, and shuts everything down. Ephemeral webhook HMAC
# secret is generated per run; no secret material is persisted.
#
# Usage:   docs/perf/run-baseline.sh [output.json]
# Output:  the loadgen JSON report (default: docs/perf/baseline-$(date).json);
#          stdout summary is the console transcript to paste into the run log.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

PORT=55444            # harness-safe devstack override (docs/devstack.md)
API_PORT=18080        # test port for the api process
KEYS=40               # 40 keys: 200 rps / 40 = 5 rps per key vs the 600/min per-key limiter
POOL=64               # seeded customers/interactions (webhook targets + conversation spread)
RAMP="50,100,200"
STEP=60s
WARMUP=10s
OUT="${1:-docs/perf/baseline-$(date -u +%Y%m%d-%H%M%S).json}"

DB_URL="postgres://postgres:postgres@127.0.0.1:${PORT}/orvexa?sslmode=disable"
SECRET="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"   # ephemeral, never logged

command -v go >/dev/null || { echo "go not on PATH (source the module toolchain)"; exit 1; }
pgrep -f devstack >/dev/null && { echo "orphan devstack running — kill it first (pgrep -f devstack)"; exit 1; }

echo "== build =="
go build -o .devstack/bin/loadgen ./cmd/loadgen
go build -o .devstack/bin/api ./cmd/api

echo "== devstack (port ${PORT}) =="
ORVEXA_DEVSTACK_PORT=$PORT scripts/devstack.sh start   # idempotent; applies migrations

echo "== bootstrap tenant + ${KEYS} keys (fresh tenant = empty identifier space) =="
go run .devstack/tools/bootstrap.go "$DB_URL" "$KEYS" > .devstack/loadgen-keys.txt
# .devstack/ is gitignored — raw keys never leave the machine.

echo "== api on ${API_PORT} =="
ORVEXA_DATABASE_URL="$DB_URL" \
ORVEXA_HTTP_ADDR="127.0.0.1:${API_PORT}" \
ORVEXA_WEBHOOK_HMAC_SECRET="$SECRET" \
ORVEXA_ENV=dev \
  .devstack/bin/api > .devstack/api.log 2>&1 &
API_PID=$!
trap 'kill "$API_PID" 2>/dev/null || true' EXIT

for _ in $(seq 1 20); do
  code=$(curl -s -m 2 -o /dev/null -w "%{http_code}" "http://127.0.0.1:${API_PORT}/readyz" || true)
  [ "$code" = "200" ] && break
  sleep 0.5
done
[ "$code" = "200" ] || { echo "api never became ready (see .devstack/api.log)"; exit 1; }

echo "== baseline ramp ${RAMP} rps × ${STEP} =="
.devstack/bin/loadgen \
  -target "http://127.0.0.1:${API_PORT}" \
  -api-keys-file .devstack/loadgen-keys.txt \
  -webhook-secret "$SECRET" \
  -ramp "$RAMP" -step "$STEP" -warmup "$WARMUP" \
  -pool "$POOL" -source-ips 64 \
  -output "$OUT"

echo "== machine context (paste into the run log) =="
echo "commit: $(git rev-parse --short HEAD)  nproc: $(nproc)"
grep MemTotal /proc/meminfo || true
uname -r
go version

kill "$API_PID" 2>/dev/null || true
echo "== report: ${OUT} =="
