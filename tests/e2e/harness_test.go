//go:build e2e

// Package e2e hosts the customer-journey suite (issue #43): three end-to-end
// journeys driven through the PUBLIC HTTP API of a real stack — the devstack
// PostgreSQL (default port 55445) plus the built api and worker binaries —
// with signed provider webhooks entering through the same fail-closed gateway
// a real carrier uses.
//
// Run (single command, no external services):
//
//      go test -race -tags=e2e ./tests/e2e/
//
// Or against an already-running stack:
//
//      ORVEXA_E2E_BASE_URL=http://localhost:8080 \
//      ORVEXA_E2E_DATABASE_URL='postgres://...' \
//      go test -race -tags=e2e ./tests/e2e/
//
// The harness is also the engine behind scripts/e2e-demo.sh (issue #89):
// transcripts of a run are the journey evidence committed under qa/evidence/.
package e2e

import (
        "bytes"
        "context"
        "crypto/hmac"
        "crypto/rand"
        "crypto/sha256"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "io"
        "net"
        "net/http"
        "os"
        "os/exec"
        "path/filepath"
        "strconv"
        "strings"
        "sync"
        "testing"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"
)

// ---- configuration knobs (environment) ----

const (
        envBaseURL      = "ORVEXA_E2E_BASE_URL"        // external running stack (skips stack management)
        envDBURL        = "ORVEXA_E2E_DATABASE_URL"    // external DB used for seeding/audit queries
        envDevstackPort = "ORVEXA_DEVSTACK_PORT"       // devstack PG port (default 55445)
        envAPIAddr      = "ORVEXA_E2E_API_ADDR"        // api listen address when harness-managed (default 127.0.0.1:55446)
        envHMACEnv      = "ORVEXA_WEBHOOK_HMAC_SECRET" // must match the api process
        envKeep         = "ORVEXA_E2E_KEEP"            // set to 1 to keep the stack alive after the run
)

const (
        defaultDevstackPort = "55445"
        defaultAPIAddr      = "127.0.0.1:55446"
        defaultHMACSecret   = "e2e-hmac-secret"
        readyTimeout        = 2 * time.Minute
)

// harness is the shared fixture every journey consumes.
var harness struct {
        baseURL string // e.g. http://127.0.0.1:55446
        dbURL   string // postgres:// URL used for seeding + audit-trail queries
        secret  string // webhook HMAC secret shared with the api
        repo    string // repository root (devstack.sh lives here)
        pool    *pgxpool.Pool

        tenants map[string]*journeyTenant // journey name → dedicated tenant

        apiCmd         *exec.Cmd
        workerCmd      *exec.Cmd
        workDir        string
        weBootDevstack bool
}

type journeyTenant struct {
        Name     string // logical journey name (j1, j2, j3)
        TenantID string
        RawKey   string
        AgentID  string // seeded AI agent (J3)
}

func envOr(key, def string) string {
        if v := strings.TrimSpace(os.Getenv(key)); v != "" {
                return v
        }
        return def
}

func TestMain(m *testing.M) {
        code, err := runMain(m)
        if err != nil {
                fmt.Fprintf(os.Stderr, "e2e harness: %v\n", err)
                code = 1
        }
        os.Exit(code)
}

func runMain(m *testing.M) (int, error) {
        cwd, err := os.Getwd()
        if err != nil {
                return 1, err
        }
        repo, err := findRepoRoot(cwd)
        if err != nil {
                return 1, err
        }
        harness.repo = repo
        harness.secret = envOr(envHMACEnv, defaultHMACSecret)
        harness.tenants = map[string]*journeyTenant{}

        external := strings.TrimSpace(os.Getenv(envBaseURL)) != ""
        if external {
                // External stack mode: the caller owns the processes; the harness only
                // seeds journey tenants (requires DB access) and runs the journeys.
                harness.baseURL = strings.TrimRight(os.Getenv(envBaseURL), "/")
                harness.dbURL = strings.TrimSpace(os.Getenv(envDBURL))
                if harness.dbURL == "" {
                        return 1, fmt.Errorf("%s mode requires %s for journey tenant seeding", envBaseURL, envDBURL)
                }
        } else {
                if err := bootStack(); err != nil {
                        return 1, err
                }
        }

        code := 1
        func() {
                defer shutdown()
                if err := seedJourneys(); err != nil {
                        fmt.Fprintf(os.Stderr, "e2e harness: seed journeys: %v\n", err)
                        return
                }
                code = m.Run()
        }()
        return code, nil
}

