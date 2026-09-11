#!/usr/bin/env bash
# Orvexa end-to-end demo + customer-journey runner (issues #43 + #89).
#
# Single command against a fresh devstack — boots everything it needs, runs the
# full interaction loop AND the Go customer-journey suite, asserts every step,
# prints a summary, and leaves text transcripts under
# qa/evidence/<UTC-date>/journeys/ (committed as journey evidence; .txt so
# the repo's *.log gitignore doesn't hide them).
#
# Usage:
#   bash scripts/e2e-demo.sh                  # MANAGED: devstack(55445) + api +
#                                            # worker + fresh tenant, loop + journeys
#   bash scripts/e2e-demo.sh --loop-only      # managed, skip the Go journey suite
#   bash scripts/e2e-demo.sh --journeys-only  # managed, skip the demo loop
#
#   ORVEXA_URL=http://localhost:8080 \
#   ORVEXA_KEY=orvx_… \
#   ORVEXA_SECRET=… \
#     bash scripts/e2e-demo.sh                 # EXTERNAL: loop against a stack you own
#                                              # (make devstack-up; go run ./cmd/api;
#                                              #  go run ./cmd/worker). Journeys run too
#                                              # if ORVEXA_E2E_DATABASE_URL is exported.
#
# Knobs (all optional): ORVEXA_DEVSTACK_PORT (55445), ORVEXA_E2E_API_ADDR
# (127.0.0.1:55446), ORVEXA_WEBHOOK_HMAC_SECRET / ORVEXA_SECRET (e2e-hmac-secret),
# ORVEXA_E2E_KEEP=1 (keep the booted stack alive after the run).
#
# The loop (fresh seeded tenant; one outbound leg per tenant — see #90):
#   health → customer identities → resolve → inbound WhatsApp interaction
#   → signed provider lifecycle webhook → REPLAY deduped → TAMPER fail-closed
#   → routing decision → assignment → outbound call via simulator carrier
#   (ringing/connected receipts) → hangup → complete → case lifecycle + link
#   → callback workflow → wrap-up → conversation close → analytics facts.
#
# Contract notes (drift routed around here, filed upstream — do not "fix" the
# workarounds until #101 lands):
#   - interactions.Rec / CreateInput carry no JSON tags (#101): responses are
#     Go-cased (data.ID, data.Status) and POST /api/v1/interactions only binds
#     Go-name keys ("customerid"), because the handler decodes with
#     DisallowUnknownFields — the spec's snake_case body is rejected 422.
#   - The comms processor vocabulary (#89 root cause) has no "inbound.whatsapp"
#     topic and requires interaction_id + tenant_id: the loop creates the
#     inbound interaction first, then delivers the SIGNED message.delivered
#     event for it — exactly what a real whatsapp_cloud adapter translates.

set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

MODE_LOOP=1 MODE_JOURNEYS=1
for arg in "$@"; do
        case "$arg" in
        --loop-only) MODE_JOURNEYS=0 ;;
        --journeys-only) MODE_LOOP=0 ;;
        -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
        *) echo "unknown flag: $arg (see --help)" >&2; exit 2 ;;
        esac
done

EXTERNAL=0
if [[ -n "${ORVEXA_URL:-}" && -n "${ORVEXA_KEY:-}" && -n "${ORVEXA_SECRET:-}" ]]; then
        EXTERNAL=1
fi

if [[ $MODE_LOOP -eq 1 && $EXTERNAL -eq 0 ]]; then
        MANAGED=1
        command -v go >/dev/null || { echo "MANAGED mode needs the go toolchain (or set ORVEXA_URL/ORVEXA_KEY/ORVEXA_SECRET)" >&2; exit 2; }
else
        MANAGED=0
fi
if [[ $MODE_JOURNEYS -eq 1 && $MANAGED -eq 0 && -z "${ORVEXA_E2E_DATABASE_URL:-}" ]]; then
        MODE_JOURNEYS=0 # external loop without DB access: journeys need seeding queries
fi

command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 2; }
command -v curl >/dev/null || { echo "curl is required" >&2; exit 2; }

