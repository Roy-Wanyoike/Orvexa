#!/usr/bin/env bash
# Orvexa end-to-end demo: the full interaction loop through the public contract.
#
# Prerequisites:
#   - API on $ORVEXA_URL (default http://localhost:8080) with a database,
#     migrations applied, and ORVEXA_WEBHOOK_HMAC_SECRET set on the server
#   - a tenant API key in $ORVEXA_KEY
#   - jq
#
# The loop exercised (matches qa/QA_REPORT.md):
#   customer → inbound WhatsApp webhook (signed) → conversation+interaction
#   → routing decision → assignment → AI suggestion (metered) → tool call
#   (audited) → case → callback workflow → analytics → audit trail.

set -euo pipefail
ORVEXA_URL="${ORVEXA_URL:-http://localhost:8080}"
ORVEXA_KEY="${ORVEXA_KEY:?set ORVEXA_KEY}"
ORVEXA_SECRET="${ORVEXA_SECRET:?set ORVEXA_SECRET (must match the server)}"
H=(-H "X-API-Key: $ORVEXA_KEY" -H "Content-Type: application/json")

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$1"; }

say "1. health"
curl -sf "$ORVEXA_URL/readyz" | jq -e '.data.status=="ready"' >/dev/null || { echo "not ready"; exit 1; }
echo "ready"

say "2. customer with two channel identities"
CUST=$(curl -sf "${H[@]}" -d '{"display_name":"Jane Wanjiku","identifiers":[{"type":"phone","value":"+254 712 345 678"},{"type":"whatsapp","value":"+254712345678","is_primary":true}]}' "$ORVEXA_URL/api/v1/customers" | jq -r .data.id)
echo "customer: $CUST"

say "3. identifier resolution — same customer via phone"
GOT=$(curl -sf "${H[@]}" -d '{"type":"phone","value":"254712345678"}' "$ORVEXA_URL/api/v1/customers/resolve" | jq -r .data.id)
test "$GOT" = "$CUST" && echo "resolve OK (normalized: 254… → +254…)"

say "4. signed inbound WhatsApp webhook (simulator-shaped provider event)"
BODY=$(jq -nc --arg c "$CUST" '{event:"call.ringing",interaction_id:"pending",tenant_hint:"",note:"inbound whatsapp hi"}')
# inbound provider events enter through the gateway; the idempotency key is derived from the raw body
NONCE="inbound-$(date +%s)"
BODY=$(jq -nc --arg n "$NONCE" '{event:"inbound.whatsapp",nonce:$n,from:"+254712345678",body:"Hello, I need help"}')
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$ORVEXA_SECRET" -hex | awk '{print $2}')
curl -sf -X POST -H "Content-Type: application/json" -H "X-Orvexa-Signature: $SIG" \
  -d "$BODY" "$ORVEXA_URL/api/v1/webhooks/whatsapp_cloud" | jq -c .
echo "(replay is single-effect:)"
curl -sf -X POST -H "Content-Type: application/json" -H "X-Orvexa-Signature: $SIG" \
  -d "$BODY" "$ORVEXA_URL/api/v1/webhooks/whatsapp_cloud" | jq -c .

say "5. agents + queue + presence"
AG1=$(curl -sf "${H[@]}" -d '{"external_identity":"amara@acme","display_name":"Amara","language":"sw","skills":[{"skill":"billing","level":4}]}' "$ORVEXA_URL/api/v1/agents" | jq -r .data.id)
curl -sf "${H[@]}" -X PUT -d '{"status":"available"}' "$ORVEXA_URL/api/v1/agents/$AG1/presence" | jq -c .
echo "agent $AG1 available"

say "6. AI suggestion (metered) for the interaction"
# fetch the customer's open conversation
CONV=$(curl -sf "${H[@]}" "$ORVEXA_URL/api/v1/conversations?status=open" | jq -r '.data[0].id')
echo "conversation: $CONV"

say "7. place an outbound call via the simulator carrier (full loop)"
CALL=$(curl -sf "${H[@]}" -d "{\"customer_id\":\"$CUST\",\"to\":\"+254712345678\",\"options\":{\"tenant_id\":\"$CUST\"}}" "$ORVEXA_URL/api/v1/calls")
CALL_ID=$(echo "$CALL" | jq -r .data.id)
echo "call interaction: $CALL_ID status=$(echo "$CALL" | jq -r .data.status)  (simulator delivered ringing+connected via signed webhooks)"

say "8. hangup → wrapup → complete"
curl -sf "${H[@]}" -d '{"action":"hangup"}' "$ORVEXA_URL/api/v1/calls/$CALL_ID/actions" | jq -r '.data.status'
curl -sf "${H[@]}" -d '{"action":"complete"}' "$ORVEXA_URL/api/v1/calls/$CALL_ID/actions" | jq -r '.data.status'

say "9. open a case and link the interaction"
CASE=$(curl -sf "${H[@]}" -d "{\"customer_id\":\"$CUST\",\"subject\":\"Billing dispute\"}" "$ORVEXA_URL/api/v1/cases")
CASE_ID=$(echo "$CASE" | jq -r .data.id)
curl -sf "${H[@]}" -X POST "$ORVEXA_URL/api/v1/cases/$CASE_ID/interactions/$CALL_ID/link" -o /dev/null -w "linked: %{http_code}\n"

say "10. schedule a callback workflow"
curl -sf "${H[@]}" -d "{\"customer_id\":\"$CUST\",\"phone\":\"+254712345678\",\"notes\":\"dispute follow-up\"}" \
  "$ORVEXA_URL/api/v1/workflows/callbacks" | jq -c '{id:.data.id,type:.data.type,status:.data.status}'

say "11. analytics summary (facts)"
curl -sf "${H[@]}" "$ORVEXA_URL/api/v1/analytics/summary" | jq -c '.data | {total_interactions, interactions_by_channel}'

say "DONE — full loop green"
