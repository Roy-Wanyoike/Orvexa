package asterisk

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// providerLabel stamps every provider event the adapter delivers. Keep in
// lockstep with registry.ProviderAsterisk (asserted in voice_test.go); it is
// a plain constant so the adapter does not depend on the registry package at
// runtime.
const providerLabel = "asterisk"

// Voice-plane error codes. Session-layer codes live in conn.go; these cover
// the telephony.VoiceProvider translation (input validation, leg tracking,
// PBX-refused actions).
const (
	codeTenantRequired      = "asterisk.tenant_required"
	codeInteractionRequired = "asterisk.interaction_required"
	codeDestinationRequired = "asterisk.destination_required"
	codeOriginRequired      = "asterisk.origin_required"
	codeLegUnknown          = "asterisk.leg_unknown"
	codeChannelPending      = "asterisk.channel_pending"
	codeLegDestination      = "asterisk.leg_destination_unknown"
)

// errUnknownLeg is the unknown-provider-ref error: 404 semantics (the leg
// does not exist for this adapter), shape-stable across repeats so callers
// can classify it reliably.
func errUnknownLeg() error {
	return apperrors.NotFound(codeLegUnknown, "asterisk: no live leg for this provider reference")
}

// leg is the adapter's view of one placed call. interactionID and tenantID
// are immutable after construction (set before the leg becomes visible to
// any other goroutine); channel, uniqueid, exten and transferTo are mutated
// only under Voice.mu.
type leg struct {
	interactionID string
	tenantID      string

	channel    string // PBX channel name (Originate response / Newchannel)
	uniqueid   string // PBX channel uniqueid (Newchannel)
	exten      string // current destination the leg is parked on (To, updated on Transfer)
	transferTo string // destination staged for the next Ringing event on this channel
}

