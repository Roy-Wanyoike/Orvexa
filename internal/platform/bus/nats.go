package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// NATS is the JetStream-backed Bus driver for multi-process deployment
// ([O-24], issue #33). It keeps the Bus contract identical to InProc:
//
//   - Delivery is at-least-once. Consumers MUST dedupe by Envelope.ID.
//   - Publish is bounded: it performs a synchronous JetStream publish and
//     returns an error when the stream is unavailable — the driver never
//     buffers unboundedly on behalf of a down backbone.
//   - Handler errors are retried by negatively acknowledging the message
//     with a bounded exponential backoff (nakBackoffBase doubling up to
//     nakBackoffMax); the message stays in the stream until acknowledged.
//
// Wire shape: every envelope is published as JSON on subject
// "orvexa.<type>" (e.g. orvexa.interaction.created) into the single stream
// "orvexa-events" covering "orvexa.>". Each Subscribe call creates a
// durable pull consumer filtered to the topic, so fan-out to multiple
// subscribers works like InProc and a re-subscribed durable resumes where
// it left off across process restarts. Unsubscribing stops delivery but
// keeps the durable (and its cursor) for later redelivery.
//
// Enabling: the driver is opt-in. NewNATSFromEnv constructs it only when
// ORVEXA_NATS_URL is set (docker-compose.dev.yml provides a JetStream
// server on nats://localhost:4222); when the variable is empty the driver
// is never constructed and the InProc default remains untouched.
const (
	// natsStreamName is the JetStream stream holding every Orvexa event.
	natsStreamName = "orvexa-events"
	// natsSubjectPrefix maps Envelope.Type onto the subject namespace.
	natsSubjectPrefix = "orvexa."
	// natsAllSubjects is the stream's subject coverage and the filter used
	// for wildcard ("*") subscriptions. Every registered topic has at least
	// two tokens, so "orvexa.>" captures all of them.
	natsAllSubjects = "orvexa.>"

	defaultNATSTimeout   = 5 * time.Second
	defaultAckWait       = 30 * time.Second
	defaultMaxAckPending = 256
	nakBackoffBase       = 100 * time.Millisecond
	nakBackoffMax        = 30 * time.Second
	streamMaxAge         = 7 * 24 * time.Hour
)

// ErrNATSDisabled reports that the NATS driver was not constructed because
// ORVEXA_NATS_URL is unset or empty. Callers can treat it as a soft signal
// to fall back to the InProc driver.
var ErrNATSDisabled = errors.New("nats bus driver disabled: ORVEXA_NATS_URL is not set")