UTC_DATE="$(date -u +%F)"
EVID_DIR="$REPO/qa/evidence/$UTC_DATE/journeys"
LOOP_T="$EVID_DIR/demo-loop-$(date -u +%H%M%S).txt"
JOURNEY_T="$EVID_DIR/go-journeys-$(date -u +%H%M%S).txt"
SCORE="$(mktemp)"
WORKDIR="$(mktemp -d)"
trap 'rm -f "$SCORE"' EXIT

mkdir -p "$EVID_DIR"

# ---- output helpers: stdout + evidence transcript ---------------------------
log() { printf '%s\n' "$*" | tee -a "$LOOP_T"; }
say() { log "" ; log "── $* ─────────────────────────────────────────"; }
ok() { log "  ✓ PASS  $*"; echo "PASS|$*" >>"$SCORE"; }
die() {
        log "  ✗ FAIL  $*"
        echo "FAIL|$*" >>"$SCORE"
        log ""
        log "══ SUMMARY — FAILURES ABOVE (transcript: $LOOP_T) ══"
        exit 1
}

# ---- managed stack lifecycle ------------------------------------------------
API_PID="" WORKER_PID="" WE_STARTED_DEVSTACK=0

cleanup() {
        [[ -n "$API_PID" ]] && kill "$API_PID" 2>/dev/null || true
        [[ -n "$WORKER_PID" ]] && kill "$WORKER_PID" 2>/dev/null || true
        if [[ $WE_STARTED_DEVSTACK -eq 1 && "${ORVEXA_E2E_KEEP:-0}" != "1" ]]; then
                ORVEXA_DEVSTACK_PORT="${ORVEXA_DEVSTACK_PORT:-55445}" bash scripts/devstack.sh stop >/dev/null 2>&1 || true
        fi
        rm -rf "$WORKDIR"
}

# port_busy <port> — 0 when a TCP listener already accepts on 127.0.0.1:<port>
port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null && { exec 3>&- 3<&-; return 0; } || return 1; }

managed_boot() {
        export ORVEXA_DEVSTACK_PORT="${ORVEXA_DEVSTACK_PORT:-55445}"
        SECRET="${ORVEXA_WEBHOOK_HMAC_SECRET:-${ORVEXA_SECRET:-e2e-hmac-secret}}"

        # API address: honor ORVEXA_E2E_API_ADDR; if that port is taken (another
        # devstack, a developer's own api), fall back to a free loopback port —
        # the same collision-avoidance contract the Go harness implements.
        local addr="${ORVEXA_E2E_API_ADDR:-127.0.0.1:55446}"
        local port="${addr##*:}"
        if ! [[ "$addr" == *:* ]] || port_busy "$port"; then
                for port in $(seq 55446 55490); do
                        port_busy "$port" || break
                done
                port_busy "$port" && die "no free api port in 55446-55490"
                addr="127.0.0.1:$port"
        fi
        ORVEXA_URL="http://$addr"

        if ! bash scripts/devstack.sh status 2>/dev/null | grep -q RUNNING; then
                WE_STARTED_DEVSTACK=1
        fi
        bash scripts/devstack.sh start >/dev/null || die "devstack start failed"
        DB_URL="$(bash scripts/devstack.sh url)"
        [[ -n "$DB_URL" ]] || die "devstack url printed nothing"

        log "building api + worker from the current tree…"
        go build -o "$WORKDIR/orvexa-api" ./cmd/api || die "go build ./cmd/api"
        go build -o "$WORKDIR/orvexa-worker" ./cmd/worker || die "go build ./cmd/worker"

        ORVEXA_DATABASE_URL="$DB_URL" ORVEXA_WEBHOOK_HMAC_SECRET="$SECRET" \
                ORVEXA_HTTP_ADDR="${ORVEXA_URL#http://}" ORVEXA_LOG_LEVEL=warn \
                "$WORKDIR/orvexa-api" >"$WORKDIR/api.log" 2>&1 &
        API_PID=$!
        ORVEXA_DATABASE_URL="$DB_URL" ORVEXA_WEBHOOK_HMAC_SECRET="$SECRET" \
                ORVEXA_LOG_LEVEL=warn "$WORKDIR/orvexa-worker" >"$WORKDIR/worker.log" 2>&1 &
        WORKER_PID=$!

        local deadline=$((SECONDS + 60))
        until curl -sf "$ORVEXA_URL/readyz" 2>/dev/null | jq -e '.data.status=="ready"' >/dev/null; do
                (( SECONDS >= deadline )) && { tail -5 "$WORKDIR/api.log" >&2 || true; die "api not ready within 60s"; }
                sleep 0.5
        done

        # Fresh demo tenant per run: org, tenant, api key, allowlisted AI agent.
        local seeded
        seeded="$(go run -tags=e2e ./tests/e2e/seed "$DB_URL" demo)" || die "tenant seeding failed"
        ORVEXA_KEY="$(echo "$seeded" | awk '{print $2}')"
        log "stack ready: $ORVEXA_URL (devstack :${ORVEXA_DEVSTACK_PORT}, pid api=$API_PID worker=$WORKER_PID)"
        log "demo tenant seeded (key ${ORVEXA_KEY:0:12}…)"
}

