package freeswitch

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
)

// The goldens below pin the exact ESL wire bytes this package produces and
// accepts. They are copied verbatim from mod_event_socket behavior (and the
// challenge form this package negotiates when the endpoint advertises it),
// so a formatting regression fails here before it can reach a switch.

func TestGoldenFrameAuthRequest(t *testing.T) {
	// mod_event_socket's very first frame on a fresh connection: no
	// Content-Length, blank line terminates the header block.
	want := "Content-Type: auth/request\n\n"
	got := string(bareFrame(ctAuthRequest, nil))
	if got != want {
		t.Fatalf("auth/request frame bytes mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestGoldenFrameAuthRequestWithChallenge(t *testing.T) {
	want := "Content-Type: auth/request\nChallenge: challenge-1772509797\n\n"
	got := string(bareFrame(ctAuthRequest, map[string]string{"Challenge": "challenge-1772509797"}))
	if got != want {
		t.Fatalf("challenge auth/request frame bytes mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestGoldenFrameAuthCommandLines(t *testing.T) {
	t.Run("plaintext (stock mod_event_socket)", func(t *testing.T) {
		want := "auth ClueCon\n"
		got := string(command("auth ClueCon"))
		if got != want {
			t.Fatalf("plaintext auth line mismatch: got %q want %q", got, want)
		}
	})
	t.Run("challenge md5 (advertised hardening)", func(t *testing.T) {
		// Golden precomputed externally: md5("ClueCon" + "challenge-1772509797").
		const wantHex = "7e649a8619eaa31711be1a924ca57a20"
		sum := md5.Sum([]byte("ClueCon" + "challenge-1772509797"))
		if hex.EncodeToString(sum[:]) != wantHex {
			t.Fatalf("md5 fixture drifted: got %x want %s", sum, wantHex)
		}
		want := "auth md5:" + wantHex + "\n"
		got := string(command("auth md5:" + wantHex))
		if got != want {
			t.Fatalf("challenge auth line mismatch: got %q want %q", got, want)
		}
	})
}

func TestGoldenFrameCommandReply(t *testing.T) {
	// bgapi acceptance: Reply-Text embeds the job id and the Job-UUID header
	// repeats it for header-based correlation.
	want := "Content-Type: command/reply\n" +
		"Reply-Text: +OK Job-UUID: 9a1b2c3d-0000-4000-8000-000000000001\n" +
		"Job-UUID: 9a1b2c3d-0000-4000-8000-000000000001\n\n"
	got := string(replyFrame("+OK Job-UUID: 9a1b2c3d-0000-4000-8000-000000000001",
		map[string]string{"Job-UUID": "9a1b2c3d-0000-4000-8000-000000000001"}))
	if got != want {
		t.Fatalf("command/reply frame bytes mismatch:\n got: %q\nwant: %q", got, want)
	}

	fr := decodeOne(t, []byte(want))
	if fr.ContentType != ctCommandReply {
		t.Fatalf("content type = %q", fr.ContentType)
	}
	if fr.ReplyText() != "+OK Job-UUID: 9a1b2c3d-0000-4000-8000-000000000001" {
		t.Fatalf("Reply-Text = %q", fr.ReplyText())
	}
	if fr.Header(hdrJobUUID) != "9a1b2c3d-0000-4000-8000-000000000001" {
		t.Fatalf("Job-UUID header = %q", fr.Header(hdrJobUUID))
	}
	if fr.Result() != fr.ReplyText() {
		t.Fatalf("Result() on command/reply must be Reply-Text, got %q", fr.Result())
	}
}

func TestGoldenFrameAPIResponse(t *testing.T) {
	// Synchronous api reply: the result is a Content-Length delimited body.
	want := "Content-Type: api/response\nContent-Length: 40\n\n" +
		"+OK 9a1b2c3d-0000-4000-8000-000000000001"
	got := string(apiResponseFrame("+OK 9a1b2c3d-0000-4000-8000-000000000001"))
	if got != want {
		t.Fatalf("api/response frame bytes mismatch:\n got: %q\nwant: %q", got, want)
	}

	fr := decodeOne(t, []byte(want))
	if fr.ContentType != ctAPIResponse {
		t.Fatalf("content type = %q", fr.ContentType)
	}
	if string(fr.Body) != "+OK 9a1b2c3d-0000-4000-8000-000000000001" {
		t.Fatalf("body = %q", fr.Body)
	}
	if fr.Result() != "+OK 9a1b2c3d-0000-4000-8000-000000000001" {
		t.Fatalf("Result() on api/response must be the body, got %q", fr.Result())
	}
}

func TestGoldenFrameEventPlainChannelAnswer(t *testing.T) {
	// A CHANNEL_ANSWER event body as mod_event_socket serializes it for
	// `event plain` subscribers (header order is the serializer's sorted
	// key order, which eventFrame preserves). Pinned verbatim.
	const goldenAnswerBody = "Answer-State: answered\n" +
		"Caller-Destination-Number: +254711111111\n" +
		"Core-UUID: 7f4db1e6-2b43-4f6a-9f1e-3c8d5a2b7e90\n" +
		"Event-Name: CHANNEL_ANSWER\n" +
		"Unique-ID: 11111111-1111-1111-1111-111111111111\n"
	frame := eventFrame(map[string]string{
		"Event-Name":                "CHANNEL_ANSWER",
		"Core-UUID":                 "7f4db1e6-2b43-4f6a-9f1e-3c8d5a2b7e90",
		"Unique-ID":                 "11111111-1111-1111-1111-111111111111",
		"Answer-State":              "answered",
		"Caller-Destination-Number": "+254711111111",
	}, "")

	want := "Content-Type: text/event-plain\nContent-Length: 187\n\n" + goldenAnswerBody
	if len(goldenAnswerBody) != 187 {
		t.Fatalf("golden body length drifted: %d", len(goldenAnswerBody))
	}
	if string(frame) != want {
		t.Fatalf("event frame bytes mismatch:\n got: %q\nwant: %q", string(frame), want)
	}

	fr := decodeOne(t, frame)
	if fr.ContentType != ctEventPlain {
		t.Fatalf("content type = %q", fr.ContentType)
	}
	fields, payload, err := ParseEvent(fr.Body)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if payload != "" {
		t.Fatalf("unexpected payload %q", payload)
	}
	if EventField(fields, "Event-Name") != "CHANNEL_ANSWER" ||
		EventField(fields, "Unique-ID") != "11111111-1111-1111-1111-111111111111" ||
		EventField(fields, "Answer-State") != "answered" {
		t.Fatalf("parsed fields wrong: %v", fields)
	}
	// Header lookup must be case-insensitive end to end.
	if fr.Header("content-length") == "" {
		t.Fatalf("case-insensitive header lookup failed")
	}
}

func TestGoldenFrameEventPlainBackgroundJobWithPayload(t *testing.T) {
	// BACKGROUND_JOB carries the api result as a payload after the blank
	// line — both +OK (success) and -ERR (dial failure) shapes parse.
	fields := map[string]string{
		"Event-Name":  "BACKGROUND_JOB",
		"Job-UUID":    "9a1b2c3d-0000-4000-8000-000000000001",
		"Job-Command": "originate",
	}
	frame := eventFrame(fields, "-ERR NO_ANSWER")

	fr := decodeOne(t, frame)
	gotFields, payload, err := ParseEvent(fr.Body)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if EventField(gotFields, "Job-UUID") != "9a1b2c3d-0000-4000-8000-000000000001" ||
		EventField(gotFields, "Event-Name") != "BACKGROUND_JOB" {
		t.Fatalf("parsed fields wrong: %v", gotFields)
	}
	if payload != "-ERR NO_ANSWER" {
		t.Fatalf("payload = %q, want -ERR NO_ANSWER", payload)
	}

	frame = eventFrame(fields, "+OK 11111111-1111-1111-1111-111111111111")
	_, payload, err = ParseEvent(decodeOne(t, frame).Body)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if payload != "+OK 11111111-1111-1111-1111-111111111111" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestParseEventNoTrailingNewline(t *testing.T) {
	fields, payload, err := ParseEvent([]byte("Event-Name: CHANNEL_HANGUP\nUnique-ID: u1"))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if payload != "" || EventField(fields, "Event-Name") != "CHANNEL_HANGUP" || EventField(fields, "Unique-ID") != "u1" {
		t.Fatalf("parse wrong: fields=%v payload=%q", fields, payload)
	}
}

func TestParseEventCRLFAndDuplicateHeaders(t *testing.T) {
	fields, _, err := ParseEvent([]byte("Event-Name: X\r\nVariable-Foo: 1\r\nVariable-Foo: 2\r\n"))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if EventField(fields, "event-name") != "X" {
		t.Fatalf("lowercased lookup failed: %v", fields)
	}
	if EventField(fields, "variable-foo") != "2" {
		t.Fatalf("duplicate headers must keep the last value: %v", fields)
	}
}

func TestParseEventRejectsBadLines(t *testing.T) {
	if _, _, err := ParseEvent([]byte("no colon here\n")); err == nil {
		t.Fatal("expected error for header line without a colon")
	}
}

func TestFrameReaderRejectsOversizedAndTruncated(t *testing.T) {
	fr := newFrameReader(strings.NewReader("Content-Type: api/response\nContent-Length: 100\n\nshort"), 64)
	if _, err := fr.ReadFrame(); err == nil {
		t.Fatal("expected oversize Content-Length rejection")
	}

	fr = newFrameReader(strings.NewReader("Content-Type: api/response\nContent-Length: 100\n\nonly-nine"), maxFrameBytes)
	if _, err := fr.ReadFrame(); err == nil {
		t.Fatal("expected truncation error for short body")
	}

	fr = newFrameReader(strings.NewReader("Not-A-Header\n\n"), maxFrameBytes)
	if _, err := fr.ReadFrame(); err == nil {
		t.Fatal("expected first-header Content-Type enforcement")
	}

	fr = newFrameReader(strings.NewReader("Content-Type: api/response\nContent-Length: nope\n\n"), maxFrameBytes)
	if _, err := fr.ReadFrame(); err == nil {
		t.Fatal("expected bad Content-Length rejection")
	}
}

func TestFrameReaderReadsConsecutiveFrames(t *testing.T) {
	wire := string(replyFrame("+OK accepted", nil)) +
		string(apiResponseFrame("+OK 1")) +
		string(eventFrame(map[string]string{"Event-Name": "CHANNEL_HANGUP", "Unique-ID": "u1", "Hangup-Cause": "NORMAL_CLEARING"}, ""))
	fr := newFrameReader(bytes.NewReader([]byte(wire)), maxFrameBytes)

	f1, err := fr.ReadFrame()
	if err != nil || f1.ContentType != ctCommandReply || f1.ReplyText() != "+OK accepted" {
		t.Fatalf("frame 1: %v %+v", err, f1)
	}
	f2, err := fr.ReadFrame()
	if err != nil || f2.ContentType != ctAPIResponse || f2.Result() != "+OK 1" {
		t.Fatalf("frame 2: %v %+v", err, f2)
	}
	f3, err := fr.ReadFrame()
	if err != nil || f3.ContentType != ctEventPlain {
		t.Fatalf("frame 3: %v %+v", err, f3)
	}
	fields, _, _ := ParseEvent(f3.Body)
	if EventField(fields, "Hangup-Cause") != "NORMAL_CLEARING" {
		t.Fatalf("hangup cause = %q", EventField(fields, "Hangup-Cause"))
	}
}

// decodeOne parses exactly one frame from wire bytes.
func decodeOne(t *testing.T, wire []byte) *Frame {
	t.Helper()
	fr, err := newFrameReader(bytes.NewReader(wire), maxFrameBytes).ReadFrame()
	if err != nil {
		t.Fatalf("decode %q: %v", wire, err)
	}
	return fr
}
