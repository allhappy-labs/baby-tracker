// Package readiness owns the node's one cached readiness state. A background
// monitor probes the database on a short interval and audits the role
// privileges at startup, after a schema change and every few minutes;
// requests only read the cached outcome, so nothing readiness-related runs
// per request.
package readiness

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"babytracker/server/internal/database"
)

// unavailable is a readiness refusal that reads as service_unavailable and
// wraps database.ErrUnavailable.
type unavailable struct{ text string }

func (e *unavailable) Error() string { return e.text }
func (e *unavailable) Unwrap() error { return database.ErrUnavailable }

var (
	// ErrNotReady is the state before the first probe completed.
	ErrNotReady error = &unavailable{"service_unavailable: readiness not established"}
	// ErrStale is a state older than Options.Stale: the monitor stopped
	// refreshing (a hung probe), so the last outcome is not trusted.
	ErrStale error = &unavailable{"service_unavailable: readiness stale"}
	// ErrInconclusive marks an audit that reached no verdict: a pool it
	// needed was busy or unreachable. It is not a verdict of drift (see
	// Options.AuditGrace).
	ErrInconclusive error = &unavailable{"service_unavailable: readiness audit inconclusive"}
)

// Inconclusive marks err as an audit that reached no verdict.
func Inconclusive(err error) error {
	if err == nil {
		return nil
	}
	return inconclusive{err}
}

type inconclusive struct{ err error }

func (e inconclusive) Error() string   { return ErrInconclusive.Error() + ": " + e.err.Error() }
func (e inconclusive) Unwrap() []error { return []error{ErrInconclusive, e.err} }

// StaleBound is the staleness bound that covers one whole refresh: the
// wait for the next tick, the fast probe's and the audit's budgets, and one
// more interval of headroom, so a slow refresh that succeeds within its
// budgets never exposes ErrStale.
func StaleBound(fastInterval, fastBudget, auditBudget time.Duration) time.Duration {
	return fastInterval + fastBudget + auditBudget + fastInterval
}

const (
	defaultFastInterval  = 2 * time.Second
	defaultAuditInterval = 5 * time.Minute
	defaultAuditGrace    = 10 * time.Minute
)

// Options are the monitor's probes and cadence.
type Options struct {
	// Fast is one round trip: it pings the database and reads the schema
	// version (and anything else one statement can check).
	Fast func(context.Context) (version int64, err error)
	// Audit checks what is too costly per tick: role privileges,
	// server_state, migration digests, the connection budget and the key
	// inventory, against the version Fast read.
	Audit func(context.Context, int64) error
	// FastInterval is the Run cadence; 0 means 2s.
	FastInterval time.Duration
	// AuditInterval is the longest time between audits; 0 means 5m.
	AuditInterval time.Duration
	// Stale is how old the cached state may be before Check refuses it;
	// 0 means 3 × FastInterval (see StaleBound).
	Stale time.Duration
	// AuditGrace is how long the last good audit verdict is kept while
	// audits are inconclusive (ErrInconclusive); 0 means 10m. A verdict of
	// drift, a schema change or an older verdict fails closed.
	AuditGrace time.Duration
	// Recovered, when set, runs after a fast probe succeeds following a
	// failed one (the outage may have broken pooled connections).
	Recovered func()
	Now       func() time.Time
	Logger    *slog.Logger
}

// state is one stored outcome: the refusal (nil when ready), the version
// the fast probe read and when the refresh completed.
type state struct {
	err     error
	version int64
	at      time.Time
	// staleLogged is set by the first Check that finds this state stale.
	staleLogged atomic.Bool
}

// Monitor caches node readiness. Refresh is serialised; Check is lock-free.
type Monitor struct {
	options Options
	current atomic.Pointer[state]

	mu             sync.Mutex // serialises Refresh and guards the fields below
	audited        bool       // an audit has succeeded
	auditedVersion int64      // the version the last successful audit saw
	goodAt         time.Time  // when the last successful audit ran
	verdictFailed  bool       // the last conclusive audit failed (drift)
	auditFailed    bool       // the last audit failed or was inconclusive
	auditedAt      time.Time  // when the last audit ran
	fastFailed     bool       // the last fast probe failed
	inconclusive   bool       // an inconclusive streak is being served from the last verdict
	lastReported   *string    // the last logged outcome ("" ready, nil none)
}

