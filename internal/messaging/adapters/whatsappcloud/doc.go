// Package whatsappcloud implements the messaging.MessagingProvider port for
// the Meta WhatsApp Cloud API (issue #27, [O-18]) — the primary channel for
// the Nairobi market.
//
// Architecture (hexagonal, per ADR-0004/0005):
//
//   - Outbound: Send / SendTemplate POST to
//     graph.facebook.com/v21.0/{phone-number-id}/messages with the Bearer
//     access token (registry.Secret — zero leakage on every rendering path).
//     Text, media (MediaURLs -> image/video/audio/document) and template
//     sends are supported; media URLs must be https.
//
//   - Inbound status receipts: Meta delivers statuses (sent/delivered/read/
//     failed) as webhooks verified at the edge by the fail-closed gateway
//     verifier (internal/webhooks VerifyWhatsAppCloud, x-hub-signature-256).
//     After gateway verification the HTTP layer hands the raw body to
//     HandleWebhook, which translates it into comms.ProviderEvent deliveries
//     (message.sent / message.delivered / message.read / message.failed)
//     through the comms.IngestFunc port — the exact path the built-in
//     Simulator uses. TranslateStatus is the pure (side-effect free) helper
//     behind that translation.
//
//   - Errors: Graph error codes map onto the apperrors taxonomy with
//     documented semantics (errors.go). The 24-hour customer service window
//     is handled honestly: a free-form send outside the window returns the
//     explicit typed *WindowClosedError (Graph code 131047); the documented
//     escape is an approved template via SendTemplate, which Meta delivers
//     outside the window.
//
//   - Retries: bounded, only on the provider-signaled transient classes
//     (HTTP 429 / 5xx / Graph 130429 + 80007); transport errors are the
//     core's retry domain (messaging.Service re-invokes provider.Send after
//     transport timeouts).
//
// Conformance: the adapter embeds RunMessagingConformance
// (internal/comms/conformance) against an httptest fake Graph server — an
// adapter PR that does not pass the kit is not reviewable.
//
// Ownership: this package is the exclusive scope of issue #27. It does not
// modify go.mod/go.sum (stdlib net/http + existing deps only), the webhook
// gateway, the registry, or the core messaging service.
//
// Security: credentials enter only via registry.Secret and are never
// rendered, logged, or embedded in errors; adversarial leakage tests pin
// that guarantee (see errors_test.go / provider_test.go).
package whatsappcloud
