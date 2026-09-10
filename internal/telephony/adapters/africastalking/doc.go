// Package africastalking implements the telephony.VoiceProvider port for
// Africa's Talking Voice (issue #28, [O-19]) — the regional first choice
// carrier for the Nairobi MVP.
//
// Architecture (hexagonal, per ADR-0004/0005):
//
//   - Dial plane: PlaceCall POSTs form-encoded parameters to
//     {APIBaseURL}/call (username, to, from/callerId, clientRequestId) with
//     the apikey header credential (registry.Secret — zero leakage on every
//     rendering path). AT queues the dial asynchronously: the REST response
//     carries entry status "Queued" plus a sessionId, and lifecycle progress
//     arrives later as status callbacks.
//
//   - Callback plane: AT reports call progress to the platform callBackUrl.
//     The adapter exposes StatusCallbackHandler (net/http) for that surface
//     and HandleStatusCallback for in-process/gateway wiring: callbacks are
//     translated into comms.ProviderEvent deliveries (call.ringing /
//     call.connected / call.ended) through the comms.IngestFunc port — the
//     exact path the built-in Simulator uses. TranslateStatus is the pure
//     (side-effect free) helper behind that translation. AT does NOT sign
//     its callbacks: transport-origin verification belongs exclusively to
//     the fail-closed webhook gateway (internal/webhooks.VerifyAfricasTalking,
//     the peer-IP allowlist path) — never in this package.
//
//   - In-call control: AT's REST API exposes NO out-of-band hangup, transfer
//     or hold/resume endpoints (research finding captured in README.md).
//     Operator control is therefore dispatched in two honest layers: the
//     platform leg state + lifecycle record is applied at acceptance
//     (delivered as a signed ProviderEvent), and the carrier-side action is
//     queued for AT's documented callback-action protocol (the adapter's
//     callback response carries the pending action for the session). The
//     capability matrix in README.md states exactly what each operation can
//     and cannot promise; operations that cannot be performed (control
//     against a terminal leg) fail with the typed ErrUnsupportedOperation —
//     never a faked success.
//
//   - Errors: every failure is an *apperrors.Error carrying a stable machine
//     code (at.*); errors never embed credentials or the apikey header.
//     409/429/5xx responses are retried with bounded, jittered backoff
//     (≤3 retries); other failures are terminal.
//
// Conformance: the adapter embeds RunVoiceConformance
// (internal/comms/conformance) in async mode against an httptest fake AT —
// an adapter PR that does not pass the kit is not reviewable.
//
// Ownership: this package is the exclusive scope of issue #28. It does not
// modify go.mod/go.sum (stdlib net/http only), the webhook gateway, the
// registry, or the core telephony service.
//
// Security: credentials enter only via registry.Secret and are never
// rendered, logged, or embedded in errors; adversarial leakage tests pin
// that guarantee (see voice_test.go).
package africastalking
