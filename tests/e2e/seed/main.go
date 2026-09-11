//go:build e2e

// Command seed provisions the stack data the inline e2e demo loop needs:
// an organization (once), a fresh tenant, an 'api'-scoped API key and an
// active AI agent (get_customer + create_case allowlisted). It is the bash
// counterpart of the journey harness's seeding (tests/e2e harness_test.go)
// used by scripts/e2e-demo.sh --loop.
//
// Usage:
//
//	go run -tags=e2e ./tests/e2e/seed <postgres-url> [label]
//
// Prints one line: <tenant_id> <raw_key> <agent_id>
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: seed <postgres-url> [label]")
		os.Exit(2)
	}
	url := os.Args[1]
	label := "loop"
	if len(os.Args) > 2 {
		label = os.Args[2]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, url)
	must(err, "connect")
	defer conn.Close(ctx)

	var orgID string
	err = conn.QueryRow(ctx, `SELECT id FROM organizations WHERE slug = 'e2e-org'`).Scan(&orgID)
	if err != nil {
		must(conn.QueryRow(ctx, `INSERT INTO organizations (id, name, slug)
                        VALUES (gen_random_uuid(), 'E2E Org', 'e2e-org') RETURNING id`).Scan(&orgID), "insert org")
	}

	stamp := time.Now().UTC().Format("20060102T150405")
	tenantName := "E2E " + label + " " + stamp
	var tenantID string
	must(conn.QueryRow(ctx, `INSERT INTO tenants (id, organization_id, name)
                VALUES (gen_random_uuid(), $1, $2) RETURNING id`, orgID, tenantName).Scan(&tenantID), "insert tenant")

	rawKey := "orvx_e2e_" + randHex(24)
	sum := sha256.Sum256([]byte(rawKey))
	_, err = conn.Exec(ctx, `INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes)
                VALUES (gen_random_uuid(), $1, 'journey', $2, '{api}')`,
		tenantID, hex.EncodeToString(sum[:]))
	must(err, "insert api key")

	var agentID string
	must(conn.QueryRow(ctx, `INSERT INTO ai_agents (id, tenant_id, name)
                VALUES (gen_random_uuid(), $1, 'support-copilot') RETURNING id`, tenantID).Scan(&agentID), "insert ai agent")
	_, err = conn.Exec(ctx, `INSERT INTO ai_agent_versions (id, agent_id, version, system_prompt)
                VALUES (gen_random_uuid(), $1, 1, 'You are Orvexa support copilot.')`, agentID)
	must(err, "insert ai agent version")
	for _, tool := range []struct {
		name string
		max  int
	}{{"get_customer", 5}, {"create_case", 2}} {
		_, err = conn.Exec(ctx, `INSERT INTO ai_agent_tools (agent_id, tool_name, allowed, max_calls_per_invocation)
                        VALUES ($1, $2, true, $3)`, agentID, tool.name, tool.max)
		must(err, "insert tool policy")
	}

	fmt.Printf("%s %s %s\n", tenantID, rawKey, agentID)
}

func must(err error, what string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "seed: %s: %v\n", what, err)
		os.Exit(1)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
