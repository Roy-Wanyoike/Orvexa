package asterisk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Error codes surfaced by the session layer.
const (
	codeUnavailable   = "asterisk.unavailable"
	codeAuthFailed    = "asterisk.auth_failed"
	codeActionTimeout = "asterisk.action_timeout"
	codeActionFailed  = "asterisk.action_failed"
	codeDisconnected  = "asterisk.disconnected"
	codeWriteFailed   = "asterisk.write_failed"
	codeActionIDBusy  = "asterisk.action_id_busy"
	codeHeaderInj     = "asterisk.header_injection"
)

// errSessionDead is the internal marker returned by doAction when the
// session dropped mid-action; call() retries on the next session within the
// same action budget. It never escapes the package.
var errSessionDead = errors.New("asterisk: session dropped mid-action")

// amiSession is one live AMI connection plus its correlation state. A
// session is created by dial, becomes ready after the login handshake, and
// dies on transport or protocol failure.
type amiSession struct {
	conn  net.Conn
	dead  chan struct{} // closed exactly once, when the session dies
	ready chan struct{} // closed after a successful login handshake

	pendingMu sync.Mutex
	pending   map[string]chan amiPacket

	closeOnce sync.Once
}

func newAMISession(conn net.Conn) *amiSession {
	return &amiSession{
		conn:    conn,
		dead:    make(chan struct{}),
		ready:   make(chan struct{}),
		pending: make(map[string]chan amiPacket),
	}
}

// die tears the session down exactly once: pending waiters are released via
// the dead channel and the socket is closed to unblock the reader.
func (s *amiSession) die() {
	s.closeOnce.Do(func() {
		close(s.dead)
		s.pendingMu.Lock()
		s.pending = make(map[string]chan amiPacket) // waiter defs still clean up harmlessly
		s.pendingMu.Unlock()
		s.conn.Close()
	})
}

func (s *amiSession) isDead() bool {
	select {
	case <-s.dead:
		return true
	default:
		return false
	}
}

// addPending registers a reply waiter for an ActionID; returns false when
// the ActionID is already in flight (duplicate concurrent use of a caller
// supplied id — rejected instead of corrupting the first waiter).
func (s *amiSession) addPending(id string, ch chan amiPacket) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if _, dup := s.pending[id]; dup {
		return false
	}
	s.pending[id] = ch
	return true
}

func (s *amiSession) removePending(id string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	delete(s.pending, id)
}

// takePending claims the waiter for id (nil when none — unsolicited reply).
func (s *amiSession) takePending(id string) chan amiPacket {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	ch, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	return ch
}

// amiClient owns the connection lifecycle: one supervisor goroutine that
// dials, handshakes, serves, and reconnects with bounded backoff; a
// mutex-guarded writer shared by every goroutine that touches the wire; and
// per-action reply correlation keyed by ActionID.
type amiClient struct {
	cfg     Config
	log     *slog.Logger
	onReply func(amiPacket) // correlation hook, run by the reader before fan-out
	onEvent func(amiPacket) // event sink, run by the reader in wire order

	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once

	sessMu  sync.Mutex
	sess    *amiSession
	changed chan struct{} // closed (and replaced) on every session change

	writeMu sync.Mutex // guards all socket writes: replies, events, actions

	seq atomic.Uint64 // internal ActionID minting

	lastErrMu sync.Mutex
	lastErr   error // most recent fatal session error (auth failures surfaced)
}

func newClient(cfg Config, onReply, onEvent func(amiPacket)) *amiClient {
	cfg = cfg.withDefaults()
	return &amiClient{
		cfg:     cfg,
		log:     cfg.Logger,
		onReply: onReply,
		onEvent: onEvent,
		done:    make(chan struct{}),
		changed: make(chan struct{}),
	}
}

// start launches the supervisor goroutine. It does not block: the first
// actions wait for readiness within their own action budget.
func (c *amiClient) start(parent context.Context) {
	c.ctx, c.cancel = context.WithCancel(parent)
	go c.supervise()
}

