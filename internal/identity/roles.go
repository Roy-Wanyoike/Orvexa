// Package identity implements Orvexa's OIDC/OAuth2 identity plane ([O-29],
// issue #38): provider discovery, JWKS-backed RS256 token validation with
// kid-rotation caching, token→principal mapping (sub, tenant claim, roles)
// and capability-based RBAC enforcement layered additively over the existing
// API-key auth.
//
// Division of trust: the IdP owns IDENTITY (who the caller is — sub, iss,
// aud, exp). Orvexa owns AUTHORIZATION (what the caller may do — role
// bindings + capability catalog, migration 0012_rbac.sql). No cookies, no
// sessions: every request re-presents its Bearer token; short-lived caches
// (JWKS keys, role bindings) carry only non-secret derived data.
package identity

// Role is a tenant-scoped authorization role bound to an IdP subject.
type Role string

// The closed role set (migration 0012_rbac.sql CHECK constraint mirrors this).
const (
	RoleViewer     Role = "viewer"
	RoleAgent      Role = "agent"
	RoleSupervisor Role = "supervisor"
	RoleAdmin      Role = "admin"
)

// Capability is a fine-grained action class checked by RequireCapability.
type Capability string

// The closed capability set (capability_catalog in migration 0012_rbac.sql).
const (
	CapInteractionRead  Capability = "interaction.read"
	CapInteractionWrite Capability = "interaction.write"
	CapCaseRead         Capability = "case.read"
	CapCaseWrite        Capability = "case.write"
	CapWorkflowRun      Capability = "workflow.run"
	CapSearchRead       Capability = "search.read"
	CapAdminOrg         Capability = "admin.org"
)

// capabilityOrder is the canonical rendering order for capability lists.
var capabilityOrder = []Capability{
	CapInteractionRead, CapInteractionWrite,
	CapCaseRead, CapCaseWrite,
	CapWorkflowRun, CapSearchRead, CapAdminOrg,
}

// roleCapabilities is the role→capability catalog. It MUST stay in sync with
// the seeded capability_catalog rows in migrations/0012_rbac.sql and the
// matrix in docs/rbac.md (all three are asserted equal by tests).
var roleCapabilities = map[Role][]Capability{
	RoleViewer: {
		CapInteractionRead, CapCaseRead, CapSearchRead,
	},
	RoleAgent: {
		CapInteractionRead, CapInteractionWrite,
		CapCaseRead, CapCaseWrite, CapSearchRead,
	},
	RoleSupervisor: {
		CapInteractionRead, CapInteractionWrite,
		CapCaseRead, CapCaseWrite,
		CapWorkflowRun, CapSearchRead,
	},
	RoleAdmin: {
		CapInteractionRead, CapInteractionWrite,
		CapCaseRead, CapCaseWrite,
		CapWorkflowRun, CapSearchRead, CapAdminOrg,
	},
}

// ParseRole maps a raw role string onto the closed role set.
func ParseRole(s string) (Role, bool) {
	r := Role(s)
	_, ok := roleCapabilities[r]
	return r, ok
}

// Binding is the resolved authorization state of a subject in one tenant.
type Binding struct {
	Roles        []string // distinct roles, catalog order (viewer→admin)
	Capabilities []string // distinct union capability set, canonical order
}

// staticBinding resolves a token's roles claim against the embedded catalog.
// This is the NO-DATABASE posture (dev/test/verify-only deployments): the
// IdP-asserted roles claim is trusted for authorization. With a database
// configured, role_bindings rows are authoritative and the roles claim is
// ignored — see Service.capabilitiesFor and docs/rbac.md.
func staticBinding(roles []string) Binding {
	roleSet := make(map[string]bool, len(roles))
	for _, raw := range roles {
		if r, ok := ParseRole(raw); ok {
			roleSet[string(r)] = true
		}
	}
	return Binding{
		Roles:        orderedRoles(roleSet),
		Capabilities: capabilitiesForRoles(roleSet),
	}
}

// orderedRoles renders the role set in escalating privilege order.
func orderedRoles(set map[string]bool) []string {
	all := []Role{RoleViewer, RoleAgent, RoleSupervisor, RoleAdmin}
	out := make([]string, 0, len(set))
	for _, r := range all {
		if set[string(r)] {
			out = append(out, string(r))
		}
	}
	return out
}

// capabilitiesForRoles unions the catalog capability sets, canonical order.
func capabilitiesForRoles(set map[string]bool) []string {
	granted := make(map[Capability]bool)
	for raw := range set {
		for _, c := range roleCapabilities[Role(raw)] {
			granted[c] = true
		}
	}
	out := make([]string, 0, len(granted))
	for _, c := range capabilityOrder {
		if granted[c] {
			out = append(out, string(c))
		}
	}
	return out
}

// capabilitiesFromSet renders an explicit capability-name set in canonical
// catalog order. Used by the storage-backed resolver, whose query already
// joins capability_catalog and therefore yields capability names directly
// (not roles) — feeding such a set to capabilitiesForRoles would look each
// capability name up as a ROLE and always resolve empty.
func capabilitiesFromSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for _, c := range capabilityOrder {
		if set[string(c)] {
			out = append(out, string(c))
		}
	}
	return out
}
