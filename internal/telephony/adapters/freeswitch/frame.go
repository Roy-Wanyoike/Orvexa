package freeswitch

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ESL content types observed on the wire (mod_event_socket).
const (
	ctAuthRequest     = "auth/request"
	ctCommandReply    = "command/reply"
	ctAPIResponse     = "api/response"
	ctEventPlain      = "text/event-plain"
	ctDisconnectNotic = "text/disconnect-notice"
	ctRudeRejection   = "text/rude-rejection"
)

// ESL header names this package reads.
const (
	hdrContentType = "content-type"
	hdrContentLen  = "content-length"
	hdrReplyText   = "reply-text"
	hdrChallenge   = "challenge"
	hdrJobUUID     = "job-uuid"
)

// Protocol sanity limits. maxFrameBytes bounds a single frame (headers and
// body): a hostile or broken peer must not be able to make the client buffer
// unbounded memory. maxHeaderBytes bounds one header line.
const (
	maxFrameBytes  = 8 << 20 // 8 MiB
	maxHeaderBytes = 4 << 10 // 4 KiB
	maxHeaders     = 128
)

// Protocol framing errors.
var (
	errFrameTooLarge  = errors.New("freeswitch: esl frame exceeds the size limit")
	errMalformedFrame = errors.New("freeswitch: malformed esl frame")
	errShortFrame     = errors.New("freeswitch: truncated esl frame")
)

// Frame is one parsed ESL protocol frame: a header block plus an optional
// Content-Length delimited body. Header names are matched case-insensitively
// (ESL header casing is not stable across versions).
type Frame struct {
	ContentType string
	Body        []byte
	headers     []hkv
}

type hkv struct{ name, val string }

// Header returns the first value for the named header (case-insensitive),
// or "" when absent.
func (f *Frame) Header(name string) string {
	if f == nil {
		return ""
	}
	for _, h := range f.headers {
		if strings.EqualFold(h.name, name) {
			return h.val
		}
	}
	return ""
}

// ReplyText is the "+OK ..."/"-ERR ..." result of a command/reply frame, or
// "" when the frame carries none.
func (f *Frame) ReplyText() string { return f.Header(hdrReplyText) }

// Result renders the command outcome regardless of reply shape: command/reply
// frames carry it in Reply-Text, api/response frames in the body.
func (f *Frame) Result() string {
	if f == nil {
		return ""
	}
	if f.ContentType == ctAPIResponse {
		return strings.TrimRight(string(f.Body), "\n")
	}
	return f.ReplyText()
}

// frameReader decodes ESL frames from a byte stream. limit caps the accepted
// frame size (tests shrink it to exercise the guard).
type frameReader struct {
	br    *bufio.Reader
	limit int
}

func newFrameReader(r io.Reader, limit int) *frameReader {
	if limit <= 0 {
		limit = maxFrameBytes
	}
	return &frameReader{br: bufio.NewReader(r), limit: limit}
}

// ReadFrame decodes exactly one frame. The grammar is: header lines
// ("Name: value") terminated by an empty line, then Content-Length bytes of
// body when the Content-Length header is present.
func (fr *frameReader) ReadFrame() (*Frame, error) {
	f := &Frame{}
	sawCT := false
	for i := 0; ; i++ {
		if i > maxHeaders {
			return nil, fmt.Errorf("%w: too many headers", errMalformedFrame)
		}
		line, err := fr.readLine()
		if err != nil {
			return nil, err
		}
		if line == "" {
			break // blank line: end of header block
		}
		name, val, ok := splitHeader(line)
		if !ok {
			return nil, fmt.Errorf("%w: bad header line %q", errMalformedFrame, line)
		}
		if i == 0 && !strings.EqualFold(name, hdrContentType) {
			return nil, fmt.Errorf("%w: first header must be Content-Type, got %q", errMalformedFrame, name)
		}
		if strings.EqualFold(name, hdrContentType) {
			f.ContentType = val
			sawCT = true
		}
		f.headers = append(f.headers, hkv{name: name, val: val})
	}
	if !sawCT {
		return nil, fmt.Errorf("%w: missing Content-Type", errMalformedFrame)
	}
	if cl := f.Header(hdrContentLen); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%w: bad Content-Length %q", errMalformedFrame, cl)
		}
		if n > fr.limit {
			return nil, errFrameTooLarge
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(fr.br, body); err != nil {
			return nil, fmt.Errorf("%w: body: %v", errShortFrame, err)
		}
		f.Body = body
	}
	return f, nil
}

