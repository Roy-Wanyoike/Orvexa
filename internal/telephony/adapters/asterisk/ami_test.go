package asterisk

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Golden encode tests: the exact wire bytes the codec emits.
// ---------------------------------------------------------------------------

func TestEncodeOriginateGolden(t *testing.T) {
	const interactionID = "11111111-2222-3333-4444-555555555555"
	req := amiRequest{
		Action: "Originate",
		Headers: []amiHeader{
			{Key: "Channel", Value: "Local/254711111111@orvexa-dial"},
			{Key: "Context", Value: "orvexa-dial"},
			{Key: "Exten", Value: "254711111111"},
			{Key: "Priority", Value: "1"},
			{Key: "CallerID", Value: "+254700000001"},
			{Key: "Async", Value: "true"},
			{Key: "Timeout", Value: "30000"},
			// Duplicate Variable headers are AMI's multi-value mechanism and
			// must survive encoding in wire order.
			{Key: "Variable", Value: "ORVEXA_INTERACTION_ID=" + interactionID},
			{Key: "Variable", Value: "ORVEXA_TENANT_ID=t-123"},
		},
	}
	got, err := req.withActionID(interactionID).encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := "Action: Originate\r\n" +
		"ActionID: " + interactionID + "\r\n" +
		"Channel: Local/254711111111@orvexa-dial\r\n" +
		"Context: orvexa-dial\r\n" +
		"Exten: 254711111111\r\n" +
		"Priority: 1\r\n" +
		"CallerID: +254700000001\r\n" +
		"Async: true\r\n" +
		"Timeout: 30000\r\n" +
		"Variable: ORVEXA_INTERACTION_ID=" + interactionID + "\r\n" +
		"Variable: ORVEXA_TENANT_ID=t-123\r\n" +
		"\r\n"
	if string(got) != want {
		t.Fatalf("golden mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestEncodeRedirectGolden(t *testing.T) {
	req := amiRequest{
		Action: "Redirect",
		Headers: []amiHeader{
			{Key: "Channel", Value: "PJSIP/orvexa-000001"},
			{Key: "Context", Value: "orvexa-hold"},
			{Key: "Exten", Value: "s"},
			{Key: "Priority", Value: "1"},
		},
	}.withActionID("ovx-7")
	got, err := req.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := "Action: Redirect\r\n" +
		"ActionID: ovx-7\r\n" +
		"Channel: PJSIP/orvexa-000001\r\n" +
		"Context: orvexa-hold\r\n" +
		"Exten: s\r\n" +
		"Priority: 1\r\n" +
		"\r\n"
	if string(got) != want {
		t.Fatalf("golden mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestEncodeRejectsHeaderInjection(t *testing.T) {
	cases := []struct {
		name    string
		req     amiRequest
		errWant error
	}{
		{
			name:    "action name",
			req:     amiRequest{Action: "Originate\r\nEvent: Bulldoze"},
			errWant: ErrMalformedPacket,
		},
		{
			name: "action id value",
			req: amiRequest{Action: "Ping", Headers: []amiHeader{
				{Key: "ActionID", Value: "x\nAction: Login\nUsername: admin"},
			}},
			errWant: ErrMalformedPacket,
		},
		{
			name: "caller id value with CR",
			req: amiRequest{Action: "Originate", Headers: []amiHeader{
				{Key: "CallerID", Value: "+254700000001\rContext: evil"},
			}},
			errWant: ErrMalformedPacket,
		},
		{
			name: "NUL byte in value",
			req: amiRequest{Action: "Hangup", Headers: []amiHeader{
				{Key: "Channel", Value: "PJSIP/1\x00;2"},
			}},
			errWant: ErrMalformedPacket,
		},
		{
			name: "framing character in key",
			req: amiRequest{Action: "Hangup", Headers: []amiHeader{
				{Key: "Channel: X", Value: "y"},
			}},
			errWant: ErrMalformedPacket,
		},
		{
			name:    "empty action",
			req:     amiRequest{Action: ""},
			errWant: ErrMalformedPacket,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.req.encode()
			if !errors.Is(err, tc.errWant) {
				t.Fatalf("encode error = %v, want %v", err, tc.errWant)
			}
		})
	}
}

func TestWithActionIDReplacesNotDuplicates(t *testing.T) {
	req := amiRequest{Action: "Ping", Headers: []amiHeader{{Key: "actionid", Value: "stale"}}}
	got, err := req.withActionID("fresh").encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if n := bytes.Count(got, []byte("ActionID:")); n != 1 {
		t.Fatalf("ActionID header count = %d, want 1: %q", n, got)
	}
	if !bytes.Contains(got, []byte("ActionID: fresh\r\n")) {
		t.Fatalf("stale ActionID not replaced: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Golden decode tests: exact wire bytes parse into the expected packets.
// ---------------------------------------------------------------------------

const (
	sampleBanner = "Asterisk Call Manager/8.0.0\r\n\r\n"

	sampleOriginateResponse = "Response: Success\r\n" +
		"ActionID: 11111111-2222-3333-4444-555555555555\r\n" +
		"Message: Originate successfully queued\r\n" +
		"Channel: Local/254711111111@orvexa-dial-1;2\r\n" +
		"\r\n"

	sampleFailureResponse = "Response: Failure\r\n" +
		"ActionID: ovx-3\r\n" +
		"Message: No such channel\r\n" +
		"\r\n"

	sampleNewstateEvent = "Event: Newstate\r\n" +
		"Privilege: call,all\r\n" +
		"Channel: PJSIP/orvexa-000001\r\n" +
		"Uniqueid: orvexa-unique-1\r\n" +
		"ChannelStateDesc: Ringing\r\n" +
		"ChannelState: 4\r\n" +
		"CallerIDNum: +254700000001\r\n" +
		"\r\n"

	// Bare-LF framing variant: tolerated so adapters keep working behind
	// proxies and older releases that emit \n instead of \r\n.
	sampleHangupEventLF = "Event: Hangup\n" +
		"Privilege: call,all\n" +
		"Channel: PJSIP/orvexa-000001\n" +
		"Uniqueid: orvexa-unique-1\n" +
		"Cause: 16\n" +
		"Cause-txt: Normal Clearing\n" +
		"\n"

	// A value that itself contains a colon must survive parsing.
	sampleColonValue = "Event: VarSet\r\n" +
		"Channel: Local/254722000000@orvexa-dial-1;2\r\n" +
		"Variable: SIPURI\r\n" +
		"Value: sip:201@10.0.0.8:5060\r\n" +
		"\r\n"
)

func parseOne(t *testing.T, raw string) amiPacket {
	t.Helper()
	r := newAMIReader(strings.NewReader(raw))
	pkt, err := r.next()
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return pkt
}

func TestParseBannerGolden(t *testing.T) {
	pkt := parseOne(t, sampleBanner)
	if pkt.isEvent() || pkt.isResponse() {
		t.Fatalf("banner parsed as traffic: %+v", pkt)
	}
	if got := len(pkt.headers); got != 1 {
		t.Fatalf("banner headers = %d, want 1", got)
	}
	if got := pkt.headers[0].Key; got != "Asterisk Call Manager/8.0.0" {
		t.Fatalf("banner line = %q", got)
	}
}

func TestParseOriginateResponseGolden(t *testing.T) {
	pkt := parseOne(t, sampleOriginateResponse)
	if pkt.response() != "Success" {
		t.Fatalf("Response = %q, want Success", pkt.response())
	}
	if pkt.actionID() != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("ActionID = %q", pkt.actionID())
	}
	if got := pkt.get("Channel"); got != "Local/254711111111@orvexa-dial-1;2" {
		t.Fatalf("Channel = %q", got)
	}
	if got := pkt.get("Message"); got != "Originate successfully queued" {
		t.Fatalf("Message = %q", got)
	}
}

func TestParseFailureResponseGolden(t *testing.T) {
	pkt := parseOne(t, sampleFailureResponse)
	if pkt.response() != "Failure" || pkt.actionID() != "ovx-3" || pkt.get("Message") != "No such channel" {
		t.Fatalf("unexpected packet: %+v", pkt)
	}
}

func TestParseNewstateEventGolden(t *testing.T) {
	pkt := parseOne(t, sampleNewstateEvent)
	if !pkt.isEvent() || pkt.event() != "Newstate" {
		t.Fatalf("event = %q", pkt.event())
	}
	if got := pkt.get("ChannelStateDesc"); got != "Ringing" {
		t.Fatalf("ChannelStateDesc = %q", got)
	}
	if got := pkt.get("Privilege"); got != "call,all" {
		t.Fatalf("Privilege = %q", got)
	}
}

func TestParseHangupEventLFOnlyGolden(t *testing.T) {
	r := newAMIReader(strings.NewReader(sampleHangupEventLF))
	pkt, err := r.next()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pkt.event() != "Hangup" || pkt.get("Cause") != "16" || pkt.get("Cause-txt") != "Normal Clearing" {
		t.Fatalf("unexpected packet: %+v", pkt)
	}
	// The stream ends without a trailing newline: clean EOF at the boundary.
	if _, err := r.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second next() = %v, want io.EOF", err)
	}
}

func TestParseColonValueSurvives(t *testing.T) {
	pkt := parseOne(t, sampleColonValue)
	if got := pkt.get("Value"); got != "sip:201@10.0.0.8:5060" {
		t.Fatalf("Value = %q, want the full colon-bearing URI", got)
	}
}

func TestParseCaseInsensitiveHeaderLookup(t *testing.T) {
	// Deployments mix ActionID/ActionId casing; lookups fold case.
	raw := "Response: Success\r\nActionId: abc\r\nMessage: ok\r\n\r\n"
	pkt := parseOne(t, raw)
	if pkt.actionID() != "abc" {
		t.Fatalf("ActionID lookup = %q, want abc", pkt.actionID())
	}
}

func TestParseStreamOfPackets(t *testing.T) {
	stream := sampleBanner + sampleOriginateResponse + sampleNewstateEvent + sampleFailureResponse
	r := newAMIReader(strings.NewReader(stream))
	want := []struct {
		event, response, actionID string
	}{
		{response: "", actionID: ""}, // banner: colonless line
		{response: "Success", actionID: "11111111-2222-3333-4444-555555555555"},
		{event: "Newstate"},
		{response: "Failure", actionID: "ovx-3"},
	}
	for i, w := range want {
		pkt, err := r.next()
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if pkt.event() != w.event || pkt.response() != w.response || pkt.actionID() != w.actionID {
			t.Fatalf("packet %d = {event:%q response:%q actionID:%q}, want %+v",
				i, pkt.event(), pkt.response(), pkt.actionID(), w)
		}
	}
	if _, err := r.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream not exhausted cleanly: %v", err)
	}
}

func TestParseGetAllPreservesDuplicateVariables(t *testing.T) {
	raw := "Response: Success\r\n" +
		"ActionID: x1\r\n" +
		"Variable: A=1\r\n" +
		"Variable: B=2\r\n" +
		"Variable: C=3\r\n" +
		"\r\n"
	pkt := parseOne(t, raw)
	got := pkt.getAll("VARIABLE")
	if len(got) != 3 || got[0] != "A=1" || got[1] != "B=2" || got[2] != "C=3" {
		t.Fatalf("getAll = %v", got)
	}
}

func TestParseSkipsStrayBlankLines(t *testing.T) {
	stream := "\r\n\r\n" + sampleNewstateEvent + "\r\n"
	pkt := parseOne(t, stream)
	if pkt.event() != "Newstate" {
		t.Fatalf("event = %q, want Newstate", pkt.event())
	}
}

func TestParseBoundViolations(t *testing.T) {
	t.Run("line too long", func(t *testing.T) {
		r := newAMIReader(strings.NewReader("Event: " + strings.Repeat("x", DefaultMaxLineLen+1) + "\r\n\r\n"))
		if _, err := r.next(); !errors.Is(err, ErrPacketTooLarge) {
			t.Fatalf("err = %v, want ErrPacketTooLarge", err)
		}
	})
	t.Run("too many headers", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i <= DefaultMaxHeaders; i++ {
			b.WriteString("X: 1\r\n")
		}
		b.WriteString("\r\n")
		r := newAMIReader(strings.NewReader(b.String()))
		if _, err := r.next(); !errors.Is(err, ErrPacketTooLarge) {
			t.Fatalf("err = %v, want ErrPacketTooLarge", err)
		}
	})
	t.Run("mid-packet EOF", func(t *testing.T) {
		r := newAMIReader(strings.NewReader("Event: Newstate\r\n"))
		if _, err := r.next(); !errors.Is(err, ErrMalformedPacket) {
			t.Fatalf("err = %v, want ErrMalformedPacket", err)
		}
	})
	t.Run("empty header key", func(t *testing.T) {
		r := newAMIReader(strings.NewReader(": value\r\n\r\n"))
		if _, err := r.next(); !errors.Is(err, ErrMalformedPacket) {
			t.Fatalf("err = %v, want ErrMalformedPacket", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Auth key derivation (Challenge→MD5 login) golden vectors.
// ---------------------------------------------------------------------------

func TestAMIMD5KeyGolden(t *testing.T) {
	cases := []struct {
		secret, challenge, want string
	}{
		{"testsecret123", "challenge789", "cae1caf3c67cfea062afddb6c90dc106"},
		{"ami-golden-secret-4242", "orvexa-challenge-001", "9d72640e3e7bdbffff3fa4fe2fcd45b2"},
		{"", "", "d41d8cd98f00b204e9800998ecf8427e"}, // empty inputs are still well-defined
	}
	for _, tc := range cases {
		if got := amiMD5Key(tc.secret, tc.challenge); got != tc.want {
			t.Errorf("amiMD5Key(%q, %q) = %q, want %q", tc.secret, tc.challenge, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// cleanWireValue: control characters and length bounds on reused values.
// ---------------------------------------------------------------------------

func TestCleanWireValue(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"strips CRLF", "normal clearing\r\nAction: evil", 0, "normal clearingAction: evil"},
		{"strips NUL and DEL", "ab\x00cd\x7fef", 0, "abcdef"},
		{"truncates", strings.Repeat("x", 100), 10, strings.Repeat("x", 10)},
		{"passthrough", "cause 16 (Normal Clearing)", 0, "cause 16 (Normal Clearing)"},
		{"empty", "", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanWireValue(tc.in, tc.limit); got != tc.want {
				t.Fatalf("cleanWireValue(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
			}
		})
	}
}
