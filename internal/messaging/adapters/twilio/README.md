# Twilio SMS adapter

`internal/messaging/adapters/twilio` implements the
`messaging.MessagingProvider` port against the Twilio Programmable Messaging
REST API. SMS is first-class; MMS media travels as `MediaUrl` parameters
without further semantics. No SDK dependency — the wire protocol is form
POSTs over `net/http`.

Exclusive scope of issue #26. Peer adapters (WhatsApp Cloud, Africa's
Talking, telephony) live in their own packages and must not be touched from
here.

## Send path

One POST to `{APIBaseURL}/2010-04-01/Accounts/{AccountSID}/Messages.json`,
form-encoded:

| Parameter | Value |
|---|---|
| `From` | E.164 sender — the message's `From` or the configured default (`twilio.from_required` if neither) |
| `To` | E.164 destination, normalized with the platform's canonical helper (`internal/customers.NormalizeIdentifier`) so the adapter and the customer identity layer can never disagree |
| `Body` | verbatim body (omitted for media-only messages) |
| `MediaUrl` | repeated per media URL; must be http(s) (`twilio.invalid_media`) |
| `StatusCallback` | the configured `StatusCallbackBase` plus `?interaction_id=…&tenant_id=…` routing keys |

Authentication is HTTP basic (AccountSID as username, AuthToken as
password). The endpoint URL embeds the account SID; transport errors are
sanitized (`twilio.transport_error` carries a `[redacted]` endpoint) so the
SID can never reach a log line.

## Receipt path (status callbacks)

Twilio reports delivery asynchronously by POSTing form-encoded status
callbacks to the `StatusCallback` URL. The routing keys ride the callback
URL's query string (Twilio does not echo them in the POST body); after
`r.ParseForm()` the query and body are merged, which is exactly the
`url.Values` `TranslateStatus` expects. `MergeCallbackForm` is available for
callers holding the two views separately (POST fields win collisions; inputs
are never mutated).

Translation — the closed vocabulary `comms.Processor` understands:

| Twilio `MessageStatus` (or legacy `SmsStatus`) | ProviderEvent | Detail |
|---|---|---|
| `queued`, `sent` | `message.sent` | — |
| `delivered` | `message.delivered` | — |
| `undelivered`, `failed` | `message.failed` | failure reason, see below |
| anything else (`accepted`, `scheduled`, `canceled`, `read`, …) | **rejected** `comms.unknown_event` | — |

Unsupported statuses fail loudly instead of being guessed: the platform
ethos is surfaced, not swallowed. The `queued`→`message.sent` and
`sent`→`message.sent` duplicates are absorbed by the processor's same-state
no-op.

Failure reasons on `message.failed` (persisted as the interaction
`end_reason`), in order: `twilio <ErrorCode>: <ErrorMessage>`, then the code
alone, then the message alone, then the bare status — a failure event never
carries an empty reason. Twilio status callbacks carry no timestamp, so
`ProviderEvent.Timestamp` is left empty and `Encode` stamps arrival time
(documented approximation).

Two delivery paths, one translation:

1. **Production:** Twilio → webhook gateway, which verifies the
   `X-Twilio-Signature` fail-closed (`internal/webhooks.VerifyTwilio`) and
   then calls `TranslateStatus`. Signature verification NEVER lives in this
   package.
2. **In-process** (tests, wiring that holds an adapter handle):
   `HandleStatusCallback` translates and delivers a signed
   `comms.ProviderEvent` through the `comms.IngestFunc` port (the same hook
   the built-in Simulator uses), labelled `twilio`. It performs no signature
   verification and must never sit on a network path without the gateway in
   front of it.

## Segmentation honesty (read before claiming anything about message length)

**Orvexa does not segment, count segments, or claim segment counts.** The
body travels verbatim in the `Body` parameter; encoding and segmentation are
decided by Twilio and the carrier. Concretely, for the two SMS encodings:

- **GSM-7** (Latin scripts): a single segment carries 160 septets. Once
  concatenation kicks in, a User Data Header consumes space in every part,
  leaving **153 septets per part** — a 306-character body is 2 segments, 459
  is 3, `n` parts carry at most `153·n`.
- **UCS-2** (anything outside GSM-7: emoji, most non-Latin scripts): a
  single segment carries **70 characters**; concatenated parts carry **67**
  — `n` parts carry at most `67·n`. One emoji in the body can therefore turn
  what "looked like" 3 GSM-7 segments into 5 UCS-2 segments.
