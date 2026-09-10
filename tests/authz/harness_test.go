//go:build authz

package authz

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
	"github.com/Roy-Wanyoike/orvexa/internal/webhooks"
)

// testHMACSecret is a fixed test fixture for the webhook-gateway probes. It is
// set on the API process env by the harness and used to compute valid
// signatures; it is not a deployment secret.
const testHMACSecret = "authz-matrix-webhook-hmac-fixture"

// defaultDevstackPort is used when the harness auto-starts the devstack.
// 55439 deliberately avoids the shared 55432 default so this suite never
// collides with another clone's stack on a workstation.
const defaultDevstackPort = "55439"

// requiredTables must exist before the matrix runs — a loud failure here
// means migrations did not apply (rather than confusing per-route errors).
var requiredTables = []string{
	"tenants", "api_keys", "audit_events",
	"customers", "customer_identifiers",
	"conversations", "conversation_participants", "interactions",
	"agents", "agent_presence", "queues",
	"cases", "case_notes", "case_interactions",
	"routing_decisions",
	"ai_agents", "ai_agent_tools",
	"workflow_instances",
}

// fixtures are the resources created through the PUBLIC API with tenant A's
// key during harness boot. Tenant B gets its own minimal set. Every id here
// is a real UUID owned by tenant A (or B) — exactly what an IDOR attack
// would need to discover.
type fixtures struct {
	runID string

	tenantA, tenantB string
	keyA, keyB       string // raw keys (never rendered unmasked)

	custA1, custA2, custB1 string
	agentA1                string
	queueA1                string
	interA1, interA2       string // chat/custA1, voice/custA1
	interA3, interA4       string // chat/custA2, voice/custA2 (webhook target)
	interA5, interA6       string // created by the A-positive POST probes
	convA1, convA2         string // conversations of interA1 / interA2
	convA3, convA4         string // conversations of interA3 / interA4
	caseA1, caseA2         string
	wfA1                   string
	aiA1                   string // seeded via SQL (no AI-agent admin API)

	interB1 string
	convB1  string

	phoneA1, phoneA2, phoneB1 string
}

type stack struct {
	base     string
	dbURL    string
	repoRoot string
	apiPort  string
	pool     *pgxpool.Pool
	fx       fixtures
	gitRev   string
	cmd      *exec.Cmd
	apiLog   string
	client   *http.Client
}

// do performs one authenticated (or not) request against the running API.
// key "" means no credentials at all. Extra headers carry the webhook
// signature on the public-ingress probes.
func (s *stack) do(t *testing.T, method, path, key, body string, hdr map[string]string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.base+path, rdr)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, string(b)
}

// jsonField extracts a top-level string field from a response body. Note:
// interactions.Rec ships without json tags, so its wire keys are the Go field
// names ("ID", "ConversationID", "TenantID") — callers pass the exact key.
func jsonField(t *testing.T, body, field string) string {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode response (%s): %v\nbody: %s", field, err, body)
	}
	v, _ := m[field].(string)
	return v
}

// maskKey renders a raw API key safely for the committed report.
func maskKey(k string) string {
	if len(k) <= 10 {
		return "****"
	}
	return k[:10] + "…"
}

