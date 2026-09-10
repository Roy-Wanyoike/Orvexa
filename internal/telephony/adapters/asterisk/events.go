package asterisk

import (
	"github.com/Roy-Wanyoike/orvexa/internal/comms"
)

// Event translation. The AMI reader hands raw packets to enqueueEvent; a
// single delivery goroutine consumes them in wire order, binds/looks up the
// owning leg, and translates the closed set of lifecycle events:
//
//	Newstate (ChannelStateDesc Ringing / 4)  -> call.ringing
//	Newstate (ChannelStateDesc Up / 6)       -> call.connected
//	Hangup   (Cause / Cause-txt)             -> call.ended (Detail = cause)
//
// Everything else (Newchannel is a binding event, all others unmodeled) is
// consumed without delivery. Packets for channels no leg owns are dropped:
// the platform never observes a call it did not place.
//
// Delivery is exactly-once through the wired comms.IngestFunc hook: a
// rejection from the hook is a processing decision (unknown interaction,
// illegal transition), not a transport failure — the adapter logs it and
// moves on instead of duplicating lifecycle traffic. The signature is
// computed per body with the configured Signer; an empty signature is a
// Signer-contract violation and the delivery is dropped before it can be
// discarded by the fail-closed gateway in production.

// eventQueueSize bounds the translate queue. When full (a wedged delivery
// hook), the reader goroutine applies TCP backpressure instead of dropping
// lifecycle events.
const eventQueueSize = 256

// enqueueEvent is the client's onEvent hook — never blocks the reader for
// longer than the queue allows, and stops cleanly with Close.
func (v *Voice) enqueueEvent(pkt amiPacket) {
	select {
	case v.events <- pkt:
	case <-v.stop:
	}
}

// deliverLoop translates queued packets in wire order until Close.
func (v *Voice) deliverLoop() {
	defer v.wg.Done()
	for {
		select {
		case <-v.stop:
			return
		case pkt := <-v.events:
			v.processPacket(pkt)
		}
	}
}

// processPacket binds and translates one packet.
func (v *Voice) processPacket(pkt amiPacket) {
	switch pkt.event() {
	case "Newchannel":
		v.onNewchannel(pkt)
	case "Newstate":
		v.onNewstate(pkt)
	case "Hangup":
		v.onHangup(pkt)
	default:
		// Unmodeled event: consumed, never delivered.
	}
}

// onNewchannel binds the PBX channel/uniqueid to the owning leg. The
// ActionID echo is the authoritative binding for a leg this adapter just
// originated; the channel index covers re-observed channels.
func (v *Voice) onNewchannel(pkt amiPacket) {
	channel := pkt.get("Channel")
	if channel == "" {
		return
	}
	uniqueid := pkt.get("Uniqueid")
	v.mu.Lock()
	defer v.mu.Unlock()
	l := v.legs[pkt.actionID()]
	if l == nil {
		l = v.byChannel[channel]
	}
	if l == nil {
		return // untracked channel: platform stays silent
	}
	if l.channel == "" {
		l.channel = channel
	}
	if uniqueid != "" {
		l.uniqueid = uniqueid
		v.byUniqueid[uniqueid] = l
	}
	v.byChannel[channel] = l
}

// bindFromResponse is the client's onReply hook: an Originate response may
// carry the channel name; binding it here (before the reply is fanned to the
// waiter) guarantees the leg is channel-bound by the time PlaceCall returns.
func (v *Voice) bindFromResponse(pkt amiPacket) {
	channel := pkt.get("Channel")
	if channel == "" {
		return
	}
	id := pkt.actionID()
	if id == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	l := v.legs[id] // nil for transport-owned replies (handshake, ping)
	if l == nil {
		return
	}
	if l.channel == "" {
		l.channel = channel
	}
	v.byChannel[channel] = l
}

// onNewstate translates ring/answer transitions for a tracked leg.
func (v *Voice) onNewstate(pkt amiPacket) {
	l := v.legForEvent(pkt)
	if l == nil {
		return
	}
	desc := pkt.get("ChannelStateDesc")
	code := pkt.get("ChannelState")
	var event string
	switch {
	case desc == "Ringing" || code == "4":
		event = "call.ringing"
	case desc == "Up" || code == "6":
		event = "call.connected"
	default:
		return // Down, Busy, ... — not part of the platform lifecycle
	}

	detail := ""
	if event == "call.ringing" {
		// Consume a staged transfer destination exactly once: the re-ring the
		// PBX emits for a blind transfer is delivered as a new ringing phase
		// whose Detail names the destination (audit requirement).
		v.mu.Lock()
		dest := l.transferTo
		l.transferTo = ""
		v.mu.Unlock()
		if dest != "" {
			detail = "transfer:" + dest
		}
	}
	v.deliver(event, l.interactionID, l.tenantID, detail)
}

// onHangup translates teardown for a tracked leg, stamps the PBX cause as
// the end reason, and retires the leg: after the channel is destroyed no
// further event can legally reference it.
func (v *Voice) onHangup(pkt amiPacket) {
	l := v.legForEvent(pkt)
	if l == nil {
		return
	}
	cause := hangupCause(pkt)
	v.removeLeg(l)
	v.deliver("call.ended", l.interactionID, l.tenantID, cause)
}

// legForEvent resolves the owning leg by uniqueid, channel name, then
// ActionID echo (event sources differ in which identifiers they carry).
func (v *Voice) legForEvent(pkt amiPacket) *leg {
	v.mu.Lock()
	defer v.mu.Unlock()
	if uid := pkt.get("Uniqueid"); uid != "" {
		if l := v.byUniqueid[uid]; l != nil {
			return l
		}
	}
	if channel := pkt.get("Channel"); channel != "" {
		if l := v.byChannel[channel]; l != nil {
			return l
		}
	}
	if id := pkt.actionID(); id != "" {
		return v.legs[id]
	}
	return nil
}

// hangupCause renders the PBX cause for the event Detail (the processor
// persists it as the interaction's end_reason). Server text is cleaned
// before reuse; silence is not a cause.
func hangupCause(pkt amiPacket) string {
	if txt := cleanWireValue(pkt.get("Cause-txt"), 120); txt != "" {
		return txt
	}
	if n := cleanWireValue(pkt.get("Cause"), 12); n != "" {
		return "cause " + n
	}
	return "hangup"
}

// deliver signs and hands one provider event to the wired hook.
func (v *Voice) deliver(event, interactionID, tenantID, detail string) {
	ev := &comms.ProviderEvent{
		Event:         event,
		InteractionID: interactionID,
		TenantID:      tenantID,
		Detail:        detail,
	}
	body, err := ev.Encode()
	if err != nil {
		v.log.Error("asterisk: provider event encode failed; delivery dropped",
			"event", event, "err", err)
		return
	}
	sig := v.cfg.Signer(body)
	if sig == "" {
		// The webhook gateway is fail-closed and would drop this anyway;
		// emitting an unsigned delivery would assert behavior that cannot
		// survive production. A Signer that returns "" is broken wiring.
		v.log.Error("asterisk: signer produced an empty signature; delivery dropped",
			"event", event, "interaction_id", interactionID)
		return
	}
	if err := v.cfg.Ingest(providerLabel, body, sig); err != nil {
		v.log.Warn("asterisk: provider event delivery rejected",
			"event", event, "interaction_id", interactionID, "err", err)
	}
}
