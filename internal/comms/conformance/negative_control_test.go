// Negative controls: deliberately broken adapters that the kit MUST reject.
//
// A conformance suite is only worth shipping if it demonstrably fails bad
// adapters. Each control below is a contract-compliant adapter with exactly
// ONE behavior sabotaged, run in a child test process (the standard Go
// self-test pattern: the suite re-executes its own test binary with an env
// role, because Run*Conformance reports violations through *testing.T, which
// cannot be faked in-process). The parent asserts the child FAILED and that
// the failure output names the designed invariant — proving every assertion
// family in voice.go/messaging.go actually bites:
//
//	voice:   event order, unknown-ref loudness, hold/resume silence
//	messaging: event order, duplicate InteractionID retry-safety
package conformance_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// negativeControlEnv is the child-process role variable. Empty in normal
// runs: the parent loop sets it per re-executed child.
const negativeControlEnv = "ORVEXA_CONFORMANCE_NEGATIVE_CONTROL"

// ---------------------------------------------------------------------------
// Voice controls
// ---------------------------------------------------------------------------

// goodVoice is a contract-compliant voice adapter. Every negative control
// embeds it and sabotages exactly one behavior, so a control's failure is
// attributable to its designed invariant — not to collateral breakage.
type goodVoice struct {
	rec  *conformance.Recorder
	mu   sync.Mutex
	legs map[string]string // interaction id -> tenant
}

func newGoodVoice(rec *conformance.Recorder) *goodVoice {
	return &goodVoice{rec: rec, legs: map[string]string{}}
}

func (g *goodVoice) has(ref string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.legs[ref]
	return ok
}

func (g *goodVoice) tenantOf(ref string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.legs[ref]
}

// emit delivers one ProviderEvent synchronously through the Recorder — the
// same signed IngestFunc path every adapter uses.
func (g *goodVoice) emit(ctx context.Context, ev comms.ProviderEvent) {
	body, err := ev.Encode()
	if err != nil {
		return
	}
	_ = g.rec.Ingest(ctx, "negative-control", body, testSigner(body))
}

func (g *goodVoice) PlaceCall(ctx context.Context, cmd telephony.CallCommand) error {
	tenant, _ := cmd.ProviderOptions["tenant_id"].(string)
	if cmd.InteractionID == "" || tenant == "" {
		return apperrors.Invalid("voice.invalid_command",
			"interaction_id and tenant_id (provider options) are required")
	}
	g.mu.Lock()
	g.legs[cmd.InteractionID] = tenant
	g.mu.Unlock()
	g.emit(ctx, comms.ProviderEvent{Event: "call.ringing", InteractionID: cmd.InteractionID, TenantID: tenant})
	g.emit(ctx, comms.ProviderEvent{Event: "call.connected", InteractionID: cmd.InteractionID, TenantID: tenant})
	return nil
}

func (g *goodVoice) Hangup(ctx context.Context, providerRef string) error {
	if !g.has(providerRef) {
		return apperrors.NotFound("voice.leg_not_found", "unknown provider leg reference")
	}
	g.emit(ctx, comms.ProviderEvent{Event: "call.ended", InteractionID: providerRef, TenantID: g.tenantOf(providerRef), Detail: "hangup"})
	return nil
}

func (g *goodVoice) Transfer(ctx context.Context, providerRef, destination string) error {
	if !g.has(providerRef) {
		return apperrors.NotFound("voice.leg_not_found", "unknown provider leg reference")
	}
	g.emit(ctx, comms.ProviderEvent{
		Event: "call.ringing", InteractionID: providerRef, TenantID: g.tenantOf(providerRef), Detail: "transfer:" + destination,
	})
	return nil
}

func (g *goodVoice) Hold(_ context.Context, providerRef string) error {
	if !g.has(providerRef) {
		return apperrors.NotFound("voice.leg_not_found", "unknown provider leg reference")
	}
	return nil // media-state flip only: lifecycle-silent
}

func (g *goodVoice) Resume(_ context.Context, providerRef string) error {
	if !g.has(providerRef) {
		return apperrors.NotFound("voice.leg_not_found", "unknown provider leg reference")
	}
	return nil
}

