package freeswitch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// providerName is the label stamped on every delivery — the webhook gateway
// routes and scopes by it, and it matches the registry's canonical
// ProviderFreeSwitch name.
const providerName = "freeswitch"

// ESL command verbs surfaced through the port (kept for log symmetry).
const (
	cmdOriginate    = "originate"
	cmdUUIDKill     = "uuid_kill"
	cmdUUIDTransfer = "uuid_transfer"
	cmdUUIDHold     = "uuid_hold"
)

// Defaults for the uuid_transfer dialplan routing when the Config leaves
// them unset (see README, "Dialplan assumptions").
const (
	defaultTransferDialplan = "XML"
	defaultTransferContext  = "default"
)

// Config configures the FreeSWITCH voice adapter.
type Config struct {
	// Addr is the host:port of mod_event_socket (default port 8021).
	Addr string

	// Password is the ESL auth password. Typed as registry.Secret so the
	// credential cannot be rendered raw by any logging or error path.
	Password registry.Secret

	// Ingest is the platform's signed-webhook delivery port. The adapter
	// reports lifecycle progress exactly like a hosted carrier does: by
	// delivering comms.ProviderEvent payloads (provider=freeswitch).
	Ingest comms.IngestFunc

	// Signer computes the webhook signature over a body. Production wiring
	// MUST inject the platform's HMAC signer (the gateway is fail-closed);
	// the default SHA-256 digest exists so an unwired signer cannot produce
	// empty signatures silently.
	Signer comms.Signer

	// Logger receives structured operational logs; credentials are never
	// logged. Default: discard.
	Logger *slog.Logger

	// TransferDialplan / TransferContext route uuid_transfer (defaults
	// XML/default).
	TransferDialplan string
	TransferContext  string

	// Timeouts and reconnect tuning. Zero values apply the defaults
	// documented on clientConfig.
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	CommandTimeout   time.Duration
	IdleTimeout      time.Duration
	BackoffBase      time.Duration
	BackoffMax       time.Duration
}

// Adapter implements telephony.VoiceProvider over the FreeSWITCH Event
// Socket Layer. It is safe for concurrent use; commands are serialized
// one-in-flight at a time on the ESL session (synchronous api replies carry
// no correlation id on the wire), while events stream in continuously and
// are delivered as signed provider webhooks.
type Adapter struct {
	cfg    Config
	client *eslClient
	legs   *legRegistry

	closeOnce sync.Once
}

// New validates the configuration and starts the ESL client (which dials
// and authenticates in the background; commands wait for readiness with
// bounded timeouts).
func New(cfg Config) (*Adapter, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, apperrors.Invalid("freeswitch.addr_required", "FreeSWITCH event socket address is required")
	}
	if cfg.Ingest == nil {
		return nil, apperrors.Invalid("freeswitch.ingest_required", "webhook delivery function (Ingest) is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Signer == nil {
		cfg.Signer = func(body []byte) string {
			sum := sha256.Sum256(body)
			return "sha256:" + hex.EncodeToString(sum[:])
		}
	}
	if cfg.TransferDialplan == "" {
		cfg.TransferDialplan = defaultTransferDialplan
	}
	if cfg.TransferContext == "" {
		cfg.TransferContext = defaultTransferContext
	}
	a := &Adapter{cfg: cfg, legs: newLegRegistry()}
	a.client = newESLClient(clientConfig{
		addr:        cfg.Addr,
		password:    string(cfg.Password),
		events:      subscribedEvents,
		dialTimeout: cfg.DialTimeout,
		hsTimeout:   cfg.HandshakeTimeout,
		cmdTimeout:  cfg.CommandTimeout,
		idleTimeout: cfg.IdleTimeout,
		backoffBase: cfg.BackoffBase,
		backoffMax:  cfg.BackoffMax,
		logger:      cfg.Logger,
		onEvent:     a.handleEvent,
	})
	return a, nil
}

// Close tears down the ESL session and stops the reconnect loop. Idempotent.
func (a *Adapter) Close() {
	a.closeOnce.Do(func() { a.client.Close() })
}