- Even within GSM-7, extension-table characters (`[`, `]`, `{`, `}`, `~`,
  `^`, `|`, `\`, `€`) count as **two** septets each, so a "160-character"
  body can spill into a second segment.

What this package deliberately does NOT do, and what no code or doc may
claim:

- **No silent concat.** We never claim a long body arrives as one SMS, never
  pre-split a body into multiple sends to simulate segments, and never
  reassemble anything. Segment counts are the carrier's billing truth; if a
  caller needs them, Twilio reports them (`NumSegments` on the status
  callback) and the platform may surface them — this adapter consumes only
  the status itself.
- **No silent truncation.** The platform gate (`messaging.ValidateMessage`)
  allows bodies up to **4096 runes, counted in runes, not bytes** (4096
  multibyte characters are legal). Twilio's SMS API caps `Body` at 1600
  characters: a body beyond that is rejected by Twilio with a **terminal
  4xx** — `twilio.request_rejected` with Twilio's error code and bounded
  message in the error details. The send fails loudly at the provider
  boundary; nothing is shrunk, split or swallowed on the way there.
- **No encoding promises.** We never promise GSM-7 delivery for a body;
  encoding downgrades and their billing effects are Twilio-side decisions.

Operational guidance: keep outbound SMS bodies within Twilio's 1600-character
`Body` limit and remember the UCS-2 cliff (70/67) when content includes
emoji or non-Latin scripts; the platform's 4096-rune cap is a messaging-plane
ceiling, not an SMS-segment guarantee.

## Retry posture

- **Bounded retries ≤3** (configurable 0–3, clamped) on **429 and 5xx only**,
  with full-jittered exponential backoff (base 250ms, ceiling 2s by default)
  so concurrent senders do not synchronize. Explicit `Retry-After` guidance
  is honored exactly, clamped to the ceiling.
- **4xx is terminal** — one attempt, mapped error, no sleep.
- **Transport errors are not retried here.** The messaging core retries
  `Send` under its own duplicate-safe contract; the adapter's error is
  sanitized (no account SID in the chain).

## Idempotency (double-billing guard)

The adapter claims `InteractionID` before the POST. A repeated InteractionID
is a silent no-op (Simulator semantics — the core may re-invoke `Send` after
a transport timeout, and a second real carrier POST would double-bill the
customer). The claim is released only when every attempt failed, so a
genuine retry after a hard outage can proceed.

## Error taxonomy (send path)

Every failure is an `*apperrors.Error` with a stable machine code; provider
text reaches error details only, bounded (256 chars), never the URL,
credentials or account SID.

| Condition | Code | Kind | Retried? |
|---|---|---|---|
| 429 | `twilio.rate_limited` | rate_limited | yes (≤3, jittered / Retry-After) |
| 5xx | `twilio.upstream_error` | internal | yes (≤3) |
| 401 | `twilio.auth_failed` | unauth | no |
| 403 | `twilio.forbidden` | forbidden | no |
| 404 / 410 | `twilio.not_found` | not_found | no |
| any other 4xx | `twilio.request_rejected` | invalid | no |
| transport | `twilio.transport_error` | internal | no (core retries) |
| invalid input | `twilio.from_required`, `twilio.invalid_phone`, `twilio.invalid_media`, `twilio.invalid_status_callback_base` | invalid | no |

Configuration failures speak early: missing credentials
(`twilio.credentials_required`) and an `Ingest`/`Signer` wiring mismatch
(`twilio.ingest_not_wired`) are rejected in `New`, before any wire traffic.

## Credentials & redaction

Credential fields (`AccountSID`, `AuthToken`, `FromNumber`) are
`registry.Secret` values: every rendering path on the secret itself
(`fmt` Stringer/GoStringer, `log/slog` LogValuer, `encoding/json`) exposes
only the masked form (`****`, or `****last4` for values ≥12 bytes).

Because fmt refuses to invoke Stringers on values reached through
UNEXPORTED fields, both `Config` and `Adapter` render THEMSELVES redacted
on every top-level path (`String`, `GoString`, `LogValue`, `MarshalJSON`).
The redaction test pins `%v`, `%+v`, `%#v`, slog and JSON renderings of the
config and adapter, plus `cfg.String()` directly: no rendering may contain
raw credential material. Adversarially tested — including transport errors
and provider error details.

Environment (via the O-13 registry): `ORVEXA_TWILIO_ACCOUNT_SID`,
`ORVEXA_TWILIO_AUTH_TOKEN`, `ORVEXA_TWILIO_FROM_NUMBER`; optional overrides
for the API base (tests) and the status-callback base (production receipts
need this — without it sends succeed but are fire-and-forget, which is the
documented posture, not a default to be silently relied on).

## Testing

- `go test -race ./internal/messaging/adapters/twilio/` — unit + wire tests
  against an `httptest` fake that plays scripted API responses (429/5xx/4xx
  sequences, Retry-After guidance, Twilio error payloads) and simulates
  Twilio's async status callbacks over real HTTP.
- Full-path receipt proof: send → fake API 201 → async callback POSTs →
  signed `comms.ProviderEvent` → interaction lifecycle (`message.sent` →
  `message.delivered`, tenant-scoped, interaction stays active).
- Conformance: `TestTwilioSMSConformance` embeds
  `internal/comms/conformance.RunMessagingConformance` (see that package's
  README) with `RequireAsyncEvents` and `EnforceSendValidation` — the
  adapter re-validates with the core gate and must carry the EXACT core
  codes.
- Redaction tests; terminal-error table (400/401/403/404/410/409/422 →
  apperrors kinds/codes, no retry, no sleeps); bounded-retry and
  Retry-After tests with a sleep seam; duplicate-send double-billing guard;
  E.164 normalization reuse.

## Out of scope (issue #26)

MMS semantics beyond `MediaUrl` pass-through; shortcode logic; any SDK
dependency; signature verification (belongs to the webhook gateway).
