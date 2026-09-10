package freeswitch

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// Client lifecycle errors. errConnLost and errAuthRejected are wrapped into
// surfaced errors so callers can classify transient vs configuration
// failures with errors.Is.
var (
	errClientClosed = errors.New("freeswitch: client closed")
	errConnLost     = errors.New("freeswitch: esl connection lost")
	errAuthRejected = errors.New("freeswitch: esl authentication rejected")
)

// clientConfig is the ESL connection tuning. Zero durations get the
// documented defaults (applied in newESLClient).
type clientConfig struct {
	addr     string   // host:port of mod_event_socket
	password string   // ESL auth password (never logged)
	events   []string // event names for the `events plain` subscription

	dialTimeout time.Duration // TCP dial budget (default 5s)
	hsTimeout   time.Duration // auth handshake budget (default 10s)
	cmdTimeout  time.Duration // per-command budget when the caller passes no deadline (default 10s)
	idleTimeout time.Duration // read deadline between frames: half-open detection (default 60s)
	backoffBase time.Duration // reconnect backoff base (default 50ms)
	backoffMax  time.Duration // reconnect backoff cap (default 2s)
	maxFrame    int           // frame size cap (default maxFrameBytes)

	logger  *slog.Logger
	onEvent func(name string, fields map[string]string, payload string)
}

// eslClient owns the ESL TCP session: connect, authenticate, subscribe,
// execute commands, and stream events back through cfg.onEvent. It is safe
// for concurrent use.
//
// Concurrency model: one goroutine owns the connection for its whole life
// (handshake, then demux). Authentication is therefore single-flight by
// construction — concurrent callers never trigger parallel auths; they block
// in Do/ensureReady until the session is ready. At most one command is in
// flight at a time (single-flight command slot), because synchronous api
// replies carry no correlation id on the wire.
type eslClient struct {
	cfg   clientConfig
	cmdCh chan struct{} // single-flight command slot (capacity 1)

	mu      sync.Mutex
	closed  bool
	conn    *clientConn
	ready   bool // handshake + subscription completed on c.conn
	gen     uint64
	lastErr error
	notify  chan struct{} // closed (and replaced) on every state transition

	doneCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// clientConn is one live TCP session.
type clientConn struct {
	nc  net.Conn
	gen uint64

	wmu sync.Mutex // serializes command writes on this conn

	resp chan *Frame // capacity 1: the in-flight command's reply

	done     chan struct{}
	deadOnce sync.Once
}

func newESLClient(cfg clientConfig) *eslClient {
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.DiscardHandler)
	}
	if cfg.dialTimeout <= 0 {
		cfg.dialTimeout = 5 * time.Second
	}
	if cfg.hsTimeout <= 0 {
		cfg.hsTimeout = 10 * time.Second
	}
	if cfg.cmdTimeout <= 0 {
		cfg.cmdTimeout = 10 * time.Second
	}
	if cfg.idleTimeout <= 0 {
		cfg.idleTimeout = 60 * time.Second
	}
	if cfg.backoffBase <= 0 {
		cfg.backoffBase = 50 * time.Millisecond
	}
	if cfg.backoffMax < cfg.backoffBase {
		cfg.backoffMax = 2 * time.Second
	}
	if cfg.maxFrame <= 0 {
		cfg.maxFrame = maxFrameBytes
	}
	c := &eslClient{
		cfg:    cfg,
		cmdCh:  make(chan struct{}, 1),
		notify: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	c.wg.Add(1)
	go c.loop()
	return c
}

// Close tears the session down and stops the reconnect loop. It blocks until
// the client goroutine exits; it is idempotent and safe to call while
// commands are in flight (they fail with errClientClosed / errConnLost).
func (c *eslClient) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		st := c.conn
		c.mu.Unlock()
		if st != nil {
			st.kill()
		}
		close(c.doneCh)
		c.wg.Wait()
	})
}

// Ready reports whether a connection is authenticated and subscribed.
func (c *eslClient) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readyLocked()
}

