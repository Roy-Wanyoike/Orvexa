package bus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// The NATS unit suite runs entirely in-process: nats.go's jetstream package
// is interface-shaped, so the driver is built behind two narrow seams
// (jsPublisher, jsStream) and the tests below supply a
// minimal JetStream-shaped fake instead of a real server (no binaries are
// downloaded; the compose-backed path lives in nats_integration_test.go
// behind the `integration` build tag). The fake reproduces just enough
// server semantics to be meaningful: durable consumer registration with
// server-side name rules, subject filtering with NATS wildcard tokens,
// asynchronous fan-out dispatch, ack/nak/term capture, and publish failure
// injection.

// ---- the fake ----

// fakeJetStream implements jsPublisher and owns the stream.
type fakeJetStream struct {
	mu        sync.Mutex
	published []fakePublish
	pubErr    error
	stream    *fakeStream
	seq       uint64
}

type fakePublish struct {
	subject string
	payload []byte
}

func (f *fakeJetStream) Publish(_ context.Context, subject string, payload []byte, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	f.mu.Lock()
	if f.pubErr != nil {
		err := f.pubErr
		f.mu.Unlock()
		return nil, err
	}
	f.published = append(f.published, fakePublish{subject: subject, payload: payload})
	f.seq++
	ack := &jetstream.PubAck{Stream: natsStreamName, Sequence: f.seq}
	stream := f.stream
	f.mu.Unlock()
	// Fan-out mirrors the server: delivery to active consumers happens off
	// the publisher's critical path.
	stream.dispatch(subject, payload)
	return ack, nil
}

func (f *fakeJetStream) publishedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

func (f *fakeJetStream) lastPublish() fakePublish {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published[len(f.published)-1]
}

// fakeStream implements jsStream: registers durable pull consumers and
// dispatches published messages to active ones.
type fakeStream struct {
	mu        sync.Mutex
	consumers map[string]*fakeConsumer
	createErr error
}

func (s *fakeStream) CreateOrUpdateConsumer(_ context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return nil, s.createErr
	}
	// Enforce the server-side durable name rules the driver must respect.
	if cfg.Durable == "" {
		return nil, errors.New("fake: durable name is required for pull consumers")
	}
	if strings.ContainsAny(cfg.Durable, ".*> \t") {
		return nil, errors.New("fake: durable name must not contain whitespace, ., *, >")
	}
	if cfg.FilterSubject == "" || !strings.HasPrefix(cfg.FilterSubject, "orvexa.") {
		return nil, errors.New("fake: filter subject must live under the orvexa namespace")
	}
	c, ok := s.consumers[cfg.Durable]
	if !ok {
		c = &fakeConsumer{name: cfg.Durable}
		s.consumers[cfg.Durable] = c
	}
	c.cfg = cfg
	return c, nil
}

func (s *fakeStream) dispatch(subject string, payload []byte) {
	s.mu.Lock()
	consumers := make([]*fakeConsumer, 0, len(s.consumers))
	for _, c := range s.consumers {
		consumers = append(consumers, c)
	}
	s.mu.Unlock()
	for _, c := range consumers {
		c.maybeDeliver(subject, payload)
	}
}

// fakeConsumer implements jetstream.Consumer (jsStream's return type) for
// one durable.
type fakeConsumer struct {
	name string
	cfg  jetstream.ConsumerConfig

	mu      sync.Mutex
	handler jetstream.MessageHandler
	ctx     *fakeConsumeContext
	msgs    []*fakeMsg
}

func (c *fakeConsumer) Consume(h jetstream.MessageHandler, _ ...jetstream.PullConsumeOpt) (jetstream.ConsumeContext, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx := &fakeConsumeContext{closed: make(chan struct{})}
	c.handler = h
	c.ctx = ctx
	return ctx, nil
}

// The remaining jetstream.Consumer surface is pull-transport machinery the
// driver never touches (it only drives Consume); the fake rejects it loudly
// so accidental reliance would fail tests, not silently no-op.
func (c *fakeConsumer) Fetch(_ int, _ ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	return nil, errors.New("fake: Fetch unsupported")
}

