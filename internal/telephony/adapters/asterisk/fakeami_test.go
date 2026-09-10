package asterisk

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Credentials shared by the fake AMI server and every test that dials it.
// The secret is deliberately long and distinctive so leakage tests can scan
// logs and errors for its raw value; it is test-only material.
const (
	testAMIUsername = "orvexa-mgr-user"
	testAMISecret   = "ami-test-secret-VALUE-777"
	testMOHContext  = "orvexa-hold"
)

// fakeChannel is the fake PBX's per-channel state: enough of Asterisk's
// channel model to drive the adapter's correlation and MOH behaviors.
type fakeChannel struct {
	name     string
	uniqueid string
	exten    string
	context  string
}

// fakeRedirect is one observed Redirect action.
type fakeRedirect struct {
	channel string
	context string
	exten   string
}

// fakeOriginate is one observed Originate action.
type fakeOriginate struct {
	actionID string
	channel  string
	callerID string
}

// fakeAMI is an in-package AMI server for tests: banner + Challenge→MD5
// login handshake, per-action replies (Originate/Redirect/Hangup/Ping/
// Logoff), scripted event injection, and behavior knobs for reconnect,
// timeout and protocol-error tests. It parses client traffic with the same
// codec the adapter uses, so any writer interleaving bug shows up as a
// server-side parse failure.
//
// All writes to one connection happen on that connection's serve goroutine —
// replies are written first, then scripted events — mirroring Asterisk's
// response-then-async-events ordering without extra locking.
type fakeAMI struct {
	t  *testing.T
	ln net.Listener

	username   string
	secret     string
	mohContext string

	mu          sync.Mutex
	seq         int
	loginCount  int
	actionSeen  int
	realActions int
	channels    map[string]*fakeChannel
	originate   []fakeOriginate
	redirects   []fakeRedirect
	hangups     []string
	parseErrs   []string
	conns       []net.Conn
	closed      bool

	// behavior knobs (set before start)
	dropAfterActions     int             // drop the connection after N answered actions per connection
	muteActions          map[string]bool // action names never answered
	failHangup           bool            // Hangup replies Failure "No such channel"
	omitOriginateChannel bool            // exercise Newchannel-ActionID binding instead
	strayReplyAfterFirst bool            // send one unsolicited reply (robustness)

	wg sync.WaitGroup
}

type fakeAMIOpt func(*fakeAMI)

func withDropAfterActions(n int) fakeAMIOpt { return func(f *fakeAMI) { f.dropAfterActions = n } }
func withMutedAction(action string) fakeAMIOpt {
	return func(f *fakeAMI) {
		if f.muteActions == nil {
			f.muteActions = map[string]bool{}
		}
		f.muteActions[action] = true
	}
}
func withFailingHangup() fakeAMIOpt        { return func(f *fakeAMI) { f.failHangup = true } }
func withOmitOriginateChannel() fakeAMIOpt { return func(f *fakeAMI) { f.omitOriginateChannel = true } }
func withStrayReply() fakeAMIOpt           { return func(f *fakeAMI) { f.strayReplyAfterFirst = true } }

// withAuth overrides the fake's accepted credentials — the auth-failure
// tests dial with the shared test secret, which this fake must reject.
func withAuth(username, secret string) fakeAMIOpt {
	return func(f *fakeAMI) { f.username, f.secret = username, secret }
}

// startFakeAMI launches the server on a loopback port and registers cleanup.
func startFakeAMI(t *testing.T, opts ...fakeAMIOpt) *fakeAMI {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake AMI listen: %v", err)
	}
	f := &fakeAMI{
		t:           t,
		ln:          ln,
		username:    testAMIUsername,
		secret:      testAMISecret,
		mohContext:  testMOHContext,
		channels:    map[string]*fakeChannel{},
		muteActions: map[string]bool{},
	}
	for _, opt := range opts {
		opt(f)
	}
	f.wg.Add(1)
	go f.acceptLoop()
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAMI) addr() string { return f.ln.Addr().String() }

func (f *fakeAMI) port() string {
	return fmt.Sprint(f.ln.Addr().(*net.TCPAddr).Port)
}

// Close stops the listener, drops every connection and waits for the serve
// goroutines. Safe to call more than once.
func (f *fakeAMI) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	conns := append([]net.Conn(nil), f.conns...)
	f.conns = nil
	f.mu.Unlock()

	f.ln.Close()
	for _, c := range conns {
		c.Close()
	}
	f.wg.Wait()
}

func (f *fakeAMI) acceptLoop() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			conn.Close()
			return
		}
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		f.wg.Add(1)
		go f.serveConn(conn)
	}
}

