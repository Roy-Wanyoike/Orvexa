# Orvexa

**The AI-native contact center OS — open infrastructure for customer conversations that remember, route, and resolve.**

Orvexa is a four-plane, event-driven Go core where customers, conversations, AI agents and business workflows form one continuous system — with every carrier, LLM and datastore behind tested, swappable ports.

[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-3b82f6.svg)](LICENSE)
[![Merged PRs](https://img.shields.io/badge/PRs_merged-36-8250DF)](https://github.com/Roy-Wanyoike/Orvexa/pulls?q=is%3Apr+is%3Amerged)
[![Verification gate](https://img.shields.io/badge/verification-ADR--0003_gate-1f6feb)](docs/adr/0003-verification-without-hosted-actions.md)
[![Security posture](https://img.shields.io/badge/security-PUBLIC--READY-2ea44f)](docs/security/security-posture.md)
[![Release verdict](https://img.shields.io/badge/release-GO_for_pilot-2ea44f)](qa/QA_REPORT.md)

---

## Why Orvexa

The contact-center industry still runs on 1990s CRUD: banks of agents with no memory of the customer, per-seat pricing with per-minute carrier lock-in, and bolt-on AI that never touches the actual workflow. Orvexa rebuilds the stack as infrastructure:

- **One conversation, many channels.** A phone call is *not* the customer relationship — it is one interaction inside a continuous conversation. WhatsApp → voice → email on the same conversation keeps one context; identity normalization means a phone number can never fork into two customers (asserted by tests: [`internal/customers`](internal/customers)).
- **Four planes, one critical path.** Interaction, Intelligence, Execution and Control communicate only through the event bus, defined interfaces or the API — nothing outside telephony → routing → agent may block a live call.
- **Carrier-agnostic by construction.** Twilio, WhatsApp Cloud, Africa's Talking, FreeSWITCH, Asterisk and the built-in simulator sit behind two ports ([`internal/telephony/ports.go`](internal/telephony/ports.go), [`internal/messaging/ports.go`](internal/messaging/ports.go)) and must pass the same conformance kit to merge. Switching carriers is an env var, not a rewrite.
- **AI with a safety boundary.** AI suggestions are metered; every AI→side-effect call passes a deny-by-default tool gateway that allowlists, schema-validates, rate-limits and audits — refusals included.

## Architecture

Four planes around a transactional event backbone; three stateless deployables; carriers behind a hexagon:

```mermaid
flowchart TB
    CH["Channels — voice · WhatsApp · SMS · USSD"] --> API
    IDP["OIDC identity provider"] --> API

    API["cmd/api<br/>REST v1 · dual auth · capability RBAC · signed webhook ingress"]
    WK["cmd/worker<br/>outbox dispatcher · audit · facts · search indexer · workflow executor"]
    RT["cmd/realtime<br/>WebSocket fan-out"]

    API --> INT
    INT --> BUS
    BUS --> WK
    BUS --> RT

    subgraph PLANES["Domain planes — bounded contexts"]
        direction LR
        INT["INTERACTION — critical path<br/>telephony · messaging · routing · presence · agents · queues"]
        EXE["EXECUTION<br/>durable workflows · callbacks · notifications"]
        NTL["INTELLIGENCE<br/>AI gateway · tool gateway · analytics facts · search"]
    end

    CTL["CONTROL — everywhere<br/>tenancy · identity · capability RBAC · audit · metering"]
    CTL -. fast checks on every request .-> API

    BUS["EVENT BUS — transactional outbox → at-least-once<br/>inproc default · NATS JetStream driver"]

    INT -. "VoiceProvider / MessagingProvider ports" .-> CAR
    subgraph CAR["Conformance-tested carrier adapters"]
        direction LR
        TW["Twilio"] --- WA["WhatsApp Cloud"] --- ATC["Africa's Talking"] --- FSW["FreeSWITCH"] --- AST["Asterisk"] --- SIM["simulator (default)"]
    end
```

| Plane | Owns | Critical path? |
|---|---|---|
| **Interaction** | Realtime customer interactions: telephony, messaging, routing, agent assignment | **Yes** — nothing else may block a call |
| **Intelligence** | AI gateway, tool gateway, transcription, analytics facts, search | No — consumes events, fails independently |
| **Execution** | Durable workflows, callbacks, notifications | No — event-driven consumers |
| **Control** | Tenancy, identity, authorization, metering, audit | Yes — but only as fast checks |

Core domain model: **Customer → Conversation → Interaction → Case → Workflow**. Decisions live in the [ADRs](docs/adr) — start with [ADR-0001](docs/adr/0001-modular-distributed-architecture.md) (planes), [ADR-0004](docs/adr/0004-event-delivery-and-outbox.md) (outbox), [ADR-0005](docs/adr/0005-provider-agnostic-communications.md) (hexagon).

## Engineering quality — every claim links to evidence

| Claim | Evidence |
|---|---|
| 32 test packages green under the race detector; build, vet, gofmt, migration and boot gates pass | verified per [ADR-0003](docs/adr/0003-verification-without-hosted-actions.md), recorded in [qa/QA_REPORT.md](qa/QA_REPORT.md); re-run on this PR |
| Provider conformance kit — every carrier adapter must pass the same lifecycle contract, audited by negative controls | [internal/comms/conformance/README.md](internal/comms/conformance/README.md) |
| Transactional outbox: state + events in one SQL transaction, lease-based dispatcher, at-least-once with idempotent consumers, failures parked — never deleted | [internal/platform/outbox](internal/platform/outbox), [ADR-0004](docs/adr/0004-event-delivery-and-outbox.md) |
| Tool gateway: the only path from AI to side-effects — allowlists, schemas, rate windows, audited refusals; AI holds no credentials | [internal/tools](internal/tools), [architecture § AI boundary](docs/architecture.md#ai-boundary) |
| Capability RBAC: IdP owns identity, Orvexa owns authorization; role→capability matrix pinned by tests; revocation lands within the TTL; 10-attack JWT forgery matrix | [docs/rbac.md](docs/rbac.md), [internal/identity](internal/identity), [ADR-0008](docs/adr/0008-oidc-identity-capability-rbac.md) |
| Tenant isolation: tenant resolved **only** from the authenticated key or verified token — never the wire; cross-tenant ids read as 404; hard isolation on realtime fan-out; server-side tenant filter on search | [internal/tenancy](internal/tenancy), [internal/realtime](internal/realtime), [QA §4](qa/QA_REPORT.md) |
| Fail-closed webhook ingress: per-provider HMAC (Twilio / WhatsApp Cloud / Africa's Talking / platform), tamper + replay + empty-secret rejection, no unsigned path exists | [internal/webhooks](internal/webhooks), [QA §2](qa/QA_REPORT.md) |
| Rate limiting on all 24 mutation routes with server-side keying and memory-bounded eviction | [security posture §4](docs/security/security-posture.md), [internal/platform/httpx](internal/platform/httpx) |
| OpenAPI contract tests: router ⇄ spec bidirectional conformance, uniform `{data,meta}` / error envelope | [tests/contract](tests/contract), [api/openapi/orvexa-v1.yaml](api/openapi/orvexa-v1.yaml) (40 paths) |
| Security sweep verdict: **PUBLIC-READY** — secret scan clean (canary-validated), headers complete, govulncheck **0 reachable vulnerabilities** | [docs/security/security-posture.md](docs/security/security-posture.md), [scripts/security-scan.sh](scripts/security-scan.sh) |
| Truthful health: `/readyz` reports component truth (503 when degraded), workers fail fast without storage, degradation is typed (never silent) | [QA §1](qa/QA_REPORT.md), [operations runbook](docs/runbooks/operations.md) |

Full-loop integration suites (webhook dedup against `provider_events`, outbox lease/ack) run against real PostgreSQL via the [devstack](docs/devstack.md): `make integration`.

## Quickstart

Verified end to end on a fresh clone (full transcript attached to the README pull request): no cloud accounts, no docker, no sudo — Go 1.26+, bash, curl, jq.

```bash
# 1. dev stack: userland PostgreSQL 16 + migrations (idempotent; docker compose also available)
make devstack-up

# 2. run the platform (two terminals)
export ORVEXA_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/orvexa?sslmode=disable'
export ORVEXA_WEBHOOK_HMAC_SECRET='dev-secret-change-me'   # signs/validates every provider webhook
go run ./cmd/api      # REST + webhook gateway on :8080
go run ./cmd/worker   # outbox dispatcher, audit + facts consumers, workflow executor

# 3. bootstrap org + tenant + API key (SQL until the admin API lands — see runbook)
psql "$ORVEXA_DATABASE_URL" <<'SQL'
INSERT INTO organizations (id, name, slug) VALUES (gen_random_uuid(), 'Acme', 'acme');
INSERT INTO tenants (id, organization_id, name) VALUES (gen_random_uuid(), (SELECT id FROM organizations), 'Acme Support');
INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes)
VALUES (gen_random_uuid(), (SELECT id FROM tenants), 'bootstrap',
        encode(sha256('orvx_dev-bootstrap-key'::bytea), 'hex'), '{api}');
SELECT id AS tenant_id FROM tenants;
SQL

# 4. drive the loop through the public contract
export ORVEXA_KEY='orvx_dev-bootstrap-key'
curl -s http://localhost:8080/readyz | jq -c .        # {"status":"ready"}

# a customer with two channel identities — one identity, not two silos
CUST=$(curl -s -X POST -H "X-API-Key: $ORVEXA_KEY" -H 'Content-Type: application/json' \
  -d '{"display_name":"Jane Wanjiku","identifiers":[{"type":"phone","value":"+254 712 345 678"},{"type":"whatsapp","value":"+254712345678","is_primary":true}]}' \
  http://localhost:8080/api/v1/customers | jq -r .data.id)

# place an outbound call: the simulator carrier reports progress via SIGNED webhooks
# through the same public gateway a real carrier uses — the hexagon, live
CALL=$(curl -s -X POST -H "X-API-Key: $ORVEXA_KEY" -H 'Content-Type: application/json' \
  -d "{\"customer_id\":\"$CUST\",\"to\":\"+254712345678\"}" \
  http://localhost:8080/api/v1/calls | jq -r .data.ID)

# prove the ingress is fail-closed and idempotent: sign, replay, tamper
BODY=$(jq -nc --arg i "$CALL" --arg n "qs-$(date +%s)" '{event:"call.connected",interaction_id:$i,nonce:$n,tenant_id:"<tenant_id from step 3>"}')
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac 'dev-secret-change-me' -hex | awk '{print $2}')
curl -s -X POST -H 'Content-Type: application/json' -H "X-Orvexa-Signature: $SIG" -d "$BODY" http://localhost:8080/api/v1/webhooks/simulator | jq -c .   # processed:true
curl -s -X POST -H 'Content-Type: application/json' -H "X-Orvexa-Signature: $SIG" -d "$BODY" http://localhost:8080/api/v1/webhooks/simulator | jq -c .   # duplicate:true — single effect
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H 'Content-Type: application/json' -H "X-Orvexa-Signature: deadbeef" -d "$BODY" http://localhost:8080/api/v1/webhooks/simulator   # 401 — tamper rejected

# finish the interaction, open a case, schedule a callback workflow
curl -s -X POST -H "X-API-Key: $ORVEXA_KEY" -H 'Content-Type: application/json' -d '{"action":"hangup"}'   http://localhost:8080/api/v1/calls/$CALL/actions | jq -r .data.Status
curl -s -X POST -H "X-API-Key: $ORVEXA_KEY" -H 'Content-Type: application/json' -d '{"action":"complete"}' http://localhost:8080/api/v1/calls/$CALL/actions | jq -r .data.Status
CASE_ID=$(curl -s -X POST -H "X-API-Key: $ORVEXA_KEY" -H 'Content-Type: application/json' \
  -d "{\"customer_id\":\"$CUST\",\"subject\":\"Billing dispute\"}" \
  http://localhost:8080/api/v1/cases | jq -r .data.id)
curl -s -X POST -H "X-API-Key: $ORVEXA_KEY" "http://localhost:8080/api/v1/cases/$CASE_ID/interactions/$CALL/link" -o /dev/null -w 'linked: %{http_code}\n'
curl -s -X POST -H "X-API-Key: $ORVEXA_KEY" -H 'Content-Type: application/json' \
  -d "{\"customer_id\":\"$CUST\",\"phone\":\"+254712345678\",\"notes\":\"dispute follow-up\"}" \
  http://localhost:8080/api/v1/workflows/callbacks | jq -c '{type:.data.type,status:.data.status}'   # waiting_timer

# stop: make devstack-down   ·   full loop incl. analytics: scripts/e2e-demo.sh (see limitations)
```

Analytics facts are asynchronous (worker consumes the outbox): give it a beat, then `GET /api/v1/analytics/summary`. Configuration reference: [operations runbook](docs/runbooks/operations.md).