// readyLocked reports session readiness: a connection exists, completed its
// handshake and subscription, and has not died since.
func (c *eslClient) readyLocked() bool {
	return c.ready && c.conn != nil && c.conn.isLive()
}

// isLive reports whether the session has completed its handshake (marked by
// resp being non-nil is not enough; readiness is tracked on the client, so
// the conn only needs to report it is not yet dead).
func (st *clientConn) isLive() bool {
	select {
	case <-st.done:
		return false
	default:
		return true
	}
}

// loop is the client's only long-running goroutine: dial, run the session to
// death, reconnect with bounded backoff. The ladder resets only after a
// session actually became ready — a switch that accepts TCP but fails the
// handshake (bad credentials, broken build-out) backs off like a dead peer
// instead of hammering it at the base rate.
func (c *eslClient) loop() {
	defer c.wg.Done()
	base, max := c.cfg.backoffBase, c.cfg.backoffMax
	delay := base
	for {
		select {
		case <-c.doneCh:
			return
		default:
		}
		nc, err := c.dial()
		if err != nil {
			c.fail(fmt.Errorf("%w: dial %s: %v", errConnLost, c.cfg.addr, err))
			if !c.sleepBackoff(delay) {
				return
			}
			delay = min(delay*2, max)
			continue
		}
		if c.runConn(nc) {
			delay = base // an established session resets the ladder
		}
		select {
		case <-c.doneCh:
			return
		default:
		}
		if !c.sleepBackoff(delay) {
			return
		}
	}
}

func (c *eslClient) dial() (net.Conn, error) {
	d := net.Dialer{Timeout: c.cfg.dialTimeout, KeepAlive: 10 * time.Second}
	return d.Dial("tcp", c.cfg.addr)
}

func (c *eslClient) sleepBackoff(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.doneCh:
		return false
	case <-t.C:
		return true
	}
}

// runConn drives one TCP connection through handshake -> subscribe -> event
// demux, then dies. All auth happens here, serially per connection. It
// reports whether the session reached the ready state (so the reconnect
// ladder can reset only for genuinely established sessions).
func (c *eslClient) runConn(nc net.Conn) (established bool) {
	st := &clientConn{nc: nc, resp: make(chan *Frame, 1), done: make(chan struct{})}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		nc.Close()
		return false
	}
	c.gen++
	st.gen = c.gen
	c.conn = st
	c.mu.Unlock()

	defer func() {
		c.detach(st)
	}()

	// One frame reader owns the connection's whole receive side: handshake,
	// subscription ack and event stream all share its buffer, so bytes that
	// arrive pipelined inside one TCP segment are never dropped between
	// phases.
	fr := newFrameReader(nc, c.cfg.maxFrame)

	if err := c.handshake(st, fr); err != nil {
		c.fail(err)
		return false
	}
	if len(c.cfg.events) > 0 {
		if err := c.subscribe(st, fr); err != nil {
			c.fail(err)
			return false
		}
	}
	c.cfg.logger.Info("freeswitch: event socket session established",
		"addr", c.cfg.addr, "gen", st.gen, "events", strings.Join(c.cfg.events, ","))
	c.setReady(st)
	c.demux(st, fr)
	return true
}

// handshake performs the ESL auth exchange. Stock mod_event_socket opens
// with `auth/request` and expects a plaintext `auth <password>` line; when
// the endpoint advertises a Challenge header the client authenticates with
// MD5 of password+challenge instead (hardening for TLS-less deployments).
func (c *eslClient) handshake(st *clientConn, fr *frameReader) error {
	st.nc.SetDeadline(time.Now().Add(c.cfg.hsTimeout))
	defer st.nc.SetDeadline(time.Time{})

	first, err := fr.ReadFrame()
	if err != nil {
		return fmt.Errorf("reading auth request: %w", err)
	}
	if first.ContentType != ctAuthRequest {
		return fmt.Errorf("expected %s, got %q", ctAuthRequest, first.ContentType)
	}

	var line string
	if challenge := first.Header(hdrChallenge); challenge != "" {
		sum := md5.Sum([]byte(c.cfg.password + challenge))
		line = "auth md5:" + hex.EncodeToString(sum[:])
	} else {
		line = "auth " + c.cfg.password
	}
	if _, err := st.nc.Write(command(line)); err != nil {
		return fmt.Errorf("sending auth: %w", err)
	}

	reply, err := fr.ReadFrame()
	if err != nil {
		return fmt.Errorf("reading auth reply: %w", err)
	}
	if reply.ContentType != ctCommandReply || !strings.HasPrefix(reply.ReplyText(), "+OK") {
		return fmt.Errorf("%w: %s", errAuthRejected, reply.ReplyText())
	}
	return nil
}

