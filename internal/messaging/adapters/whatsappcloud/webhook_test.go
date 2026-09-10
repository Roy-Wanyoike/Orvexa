package whatsappcloud

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// ─── fake-gateway payload builders (shared with the conformance wiring) ─────

// graphStatusEntry renders one statuses[] element as the fake gateway would
// deliver it. The timestamp matches Meta's documented webhook examples.
func graphStatusEntry(wamid, status string, errs ...map[string]any) map[string]any {
	s := map[string]any{
		"id":           wamid,
		"status":       status,
		"timestamp":    "1664268184",
		"recipient_id": "254711111111",
	}
	if len(errs) > 0 {
		s["errors"] = errs
	}
	return s
}

// graphStatusErrorEntry renders one errors[] element on a failed status.
func graphStatusErrorEntry(code int, title, details string) map[string]any {
	e := map[string]any{"code": code, "title": title, "message": title}
	if details != "" {
		e["error_data"] = map[string]any{"messaging_product": "whatsapp", "details": details}
	}
	return e
}

// statusWebhookBody renders a full Cloud API statuses webhook payload: one
// WABA entry, one messages change, the given statuses.
func statusWebhookBody(t *testing.T, phoneNumberID string, statuses ...map[string]any) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"object": "whatsapp_business_account",
		"entry": []any{map[string]any{
			"id": "waba-fixture-0",
			"changes": []any{map[string]any{
				"field": "messages",
				"value": map[string]any{
					"messaging_product": "whatsapp",
					"metadata": map[string]any{
						"display_phone_number": "+254700000001",
						"phone_number_id":      phoneNumberID,
					},
					"statuses": statuses,
				},
			}},
		}},
	})
}

// testResolver resolves the wamids a test registered; anything else is
// unknown (the production ledger's verdict for unregistered wamids).
func testResolver(known map[string]msgRef) StatusResolver {
	return func(wamid string) (interactionID, tenantID string, ok bool) {
		ref, ok := known[wamid]
		if !ok {
			return "", "", false
		}
		return ref.interactionID, ref.tenantID, true
	}
}

// ─── TranslateStatus: the pure translation table ────────────────────────────