// bootStack brings up the full local stack: devstack PostgreSQL on the
// overridden port (55445 per the e2e wave), then the freshly built api and
// worker binaries against it.
func bootStack() error {
        port := envOr(envDevstackPort, defaultDevstackPort)
        base := map[string]string{"ORVEXA_DEVSTACK_PORT": port}

        // The devstack is idempotent: start on an already-running cluster is a no-op.
        st, err := runScript("status", base)
        if err != nil {
                return err
        }
        harness.weBootDevstack = !strings.Contains(st, "RUNNING")
        if _, err := runScript("start", base); err != nil {
                return fmt.Errorf("devstack start: %w", err)
        }
        out, err := runScript("url", base)
        if err != nil {
                return err
        }
        harness.dbURL = strings.TrimSpace(out)
        if harness.dbURL == "" {
                return fmt.Errorf("devstack url printed nothing")
        }

        // Build the deployables from the current tree — journeys must run the code
        // under test, never a stale binary.
        harness.workDir, err = os.MkdirTemp("", "orvexa-e2e-*")
        if err != nil {
                return err
        }
        apiBin := filepath.Join(harness.workDir, "orvexa-api")
        workerBin := filepath.Join(harness.workDir, "orvexa-worker")
        if out, err := goBuild(apiBin, "./cmd/api"); err != nil {
                return fmt.Errorf("build api: %w\n%s", err, out)
        }
        if out, err := goBuild(workerBin, "./cmd/worker"); err != nil {
                return fmt.Errorf("build worker: %w\n%s", err, out)
        }

        // The API address defaults to a free ephemeral loopback port so the
        // harness never collides with other listeners on the machine (other
        // devstacks, a developer's own api, CI sidecars).
        addr := envOr(envAPIAddr, "")
        if addr == "" {
                l, err := net.Listen("tcp", "127.0.0.1:0")
                if err != nil {
                        return fmt.Errorf("reserve api port: %w", err)
                }
                addr = l.Addr().String()
                _ = l.Close()
        }
        apiEnv := append(os.Environ(),
                "ORVEXA_DATABASE_URL="+harness.dbURL,
                "ORVEXA_WEBHOOK_HMAC_SECRET="+harness.secret,
                "ORVEXA_HTTP_ADDR="+addr,
                "ORVEXA_LOG_LEVEL=warn",
        )
        if harness.apiCmd, err = startProcess(apiBin, apiEnv, filepath.Join(harness.workDir, "api.log")); err != nil {
                return fmt.Errorf("start api: %w", err)
        }
        workerEnv := append(os.Environ(),
                "ORVEXA_DATABASE_URL="+harness.dbURL,
                "ORVEXA_WEBHOOK_HMAC_SECRET="+harness.secret,
                "ORVEXA_LOG_LEVEL=warn",
        )
        if harness.workerCmd, err = startProcess(workerBin, workerEnv, filepath.Join(harness.workDir, "worker.log")); err != nil {
                return fmt.Errorf("start worker: %w", err)
        }

        harness.baseURL = "http://" + addr
        if err := waitReady(harness.baseURL, readyTimeout); err != nil {
                return err
        }
        return nil
}

func seedJourneys() error {
        ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
        defer cancel()
        pool, err := pgxpool.New(ctx, harness.dbURL)
        if err != nil {
                return err
        }
        harness.pool = pool

        for _, name := range []string{"j1", "j2", "j3"} {
                tn, err := seedTenant(ctx, pool, name)
                if err != nil {
                        return fmt.Errorf("seed %s: %w", name, err)
                }
                harness.tenants[name] = tn
        }
        return nil
}

