# Provider Conformance Kit

`internal/comms/conformance` is the executable behavioral contract for every
Orvexa carrier adapter — voice (`telephony.VoiceProvider`) and messaging
(`messaging.MessagingProvider`). The built-in [Simulator](../simulator.go) is
the reference behavior and passes both contracts green in `kit_test.go`, so
the kit and the reference cannot drift apart silently.

Adapters arriving in the adapter wave — twilio (voice/sms), whatsapp_cloud,
africastalking (voice/sms/ussd), freeswitch, asterisk — embed this kit in
their own `_test.go` files. **An adapter PR that does not pass the kit is not
reviewable.**

## How an adapter embeds the kit

The kit is passive: it ships no HTTP server and makes no network calls. You
wire the adapter's delivery hook to the kit's `Recorder`, then run the
contract.

```go
package twilio

import (
        "testing"

        "github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
)

func TestTwilioVoiceConformance(t *testing.T) {
        rec := conformance.NewRecorder()

        // Construct the adapter with rec.Ingest as its webhook-delivery hook
        // (comms.IngestFunc) and the adapter's own HMAC signer. Native Twilio
        // status callbacks must be translated into comms.ProviderEvent and
        // delivered through that hook — the same path the Simulator uses.
        p := New(Config{
                AccountSID: "AC_test",
                AuthToken:  "test-only", // kit asserts signature presence, not HMAC
                Ingest:     rec.Ingest,
        })

        conformance.RunVoiceConformance(t, p, conformance.Options{
                Recorder:           rec,           // the SAME recorder the adapter delivers into
                ProviderName:       "twilio",      // label pinned on every delivery
                RequireAsyncEvents: true,          // Twilio callbacks arrive async
                EventSettleTimeout: 2 * time.Second,
        })
}

func TestTwilioSMSConformance(t *testing.T) {
        rec := conformance.NewRecorder()
        mp := NewSMS(Config{Ingest: rec.Ingest /* ... */})
        conformance.RunMessagingConformance(t, mp, conformance.Options{
                Recorder:     rec,
                ProviderName: "twilio",
                // EnforceSendValidation: true, // opt in if the adapter re-validates
        })
}
```

### Wiring rules (the two mistakes to avoid)

1. **One Recorder, wired twice.** The adapter must deliver into
   `rec.Ingest`, and the same `rec` must be passed as `Options.Recorder`.
   A recorder the adapter never sees simply observes silence, and the first
   lifecycle assertion fails with a wiring hint.
2. **Async adapters must say so.** The default contract is the Simulator's:
   by the time a port call returns, its events have been delivered and
   applied. Adapters that translate native callbacks on goroutines MUST set
   `RequireAsyncEvents: true` or they will fail the sync contract loudly.

## The voice contract (`RunVoiceConformance`)

| Scenario | Invariant |
|---|---|
| place-call lifecycle | Accepted tenant-bound dial; events are exactly `call.ringing` → `call.connected`, stamped with the dialed interaction id + tenant; interaction ends the step **active**; all deliveries signed with a stable provider label |
| hangup | Live leg accepts hangup; order `ringing → connected → ended`; `call.ended` carries a non-empty cause (persisted as `end_reason`); lands **wrapup**, never completed (after-call work belongs to the agent) |
| transfer | Live leg accepts transfer; transfer is a **new ringing phase** on the same leg with the destination observable in `Detail`; lifecycle stays **active** mid-transfer; full order `ringing → connected → ringing → ended` |
| hold / resume | Accepted, and **lifecycle-silent** — the event vocabulary is closed and has no hold semantics; a stray event here (e.g. `call.ended`) would wrap up the call mid-hold. Leg stays active and operable |
| unknown providerRef | `Hangup`/`Transfer`/`Hold`/`Resume` all **fail loudly**, deterministically (repeat-stable error shape); apperrors-speaking adapters must use **not_found**; zero emissions — no event may exist for a leg the platform never placed |
| tenant context | PlaceCall without `tenant_id` (missing, nil options, or non-string) is **rejected** before any emission; apperrors-speaking adapters use **invalid** |
| call-command validation (opt-in) | `EnforceCommandValidation: true` → malformed commands (missing interaction id / from / to / tenant) must be `*apperrors.Error` with kind **invalid** and a stable code, and must not emit anything |

