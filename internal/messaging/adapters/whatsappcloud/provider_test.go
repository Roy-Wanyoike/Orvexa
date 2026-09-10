package whatsappcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// requireAppErr asserts the error speaks the application model with the
// exact kind and machine code — the adapter's promise to the taxonomy.
func requireAppErr(t *testing.T, err error, kind apperrors.Kind, code string) *apperrors.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an application error, got nil")
	}
	var ae *apperrors.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error must unwrap to *apperrors.Error, got %T: %v", err, err)
	}
	if ae.Kind != kind {
		t.Errorf("kind must be %q, got %q (code %q)", kind, ae.Kind, ae.Code)
	}
	if ae.Code != code {
		t.Errorf("code must be exactly %q, got %q", code, ae.Code)
	}
	return ae
}

// validMsg returns a fully valid outbound message (core-gate clean).
func validMsg() messaging.Message {
	return messaging.Message{
		InteractionID: "019393a0-1000-7000-8000-00000000cafe",
		TenantID:      "tenant-1",
		Channel:       messaging.ChannelWhatsApp,
		From:          "+254700000001",
		To:            "+254711111111",
		Body:          "orvexa whatsapp ping",
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	rec := conformance.NewRecorder()
	cases := []struct {
		name string
		mut  func(c *Config)
		want string
	}{
		{"missing phone number id", func(c *Config) { c.PhoneNumberID = " " }, "whatsapp.phone_number_id_required"},
		{"missing access token", func(c *Config) { c.AccessToken = "" }, "whatsapp.access_token_required"},
		{"missing ingest hook", func(c *Config) { c.Ingest = nil }, "whatsapp.ingest_required"},
		{"missing signer", func(c *Config) { c.Signer = nil }, "whatsapp.signer_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				PhoneNumberID: testPhoneNumberID,
				AccessToken:   "tok",
				Ingest:        rec.Ingest,
				Signer:        testSigner,
			}
			tc.mut(&cfg)
			_, err := New(cfg)
			requireAppErr(t, err, apperrors.KindInvalid, tc.want)
		})
	}
}

func TestSendTextRequest(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	msg := validMsg() // To carries a leading "+" — the adapter normalizes
	if err := p.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := fake.count(); got != 1 {
		t.Fatalf("exactly one Graph request expected, got %d", got)
	}
	req := fake.requestAt(0)
	// Invariant: the endpoint is POST {base}/v21.0/{phone-number-id}/messages
	// with the Bearer credential — the Cloud API contract.
	if req.Method != "POST" {
		t.Errorf("method must be POST, got %s", req.Method)
	}
	if want := "/v21.0/" + testPhoneNumberID + "/messages"; req.Path != want {
		t.Errorf("path must be %q, got %q", want, req.Path)
	}
	if want := "Bearer " + testAccessToken; req.Auth != want {
		t.Errorf("Authorization must be the Bearer credential, got %q", req.Auth)
	}
	if req.ContentType != "application/json" {
		t.Errorf("Content-Type must be application/json, got %q", req.ContentType)
	}

	// Invariant: the payload is the WhatsApp text message shape with a
	// normalized recipient (E.164 digits, leading "+" stripped).
	if got, _ := req.Body["messaging_product"].(string); got != "whatsapp" {
		t.Errorf("messaging_product must be whatsapp, got %v", req.Body["messaging_product"])
	}
	if got, _ := req.Body["recipient_type"].(string); got != "individual" {
		t.Errorf("recipient_type must be individual, got %v", req.Body["recipient_type"])
	}
	if got, _ := req.Body["to"].(string); got != "254711111111" {
		t.Errorf("to must be normalized digits, got %q", got)
	}
	if got, _ := req.Body["type"].(string); got != "text" {
		t.Errorf("type must be text, got %v", req.Body["type"])
	}
	text, _ := req.Body["text"].(map[string]any)
	if text == nil || text["body"] != msg.Body {
		t.Errorf("text.body must carry the message body, got %v", req.Body["text"])
	}

	// Invariant: the send registers its wamid so status receipts can resolve
	// back to this interaction + tenant.
	ref, ok := p.ledger.lookup("wamid.fake-1")
	if !ok {
		t.Fatalf("send must register the returned wamid in the correlation ledger")
	}
	if ref.interactionID != msg.InteractionID || ref.tenantID != msg.TenantID {
		t.Errorf("ledger entry must carry interaction=%s tenant=%s, got %+v", msg.InteractionID, msg.TenantID, ref)
	}
}

