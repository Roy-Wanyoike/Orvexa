#!/usr/bin/env bash
#
# Orvexa portable PostgreSQL devstack — no docker, no sudo required.
#
# Provisions a userland PostgreSQL 16 server from the pinned Zonky
# embedded-postgres-binaries Maven artifact (a ZIP containing a .txz of the
# PostgreSQL bin/ tree), runs it bound to loopback only, and auto-applies
# migrations/*.sql in lexical order with ON_ERROR_STOP semantics.
#
# Subcommands:
#   init     Download bundle (cached), initdb, write config; safe to re-run
#   start    Idempotent: init if needed, start server, wait until ready,
#            apply pending migrations (already-running -> no-op)
#   stop     Clean shutdown (smart -> fast -> immediate escalation), removes pid
#   status   Report running/stopped + pid + port
#   psql     Open psql (from the bundle) against the devstack database
#   url      Print the postgres:// URL (honors ORVEXA_TEST_DATABASE_URL override)
#   clean    Stop and remove runtime state (data/log/pid); cache kept unless
#            --all is passed (cache removal forces a re-download)
#
# Environment overrides:
#   ORVEXA_DEVSTACK_PORT      default 55432
#   ORVEXA_DEVSTACK_HOST      default 127.0.0.1 (loopback bind; do not loosen)
#   ORVEXA_DEVSTACK_USER      default postgres
#   ORVEXA_DEVSTACK_PASSWORD  default postgres
#   ORVEXA_DEVSTACK_DB        default orvexa
#   ORVEXA_TEST_DATABASE_URL  if set, `url` prints it verbatim
#                             (lets `make integration` target an external DB)
#
# Security posture:
#   - Server listens on 127.0.0.1 only; scram-sha-256 host auth.
#   - Single pinned Maven artifact; sha256 verified on every download against
#     BOTH the pinned constant below and the .sha256 served by Maven Central.
#   - No credentials are written to logs (URLs are masked).
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Pinning (SRE: exact artifact + sha256; verify with:
#   curl -fsSL "$ZONKY_BASE_URL/$ZONKY_JAR" | sha256sum   # must equal PINNED
# or compare against the .sha256 file served next to the artifact).
# ---------------------------------------------------------------------------
readonly ZONKY_GROUP_PATH="io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64"
readonly ZONKY_VERSION_PINNED="16.4.0"
readonly ZONKY_SHA256_PINNED="14a5cf546aee7d327a2f5b46be6c571f2f724a2b485c270d46f3e44a1ac3df18"
readonly MAVEN_BASE="https://repo1.maven.org/maven2"
readonly PG_READY_TIMEOUT_SECS=60
readonly PG_STOP_TIMEOUT_SECS=30

# ---------------------------------------------------------------------------
# Layout (all state under <repo>/.devstack — gitignored)
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ROOT="${REPO_ROOT}/.devstack"
CACHE_DIR="${ROOT}/cache/zonky"
BUNDLE_DIR="${ROOT}/bundle"
PGDATA="${ROOT}/pg/data"
RUN_DIR="${ROOT}/run"
LOG_DIR="${ROOT}/log"
STATE_FILE="${ROOT}/state.env"
PID_FILE="${ROOT}/devstack.pid"
LOCK_FILE="${ROOT}/.lock"
MIGRATIONS_DIR="${REPO_ROOT}/migrations"
DEVSTACK_LOG="${LOG_DIR}/devstack.log"
# Applied-marker audit trail under .devstack/ (source of truth remains the
# schema_migrations table): "<RFC3339 UTC> <filename>" per successful apply.
MIGRATIONS_MARKER="${ROOT}/migrations-applied.log"

DEVSTACK_HOST="${ORVEXA_DEVSTACK_HOST:-127.0.0.1}"
DEVSTACK_PORT="${ORVEXA_DEVSTACK_PORT:-55432}"
DEVSTACK_USER="${ORVEXA_DEVSTACK_USER:-postgres}"
DEVSTACK_PASSWORD="${ORVEXA_DEVSTACK_PASSWORD:-postgres}"
DEVSTACK_DB="${ORVEXA_DEVSTACK_DB:-orvexa}"

# ---------------------------------------------------------------------------
# Logging (stderr; never logs credentials)
# ---------------------------------------------------------------------------
log() {
        local ts
        ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        # Lazy log-dir creation: `clean` removes LOG_DIR, later calls must still work.
        mkdir -p "${LOG_DIR}" 2>/dev/null || true
        { echo "[devstack ${ts}] $*" | tee -a "${DEVSTACK_LOG}" >&2; } || true
}

die() { log "ERROR: $*"; exit 1; }

mask_url() { sed -E 's#//([^:/@]+):[^@]*@#//\1:***@#'; }

require_dirs() { mkdir -p "${CACHE_DIR}" "${RUN_DIR}" "${LOG_DIR}"; }

# ---------------------------------------------------------------------------
# Bundle acquisition (Zonky embedded-postgres binaries; userland only)
# ---------------------------------------------------------------------------
jar_url() { echo "${MAVEN_BASE}/${ZONKY_GROUP_PATH}/${1}/embedded-postgres-binaries-linux-amd64-${1}.jar"; }

resolve_version() {
        # Prefer the pinned version. If Maven answers 404 for it, fall back to the
        # closest 16.x listed in maven-metadata.xml (>= pinned, else highest 16.x).
        # Transient/network failures are NOT silently downgraded to a fallback: they
        # fail loud (set -e) so offline users are told to warm the cache instead.
        local pinned="${ZONKY_VERSION_PINNED}" curl_rc versions
        set +e
        curl -fsSL -o /dev/null --retry 2 --max-time 30 "$(jar_url "${pinned}")"
        curl_rc=$?
        set -e
        if [ "${curl_rc}" = "0" ]; then
                echo "${pinned}"; return 0
        fi
        [ "${curl_rc}" = "22" ] || die "pinned artifact probe failed (curl rc=${curl_rc}); network problem — not falling back, fix connectivity or warm .devstack/cache"
        log "pinned ${pinned} not present on Maven (HTTP error); consulting maven-metadata.xml for closest 16.x"
        versions="$(curl -fsSL --retry 2 --max-time 30 "${MAVEN_BASE}/${ZONKY_GROUP_PATH}/maven-metadata.xml" \
                | grep -oE '16\.[0-9]+\.[0-9]+' | sort -uV)" || die "cannot reach Maven metadata and pinned artifact is gone"
        local closest="" v
        for v in ${versions}; do
                if [[ "${v}" > "${pinned}" || "${v}" == "${pinned}" ]]; then closest="${v}"; break; fi
        done
        [ -n "${closest}" ] || closest="$(echo "${versions}" | tail -1)"
        log "FALLBACK VERSION SELECTED: ${closest} (pinned ${pinned} unavailable)"
        echo "${closest}"
}

download_and_verify() {
        # $1=version  -> verifies sha256 against Maven-served .sha256 and, for the
        # pinned version, against the compile-time constant. Cached afterwards.
        local ver="$1" url sha_file expected_remote actual pinned_ok
        url="$(jar_url "${ver}")"
        sha_file="${CACHE_DIR}/embedded-postgres-binaries-linux-amd64-${ver}.jar"
        if [ -f "${sha_file}" ] && [ -s "${sha_file}" ]; then
                log "bundle jar cached: ${sha_file}"
                return 0
        fi
        local tmp; tmp="$(mktemp "${CACHE_DIR}/.dl.XXXXXX")"
        log "downloading ${url} (retries=3)"
        curl -fSL --retry 3 --retry-delay 2 --max-time 600 -o "${tmp}" "${url}" \
                || { rm -f "${tmp}"; die "download failed for ${url}"; }
        actual="$(sha256sum "${tmp}" | awk '{print $1}')"
        expected_remote="$(curl -fsSL --retry 2 --max-time 60 "${url}.sha256" | awk '{print $1}')" \
                || die "could not fetch remote .sha256 for verification (refusing to use unverified artifact)"
        [ "${actual}" = "${expected_remote}" ] || { rm -f "${tmp}"; die "sha256 MISMATCH vs Maven .sha256: ${actual} != ${expected_remote}"; }
        if [ "${ver}" = "${ZONKY_VERSION_PINNED}" ]; then
                pinned_ok=false
                [ "${actual}" = "${ZONKY_SHA256_PINNED}" ] && pinned_ok=true
                ${pinned_ok} || { rm -f "${tmp}"; die "sha256 does not match PINNED constant: ${actual} != ${ZONKY_SHA256_PINNED}"; }
        fi
        mv "${tmp}" "${sha_file}"
        echo "${actual}" > "${sha_file}.sha256"
        log "sha256 verified OK: ${actual}  (artifact ${ver}; verify: curl -fsSL ${url} | sha256sum)"
}

extract_bundle() {
        # jar (zip) -> .txz -> bin/ + share/ + lib/
        local ver="$1" jar bundle
        jar="${CACHE_DIR}/embedded-postgres-binaries-linux-amd64-${ver}.jar"
        bundle="${BUNDLE_DIR}/${ver}"
        if [ -x "${bundle}/bin/postgres" ] && [ -x "${bundle}/bin/initdb" ]; then
                log "bundle already extracted: ${bundle}"
                echo "${bundle}"; return 0
        fi
        rm -rf "${bundle}"
        mkdir -p "${bundle}"
        local tmp; tmp="$(mktemp -d "${ROOT}/tmp.extract.XXXXXX")"
        unzip -q -o "${jar}" -d "${tmp}"
        local txz; txz="$(find "${tmp}" -maxdepth 1 -name '*.txz' -print -quit)"
        [ -n "${txz}" ] || { rm -rf "${tmp}"; die "no .txz inside jar (unexpected artifact layout)"; }
        tar -xJf "${txz}" -C "${bundle}"
        rm -rf "${tmp}"
        chmod +x "${bundle}/bin/"* 2>/dev/null || true
        [ -x "${bundle}/bin/postgres" ] && [ -x "${bundle}/bin/initdb" ] \
                || die "bundle extraction did not yield bin/postgres + bin/initdb"
        echo "${bundle}"
}

bundle_psql() {
        local bundle="$1"
        if [ -x "${bundle}/bin/psql" ]; then echo "${bundle}/bin/psql"; return 0; fi
        echo ""
}

pg_env() {
        # Bundle binaries need their own lib dir on LD_LIBRARY_PATH.
        local bundle="$1"
        LD_LIBRARY_PATH="${bundle}/lib${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}"
        export LD_LIBRARY_PATH
}

# The Zonky server bundle ships ONLY initdb/pg_ctl/postgres (no psql, no
# pg_isready). Readiness, database bootstrap and migrations therefore use one
# small pgx utility compiled from the module's existing dependencies.
# go.mod/go.sum are never touched (GOFLAGS=-mod=readonly). The probe speaks the
# real wire protocol — a STRICTER readiness signal than a TCP ping.
PGTOOL_DIR="${ROOT}/tools/pgtool"
PGTOOL_BIN="${PGTOOL_DIR}/pgtool"
PGTOOL_SRC_VERSION="3"   # bump when the embedded main.go below changes
PGTOOL_STAMP="${PGTOOL_DIR}/main.go.stamp"

ensure_pgtool() {
        # Self-upgrade the embedded source when missing or stamped older than the
        # script's copy (stale on-disk sources cannot shadow the embedded main.go).
        if [ ! -f "${PGTOOL_DIR}/main.go" ] || [ "${PGTOOL_SRC_VERSION}" != "$(cat "${PGTOOL_STAMP}" 2>/dev/null || true)" ]; then
                command -v go >/dev/null 2>&1 \
                        || die "pgtool source update needed but 'go' is not on PATH — source the repo toolchain (env.sh)"
                mkdir -p "${PGTOOL_DIR}"
                cat > "${PGTOOL_DIR}/main.go" <<'GOEOF'
// pgx utility for the Orvexa devstack (the Zonky server bundle ships no client
// tools). Modes are selected by environment variables; lives under .devstack/
// (gitignored, excluded from go build ./...).
//
//	PROBE_DATABASE_URL              readiness: connect + SELECT 1
//	BOOTSTRAP_ADMIN_URL+BOOTSTRAP_DB create the dev database if missing
//	MIGRATE_DATABASE_URL + arg[1]   apply *.sql in lexical order, tracked
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pgtool: "+format+"\n", args...)
	os.Exit(1)
}

func probe(ctx context.Context, url string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		fatal("probe: %v", err)
	}
	defer conn.Close(context.Background())
	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		fatal("probe: query failed: %v", err)
	}
}

