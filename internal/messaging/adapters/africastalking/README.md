# Africa's Talking adapter (SMS + USSD)

`internal/messaging/adapters/africastalking` implements the
`messaging.MessagingProvider` port for the Africa's Talking bulk-SMS API and
a minimal **inbound** USSD session surface. It is tested against an
`httptest` fake carrier (the wire contract below is pinned by
`fake_test.go`), embeds the provider conformance kit for SMS, and emits every
lifecycle receipt as a signed `comms.ProviderEvent` through the same
`IngestFunc` path the built-in Simulator uses — no provider bypasses the
fail-closed webhook gateway.

## Wire contract (SMS)

- **Endpoint**: `POST {BaseURL}/messaging`, where `BaseURL` defaults to
  `https://api.africastalking.com/version1` (the carrier's version-1 API
  root; the issue's `/v1/messaging` shorthand maps to this path).
- **Auth**: the account API key travels **only** in the `apiKey` request
  header — Africa's Talking's real contract. It is *not* a Bearer
  Authorization header (the task shorthand said "Bearer"; the carrier's
  documented header is `apiKey`, and the fake pins that). The key is never
  in request bodies, error strings, logs or test fixtures.
- **Request body** (JSON): `username`, `to`, `message`, `from`.
  - `username` — account username (`ORVEXA_AT_USERNAME`).
  - `to` — the destination, canonicalized to strict E.164 before the carrier
    sees it. Canonicalization **reuses the platform's phone taxonomy**:
    `customers.NormalizeIdentifier(IdentPhone, …)` (trim → drop `00` → add
    `+`), then the adapter enforces the strict international shape
    (`+` + country code with no leading zero, 8–15 digits). Local formats
    (e.g. `07…`) are rejected at the boundary with `at.to_not_e164` —
    the CRM rule canonicalizes, the carrier rule refuses what AT cannot
    route.
  - `message` — the text body. Media-only messages (no body) become their
    newline-joined media URLs as text: SMS cannot carry media, and the
    conformance kit requires media-only messages to drive the same
    lifecycle rather than fork it. The 4096-rune core limit is re-checked
    after the join.
  - `from` — the **sender ID**: the registry sender ID
    (`ORVEXA_AT_SENDER_ID`), overridden by a per-message `From` when
    present. The adapter passes it through verbatim; sender IDs must be
    registered/approved with Africa's Talking in advance (request one via
    the AT portal or your account manager — unregistered sender IDs are
    rejected synchronously by the carrier). The adapter cannot register
    sender IDs and does not try.
- **Response** (JSON): `SMSMessageData.Recipients[]` with `statusCode`,
  `status`, `messageId`. Accepted sync statuses: `100` (Processed), `101`
  (Sent), `102` (Delivered) or the literal `"Success"`. Anything else is a
  synchronous rejection (`at.recipient_rejected`) and fails the send.
- **Retries**: bounded budget (default 2 retries after the first attempt).
  429 and 5xx responses and transport errors are retried with exponential
  backoff (25 ms base, 2 s cap, honoring a bounded `Retry-After` seconds
  header). Other 4xx fail immediately. There is no jitter — determinism
  keeps the fake-server tests exact.
