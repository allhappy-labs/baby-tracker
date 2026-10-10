package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/readiness"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// authorityLoopInterval is the cadence of the background audit chain
	// verifier and of the credential key inventory refresh.
	authorityLoopInterval = 5 * time.Minute
	// keyInventoryRetryInterval is the retry cadence while the key
	// inventory has never been computed (readiness refuses meanwhile).
	keyInventoryRetryInterval = 5 * time.Second
	// keyInventoryStartupBudget bounds the startup inventory attempt.
	keyInventoryStartupBudget = time.Second
)

// errKeyInventoryPending refuses readiness until the first key inventory.
// It is not credential_key_unavailable (no key is known to be missing), so
// clients see service_unavailable.
var errKeyInventoryPending = errors.New("key_inventory_pending")

const (
	// readinessInterval is the monitor's cadence.
	readinessInterval = 2 * time.Second
	// fastProbeTimeout bounds the readiness monitor's per-tick probe.
	fastProbeTimeout = 2 * time.Second
	// auditTimeout bounds the readiness audit.
	auditTimeout = 3 * time.Second
	// maintenanceLogin is the login every maintenance-credential pool must
	// be (the isolated member of baby_tracker_maintenance).
	maintenanceLogin = "baby_tracker_export_app"
)

// readinessStaleAfter is how old the cached readiness (and the identity
// /v1/server publishes) may be: one whole refresh at its full budgets plus
// headroom, so a slow refresh that succeeds never reads as stale.
var readinessStaleAfter = readiness.StaleBound(readinessInterval, fastProbeTimeout, auditTimeout)

// fastProbeSQL is the monitor's one round trip per tick: the schema version
// and this node's server_state row (absent until `authority init`).
const fastProbeSQL = `SELECT c.current_version, s.instance_id::text, s.recovery_epoch, s.actor_key_commitment
	FROM public.schema_compatibility c LEFT JOIN public.server_state s ON s.singleton WHERE c.singleton`

// readinessPools are the pools node readiness probes, audits and resets.
type readinessPools struct {
	// Readiness is the runtime-credential readiness pool (one connection):
	// the fast probe and the audit run on it and nothing else does, so no
	// background job can starve them.
	Readiness *pgxpool.Pool
	// Maintenance is each distinct maintenance-credential pool (the
	// maintenance pool, and the export pool when it has its own URL). Its
	// workers take a database.WorkerSlots slot, so the audit always finds a
	// connection.
	Maintenance []*pgxpool.Pool
	// MaintenanceLogin is the login each maintenance pool must be.
	MaintenanceLogin string
	// Budgets are the composed pools' connection budgets.
	Budgets []database.NamedPoolBudget
	// Reset are the pools whose idle connections an outage may have broken:
	// they are reset when the fast probe recovers, so a request does not
	// meet a terminated connection (57P01) that the minute-long ShouldPing
	// would not have replaced.
	Reset []*pgxpool.Pool
}

// readinessOption adjusts a databaseReadiness before its monitor is built
// (tests substitute the clock, schema catalog or inventory source).
type readinessOption func(*databaseReadiness)

// databaseReadiness is node readiness, cached off the request path: a
// readiness.Monitor runs the probes below on the readiness pool and
// requests read its cached outcome (Check, Gate, ServerState), so a request
// makes no readiness round trip.
//   - fast (every 2s, one statement): the database answers, the schema is a
//     compatible version, and server_state is this node (instance ID,
//     recovery epoch and the commitment of the configured actor-ID key);
//   - audit (at startup, when the schema version changes, after a fast
//     probe recovers, after a failed audit and every 5 minutes): the
//     declared connection budget holds, the applied migrations are the known
//     ones at that version, the runtime and every maintenance login hold
//     exactly the manifest's privileges, server_state is this node, and the
//     cached credential key inventory (computed by the authority loop)
//     references only configured keys. A step that cannot reach its pool is
//     inconclusive, not drift (readiness.Inconclusive).
type databaseReadiness struct {
	pools            []database.NamedPoolBudget
	budget           config.DatabasePools
	pool             *pgxpool.Pool
	maintenance      []*pgxpool.Pool
	maintenanceLogin string
	reset            []*pgxpool.Pool
	runner           *migrate.Runner
	compatibility    migrate.Compatibility
	instanceID       string
	epoch            uint64
	actorKey         [32]byte
	inventory        *keyInventory
	// background is the authority over the background pool: the key
	// inventory reads through it and the retention loop prunes through it.
	background *authority.Service
	logger     *slog.Logger
	now        func() time.Time
	monitor    *readiness.Monitor
	// connectionLost is set when a fast probe fails on a lost connection
	// and cleared by the reset on recovery. fast and resetPools both run
	// inside the monitor's serialised Refresh.
	connectionLost bool
	// identity is the last fast probe's server identity outcome, which
	// /v1/server publishes (see ServerState).
	identity atomic.Pointer[identityOutcome]
}

