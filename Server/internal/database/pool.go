package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrOpen        = errors.New("database_open_failed")
	ErrUnavailable = errors.New("database_unavailable")
)

// Open validates configuration and constructs a lazy pool. Dependency outages
// must not prevent the HTTP process from serving liveness.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
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