// subscribe re-establishes the `events plain` subscription. It runs on every
// fresh connection, so a reconnect resumes the event stream without caller
// involvement.
//
// The reply is read inline on the connection goroutine: at this point the
// protocol is still strictly request/response (the switch acknowledges the
// subscription before it starts streaming events), and the event demux loop
// has not started yet — waiting on st.resp here would deadlock against a
// reader that does not exist.
func (c *eslClient) subscribe(st *clientConn, fr *frameReader) error {
	line := "events plain " + strings.Join(c.cfg.events, " ")
	st.nc.SetReadDeadline(time.Now().Add(c.cfg.cmdTimeout))
	defer st.nc.SetReadDeadline(time.Time{})
	if _, err := c.writeLine(st, command(line)); err != nil {
		return fmt.Errorf("sending events subscription: %w", err)
	}
	reply, err := fr.ReadFrame()
	if err != nil {
		return fmt.Errorf("%w during events subscription: %v", errConnLost, err)
	}
	if reply.ContentType != ctCommandReply || !strings.HasPrefix(reply.Result(), "+OK") {
		return fmt.Errorf("events subscription rejected: %s", reply.Result())
	}
	return nil
}

func (c *eslClient) writeLine(st *clientConn, line []byte) (int, error) {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	st.nc.SetWriteDeadline(time.Now().Add(c.cfg.cmdTimeout))
	return st.nc.Write(line)
}

// demux reads frames forever and routes them: command replies to the
// in-flight command, events to the adapter, notices to session death.
func (c *eslClient) demux(st *clientConn, fr *frameReader) {
	for {
		st.nc.SetReadDeadline(time.Now().Add(c.cfg.idleTimeout))
		f, err := fr.ReadFrame()
		if err != nil {
			c.fail(fmt.Errorf("%w: read: %v", errConnLost, err))
			return
		}
		switch f.ContentType {
		case ctCommandReply, ctAPIResponse:
			select {
			case st.resp <- f:
			default:
				// Unsolicited or stale reply (e.g. the command that owned it
				// timed out and the session was rebuilt): drop it.
				c.cfg.logger.Warn("freeswitch: dropping unsolicited command reply",
					"content_type", f.ContentType, "gen", st.gen)
			}
		case ctEventPlain:
			c.dispatchEvent(f)
		case ctDisconnectNotic, ctRudeRejection:
			c.cfg.logger.Info("freeswitch: server closed the event socket",
				"content_type", f.ContentType, "gen", st.gen)
			c.fail(fmt.Errorf("%w: %s", errConnLost, f.ContentType))
			return
		case ctAuthRequest:
			// A mid-session auth demand means the server-side session is
			// gone; treat the connection as broken and rebuild.
			c.fail(fmt.Errorf("%w: unexpected %s mid-session", errConnLost, ctAuthRequest))
			return
		default:
			c.cfg.logger.Warn("freeswitch: ignoring frame",
				"content_type", f.ContentType, "gen", st.gen)
		}
	}
}

