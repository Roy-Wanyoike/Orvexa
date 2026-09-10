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
# Engine-specific files (e.g. migrations/*clickhouse*.sql, issue #35) are
# NOT PostgreSQL DDL — they are skipped here and applied to their own engine
# (compose init or clickhouse-client; see docs/devstack.md).
migrations:
	@set -euo pipefail; \
	for f in migrations/*.sql; do \
		case "$$f" in \
		*clickhouse*) \
			echo "== skipping engine-specific $$f (ClickHouse DDL; see docs/devstack.md)"; \
			continue ;; \
		esac; \
		echo "== applying $$f"; \
		psql "$${ORVEXA_DATABASE_URL:?set ORVEXA_DATABASE_URL}" -v ON_ERROR_STOP=1 -f "$$f"; \
	done

# Guard: TODO/FIXME markers must be intentional debt, not silent gaps.
lint-todos:
	@! grep -RInE "TODO|FIXME|HACK|XXX" --include="*.go" internal cmd pkg | grep -v "_test.go" || true

# --- Devstack + integration ([O-14] issue #23) — appended; existing target commands unchanged ---
.PHONY: devstack-up devstack-down devstack-status devstack-clean integration

DEVSTACK := scripts/devstack.sh

# Userland PostgreSQL 16 (Zonky binaries, loopback bind). No docker/sudo needed.
devstack-up:
	@$(DEVSTACK) start

devstack-down:
	@$(DEVSTACK) stop

devstack-status:
	@$(DEVSTACK) status

devstack-clean:
	@$(DEVSTACK) clean

# One-command integration suite: auto-starts the devstack when
# ORVEXA_TEST_DATABASE_URL is not already provided (external DB override),
# then runs the tagged tests with the race detector.
integration:
	@set -euo pipefail; \
	url="$${ORVEXA_TEST_DATABASE_URL:-}"; \
	if [ -z "$$url" ]; then \
		$(DEVSTACK) start; \
		url="$$($(DEVSTACK) url)"; \
	fi; \
	echo "== integration suite against $$(echo "$$url" | sed -E 's#//([^:/@]+):[^@]*@#//\1:***@#')"; \
	ORVEXA_TEST_DATABASE_URL="$$url" $(GO) test -race -tags=integration ./...
