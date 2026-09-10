// Environment gating and defaults for the ClickHouse facts store ([O-26],
// issue #35). ORVEXA_CLICKHOUSE_URL is the on/off switch: unset or empty →
// FromEnv returns (nil, nil) and the existing Postgres facts path in
// internal/analytics runs byte-identically. Set → the batched ClickHouse
// store is built; all tuning knobs have safe defaults so a bare URL is
// enough. No credential material is ever logged.
package clickhouse

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables read by FromEnv.
const (
	// EnvURL is the gate: when unset/empty the Postgres facts path is kept.
	// Formats: clickhouse://user:pass@host:9000/db (native) or
	// http://user:pass@host:8123/db (HTTP) — parsed by clickhouse-go.
	EnvURL = "ORVEXA_CLICKHOUSE_URL"
	// EnvMaxBatchRows — flush when this many buffered rows accumulate.
	EnvMaxBatchRows = "ORVEXA_CLICKHOUSE_MAX_BATCH_ROWS"
	// EnvFlushInterval — background flush cadence (Go duration, e.g. 2s).
	EnvFlushInterval = "ORVEXA_CLICKHOUSE_FLUSH_INTERVAL"
	// EnvWriteTimeout — per-flush context timeout (Go duration, e.g. 5s).
	EnvWriteTimeout = "ORVEXA_CLICKHOUSE_WRITE_TIMEOUT"
	// EnvEnqueueTimeout — how long Handle may block when the bounded buffer
	// is full before failing with analytics.clickhouse_backpressure.
	EnvEnqueueTimeout = "ORVEXA_CLICKHOUSE_ENQUEUE_TIMEOUT"
)

// FromEnv builds the store from ORVEXA_CLICKHOUSE_* variables.
//
// Gate: with ORVEXA_CLICKHOUSE_URL unset or empty it returns (nil, nil) —
// the caller keeps its existing facts store (the Postgres consumer) with
// zero drift; this is the documented byte-identical fallback path. An
// invalid URL or knob is a configuration error and returns a non-nil error.
func FromEnv(log *slog.Logger) (*Store, error) {
	url := strings.TrimSpace(os.Getenv(EnvURL))
	if url == "" {
		return nil, nil
	}
	opts := Options{URL: url}
	if v, ok := envPositiveInt(EnvMaxBatchRows, log); ok {
		opts.MaxBatchRows = v
	}
	if v, ok := envPositiveDuration(EnvFlushInterval, log); ok {
		opts.FlushInterval = v
	}
	if v, ok := envPositiveDuration(EnvWriteTimeout, log); ok {
		opts.WriteTimeout = v
	}
	if v, ok := envPositiveDuration(EnvEnqueueTimeout, log); ok {
		opts.EnqueueTimeout = v
	}
	return New(opts, log)
}

func envPositiveInt(key string, log *slog.Logger) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		warnInvalid(log, key)
		return 0, false
	}
	return v, true
}

func envPositiveDuration(key string, log *slog.Logger) (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		warnInvalid(log, key)
		return 0, false
	}
	return v, true
}

func warnInvalid(log *slog.Logger, key string) {
	if log == nil {
		return
	}
	log.Warn("invalid value for ClickHouse env knob; using default", "env", key)
	// The raw value is deliberately not logged — it is unvalidated config
	// input and could carry credential-looking material.
}
