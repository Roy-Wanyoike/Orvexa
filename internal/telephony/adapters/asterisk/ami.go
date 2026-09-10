package asterisk

import (
        "bufio"
        "crypto/md5"
        "encoding/hex"
        "errors"
        "fmt"
        "io"
        "strings"
)

// AMI wire framing (Asterisk Manager Interface, main/manager.c):
//
//   - every packet is a sequence of "Key: value" header lines terminated by
//     an empty line — on the wire CRLF CRLF; bare LF is accepted so adapters
//     keep working against proxies and older releases that emit \n;
//   - the first packet after connect is the banner: a single colonless line
//     such as "Asterisk Call Manager/8.0.0";
//   - responses carry "Response: Success|Failure|Error" and echo the
//     request's ActionID; events carry "Event: <Name>" and never block a
//     caller — they are what drives the adapter's event translation.
//
// Header matching is case-insensitive (deployments mix ActionID/ActionId);
// values are everything after the first colon with one leading space
// stripped, so values that contain colons survive round-trips.

const (
        // DefaultMaxLineLen bounds one header line. AMI values are short; a line
        // this long is a hostile or broken peer, not traffic.
        DefaultMaxLineLen = 8 * 1024
        // DefaultMaxPacketSize bounds one whole packet (all header lines) so a
        // peer cannot balloon reader memory with many small headers.
        DefaultMaxPacketSize = 1 << 20
        // DefaultMaxHeaders bounds the header count of a single packet.
        DefaultMaxHeaders = 128
)

// Codec errors. They surface from the reader when the peer violates the
// protocol bounds; the session treats them as fatal and reconnects.
var (
        ErrPacketTooLarge  = errors.New("asterisk: AMI packet exceeds protocol bounds")
        ErrMalformedPacket = errors.New("asterisk: malformed AMI packet")
)

// amiHeader is one wire header. Duplicates are preserved (Originate carries
// repeated Variable headers), so the packet is an ordered slice, not a map.
type amiHeader struct {
        Key   string
        Value string
}

// amiPacket is one parsed AMI packet: an ordered, duplicate-preserving
// header block.
type amiPacket struct {
        headers []amiHeader
}

// get returns the first value for the case-insensitive key.
func (p *amiPacket) get(key string) string {
        for _, h := range p.headers {
                if strings.EqualFold(h.Key, key) {
                        return h.Value
                }
        }
        return ""
}

// getAll returns every value for the case-insensitive key, in wire order.
func (p *amiPacket) getAll(key string) []string {
        var out []string
        for _, h := range p.headers {
                if strings.EqualFold(h.Key, key) {
                        out = append(out, h.Value)
                }
        }
        return out
}

// event returns the Event header value, or "" for non-event packets.
func (p *amiPacket) event() string { return p.get("Event") }

// isEvent reports whether the packet is a manager event.
func (p *amiPacket) isEvent() bool { return p.get("Event") != "" }

// response returns the Response header value, or "" for non-response packets.
func (p *amiPacket) response() string { return p.get("Response") }

// isResponse reports whether the packet is an action response.
func (p *amiPacket) isResponse() bool { return p.get("Response") != "" }

// actionID returns the echoed ActionID header value (may be empty).
func (p *amiPacket) actionID() string { return p.get("ActionID") }

// size returns the total header bytes, for packet-level bounds.
func (p *amiPacket) size() int {
        n := 0
        for _, h := range p.headers {
                n += len(h.Key) + len(h.Value) + 4 // "Key: value\r\n"
        }
        return n
}

// amiRequest is one client action: the Action name plus ordered headers.
// ActionID is injected by the session layer (correlation is not the
// builder's concern).
type amiRequest struct {
        Action  string
        Headers []amiHeader
}

// encode renders the request in AMI wire form ("Action" first, then headers
// in order, blank-line terminated with CRLF). Keys and values containing CR,
// LF or NUL are rejected — a caller-controlled value must never be able to
// inject an extra header or terminate the packet early.
func (r amiRequest) encode() ([]byte, error) {
        if r.Action == "" {
                return nil, fmt.Errorf("%w: action name is required", ErrMalformedPacket)
        }
        if err := checkHeaderToken("Action", r.Action); err != nil {
                return nil, err
        }
        var b strings.Builder
        b.WriteString("Action: ")
        b.WriteString(r.Action)
        b.WriteString("\r\n")
        for _, h := range r.Headers {
                if err := checkHeaderToken(h.Key, h.Value); err != nil {
                        return nil, err
                }
                b.WriteString(h.Key)
                b.WriteString(": ")
                b.WriteString(h.Value)
                b.WriteString("\r\n")
        }
        b.WriteString("\r\n")
        return []byte(b.String()), nil
}

// withActionID returns a copy of the request whose ActionID header is id
// (any existing ActionID header is replaced — never duplicated).
func (r amiRequest) withActionID(id string) amiRequest {
        out := amiRequest{Action: r.Action, Headers: make([]amiHeader, 0, len(r.Headers)+1)}
        out.Headers = append(out.Headers, amiHeader{Key: "ActionID", Value: id})
        for _, h := range r.Headers {
                if strings.EqualFold(h.Key, "ActionID") {
                        continue
                }
                out.Headers = append(out.Headers, h)
        }
        return out
}