// supervise reconnects until Close (or the parent context) ends the loop.
// The schedule is bounded exponential backoff: a PBX restart is a hiccup,
// an outage must not grow the timer without limit.
func (c *amiClient) supervise() {
	defer close(c.done)
	addr := c.cfg.addr()
	attempt := 0
	for {
		if c.ctx.Err() != nil {
			return
		}
		err := c.dialAndServe()
		if c.ctx.Err() != nil {
			return
		}
		c.recordLastErr(err)
		attempt++
		wait := boundedBackoff(c.cfg.BackoffBase, c.cfg.BackoffMax, attempt)
		c.log.Warn("asterisk: AMI session lost; reconnecting",
			"addr", addr, "attempt", attempt, "wait", wait, "err", err)
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// boundedBackoff returns the wait before reconnect attempt n (1-based):
// base, 2×base, 4×base, ... capped at max. Deterministic (no jitter) so
// worst-case reconnect latency is reason-able in operations runbooks.
func boundedBackoff(base, max time.Duration, n int) time.Duration {
	if n < 1 {
		n = 1
	}
	wait := base
	for i := 1; i < n && wait < max; i++ {
		wait *= 2
	}
	if wait > max {
		wait = max
	}
	return wait
}

// dialAndServe runs one full session lifetime: dial, handshake, serve. It
// returns when the session dies (or the context ends); the supervisor
// decides what happens next.
func (c *amiClient) dialAndServe() error {
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	d := net.Dialer{Timeout: c.cfg.DialTimeout}
	conn, err := d.DialContext(c.ctx, "tcp", c.cfg.addr())
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.cfg.addr(), err)
	}
	s := newAMISession(conn)
	c.install(s)
	defer c.uninstall(s)
	defer s.die()

	// One reader for the whole session lifetime: the handshake consumes
	// the banner and the login round-trips through it, then serve keeps
	// reading from the same stream. A fresh reader in serve could discard
	// bytes the bufio window had already absorbed — real Asterisk emits
	// FullyBooted immediately after Login, and an event lost there would
	// silently shift the wire timeline.
	reader := newAMIReader(s.conn)
	if err := c.handshake(s, reader); err != nil {
		return err
	}
	close(s.ready) // actions may proceed
	c.clearLastErr()
	c.log.Info("asterisk: AMI session ready", "addr", c.cfg.addr())

	if c.cfg.PingInterval > 0 {
		go c.pingLoop(s)
	}
	return c.serve(s, reader)
}

// install makes s the current session and wakes every waiter for a session
// change (callers blocked on call() re-capture; each channel closes once).
func (c *amiClient) install(s *amiSession) {
	c.sessMu.Lock()
	old := c.changed
	c.sess = s
	c.changed = make(chan struct{})
	close(old)
	c.sessMu.Unlock()
}

// uninstall clears the current session if it is still s.
func (c *amiClient) uninstall(s *amiSession) {
	c.sessMu.Lock()
	old := c.changed
	if c.sess == s {
		c.sess = nil
		c.changed = make(chan struct{})
		close(old)
	}
	c.sessMu.Unlock()
}

// handshake performs the connect banner read and the Challenge→MD5 login.
// The raw secret never crosses the wire: only its MD5 digest over the
// server-provided challenge does. Failures carry the server's own Message
// (sanitized) — never credential material.
func (c *amiClient) handshake(s *amiSession, reader *amiReader) error {
	// Banner: "Asterisk Call Manager/<version>" — a colonless single-line
	// packet. Real Asterisk always sends it; a missing banner is treated as
	// protocol drift, not fatal (some gateways omit it).
	banner, err := reader.next()
	if err == nil && banner.isResponse() {
		return apperrors.Internal(codeActionFailed,
			"asterisk: unexpected response instead of the AMI banner")
	} else if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read AMI banner: %w", err)
	}

	challenge, err := c.exchange(s, reader, amiRequest{
		Action:  "Challenge",
		Headers: []amiHeader{{Key: "AuthType", Value: "MD5"}},
	})
	if err != nil {
		return err
	}
	if challenge.response() != "Success" {
		return apperrors.Unauth(codeAuthFailed,
			"asterisk: AMI rejected the Challenge request: "+cleanWireValue(firstNonEmpty(challenge.get("Message"), challenge.response()), 0))
	}
	seed := challenge.get("Challenge")

	login, err := c.exchange(s, reader, amiRequest{
		Action: "Login",
		Headers: []amiHeader{
			{Key: "Username", Value: c.cfg.Username},
			{Key: "Key", Value: amiMD5Key(c.cfg.Secret, seed)},
		},
	})
	if err != nil {
		return err
	}
	if login.response() != "Success" {
		return apperrors.Unauth(codeAuthFailed,
			"asterisk: AMI rejected the credentials: "+cleanWireValue(firstNonEmpty(login.get("Message"), login.response()), 0))
	}
	return nil
}

// exchange performs one write + direct read round-trip during the handshake
// (single-threaded by construction: the serve loop has not started yet).
func (c *amiClient) exchange(s *amiSession, reader *amiReader, req amiRequest) (amiPacket, error) {
	id := "ovx-hs-" + fmt.Sprint(c.seq.Add(1))
	req = req.withActionID(id)
	buf, err := req.encode()
	if err != nil {
		return amiPacket{}, apperrors.Invalid(codeHeaderInj, "asterisk: handshake request invalid: "+err.Error())
	}
	if err := c.write(s, buf); err != nil {
		return amiPacket{}, apperrors.Internal(codeWriteFailed, "asterisk: handshake request could not be written").WithCause(err)
	}
	pkt, err := reader.next()
	if err != nil {
		return amiPacket{}, fmt.Errorf("read AMI handshake reply: %w", err)
	}
	return pkt, nil
}