// PlaceCall implements telephony.VoiceProvider: the interaction id becomes
// the FreeSWITCH origination_uuid, so every channel event for this leg
// carries the platform's own reference back. The dial is submitted as a
// background job (bgapi); its Job-UUID is correlated to the interaction so
// the BACKGROUND_JOB result can be translated. The command returns when the
// switch has accepted the dial, not when the call is answered — progress
// arrives as events.
func (a *Adapter) PlaceCall(ctx context.Context, cmd telephony.CallCommand) error {
	tenantID, _ := cmd.ProviderOptions["tenant_id"].(string)
	if tenantID == "" {
		return apperrors.Invalid("freeswitch.tenant_required",
			"tenant_id provider option is required")
	}
	if cmd.InteractionID == "" {
		return apperrors.Invalid("freeswitch.interaction_id_required", "interaction id is required")
	}
	if _, err := uuid.Parse(cmd.InteractionID); err != nil {
		return apperrors.Invalid("freeswitch.interaction_id_invalid",
			"interaction id must be a UUID: FreeSWITCH carries it as the origination_uuid")
	}
	to, err := sanitizeDialString("destination", "freeswitch.destination_required", " &(", cmd.To)
	if err != nil {
		return err
	}
	from, err := sanitizeDialString("origin", "freeswitch.origin_required", " (),", cmd.From)
	if err != nil {
		return err
	}

	a.legs.trackLeg(cmd.InteractionID, tenantID)
	reply, err := a.client.Do(ctx, "bgapi originate {origination_uuid="+cmd.InteractionID+"}"+to+" &bridge("+from+")")
	if err != nil {
		a.legs.endLeg(cmd.InteractionID)
		return a.wrapCommandErr(err, cmdOriginate)
	}
	if !strings.HasPrefix(reply.Result(), "+OK") {
		a.legs.endLeg(cmd.InteractionID)
		return apperrors.Internal("freeswitch.originate_rejected",
			"FreeSWITCH rejected the dial: "+reply.Result())
	}
	job := reply.Header(hdrJobUUID)
	if job == "" {
		job = jobUUIDFromReply(reply.ReplyText())
	}
	if job == "" {
		a.legs.endLeg(cmd.InteractionID)
		return apperrors.Internal("freeswitch.job_uuid_missing",
			"bgapi originate reply carried no Job-UUID: the dial cannot be correlated")
	}
	a.legs.trackJob(job, cmd.InteractionID)
	return nil
}

// Hangup implements telephony.VoiceProvider via uuid_kill. The leg's
// CHANNEL_HANGUP event (not this command's reply) moves the interaction to
// its ended state, carrying the switch's hangup cause.
func (a *Adapter) Hangup(ctx context.Context, providerRef string) error {
	if _, ok := a.legs.legOf(providerRef); !ok {
		return errUnknownLeg()
	}
	reply, err := a.client.Do(ctx, "api "+cmdUUIDKill+" "+providerRef)
	if err != nil {
		return a.wrapCommandErr(err, cmdUUIDKill)
	}
	if res := reply.Result(); !strings.HasPrefix(res, "+OK") {
		return a.replyErr(res, cmdUUIDKill, providerRef)
	}
	return nil
}

// Transfer implements telephony.VoiceProvider via uuid_transfer
// <uuid> <dialplan> <context> <dest>. A successful transfer re-rings the
// live leg toward the destination — the destination's ringing phase is
// emitted as call.ringing with the destination in the detail.
func (a *Adapter) Transfer(ctx context.Context, providerRef, destination string) error {
	if _, ok := a.legs.legOf(providerRef); !ok {
		return errUnknownLeg()
	}
	dest, err := sanitizeDialString("destination", "freeswitch.destination_required", "", destination)
	if err != nil {
		return err
	}
	reply, err := a.client.Do(ctx, fmt.Sprintf("api %s %s %s %s %s",
		cmdUUIDTransfer, providerRef, a.cfg.TransferDialplan, a.cfg.TransferContext, dest))
	if err != nil {
		return a.wrapCommandErr(err, cmdUUIDTransfer)
	}
	if res := reply.Result(); !strings.HasPrefix(res, "+OK") {
		return a.replyErr(res, cmdUUIDTransfer, providerRef)
	}
	tenantID, ok := a.legTenant(providerRef)
	if !ok {
		return errUnknownLeg()
	}
	a.emit(&comms.ProviderEvent{
		Event:         evCallRinging,
		InteractionID: providerRef,
		TenantID:      tenantID,
		Detail:        "transfer:" + dest,
	})
	return nil
}

