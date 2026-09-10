# OIDC Identity & Capability RBAC

Contract reference for the identity plane (`internal/identity`, migration `0012_rbac.sql`, issue #38). The code comments in the package, the migration seeds and the matrix in this file are three renderings of the same contract — the identity package tests assert them equal, so this document cannot silently drift.

Division of trust: **the IdP owns identity** (who the caller is — `sub`, `iss`, `aud`, `exp`); **Orvexa owns authorization** (what the caller may do — role bindings + capability catalog). No cookies, no sessions: every request re-presents its Bearer token; the only caches (JWKS keys, role bindings) hold non-secret derived data behind short TTLs.

The plane is additive: when the OIDC environment is absent, the API-key path is byte-identical to the pre-#38 stack. Decision record: [ADR-0008](adr/0008-oidc-identity-capability-rbac.md).

## Configuration

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `ORVEXA_OIDC_ISSUER` | yes | — | IdP discovery root (e.g. `https://idp.example.com`). Discovery fetches `/.well-known/openid-configuration`; the document's `issuer` MUST equal this value (OIDC Discovery §4.3 issuer binding) |
| `ORVEXA_OIDC_CLIENT_ID` | yes | — | Confidential client id; tokens' `aud` must contain it. Also keys the state HMAC material |
| `ORVEXA_OIDC_CLIENT_SECRET` | yes | — | Confidential-client secret for the code exchange; never logged, never echoed |
| `ORVEXA_OIDC_REDIRECT_URL` | no | derived | Fixed callback override; when unset the callback is derived from the request host (`<scheme>://<host>/api/v1/identity/callback`) — safe because the IdP enforces its registered-redirect allowlist |
| `ORVEXA_OIDC_SCOPES` | no | `openid` | Comma-separated authorization scopes requested at the authorize endpoint |

All three core variables must be set for OIDC to be enabled; when any is missing the identity plane is disabled (login endpoints are not mounted) and the API-key path is unchanged. Constants: state TTL 10m, state nonce 32 random bytes, clock leeway 30s, JWKS cache TTL 10m, JWKS min refresh floor 2s, binding-cache TTL 30s bounded at 4096 (tenant, subject) entries.

## Login flow (stateless)

1. `GET /api/v1/identity/authorize-url` → `{authorize_url, state, nonce, expires_in, redirect_uri}`. The state is `<b64url(payload)>.<b64url(HMAC-SHA256)>` with payload `<nonce>|<expiry>`; the HMAC key is derived from the client secret, so a state cannot be forged, replayed after 10 minutes, or reused across deployments.
2. The caller redirects the browser to `authorize_url`; the IdP authenticates the user and returns `?code=&state=`.
3. `GET /api/v1/identity/callback` verifies the state MAC and freshness, exchanges the code, verifies the returned ID token against the same §token contract, and returns **tenant-scoped principal info only** — no access/refresh/ID tokens are echoed, so responses are safe to log and carry no replayable credentials.

Public endpoints (mounted outside the authenticated tree); disabled deployments answer 404 `identity.disabled`.

## Token contract

A Bearer token is considered an OIDC credential only when it is **JWT-shaped** (three non-empty base64url segments). Acceptance requirements — every rejection is the typed 401 `identity.invalid_token` with a coarse `reason`:

- `alg` is **RS256 only** — checked explicitly before any crypto, rejecting `alg:none` and the HS256-confusion family (`parser.WithValidMethods` stays as defense in depth).
- `kid` REQUIRED and resolvable through the §JWKS cache.
- `iss` == configured issuer; `aud` contains the configured client id; `exp` REQUIRED and in the future (30s leeway applies; `nbf`/`iat` validated by the parser).
- `sub` REQUIRED and non-empty. It is the authorization principal key (`role_bindings.principal`) — never an email or mutable username.
- Tenant: the `tenant` claim (alias `tenant_id`) carries the caller's tenant UUID. The tenant ALWAYS comes from the verified token — never from request bodies or headers.

## JWKS

`jwks_uri` comes from discovery. The cache is `kid`-keyed with these properties:

- **Rotation on miss:** an unknown `kid` triggers one throttled re-fetch, so key rotation is picked up within milliseconds without polling.
- **TTL refresh:** at TTL age (10m default) the cache re-fetches lazily on the next lookup; a rotated-out `kid` fails closed instead of lingering.
- **Amplifier floor:** a minimum interval (2s default) between fetches — a flood of unknown-`kid` tokens cannot turn the verifier into a key-endpoint DDoS amplifier.
- **Bounded memory:** one validated `*rsa.PublicKey` per `kid`, replaced wholesale on each successful fetch.

## Capability catalog

Closed role set (mirrored by the `CHECK` constraint on `role_bindings.role`) and the role → capability matrix (mirrored by the `capability_catalog` seeds — escalating privilege, `viewer ⊂ agent ⊂ supervisor ⊂ admin`):

| Capability | `viewer` | `agent` | `supervisor` | `admin` | Gates |
|---|---|---|---|---|---|
| `interaction.read` | ✓ | ✓ | ✓ | ✓ | GET/HEAD/OPTIONS on conversations + interactions |
| `case.read` | ✓ | ✓ | ✓ | ✓ | GET/HEAD/OPTIONS on cases |
| `search.read` | ✓ | ✓ | ✓ | ✓ | (catalog capability for the search read model) |
| `interaction.write` | | ✓ | ✓ | ✓ | non-GET methods on conversations + interactions |
| `case.write` | | ✓ | ✓ | ✓ | non-GET methods on cases |
| `workflow.run` | | | ✓ | ✓ | workflow start/cancel/tick routes |
| `admin.org` | | | | ✓ | organization administration (reserved for control-plane use) |

- Enforcement: `identity.RequireCapability(cap)` and `identity.RequireMethodCapability(read, write)` middleware in the v1 route tree; denials are 403 `identity.capability_missing` with `details.required_capability` naming the capability.
- **API-key compatibility:** API-key principals remain governed by their minted scope set. Capability-shaped scopes pass; the legacy blanket scopes `api` and `*` keep historical keys working byte-identically. A forged JWT is never retried as an API key (branch selection happens before any API-key code runs).
- **Resolution:** with a database pool wired, `role_bindings ⋈ capability_catalog` rows are authoritative behind the short-TTL resolver — revoking a binding (`status='revoked'`) removes capabilities within the TTL even while the caller's token is still cryptographically valid. Without a database, the IdP-asserted `roles` claim maps through the embedded catalog (static posture; the boot log says so). An unknown `(tenant, subject)` pair resolves to an **empty** binding — no binding, no capabilities, 403.
- **Binding administration is a control-plane concern** (SQL today; no runtime write API ships with the identity plane).

## Error taxonomy

Stable machine codes; no error ever carries token material, signatures or claim payloads — reasons are coarse words so clients can distinguish failure classes without learning which byte failed.

| Code | HTTP | Meaning | `details.reason` values |
|---|---|---|---|
| `identity.invalid_token` | 401 | Token rejected by §token contract | `malformed`, `algorithm`, `key`, `expired`, `not_yet_valid`, `issuer`, `audience`, `signature`, `claims`, `unverifiable` |
| `identity.state_invalid` | 401 | Login state failed MAC or freshness check | — |
| `identity.exchange_failed` | 401 | Authorization-code exchange failed | — |
| `identity.id_token_missing` | 401 | IdP returned no ID token | — |
| `identity.request_invalid` | 422 | Callback missing `code`/`state` | — |
| `identity.capability_missing` | 403 | Principal lacks the required capability | — (capability named in `details.required_capability`) |
| `identity.rate_limited` | 429 | Per-subject rate limit exceeded (`Retry-After` set, rounded up) | — |
| `identity.disabled` | 404 | OIDC not configured on this deployment | — |
| `identity.discovery_failed` | 500 | IdP discovery failed (retried on later requests; an unreachable IdP must not poison the process forever) | — |
| `identity.binding_lookup_failed` | 500 | Authorization storage lookup failed | — |

## Forgery matrix

`TestVerifyForgeryMatrix` asserts every attack below lands on 401 `identity.invalid_token` with the listed reason (and that the valid control token verifies):

| Attack | Reason |
|---|---|
| `alg: none` | `algorithm` |
| HS256 signed with an attacker secret | `algorithm` |
| HS256 signed with the RSA public key DER (classic RS256-confusion) | `algorithm` |
| Wrong `iss` / wrong `aud` | `issuer` / `audience` |
| Expired / `nbf` in the future / missing `exp` | `expired` / `not_yet_valid` / `unverifiable` |
| Missing `kid` / unknown `kid` | `key` / `unverifiable` |
| Tampered signature (first signature byte flipped) | `signature` |
| Empty `sub` | `claims` |
| Garbage string / empty string | `malformed` |

Companion suites: DualAuth branch selection (non-JWT credentials take the API-key path; forged JWTs never fall back) and the capability API-key compatibility matrix live in `internal/identity/middleware_test.go`; the Go ↔ SQL catalog parity is enforced in `internal/identity/roles_test.go`.