func waitReady(t *testing.T, base string, deadline time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		resp, err := client.Get(base + "/readyz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("API did not become ready within %s at %s", deadline, base)
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// bootStack brings up the REAL stack: devstack PG (unless an external DB URL
// is provided), migrations (applied by the devstack), two minted tenants,
// the API process on a test port, and API-created fixtures.
func bootStack(t *testing.T) *stack {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root detection failed (no go.mod at %s): %v", root, err)
	}

	s := &stack{repoRoot: root, client: &http.Client{Timeout: 20 * time.Second}}
	s.dbURL = os.Getenv("ORVEXA_TEST_DATABASE_URL")
	if s.dbURL == "" {
		s.startDevstack(t)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(s.dbURL)
	if err != nil {
		t.Fatalf("parse db url: %v", err)
	}
	cfg.MaxConns = 4
	s.pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect db: %v", err)
	}
	t.Cleanup(s.pool.Close)

	for _, tbl := range requiredTables {
		var reg any
		if err := s.pool.QueryRow(ctx,
			`SELECT to_regclass('public.'||$1)`, tbl).Scan(&reg); err != nil || reg == nil {
			t.Fatalf("migrations incomplete: table %q missing (run scripts/devstack.sh start first)", tbl)
		}
	}

	s.mintTenants(t)
	s.gitRev = gitRev(root)
	s.startAPI(t)
	s.seedFixtures(t)
	return s
}

// gitRev records the commit under test for the committed evidence report.
func gitRev(root string) string {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func (s *stack) startDevstack(t *testing.T) {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command(filepath.Join(s.repoRoot, "scripts", "devstack.sh"), args...)
		cmd.Dir = s.repoRoot
		port := os.Getenv("ORVEXA_DEVSTACK_PORT")
		if port == "" {
			port = defaultDevstackPort
		}
		cmd.Env = append(os.Environ(), "ORVEXA_DEVSTACK_PORT="+port)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("devstack %s failed: %v\n%s", strings.Join(args, " "), err, tail(out, 4000))
		}
		return string(out)
	}
	run("start")
	url := strings.TrimSpace(run("url"))
	if url == "" {
		t.Fatal("devstack url command produced no output")
	}
	s.dbURL = url
}

func (s *stack) startAPI(t *testing.T) {
	t.Helper()
	s.apiPort = freePort(t)
	s.base = "http://127.0.0.1:" + s.apiPort

	logf, err := os.CreateTemp(t.TempDir(), "orvexa-api-*.log")
	if err != nil {
		t.Fatalf("api log: %v", err)
	}
	s.apiLog = logf.Name()

	// Deterministic posture: API-key-only auth (no OIDC), inproc bus,
	// simulator providers, degraded search, loopback bind. ORVEXA_* vars that
	// would flip any of those are stripped from the inherited environment.
	var env []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "ORVEXA_OIDC_"),
			strings.HasPrefix(kv, "ORVEXA_OPENSEARCH_"),
			strings.HasPrefix(kv, "ORVEXA_REDIS_"),
			strings.HasPrefix(kv, "ORVEXA_CLICKHOUSE_"),
			strings.HasPrefix(kv, "ORVEXA_HTTP_ADDR"),
			strings.HasPrefix(kv, "ORVEXA_DATABASE_URL"):
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"ORVEXA_HTTP_ADDR=127.0.0.1:"+s.apiPort,
		"ORVEXA_DATABASE_URL="+s.dbURL,
		"ORVEXA_LOG_LEVEL=warn",
		"ORVEXA_ENV=development",
		"ORVEXA_BUS_DRIVER=inproc",
		"ORVEXA_WEBHOOK_HMAC_SECRET="+testHMACSecret,
	)

	cmd := exec.Command("go", "run", "./cmd/api")
	cmd.Dir = s.repoRoot
	cmd.Env = env
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group: kill the `go run` child too
	if err := cmd.Start(); err != nil {
		t.Fatalf("start api: %v", err)
	}
	s.cmd = cmd
	t.Cleanup(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
			done := make(chan struct{})
			go func() { _, _ = s.cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			}
		}
		logf.Close()
	})

	waitReady(t, s.base, 150*time.Second)
}

