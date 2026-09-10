package freeswitch

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeSwitch is the in-package test double for mod_event_socket: a minimal
// but wire-faithful ESL server. It speaks the same framing the golden tests
// pin (bareFrame/replyFrame/apiResponseFrame/eventFrame are the production
// serializers), so the adapter is exercised against real wire bytes:
//
//   - handshake: opens with auth/request (optionally advertising a
//     Challenge), validates the auth line (plaintext or md5 form), replies
//     command/reply +OK, or -ERR + close on a wrong password;
//   - "events plain <names>": records the subscription, registers the
//     session as the event sink, replies +OK;
//   - "bgapi originate {origination_uuid=X}...": mints a Job-UUID, replies
//     command/reply +OK Job-UUID, then asynchronously injects the
//     BACKGROUND_JOB event (+OK <uuid> or the configured failure payload)
//     followed by CHANNEL_ANSWER for the originated channel;
//   - "api uuid_kill/uuid_transfer/uuid_hold": +OK for known channels
//     (uuid_kill also injects CHANNEL_HANGUP with a hangup cause), -ERR
//     "no such channel" otherwise;
//   - restart(): kills the listener and every session while keeping switch
//     state, then re-opens the same address — the reconnect test's switch
//     reboot.
//
// It exists only in tests: production builds carry no test server.
type fakeSwitch struct {
	t *testing.T

	password  string // expected ESL password
	challenge string // when non-empty, advertised on auth/request

	// originateResult overrides the BACKGROUND_JOB payload for originate
	// jobs (default "+OK <origination uuid>"; "-ERR NO_ANSWER" exercises the
	// dial-failure correlation path).
	originateResult string
	// progressDelay / answerDelay pace the injected event stream so tests
	// can observe ordering without busy waits.
	progressDelay time.Duration
	answerDelay   time.Duration
	// replyDelay, when set via setReplyDelay, returns how long the server
	// should stall before replying to a command line (command-timeout tests).
	replyDelay func(line string) time.Duration

	ln net.Listener

	mu         sync.Mutex
	closed     bool
	conns      map[*fakeSession]struct{}
	sink       *fakeSession        // session currently subscribed to events
	origins    map[string]string   // Job-UUID -> origination uuid
	channels   map[string]struct{} // channel uuids the switch knows
	subscribes int                 // how many "events plain" were accepted
	subscribed string              // last accepted subscription argument
	originate  []string            // wire log of originate command lines
	wg         sync.WaitGroup
}

func newFakeSwitch(t *testing.T) *fakeSwitch {
	t.Helper()
	fs := &fakeSwitch{
		t:             t,
		password:      "ClueCon",
		progressDelay: 5 * time.Millisecond,
		answerDelay:   10 * time.Millisecond,
		conns:         map[*fakeSession]struct{}{},
		origins:       map[string]string{},
		channels:      map[string]struct{}{},
	}
	fs.listen()
	t.Cleanup(fs.close)
	return fs
}

func (fs *fakeSwitch) listen() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fs.t.Fatalf("fake switch listen: %v", err)
	}
	fs.mu.Lock()
	fs.ln = ln
	fs.mu.Unlock()
	fs.wg.Add(1)
	go fs.acceptLoop(ln)
}

func (fs *fakeSwitch) addr() string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.ln == nil {
		return ""
	}
	return fs.ln.Addr().String()
}

// close shuts the switch down for good (idempotent; also run by t.Cleanup).
func (fs *fakeSwitch) close() {
	fs.mu.Lock()
	if fs.closed {
		fs.mu.Unlock()
		return
	}
	fs.closed = true
	ln := fs.ln
	fs.ln = nil
	sessions := fs.takeSessionsLocked()
	fs.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, s := range sessions {
		_ = s.nc.Close()
	}
	fs.wg.Wait()
}