// Voice implements telephony.VoiceProvider over the Asterisk Manager
// Interface. Commands become AMI actions on the shared conn.go session;
// observations come back as Newstate/Hangup events, which events.go
// translates into signed comms.ProviderEvent deliveries on the adapter's
// own goroutine (wire order preserved, exactly-once emission).
//
// Leg tracking is keyed by the platform interaction id (telephony.ProviderRef
// convention): the Originate action carries ActionID = interaction id, the
// PBX echoes it, and every later event for that channel is stamped with the
// interaction id and tenant. Legs the adapter did not place are never
// delivered — the platform observes no foreign calls.
type Voice struct {
	cfg    Config
	client *amiClient
	log    *slog.Logger

	mu         sync.Mutex
	legs       map[string]*leg // interaction id -> leg
	byChannel  map[string]*leg // PBX channel name -> leg
	byUniqueid map[string]*leg // PBX channel uniqueid -> leg

	events   chan amiPacket // raw event packets, translated in wire order
	stop     chan struct{}  // closed by Close; unblocks enqueue and delivery
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New constructs the adapter. Ingest and Signer are required: events are
// reported through the platform's fail-closed signed webhook path, and an
// adapter wired without them would silently drop the lifecycle.
// Construction does not block on the network — the session supervisor dials
// in the background and the first action waits within its own budget
// (Config.ActionTimeout).
func New(cfg Config) (*Voice, error) {
	if cfg.Ingest == nil {
		return nil, apperrors.Invalid("asterisk.ingest_required", "asterisk: Ingest delivery hook is required")
	}
	if cfg.Signer == nil {
		return nil, apperrors.Invalid("asterisk.signer_required", "asterisk: Signer is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	v := &Voice{
		cfg:        cfg,
		log:        cfg.Logger,
		legs:       make(map[string]*leg),
		byChannel:  make(map[string]*leg),
		byUniqueid: make(map[string]*leg),
		events:     make(chan amiPacket, eventQueueSize),
		stop:       make(chan struct{}),
	}
	v.client = newClient(cfg, v.bindFromResponse, v.enqueueEvent)
	v.wg.Add(1)
	go v.deliverLoop()
	v.client.start(context.Background())
	return v, nil
}

// Close shuts the adapter down: event delivery stops, the AMI session is
// logged off and torn down. Idempotent.
func (v *Voice) Close() {
	v.stopOnce.Do(func() {
		close(v.stop)
		v.client.close()
		v.wg.Wait()
	})
}

// PlaceCall implements telephony.VoiceProvider: an async Originate whose
// ActionID is the interaction id. The channel is a Local leg into the
// configured dialplan context (Config.DialContext), so the PBX — not the
// platform — owns how the destination is actually dialed.
func (v *Voice) PlaceCall(ctx context.Context, cmd telephony.CallCommand) error {
	tenantID, _ := cmd.ProviderOptions["tenant_id"].(string)
	if strings.TrimSpace(tenantID) == "" {
		return apperrors.Invalid(codeTenantRequired,
			"asterisk: place-call requires a string tenant_id provider option")
	}
	switch {
	case strings.TrimSpace(cmd.InteractionID) == "":
		return apperrors.Invalid(codeInteractionRequired, "asterisk: place-call requires an interaction id")
	case strings.TrimSpace(cmd.To) == "":
		return apperrors.Invalid(codeDestinationRequired, "asterisk: place-call requires a destination (To)")
	case strings.TrimSpace(cmd.From) == "":
		return apperrors.Invalid(codeOriginRequired, "asterisk: place-call requires an origin (From)")
	}

	l := &leg{interactionID: cmd.InteractionID, tenantID: tenantID, exten: cmd.To}
	if !v.registerLeg(l) {
		return apperrors.Conflict(codeActionIDBusy,
			"asterisk: an action with this ActionID is already in flight")
	}

	req := amiRequest{
		Action: "Originate",
		Headers: []amiHeader{
			{Key: "Channel", Value: "Local/" + cmd.To + "@" + v.cfg.DialContext},
			{Key: "Context", Value: v.cfg.DialContext},
			{Key: "Exten", Value: cmd.To},
			{Key: "Priority", Value: "1"},
			{Key: "CallerID", Value: cmd.From},
			{Key: "Async", Value: "true"},
			{Key: "Timeout", Value: strconv.FormatInt(v.cfg.OriginateTimeout.Milliseconds(), 10)},
		},
	}
	pkt, err := v.client.call(ctx, cmd.InteractionID, req)
	if err != nil {
		v.removeLeg(l) // the leg never started: stop tracking before surfacing the error
		return err
	}
	if pkt.response() != "Success" {
		v.removeLeg(l)
		return actionError("Originate", pkt.response(), pkt.get("Message"))
	}
	return nil // lifecycle events (ringing/connected/ended) arrive asynchronously
}

// Hangup implements telephony.VoiceProvider: Action: Hangup on the tracked
// PBX channel. The resulting Hangup event (events.go) is what moves the
// interaction to wrapup with the PBX's cause — the command itself only
// requests the teardown.
func (v *Voice) Hangup(ctx context.Context, providerRef string) error {
	l, ok := v.legByID(providerRef)
	if !ok {
		return errUnknownLeg()
	}
	channel, err := v.awaitChannel(ctx, l)
	if err != nil {
		return err
	}
	pkt, err := v.client.call(ctx, "", amiRequest{
		Action:  "Hangup",
		Headers: []amiHeader{{Key: "Channel", Value: channel}},
	})
	if err != nil {
		return err
	}
	if pkt.response() != "Success" {
		return actionError("Hangup", pkt.response(), pkt.get("Message"))
	}
	return nil
}

// Transfer implements telephony.VoiceProvider as a blind transfer: Action:
// Redirect of the live channel back into the dialplan context at the new
// destination. The destination rings (a Newstate Ringing on the same
// channel), which events.go translates into a new call.ringing phase whose
// Detail names the transfer destination.
func (v *Voice) Transfer(ctx context.Context, providerRef, destination string) error {
	if strings.TrimSpace(destination) == "" {
		return apperrors.Invalid(codeDestinationRequired, "asterisk: transfer requires a destination")
	}
	l, ok := v.legByID(providerRef)
	if !ok {
		return errUnknownLeg()
	}
	channel, err := v.awaitChannel(ctx, l)
	if err != nil {
		return err
	}
	// Stage the destination before the wire: the Ringing event must carry it
	// in Detail, and that event is translated on another goroutine as soon as
	// the PBX emits it. Cleared only if the redirect never took effect.
	v.stageTransfer(l, destination)
	pkt, err := v.client.call(ctx, "", amiRequest{
		Action: "Redirect",
		Headers: []amiHeader{
			{Key: "Channel", Value: channel},
			{Key: "Context", Value: v.cfg.DialContext},
			{Key: "Exten", Value: destination},
		},
	})
	if err != nil {
		v.clearStagedTransfer(l, destination)
		return err
	}
	if pkt.response() != "Success" {
		v.clearStagedTransfer(l, destination)
		return actionError("Redirect", pkt.response(), pkt.get("Message"))
	}
	v.commitTransfer(l, destination)
	return nil
}

// Hold implements telephony.VoiceProvider by redirecting the customer leg
// into the music-on-hold context (Config.MOHContext). This is the classic
// AMI hold pattern: the platform never generates dialplan, so the MOH
// context must be provisioned on the PBX — the package README documents the
// exact expectation and the honest limitation (the redirect tears down the
// current bridge; the agent side hears the far party park).
func (v *Voice) Hold(ctx context.Context, providerRef string) error {
	l, ok := v.legByID(providerRef)
	if !ok {
		return errUnknownLeg()
	}
	channel, err := v.awaitChannel(ctx, l)
	if err != nil {
		return err
	}
	pkt, err := v.client.call(ctx, "", amiRequest{
		Action: "Redirect",
		Headers: []amiHeader{
			{Key: "Channel", Value: channel},
			{Key: "Context", Value: v.cfg.MOHContext},
			{Key: "Exten", Value: "s"},
		},
	})
	if err != nil {
		return err
	}
	if pkt.response() != "Success" {
		return actionError("Redirect", pkt.response(), pkt.get("Message"))
	}
	return nil // hold is a media operation: deliberately lifecycle-silent
}

// Resume implements telephony.VoiceProvider by redirecting the leg back to
// its current destination in the dialplan context — the inverse of Hold.
// The PBX re-enters the dialplan at the extension the call is parked on; a
// redirect to the destination the call is already on is silent.
func (v *Voice) Resume(ctx context.Context, providerRef string) error {
	l, ok := v.legByID(providerRef)
	if !ok {
		return errUnknownLeg()
	}
	exten := v.extenOf(l)
	if exten == "" {
		return apperrors.Conflict(codeLegDestination,
			"asterisk: the leg's current dialplan destination is unknown; cannot resume")
	}
	channel, err := v.awaitChannel(ctx, l)
	if err != nil {
		return err
	}
	pkt, err := v.client.call(ctx, "", amiRequest{
		Action: "Redirect",
		Headers: []amiHeader{
			{Key: "Channel", Value: channel},
			{Key: "Context", Value: v.cfg.DialContext},
			{Key: "Exten", Value: exten},
		},
	})
	if err != nil {
		return err
	}
	if pkt.response() != "Success" {
		return actionError("Redirect", pkt.response(), pkt.get("Message"))
	}
	return nil // resume is a media operation: deliberately lifecycle-silent
}

// ---------------------------------------------------------------------------
// leg registry (all mutations under v.mu)
// ---------------------------------------------------------------------------

func (v *Voice) registerLeg(l *leg) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, exists := v.legs[l.interactionID]; exists {
		return false
	}
	v.legs[l.interactionID] = l
	return true
}

// removeLeg drops a leg from every index. Identity-checked: a pointer that
// already lost its slot (replaced or removed) cleans nothing.
func (v *Voice) removeLeg(l *leg) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if cur := v.legs[l.interactionID]; cur == l {
		delete(v.legs, l.interactionID)
	}
	if l.channel != "" {
		if cur := v.byChannel[l.channel]; cur == l {
			delete(v.byChannel, l.channel)
		}
	}
	if l.uniqueid != "" {
		if cur := v.byUniqueid[l.uniqueid]; cur == l {
			delete(v.byUniqueid, l.uniqueid)
		}
	}
}