- **Idempotency**: a repeated `InteractionID` never errors (Simulator
  semantics), but an already-accepted send is **never re-POSTed** — silent
  double billing is worse than a suppressed receipt. The table is in-memory
  and bounded (oldest completed entries evicted); durable dedup lives
  upstream (webhook-gateway event dedup + the processor's same-state no-op).

## Receipt mapping (SMS)

| Carrier signal | ProviderEvent |
|---|---|
| synchronous acceptance | `message.sent` |
| sync status `102`/`"Delivered"` (networks that confirm inline) | `message.delivered` (same round trip) |
| delivery report `status` `Success`/`Delivered` | `message.delivered` |
| delivery report `status` `Failed`/`Rejected`/`Discarded`/`Expired`/`Cancelled`/`Undelivered` | `message.failed` (`Detail` = `failureReason`, else the status) |
| delivery report intermediate status (`Sent`, `Enroute`, …) or unknown | acknowledged, **no event** — never guessed |
| sync recipient rejection | send error (`at.recipient_rejected`); `message.failed` is reserved for delivery reports |

`TranslateDelivery(ctx, contentType, body)` accepts AT's documented JSON
report (`id`, `status`, `failureReason`) and a tolerant form-encoded variant
(same keys, urlencoded — some gateways forward that shape); the content type
is honored, and absent/unknown types are sniffed (JSON first, then form).
Reports keying on an unknown carrier `messageId` fail as `not_found`
(`at.unknown_message_id`): receipts may never materialize state for messages
this platform never sent. **The caller must verify callback origin first**
— Africa's Talking signs nothing; the only server-side origin control is the
peer-IP allowlist implemented by `internal/webhooks.VerifyAfricasTalking`
(fail-closed when configured, structured warning when not). The adapter
trusts that gate and re-stamps the platform HMAC on every emitted event.

## USSD surface (inbound only)

Africa's Talking USSD is operator-initiated: the user dials the service
code, AT calls **your** callback with the session state, you answer with a
plain-text response, and AT renders it as the next screen. The adapter
exposes exactly one translation entry point:

```go
wire, err := adapter.TranslateUssdCallback(ctx, rawCallbackBody)
// wire is what the gateway writes back: "CON <menu>" or "END <message>"
```

- **Payload**: form-encoded fields (or JSON with the same keys / a
  query-string legacy GET): `sessionId`, `serviceCode`, `phoneNumber`,
  `text`, `networkCode`. `text` is the cumulative user input (`""` on the
  bare dial).
- **Handler**: `Config.UssdHandler` builds the menu. It receives the parsed
  `UssdCallback` (with the resolved `Phase`: started/continued) and returns
  a `UssdReply`. **Menu text is passed through verbatim** — the adapter
  prefixes `CON `/`END ` per `End` (text already carrying those prefixes is
  passed through untouched) and never reflows, truncates or re-encodes. An
  empty (or whitespace-only) reply is a handler bug, never a valid carrier
  answer: it is rejected with `ussd.empty_reply` and the session reservation
  is rewound, so the carrier's retry starts fresh.
- **sessionId↔interaction-id convention**: the platform adopts the carrier
  `sessionId` as the interaction identity of the USSD leg. Emitted events
  carry `sessionId` as `InteractionID` and `Config.TenantID` as `TenantID`
  (one AT account serves one tenant; inbound sessions have no per-message
  tenant). The adapter never mints platform ids.
- **Events** (schema unchanged — `Event` is a string):
  - first callback of a session → `ussd.session.started` (`Detail` = the
    service code),
  - subsequent callback → `ussd.session.continued`,
  - handler ends the session → `ussd.session.ended` (after the phase event).
  - *Integration gap, stated honestly:* `comms.Processor`'s event
    vocabulary models `call.*`/`message.*` only (and requires UUID
    interaction ids), so until the processor grows the `ussd.session.*`
    family these deliveries are recorded by the gateway but not applied to
    interaction lifecycles. Extending that vocabulary is out of this
    adapter's file ownership; the adapter never fabricates ids or tenants to
    force events through.
- **Retry idempotency**: AT retries callbacks it received no response for.
  A callback for a session already answered with `END` replays the **same**
  wire text, emits nothing and does **not** re-run the handler (no double
  side effects). A handler failure rewinds the session reservation so the
  carrier's retry starts fresh.

### Documented AT USSD limits (honored, not hidden)

- **Text-only.** No media, no formatting, no pagination: one screen of
  plain text per round trip, encoded by the carrier. The adapter passes
  menu text through verbatim — **the handler owns menu sizing** within AT's
  per-screen character budget (keep entry screens short; long text renders
  unpredictably across handsets).
- **One session at a time per subscriber.** A user has a single live USSD
  session per operator; the adapter tracks session phase per `sessionId`
  (bounded in-memory table — sessions are carrier-enforced short-lived).
  Sessions this process forgot (restart, eviction) re-enter as `started`;
  that re-labels at most one phase event and is lifecycle-safe downstream.
- **No explicit hangup signal.** AT never notifies you that the user
  abandoned a session mid-menu: abandoned sessions simply stop calling back.
  The adapter therefore emits `ussd.session.ended` only when the *handler*
  ends the session (`END …`), never on abandonment — and does not guess.
- **No app-initiated sessions.** The carrier's session-push endpoint is not
  implemented: standard AT accounts are callback-driven, and pretending
  otherwise would be a wire-contract fiction. Outbound USSD "push" is a
  special carrier arrangement, tracked separately if ever needed.
- **No state guarantees between callbacks.** Sessions are tracked
  per-process; durable session state belongs to the platform's flow engine,
  not to a carrier adapter.

## Configuration

| Config field | Env (via registry, fail-closed at boot) |
|---|---|
| `Username` | `ORVEXA_AT_USERNAME` |
| `APIKey` (header only) | `ORVEXA_AT_API_KEY` |
| `SenderID` | `ORVEXA_AT_SENDER_ID` |
| `TenantID` | wiring (per-tenant account mapping) |

Credentials are `registry.Secret` values: `String`, `GoString`, `LogValue`
and `MarshalJSON` all render redacted, and tests assert the API key appears
in exactly one place — the `apiKey` request header — across sends, error
paths, logs and `%#v` renderings.

## Conformance

`sms_test.go` embeds `conformance.RunMessagingConformance` (the kit's
reference is the built-in Simulator) with `EnforceSendValidation: true` —
the adapter re-validates every message through `messaging.ValidateMessage`
and must carry the exact core codes. The kit runs against the fake carrier
answering with sync status `102`, which is a real AT response (inline
delivery confirmation), so the sync contract (`sent` → `delivered` by the
time `Send` returns) holds without pretending every network delivers
inline: the delivery-path tests cover the `101` + async-report flow.

## Test map

| File | Covers |
|---|---|
| `fake_test.go` | The `httptest` fake carrier (`/messaging`, `apiKey` header auth, scriptable statuses, `Retry-After`), the credential fixture, the Recorder seeding helper, and the raw `capture` hook the USSD tests assert through |
| `sms_test.go` | Conformance kit wiring, sync/async delivery lifecycles → `ProviderEvent`, delivery-report status table, guard taxonomy, bounded retry budget (429/5xx/`Retry-After`), duplicate-send suppression + concurrent single-flight, strict E.164 boundary table, zero credential/PII leakage sweep |
| `ussd_test.go` | Callback parsing table (form/JSON/malformed), session lifecycle events (`started`/`continued`/`ended`), `CON`/`END` wire passthrough, ended-session retry idempotency, handler-failure rewind, best-effort emission posture, concurrent-session race probe |

The USSD tests observe events through the raw capture hook (delivery layer)
because `comms.Processor` does not apply the `ussd.session.*` vocabulary
yet — the integration gap documented above. The SMS tests observe events
through the conformance Recorder (processor applied), which is the
authoritative path.
