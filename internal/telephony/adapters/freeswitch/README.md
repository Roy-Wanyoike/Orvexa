# FreeSWITCH ESL voice adapter

`telephony.VoiceProvider` over the FreeSWITCH **Event Socket Layer**
(`mod_event_socket`, default TCP port 8021). The ESL text protocol is
implemented by hand — stdlib `net` + `bufio` only, no third-party ESL
dependency — with the wire format pinned by golden tests and every
behavioral scenario exercised against an in-package fake switch.

```
                ESL (TCP 8021)                      signed webhook (in-process)
Orvexa core ──▶ Adapter ──▶ eslClient ──▶ FreeSWITCH      Adapter ──▶ comms.IngestFunc
              PlaceCall…   api / bgapi     mod_event_socket ◀── text/event-plain → ProviderEvent
```

## Port mapping

| Port call | ESL command | Result path |
|---|---|---|
| `PlaceCall` | `bgapi originate {origination_uuid=<interaction id>}<to> &bridge(<from>)` | command/reply carries `Job-UUID`; dial result arrives as `BACKGROUND_JOB`; answer as `CHANNEL_ANSWER` |
| `Hangup` | `api uuid_kill <uuid>` | the leg's `CHANNEL_HANGUP` (not this reply) ends the interaction with the switch cause |
| `Transfer` | `api uuid_transfer <uuid> <dialplan> <context> <dest>` | success re-rings the leg: `call.ringing` with `Detail: "transfer:<dest>"` |
| `Hold` | `api uuid_hold on <uuid>` | lifecycle-silent (media operation) |
| `Resume` | `api uuid_hold off <uuid>` | lifecycle-silent (media operation) |

Event translation (`events plain CHANNEL_ANSWER CHANNEL_HANGUP BACKGROUND_JOB`):

| ESL event | ProviderEvent | Notes |
|---|---|---|
| `BACKGROUND_JOB` (result `+OK <uuid>`) | `call.ringing` | the switch accepted the dial |
| `BACKGROUND_JOB` (result `-ERR <cause>`) | `call.failed` (`Detail` = cause) | leg retired; later ops are `not_found` |
| `CHANNEL_ANSWER` (`Unique-ID` = tracked leg) | `call.connected` | |
| `CHANNEL_HANGUP` (`Unique-ID` = tracked leg) | `call.ended` (`Detail` = `Hangup-Cause`, `UNKNOWN` fallback) | leg retired |

## Correlation contract: `origination_uuid` = interaction id

The platform's interaction id is carried as the FreeSWITCH channel variable
`origination_uuid`, which FreeSWITCH copies into the channel's `Unique-ID`.
Every `CHANNEL_*` event for the leg therefore carries the platform reference
natively — no side table, no state to reconcile after a reconnect. The
in-flight `bgapi` job is additionally correlated by its `Job-UUID`
(header first, `+OK Job-UUID: <uuid>` reply-text fallback, and a
payload-based fallback for jobs whose mapping was lost to a reconnect).

**Dialplan assumptions (operator contract):**

- Do **not** strip channel variables on the A-leg: an originate without
  `origination_uuid` cannot be correlated and its events are ignored.
- `To`/`From` must be **single-token dial strings** — no whitespace, and no
  `&`, `(`, `)`, `,` (they terminate/separate originate arguments and would
  corrupt the command line). Valid: `1001`, `user/1001`,
  `loopback/+2547…/default`, `sofia/gateway/pstn/+2547…`. The adapter
  rejects anything else as `invalid` before any wire traffic.
- `PlaceCall` is click-to-call shaped: dial `<to>`, then inline-bridge the
  answered leg to `<from>` (`&bridge(<from>)`). One-way notification dials
  should point `To` at a playback-style target instead.
- `Transfer` routes through the configured dialplan/context (defaults
  `XML`/`default`, override via `TransferDialplan`/`TransferContext`); the
  destination must be routable there or the switch answers `-ERR`.

## Eventing assumptions

- The subscription is exactly `events plain CHANNEL_ANSWER CHANNEL_HANGUP
  BACKGROUND_JOB` and is re-sent on every fresh connection (reconnect
  resumes the stream without caller involvement).
- **No `CHANNEL_PROGRESS`/`CHANNEL_RINGING` subscription, by design:** the
  adapter only translates state for legs *it* placed (tracked in a
  process-local leg registry keyed by origination uuid). Generic channel
  chatter — inbound calls, other integrations sharing the socket — is
  ignored, so the platform never observes state for interactions it does not
  own. The first ringing phase comes from the correlated `BACKGROUND_JOB`.
- Events that arrive between disconnect and re-subscribe are lost (the
  switch does not buffer for unsubscribed clients). The lifecycle processor
  is idempotent and the leg registry keeps the leg operable, but a hangup
  that lands inside that window is observed only via the rebuilt stream or
  as `not_found` on a later command.

## Configuration

Registry env (see `internal/comms/registry/telephony.go`):