func bootstrap(ctx context.Context, adminURL, db string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		fatal("bootstrap connect: %v", err)
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, db).Scan(&exists); err != nil {
		fatal("bootstrap lookup: %v", err)
	}
	if exists {
		fmt.Printf("database %q already exists\n", db)
		return
	}
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("CREATE DATABASE %s", pgx.Identifier{db}.Sanitize())); err != nil {
		fatal("create database %q: %v", db, err)
	}
	fmt.Printf("database %q created\n", db)
}

func migrate(ctx context.Context, url, dir string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		fatal("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
                version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		fatal("schema_migrations: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(paths) == 0 {
		fatal("no migration files found in %s", dir)
	}
	sort.Strings(paths)
	for _, p := range paths {
		version := filepath.Base(p)
		var done bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&done); err != nil {
			fatal("lookup %s: %v", version, err)
		}
		if done {
			fmt.Printf("  = %s (already applied)\n", version)
			continue
		}
		sql, err := os.ReadFile(p)
		if err != nil {
			fatal("read %s: %v", p, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			fatal("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			fatal("apply %s: %v", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			fatal("record %s: %v", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			fatal("commit %s: %v", version, err)
		}
		if marker := os.Getenv("MIGRATE_MARKER_FILE"); marker != "" {
			if f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), version)
				_ = f.Close()
			}
		}
		fmt.Printf("  ok %s\n", version)
	}
}

