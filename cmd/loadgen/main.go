// Command loadgen is Orvexa's self-contained HTTP load generator (issue #42).
// It is stdlib-only, cross-builds with the module, and produces structured
// JSON evidence for docs/slo.md.
//
// Scenarios (selected via -scenarios, preset "all" = the four below, issued
// round-robin with equal weight):
//
//	webhook          POST /api/v1/webhooks/whatsapp_cloud — WhatsApp-shaped
//	                 ProviderEvent ({"event":"message.delivered", ...}) with the
//	                 platform signature header: X-Orvexa-Signature =
//	                 hex(HMAC-SHA256(secret, rawBody)) — the exact scheme
//	                 scripts/e2e-demo.sh and internal/webhooks enforce. Each
//	                 body carries a unique nonce (idempotency is derived from
//	                 the raw body), so every ingest takes the fresh-event path.
//	                 Events reference pre-seeded ACTIVE interactions, so the
//	                 processor applies the documented same-state no-op
//	                 (carrier-replay semantics) after the ledger insert.
//	interaction      POST /api/v1/interactions — interaction create against
//	                 pre-seeded customers (conversation find-or-open reused).
//	customer-create  POST /api/v1/customers — unique phone per request.
//	customer-list    GET  /api/v1/customers?limit=50.
//
// Rate model: stepped ramp (-ramp "50,100,200", -step 60s per step) issued
// open-loop at each step's fixed rate. Latency is client-observed (loopback
// request-to-response) and reported per step per scenario as p50/p95/p99 +
// min/max/mean, alongside per-status-code counts and an error rate (every
// non-2xx is an error for SLO purposes; 429s are additionally broken out).
//
// Limiter-aware traffic shaping (both documented, both honest):
//
//   - Authenticated routes are limited per API key (600/min, burst 120 at the
//     shipped wiring). -api-keys accepts the bootstrap key set; requests
//     round-robin keys so per-key load stays under the sustained cap.
//   - Webhook ingress is limited per source IP (120/min, burst 60). Real
//     carriers deliver from many addresses; -source-ips N spreads webhook
//     connections over 127.0.0.1..127.0.0.N-1 (loopback /8) so the measured
//     path is the ingest handler, not the deny path. Each address is probed
//     first; addresses that cannot bind are dropped, and if none bind the run
//     proceeds single-source with the rotation state recorded in the report.
//
// Graceful stop: SIGINT/SIGTERM stops issuing immediately, waits for in-flight
// requests, and flushes the (marked interrupted) report — partial evidence is
// still valid evidence.
//
// Usage (baselines):
//
//	loadgen -target http://127.0.0.1:18080 -api-keys "orvx_k0,orvx_k1,..." \
//	  -webhook-secret "$SECRET" -ramp "50,100,200" -step 60s \
//	  -output docs/perf/baseline.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	var (
		target      = flag.String("target", "http://127.0.0.1:8080", "base URL of the Orvexa API")
		apiKeysFlag = flag.String("api-keys", "", "comma-separated bootstrap API keys (rotated per request)")
		apiKeysFile = flag.String("api-keys-file", "", "file with one API key per line (overrides -api-keys)")
		secret      = flag.String("webhook-secret", "", "ORVEXA_WEBHOOK_HMAC_SECRET value (env fallback)")
		scenarios   = flag.String("scenarios", "all", "comma list: all|webhook|interaction|customer-create|customer-list")
		rampFlag    = flag.String("ramp", "50,100,200", "comma list of target requests/second, one per step")
		step        = flag.Duration("step", 60*time.Second, "duration of each ramp step")
		warmup      = flag.Duration("warmup", 0*time.Second, "unmeasured warmup issued at the first step's rate")
		timeout     = flag.Duration("timeout", 2*time.Second, "per-request timeout")
		maxInflight = flag.Int("max-inflight", 2048, "cap on concurrent in-flight requests")
		sourceIPs   = flag.Int("source-ips", 64, "loopback source addresses (127.0.0.1..N) for webhook ingress rotation")
		pool        = flag.Int("pool", 64, "customers/interactions seeded before the run (webhook targets + conversation spread)")
		output      = flag.String("output", "", "write the JSON report to this path (stdout summary always printed)")
		seedTimeout = flag.Duration("seed-timeout", 60*time.Second, "budget for the seeding phase")
	)
	flag.Parse()

	report, err := run(*target, *apiKeysFlag, *apiKeysFile, *secret, *scenarios, *rampFlag,
		*step, *warmup, *timeout, *maxInflight, *sourceIPs, *pool, *output, *seedTimeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
	printSummary(os.Stdout, report)
}

