// Package logging provides the structured logging kernel. All Orvexa processes
// log JSON through slog with stable field names so downstream collectors can
// rely on: level, time, msg, service, env, request_id, tenant_id, agent_id,
// correlation_id. Secrets and PII must never be added to log records.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota + 1
	ctxTenantID
	ctxAgentID
	ctxCorrelationID
)

// New builds the process logger from a level string (debug|info|warn|error).
func New(level, service, envName string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With("service", service, "env", envName)
}

// WithRequestID stores a request id in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// WithTenantID stores the resolved tenant id in the context.
func WithTenantID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxTenantID, id)
}

// WithAgentID stores the acting agent id in the context.
func WithAgentID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxAgentID, id)
}

// WithCorrelationID stores the event correlation id in the context.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxCorrelationID, id)
}

func str(ctx context.Context, k ctxKey) string {
	v, _ := ctx.Value(k).(string)
	return v
}

// FromContext returns a logger annotated with whatever context identity exists.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	l := base
	if v := str(ctx, ctxRequestID); v != "" {
		l = l.With("request_id", v)
	}
	if v := str(ctx, ctxTenantID); v != "" {
		l = l.With("tenant_id", v)
	}
	if v := str(ctx, ctxAgentID); v != "" {
		l = l.With("agent_id", v)
	}
	if v := str(ctx, ctxCorrelationID); v != "" {
		l = l.With("correlation_id", v)
	}
	return l
}
