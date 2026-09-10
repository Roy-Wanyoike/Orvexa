# Changelog

All notable changes to Orvexa are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (while in 0.x, minor releases may carry breaking changes as the API surface stabilizes).

## [Unreleased]

## [0.10.0] — 2026-09-10

First tagged release. It consolidates the nine foundation waves that stood up the platform (repository bootstrap + issues #1–#9, merged as PRs #10–#19) and the production push that made it operable against real providers, infrastructure and identity — plus the release engineering this wave adds ([O-37], issue #46). Everything below is traceable to `git log`.

### Added — platform foundation (waves 1–9)

- Foundation: Go module and shared kernels (config, typed errors, structured logging, pagination, event envelopes, idempotency), the three deployables (`cmd/api`, `cmd/worker`, `cmd/realtime`), the control-plane schema (organizations, tenants, hashed API keys, append-only audit) and CI (#11).
- Domain core: customers, conversations, interactions, agents, queues and cases with the REST v1 surface and API-key tenant authentication (#12).
- Events: webhook ingestion gateway, transactional outbox dispatcher wiring, in-process event bus and the audit consumer (#13).
- Communications: provider-agnostic telephony + messaging ports, the signed-webhook simulator and the delivery lifecycle processor (#14).
- Routing: deterministic decision engine, recorded decisions and the agent presence store (#15).
- Realtime: authenticated WebSocket hub with tenant isolation, agent filtering and connection caps (#16).
- AI: governed AI gateway, agent runtime and the tool-gateway security boundary (#17).
- Execution: durable workflow engine (callback + collections) and the analytics facts pipeline (#18).
- Release-gate documentation: OpenAPI v1 contract, architecture/domains docs, the operations runbook, the E2E demo script and the QA report (#19).

### Added — production push

- Communications adapters (each behind the conformance kit or wire-level tests): Twilio SMS (#64), WhatsApp Cloud API (#65), FreeSWITCH ESL (#66), Asterisk AMI (#67), Africa's Talking SMS + USSD (#68), Africa's Talking Voice (#69) and Twilio Voice (#70); the provider factory now wires registry → adapters → `cmd/api` (#71), on top of the provider conformance test-kit (#60) and the provider registry with redacted credential configuration (#61).
- Infrastructure drivers: NATS JetStream bus driver behind the `Bus` abstraction (#72), Redis presence cache + distributed rate-limit drivers (#75), ClickHouse facts writer (batched, non-blocking, env-gated) (#76), OpenSearch conversation indexer + tenant-scoped search API (#74) and the Temporal-backed workflow driver behind the engine interfaces (build-tagged) (#73); plus the portable PostgreSQL devstack with the one-command integration suite (#62).
- Identity: OIDC/OAuth2 login with capability-based RBAC enforcement (#77).
- Security: real-provider webhook signature verification for Twilio, WhatsApp Cloud and Africa's Talking (#63).
- Wiring: search routes mounted and the analytics facts sink selected by environment ([O-27]/[O-26]) (#80).
- Dependencies pre-provisioned for the production waves (NATS, Temporal, ClickHouse, Redis, JWT, OIDC, OAuth2, YAML) (#53).
- Release engineering (this wave): build stamping via `-ldflags -X` into `internal/platform/buildinfo`, surfaced additively on `/healthz` and `/readyz`; `make release` builds all three stamped binaries plus the SBOM into `dist/`; `scripts/sbom.sh` emits a deterministic CycloneDX 1.5 SBOM from `go.mod` (python3 stdlib only, self-validating); a Keep-a-Changelog structure guard (`make changelog-check`).

### Changed

- Documentation refresh: architecture/domains/runbook docs synced to shipped reality, ADR-0007 (multi-region readiness) and ADR-0008 (OIDC identity + capability RBAC) added (#83); repository hygiene audit with marker census, env completeness and drift inventory (#58).
- Contract safety: OpenAPI ⇄ router bidirectional conformance suite now guards against spec drift (#86).

### Fixed

- Vet-clean Temporal integration test under combined build tags (#78) and merge-marker regressions in compose + the Temporal integration test (hotfix) (#79).
- Release-gate fixes: dependency bumps, replay contract, CI pin and dead-knob cleanup (#87).

### Security

- Independent security sweep: secret scanning, govulncheck, security headers and the rate-limit matrix (#85).
- Webhook ingress remains fail-closed — unsigned or tampered bodies are rejected and never persisted as processed (#63).

[Unreleased]: https://github.com/Roy-Wanyoike/Orvexa/compare/v0.10.0...HEAD
[0.10.0]: https://github.com/Roy-Wanyoike/Orvexa/releases/tag/v0.10.0
