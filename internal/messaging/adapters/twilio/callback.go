package twilio

import (
	"context"
	"net/url"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// HandleStatusCallback translates one Twilio status callback and delivers it
// as a SIGNED provider webhook through the configured Ingest hook — the same
// comms.IngestFunc port the built-in Simulator uses, with the same
// provider label (ProviderName = "twilio") the webhook gateway routes by.
//
// This is the adapter's in-process receipt path (test/conformance wiring and
// callers that hold an adapter handle). The production webhook gateway
// instead verifies the callback's Twilio signature fail-closed
// (internal/webhooks.VerifyTwilio) and calls TranslateStatus directly —
// HandleStatusCallback performs NO signature verification and must never be
// exposed on a network path without the gateway in front of it.
func (a *Adapter) HandleStatusCallback(ctx context.Context, form url.Values) error {
	if a.ingest == nil || a.signer == nil {
		return apperrors.Invalid("twilio.ingest_not_wired",
			"status callback delivery requires Config.Ingest and Config.Signer")
	}
	ev, err := TranslateStatus(form)
	if err != nil {
		return err
	}
	body, err := ev.Encode()
	if err != nil {
		return apperrors.Internal("twilio.event_encode_failed",
			"status callback could not be encoded").WithCause(err)
	}
	return a.ingest(ctx, ProviderName, body, a.signer(body))
}
