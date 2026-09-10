# Africa's Talking Voice adapter (`telephony.VoiceProvider`)

Issue [#28] ([O-19]) — the regional first-choice voice carrier for the Nairobi
MVP. This package is the exclusive scope of that issue: it implements the
hexagonal voice port against the Africa's Talking (AT) Voice REST surface and
owns nothing else (no go.mod changes — stdlib `net/http` only, no webhook
gateway, no registry, no core telephony service edits).

```
PlaceCall ──POST /call (form-encoded, apikey header)──▶ AT Voice
   ▲                                                      │ (async, later)
   │                                        status callbacks (JSON, UNSIGNED)
HandleStatusCallback ◀── webhook gateway (peer-IP allowlist) ──────────────┘
   │ TranslateStatus (pure)
   └── signed comms.ProviderEvent ──▶ IngestFunc ──▶ fail-closed gateway ──▶ Processor
```

## Environment variables

Selected with the registry (`internal/comms/registry`):

| Variable | Required | Meaning |
|---|---|---|
| `ORVEXA_TELEPHONY_PROVIDER` | yes | `africastalking` selects this adapter for the voice plane |
| `ORVEXA_AT_USERNAME` | yes (secret) | AT account username, sent in every make-call form |
| `ORVEXA_AT_API_KEY` | yes (secret) | AT API key, sent as the **`apikey` header** |
| `ORVEXA_AT_VOICE_PRODUCT_CODE` | yes (secret) | Registry boot-validation field; the `/call` dial authenticates with username+apikey — the product code is reserved for product-scoped AT surfaces |

Credentials enter only as `registry.Secret` (zero-leakage renderings); the
adapter renders itself redacted on every fmt/slog/json path (proven by the
redaction matrix in `voice_test.go`).

Not env vars, deliberately:

- **`Config.APIBaseURL`** — a wiring-time `Config` field, not an env var.
  Production default `https://voice.africastalking.com`; sandbox deployments
  set `SandboxAPIBaseURL` (`https://voice.sandbox.africastalking.com`). (The
  voice surface lives on the `voice.*` host, not `api.africastalking.com`.)
- **Callback URL** — configure the AT dashboard's callBackUrl to point at the
  platform webhook ingress: `POST /api/v1/webhooks/africastalking` (behind the
  fail-closed gateway, below).

## Authentication contract (research finding)

AT authenticates the voice surface with the **`apikey` header plus the
`username` form field** — it is not a Bearer scheme. The issue brief said
"Bearer apiKey"; the documented wire contract says otherwise and this adapter
follows the wire (see `client.go`). The fake carrier in `fake_test.go` enforces
exactly this and the wire-contract test pins that **no `Authorization` header
is ever sent**.

## Call lifecycle (async by carrier contract)

`PlaceCall` POSTs `username`, `to` (E.164), `from` (callerId) and
`clientRequestId=<interaction id>` to `/call`. A 2xx envelope with entry status
`Queued` means AT accepted the dial — **nothing has rung yet**, and this
adapter emits no events on the dial path. Lifecycle progress arrives later as
status callbacks, routed by the `clientRequestId` echo first, then the
adapter's `sessionId` registry:

| AT status | ProviderEvent | Processor effect |
|---|---|---|
| `Queued` | `call.ringing` | pending → active |
| `InProgress` | `call.connected` | active (same-state no-op) |
| `Completed` | `call.ended` (Detail `completed [ (cause)]`) | active → wrapup |
| `Failed` | `call.ended` (`failed`) | active → wrapup |
| `Busy` | `call.ended` (`busy`) | active → wrapup |
| `NoAnswer` | `call.ended` (`no_answer`) | active → wrapup |
| anything else | typed `at.unknown_status` (422) | nothing — never a guessed event |

A callback that carries neither a known `clientRequestId` nor a known
`sessionId` is rejected `at.callback_unroutable` (404) — phantom callbacks
never materialize state. Terminal statuses mark the leg ended (carrier truth).

## Capability matrix (honest)

| Operation | AT native support | Adapter behavior |
|---|---|---|
| Place call | `POST /call`, async (`Queued` + sessionId) | REST dial; lifecycle only via callbacks |
| Hangup | **no out-of-band REST hangup** | Two honest layers: (1) platform — leg marked ended and a signed `call.ended` (Detail `hangup`) delivered **at acceptance**, the audit record of the operator request; (2) carrier — an empty action body queued for the session's next callback exchange (AT ends the leg when its action queue runs out). The carrier's own terminal callback remains the carrier confirmation. Idempotent no-op on an already-ended leg |
| Transfer | **no REST transfer**; documented `<Dial>` callback action | Platform: a new ringing phase on the same leg with `Detail "transfer:<dest>"` (audit: where the customer was sent). Carrier: `<Dial phoneNumbers="…"/>` rides the next callback response |
| Hold / Resume | **no native hold API** | Platform-side leg-state flip only, deliberately lifecycle-silent (the closed event vocabulary has no hold semantics; inventing one would wrap up or re-ring a live call). Media-level hold belongs to the deployment's AT call flow |
| Control a terminal leg | AT exposes **no API for terminated sessions** | Typed `ErrUnsupportedOperation` (`at.unsupported_operation`, conflict) — never a faked success |
| Act on an unknown reference | — | Typed `at.unknown_leg` (404), deterministic |

## Unsigned callbacks — the gateway allowlist is the pairing piece

**AT does not sign its callbacks.** There is no HMAC, no checksum, no
signature header (research finding, issue #24; unchanged for voice). This
adapter therefore performs **no origin verification** — by design, never by
omission — and relies on the fail-closed webhook gateway
(`internal/webhooks.VerifyAfricasTalking`), which implements AT's only viable
server-side control, the **peer-IP allowlist**:

- Allowlist **configured** → the delivery's peer address (rightmost
  `X-Forwarded-For` entry, or the header named by `ATPeerIPHeader`) must match
  a configured CIDR, else the callback is rejected (401-class). Host-bits-set
  or unparseable entries disable themselves (fail-closed).
- Allowlist **unconfigured** → the callback is accepted **with a structured
  WARN on every such acceptance** — an honest residual risk (callback forgery
  is possible unless network-layer controls exist), never a silent bypass.

Deployment requirement: point AT's callBackUrl at the gateway route and
configure the AT egress CIDRs in the gateway's `VerifierConfig`.

**Multi-replica note:** the `sessionId → leg` registry is in-process. Deploy
more than one replica and the edge must route an AT callback to the replica
holding the session (sticky routing); callbacks landing elsewhere are
rejected `at.callback_unroutable` rather than guessed. The
`clientRequestId` echo carries the interaction id, but tenant resolution
still requires this process's leg registry.

## Error taxonomy

All failures are `*apperrors.Error` with stable `at.*` machine codes; provider
text is bounded into `Details` (`http_status`, `at_message`, `at_entry_message`)
and no error ever contains credential material.

| Code | Kind | When |
|---|---|---|
| `at.credentials_required` / `at.ingest_required` / `at.ingest_not_wired` | invalid | construction (Ingest + Signer are a mandatory pair) |
| `at.interaction_required`, `at.caller_required`, `at.destination_required`, `comms.tenant_required` | invalid | command validation, before any carrier contact |
| `at.invalid_phone` | invalid | E.164 normalization (shared customer normalizer) |
| `at.interaction_already_placed` | conflict | duplicate dial — the double-bill guard |
| `at.call_rejected` / `at.call_not_queued` | conflict | 2xx envelope that did not queue the call |
| `at.request_rejected` / `at.auth_failed` / `at.forbidden` / `at.not_found` | invalid / unauth / forbidden / not_found | terminal 4xx |
| `at.conflict` / `at.rate_limited` / `at.upstream_error` | conflict / rate_limited / internal | 409 / 429 / 5xx after the bounded retry budget |
| `at.transport_error` | internal | delivery failure (URL sanitized in the cause) |
| `at.envelope_invalid` | internal | unreadable 2xx envelope / no matching entry |
| `at.unsupported_operation` | conflict | typed sentinel `ErrUnsupportedOperation` (terminal-leg control) |
| `at.unknown_leg` | not_found | unknown provider reference |
| `at.callback_malformed` / `at.unknown_status` | invalid | callback parsing / closed status vocabulary |
| `at.callback_unroutable` | not_found | phantom callback (no known route) |

**Retry posture:** only 409/429/5xx are retried, bounded at ≤3 attempts
(`MaxRetries` clamps to the ceiling), full-jitter exponential backoff
(default base 250 ms, ceiling 2 s); server `Retry-After` guidance is honored
exactly, never jittered. Other 4xx and transport errors are terminal — the
core's provider-failure path owns the retry-after-failure story.

## Conformance

`voice_test.go` embeds `conformance.RunVoiceConformance`
(`internal/comms/conformance`) in **async mode**
(`RequireAsyncEvents: true`, `EnforceCommandValidation: true`, provider label
pinned to `africastalking`). The fake carrier (`fake_test.go`) is an httptest
server on both planes: the `/call` dial surface (apikey-gated, JSON
envelopes, scriptable responses) and the callback pump that plays the
carrier's status callbacks over the adapter's real `StatusCallbackHandler`
mounted as a second httptest server — the same ingress the gateway exposes in
production. Scenarios covered beyond the kit: the carrier-failure table,
bounded-retry proofs (retry-then-succeed, exact `Retry-After`, jitter
schedule, cancellation), callback routing e2e, in-call control action
delivery, post-terminal semantics, oversized-body rejection and the
adversarial credential-redaction matrix.

```sh
go test -race ./internal/telephony/...
```

## Ownership & rollback

Exclusive to issue #28; changes land only under
`internal/telephony/adapters/africastalking/`. Rollback is a revert of the
single squash PR — no data migration, no schema, no event-contract change
(`ProviderEvent` is the shared vocabulary this package only produces).
