package freeswitch

import (
	"context"
	"strings"
	"sync"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
)

// ESL event names the adapter subscribes to (events plain ...).
const (
	evChannelAnswer = "CHANNEL_ANSWER"
	evChannelHangup = "CHANNEL_HANGUP"
	evBackgroundJob = "BACKGROUND_JOB"
)

// subscribedEvents is the `events plain` argument list sent on every fresh
// session. Dial progress is observed through BACKGROUND_JOB correlation
// (the Job-UUID of the bgapi originate), answer through CHANNEL_ANSWER, and
// call end through CHANNEL_HANGUP. CHANNEL_PROGRESS/RINGING are not
// subscribed: the adapter never claims a ringing phase it cannot correlate
// to a leg it placed (see README, "Eventing assumptions").
var subscribedEvents = []string{evChannelAnswer, evChannelHangup, evBackgroundJob}

// Event header names this layer reads (case-insensitive via EventField).
const (
	hdrUniqueID    = "Unique-ID"
	hdrHangupCause = "Hangup-Cause"
	hdrEventName   = "Event-Name"
)

// leg is one outbound call the adapter placed. The platform's interaction
// id doubles as the FreeSWITCH origination_uuid, so it is simultaneously
// the provider leg reference and the channel Unique-ID on every event.
type leg struct {
	tenantID string
	jobUUID  string // bgapi job carrying this dial's result; empty once resolved
}

// legRegistry tracks the legs this adapter placed and the in-flight
// originate jobs. Everything the adapter emits is scoped to a tracked leg:
// switch chatter about channels nobody dialed through this process (inbound
// calls, other integrations sharing the event socket) is ignored, so the
// platform never observes state for interactions it does not own.
type legRegistry struct {
	mu   sync.Mutex
	legs map[string]*leg   // interaction id (== origination_uuid) -> leg
	jobs map[string]string // Job-UUID -> interaction id
}

func newLegRegistry() *legRegistry {
	return &legRegistry{legs: map[string]*leg{}, jobs: map[string]string{}}
}

func (r *legRegistry) trackLeg(interactionID, tenantID string) {
	r.mu.Lock()
	r.legs[interactionID] = &leg{tenantID: tenantID}
	r.mu.Unlock()
}

func (r *legRegistry) trackJob(jobUUID, interactionID string) {
	r.mu.Lock()
	r.jobs[jobUUID] = interactionID
	if l, ok := r.legs[interactionID]; ok {
		l.jobUUID = jobUUID
	}
	r.mu.Unlock()
}

func (r *legRegistry) legOf(interactionID string) (*leg, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.legs[interactionID]
	return l, ok
}

// resolveJob pops one job correlation: a BACKGROUND_JOB fires exactly once
// per job, so the mapping is consumed on first use.
func (r *legRegistry) resolveJob(jobUUID string) (interactionID string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	interactionID, ok = r.jobs[jobUUID]
	if ok {
		delete(r.jobs, jobUUID)
	}
	return interactionID, ok
}

// endLeg drops a leg and its pending job correlation (the call is over or
// was never accepted). Returns the tenant for the terminal event.
func (r *legRegistry) endLeg(interactionID string) (tenantID string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, exists := r.legs[interactionID]
	if !exists {
		return "", false
	}
	if l.jobUUID != "" {
		delete(r.jobs, l.jobUUID)
	}
	delete(r.legs, interactionID)
	return l.tenantID, true
}

// handleEvent is the eslClient's onEvent hook: translate one raw ESL event
// into the comms.ProviderEvent vocabulary. Runs inline on the event
// stream's goroutine; the client panic-isolates the hook.
func (a *Adapter) handleEvent(name string, fields map[string]string, payload string) {
	switch name {
	case evBackgroundJob:
		a.onBackgroundJob(EventField(fields, hdrJobUUID), payload)
	case evChannelAnswer:
		a.onChannelAnswer(EventField(fields, hdrUniqueID))
	case evChannelHangup:
		a.onChannelHangup(EventField(fields, hdrUniqueID), EventField(fields, hdrHangupCause))
	default:
		a.cfg.Logger.Debug("freeswitch: ignoring event outside the adapter's vocabulary",
			"event", EventField(fields, hdrEventName))
	}
}