// serveConn runs one connection: banner, then a Challenge→MD5-guarded
// action loop parsed with the production codec.
func (f *fakeAMI) serveConn(conn net.Conn) {
	defer f.wg.Done()
	defer conn.Close()

	f.mu.Lock()
	f.seq++
	nonce := f.seq
	f.mu.Unlock()
	challenge := fmt.Sprintf("orvexa-challenge-%d", nonce)

	w := &connWriter{conn: conn}
	fmt.Fprintf(w, "Asterisk Call Manager/8.0.0\r\n\r\n")

	reader := newAMIReader(conn)
	straySent := false
	served := 0 // real actions answered on THIS connection
	for {
		pkt, err := reader.next()
		if err != nil {
			if !errors.Is(err, io.EOF) && connHealthy(err) {
				f.mu.Lock()
				f.parseErrs = append(f.parseErrs, err.Error())
				f.mu.Unlock()
			}
			return
		}
		action := pkt.get("Action")
		if action == "" {
			continue
		}
		// actionSeen counts every action packet; realActions excludes the
		// handshake. Muted actions are read and never answered — the knob
		// models a half-open PBX for keepalive tests.
		f.mu.Lock()
		f.actionSeen++
		f.mu.Unlock()
		if f.muteActions[action] {
			continue
		}
		f.mu.Lock()
		if action != "Challenge" && action != "Login" {
			f.realActions++
			served++ // only real actions count toward dropAfterActions
		}
		f.mu.Unlock()
		switch action {
		case "Challenge":
			fmt.Fprintf(w, "Response: Success\r\nActionID: %s\r\nChallenge: %s\r\n\r\n", pkt.actionID(), challenge)
		case "Login":
			userOK := pkt.get("Username") == f.username
			keyOK := pkt.get("Key") == amiMD5Key(f.secret, challenge)
			f.mu.Lock()
			f.loginCount++
			f.mu.Unlock()
			if userOK && keyOK {
				fmt.Fprintf(w, "Response: Success\r\nActionID: %s\r\nMessage: Authentication accepted\r\n\r\n", pkt.actionID())
				// Real Asterisk emits FullyBooted immediately after a
				// successful Login — often buffered in the same TCP
				// segment as the login reply. The client must carry
				// the handshake reader's buffered bytes into its
				// serve loop instead of dropping the window.
				fmt.Fprintf(w, "Event: FullyBooted\r\nPrivilege: system,all\r\nStatus: Fully Booted\r\n\r\n")
			} else {
				fmt.Fprintf(w, "Response: Error\r\nActionID: %s\r\nMessage: Authentication failed\r\n\r\n", pkt.actionID())
			}
		case "Ping":
			fmt.Fprintf(w, "Response: Pong\r\nActionID: %s\r\nPing: Pong\r\nTimestamp: %d\r\n\r\n",
				pkt.actionID(), time.Now().UnixMicro())
		case "Originate":
			f.handleOriginate(w, pkt)
		case "Redirect":
			f.handleRedirect(w, pkt)
		case "Hangup":
			f.handleHangup(w, pkt)
		case "Logoff":
			fmt.Fprintf(w, "Response: Goodbye\r\nActionID: %s\r\nMessage: Thanks for all the fish.\r\n\r\n", pkt.actionID())
			return
		default:
			fmt.Fprintf(w, "Response: Error\r\nActionID: %s\r\nMessage: Unknown or unhandled action\r\n\r\n", pkt.actionID())
		}

		if f.strayReplyAfterFirst && !straySent && isCorrelationProbeAction(action) {
			straySent = true
			fmt.Fprintf(w, "Response: Success\r\nActionID: ovx-nobody-owns-this\r\nMessage: stray\r\n\r\n")
		}

		// served was already incremented inside the lock above for real
		// (non-handshake) actions; handshake actions never count toward
		// dropAfterActions — the knob models losing a live session, not
		// breaking the login itself.
		if f.dropAfterActions > 0 && served >= f.dropAfterActions {
			return // drop without goodbye: exercises client reconnect
		}
	}
}

func connHealthy(err error) bool {
	// Clean transport ends are not codec failures.
	s := err.Error()
	return !strings.Contains(s, "use of closed") && !strings.Contains(s, "connection reset") && !strings.Contains(s, "broken pipe")
}

// isCorrelationProbeAction reports whether the action is one the client
// sends after the handshake — only those are safe positions for the stray
// reply (the handshake reads exactly one reply packet per request).
func isCorrelationProbeAction(action string) bool {
	switch action {
	case "Ping", "Originate", "Redirect", "Hangup":
		return true
	}
	return false
}

type connWriter struct{ conn net.Conn }

func (w *connWriter) Write(p []byte) (int, error) { return w.conn.Write(p) }

