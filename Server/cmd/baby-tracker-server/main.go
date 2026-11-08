package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"babytracker/server/internal/account"
	"babytracker/server/internal/admission"
	"babytracker/server/internal/apns"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/delivery"
	"babytracker/server/internal/diagnostics"
	familyexport "babytracker/server/internal/export"
	"babytracker/server/internal/family"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/importer"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/maintenance"
	"babytracker/server/internal/probe"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/pushtoken"
	"babytracker/server/internal/realtime"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/wake"

	"github.com/jackc/pgx/v5/pgxpool"
)

const maxHeaderBytes = 1 << 20

const identityTestHostedAdapterEnv = "BT_IDENTITYTEST_HOSTED_ADAPTER"

var sourceVersion = "unknown"

const sourceURL = "https://github.com/allhappy-labs/baby-tracker"

var errHostedAdapterUnavailable = errors.New("hosted_adapter_unavailable")

func main() {
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		probeCommand(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "apns-preflight" {
		apnsPreflightCommand(os.Args[2:])
		return
	}
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: baby-tracker-server [probe --url <health-url> | apns-preflight --environment sandbox|production]")
		os.Exit(2)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadServerConfig()
	if err != nil {
		logger.Error("load configuration", slog.String("error", err.Error()))
		os.Exit(1)
	}
	slog.SetDefault(logger)
	// identitytest builds only: pprof on its own listener (a no-op in the
	// release build, which links no pprof).
	stopProfiling := startProfiling(logger)
	defer stopProfiling()
	// Before any pool opens: the GC steers under 85% of the container's
	// memory unless GOMEMLIMIT is set, and under that limit collects at
	// GOGC 400 unless GOGC is set.
	applyGCPercent(os.Getenv, applyMemoryLimit(os.Getenv, os.ReadFile, logger), logger)
	logRuntimeLimits(logger, os.ReadFile)
	pool, err := openApplicationPool(context.Background(), cfg.DatabaseURL, cfg.DatabasePools.Runtime, cfg.DatabasePools.Explicit())
	if err != nil {
		logger.Error("open database", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pool.Close()
	// Beside the request pool, on the runtime credential: the readiness pool
	// (one connection, the readiness monitor alone, so nothing can starve
	// it) and the background pool (one connection: the audit verifier, the
	// key inventory and the authority sweeps, one at a time). Neither waits
	// for, or takes, a request connection.
	readinessPool, err := database.OpenReadiness(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Error("open readiness database", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer readinessPool.Close()
	background, err := database.OpenBackground(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Error("open background database", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer background.Close()
	// The maintenance credential is required in every mode (decision D3).
	// Its login is verified by the readiness audit, so a database outage
	// never stops the process serving liveness.
	maintenancePool, err := openApplicationPool(context.Background(), cfg.RecoveryCleanup.WorkerDatabaseURL, cfg.DatabasePools.Maintenance, cfg.DatabasePools.Explicit())
	if err != nil {
		logger.Error("open maintenance database")
		os.Exit(1)
	}
	defer maintenancePool.Close()
	// Maintenance workers run inside a slot: one connection of each
	// maintenance pool stays free for the readiness audit.
	maintenanceSlots, err := database.NewWorkerSlots(maintenancePool)
	if err != nil {
		logger.Error("reserve the maintenance audit connection", slog.String("error", err.Error()))
		os.Exit(1)
	}
	maintenancePools := []*pgxpool.Pool{maintenancePool}
	var exportPool *pgxpool.Pool
	exportSlots := maintenanceSlots
	if cfg.Export.Enabled {
		exportPool = maintenancePool
		if cfg.Export.WorkerDatabaseURL != cfg.RecoveryCleanup.WorkerDatabaseURL {
			exportPool, err = openApplicationPool(context.Background(), cfg.Export.WorkerDatabaseURL, cfg.DatabasePools.Maintenance, cfg.DatabasePools.Explicit())
			if err != nil {
				logger.Error("open export worker database")
				os.Exit(1)
			}
			defer exportPool.Close()
			if exportSlots, err = database.NewWorkerSlots(exportPool); err != nil {
				logger.Error("reserve the export audit connection", slog.String("error", err.Error()))
				os.Exit(1)
			}
			maintenancePools = append(maintenancePools, exportPool)
		}
	}

	// The dedicated MCP pool exists only when MCP is enabled; disabled mode
	// opens no MCP connection and reads no MCP secret.
	mcpPool, err := openMCPPool(context.Background(), cfg)
	if err != nil {
		logger.Error("open mcp database")
		os.Exit(1)
	}
	if mcpPool != nil {
		defer mcpPool.Close()
	}

	budgets := serverPoolBudgets(pool, mcpPool, readinessPool, background, maintenancePool, exportPool)
	if cfg.DatabasePools.Explicit() {
		if err := database.ValidateBudget(budgets, cfg.DatabasePools.ServerMax, cfg.DatabasePools.Reserved); err != nil {
			logger.Error("validate database connection budget", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}
	var total int64
	for _, budget := range budgets {
		total += int64(budget.Max)
	}
	logger.Info("effective database connection budgets", slog.Int("runtime", int(pool.Config().MaxConns)), slog.Int("readiness", int(readinessPool.Config().MaxConns)), slog.Int("background", int(background.Config().MaxConns)), slog.Int("maintenance_pools", len(maintenancePools)), slog.Int64("total", total), slog.Int("mcp", int(cfg.DatabasePools.MCP)), slog.Int("mcp_admission", cfg.Admission.MCPMax), slog.Bool("mcp_enabled", cfg.MCP.Enabled), slog.Int("server_max", int(cfg.DatabasePools.ServerMax)), slog.Int("reserved", int(cfg.DatabasePools.Reserved)))
	authorityService, familyService, err := runtimeServices(cfg, pool)
	if err != nil {
		logger.Error("construct security authority", slog.String("error", err.Error()))
		os.Exit(1)
	}
	readiness, verifier, err := composeNodeReadiness(cfg, readinessPools{Readiness: readinessPool, Maintenance: maintenancePools,
		MaintenanceLogin: maintenanceLogin, Budgets: budgets,
		Reset: append([]*pgxpool.Pool{pool, background, mcpPool}, maintenancePools...)}, background, logger)
	if err != nil {
		logger.Error("construct readiness", slog.String("error", err.Error()))
		os.Exit(1)
	}
	// Device progress for the wake worker, recorded in memory by the request
	// path (sync and snapshot) and the post-commit observer.
	wakeTracker := wake.NewTracker(0)
	// Committed family changes for the Live Activity worker (push on only).
	liveActivityDirty := newLiveActivityDirty(cfg)
	options, err := applicationOptions(cfg, logger, readiness, authorityService, familyService, pool, wakeTracker, liveActivityDirty)
	if err != nil {
		logger.Error("construct application services", slog.String("error", err.Error()))
		os.Exit(1)
	}
	var exportWorker *familyexport.Worker
	if cfg.Export.Enabled {
		identity := protocol.ServerInfo{InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch,
			CanonicalOrigin: cfg.PublicOrigin.String(), APIVersions: []string{"v1"}, Capabilities: options.Capabilities,
			AppleAuthorizationMode: options.AppleAuthorizationMode, Limits: options.SyncLimits}
		options.ExportService, exportWorker, err = familyexport.New(familyexport.Options{Authority: authorityService,
			Identity: identity, Runtime: pool,
			WorkerPool: exportPool, Keys: cfg.Export.Keys, ActiveKeyID: cfg.Export.ActiveKeyID, Now: time.Now})
		if err != nil {
			logger.Error("construct export service")
			os.Exit(1)
		}
	}
	// Apple push exists only when wakes (decision D4) or Live Activities
	// are configured, as one client for both: the wake worker sends through
	// it; the push-token route, which stores the tokens, is composed under
	// the wake flag (applicationOptions).
	apnsClient, err := apnsClientFor(cfg, newAPNSClient)
	if err != nil {
		logger.Error("construct apns client", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if apnsClient != nil {
		defer apnsClient.CloseIdleConnections()
	}
	// Live Activity push (on with BT_LIVE_ACTIVITY_PUSH_ENABLED): its worker
	// sends through the same APNs client and runs on the maintenance pool.
	liveActivityWorker, err := composeLiveActivity(cfg, &options, authorityService, liveActivityDirty, maintenancePool, maintenanceSlots, apnsClient, logger)
	if err != nil {
		logger.Error("construct live activity push", slog.String("error", err.Error()))
		os.Exit(1)
	}
	var capacity, mcpCapacity *diagnostics.CapacityCollector
	var mcpRecorder diagnostics.Recorder
	if os.Getenv("BT_SYNC_DIAGNOSTICS") == "1" {
		capacity = &diagnostics.CapacityCollector{}
		options.SyncDiagnosticRecorder = capacity.Recorder()
		options.AdmissionMetrics = capacity
		if cfg.MCP.Enabled {
			// MCP guard phases use their own collector so sync histograms and
			// integrity totals stay sync-only.
			mcpCapacity = &diagnostics.CapacityCollector{}
			mcpRecorder = mcpCapacity.Recorder()
		}
	}
	if err := attachMCP(cfg, &options, mcpDependencies{Authority: authorityService, Pool: mcpPool,
		Others: []*pgxpool.Pool{pool, readinessPool, background, maintenancePool, exportPool}, Recorder: mcpRecorder}); err != nil {
		logger.Error("construct mcp adapter", slog.String("error", err.Error()))
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           wrapIdentityTestHandler(httpapi.New(options), cfg.AppleAudience),
		ConnContext:       realtime.TransportContext,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The key inventory and the first readiness refresh (fast probe and
	// audit) run before serving, within a bounded attempt so an unreachable
	// database never delays liveness; until they succeed readiness refuses,
	// the authority loop retries the inventory and the monitor re-probes
	// every 2 seconds.
	// Beside them, the live sessions' refresh digests warm the known refresh
	// set (startKnownRefreshWarm), within their own bound: the listener
	// opens once both are done, so a restart does not send every device's
	// next refresh to the auth lane.
	waitKnownRefreshes := startKnownRefreshWarm(ctx, authorityService, options.KnownRefreshes, knownRefreshWarmBudget, logger)
	startupContext, cancelStartup := context.WithTimeout(ctx, keyInventoryStartupBudget)
	if err := readiness.inventory.Refresh(startupContext); err != nil {
		logger.Warn("credential key inventory unavailable at startup", slog.String("error", err.Error()))
	}
	_ = readiness.Refresh(startupContext)
	cancelStartup()
	waitKnownRefreshes()
	stopAuthorityLoops := startAuthorityLoops(ctx, verifier, readiness.inventory, logger)
	defer stopAuthorityLoops()
	monitorContext, cancelMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); readiness.Run(monitorContext) }()
	defer func() { cancelMonitor(); <-monitorDone }()
	if exportWorker != nil {
		go runExportWorker(ctx, options.Readiness, exportSlots, exportWorker, logger)
	}
	// The wake worker flushes the tracker to device_sync_cursors and wakes
	// devices behind their family through the live hub, or through an APNs
	// background push when they hold no connection and APNs wakes are
	// enabled, on the maintenance pool inside a worker slot, while the node
	// is ready.
	wakeWorker := newWakeWorker(cfg, options.LiveHub, maintenancePool, maintenanceSlots, wakeTracker, apnsClient, logger)
	wakeContext, cancelWake := context.WithCancel(ctx)
	wakeDone := make(chan struct{})
	go func() { defer close(wakeDone); wakeWorker.Run(wakeContext, options.Readiness.Check) }()
	// The worker stops at the signal (no new tick or push during the drain)
	// and flushes the tracker as it stops. This deferred call runs after the
	// HTTP drain and the hub's shutdown (runServerLifecycle has returned)
	// and before the pools close: it flushes once more the progress the
	// draining requests recorded after the worker's own flush (C42).
	defer stopWakeWorker(cancelWake, wakeDone, wakeWorker)
	// The resource sampler (BT_SYNC_DIAGNOSTICS=1) starts once the wake
	// worker exists, whose totals it reads; it is joined before the sampled
	// pools close.
	if capacity != nil {
		pools := poolRoles(pool, nil, exportPool, maintenancePool, mcpPool)
		sources := resourceSources{Live: options.LiveHub.Stats, TrackerLen: wakeTracker.Len, TrackerDropped: wakeTracker.Dropped, Wake: wakeWorker.Totals}
		if limiter, ok := options.Limiter.(*limits.Service); ok {
			sources.Limiter = limiter.Stats
		}
		sampler := resourceSampler{collector: capacity, mcp: mcpCapacity, read: func() diagnostics.ResourceSample { return readResources(pools, sources) }, emit: func(row diagnostics.ResourceSample) { logger.Info("capacity resource sample", slog.Any("sample", row)) }}
		sampleContext, cancelSamples := context.WithCancel(ctx)
		sampleDone := make(chan struct{})
		go func() { defer close(sampleDone); sampler.RunResourceSampler(sampleContext, 5*time.Second) }()
		defer func() { cancelSamples(); <-sampleDone }()
	}
	// The Live Activity worker flushes the change feed, sweeps and claims on
	// the maintenance pool inside its worker slots while the node is ready;
	// live-activity-push-v1 is advertised once its first pass completed.
	if liveActivityWorker != nil {
		liveActivityContext, cancelLiveActivity := context.WithCancel(ctx)
		liveActivityDone := make(chan struct{})
		go func() {
			defer close(liveActivityDone)
			_ = liveActivityWorker.Run(liveActivityContext, options.Readiness.Check)
		}()
		defer func() { cancelLiveActivity(); <-liveActivityDone }()
	}
	// Retention of the authority's rows (background pool) and of care
	// receipts, page bindings, snapshot sessions and device cursors, the
	// audit partition roll (maintenance pool), which runs once before
	// serving when the node starts ready, the hourly push-token sweep, and the
	// family purge (the request authority, so its invalidations reach the live
	// hub, and the maintenance pool).
	_, stopMaintenance := startMaintenance(ctx, readiness, authorityService, maintenancePool, maintenanceSlots, logger)
	defer stopMaintenance()
	if err := runServerLifecycle(ctx, func(runContext context.Context) error {
		return httpapi.Run(runContext, server)
	}, options.LiveHub.Shutdown); err != nil {
		logger.Error("server stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

const (
	// retentionLoopOffset starts the retention loop half an interval after
	// startup. It shares the one-connection background pool with the audit
	// verifier (which starts at once, then every 5 minutes) and the key
	// inventory (2.5 minutes out of phase): every 10 s pass then falls
	// between their starts. Each pass holds the background pool for three
	// short transactions (its care sweeps run on the maintenance pool), so
	// neither waits on it for long.
	retentionLoopOffset = maintenance.RetentionInterval / 2
	// auditRollStartupBudget bounds the audit partition roll before serving
	// (its own lock waits are bounded to 1 s).
	auditRollStartupBudget = 2 * time.Second
	// pushTokenLoopOffset starts the push-token sweep five minutes after
	// startup: never before serving, and clear of the startup audit roll.
	pushTokenLoopOffset = 5 * time.Minute
)

// maintenanceLoops composes the maintenance loops over the node: retention
// every 10 s and the audit partition roll every hour. Retention runs, in
// order, SessionSweep through the node's background authority (on the
// background pool: the authority's rows are pruned with the runtime
// credential, never on a request connection), then on the maintenance pool
// inside its worker slots the care receipt sweep (30 days, releasing each
// receipt's charge) and the expiry sweeps of continuation bindings, snapshot
// sessions (an hour past expiry) and device cursors (35 days unseen). The
// audit roll runs on the maintenance pool inside its worker slots (at once
// at startup, and after a not-ready start as soon as the node is ready; a
// failed roll is retried within a minute). The push-token sweep (APNs tokens
// of devices without a family session) runs hourly in its own loop on the
// maintenance pool inside its worker slots, first pushTokenLoopOffset after
// startup (never before serving), a failed pass retried within a minute; it
// shares no pass with the audit roll, so neither one's failure re-runs the
// other. All run only while the node's cached readiness reports ready.
func maintenanceLoops(node *databaseReadiness, maintenancePool *pgxpool.Pool, slots *database.WorkerSlots,
	logger *slog.Logger) (retention, auditPartitions, pushTokens maintenance.Loop) {
	retention = maintenance.Loop{Name: "retention", Sweeps: []maintenance.Sweep{
		&maintenance.SessionSweep{Authority: node.background, Logger: logger},
		&maintenance.ReceiptSweep{Pool: maintenancePool, Slots: slots, Logger: logger},
		&maintenance.ExpirySweep{Pool: maintenancePool, Table: "sync_page_bindings", Slots: slots, Logger: logger},
		&maintenance.ExpirySweep{Pool: maintenancePool, Table: "sync_snapshot_sessions", Slots: slots, Logger: logger},
		&maintenance.ExpirySweep{Pool: maintenancePool, Table: "device_sync_cursors", Slots: slots, Logger: logger},
	}, Interval: maintenance.RetentionInterval, Offset: retentionLoopOffset, Ready: node.Check, Now: time.Now, Logger: logger}
	auditPartitions = maintenance.Loop{Name: "audit-partitions",
		Sweeps:   []maintenance.Sweep{maintenance.AuditPartitionSweep{Pool: maintenancePool, Slots: slots, Logger: logger}},
		Interval: maintenance.AuditPartitionInterval, FailureRetry: maintenance.DefaultFailureRetry, Ready: node.Check, Now: time.Now, Logger: logger}
	pushTokens = maintenance.Loop{Name: "push-tokens",
		Sweeps:   []maintenance.Sweep{&maintenance.PushTokenSweep{Pool: maintenancePool, Slots: slots, Logger: logger}},
		Interval: maintenance.PushTokenInterval, Offset: pushTokenLoopOffset, FailureRetry: maintenance.DefaultFailureRetry,
		Ready: node.Check, Now: time.Now, Logger: logger}
	return retention, auditPartitions, pushTokens
}

// rollBeforeServing rolls the audit partitions once before the node serves,
// bounded by auditRollStartupBudget, when the node is already ready: after a
// long downtime the current month's partition then exists before the
// month's first audit row is written (a row in the default partition would
// block that month's attach). It returns the loop to run: when the roll
// succeeded, its first pass waits a whole interval (no second roll at
// startup); when the node is not ready yet (an unreachable database never
// delays liveness) or the roll failed, the loop's first pass comes at once
// and is retried as the loop retries.
func rollBeforeServing(ctx context.Context, roll maintenance.Loop) maintenance.Loop {
	if roll.Ready != nil && roll.Ready(ctx) != nil {
		return roll
	}
	rollContext, cancel := context.WithTimeout(ctx, auditRollStartupBudget)
	defer cancel()
	if _, err := roll.Pass(rollContext); err == nil {
		roll.Offset = roll.Interval
	}
	return roll
}

// purgeLoopOffset starts the purge loop a retention interval after startup,
// out of phase with the retention passes (5 s, then every 10 s).
const purgeLoopOffset = maintenance.RetentionInterval

// purgeLoop composes the family purge every 30 s: maintenance.Purger over
// the request authority (StartFamilyPurge and CompleteFamilyPurge publish
// their invalidations to its subscribers, the live hub among them) and the
// maintenance pool inside its worker slots, while the node's cached
// readiness reports ready.
func purgeLoop(node *databaseReadiness, requestAuthority *authority.Service, maintenancePool *pgxpool.Pool, slots *database.WorkerSlots,
	logger *slog.Logger) maintenance.Loop {
	return maintenance.Loop{Name: "family-purge",
		Sweeps:   []maintenance.Sweep{&maintenance.Purger{Authority: requestAuthority, Pool: maintenancePool, Slots: slots, Logger: logger}},
		Interval: maintenance.PurgeInterval, Offset: purgeLoopOffset, Ready: node.Check, Now: time.Now, Logger: logger}
}

// auditRetentionLoopOffset starts the audit retention loop well after
// startup, clear of the pre-serving audit roll and the push-token loop.
const auditRetentionLoopOffset = 10 * time.Minute

// auditRetentionLoop composes the audit retention (owner decision D1): every
// hour, auditRetentionLoopOffset after startup, maintenance.AuditRetentionSweep
// drops the audit partitions older than 13 months on the maintenance pool
// inside its worker slots, while the node's cached readiness reports ready.
// A failed pass waits the whole interval: a drop that gave up behind a long
// reader (55P03) queued audit inserts for up to its 1 s lock timeout, so it
// is not retried every minute for as long as the reader runs. It has its
// own loop, so neither its failure nor the roll's re-runs the other, and the
// pre-serving roll never drops anything.
func auditRetentionLoop(node *databaseReadiness, maintenancePool *pgxpool.Pool, slots *database.WorkerSlots, logger *slog.Logger) maintenance.Loop {
	return maintenance.Loop{Name: "audit-retention",
		Sweeps:   []maintenance.Sweep{maintenance.AuditRetentionSweep{Pool: maintenancePool, Slots: slots, Logger: logger}},
		Interval: maintenance.AuditRetentionInterval, Offset: auditRetentionLoopOffset, FailureRetry: maintenance.AuditRetentionInterval, Timeout: maintenance.AuditRetentionSweepTimeout,
		Ready: node.Check, Now: time.Now, Logger: logger}
}

// startMaintenance composes the maintenance loops (maintenanceLoops,
// purgeLoop and auditRetentionLoop), rolls the audit partitions before
// returning when the node is ready (rollBeforeServing), and runs the loops
// until ctx ends. It returns the loops it started and their stop, which
// cancels them and waits.
func startMaintenance(ctx context.Context, node *databaseReadiness, requestAuthority *authority.Service, maintenancePool *pgxpool.Pool,
	slots *database.WorkerSlots, logger *slog.Logger) ([]maintenance.Loop, func()) {
	retention, auditPartitions, pushTokens := maintenanceLoops(node, maintenancePool, slots, logger)
	auditPartitions = rollBeforeServing(ctx, auditPartitions)
	purge := purgeLoop(node, requestAuthority, maintenancePool, slots, logger)
	loops := []maintenance.Loop{retention, auditPartitions, pushTokens, purge, auditRetentionLoop(node, maintenancePool, slots, logger)}
	return loops, runLoops(ctx, loops...)
}

// runLoops runs each loop on its own goroutine until ctx ends; the returned
// stop cancels them and waits.
func runLoops(ctx context.Context, loops ...maintenance.Loop) func() {
	loopContext, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, loop := range loops {
		wg.Go(func() { loop.Run(loopContext) })
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

// apnsClientFor builds the server's one APNs client when wakes or Live
// Activities are enabled, and none otherwise. Both read the same BT_APNS_*
// credentials; a composition whose two switches disagree is refused.
func apnsClientFor(cfg config.Config, build func(config.APNSCredentials) (*apns.Client, error)) (*apns.Client, error) {
	switch {
	case cfg.APNSWakes.Enabled && cfg.LiveActivity.Enabled && cfg.APNSWakes.Credentials != cfg.LiveActivity.Credentials:
		return nil, errors.New("APNs wakes and Live Activities must share the BT_APNS_* credentials")
	case cfg.APNSWakes.Enabled:
		return build(cfg.APNSWakes.Credentials)
	case cfg.LiveActivity.Enabled:
		return build(cfg.LiveActivity.Credentials)
	default:
		return nil, nil
	}
}

// stopWakeWorker stops the wake worker at shutdown: it ends Run (cancel),
// waits for it (Run applies its kept settles and flushes the tracker as it
// stops), then flushes the tracker once more, for the progress requests
// recorded after Run's own flush. main defers it so that it runs after the
// HTTP drain and before the pools close (C42).
func stopWakeWorker(cancel context.CancelFunc, done <-chan struct{}, worker *wake.Worker) {
	cancel()
	<-done
	worker.FlushTracker(context.Background())
}

// newWakeWorker is the production wake worker: it flushes tracker (which
// the request path records into) on the maintenance pool, inside its worker
// slots, and wakes devices behind their family: those the live hub holds a
// connection for through the hub, the others through push (an APNs
// background push, only when APNs wakes are enabled). An identitytest build
// also hands it to GET /__identitytest/wake, which reports its ticks.
func newWakeWorker(cfg config.Config, hub *realtime.Hub, pool *pgxpool.Pool, slots *database.WorkerSlots, tracker *wake.Tracker,
	push *apns.Client, logger *slog.Logger) *wake.Worker {
	worker := &wake.Worker{Pool: pool, Slots: slots, Tracker: tracker, Logger: logger, Now: time.Now, Connected: hub.DeviceConnected,
		Sender: wake.HubSender{Hub: hub, Server: protocol.ServerInfo{InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch}}}
	if cfg.APNSWakes.Enabled && push != nil { // never a typed-nil Pusher
		worker.Pusher = push
	}
	identityTestWakeWorker(worker)
	return worker
}

// applicationOptions composes the HTTP application. progress (nil: none)
// records each device's sync progress and each committed change for the wake
// worker; liveActivity (newLiveActivityDirty: nil when Live Activity push is
// off, required when it is on) records each committed change for the Live
// Activity worker.
func applicationOptions(cfg config.Config, logger *slog.Logger, readiness *databaseReadiness, authorityService *authority.Service,
	familyService *family.Service, runtimePool *pgxpool.Pool, progress *wake.Tracker, liveActivity *liveactivity.Dirty) (httpapi.Options, error) {
	if readiness == nil || authorityService == nil || familyService == nil || runtimePool == nil {
		return httpapi.Options{}, errors.New("readiness, security authority, family service, and database pool are required")
	}
	if cfg.LiveActivity.Enabled && liveActivity == nil {
		return httpapi.Options{}, errors.New("live activity push needs its change feed recorder")
	}
	// Direct test compositions use the same defaults as Load.
	if cfg.Admission == (config.AdmissionConfig{}) {
		cfg.Admission = config.DefaultAdmission()
	}
	if cfg.Admission.AuthMax == 0 {
		cfg.Admission.AuthMax = admission.DefaultAuth
	}
	admissionController, err := admission.NewLanes(cfg.Admission.Total, cfg.Admission.MCPMax, cfg.Admission.AuthMax, cfg.Admission.Wait)
	if err != nil {
		return httpapi.Options{}, err
	}
	verifier, err := newConfiguredIdentityVerifier(cfg)
	if err != nil {
		return httpapi.Options{}, err
	}
	identityService, err := identity.New(identity.Options{
		Verifier: verifier, Authority: authorityService.IdentityAuthority(), Origin: cfg.PublicOrigin.String(), InstanceID: cfg.ServerInstanceID,
		IdentityKeys: composeDigestKeyring(cfg.Auth.IdentityKeys), Now: time.Now,
	})
	if err != nil {
		return httpapi.Options{}, err
	}
	sessionOptions := sessionServiceOptions(cfg, identityService, verifier, authorityService)
	var notificationService httpapi.AppleNotificationService
	if cfg.Mode == config.ModeHosted {
		// Hosted sign-in exchanges the Apple authorization code before the
		// sign-in transaction; only the identitytest exchanger exists, so
		// hosted composition without it fails here (runtimeServices refuses
		// it first).
		available, err := configureIdentityTestHostedAuth(cfg, &sessionOptions)
		if err != nil {
			return httpapi.Options{}, err
		}
		if !available {
			return httpapi.Options{}, errHostedAdapterUnavailable
		}
		created, err := identity.NewNotificationService(identity.NotificationOptions{
			Audience: cfg.AppleAudience, InstanceID: cfg.ServerInstanceID,
			IdentityKeys: composeDigestKeyring(cfg.Auth.IdentityKeys), NotificationKeys: composeDigestKeyring(cfg.Auth.AppleNotificationKeys),
			Authority: authorityService.IdentityAuthority(), Now: time.Now,
		})
		if err != nil {
			return httpapi.Options{}, err
		}
		notificationService = created
	}
	sessions, err := auth.New(sessionOptions)
	if err != nil {
		return httpapi.Options{}, err
	}
	limiter, err := limits.New(limits.Options{Pepper: cfg.Auth.RateLimitPepper, Now: time.Now, Logger: logger})
	if err != nil {
		return httpapi.Options{}, err
	}
	resolver, err := limits.NewClientIPResolver(cfg.TrustedProxyCIDRs)
	if err != nil {
		return httpapi.Options{}, err
	}
	authorizationMode := "identity_token_only"
	if cfg.Mode == config.ModeHosted {
		authorizationMode = "code_exchange_required"
	}
	challengePolicy, ok := cfg.Limits.Policies[limits.ScopeAuthChallengeIP]
	if !ok {
		return httpapi.Options{}, errors.New("challenge rate-limit policy is required")
	}
	exchangePolicy, exchangeOK := cfg.Limits.Policies[limits.ScopeAuthExchangeIP]
	refreshIPPolicy, refreshIPOK := cfg.Limits.Policies[limits.ScopeAuthRefreshIP]
	refreshAccountPolicy, refreshAccountOK := cfg.Limits.Policies[limits.ScopeAuthRefreshAccount]
	refreshDevicePolicy, refreshDeviceOK := cfg.Limits.Policies[limits.ScopeAuthRefreshDevice]
	if !exchangeOK || !refreshIPOK || !refreshAccountOK || !refreshDeviceOK {
		return httpapi.Options{}, errors.New("session rate-limit policies are required")
	}
	familyStructuralAccountPolicy, familyStructuralAccountOK := cfg.Limits.Policies[limits.ScopeFamilyStructuralAccount]
	familyStructuralDevicePolicy, familyStructuralDeviceOK := cfg.Limits.Policies[limits.ScopeFamilyStructuralDevice]
	invitationCreateAccountPolicy, invitationCreateAccountOK := cfg.Limits.Policies[limits.ScopeInvitationCreateAccount]
	invitationCreateIPPolicy, invitationCreateIPOK := cfg.Limits.Policies[limits.ScopeInvitationCreateIP]
	invitationRedeemAccountPolicy, invitationRedeemAccountOK := cfg.Limits.Policies[limits.ScopeInvitationRedeemAccount]
	invitationRedeemIPPolicy, invitationRedeemIPOK := cfg.Limits.Policies[limits.ScopeInvitationRedeemIP]
	syncMutationDevicePolicy, syncMutationDeviceOK := cfg.Limits.Policies[limits.ScopeSyncMutationDevice]
	syncMutationAccountPolicy, syncMutationAccountOK := cfg.Limits.Policies[limits.ScopeSyncMutationAccount]
	syncMutationFamilyPolicy, syncMutationFamilyOK := cfg.Limits.Policies[limits.ScopeSyncMutationFamily]
	syncPullDevicePolicy, syncPullDeviceOK := cfg.Limits.Policies[limits.ScopeSyncPullDevice]
	syncPullAccountPolicy, syncPullAccountOK := cfg.Limits.Policies[limits.ScopeSyncPullAccount]
	liveAttemptIPPolicy, liveAttemptIPOK := cfg.Limits.Policies[limits.ScopeLiveAttemptIP]
	liveAttemptDevicePolicy, liveAttemptDeviceOK := cfg.Limits.Policies[limits.ScopeLiveAttemptDevice]
	liveAttemptAccountPolicy, liveAttemptAccountOK := cfg.Limits.Policies[limits.ScopeLiveAttemptAccount]
	liveAttemptFamilyPolicy, liveAttemptFamilyOK := cfg.Limits.Policies[limits.ScopeLiveAttemptFamily]
	exportIPPolicy, exportIPOK := cfg.Limits.Policies[limits.ScopeExportIP]
	exportAccountPolicy, exportAccountOK := cfg.Limits.Policies[limits.ScopeExportAccount]
	exportDevicePolicy, exportDeviceOK := cfg.Limits.Policies[limits.ScopeExportDevice]
	pushTokenDevicePolicy, pushTokenDeviceOK := cfg.Limits.Policies[limits.ScopePushTokenDevice]
	pushTokenAccountPolicy, pushTokenAccountOK := cfg.Limits.Policies[limits.ScopePushTokenAccount]
	bearerFailureIPPolicy, bearerFailureIPOK := cfg.Limits.Policies[limits.ScopeBearerFailureIP]
	liveActivityAccountPolicy, liveActivityAccountOK := cfg.Limits.Policies[limits.ScopeLiveActivityAccount]
	liveActivityDevicePolicy, liveActivityDeviceOK := cfg.Limits.Policies[limits.ScopeLiveActivityDevice]
	if !liveActivityAccountOK || !liveActivityDeviceOK {
		return httpapi.Options{}, errors.New("live activity rate-limit policies are required")
	}
	if !familyStructuralAccountOK || !familyStructuralDeviceOK || !invitationCreateAccountOK || !invitationCreateIPOK ||
		!invitationRedeemAccountOK || !invitationRedeemIPOK {
		return httpapi.Options{}, errors.New("family rate-limit policies are required")
	}
	if !syncMutationDeviceOK || !syncMutationAccountOK || !syncMutationFamilyOK || !syncPullDeviceOK || !syncPullAccountOK {
		return httpapi.Options{}, errors.New("sync rate-limit policies are required")
	}
	if !liveAttemptIPOK || !liveAttemptDeviceOK || !liveAttemptAccountOK || !liveAttemptFamilyOK {
		return httpapi.Options{}, errors.New("live rate-limit policies are required")
	}
	if !exportIPOK || !exportAccountOK || !exportDeviceOK {
		return httpapi.Options{}, errors.New("export rate-limit policies are required")
	}
	if !pushTokenDeviceOK || !pushTokenAccountOK {
		return httpapi.Options{}, errors.New("push-token rate-limit policies are required")
	}
	if !bearerFailureIPOK {
		return httpapi.Options{}, errors.New("failed-bearer rate-limit policy is required")
	}
	capabilities := protocol.ServerCapabilities()
	if !cfg.Export.Enabled {
		capabilities = slices.DeleteFunc(capabilities, func(value string) bool { return value == protocol.CapabilityExportV1 })
	}
	// The device push-token route (push-token-v1) exists only on a server
	// with APNs wakes configured (decision D4); without them it is not
	// registered and the capability is not advertised.
	var pushTokens httpapi.PushTokenService
	if cfg.APNSWakes.Enabled {
		pushTokens = pushtoken.New(authorityService, time.Now)
	} else {
		capabilities = slices.DeleteFunc(capabilities, func(value string) bool { return value == protocol.CapabilityPushTokenV1 })
	}
	// live-activity-push-v1 is not among them: httpapi advertises it only
	// while Live Activity push is composed (composeLiveActivity) and its
	// worker and the node are ready.
	serverInfo := protocol.ServerInfo{InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch, CanonicalOrigin: cfg.PublicOrigin.String(),
		APIVersions: []string{"v1"}, Capabilities: capabilities, AppleAuthorizationMode: authorizationMode, Limits: &cfg.Sync.Limits}
	liveAuthorizer, err := realtime.NewAuthorityAuthorizer(authorityService)
	if err != nil {
		return httpapi.Options{}, err
	}
	liveHub, err := realtime.NewHub(context.Background(), realtime.HubOptions{Limits: cfg.Sync.Limits, Authorizer: liveAuthorizer, Invalidations: authorityService})
	if err != nil {
		return httpapi.Options{}, err
	}
	// The write transaction schedules no wake; the observer publishes the
	// new high water to the hub after the commit and records the change for
	// the wake worker, which also reads each request's progress.
	// It also records it for the Live Activity worker when push is on.
	var syncProgress familysync.Progress
	if progress != nil {
		syncProgress = progress
	}
	observer := delivery.NewObserver(liveHub, changeFeed(progress, liveActivity))
	syncService := familysync.New(familysync.Options{Guard: authorityService, Server: serverInfo, Limits: cfg.Sync.Limits, Now: time.Now,
		Publisher: observer, PageKeys: composeDigestKeyring(cfg.Auth.PageTokenKeys),
		Allowed: family.Allowed, Progress: syncProgress})
	importService := importer.NewService(authorityService, serverInfo, cfg.Sync.Limits, composeDigestKeyring(cfg.Auth.PageTokenKeys), time.Now)
	importService.SetPublisher(observer)
	syncService.SetImportAdmission(importService)
	return httpapi.Options{
		Admission:              admissionController,
		Logger:                 logger,
		Readiness:              readiness,
		InstanceID:             cfg.ServerInstanceID,
		RecoveryEpoch:          cfg.RecoveryEpoch,
		CanonicalOrigin:        cfg.PublicOrigin.String(),
		SyncLimits:             &cfg.Sync.Limits,
		BodyReadTimeout:        cfg.Sync.BodyReadTimeout,
		Gate:                   readiness,
		ServerState:            readiness,
		AppleAuthorizationMode: authorizationMode,
		SourceVersion:          sourceVersion,
		SourceURL:              sourceURL,
		Capabilities:           capabilities,
		ChallengeService:       identityService,
		SessionService:         sessions,
		RecoveryService:        sessions,
		// Warmed from the database before serving (startKnownRefreshWarm).
		KnownRefreshes: httpapi.NewKnownRefreshes(logger),
		// Refresh recovery follows the maintenance credential, which every
		// configuration now supplies (decision D3); the authority prunes
		// refresh receipts inside the refresh transactions. Recovery has no
		// dependency beyond the node readiness, which the recovery routes
		// (authDependenciesError) and the capability check (/v1/server)
		// already read, so it is not read a second time per request.
		RecoveryEnabled:               cfg.RecoveryCleanup.Enabled,
		RecoveryReadiness:             nodeReadinessCovered{},
		AppleNotifications:            notificationService,
		AccountService:                &account.Service{Authority: authorityService},
		FamilyService:                 familyService,
		SyncService:                   syncService,
		ImportService:                 importService,
		PushTokens:                    pushTokens,
		RequestAuthorizer:             authorityService,
		BearerAuthenticator:           authorityService,
		LiveHub:                       liveHub,
		Limiter:                       limiter,
		ClientIPResolver:              resolver,
		ChallengePolicy:               challengePolicy,
		ExchangePolicy:                exchangePolicy,
		RefreshIPPolicy:               refreshIPPolicy,
		RefreshAccountPolicy:          refreshAccountPolicy,
		RefreshDevicePolicy:           refreshDevicePolicy,
		FamilyStructuralAccountPolicy: familyStructuralAccountPolicy,
		FamilyStructuralDevicePolicy:  familyStructuralDevicePolicy,
		InvitationCreateAccountPolicy: invitationCreateAccountPolicy,
		InvitationCreateIPPolicy:      invitationCreateIPPolicy,
		InvitationRedeemAccountPolicy: invitationRedeemAccountPolicy,
		InvitationRedeemIPPolicy:      invitationRedeemIPPolicy,
		SyncMutationDevicePolicy:      syncMutationDevicePolicy,
		SyncMutationAccountPolicy:     syncMutationAccountPolicy,
		SyncMutationFamilyPolicy:      syncMutationFamilyPolicy,
		SyncPullDevicePolicy:          syncPullDevicePolicy,
		SyncPullAccountPolicy:         syncPullAccountPolicy,
		LiveAttemptIPPolicy:           liveAttemptIPPolicy,
		LiveAttemptDevicePolicy:       liveAttemptDevicePolicy,
		LiveAttemptAccountPolicy:      liveAttemptAccountPolicy,
		LiveAttemptFamilyPolicy:       liveAttemptFamilyPolicy,
		ExportIPPolicy:                exportIPPolicy,
		ExportAccountPolicy:           exportAccountPolicy,
		ExportDevicePolicy:            exportDevicePolicy,
		PushTokenDevicePolicy:         pushTokenDevicePolicy,
		PushTokenAccountPolicy:        pushTokenAccountPolicy,
		BearerFailureIPPolicy:         bearerFailureIPPolicy,
		// A phone may always withdraw its Live Activity registration, push
		// on or off; registration itself is composeLiveActivity's.
		LiveActivityUnregistration: liveactivity.NewUnregistration(authorityService),
		LiveActivityAccountPolicy:  liveActivityAccountPolicy,
		LiveActivityDevicePolicy:   liveActivityDevicePolicy,
	}, nil
}

// changeFeed is the post-commit observer's recorder: the wake tracker, then
// the Live Activity worker's Dirty, each when composed (nil: none). Only
// composed recorders are passed on: a typed nil is not a nil recorder.
func changeFeed(progress *wake.Tracker, liveActivity *liveactivity.Dirty) delivery.ChangeRecorder {
	var recorders []delivery.ChangeRecorder
	if progress != nil {
		recorders = append(recorders, progress)
	}
	if liveActivity != nil {
		recorders = append(recorders, liveActivity)
	}
	return delivery.Changes(recorders...)
}

// Development sessions use the same lifetime policy as self-hosted sessions.
func authenticationMode(mode config.Mode) config.Mode {
	if mode == config.ModeDevelopment {
		return config.ModeSelfHosted
	}
	return mode
}

func sessionServiceOptions(cfg config.Config, identityService *identity.Service, verifier identity.IdentityVerifier,
	authorityService *authority.Service) auth.Options {
	origin := ""
	if cfg.PublicOrigin != nil {
		origin = cfg.PublicOrigin.String()
	}
	return auth.Options{
		Identity: identityService, Verifier: verifier, Authority: authorityService, Mode: authenticationMode(cfg.Mode),
		Origin: origin, InstanceID: cfg.ServerInstanceID, TokenKeys: composeDigestKeyring(cfg.Auth.SessionTokenKeys),
		DeviceKeys: composeDigestKeyring(cfg.Auth.DeviceContinuityKeys), ActorIDKey: cfg.Auth.ActorIDPepper, Now: time.Now,
	}
}

func composeDigestKeyring(source config.DigestKeyringConfig) identity.DigestKeyring {
	result := identity.DigestKeyring{Current: identity.DigestKey{ID: source.Current.ID, Secret: source.Current.Secret}}
	result.Previous = make([]identity.DigestKey, len(source.Previous))
	for index, key := range source.Previous {
		result.Previous[index] = identity.DigestKey{ID: key.ID, Secret: key.Secret}
	}
	return result
}

func configuredKeyIDs(ring config.DigestKeyringConfig) map[string]struct{} {
	result := make(map[string]struct{}, len(ring.Previous)+1)
	result[ring.Current.ID] = struct{}{}
	for _, key := range ring.Previous {
		result[key.ID] = struct{}{}
	}
	return result
}

// requireFamilySyncEntitlement is whether mode requires the family_sync
// entitlement (hosted).
func requireFamilySyncEntitlement(mode config.Mode) bool { return mode == config.ModeHosted }

// authorityOptions is the production authority composition. The token
// keyring is the exact keyring auth digests tokens with (same keys, same
// order): auth.New refuses any other.
func authorityOptions(cfg config.Config, runtimePool *pgxpool.Pool) authority.Options {
	return authority.Options{
		Pool: runtimePool, Now: time.Now, InstanceID: cfg.ServerInstanceID, RecoveryEpoch: cfg.RecoveryEpoch,
		TokenKeys:     composeDigestKeyring(cfg.Auth.SessionTokenKeys),
		MaxCaregivers: cfg.Limits.MaxCaregivers, MaxInvitations: cfg.Limits.MaxInvitations,
		RequireFamilySyncEntitlement: requireFamilySyncEntitlement(cfg.Mode),
		Allowed:                      family.Allowed, EntitlementExempt: authority.LegacyEntitlementExempt,
	}
}

// runtimeServices composes the PostgreSQL authority over the request pool
// and the family service over it. Hosted mode without an Apple code
// exchanger is refused before anything is composed.
func runtimeServices(cfg config.Config, runtimePool *pgxpool.Pool) (*authority.Service, *family.Service, error) {
	if runtimePool == nil {
		return nil, nil, errors.New("runtime pool is required")
	}
	if cfg.Mode == config.ModeHosted && !identityTestHostedAdapterEnabled(cfg) {
		return nil, nil, errHostedAdapterUnavailable
	}
	authorityService, err := authority.New(authorityOptions(cfg, runtimePool))
	if err != nil {
		return nil, nil, err
	}
	origin := ""
	if cfg.PublicOrigin != nil {
		origin = cfg.PublicOrigin.String()
	}
	familyService, err := family.New(family.Options{Authority: authorityService, InvitationKeys: composeDigestKeyring(cfg.Auth.InvitationKeys),
		Origin: origin, Now: time.Now, InvitationTTL: cfg.Limits.InvitationTTL})
	if err != nil {
		return nil, nil, err
	}
	return authorityService, familyService, nil
}

// composeNodeReadiness composes node readiness on pools.Readiness, and on
// the background pool the authority its key inventory reads and the audit
// chain verifier: the readiness monitor never shares a connection with
// them.
func composeNodeReadiness(cfg config.Config, pools readinessPools, background *pgxpool.Pool, logger *slog.Logger,
	options ...readinessOption) (*databaseReadiness, *authority.Verifier, error) {
	if pools.Readiness != nil && pools.Readiness == background {
		return nil, nil, errors.New("readiness and background pools must differ")
	}
	backgroundAuthority, verifier, err := backgroundServices(cfg, background, logger)
	if err != nil {
		return nil, nil, err
	}
	readiness, err := newDatabaseReadiness(cfg, pools, backgroundAuthority, logger, options...)
	if err != nil {
		return nil, nil, err
	}
	return readiness, verifier, nil
}

// backgroundServices composes the authority over the background pool (the
// key inventory reads it) and the audit chain verifier on that pool, so
// neither takes a request connection.
func backgroundServices(cfg config.Config, background *pgxpool.Pool, logger *slog.Logger) (*authority.Service, *authority.Verifier, error) {
	if background == nil {
		return nil, nil, errors.New("background pool is required")
	}
	backgroundAuthority, err := authority.New(authorityOptions(cfg, background))
	if err != nil {
		return nil, nil, err
	}
	verifier, err := authority.NewAuditVerifier(background, time.Now, logger)
	if err != nil {
		return nil, nil, err
	}
	return backgroundAuthority, verifier, nil
}

func probeCommand(arguments []string) {
	flags := flag.NewFlagSet("probe", flag.ExitOnError)
	flags.SetOutput(os.Stderr)
	healthURL := flags.String("url", "", "health endpoint URL")
	flags.Parse(arguments)
	if *healthURL == "" {
		fmt.Fprintln(os.Stderr, "probe requires --url <health-url>")
		os.Exit(2)
	}
	if err := probe.Check(context.Background(), *healthURL); err != nil {
		fmt.Fprintln(os.Stderr, "probe failed")
		os.Exit(1)
	}
}

func openApplicationPool(ctx context.Context, dsn string, max int32, explicit bool) (*pgxpool.Pool, error) {
	if explicit {
		return database.OpenWithMax(ctx, dsn, max)
	}
	return database.Open(ctx, dsn)
}

// actualPoolBudgets assigns identifiers in composition order (runtime,
// readiness, background, then each maintenance-credential pool),
// deduplicating shared pointers rather than credentials or role.
// serverPoolBudgets adds the dedicated MCP pool.
func actualPoolBudgets(runtime, readiness, background *pgxpool.Pool, maintenance ...*pgxpool.Pool) []database.NamedPoolBudget {
	var result []database.NamedPoolBudget
	seen := map[*pgxpool.Pool]bool{}
	appendPool := func(pool *pgxpool.Pool, identity string) {
		if pool == nil || seen[pool] {
			return
		}
		seen[pool] = true
		result = append(result, database.NamedPoolBudget{Identity: identity, Max: pool.Config().MaxConns})
	}
	appendPool(runtime, "runtime")
	appendPool(readiness, "readiness")
	appendPool(background, "background")
	for i, pool := range maintenance {
		appendPool(pool, fmt.Sprintf("maintenance-%d", i+1))
	}
	return result
}
