package identity

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The catalog is a THREE-WAY contract: internal/identity/roles.go (this
// test), the capability_catalog seeds in migrations/0012_rbac.sql, and the
// matrix table in docs/rbac.md. This file enforces the Go↔SQL half; the
// docs half is asserted in rbac_doc_test.go (docs land in the same PR).

func TestParseRoleClosedSet(t *testing.T) {
	for _, r := range []Role{RoleViewer, RoleAgent, RoleSupervisor, RoleAdmin} {
		if got, ok := ParseRole(string(r)); !ok || got != r {
			t.Fatalf("ParseRole(%q) = %q, %v", r, got, ok)
		}
	}
	for _, bad := range []string{"", "root", "ADMIN", "superuser", "viewer "} {
		if _, ok := ParseRole(bad); ok {
			t.Fatalf("ParseRole(%q) must be rejected", bad)
		}
	}
}

func TestCapabilityCatalogIsClosedAndOrdered(t *testing.T) {
	if len(capabilityOrder) != 7 {
		t.Fatalf("capability catalog must hold exactly the 7 documented capabilities, got %d", len(capabilityOrder))
	}
	seen := map[Capability]bool{}
	for _, c := range capabilityOrder {
		if seen[c] {
			t.Fatalf("duplicate capability %q in canonical order", c)
		}
		seen[c] = true
	}
	for role, caps := range roleCapabilities {
		for _, c := range caps {
			if !seen[c] {
				t.Fatalf("role %q grants %q which is not in the canonical catalog", role, c)
			}
		}
	}
}

func TestStaticBindingUnionAndCanonicalOrder(t *testing.T) {
	b := staticBinding([]string{"supervisor", "viewer", "bogus-role", "supervisor"})
	wantRoles := "viewer,supervisor"
	if got := strings.Join(b.Roles, ","); got != wantRoles {
		t.Fatalf("roles = %q, want %q (unknown dropped, dedup, escalating order)", got, wantRoles)
	}
	wantCaps := strings.Join([]string{
		string(CapInteractionRead), string(CapInteractionWrite),
		string(CapCaseRead), string(CapCaseWrite),
		string(CapWorkflowRun), string(CapSearchRead),
	}, ",")
	if got := strings.Join(b.Capabilities, ","); got != wantCaps {
		t.Fatalf("capabilities = %q, want %q", got, wantCaps)
	}
	if b := staticBinding([]string{"nope"}); len(b.Capabilities) != 0 || len(b.Roles) != 0 {
		t.Fatalf("no known role ⇒ empty binding, got %+v", b)
	}
}

func TestCapabilitiesFromSetCanonicalOrder(t *testing.T) {
	set := map[string]bool{
		string(CapAdminOrg): true, string(CapInteractionRead): true,
		string(CapWorkflowRun): true, "bogus.capability": true,
	}
	got := strings.Join(capabilitiesFromSet(set), ",")
	want := strings.Join([]string{string(CapInteractionRead), string(CapWorkflowRun), string(CapAdminOrg)}, ",")
	if got != want {
		t.Fatalf("capabilitiesFromSet = %q, want %q (unknown dropped, canonical order)", got, want)
	}
}

func TestRoleEscalationChain(t *testing.T) {
	// documented subset chain: viewer ⊂ agent ⊂ supervisor ⊂ admin
	roles := []Role{RoleViewer, RoleAgent, RoleSupervisor, RoleAdmin}
	for i := 0; i < len(roles)-1; i++ {
		higher := map[string]bool{}
		for _, c := range roleCapabilities[roles[i+1]] {
			higher[string(c)] = true
		}
		for _, c := range roleCapabilities[roles[i]] {
			if !higher[string(c)] {
				t.Fatalf("%q must be a subset of %q; %q missing", roles[i], roles[i+1], c)
			}
		}
	}
	// and each step must actually add something
	for i := 0; i < len(roles)-1; i++ {
		if len(roleCapabilities[roles[i]]) >= len(roleCapabilities[roles[i+1]]) {
			t.Fatalf("%q must grant strictly more than %q", roles[i+1], roles[i])
		}
	}
}

// TestCatalogSeedsMatchGoCatalog parses the INSERT block of
// migrations/0012_rbac.sql and asserts it seeds exactly the Go catalog.
func TestCatalogSeedsMatchGoCatalog(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0012_rbac.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sqlText := string(raw)
	start := strings.Index(sqlText, "INSERT INTO capability_catalog")
	if start < 0 {
		t.Fatal("capability_catalog seed INSERT not found in migration 0012")
	}
	end := strings.Index(sqlText[start:], "ON CONFLICT")
	if end < 0 {
		t.Fatal("seed INSERT must be idempotent (ON CONFLICT DO NOTHING)")
	}
	block := sqlText[start : start+end]

	tupleRe := regexp.MustCompile(`\('([a-z]+)',\s*'([a-z.]+)'\)`)
	matches := tupleRe.FindAllStringSubmatch(block, -1)
	if len(matches) == 0 {
		t.Fatal("no seed tuples parsed — migration format changed?")
	}
	sqlCatalog := map[string]map[string]bool{}
	for _, m := range matches {
		if sqlCatalog[m[1]] == nil {
			sqlCatalog[m[1]] = map[string]bool{}
		}
		sqlCatalog[m[1]][m[2]] = true
	}

	for role, caps := range roleCapabilities {
		want := map[string]bool{}
		for _, c := range caps {
			want[string(c)] = true
		}
		got := sqlCatalog[string(role)]
		if got == nil {
			t.Fatalf("role %q missing from SQL seeds", role)
		}
		for c := range want {
			if !got[c] {
				t.Fatalf("role %q: capability %q in Go catalog but missing from SQL seeds", role, c)
			}
		}
		for c := range got {
			if !want[c] {
				t.Fatalf("role %q: SQL seeds capability %q which the Go catalog does not grant", role, c)
			}
		}
		delete(sqlCatalog, string(role))
	}
	for role := range sqlCatalog {
		t.Fatalf("SQL seeds role %q which does not exist in the Go catalog", role)
	}
}

// TestMigrationDefinesRoleBindings pins the storage contract the DB resolver
// queries (internal/identity/roles_db.go): table + column names.
func TestMigrationDefinesRoleBindings(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0012_rbac.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sqlText := string(raw)
	for _, must := range []string{
		"CREATE TABLE IF NOT EXISTS role_bindings",
		"principal  TEXT NOT NULL",
		"role       TEXT NOT NULL",
		"CREATE TABLE IF NOT EXISTS capability_catalog",
		"UUID NOT NULL REFERENCES tenants(id)",
	} {
		if !strings.Contains(sqlText, must) {
			t.Fatalf("migration 0012 must define %q (roles_db.go query contract)", must)
		}
	}
}
