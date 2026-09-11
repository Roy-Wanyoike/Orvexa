# Replay evidence — issue #47 (API client collection)

Live replay of the collection/example journey against a local devstack:

- **Stack:** `scripts/devstack.sh` PostgreSQL 16 (`ORVEXA_DEVSTACK_PORT=55446`, loopback) + `go run ./cmd/api` (`ORVEXA_HTTP_ADDR=127.0.0.1:18080`), migrations applied by the devstack runner.
- **Auth:** bootstrap SQL per `docs/runbooks/operations.md` § First tenant bootstrap; requests carry `X-API-Key: d9_…redacted` (raw key generated for the run, discarded after; never committed).
- **Webhook secret:** server env `ORVEXA_WEBHOOK_HMAC_SECRET` (value never printed). The signature below is `hex(HMAC-SHA256(secret, rawBody))` computed exactly as `scripts/e2e-demo.sh` does.
- **Hygiene:** no credential material in this transcript (verified by grep before commit).

Verdict: **ALL REPLAY STEPS GREEN** — health, discovery, fail-closed auth, customer create/list, interaction create, signed webhook ingest (202) + idempotent replay (200, `duplicate:true`), tampered/unsigned deliveries rejected 401 without persistence.

---


== 0. environment ==
API: http://127.0.0.1:18080  (go run ./cmd/api, ORVEXA_HTTP_ADDR=127.0.0.1:18080)
DB:  devstack PostgreSQL 16 on 127.0.0.1:55446  (ORVEXA_DEVSTACK_PORT=55446, scripts/devstack.sh)
Auth: X-API-Key: d9_…redacted — bootstrap SQL per docs/runbooks/operations.md § First tenant bootstrap
Webhook secret: server env ORVEXA_WEBHOOK_HMAC_SECRET (value never printed)

== 1. readiness — the journey gate ==
$ curl -s http://127.0.0.1:18080/readyz
{"data":{"checks":[{"name":"db","status":"up"}],"status":"ready","version":"dev (commit=none, built=unknown)"}}

readyz: ready OK

== 2. liveness ==
$ curl -s http://127.0.0.1:18080/healthz
{"data":{"status":"alive","version":"dev (commit=none, built=unknown)"}}

healthz: alive OK

== 3. discovery index GET /api/v1/ (authenticated; new request added to the collection) ==
$ curl -s http://127.0.0.1:18080/api/v1/ -H 'X-API-Key: d9_…redacted'
{"data":{"resources":["customers","conversations","interactions","agents","queues","cases"]}}

discovery resources OK: customers, conversations, interactions, agents, queues, cases

== 4. auth fail-closed: no key → 401 auth.missing_key ==
$ curl -s http://127.0.0.1:18080/api/v1/customers   (no key)
{"error":{"code":"auth.missing_key","message":"missing API key"}}
HTTP 401
fail-closed OK

== 5. customers create → 201 ==
$ curl -s -X POST http://127.0.0.1:18080/api/v1/customers -H 'X-API-Key: d9_…redacted' -H 'Content-Type: application/json' -d '{"display_name":"Jane Wanjiku","language":"en","timezone":"Africa/Nairobi","identifiers":[{"type":"phone","value":"+254 712 345 678"},{"type":"whatsapp","value":"+254712345678","is_primary":true}],"tags":["d9-replay"]}'
{"data":{"id":"0e2da82d-b418-4667-942d-8aa0d56c2ff4","tenant_id":"a06e5126-f635-47e6-85b6-8d9d8e6a3c36","display_name":"Jane Wanjiku","language":"en","timezone":"Africa/Nairobi","attributes":{},"tags":["d9-replay"],"identifiers":[{"id":"e307b84e-11c1-4416-946a-b27cf9c7ec28","type":"whatsapp","value":"+254712345678","is_primary":true,"created_at":"2026-09-11T00:34:14.700078Z"},{"id":"aa523c68-f2e4-4afb-8001-3e07f20109f6","type":"phone","value":"+254712345678","is_primary":false,"created_at":"2026-09-11T00:34:14.700078Z"}],"created_at":"2026-09-11T00:34:14.700078Z","updated_at":"2026-09-11T00:34:14.700078Z"}}

201

captured: customer_id=0e2da82d-b418-4667-942d-8aa0d56c2ff4

