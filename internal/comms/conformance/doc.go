// Package conformance is the executable behavioral contract for every
// Orvexa carrier adapter (voice and messaging). Real providers — twilio,
// whatsapp_cloud, africastalking, freeswitch, asterisk — embed this kit in
// their own _test.go files and must pass it green before merge.
//
// The built-in Simulator (internal/comms/simulator.go) is the reference
// behavior: the kit dogfoods against it (kit_test.go) so the contract and
// the reference can never drift apart silently.
//
// # The contract in one paragraph
//
// Every adapter implements telephony.VoiceProvider / messaging.MessagingProvider
// and reports lifecycle progress exactly like a carrier does: by delivering
// comms.ProviderEvent payloads as signed webhooks through the comms.IngestFunc
// port. The kit's Recorder sits on that port — it records every delivery and
// applies it through comms.Processor to a guarded interaction lifecycle, the
// exact harness the Simulator itself is tested with. RunVoiceConformance and
// RunMessagingConformance then drive the ports and assert observable
// behavior: event sequences and ordering, the pkg/errors error taxonomy,
// tenant binding, retry-safety, and the closed event vocabulary.
//
// # Wiring (see README.md for the full guide)
//
// The adapter is constructed by the embedding test, so the kit's recorder
// must be handed to the adapter BEFORE the conformance run:
//
//	rec := conformance.NewRecorder()
//	p := twilio.New(twilio.Config{Ingest: rec.Ingest, ...})
//	conformance.RunVoiceConformance(t, p, conformance.Options{
//	        Recorder:    rec,
//	        ProviderName: "twilio",
//	})
//
// # What the kit deliberately does not do
//
// It performs no network I/O and ships no HTTP server: the adapter delivers
// in-process through Recorder.Ingest. Signature values are checked for
// presence and stability only — HMAC verification against a shared secret
// stays in the webhook gateway (kit tests carry no credential material).
package conformance