func TestSendTenantRequiredBeforeAnyEmission(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	msg := validMsg()
	msg.TenantID = ""
	err := p.Send(context.Background(), msg)
	requireAppErr(t, err, apperrors.KindInvalid, "whatsapp.tenant_required")
	if got := fake.count(); got != 0 {
		t.Errorf("a rejected send must not touch the carrier, got %d requests", got)
	}
}

func TestSendReRunsCoreGateVerbatim(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	// Invariant: the adapter re-runs messaging.ValidateMessage as defense in
	// depth and reuses the core codes verbatim (one taxonomy across the
	// platform, whatever layer catches the bad message).
	msg := validMsg()
	msg.Body = ""
	msg.MediaURLs = nil
	err := p.Send(context.Background(), msg)
	requireAppErr(t, err, apperrors.KindInvalid, "message.body_required")
	if got := fake.count(); got != 0 {
		t.Errorf("a rejected message must not touch the carrier, got %d requests", got)
	}
}

func TestSendMediaMapping(t *testing.T) {
	cases := []struct {
		url      string
		wantType string
	}{
		{"https://cdn.example.test/a.jpg", "image"},
		{"https://cdn.example.test/a.PNG", "image"}, // case-insensitive extension
		{"https://cdn.example.test/a.mp4", "video"},
		{"https://cdn.example.test/a.mp3", "audio"},
		{"https://cdn.example.test/a.pdf", "document"},
		{"https://cdn.example.test/a.bin", "document"}, // unknown extension rides as document
		{"https://cdn.example.test/deep/path/a.jpeg?sig=abc", "image"},
	}
	for _, tc := range cases {
		t.Run(tc.wantType+" "+tc.url, func(t *testing.T) {
			fake := newFakeGraph(t)
			rec := conformance.NewRecorder()
			p := newTestProvider(t, fake, rec, nil)

			msg := validMsg()
			msg.Body = ""
			msg.MediaURLs = []string{tc.url}
			if err := p.Send(context.Background(), msg); err != nil {
				t.Fatalf("Send(media): %v", err)
			}

			req := fake.requestAt(0)
			if got, _ := req.Body["type"].(string); got != tc.wantType {
				t.Fatalf("media type must be %q, got %v", tc.wantType, req.Body["type"])
			}
			media, _ := req.Body[tc.wantType].(map[string]any)
			if media == nil || media["link"] != tc.url {
				t.Errorf("%s.link must carry the https URL, got %v", tc.wantType, req.Body[tc.wantType])
			}
			if _, has := media["caption"]; has {
				t.Errorf("media-only message must not carry a caption, got %v", media)
			}
		})
	}
}

func TestSendMediaWithCaption(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	msg := validMsg()
	msg.MediaURLs = []string{"https://cdn.example.test/receipt.png"}
	if err := p.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	media, _ := fake.requestAt(0).Body["image"].(map[string]any)
	if media == nil || media["caption"] != msg.Body {
		t.Errorf("body must ride along as the caption on image media, got %v", media)
	}
}

