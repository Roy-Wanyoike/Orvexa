package asterisk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// The voice-plane tests run the adapter against the fake AMI server over a
// real TCP socket: every assertion below is a wire-level behavioral claim
// (what bytes went out, what events came back), not a mock-observation.

// secretGuard is a slog sink that records everything the adapter logs so
// leakage tests can scan the full output for credential material.
type secretGuard struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (g *secretGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *secretGuard) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

// testSigner is the stand-in for the gateway HMAC signer: deterministic,
// always non-empty (the delivery contract requires a signature per body).
func testSigner(body []byte) string {
	sum := sha256.Sum256(body)
	return "ovx-test-" + hex.EncodeToString(sum[:8])
}

func hostOf(t *testing.T, addr string) string {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("fake AMI address %q: %v", addr, err)
	}
	return host
}

// newVoiceForTest constructs the adapter wired to the fake and the recorder,
// with fast reconnect knobs, and registers Close. Returns the config and the
// log sink for the tests that assert on them.
func newVoiceForTest(t *testing.T, f *fakeAMI, rec *conformance.Recorder, tune func(*Config)) (*Voice, *Config, *secretGuard) {
	t.Helper()
	g := &secretGuard{}
	cfg := Config{
		Host:        hostOf(t, f.addr()),
		Port:        f.port(),
		Username:    testAMIUsername,
		Secret:      testAMISecret,
		DialContext: "from-internal",
		MOHContext:  testMOHContext,
		Ingest: func(provider string, body []byte, signature string) error {
			return rec.Ingest(context.Background(), provider, body, signature)
		},
		Signer:        testSigner,
		DialTimeout:   2 * time.Second,
		ActionTimeout: 3 * time.Second,
		BackoffBase:   20 * time.Millisecond,
		BackoffMax:    200 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(g, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if tune != nil {
		tune(&cfg)
	}
	v, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(v.Close)
	return v, &cfg, g
}

// dialAndWaitConnected places a call and waits for the ringing->connected
// phase to be applied through the recorder (the async translate path).
func dialAndWaitConnected(t *testing.T, v *Voice, rec *conformance.Recorder, tenant, interactionID string) {
	t.Helper()
	rec.Seed(tenant, interactionID, interactions.StatusPending)
	err := v.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   interactionID,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": tenant},
	})
	if err != nil {
		t.Fatalf("PlaceCall: %v", err)
	}
	if !rec.WaitApplied(2, 3*time.Second) {
		t.Fatalf("timed out waiting for the ringing->connected phase (got %d applied)", len(rec.Applied()))
	}
}