func main() {
	ctx := context.Background()
	switch {
	case os.Getenv("PROBE_DATABASE_URL") != "":
		probe(ctx, os.Getenv("PROBE_DATABASE_URL"))
	case os.Getenv("BOOTSTRAP_ADMIN_URL") != "":
		db := os.Getenv("BOOTSTRAP_DB")
		if db == "" {
			fatal("BOOTSTRAP_DB not set")
		}
		bootstrap(ctx, os.Getenv("BOOTSTRAP_ADMIN_URL"), db)
	case os.Getenv("MIGRATE_DATABASE_URL") != "":
		if len(os.Args) < 2 {
			fatal("usage: pgtool <migrations-dir>")
		}
		migrate(ctx, os.Getenv("MIGRATE_DATABASE_URL"), os.Args[1])
	default:
		fatal("no mode selected (set PROBE_DATABASE_URL, BOOTSTRAP_ADMIN_URL or MIGRATE_DATABASE_URL)")
	}
}
GOEOF
                echo "${PGTOOL_SRC_VERSION}" > "${PGTOOL_STAMP}"
        fi
        # Fast path: an up-to-date binary needs no toolchain.
        if [ -x "${PGTOOL_BIN}" ] && [ "${PGTOOL_DIR}/main.go" -ot "${PGTOOL_BIN}" ]; then
                return 0
        fi
        command -v go >/dev/null 2>&1 \
                || die "pgtool (re)build needed but 'go' is not on PATH — source the repo toolchain (env.sh) or put go on PATH"
        log "building pgx utility (pgtool) with module deps (GOFLAGS=-mod=readonly)"
        ( cd "${REPO_ROOT}" && GOFLAGS="-mod=readonly" go build -o "${PGTOOL_BIN}" "${PGTOOL_DIR}/main.go" ) \
                || die "could not build pgtool — see go output above"
}

