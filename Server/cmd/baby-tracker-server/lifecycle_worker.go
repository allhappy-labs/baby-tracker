package main

import (
	"context"
	"log/slog"
	"time"

	"babytracker/server/internal/httpapi"
)

type lifecycleWorker interface {
	RunOnce(context.Context) error
}

// runLifecycleWorker waits for application readiness before every claim. Once
// an iteration starts, shutdown stops future claims but gives that iteration a
// lease-bounded context so its durable transition can finish or time out.
func runLifecycleWorker(ctx context.Context, readiness httpapi.Readiness, worker lifecycleWorker, pollInterval, leaseTTL time.Duration, logger *slog.Logger) {
	if readiness == nil || worker == nil || pollInterval <= 0 || leaseTTL <= 0 {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if err := readiness.Check(ctx); err != nil {
			if !waitLifecyclePoll(ctx, pollInterval) {
				return
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		iteration, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseTTL)
		if ctx.Err() != nil {
			cancel()
			return
		}
		err := worker.RunOnce(iteration)
		cancel()
		if err != nil && logger != nil {
			logger.Warn("apple revocation iteration failed")
		}
		if !waitLifecyclePoll(ctx, pollInterval) {
			return
		}
	}
}

func waitLifecyclePoll(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
