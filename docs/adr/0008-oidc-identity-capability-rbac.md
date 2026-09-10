# ADR-0008 — OIDC identity with capability-based RBAC

- **Status:** Accepted
- **Date:** 2026-09-11

## Context

Waves 1–8 shipped machine authentication: hashed API keys with minted scopes, tenant always derived from the key, per-key rate limits. That contract is right for integrations and wrong for workforces: contact-center staff are people with IdP accounts, joiners/movers/leavers happen daily, and static per-person secrets are an audit liability. Workforce identity (issue #38, roadmap [#10](https://github.com/Roy-Wanyoike/Orvexa/issues/10)) needs a decision before the UI grew around the API-key model.

Two sub-decisions were entangled and are separated here: (a) how callers prove identity, and (b) who decides what an authenticated caller may do.

## Problem

Add workforce identity without destabilizing the API-key contract, and choose an authorization model that survives role changes, revocations, and multi-tenant scoping — with zero unverifiable claims (the [ADR-0003](0003-verification-without-hosted-actions.md) gate applies to this decision like any other).

## Decision

1. **The IdP owns identity; Orvexa owns authorization.** A verified token asserts who the caller is (`sub`, `iss`, `aud`, `exp`). What the caller may do is decided by Orvexa-owned data: `role_bindings` rows joined to a closed `capability_catalog` (migration `0012_rbac.sql`). The full contract is rendered in [docs/rbac.md](../rbac.md) — code comments, migration seeds, and the doc are three renderings kept equal by tests.
2. **OIDC/OAuth2 authorization-code flow for people; RS256 verification for API calls.** Discovery with issuer binding, JWKS-cached public keys with kid rotation, `exp` required with leeway. No cookies, no sessions: every request re-presents its Bearer token, keeping the API stateless and horizontally scalable.
3. **Capability RBAC over raw permission claims.** A closed role set (`viewer ⊂ agent ⊂ supervisor ⊂ admin`) maps to a closed capability set (`interaction.read/write`, `case.read/write`, `workflow.run`, `search.read`, `admin.org`). Capabilities — not the IdP's opinion — are the enforcement currency, enforced by `RequireCapability`/`RequireMethodCapability` middleware on the interaction, case, and workflow route mounts.
4. **Database-backed bindings are authoritative; the token's `roles` claim is only a fallback.** With a pool wired, bindings resolve per `(tenant, subject)` behind a short-TTL bounded cache: revocation takes effect within the TTL even while a token is cryptographically valid. Without a database (dev/verify posture), the static catalog maps the IdP-asserted roles claim and the boot log says so. An unknown `(tenant, subject)` resolves to an **empty** binding — fail closed.
5. **Additive deployment, branch on credential shape.** The identity plane exists only when `ORVEXA_OIDC_ISSUER/CLIENT_ID/CLIENT_SECRET` are all configured; otherwise the stack is byte-identical API-key-only. In dual-auth mode, JWT-shaped Bearer credentials take the OIDC path and every other credential shape is delegated to the unchanged API-key middleware. A forged JWT is rejected 401 and is **never retried as an API key**. Legacy blanket scopes (`api`, `*`) keep historical keys passing capability gates unchanged.
6. **Tenant scope comes only from verified claims or the hashed key** — never from request bodies — and bindings are resolved per `(tenant, subject)`, so a cross-tenant claim cannot buy capabilities.
7. **Binding administration is a control-plane concern** (SQL today); no runtime write API ships with the identity plane, deliberately.

## Alternatives considered

- **IdP-embedded permissions (JWT claims carry capabilities):** revocation latency = token lifetime, and authorization authority leaks outside Orvexa. Rejected — this is why decision 4 keeps the database authoritative.
- **Extend API-key scopes to humans:** static secrets with no rotation story for people, no SSO, no joiner/mover/leaver flow. Rejected for humans; kept for machines.
- **Sessions/cookies after login:** stateful edge, CSRF surface, and a second auth truth. Rejected; bearer-per-request keeps one model for both credential shapes.
- **`go-oidc` for discovery/verification:** its transitive `go-jose` checksums were unavailable under the wave's no-`go.mod`-edit rule, so discovery is implemented directly over HTTP with issuer validation. Noted honestly as a seam: the library can replace the thin reader later without API change.
- **Per-tenant custom roles from day one:** maximal flexibility, unverifiable matrix, and no evidence any deployment needs it; the closed catalog is extensible by ADR, not by runtime config. Rejected for this wave.

## Consequences

- Every authenticated request on the OIDC path pays verification + binding resolution; both are cached behind bounded TTLs (JWKS 10m, bindings 30s/4096 entries), so the cost is bounded and measurable.
- The `roles` claim is trusted only in the static posture; production deployments with a database get revocation semantics instead of token-lifetime trust.
- Machine and human callers share one route tree, one error envelope, one rate-limit model — but two credential lifecycles, documented separately ([docs/rbac.md](../rbac.md), operations runbook).
- Role/capability changes now require a migration plus test updates in three synced places (code, SQL seeds, docs) — deliberate friction that keeps the matrix honest.

## Security implications

- RS256-only, with the algorithm allowlist checked **before** any cryptographic operation (`alg:none` and HS256-confusion die on a coarse reason); issuer binding prevents discovery substitution; the JWKS cache has a fetch floor so unknown-`kid` floods cannot amplify into the IdP.
- Login state is MAC-protected and short-lived (10m); the callback echoes principal info only — no access/refresh/ID tokens are returned to callers, so responses carry no replayable credentials.
- The error taxonomy is coarse by design (`docs/rbac.md` §error taxonomy): clients learn failure classes, never which byte failed; no token material, signatures, or claim payloads appear in errors or logs.
- The forgery matrix (15 attacks from `alg:none` to tampered signatures) is asserted in tests, not described in prose.

## Operational implications

- Configuration is five `ORVEXA_OIDC_*` variables (three required); disabled is the zero-config default and is logged as such.
- Bindings are SQL today (runbook: First tenant bootstrap); revocation is an `UPDATE` whose effect lands within the 30s TTL.
- Operator-visible degradation: an unreachable IdP fails discovery per-request (never poisoning the process) while cached JWKS material keeps already-issued tokens verifying within TTL.
