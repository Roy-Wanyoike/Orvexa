# Twilio Voice Adapter (`internal/telephony/adapters/twilio`)

`telephony.VoiceProvider` over Twilio Programmable Voice — pure `net/http`
against the REST API (v2010-04-01, `.json` flavor), no Twilio SDK. First real
carrier for the voice plane (issue #25); the built-in Simulator remains the
reference behavior and both pass the same conformance kit
(`internal/comms/conformance`).

```
core use-cases ──> telephony.VoiceProvider ──> twilio.Adapter ──HTTP──> Twilio REST (api.twilio.com)
                                                    │
                                                    └── native status callbacks ──> comms.ProviderEvent
                                                        (signed, via comms.IngestFunc → fail-closed
                                                        webhook gateway O-15)
```

## Files

| File | Contents |
|---|---|
| `client.go` | REST transport: basic auth, JSON error envelope → apperrors taxonomy (`classify`), bounded jittered retry (429/5xx/transport only, ≤3 retries, honors bounded `Retry-After`), redactable `Config` |
| `voice.go` | `VoiceProvider` operations: `PlaceCall` / `Hangup` / `Transfer` / `Hold` / `Resume`, leg registry (interaction id → CallSid), per-command validation |
| `events.go` | `TranslateStatusCallback` (native form → `comms.ProviderEvent`), `HandleStatusCallback` / `ServeStatusCallback` (the verified-gateway entry) |
| `fake_test.go` | Injectable fake Twilio: auth wall, scripted REST failures, async callback delivery |
| `voice_test.go` | Lifecycle e2e, error taxonomy, retry, redaction, translation table, `TestTwilioVoiceConformance` |

## Environment / configuration

Credentials come from the comms registry (O-13) — selected with
`ORVEXA_TELEPHONY_PROVIDER=twilio`:

| Env var | Secret | Purpose |
|---|---|---|
| `ORVEXA_TWILIO_ACCOUNT_SID` | yes | Basic-auth username + account scope in the REST paths |
| `ORVEXA_TWILIO_AUTH_TOKEN` | yes | Basic-auth password; also keys the default event signer |
| `ORVEXA_TWILIO_FROM_NUMBER` | yes | Account default origin (the core passes per-call `From`; adapter falls back to nothing) |

Adapter wiring config (`twilio.Config`, supplied by the resolver layer, not
env-parsed today):

| Field | Required for | Meaning |
|---|---|---|
| `TwiMLBaseURL` | `PlaceCall` | The app's TwiML entry document Twilio fetches when the call is answered (per-call override: `ProviderOptions["twilio_url"]`). Must be reachable **from Twilio's network**. |
| `CallbackBaseURL` | `PlaceCall` | Externally routable status-callback URL — in production the platform's fail-closed webhook gateway endpoint. The adapter appends `?interaction=<id>&tenant=<tenant>` so every callback is self-scoping. Must be reachable **from Twilio's network** (a public URL or tunnel in dev; `localhost` only works with the fake). |
| `HoldTwiMLURL` | `Hold` | The looping hold document (`<Pause length="3600"/>` + `<Redirect>` back to itself). Without it `Hold` fails typed (`twilio.hold_not_configured`) rather than faking semantics. |
| `ResumeURL` | `Resume` (optional) | Post-hold continuation document; falls back to the leg's original entry TwiML. |
| `Ingest` | event delivery | `comms.IngestFunc` — production: the verified-event application port behind the gateway; tests: the conformance Recorder. |
| `Signer` | optional | Defaults to HMAC-SHA256 over the body keyed by the auth token (`sha256=<hex>`). |
| `BaseURL`, `HTTPClient`, `RetryBaseDelay` | tests/tuning | REST root override (httptest), transport injection, backoff base. |

`Config` renders with zero credential material (`String`/`GoString` via
`registry.Redacted`) — asserted by `TestConfigRedaction`.

## Capability notes

- **PlaceCall** — `POST /Calls.json` with `To`, `From`, `Url`, `Method=POST`,
  `StatusCallback`, `StatusCallbackMethod=POST`, `StatusCallbackEvent=ringing
  answered completed`, optional `Record=true` (`ProviderOptions["record"]`).
  E.164-ish dial strings only (optional `+`, 3–15 digits — PSTN numbers and
  numeric internal extensions). Tenant context is mandatory
  (`ProviderOptions["tenant_id"]`); a rejected command makes zero carrier
  calls and emits zero events.
- **Hangup** — `POST /Calls/{Sid}.json` with `Status=completed`; the native
  `completed` callback carries `call.ended` (Detail = the native cause,
  persisted as the interaction's `end_reason`).
- **Transfer** — `POST /Calls/{Sid}.json` with
  `Twiml=<Response><Dial><Number>dest</Number></Dial></Response>`. Twilio
  fires **no** status callback for a TwiML redirect (the leg stays
  `in-progress`), so the adapter announces the new ringing phase itself with
  `Detail=transfer:<dest>` — the destination is always auditable.
- **Hold / Resume** — Twilio has no native hold on a live leg. Hold redirects
  the leg to `<Response><Pause length="3600"/><Redirect method="POST">HOLD</Redirect></Response>`
  (a full-hour silent pause looping back to the hold document); Resume
  redirects it away. Both are lifecycle-silent by contract (the platform's
  event vocabulary has no hold semantics). Hold requires `HoldTwiMLURL`.
- **Status callbacks** — `queued`/`ringing` → `call.ringing`,
  `in-progress` → `call.connected`, `completed`/`no-answer`/`busy`/`failed`/
  `canceled` → `call.ended` with the native status as `Detail`. Unknown
  statuses are a no-op (204) — carriers add vocabulary over time and the
  gateway must not fail on it. Unscoped callbacks (no interaction
  passthrough) are rejected `invalid` (`twilio.callback_unscoped`) → 400.
- **Unknown legs** — `Hangup`/`Transfer`/`Hold`/`Resume` on an interaction id
  this process never placed fail typed `not_found` (`twilio.unknown_leg`),
  deterministically, with zero carrier calls and zero events.

## Security

- REST auth: HTTP basic (AccountSID/AuthToken) — credentials never rendered
  (`registry.Secret`).
- Callback verification is **not** the adapter's job: the fail-closed
  webhook gateway (O-15) verifies `X-Twilio-Signature` before the adapter
  sees the request. The adapter signs its own outgoing event deliveries
  (HMAC-SHA256, default signer) so the gateway treats adapter traffic and
  simulator traffic identically.
- Retry policy never retries deterministic 4xx rejections; backoff is
  bounded, jittered, and context-cancellable.

## Conformance

`TestTwilioVoiceConformance` embeds the provider conformance kit
(`RunVoiceConformance`) with `RequireAsyncEvents: true` (native callbacks are
delivered asynchronously by the fake) and `EnforceCommandValidation: true`
(the adapter re-validates commands). Scenarios: place-call lifecycle,
hangup-with-cause, transfer re-ring, silent hold/resume, loud unknown refs,
tenant-context enforcement, full command validation. **Status: PASS** under
`go test -race` (see PR description for the run log).

## Known limitations / risks

- The leg registry (interaction id → CallSid) is process-local; after a
  restart, live legs can no longer be managed (hangup/transfer/hold) until
  the core persists provider refs — event delivery keeps working because the
  StatusCallback URLs are self-scoping.
- `POST /Calls` is not idempotent on Twilio's side; the bounded retry budget
  applies to 429/5xx/transport failures, so a response lost after a
  successful dial could in principle double-place. Twilio-side dedup does
  not exist; accepted for this wave.
- No media/streaming features and no conference/recording APIs beyond the
  basic per-call `Record` flag (issue #25 scope).
