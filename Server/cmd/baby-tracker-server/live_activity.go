package main

import (
	"errors"
	"log/slog"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/liveactivity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Live Activity push composition (owner decision D1). main's worker
// login, its pool budget, its supervisor and its preflight are not ported:
// the worker runs on the maintenance pool inside its worker slots, the
// node's readiness gates its Run, and apns-preflight checks Apple.

// newLiveActivityDirty is the post-commit change feed's Live Activity
// recorder (beside the wake tracker): nil when push is off.
func newLiveActivityDirty(cfg config.Config) *liveactivity.Dirty {
	if !cfg.LiveActivity.Enabled {
		return nil
	}
	return liveactivity.NewDirty()
}

// composeLiveActivity composes Live Activity push when
// BT_LIVE_ACTIVITY_PUSH_ENABLED is on and returns its worker, which the
// caller runs (Run with the node's readiness): the queue on the maintenance
// pool in its worker slots, the authority's snapshot, the one APNs client
// (shared with the wake pusher) and dirty, the recorder the change feed
// writes to (applicationOptions). It sets the registration service (the
// topic is the bundle ID + ".push-type.liveactivity") and the worker's
// readiness, which together advertise live-activity-push-v1 and open the
// PUT routes. With push off it composes nothing and returns nil; the
// unregistration route is applicationOptions'.
func composeLiveActivity(cfg config.Config, options *httpapi.Options, authorityService *authority.Service, dirty *liveactivity.Dirty,
	maintenancePool *pgxpool.Pool, slots *database.WorkerSlots, client *apns.Client, logger *slog.Logger) (*liveactivity.Worker, error) {
	if !cfg.LiveActivity.Enabled {
		return nil, nil
	}
	if options == nil || authorityService == nil || dirty == nil || maintenancePool == nil || slots == nil || client == nil {
		return nil, errors.New("live activity push needs the authority, its change feed, the maintenance pool and its worker slots, and the APNs client")
	}
	worker := liveactivity.NewWorker(liveactivity.NewQueue(maintenancePool), authorityService, liveactivity.APNSTransport{Client: client}, dirty, slots)
	worker.RequireEntitlement = requireFamilySyncEntitlement(cfg.Mode)
	worker.Logger = logger
	options.LiveActivityService = liveactivity.NewService(authorityService, cfg.LiveActivity.Credentials.BundleID+".push-type.liveactivity")
	options.LiveActivityReady = worker.Ready
	return worker, nil
}