// TestTranslateStatusTable walks the documented status→event table: exact
// event names, identity stamping through the resolver, timestamp conversion,
// failure reasons in Detail, and the loud-error edges.
func TestTranslateStatusTable(t *testing.T) {
	const interaction = "019393a0-1000-7000-8000-00000000cafe"
	known := map[string]msgRef{
		"wamid.known": {interactionID: interaction, tenantID: "tenant-1"},
	}
	resolve := testResolver(known)

	t.Run("sent delivered read map 1:1", func(t *testing.T) {
		for status, wantEvent := range map[string]string{
			"sent":      "message.sent",
			"delivered": "message.delivered",
			"read":      "message.read",
		} {
			events, err := TranslateStatus(
				statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.known", status)),
				resolve)
			if err != nil {
				t.Fatalf("status %q: unexpected error: %v", status, err)
			}
			if len(events) != 1 {
				t.Fatalf("status %q: exactly one event expected, got %d", status, len(events))
			}
			ev := events[0]
			if ev.Event != wantEvent {
				t.Errorf("status %q must translate to %q, got %q", status, wantEvent, ev.Event)
			}
			// Invariant: receipts are stamped with the platform identity the
			// send registered — never with provider-side substitutes.
			if ev.InteractionID != interaction || ev.TenantID != "tenant-1" {
				t.Errorf("status %q: identity stamping wrong: %+v", status, ev)
			}
			if ev.Detail != "" {
				t.Errorf("status %q must not carry a Detail, got %q", status, ev.Detail)
			}
		}
	})

	t.Run("graph unix timestamp becomes RFC3339 UTC", func(t *testing.T) {
		events, err := TranslateStatus(
			statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.known", "sent")), resolve)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ts := events[0].Timestamp
		// Meta's fixture value 1664268184 must survive as the same instant,
		// rendered RFC3339 in UTC (the ProviderEvent wire contract).
		parsed, perr := time.Parse(time.RFC3339, ts)
		if perr != nil {
			t.Fatalf("timestamp must be RFC3339, got %q: %v", ts, perr)
		}
		if parsed.Unix() != 1664268184 || !strings.HasSuffix(ts, "Z") {
			t.Errorf("timestamp must render unix 1664268184 as RFC3339 UTC, got %q", ts)
		}
	})

	t.Run("absent timestamp yields empty (delivery stamps ingest time)", func(t *testing.T) {
		raw := statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.known", "sent"))
		// Strip the timestamp field the builder always writes.
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		entry := payload["entry"].([]any)[0].(map[string]any)
		change := entry["changes"].([]any)[0].(map[string]any)
		value := change["value"].(map[string]any)
		st := value["statuses"].([]any)[0].(map[string]any)
		delete(st, "timestamp")
		raw = mustJSON(t, payload)

		events, err := TranslateStatus(raw, resolve)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if events[0].Timestamp != "" {
			t.Errorf("absent provider timestamp must yield empty, got %q", events[0].Timestamp)
		}
	})

	t.Run("failed carries the primary error reason in Detail", func(t *testing.T) {
		events, err := TranslateStatus(
			statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.known", "failed",
				graphStatusErrorEntry(131047, "Re-engagement message",
					"more than 24 hours have passed since the customer last replied"),
				graphStatusErrorEntry(1, "secondary", ""),
			)), resolve)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(events) != 1 || events[0].Event != "message.failed" {
			t.Fatalf("expected one message.failed event, got %+v", events)
		}
		d := events[0].Detail
		// Invariant: the operator-facing reason names the Graph code, Meta's
		// title and the error_data details — the first error is the primary
		// reason, later ones are context.
		if !strings.Contains(d, "graph code 131047") ||
			!strings.Contains(d, "Re-engagement message") ||
			!strings.Contains(d, "24 hours") {
			t.Errorf("failed Detail must carry code+title+details, got %q", d)
		}
	})

	t.Run("failed without errors still fails loudly", func(t *testing.T) {
		events, err := TranslateStatus(
			statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.known", "failed")), resolve)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if events[0].Event != "message.failed" {
			t.Fatalf("expected message.failed, got %q", events[0].Event)
		}
		if events[0].Detail == "" {
			t.Error("a failure without error details must still carry a Detail")
		}
	})

	t.Run("edges fail loudly, not with guessed events", func(t *testing.T) {
		cases := []struct {
			name     string
			body     []byte
			wantCode string
			wantKind apperrors.Kind
		}{
			{
				name:     "unknown status value",
				body:     statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.known", "deleted")),
				wantCode: "whatsapp.webhook_unknown_status",
				wantKind: apperrors.KindInvalid,
			},
			{
				name:     "status without message id",
				body:     statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("", "sent")),
				wantCode: "whatsapp.webhook_status_missing_id",
				wantKind: apperrors.KindInvalid,
			},
			{
				name:     "unresolvable wamid",
				body:     statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.never-sent", "delivered")),
				wantCode: "whatsapp.unknown_message_id",
				wantKind: apperrors.KindNotFound,
			},
			{
				name:     "nil resolver cannot attribute anything",
				body:     statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.known", "sent")),
				wantCode: "whatsapp.unknown_message_id",
				wantKind: apperrors.KindNotFound,
			},
			{
				name:     "unparsable body",
				body:     []byte("<html>proxy noise</html>"),
				wantCode: "whatsapp.webhook_malformed",
				wantKind: apperrors.KindInvalid,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var resolve StatusResolver
				if tc.name != "nil resolver cannot attribute anything" {
					resolve = testResolver(known)
				}
				events, err := TranslateStatus(tc.body, resolve)
				if err == nil {
					t.Fatalf("expected an error, got events %+v", events)
				}
				requireAppErr(t, err, tc.wantKind, tc.wantCode)
			})
		}
	})

	t.Run("payload without statuses translates to nothing", func(t *testing.T) {
		// An inbound-message change (or an empty payload) is not this
		// adapter's problem: nothing to attribute is not an error.
		raw := mustJSON(t, map[string]any{
			"object": "whatsapp_business_account",
			"entry": []any{map[string]any{
				"id": "waba-fixture-0",
				"changes": []any{map[string]any{
					"field": "messages",
					"value": map[string]any{
						"messaging_product": "whatsapp",
						"metadata":          map[string]any{"phone_number_id": testPhoneNumberID},
						"contacts":          []any{},
					},
				}},
			}},
		})
		events, err := TranslateStatus(raw, resolve)
		if err != nil {
			t.Fatalf("a status-less payload must not error, got: %v", err)
		}
		if len(events) != 0 {
			t.Errorf("expected no events, got %+v", events)
		}
	})

	t.Run("batched entries and changes keep arrival order", func(t *testing.T) {
		// Meta batches: several entries × changes × statuses in one payload.
		// The translation must preserve arrival order — the lifecycle is
		// order-sensitive (sent before delivered).
		raw := mustJSON(t, map[string]any{
			"object": "whatsapp_business_account",
			"entry": []any{
				map[string]any{"id": "waba-0", "changes": []any{map[string]any{
					"field": "messages",
					"value": map[string]any{"statuses": []any{
						map[string]any{"id": "wamid.known", "status": "sent", "timestamp": "1664268184"},
					}},
				}}},
				map[string]any{"id": "waba-1", "changes": []any{
					map[string]any{
						"field": "messages",
						"value": map[string]any{"statuses": []any{
							map[string]any{"id": "wamid.known", "status": "delivered", "timestamp": "1664268190"},
						}},
					},
					map[string]any{
						"field": "messages",
						"value": map[string]any{"statuses": []any{
							map[string]any{"id": "wamid.known", "status": "read", "timestamp": "1664268200"},
						}},
					},
				}},
			},
		})
		events, err := TranslateStatus(raw, resolve)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"message.sent", "message.delivered", "message.read"}
		if len(events) != len(want) {
			t.Fatalf("expected %d events, got %d", len(want), len(events))
		}
		for i, w := range want {
			if events[i].Event != w {
				t.Errorf("event #%d must be %q, got %q", i, w, events[i].Event)
			}
		}
	})
}

