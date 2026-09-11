package interactions

// Regression tests for the P0 cross-tenant isolation fixes (MAT-D1, issue #92).
// They are DB-backed and run against a real PostgreSQL when ORVEXA_TEST_DATABASE_URL
// points at an Orvexa database with migrations applied (scripts/devstack.sh start;
// the URL it prints). Without the variable the suite skips, matching the repo's
// integration-test convention (bus, clickhouse, opensearch, redis drivers).

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// tenantScopeHarness mints two isolated tenants with one customer each via the
// documented SQL bootstrap path (the same path the authz matrix and the
// operations runbook use) and wires a Service over the shared pool.
type tenantScopeHarness struct {
	pool    *pgxpool.Pool
	svc     *Service
	org     string
	tenantA string
	tenantB string
	custA   string // customer owned by tenant A (foreign from B's point of view)
	custB   string // customer owned by tenant B
}

func newTenantScopeHarness(t *testing.T) *tenantScopeHarness {
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

	h := &tenantScopeHarness{pool: pool, org: uuid.NewString()}
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$3)`,
		h.org, "interactions tenant-scope", "ix-tscope-"+h.org[:8]); err != nil {
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
	h.tenantA = mintTenant("tscope A")
	h.tenantB = mintTenant("tscope B")
	mintCustomer := func(tenant, phone string) string {
		id := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO customers (id, tenant_id, display_name) VALUES ($1,$2,$3)`,
			id, tenant, "cust "+phone); err != nil {
			t.Fatalf("mint customer for %s: %v", tenant, err)
		}
		return id
	}
	h.custA = mintCustomer(h.tenantA, "254710000001")
	h.custB = mintCustomer(h.tenantB, "254710000002")

	h.svc = NewService(pool, outbox.NewWriter(pool), "tenant-scope-test")

	t.Cleanup(func() { h.teardown() })
	return h
}

// teardown removes every row the harness minted so re-runs never fight
// constraints (best-effort; the devstack itself stays factory-resettable via
// scripts/devstack.sh clean).
func (h *tenantScopeHarness) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stmts := []string{
		`DELETE FROM outbox_events WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
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

func (h *tenantScopeHarness) createInput(customerID string) CreateInput {
	return CreateInput{
		CustomerID:  customerID,
		Channel:     ChannelChat,
		Direction:   DirectionInbound,
		Source:      "tscope:test",
		Destination: "queue-tscope",
		Provider:    "tscope",
	}
}

func wantNotFound(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", code)
	}
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected app error, got %T: %v", err, err)
	}
	if appErr.Kind != apperrors.KindNotFound {
		t.Fatalf("expected KindNotFound, got %s (%s)", appErr.Kind, appErr.Code)
	}
	if appErr.Code != code {
		t.Fatalf("expected code %s, got %s", code, appErr.Code)
	}
}

var tscopeRef atomic.Uint64

// TestCreateRejectsForeignTenantCustomer pins #92 (MAT-D1): an interaction
// create referencing a customer owned by ANOTHER tenant must 404 (not 201)
// and must persist nothing — no interaction, and critically no conversation
// auto-opened against the foreign customer.
func TestCreateRejectsForeignTenantCustomer(t *testing.T) {
	h := newTenantScopeHarness(t)
	ctx := context.Background()

	in := h.createInput(h.custA) // tenant-A customer reached with tenant-B credentials
	_, err := h.svc.Create(ctx, h.tenantB, in)
	wantNotFound(t, err, "customer.not_found")

	var interactions, conversations int
	if err := h.pool.QueryRow(ctx,
		`SELECT
			(SELECT count(*) FROM interactions WHERE tenant_id = $1),
			(SELECT count(*) FROM conversations WHERE tenant_id = $1)`,
		h.tenantB).Scan(&interactions, &conversations); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if interactions != 0 || conversations != 0 {
		t.Fatalf("cross-tenant rows persisted: interactions=%d conversations=%d (want 0/0)",
			interactions, conversations)
	}
}

// TestCreateRejectsUnknownCustomer pins that a garbage customer id behaves
// exactly like a foreign one (no enumeration oracle).
func TestCreateRejectsUnknownCustomer(t *testing.T) {
	h := newTenantScopeHarness(t)
	_, err := h.svc.Create(context.Background(), h.tenantB, h.createInput(uuid.NewString()))
	wantNotFound(t, err, "customer.not_found")
}

// TestCreateOwnTenantCustomerStillSucceeds preserves the owning tenant's
// behavior end-to-end: create succeeds, conversation auto-opens for the OWN
// customer, and the interaction row is anchored to the caller's tenant.
func TestCreateOwnTenantCustomerStillSucceeds(t *testing.T) {
	h := newTenantScopeHarness(t)
	ctx := context.Background()

	in := h.createInput(h.custA)
	in.ProviderRef = "tscope-ref-1"
	rec, err := h.svc.Create(ctx, h.tenantA, in)
	if err != nil {
		t.Fatalf("own-tenant create failed: %v", err)
	}
	if rec.TenantID != h.tenantA || rec.ConversationID == "" {
		t.Fatalf("unexpected rec: tenant=%s conversation=%q", rec.TenantID, rec.ConversationID)
	}

	var convCustomer string
	var convTenant string
	if err := h.pool.QueryRow(ctx,
		`SELECT tenant_id::text, customer_id::text FROM conversations WHERE id = $1`,
		rec.ConversationID).Scan(&convTenant, &convCustomer); err != nil {
		t.Fatalf("auto-opened conversation missing: %v", err)
	}
	if convTenant != h.tenantA || convCustomer != h.custA {
		t.Fatalf("conversation anchored wrong: tenant=%s customer=%s", convTenant, convCustomer)
	}

	// Same-tenant replay with the same provider ref stays idempotent (201 semantics).
	same, err := h.svc.Create(ctx, h.tenantA, in)
	if err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	if same.ID != rec.ID {
		t.Fatalf("replay returned different interaction %s (want %s)", same.ID, rec.ID)
	}
}