# ---- HTTP helpers -----------------------------------------------------------
SIG_OF() { printf '%s' "$1" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}'; }
# poll <timeout_s> <url-path> <jq-assertion> — GET until the assertion holds
poll() {
        local deadline=$((SECONDS + $1)) path=$2 expr=$3
        while :; do
                curl -sf "${H[@]}" "$ORVEXA_URL$path" 2>/dev/null | jq -e "$expr" >/dev/null && return 0
                (( SECONDS >= deadline )) && return 1
                sleep 0.3
        done
}

# ---- the demo loop ----------------------------------------------------------
demo_loop() {
        local CUST CONV INB CALL AGENT CASE_ID WF EVENT_BODY SIG EVENT_ID

        say "1/12 health"
        curl -sf "$ORVEXA_URL/readyz" | jq -e '.data.status=="ready"' >/dev/null \
                || die "1/12 health: /readyz not ready"
        ok "1/12 stack ready"

        say "2/12 customer with two channel identities"
        CUST="$(curl -sf "${H[@]}" -d '{"display_name":"Jane Wanjiku","identifiers":[{"type":"phone","value":"+254 712 345 678"},{"type":"whatsapp","value":"+254712345678","is_primary":true}]}' \
                "$ORVEXA_URL/api/v1/customers" | jq -er '.data.id | select(length>0)')" \
                || die "2/12 customer create failed"
        ok "2/12 customer $CUST (phone + whatsapp identities)"

        say "3/12 identifier resolution — same customer via phone"
        local got
        got="$(curl -sf "${H[@]}" -d '{"type":"phone","value":"254712345678"}' "$ORVEXA_URL/api/v1/customers/resolve" | jq -er '.data.id')" \
                || die "3/12 resolve failed"
        [[ "$got" == "$CUST" ]] || die "3/12 resolve returned $got, want $CUST (E.164 normalization)"
        ok "3/12 resolve OK (254712345678 → +254712345678 → same customer)"

        say "4/12 inbound WhatsApp interaction (conversation auto-opened)"
        # "customerid" binds case-insensitively to CustomerID; the documented
        # snake_case body is rejected 422 until #101 lands (#89 workaround).
        INB="$(curl -sf "${H[@]}" -d "{\"customerid\":\"$CUST\",\"channel\":\"whatsapp\",\"direction\":\"inbound\",\"source\":\"+254712345678\",\"destination\":\"+254700000000\"}" \
                "$ORVEXA_URL/api/v1/interactions" | jq -er '.data.ID | select(length>0)')" \
                || die "4/12 interaction create failed"
        CONV="$(curl -sf "${H[@]}" "$ORVEXA_URL/api/v1/interactions/$INB" | jq -er '.data.ConversationID | select(length>0)')" \
                || die "4/12 interaction missing ConversationID"
        poll 15 "/api/v1/interactions/$INB" '.data.Status=="pending"' \
                || die "4/12 inbound interaction not pending"
        ok "4/12 inbound whatsapp interaction $INB (conversation $CONV, pending)"

        say "5/12 signed provider webhook → replay deduped → tamper fail-closed"
        # The processor requires interaction_id + tenant_id (#89): read both from
        # the interaction record — exactly what a real adapter translates.
        EVENT_BODY="$(curl -sf "${H[@]}" "$ORVEXA_URL/api/v1/interactions/$INB" | jq -c \
                --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
                '{event:"message.delivered",interaction_id:.data.ID,tenant_id:.data.TenantID,timestamp:$now,detail:"demo inbound delivery"}')"
        SIG="$(SIG_OF "$EVENT_BODY")"
        local first
        first="$(curl -sf -X POST -H "Content-Type: application/json" -H "X-Orvexa-Signature: $SIG" \
                -d "$EVENT_BODY" "$ORVEXA_URL/api/v1/webhooks/whatsapp_cloud")" || die "5a webhook rejected"
        echo "$first" | jq -e '.data.accepted==true and .data.duplicate==false and .data.processed==true and (.data.event_id|length>0)' >/dev/null \
                || die "5a first delivery must be accepted+processed: $first"
        EVENT_ID="$(echo "$first" | jq -r .data.event_id)"
        ok "5a signed webhook accepted (202, processed): event_id=$EVENT_ID"

        local replay
        replay="$(curl -s -X POST -H "Content-Type: application/json" -H "X-Orvexa-Signature: $SIG" \
                -d "$EVENT_BODY" "$ORVEXA_URL/api/v1/webhooks/whatsapp_cloud")"
        echo "$replay" | jq -e --arg id "$EVENT_ID" '.data.duplicate==true and .data.event_id==$id' >/dev/null \
                || die "5b replay must dedupe to the original event id: $replay"
        ok "5b FAILURE INJECTION replay: duplicate=true, same event_id (single effect)"

        local tampered
        tampered="$(curl -s -X POST -H "Content-Type: application/json" -H "X-Orvexa-Signature: deadbeef" \
                -d "$EVENT_BODY" "$ORVEXA_URL/api/v1/webhooks/whatsapp_cloud")"
        echo "$tampered" | jq -e '.error.code=="webhook.invalid_signature"' >/dev/null \
                || die "5c tampered signature must be 401 webhook.invalid_signature: $tampered"
        curl -s -o /dev/null -w '%{http_code}' -X POST -H "Content-Type: application/json" -H "X-Orvexa-Signature: deadbeef" \
                -d "$EVENT_BODY" "$ORVEXA_URL/api/v1/webhooks/whatsapp_cloud" | grep -q 401 \
                || die "5c tampered signature must fail closed with HTTP 401"
        ok "5c FAILURE INJECTION tamper: 401 webhook.invalid_signature (fail-closed ingress)"

        poll 15 "/api/v1/interactions/$INB" '.data.Status=="active"' \
                || die "5d provider event must drive pending→active"
        ok "5d interaction active after the signed provider event"

        say "6/12 agent presence"
        AGENT="$(curl -sf "${H[@]}" -d '{"external_identity":"amara@acme.test","display_name":"Amara","language":"sw","skills":[{"skill":"billing","level":4}]}' \
                "$ORVEXA_URL/api/v1/agents" | jq -er '.data.id')" || die "6a agent create failed"
        curl -sf "${H[@]}" -X PUT -d '{"status":"available"}' "$ORVEXA_URL/api/v1/agents/$AGENT/presence" \
                | jq -e '.data.status=="available"' >/dev/null || die "6b presence not available"
        ok "6/12 agent $AGENT available (skills: billing, language: sw)"

        say "7/12 routing decision → assignment"
        curl -sf "${H[@]}" -d '{"required_skills":["billing"],"language":"sw","priority":5}' \
                "$ORVEXA_URL/api/v1/routing/interactions/$INB" \
                | jq -e --arg a "$AGENT" '.data.outcome=="assigned_agent" and .data.assigned_agent_id==$a' >/dev/null \
                || die "7a routing must assign the available billing agent"
        curl -sf "${H[@]}" "$ORVEXA_URL/api/v1/routing/decisions?interaction_id=$INB" \
                | jq -e '.data|length>=1' >/dev/null || die "7b decision record not queryable"
        curl -sf "${H[@]}" "$ORVEXA_URL/api/v1/interactions/$INB" \
                | jq -e --arg a "$AGENT" '.data.AssignedAgentID==$a' >/dev/null \
                || die "7c interaction must carry AssignedAgentID after routing"
        ok "7/12 assigned_agent decision recorded + applied to the interaction"

        say "8/12 outbound call via the simulator carrier (one leg per tenant — #90)"
        CALL="$(curl -sf "${H[@]}" -d "{\"customer_id\":\"$CUST\",\"to\":\"+254712345678\"}" \
                "$ORVEXA_URL/api/v1/calls" | jq -er '.data.ID | select(length>0)')" \
                || die "8a call place failed (data.ID — Go-cased response, #101)"
        # The simulator's own ringing/connected receipts are ledger-only today
        # (#103): internal deliveries persist to provider_events with no consumer,
        # so pending→active needs the carrier-callback path — the public,
        # signature-validated webhook gateway, exactly what a real carrier hits.
        local ev
        for ev in call.ringing call.connected; do
                local cb cb_sig
                cb="$(curl -sf "${H[@]}" "$ORVEXA_URL/api/v1/interactions/$CALL" | jq -c \
                        --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg e "$ev" \
                        '{event:$e,interaction_id:.data.ID,tenant_id:.data.TenantID,timestamp:$now,detail:"demo carrier callback"}')"
                cb_sig="$(SIG_OF "$cb")"
                curl -sf -X POST -H "Content-Type: application/json" -H "X-Orvexa-Signature: $cb_sig" \
                        -d "$cb" "$ORVEXA_URL/api/v1/webhooks/simulator" | jq -e '.data.processed==true' >/dev/null \
                        || die "8b carrier callback $ev rejected"
        done
        poll 15 "/api/v1/interactions/$CALL" '.data.Status=="active"' \
                || die "8c call must be active after the ringing/connected callbacks"
        ok "8a outbound call $CALL placed; carrier callbacks (call.ringing → call.connected) active"

        sleep 0.3 # carrier-side pacing, as between real callbacks
        curl -sf "${H[@]}" -d '{"action":"hangup"}' "$ORVEXA_URL/api/v1/calls/$CALL/actions" \
                | jq -e '.data.Status|length>0' >/dev/null || die "8d hangup failed"
        curl -sf "${H[@]}" -d '{"action":"complete"}' "$ORVEXA_URL/api/v1/calls/$CALL/actions" \
                | jq -e '.data.Status=="completed"' >/dev/null || die "8e complete must end at completed"
        ok "8b hangup → complete: interaction completed"

        say "9/12 case lifecycle + link"
        CASE_ID="$(curl -sf "${H[@]}" -d "{\"customer_id\":\"$CUST\",\"subject\":\"Billing dispute\",\"priority\":\"high\"}" \
                "$ORVEXA_URL/api/v1/cases" | jq -er '.data.id | select(length>0)')" || die "9a case open failed"
        curl -sf -X POST "${H[@]}" "$ORVEXA_URL/api/v1/cases/$CASE_ID/interactions/$CALL/link" -o /dev/null \
                || die "9b case link failed"
        for step in in_progress resolved closed; do
                curl -sf "${H[@]}" -d "{\"to\":\"$step\",\"reason\":\"demo loop\"}" \
                        "$ORVEXA_URL/api/v1/cases/$CASE_ID/transition" | jq -e --arg s "$step" '.data.status==$s' >/dev/null \
                        || die "9c case transition to $step failed"
        done
        ok "9/12 case linked + open → in_progress → resolved → closed"

        say "10/12 callback workflow"
        WF="$(curl -sf "${H[@]}" -d "{\"customer_id\":\"$CUST\",\"phone\":\"+254712345678\",\"notes\":\"dispute follow-up\"}" \
                "$ORVEXA_URL/api/v1/workflows/callbacks" | jq -er '.data.id | select(length>0)')" || die "10 callback workflow failed"
        ok "10/12 callback workflow $WF started"

        say "11/12 wrap-up + conversation close"
        curl -sf "${H[@]}" -d '{"to":"wrapup"}' "$ORVEXA_URL/api/v1/interactions/$INB/transition" \
                | jq -e '.data.Status=="wrapup"' >/dev/null || die "11a wrapup transition failed"
        curl -sf "${H[@]}" -d '{"to":"completed","end_reason":"resolved"}' "$ORVEXA_URL/api/v1/interactions/$INB/transition" \
                | jq -e '.data.Status=="completed"' >/dev/null || die "11b completed transition failed"
        curl -sf "${H[@]}" -d '{"reason":"resolved"}' "$ORVEXA_URL/api/v1/conversations/$CONV/close" \
                | jq -e '.data.status=="closed"' >/dev/null || die "11c conversation close failed"
        ok "11/12 inbound interaction wrap-up → completed; conversation closed (continuous context resolved)"

        say "12/12 analytics facts"
        poll 30 "/api/v1/analytics/summary" \
                '(.data.total_interactions>=2) and (.data.interactions_by_channel.voice>=1) and (.data.interactions_by_channel.whatsapp>=1) and (.data.window.to|length>0)' \
                || die "12 analytics facts must count the whatsapp + voice interactions (worker pipeline)"
        local summary
        summary="$(curl -sf "${H[@]}" "$ORVEXA_URL/api/v1/analytics/summary" | jq -c '.data | {total_interactions, interactions_by_channel}')"
        ok "12/12 analytics summary: $summary"

        say "DONE — demo loop green (12/12)"
}

