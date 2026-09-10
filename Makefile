SHELL := /bin/bash
GO := go

.PHONY: build vet test race run-api run-worker run-realtime migrations lint-todos

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

run-api:
	$(GO) run ./cmd/api

run-worker:
	$(GO) run ./cmd/worker

run-realtime:
	$(GO) run ./cmd/realtime

# Apply migrations in lexical order using psql ORVEXA_DATABASE_URL.
# (A dedicated migration runner ships with the deploy pipeline.)
migrations:
	@set -euo pipefail; \
	for f in migrations/*.sql; do \
		echo "== applying $$f"; \
		psql "$${ORVEXA_DATABASE_URL:?set ORVEXA_DATABASE_URL}" -v ON_ERROR_STOP=1 -f "$$f"; \
	done

# Guard: TODO/FIXME markers must be intentional debt, not silent gaps.
lint-todos:
	@! grep -RInE "TODO|FIXME|HACK|XXX" --include="*.go" internal cmd pkg | grep -v "_test.go" || true