// takeSessionsLocked detaches every live session. Callers hold fs.mu.
func (fs *fakeSwitch) takeSessionsLocked() []*fakeSession {
	sessions := make([]*fakeSession, 0, len(fs.conns))
	for s := range fs.conns {
		sessions = append(sessions, s)
	}
	fs.conns = map[*fakeSession]struct{}{}
	fs.sink = nil
	return sessions
}

// restart simulates a switch reboot: every TCP session dies and the
// listener re-opens on the same address; switch state (known jobs and
// channels) survives.
func (fs *fakeSwitch) restart() {
	fs.mu.Lock()
	if fs.closed || fs.ln == nil {
		fs.mu.Unlock()
		return
	}
	addr := fs.ln.Addr().String()
	ln := fs.ln
	sessions := fs.takeSessionsLocked()
	fs.mu.Unlock()

	_ = ln.Close()
	for _, s := range sessions {
		_ = s.nc.Close()
	}

	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		fs.t.Fatalf("fake switch restart listen %s: %v", addr, err)
	}
	fs.mu.Lock()
	if fs.closed {
		fs.mu.Unlock()
		_ = ln2.Close()
		return
	}
	fs.ln = ln2
	fs.mu.Unlock()
	fs.wg.Add(1)
	go fs.acceptLoop(ln2)
}

func (fs *fakeSwitch) acceptLoop(ln net.Listener) {
	defer fs.wg.Done()
	for {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		fs.mu.Lock()
		if fs.closed {
			fs.mu.Unlock()
			_ = nc.Close()
			return
		}
		s := &fakeSession{srv: fs, nc: nc, br: bufio.NewReader(nc)}
		fs.conns[s] = struct{}{}
		fs.mu.Unlock()
		fs.wg.Add(1)
		go s.serve()
	}
}

func (fs *fakeSwitch) drop(s *fakeSession) {
	fs.mu.Lock()
	delete(fs.conns, s)
	if fs.sink == s {
		fs.sink = nil
	}
	fs.mu.Unlock()
	_ = s.nc.Close()
}

// write sends one frame to the session's socket. Writes are serialized per
// session so concurrent injectors (async originate jobs, test goroutines)
// cannot interleave frame bytes mid-frame.
func (s *fakeSession) write(b []byte) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = s.nc.Write(b)
}

// injectEvent pushes one text/event-plain frame to the subscribed session.
// With no subscriber the event is dropped — a switch does not buffer events
// for an unsubscribed client either.
func (fs *fakeSwitch) injectEvent(fields map[string]string, payload string) {
	fs.mu.Lock()
	sink := fs.sink
	fs.mu.Unlock()
	if sink == nil {
		return
	}
	sink.write(eventFrame(fields, payload))
}

func (fs *fakeSwitch) numSubscribes() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.subscribes
}

func (fs *fakeSwitch) subscribedEvents() string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.subscribed
}

// originateLines returns the wire log of originate commands the switch saw.
func (fs *fakeSwitch) originateLines() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]string, len(fs.originate))
	copy(out, fs.originate)
	return out
}

// forgetChannel makes the switch lose track of a channel it previously
// originated — the server-side stand-in for a channel that died between the
// dial and a later command ("-ERR no such channel" reply path).
func (fs *fakeSwitch) forgetChannel(uuid string) {
	fs.mu.Lock()
	delete(fs.channels, uuid)
	fs.mu.Unlock()
}

// setOriginateResult makes subsequent originate jobs complete with the
// given api result payload (e.g. "-ERR NO_ANSWER").
func (fs *fakeSwitch) setOriginateResult(payload string) {
	fs.mu.Lock()
	fs.originateResult = payload
	fs.mu.Unlock()
}

func (s *fakeSession) serve() {
	defer s.srv.wg.Done()
	defer s.srv.drop(s)

	s.write(bareFrame(ctAuthRequest, s.challengeHeaders()))
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			return
		}
		if d := s.replyDelayFor(line); d > 0 {
			time.Sleep(d)
		}
		if !s.dispatch(strings.TrimSpace(line)) {
			return
		}
	}
}

