package whatsappcloud

import "sync"

// msgRef is the correlation record a send registers: the Graph message id
// (wamid) mapped back to the platform interaction + tenant it belongs to.
// Status webhooks carry only the wamid, so this ledger is what lets
// translated receipts be stamped with the platform identity the processor
// requires (comms.ProviderEvent.InteractionID/TenantID).
type msgRef struct {
	interactionID string
	tenantID      string
}

// msgLedger is the send→receipt correlation table. Scope note (honest
// limitation, documented in the package README): the ledger is in-memory and
// process-local. Statuses arriving for wamids this process never sent —
// after a restart, or for messages sent by another adapter instance — cannot
// be resolved and are reported by HandleWebhook as unknown_message_id
// instead of being silently attributed. A durable/TTL'd mapping (outbox row
// keyed by wamid) is the production hardening step for the cmd wiring wave.
type msgLedger struct {
	mu      sync.Mutex
	byWAMID map[string]msgRef
}

func newMsgLedger() *msgLedger {
	return &msgLedger{byWAMID: map[string]msgRef{}}
}

// remember registers the correlation for a successful send. Re-sending the
// same InteractionID (Simulator idempotency semantics) registers the fresh
// wamid alongside the old one — both resolve to the same interaction, and
// the processor's same-state no-op keeps the lifecycle honest.
func (l *msgLedger) remember(wamid, interactionID, tenantID string) {
	if wamid == "" {
		return // unreachable via sendResponse.messageID's guard; defensive
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byWAMID[wamid] = msgRef{interactionID: interactionID, tenantID: tenantID}
}

// lookup resolves a wamid to its platform identity.
func (l *msgLedger) lookup(wamid string) (msgRef, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ref, ok := l.byWAMID[wamid]
	return ref, ok
}