// jsPublisher narrows the JetStream surface the driver publishes through.
// jetstream.JetStream satisfies it; the seam keeps the driver logic
// testable against an in-process fake without a live server.
type jsPublisher interface {
	Publish(ctx context.Context, subject string, payload []byte, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

// jsStream narrows the stream surface used to create per-subscription
// consumers. jetstream.Stream satisfies it.
type jsStream interface {
	CreateOrUpdateConsumer(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error)
}

// NATSConfig configures the NATS driver.
type NATSConfig struct {
	// URL is the NATS server URL, conventionally ORVEXA_NATS_URL. Empty
	// (or whitespace) makes NewNATS fail with ErrNATSDisabled.
	URL string
	// ConnectTimeout bounds the initial dial and stream setup. Zero means
	// defaultNATSTimeout.
	ConnectTimeout time.Duration
	// Logger receives structured events (disconnects, undecodable messages,
	// consumer loops ending). Nil discards everything; no envelope payload
	// or credential material is ever logged.
	Logger *slog.Logger
}

// NATS is a Bus over NATS JetStream. Construct with NewNATS or
// NewNATSFromEnv; the zero value is not usable.
type NATS struct {
	conn   *nats.Conn // nil only in fake-backed tests
	js     jsPublisher
	stream jsStream
	logger *slog.Logger

	nakBase time.Duration
	nakMax  time.Duration

	mu     sync.Mutex
	next   int
	subs   map[int]*natsSubscription
	closed bool
}

type natsSubscription struct {
	id      int
	durable string
	consume jetstream.ConsumeContext
}

// NewNATS connects to the JetStream server at cfg.URL and ensures the
// orvexa-events stream exists. Errors are returned, never buffered: an
// unreachable server (or a server without JetStream enabled) fails
// construction so callers can fall back or abort loudly.
func NewNATS(ctx context.Context, cfg NATSConfig) (*NATS, error) {
	url := strings.TrimSpace(cfg.URL)
	if url == "" {
		return nil, fmt.Errorf("bus/nats: %w", ErrNATSDisabled)
	}
	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultNATSTimeout
	}
	// Initial dial fails fast (no retry-on-failed-connect); after a
	// connection is established the client reconnects indefinitely with
	// backoff, matching a long-lived event backbone.
	nc, err := nats.Connect(url, nats.Timeout(timeout), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second))
	if err != nil {
		return nil, fmt.Errorf("bus/nats: connect %s: %w", redactURL(url), err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bus/nats: jetstream unavailable: %w", err)
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stream, err := js.CreateOrUpdateStream(sctx, jetstream.StreamConfig{
		Name:        natsStreamName,
		Description: "Orvexa canonical event envelopes ([O-24] issue #33)",
		Subjects:    []string{natsAllSubjects},
		Retention:   jetstream.LimitsPolicy, // fan-out to per-subscription durables
		Storage:     jetstream.FileStorage,
		MaxAge:      streamMaxAge,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bus/nats: ensure stream %s: %w", natsStreamName, err)
	}
	return newNATSDriver(js, stream, nc, cfg.Logger), nil
}

// NewNATSFromEnv constructs the driver from ORVEXA_NATS_URL. When the
// variable is unset or empty the driver is never constructed: the call
// returns ErrNATSDisabled and a nil driver, leaving the InProc default
// byte-identical.
func NewNATSFromEnv(ctx context.Context, cfg NATSConfig) (*NATS, error) {
	cfg.URL = strings.TrimSpace(os.Getenv("ORVEXA_NATS_URL"))
	if cfg.URL == "" {
		return nil, fmt.Errorf("bus/nats: %w", ErrNATSDisabled)
	}
	return NewNATS(ctx, cfg)
}

// newNATSDriver assembles the driver around already-established seams. It
// is the single construction path used by NewNATS and the test fake.
func newNATSDriver(js jsPublisher, stream jsStream, conn *nats.Conn, logger *slog.Logger) *NATS {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &NATS{
		conn:    conn,
		js:      js,
		stream:  stream,
		logger:  logger,
		nakBase: nakBackoffBase,
		nakMax:  nakBackoffMax,
		subs:    map[int]*natsSubscription{},
	}
}

// Publish marshals the envelope to JSON and performs a synchronous
// JetStream publish on subject "orvexa.<type>". It is bounded: it blocks
// only until the server acks (or ctx expires) and returns an error when
// JetStream is unavailable — there is no unbounded local buffering.
func (b *NATS) Publish(ctx context.Context, env *events.Envelope) error {
	data, err := marshalEnvelope(env)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	subject := subjectFor(env.Type)
	if _, err := b.js.Publish(ctx, subject, data); err != nil {
		return fmt.Errorf("bus/nats: publish %s: %w", subject, err)
	}
	return nil
}

// Subscribe creates a durable pull consumer for topic and starts invoking
// h for every matching envelope. topic "*" subscribes to every Orvexa
// event, mirroring the InProc wildcard. Each call gets its own durable
// (multiple subscribers on the same topic each receive every message,
// matching InProc fan-out); the durable name is derived from the topic and
// the per-driver subscription sequence.
func (b *NATS) Subscribe(topic string, h Handler) (func(), error) {
	if topic == "" {
		return nil, errors.New("topic must not be empty")
	}
	if h == nil {
		return nil, errors.New("handler must not be nil")
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("bus/nats: driver is closed")
	}
	b.next++
	id := b.next
	b.mu.Unlock()

	durable := durableFor(topic, id)
	ctx, cancel := context.WithTimeout(context.Background(), defaultNATSTimeout)
	defer cancel()
	consumer, err := b.stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: filterForTopic(topic),
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverNewPolicy, // new subscribers see new events; durables resume their stored cursor
		AckWait:       defaultAckWait,
		MaxAckPending: defaultMaxAckPending,
	})
	if err != nil {
		return nil, fmt.Errorf("bus/nats: durable consumer %s: %w", durable, err)
	}
	cc, err := consumer.Consume(b.deliver(h))
	if err != nil {
		return nil, fmt.Errorf("bus/nats: consume loop %s: %w", durable, err)
	}

	b.mu.Lock()
	if b.closed { // Close raced with Subscribe; release the consumer immediately
		b.mu.Unlock()
		cc.Stop()
		return func() {}, nil
	}
	b.subs[id] = &natsSubscription{id: id, durable: durable, consume: cc}
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		sub, ok := b.subs[id]
		delete(b.subs, id)
		b.mu.Unlock()
		if ok {
			sub.consume.Stop()
		}
	}
	return unsub, nil
}

