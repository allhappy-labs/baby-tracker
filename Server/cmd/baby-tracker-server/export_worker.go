package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"babytracker/server/internal/httpapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

func verifyExportPool(ctx context.Context, pool *pgxpool.Pool, expectedRole string) error {
	if pool == nil || expectedRole == "" {
		return errors.New("export worker role is unavailable")
	}
	var role string
	var canLogin, inherits, superuser, bypassRLS, migrator, runtime, projector, revoker, maintenance bool
	err := pool.QueryRow(ctx, `SELECT current_user,r.rolcanlogin,r.rolinherit,r.rolsuper,r.rolbypassrls,
		CASE WHEN to_regrole('baby_tracker_migrator') IS NULL THEN false
			ELSE pg_has_role(current_user,'baby_tracker_migrator','MEMBER') END,
		pg_has_role(current_user,'baby_tracker_runtime','MEMBER'),
		pg_has_role(current_user,'baby_tracker_projector','MEMBER'),
		pg_has_role(current_user,'baby_tracker_apple_revoker','MEMBER'),
		pg_has_role(current_user,'baby_tracker_maintenance','MEMBER')
		FROM pg_roles r WHERE r.rolname=current_user`).Scan(&role, &canLogin, &inherits, &superuser, &bypassRLS,
		&migrator, &runtime, &projector, &revoker, &maintenance)
	if err != nil || role != expectedRole || !canLogin || !inherits || superuser || bypassRLS || migrator || runtime || projector || revoker || !maintenance {
		return errors.New("export worker database role is not isolated maintenance principal")
	}
	return nil
}

type exportWorkerRunner interface {
	RunOnce(context.Context, int) error
}

func runExportWorker(ctx context.Context, readiness httpapi.Readiness, worker exportWorkerRunner, logger *slog.Logger) {
	if readiness == nil || worker == nil {
		return
	}
	for ctx.Err() == nil {
		if readiness.Check(ctx) == nil {
			iteration, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := worker.RunOnce(iteration, 1)
			cancel()
			if err != nil && ctx.Err() == nil && logger != nil {
				logger.Warn("export worker iteration failed")
			}
		}
		if !waitLifecyclePoll(ctx, 5*time.Second) {
			return
		}
	}
}
