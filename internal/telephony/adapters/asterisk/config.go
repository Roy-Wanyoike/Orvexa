package asterisk

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Registry/env defaults (keep in lockstep with internal/comms/registry: the
// registry applies ORVEXA_ASTERISK_PORT default 5038 before handing the
// config over; the adapter defends the same default for hand-built configs).
const (
	DefaultPort         = "5038"
	DefaultDialContext  = "from-internal"
	DefaultMOHContext   = "orvexa-hold"
	defaultDialTimeout  = 3 * time.Second
	defaultActionTime   = 5 * time.Second
	defaultOriginateTTL = 30 * time.Second
	defaultBackoffBase  = 500 * time.Millisecond
	defaultBackoffMax   = 15 * time.Second
	defaultPingInterval = 0 // keepalive off unless wired explicitly
)

// Config is the adapter's construction contract. Credential fields are plain
// strings at this layer — they arrive unwrapped from registry.Secret values
// (see FromRegistry) and are handled as load-bearing secrets throughout:
// never logged, never embedded in errors (String() renders them redacted).
type Config struct {
	// Host and Port locate the AMI TCP endpoint. Port empty -> DefaultPort.
	Host string
	Port string

	// Username and Secret are the AMI manager credentials. Login uses the
	// Challenge→MD5 handshake, so the secret never crosses the wire in the
	// clear after the connect banner.
	Username string
	Secret   string

	// DialContext is the PBX dialplan context calls are placed into: the
	// default Originate channel is Local/<destination>@<DialContext>, and
	// Transfer redirects back into it. Provision it on the PBX (manager.conf
	// hints and the capability matrix live in the package README).
	DialContext string

	// MOHContext is the dialplan context Hold redirects the customer leg
	// into; it must contain a MusicOnHold() application call. The platform
	// never generates dialplan (documented limitation, not an oversight).
	MOHContext string

	// Ingest receives translated provider events (comms.IngestFunc) — the
	// same fail-closed webhook path every adapter reports through. Required.
	Ingest func(provider string, body []byte, signature string) error

	// Signer signs each event body (comms.Signer). Required: the webhook
	// gateway is fail-closed and drops unsigned traffic.
	Signer func(body []byte) string

	// DialTimeout bounds the TCP dial. Default 3s.
	DialTimeout time.Duration
	// ActionTimeout bounds waiting for a session and for each action reply.
	// Default 5s.
	ActionTimeout time.Duration
	// OriginateTimeout is the ring timeout advertised to Asterisk in the
	// Originate "Timeout" header (milliseconds on the wire). Default 30s.
	OriginateTimeout time.Duration
	// BackoffBase and BackoffMax bound the reconnect schedule: base,
	// 2×base, 4×base, ... capped at BackoffMax. Defaults 500ms / 15s.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// PingInterval is the keepalive period (Action: Ping); 0 disables it.
	// A failed ping forces the session closed so the supervisor reconnects
	// instead of leaving actions to time out against a half-open TCP peer.
	PingInterval time.Duration

	// Logger receives structured, credential-free operational logs.
	// Default slog.Default().
	Logger *slog.Logger
}

// withDefaults fills unset knobs. Values are never zeroed silently: zero
// means "use the documented default", not "disable".
func (c Config) withDefaults() Config {
	if c.Port == "" {
		c.Port = DefaultPort
	}
	if c.DialContext == "" {
		c.DialContext = DefaultDialContext
	}
	if c.MOHContext == "" {
		c.MOHContext = DefaultMOHContext
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.ActionTimeout <= 0 {
		c.ActionTimeout = defaultActionTime
	}
	if c.OriginateTimeout <= 0 {
		c.OriginateTimeout = defaultOriginateTTL
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = defaultBackoffBase
	}
	if c.BackoffMax < c.BackoffBase {
		c.BackoffMax = defaultBackoffMax
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// validate enforces the construction contract for the transport layer.
// Delivery wiring (Ingest/Signer) is checked by New — the client works
// without them in transport-only tests.
func (c Config) validate() error {
	switch {
	case c.Host == "":
		return apperrors.Invalid("asterisk.host_required", "asterisk: AMI host is required")
	case c.Username == "":
		return apperrors.Invalid("asterisk.username_required", "asterisk: AMI username is required")
	case c.Secret == "":
		return apperrors.Invalid("asterisk.secret_required", "asterisk: AMI secret is required")
	}
	return nil
}

// addr renders the dial address (IPv6-safe via net.JoinHostPort).
func (c Config) addr() string {
	return joinHostPort(c.Host, c.Port)
}

// String renders the configuration with zero credential material: username
// and secret go through the shared registry redaction (****-masking, final
// four bytes only for values >= 12 bytes).
func (c Config) String() string {
	return fmt.Sprintf("asterisk(addr=%s dial_context=%s moh_context=%s username=%s secret=%s)",
		c.addr(), c.DialContext, c.MOHContext, registry.Redacted(c.Username), registry.Redacted(c.Secret))
}

// FromRegistry builds the adapter Config from the validated voice-plane
// registry entry. It fails loudly when the entry is missing, names another
// provider, or has not passed registry validation — misrouted telephony
// credentials are a startup error, never a runtime surprise. The registry
// Secret fields are unwrapped here, once; downstream code treats the values
// as raw credential material and renders them redacted on every path.
func FromRegistry(tc *registry.TelephonyConfig) (Config, error) {
	if tc == nil {
		return Config{}, apperrors.Invalid("asterisk.config_required", "asterisk: telephony configuration is required")
	}
	if err := registry.ValidateForPlane(tc.Provider, registry.PlaneVoice); err != nil {
		return Config{}, err
	}
	if tc.Provider != registry.ProviderAsterisk {
		return Config{}, apperrors.Invalid("asterisk.provider_mismatch",
			fmt.Sprintf("asterisk: configuration is for provider %q, not %q", tc.Provider, registry.ProviderAsterisk))
	}
	if err := registry.ValidateTelephony(tc); err != nil {
		return Config{}, err
	}
	return Config{
		Host:     tc.AsteriskHost,
		Port:     tc.AsteriskPort,
		Username: string(tc.AsteriskUsername),
		Secret:   string(tc.AsteriskSecret),
	}, nil
}