// ─── HandleWebhook: the full status path through the fake gateway ───────────

// TestHandleWebhookFullPathE2E is the issue's acceptance criterion: the
// status path flows end-to-end through the fake gateway — Send is accepted
// by the fake Graph, the fake delivers Meta's async status callback, the
// adapter translates it and the signed receipts drive the interaction
// lifecycle (sent → delivered keeps it active; read completes it; a later
// failure fails it with the provider reason persisted).
func TestHandleWebhookFullPathE2E(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	// Wire the fake's async callback: after each accepted send, the fake
	// stands in for Meta's webhook and delivers sent+delivered for the fresh
	// wamid through HandleWebhook — the exact production path (verify at the
	// gateway, then hand the raw body to the adapter).
	fake.onSend = func(wamid string) {
		awaitWAMIDRegistration(t, p, wamid)
		body := statusWebhookBody(t, testPhoneNumberID,
			graphStatusEntry(wamid, "sent"),
			graphStatusEntry(wamid, "delivered"),
		)
		if err := p.HandleWebhook(context.Background(), body); err != nil {
			t.Errorf("async status webhook for %s: %v", wamid, err)
		}
	}

	msg := validMsg()
	rec.Seed(msg.TenantID, msg.InteractionID, interactions.StatusActive)
	if err := p.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Meta's receipts arrive after Send returns — poll the settle window.
	if !rec.WaitApplied(2, 2*time.Second) {
		t.Fatalf("timed out waiting for the sent+delivered receipts, got %+v", rec.Applied())
	}
	evs := rec.AppliedFor(msg.TenantID, msg.InteractionID)
	if len(evs) != 2 || evs[0].Event != "message.sent" || evs[1].Event != "message.delivered" {
		t.Fatalf("lifecycle must be sent → delivered, got %+v", evs)
	}
	for _, ev := range evs {
		if ev.InteractionID != msg.InteractionID || ev.TenantID != msg.TenantID {
			t.Errorf("receipt %q must carry interaction=%s tenant=%s, got %+v",
				ev.Event, msg.InteractionID, msg.TenantID, ev)
		}
	}
	// Invariant: every receipt is SIGNED and stamped with the provider label
	// the fail-closed gateway routes by.
	for i, d := range rec.Deliveries() {
		if d.Provider != ProviderName {
			t.Errorf("delivery #%d must carry provider label %q, got %q", i, ProviderName, d.Provider)
		}
		if d.Signature == "" || d.Signature != testSigner(d.Body) {
			t.Errorf("delivery #%d must be signed by the configured signer, got %q", i, d.Signature)
		}
	}
	// sent+delivered leave the conversation open: completed is message.read's.
	if st, _ := rec.Status(msg.TenantID, msg.InteractionID); st != interactions.StatusActive {
		t.Errorf("interaction must stay active after sent+delivered, got %s", st)
	}

	// The read receipt completes the conversation (active → completed).
	readBody := statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.fake-1", "read"))
	if err := p.HandleWebhook(context.Background(), readBody); err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if st, _ := rec.Status(msg.TenantID, msg.InteractionID); st != interactions.StatusCompleted {
		t.Errorf("read receipt must complete the interaction, got %s", st)
	}

	// A failed status on a second conversation fails it, with Meta's reason
	// persisted in the event Detail (the processor passes it as end_reason).
	msg2 := validMsg()
	msg2.InteractionID = uuid.NewString()
	rec.Seed(msg2.TenantID, msg2.InteractionID, interactions.StatusActive)
	if err := p.Send(context.Background(), msg2); err != nil {
		t.Fatalf("Send #2: %v", err)
	}
	if !rec.WaitApplied(4, 2*time.Second) {
		t.Fatalf("timed out waiting for send #2 receipts, got %+v", rec.Applied())
	}
	failedBody := statusWebhookBody(t, testPhoneNumberID,
		graphStatusEntry("wamid.fake-2", "failed",
			graphStatusErrorEntry(131047, "Re-engagement message",
				"more than 24 hours have passed since the customer last replied")))
	if err := p.HandleWebhook(context.Background(), failedBody); err != nil {
		t.Fatalf("failed status: %v", err)
	}
	if st, _ := rec.Status(msg2.TenantID, msg2.InteractionID); st != interactions.StatusFailed {
		t.Errorf("failed status must fail the interaction, got %s", st)
	}
	var failedDetail string
	for _, d := range rec.Deliveries() {
		if d.Event.InteractionID == msg2.InteractionID && d.Event.Event == "message.failed" {
			failedDetail = d.Event.Detail
		}
	}
	if !strings.Contains(failedDetail, "graph code 131047") || !strings.Contains(failedDetail, "24 hours") {
		t.Errorf("failed receipt must persist the provider reason, got %q", failedDetail)
	}
}