// serve is the session's reader loop: it fans responses to per-ActionID
// waiters (after the correlation hook, so leg binding is ordered before any
// caller observes the reply) and events to the adapter in wire order.
func (c *amiClient) serve(s *amiSession, reader *amiReader) error {
	for {
		pkt, err := reader.next()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			c.log.Warn("asterisk: AMI read failed; session dies", "err", err)
			return err
		}
		switch {
		case pkt.isEvent():
			if c.onEvent != nil {
				c.onEvent(pkt)
			}
		case pkt.isResponse():
			if c.onReply != nil {
				c.onReply(pkt)
			}
			if id := pkt.actionID(); id != "" {
				if ch := s.takePending(id); ch != nil {
					ch <- pkt // buffered(1): the reader never blocks on fan-out
				}
			}
		default:
			c.log.Debug("asterisk: ignoring non-traffic packet", "headers", len(pkt.headers))
		}
	}
}

// write sends bytes on the session socket. Every wire write funnels through
// the client-wide mutex — actions, keepalive pings and handshake traffic can
// originate on different goroutines, and interleaved writes would corrupt
// the line-oriented framing.
func (c *amiClient) write(s *amiSession, b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if s.isDead() {
		return errSessionDead
	}
	if _, err := s.conn.Write(b); err != nil {
		if s.isDead() {
			return errSessionDead
		}
		return err
	}
	return nil
}

// call performs one AMI action and waits for its reply, bounded by the
// action budget. If no session is live it waits for one (bounded); if the
// session drops mid-action it retries on the next one within the same
// budget. The returned error is an *apperrors.Error on every failure path.
func (c *amiClient) call(ctx context.Context, actionID string, req amiRequest) (amiPacket, error) {
	if actionID == "" {
		actionID = "ovx-" + fmt.Sprint(c.seq.Add(1))
	}
	req = req.withActionID(actionID)
	buf, err := req.encode()
	if err != nil {
		return amiPacket{}, apperrors.Invalid(codeHeaderInj, "asterisk: request invalid: "+err.Error())
	}
	deadline := time.Now().Add(c.cfg.ActionTimeout)

	for {
		if err := ctx.Err(); err != nil {
			return amiPacket{}, err
		}
		c.sessMu.Lock()
		s := c.sess
		changed := c.changed
		c.sessMu.Unlock()

		if s == nil || s.isDead() {
			if !c.waitSessionChange(ctx, changed, deadline) {
				return amiPacket{}, c.unavailable()
			}
			continue
		}
		select {
		case <-s.ready:
		case <-s.dead:
			continue
		case <-time.After(time.Until(deadline)):
			return amiPacket{}, c.unavailable()
		case <-ctx.Done():
			return amiPacket{}, ctx.Err()
		}

		pkt, err := c.doAction(ctx, s, actionID, buf, deadline)
		if errors.Is(err, errSessionDead) && time.Now().Before(deadline) && ctx.Err() == nil {
			continue // supervisor is reconnecting; stay within the budget
		}
		if errors.Is(err, errSessionDead) {
			return amiPacket{}, apperrors.Internal(codeDisconnected, "asterisk: AMI session dropped before the action was answered")
		}
		return pkt, err
	}
}

// callOnce is call() without retry semantics: one attempt on the current
// session. Keepalive pings use it — a failed ping must kill the session, not
// silently hop to the next one.
func (c *amiClient) callOnce(ctx context.Context, req amiRequest) (amiPacket, error) {
	actionID := "ovx-ping-" + fmt.Sprint(c.seq.Add(1))
	req = req.withActionID(actionID)
	buf, err := req.encode()
	if err != nil {
		return amiPacket{}, apperrors.Invalid(codeHeaderInj, "asterisk: request invalid: "+err.Error())
	}
	deadline := time.Now().Add(c.cfg.ActionTimeout)

	c.sessMu.Lock()
	s := c.sess
	c.sessMu.Unlock()
	if s == nil {
		return amiPacket{}, c.unavailable()
	}
	select {
	case <-s.ready:
	case <-s.dead:
		return amiPacket{}, apperrors.Internal(codeDisconnected, "asterisk: AMI session dropped before the action was answered")
	case <-time.After(time.Until(deadline)):
		return amiPacket{}, c.unavailable()
	case <-ctx.Done():
		return amiPacket{}, ctx.Err()
	}
	pkt, err := c.doAction(ctx, s, actionID, buf, deadline)
	if errors.Is(err, errSessionDead) {
		return amiPacket{}, apperrors.Internal(codeDisconnected, "asterisk: AMI session dropped before the action was answered")
	}
	return pkt, err
}