== 6. customers list → 200, contains the created id ==
$ curl -s 'http://127.0.0.1:18080/api/v1/customers?limit=25' -H 'X-API-Key: d9_…redacted'
{"data":[{"id":"0e2da82d-b418-4667-942d-8aa0d56c2ff4","tenant_id":"","display_name":"Jane Wanjiku","language":"en","timezone":"Africa/Nairobi","attributes":null,"created_at":"2026-09-11T00:34:14.700078Z","updated_at":"2026-09-11T00:34:14.700078Z"}],"meta":{"next_cursor":""}}

list contains created customer OK

== 7. interactions create → 201 (live-wire Go field binding: customerid/channel/…, DisallowUnknownFields — see api/examples/04-interactions-lifecycle.http) ==
$ curl -s -X POST http://127.0.0.1:18080/api/v1/interactions -H 'X-API-Key: d9_…redacted' -H 'Content-Type: application/json' -d '{"customerid":"0e2da82d-b418-4667-942d-8aa0d56c2ff4","channel":"chat","direction":"inbound","source":"web","destination":"support","provider":"internal","idempotencykey":"d9-replay-31539"}'
{"data":{"ID":"311f4310-dbf7-4a84-b949-544e353cd48f","TenantID":"a06e5126-f635-47e6-85b6-8d9d8e6a3c36","ConversationID":"5a25c653-3901-4ee3-8ef9-03210bec5f34","CustomerID":"0e2da82d-b418-4667-942d-8aa0d56c2ff4","Channel":"chat","Direction":"inbound","Status":"pending","Source":"web","Destination":"support","AssignedAgentID":"","AssignedQueueID":"","StartedAt":"2026-09-11T00:34:14.760407099Z","AnsweredAt":null,"EndedAt":null,"EndReason":"","Provider":"internal","ProviderRef":"","IdempotencyKey":"d9-replay-31539","Attributes":{}}}

201

captured: interaction ID=311f4310-dbf7-4a84-b949-544e353cd48f  TenantID=a06e5126-f635-47e6-85b6-8d9d8e6a3c36
note: response uses Go-default field names (ID/TenantID/…) — pinned as a live-wire note in the examples + docs

== 8. signed webhook ingest → 202 {duplicate:false, processed:true} ==
signature recipe (exactly scripts/e2e-demo.sh):
  SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$ORVEXA_SECRET" -hex | awk '{print $2}')
raw body: {"event":"message.delivered","interaction_id":"311f4310-dbf7-4a84-b949-544e353cd48f","tenant_id":"a06e5126-f635-47e6-85b6-8d9d8e6a3c36","detail":"delivered to handset"}
X-Orvexa-Signature: 159bd7c158461f259689a232a9ffca6323d42852588972336b8f090c0be02fa9
$ curl -s -X POST http://127.0.0.1:18080/api/v1/webhooks/whatsapp_cloud -H 'Content-Type: application/json' -H 'X-Orvexa-Signature: <SIG>' -d <raw body>
{"data":{"accepted":true,"duplicate":false,"event_id":"auto_01758a816a45fe1c4ea29335ad50b617","processed":true}}

202

ingest OK: {'accepted': True, 'duplicate': False, 'event_id': 'auto_01758a816a45fe1c4ea29335ad50b617', 'processed': True}

== 9. webhook replay — same raw body + same signature → 200 {duplicate:true} (single-effect) ==
$ curl -s -X POST …/webhooks/whatsapp_cloud   (byte-identical body + signature)
{"data":{"accepted":true,"duplicate":true,"event_id":"auto_01758a816a45fe1c4ea29335ad50b617","processed":true}}

200

idempotent replay OK: {'accepted': True, 'duplicate': True, 'event_id': 'auto_01758a816a45fe1c4ea29335ad50b617', 'processed': True}

== 10. tampered signature → 401 webhook.invalid_signature (fail-closed) ==
$ curl -s -X POST …/webhooks/whatsapp_cloud   (first bytes of signature flipped)
{"error":{"code":"webhook.invalid_signature","message":"signature validation failed"}}

401

tamper rejected OK

== 11. unsigned → 401 webhook.invalid_signature (no unsigned ingestion path exists) ==
$ curl -s -X POST …/webhooks/whatsapp_cloud   (no X-Orvexa-Signature header)
{"error":{"code":"webhook.invalid_signature","message":"signature validation failed"}}

401

unsigned rejected OK

ALL REPLAY STEPS GREEN
