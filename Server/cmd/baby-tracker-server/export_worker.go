package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/httpapi"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// verifyMaintenancePool checks that pool's login is expectedRole, an
// isolated member of baby_tracker_maintenance only: a login, inheriting, not
// a superuser, not bypassing RLS, and a member of no other application
// group. The readiness audit runs it on every maintenance-credential pool
// (the maintenance pool, and the export pool when it has its own URL).
//
// A pool it cannot query (busy, unreachable, cancelled) is
// errMaintenanceUnavailable and a refused login is
// errMaintenanceAuthentication; only a login that answers and is not the
// isolated principal is an isolation failure. No error carries the
// database's own text (which may name hosts or users).
func verifyMaintenancePool(ctx context.Context, pool *pgxpool.Pool, expectedRole string) error {
	if pool == nil || expectedRole == "" {
		return errors.New("maintenance database role is not configured")
	}
	var role string
	var canLogin, inherits, superuser, bypassRLS, migrator, runtime, maintenance bool
	err := pool.QueryRow(ctx, `SELECT current_user,r.rolcanlogin,r.rolinherit,r.rolsuper,r.rolbypassrls,
		CASE WHEN to_regrole('baby_tracker_migrator') IS NULL THEN false
			ELSE pg_has_role(current_user,'baby_tracker_migrator','MEMBER') END,
		pg_has_role(current_user,'baby_tracker_runtime','MEMBER'),
		pg_has_role(current_user,'baby_tracker_maintenance','MEMBER')
		FROM pg_roles r WHERE r.rolname=current_user`).Scan(&role, &canLogin, &inherits, &superuser, &bypassRLS,
		&migrator, &runtime, &maintenance)
	if err != nil {
		return maintenanceQueryError(err)
	}
	if role != expectedRole || !canLogin || !inherits || superuser || bypassRLS || migrator || runtime || !maintenance {
		return errMaintenanceIsolation
	}
	return nil
}

// maintenanceQueryError classifies a failed maintenance login check: a
// refused credential, an unreachable database (database.Unreachable: no
// verdict) or any other refusal by the server (a verdict, named by its
// SQLSTATE only).
func maintenanceQueryError(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && (pgErr.Code == "28P01" || pgErr.Code == "28000"):
		return errMaintenanceAuthentication
	case database.Unreachable(err):
		return errMaintenanceUnavailable
	case errors.As(err, &pgErr):
		return fmt.Errorf("%w (SQLSTATE %s)", errMaintenanceRefused, pgErr.Code)
	default:
		return errMaintenanceRefused
	}
}

var (
	// errMaintenanceRefused is a maintenance login check the database
	// refused (for example a revoked privilege or a dropped group role).
	errMaintenanceRefused = errors.New("maintenance database role check refused")
	// errMaintenanceUnavailable is a maintenance pool that could not be
	// queried: no verdict on the login.
	errMaintenanceUnavailable = fmt.Errorf("%w: maintenance database unavailable", database.ErrUnavailable)
	// errMaintenanceAuthentication is a maintenance credential the
	// database refused.
	errMaintenanceAuthentication = errors.New("maintenance database authentication failed")
	// errMaintenanceIsolation is a maintenance login that is not the
	// isolated maintenance principal.
	errMaintenanceIsolation = errors.New("maintenance database role is not the isolated maintenance principal")
)

type exportWorkerRunner interface {
	RunOnce(context.Context, int) error
}

// runExportWorker runs the export worker while the node is ready, each
// iteration inside a maintenance worker slot (one connection), so the
// readiness audit always finds a maintenance connection.
func runExportWorker(ctx context.Context, readiness httpapi.Readiness, slots *database.WorkerSlots, worker exportWorkerRunner, logger *slog.Logger) {
	if readiness == nil || slots == nil || worker == nil {
		return
	}
	for ctx.Err() == nil {
		if readiness.Check(ctx) == nil {
			iteration, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := slots.Do(iteration, func(iteration context.Context) error { return worker.RunOnce(iteration, 1) })
			cancel()
			if err != nil && ctx.Err() == nil && logger != nil {
				logger.Warn("export worker iteration failed")
			}
		}
		if !waitPoll(ctx, 5*time.Second) {
			return
		}
	}
}

// waitPoll waits duration and reports false when ctx ended first.
func waitPoll(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
