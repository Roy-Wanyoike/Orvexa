package comms

import (
	"context"
	"strings"
	"testing"

	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// fakeCore records transitions and emulates the guarded lifecycle.
type fakeCore struct {
	statuses map[string]interactions.Status
	events   []ProviderEvent
	failNext error
}

func (f *fakeCore) Get(_ context.Context, tenantID, id string) (*interactions.Rec, error) {
	if f.failNext != nil {
		return nil, f.failNext
	}
	s, ok := f.statuses[tenantID+"/"+id]
	if !ok {
		s = interactions.StatusPending
	}
	return &interactions.Rec{ID: id, TenantID: tenantID, Status: s}, nil
}

func (f *fakeCore) Transition(_ context.Context, tenantID, id string, to interactions.Status, _ string) (*interactions.Rec, error) {
	if f.failNext != nil {
		return nil, f.failNext
	}
	key := tenantID + "/" + id
	cur, ok := f.statuses[key]
	if !ok {
		cur = interactions.StatusPending
	}
	if !interactions.CanTransition(cur, to) {
		return nil, apperrors.Conflict("interaction.invalid_transition", "illegal")
	}
	f.statuses[key] = to
	return &interactions.Rec{ID: id, TenantID: tenantID, Status: to}, nil
}

func newHarness(t *testing.T) (*Simulator, *fakeCore, *[]string) {
	t.Helper()
	core := &fakeCore{statuses: map[string]interactions.Status{}}
	var delivered []string
	signer := func(body []byte) string { return "sig-" + string(body[len(body)-1:]) }
	ingest := func(_ context.Context, provider string, body []byte, sig string) error {
		if provider != "simulator" {
			t.Errorf("unexpected provider %q", provider)
		}
		if !strings.HasPrefix(sig, "sig-") {
			t.Error("delivery must be signed")
		}
		delivered = append(delivered, string(body))
		return nil
	}
	sim := NewSimulator(ingest, signer, 0)
	// wire delivered events through the processor for the full loop
	sim.ingest = func(ctx context.Context, provider string, body []byte, sig string) error {
		if err := ingest(ctx, provider, body, sig); err != nil {
			return err
		}
		var ev ProviderEvent
		if err := jsonUnmarshal(body, &ev); err != nil {
			return err
		}
		core.events = append(core.events, ev)
		return (&Processor{Interactions: core}).Process(ctx, &ev)
	}
	return sim, core, &delivered
}

func TestSimulatorCallLifecycleDrivesInteraction(t *testing.T) {
	sim, core, delivered := newHarness(t)
	tenantID := "tenant-1"

	err := sim.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   "019393a0-1000-7000-8000-00000000aaa1",
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": tenantID},
	})
	if err != nil {
		t.Fatalf("PlaceCall: %v", err)
	}
	// ringing + connected delivered and applied: pending→active
	got, _ := core.Get(context.Background(), tenantID, "019393a0-1000-7000-8000-00000000aaa1")
	if got.Status != interactions.StatusActive {
		t.Fatalf("call must be active after connected, got %s", got.Status)
	}
	if len(*delivered) != 2 {
		t.Fatalf("expected 2 webhook deliveries, got %d", len(*delivered))
	}

	// hangup: call.ended → wrapup
	if err := sim.Hangup(context.Background(), "019393a0-1000-7000-8000-00000000aaa1"); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	got, _ = core.Get(context.Background(), tenantID, "019393a0-1000-7000-8000-00000000aaa1")
	if got.Status != interactions.StatusWrapup {
		t.Fatalf("after hangup interaction must be wrapup, got %s", got.Status)
	}
}

func TestSimulatorRequiresTenantContext(t *testing.T) {
	sim, _, _ := newHarness(t)
	err := sim.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID: "019393a0-1000-7000-8000-00000000aaa2",
		From:          "a", To: "b",
	})
	if err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("missing tenant must be rejected, got %v", err)
	}
}

func TestSimulatorMessageLifecycle(t *testing.T) {
	sim, core, _ := newHarness(t)
	tenantID := "tenant-1"
	id := "019393a0-1000-7000-8000-00000000aaa3"
	// messaging service transitions pending→active before provider.Send
	core.statuses[tenantID+"/"+id] = interactions.StatusActive

	err := sim.Send(context.Background(), messaging.Message{
		InteractionID: id,
		TenantID:      tenantID,
		Channel:       messaging.ChannelWhatsApp,
		From:          "+254700",
		To:            "+254711",
		Body:          "hi",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// sent+delivered both map to active — same-state no-ops, no conflicts
	got, _ := core.Get(context.Background(), tenantID, id)
	if got.Status != interactions.StatusActive {
		t.Fatalf("message must stay active after receipts, got %s", got.Status)
	}
}

func TestProcessorIdempotentOnSameState(t *testing.T) {
	core := &fakeCore{statuses: map[string]interactions.Status{
		"tenant-1/019393a0-1000-7000-8000-00000000aaa4": interactions.StatusActive,
	}}
	p := &Processor{Interactions: core}
	err := p.Process(context.Background(), &ProviderEvent{
		Event: "call.ringing", InteractionID: "019393a0-1000-7000-8000-00000000aaa4", TenantID: "tenant-1",
	})
	if err != nil {
		t.Fatalf("same-state delivery must be a no-op, got %v", err)
	}
}

func TestProcessorRejectsUnknownEventsAndBadIDs(t *testing.T) {
	core := &fakeCore{statuses: map[string]interactions.Status{}}
	p := &Processor{Interactions: core}
	if err := p.Process(context.Background(), &ProviderEvent{
		Event: "carrier.pigeon.loosed", InteractionID: "019393a0-1000-7000-8000-00000000aaa5", TenantID: "t",
	}); err == nil {
		t.Fatal("unknown event must be rejected")
	}
	if err := p.Process(context.Background(), &ProviderEvent{
		Event: "call.ringing", InteractionID: "not-a-uuid", TenantID: "t",
	}); err == nil {
		t.Fatal("non-uuid interaction id must be rejected")
	}
}

func TestSimulatorUnknownLegRejected(t *testing.T) {
	sim, _, _ := newHarness(t)
	if err := sim.Hangup(context.Background(), "019393a0-1000-7000-8000-00000000aaa6"); err == nil {
		t.Fatal("hangup on unknown leg must fail")
	}
}
