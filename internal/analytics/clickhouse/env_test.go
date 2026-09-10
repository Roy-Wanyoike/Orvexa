package clickhouse

import (
	"errors"
	"testing"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// TestFromEnvGateIsOffByDefault pins the byte-identical fallback contract:
// with ORVEXA_CLICKHOUSE_URL unset or empty, FromEnv returns (nil, nil) and
// callers keep the existing Postgres facts path untouched.
func TestFromEnvGateIsOffByDefault(t *testing.T) {
	t.Setenv(EnvURL, "")
	store, err := FromEnv(discardLogger())
	if err != nil {
		t.Fatalf("FromEnv with empty URL: %v", err)
	}
	if store != nil {
		t.Fatal("FromEnv must return nil store when the gate env is unset/empty")
	}
}

func TestFromEnvRejectsInvalidURL(t *testing.T) {
	t.Setenv(EnvURL, "not-a-dsn")
	store, err := FromEnv(discardLogger())
	if store != nil || err == nil {
		t.Fatalf("want error and nil store for malformed DSN, got (%v, %v)", store, err)
	}
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) || appErr.Code != "analytics.clickhouse_url_invalid" {
		t.Fatalf("want analytics.clickhouse_url_invalid, got %v", err)
	}
}

func TestFromEnvBuildsStoreWithDefaults(t *testing.T) {
	t.Setenv(EnvURL, testURL)
	store, err := FromEnv(discardLogger())
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if store == nil {
		t.Fatal("store must be built when the gate env is set")
	}
	defer func() { _ = store.Close() }()
	if store.maxRows != DefaultMaxBatchRows {
		t.Fatalf("maxRows=%d, want default %d", store.maxRows, DefaultMaxBatchRows)
	}
	if store.flushEvery != DefaultFlushInterval {
		t.Fatalf("flushEvery=%v, want default %v", store.flushEvery, DefaultFlushInterval)
	}
	if store.writeTimeout != DefaultWriteTimeout {
		t.Fatalf("writeTimeout=%v, want default %v", store.writeTimeout, DefaultWriteTimeout)
	}
	if store.enqueueTimeout != DefaultEnqueueTimeout {
		t.Fatalf("enqueueTimeout=%v, want default %v", store.enqueueTimeout, DefaultEnqueueTimeout)
	}
	// No rows buffered → Close must not dial the (nonexistent) server.
	if err := store.Close(); err != nil {
		t.Fatalf("close without traffic must not dial: %v", err)
	}
}

func TestFromEnvAppliesKnobs(t *testing.T) {
	t.Setenv(EnvURL, testURL)
	t.Setenv(EnvMaxBatchRows, "7")
	t.Setenv(EnvFlushInterval, "250ms")
	t.Setenv(EnvWriteTimeout, "3s")
	t.Setenv(EnvEnqueueTimeout, "150ms")

	store, err := FromEnv(discardLogger())
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	defer func() { _ = store.Close() }()
	if store.maxRows != 7 || store.flushEvery != 250*time.Millisecond ||
		store.writeTimeout != 3*time.Second || store.enqueueTimeout != 150*time.Millisecond {
		t.Fatalf("knobs not applied: maxRows=%d flushEvery=%v writeTimeout=%v enqueueTimeout=%v",
			store.maxRows, store.flushEvery, store.writeTimeout, store.enqueueTimeout)
	}
}

func TestFromEnvFallsBackToDefaultsOnInvalidKnobs(t *testing.T) {
	t.Setenv(EnvURL, testURL)
	t.Setenv(EnvMaxBatchRows, "-5")
	t.Setenv(EnvFlushInterval, "not-a-duration")
	t.Setenv(EnvWriteTimeout, "0s")
	t.Setenv(EnvEnqueueTimeout, "-1ms")

	store, err := FromEnv(discardLogger())
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	defer func() { _ = store.Close() }()
	if store.maxRows != DefaultMaxBatchRows || store.flushEvery != DefaultFlushInterval ||
		store.writeTimeout != DefaultWriteTimeout || store.enqueueTimeout != DefaultEnqueueTimeout {
		t.Fatalf("invalid knobs must fall back to defaults: maxRows=%d flushEvery=%v writeTimeout=%v enqueueTimeout=%v",
			store.maxRows, store.flushEvery, store.writeTimeout, store.enqueueTimeout)
	}
}

func TestNewRequiresURL(t *testing.T) {
	_, err := New(Options{}, discardLogger())
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) || appErr.Code != "analytics.clickhouse_url_missing" {
		t.Fatalf("want analytics.clickhouse_url_missing, got %v", err)
	}
}

func TestHTTPAndNativeDSNSchemesAccepted(t *testing.T) {
	for _, url := range []string{
		"clickhouse://127.0.0.1:9000/orvexa",
		"http://default:orvexa@127.0.0.1:8123/orvexa",
	} {
		s, err := New(Options{URL: url, FlushInterval: time.Hour}, discardLogger())
		if err != nil {
			t.Fatalf("New(%s): %v", url, err)
		}
		_ = s.Close()
	}
}