func (c *fakeConsumer) FetchBytes(_ int, _ ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	return nil, errors.New("fake: FetchBytes unsupported")
}

func (c *fakeConsumer) FetchNoWait(_ int) (jetstream.MessageBatch, error) {
	return nil, errors.New("fake: FetchNoWait unsupported")
}

func (c *fakeConsumer) Messages(_ ...jetstream.PullMessagesOpt) (jetstream.MessagesContext, error) {
	return nil, errors.New("fake: Messages unsupported")
}

func (c *fakeConsumer) Next(_ ...jetstream.FetchOpt) (jetstream.Msg, error) {
	return nil, errors.New("fake: Next unsupported")
}

func (c *fakeConsumer) Info(context.Context) (*jetstream.ConsumerInfo, error) {
	return nil, errors.New("fake: Info unsupported")
}

func (c *fakeConsumer) CachedInfo() *jetstream.ConsumerInfo { return nil }

func (c *fakeConsumer) maybeDeliver(subject string, payload []byte) {
	c.mu.Lock()
	h := c.handler
	ctx := c.ctx
	c.mu.Unlock()
	if h == nil || ctx == nil || ctx.isStopped() {
		return
	}
	if !matchFilter(c.cfg.FilterSubject, subject) {
		return
	}
	msg := &fakeMsg{subject: subject, data: payload, numDelivered: 1,
		meta: &jetstream.MsgMetadata{Stream: natsStreamName, Consumer: c.name, NumDelivered: 1}}
	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()
	h(msg)
}

func (c *fakeConsumer) handlerSnapshot() jetstream.MessageHandler {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handler
}

func (c *fakeConsumer) consumeContext() *fakeConsumeContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ctx
}

func (c *fakeConsumer) messages() []*fakeMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*fakeMsg(nil), c.msgs...)
}

// fakeConsumeContext implements jetstream.ConsumeContext.
type fakeConsumeContext struct {
	stopped bool
	drained bool
	closed  chan struct{}
	mu      sync.Mutex
}

func (c *fakeConsumeContext) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		c.stopped = true
		close(c.closed)
	}
}

func (c *fakeConsumeContext) Drain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drained = true
	if !c.stopped {
		c.stopped = true
		close(c.closed)
	}
}

func (c *fakeConsumeContext) Closed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeConsumeContext) isStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

func (c *fakeConsumeContext) isDrained() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.drained
}

// fakeMsg implements jetstream.Msg and records ack/nak/term decisions.
type fakeMsg struct {
	subject      string
	data         []byte
	numDelivered uint64
	meta         *jetstream.MsgMetadata

	mu       sync.Mutex
	acked    bool
	termed   bool
	naks     int
	nakDelay time.Duration
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) { return m.meta, nil }
func (m *fakeMsg) Data() []byte                              { return m.data }
func (m *fakeMsg) Headers() nats.Header                      { return nil }
func (m *fakeMsg) Subject() string                           { return m.subject }
func (m *fakeMsg) Reply() string                             { return "" }
func (m *fakeMsg) Ack() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acked = true
	return nil
}
func (m *fakeMsg) DoubleAck(context.Context) error { return m.Ack() }
func (m *fakeMsg) Nak() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.naks++
	return nil
}
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.naks++
	m.nakDelay = d
	return nil
}
func (m *fakeMsg) InProgress() error { return nil }
func (m *fakeMsg) Term() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.termed = true
	return nil
}
func (m *fakeMsg) TermWithReason(string) error { return m.Term() }

func (m *fakeMsg) state() (acked, termed bool, naks int, delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked, m.termed, m.naks, m.nakDelay
}

// matchFilter applies NATS subject-token wildcard semantics (">" = one or
// more remaining tokens, "*" = exactly one token).
func matchFilter(filter, subject string) bool {
	ft := strings.Split(filter, ".")
	st := strings.Split(subject, ".")
	for i, tok := range ft {
		switch tok {
		case ">":
			return i < len(st)
		case "*":
			if i >= len(st) {
				return false
			}
		default:
			if i >= len(st) || ft[i] != st[i] {
				return false
			}
		}
	}
	return len(ft) == len(st)
}

