package main

import (
	"context"
	"log/slog"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/httpapi"
)

// knownRefreshWarmBudget bounds the startup load of the live sessions'
// refresh digests. The statement reads one partial index (about 5 ms for
// 20,000 sessions); the budget only bounds an unreachable or stalled
// database, which then costs startup at most this long.
const knownRefreshWarmBudget = 2 * time.Second

// knownRefreshWarmGrace is how long past its budget wait waits for a load
// that ignores its context.
const knownRefreshWarmGrace = 250 * time.Millisecond

// liveRefreshSource lists the live sessions' refresh digests
// (authority.Service.LiveRefreshDigests).
type liveRefreshSource interface {
	LiveRefreshDigests(context.Context, int) ([]authority.KeyedDigest, error)
}

// startKnownRefreshWarm loads, beside the startup readiness refresh, the
// refresh digests of the sessions whose refresh token can still be used
// into known, so a device's first refresh after a restart (or a first
// start) takes a Sync slot rather than the auth lane; wait blocks until the
// load is done or budget has passed, even if the source ignored its
// context (a load done later still warms the set, which is safe to warm
// while serving). main waits before the listener accepts traffic, so no
// refresh is admitted before the set is warm. It
// runs on the runtime pool, through the request authority: sessions is
// authority_only under RLS and granted to baby_tracker_runtime alone, the
// pool is idle before serving, and the one-connection background pool is
// the startup key inventory's at that moment. A failure or an expired
// budget leaves the set empty (the node then behaves as before: such
// refreshes take the lane) and is logged; it never stops the node
// serving. Either way one line reports the digests held and the time
// taken.
func startKnownRefreshWarm(ctx context.Context, source liveRefreshSource, known *httpapi.KnownRefreshes, budget time.Duration, logger *slog.Logger) (wait func()) {
	done := make(chan struct{})
	// A little past budget, so a load cut at its own deadline is logged
	// before wait returns.
	deadline := time.NewTimer(budget + knownRefreshWarmGrace)
	go func() {
		defer close(done)
		started := time.Now()
		loadContext, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		digests, err := source.LiveRefreshDigests(loadContext, httpapi.KnownRefreshWarmLimit)
		took := time.Since(started)
		if err != nil {
			logger.Warn("known refresh tokens not loaded", slog.String("error", err.Error()), slog.Int64("duration_ms", took.Milliseconds()))
			return
		}
		logger.Info("known refresh tokens loaded", slog.Int("digests", known.Warm(digests)), slog.Int("limit", httpapi.KnownRefreshWarmLimit),
			slog.Int64("duration_ms", took.Milliseconds()))
	}()
	return func() {
		defer deadline.Stop()
		select {
		case <-done:
		case <-deadline.C:
		}
	}
}