// Hold implements telephony.VoiceProvider via uuid_hold on <uuid>.
// Hold/Resume are media operations: they emit no lifecycle events.
func (a *Adapter) Hold(ctx context.Context, providerRef string) error {
	return a.hold(ctx, providerRef, "on")
}

// Resume implements telephony.VoiceProvider via uuid_hold off <uuid>.
func (a *Adapter) Resume(ctx context.Context, providerRef string) error {
	return a.hold(ctx, providerRef, "off")
}

func (a *Adapter) hold(ctx context.Context, providerRef, onOff string) error {
	if _, ok := a.legs.legOf(providerRef); !ok {
		return errUnknownLeg()
	}
	// The explicit on/off forms (not the toggle form) keep Hold idempotent.
	reply, err := a.client.Do(ctx, fmt.Sprintf("api %s %s %s", cmdUUIDHold, onOff, providerRef))
	if err != nil {
		return a.wrapCommandErr(err, cmdUUIDHold)
	}
	if res := reply.Result(); !strings.HasPrefix(res, "+OK") {
		return a.replyErr(res, cmdUUIDHold, providerRef)
	}
	return nil
}

// replyErr translates a -ERR api reply into the application error model. A
// channel the switch no longer knows is not_found (the leg is already
// gone — no event will end it, so the registry entry is retired here);
// anything else is internal.
func (a *Adapter) replyErr(result, cmd, providerRef string) error {
	if strings.Contains(strings.ToLower(result), "no such channel") {
		a.legs.endLeg(providerRef)
		return apperrors.NotFound("freeswitch.leg_not_found",
			"FreeSWITCH reports no such channel for this leg")
	}
	return apperrors.Internal("freeswitch.command_rejected",
		"FreeSWITCH rejected "+cmd+": "+result)
}

// wrapCommandErr classifies transport-level command failures. An auth
// rejection is unauthorized (misconfiguration, fixable by the operator);
// everything else (connection lost, deadline) is internal and retriable at
// the caller's discretion — the client has already rebuilt its session.
func (a *Adapter) wrapCommandErr(err error, cmd string) error {
	if errors.Is(err, errAuthRejected) {
		return apperrors.Wrap(err, apperrors.KindUnauth, "freeswitch.auth_rejected",
			"FreeSWITCH event socket rejected the platform's credentials")
	}
	return apperrors.Wrap(err, apperrors.KindInternal, "freeswitch.command_failed",
		"FreeSWITCH command "+cmd+" failed")
}

// errUnknownLeg is the typed, shape-stable error for operations against a
// reference this adapter never placed: not_found, no wire traffic, no
// events. Silent success would strand a live interaction (the platform
// believes the leg was released while the carrier keeps billing it).
func errUnknownLeg() error {
	return apperrors.NotFound("freeswitch.unknown_leg",
		"no provider leg is tracked for this reference")
}

// sanitizeDialString guards the ESL command line: dial strings are embedded
// verbatim in a single-line command, so whitespace (line corruption) and
// the characters that would start extra channel variables or applications
// are rejected as invalid input.
func sanitizeDialString(field, emptyCode, forbidden string, s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", apperrors.Invalid(emptyCode, field+" is required")
	}
	if strings.ContainsAny(s, " \t\r\n"+forbidden) {
		return "", apperrors.Invalid("freeswitch.dial_string_invalid",
			field+" contains characters that are illegal in a dial string")
	}
	return s, nil
}

// jobUUIDFromReply extracts the Job-UUID from a Reply-Text of the form
// "+OK Job-UUID: <uuid>" (the header-based correlation is preferred; this
// is the fallback for switches that only populate the reply text).
func jobUUIDFromReply(replyText string) string {
	i := strings.Index(replyText, "Job-UUID:")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(replyText[i+len("Job-UUID:"):])
	if f := strings.Fields(rest); len(f) > 0 {
		return f[0]
	}
	return ""
}
