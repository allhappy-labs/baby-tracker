package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/security"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const liveActivityWorkerConnections int64 = 2

// Local schema/role/budget/readiness validation and the initial worker pass have
// separate budgets: an APNs send (bounded by WorkerSendTimeout) must never use up
// the validation budget, and the pass keeps a full validation-sized database
// margin beyond the send, so Apple latency alone cannot fail activation.
const (
	liveActivityValidationTimeout  = 10 * time.Second
	liveActivityInitialPassTimeout = liveActivityValidationTimeout + liveactivity.WorkerSendTimeout
)

func runInitialLiveActivityPass(ctx context.Context, worker *liveactivity.Worker) error {
	pass, cancel := context.WithTimeout(ctx, liveActivityInitialPassTimeout)
	defer cancel()
	return worker.RunOnce(pass, 1)
}

// This is a declared single-process maximum, not admission across replicas or
// unrelated clients. Shared export/recovery pools contribute only once.
type databasePoolBudget struct{ pools map[*pgxpool.Pool]struct{} }

func newDatabasePoolBudget(pools ...*pgxpool.Pool) databasePoolBudget {
	b := databasePoolBudget{pools: make(map[*pgxpool.Pool]struct{})}
	for _, p := range pools {
		if p != nil {
			b.pools[p] = struct{}{}
		}
	}
	return b
}
func (b databasePoolBudget) maximum() int64 {
	var total int64
	for p := range b.pools {
		maximum := int64(p.Config().MaxConns)
		if maximum <= 0 || total > math.MaxInt64-maximum {
			return math.MaxInt64
		}
		total += maximum
	}
	return total
}
func (b databasePoolBudget) validate(maximum, superuserReserved, reserved int64) error {
	if maximum <= 0 || superuserReserved < 0 || reserved < 0 || superuserReserved >= maximum || reserved >= maximum-superuserReserved || maximum-superuserReserved-reserved < liveActivityWorkerConnections || b.maximum() > maximum-superuserReserved-reserved-liveActivityWorkerConnections {
		return errors.New("live_activity_database_budget_exceeded")
	}
	return nil
}
func (b databasePoolBudget) check(ctx context.Context, pool *pgxpool.Pool) error {
	var maximum, superuserReserved, reserved int64
	err := pool.QueryRow(ctx, `SELECT
  (SELECT setting::bigint FROM pg_settings WHERE name='max_connections'),
  (SELECT setting::bigint FROM pg_settings WHERE name='superuser_reserved_connections'),
  COALESCE((SELECT setting::bigint FROM pg_settings WHERE name='reserved_connections'),0)`).Scan(&maximum, &superuserReserved, &reserved)
	if err != nil {
		return errors.New("live_activity_database_budget_unavailable")
	}
	return b.validate(maximum, superuserReserved, reserved)
}

type liveActivityRuntime struct {
	pool          *pgxpool.Pool
	runtime       *pgxpool.Pool
	runner        *migrate.Runner
	compatibility migrate.Compatibility
	budget        databasePoolBudget
	readiness     httpapi.Readiness
	running       atomic.Bool
}

// Check application/recovery readiness before acquiring the authority fence.
// Readiness can itself acquire that fence, so checking inside it would deadlock.
type liveActivityReadyGuard struct {
	guard     liveactivity.SnapshotGuard
	readiness httpapi.Readiness
}

// An unready application is the worker's dependency failing, never one job's:
// it stops the worker for supervision instead of parking work.
func (g liveActivityReadyGuard) WithLiveActivitySnapshot(ctx context.Context, subject security.LiveActivitySubject, run func(pgx.Tx, access.Grant) error) error {
	if err := g.readiness.Check(ctx); err != nil {
		return errors.Join(liveactivity.ErrDependencyUnavailable, err)
	}
	return g.guard.WithLiveActivitySnapshot(ctx, subject, run)
}

func (g liveActivityReadyGuard) SweepLiveActivityRegistrations(ctx context.Context) (int, error) {
	if err := g.readiness.Check(ctx); err != nil {
		return 0, errors.Join(liveactivity.ErrDependencyUnavailable, err)
	}
	return g.guard.SweepLiveActivityRegistrations(ctx)
}

