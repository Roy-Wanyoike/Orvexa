package whatsappcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// DefaultAPIBaseURL pins the Graph API version the adapter speaks. Upgrading
// the pin is a deliberate, reviewed change: Meta's error-code semantics are
// documented per version.
const DefaultAPIBaseURL = "https://graph.facebook.com/v21.0"

// ProviderName is the stable provider label stamped on every webhook
// delivery. It matches the registry (registry.ProviderWhatsAppCloud) and the
// webhook gateway verifier key (webhooks.ProviderWhatsAppCloud) so routing,
// signature verification and receipts line up end-to-end.
const ProviderName = "whatsappcloud"

// Config wires the adapter. Credentials come from the provider registry
// (registry.MessagingConfig resolves ORVEXA_WHATSAPP_* into Secret fields);
// Ingest/Signer follow the Simulator's signed-webhook delivery contract
// (comms.IngestFunc + comms.Signer) so translated receipts flow through the
// same fail-closed path.
type Config struct {
	// PhoneNumberID is the WhatsApp Business phone number id (Graph path
	// segment: /{phone-number-id}/messages).
	PhoneNumberID string

	// AccessToken is the permanent system-user token sent as the Bearer
	// credential. Secret-typed: every rendering path redacts it.
	AccessToken registry.Secret

	// APIBaseURL overrides the Graph endpoint (tests / Meta sandbox
	// environments). Empty = DefaultAPIBaseURL.
	APIBaseURL string

	// HTTPClient is the transport. Empty = http.DefaultClient with a
	// conservative timeout floor (see New).
	HTTPClient *http.Client

	// Ingest is the signed-webhook delivery hook for translated status
	// receipts (production: the platform's ingest path; conformance: the
	// kit's Recorder). Required — receipts must never be dropped silently.
	Ingest comms.IngestFunc

	// Signer signs each translated ProviderEvent body. Required whenever
	// Ingest is set: the webhook gateway is fail-closed and drops unsigned
	// traffic, so an unsigned delivery is behavior that cannot survive
	// production.
	Signer comms.Signer

	// Retry bounds the in-process retry budget. Zero = documented defaults.
	Retry RetryPolicy
}

// Provider implements messaging.MessagingProvider for the WhatsApp Cloud API.
// The zero value is not usable; construct with New.
type Provider struct {
	cfg    Config
	client *graphClient
	ledger *msgLedger
}

// New validates the configuration and builds the adapter. Invalid
// configurations fail loudly at construction (apperrors Invalid) rather than
// at first send.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.PhoneNumberID) == "" {
		return nil, apperrors.Invalid("whatsapp.phone_number_id_required",
			"WhatsApp Cloud phone number id is required (ORVEXA_WHATSAPP_PHONE_NUMBER_ID)")
	}
	if cfg.AccessToken == "" {
		return nil, apperrors.Invalid("whatsapp.access_token_required",
			"WhatsApp Cloud access token is required (ORVEXA_WHATSAPP_ACCESS_TOKEN)")
	}
	if cfg.Ingest == nil {
		return nil, apperrors.Invalid("whatsapp.ingest_required",
			"webhook ingest hook is required: status receipts must flow through the signed delivery path")
	}
	if cfg.Signer == nil {
		return nil, apperrors.Invalid("whatsapp.signer_required",
			"webhook signer is required: every delivery must be signed (fail-closed gateway)")
	}
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Provider{
		cfg: cfg,
		client: &graphClient{
			baseURL:       cfg.APIBaseURL,
			phoneNumberID: cfg.PhoneNumberID,
			token:         cfg.AccessToken,
			http:          httpc,
			retry:         cfg.Retry.withDefaults(),
		},
		ledger: newMsgLedger(),
	}, nil
}