| Env | Meaning |
|---|---|
| `ORVEXA_TELEPHONY_PROVIDER=freeswitch` | select this adapter |
| `ORVEXA_FREESWITCH_HOST` | mod_event_socket host (required) |
| `ORVEXA_FREESWITCH_PORT` | default `8021` |
| `ORVEXA_FREESWITCH_PASSWORD` | ESL password — **secret-classified, redacted in every log/error rendering** |

Adapter `Config` (programmatic): `Addr`, `Password` (`registry.Secret`),
`Ingest` (required — the signed-webhook delivery port), `Signer`
(platform HMAC signer in production; the SHA-256 default exists so an
unwired signer cannot produce empty signatures silently), `Logger`,
`TransferDialplan`/`TransferContext`, and the timeouts below (zero = default).

| Tuning | Default | Meaning |
|---|---|---|
| `DialTimeout` | 5s | TCP dial budget |
| `HandshakeTimeout` | 10s | auth handshake budget |
| `CommandTimeout` | 10s | per-command budget when the caller passes no deadline |
| `IdleTimeout` | 60s | read deadline between frames (half-open detection) |
| `BackoffBase` / `BackoffMax` | 50ms / 2s | reconnect backoff ladder |

## Concurrency, failures, reconnect

- **Commands are single-flight** (one in flight at a time): synchronous
  `api` replies carry no correlation id on the wire, so two overlapping
  commands could steal each other's reply. Events stream continuously.
- A command that outlives its deadline **forces a connection rebuild**
  rather than trusting a late reply. The next command waits for readiness
  (bounded by its own context/deadline) on the rebuilt session.
- Reconnect is automatic with bounded backoff; the ladder resets only after
  a session is *genuinely established* (auth + subscription), so a peer that
  accepts TCP but rejects credentials backs off like a dead one.
- An auth rejection surfaces as `unauthorized` (operator-fixable); a lost
  connection or deadline as `internal` (retriable at the caller's
  discretion). `-ERR` on a live command: "no such channel" → `not_found`
  (the leg is retired — no event will end it); anything else → `internal`.

### Error taxonomy (machine codes)

| Code | Kind | Trigger |
|---|---|---|
| `freeswitch.addr_required` / `.ingest_required` | invalid | misconfigured `Config` |
| `freeswitch.tenant_required` | invalid | `PlaceCall` without `tenant_id` |
| `freeswitch.interaction_id_required` / `.interaction_id_invalid` | invalid | interaction id must be a UUID (it becomes the origination uuid) |
| `freeswitch.destination_required` / `.origin_required` / `.dial_string_invalid` | invalid | dial-string policy above |
| `freeswitch.originate_rejected` / `.job_uuid_missing` | internal | switch refused the dial / reply lost its correlation handle |
| `freeswitch.command_rejected` | internal | `-ERR` on a live command (non "no such channel") |
| `freeswitch.leg_not_found` / `.unknown_leg` | not_found | switch lost the channel / reference never placed |
| `freeswitch.auth_rejected` | unauthorized | ESL credentials rejected |
| `freeswitch.command_failed` | internal | transport failure (lost conn, deadline) |

## Security

- The ESL password is accepted as `registry.Secret` and is never rendered
  raw by any logging or error path of this package.
- Stock `mod_event_socket` authenticates with a plaintext `auth <password>`
  line. When the endpoint advertises a `Challenge` header the client uses
  `auth md5:<hex>` (MD5 of password+challenge) — a hardening for deployments
  that cannot put TLS in front of ESL. ESL itself has no transport
  encryption: run it on a trusted/loopback network or front it with a TLS
  terminator.
- Lifecycle events are delivered only through the signed-webhook port
  (`Ingest` + `Signer`) — the same fail-closed gateway path as hosted
  carriers; the adapter never writes lifecycle state directly.

## Testing

- `frame_test.go` — golden wire tests: exact bytes for handshake frames,
  command lines, `Job-UUID` replies, api responses and event bodies.
- `testdial_test.go` — the in-package fake switch (`_test` only, never
  shipped): wire-faithful handshake (plaintext + challenge), `events plain`
  registration, `bgapi originate` jobs with async `BACKGROUND_JOB` +
  `CHANNEL_ANSWER` injection, `uuid_kill/transfer/hold` with `-ERR no such
  channel` paths, reply stalls, and `restart()` (switch reboot).
- `conn_test.go` — handshake variants, auth rejection, command round-trips,
  event delivery, switch-reboot reconnect, command-timeout forced rebuild.
- `voice_test.go` — `conformance.RunVoiceConformance` (with
  `EnforceCommandValidation`), exact originate wire format + Job-UUID
  correlation, failed-dial correlation, typed `-ERR` taxonomy, and the
  mid-call switch-reboot reconnect scenario.

```
go test -race ./internal/telephony/adapters/freeswitch/...
```

## Out of scope

- No `mod_verto` overrides, no audio/media bridging logic (media is the
  switch's job; this adapter owns signaling only).
- Single in-flight command per session — sufficient for the platform's dial
  volume by design; the reconnect/timeout semantics above are the
  documented behavior under load, not a bug to "fix" with pipelining.
