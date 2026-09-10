// Package freeswitch implements the telephony.VoiceProvider port over the
// FreeSWITCH Event Socket Layer (ESL): a hand-rolled, dependency-free
// implementation of the ESL TCP text protocol (mod_event_socket, default
// port 8021).
//
// Wire behavior implemented here:
//
//   - frames: a header block ("Name: value" lines, blank-line terminated)
//     optionally followed by a Content-Length delimited body;
//   - handshake: the server opens with Content-Type auth/request; the client
//     answers with "auth <password>" — or, when the endpoint advertises a
//     Challenge header, with "auth md5:<hex>" where hex is MD5 of
//     password+challenge (hardening for deployments that run ESL without
//     TLS; stock mod_event_socket uses the plaintext form);
//   - commands: "api <args>" (synchronous reply, Content-Type api/response)
//     and "bgapi <args>" (asynchronous job, command/reply carrying a
//     Job-UUID, with the result delivered later as a BACKGROUND_JOB event);
//   - events: "events plain <names>" subscription, translated from
//     text/event-plain frames.
//
// The Adapter maps Orvexa call semantics onto ESL:
//
//	PlaceCall -> bgapi originate {origination_uuid=<interaction id>}<target> &bridge(<from>)
//	Hangup    -> api uuid_kill <uuid>
//	Transfer  -> api uuid_transfer <uuid> <dialplan> <context> <dest>
//	Hold      -> api uuid_hold on <uuid>
//	Resume    -> api uuid_hold off <uuid>
//
// and translates CHANNEL_PROGRESS / CHANNEL_ANSWER / CHANNEL_HANGUP /
// BACKGROUND_JOB frames into comms.ProviderEvent deliveries (the same
// signed-webhook path the built-in simulator uses), correlating legs by the
// origination UUID, which equals the platform interaction id.
//
// Credentials are accepted as registry.Secret values and are never rendered
// raw by any logging/error path of this package. The package carries no
// third-party dependencies: the protocol is stdlib net + bufio only.
//
// See README.md in this directory for configuration, dialplan assumptions
// and operational limitations.
package freeswitch
