# Asterisk AMI adapter (`internal/telephony/adapters/asterisk`)

Implements the `telephony.VoiceProvider` port over the **Asterisk Manager
Interface (AMI)** — issue [#31](https://github.com/Roy-Wanyoike/Orvexa/issues/31),
`[O-22]`. Asterisk is the most deployed self-hosted PBX; this adapter makes
Orvexa deployable onto the existing estate with no intermediary trunk
service: the platform speaks AMI (TCP, default port 5038) directly.

Passes `conformance.RunVoiceConformance` green against a fake AMI server
that parses client traffic with the production codec (see the
[conformance kit README](../../comms/conformance/README.md)); an adapter PR
that does not pass the kit is not reviewable.

## Layout

| File | Role |
|---|---|
| `ami.go` | Wire codec: `Key: value` header blocks framed by blank lines (`\r\n\r\n`, bare `\n` tolerated), protocol bounds, request encoder rejecting CR/LF/NUL injection, Challenge→MD5 key derivation |
| `conn.go` | AMI session: dial + banner + login handshake, single reader goroutine fanning replies to per-ActionID waiters and events to the adapter, mutex-guarded writer, per-action budgets, keepalive, bounded-backoff reconnect supervisor, `Close` |
| `voice.go` | Voice plane commands: `PlaceCall`/`Hangup`/`Transfer`/`Hold`/`Resume` → Originate/Hangup/Redirect; per-interaction leg tracking |
| `events.go` | Voice plane observations: Newstate/Hangup → signed `comms.ProviderEvent` deliveries in wire order |
| `config.go` | `Config` (defaults, redacting `String`), `FromRegistry` (validated registry entry → config) |

No SDK dependency (stdlib `net`); `go.mod` untouched.

## PBX setup

### 1. `manager.conf` — the AMI manager account

Create a dedicated manager user (do not reuse `admin`). In
`/etc/asterisk/manager.conf` (or `manager.d/orvexa.conf` on distributions
that fragment the file):

```ini
[general]
enabled = yes
webenabled = no             ; Orvexa uses raw AMI, not the HTTP manager
bindaddr = 127.0.0.1        ; see "Network exposure" below before changing
port = 5038

[orvexa]
secret = <generate-a-long-random-secret>
deny = 0.0.0.0/0.0.0.0
permit = 127.0.0.1/32       ; the Orvexa host's IP when not co-located
read  = call,system,cdr     ; call is what drives Newstate/Hangup
write = call,system,originate,reporting
```

- `read = call` is required: without it the PBX emits no channel events and
  the platform never learns a call rang, connected or ended.
- `write = originate` covers Originate; `Redirect` and `Hangup` fall under
  `call`.
- The account is used with the **Challenge→MD5 login** (`AuthType: MD5`):
  the raw secret never crosses the wire after the connect banner — only
  `MD5(secret + server-challenge)` does. Keep the secret long and random
  anyway; MD5 is the protocol's choice, not a strength claim.

### 2. Dialplan — one context, one music-on-hold context

The adapter never generates dialplan. It dials and redirects into two
contexts that the PBX administrator provisions:

- **`DialContext` (default `from-internal`)** — the context calls are placed
  into (`Local/<destination>@<context>`), and the context transfers re-enter.
  Any context that routes the destination works; an internal-context-style
  dialplan with an unauthenticated `Dial(PJSIP/<dest>)` pattern is typical.
- **`MOHContext` (default `orvexa-hold`)** — the context Hold redirects the
  customer leg into. It must exist and play music:

```ini
[orvexa-hold]
exten => s,1,Answer()
 same => n,MusicOnHold()
```

### 3. Environment / registry

Selected with `ORVEXA_TELEPHONY_PROVIDER=asterisk`. Credentials flow through
the provider registry (never in code, logs or flags):

| Variable | Required | Meaning |
|---|---|---|
| `ORVEXA_ASTERISK_HOST` | yes | AMI TCP host (the `bindaddr` side) |
| `ORVEXA_ASTERISK_PORT` | no | Defaults to `5038` |
| `ORVEXA_ASTERISK_USERNAME` | yes | The `manager.conf` section name; `registry.Secret` (redacted everywhere) |
| `ORVEXA_ASTERISK_SECRET` | yes | The manager secret; `registry.Secret` (redacted everywhere) |

The adapter additionally requires, at construction, a delivery hook
(`Ingest`) and a signer (`Signer`) — events are reported through the
platform's fail-closed signed-webhook path, and an adapter wired without
them would silently drop the call lifecycle. `New` fails loudly
(`apperrors.Invalid`) instead of half-wiring.

## Capability matrix

| Platform capability | AMI translation | Notes |
|---|---|---|
| `PlaceCall` | `Action: Originate`, `Async: true`, `ActionID = interaction id`, `Channel: Local/<to>@<DialContext>` | The PBX owns how the destination is dialed; `Timeout` header carries `Config.OriginateTimeout` (ms) |
| `Hangup` | `Action: Hangup` on the tracked channel | The resulting `Hangup` event (not the response) moves the interaction to wrapup with the PBX cause |
| `Transfer` | `Action: Redirect` into `DialContext` at the destination | Blind transfer; the destination ringing is delivered as a **new `call.ringing` phase** whose `Detail` names the destination |
| `Hold` | `Action: Redirect` into `MOHContext`, exten `s` | See "The hold pattern, honestly" below |
| `Resume` | `Action: Redirect` back to the leg's current destination in `DialContext` | The inverse redirect; lifecycle-silent |
| Lifecycle events | `Newstate` (`Ringing`/`4` → `call.ringing`, `Up`/`6` → `call.connected`), `Hangup` (`Cause`/`Cause-txt` → `call.ended` detail) | Correlated per interaction via `Uniqueid` → channel name → `ActionID` echo |
| Unmodeled events | consumed, never delivered | The event vocabulary is closed; foreign channels produce no deliveries |

Not supported (deliberately): **ARI** (the REST interface is future work —
AMI is the deployment-neutral lowest common denominator), dialplan
generation,attended-transfer bridging (the Redirect pattern is blind;
`Bridge`-based supervised transfer is ARI territory), per-leg media
operations beyond hold/resume.

### The hold pattern, honestly

AMI predates Asterisk's modern bridging APIs and has no native
"hold this channel" manager action. The classic pattern — used here — is a
`Redirect` of the customer channel into a music-on-hold context. It works
everywhere AMI works, but its semantics are redirect semantics, and this
README would rather document them than overstate the feature:

- **It tears down the current bridge.** The customer leg is pulled out of
  its conversation with the agent and parked in `MOHContext`. The agent side
  hears the far party park (dialplan-dependent); reuniting the parties is
  **not** what Resume does.
- **Resume re-enters the dialplan**, it does not re-bridge the original
  bridge. The customer leg is redirected back to the destination the call
  was parked on (`Config.DialContext` at the leg's current exten). Whether
  the original agent reconnects depends on the dialplan (e.g. a
  `Dial(Local/<agent-exten>)` pattern), not on the platform.
- **Hold/Resume are lifecycle-silent by design**: the platform's event
  vocabulary has no hold semantics, and inventing one (worst case a
  `call.ended`) would wrap up a live call mid-hold. The conformance kit
  asserts this silence.
- Deployments needing true bridge-preserving hold (or attended transfer)
  should drive that through ARI when that adapter lands.

## Session behavior

- **Handshake**: connect banner → `Challenge (AuthType: MD5)` → `Login`
  with `Key = MD5(secret + challenge)`. A missing banner is tolerated
  (gateway drift); a rejected Challenge/Login surfaces as
  `asterisk.auth_failed` (unauthorized — a configuration error, not a
  transport fault) and is reported as the session's last fatal error until
  a session becomes ready again.
- **Correlation**: every action carries an `ActionID`. The Originate uses
  the platform interaction id (`telephony.ProviderRef` convention); internal
  actions mint `ovx-*` ids. Unsolicited or foreign replies are ignored, and
  a duplicate in-flight ActionID is rejected (`asterisk.action_id_busy`)
  rather than corrupting the first waiter.
- **Reconnect**: a supervisor re-dials with bounded exponential backoff
  (`Config.BackoffBase` doubling, capped at `Config.BackoffMax` — defaults
  500ms → 15s). Actions that lose their session mid-flight transparently
  retry on the re-established session within the original action budget.
- **Keepalive**: `Config.PingInterval` (off by default) sends `Action:
  Ping`; a failed ping kills the session so the supervisor reconnects
  immediately instead of leaving actions to time out against a half-open
  TCP peer. Recommended in production (e.g. `10s` behind NAT or stateful
  firewalls).
- **Bounds**: line (8 KiB), packet (1 MiB) and header-count (128) caps
  reject hostile or broken peers; a bound violation is fatal to the session
  and reconnects.

## Network exposure

AMI is a management plane with broad privileges — never expose it beyond
the interface the Orvexa host needs. Prefer a loopback or private-network
`bindaddr` + `permit` ACL (as above); if AMI must cross an untrusted
network, tunnel it (WireGuard/ssh). The Challenge→MD5 handshake protects
the secret on the wire but does not encrypt traffic, and MD5 digest auth is
vulnerable to offline dictionary attacks against a captured challenge —
both are reasons the PBX-side ACL is the real control.

## Testing

`go test -race ./internal/telephony/...` — all green:

- **Codec golden tests** (`ami_test.go`): encode/decode vectors incl.
  banner, failure responses, duplicate `Variable` headers, bare-LF framing,
  bound violations, header-injection rejection, MD5 vectors.
- **Conformance** (`voice_test.go`): the full
  `conformance.RunVoiceConformance` contract over a real TCP socket against
  the fake AMI, twice — once with the Originate response carrying the
  channel name, once with channel binding achieved only through the
  ActionID-echoed `Newchannel` event (how real Asterisk behaves).
- **Reconnect**: the fake drops the connection after the first real action;
  the call still completes and the supervisor provably re-logs in.
  Keepalive test: a muted Ping forces the same recovery.
- **Robustness**: untracked channels stay silent, stray replies never
  corrupt correlation, PBX refusals surface as typed `*apperrors.Error`,
  and the auth-failure battery asserts no credential material in errors,
  logs or config rendering.

The fake AMI server (`fakeami_test.go`) is wire-faithful: it parses client
traffic with the production codec, so any writer interleaving bug shows up
as a server-side parse error rather than a silent test pass.