conn_url() {
        # Compose the canonical URL for the local devstack instance.
        local host="${DEVSTACK_HOST}" port="${DEVSTACK_PORT}"
        echo "postgres://${DEVSTACK_USER}:${DEVSTACK_PASSWORD}@${host}:${port}/${DEVSTACK_DB}?sslmode=disable"
}

conn_url_admin() {
        # Admin URL against the always-present 'postgres' database (bootstrap).
        echo "postgres://${DEVSTACK_USER}:${DEVSTACK_PASSWORD}@${DEVSTACK_HOST}:${DEVSTACK_PORT}/postgres?sslmode=disable"
}

# ---------------------------------------------------------------------------
# Server lifecycle
# ---------------------------------------------------------------------------
server_pid() {
        # PID from our pid file, validated as a live postgres process. Falls back to
        # PGDATA/postmaster.pid so a crashed script run mid-start is adopted instead
        # of wedging future starts (fail loud if the pid is alive but not ours).
        local pid
        if [ -f "${PID_FILE}" ]; then
                pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
                if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
                        echo "${pid}"; return 0
                fi
        fi
        if [ -f "${PGDATA}/postmaster.pid" ]; then
                pid="$(head -1 "${PGDATA}/postmaster.pid" 2>/dev/null || true)"
                if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
                        if ! tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null | grep -q "${PGDATA}"; then
                                die "pid ${pid} in ${PGDATA}/postmaster.pid is alive but is not our postgres — refusing to touch it"
                        fi
                        echo "${pid}" > "${PID_FILE}"
                        echo "${pid}"; return 0
                fi
        fi
        return 1
}