// onBackgroundJob resolves a finished bgapi job. Successful originates move
// the leg into its first ringing phase (the switch has accepted the dial);
// failed ones end it. The api result rides the event payload
// ("+OK <uuid>" / "-ERR CAUSE").
func (a *Adapter) onBackgroundJob(jobUUID, payload string) {
	if jobUUID == "" {
		return
	}
	interactionID, ok := a.legs.resolveJob(jobUUID)
	if !ok {
		// Belt and braces: the adapter places every dial with
		// origination_uuid=<interaction id>, and a successful originate
		// reports exactly that uuid in its payload — so the payload itself
		// can resolve a correlation whose Job-UUID mapping was already
		// consumed (or was never registered because the command reply was
		// lost to a reconnect).
		if res, found := splitOK(payload); found {
			if _, legOK := a.legs.legOf(res); legOK {
				interactionID, ok = res, true
			}
		}
	}
	if !ok {
		a.cfg.Logger.Debug("freeswitch: background job for an untracked dial", "job_uuid", jobUUID)
		return
	}
	tenantID, live := a.legTenant(interactionID)
	if !live {
		return // leg already ended; nothing may move its lifecycle
	}
	switch {
	case strings.HasPrefix(payload, "+OK"):
		a.emit(&comms.ProviderEvent{
			Event:         evCallRinging,
			InteractionID: interactionID,
			TenantID:      tenantID,
		})
	case strings.HasPrefix(payload, "-ERR"):
		cause := strings.TrimSpace(strings.TrimPrefix(payload, "-ERR"))
		a.legs.endLeg(interactionID) // dial failed: the leg will never answer
		a.emit(&comms.ProviderEvent{
			Event:         evCallFailed,
			InteractionID: interactionID,
			TenantID:      tenantID,
			Detail:        cause,
		})
	default:
		a.cfg.Logger.Warn("freeswitch: background job with unclassifiable result",
			"job_uuid", jobUUID, "result", payload)
	}
}

// onChannelAnswer maps CHANNEL_ANSWER for a tracked leg to call.connected.
func (a *Adapter) onChannelAnswer(uniqueID string) {
	tenantID, ok := a.legTenant(uniqueID)
	if !ok {
		a.cfg.Logger.Debug("freeswitch: answer for an untracked channel", "unique_id", uniqueID)
		return
	}
	a.emit(&comms.ProviderEvent{
		Event:         evCallConnected,
		InteractionID: uniqueID,
		TenantID:      tenantID,
	})
}

// onChannelHangup maps CHANNEL_HANGUP for a tracked leg to call.ended with
// the switch's hangup cause as the end reason, and retires the leg: any
// late event for the channel (a BACKGROUND_JOB result racing the hangup)
// must not move the lifecycle again.
func (a *Adapter) onChannelHangup(uniqueID, cause string) {
	tenantID, ok := a.legs.endLeg(uniqueID)
	if !ok {
		a.cfg.Logger.Debug("freeswitch: hangup for an untracked channel", "unique_id", uniqueID)
		return
	}
	if strings.TrimSpace(cause) == "" {
		cause = "UNKNOWN" // the processor requires a non-empty end reason
	}
	a.emit(&comms.ProviderEvent{
		Event:         evCallEnded,
		InteractionID: uniqueID,
		TenantID:      tenantID,
		Detail:        cause,
	})
}

// legTenant reports the tenant of a still-tracked leg.
func (a *Adapter) legTenant(interactionID string) (string, bool) {
	l, ok := a.legs.legOf(interactionID)
	if !ok {
		return "", false
	}
	return l.tenantID, true
}

// emit delivers one provider event through the signed-webhook port. Events
// originate on the event stream's goroutine, which outlives any single port
// call, so delivery runs on a fresh context. A rejected delivery is logged,
// never retried here: the webhook gateway owns retry policy.
func (a *Adapter) emit(ev *comms.ProviderEvent) {
	body, err := ev.Encode()
	if err != nil {
		a.cfg.Logger.Error("freeswitch: encoding provider event", "event", ev.Event, "err", err)
		return
	}
	sig := a.cfg.Signer(body)
	if err := a.cfg.Ingest(context.Background(), providerName, body, sig); err != nil {
		a.cfg.Logger.Warn("freeswitch: provider event delivery rejected",
			"event", ev.Event, "interaction_id", ev.InteractionID, "err", err)
	}
}

// splitOK splits a "+OK <rest>" payload.
func splitOK(payload string) (rest string, ok bool) {
	if !strings.HasPrefix(payload, "+OK") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(payload, "+OK")), true
}

// ProviderEvent names the adapter emits (the closed comms vocabulary).
const (
	evCallRinging   = "call.ringing"
	evCallConnected = "call.connected"
	evCallEnded     = "call.ended"
	evCallFailed    = "call.failed"
)