// seedTenant provisions one isolated journey tenant: organization (once),
// tenant, an 'api'-scoped API key, and an AI agent config. The AI agent
// allows get_customer + create_case only — send_whatsapp is deliberately
// absent so J3 can assert a policy REFUSAL (audited) as its failure injection.
func seedTenant(ctx context.Context, pool *pgxpool.Pool, name string) (*journeyTenant, error) {
        var orgID string
        err := pool.QueryRow(ctx, `SELECT id FROM organizations WHERE slug = 'e2e-org'`).Scan(&orgID)
        if err != nil {
                if err := pool.QueryRow(ctx, `INSERT INTO organizations (id, name, slug)
                                VALUES (gen_random_uuid(), 'E2E Org', 'e2e-org') RETURNING id`).Scan(&orgID); err != nil {
                        return nil, err
                }
        }
        stamp := time.Now().UTC().Format("20060102T150405") + "-" + randHex(4)
        tn := &journeyTenant{Name: name, RawKey: "orvx_e2e_" + randHex(24)}
        if err := pool.QueryRow(ctx, `INSERT INTO tenants (id, organization_id, name)
                        VALUES (gen_random_uuid(), $1, $2) RETURNING id`, orgID, "E2E "+name+" "+stamp).Scan(&tn.TenantID); err != nil {
                return nil, err
        }
        keyHash := sha256Hex(tn.RawKey)
        if _, err := pool.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes)
                        VALUES (gen_random_uuid(), $1, 'journey', $2, '{api}')`, tn.TenantID, keyHash); err != nil {
                return nil, err
        }
        if err := pool.QueryRow(ctx, `INSERT INTO ai_agents (id, tenant_id, name)
                        VALUES (gen_random_uuid(), $1, 'support-copilot') RETURNING id`, tn.TenantID).Scan(&tn.AgentID); err != nil {
                return nil, err
        }
        if _, err := pool.Exec(ctx, `INSERT INTO ai_agent_versions (id, agent_id, version, system_prompt)
                        VALUES (gen_random_uuid(), $1, 1, 'You are Orvexa support copilot.')`, tn.AgentID); err != nil {
                return nil, err
        }
        for _, tool := range []struct {
                name string
                max  int
        }{{"get_customer", 5}, {"create_case", 2}} {
                if _, err := pool.Exec(ctx, `INSERT INTO ai_agent_tools (agent_id, tool_name, allowed, max_calls_per_invocation)
                                VALUES ($1, $2, true, $3)`, tn.AgentID, tool.name, tool.max); err != nil {
                        return nil, err
                }
        }
        return tn, nil
}

func shutdown() {
        if harness.apiCmd != nil {
                killProcess(harness.apiCmd)
        }
        if harness.workerCmd != nil {
                killProcess(harness.workerCmd)
        }
        if harness.pool != nil {
                harness.pool.Close()
        }
        if harness.weBootDevstack && os.Getenv(envKeep) != "1" {
                _, _ = runScript("stop", map[string]string{"ORVEXA_DEVSTACK_PORT": envOr(envDevstackPort, defaultDevstackPort)})
        }
        if harness.workDir != "" && os.Getenv(envKeep) != "1" {
                _ = os.RemoveAll(harness.workDir)
        }
}

// ---- process helpers ----

func findRepoRoot(dir string) (string, error) {
        for i := 0; i < 8; i++ {
                if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
                        return dir, nil
                }
                parent := filepath.Dir(dir)
                if parent == dir {
                        break
                }
                dir = parent
        }
        return "", fmt.Errorf("repository root (go.mod) not found upward from %s", dir)
}

func runScript(sub string, extraEnv map[string]string) (string, error) {
        cmd := exec.Command("bash", "scripts/devstack.sh", sub)
        cmd.Dir = harness.repo
        cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH"))
        for k, v := range extraEnv {
                cmd.Env = append(cmd.Env, k+"="+v)
        }
        var buf bytes.Buffer
        cmd.Stdout, cmd.Stderr = &buf, &buf
        if err := cmd.Run(); err != nil {
                return "", fmt.Errorf("devstack %s: %w: %s", sub, err, buf.String())
        }
        return buf.String(), nil
}

func goBuild(out, pkg string) (string, error) {
        cmd := exec.Command("go", "build", "-o", out, pkg)
        cmd.Dir = harness.repo
        var buf bytes.Buffer
        cmd.Stdout, cmd.Stderr = &buf, &buf
        err := cmd.Run()
        return buf.String(), err
}

func startProcess(bin string, env []string, logFile string) (*exec.Cmd, error) {
        f, err := os.Create(logFile)
        if err != nil {
                return nil, err
        }
        defer f.Close()
        cmd := exec.Command(bin)
        cmd.Env = env
        cmd.Stdout, cmd.Stderr = f, f
        if err := cmd.Start(); err != nil {
                return nil, err
        }
        return cmd, nil
}

var killMu sync.Mutex

func killProcess(cmd *exec.Cmd) {
        killMu.Lock()
        defer killMu.Unlock()
        if cmd.Process == nil {
                return
        }
        _ = cmd.Process.Kill()
        _, _ = cmd.Process.Wait()
}

func waitReady(baseURL string, timeout time.Duration) error {
        deadline := time.Now().Add(timeout)
        client := &http.Client{Timeout: 2 * time.Second}
        var last string
        for time.Now().Before(deadline) {
                resp, err := client.Get(baseURL + "/readyz")
                if err == nil {
                        body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
                        resp.Body.Close()
                        var env envelope
                        if json.Unmarshal(body, &env) == nil && env.Data != nil {
                                var data struct {
                                        Status string `json:"status"`
                                }
                                if json.Unmarshal(env.Data, &data) == nil && data.Status == "ready" {
                                        return nil
                                }
                                last = "readyz body: " + string(body)
                        } else {
                                last = "unparseable readyz body"
                        }
                } else {
                        last = err.Error()
                }
                time.Sleep(250 * time.Millisecond)
        }
        return fmt.Errorf("stack not ready within %s (last: %s)", timeout, last)
}

// ---- HTTP helpers (public contract only) ----

type eInfo struct {
        Code    string `json:"code"`
        Message string `json:"message"`
}

type envelope struct {
        Data  json.RawMessage `json:"data"`
        Meta  json.RawMessage `json:"meta"`
        Error *eInfo          `json:"error"`
}

type apiResponse struct {
        Status int
        Body   *envelope
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func doJSON(t *testing.T, method, path, apiKey string, body any) apiResponse {
        t.Helper()
        var rdr io.Reader
        switch b := body.(type) {
        case nil:
        case []byte:
                rdr = bytes.NewReader(b)
        default:
                raw, err := json.Marshal(body)
                if err != nil {
                        t.Fatalf("marshal request body: %v", err)
                }
                rdr = bytes.NewReader(raw)
        }
        req, err := http.NewRequest(method, harness.baseURL+path, rdr)
        if err != nil {
                t.Fatalf("build request: %v", err)
        }
        if apiKey != "" {
                req.Header.Set("X-API-Key", apiKey)
        }
        if body != nil {
                req.Header.Set("Content-Type", "application/json")
        }
        resp, err := httpClient.Do(req)
        if err != nil {
                t.Fatalf("%s %s: %v", method, path, err)
        }
        defer resp.Body.Close()
        raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        env := envelope{}
        if len(raw) > 0 { // 204 No Content (case link) has no body by contract
                if err := json.Unmarshal(raw, &env); err != nil {
                        t.Fatalf("%s %s: response is not the {data,meta}/{error} envelope: %v (%s)", method, path, err, truncate(raw))
                }
        }
        return apiResponse{Status: resp.StatusCode, Body: &env}
}

// decode unwraps data into dst, failing the test on any error envelope or
// unexpected status.
func decode(t *testing.T, resp apiResponse, wantStatus int, dst any) json.RawMessage {
        t.Helper()
        if resp.Status != wantStatus {
                t.Fatalf("status = %d, want %d (error=%+v)", resp.Status, wantStatus, resp.Body.Error)
        }
        if resp.Body.Error != nil {
                t.Fatalf("unexpected error envelope: %+v", resp.Body.Error)
        }
        if dst == nil {
                return resp.Body.Data
        }
        if err := json.Unmarshal(resp.Body.Data, dst); err != nil {
                t.Fatalf("decode data: %v (%s)", err, truncate(resp.Body.Data))
        }
        return resp.Body.Data
}

// sign computes the X-Orvexa-Signature HMAC-SHA256 hex digest providers (and
// the built-in simulator) deliver over the raw webhook body.
func sign(body []byte) string {
        mac := hmac.New(sha256.New, []byte(harness.secret))
        mac.Write(body)
        return hex.EncodeToString(mac.Sum(nil))
}

// postWebhook drives the PUBLIC webhook gateway: POST /api/v1/webhooks/{provider}.
func postWebhook(t *testing.T, provider string, body []byte, signature string) apiResponse {
        t.Helper()
        req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/api/v1/webhooks/"+provider, bytes.NewReader(body))
        if err != nil {
                t.Fatalf("build webhook request: %v", err)
        }
        req.Header.Set("Content-Type", "application/json")
        if signature != "" {
                req.Header.Set("X-Orvexa-Signature", signature)
        }
        resp, err := httpClient.Do(req)
        if err != nil {
                t.Fatalf("webhook %s: %v", provider, err)
        }
        defer resp.Body.Close()
        raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
        var env envelope
        if err := json.Unmarshal(raw, &env); err != nil {
                t.Fatalf("webhook %s: non-JSON response: %v (%s)", provider, err, truncate(raw))
        }
        return apiResponse{Status: resp.StatusCode, Body: &env}
}

// waitFor polls cond until it holds or the timeout elapses (worker pipelines —
// outbox → bus → audit/facts — are asynchronous by design, so journeys poll).
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() (bool, string)) {
        t.Helper()
        deadline := time.Now().Add(timeout)
        var last string
        for time.Now().Before(deadline) {
                if ok, why := cond(); ok {
                        return
                } else if why != "" {
                        last = why
                }
                time.Sleep(200 * time.Millisecond)
        }
        t.Fatalf("timed out after %s waiting for %s (%s)", timeout, what, last)
}

// countQuery is the audit-trail query helper (J3): the audit trail's read
// model is the audit_events table written by the worker's audit consumer —
// there is no HTTP read endpoint yet, so the journey queries the store
// directly (a typed audit read API is a separate work item, not a journey).
func countQuery(t *testing.T, query string, args ...any) int {
        t.Helper()
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        var n int
        if err := harness.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
                t.Fatalf("audit query: %v", err)
        }
        return n
}

// journeyTenantFor returns the dedicated tenant fixture for one journey.
// Journey tenant isolation is deliberate: each journey runs against its own
// tenant so analytics facts, audit rows and the one-outbound-leg-per-tenant
// simulator constraint (issue #90 — routed around, not fixed here) never
// interact across journeys.
func journeyTenantFor(t *testing.T, name string) *journeyTenant {
        t.Helper()
        tn, ok := harness.tenants[name]
        if !ok {
                t.Fatalf("no tenant seeded for journey %s", name)
        }
        return tn
}

func randHex(n int) string {
        b := make([]byte, n)
        _, _ = rand.Read(b)
        return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
        sum := sha256.Sum256([]byte(s))
        return hex.EncodeToString(sum[:])
}

func truncate(b []byte) string {
        s := string(b)
        if len(s) > 300 {
                return s[:300] + "…"
        }
        return s
}

func itoa(n int) string { return strconv.Itoa(n) }

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
