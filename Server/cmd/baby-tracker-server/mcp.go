package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/familyread"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/mcpapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mcpPoolIdentity names the dedicated MCP pool in the aggregate budget.
const mcpPoolIdentity = "mcp"

// openMCPPool opens the dedicated MCP pool only when MCP is enabled. Disabled
// mode reads no secret and opens nothing. The pool is configured separately
// from BT_MCP_DATABASE_URL_FILE (never the runtime pool object or a copy of
// its config), lazily connects, holds exactly the explicit MCP budget and
// makes every session default to read-only transactions.
func openMCPPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	if !cfg.MCP.Enabled {
		return nil, nil
	}
	if cfg.DatabasePools.MCP < 1 || cfg.DatabasePools.MCP > config.MaxMCPConnections {
		return nil, errors.New("mcp database pool budget must be from 1 through 4")
	}
	return database.OpenReadOnlyWithMax(ctx, cfg.MCP.DatabaseURL, cfg.DatabasePools.MCP)
}

type mcpDependencies struct {
	// Authority is the server's security authority (the guard's lease source).
	Authority *authority.Service
	// Pool is the dedicated read-only MCP pool from openMCPPool.
	Pool *pgxpool.Pool
	// Others are every other composed pool; the MCP pool must differ from all.
	Others []*pgxpool.Pool
	// Admission is the shared finite-work controller (one MCP slot/request).
	Admission *admission.Controller
	// Limiter is the existing limiter; quota reservations are operational
	// metadata on the runtime pool, never care-record writes.
	Limiter limits.Limiter
	// Metrics, when set, receives fixed-class MCP admission counts.
	Metrics *diagnostics.CapacityCollector
	// Recorder, when set, receives the MCP guard's phase timings and
	// integrity counters through the deferred Flush contract.
	Recorder diagnostics.Recorder
	// Readiness is the node readiness; every MCP request is refused (503)
	// while the node is not ready. attachMCP sets it from the router's.
	Readiness mcpapi.Readiness
	Now       func() time.Time
}

type mcpComposition struct {
	handler http.Handler
	reader  *authority.IntegrationReader
	gate    *admission.Keyed
}