// assertAppliedSeq asserts the exact applied event sequence for one leg.
func assertAppliedSeq(t *testing.T, rec *conformance.Recorder, tenant, interactionID string, want ...string) {
	t.Helper()
	evs := rec.AppliedFor(tenant, interactionID)
	got := make([]string, 0, len(evs))
	for _, ev := range evs {
		got = append(got, ev.Event)
	}
	if len(got) != len(want) {
		t.Fatalf("applied sequence for %s must be %v, got %v", interactionID, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applied sequence for %s must be %v, got %v", interactionID, want, got)
		}
	}
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProviderLabelMatchesRegistry pins the delivery label to the registry's
// provider name — the webhook gateway routes by it.
func TestProviderLabelMatchesRegistry(t *testing.T) {
	if string(registry.ProviderAsterisk) != providerLabel {
		t.Fatalf("provider label %q must match registry.ProviderAsterisk %q", providerLabel, registry.ProviderAsterisk)
	}
}

// runVoiceConformance runs the shared voice behavioral contract against the
// adapter over the fake PBX. The adapter is async (events arrive on the AMI
// reader and are translated on the delivery goroutine), so the async option
// is mandatory, and the adapter speaks the full application error model, so
// command validation is enforced.
func runVoiceConformance(t *testing.T, f *fakeAMI) {
	t.Helper()
	rec := conformance.NewRecorder()
	v, _, _ := newVoiceForTest(t, f, rec, nil)
	conformance.RunVoiceConformance(t, v, conformance.Options{
		Recorder:                 rec,
		ProviderName:             providerLabel,
		RequireAsyncEvents:       true,
		EventSettleTimeout:       3 * time.Second,
		EnforceCommandValidation: true,
	})
}

// TestVoiceConformance is the gate: the full RunVoiceConformance contract
// against the adapter with the fake's default behavior (Originate response
// carries the channel name).
func TestVoiceConformance(t *testing.T) {
	runVoiceConformance(t, startFakeAMI(t))
}

// TestVoiceConformanceNewchannelBindingOnly re-runs the whole contract with
// a PBX that omits the Channel header on the Originate response — the
// channel binding must then come from the ActionID-echoed Newchannel event
// before any lifecycle event is translated (real Asterisk behaves this way).
func TestVoiceConformanceNewchannelBindingOnly(t *testing.T) {
	runVoiceConformance(t, startFakeAMI(t, withOmitOriginateChannel()))
}

// TestVoiceReconnectMidCall proves the bounded-backoff supervisor: the fake
// drops the connection right after the first real action, and the call still
// completes — the second action transparently lands on the re-established
// session, and the leg tracking survives the transport loss.
func TestVoiceReconnectMidCall(t *testing.T) {
	f := startFakeAMI(t, withDropAfterActions(1))
	rec := conformance.NewRecorder()
	v, _, _ := newVoiceForTest(t, f, rec, nil)

	const tenant = "tenant-reconnect"
	id := uuid.NewString()
	dialAndWaitConnected(t, v, rec, tenant, id)

	if err := v.Hangup(context.Background(), id); err != nil {
		t.Fatalf("Hangup across the reconnect must succeed, got: %v", err)
	}
	if !rec.WaitApplied(3, 3*time.Second) {
		t.Fatalf("timed out waiting for call.ended after the reconnect")
	}
	assertAppliedSeq(t, rec, tenant, id, "call.ringing", "call.connected", "call.ended")
	if got := f.loginAttempts(); got < 2 {
		t.Errorf("the supervisor must re-login after the drop, got %d login(s)", got)
	}
	if f.parseErrorCount() != 0 {
		t.Errorf("fake AMI observed parse errors: %v", f.parseErrs)
	}
}

// TestVoiceKeepaliveForcesReconnect proves the half-open detection: the PBX
// stops answering Ping, the keepalive kills the session, and the supervisor
// re-establishes it — the next command works without manual intervention.
func TestVoiceKeepaliveForcesReconnect(t *testing.T) {
	f := startFakeAMI(t, withMutedAction("Ping"))
	rec := conformance.NewRecorder()
	v, _, _ := newVoiceForTest(t, f, rec, func(c *Config) {
		c.PingInterval = 80 * time.Millisecond
		c.ActionTimeout = 400 * time.Millisecond
	})

	const tenant = "tenant-keepalive"
	id := uuid.NewString()
	dialAndWaitConnected(t, v, rec, tenant, id)

	waitFor(t, 3*time.Second, func() bool { return f.loginAttempts() >= 2 })

	if err := v.Hangup(context.Background(), id); err != nil {
		t.Fatalf("Hangup after keepalive-forced reconnect must succeed, got: %v", err)
	}
	if !rec.WaitApplied(3, 3*time.Second) {
		t.Fatalf("timed out waiting for call.ended after the keepalive reconnect")
	}
	assertAppliedSeq(t, rec, tenant, id, "call.ringing", "call.connected", "call.ended")
}

// TestVoiceDropsUntrackedChannelEvents proves the platform-silence rule:
// lifecycle events for channels the adapter never placed produce no
// deliveries at all — the platform must never observe a foreign call.
func TestVoiceDropsUntrackedChannelEvents(t *testing.T) {
	f := startFakeAMI(t)
	rec := conformance.NewRecorder()
	v, _, _ := newVoiceForTest(t, f, rec, nil)

	const tenant = "tenant-untracked"
	id := uuid.NewString()
	dialAndWaitConnected(t, v, rec, tenant, id)
	d0, a0 := len(rec.Deliveries()), len(rec.Applied())

	f.injectRaw("Event: Newchannel\r\nPrivilege: call,all\r\n" +
		"Channel: PJSIP/stranger-1\r\nUniqueid: stranger-1\r\n" +
		"ChannelStateDesc: Down\r\nChannelState: 0\r\n\r\n")
	f.injectRaw("Event: Newstate\r\nPrivilege: call,all\r\n" +
		"Channel: PJSIP/stranger-1\r\nUniqueid: stranger-1\r\n" +
		"ChannelStateDesc: Up\r\nChannelState: 6\r\n\r\n")
	f.injectRaw("Event: Hangup\r\nPrivilege: call,all\r\n" +
		"Channel: PJSIP/stranger-1\r\nUniqueid: stranger-1\r\n" +
		"Cause: 16\r\nCause-txt: Normal Clearing\r\n\r\n")

	time.Sleep(500 * time.Millisecond) // settle window for the async translator

	if got := len(rec.Deliveries()); got != d0 {
		t.Errorf("untracked events must not produce deliveries, got %d new", got-d0)
	}
	if got := len(rec.Applied()); got != a0 {
		t.Errorf("untracked events must not move any lifecycle, got %d new applied", got-a0)
	}
	if f.parseErrorCount() != 0 {
		t.Errorf("fake AMI observed parse errors: %v", f.parseErrs)
	}
}

// TestVoiceStrayReplyDoesNotCorruptCorrelation proves ActionID correlation
// is exact: an unsolicited reply with a foreign ActionID is ignored and the
// placed leg's lifecycle stays exact.
func TestVoiceStrayReplyDoesNotCorruptCorrelation(t *testing.T) {
	f := startFakeAMI(t, withStrayReply())
	rec := conformance.NewRecorder()
	v, _, _ := newVoiceForTest(t, f, rec, nil)

	const tenant = "tenant-stray"
	id := uuid.NewString()
	dialAndWaitConnected(t, v, rec, tenant, id)

	if err := v.Hangup(context.Background(), id); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	if !rec.WaitApplied(3, 3*time.Second) {
		t.Fatalf("timed out waiting for the full lifecycle")
	}
	assertAppliedSeq(t, rec, tenant, id, "call.ringing", "call.connected", "call.ended")
	if got := len(rec.FailedDeliveries()); got != 0 {
		t.Errorf("the stray reply must not corrupt translation, got %d failed deliveries", got)
	}
}

// TestVoiceHangupFailureIsTypedError proves Response: Failure carries the
// application error taxonomy (the caller can classify PBX refusals).
func TestVoiceHangupFailureIsTypedError(t *testing.T) {
	f := startFakeAMI(t, withFailingHangup())
	rec := conformance.NewRecorder()
	v, _, _ := newVoiceForTest(t, f, rec, nil)

	const tenant = "tenant-failhangup"
	id := uuid.NewString()
	dialAndWaitConnected(t, v, rec, tenant, id)

	err := v.Hangup(context.Background(), id)
	var ae *apperrors.Error
	if !errors.As(err, &ae) {
		t.Fatalf("Hangup on a PBX-refused action must return *apperrors.Error, got %T: %v", err, err)
	}
	if ae.Kind != apperrors.KindInternal || ae.Code != codeActionFailed {
		t.Errorf("failure response must map to internal/%s, got %s/%s", codeActionFailed, ae.Kind, ae.Code)
	}
	if f.parseErrorCount() != 0 {
		t.Errorf("fake AMI observed parse errors: %v", f.parseErrs)
	}
}

// TestVoiceCommandValidationAndHeaderInjection proves the input contract:
// malformed commands are rejected with stable codes before any wire traffic,
// and caller-controlled values cannot inject AMI headers.
func TestVoiceCommandValidationAndHeaderInjection(t *testing.T) {
	f := startFakeAMI(t)
	rec := conformance.NewRecorder()
	v, _, _ := newVoiceForTest(t, f, rec, nil)
	ctx := context.Background()

	cases := []struct {
		name     string
		wantCode string
		cmd      telephony.CallCommand
	}{
		{
			name:     "missing tenant context",
			wantCode: codeTenantRequired,
			cmd:      telephony.CallCommand{InteractionID: uuid.NewString(), From: "+1", To: "+2"},
		},
		{
			name:     "missing interaction id",
			wantCode: codeInteractionRequired,
			cmd:      telephony.CallCommand{From: "+1", To: "+2", ProviderOptions: map[string]any{"tenant_id": "t1"}},
		},
		{
			name:     "missing destination",
			wantCode: codeDestinationRequired,
			cmd:      telephony.CallCommand{InteractionID: uuid.NewString(), From: "+1", ProviderOptions: map[string]any{"tenant_id": "t1"}},
		},
		{
			name:     "missing origin",
			wantCode: codeOriginRequired,
			cmd:      telephony.CallCommand{InteractionID: uuid.NewString(), To: "+2", ProviderOptions: map[string]any{"tenant_id": "t1"}},
		},
	}
	for _, tc := range cases {
		err := v.PlaceCall(ctx, tc.cmd)
		var ae *apperrors.Error
		if !errors.As(err, &ae) {
			t.Errorf("%s: must reject with *apperrors.Error, got %T: %v", tc.name, err, err)
			continue
		}
		if ae.Kind != apperrors.KindInvalid || ae.Code != tc.wantCode {
			t.Errorf("%s: must reject as invalid/%s, got %s/%s", tc.name, tc.wantCode, ae.Kind, ae.Code)
		}
	}

	// Header injection: a destination with AMI framing characters must be
	// rejected at request-build time, before any byte reaches the PBX.
	injID := uuid.NewString()
	rec.Seed("t1", injID, interactions.StatusPending)
	inj := telephony.CallCommand{
		InteractionID:   injID,
		From:            "+254700000001",
		To:              "+254711111111\r\nChannel: PJSIP/injected",
		ProviderOptions: map[string]any{"tenant_id": "t1"},
	}
	err := v.PlaceCall(ctx, inj)
	var ae *apperrors.Error
	if !errors.As(err, &ae) || ae.Kind != apperrors.KindInvalid || ae.Code != codeHeaderInj {
		t.Errorf("header injection must be rejected as invalid/%s, got %v", codeHeaderInj, err)
	}

	time.Sleep(300 * time.Millisecond) // settle: nothing may have been emitted
	if got := len(rec.Deliveries()); got != 0 {
		t.Errorf("rejected commands must not emit deliveries, got %d", got)
	}
	if f.parseErrorCount() != 0 {
		t.Errorf("injected bytes must never reach the wire; fake saw parse errors: %v", f.parseErrs)
	}
}

// TestVoiceAuthFailureNoSecretLeakage proves the auth failure surfaces as
// unauthorized with the stable code, and neither the error text, nor the
// adapter's logs, nor the config rendering ever carry credential material.
func TestVoiceAuthFailureNoSecretLeakage(t *testing.T) {
	const realSecret = "real-ami-secret-VALUE-42"
	f := startFakeAMI(t, withAuth("real-mgr", realSecret))
	rec := conformance.NewRecorder()
	// The adapter is configured with the shared test secret, which this fake
	// deliberately rejects, and a short action budget so the unavailable
	// path (surfacing the auth failure) is reached quickly.
	v, cfg, logs := newVoiceForTest(t, f, rec, func(c *Config) {
		c.ActionTimeout = 600 * time.Millisecond
	})

	cmd := telephony.CallCommand{
		InteractionID:   uuid.NewString(),
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": "t-auth"},
	}
	rec.Seed("t-auth", cmd.InteractionID, interactions.StatusPending)

	err := v.PlaceCall(context.Background(), cmd)
	var ae *apperrors.Error
	if !errors.As(err, &ae) {
		t.Fatalf("auth failure must return *apperrors.Error, got %T: %v", err, err)
	}
	if ae.Kind != apperrors.KindUnauth || ae.Code != codeAuthFailed {
		t.Errorf("auth failure must map to unauthorized/%s, got %s/%s", codeAuthFailed, ae.Kind, ae.Code)
	}

	for _, secret := range []string{testAMISecret, realSecret} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error text leaks the configured secret %q", registry.Redacted(secret))
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("adapter logs leak the secret %q", registry.Redacted(secret))
		}
		if strings.Contains(cfg.String(), secret) {
			t.Errorf("Config.String leaks the secret %q", registry.Redacted(secret))
		}
	}
	if !strings.Contains(cfg.String(), "****") {
		t.Errorf("Config.String must render redacted credentials, got %q", cfg.String())
	}
}

// silence compile-time interface assertion: *Voice is a telephony.VoiceProvider.
var _ telephony.VoiceProvider = (*Voice)(nil)