// Send implements messaging.MessagingProvider. The core service pre-validates
// the message and flips the interaction pending->active before this call; the
// adapter re-runs the core gate as defense in depth (exact codes verbatim),
// rejects sends without tenant context, builds the Graph payload (text or
// single-attachment media), and POSTs it with the bounded retry budget.
//
// Idempotency matches the Simulator semantics: re-sending the SAME
// InteractionID is accepted (Graph has no idempotency key — each POST yields
// a fresh wamid) and duplicate suppression happens at the processor's
// same-state no-op. RunMessagingConformance pins this contract.
func (p *Provider) Send(ctx context.Context, msg messaging.Message) error {
	if msg.TenantID == "" {
		return apperrors.Invalid("whatsapp.tenant_required",
			"tenant context is required: receipts must be scoped to a tenant")
	}
	if err := messaging.ValidateMessage(&msg); err != nil {
		return err // exact core codes, verbatim
	}
	payload, err := buildMessagePayload(&msg)
	if err != nil {
		return err
	}
	resp, err := p.client.post(ctx, payload)
	if err != nil {
		return err
	}
	wamid, err := resp.messageID()
	if err != nil {
		return err
	}
	p.ledger.remember(wamid, msg.InteractionID, msg.TenantID)
	return nil
}

// TemplateRequest is the adapter-level template send. The messaging port
// (messaging.Message) carries only body/media, so template sends surface as
// this explicit extension — which doubles as the documented escape from the
// 24-hour window (see WindowClosedError): approved templates are deliverable
// outside the window.
type TemplateRequest struct {
	// InteractionID is the platform interaction the template send belongs to
	// (status receipts resolve back to it via the wamid ledger).
	InteractionID string
	// TenantID scopes receipts (mandatory, like Send).
	TenantID string
	// To is the recipient (E.164 digits; a single leading "+" is stripped).
	To string
	// TemplateName is the approved template name (Meta Business Manager).
	TemplateName string
	// LanguageCode is the template language policy code (e.g. "en", "sw").
	LanguageCode string
	// BodyParams fills the template's {{n}} body variables, in order
	// (convenience sugar rendered as the body text component).
	BodyParams []string
	// Components is raw passthrough for header/button/carousel components.
	// No builders beyond passthrough (issue #27 out-of-scope note).
	Components []json.RawMessage
}

// SendTemplate posts a template message. The same retry budget, error
// taxonomy and zero-leakage rules as Send apply.
func (p *Provider) SendTemplate(ctx context.Context, t TemplateRequest) error {
	if t.TenantID == "" {
		return apperrors.Invalid("whatsapp.tenant_required",
			"tenant context is required: receipts must be scoped to a tenant")
	}
	if strings.TrimSpace(t.InteractionID) == "" {
		return apperrors.Invalid("whatsapp.interaction_required",
			"interaction id is required for template sends")
	}
	if strings.TrimSpace(t.To) == "" {
		return apperrors.Invalid("whatsapp.to_required", "destination is required")
	}
	if strings.TrimSpace(t.TemplateName) == "" {
		return apperrors.Invalid("whatsapp.template_name_required",
			"template name is required (approved template from Meta Business Manager)")
	}
	if strings.TrimSpace(t.LanguageCode) == "" {
		return apperrors.Invalid("whatsapp.template_language_required",
			"template language code is required (Graph rejects language-less template sends)")
	}
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                normalizeRecipient(t.To),
		"type":              "template",
		"template":          templateObject(t),
	}
	resp, err := p.client.post(ctx, payload)
	if err != nil {
		return err
	}
	wamid, err := resp.messageID()
	if err != nil {
		return err
	}
	p.ledger.remember(wamid, t.InteractionID, t.TenantID)
	return nil
}

// templateObject renders the template payload: name + language plus the
// optional body-parameter component and raw passthrough components (in that
// order — body first matches Meta's examples and keeps the common case
// deterministic).
func templateObject(t TemplateRequest) map[string]any {
	obj := map[string]any{
		"name":     t.TemplateName,
		"language": map[string]any{"code": t.LanguageCode},
	}
	var components []any
	if len(t.BodyParams) > 0 {
		params := make([]any, 0, len(t.BodyParams))
		for _, v := range t.BodyParams {
			params = append(params, map[string]any{"type": "text", "text": v})
		}
		components = append(components, map[string]any{"type": "body", "parameters": params})
	}
	for _, raw := range t.Components {
		components = append(components, raw)
	}
	if len(components) > 0 {
		obj["components"] = components
	}
	return obj
}

// normalizeRecipient strips surrounding spaces and one leading "+" — Graph
// documents the "to" field as WhatsApp digits (E.164 without the plus).
func normalizeRecipient(to string) string {
	to = strings.TrimSpace(to)
	return strings.TrimPrefix(to, "+")
}