// composeMCP builds the private read-only MCP handler: the guarded reader on
// the dedicated read-only pool, familyread, the per-credential keyed registry
// (one in-flight request per grant, bounded by the global MCP maximum), the
// fixed integration/account/family quotas and one shared MCP admission slot.
func composeMCP(cfg config.Config, deps mcpDependencies) (*mcpComposition, error) {
	if !cfg.MCP.Enabled {
		return nil, errors.New("mcp is disabled")
	}
	if cfg.PublicOrigin == nil {
		return nil, errors.New("mcp requires the canonical private origin")
	}
	if deps.Pool == nil {
		return nil, errors.New("mcp requires its dedicated database pool")
	}
	if !database.ReadOnlyByDefault(deps.Pool) {
		return nil, errors.New("mcp database pool must default to read-only transactions")
	}
	if deps.Pool.Config().MaxConns > config.MaxMCPConnections {
		return nil, errors.New("mcp database pool must hold at most four connections")
	}
	for _, other := range deps.Others {
		if other == deps.Pool {
			return nil, errors.New("mcp database pool must be dedicated, not a shared pool")
		}
	}
	if deps.Admission == nil {
		return nil, errors.New("mcp requires the shared admission controller")
	}
	if deps.Limiter == nil {
		return nil, errors.New("mcp requires the rate limiter")
	}
	if cfg.Admission.MCPMax < 1 || cfg.Admission.MCPMax > config.MaxMCPConnections {
		return nil, errors.New("mcp admission maximum must be from 1 through 4")
	}
	quota, err := newMCPQuota(deps.Limiter, cfg.Limits.Policies)
	if err != nil {
		return nil, err
	}
	reader, err := authority.NewIntegrationReader(deps.Authority, deps.Pool)
	if err != nil {
		return nil, err
	}
	// Every registry holder also holds one global MCP slot, so the registry
	// can never need more entries than the global MCP maximum.
	gate, err := admission.NewKeyed(cfg.Admission.MCPMax)
	if err != nil {
		return nil, err
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	handler, err := mcpapi.NewHandler(mcpapi.Options{
		Reader: familyread.New(reader, cfg.MCP.CursorKey, now), Authenticator: reader,
		Admission: observedAdmission{controller: deps.Admission, metrics: deps.Metrics},
		Gate:      gate, Quota: quota, Readiness: deps.Readiness, CanonicalOrigin: cfg.PublicOrigin.String(),
	})
	if err != nil {
		return nil, err
	}
	if deps.Recorder != nil {
		handler = recordedMCP(handler, deps.Recorder)
	}
	return &mcpComposition{handler: handler, reader: reader, gate: gate}, nil
}

// attachMCP mounts /mcp on options when MCP is enabled, using the router's
// own shared admission controller and limiter. Disabled leaves the route
// absent (404).
func attachMCP(cfg config.Config, options *httpapi.Options, deps mcpDependencies) error {
	if !cfg.MCP.Enabled {
		return nil
	}
	if options == nil || options.Admission == nil {
		return errors.New("mcp requires the composed router options")
	}
	deps.Admission = options.Admission
	if deps.Limiter == nil {
		deps.Limiter = options.Limiter
	}
	if deps.Metrics == nil {
		deps.Metrics = options.AdmissionMetrics
	}
	if options.Readiness == nil {
		return errors.New("mcp requires the node readiness gate")
	}
	deps.Readiness = options.Readiness
	composition, err := composeMCP(cfg, deps)
	if err != nil {
		return err
	}
	options.MCPHandler = composition.handler
	return nil
}

// recordedMCP opts each MCP request into the deferred diagnostics recorder
// and flushes exactly once, after the handler (and with it every guard lease
// and read transaction) has returned.
func recordedMCP(next http.Handler, recorder diagnostics.Recorder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := diagnostics.WithRecorder(r.Context(), recorder)
		defer diagnostics.Flush(ctx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// mcpQuota charges the integration, owner-account and family quotas together
// (all or none) for one authenticated MCP request.
type mcpQuota struct {
	limiter                      limits.Limiter
	integration, account, family limits.Policy
}

func newMCPQuota(limiter limits.Limiter, policies map[string]limits.Policy) (mcpQuota, error) {
	integration, okIntegration := policies[limits.ScopeMCPIntegration]
	account, okAccount := policies[limits.ScopeMCPAccount]
	family, okFamily := policies[limits.ScopeMCPFamily]
	if !okIntegration || !okAccount || !okFamily {
		return mcpQuota{}, errors.New("mcp rate-limit policies are required")
	}
	return mcpQuota{limiter: limiter, integration: integration, account: account, family: family}, nil
}

func (q mcpQuota) Charge(ctx context.Context, identity authority.IntegrationIdentity) (limits.Decision, error) {
	if identity.GrantID == "" || identity.AccountID == "" || identity.FamilyID == "" {
		return limits.Decision{}, errors.New("mcp quota requires an authenticated identity")
	}
	return q.limiter.TakeMany(ctx, []limits.Charge{
		{Scope: limits.ScopeMCPIntegration, Key: identity.GrantID, Cost: 1, Policy: q.integration},
		{Scope: limits.ScopeMCPAccount, Key: identity.AccountID, Cost: 1, Policy: q.account},
		{Scope: limits.ScopeMCPFamily, Key: identity.FamilyID, Cost: 1, Policy: q.family},
	})
}

// observedAdmission records fixed-class MCP admission outcomes on the shared
// collector; the controller itself is the router's shared controller.
type observedAdmission struct {
	controller *admission.Controller
	metrics    *diagnostics.CapacityCollector
}

func (a observedAdmission) TryAcquire(class admission.Class) (func(), bool) {
	release, ok := a.controller.TryAcquire(class)
	if a.metrics == nil || class != admission.MCP {
		return release, ok
	}
	if !ok {
		a.metrics.ObserveAdmission(diagnostics.MCPAdmission, diagnostics.AdmissionRejected)
		return release, ok
	}
	a.metrics.ObserveAdmission(diagnostics.MCPAdmission, diagnostics.AdmissionAccepted)
	var once sync.Once
	return func() {
		once.Do(func() {
			release()
			a.metrics.ObserveAdmission(diagnostics.MCPAdmission, diagnostics.AdmissionReleased)
		})
	}, true
}

// serverPoolBudgets lists every actual composed pool once, by identity. The
// dedicated MCP pool counts in the aggregate budget under its own identity.
func serverPoolBudgets(runtime, mcp, readiness, background *pgxpool.Pool, maintenance ...*pgxpool.Pool) []database.NamedPoolBudget {
	budgets := actualPoolBudgets(runtime, readiness, background, maintenance...)
	if mcp == nil || mcp == runtime || mcp == readiness || mcp == background {
		return budgets
	}
	for _, pool := range maintenance {
		if pool == mcp {
			return budgets
		}
	}
	return append(budgets, database.NamedPoolBudget{Identity: mcpPoolIdentity, Max: mcp.Config().MaxConns})
}
