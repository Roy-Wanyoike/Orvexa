package workflows

// Regression test for the P0 cross-tenant isolation fix (MAT-D6, issue #97):
// POST /api/v1/workflows/callbacks and /collections accepted a customer UUID
// belonging to another tenant (workflow_instances carries no customer FK).
// DB-backed: runs against a real PostgreSQL when ORVEXA_TEST_DATABASE_URL
// points at an Orvexa database with migrations applied (scripts/devstack.sh
// start; the URL it prints). Without the variable the tests skip, matching the
// repo's integration-test convention.

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

// workflowTenantHarness mints two isolated tenants with one customer each via
// the documented SQL bootstrap path and wires the workflow Engine.
type workflowTenantHarness struct {
	pool    *pgxpool.Pool
	engine  *Engine
	org     string
	tenantA string
	tenantB string
	custA   string // customer owned by tenant A (foreign from B's point of view)
	custB   string // customer owned by tenant B
}

func newWorkflowTenantHarness(t *testing.T) *workflowTenantHarness {
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

	h := &workflowTenantHarness{pool: pool, org: uuid.NewString()}
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$3)`,
		h.org, "workflows tenant-scope", "wf-tscope-"+h.org[:8]); err != nil {
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
	h.tenantA = mintTenant("wf-tscope A")
	h.tenantB = mintTenant("wf-tscope B")
	mintCustomer := func(tenant, phone string) string {
		id := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO customers (id, tenant_id, display_name) VALUES ($1,$2,$3)`,
			id, tenant, "cust "+phone); err != nil {
			t.Fatalf("mint customer for %s: %v", tenant, err)
		}
		return id
	}
	h.custA = mintCustomer(h.tenantA, "254730000001")
	h.custB = mintCustomer(h.tenantB, "254730000002")

	h.engine = NewEngine(pool, outbox.NewWriter(pool), nil, "workflow-tenant-scope-test")

	t.Cleanup(func() { h.teardown() })
	return h
}

func (h *workflowTenantHarness) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stmts := []string{
		`DELETE FROM outbox_events WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM workflow_steps WHERE instance_id IN (SELECT id FROM workflow_instances WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1))`,
		`DELETE FROM workflow_instances WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM customers WHERE tenant_id IN (SELECT id FROM tenants WHERE organization_id = $1)`,
		`DELETE FROM tenants WHERE organization_id = $1`,
		`DELETE FROM organizations WHERE id = $1`,
	}
	for _, q := range stmts {
		_, _ = h.pool.Exec(ctx, q, h.org)
	}
	h.pool.Close()
}

func wantCustomerNotFound(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("foreign customer accepted — #97 not fixed")
	}
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) || appErr.Kind != apperrors.KindNotFound || appErr.Code != "customer.not_found" {
		t.Fatalf("expected 404 customer.not_found, got %v", err)
	}
}

func (h *workflowTenantHarness) instancesFor(t *testing.T, tenant string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM workflow_instances WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	return n
}

// TestStartCallbackRejectsForeignTenantCustomer pins #97 (MAT-D6) on the
// callback ingest path: a foreign-tenant customer_id must 404 and start
// nothing.
func TestStartCallbackRejectsForeignTenantCustomer(t *testing.T) {
	h := newWorkflowTenantHarness(t)

	_, err := h.engine.StartCallback(context.Background(), h.tenantB,
		StartCallbackInput{CustomerID: h.custA, Phone: "254730000099"})
	wantCustomerNotFound(t, err)

	if n := h.instancesFor(t, h.tenantB); n != 0 {
		t.Fatalf("callback instance persisted for foreign customer: %d (want 0)", n)
	}
}

// TestStartCollectionsRejectsForeignTenantCustomer pins #97 (MAT-D6) on the
// collections ingest path: a foreign-tenant customer_id must 404 and start
// nothing.
func TestStartCollectionsRejectsForeignTenantCustomer(t *testing.T) {
	h := newWorkflowTenantHarness(t)

	_, err := h.engine.StartCollections(context.Background(), h.tenantB,
		StartCollectionsInput{CustomerID: h.custA, InvoiceRef: "INV-1", AmountDue: 100, Phone: "254730000099"})
	wantCustomerNotFound(t, err)

	if n := h.instancesFor(t, h.tenantB); n != 0 {
		t.Fatalf("collections instance persisted for foreign customer: %d (want 0)", n)
	}
}

// TestStartOwnTenantWorkflowsStillSucceeds preserves the owning tenant's
// behavior on both ingest paths.
func TestStartOwnTenantWorkflowsStillSucceeds(t *testing.T) {
	h := newWorkflowTenantHarness(t)
	ctx := context.Background()

	cb, err := h.engine.StartCallback(ctx, h.tenantA,
		StartCallbackInput{CustomerID: h.custA, Phone: "254730000001", Notes: "tscope"})
	if err != nil {
		t.Fatalf("own-tenant callback failed: %v", err)
	}
	if cb.TenantID != h.tenantA || cb.Type != TypeCallback {
		t.Fatalf("unexpected callback: tenant=%s type=%s", cb.TenantID, cb.Type)
	}

	co, err := h.engine.StartCollections(ctx, h.tenantA,
		StartCollectionsInput{CustomerID: h.custA, InvoiceRef: "INV-2", AmountDue: 500, Phone: "254730000002"})
	if err != nil {
		t.Fatalf("own-tenant collections failed: %v", err)
	}
	if co.TenantID != h.tenantA || co.Type != TypeCollections {
		t.Fatalf("unexpected collections: tenant=%s type=%s", co.TenantID, co.Type)
	}

	if n := h.instancesFor(t, h.tenantA); n != 2 {
		t.Fatalf("own-tenant instances: %d (want 2)", n)
	}
}
