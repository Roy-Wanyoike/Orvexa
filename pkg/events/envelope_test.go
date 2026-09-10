package events

import (
	"strings"
	"testing"
	"time"
)

func validEnvelope() *Envelope {
	return &Envelope{
		ID:       "019393a0-1000-7000-8000-000000000001",
		Type:     TopicInteractionCreated,
		Source:   "orvexa-test",
		TenantID: "tenant-1",
		Time:     time.Now().UTC(),
		Data:     map[string]any{"channel": "whatsapp"},
	}
}

func TestEnvelopeValidateAcceptsWellFormed(t *testing.T) {
	e := validEnvelope()
	if err := e.Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestEnvelopeValidateRejectsUnregisteredTopic(t *testing.T) {
	e := validEnvelope()
	e.Type = "mystery.subject.verb"
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "unregistered topic") {
		t.Fatalf("want unregistered-topic error, got %v", err)
	}
}

func TestEnvelopeValidateRejectsMissingTenant(t *testing.T) {
	e := validEnvelope()
	e.TenantID = ""
	if err := e.Validate(); err == nil {
		t.Fatal("missing tenant must be rejected")
	}
}

func TestEnvelopeValidateRejectsBadID(t *testing.T) {
	e := validEnvelope()
	e.ID = "not-a-uuid"
	if err := e.Validate(); err == nil {
		t.Fatal("bad id must be rejected")
	}
}

func TestNewFillsDefaultsAndValidates(t *testing.T) {
	e, err := New(TopicUsageRecorded, "orvexa-test", "usage-1", "tenant-1", "corr-9",
		map[string]any{"metric": "ai_tokens", "amount": 42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.ID == "" || e.Time.IsZero() || e.Data == nil {
		t.Fatal("New must fill id/time/data")
	}
	if e.CorrelationID != "corr-9" {
		t.Fatal("correlation id lost")
	}
}

func TestTopicCatalogIsDeterministic(t *testing.T) {
	a, b := Topics(), Topics()
	if len(a) == 0 || len(a) != len(b) {
		t.Fatal("catalog must be non-empty and stable in size")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("catalog ordering unstable at %d: %s vs %s", i, a[i], b[i])
		}
	}
	if !IsRegistered(TopicCallCompleted) || IsRegistered("call.something_else") {
		t.Fatal("registry membership wrong")
	}
}