func TestSendMediaSchemeValidation(t *testing.T) {
	cases := []string{
		"http://cdn.example.test/a.jpg",            // plaintext: Meta fetches server-side
		"ftp://cdn.example.test/a.jpg",             // not fetchable
		"file:///etc/passwd",                       // local resource
		"javascript:alert(1)",                      // nonsense scheme
		"https:///no-host/a.jpg",                   // missing host
		"https://user:pass@cdn.example.test/a.jpg", // credentials in URL
		"   ", // blank
	}
	for _, url := range cases {
		t.Run(url, func(t *testing.T) {
			fake := newFakeGraph(t)
			rec := conformance.NewRecorder()
			p := newTestProvider(t, fake, rec, nil)

			msg := validMsg()
			msg.Body = ""
			msg.MediaURLs = []string{url}
			err := p.Send(context.Background(), msg)
			requireAppErr(t, err, apperrors.KindInvalid, "whatsapp.media_scheme")
			if got := fake.count(); got != 0 {
				t.Errorf("a scheme-invalid URL must never reach the carrier, got %d requests", got)
			}
		})
	}
}

func TestSendMultipleMediaRejected(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	msg := validMsg()
	msg.Body = ""
	msg.MediaURLs = []string{
		"https://cdn.example.test/a.jpg",
		"https://cdn.example.test/b.jpg",
	}
	err := p.Send(context.Background(), msg)
	// Invariant: WhatsApp carries one attachment per message. Silent
	// truncation would drop attachments callers believe were delivered;
	// an explicit invalid input keeps multi-attachment sends the caller's
	// modeling decision.
	requireAppErr(t, err, apperrors.KindInvalid, "whatsapp.media_single")
	if got := fake.count(); got != 0 {
		t.Errorf("rejected multi-media send must not touch the carrier, got %d requests", got)
	}
}

func TestSendAudioCaptionUnsupported(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	msg := validMsg()
	msg.MediaURLs = []string{"https://cdn.example.test/voice-note.mp3"}
	err := p.Send(context.Background(), msg)
	requireAppErr(t, err, apperrors.KindInvalid, "whatsapp.media_caption_unsupported")
	if got := fake.count(); got != 0 {
		t.Errorf("rejected audio+caption must not touch the carrier, got %d requests", got)
	}
}

func TestSendTemplateRequest(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	err := p.SendTemplate(context.Background(), TemplateRequest{
		InteractionID: "019393a0-1000-7000-8000-00000000beef",
		TenantID:      "tenant-1",
		To:            "+254722222222",
		TemplateName:  "appointment_reminder",
		LanguageCode:  "sw",
		BodyParams:    []string{"Asha", "tomorrow 10:00"},
	})
	if err != nil {
		t.Fatalf("SendTemplate: %v", err)
	}

	req := fake.requestAt(0)
	if got, _ := req.Body["type"].(string); got != "template" {
		t.Fatalf("type must be template, got %v", req.Body["type"])
	}
	if got, _ := req.Body["to"].(string); got != "254722222222" {
		t.Errorf("to must be normalized digits, got %q", got)
	}
	tmpl, _ := req.Body["template"].(map[string]any)
	if tmpl == nil {
		t.Fatalf("template object missing: %v", req.Body)
	}
	if tmpl["name"] != "appointment_reminder" {
		t.Errorf("template.name mismatch: %v", tmpl["name"])
	}
	lang, _ := tmpl["language"].(map[string]any)
	if lang == nil || lang["code"] != "sw" {
		t.Errorf("template.language.code must be sw, got %v", tmpl["language"])
	}
	comps, _ := tmpl["components"].([]any)
	if len(comps) != 1 {
		t.Fatalf("expected exactly the body component, got %v", comps)
	}
	body, _ := comps[0].(map[string]any)
	if body == nil || body["type"] != "body" {
		t.Errorf("first component must be the body component, got %v", comps[0])
	}
	params, _ := body["parameters"].([]any)
	if len(params) != 2 {
		t.Errorf("body component must carry both parameters, got %v", params)
	}

	ref, ok := p.ledger.lookup("wamid.fake-1")
	if !ok || ref.interactionID != "019393a0-1000-7000-8000-00000000beef" || ref.tenantID != "tenant-1" {
		t.Errorf("template send must register the wamid correlation, got %+v ok=%v", ref, ok)
	}
}