// checkHeaderToken rejects CR, LF and NUL in header keys and values: the AMI
// framing is line-based, so a control character would terminate the line (or
// packet) early and smuggle attacker-controlled headers onto the wire.
func checkHeaderToken(key, value string) error {
        if key == "" {
                return fmt.Errorf("%w: empty header key", ErrMalformedPacket)
        }
        if strings.ContainsAny(value, "\r\n\x00") {
                return fmt.Errorf("%w: header %q value contains a control character (CR/LF/NUL)", ErrMalformedPacket, key)
        }
        if strings.ContainsAny(key, "\r\n\x00: ") {
                return fmt.Errorf("%w: header key %q contains a framing character", ErrMalformedPacket, key)
        }
        return nil
}

// amiReader parses an AMI byte stream into packets with protocol bounds.
type amiReader struct {
        br         *bufio.Reader
        maxLine    int
        maxPacket  int
        maxHeaders int
}

// newAMIReader wraps r with the default protocol bounds.
func newAMIReader(r io.Reader) *amiReader {
        return &amiReader{
                br:         bufio.NewReaderSize(r, DefaultMaxLineLen+64),
                maxLine:    DefaultMaxLineLen,
                maxPacket:  DefaultMaxPacketSize,
                maxHeaders: DefaultMaxHeaders,
        }
}

// next returns the next complete packet. It returns io.EOF when the stream
// ends cleanly at a packet boundary and an error for mid-packet truncation
// or bound violations. Stray blank lines between packets are skipped, so
// servers that pad the framing stay parseable.
func (r *amiReader) next() (amiPacket, error) {
        var pkt amiPacket
        for {
                line, err := r.readLine()
                if err != nil {
                        if errors.Is(err, io.EOF) && len(pkt.headers) == 0 {
                                return amiPacket{}, io.EOF // clean end between packets
                        }
                        if errors.Is(err, io.EOF) {
                                return amiPacket{}, fmt.Errorf("%w: stream ended mid-packet", ErrMalformedPacket)
                        }
                        return amiPacket{}, err
                }
                if line == "" {
                        if len(pkt.headers) > 0 {
                                return pkt, nil // blank line terminates the packet
                        }
                        continue // stray blank line between packets
                }
                key, value, ok := strings.Cut(line, ":")
                if !ok {
                        // Colonless line: the connect banner ("Asterisk Call
                        // Manager/8.0.0"). Kept verbatim as a single header with an
                        // empty value so the packet is never silently dropped.
                        pkt.headers = append(pkt.headers, amiHeader{Key: line})
                } else {
                        key = strings.TrimSpace(key)
                        if key == "" {
                                return amiPacket{}, fmt.Errorf("%w: header line %q has an empty key", ErrMalformedPacket, line)
                        }
                        value = strings.TrimPrefix(value, " ") // exactly one separator space
                        pkt.headers = append(pkt.headers, amiHeader{Key: key, Value: value})
                }
                if len(pkt.headers) > r.maxHeaders {
                        return amiPacket{}, fmt.Errorf("%w: more than %d headers", ErrPacketTooLarge, r.maxHeaders)
                }
                if pkt.size() > r.maxPacket {
                        return amiPacket{}, fmt.Errorf("%w: packet exceeds %d bytes", ErrPacketTooLarge, r.maxPacket)
                }
        }
}

// readLine returns the next line without its terminator. Both CRLF and bare
// LF terminate a line; a line longer than maxLine (or a buffer-full read)
// fails with ErrPacketTooLarge so a hostile peer cannot balloon memory.
func (r *amiReader) readLine() (string, error) {
        frag, err := r.br.ReadSlice('\n')
        if err == bufio.ErrBufferFull {
                // The bufio window filled before any newline: unbounded line.
                return "", fmt.Errorf("%w: line exceeds %d bytes", ErrPacketTooLarge, r.maxLine)
        }
        if err != nil {
                if errors.Is(err, io.EOF) && len(frag) > 0 {
                        return "", fmt.Errorf("%w: stream ended mid-line", ErrMalformedPacket)
                }
                return "", err // clean io.EOF or transport error
        }
        line := frag[:len(frag)-1] // drop '\n'
        if len(line) > 0 && line[len(line)-1] == '\r' {
                line = line[:len(line)-1] // drop '\r'
        }
        if len(line) > r.maxLine {
                return "", fmt.Errorf("%w: line exceeds %d bytes", ErrPacketTooLarge, r.maxLine)
        }
        return string(line), nil
}

// amiMD5Key derives the AMI challenge-response login key: the hex-encoded
// MD5 of secret+challenge, sent as the "Key:" header of the Login action.
// This is the standard AMI Challenge/AuthType=MD5 handshake — the raw secret
// never crosses the wire after connect. (MD5 is the protocol's choice, not a
// strength claim; network exposure is governed by manager.conf bindaddr.)
func amiMD5Key(secret, challenge string) string {
        sum := md5.Sum([]byte(secret + challenge))
        return hex.EncodeToString(sum[:])
}

// cleanWireValue makes a wire-provided string safe for reuse in logs, error
// messages and provider event payloads: control characters (which AMI
// framing guarantees cannot appear mid-line, but defense in depth keeps
// promises even against misbehaving peers) are dropped and the value is
// truncated to limit bytes.
func cleanWireValue(s string, limit int) string {
        if limit <= 0 {
                limit = DefaultMaxLineLen
        }
        b := make([]byte, 0, min(len(s), limit))
        for i := 0; i < len(s); i++ {
                if c := s[i]; c < 0x20 || c == 0x7f {
                        continue // control character: dropped
                }
                if len(b) == limit {
                        break // truncated
                }
                b = append(b, s[i])
        }
        return string(b)
}
