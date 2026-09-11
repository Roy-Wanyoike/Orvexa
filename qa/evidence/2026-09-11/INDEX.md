# Evidence pack index — 2026-09-11 (full-matrix verification run)

Captured during the issue [#48](https://github.com/Roy-Wanyoike/Orvexa/issues/48) full-matrix run and the
[#49](https://github.com/Roy-Wanyoike/Orvexa/issues/49)/[#51](https://github.com/Roy-Wanyoike/Orvexa/issues/51) re-verification waves, on `main` @ `283aed5`–`494d354`.
Every gate artifact is a captured command with an `exit_code` / `finished_utc` footer.
Machine context: shared 2 vCPU / 4 GiB sandbox, go1.27.1 linux/amd64; `host_psql_check` in every
footer points at the devstack gate (artifact 05).

| # | Artifact | Gate | Result |
|---|---|---|---|
| 01 | [01-gofmt.txt](01-gofmt.txt) | `gofmt -l .` | exit 0, no output |
| 02 | [02-vet.txt](02-vet.txt) | `go vet ./...` + tags `integration authz e2e temporal` | exit 0 (both parts) |
| 03 | [03-build.txt](03-build.txt) | `go build ./...` | exit 0 |
| 04 | [04-unit-race.txt](04-unit-race.txt) | `go test -race -count=1 ./...` | exit 0 — 35 packages ok |
| 05 | [05-devstack.txt](05-devstack.txt) | devstack up + migrations 0001–0012 | applied (0011 is ClickHouse-engine, skipped by PG runner) |
| 06 | [06-integration.txt](06-integration.txt) | `-tags=integration` suite vs real PostgreSQL | exit 0 — incl. `tests/integration` ok |
| 07 | [07-authz-matrix.txt](07-authz-matrix.txt) | `-tags=authz` tenancy matrix vs wired stack | exit 0 — report: [tests/authz/MATRIX.md](../../../tests/authz/MATRIX.md) |
| 08 | [08-e2e-journeys.txt](08-e2e-journeys.txt) · [run1 FAIL](08-e2e-journeys.failed-run1.txt) | `-tags=e2e` customer journeys | run1 RED (422 wire drift) → payloads aligned to #101/#110 → GREEN 34.5s |
| 09 | [09-e2e-demo.txt](09-e2e-demo.txt) · [run1 FAIL](09-e2e-demo.failed-run1.txt) | `scripts/e2e-demo.sh` | run1 FAIL at 4/12 → run2 ALL GREEN 12/12 |
| 10 | [10-security-scan.txt](10-security-scan.txt) | `bash scripts/security-scan.sh` | CLEAN — zero actionable findings (canary-validated) |
| 11 | [11-release.txt](11-release.txt) | `make release` + boot smoke | 3 stamped binaries + CycloneDX SBOM, version v0.10.1-rc1 @ `35de2a0` |
| 12 | [12-carrier-conformance.txt](12-carrier-conformance.txt) | conformance re-run: telephony/messaging/comms | 12 packages ok, 0 fail (8 embedded kit runs PASS) |
| 13 | [13-driver-planes.txt](13-driver-planes.txt) | driver planes re-run (bus/CH/search/routing/httpx/identity/workflows) | green default + `-tags=temporal`; `go vet -tags=integration,temporal` clean |
| — | [umbrella-10-closure.md](umbrella-10-closure.md) | per-item evidence table for umbrella #10 | all items LANDED, re-verified green at HEAD; residual wired-status → #113 |
| — | [journeys/](journeys/) | raw journey transcripts | `go-journeys-*.txt` (J1/J2/J3 go test) + `demo-loop-*.txt` (12-step demo) |

First-failure artifacts (`.failed-run1.txt`) are kept deliberately: they are the record of the
verification loop catching real drift (wire-contract misalignment) and of the fix being re-run to green.