// ---- harness ----

// newFakeNATS builds the driver over the fake. mutate adjusts failure
// injection before any test interaction.
func newFakeNATS(t *testing.T, mutate func(js *fakeJetStream, stream *fakeStream)) (*NATS, *fakeJetStream, *fakeStream) {
	t.Helper()
	stream := &fakeStream{consumers: map[string]*fakeConsumer{}}
	js := &fakeJetStream{stream: stream}
	if mutate != nil {
		mutate(js, stream)
	}
	return newNATSDriver(js, stream, nil, nil), js, stream
}

// waitUntil polls cond until it holds or the deadline passes.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(kitSettle)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within the settle window")
}

// ---- conformance parity ----

// TestNATSDriverPassesBusConformance runs the shared kit against the fake:
// the NATS driver cannot pass this while diverging from the InProc
// reference. The real-server proof is the integration-tagged kit run.
func TestNATSDriverPassesBusConformance(t *testing.T) {
	runBusConformance(t, "nats", func(t *testing.T) Bus {
		drv, _, _ := newFakeNATS(t, nil)
		return drv
	})
}

// ---- publish ----

func TestNATSPublishUsesCanonicalWireFormat(t *testing.T) {
	drv, js, _ := newFakeNATS(t, nil)
	env := kitEnvelope(t, events.TopicInteractionCreated)
	if err := drv.Publish(context.Background(), env); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if js.publishedCount() != 1 {
		t.Fatalf("expected 1 publish, got %d", js.publishedCount())
	}
	pub := js.lastPublish()
	if pub.subject != "orvexa.interaction.created" {
		t.Fatalf("subject = %q, want orvexa.interaction.created", pub.subject)
	}
	var decoded map[string]any
	if err := json.Unmarshal(pub.payload, &decoded); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	for _, key := range []string{"id", "type", "source", "subject", "tenant_id", "correlation_id", "time", "data"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("canonical key %q missing from payload: %s", key, pub.payload)
		}
	}
	var back events.Envelope
	if err := json.Unmarshal(pub.payload, &back); err != nil {
		t.Fatalf("envelope round-trip: %v", err)
	}
	if back.ID != env.ID || back.Type != env.Type || back.TenantID != env.TenantID || back.Source != env.Source {
		t.Fatalf("round-trip drift: sent %+v got %+v", env, back)
	}
}

func TestNATSPublishRejectsInvalidEnvelope(t *testing.T) {
	drv, js, _ := newFakeNATS(t, nil)
	if err := drv.Publish(context.Background(), &events.Envelope{ID: "not-a-uuid", Type: "not.a.topic", TenantID: "t"}); err == nil {
		t.Fatal("invalid envelope must be rejected")
	}
	if got := js.publishedCount(); got != 0 {
		t.Fatalf("invalid envelope must not reach the wire, got %d publishes", got)
	}
}

func TestNATSPublishReturnsErrorWhenJetStreamUnavailable(t *testing.T) {
	drv, _, _ := newFakeNATS(t, func(js *fakeJetStream, _ *fakeStream) {
		js.pubErr = errors.New("jetstream unavailable")
	})
	env := kitEnvelope(t, events.TopicInteractionCreated)
	err := drv.Publish(context.Background(), env)
	if err == nil {
		t.Fatal("publish must surface the JetStream failure, never buffer")
	}
	if !strings.Contains(err.Error(), "orvexa.interaction.created") {
		t.Fatalf("error must name the subject, got %v", err)
	}
}

// ---- subscribe ----

