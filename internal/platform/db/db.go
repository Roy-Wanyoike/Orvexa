// Package db owns PostgreSQL connectivity for all Orvexa processes.
//
// The pool is lazy: processes boot and serve health endpoints even when the
// database is unreachable, and /readyz reports the real state. Durable
// operation requires ORVEXA_DATABASE_URL; unit tests never touch the pool.
package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect creates a connection pool. It pings once with a short deadline but
// does not fail the process on failure — readiness is reported by health.
func Connect(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = pool.Ping(pingCtx) // reported via readiness, never fatal at boot
	return pool, nil
}

// Healthy reports whether the pool can serve a trivial query right now.
func Healthy(ctx context.Context, pool *pgxpool.Pool) bool {
	if pool == nil {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return pool.Ping(pingCtx) == nil
}
