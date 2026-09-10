// Package asterisk adapts Orvexa's telephony.VoiceProvider port to the
// Asterisk Manager Interface (AMI) — the management TCP protocol of the
// Asterisk PBX (default port 5038). It makes the platform deployable onto
// the most widely installed self-hosted PBX estate without an intermediary
// trunk service.
//
// Layering (three files, three concerns):
//
//	ami.go    — the wire codec: "Key: value" header blocks framed by blank
//	            lines (\r\n\r\n; bare \n accepted), with bounded line, header
//	            and packet sizes, and the Challenge→MD5 login key derivation.
//	client.go — the AMI session: dial + banner + challenge/login handshake,
//	          a single reader goroutine fanning responses to per-ActionID
//	          waiters and events to the adapter, a mutex-guarded writer,
//	          per-action timeouts, a keepalive knob, and a supervisor that
//	          reconnects with bounded exponential backoff until Close.
//	adapter.go — the voice plane translation: PlaceCall/Hangup/Transfer/
//	          Hold/Resume mapped to Originate/Hangup/Redirect actions, and
//	          Newstate/Hangup events translated into comms.ProviderEvent
//	          deliveries on the platform's webhook path (comms.IngestFunc).
//
// Correlation: async Originate carries ActionID = the platform interaction id
// (telephony.ProviderRef convention). The channel name from the Originate
// response, the ActionID echoed on the immediate Newchannel event, and the
// channel Uniqueid are bound to the interaction; subsequent Newstate and
// Hangup events on that channel are translated and stamped with the
// interaction id and tenant. Events for untracked channels are dropped —
// the platform never observes a leg it did not place.
//
// Security posture: credentials come from the provider registry as
// registry.Secret values (FromRegistry); the Challenge→MD5 handshake is used
// so the raw secret never appears on the wire after connect. Credentials are
// never logged or embedded in errors — Config.String, log fields and error
// messages are covered by leakage tests. Header injection (CR/LF in caller
// controlled values) is rejected at request-build time, not laundered.
//
// Scope notes: ARI (Asterisk REST Interface) is deliberately out of scope
// (future work), as is dialplan generation — Hold/Resume use a redirect into
// a music-on-hold context that the PBX administrator provisions; see the
// package README for the exact manager.conf and dialplan expectations.
package asterisk
