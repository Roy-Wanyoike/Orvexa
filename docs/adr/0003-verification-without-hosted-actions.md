# ADR-0003 — Verification strategy without GitHub Actions

- **Status:** Accepted
- **Date:** 2026-09-10

## Context

The repository's CI workflow (`.github/workflows/ci.yml`) defines build + vet + `go test -race` + migration sanity gates. On the current owner account, GitHub Actions runs fail at startup (account-level Actions availability), independently of workflow correctness. This matches the owner's established verification practice: verification is performed locally and recorded as evidence.

## Problem

The engineering protocol requires every PR to pass CI before merge. With hosted Actions unavailable, what is the authoritative verification gate, and how is it evidenced?

## Decision

1. **Authoritative gate = full local verification matrix**, run for every PR before merge and recorded in `qa/QA_REPORT.md`:
   - `go build ./...` — compiles
   - `go vet ./...` — static analysis
   - `go test -race ./...` — unit + integration tests under the race detector
   - `bash scripts/e2e-demo.sh` — end-to-end platform scenario (release-gate wave)
   - migration sanity: ordered, non-empty, idempotent-safe SQL
2. `.github/workflows/ci.yml` remains in the repository and becomes the enforcement gate automatically if/when Actions becomes available on the account; no workflow changes will be needed.
3. PR bodies carry the verification transcript; the QA report aggregates them.

## Alternatives considered

- Blocking all merges on hosted CI: would freeze delivery on an account-level infrastructure issue outside the repository's control.
- Dropping the workflow file: loses the ready-to-arm CI definition; rejected.

## Consequences

- Verification discipline moves into the merge checklist; the QA report is the audit artifact.
- GitGuardian (already active on push) continues to provide secret scanning independently of Actions.

## Security implications

Local gates include secret-hygiene checks (`grep` sweep in release gate) and the dependency surface is intentionally minimal (chi, pgx, uuid only).

## Operational implications

If Actions later activates, required checks should be set to the existing job (`build-test`) with no workflow edits.
