package conformance

import (
	"context"
	"strings"
	"testing"

	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// messageCase is one row of the shared invalid-message taxonomy table: a
// mutation that makes a valid message invalid in exactly one class (or an
// adversarial boundary probe that must stay valid), and the exact core code
// the failure must carry. Empty want = the message must be accepted.
type messageCase struct {
	name   string
	mutate func(m *messaging.Message)
	want   string
}

// invalidMessageCases is the authoritative invalid-outbound-message table.
// It is shared by the ValidateMessage gate (always enforced) and the opt-in
// adapter re-validation scenario, so the two can never drift. It contains
// adversarial probes on purpose:
//
//   - whitespace-only inputs (validation must trim, not just check empty);
//   - the 4096/4097 multibyte rune boundary (validation must count runes,
//     not bytes — 4096 'é' is 8192 bytes and MUST pass);
//   - precedence probes (a message failing two rules must carry the code of
//     the first rule in ValidateMessage's documented order).
func invalidMessageCases() []messageCase {
	return []messageCase{
		{
			name:   "missing interaction id",
			mutate: func(m *messaging.Message) { m.InteractionID = "" },
			want:   "message.interaction_required",
		},
		{
			name:   "empty channel",
			mutate: func(m *messaging.Message) { m.Channel = "" },
			want:   "message.invalid_channel",
		},
		{
			name:   "unknown channel",
			mutate: func(m *messaging.Message) { m.Channel = "pigeon" },
			want:   "message.invalid_channel",
		},
		{
			name:   "empty destination",
			mutate: func(m *messaging.Message) { m.To = "" },
			want:   "message.to_required",
		},
		{
			name:   "whitespace-only destination",
			mutate: func(m *messaging.Message) { m.To = "   " },
			want:   "message.to_required",
		},
		{
			name: "no body and no media",
			mutate: func(m *messaging.Message) {
				m.Body = ""
				m.MediaURLs = nil
			},
			want: "message.body_required",
		},
		{
			name:   "whitespace-only body and no media",
			mutate: func(m *messaging.Message) { m.Body = "   " },
			want:   "message.body_required",
		},
		{
			name:   "body over 4096 runes",
			mutate: func(m *messaging.Message) { m.Body = strings.Repeat("é", 4097) },
			want:   "message.body_too_long",
		},
		{
			name:   "exactly 4096 multibyte runes is valid",
			mutate: func(m *messaging.Message) { m.Body = strings.Repeat("é", 4096) },
			want:   "",
		},
		{
			name: "precedence: channel beats destination",
			mutate: func(m *messaging.Message) {
				m.Channel = ""
				m.To = ""
			},
			want: "message.invalid_channel",
		},
		{
			name: "precedence: interaction beats channel",
			mutate: func(m *messaging.Message) {
				m.InteractionID = ""
				m.Channel = "pigeon"
			},
			want: "message.interaction_required",
		},
	}
}

// validMessage returns a fully valid outbound message the scenarios mutate.
func validMessage(tenantID string) messaging.Message {
	return messaging.Message{
		InteractionID: newLegID(),
		TenantID:      tenantID,
		Channel:       messaging.ChannelWhatsApp,
		From:          "+254700000001",
		To:            "+254711111111",
		Body:          "orvexa conformance ping",
	}
}

// RunMessagingConformance runs the full messaging behavioral contract
// against a MessagingProvider adapter. The built-in Simulator is the
// reference behavior and passes it green.
//
// Wiring: construct the Recorder, hand Recorder.Ingest to the adapter as its
// delivery hook, pass the same Recorder via Options.Recorder.
//
// Scenarios (each documents the invariant it protects):
//
//  1. validate-message taxonomy (always enforced — the core gate)
//  2. send lifecycle: sent then delivered
//  3. media-only messages are first-class
//  4. send is retry-safe on repeated InteractionID
//  5. send requires tenant context
//  6. send rejects invalid messages with the core codes (only with
//     Options.EnforceSendValidation)
func RunMessagingConformance(t *testing.T, mp messaging.MessagingProvider, opts Options) {
	t.Helper()
	if mp == nil {
		t.Fatal("conformance: nil messaging.MessagingProvider")
	}
	rec := requireRecorder(t, opts)
	opts = opts.normalize()
	ctx := context.Background()

	t.Run("validate-message taxonomy", func(t *testing.T) {
		for _, tc := range invalidMessageCases() {
			t.Run(tc.name, func(t *testing.T) {
				msg := validMessage(opts.TenantID)
				tc.mutate(&msg)
				err := messaging.ValidateMessage(&msg)
				if tc.want == "" {
					// Invariant: the 4096-rune boundary is inclusive and
					// counted in RUNES — a 8192-byte body of multibyte
					// characters is legal. Byte-counting implementations
					// reject half the world's languages at half the limit.
					if err != nil {
						t.Fatalf("boundary message must be accepted, got: %v", err)
					}
					return
				}
				// Invariant: ValidateMessage is the core-side gate every
				// outbound message passes (messaging.Service.Send) and the
				// authoritative error taxonomy of the messaging plane: one
				// stable machine code per failure class, always KindInvalid
				// (httpx renders that as the 422 envelope). This table is
				// the exact, complete set — adapters re-validating must
				// reuse these codes verbatim.
				ae := requireAppError(t, err, apperrors.KindInvalid, "ValidateMessage")
				// Invariant: the machine code is EXACT. Clients, ops
				// runbooks and adapter tests key on the code, not the
				// human message; near-miss codes fork the taxonomy.
				if ae != nil && ae.Code != tc.want {
					t.Errorf("code must be exactly %q, got %q", tc.want, ae.Code)
				}
			})
		}
	})

	t.Run("send lifecycle: sent then delivered", func(t *testing.T) {
		msg := validMessage(opts.TenantID)
		seedActive(t, rec, opts.TenantID, msg.InteractionID)
		d0, f0, a0 := len(rec.Deliveries()), len(rec.FailedDeliveries()), len(rec.Applied())

		err := mp.Send(ctx, msg)
		// Invariant: a valid, tenant-bound message is accepted for
		// delivery — the core already validated it and flipped the
		// interaction pending->active before this call.
		if err != nil {
			t.Fatalf("Send must succeed for a valid message, got: %v", err)
		}
		awaitApplied(t, opts, rec, a0+2)

		evs := rec.AppliedFor(opts.TenantID, msg.InteractionID)
		// Invariant: the outbound lifecycle is observable as exactly
		// message.sent -> message.delivered, in that order. Receipts must
		// not overtake the send acknowledgement, or the realtime feed
		// would announce delivery before the carrier accepted the message.
		wantSeq(t, evs, "message.sent", "message.delivered")

		for _, ev := range evs {
			// Invariant: receipts are stamped with the message's own
			// interaction id and tenant — a receipt for another leg would
			// complete or fail a foreign conversation.
			if ev.InteractionID != msg.InteractionID || ev.TenantID != opts.TenantID {
				t.Errorf("lifecycle event %q must carry interaction_id=%s tenant_id=%s, got interaction_id=%q tenant_id=%q",
					ev.Event, msg.InteractionID, opts.TenantID, ev.InteractionID, ev.TenantID)
			}
		}
		auditDeliveries(t, rec.Deliveries()[d0:], opts)
		assertNoFailedDeliveries(t, rec, f0, "send lifecycle")

		// Invariant: sent/delivered leave the outbound message ACTIVE.
		// completed is reserved for the customer-side read receipt
		// (message.read) — delivery alone never completes a conversation.
		assertStatus(t, rec, opts.TenantID, msg.InteractionID, interactions.StatusActive)
	})

	t.Run("media-only messages are first-class", func(t *testing.T) {
		msg := validMessage(opts.TenantID)
		msg.Body = ""
		msg.MediaURLs = []string{"https://cdn.example.test/conformance/media-a.jpg"}

		// Invariant: body_required fires only when BOTH body and media are
		// absent — a WhatsApp image with no caption is a legal message, and
		// adapters must not re-implement body-required on top of the core
		// gate (they receive pre-validated messages).
		if err := messaging.ValidateMessage(&msg); err != nil {
			t.Fatalf("media-only message must pass ValidateMessage, got: %v", err)
		}

		seedActive(t, rec, opts.TenantID, msg.InteractionID)
		d0, f0, a0 := len(rec.Deliveries()), len(rec.FailedDeliveries()), len(rec.Applied())

		if err := mp.Send(ctx, msg); err != nil {
			t.Fatalf("Send must succeed for a media-only message, got: %v", err)
		}
		awaitApplied(t, opts, rec, a0+2)

		// Invariant: media-only messages drive the same receipt lifecycle
		// as text — adapters must not fork behavior by content type.
		wantSeq(t, rec.AppliedFor(opts.TenantID, msg.InteractionID), "message.sent", "message.delivered")
		assertStatus(t, rec, opts.TenantID, msg.InteractionID, interactions.StatusActive)
		auditDeliveries(t, rec.Deliveries()[d0:], opts)
		assertNoFailedDeliveries(t, rec, f0, "media-only lifecycle")
	})

	t.Run("send is retry-safe on repeated InteractionID", func(t *testing.T) {
		msg := validMessage(opts.TenantID)
		seedActive(t, rec, opts.TenantID, msg.InteractionID)
		f0, a0 := len(rec.FailedDeliveries()), len(rec.Applied())

		if err := mp.Send(ctx, msg); err != nil {
			t.Fatalf("first Send must succeed, got: %v", err)
		}
		awaitApplied(t, opts, rec, a0+2)

		// Invariant: re-sending the SAME InteractionID must NOT error. The
		// core retries provider.Send after transport timeouts and turns a
		// provider error into interaction failure
		// (message.provider_failed -> active->failed) — an adapter that
		// rejects duplicates would fail REAL interactions on harmless
		// retries, and one that double-bills silently is worse. Duplicate
		// suppression downstream is the processor's same-state no-op
		// (provider_event.go), so both honest strategies pass: re-emit the
		// receipts (the reference behavior) or suppress them silently.
		if err := mp.Send(ctx, msg); err != nil {
			t.Fatalf("duplicate Send with the same InteractionID must be accepted (reference behavior), got: %v", err)
		}
		awaitQuiet(t, opts, rec)

		evs := eventNames(rec.AppliedFor(opts.TenantID, msg.InteractionID))
		switch {
		case equalSeq(evs, []string{"message.sent", "message.delivered"}):
			// adapter suppressed duplicate receipts — lifecycle untouched
		case equalSeq(evs, []string{"message.sent", "message.delivered", "message.sent", "message.delivered"}):
			// reference behavior: receipts re-emitted, processor no-oped
		default:
			t.Errorf("after a duplicate send the lifecycle must be unchanged (receipts re-emitted or suppressed), got %v", evs)
		}

		// Invariant: the interaction survives a retry as active — no
		// corruption, no terminal transition — and every induced delivery
		// (both sends) applied cleanly.
		assertStatus(t, rec, opts.TenantID, msg.InteractionID, interactions.StatusActive)
		assertNoFailedDeliveries(t, rec, f0, "duplicate send")
	})

	t.Run("send requires tenant context", func(t *testing.T) {
		msg := validMessage("")
		d0, f0, a0 := len(rec.Deliveries()), len(rec.FailedDeliveries()), len(rec.Applied())

		err := mp.Send(ctx, msg)
		// Invariant: tenant context is mandatory on the messaging plane
		// too: adapters map tenant -> carrier account/config, and every
		// receipt must carry tenant_id for the processor. A send without
		// tenant context cannot be scoped or billed — the reference
		// rejects it and so must every adapter.
		if err == nil {
			t.Fatalf("Send without tenant context must be rejected, got nil error")
		}
		// Invariant: an unscoped send is an INPUT problem — where the
		// adapter speaks the application error model it maps to invalid
		// (422), not internal.
		requireErrKind(t, err, apperrors.KindInvalid, "send without tenant context")
		awaitQuiet(t, opts, rec)

		// Invariant: a rejected send must not touch the carrier — no
		// deliveries, no receipts, no lifecycle movement.
		assertNoNewObservations(t, rec, d0, f0, a0, "rejected send")
	})

	if opts.EnforceSendValidation {
		t.Run("send rejects invalid messages with the core codes", func(t *testing.T) {
			for _, tc := range invalidMessageCases() {
				t.Run(tc.name, func(t *testing.T) {
					msg := validMessage(opts.TenantID)
					tc.mutate(&msg)
					seedActive(t, rec, opts.TenantID, msg.InteractionID)
					d0, a0 := len(rec.Deliveries()), len(rec.Applied())

					if tc.want == "" {
						// The boundary probe is a VALID message: a
						// re-validating adapter must accept it and drive
						// the normal receipt lifecycle — validation
						// knobs must never become over-rejection.
						if err := mp.Send(ctx, msg); err != nil {
							t.Fatalf("boundary message must be accepted, got: %v", err)
						}
						awaitApplied(t, opts, rec, a0+2)
						wantSeq(t, rec.AppliedFor(opts.TenantID, msg.InteractionID), "message.sent", "message.delivered")
						return
					}

					err := mp.Send(ctx, msg)
					// Invariant: adapters that re-validate inbound commands
					// MUST reuse the core codes verbatim (same table as the
					// ValidateMessage gate) — one taxonomy across the
					// platform, whatever layer catches the bad message.
					ae := requireAppError(t, err, apperrors.KindInvalid, "Send(invalid message)")
					if ae != nil && ae.Code != tc.want {
						t.Errorf("code must be exactly the core code %q, got %q", tc.want, ae.Code)
					}

					// Invariant: a rejected message must not touch the
					// carrier — rejection happens before emission.
					if got := len(rec.Deliveries()); got != d0 {
						t.Errorf("rejected message must not emit deliveries, got %d new", got-d0)
					}
					if got := len(rec.Applied()); got != a0 {
						t.Errorf("rejected message must not move the lifecycle, got %d new applied events", got-a0)
					}
				})
			}
		})
	}
}