// New validates the options and returns a monitor that is not ready until
// its first Refresh.
func New(o Options) (*Monitor, error) {
	if o.Fast == nil || o.Audit == nil {
		return nil, errors.New("readiness: fast and audit probes are required")
	}
	if o.FastInterval < 0 || o.AuditInterval < 0 || o.Stale < 0 || o.AuditGrace < 0 {
		return nil, errors.New("readiness: intervals must not be negative")
	}
	if o.FastInterval == 0 {
		o.FastInterval = defaultFastInterval
	}
	if o.AuditInterval == 0 {
		o.AuditInterval = defaultAuditInterval
	}
	if o.Stale == 0 {
		o.Stale = 3 * o.FastInterval
	}
	if o.AuditGrace == 0 {
		o.AuditGrace = defaultAuditGrace
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Monitor{options: o}, nil
}

// Refresh runs one fast probe and, when it succeeds and an audit is due,
// the audit. An audit is due when none has succeeded, when the last one
// failed or was inconclusive, when the fast probe failed before this one
// (the database that came back may be another cluster), when the schema
// version changed since the last successful one, or when AuditInterval has
// passed since the last one. An inconclusive audit keeps the last good
// verdict while it is younger than AuditGrace (same schema version, no
// drift verdict since). The outcome is stored for Check and returned; a
// change of outcome is logged once.
func (m *Monitor) Refresh(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	version, err := m.options.Fast(ctx)
	if err != nil {
		m.fastFailed = true
	} else {
		recovered := m.fastFailed
		m.fastFailed = false
		if recovered && m.options.Recovered != nil {
			m.options.Recovered()
		}
		now := m.options.Now()
		if !m.audited || m.auditFailed || recovered || version != m.auditedVersion || now.Sub(m.auditedAt) >= m.options.AuditInterval {
			err = m.audit(ctx, version, now)
		}
	}
	m.current.Store(&state{err: err, version: version, at: m.options.Now()})
	m.report(err)
	return err
}

// audit runs the audit and returns the outcome to serve.
func (m *Monitor) audit(ctx context.Context, version int64, now time.Time) error {
	err := m.options.Audit(ctx, version)
	m.auditedAt, m.auditFailed = now, err != nil
	switch {
	case err == nil:
		m.audited, m.auditedVersion, m.goodAt, m.verdictFailed, m.inconclusive = true, version, now, false, false
		return nil
	case !errors.Is(err, ErrInconclusive):
		m.verdictFailed, m.inconclusive = true, false
		return err
	case m.audited && !m.verdictFailed && version == m.auditedVersion && now.Sub(m.goodAt) < m.options.AuditGrace:
		if !m.inconclusive {
			m.inconclusive = true
			m.options.Logger.Warn("readiness audit inconclusive; serving the last good verdict",
				slog.String("reason", err.Error()), slog.Time("verdict_at", m.goodAt))
		}
		return nil
	default:
		m.inconclusive = false
		return err
	}
}

func (m *Monitor) report(err error) {
	outcome := ""
	if err != nil {
		outcome = err.Error()
	}
	if m.lastReported != nil && *m.lastReported == outcome {
		return
	}
	m.lastReported = &outcome
	if err != nil {
		m.options.Logger.Error("node not ready", slog.String("reason", outcome))
		return
	}
	m.options.Logger.Info("node ready")
}

// Run refreshes every FastInterval until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.options.FastInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = m.Refresh(ctx)
		}
	}
}

// Check is the cached readiness: nil when ready, the last refusal, ErrNotReady
// before the first refresh, or ErrStale when the last refresh is older than
// Stale. It touches no database.
func (m *Monitor) Check(context.Context) error {
	current := m.current.Load()
	if current == nil {
		return ErrNotReady
	}
	if current.err != nil {
		return current.err
	}
	if m.options.Now().Sub(current.at) > m.options.Stale {
		if current.staleLogged.CompareAndSwap(false, true) {
			m.options.Logger.Error("readiness stale: the monitor has not refreshed", slog.Time("refreshed_at", current.at))
		}
		return ErrStale
	}
	return nil
}