// identityOutcome is whether server_state named this node when the fast
// probe last read it.
type identityOutcome struct {
	err error
	at  time.Time
}

func newDatabaseReadiness(cfg config.Config, pools readinessPools, backgroundAuthority *authority.Service,
	logger *slog.Logger, options ...readinessOption) (*databaseReadiness, error) {
	if pools.Readiness == nil || len(pools.Maintenance) == 0 || slices.Contains(pools.Maintenance, nil) || pools.MaintenanceLogin == "" || backgroundAuthority == nil {
		return nil, errors.New("readiness pool, maintenance pools and login, and the background authority are required")
	}
	compatibility, err := migrate.EmbeddedCompatibility()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	r := &databaseReadiness{pools: pools.Budgets, budget: cfg.DatabasePools, pool: pools.Readiness, maintenance: pools.Maintenance,
		maintenanceLogin: pools.MaintenanceLogin, reset: slices.DeleteFunc(slices.Clone(pools.Reset), func(p *pgxpool.Pool) bool { return p == nil }),
		runner: migrate.New(migrations.Files), compatibility: compatibility,
		instanceID: cfg.ServerInstanceID, epoch: cfg.RecoveryEpoch, actorKey: cfg.Auth.ActorIDPepper,
		// The session lifetime the inventory applies is the one refresh
		// enforces (development uses the self-hosted policy).
		inventory: newKeyInventory(cfg.Auth, func(ctx context.Context) (authority.KeyInventory, error) {
			return backgroundAuthority.ReferencedKeyIDs(ctx, authority.Mode(authenticationMode(cfg.Mode)))
		}), background: backgroundAuthority, logger: logger, now: time.Now}
	for _, option := range options {
		option(r)
	}
	r.monitor, err = readiness.New(readiness.Options{Fast: r.fast, Audit: r.audit, FastInterval: readinessInterval, Stale: readinessStaleAfter,
		Recovered: r.resetPools, Now: r.now, Logger: r.logger})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Check is the readiness gate: the monitor's cached outcome, no round trip.
func (r *databaseReadiness) Check(ctx context.Context) error { return r.monitor.Check(ctx) }

// Refresh runs the monitor's probes once (the fast probe, and the audit
// when it is due).
func (r *databaseReadiness) Refresh(ctx context.Context) error { return r.monitor.Refresh(ctx) }

// Run refreshes readiness every 2 seconds until ctx ends.
func (r *databaseReadiness) Run(ctx context.Context) { r.monitor.Run(ctx) }

// resetPools drops every pooled connection of the reset pools when the
// fast probe has just recovered from a lost connection (an outage that may
// have terminated them). A probe that only timed out, or a schema or
// identity refusal, leaves the pools alone.
func (r *databaseReadiness) resetPools() {
	if !r.connectionLost {
		return
	}
	r.connectionLost = false
	for _, pool := range r.reset {
		pool.Reset()
	}
	if len(r.reset) > 0 {
		r.logger.Info("database reachable again; pooled connections reset", slog.Int("pools", len(r.reset)))
	}
}

func (r *databaseReadiness) fast(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, fastProbeTimeout)
	defer cancel()
	var version int64
	var instanceID *string
	var epoch *int64
	var commitment []byte
	if err := r.pool.QueryRow(ctx, fastProbeSQL).Scan(&version, &instanceID, &epoch, &commitment); err != nil {
		if database.ConnectionLost(err) {
			r.connectionLost = true
		}
		r.identity.Store(&identityOutcome{err: database.ErrUnavailable, at: r.now()})
		return 0, database.Unavailable(err)
	}
	var state authority.ServerState
	identityErr := error(nil)
	switch {
	case instanceID == nil:
		identityErr = authority.ErrServerStateMissing
	case epoch == nil || *epoch < 1:
		identityErr = authority.ErrServerIdentityMismatch
	default:
		state = authority.ServerState{InstanceID: *instanceID, RecoveryEpoch: uint64(*epoch), ActorKeyCommitment: commitment}
		if state.InstanceID != r.instanceID || state.RecoveryEpoch != r.epoch {
			identityErr = authority.ErrServerIdentityMismatch
		}
	}
	r.identity.Store(&identityOutcome{err: identityErr, at: r.now()})
	if version < r.compatibility.Min || version > r.compatibility.Max {
		return 0, fmt.Errorf("%w: schema_incompatible", database.ErrUnavailable)
	}
	if identityErr != nil {
		return 0, identityErr
	}
	if err := state.Check(r.instanceID, r.epoch, r.actorKey); err != nil {
		return 0, err
	}
	return version, nil
}

