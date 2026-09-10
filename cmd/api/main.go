// Command api is the Orvexa HTTP API deployable: interaction-plane resources,
// control-plane auth, health endpoints. It boots with or without a reachable
// database (readiness reports the truth).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/agents"
	"github.com/Roy-Wanyoike/orvexa/internal/ai"
	"github.com/Roy-Wanyoike/orvexa/internal/analytics"
	"github.com/Roy-Wanyoike/orvexa/internal/cases"
	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/factory"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/conversations"
	"github.com/Roy-Wanyoike/orvexa/internal/customers"
	"github.com/Roy-Wanyoike/orvexa/internal/httpserver"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/db"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	"github.com/Roy-Wanyoike/orvexa/internal/queues"
	"github.com/Roy-Wanyoike/orvexa/internal/routing"
	"github.com/Roy-Wanyoike/orvexa/internal/search"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
	"github.com/Roy-Wanyoike/orvexa/internal/tools"
	"github.com/Roy-Wanyoike/orvexa/internal/webhooks"
	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
	"github.com/Roy-Wanyoike/orvexa/pkg/config"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/logging"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config load failed:", err)
		os.Exit(1)
	}
	log := logging.New(cfg.LogLevel, "orvexa-api", cfg.Env)
	slog := httpx.Logger(func(msg string, args ...any) { log.Info(msg, args...) })

	// communications provider selection (issue #32): parse + validate the
	// provider configuration at startup, fail-closed and independent of the
	// database state. The error names the offending env vars (never values).
	telCfg, msgCfg, err := registry.LoadFromEnv()
	if err != nil {
		log.Error("communications provider configuration invalid", "err", err)
		fmt.Fprintln(os.Stderr, "communications provider configuration invalid:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var pool *pgxpool.Pool
	if cfg.DatabaseURL != "" {
		p, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
		if err != nil {
			log.Error("database connect failed (continuing degraded)", "err", err)
		} else {
			pool = p
			defer p.Close()
		}
	} else {
		log.Warn("no ORVEXA_DATABASE_URL configured — running without durable storage")
	}

	deps := httpserver.DomainDeps{Tenancy: tenancy.NewService(pool)}
	// conversation search ([O-27]): constructible even when OpenSearch is not
	// configured — the service reports ErrNotConfigured honestly; the indexer
	// (worker) stays off and the route serves a typed 503.
	deps.Search = search.NewService(search.NewServiceFromEnv())
	if pool != nil {
		writer := outbox.NewWriter(pool)
		gateway := webhooks.NewGateway(pool, cfg.WebhookHMACSecret)
		deps.Webhooks = gateway
		deps.Customers = customers.NewService(pool)
		deps.Conversations = conversations.NewService(pool, writer, "orvexa-api")
		interactionSvc := interactions.NewService(pool, writer, "orvexa-api")
		deps.Interactions = interactionSvc
		deps.Agents = agents.NewService(pool)
		deps.Queues = queues.NewService(pool)
		deps.Cases = cases.NewService(pool, writer, "orvexa-api")

		// communications plane: provider factory ([O-23]) + signed-webhook loop.
		// A real provider replaces the simulator plane-by-plane; unset planes
		// boot the simulator and say so honestly (LogSelection below). Every
		// provider — real or simulated — delivers through the same fail-closed
		// gateway entry, so nothing bypasses signature validation.
		secret := cfg.WebhookHMACSecret
		ingest := func(ctx context.Context, provider string, body []byte, signature string) error {
			_, err := gateway.Ingest(ctx, provider, body, signature)
			return err
		}
		voice, messenger, teardownComms, err := factory.Build(ctx, factory.Config{
			Telephony: telCfg,
			Messaging: msgCfg,
			Ingest:    ingest,
			Signer:    func(body []byte) string { return webhooks.ComputeSignature(secret, body) },
		})
		if err != nil {
			log.Error("communications provider construction failed", "err", err)
			fmt.Fprintln(os.Stderr, "communications provider construction failed:", err)
			os.Exit(1)
		}
		factory.LogSelection(log, "telephony", string(telCfg.Provider))
		factory.LogSelection(log, "messaging", string(msgCfg.Provider))
		defer teardownComms() // graceful: session-based adapters close after the server drains

		deps.CommsProcessor = &comms.Processor{Interactions: interactionSvc}
		deps.Calls = telephony.NewService(interactionSvc, voice)
		deps.Messages = messaging.NewService(interactionSvc, messenger)
		deps.Routing = routing.NewService(pool, routing.NewPresenceStore(pool, 2*time.Second), writer, "orvexa-api")

		// intelligence plane: rules provider (deterministic) + tool gateway
		// over platform services - AI never touches storage or providers directly
		customerSvc := customers.NewService(pool)
		caseSvc := deps.Cases
		msgSvc := deps.Messages
		executor := &platformToolExecutor{customers: customerSvc, cases: caseSvc, messages: msgSvc}
		usageWriter := writer
		aiGateway := ai.NewGateway(ai.NewRulesProvider("orvexa-api"), cfg.AIMaxTokens, cfg.AITimeout,
			ai.WithUsageSink(func(ctx context.Context, r *ai.Result, inv *ai.Invocation) {
				if env := ai.EmitUsageEvent("orvexa-api", r, inv); env != nil {
					_ = usageWriter.PublishLater(ctx, env)
				}
			}))
		policySource := &dbToolPolicy{pool: pool}
		toolGateway := tools.NewGateway(executor, policySource,
			func(ctx context.Context, c *tools.Call, o *tools.Outcome, err error) {
				if env := tools.AuditEnvelope("orvexa-api", c, o, err); env != nil {
					_ = usageWriter.PublishLater(ctx, env)
				}
			})
		deps.AI = ai.NewRuntime(pool, aiGateway, toolGateway, writer, "orvexa-api")

		// execution plane: durable workflows + analytics read model
		deps.Workflows = workflows.NewEngine(pool, writer, &apiWorkflowServices{messages: msgSvc, pool: pool}, "orvexa-api")
		deps.Analytics = analytics.NewService(pool)
		deps.Writer = writer
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpserver.New(httpserver.Deps{
			Pool:    pool,
			Domain:  deps,
			Limiter: httpx.NewRateLimit(600, 120, 10_000), // per-key: 600/min, burst 120
			Logger:  slog,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("orvexa-api listening", "addr", cfg.HTTPAddr)

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	log.Info("orvexa-api stopped")
}

// platformToolExecutor executes allowlisted tools against platform services.
// This is the ONLY code AI can reach for side-effects — no credentials flow
// through it, and every call is tenant-scoped by construction.
type platformToolExecutor struct {
	customers *customers.Service
	cases     *cases.Service
	messages  *messaging.Service
}

func (e *platformToolExecutor) Execute(ctx context.Context, c *tools.Call) (map[string]any, error) {
	switch c.Tool {
	case tools.ToolGetCustomer:
		id, _ := c.Args["customer_id"].(string)
		cust, err := e.customers.Get(ctx, c.TenantID, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"customer": cust}, nil
	case tools.ToolCreateCase:
		custID, _ := c.Args["customer_id"].(string)
		subject, _ := c.Args["subject"].(string)
		kase, err := e.cases.Open(ctx, c.TenantID, cases.CreateInput{
			CustomerID: custID, Subject: subject,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"case": kase}, nil
	case tools.ToolSendWhatsApp:
		to, _ := c.Args["to"].(string)
		body, _ := c.Args["body"].(string)
		rec, err := e.messages.Send(ctx, c.TenantID, messaging.SendInput{
			Channel: "whatsapp", To: to, Body: body,
			CustomerID: strArg(c.Args["customer_id"]),
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"interaction": rec}, nil
	default:
		return nil, fmt.Errorf("unknown tool %q", c.Tool)
	}
}

func strArg(v any) string {
	s, _ := v.(string)
	return s
}

// dbToolPolicy resolves agent tool allowlists from ai_agent_tools.
type dbToolPolicy struct {
	pool *pgxpool.Pool
}

func (d *dbToolPolicy) Allowlist(ctx context.Context, tenantID, agentID string) (map[string]int, error) {
	rows, err := d.pool.Query(ctx, `
                SELECT t.tool_name, t.max_calls_per_invocation
                FROM ai_agent_tools t
                JOIN ai_agents a ON a.id = t.agent_id
                WHERE a.id = $1 AND a.tenant_id = $2 AND t.allowed`, agentID, tenantID)
	if err != nil {
		return nil, apperrors.Internal("db.read_failed", "policy lookup failed").WithCause(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, apperrors.Internal("db.scan_failed", "policy lookup failed").WithCause(err)
		}
		out[name] = n
	}
	return out, nil
}

// apiWorkflowServices adapts messaging for workflow steps.
type apiWorkflowServices struct {
	messages *messaging.Service
	pool     *pgxpool.Pool
}

func (w *apiWorkflowServices) QueueMessage(ctx context.Context, tenantID, customerID, typ, to, body string) error {
	if w.messages != nil && customerID != "" {
		_, err := w.messages.Send(ctx, tenantID, messaging.SendInput{
			CustomerID: customerID, Channel: typ, To: to, Body: body,
		})
		return err
	}
	_ = w.pool
	return nil
}