// WhatsApp media classes the Cloud API carries on a single message. The link
// media key IS the type ("image"/"video"/"audio"/"document"); audio and
// sticker cannot carry captions.
var mediaExtensions = map[string]string{
	".jpg": "image", ".jpeg": "image", ".png": "image", ".webp": "image",
	".mp4": "video", ".3gp": "video",
	".mp3": "audio", ".m4a": "audio", ".aac": "audio", ".amr": "audio", ".ogg": "audio",
	// Everything else (pdf, docx, xlsx, txt, ...) travels as document.
}

// captionCapable reports whether the media class supports a caption.
func captionCapable(mediaType string) bool {
	return mediaType == "image" || mediaType == "video" || mediaType == "document"
}

// buildMessagePayload renders the Graph messages request body for a
// port-level message: text when no media is attached, otherwise exactly one
// attachment (WhatsApp carries a single media object per message — additional
// MediaURLs are rejected as invalid input so callers model multi-attachment
// sends as separate interactions instead of silently losing attachments).
func buildMessagePayload(msg *messaging.Message) (map[string]any, error) {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                normalizeRecipient(msg.To),
	}
	if len(msg.MediaURLs) == 0 {
		payload["type"] = "text"
		payload["text"] = map[string]any{"body": msg.Body}
		return payload, nil
	}
	if len(msg.MediaURLs) > 1 {
		return nil, apperrors.Invalid("whatsapp.media_single",
			"WhatsApp Cloud carries one media attachment per message; send additional attachments as separate messages")
	}
	mediaType, err := classifyMediaURL(msg.MediaURLs[0])
	if err != nil {
		return nil, err
	}
	media := map[string]any{"link": msg.MediaURLs[0]}
	if msg.Body != "" {
		if !captionCapable(mediaType) {
			return nil, apperrors.Invalid("whatsapp.media_caption_unsupported",
				"message body cannot ride along as a caption on a "+mediaType+" attachment; send the text separately")
		}
		media["caption"] = msg.Body
	}
	payload["type"] = mediaType
	payload[mediaType] = media
	return payload, nil
}

// classifyMediaURL validates the media link and maps it to the Graph media
// type from the URL path extension (unknown extensions travel as document —
// Graph inspects the content at the link anyway).
func classifyMediaURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return "", apperrors.Invalid("whatsapp.media_scheme",
			"media URL must be an absolute https URL")
	}
	// Meta fetches the link server-side over the public internet; anything
	// but https is either unencrypted or not fetchable at all.
	if u.Scheme != "https" {
		return "", apperrors.Invalid("whatsapp.media_scheme",
			"media URL scheme must be https (Meta fetches the link server-side)")
	}
	if u.Host == "" {
		return "", apperrors.Invalid("whatsapp.media_scheme",
			"media URL must carry a host")
	}
	// A URL is not the place for credentials — reject rather than forward.
	if u.User != nil {
		return "", apperrors.Invalid("whatsapp.media_scheme",
			"media URL must not embed credentials (user:pass)")
	}
	ext := strings.ToLower(path.Ext(u.Path))
	if t, ok := mediaExtensions[ext]; ok {
		return t, nil
	}
	return "document", nil
}

// String renders the provider without credential material (defense in depth
// against accidental fmt.%s logging).
func (p *Provider) String() string {
	return fmt.Sprintf("whatsappcloud.Provider(phone_number_id=%s access_token=%s)",
		p.cfg.PhoneNumberID, p.cfg.AccessToken.Redacted())
}

// GoString renders the redacted form so %#v cannot disclose the token.
func (p *Provider) GoString() string { return p.String() }

// String renders the config with zero credential material.
func (c Config) String() string {
	return fmt.Sprintf("whatsappcloud.Config(phone_number_id=%s access_token=%s ingest=%t signer=%t)",
		c.PhoneNumberID, c.AccessToken.Redacted(), c.Ingest != nil, c.Signer != nil)
}

// GoString renders the masked form so %#v cannot disclose the token
// (registry.Secret's own GoString is bypassed by fmt on nested struct
// fields — the override here closes that path for the config value itself).
func (c Config) GoString() string { return c.String() }