// auditStep classifies a failed audit step: when the database could not be
// reached (database.Unreachable) the step reached no verdict; any other
// failure (a refused query, 42501 for a revoked privilege) is a verdict that
// fails closed. The log line names the SQLSTATE, never the server's text.
func auditStep(step string, err error) error {
	if database.Unreachable(err) {
		return readiness.Inconclusive(fmt.Errorf("%w: %s unavailable", database.ErrUnavailable, step))
	}
	return fmt.Errorf("%w: %s refused%s", database.ErrUnavailable, step, sqlState(err))
}

// sqlState is " (SQLSTATE x)" for an error the server answered with.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return " (SQLSTATE " + pgErr.Code + ")"
	}
	return ""
}

// schemaCheck classifies a CompatibleVersion failure: a validation failure
// (no database cause) is schema_incompatible; a database error is a step.
func schemaCheck(err error) error {
	if errors.Unwrap(err) == nil {
		return fmt.Errorf("%w: schema_incompatible", database.ErrUnavailable)
	}
	return auditStep("schema check", err)
}

func (r *databaseReadiness) audit(ctx context.Context, version int64) error {
	ctx, cancel := context.WithTimeout(ctx, auditTimeout)
	defer cancel()
	if r.budget.Explicit() {
		if err := database.ValidateBudget(r.pools, r.budget.ServerMax, r.budget.Reserved); err != nil {
			return database.ErrUnavailable
		}
		var actualMax int32
		if err := r.pool.QueryRow(ctx, "SELECT current_setting('max_connections')::integer").Scan(&actualMax); err != nil {
			return auditStep("connection ceiling check", err)
		}
		if actualMax < r.budget.ServerMax {
			return database.ErrUnavailable
		}
	}
	applied, err := r.runner.CompatibleVersion(ctx, r.pool, r.compatibility)
	if err != nil {
		return schemaCheck(err)
	}
	if applied != version {
		return fmt.Errorf("%w: schema_changed", database.ErrUnavailable)
	}
	if err := database.VerifyRuntimeRole(ctx, r.pool, version); err != nil {
		if errors.Is(err, database.ErrUnavailable) {
			return auditStep("runtime role check", err)
		}
		return fmt.Errorf("%w: runtime %v", database.ErrUnavailable, err)
	}
	for _, maintenance := range r.maintenance {
		if err := verifyMaintenancePool(ctx, maintenance, r.maintenanceLogin); err != nil {
			if errors.Is(err, errMaintenanceUnavailable) {
				return readiness.Inconclusive(err)
			}
			return fmt.Errorf("%w: %v", database.ErrUnavailable, err)
		}
		if err := database.VerifyMaintenanceRole(ctx, r.pool, maintenance, version); err != nil {
			if errors.Is(err, database.ErrUnavailable) {
				return auditStep("maintenance role check", err)
			}
			return fmt.Errorf("%w: maintenance %v", database.ErrUnavailable, err)
		}
	}
	state, err := authority.LoadServerState(ctx, r.pool)
	if err != nil {
		if errors.Is(err, authority.ErrUnavailable) {
			return auditStep("server state check", err)
		}
		return err
	}
	if err := state.Check(r.instanceID, r.epoch, r.actorKey); err != nil {
		return err
	}
	confirmed, err := r.runner.CompatibleVersion(ctx, r.pool, r.compatibility)
	if err != nil {
		return schemaCheck(err)
	}
	if confirmed != version {
		return fmt.Errorf("%w: schema_changed", database.ErrUnavailable)
	}
	// Until the inventory is first computed, the audit asks the authority
	// loop (the inventory's only computation, on the background pool) to
	// compute it now rather than at its next retry.
	if !r.inventory.wasComputed() {
		r.inventory.Nudge()
	}
	return r.inventory.Check()
}

// ServerState reports the node identity /v1/server publishes: the
// configured instance ID and epoch, but only while server_state holds the
// same ones (a node whose database belongs to another instance or epoch is
// not ready). It is the fast probe's cached outcome, refused when older
// than the readiness staleness bound.
func (r *databaseReadiness) ServerState(context.Context) (string, uint64, error) {
	outcome := r.identity.Load()
	switch {
	case outcome == nil:
		return "", 0, readiness.ErrNotReady
	case outcome.err != nil:
		return "", 0, outcome.err
	case r.now().Sub(outcome.at) > readinessStaleAfter:
		return "", 0, readiness.ErrStale
	}
	return r.instanceID, r.epoch, nil
}

