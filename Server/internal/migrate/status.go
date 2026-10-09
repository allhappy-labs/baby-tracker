package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Status struct {
	Current  int64
	Required int64
}

func (r *Runner) Status(ctx context.Context, pool *pgxpool.Pool) (Status, error) {
	migrations, err := r.loadMigrations()
	if err != nil {
		return Status{}, err
	}
	if pool == nil {
		return Status{}, errors.New("database pool is required")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("acquire status connection: %w", err)
	}
	defer conn.Release()
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return Status{}, fmt.Errorf("check migration table: %w", err)
	}
	if !exists {
		return Status{Required: migrations[len(migrations)-1].version}, nil
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return Status{}, err
	}
	if err := validateApplied(migrations, applied); err != nil {
		return Status{}, err
	}
	var current int64
	for version := range applied {
		if version > current {
			current = version
		}
	}
	return Status{Current: current, Required: migrations[len(migrations)-1].version}, nil
}