func (s *fakeSession) challengeHeaders() map[string]string {
	s.srv.mu.Lock()
	defer s.srv.mu.Unlock()
	if s.srv.challenge == "" {
		return nil
	}
	return map[string]string{"Challenge": s.srv.challenge}
}

// setReplyDelay installs/removes the server-side reply stall.
func (fs *fakeSwitch) setReplyDelay(fn func(line string) time.Duration) {
	fs.mu.Lock()
	fs.replyDelay = fn
	fs.mu.Unlock()
}

func (s *fakeSession) replyDelayFor(line string) time.Duration {
	s.srv.mu.Lock()
	fn := s.srv.replyDelay
	s.srv.mu.Unlock()
	if fn == nil {
		return 0
	}
	return fn(strings.TrimSpace(line))
}

// dispatch handles one client command line; false ends the session.
func (s *fakeSession) dispatch(line string) bool {
	switch {
	case strings.HasPrefix(line, "auth "):
		return s.auth(line)

	case !s.authed:
		s.write(replyFrame("-ERR not authenticated", nil))
		return false

	case strings.HasPrefix(line, "events plain"):
		return s.subscribeEvents(line)

	case strings.HasPrefix(line, "bgapi originate "):
		return s.originate(line)

	case strings.HasPrefix(line, "bgapi "):
		s.write(replyFrame("+OK Job-UUID: "+uuid.NewString(), nil))
		return true

	case strings.HasPrefix(line, "api uuid_kill "):
		return s.uuidKill(line)

	case strings.HasPrefix(line, "api uuid_transfer "):
		return s.uuidTransfer(line)

	case strings.HasPrefix(line, "api uuid_hold "):
		return s.uuidHold(line)

	case strings.HasPrefix(line, "api status"):
		s.write(apiResponseFrame("+OK fake switch up"))
		return true

	case strings.HasPrefix(line, "api "):
		s.write(apiResponseFrame("-ERR command not implemented"))
		return true

	default:
		s.write(replyFrame("-ERR unknown command", nil))
		return false
	}
}

// auth validates the auth line: plaintext ("auth <pw>") against a
// challenge-less endpoint, or the md5 form ("auth md5:<hex>") when a
// Challenge was advertised. A wrong password gets -ERR and a disconnect.
// Runs on the session goroutine before any other command.
func (s *fakeSession) auth(line string) bool {
	s.srv.mu.Lock()
	password, challenge := s.srv.password, s.srv.challenge
	s.srv.mu.Unlock()

	want := "auth " + password
	if challenge != "" {
		sum := md5.Sum([]byte(password + challenge))
		want = "auth md5:" + hex.EncodeToString(sum[:])
	}
	if line != want {
		s.write(replyFrame("-ERR invalid password", nil))
		return false
	}
	s.authed = true
	s.write(replyFrame("+OK accepted", nil))
	return true
}

func (s *fakeSession) subscribeEvents(line string) bool {
	names := strings.TrimSpace(strings.TrimPrefix(line, "events plain"))
	s.srv.mu.Lock()
	s.srv.subscribes++
	s.srv.subscribed = names
	s.srv.sink = s
	s.srv.mu.Unlock()
	s.write(replyFrame("+OK event listener enabled plain", nil))
	return true
}