func TestSendTemplatePassthroughComponents(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	err := p.SendTemplate(context.Background(), TemplateRequest{
		InteractionID: "019393a0-1000-7000-8000-00000000beef",
		TenantID:      "tenant-1",
		To:            "254722222222",
		TemplateName:  "order_update",
		LanguageCode:  "en",
		Components: []json.RawMessage{
			json.RawMessage(`{"type":"header","parameters":[{"type":"text","text":"ORV-1"}]}`),
		},
	})
	if err != nil {
		t.Fatalf("SendTemplate: %v", err)
	}
	tmpl, _ := fake.requestAt(0).Body["template"].(map[string]any)
	comps, _ := tmpl["components"].([]any)
	if len(comps) != 1 {
		t.Fatalf("passthrough component must be forwarded verbatim, got %v", comps)
	}
	header, _ := comps[0].(map[string]any)
	if header == nil || header["type"] != "header" {
		t.Errorf("passthrough component mismatch: %v", comps[0])
	}
}

func TestSendTemplateValidation(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	base := TemplateRequest{
		InteractionID: "019393a0-1000-7000-8000-00000000beef",
		TenantID:      "tenant-1",
		To:            "254722222222",
		TemplateName:  "appointment_reminder",
		LanguageCode:  "sw",
	}
	cases := []struct {
		name string
		mut  func(t *TemplateRequest)
		want string
	}{
		{"missing tenant", func(r *TemplateRequest) { r.TenantID = "" }, "whatsapp.tenant_required"},
		{"missing interaction", func(r *TemplateRequest) { r.InteractionID = " " }, "whatsapp.interaction_required"},
		{"missing destination", func(r *TemplateRequest) { r.To = " " }, "whatsapp.to_required"},
		{"missing template name", func(r *TemplateRequest) { r.TemplateName = "" }, "whatsapp.template_name_required"},
		{"missing language", func(r *TemplateRequest) { r.LanguageCode = "" }, "whatsapp.template_language_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mut(&req)
			err := p.SendTemplate(context.Background(), req)
			requireAppErr(t, err, apperrors.KindInvalid, tc.want)
		})
	}
	if got := fake.count(); got != 0 {
		t.Errorf("invalid template requests must never reach the carrier, got %d requests", got)
	}
}

func TestSendMalformedSuccessEnvelope(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	// A 2xx without a message id is a contract violation: status receipts
	// resolve through the wamid, so accepting such a send would orphan the
	// interaction's lifecycle. Fail loudly instead.
	fake.enqueue(fakeReply{status: 200, body: map[string]any{"messaging_product": "whatsapp"}})
	err := p.Send(context.Background(), validMsg())
	requireAppErr(t, err, apperrors.KindInternal, "whatsapp.malformed_send_response")
}

// TestProviderAndConfigRenderingsNeverLeakToken is the adapter-side half of
// the zero-leakage guarantee: every fmt rendering path of the provider and
// its config surfaces only redacted credential material. (%#v on nested
// struct fields bypasses GoString — that residual risk is registry-level
// documented and unchanged here.)
func TestProviderAndConfigRenderingsNeverLeakToken(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	cfg := Config{
		PhoneNumberID: testPhoneNumberID,
		AccessToken:   testAccessToken,
		Ingest:        rec.Ingest,
		Signer:        testSigner,
	}
	renderings := []string{
		p.String(),
		p.GoString(),
		fmt.Sprintf("%v", p),
		fmt.Sprintf("%+v", p),
		fmt.Sprintf("%#v", p),
		cfg.String(),
		cfg.GoString(),
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%#v", cfg),
	}
	for i, r := range renderings {
		if strings.Contains(r, testAccessToken) {
			t.Errorf("rendering #%d leaks the access token: %s", i, r)
		}
		if strings.Contains(r, "EAAG-") {
			t.Errorf("rendering #%d leaks token material: %s", i, r)
		}
	}
}
