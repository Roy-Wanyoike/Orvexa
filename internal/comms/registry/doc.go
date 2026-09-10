// Package registry is the single place in Orvexa that knows which
// communications providers exist, the exact credential shape each one
// requires, and how to turn environment variables into validated, redactable
// provider configuration for the telephony and messaging planes.
//
// # Provider names
//
// The closed set is: simulator (built-in, default, no credentials), twilio,
// whatsappcloud, africastalking, freeswitch, asterisk. Names are lowercase
// and matched exactly, so configuration typos fail loudly at startup instead
// of silently rerouting traffic.
//
// # Environment contract
//
// Selection (one key per plane, default simulator):
//
//	ORVEXA_TELEPHONY_PROVIDER
//	ORVEXA_MESSAGING_PROVIDER
//
// Credentials, read only for the selected provider:
//
//	twilio:          ORVEXA_TWILIO_ACCOUNT_SID, ORVEXA_TWILIO_AUTH_TOKEN, ORVEXA_TWILIO_FROM_NUMBER
//	whatsappcloud:   ORVEXA_WHATSAPP_PHONE_NUMBER_ID, ORVEXA_WHATSAPP_ACCESS_TOKEN, ORVEXA_WHATSAPP_APP_SECRET, ORVEXA_WHATSAPP_VERIFY_TOKEN
//	africastalking:  ORVEXA_AT_USERNAME, ORVEXA_AT_API_KEY, ORVEXA_AT_VOICE_PRODUCT_CODE (voice), ORVEXA_AT_SENDER_ID (messaging)
//	freeswitch:      ORVEXA_FREESWITCH_HOST, ORVEXA_FREESWITCH_PORT (default 8021), ORVEXA_FREESWITCH_PASSWORD
//	asterisk:        ORVEXA_ASTERISK_HOST, ORVEXA_ASTERISK_PORT (default 5038), ORVEXA_ASTERISK_USERNAME, ORVEXA_ASTERISK_SECRET
//
// pkg/config mirrors only the two selection keys (additively); every
// credential is parsed, validated and redacted here and nowhere else.
//
// # Validation contract
//
// TelephonyFromEnv / MessagingFromEnv / LoadFromEnv fail at startup with an
// *apperrors.Error of Kind invalid when the provider name is unknown (code
// comms.unknown_provider), the provider does not serve the requested plane
// (comms.unsupported_plane), or required credentials are missing
// (comms.missing_credentials — the message lists the missing env var NAMES,
// never their values). ValidateTelephony / ValidateMessaging apply the same
// rules to hand-built or per-tenant configurations.
//
// # Redaction contract
//
// Every credential field is typed Secret. All config types render zero
// credential material through fmt.Stringer, fmt.GoStringer, log/slog LogValuer
// and json.Marshaler: values shorter than 12 bytes are fully masked ("****");
// longer values reveal only their final four bytes ("****abcd") so operators
// can correlate credential rotations. Redacted is the shared helper.
//
// Residual bypasses no Stringer/GoStringer scheme can stop: fmt verbs outside
// v/s/q/x/X fall back to fmt's raw bad-verb rendering, and direct reflection
// or unsafe access reads the underlying string. Format configs only with the
// covered verbs; never route credential structs through reflection-based
// dumpers.
//
// # Per-tenant resolution
//
// Resolver is the seam for future CRM-driven per-tenant provider
// configuration. The default (NewGlobalResolver) returns a defensive clone of
// the global configuration for every tenant. Any tenant-specific
// configuration — however sourced — MUST pass ValidateTelephony or
// ValidateMessaging before an adapter consumes it.
//
// # Layering
//
// This package depends only on the standard library and pkg/errors. Provider
// adapters (roadmap O-10) and process wiring (O-23) consume the typed
// configuration; the domain core never sees provider names or credentials.
package registry