is_ready() {
        # Server-level readiness: probe the always-present 'postgres' database (the
        # app database may not exist yet at this point — bootstrap follows). A real
        # SQL handshake is a stronger signal than pg_isready/TCP pings.
        ensure_pgtool
        PROBE_DATABASE_URL="$(conn_url_admin)" "${PGTOOL_BIN}" >/dev/null 2>&1
}

ensure_database() {
        # initdb does not create the application database — bootstrap it if missing.
        ensure_pgtool
        BOOTSTRAP_ADMIN_URL="$(conn_url_admin)" BOOTSTRAP_DB="${DEVSTACK_DB}" "${PGTOOL_BIN}" \
                || die "could not bootstrap database ${DEVSTACK_DB}"
}

write_state() {
        cat > "${STATE_FILE}" <<EOF
ZONKY_VERSION=${ZONKY_VERSION}
PG_PORT=${DEVSTACK_PORT}
PG_HOST=${DEVSTACK_HOST}
PG_USER=${DEVSTACK_USER}
PG_DB=${DEVSTACK_DB}
EOF
}

ensure_bundle() {
        local ver bundle
        ver="$(resolve_version)"
        ZONKY_VERSION="${ver}"
        download_and_verify "${ver}"
        bundle="$(extract_bundle "${ver}")"
        BUNDLE_PATH="${bundle}"
        write_state
}

ensure_datadir() {
        local bundle="$1"
        if [ -f "${PGDATA}/PG_VERSION" ]; then
                log "data dir already initialized: ${PGDATA}"
                return 0
        fi
        mkdir -p "${PGDATA}" "${RUN_DIR}"
        log "initdb: creating cluster (user=${DEVSTACK_USER}, locale=C, encoding=UTF8, auth=scram-sha-256)"
        local pwfile; pwfile="$(mktemp "${RUN_DIR}/.pw.XXXXXX")"
        printf '%s\n' "${DEVSTACK_PASSWORD}" > "${pwfile}"
        pg_env "${bundle}"
        if ! "${bundle}/bin/initdb" \
                -D "${PGDATA}" \
                --username="${DEVSTACK_USER}" \
                --pwfile="${pwfile}" \
                --auth-host=scram-sha-256 \
                --auth-local=scram-sha-256 \
                --encoding=UTF8 \
                --no-locale \
                --no-instructions >> "${LOG_DIR}/initdb.log" 2>&1; then
                rm -f "${pwfile}"
                die "initdb failed — see ${LOG_DIR}/initdb.log"
        fi
        rm -f "${pwfile}"
        write_pg_conf "${bundle}"
        log "initdb complete"
}

write_pg_conf() {
        # Loopback bind only + dev-friendly durability tradeoffs (documented in
        # docs/devstack.md). Appended last => effective settings.
        local bundle="$1"
        cat >> "${PGDATA}/postgresql.conf" <<EOF

# --- Orvexa devstack (managed by scripts/devstack.sh) ---
listen_addresses = '${DEVSTACK_HOST}'
port = ${DEVSTACK_PORT}
unix_socket_directories = '${RUN_DIR}'
# Dev-only durability tradeoffs for speed; devstack data is disposable.
fsync = off
synchronous_commit = off
full_page_writes = off
# Observability: stderr goes to .devstack/log/postgres.log via pg_ctl -l.
log_min_messages = warning
log_line_prefix = '%m [%p] '
max_connections = 50
EOF
}