// orderBrokenVoice emits connected BEFORE ringing — the dial-phase order is
// the observable the realtime fan-out and routing depend on. The processor
// happens to accept both (same target state), so only the kit's sequence
// assertion can catch this; that is exactly what the control proves.
type orderBrokenVoice struct{ *goodVoice }

func (o *orderBrokenVoice) PlaceCall(ctx context.Context, cmd telephony.CallCommand) error {
	if err := o.goodVoice.PlaceCall(ctx, cmd); err != nil {
		return err
	}
	// sabotage: the leg was registered by the base, so re-emit the dial phase
	// in the wrong order (connected then ringing).
	tenant := o.tenantOf(cmd.InteractionID)
	o.emit(ctx, comms.ProviderEvent{Event: "call.connected", InteractionID: cmd.InteractionID, TenantID: tenant})
	o.emit(ctx, comms.ProviderEvent{Event: "call.ringing", InteractionID: cmd.InteractionID, TenantID: tenant})
	return nil
}

// silentUnknownRefVoice succeeds SILENTLY on Hangup for a leg it does not
// own — the most dangerous failure mode: the platform believes the call is
// over while the carrier keeps the leg (and the meter) running.
type silentUnknownRefVoice struct{ *goodVoice }

func (s *silentUnknownRefVoice) Hangup(ctx context.Context, providerRef string) error {
	if !s.has(providerRef) {
		return nil // sabotage: silent success on an unknown ref
	}
	return s.goodVoice.Hangup(ctx, providerRef)
}

// holdEmitterVoice translates its native hold state into a call.ended event
// — an invented lifecycle claim: the closed event vocabulary has no hold
// semantics, and ended wraps up a live call mid-hold.
type holdEmitterVoice struct{ *goodVoice }

func (h *holdEmitterVoice) Hold(ctx context.Context, providerRef string) error {
	if !h.has(providerRef) {
		return apperrors.NotFound("voice.leg_not_found", "unknown provider leg reference")
	}
	h.emit(ctx, comms.ProviderEvent{
		Event: "call.ended", InteractionID: providerRef, TenantID: h.tenantOf(providerRef), Detail: "hold",
	})
	return nil
}

// ---------------------------------------------------------------------------
// Messaging controls
// ---------------------------------------------------------------------------

// goodMessenger is a contract-compliant messaging adapter (sync receipts,
// tenant required, duplicates accepted like the reference).
type goodMessenger struct {
	rec *conformance.Recorder
}

func (m *goodMessenger) deliver(ctx context.Context, msg messaging.Message, events ...string) error {
	if msg.TenantID == "" {
		return apperrors.Invalid("message.tenant_required", "tenant context is required")
	}
	for _, name := range events {
		ev := comms.ProviderEvent{Event: name, InteractionID: msg.InteractionID, TenantID: msg.TenantID}
		body, err := ev.Encode()
		if err != nil {
			return err
		}
		if err := m.rec.Ingest(ctx, "negative-control", body, testSigner(body)); err != nil {
			return err
		}
	}
	return nil
}

func (m *goodMessenger) Send(ctx context.Context, msg messaging.Message) error {
	return m.deliver(ctx, msg, "message.sent", "message.delivered")
}

// orderBrokenMessenger delivers the read-side receipt before the send
// acknowledgement — receipts must never overtake the send.
type orderBrokenMessenger struct{ *goodMessenger }

func (o *orderBrokenMessenger) Send(ctx context.Context, msg messaging.Message) error {
	return o.deliver(ctx, msg, "message.delivered", "message.sent")
}

// dupRejectMessenger rejects a repeated InteractionID. The core retries
// provider.Send after transport timeouts and turns a provider error into
// interaction failure — so rejecting duplicates fails REAL interactions on
// harmless retries.
type dupRejectMessenger struct {
	*goodMessenger
	seen map[string]bool
}