# ---- the Go customer-journey suite (J1/J2/J3, issue #43) --------------------
run_journeys() {
        log ""
        log "══ customer-journey suite (go test -race -tags=e2e ./tests/e2e/) ══"
        local rc=0
        if [[ $MANAGED -eq 1 ]]; then
                ORVEXA_WEBHOOK_HMAC_SECRET="${SECRET:-${ORVEXA_SECRET:-e2e-hmac-secret}}" \
                        go test -race -tags=e2e -count=1 -v ./tests/e2e/ 2>&1 | tee -a "$JOURNEY_T" || rc=1
        else
                ORVEXA_E2E_BASE_URL="$ORVEXA_URL" ORVEXA_E2E_DATABASE_URL="${ORVEXA_E2E_DATABASE_URL:?journeys need the stack DB}" \
                        ORVEXA_WEBHOOK_HMAC_SECRET="$SECRET" \
                        go test -race -tags=e2e -count=1 -v ./tests/e2e/ 2>&1 | tee -a "$JOURNEY_T" || rc=1
        fi
        log ""
        if [[ $rc -eq 0 ]]; then
                ok "journeys J1+J2+J3 GREEN (transcript: $JOURNEY_T)"
        else
                log "  ✗ FAIL  journey suite failed (transcript: $JOURNEY_T)"
                echo "FAIL|journey suite J1+J2+J3 (see $JOURNEY_T)" >>"$SCORE"
        fi
        return "$rc"
}