## The messaging contract (`RunMessagingConformance`)

| Scenario | Invariant |
|---|---|
| validate-message taxonomy | The exact `messaging.ValidateMessage` table — one stable machine code per class (`message.interaction_required`, `message.invalid_channel`, `message.to_required`, `message.body_required`, `message.body_too_long`), always kind **invalid**, with adversarial probes: whitespace-only inputs, the 4096/4097 **multibyte rune** boundary (byte-counters fail), and rule-precedence probes |
| send lifecycle | Valid send accepted; receipts exactly `message.sent` → `message.delivered`, stamped with the message's interaction id + tenant; interaction stays **active** (completed is reserved for `message.read`) |
| media-only | Body-empty + media messages are first-class: `ValidateMessage` passes and the receipt lifecycle is identical to text |
| retry-safety | Re-sending the **same InteractionID must not error** and must not corrupt the lifecycle (receipts re-emitted like the reference, or silently suppressed — both pass; every delivery still applies cleanly). Rationale: the core turns a provider error into interaction failure (`message.provider_failed`), so rejecting duplicates fails real interactions on harmless retries |
| tenant context | Send with empty `TenantID` is rejected before any emission |
| send validation (opt-in) | `EnforceSendValidation: true` → invalid messages must be rejected with `*apperrors.Error`, kind **invalid**, carrying the **exact core code** from the shared table — one taxonomy across the platform |

## Options

| Field | Default | Meaning |
|---|---|---|
| `Recorder` | — (required) | The event receiver wired as the adapter's delivery hook |
| `ProviderName` | `""` (any non-empty, stable label) | Pins the provider label stamped on every delivery |
| `RequireAsyncEvents` | `false` | `false` = reference sync contract (events applied by the time the port call returns); `true` = the adapter emits async and the kit polls |
| `EventSettleTimeout` | `2s` | Poll/silence window per expected batch; only used when async |
| `TenantID` | `conformance-tenant` | Tenant context all scenarios run in |
| `EnforceCommandValidation` | `false` | Opt in to full voice command-validation checks |
| `EnforceSendValidation` | `false` | Opt in to message re-validation checks |

Defaults are honest: the zero value matches the Simulator.

## What the kit deliberately does not check

- **HMAC correctness.** The kit asserts signature presence and stability
  only; HMAC verification belongs to the fail-closed webhook gateway, and
  conformance tests must not carry credential material.
- **Provider-side validation beyond the reference.** The Simulator only
  enforces tenant context, so that is all the default voice/messaging
  invalid-input scenarios enforce; the `Enforce*` knobs let stricter
  adapters prove their extra validation with the canonical taxonomy.
- **Post-terminal ref behavior.** Whether hangup-able refs survive the call
  ending (idempotent double-hangup vs. leg deletion) is adapter-specific;
  both are lifecycle-safe through the processor's same-state no-op.
- **Network behavior.** Timeouts, backoff and rate limits are transport
  concerns; the kit is in-process by design.

## Negative controls: the kit audits itself

A conformance suite is only worth shipping if it demonstrably fails bad
adapters. `negative_control_test.go` runs deliberately broken adapters —
connected-before-ringing, silent success on unknown refs, a `call.ended`
invented during hold, delivered-before-sent receipts, duplicate-rejecting
sends — through the kit in child test processes (`os/exec` on the test
binary, the standard Go self-test pattern) and asserts each run FAILS **for
the designed reason**. This keeps every assertion in the tables above
honest as the kit evolves: if the kit ever waves a violation through, or a
control stops isolating its invariant, `TestKitNegativeControls` fails.

## Mechanics

- `Recorder` implements `comms.IngestFunc`: it decodes each delivery into a
  `comms.ProviderEvent` and applies it through `comms.Processor` to a
  guarded interaction lifecycle — the same harness pattern the Simulator is
  tested with (`internal/comms/simulator_test.go`).
- The lifecycle core is **strict**: interactions must be `Seed`ed (the kit
  does this, mirroring the service that creates the interaction before
  invoking the provider). Events for unknown interactions fail as
  `interaction.not_found`, so phantom legs can never materialize state.
- Test hygiene: the kit itself spawns no goroutines; the Recorder is
  mutex-guarded and race-clean under `-race`. The negative-control children
  re-execute the test binary and inherit the same build flags (including
  `-race`).