func (d *dupRejectMessenger) Send(ctx context.Context, msg messaging.Message) error {
	if d.seen[msg.InteractionID] {
		return apperrors.Conflict("messenger.duplicate_interaction", "interaction already sent")
	}
	d.seen[msg.InteractionID] = true
	return d.goodMessenger.Send(ctx, msg)
}

// ---------------------------------------------------------------------------
// The control matrix (parent) and its child-process role
// ---------------------------------------------------------------------------

type negativeControl struct {
	env     string // child-process role
	name    string // human label in -run output
	wantSub string // failure output MUST name this designed invariant
}

var negativeControls = []negativeControl{
	{
		env:     "voice-order",
		name:    "voice: emits connected before ringing",
		wantSub: "lifecycle event sequence must be exactly",
	},
	{
		env:     "voice-silent-unknown-ref",
		name:    "voice: silently ignores unknown refs",
		wantSub: "must return an error, got nil",
	},
	{
		env:     "voice-hold-emits-ended",
		name:    "voice: emits call.ended on Hold",
		wantSub: "must be lifecycle-silent",
	},
	{
		env:     "messaging-order",
		name:    "messaging: emits delivered before sent",
		wantSub: "lifecycle event sequence must be exactly",
	},
	{
		env:     "messaging-dup-reject",
		name:    "messaging: rejects duplicate InteractionID",
		wantSub: "must be accepted",
	},
}

// TestKitNegativeControls re-executes this test binary once per control and
// asserts the child run FAILED for the designed reason. If the kit ever
// accepts a broken adapter (or fails it for a collateral, unintended
// reason), this test fails — the kit audits itself.
func TestKitNegativeControls(t *testing.T) {
	if os.Getenv(negativeControlEnv) != "" {
		t.Skip("child-process role only")
	}
	for _, ctrl := range negativeControls {
		t.Run(ctrl.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestKitNegativeControlChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), negativeControlEnv+"="+ctrl.env)
			out, err := cmd.CombinedOutput()
			// Invariant: a broken adapter MUST fail the conformance run. A
			// zero exit here means the kit waved a contract violation through.
			if err == nil {
				t.Fatalf("the kit ACCEPTED a broken adapter (%s) — the contract is not enforced", ctrl.name)
			}
			// Invariant: the failure must be the DESIGNED one. A failure for
			// any other reason means the control stopped isolating its
			// invariant (or an unrelated assertion regressed) — either way the
			// control no longer proves what it claims.
			if !strings.Contains(string(out), ctrl.wantSub) {
				t.Fatalf("broken adapter failed, but not for the designed reason; want output containing %q, got:\n%s",
					ctrl.wantSub, out)
			}
		})
	}
}

// TestKitNegativeControlChild is the child-process role of the control
// matrix: it runs ONE deliberately broken adapter through the kit and lets
// the kit's own assertions fail the process. Never selected in a normal
// run (skipped without the role variable).
func TestKitNegativeControlChild(t *testing.T) {
	role := os.Getenv(negativeControlEnv)
	if role == "" {
		t.Skip("run via TestKitNegativeControls")
	}
	rec := conformance.NewRecorder()
	opts := conformance.Options{Recorder: rec}
	switch role {
	case "voice-order":
		conformance.RunVoiceConformance(t, &orderBrokenVoice{goodVoice: newGoodVoice(rec)}, opts)
	case "voice-silent-unknown-ref":
		conformance.RunVoiceConformance(t, &silentUnknownRefVoice{goodVoice: newGoodVoice(rec)}, opts)
	case "voice-hold-emits-ended":
		conformance.RunVoiceConformance(t, &holdEmitterVoice{goodVoice: newGoodVoice(rec)}, opts)
	case "messaging-order":
		conformance.RunMessagingConformance(t, &orderBrokenMessenger{goodMessenger: &goodMessenger{rec: rec}}, opts)
	case "messaging-dup-reject":
		conformance.RunMessagingConformance(t, &dupRejectMessenger{
			goodMessenger: &goodMessenger{rec: rec}, seen: map[string]bool{},
		}, opts)
	default:
		t.Fatalf("unknown negative-control role %q", role)
	}
}
