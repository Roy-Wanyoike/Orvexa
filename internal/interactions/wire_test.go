package interactions

// Wire-contract regression tests for issue #101: the interaction plane's
// request and response JSON is the snake_case documented by
// api/openapi/orvexa-v1.yaml — never Go field names.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRecWireIsSnakeCase pins the response wire contract with a golden JSON
// body: field ORDER follows the struct, keys are the documented snake_case
// names, and absent optionals are omitted (omitempty) — a contract test or
// spec-generated client can rely on exactly these key names.
func TestRecWireIsSnakeCase(t *testing.T) {
	started := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	answered := started.Add(2 * time.Second)
	ended := started.Add(61 * time.Second)
	rec := &Rec{
		ID:              "9be1a3e6-3f55-4a3f-9d0a-6f2b1f0f1a01",
		TenantID:        "11111111-1111-1111-1111-111111111111",
		ConversationID:  "22aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01",
		CustomerID:      "33bbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbb01",
		Channel:         ChannelVoice,
		Direction:       DirectionOutbound,
		Status:          StatusCompleted,
		Source:          "orvexa-voice",
		Destination:     "+254712345678",
		AssignedAgentID: "44cccccc-cccc-cccc-cccc-cccccccccc01",
		StartedAt:       started,
		AnsweredAt:      &answered,
		EndedAt:         &ended,
		EndReason:       "hangup",
		Provider:        "simulator",
		ProviderRef:     "CA-1",
		IdempotencyKey:  "call:CA-1",
		Attributes:      map[string]any{"options": map[string]any{"x": 1}},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := string(b)
	want := `{"id":"9be1a3e6-3f55-4a3f-9d0a-6f2b1f0f1a01",` +
		`"tenant_id":"11111111-1111-1111-1111-111111111111",` +
		`"conversation_id":"22aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01",` +
		`"customer_id":"33bbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbb01",` +
		`"channel":"voice","direction":"outbound","status":"completed",` +
		`"source":"orvexa-voice","destination":"+254712345678",` +
		`"assigned_agent_id":"44cccccc-cccc-cccc-cccc-cccccccccc01",` +
		`"started_at":"2026-09-12T10:00:00Z",` +
		`"answered_at":"2026-09-12T10:00:02Z",` +
		`"ended_at":"2026-09-12T10:01:01Z","end_reason":"hangup",` +
		`"provider":"simulator","provider_ref":"CA-1",` +
		`"idempotency_key":"call:CA-1",` +
		`"attributes":{"options":{"x":1}}}`
	if got != want {
		t.Fatalf("Rec wire drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestRecZeroValueOmitsEmptyOptionals: the omitempty set keeps unset
// optionals off the wire (conversations/cases convention) while the identity
// and lifecycle keys stay present in every response.
func TestRecZeroValueOmitsEmptyOptionals(t *testing.T) {
	b, err := json.Marshal(&Rec{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"id", "tenant_id", "conversation_id", "customer_id",
		"channel", "direction", "status", "source", "destination", "started_at", "provider", "attributes"} {
		if _, ok := m[key]; !ok {
			t.Errorf("stable key %q missing from zero-value Rec", key)
		}
	}
	for _, key := range []string{"assigned_agent_id", "assigned_queue_id", "answered_at",
		"ended_at", "end_reason", "provider_ref", "idempotency_key"} {
		if _, ok := m[key]; ok {
			t.Errorf("optional key %q must be omitted when unset, got %v", key, m[key])
		}
	}
	for key := range m {
		if strings.ToUpper(key[:1]) == key[:1] {
			t.Errorf("Go-cased key %q on the wire (#101 regression)", key)
		}
	}
}

// decodeStrictly mirrors httpserver.decodeJSON's semantics (the handler
// decoder rejects unknown fields) without importing the HTTP layer.
func decodeStrictly(t *testing.T, body string) CreateInput {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var in CreateInput
	if err := dec.Decode(&in); err != nil {
		t.Fatalf("documented body must bind: %v", err)
	}
	return in
}

// TestCreateInputBindsDocumentedSnakeCaseBody: the exact InteractionCreate
// schema of api/openapi/orvexa-v1.yaml (and the issue #101 repro body) binds
// every documented property — previously "customer_id" was rejected 422.
func TestCreateInputBindsDocumentedSnakeCaseBody(t *testing.T) {
	in := decodeStrictly(t, `{"customer_id":"c-1","conversation_id":"cv-1",`+
		`"channel":"whatsapp","direction":"inbound","source":"+254712345678",`+
		`"destination":"+254700000000","provider":"twilio","provider_ref":"SM-1",`+
		`"idempotency_key":"prov:SM-1:replay"}`)
	if in.CustomerID != "c-1" || in.ConversationID != "cv-1" {
		t.Errorf("identity fields not bound: %+v", in)
	}
	if in.Channel != ChannelWhatsApp || in.Direction != DirectionInbound {
		t.Errorf("typed enums not bound: %+v", in)
	}
	if in.Source != "+254712345678" || in.Destination != "+254700000000" {
		t.Errorf("endpoints not bound: %+v", in)
	}
	if in.Provider != "twilio" || in.ProviderRef != "SM-1" {
		t.Errorf("provider fields not bound: %+v", in)
	}
	if in.IdempotencyKey != "prov:SM-1:replay" {
		t.Errorf("idempotency_key not bound: %+v", in)
	}
}

// TestCreateInputRejectsUndocumentedProperties: attributes is NOT part of the
// InteractionCreate schema, so the strict decoder must keep rejecting it —
// adding tags must not silently grow the accepted request surface.
func TestCreateInputRejectsUndocumentedProperties(t *testing.T) {
	bodies := []string{
		`{"customer_id":"c-1","channel":"chat","direction":"inbound","source":"s","destination":"d","attributes":{"x":1}}`,
		`{"customer_id":"c-1","channel":"chat","direction":"inbound","source":"s","destination":"d","tenant_id":"t-1"}`,
	}
	for _, body := range bodies {
		dec := json.NewDecoder(strings.NewReader(body))
		dec.DisallowUnknownFields()
		var in CreateInput
		if err := dec.Decode(&in); err == nil {
			t.Errorf("undocumented body accepted: %s", body)
		}
	}
}