// Check is the request-path view: only the supervised worker state. It never
// touches the worker or runtime pool (/v1/server is unauthenticated), and HTTP
// combines it with the application readiness it already checks once per request.
// Full local revalidation (checkLocal) runs only at startup and before resuming.
func (r *liveActivityRuntime) Check(context.Context) error {
	if !r.running.Load() {
		return errors.New("live_activity_worker_unavailable")
	}
	return nil
}

func (r *liveActivityRuntime) checkLocal(ctx context.Context) error {
	version, err := r.runner.CompatibleVersion(ctx, r.runtime, r.compatibility)
	if err != nil || version < 28 {
		return errors.New("live_activity_schema_unavailable")
	}
	if err = database.VerifyLiveActivityRoleSeparation(ctx, r.runtime, r.pool, version); err != nil {
		return err
	}
	return r.budget.check(ctx, r.runtime)
}

func waitLiveActivityRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// activate is every check that needs the database or application readiness —
// schema, exact worker privileges, shared database, pool budget, readiness —
// within the validation budget, then the initial pass within its own. It runs
// once at boot and before every resume; a failure is never a process exit.
func (r *liveActivityRuntime) activate(ctx context.Context, worker *liveactivity.Worker) error {
	attempt, cancel := context.WithTimeout(ctx, liveActivityValidationTimeout)
	err := r.checkLocal(attempt)
	if err == nil {
		if unready := r.readiness.Check(attempt); unready != nil {
			err = errors.Join(liveactivity.ErrDependencyUnavailable, unready)
		}
	}
	cancel()
	if err != nil {
		return err
	}
	return runInitialLiveActivityPass(ctx, worker)
}

// Run returns only after joining its runners. Reuse that worker and pool;
// retries never overlap runs and remain unavailable until local checks pass.
// With active false the worker has not been activated yet (boot found the
// database or readiness not ready): supervision starts with the backoff loop.
func (r *liveActivityRuntime) supervise(ctx context.Context, worker *liveactivity.Worker, logger *slog.Logger, wait func(context.Context, time.Duration) error, active bool) {
	defer r.running.Store(false)
	for {
		if active {
			r.running.Store(true)
			stopped := worker.Run(ctx)
			r.running.Store(false)
			if ctx.Err() != nil {
				return
			}
			if logger != nil {
				// A code only: an error's message may carry data.
				logger.Error("live activity worker stopped", "reason", "local_dependency_unavailable", "code", liveactivity.SafeErrorCode(stopped))
			}
		}
		delay := 2 * time.Second
		for {
			if err := wait(ctx, delay); err != nil {
				return
			}
			err := r.activate(ctx, worker)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				break
			}
			delay = min(delay*2, time.Minute)
			if logger != nil {
				logger.Warn("live activity worker resume failed", "code", liveactivity.SafeErrorCode(err), "retry_in", delay.String())
			}
		}
		if logger != nil {
			logger.Info("live activity worker resumed")
		}
		active = true
	}
}

// liveActivityTransport is Apple's transport with its idle-connection cleanup.
// Production builds construct only liveactivity.NewAPNSTransport; identitytest
// builds may substitute an in-process recorder (live_activity_transport_*.go).
type liveActivityTransport interface {
	liveactivity.Transport
	CloseIdleConnections()
}

// Push activation is independent of authentication mode. Disabled servers open
// no pool and need no APNs key or push-only database check.
func configureLiveActivities(ctx context.Context, cfg config.Config, authority *security.Service, runtime *pgxpool.Pool, options *httpapi.Options, budget databasePoolBudget) (func(), error) {
	if !cfg.LiveActivity.Enabled {
		return func() {}, nil
	}
	transport, err := newLiveActivityTransport(cfg.LiveActivity)
	if err != nil {
		return nil, err
	}
	shutdown, err := configureLiveActivityWorker(ctx, cfg, authority, runtime, options, budget, transport)
	if err != nil {
		transport.CloseIdleConnections()
	}
	return shutdown, err
}