// mintTenants provisions two isolated tenants + API keys exactly the way the
// operations runbook bootstraps the first tenant (SQL; hashed keys, scope
// "api"). There is no admin API for this — that is the documented bootstrap
// path, and the matrix treats it as the honest source of credentials.
func (s *stack) mintTenants(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	s.fx.runID = randDigits(8)

	org := uuid.NewString()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$3)`,
		org, "Authz Matrix", "authz-matrix-"+s.fx.runID); err != nil {
		t.Fatalf("mint org: %v", err)
	}
	mint := func(name string) (tenantID, rawKey string) {
		tenantID = uuid.NewString()
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO tenants (id, organization_id, name) VALUES ($1,$2,$3)`,
			tenantID, org, name); err != nil {
			t.Fatalf("mint tenant %s: %v", name, err)
		}
		rawKey = tenancy.NewRawKey()
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes)
                         VALUES ($1,$2,$3,$4,$5)`,
			uuid.NewString(), tenantID, "authz-matrix", tenancy.HashKey(rawKey), []string{"api"}); err != nil {
			t.Fatalf("mint key for %s: %v", name, err)
		}
		return tenantID, rawKey
	}
	s.fx.tenantA, s.fx.keyA = mint("Matrix Tenant A")
	s.fx.tenantB, s.fx.keyB = mint("Matrix Tenant B")

	t.Cleanup(func() { s.cleanupRows() })
}

// cleanupRows best-effort removes everything the harness minted/created so
// re-runs never fight unique constraints. The devstack remains running
// (idempotent, documented) — `scripts/devstack.sh clean` still gives a
// factory-fresh database.
func (s *stack) cleanupRows() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stmts := []string{
		`DELETE FROM case_interactions WHERE case_id IN (SELECT id FROM cases WHERE tenant_id IN ($1,$2))`,
		`DELETE FROM case_notes WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM cases WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM interactions WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM conversation_participants WHERE conversation_id IN (SELECT id FROM conversations WHERE tenant_id IN ($1,$2))`,
		`DELETE FROM conversations WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM customers WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM agent_presence WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM routing_decisions WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM agents WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM queues WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM ai_agent_tools WHERE agent_id IN (SELECT id FROM ai_agents WHERE tenant_id IN ($1,$2))`,
		`DELETE FROM ai_agents WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM workflow_instances WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM api_keys WHERE tenant_id IN ($1,$2)`,
		`DELETE FROM tenants WHERE id IN ($1,$2)`,
		`DELETE FROM organizations WHERE slug = $3`,
	}
	for _, q := range stmts {
		args := []any{s.fx.tenantA, s.fx.tenantB}
		if strings.Contains(q, "$3") {
			args = append(args, "authz-matrix-"+s.fx.runID)
		}
		_, _ = s.pool.Exec(ctx, q, args...)
	}
}

// seedFixtures creates every adversarial id through the PUBLIC API with the
// tenant keys themselves (except the AI agent config, which has no admin
// surface and is seeded via SQL the way a tenant would provision it).
func (s *stack) seedFixtures(t *testing.T) {
	t.Helper()
	fx := &s.fx
	rnd := randDigits(7)
	fx.phoneA1 = "25471" + rnd
	fx.phoneA2 = "25472" + rnd
	fx.phoneB1 = "25473" + rnd

	post := func(key, path, body string) (int, string) {
		return s.do(t, http.MethodPost, path, key, body, nil)
	}
	must201 := func(key, path, body, what string) string {
		code, resp := post(key, path, body)
		if code != http.StatusCreated {
			t.Fatalf("seed %s: got %d want 201\n%s", what, code, resp)
		}
		return jsonField(t, resp, "id")
	}

	fx.custA1 = must201(fx.keyA, "/api/v1/customers/",
		fmt.Sprintf(`{"display_name":"Matrix Cust A1","identifiers":[{"type":"phone","value":%q,"is_primary":true}]}`, fx.phoneA1),
		"custA1")
	fx.custA2 = must201(fx.keyA, "/api/v1/customers/",
		fmt.Sprintf(`{"display_name":"Matrix Cust A2","identifiers":[{"type":"phone","value":%q,"is_primary":true}]}`, fx.phoneA2),
		"custA2")
	fx.custB1 = must201(fx.keyB, "/api/v1/customers/",
		fmt.Sprintf(`{"display_name":"Matrix Cust B1","identifiers":[{"type":"phone","value":%q,"is_primary":true}]}`, fx.phoneB1),
		"custB1")

	fx.agentA1 = must201(fx.keyA, "/api/v1/agents/",
		fmt.Sprintf(`{"external_identity":"authz-agent-a-%s","display_name":"Agent A1"}`, fx.fx_run()),
		"agentA1")
	fx.queueA1 = must201(fx.keyA, "/api/v1/queues/",
		fmt.Sprintf(`{"name":"authz-q-a-%s","priority":5}`, fx.fx_run()),
		"queueA1")

	seedInteraction := func(key, cust, channel, dest string) (interID, convID string) {
		code, resp := post(key, "/api/v1/interactions/",
			fmt.Sprintf(`{"customer_id":%q,"channel":%q,"direction":"inbound","source":"seed:matrix","destination":%q}`, cust, channel, dest))
		if code != http.StatusCreated {
			t.Fatalf("seed interaction (%s): got %d want 201\n%s", channel, code, resp)
		}
		// interactions.Rec has no json tags → wire keys are "ID"/"ConversationID".
		return jsonField(t, resp, "ID"), jsonField(t, resp, "ConversationID")
	}
	fx.interA1, fx.convA1 = seedInteraction(fx.keyA, fx.custA1, "chat", "queue-a")
	fx.interA2, fx.convA2 = seedInteraction(fx.keyA, fx.custA1, "voice", "+254711000001")
	fx.interA3, fx.convA3 = seedInteraction(fx.keyA, fx.custA2, "chat", "queue-a")
	fx.interA4, fx.convA4 = seedInteraction(fx.keyA, fx.custA2, "voice", "+254711000002")
	fx.interB1, fx.convB1 = seedInteraction(fx.keyB, fx.custB1, "chat", "queue-b")

	fx.caseA1 = must201(fx.keyA, "/api/v1/cases/",
		fmt.Sprintf(`{"customer_id":%q,"subject":"authz matrix case A1"}`, fx.custA1), "caseA1")
	fx.caseA2 = must201(fx.keyA, "/api/v1/cases/",
		fmt.Sprintf(`{"customer_id":%q,"subject":"authz matrix case A2"}`, fx.custA2), "caseA2")

	// AI agent config: data plane with no REST admin surface (migration 0009
	// is the provisioning contract) — seeded via SQL, scoped to tenant A.
	ctx := context.Background()
	fx.aiA1 = uuid.NewString()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ai_agents (id, tenant_id, name, model_hint, status) VALUES ($1,$2,$3,'rules-v1','active')`,
		fx.aiA1, fx.tenantA, "authz-ai-a-"+fx.fx_run()); err != nil {
		t.Fatalf("seed ai agent: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO ai_agent_tools (agent_id, tool_name, allowed, max_calls_per_invocation) VALUES ($1,'get_customer',true,3)`,
		fx.aiA1); err != nil {
		t.Fatalf("seed ai tool: %v", err)
	}

	code, resp := post(fx.keyA, "/api/v1/workflows/callbacks",
		fmt.Sprintf(`{"customer_id":%q,"phone":%q,"notes":"authz matrix"}`, fx.custA1, fx.phoneA1))
	if code != http.StatusCreated {
		t.Fatalf("seed workflow: got %d want 201\n%s", code, resp)
	}
	fx.wfA1 = jsonField(t, resp, "id")
}

// fx_run keeps format strings short (unique-per-run suffix).
func (f *fixtures) fx_run() string { return f.runID }

// randDigits returns n decimal digits (phone/number fixtures must be
// digits-only to satisfy the customer identifier validation).
func randDigits(n int) string {
	b := make([]byte, n)
	_, _ = crand.Read(b)
	for i := range b {
		b[i] = byte('0' + b[i]%10)
	}
	return string(b)
}

// tail returns the last n bytes of a log (diagnostics only; the API never
// logs secrets).
func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "…\n" + string(b[len(b)-n:])
}

// signedWebhook builds a valid simulator webhook body + signature header set
// targeting a tenant-A interaction (the signature fixture mirrors the
// documented X-Orvexa-Signature = hex(hmac_sha256(secret, raw body))).
func signedWebhook(t *testing.T, event, tenantID, interactionID string) (string, map[string]string) {
	t.Helper()
	body := fmt.Sprintf(`{"event":%q,"interaction_id":%q,"tenant_id":%q,"timestamp":%q}`,
		event, interactionID, tenantID, time.Now().UTC().Format(time.RFC3339Nano))
	return body, map[string]string{"X-Orvexa-Signature": webhooks.ComputeSignature(testHMACSecret, []byte(body))}
}
