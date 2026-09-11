package cases

// Regression tests for the P0 cross-tenant isolation fixes in the case module:
//
//	#93 (MAT-D2) Open accepts a foreign-tenant customer_id
//	#94 (MAT-D3) AddNote writes a note onto a foreign tenant's case
//	#95 (MAT-D4) LinkInteraction verifies the interaction tenant but not the case
//
// They are DB-backed and run against a real PostgreSQL when ORVEXA_TEST_DATABASE_URL
// points at an Orvexa database with migrations applied (scripts/devstack.sh start;
// the URL it prints). Without the variable the suite skips, matching the repo's
// integration-test convention (bus, clickhouse, opensearch, redis drivers).

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

// caseTenantHarness mints two isolated tenants with one customer each via the
// documented SQL bootstrap path and wires a case Service over the shared pool.
type caseTenantHarness struct {
	pool    *pgxpool.Pool
	svc     *Service
	org     string
	tenantA string
	tenantB string
	custA   string // customer owned by tenant A (foreign from B's point of view)
	custB   string // customer owned by tenant B
}

func newCaseTenantHarness(t *testing.T) *caseTenantHarness {
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

	h := &caseTenantHarness{pool: pool, org: uuid.NewString()}
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$3)`,
		h.org, "cases tenant-scope", "cs-tscope-"+h.org[:8]); err != nil {
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
	h.tenantA = mintTenant("cs-tscope A")
	h.tenantB = mintTenant("cs-tscope B")
	mintCustomer := func(tenant, phone string) string {
		id := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO customers (id, tenant_id, display_name) VALUES ($1,$2,$3)`,
			id, tenant, "cust "+phone); err != nil {
			t.Fatalf("mint customer for %s: %v", tenant, err)
		}
		return id
	}
	h.custA = mintCustomer(h.tenantA, "254720000001")
	h.custB = mintCustomer(h.tenantB, "254720000002")

	h.svc = NewService(pool, outbox.NewWriter(pool), "case-tenant-scope-test")

	t.Cleanup(func() { h.teardown() })
	return h
}

func (h *caseTenantHarness) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stmts := []string{
		`DELETE FROM outbox_events WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM case_interactions WHERE case_id IN (SELECT id FROM cases WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1))`,
		`DELETE FROM case_notes WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM cases WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
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

// seedInteraction inserts one conversation+interaction pair directly (the
// interaction plane is exercised by its own module's tests); used as the
// link target for #95.
func (h *caseTenantHarness) seedInteraction(t *testing.T, tenant, customer, provider string) string {
	t.Helper()
	ctx := context.Background()
	conv := uuid.NewString()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO conversations (id, tenant_id, customer_id, channel) VALUES ($1,$2,$3,'chat')`,
		conv, tenant, customer); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	inter := uuid.NewString()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO interactions (id, tenant_id, conversation_id, customer_id, channel, direction, status, source, destination, provider, provider_ref)
		 VALUES ($1,$2,$3,$4,'chat','inbound','pending','cs-tscope:test','queue-x',$5,$5||'-ref')`,
		inter, tenant, conv, customer, provider); err != nil {
		t.Fatalf("seed interaction: %v", err)
	}
	return inter
}

func wantCaseNotFound(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", code)
	}
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected app error, got %T: %v", err, err)
	}
	if appErr.Kind != apperrors.KindNotFound || appErr.Code != code {
		t.Fatalf("expected KindNotFound/%s, got %s/%s", code, appErr.Kind, appErr.Code)
	}
}

// TestOpenRejectsForeignTenantCustomer pins #93 (MAT-D2): a case create
// referencing a customer owned by ANOTHER tenant must 404 and persist nothing.
func TestOpenRejectsForeignTenantCustomer(t *testing.T) {
	h := newCaseTenantHarness(t)
	ctx := context.Background()

	_, err := h.svc.Open(ctx, h.tenantB, CreateInput{CustomerID: h.custA, Subject: "foreign ref"})
	wantCaseNotFound(t, err, "customer.not_found")

	var n int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM cases WHERE tenant_id = $1`, h.tenantB).Scan(&n); err != nil {
		t.Fatalf("count cases: %v", err)
	}
	if n != 0 {
		t.Fatalf("cross-tenant case persisted: %d rows (want 0)", n)
	}
}

// TestOpenOwnTenantCustomerStillSucceeds preserves the owning tenant's
// behavior: create succeeds with the per-tenant serial ref.
func TestOpenOwnTenantCustomerStillSucceeds(t *testing.T) {
	h := newCaseTenantHarness(t)
	c, err := h.svc.Open(context.Background(), h.tenantA, CreateInput{CustomerID: h.custA, Subject: "own ref"})
	if err != nil {
		t.Fatalf("own-tenant open failed: %v", err)
	}
	if c.TenantID != h.tenantA || c.CustomerID != h.custA {
		t.Fatalf("unexpected case: tenant=%s customer=%s", c.TenantID, c.CustomerID)
	}
	if c.Ref == "" {
		t.Fatal("per-tenant serial ref missing")
	}
}