// dispatchEvent hands one event to the adapter. The handler runs inline (the
// adapter's delivery hook is expected to be fast — it is the platform's
// in-process gateway path) but is panic-isolated so a bad delivery cannot
// kill the event stream.
func (c *eslClient) dispatchEvent(f *Frame) {
	defer func() {
		if r := recover(); r != nil {
			c.cfg.logger.Error("freeswitch: event handler panicked", "panic", r)
		}
	}()
	fields, payload, err := ParseEvent(f.Body)
	if err != nil {
		c.cfg.logger.Warn("freeswitch: undecodable event body", "err", err)
		return
	}
	name := EventField(fields, "Event-Name")
	if c.cfg.onEvent != nil {
		c.cfg.onEvent(name, fields, payload)
	}
}

// Do sends one ESL command line ("api ...", "bgapi ...") and awaits its
// reply frame. Commands are single-flight: a second Do blocks until the
// first finishes. When ctx carries no deadline the default command timeout
// bounds the call.
//
// A command whose deadline expires forces a connection rebuild rather than
// returning the connection to the pool: api replies carry no correlation id,
// so a late reply could otherwise be misattributed to the next command.
func (c *eslClient) Do(ctx context.Context, cmd string) (*Frame, error) {
	if cmd == "" {
		return nil, errors.New("freeswitch: empty command")
	}
	select {
	case c.cmdCh <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.doneCh:
		return nil, errClientClosed
	}
	defer func() { <-c.cmdCh }()

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.cmdTimeout)
		defer cancel()
	}

	st, err := c.ensureReady(ctx)
	if err != nil {
		return nil, err
	}

	// Defensive: nothing should be left in the reply slot (single-flight
	// plus rebuild-on-timeout keep it drained).
	select {
	case <-st.resp:
	default:
	}

	if _, err := c.writeLine(st, command(cmd)); err != nil {
		c.detach(st)
		c.fail(fmt.Errorf("%w: write: %v", errConnLost, err))
		return nil, fmt.Errorf("%w: write: %v", errConnLost, err)
	}

	select {
	case fr := <-st.resp:
		return fr, nil
	case <-st.done:
		return nil, fmt.Errorf("%w while awaiting reply to %q", errConnLost, firstWord(cmd))
	case <-ctx.Done():
		c.detach(st)
		return nil, fmt.Errorf("freeswitch: command %q timed out: %w", firstWord(cmd), ctx.Err())
	}
}

// ensureReady blocks until a connection is authenticated and subscribed, the
// context expires, or the client closes. The last provider error (e.g. an
// auth rejection) is wrapped alongside the context error so callers can
// classify the failure.
func (c *eslClient) ensureReady(ctx context.Context) (*clientConn, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, errClientClosed
		}
		if c.readyLocked() {
			st := c.conn
			c.mu.Unlock()
			return st, nil
		}
		notify, lerr := c.notify, c.lastErr
		c.mu.Unlock()

		select {
		case <-notify:
		case <-ctx.Done():
			if lerr != nil {
				return nil, fmt.Errorf("%w (last provider error: %w)", ctx.Err(), lerr)
			}
			return nil, ctx.Err()
		case <-c.doneCh:
			return nil, errClientClosed
		}
	}
}

// detach retires a connection: closes it, wakes command waiters, and clears
// readiness only if it is still the current connection.
func (c *eslClient) detach(st *clientConn) {
	st.kill()
	c.mu.Lock()
	if c.conn == st {
		c.conn = nil
		c.ready = false
	}
	c.mu.Unlock()
	c.wake()
}

func (st *clientConn) kill() {
	st.deadOnce.Do(func() {
		close(st.done)
		st.nc.Close()
	})
}

func (c *eslClient) setReady(st *clientConn) {
	c.mu.Lock()
	if c.conn == st && !c.closed {
		c.ready = true
	}
	c.mu.Unlock()
	c.wake()
}

func (c *eslClient) fail(err error) {
	c.mu.Lock()
	c.lastErr = err
	c.mu.Unlock()
	c.wake()
}

func (c *eslClient) wake() {
	c.mu.Lock()
	close(c.notify)
	c.notify = make(chan struct{})
	c.mu.Unlock()
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}
