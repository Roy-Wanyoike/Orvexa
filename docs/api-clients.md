# Orvexa API clients — Postman collection + runnable .http examples

Issue: [#47](https://github.com/Roy-Wanyoike/Orvexa/issues/47) — [O-38].

Batteries-included request collections for the full v1 surface
(`api/openapi/orvexa-v1.yaml`), ordered as the end-to-end integration journey:

health → discovery → customer → resolution → agents & presence → interaction
lifecycle → conversation → routing decision → AI suggestion & governed tools →
outbound call → messages → case & notes → durable workflow → analytics →
search → identity → signed provider webhook.

| Asset | For | File |
| --- | --- | --- |
| Postman v2.1 collection (also imports into Bruno) | Postman / Bruno desktop | `api/clients/orvexa.postman_collection.json` |
| REST Client examples (15 numbered journey files) | VS Code `ms-vscode.restclient`, JetBrains HTTP client, `curl` | `api/examples/*.http` |
| Live replay transcript (evidence) | reviewers | `api/examples/REPLAY.md` |

## 1. Boot a local stack

```bash
ORVEXA_DEVSTACK_PORT=55446 ./scripts/devstack.sh start   # userland PostgreSQL 16, no docker
ORVEXA_DATABASE_URL="$(ORVEXA_DEVSTACK_PORT=55446 ./scripts/devstack.sh url)" \
ORVEXA_WEBHOOK_HMAC_SECRET='<any long random string>' \
go run ./cmd/api
```

Details and the docker-compose flavor: [docs/devstack.md](devstack.md).

## 2. Bootstrap a tenant + API key

SQL until the admin API lands ([#56](https://github.com/Roy-Wanyoike/Orvexa/issues/56)) —
full walkthrough in [docs/runbooks/operations.md](runbooks/operations.md) § First tenant bootstrap:

```sql
INSERT INTO organizations (id, name, slug) VALUES (gen_random_uuid(), 'Acme', 'acme');
INSERT INTO tenants (id, organization_id, name)
  VALUES (gen_random_uuid(), (SELECT id FROM organizations), 'Acme Support');
-- store only the SHA-256 hash of the raw key:
INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes)
  VALUES (gen_random_uuid(), (SELECT id FROM tenants), 'bootstrap',
          encode(sha256(convert_to('<raw-key>','UTF8')),'hex'), '{api}');
```

## 3. Postman / Bruno setup

1. **Import:** Postman → *Import* → file `api/clients/orvexa.postman_collection.json`
   (Bruno: *Import Collection* → Postman format). The schema is Postman v2.1
   (`info.schema`), 16 domain folders, 51 requests, all with `pm.test` snippets.
2. **Variables** (collection → *Variables* — set the first three; the rest are
   captured automatically by test scripts as the journey runs):

   | Variable | Meaning | Notes |
   | --- | --- | --- |
   | `ORVEXA_URL` | API base URL | e.g. `http://localhost:8080` |
   | `ORVEXA_KEY` | tenant API key (raw) | sent as `X-API-Key` via collection auth. **Never commit a real key.** |
   | `ORVEXA_SECRET` | webhook HMAC secret | must equal the server's `ORVEXA_WEBHOOK_HMAC_SECRET`. **Never commit it.** |
   | `customer_id`, `conversation_id`, `interaction_id`, `tenant_id`, `agent_id`, `queue_id`, `case_id`, `call_id`, `workflow_id`, `webhook_body`, `webhook_sig`, `webhook_event_id` | journey state | written by the requests that create the resource |

3. **Run Collection** — folders are journey-ordered top-to-bottom; the runner
   chains ids between requests. Success responses use `{data, meta}`; errors
   use `{error: {code, message, details?}}`.
4. The **Webhooks** folder is self-contained: its pre-request script builds the
   raw body and computes `X-Orvexa-Signature: hex(HMAC-SHA256(ORVEXA_SECRET, rawBody))`
   in Postman script — the same math as `scripts/e2e-demo.sh`:

   ```bash
   SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$ORVEXA_SECRET" -hex | awk '{print $2}')
   ```

   Carrier-registered providers (`twilio`, `whatsappcloud`, `africastalking`)
   verify with their own schemes instead — see
   [docs/runbooks/operations.md](runbooks/operations.md) § Webhook signatures.

## 4. VS Code REST Client / JetBrains HTTP client

```bash
export ORVEXA_URL=http://localhost:8080 \
       ORVEXA_KEY=<raw key> \
       ORVEXA_SECRET='<the server secret>'
```

Then open the numbered files in order — `01-health-and-discovery.http` through
`15-identity.http`. Requests are named (`# @name`) and chain responses via
`{{createCustomer.response.body.$.data.id}}` style references, so each file
plays the journey step it covers. No secrets are ever written inside the
files: everything resolves from the environment (`{{$processEnv …}}`).

`05-signed-webhook.http` documents the signature recipe inline (same
`openssl dgst` one-liner as above) — hash the **exact raw body bytes**, a
one-byte difference rejects. The file also shows the fail-closed negatives:
tampered and unsigned deliveries are rejected `401 webhook.invalid_signature`
and never persisted as processed.

## 5. Live-wire contract notes

One place where this build's HTTP binding is narrower than the OpenAPI
document (the example pins what actually runs, with a ⚠ note in-file):

- `POST /api/v1/ai/agents/{id}/tools` binds its request body with
  `DisallowUnknownFields` to Go field names (`Tool`/`InteractionID`/`Args`)
  — the snake_case variants documented in the spec answer 422.

The interaction plane drifted the same way until issue #101 fixed it:
`interactions.Rec` and `interactions.CreateInput` now carry snake_case json
tags, so interaction-plane requests and responses speak the documented
contract exactly (`customer_id`, `id`, `status`, …), and `POST
/api/v1/messages` applies the platform default sender (`orvexa-messaging`)
when the optional `from` is omitted. The AI-tools bullet above remains the
only live drift; reconciling it is tracked outside this issue.

## 6. Quickstart pointer

The 60-second tour: `scripts/e2e-demo.sh` (customer → signed inbound webhook →
conversation + interaction → routing → AI → call → case → workflow →
analytics). The collections replay the same journey one request at a time.
Evidence that the collection/example requests work against a real stack:
[api/examples/REPLAY.md](../api/examples/REPLAY.md).