// TestHandleWebhookUnknownWAMIDNotAttributed pins the honest-attribution
// rule: a status for a wamid this adapter never sent (restart, other
// instance, other phone number) is reported, never attributed to a guessed
// interaction.
func TestHandleWebhookUnknownWAMIDNotAttributed(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	body := statusWebhookBody(t, testPhoneNumberID, graphStatusEntry("wamid.never-sent", "delivered"))
	err := p.HandleWebhook(context.Background(), body)
	requireAppErr(t, err, apperrors.KindNotFound, "whatsapp.unknown_message_id")
	if got := len(rec.Deliveries()); got != 0 {
		t.Errorf("an unattributable status must not be delivered, got %d deliveries", got)
	}
}

// TestHandleWebhookBatchPartialFailure pins the batch semantics: one bad
// status must not block its honest siblings — valid statuses are delivered,
// the bad one is reported.
func TestHandleWebhookBatchPartialFailure(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	p.ledger.remember("wamid.mine", validMsg().InteractionID, "tenant-1")
	rec.Seed("tenant-1", validMsg().InteractionID, interactions.StatusActive)
	body := statusWebhookBody(t, testPhoneNumberID,
		graphStatusEntry("wamid.someone-elses", "delivered"),
		graphStatusEntry("wamid.mine", "sent"),
	)
	err := p.HandleWebhook(context.Background(), body)

	// Both halves of the contract: the sibling was delivered…
	if got := len(rec.Applied()); got != 1 {
		t.Errorf("the honest sibling must still be delivered, got %d applied", got)
	}
	// …and the bad entry reported.
	requireAppErr(t, err, apperrors.KindNotFound, "whatsapp.unknown_message_id")
}