func configureLiveActivityWorker(ctx context.Context, cfg config.Config, authority *security.Service, runtime *pgxpool.Pool, options *httpapi.Options, budget databasePoolBudget, transport liveactivity.Transport) (func(), error) {
	return configureLiveActivityWorkerWithWait(ctx, cfg, authority, runtime, options, budget, transport, waitLiveActivityRetry)
}

func configureLiveActivityWorkerWithWait(ctx context.Context, cfg config.Config, authority *security.Service, runtime *pgxpool.Pool, options *httpapi.Options, budget databasePoolBudget, transport liveactivity.Transport, wait func(context.Context, time.Duration) error) (func(), error) {
	if !cfg.LiveActivity.Enabled {
		return func() {}, nil
	}
	if runtime == nil || options == nil || transport == nil || wait == nil {
		return nil, errors.New("live_activity_runtime_required")
	}
	// Include the runtime even when a caller supplies an incomplete composition;
	// main supplies all other actual pools. No pool credentials enter HTTP options.
	if _, ok := budget.pools[runtime]; !ok {
		return nil, errors.New("live_activity_database_budget_incomplete")
	}
	if options.FamilyService == nil {
		return nil, errors.New("live_activity_family_service_required")
	}
	if options.Readiness == nil {
		return nil, errors.New("live_activity_application_readiness_required")
	}
	// Up to here, and the worker URL and guard below, is configuration and
	// composition: malformed, it fails startup. Everything that needs the
	// database or readiness is activation, which may be deferred.
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		return nil, err
	}
	workerConfig, err := pgxpool.ParseConfig(cfg.LiveActivity.WorkerDatabaseURL)
	if err != nil {
		return nil, database.ErrOpen
	}
	workerConfig.MaxConns = int32(liveActivityWorkerConnections)
	workerConfig.MinConns = 0
	workerConfig.MinIdleConns = 0
	guard, err := security.NewLiveActivitySnapshotGuard(authority, runtime)
	if err != nil {
		return nil, err
	}
	// Opening the pool connects nothing: no minimum connections.
	pool, err := pgxpool.NewWithConfig(ctx, workerConfig)
	if err != nil {
		return nil, database.ErrOpen
	}
	worker := liveactivity.NewWorker(liveactivity.NewQueue(pool), liveActivityReadyGuard{guard: guard, readiness: options.Readiness}, transport)
	worker.Logger = options.Logger
	state := &liveActivityRuntime{pool: pool, runtime: runtime, runner: migrate.New(migrations.Files), compatibility: compatibility, budget: budget, readiness: options.Readiness}
	logger := options.Logger
	// Exercise schema, privileges, budget, readiness and the actual authority
	// sweep and claim path before exposing support. APNs rejection, timeout or
	// ambiguity is persisted by RunOnce as durable retry state; it is never
	// global readiness and has its own budget (see above). Not ready yet — the
	// migrator still running, recovery not reconciled, a privilege or budget
	// mismatch — defers activation to supervision, exactly as after a stop;
	// the process keeps serving with push unavailable and unadvertised.
	err = state.activate(ctx, worker)
	if ctx.Err() != nil {
		// The process is shutting down mid-boot; main treats this as a clean
		// stop (liveActivityShutdownRequested), not as configuration failure.
		pool.Close()
		return nil, ctx.Err()
	}
	active := err == nil
	if !active && logger != nil {
		logger.Warn("live activity worker activation deferred", "code", liveactivity.SafeErrorCode(err), "retry_in", (2 * time.Second).String())
	}
	state.running.Store(active)
	runContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		state.supervise(runContext, worker, logger, wait, active)
	}()
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			cancel()
			<-done
			pool.Close()
			if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
		})
	}
	options.LiveActivityService = liveactivity.NewService(authority, syncFamilyResolver{options.FamilyService}, cfg.LiveActivity.BundleID+".push-type.liveactivity")
	options.LiveActivityReadiness = state
	capabilities := options.Capabilities
	if capabilities == nil {
		capabilities = protocol.ServerCapabilities()
	}
	options.Capabilities = slices.Clone(capabilities)
	if !slices.Contains(options.Capabilities, protocol.CapabilityLiveActivityPushV1) {
		options.Capabilities = append(options.Capabilities, protocol.CapabilityLiveActivityPushV1)
	}
	return shutdown, nil
}