// keyInventory caches whether every key ID a live credential references is
// configured, so an operator cannot retire a key that still verifies a
// credential. Credentials are only issued under configured keys, so the
// answer can only improve while the configuration is unchanged (and the
// configuration only changes on restart, which recomputes it before
// serving). A failed refresh keeps the last answer.
//
// After startup the authority loop is its only computation (single
// flight): the readiness audit only nudges the loop.
type keyInventory struct {
	read                                  func(context.Context) (authority.KeyInventory, error)
	session, device, identity, invitation map[string]struct{}
	mu                                    sync.RWMutex
	computed                              bool
	state                                 error
	nudge                                 chan struct{}
}

func newKeyInventory(keys config.AuthConfig, read func(context.Context) (authority.KeyInventory, error)) *keyInventory {
	return &keyInventory{read: read, session: configuredKeyIDs(keys.SessionTokenKeys), device: configuredKeyIDs(keys.DeviceContinuityKeys),
		identity: configuredKeyIDs(keys.IdentityKeys), invitation: configuredKeyIDs(keys.InvitationKeys), state: errKeyInventoryPending,
		nudge: make(chan struct{}, 1)}
}

// Nudge asks the authority loop to compute the inventory now; it never
// blocks and never computes itself.
func (k *keyInventory) Nudge() {
	if k == nil {
		return
	}
	select {
	case k.nudge <- struct{}{}:
	default:
	}
}

// Refresh reads the inventory once and stores the verdict.
func (k *keyInventory) Refresh(ctx context.Context) error {
	if k == nil || k.read == nil {
		return errKeyInventoryPending
	}
	referenced, err := k.read(ctx)
	if err != nil {
		return err
	}
	verdict := k.compare(referenced)
	k.mu.Lock()
	k.computed, k.state = true, verdict
	k.mu.Unlock()
	return nil
}

func (k *keyInventory) compare(referenced authority.KeyInventory) error {
	for _, class := range []struct {
		name       string
		configured map[string]struct{}
		referenced []string
	}{
		{"session_token", k.session, referenced.SessionToken}, {"device_continuity", k.device, referenced.DeviceContinuity},
		{"identity", k.identity, referenced.Identity}, {"invitation", k.invitation, referenced.Invitation},
	} {
		for _, id := range class.referenced {
			if _, ok := class.configured[id]; !ok {
				return fmt.Errorf("%w: %s key %s", authority.ErrCredentialKeyUnavailable, class.name, id)
			}
		}
	}
	return nil
}

// Check is the cached verdict: nil when every referenced key is configured.
func (k *keyInventory) Check() error {
	if k == nil {
		return errKeyInventoryPending
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.state
}

func (k *keyInventory) wasComputed() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.computed
}

// startAuthorityLoops runs the audit chain verifier (incremental passes)
// and the key inventory refresh until ctx ends; the returned stop waits for
// both. They share the one-connection background pool, so the inventory
// runs half an interval out of phase with the verifier (which starts at
// once).
func startAuthorityLoops(ctx context.Context, verifier *authority.Verifier, inventory *keyInventory, logger *slog.Logger) func() {
	loopContext, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	if verifier != nil {
		wg.Go(func() { verifier.Run(loopContext, authorityLoopInterval) })
	}
	if inventory != nil {
		wg.Go(func() {
			runKeyInventoryLoop(loopContext, inventory, authorityLoopInterval, keyInventoryRetryInterval, authorityLoopInterval/2, logger)
		})
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

// runKeyInventoryLoop refreshes the inventory: first after offset, then
// every interval, every retry while it has never been computed, and at once
// when nudged.
func runKeyInventoryLoop(ctx context.Context, inventory *keyInventory, interval, retry, offset time.Duration, logger *slog.Logger) {
	wait := offset
	for {
		if !inventory.wasComputed() {
			wait = retry
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-inventory.nudge:
			timer.Stop()
		}
		if err := inventory.Refresh(ctx); err != nil && ctx.Err() == nil && logger != nil {
			logger.Warn("credential key inventory refresh failed", slog.String("error", err.Error()))
		}
		wait = interval
	}
}

// nodeReadinessCovered is the recovery-specific readiness: recovery needs
// nothing beyond the node readiness, which every recovery route and the
// capability check read themselves (the cached state), so it is not read a
// second time per request.
type nodeReadinessCovered struct{}

func (nodeReadinessCovered) Check(context.Context) error { return nil }