// readLine reads one "\n" (or "\r\n") terminated line, bounded by the frame
// limit. The trailing newline/CR is stripped.
func (fr *frameReader) readLine() (string, error) {
	var buf []byte
	for {
		chunk, err := fr.br.ReadSlice('\n')
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return trimLine(buf, fr.limit)
		case errors.Is(err, bufio.ErrBufferFull):
			if len(buf) > fr.limit {
				return "", errFrameTooLarge
			}
			continue // header line longer than bufio's buffer: keep reading
		default:
			if len(buf) > 0 && errors.Is(err, io.EOF) {
				return "", fmt.Errorf("%w: EOF inside a frame", errShortFrame)
			}
			return "", err
		}
	}
}

func trimLine(buf []byte, limit int) (string, error) {
	if len(buf) > limit {
		return "", errFrameTooLarge
	}
	line := string(buf)
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r"), nil
}

// splitHeader splits "Name: value" at the first colon; ESL forbids empty
// names and consumes the single separating space after the colon (values may
// themselves contain colons and trailing whitespace is significant).
func splitHeader(line string) (name, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	name, val = line[:i], line[i+1:]
	if strings.TrimSpace(name) == "" {
		return "", "", false
	}
	return name, strings.TrimPrefix(val, " "), true
}

// ParseEvent decodes a text/event-plain body: "Name: value" header lines
// followed by an optional blank line and a free-form payload (the api result
// text for BACKGROUND_JOB frames). Duplicate header names keep the last
// value.
func ParseEvent(body []byte) (fields map[string]string, payload string, err error) {
	fields = map[string]string{}
	rest := body
	for len(rest) > 0 {
		var line []byte
		if i := indexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			line, rest = rest, nil
		}
		trimmed := strings.TrimSuffix(string(line), "\r")
		if trimmed == "" {
			break // blank line: payload follows (if any)
		}
		name, val, ok := splitHeader(trimmed)
		if !ok {
			return nil, "", fmt.Errorf("%w: bad event line %q", errMalformedFrame, trimmed)
		}
		fields[strings.ToLower(name)] = val
	}
	payload = ""
	if len(rest) > 0 {
		payload = string(rest)
	}
	return fields, payload, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// EventField is a case-insensitive field lookup over parsed event headers
// (event header casing varies; keys are normalized to lowercase in
// ParseEvent).
func EventField(fields map[string]string, name string) string {
	return fields[strings.ToLower(name)]
}

// command is the wire encoding of one client command line: the command text
// plus the ESL line terminator.
func command(text string) []byte {
	return []byte(text + "\n")
}

// eventFrame serializes a text/event-plain frame the way mod_event_socket
// does: Content-Type and Content-Length headers, blank line, then the
// serialized event (headers, optional blank line, payload). Used by the
// in-package test server and the golden tests.
func eventFrame(fields map[string]string, payload string) []byte {
	var b strings.Builder
	b.WriteString("Content-Type: text/event-plain\n")
	b.WriteString("Content-Length: ")
	b.WriteString(strconv.Itoa(len(payloadBody(fields, payload))))
	b.WriteString("\n\n")
	b.WriteString(payloadBody(fields, payload))
	return []byte(b.String())
}

func payloadBody(fields map[string]string, payload string) string {
	var b strings.Builder
	for _, k := range sortedKeys(fields) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(fields[k])
		b.WriteString("\n")
	}
	body := b.String()
	if payload != "" {
		body += "\n" + payload
	}
	return body
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// replyFrame serializes a command/reply frame (Reply-Text plus optional
// extra headers) — test server and golden tests only.
func replyFrame(replyText string, extra map[string]string) []byte {
	var b strings.Builder
	b.WriteString("Content-Type: command/reply\n")
	b.WriteString("Reply-Text: ")
	b.WriteString(replyText)
	b.WriteString("\n")
	for _, k := range sortedKeys(extra) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(extra[k])
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return []byte(b.String())
}

// apiResponseFrame serializes an api/response frame with a Content-Length
// delimited body.
func apiResponseFrame(body string) []byte {
	var b strings.Builder
	b.WriteString("Content-Type: api/response\n")
	b.WriteString("Content-Length: ")
	b.WriteString(strconv.Itoa(len(body)))
	b.WriteString("\n\n")
	b.WriteString(body)
	return []byte(b.String())
}

// bareFrame serializes a header-only frame (e.g. the initial auth/request).
func bareFrame(contentType string, extra map[string]string) []byte {
	var b strings.Builder
	b.WriteString("Content-Type: ")
	b.WriteString(contentType)
	b.WriteString("\n")
	for _, k := range sortedKeys(extra) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(extra[k])
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return []byte(b.String())
}