// run executes the whole campaign and returns the report (also written to
// outputPath when non-empty).
func run(target, apiKeysFlag, apiKeysFile, secret, scenariosFlag, rampFlag string,
	step, warmup, timeout time.Duration, maxInflight, sourceIPs, pool int,
	outputPath string, seedTimeout time.Duration) (*Report, error) {

	if secret == "" {
		secret = os.Getenv("ORVEXA_WEBHOOK_HMAC_SECRET")
	}
	keys, err := loadKeys(apiKeysFlag, apiKeysFile)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no API keys: pass -api-keys or -api-keys-file (bootstrap SQL is documented in docs/runbooks/operations.md)")
	}
	if secret == "" {
		return nil, fmt.Errorf("no webhook secret: pass -webhook-secret or set ORVEXA_WEBHOOK_HMAC_SECRET (empty secrets fail closed server-side)")
	}
	names, err := parseScenarios(scenariosFlag)
	if err != nil {
		return nil, err
	}
	ramp, err := parseRamp(rampFlag)
	if err != nil {
		return nil, err
	}
	if step <= 0 || pool < 1 || maxInflight < 1 || sourceIPs < 1 || timeout <= 0 {
		return nil, fmt.Errorf("invalid flags: step/pool/max-inflight/source-ips/timeout must be positive")
	}

	client, err := NewClient(target, keys, timeout, sourceIPs)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now().UTC()

	work, err := seed(ctx, client, secret, names, pool, seedTimeout)
	if err != nil {
		return nil, fmt.Errorf("seeding failed (is the server up and the keys valid?): %w", err)
	}

	report := &Report{
		Meta: Meta{
			Target:          target,
			StartedAt:       started.Format(time.RFC3339Nano),
			RampRPS:         ramp,
			StepSeconds:     step.Seconds(),
			WarmupSeconds:   warmup.Seconds(),
			Scenarios:       names,
			APIKeys:         len(keys),
			SourceIPsProbed: client.ProbedSourceIPs(),
			SourceIPsUsed:   client.ActiveSourceIPs(),
			Seeded:          SeedSummary{Customers: work.CustomerCount(), Interactions: len(work.interactions)},
			GoVersion:       runtime.Version(),
			GOMAXPROCS:      runtime.GOMAXPROCS(0),
			NumCPU:          runtime.NumCPU(),
			OS:              runtime.GOOS,
			Arch:            runtime.GOARCH,
		},
	}
	if client.SourceIPRotationDegraded() {
		report.Meta.SourceIPsNote = "loopback source rotation unavailable; webhook ingress ran from a single source (the per-IP limiter will dominate this scenario)"
	}

	runner := newRunner(client, work, names, maxInflight)

	if warmup > 0 {
		// Unmeasured: warms connections, JIT-ish caches and the limiter's
		// steady state. The result is discarded, only failures abort.
		if _, werr := runner.step(ctx, 0, ramp[0], warmup, true); werr != nil {
			return nil, werr
		}
	}

	for i, rps := range ramp {
		if ctx.Err() != nil {
			break
		}
		r, err := runner.step(ctx, i+1, rps, step, false)
		if err != nil {
			return nil, err
		}
		report.Steps = append(report.Steps, r)
		fmt.Printf("step %d @ %.0f rps: issued=%d ok=%d err=%d (p50=%.2fms p95=%.2fms p99=%.2fms)\n",
			i+1, rps, r.Total.Issued, r.Total.OK, r.Total.Errors,
			r.Total.LatencyMS.P50, r.Total.LatencyMS.P95, r.Total.LatencyMS.P99)
	}

	report.Meta.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	report.Meta.Interrupted = ctx.Err() != nil

	if outputPath != "" {
		if err := writeJSONFile(outputPath, report); err != nil {
			return nil, err
		}
	}
	return report, nil
}

func loadKeys(flagVal, fileVal string) ([]string, error) {
	if fileVal != "" {
		raw, err := os.ReadFile(fileVal)
		if err != nil {
			return nil, err
		}
		var keys []string
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				keys = append(keys, line)
			}
		}
		return keys, nil
	}
	var keys []string
	for _, k := range strings.Split(flagVal, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func parseScenarios(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch part {
		case "":
			continue
		case "all":
			return []string{scnWebhook, scnInteraction, scnCustomerCreate, scnCustomerList}, nil
		case scnWebhook, scnInteraction, scnCustomerCreate, scnCustomerList:
			out = append(out, part)
		default:
			return nil, fmt.Errorf("unknown scenario %q (want all|webhook|interaction|customer-create|customer-list)", part)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no scenarios selected")
	}
	return out, nil
}

func parseRamp(s string) ([]float64, error) {
	var out []float64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var rps float64
		if _, err := fmt.Sscanf(part, "%g", &rps); err != nil {
			return nil, fmt.Errorf("bad ramp entry %q: %w", part, err)
		}
		if rps <= 0 || rps > 100_000 {
			return nil, fmt.Errorf("ramp entries must be in (0, 100000], got %g", rps)
		}
		out = append(out, rps)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty ramp")
	}
	return out, nil
}

func writeJSONFile(path string, report *Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