sync_pg_conf() {
        # Reconcile the managed settings with current env overrides (e.g. a new
        # ORVEXA_DEVSTACK_PORT on an existing datadir). Takes effect on next start.
        if [ -f "${PGDATA}/postgresql.conf" ]; then
                sed -i \
                        -e "s|^listen_addresses = .*|listen_addresses = '${DEVSTACK_HOST}'|" \
                        -e "s|^port = [0-9][0-9]*\$|port = ${DEVSTACK_PORT}|" \
                        "${PGDATA}/postgresql.conf"
        fi
}

start_server() {
        local bundle="$1" pid
        if pid="$(server_pid)"; then
                log "already running (pid ${pid}) — no-op"
                return 0
        fi
        rm -f "${PID_FILE}"   # stale pid from a crash
        sync_pg_conf          # reconcile env overrides (port/host) on existing datadirs
        pg_env "${bundle}"
        log "starting postgres on ${DEVSTACK_HOST}:${DEVSTACK_PORT}"
        "${bundle}/bin/pg_ctl" -D "${PGDATA}" \
                -l "${LOG_DIR}/postgres.log" \
                -w -t "${PG_READY_TIMEOUT_SECS}" start \
                || die "pg_ctl start failed — see ${LOG_DIR}/postgres.log"
        # Explicit readiness gate: probe loop, fail loud after ${PG_READY_TIMEOUT_SECS}s.
        local deadline=$((SECONDS + PG_READY_TIMEOUT_SECS))
        while ! is_ready; do
                [ "$SECONDS" -lt "${deadline}" ] \
                        || die "postgres not ready after ${PG_READY_TIMEOUT_SECS}s — see ${LOG_DIR}/postgres.log"
                sleep 0.5
        done
        pid="$(head -1 "${PGDATA}/postmaster.pid")"
        echo "${pid}" > "${PID_FILE}"
        log "postgres ready (pid ${pid}, port ${DEVSTACK_PORT})"
}

stop_server() {
        local bundle="$1" pid mode
        if ! pid="$(server_pid)"; then
                log "not running — stop is a no-op"
                rm -f "${PID_FILE}"
                return 0
        fi
        pg_env "${bundle}"
        for mode in smart fast immediate; do
                log "stopping (mode=${mode})"
                if "${bundle}/bin/pg_ctl" -D "${PGDATA}" -m "${mode}" -w -t "${PG_STOP_TIMEOUT_SECS}" stop; then
                        break
                fi
                log "pg_ctl stop mode=${mode} did not complete; escalating"
                [ "${mode}" = "immediate" ] && die "postgres refused to stop even with immediate mode"
        done
        kill -0 "${pid}" 2>/dev/null && die "postgres process ${pid} still alive after stop"
        rm -f "${PID_FILE}"
        log "stopped cleanly (pid ${pid} gone, pid file removed)"
}

apply_migrations() {
        local bundle="$1" url f version psql_bin
        url="$(conn_url)"
        psql_bin="$(bundle_psql "${bundle}")"
        log "applying migrations from ${MIGRATIONS_DIR} (lexical order, ON_ERROR_STOP)"
        if [ -n "${psql_bin}" ]; then
                apply_migrations_psql "${psql_bin}" "${url}"
        else
                log "no psql in bundle — using pgx runner (one tx per file, schema_migrations tracked)"
                ensure_pgtool
                MIGRATE_DATABASE_URL="${url}" MIGRATE_MARKER_FILE="${MIGRATIONS_MARKER}" \
                        "${PGTOOL_BIN}" "${MIGRATIONS_DIR}" \
                        || die "migration run FAILED; server state kept, fix and re-run"
        fi
        log "migrations up to date"
}