func (v *Voice) legByID(id string) (*leg, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	l := v.legs[id]
	return l, l != nil
}

func (v *Voice) extenOf(l *leg) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return l.exten
}

func (v *Voice) stageTransfer(l *leg, dest string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	l.transferTo = dest
}

// clearStagedTransfer clears the staged destination only if it is still the
// one this caller staged — a newer transfer's staging is never clobbered.
func (v *Voice) clearStagedTransfer(l *leg, dest string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if l.transferTo == dest {
		l.transferTo = ""
	}
}

func (v *Voice) commitTransfer(l *leg, dest string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	l.exten = dest
}

// awaitChannel waits until the leg's PBX channel name is known. The async
// Originate reveals the channel on its response (when the PBX echoes it) or
// on the ActionID-tagged Newchannel event, which can trail the response by
// a beat on a loaded PBX. Bounded by Config.ActionTimeout; a leg whose
// channel never materialized is a Conflict — the caller can retry.
func (v *Voice) awaitChannel(ctx context.Context, l *leg) (string, error) {
	deadline := time.Now().Add(v.cfg.ActionTimeout)
	for {
		v.mu.Lock()
		channel := l.channel
		v.mu.Unlock()
		if channel != "" {
			return channel, nil
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !time.Now().Before(deadline) {
			return "", apperrors.Conflict(codeChannelPending,
				"asterisk: the PBX has not confirmed the leg's channel yet")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