// originate implements the bgapi originate job: an immediate command/reply
// carrying the Job-UUID, then the BACKGROUND_JOB event (with the api result
// as payload) and, on success, CHANNEL_ANSWER for the originated channel.
func (s *fakeSession) originate(line string) bool {
	cmd := strings.TrimPrefix(line, "bgapi ")
	origUUID := originationUUID(cmd)
	if origUUID == "" {
		s.write(replyFrame("-ERR missing origination_uuid", nil))
		return true
	}

	s.srv.mu.Lock()
	job := uuid.NewString()
	s.srv.origins[job] = origUUID
	s.srv.channels[origUUID] = struct{}{}
	result := s.srv.originateResult
	progress, answer := s.srv.progressDelay, s.srv.answerDelay
	s.srv.originate = append(s.srv.originate, line)
	s.srv.mu.Unlock()

	if result == "" {
		result = "+OK " + origUUID
	}
	s.write(replyFrame("+OK Job-UUID: "+job, map[string]string{"Job-UUID": job}))

	go func() {
		time.Sleep(progress)
		s.srv.injectEvent(map[string]string{
			"Event-Name":  "BACKGROUND_JOB",
			"Job-UUID":    job,
			"Job-Command": "originate",
		}, result)
		if !strings.HasPrefix(result, "+OK") {
			return // dial failed: no CHANNEL_ANSWER follows
		}
		time.Sleep(answer)
		s.srv.injectEvent(map[string]string{
			"Event-Name":                "CHANNEL_ANSWER",
			"Unique-ID":                 origUUID,
			"Answer-State":              "answered",
			"Caller-Destination-Number": "+254711111111",
		}, "")
	}()
	return true
}

// originationUUID extracts the origination_uuid channel variable from an
// originate command ("originate {origination_uuid=X}...").
func originationUUID(cmd string) string {
	const key = "origination_uuid="
	i := strings.Index(cmd, key)
	if i < 0 {
		return ""
	}
	rest := cmd[i+len(key):]
	if j := strings.IndexByte(rest, '}'); j >= 0 {
		return rest[:j]
	}
	if f := strings.Fields(rest); len(f) > 0 {
		return f[0]
	}
	return ""
}

func (s *fakeSession) uuidKill(line string) bool {
	uuidArg := strings.TrimSpace(strings.TrimPrefix(line, "api uuid_kill "))
	s.srv.mu.Lock()
	_, known := s.srv.channels[uuidArg]
	if known {
		delete(s.srv.channels, uuidArg)
	}
	s.srv.mu.Unlock()
	if !known {
		s.write(apiResponseFrame("-ERR no such channel " + uuidArg))
		return true
	}
	s.write(apiResponseFrame("+OK"))
	go func() {
		time.Sleep(5 * time.Millisecond)
		s.srv.injectEvent(map[string]string{
			"Event-Name":   "CHANNEL_HANGUP",
			"Unique-ID":    uuidArg,
			"Hangup-Cause": "NORMAL_CLEARING",
		}, "")
	}()
	return true
}

func (s *fakeSession) uuidTransfer(line string) bool {
	fields := strings.Fields(strings.TrimPrefix(line, "api uuid_transfer "))
	if len(fields) == 0 {
		s.write(apiResponseFrame("-ERR usage: uuid_transfer <uuid> <dialplan> <context> <dest>"))
		return true
	}
	uuidArg, dest := fields[0], fields[len(fields)-1]
	s.srv.mu.Lock()
	_, known := s.srv.channels[uuidArg]
	s.srv.mu.Unlock()
	if !known {
		s.write(apiResponseFrame("-ERR no such channel " + uuidArg))
		return true
	}
	s.write(apiResponseFrame("+OK Transfer to " + dest))
	return true
}

func (s *fakeSession) uuidHold(line string) bool {
	fields := strings.Fields(strings.TrimPrefix(line, "api uuid_hold "))
	uuidArg := ""
	for i, f := range fields {
		if f == "on" || f == "off" {
			if i+1 < len(fields) {
				uuidArg = fields[i+1]
			}
			break
		}
	}
	if uuidArg == "" && len(fields) > 0 {
		uuidArg = fields[len(fields)-1]
	}
	s.srv.mu.Lock()
	_, known := s.srv.channels[uuidArg]
	s.srv.mu.Unlock()
	if !known {
		s.write(apiResponseFrame("-ERR no such channel " + uuidArg))
		return true
	}
	s.write(apiResponseFrame("+OK"))
	return true
}

// fakeSession is one accepted TCP connection. All command handling runs on
// the session goroutine (serve); only frame writes are shared.
type fakeSession struct {
	srv *fakeSwitch
	nc  net.Conn
	br  *bufio.Reader
	wmu sync.Mutex

	authed bool // set once by auth() on the session goroutine
}