apply_migrations_psql() {
        local psql_bin="$1" url="$2" f version
        local psql_env=(env PGPASSWORD="${DEVSTACK_PASSWORD}" PGCONNECT_TIMEOUT=10 "${psql_bin}" -X --no-psqlrc \
                -h "${DEVSTACK_HOST}" -p "${DEVSTACK_PORT}" -U "${DEVSTACK_USER}" -d "${DEVSTACK_DB}" \
                -v ON_ERROR_STOP=1)
        "${psql_env[@]}" -q -c "CREATE TABLE IF NOT EXISTS schema_migrations (
                version TEXT PRIMARY KEY,
                applied_at TIMESTAMPTZ NOT NULL DEFAULT now())" \
                || die "could not ensure schema_migrations table"
        for f in "${MIGRATIONS_DIR}"/*.sql; do
                [ -e "${f}" ] || die "no migration files found in ${MIGRATIONS_DIR}"
                version="$(basename "${f}")"
                [[ "${version}" =~ ^[A-Za-z0-9._-]+$ ]] || die "suspicious migration filename, refusing: ${version}"
                if "${psql_env[@]}" -tAqc "SELECT 1 FROM schema_migrations WHERE version='${version}'" | grep -q 1; then
                        log "  = ${version} (already applied)"
                        continue
                fi
                log "  -> applying ${version}"
                "${psql_env[@]}" -1 -q -f "${f}" || die "migration ${version} FAILED (ON_ERROR_STOP); server state kept, fix and re-run"
                "${psql_env[@]}" -q -c "INSERT INTO schema_migrations(version) VALUES ('${version}')" \
                        || die "could not record ${version} in schema_migrations"
                printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${version}" >> "${MIGRATIONS_MARKER}"
                log "  ok ${version}"
        done
}

# ---------------------------------------------------------------------------
# Subcommands
# ---------------------------------------------------------------------------
cmd_init() {
        require_dirs
        exec_with_lock ensure_bundle_and_init
        log "init complete (data: ${PGDATA})"
}

ensure_bundle_and_init() {
        ensure_bundle
        ensure_datadir "${BUNDLE_PATH}"
}

cmd_start() {
        require_dirs
        exec_with_lock devstack_start_impl
}

devstack_start_impl() {
        ensure_bundle
        ensure_datadir "${BUNDLE_PATH}"
        start_server "${BUNDLE_PATH}"
        ensure_database
        apply_migrations "${BUNDLE_PATH}"
        log "start complete — url: $(conn_url | mask_url)"
}

cmd_stop() {
        require_dirs
        exec_with_lock devstack_stop_impl
}

devstack_stop_impl() {
        # Bundle may be missing after a clean; fall back to last known version.
        if [ -z "${BUNDLE_PATH:-}" ]; then
                if [ -f "${STATE_FILE}" ]; then
                        # shellcheck disable=SC1090
                        source "${STATE_FILE}"
                        BUNDLE_PATH="${BUNDLE_DIR}/${ZONKY_VERSION:-${ZONKY_VERSION_PINNED}}"
                else
                        BUNDLE_PATH="${BUNDLE_DIR}/${ZONKY_VERSION_PINNED}"
                fi
        fi
        stop_server "${BUNDLE_PATH}"
}

cmd_status() {
        local pid
        if pid="$(server_pid)"; then
                local port="${DEVSTACK_PORT}"
                # postmaster.pid line 4 = port, line 5 = socket dir.
                if [ -f "${PGDATA}/postmaster.pid" ]; then port="$(sed -n 4p "${PGDATA}/postmaster.pid" 2>/dev/null || echo "${port}")"; fi
                echo "devstack: RUNNING pid=${pid} host=${DEVSTACK_HOST} port=${port} db=${DEVSTACK_DB}"
                echo "url: $(conn_url | mask_url)"
                return 0
        fi
        echo "devstack: STOPPED (data: ${PGDATA})"
        return 0
}

cmd_psql() {
        require_dirs
        ensure_bundle
        local psql_bin; psql_bin="$(bundle_psql "${BUNDLE_PATH}")"
        if [ -z "${psql_bin}" ]; then
                # Zonky server bundle has no psql: fall back to a system psql if present.
                psql_bin="$(command -v psql 2>/dev/null || true)"
                [ -n "${psql_bin}" ] || die "no psql in the Zonky bundle and none on PATH — use scripts/devstack.sh url with any SQL client"
                log "bundle has no psql; using system psql at ${psql_bin}"
        fi
        pg_env "${BUNDLE_PATH}"
        exec env PGPASSWORD="${DEVSTACK_PASSWORD}" PGCONNECT_TIMEOUT=10 "${psql_bin}" \
                --no-psqlrc -X -h "${DEVSTACK_HOST}" -p "${DEVSTACK_PORT}" -U "${DEVSTACK_USER}" -d "${DEVSTACK_DB}" "$@"
}

cmd_url() {
        # Env override wins: lets callers target an external database.
        if [ -n "${ORVEXA_TEST_DATABASE_URL:-}" ]; then
                echo "${ORVEXA_TEST_DATABASE_URL}"
                return 0
        fi
        echo "$(conn_url)"
}

cmd_clean() {
        local hard=false
        [ "${1:-}" = "--all" ] && hard=true
        exec_with_lock devstack_clean_impl "${hard}"
}

devstack_clean_impl() {
        local hard="$1"
        if [ -n "$(ls -A "${ROOT}" 2>/dev/null || true)" ]; then
                if server_pid >/dev/null 2>&1; then
                        if [ -z "${BUNDLE_PATH:-}" ]; then
                                if [ -f "${STATE_FILE}" ]; then
                                        # shellcheck disable=SC1090
                                        source "${STATE_FILE}"
                                        BUNDLE_PATH="${BUNDLE_DIR}/${ZONKY_VERSION:-${ZONKY_VERSION_PINNED}}"
                                else
                                        BUNDLE_PATH="${BUNDLE_DIR}/${ZONKY_VERSION_PINNED}"
                                fi
                        fi
                        stop_server "${BUNDLE_PATH}"
                fi
                log "clean: removing runtime state under ${ROOT}"
                rm -rf "${PGDATA}" "${RUN_DIR}" "${LOG_DIR}" "${STATE_FILE}" "${PID_FILE}" "${LOCK_FILE}" \
                        "${MIGRATIONS_MARKER}" "${ROOT}/tmp.extract."* 2>/dev/null || true
        fi
        if ${hard}; then
                log "clean --all: also removing download cache ${CACHE_DIR}"
                rm -rf "${CACHE_DIR}" "${BUNDLE_DIR}"
        fi
        log "clean complete"
}

exec_with_lock() {
        # Serialize lifecycle mutations; never block forever (fail loud after 120s).
        # fd 9 is closed for the child process tree ("$@" 9>&-) so postgres does not
        # inherit the lock fd — otherwise the lock would outlive this script and
        # wedge every future stop/start (postmasters live for days).
        mkdir -p "${ROOT}"
        (
                flock -w 120 9 || die "another devstack operation holds the lock"
                "$@" 9>&-
        ) 9>"${LOCK_FILE}"
}

# ---------------------------------------------------------------------------
# Dispatch
# ---------------------------------------------------------------------------
[ -n "${ZONKY_VERSION:-}" ] || ZONKY_VERSION="${ZONKY_VERSION_PINNED}"
[ -n "${BUNDLE_PATH:-}" ] || BUNDLE_PATH="${BUNDLE_DIR}/${ZONKY_VERSION}"

case "${1:-}" in
        init)    shift; cmd_init "$@" ;;
        start)   shift; cmd_start "$@" ;;
        stop)    shift; cmd_stop "$@" ;;
        status)  shift; cmd_status "$@" ;;
        psql)    shift; cmd_psql "$@" ;;
        url)     shift; cmd_url "$@" ;;
        clean)   shift; cmd_clean "$@" ;;
        ""|-h|--help|help)
                echo "usage: scripts/devstack.sh {init|start|stop|status|psql|url|clean [--all]}"
                echo "  init     download bundle + initdb (idempotent)"
                echo "  start    idempotent start + auto-migrate (no-op if running)"
                echo "  stop     clean shutdown (smart->fast->immediate)"
                echo "  status   running/stopped + pid + port"
                echo "  psql     psql shell against the devstack db"
                echo "  url      print postgres:// URL (ORVEXA_TEST_DATABASE_URL overrides)"
                echo "  clean    stop + wipe runtime state (keep downloads; --all wipes cache too)"
                [ "${1:-}" = "" ] && exit 1 || exit 0
                ;;
        *) die "unknown subcommand: $*" ;;
esac
