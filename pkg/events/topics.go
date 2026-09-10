package events

// Topic registry — the single source of truth for the event catalog.
// Add a topic here before emitting it anywhere; New/Validate reject unregistered
// topics so the catalog cannot drift silently.
const (
	// Interaction plane — conversations & interactions
	TopicConversationOpened   = "conversation.opened"
	TopicConversationClosed   = "conversation.closed"
	TopicInteractionCreated   = "interaction.created"
	TopicInteractionUpdated   = "interaction.updated"
	TopicInteractionAssigned  = "interaction.assigned"
	TopicInteractionCompleted = "interaction.completed"

	// Interaction plane — telephony & messaging (provider-agnostic)
	TopicCallRequested = "call.requested"
	TopicCallRinging   = "call.ringing"
	TopicCallConnected = "call.connected"
	TopicCallCompleted = "call.completed"
	TopicCallFailed    = "call.failed"
	TopicMessageSent   = "message.sent"
	TopicMessageDelivered = "message.delivered"
	TopicMessageRead      = "message.read"
	TopicMessageReceived  = "message.received"

	// Interaction plane — customers & cases
	TopicCustomerCreated = "customer.created"
	TopicCustomerUpdated = "customer.updated"
	TopicCaseOpened      = "case.opened"
	TopicCaseUpdated     = "case.updated"
	TopicCaseClosed      = "case.closed"

	// Interaction plane — routing & agents
	TopicRoutingDecisionRecorded = "routing.decision.recorded"
	TopicAgentStateChanged       = "agent.state.changed"
	TopicQueueUpdated            = "queue.updated"

	// Intelligence plane
	TopicAIInvocationCompleted = "ai.invocation.completed"
	TopicAISuggestionIssued    = "ai.suggestion.issued"

	// Execution plane
	TopicWorkflowStarted       = "workflow.started"
	TopicWorkflowStepCompleted = "workflow.step.completed"
	TopicWorkflowCompleted     = "workflow.completed"
	TopicWorkflowFailed        = "workflow.failed"
	TopicToolExecutionAudited  = "tool.execution.audited"

	// Control plane
	TopicUsageRecorded   = "usage.recorded"
	TopicAuditRecorded   = "audit.recorded"
	TopicWebhookReceived = "webhook.received"
)

var registry = map[string]struct{}{}

func init() {
	for _, t := range []string{
		TopicConversationOpened, TopicConversationClosed,
		TopicInteractionCreated, TopicInteractionUpdated, TopicInteractionAssigned, TopicInteractionCompleted,
		TopicCallRequested, TopicCallRinging, TopicCallConnected, TopicCallCompleted, TopicCallFailed,
		TopicMessageSent, TopicMessageDelivered, TopicMessageRead, TopicMessageReceived,
		TopicCustomerCreated, TopicCustomerUpdated,
		TopicCaseOpened, TopicCaseUpdated, TopicCaseClosed,
		TopicRoutingDecisionRecorded, TopicAgentStateChanged, TopicQueueUpdated,
		TopicAIInvocationCompleted, TopicAISuggestionIssued,
		TopicWorkflowStarted, TopicWorkflowStepCompleted, TopicWorkflowCompleted, TopicWorkflowFailed,
		TopicToolExecutionAudited,
		TopicUsageRecorded, TopicAuditRecorded, TopicWebhookReceived,
	} {
		registry[t] = struct{}{}
	}
}

// IsRegistered reports whether the topic exists in the catalog.
func IsRegistered(topic string) bool {
	_, ok := registry[topic]
	return ok
}

// Topics returns the registered catalog in stable order (used by docs/tests).
func Topics() []string {
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	// insertion order is lost in a map; sort for determinism
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