func (f *fakeAMI) handleOriginate(w io.Writer, pkt amiPacket) {
	f.mu.Lock()
	f.seq++
	n := f.seq
	ch := &fakeChannel{
		name:     fmt.Sprintf("PJSIP/orvexa-%06d", n),
		uniqueid: fmt.Sprintf("orvexa-unique-%d", n),
		exten:    pkt.get("Exten"),
		context:  pkt.get("Context"),
	}
	f.channels[ch.name] = ch
	rec := fakeOriginate{actionID: pkt.actionID(), channel: ch.name, callerID: pkt.get("CallerID")}
	f.originate = append(f.originate, rec)
	omit := f.omitOriginateChannel
	f.mu.Unlock()

	reply := "Response: Success\r\n" +
		"ActionID: " + pkt.actionID() + "\r\n" +
		"Message: Originate successfully queued\r\n"
	if !omit {
		reply += "Channel: " + ch.name + "\r\n"
	}
	reply += "\r\n"
	fmt.Fprint(w, reply)

	// Scripted lifecycle, in Asterisk's wire order after the response:
	// channel creation (ActionID-echoed), ringing, then answer.
	fmt.Fprintf(w, "Event: Newchannel\r\n"+
		"Privilege: call,all\r\n"+
		"Channel: %s\r\n"+
		"Uniqueid: %s\r\n"+
		"ChannelStateDesc: Down\r\n"+
		"ChannelState: 0\r\n"+
		"CallerIDNum: %s\r\n"+
		"ActionID: %s\r\n\r\n",
		ch.name, ch.uniqueid, rec.callerID, pkt.actionID())
	f.sendNewstate(w, ch, "Ringing", 4)
	f.sendNewstate(w, ch, "Up", 6)
}

// handleRedirect models the PBX's redirect semantics:
//   - into the MOH context: the channel parks on hold — silent (a held
//     channel stays Up; no lifecycle-visible state change);
//   - to a NEW destination: the destination rings — a Newstate Ringing on
//     the same channel;
//   - back to the current destination (resume): silent, nothing re-rings.
func (f *fakeAMI) handleRedirect(w io.Writer, pkt amiPacket) {
	red := fakeRedirect{channel: pkt.get("Channel"), context: pkt.get("Context"), exten: pkt.get("Exten")}
	f.mu.Lock()
	f.redirects = append(f.redirects, red)
	ch := f.channels[red.channel]
	f.mu.Unlock()
	if ch == nil {
		fmt.Fprintf(w, "Response: Failure\r\nActionID: %s\r\nMessage: No such channel\r\n\r\n", pkt.actionID())
		return
	}
	if red.context == f.mohContext {
		fmt.Fprintf(w, "Response: Success\r\nActionID: %s\r\nMessage: Redirect successful\r\n\r\n", pkt.actionID())
		return
	}
	f.mu.Lock()
	moved := ch.exten != red.exten
	if moved {
		ch.exten = red.exten
		ch.context = red.context
	}
	f.mu.Unlock()
	fmt.Fprintf(w, "Response: Success\r\nActionID: %s\r\nMessage: Redirect successful\r\n\r\n", pkt.actionID())
	if moved {
		f.sendNewstate(w, ch, "Ringing", 4)
	}
}

func (f *fakeAMI) handleHangup(w io.Writer, pkt amiPacket) {
	f.mu.Lock()
	ch := f.channels[pkt.get("Channel")]
	f.hangups = append(f.hangups, pkt.get("Channel"))
	fail := f.failHangup
	f.mu.Unlock()
	if fail || ch == nil {
		fmt.Fprintf(w, "Response: Failure\r\nActionID: %s\r\nMessage: No such channel\r\n\r\n", pkt.actionID())
		return
	}
	fmt.Fprintf(w, "Response: Success\r\nActionID: %s\r\nMessage: Channel destroyed\r\n\r\n", pkt.actionID())
	fmt.Fprintf(w, "Event: Hangup\r\n"+
		"Privilege: call,all\r\n"+
		"Channel: %s\r\n"+
		"Uniqueid: %s\r\n"+
		"CallerIDNum: \r\n"+
		"Cause: 16\r\n"+
		"Cause-txt: Normal Clearing\r\n\r\n", ch.name, ch.uniqueid)
}

func (f *fakeAMI) sendNewstate(w io.Writer, ch *fakeChannel, state string, code int) {
	fmt.Fprintf(w, "Event: Newstate\r\n"+
		"Privilege: call,all\r\n"+
		"Channel: %s\r\n"+
		"Uniqueid: %s\r\n"+
		"ChannelStateDesc: %s\r\n"+
		"ChannelState: %d\r\n\r\n", ch.name, ch.uniqueid, state, code)
}

// injectRaw writes an arbitrary packet onto the most recent connection —
// used to feed events for channels the adapter never placed (the platform
// must stay silent about legs it does not own).
func (f *fakeAMI) injectRaw(raw string) {
	f.mu.Lock()
	conns := append([]net.Conn(nil), f.conns...)
	f.mu.Unlock()
	if len(conns) == 0 {
		f.t.Fatal("injectRaw: no live fake AMI connection")
	}
	if _, err := conns[len(conns)-1].Write([]byte(raw)); err != nil {
		f.t.Fatalf("injectRaw: %v", err)
	}
}

// --- observation helpers (all mutex-guarded) ---

func (f *fakeAMI) loginAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCount
}

func (f *fakeAMI) parseErrorCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.parseErrs)
}

func (f *fakeAMI) observedRedirects() []fakeRedirect {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRedirect(nil), f.redirects...)
}

func (f *fakeAMI) observedHangups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hangups...)
}
