package twilio

import (
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
)

// TestTwilioSMSConformance embeds the platform's messaging behavioral
// contract (internal/comms/conformance — see its README) against the Twilio
// adapter wired through the httptest fake:
//
//   - the fake API server receives the real form POSTs;
//   - Twilio's async status callbacks come back over real HTTP to the
//     adapter-configured StatusCallback URL;
//   - HandleStatusCallback translates them into SIGNED comms.ProviderEvent
//     deliveries through the Recorder's IngestFunc, the same path the
//     built-in Simulator (the reference behavior) uses.
//
// One Recorder, wired twice — the same instance is the adapter's delivery
// hook AND Options.Recorder (kit wiring rule #1). The adapter emits async
// (callbacks arrive on goroutines), so RequireAsyncEvents is declared per
// kit wiring rule #2. EnforceSendValidation is opted into because the
// adapter re-applies messaging.ValidateMessage before POSTing: the kit then
// proves the re-validation carries EXACTLY the core codes — one taxonomy
// across the platform, no near-miss forks.
func TestTwilioSMSConformance(t *testing.T) {
	f := newFakeTwilio(t)
	rec := conformance.NewRecorder()
	a := newTestAdapter(t, f, func(c *Config) {
		c.Ingest = rec.Ingest
		c.Signer = testSigner
	})
	f.setSink(a.HandleStatusCallback)

	conformance.RunMessagingConformance(t, a, conformance.Options{
		Recorder:              rec,
		ProviderName:          ProviderName,
		RequireAsyncEvents:    true, // Twilio status callbacks arrive async over HTTP
		EventSettleTimeout:    2 * time.Second,
		EnforceSendValidation: true, // adapter re-validates with the core gate; codes must match verbatim
	})
}