func TestNATSSubscribeCreatesDurablePullConsumer(t *testing.T) {
	drv, _, st := newFakeNATS(t, nil)
	if _, err := drv.Subscribe(events.TopicInteractionCreated, newKitRecorder().handler()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	c, ok := st.consumers["orvexa-interaction-created-1"]
	if !ok {
		t.Fatalf("durable consumer missing; have %v", keys(st.consumers))
	}
	cfg := c.cfg
	if cfg.FilterSubject != "orvexa.interaction.created" {
		t.Fatalf("filter = %q", cfg.FilterSubject)
	}
	if cfg.AckPolicy != jetstream.AckExplicitPolicy {
		t.Fatalf("ack policy = %v, want explicit", cfg.AckPolicy)
	}
	if cfg.DeliverPolicy != jetstream.DeliverNewPolicy {
		t.Fatalf("deliver policy = %v, want new", cfg.DeliverPolicy)
	}
	if cfg.AckWait != defaultAckWait {
		t.Fatalf("ack wait = %v", cfg.AckWait)
	}
	if cfg.MaxAckPending != defaultMaxAckPending {
		t.Fatalf("max ack pending = %d", cfg.MaxAckPending)
	}
}

func TestNATSWildcardSubscriptionFiltersAllOrvexaSubjects(t *testing.T) {
	drv, _, st := newFakeNATS(t, nil)
	if _, err := drv.Subscribe("*", newKitRecorder().handler()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	c, ok := st.consumers["orvexa-all-1"]
	if !ok {
		t.Fatalf("wildcard durable missing; have %v", keys(st.consumers))
	}
	if c.cfg.FilterSubject != natsAllSubjects {
		t.Fatalf("wildcard filter = %q, want %q", c.cfg.FilterSubject, natsAllSubjects)
	}
}

func TestNATSSubscribersOnSameTopicGetDistinctDurables(t *testing.T) {
	drv, _, st := newFakeNATS(t, nil)
	for i := 0; i < 2; i++ {
		if _, err := drv.Subscribe(events.TopicInteractionCreated, newKitRecorder().handler()); err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
	}
	for _, name := range []string{"orvexa-interaction-created-1", "orvexa-interaction-created-2"} {
		if _, ok := st.consumers[name]; !ok {
			t.Fatalf("durable %s missing (fan-out requires per-subscription durables)", name)
		}
	}
}

func TestNATSSubscribeRejectsEmptyTopicAndNilHandler(t *testing.T) {
	drv, _, _ := newFakeNATS(t, nil)
	if _, err := drv.Subscribe("", newKitRecorder().handler()); err == nil {
		t.Fatal("empty topic must be rejected")
	}
	if _, err := drv.Subscribe(events.TopicInteractionCreated, nil); err == nil {
		t.Fatal("nil handler must be rejected")
	}
}

func TestNATSSubscribeAfterCloseFails(t *testing.T) {
	drv, _, _ := newFakeNATS(t, nil)
	if err := drv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := drv.Subscribe(events.TopicInteractionCreated, newKitRecorder().handler()); err == nil {
		t.Fatal("subscribe after close must fail")
	}
}

// ---- delivery semantics ----

func TestNATSSuccessfulHandlerAcks(t *testing.T) {
	drv, _, st := newFakeNATS(t, nil)
	if _, err := drv.Subscribe(events.TopicInteractionCreated, newKitRecorder().handler()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	env := kitEnvelope(t, events.TopicInteractionCreated)
	if err := drv.Publish(context.Background(), env); err != nil {
		t.Fatalf("publish: %v", err)
	}
	c := st.consumers["orvexa-interaction-created-1"]
	waitUntil(t, func() bool {
		msgs := c.messages()
		if len(msgs) == 0 {
			return false
		}
		acked, termed, naks, _ := msgs[0].state()
		return acked && !termed && naks == 0
	})
}

func TestNATSHandlerErrorNaksWithBoundedBackoff(t *testing.T) {
	drv, _, st := newFakeNATS(t, nil)
	// Shrunk backoff so the doubling ladder is assertable without waits.
	drv.nakBase = 10 * time.Millisecond
	drv.nakMax = 40 * time.Millisecond
	if _, err := drv.Subscribe(events.TopicInteractionCreated, func(context.Context, *events.Envelope) error {
		return errors.New("boom")
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	h := st.consumers["orvexa-interaction-created-1"].handlerSnapshot()
	if h == nil {
		t.Fatal("consume handler not registered")
	}
	payload, err := json.Marshal(kitEnvelope(t, events.TopicInteractionCreated))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cases := []struct {
		delivered uint64
		wantDelay time.Duration
	}{
		{1, 10 * time.Millisecond},
		{2, 20 * time.Millisecond},
		{3, 40 * time.Millisecond},
		{4, 40 * time.Millisecond},    // capped
		{1000, 40 * time.Millisecond}, // deep retries stay capped, never overflow
	}
	for _, tc := range cases {
		msg := &fakeMsg{data: payload, numDelivered: tc.delivered, meta: &jetstream.MsgMetadata{NumDelivered: tc.delivered}}
		h(msg)
		acked, termed, naks, delay := msg.state()
		if acked || termed {
			t.Fatalf("delivery %d: failed handler must neither ack nor term", tc.delivered)
		}
		if naks != 1 {
			t.Fatalf("delivery %d: naks = %d, want 1", tc.delivered, naks)
		}
		if delay != tc.wantDelay {
			t.Fatalf("delivery %d: backoff = %v, want %v", tc.delivered, delay, tc.wantDelay)
		}
	}
}

func TestNATSHandlerRecoveryAcksOnRetry(t *testing.T) {
	drv, _, st := newFakeNATS(t, nil)
	drv.nakBase = time.Millisecond
	calls := 0
	if _, err := drv.Subscribe(events.TopicInteractionCreated, func(_ context.Context, _ *events.Envelope) error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	h := st.consumers["orvexa-interaction-created-1"].handlerSnapshot()
	payload, err := json.Marshal(kitEnvelope(t, events.TopicInteractionCreated))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	first := &fakeMsg{data: payload, numDelivered: 1, meta: &jetstream.MsgMetadata{NumDelivered: 1}}
	h(first)
	if acked, _, naks, _ := first.state(); acked || naks != 1 {
		t.Fatalf("first attempt must nak, got acked=%v naks=%d", acked, naks)
	}
	// Server redelivery: same message, delivery count bumped.
	second := &fakeMsg{data: payload, numDelivered: 2, meta: &jetstream.MsgMetadata{NumDelivered: 2}}
	h(second)
	if acked, _, naks, _ := second.state(); !acked || naks != 0 {
		t.Fatalf("recovered handler must ack, got acked=%v naks=%d", acked, naks)
	}
}

func TestNATSPoisonMessageIsTerminated(t *testing.T) {
	drv, _, st := newFakeNATS(t, nil)
	handled := 0
	if _, err := drv.Subscribe(events.TopicInteractionCreated, func(context.Context, *events.Envelope) error {
		handled++
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	h := st.consumers["orvexa-interaction-created-1"].handlerSnapshot()
	msg := &fakeMsg{subject: "orvexa.interaction.created", data: []byte("{not json")}
	h(msg)
	if _, termed, _, _ := msg.state(); !termed {
		t.Fatal("undecodable payload must be terminated, not retried forever")
	}
	if handled != 0 {
		t.Fatalf("handler must not run for undecodable payloads, ran %d times", handled)
	}
}

// ---- lifecycle ----

func TestNATSUnsubscribeStopsConsumption(t *testing.T) {
	drv, _, st := newFakeNATS(t, nil)
	unsub, err := drv.Subscribe(events.TopicInteractionCreated, newKitRecorder().handler())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	c := st.consumers["orvexa-interaction-created-1"]
	unsub()
	if !c.consumeContext().isStopped() {
		t.Fatal("unsubscribe must stop the consume loop")
	}
	unsub() // idempotent
}

func TestNATSCloseDrainsSubscriptionsAndIsIdempotent(t *testing.T) {
	drv, _, st := newFakeNATS(t, nil)
	for i := 0; i < 2; i++ {
		if _, err := drv.Subscribe(events.TopicInteractionCreated, newKitRecorder().handler()); err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
	}
	if err := drv.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	for name, c := range st.consumers {
		if !c.consumeContext().isDrained() {
			t.Fatalf("consumer %s was not drained on close", name)
		}
	}
	if err := drv.Close(); err != nil {
		t.Fatalf("second close must stay nil, got %v", err)
	}
}

// ---- construction gating ----

func TestNATSDriverNeverConstructsWithoutURL(t *testing.T) {
	t.Setenv("ORVEXA_NATS_URL", "")
	ctx := context.Background()
	drv, err := NewNATS(ctx, NATSConfig{URL: ""})
	if !errors.Is(err, ErrNATSDisabled) {
		t.Fatalf("empty URL must wrap ErrNATSDisabled, got %v", err)
	}
	if drv != nil {
		t.Fatal("no driver may be constructed without a URL")
	}
	drv, err = NewNATSFromEnv(ctx, NATSConfig{})
	if !errors.Is(err, ErrNATSDisabled) {
		t.Fatalf("unset env must wrap ErrNATSDisabled, got %v", err)
	}
	if drv != nil {
		t.Fatal("no driver may be constructed from an empty environment")
	}
}

func TestNATSConstructorFailsFastWhenServerUnavailable(t *testing.T) {
	// Closed loopback port: proves "error when JetStream unavailable"
	// without any server or network egress.
	drv, err := NewNATS(context.Background(), NATSConfig{URL: "nats://127.0.0.1:1", ConnectTimeout: 500 * time.Millisecond})
	if err == nil {
		t.Fatal("unreachable server must fail construction")
	}
	if drv != nil {
		t.Fatal("no driver may outlive a failed connection")
	}
	if !strings.Contains(err.Error(), "bus/nats: connect") {
		t.Fatalf("error must identify the connect failure, got %v", err)
	}
}

// ---- pure mapping helpers ----

func TestNATSTopicMappingHelpers(t *testing.T) {
	cases := []struct {
		topic           string
		filter, durable string
	}{
		{events.TopicInteractionCreated, "orvexa.interaction.created", "orvexa-interaction-created-1"},
		{events.TopicRoutingDecisionRecorded, "orvexa.routing.decision.recorded", "orvexa-routing-decision-recorded-2"},
		{"*", "orvexa.>", "orvexa-all-3"},
	}
	for i, tc := range cases {
		seq := i + 1
		if got := filterForTopic(tc.topic); got != tc.filter {
			t.Fatalf("case %d: filterForTopic(%q) = %q, want %q", i, tc.topic, got, tc.filter)
		}
		if got := durableFor(tc.topic, seq); got != tc.durable {
			t.Fatalf("case %d: durableFor(%q, %d) = %q, want %q", i, tc.topic, seq, got, tc.durable)
		}
		if strings.ContainsAny(durableFor(tc.topic, seq), ".*>") {
			t.Fatalf("durable %q violates server name rules", durableFor(tc.topic, seq))
		}
	}
	if got := subjectFor(events.TopicCallRequested); got != "orvexa.call.requested" {
		t.Fatalf("subjectFor = %q", got)
	}
}

func TestNATSMatchFilter(t *testing.T) {
	cases := []struct {
		filter, subject string
		want            bool
	}{
		{"orvexa.>", "orvexa.interaction.created", true},
		{"orvexa.>", "orvexa.interaction.created.deep", true},
		{"orvexa.>", "other.topic", false},
		{"orvexa.interaction.created", "orvexa.interaction.created", true},
		{"orvexa.interaction.created", "orvexa.interaction.updated", false},
		{"orvexa.*.created", "orvexa.interaction.created", true},
		{"orvexa.*.created", "orvexa.interaction.updated", false},
		{">", "anything.at.all", true},
		{"orvexa.>", "orvexa", false}, // ">" needs at least one token
	}
	for _, tc := range cases {
		if got := matchFilter(tc.filter, tc.subject); got != tc.want {
			t.Fatalf("matchFilter(%q, %q) = %v, want %v", tc.filter, tc.subject, got, tc.want)
		}
	}
}

func keys(m map[string]*fakeConsumer) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
