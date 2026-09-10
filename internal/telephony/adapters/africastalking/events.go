package africastalking

import (
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Status callback vocabulary (issue #28 contract). AT reports call progress
// asynchronously; the adapter translates each report into exactly one
// ProviderEvent. The set is closed: an unknown status is a typed error,
// never a guessed event.
const (
	StatusQueued     = "Queued"     // dial accepted, destination ringing
	StatusInProgress = "InProgress" // call answered / in progress
	StatusCompleted  = "Completed"  // call finished normally
	StatusFailed     = "Failed"     // carrier could not complete the call
	StatusBusy       = "Busy"       // destination busy
	StatusNoAnswer   = "NoAnswer"   // destination did not answer
)

// StatusCallback is one AT voice status notification. The adapter sends
// clientRequestId=<interaction id> at dial time; AT echoes it on callbacks,
// which — together with the adapter's session registry (sessionId → leg) —
// is how a callback is routed back to the originating interaction and
// tenant. A callback that carries neither key is rejected, never guessed.
type StatusCallback struct {
	SessionID       string `json:"sessionId"`
	ClientRequestID string `json:"clientRequestId"`
	PhoneNumber     string `json:"phoneNumber,omitempty"`
	Status          string `json:"status"`
	HangupCause     string `json:"hangupCause,omitempty"`
	DurationSecs    int    `json:"durationSecs,omitempty"`
	Timestamp       string `json:"timestamp,omitempty"` // provider-side occurrence, RFC3339
}

// endReason returns the closed-vocabulary reason recorded on call.ended for
// a terminal status. The processor persists Detail as the interaction's
// end_reason — the audit trail for why the call ended.
func endReason(status string) string {
	switch status {
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusBusy:
		return "busy"
	case StatusNoAnswer:
		return "no_answer"
	}
	return ""
}

// IsTerminal reports whether a status ends the call leg.
func IsTerminal(status string) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusBusy, StatusNoAnswer:
		return true
	}
	return false
}

// TranslateStatus converts one AT status callback into the platform's
// ProviderEvent contract (pure — no I/O, no registry access):
//
//	Queued     → call.ringing            (dial accepted, destination ringing)
//	InProgress → call.connected          (call answered)
//	Completed  → call.ended  (completed)
//	Failed     → call.ended  (failed)    ─ terminal causes all land on
//	Busy       → call.ended  (busy)        call.ended with the reason in
//	NoAnswer   → call.ended  (no_answer)   Detail (the processor's audit
//	                                       trail for end_reason).
//
// The mapping is the one pinned by issue #28: AT's terminal causes are call
// endings, not platform "failures" of the interaction — a busy/no-answer
// leg is an ended call with a reason, exactly as a carrier would report it.
// interactionID and tenantID come from the caller's routing resolution
// (clientRequestId echo or session registry); TranslateStatus never guesses.
func TranslateStatus(cb *StatusCallback, interactionID, tenantID string) (*comms.ProviderEvent, error) {
	if cb == nil {
		return nil, apperrors.Invalid("at.callback_malformed", "status callback payload is required")
	}
	if interactionID == "" || tenantID == "" {
		return nil, apperrors.NotFound("at.callback_unroutable",
			"status callback could not be routed to an interaction (no clientRequestId or known sessionId)")
	}

	ev := &comms.ProviderEvent{
		InteractionID: interactionID,
		TenantID:      tenantID,
		Timestamp:     callbackTimestamp(cb.Timestamp),
	}
	switch cb.Status {
	case StatusQueued:
		ev.Event = "call.ringing"
	case StatusInProgress:
		ev.Event = "call.connected"
	case StatusCompleted, StatusFailed, StatusBusy, StatusNoAnswer:
		ev.Event = "call.ended"
		reason := endReason(cb.Status)
		if cause := cleanMsg(cb.HangupCause); cause != "" && cause != reason {
			ev.Detail = reason + " (" + cause + ")"
		} else {
			ev.Detail = reason
		}
	default:
		return nil, apperrors.Invalid("at.unknown_status", "unsupported Africa's Talking status "+truncate(cb.Status, 64))
	}
	return ev, nil
}

// callbackTimestamp passes through a parseable provider timestamp; anything
// else yields "" so the event encoder stamps the platform clock (the
// processor requires RFC3339-shaped values, and an unparsable carrier stamp
// must not corrupt the timeline).
func callbackTimestamp(v string) string {
	if v == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		return ""
	}
	return v
}
