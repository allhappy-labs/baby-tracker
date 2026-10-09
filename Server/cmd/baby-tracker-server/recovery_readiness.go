package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// recoveryFunctionReadiness checks the maintenance-only function through the
// maintenance principal. Runtime/projector function and key inventory checks
// remain in applicationReadiness, so this gate affects only recovery routes.
type recoveryFunctionReadiness struct{ pool *pgxpool.Pool }

func (readiness recoveryFunctionReadiness) Check(ctx context.Context) error {
	if readiness.pool == nil {
		return errors.New("service_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var ready bool
	err := readiness.pool.QueryRow(ctx, `SELECT
		has_schema_privilege(current_user, 'app', 'USAGE')
		AND COALESCE(has_function_privilege(current_user,
			to_regprocedure('app.cleanup_refresh_recovery_receipts(timestamp with time zone)'), 'EXECUTE'), false)`).Scan(&ready)
	if err != nil || !ready {
		return errors.New("service_unavailable")
	}
	return nil
}
