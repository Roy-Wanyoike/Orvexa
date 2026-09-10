# WhatsApp Cloud API adapter (`internal/messaging/adapters/whatsappcloud`)

Implements the `messaging.MessagingProvider` port for the Meta **WhatsApp
Cloud API** (Graph v21.0) — issue [#27](https://github.com/Roy-Wanyoike/Orvexa/issues/27),
`[O-18]`. WhatsApp is the primary channel for the Nairobi market; this
adapter is the production carrier behind `messaging.ChannelWhatsApp`.

Passes `conformance.RunMessagingConformance` green against an httptest fake
Graph server (see the [conformance kit README](../../comms/conformance/README.md));
an adapter PR that does not pass the kit is not reviewable.

## Layout

| File | Role |
|---|---|
| `provider.go` | `Provider` (port implementation): `Send` (text/media) + `SendTemplate`; config, validation, payload building |
| `client.go` | Minimal Graph HTTP client: Bearer auth, bounded response reads, bounded retries (`RetryPolicy`) |
| `errors.go` | Graph error-code → `pkg/errors` taxonomy, with typed errors (`WindowClosedError`, …) and documented retry verdicts |
| `webhook.go` | Inbound: `TranslateStatus` (pure) + `HandleWebhook` (signed receipt delivery) |
| `ledger.go` | In-memory wamid → (interaction, tenant) correlation for status attribution |

No SDK dependency (stdlib `net/http`); `go.mod` untouched.

## Setup

### 1. Phone number ID + access token

The adapter talks to exactly one WhatsApp Business phone number per
instance. From the [Meta App Dashboard](https://developers.facebook.com/)
→ WhatsApp → API Setup, collect:

- **Phone number ID** — the Graph path segment (`/{phone-number-id}/messages`).
  Not the phone number itself, not the WABA ID.
- **Access token** — a *permanent system-user token* with
  `whatsapp_business_messaging` + `whatsapp_business_management`
  permissions. Temporary dashboard tokens expire in 24h; do not ship one.

Wire them through the provider registry (never in code, logs or flags):

```bash
export ORVEXA_WHATSAPP_PHONE_NUMBER_ID="109876543210987"
export ORVEXA_WHATSAPP_ACCESS_TOKEN="EAAG..."   # registry.Secret — redacted everywhere
```

Two further registry variables complete the deployment (owned by the
webhook gateway, not this adapter): `ORVEXA_WHATSAPP_APP_SECRET`
(verifies Meta's `x-hub-signature-256` at the edge) and
`ORVEXA_WHATSAPP_VERIFY_TOKEN` (the dashboard subscription handshake).

`registry.MessagingConfig` resolves them; `whatsappcloud.New` fails loudly
(`apperrors.Invalid`) at construction when either is missing, when no
`Ingest` hook is wired, or when no `Signer` is configured — receipts must
never be dropped silently or delivered unsigned (the webhook gateway is
fail-closed).

The token lives only in `registry.Secret`: every rendering path
(`String`, `GoString`, `%v`, `%#v`, errors, traces) is redaction-tested.

### 2. Webhook configuration

In the dashboard (WhatsApp → Configuration), point the callback URL at the
platform webhook gateway and subscribe to the `messages` field. Meta signs
every delivery with `x-hub-signature-256` (HMAC-SHA256 over the exact raw
body bytes using the **app secret**); the gateway verifier
(`internal/webhooks.VerifyWhatsAppCloud`, O-15) is fail-closed — only
verified bodies reach `Provider.HandleWebhook`.

The status path is then:

```
Meta ──(signed POST)──▶ gateway: verify x-hub-signature-256 ──▶ Provider.HandleWebhook
    TranslateStatus (pure) ──▶ ledger wamid lookup ──▶ sign ──▶ Ingest ──▶ comms.Processor
```

Translated receipts: `sent → message.sent`, `delivered → message.delivered`,
`read → message.read` (completes the interaction), `failed → message.failed`
(Graph's primary error code + text persisted as `Detail`/`end_reason`).

**Correlation is wamid-scoped.** Statuses carry only the Graph message id;
the adapter resolves them against sends *this process made*. Statuses for
unknown wamids (restart, another instance, another phone number) surface as
`whatsapp.unknown_message_id` and are never attributed to a guessed
interaction. The ledger is in-memory and process-local by design; a
durable/TTL'd mapping (outbox row keyed by wamid) is the production
hardening step for the cmd wiring wave. Multi-number deployments run one
adapter instance per phone number ID.

## The 24-hour customer service window (honest handling)

Meta only delivers **free-form** messages within 24 hours of the customer's
last message to the business number. Outside that window Graph rejects the
send with error code **131047** ("Re-engagement message").

The adapter's contract — deliberate, documented, tested
(`TestWindowClosedErrorSemantics`):

1. **No silent fallback.** A free-form send outside the window returns the
   explicit typed `*WindowClosedError`. Silently converting a caller's
   message into a template would hide a product decision that belongs to
   the caller.
2. **Typed + taxonomy, both reachable.**
   `errors.As(err, *WindowClosedError)` for identity;
   `errors.As(err, *apperrors.Error)` for the machine code
   `whatsapp.window_closed`, kind **conflict** (409 class) — the request is
   well-formed but conflicts with the conversation's state.
3. **Never retried.** Re-sending the same free-form body cannot succeed —
   the window opens only when the *customer* replies. The adapter spends
   zero retry budget on it.
4. **The escape hatch is templates.** The 409's client-safe message says
   so explicitly: send an approved template via `Provider.SendTemplate`.
   Meta delivers template messages outside the window *by design* — that is
   what the window exists to enforce (businesses re-engage through reviewed
   templates, not ad-hoc text).

```go
err := provider.Send(ctx, msg)
var wce *whatsappcloud.WindowClosedError
if errors.As(err, &wce) {
    // 409 whatsapp.window_closed — switch to an approved template:
    provider.SendTemplate(ctx, whatsappcloud.TemplateRequest{ /* ... */ })
}
```

### Template guidance

- Templates must be **approved** in Meta Business Manager before sending;
  the adapter passes name + language + components through as-is.
- `BodyParams` fills the body's `{{1}}`, `{{2}}`, … variables in order;
  `Components` is raw JSON passthrough for header/button/carousel
  components (no builders beyond passthrough — issue #27 out-of-scope note).
- **No template escape hatch for content review:** approval is Meta's
  review process, not ours. A template that renders variables the reviewer
  did not approve will be rejected by Graph (template errors surface via
  the taxonomy below).
- Body text in free-form sends is passed to Graph verbatim. Meta renders
  `*bold*`, `_italic_`, `~strikethrough~`, `` ```mono``` `` and will treat
  them as markup; escape literals as `\_` if needed. Template bodies are
  reviewed and rendered by Meta — positional `{{n}}` parameters are plain
  text substitution, no markup escaping is applied by this adapter.

## Error-code semantics

`mapGraphError` classifies every failed Graph response into the
`pkg/errors` taxonomy. Every mapped error is an `*apperrors.Error` (rendered
by the httpx envelope) or a typed error unwrapping to one — one error, both
surfaces.

| Graph code | HTTP | Meaning | Machine code | Kind | Typed error | Retried? |
|---|---|---|---|---|---|---|
| `131047` | any (often 400) | Free-form send outside the 24h window | `whatsapp.window_closed` | conflict | `*WindowClosedError` | **never** (re-sending cannot succeed) |
| `130429` | 429 | Cloud API rate limit hit | `whatsapp.rate_limited` | rate_limited | `*RateLimitedError` (carries `Retry-After`) | yes, bounded |
| `80007` | 429 | Legacy rate-limit code still observed | `whatsapp.rate_limited` | rate_limited | `*RateLimitedError` | yes, bounded |
| `131026` | 4xx | Message undeliverable (number not on WhatsApp / blocked) | `whatsapp.undeliverable` | invalid | `*UndeliverableError` | **never** (permanent; follow up on the contact) |
| `190` / 401 | 401 | Access token invalid/expired/revoked | `whatsapp.auth_failed` | unauth | `*AuthFailedError` | **never** (rotate the credential; in-process retries cannot fix auth) |
| — | 429 | Rate limit signaled by status alone | `whatsapp.rate_limited` | rate_limited | `*RateLimitedError` | yes, bounded |
| — | 5xx | Provider-side transient (Graph or proxy) | `whatsapp.provider_error` | internal | `*ProviderError` (status preserved) | yes, bounded |
| other 4xx | 4xx | Unknown Graph code | `whatsapp.provider_error` | internal (deliberately conservative) | `*ProviderError` (Graph code preserved) | **never** |

Classification is **code-first**: a 131047 riding on a 429 surfaces the
window error, not the retryable rate-limit error
(`TestWindowClosedFrom429`) — re-sending is futile either way.

**Retry-domain split (load discipline):** the adapter retries ONLY
provider-signaled transient classes (429/5xx/130429/80007) within
`RetryPolicy` (default 3 attempts, 200ms → 2s exponential, `Retry-After`
honored but capped at `MaxDelay`). Transport errors are *never* retried
here — `messaging.Service` owns transport retry, and retrying both layers
would multiply the effective budget beyond the documented bound. Jitter is
deliberately omitted for deterministic tests.

## Idempotency & duplicate receipts

- `Send` with the **same `InteractionID` twice is accepted** (Graph has no
  idempotency key; the core retries `Send` after transport timeouts and
  must not fail real interactions on harmless retries). Each POST yields a
  fresh wamid; both ledger entries resolve to the same interaction.
- Duplicate receipts are harmless: `comms.Processor` no-ops same-state
  deliveries, so re-emitted `sent`/`delivered` receipts apply cleanly.
  Pinned adapter-side (`TestSendDuplicateInteractionIDMatchesSimulatorSemantics`)
  and kit-side (the retry-safety scenario).

## Conformance

`TestWhatsAppCloudMessagingConformance` embeds
`conformance.RunMessagingConformance` against the fake Graph server:

- one `Recorder` wired twice (adapter hook + `Options.Recorder`);
- the fake's async status callbacks relayed FIFO (`statusRelay`) —
  per-message `sent→delivered` is Meta's ordering guarantee, cross-message
  order follows send acceptance;
- `RequireAsyncEvents: true` (receipts arrive via callback, not within
  `Send`), `EnforceSendValidation: true` (the adapter re-validates with the
  core codes verbatim), provider label pinned to `whatsappcloud`.

## Operations notes

- Media sends require **https** links (Meta fetches them server-side);
  credentials-in-URL and plaintext schemes are rejected pre-flight.
- WhatsApp carries **one media object per message**; multi-attachment
  sends are rejected as invalid input (`whatsapp.media_single`) so callers
  model them as separate messages instead of silently losing attachments.
- Default HTTP timeout 15s; response bodies bounded at 64 KiB.
- A 2xx success envelope without a message id fails loudly
  (`whatsapp.malformed_send_response`) — accepting it would orphan the
  interaction's receipt lifecycle.

## Out of scope (issue #27)

- On-premises WhatsApp Business API.
- Interactive button/list builders beyond template passthrough.
- Inbound *message* translation (this adapter owns statuses; inbound
  customer messages ride the same gateway with their own translation wave).