// deliver wraps a Bus handler into the JetStream message callback:
// decode → invoke → ack on success; nak with bounded backoff on handler
// error; term on undecodable payloads (poison messages must not loop).
func (b *NATS) deliver(h Handler) jetstream.MessageHandler {
	return func(msg jetstream.Msg) {
		var env events.Envelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			b.logger.Warn("bus/nats: undecodable envelope terminated", "subject", msg.Subject())
			_ = msg.Term()
			return
		}
		if err := h(context.Background(), &env); err != nil {
			delay := b.nakDelay(deliveredOf(msg))
			b.logger.Warn("bus/nats: handler error, message redelivered",
				"subject", msg.Subject(), "event_id", env.ID, "delivery", deliveredOf(msg), "delay", delay.String())
			_ = msg.NakWithDelay(delay)
			return
		}
		_ = msg.Ack()
	}
}

// nakDelay maps the delivery attempt count onto a bounded exponential
// backoff: base doubling per redelivery, capped at max. Delivery 1 (the
// first nak) waits the base delay.
func (b *NATS) nakDelay(delivered uint64) time.Duration {
	d := b.nakBase
	for i := uint64(1); i < delivered && d < b.nakMax; i++ {
		d *= 2
	}
	if d > b.nakMax {
		d = b.nakMax
	}
	return d
}

// deliveredOf extracts the delivery attempt from JetStream metadata,
// defaulting to 1 when metadata is unavailable.
func deliveredOf(msg jetstream.Msg) uint64 {
	m, err := msg.Metadata()
	if err != nil || m == nil || m.NumDelivered == 0 {
		return 1
	}
	return m.NumDelivered
}

// Close drains every subscription (in-flight handlers finish, buffered
// messages are processed) and drains the underlying connection so pending
// publishes flush before shutdown. It is idempotent and safe to call
// concurrently with Subscribe.
func (b *NATS) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := make([]*natsSubscription, 0, len(b.subs))
	for _, s := range b.subs {
		subs = append(subs, s)
	}
	b.subs = map[int]*natsSubscription{}
	b.mu.Unlock()
	for _, s := range subs {
		s.consume.Drain()
	}
	if b.conn != nil {
		return b.conn.Drain()
	}
	return nil
}

// subjectFor maps an envelope type onto its wire subject.
func subjectFor(envType string) string {
	return natsSubjectPrefix + envType
}

// filterForTopic maps a Bus topic onto a consumer filter subject. "*"
// matches every Orvexa event, exactly like the InProc wildcard.
func filterForTopic(topic string) string {
	if topic == "*" {
		return natsAllSubjects
	}
	return subjectFor(topic)
}

// durableFor derives a server-safe durable consumer name (no dots, no
// wildcard tokens) from the topic and subscription sequence.
func durableFor(topic string, seq int) string {
	var sb strings.Builder
	sb.WriteString("orvexa-")
	if topic == "*" {
		sb.WriteString("all")
	} else {
		for _, r := range topic {
			switch {
			case r == '.':
				sb.WriteByte('-')
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				sb.WriteRune(r)
			default:
				sb.WriteByte('-')
			}
		}
	}
	fmt.Fprintf(&sb, "-%d", seq)
	return sb.String()
}

// marshalEnvelope validates the envelope and encodes it as the canonical
// JSON wire format. Validation matches the InProc driver exactly, so the
// same invalid envelopes are rejected by both.
func marshalEnvelope(env *events.Envelope) ([]byte, error) {
	if err := env.Validate(); err != nil {
		return nil, fmt.Errorf("bus/nats: %w", err)
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("bus/nats: marshal envelope: %w", err)
	}
	return data, nil
}

// redactURL strips userinfo from a server URL before it lands in an error.
func redactURL(url string) string {
	if i := strings.Index(url, "@"); i >= 0 {
		if s := strings.Index(url, "://"); s >= 0 && s < i {
			return url[:s+3] + "***" + url[i:]
		}
	}
	return url
}