// TestHandleWebhookSurfacesIngestFailures pins that processor/gateway
// rejections are surfaced, never swallowed — and that translation errors and
// ingest errors can both ride in one joined report.
func TestHandleWebhookSurfacesIngestFailures(t *testing.T) {
	sentinel := errors.New("gateway down")
	p, err := New(Config{
		PhoneNumberID: testPhoneNumberID,
		AccessToken:   testAccessToken,
		Ingest:        func(context.Context, string, []byte, string) error { return sentinel },
		Signer:        testSigner,
		Retry:         RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.ledger.remember("wamid.mine", validMsg().InteractionID, "tenant-1")

	body := statusWebhookBody(t, testPhoneNumberID,
		graphStatusEntry("wamid.never-sent", "delivered"), // translation error
		graphStatusEntry("wamid.mine", "sent"),            // delivered, ingest rejects
	)
	err = p.HandleWebhook(context.Background(), body)

	// Invariant: BOTH failure classes surface in one report — the caller
	// (webhook HTTP layer) decides retry/requeue semantics from it.
	requireAppErr(t, err, apperrors.KindNotFound, "whatsapp.unknown_message_id")
	if !errors.Is(err, sentinel) {
		t.Errorf("the ingest rejection must surface in the joined error, got: %v", err)
	}
}

// ─── idempotency: the Simulator contract, pinned adapter-side ───────────────

// TestSendDuplicateInteractionIDMatchesSimulatorSemantics pins the
// retry-safety contract at the adapter level: re-sending the SAME
// InteractionID is accepted (Graph has no idempotency key — the core retries
// Send after transport timeouts and must not fail real interactions on
// harmless retries), both wamids resolve to the same interaction, and the
// duplicate receipts re-apply cleanly (the processor's same-state no-op).
func TestSendDuplicateInteractionIDMatchesSimulatorSemantics(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	msg := validMsg()
	rec.Seed(msg.TenantID, msg.InteractionID, interactions.StatusActive)

	if err := p.Send(context.Background(), msg); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	// Invariant: the duplicate is ACCEPTED, not rejected — rejecting
	// duplicates would fail real interactions when the core retries.
	if err := p.Send(context.Background(), msg); err != nil {
		t.Fatalf("duplicate Send with the same InteractionID must be accepted (Simulator semantics), got: %v", err)
	}

	// Invariant: each POST yields a fresh wamid and both ledger entries
	// resolve to the same platform interaction + tenant.
	ref1, ok1 := p.ledger.lookup("wamid.fake-1")
	ref2, ok2 := p.ledger.lookup("wamid.fake-2")
	if !ok1 || !ok2 {
		t.Fatalf("both sends must register their wamid, ok1=%v ok2=%v", ok1, ok2)
	}
	if ref1.interactionID != msg.InteractionID || ref2.interactionID != msg.InteractionID ||
		ref1.tenantID != msg.TenantID || ref2.tenantID != msg.TenantID {
		t.Errorf("both wamids must resolve to the same interaction+tenant, got %+v and %+v", ref1, ref2)
	}

	// Invariant: receipts for BOTH wamids stamp the same interaction and
	// re-apply cleanly (reference re-emission behavior).
	body := statusWebhookBody(t, testPhoneNumberID,
		graphStatusEntry("wamid.fake-1", "sent"),
		graphStatusEntry("wamid.fake-1", "delivered"),
		graphStatusEntry("wamid.fake-2", "sent"),
		graphStatusEntry("wamid.fake-2", "delivered"),
	)
	if err := p.HandleWebhook(context.Background(), body); err != nil {
		t.Fatalf("receipts for a duplicated send must all apply cleanly: %v", err)
	}
	evs := rec.AppliedFor(msg.TenantID, msg.InteractionID)
	want := []string{"message.sent", "message.delivered", "message.sent", "message.delivered"}
	if len(evs) != len(want) {
		t.Fatalf("expected the re-emitted receipt sequence %v, got %+v", want, evs)
	}
	for i, w := range want {
		if evs[i].Event != w {
			t.Errorf("event #%d must be %q, got %q", i, w, evs[i].Event)
		}
	}
	if st, _ := rec.Status(msg.TenantID, msg.InteractionID); st != interactions.StatusActive {
		t.Errorf("a duplicate send must leave the interaction active (no terminal drift), got %s", st)
	}
}

// awaitWAMIDRegistration models Meta's delivery ordering inside the fake:
// a real status callback arrives over a separate connection, strictly after
// the send's HTTP response reached the sender — i.e. after the adapter has
// registered the fresh wamid in its correlation ledger. The fake's delivery
// goroutine can outrun that client-side registration, so async harnesses
// wait for the registration (the ledger IS the synchronization point)
// before translating. Without this, a hot goroutine would race Send's
// ledger.remember and be (honestly, but uselessly) refused as
// whatsapp.unknown_message_id.
func awaitWAMIDRegistration(t *testing.T, p *Provider, wamid string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := p.ledger.lookup(wamid); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("wamid %s was never registered by a send; status delivery would be refused", wamid)
			return
		}
		time.Sleep(time.Millisecond)
	}
}
