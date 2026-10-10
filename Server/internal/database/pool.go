package database

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrOpen        = errors.New("database_open_failed")
	ErrUnavailable = errors.New("database_unavailable")
)

// ReadinessConns is the readiness pool's size: the readiness monitor's
// fast probe and audit own this connection on the runtime credential, so no
// background job can starve them. It counts in the connection budget.
const ReadinessConns int32 = 1

// BackgroundConns is the background pool's size: the audit verifier, the
// key inventory and the authority sweeps share it, one at a time, on the
// runtime credential. It counts in the connection budget.
const BackgroundConns int32 = 1

// idlePingAfter is the pgxpool ShouldPing threshold: a connection idle for
// less than this is used without a ping round trip (pgx's default pings any
// connection idle for over a second, one extra round trip per request).
const idlePingAfter = time.Minute

// Open validates configuration and constructs a lazy pool. Dependency outages
// must not prevent the HTTP process from serving liveness.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrOpen
	}
	return openConfigured(ctx, cfg)
}

// OpenReadiness opens the readiness pool on the runtime DSN: lazily,
// ReadinessConns connection and none kept open.
func OpenReadiness(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return openRuntimeSide(ctx, databaseURL, ReadinessConns)
}

// OpenBackground opens the background pool on the runtime DSN: lazily,
// BackgroundConns connection and none kept open.
func OpenBackground(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return openRuntimeSide(ctx, databaseURL, BackgroundConns)
}

// openRuntimeSide opens a small fixed pool beside the request pool on its
// DSN. The DSN's own pool ceiling and minimums are the request pool's and
// do not apply here.
func openRuntimeSide(ctx context.Context, databaseURL string, max int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrOpen
	}
	cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns = max, 0, 0
	// pgxpool finishes a connection attempt after its Acquire is cancelled,
	// so without a bound an attempt stalled by an unresponsive server would
	// hold the pool's only connection, and readiness, indefinitely.
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = sideConnectTimeout
	}
	return openConfigured(ctx, cfg)
}

// sideConnectTimeout bounds a readiness or background connection attempt
// when the DSN sets no connect_timeout.
const sideConnectTimeout = 2 * time.Second

// openConfigured applies what every pool shares and constructs it lazily:
// the acquire ping only after a minute idle, and the per-connection
// statement cache, so every statement is prepared once per connection.
func openConfigured(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	cfg.ShouldPing = func(_ context.Context, p pgxpool.ShouldPingParams) bool { return p.IdleDuration > idlePingAfter }
	// pgx parses default_query_exec_mode and statement_cache_capacity out of
	// the DSN into the config; its defaults are the per-connection statement
	// cache, so any other value was supplied.
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement || cfg.ConnConfig.StatementCacheCapacity <= 0 {
		return nil, ErrOpen
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, ErrOpen
	}
	return pool, nil
}

// OpenAndPing is for commands that require connectivity before doing any work.
func OpenAndPing(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pool, err := Open(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, ErrUnavailable
	}
	return pool, nil
}

// OpenWithMax preserves pgx connection/pool controls and lazy opening, while
// rejecting a supplied DSN ceiling that conflicts with the explicit profile.
func OpenWithMax(ctx context.Context, databaseURL string, max int32) (*pgxpool.Pool, error) {
	return openWithMax(ctx, databaseURL, max, false)
}

// readOnlyParameter makes every session's transactions read-only by default,
// so even a transaction-control statement inside a guarded callback (a raw
// COMMIT ending the read-only transaction) cannot start a writable one.
const readOnlyParameter = "default_transaction_read_only"

// OpenReadOnlyWithMax opens a separately configured, lazily connecting pool
// whose sessions default to read-only transactions. A DSN that supplies a
// different read-only default or a conflicting ceiling is rejected.
func OpenReadOnlyWithMax(ctx context.Context, databaseURL string, max int32) (*pgxpool.Pool, error) {
	return openWithMax(ctx, databaseURL, max, true)
}

// ReadOnlyByDefault reports whether pool was configured by OpenReadOnlyWithMax.
func ReadOnlyByDefault(pool *pgxpool.Pool) bool {
	return pool != nil && pool.Config().ConnConfig.RuntimeParams[readOnlyParameter] == "on"
}

func openWithMax(ctx context.Context, databaseURL string, max int32, readOnly bool) (*pgxpool.Pool, error) {
	if max <= 0 {
		return nil, ErrOpen
	}
	connection, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, ErrOpen
	}
	if raw, present := connection.RuntimeParams["pool_max_conns"]; present {
		supplied, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || int32(supplied) != max {
			return nil, ErrOpen
		}
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil || cfg.MinConns > max || cfg.MinIdleConns > max {
		return nil, ErrOpen
	}
	cfg.MaxConns = max
	if readOnly {
		if supplied, present := cfg.ConnConfig.RuntimeParams[readOnlyParameter]; present && supplied != "on" {
			return nil, ErrOpen
		}
		cfg.ConnConfig.RuntimeParams[readOnlyParameter] = "on"
	}
	return openConfigured(ctx, cfg)
}