# ---- summary ----------------------------------------------------------------
summary() {
        log ""
        log "════════ E2E SUMMARY ($(date -u +%FT%TZ)) ════════"
        while IFS='|' read -r verdict line; do
                if [[ "$verdict" == "PASS" ]]; then
                        log "  ✓ $line"
                else
                        log "  ✗ $line"
                fi
        done <"$SCORE"
        local fails
        fails="$(grep -c '^FAIL' "$SCORE" || true)"
        log "════════ transcript: $LOOP_T"
        if [[ "$fails" -gt 0 ]]; then
                log "════════ RESULT: FAIL ($fails failed) ════════"
                return 1
        fi
        log "════════ RESULT: ALL GREEN ════════"
}

# ---- main -------------------------------------------------------------------
: >"$LOOP_T" # transcript starts here even if boot fails

if [[ $MANAGED -eq 1 ]]; then
        log "orvexa e2e demo — MANAGED mode (booting devstack + api + worker + fresh tenant)"
        trap 'cleanup; rm -f "$SCORE"' EXIT
        managed_boot
else
        SECRET="$ORVEXA_SECRET"
        log "orvexa e2e demo — EXTERNAL mode ($ORVEXA_URL)"
fi
H=(-H "X-API-Key: $ORVEXA_KEY" -H "Content-Type: application/json")

RC=0
if [[ $MODE_LOOP -eq 1 ]]; then
        demo_loop || RC=1
fi
if [[ $MODE_JOURNEYS -eq 1 ]]; then
        run_journeys || RC=1
fi

summary || RC=1
exit "$RC"