// doAction registers the reply waiter, writes the request, and blocks for
// the reply, session death, the action deadline, or caller cancellation.
func (c *amiClient) doAction(ctx context.Context, s *amiSession, id string, buf []byte, deadline time.Time) (amiPacket, error) {
	ch := make(chan amiPacket, 1)
	if !s.addPending(id, ch) {
		return amiPacket{}, apperrors.Conflict(codeActionIDBusy,
			"asterisk: an action with this ActionID is already in flight")
	}
	defer s.removePending(id)

	if err := c.write(s, buf); err != nil {
		if errors.Is(err, errSessionDead) {
			return amiPacket{}, errSessionDead
		}
		if s.isDead() {
			return amiPacket{}, errSessionDead
		}
		return amiPacket{}, apperrors.Internal(codeWriteFailed, "asterisk: AMI request could not be written").WithCause(err)
	}

	select {
	case pkt := <-ch:
		return pkt, nil
	case <-s.dead:
		return amiPacket{}, errSessionDead
	case <-time.After(time.Until(deadline)):
		return amiPacket{}, apperrors.Internal(codeActionTimeout,
			"asterisk: AMI action timed out waiting for a reply")
	case <-ctx.Done():
		return amiPacket{}, ctx.Err()
	}
}

// waitSessionChange blocks until the session set changes, the caller context
// ends, or the deadline passes. Reports whether a change was observed.
func (c *amiClient) waitSessionChange(ctx context.Context, changed chan struct{}, deadline time.Time) bool {
	select {
	case <-changed:
		return true
	case <-ctx.Done():
		return false
	case <-time.After(time.Until(deadline)):
		return false
	}
}

// unavailable renders the "no usable session" error, surfacing an auth
// rejection distinctly: a wrong secret is a configuration error (401
// semantics), not a transient transport fault.
func (c *amiClient) unavailable() error {
	c.lastErrMu.Lock()
	last := c.lastErr
	c.lastErrMu.Unlock()
	var ae *apperrors.Error
	if errors.As(last, &ae) && ae.Code == codeAuthFailed {
		return apperrors.Unauth(codeAuthFailed, "asterisk: AMI rejected the configured credentials")
	}
	return apperrors.Internal(codeUnavailable, "asterisk: no AMI session is available").WithCause(last)
}

// actionError renders an AMI Failure/Error response. The server's Message is
// sanitized and included — it is server text, never credential material.
func actionError(action, response, message string) error {
	detail := cleanWireValue(firstNonEmpty(message, response), 0)
	return apperrors.Internal(codeActionFailed,
		fmt.Sprintf("asterisk: AMI %s action failed: %s", action, detail))
}

// pingLoop is the per-session keepalive: a missed ping (timeout, write
// failure, dead session) closes the session so the supervisor reconnects
// immediately instead of leaving callers to time out against a half-open
// TCP peer.
func (c *amiClient) pingLoop(s *amiSession) {
	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.dead:
			return
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if _, err := c.callOnce(c.ctx, amiRequest{Action: "Ping"}); err != nil {
				if s.isDead() {
					return
				}
				c.log.Warn("asterisk: AMI keepalive failed; forcing reconnect", "err", err)
				s.die()
				return
			}
		}
	}
}

// close performs the graceful shutdown: a best-effort Logoff, then context
// cancellation and socket teardown. Idempotent; safe to call from anywhere.
func (c *amiClient) close() {
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_, _ = c.callOnce(ctx, amiRequest{Action: "Logoff"})
		cancel()
		if c.cancel != nil {
			c.cancel()
		}
		c.sessMu.Lock()
		s := c.sess
		c.sessMu.Unlock()
		if s != nil {
			s.die()
		}
		select {
		case <-c.done:
		case <-time.After(time.Second):
		}
	})
}

func (c *amiClient) recordLastErr(err error) {
	if err == nil {
		return
	}
	c.lastErrMu.Lock()
	c.lastErr = err
	c.lastErrMu.Unlock()
}

// clearLastErr drops the recorded fatal-error history the moment a session
// becomes ready again: a past auth rejection must not outlive the operator
// fixing manager.conf, or the next transient timeout would be misreported
// as still-bad credentials.
func (c *amiClient) clearLastErr() {
	c.lastErrMu.Lock()
	c.lastErr = nil
	c.lastErrMu.Unlock()
}

// ---------------------------------------------------------------------------
// small shared helpers
// ---------------------------------------------------------------------------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// joinHostPort renders the dial address (IPv6-safe).
func joinHostPort(host, port string) string {
	return net.JoinHostPort(host, port)
}
