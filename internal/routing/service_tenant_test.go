package routing

// Regression test for the P0 cross-tenant isolation fix (MAT-D5, issue #96):
// POST /api/v1/routing/interactions/{id} accepted a foreign-tenant interaction
// UUID and persisted a routing_decision under the CALLER's tenant embedding the
// foreign id. DB-backed: runs against a real PostgreSQL when
// ORVEXA_TEST_DATABASE_URL points at an Orvexa database with migrations applied
// (scripts/devstack.sh start; the URL it prints). Without the variable the test
// skips, matching the repo's integration-test convention.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// fakePresence is a deterministic PresenceReader: no agents available, so the
// engine records a queued/no-agent outcome without assigning anything.
type fakePresence struct{}

func (fakePresence) AvailableAgents(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// routingTenantHarness mints two isolated tenants with one interaction each
// via the documented SQL bootstrap path and wires a routing Service.
type routingTenantHarness struct {
	pool    *pgxpool.Pool
	svc     *Service
	org     string
	tenantA string
	tenantB string
	interA  string // interaction owned by tenant A (foreign from B's view)
	interB  string // interaction owned by tenant B
}

func newRoutingTenantHarness(t *testing.T) *routingTenantHarness {
	t.Helper()
	dsn := os.Getenv("ORVEXA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORVEXA_TEST_DATABASE_URL not set — skipping tenant-scope regression (devstack path: scripts/devstack.sh start)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect db: %v", err)
	}

	h := &routingTenantHarness{pool: pool, org: uuid.NewString()}
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$3)`,
		h.org, "routing tenant-scope", "rt-tscope-"+h.org[:8]); err != nil {
		t.Fatalf("mint org: %v", err)
	}
	mintTenant := func(name string) string {
		id := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO tenants (id, organization_id, name) VALUES ($1,$2,$3)`, id, h.org, name); err != nil {
			t.Fatalf("mint %s: %v", name, err)
		}
		return id
	}
	h.tenantA = mintTenant("rt-tscope A")
	h.tenantB = mintTenant("rt-tscope B")
	mintInteraction := func(tenant, provider string) string {
		cust := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO customers (id, tenant_id, display_name) VALUES ($1,$2,$3)`,
			cust, tenant, "cust "+provider); err != nil {
			t.Fatalf("mint customer: %v", err)
		}
		conv := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO conversations (id, tenant_id, customer_id, channel) VALUES ($1,$2,$3,'chat')`,
			conv, tenant, cust); err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
		inter := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO interactions (id, tenant_id, conversation_id, customer_id, channel, direction, status, source, destination, provider, provider_ref)
			 VALUES ($1,$2,$3,$4,'chat','inbound','pending','rt-tscope:test','queue-x',$5,$5||'-ref')`,
			inter, tenant, conv, cust, provider); err != nil {
			t.Fatalf("seed interaction: %v", err)
		}
		return inter
	}
	h.interA = mintInteraction(h.tenantA, "rt-a")
	h.interB = mintInteraction(h.tenantB, "rt-b")

	h.svc = NewService(pool, fakePresence{}, outbox.NewWriter(pool), "routing-tenant-scope-test")

	t.Cleanup(func() { h.teardown() })
	return h
}

func (h *routingTenantHarness) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stmts := []string{
		`DELETE FROM outbox_events WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM routing_decisions WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM agent_presence WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM interactions WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM conversation_participants WHERE conversation_id IN (SELECT id FROM conversations c JOIN tenants t ON t.id = c.tenant_id WHERE t.organization_id = $1)`,
		`DELETE FROM conversations WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM customers WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM tenants WHERE organization_id = $1`,
		`DELETE FROM organizations WHERE id = $1`,
	}
	for _, q := range stmts {
		_, _ = h.pool.Exec(ctx, q, h.org)
	}
	h.pool.Close()
}

// TestRouteRejectsForeignInteraction pins #96 (MAT-D5): routing a
// foreign-tenant interaction id must 404 and must NOT persist any decision.
func TestRouteRejectsForeignInteraction(t *testing.T) {
	h := newRoutingTenantHarness(t)
	ctx := context.Background()

	_, err := h.svc.Route(ctx, h.tenantB, Request{InteractionID: h.interA, Priority: 5})
	if err == nil {
		t.Fatal("foreign interaction routed — #96 not fixed")
	}
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) || appErr.Kind != apperrors.KindNotFound || appErr.Code != "interaction.not_found" {
		t.Fatalf("expected 404 interaction.not_found, got %v", err)
	}

	var decisions int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM routing_decisions
		 WHERE tenant_id = $1 OR interaction_id = $2`,
		h.tenantB, uuid.MustParse(h.interA)).Scan(&decisions); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if decisions != 0 {
		t.Fatalf("cross-tenant decision persisted: %d rows (want 0)", decisions)
	}
}

// TestRouteOwnTenantInteractionStillSucceeds preserves the owning tenant's
// behavior: the decision evaluates, persists under the caller's tenant, and
// references the caller's own interaction.
func TestRouteOwnTenantInteractionStillSucceeds(t *testing.T) {
	h := newRoutingTenantHarness(t)
	ctx := context.Background()

	d, err := h.svc.Route(ctx, h.tenantA, Request{InteractionID: h.interA, Priority: 5})
	if err != nil {
		t.Fatalf("own-tenant route failed: %v", err)
	}
	if d.InteractionID != h.interA {
		t.Fatalf("unexpected decision: interaction=%s", d.InteractionID)
	}
	if d.Outcome == "" {
		t.Fatal("decision recorded without an outcome")
	}

	var tenantID string
	if err := h.pool.QueryRow(ctx,
		`SELECT tenant_id::text FROM routing_decisions WHERE id = $1`, uuid.MustParse(d.ID)).Scan(&tenantID); err != nil {
		t.Fatalf("decision not persisted: %v", err)
	}
	if tenantID != h.tenantA {
		t.Fatalf("decision persisted under %s (want %s)", tenantID, h.tenantA)
	}
}
