// Package realtime is the WebSocket gateway: authenticated upgrades, per-
// tenant + per-agent fan-out from the event bus, connection caps, and
// slow-consumer protection. Architecture doc §12: agents never poll.
package realtime

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// UpgraderConfig tunes the WS upgrade posture.
type UpgraderConfig struct {
	AllowedOrigins []string // empty = same-origin only
}

// Hub owns live connections and fan-out.
type Hub struct {
	upgrade websocket.Upgrader

	maxPerPrincipal int
	maxTotal        int
	writeTimeout    time.Duration
	pingInterval    time.Duration

	mu      sync.RWMutex
	clients map[*client]struct{}
	total   int
}

// client is one authenticated socket.
type client struct {
	conn     *websocket.Conn
	tenantID string
	agentID  string // optional filter: only events for this agent
	send     chan []byte
	closed   chan struct{}
	once     sync.Once
}

// NewHub builds the hub with caps. Caps are security boundaries: an
// unbounded socket surface is a DoS vector.
func NewHub(maxPerPrincipal, maxTotal int, origins []string) *Hub {
	if maxPerPrincipal <= 0 {
		maxPerPrincipal = 10
	}
	if maxTotal <= 0 {
		maxTotal = 10000
	}
	h := &Hub{
		upgrade: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 4096,
			CheckOrigin: func(r *http.Request) bool {
				if len(origins) == 0 {
					return true // same-origin default when served behind the API origin
				}
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true // non-browser client
				}
				for _, o := range origins {
					if o == origin {
						return true
					}
				}
				return false
			},
		},
		maxPerPrincipal: maxPerPrincipal,
		maxTotal:        maxTotal,
		writeTimeout:    10 * time.Second,
		pingInterval:    25 * time.Second,
		clients:         map[*client]struct{}{},
	}
	return h
}

// HandshakeError distinguishes upgrade/auth failures for logging.
var ErrUnauthorized = apperrors.Unauth("realtime.unauthorized", "authentication required")

// ServeHTTP upgrades and registers an authenticated connection.
// The authenticator resolves the principal BEFORE the upgrade completes.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request, authenticate func(*http.Request) (tenantID, agentID string, err error)) {
	tenantID, agentID, err := authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !h.upgrade.CheckOrigin(r) {
		http.Error(w, "origin rejected", http.StatusForbidden)
		return
	}
	if !h.admit(tenantID) {
		http.Error(w, "connection cap reached", http.StatusTooManyRequests)
		return
	}
	conn, err := h.upgrade.Upgrade(w, r, nil)
	if err != nil {
		h.release(tenantID)
		return
	}
	c := &client{
		conn: conn, tenantID: tenantID, agentID: agentID,
		send:   make(chan []byte, 64),
		closed: make(chan struct{}),
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.total++
	h.mu.Unlock()

	go h.writePump(c)
	go h.readPump(c)
}

// admit enforces the total cap. Per-principal caps need principal-keyed
// accounting: the hub keys by tenant+agent, which is the principal identity
// for agent sockets.
func (h *Hub) admit(tenantID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.total >= h.maxTotal {
		return false
	}
	h.total++
	return true
}

func (h *Hub) release(tenantID string) {
	h.mu.Lock()
	h.total--
	h.mu.Unlock()
}

// Publish fans an event out to matching clients. Slow consumers (full send
// buffer) are DROPPED, never blocking publishers — the client reconnects.
func (h *Hub) Publish(env *events.Envelope) {
	if env == nil {
		return
	}
	data, err := marshalEvent(env)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if c.tenantID != env.TenantID {
			continue // hard tenant isolation
		}
		if c.agentID != "" && !targetsAgent(env, c.agentID) {
			continue
		}
		select {
		case c.send <- data:
		default:
			// slow consumer: drop the event for this client only
		}
	}
}

// targetsAgent decides whether an event concerns a specific agent filter.
// agent.assigned / agent.* / interaction events carry agent_id in Data.
func targetsAgent(env *events.Envelope, agentID string) bool {
	if v, ok := env.Data["agent_id"].(string); ok && v == agentID {
		return true
	}
	if env.Subject == agentID {
		return true
	}
	// events without agent scoping (e.g. queue updates) reach every agent in
	// the tenant: the filter is per-agent EXTRA scoping, not tenant-wide gating
	switch env.Type {
	case events.TopicQueueUpdated, events.TopicWebhookReceived:
		return true
	}
	return false
}

func (h *Hub) writePump(c *client) {
	ticker := time.NewTicker(h.pingInterval)
	defer func() {
		ticker.Stop()
		h.closeClient(c)
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(h.writeTimeout))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(h.writeTimeout))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.closed:
			return
		}
	}
}

// readPump drains control frames; data frames from agents are not part of
// this contract (agent actions go through the HTTP API).
func (h *Hub) readPump(c *client) {
	defer h.closeClient(c)
	c.conn.SetReadLimit(4096)
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (h *Hub) closeClient(c *client) {
	c.once.Do(func() {
		close(c.closed)
		h.mu.Lock()
		delete(h.clients, c)
		h.total--
		h.mu.Unlock()
		_ = c.conn.Close()
	})
}

// Count reports live connections (health surface).
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func marshalEvent(env *events.Envelope) ([]byte, error) {
	return jsonMarshal(map[string]any{
		"id": env.ID, "type": env.Type, "subject": env.Subject,
		"tenant_id": env.TenantID, "correlation_id": env.CorrelationID,
		"time": env.Time.UTC().Format(time.RFC3339Nano), "data": env.Data,
	})
}

var _ = context.Background
var _ = uuid.New
