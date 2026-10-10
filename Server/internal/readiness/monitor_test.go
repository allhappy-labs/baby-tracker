package readiness_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/readiness"
)

type probes struct {
	mu            sync.Mutex
	version       int64
	fastErr       error
	auditErr      error
	fasts, audits int
}

func (p *probes) fast(context.Context) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fasts++
	return p.version, p.fastErr
}

func (p *probes) audit(context.Context, int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.audits++
	return p.auditErr
}

func (p *probes) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fasts, p.audits
}

func TestCheckIsNotReadyBeforeTheFirstAudit(t *testing.T) {
	p := &probes{version: 31}
	m, err := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Check(t.Context()); !errors.Is(err, readiness.ErrNotReady) {
		t.Fatalf("check before any probe: %v", err)
	}
	if fasts, audits := p.counts(); fasts != 0 || audits != 0 {
		t.Fatalf("check ran probes: fast %d audit %d", fasts, audits)
	}
}

func TestCheckServesTheCachedState(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: func() time.Time { return now }})
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		if err := m.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if p.fasts != 1 || p.audits != 1 {
		t.Fatalf("checks ran probes: fast %d audit %d", p.fasts, p.audits)
	}
}

func TestAuditRunsAtStartOnSchemaChangeAndEveryInterval(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, AuditInterval: 5 * time.Minute, Now: func() time.Time { return now }})
	_ = m.Refresh(t.Context()) // start: audit
	now = now.Add(2 * time.Second)
	_ = m.Refresh(t.Context()) // fast only
	p.version = 32             // a migration ran
	now = now.Add(2 * time.Second)
	_ = m.Refresh(t.Context()) // audit
	now = now.Add(5 * time.Minute)
	_ = m.Refresh(t.Context()) // audit (interval)
	if p.fasts != 4 || p.audits != 3 {
		t.Fatalf("fast %d audit %d, want 4 and 3", p.fasts, p.audits)
	}
	// Just short of the interval since the last audit: fast only.
	now = now.Add(5*time.Minute - time.Second)
	_ = m.Refresh(t.Context())
	if p.fasts != 5 || p.audits != 3 {
		t.Fatalf("fast %d audit %d, want 5 and 3", p.fasts, p.audits)
	}
}

// The default audit interval is five minutes.
func TestAuditIntervalDefaultsToFiveMinutes(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: func() time.Time { return now }})
	_ = m.Refresh(t.Context())
	now = now.Add(5*time.Minute - time.Second)
	_ = m.Refresh(t.Context())
	if _, audits := p.counts(); audits != 1 {
		t.Fatalf("audited %d times before the default interval", audits)
	}
	now = now.Add(time.Second)
	_ = m.Refresh(t.Context())
	if _, audits := p.counts(); audits != 2 {
		t.Fatalf("audited %d times at the default interval", audits)
	}
}

func TestStaleOrFailedStateFailsClosed(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, FastInterval: 2 * time.Second, Now: func() time.Time { return now }})
	_ = m.Refresh(t.Context())
	now = now.Add(6 * time.Second) // exactly three intervals: still fresh
	if err := m.Check(t.Context()); err != nil {
		t.Fatalf("state within three intervals refused: %v", err)
	}
	now = now.Add(time.Second) // no probe for more than 3 intervals
	if err := m.Check(t.Context()); !errors.Is(err, readiness.ErrStale) {
		t.Fatalf("stale state served: %v", err)
	}
	p.auditErr = errors.New("database_role_separation_invalid")
	p.version = 33
	_ = m.Refresh(t.Context())
	if err := m.Check(t.Context()); err == nil {
		t.Fatal("a failed audit was served as ready")
	}
	p.auditErr = nil
	_ = m.Refresh(t.Context()) // the failed audit is retried on the next tick
	if err := m.Check(t.Context()); err != nil {
		t.Fatalf("recovered audit still refused: %v", err)
	}
}

// A failed audit (drift found at the periodic audit, schema unchanged)
// stays refused: every following tick re-runs the audit, never serving the
// fast probe's success, until the audit passes again.
func TestFailedAuditIsRetriedEveryTickUntilItPasses(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: func() time.Time { return now }})
	_ = m.Refresh(t.Context())
	now = now.Add(5 * time.Minute)
	p.auditErr = errors.New("database_role_separation_invalid")
	for tick := range 3 {
		_ = m.Refresh(t.Context())
		if err := m.Check(t.Context()); err == nil {
			t.Fatalf("tick %d served ready while the audit fails", tick)
		}
		now = now.Add(2 * time.Second)
	}
	if _, audits := p.counts(); audits != 4 {
		t.Fatalf("failed audit retried %d times, want every tick", audits-1)
	}
	p.auditErr = nil
	_ = m.Refresh(t.Context())
	if err := m.Check(t.Context()); err != nil {
		t.Fatalf("passing audit still refused: %v", err)
	}
	now = now.Add(2 * time.Second)
	_ = m.Refresh(t.Context())
	if _, audits := p.counts(); audits != 5 {
		t.Fatalf("audit ran after it passed: %d", audits)
	}
}

// A failed fast probe is served as the refusal and runs no audit; when the
// database comes back the next fast probe serves ready again without an
// audit (the audited version is unchanged).
func TestFastFailureFailsClosedAndRecovers(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: func() time.Time { return now }})
	_ = m.Refresh(t.Context())
	down := errors.Join(errors.New("dial"), database.ErrUnavailable)
	p.fastErr = down
	now = now.Add(2 * time.Second)
	if err := m.Refresh(t.Context()); !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("refresh with the database down: %v", err)
	}
	if err := m.Check(t.Context()); !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("check with the database down: %v", err)
	}
	if _, audits := p.counts(); audits != 1 {
		t.Fatalf("a failed fast probe ran the audit: %d", audits)
	}
	p.fastErr = nil
	now = now.Add(2 * time.Second)
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh after the database came back: %v", err)
	}
	if err := m.Check(t.Context()); err != nil {
		t.Fatalf("check after the database came back: %v", err)
	}
	// The database that came back may be another cluster (a failover or a
	// restore) with other grants: recovery re-audits even at the same
	// schema version, once.
	if _, audits := p.counts(); audits != 2 {
		t.Fatalf("recovery audited %d times in all, want 2", audits)
	}
	now = now.Add(2 * time.Second)
	_ = m.Refresh(t.Context())
	if _, audits := p.counts(); audits != 2 {
		t.Fatalf("steady ticks after recovery re-audited: %d", audits)
	}
}

// Recovered runs once each time the fast probe succeeds after failing (so
// the composition can drop pooled connections the outage broke), never on
// steady success.
func TestRecoveredRunsOnceWhenTheFastProbeRecovers(t *testing.T) {
	p := &probes{version: 31}
	recovered := 0
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: time.Now, Recovered: func() { recovered++ }})
	for range 3 {
		_ = m.Refresh(t.Context())
	}
	if recovered != 0 {
		t.Fatalf("recovered on steady success: %d", recovered)
	}
	for outage := 1; outage <= 2; outage++ {
		p.fastErr = database.ErrUnavailable
		_ = m.Refresh(t.Context())
		_ = m.Refresh(t.Context())
		p.fastErr = nil
		for range 3 {
			_ = m.Refresh(t.Context())
		}
		if recovered != outage {
			t.Fatalf("after outage %d recovered %d times", outage, recovered)
		}
	}
}

// An audit that reaches no verdict (a busy or unreachable pool, not drift)
// keeps the last good verdict while it is younger than the grace (10 min by
// default), retrying every tick; after that it fails closed.
func TestInconclusiveAuditKeepsTheLastGoodVerdictForTheGrace(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	_ = m.Refresh(t.Context()) // good verdict at 12:00
	busy := readiness.Inconclusive(errors.New("maintenance database unavailable"))
	p.auditErr = busy
	now = now.Add(5 * time.Minute) // periodic audit: inconclusive
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatalf("inconclusive audit within the grace refused: %v", err)
	}
	for now.Add(2*time.Second).Sub(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)) < 10*time.Minute {
		now = now.Add(2 * time.Second)
		if err := refreshAndCheck(t, m); err != nil {
			t.Fatalf("at %v within the grace: %v", now, err)
		}
	}
	if _, audits := p.counts(); audits < 100 {
		t.Fatalf("inconclusive audit not retried every tick: %d audits", audits)
	}
	if strings.Count(logs.String(), "serving the last good verdict") != 1 {
		t.Fatalf("inconclusive streak not logged once: %s", logs.String())
	}
	now = now.Add(2 * time.Second) // the last good verdict is now 10 minutes old
	err := refreshAndCheck(t, m)
	if !errors.Is(err, readiness.ErrInconclusive) || !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("inconclusive audit past the grace: %v", err)
	}
	p.auditErr = nil
	now = now.Add(2 * time.Second)
	if err := refreshAndCheck(t, m); err != nil {
		t.Fatalf("conclusive audit after the grace: %v", err)
	}
}

// An inconclusive audit never vouches for a node that has no good verdict
// for this schema: none yet, a drift verdict, or another schema version.
func TestInconclusiveAuditNeverOverridesAFailedOrMissingVerdict(t *testing.T) {
	busy := readiness.Inconclusive(errors.New("maintenance database unavailable"))
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	t.Run("no verdict yet", func(t *testing.T) {
		p := &probes{version: 31, auditErr: busy}
		m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: clock})
		if err := refreshAndCheck(t, m); !errors.Is(err, readiness.ErrInconclusive) {
			t.Fatalf("first audit inconclusive: %v", err)
		}
	})
	t.Run("drift verdict", func(t *testing.T) {
		p := &probes{version: 31}
		m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: clock})
		_ = m.Refresh(t.Context())
		now = now.Add(5 * time.Minute)
		drift := errors.New("database_role_separation_invalid")
		p.auditErr = drift
		if err := refreshAndCheck(t, m); !errors.Is(err, drift) {
			t.Fatalf("drift: %v", err)
		}
		p.auditErr = busy
		now = now.Add(2 * time.Second)
		if err := refreshAndCheck(t, m); err == nil {
			t.Fatal("an inconclusive audit after a drift verdict served ready")
		}
	})
	t.Run("schema changed", func(t *testing.T) {
		p := &probes{version: 31}
		m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: clock})
		_ = m.Refresh(t.Context())
		p.version, p.auditErr = 32, busy
		now = now.Add(2 * time.Second)
		if err := refreshAndCheck(t, m); !errors.Is(err, readiness.ErrInconclusive) {
			t.Fatalf("inconclusive audit of a new schema: %v", err)
		}
	})
}

// The staleness bound covers a whole slow refresh: a refresh that starts on
// the next tick and spends its full fast and audit budgets still completes
// before the previous state reads stale.
func TestSlowSuccessfulRefreshNeverExposesStale(t *testing.T) {
	const interval, fastBudget, auditBudget = 2 * time.Second, 2 * time.Second, 3 * time.Second
	bound := readiness.StaleBound(interval, fastBudget, auditBudget)
	if bound < interval+fastBudget+auditBudget {
		t.Fatalf("stale bound %v below one refresh's budget", bound)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var m *readiness.Monitor
	version, audits := int64(31), 0
	m, _ = readiness.New(readiness.Options{FastInterval: interval, Stale: bound, Now: func() time.Time { return now },
		Fast: func(context.Context) (int64, error) {
			now = now.Add(fastBudget) // the whole fast budget
			return version, nil
		},
		Audit: func(ctx context.Context, _ int64) error {
			audits++
			if audits == 1 {
				return nil
			}
			for range 9 { // a 4.5s audit, observed by requests throughout
				now = now.Add(500 * time.Millisecond)
				if err := m.Check(ctx); err != nil {
					t.Fatalf("request during a slow audit at %v: %v", now, err)
				}
			}
			return nil
		}})
	_ = m.Refresh(t.Context()) // completes at T
	now = now.Add(interval)    // the next tick
	version = 32               // a migration: the audit is due
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if audits != 2 {
		t.Fatalf("audits %d", audits)
	}
}

// A monitor whose refreshes stall is logged when its state goes stale, once
// per stall.
func TestStaleTransitionsAreLogged(t *testing.T) {
	p := &probes{version: 31}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	_ = m.Refresh(t.Context())
	now = now.Add(time.Minute)
	for range 5 {
		_ = m.Check(t.Context())
	}
	if got := strings.Count(logs.String(), "readiness stale"); got != 1 {
		t.Fatalf("stale logged %d times: %s", got, logs.String())
	}
	_ = m.Refresh(t.Context())
	now = now.Add(time.Minute)
	_ = m.Check(t.Context())
	if got := strings.Count(logs.String(), "readiness stale"); got != 2 {
		t.Fatalf("second stall logged %d times in all: %s", got, logs.String())
	}
}

func refreshAndCheck(t *testing.T, m *readiness.Monitor) error {
	t.Helper()
	refreshErr := m.Refresh(t.Context())
	checkErr := m.Check(t.Context())
	if (refreshErr == nil) != (checkErr == nil) {
		t.Fatalf("refresh %v, check %v", refreshErr, checkErr)
	}
	return checkErr
}

// A fast failure before the first audit leaves the audit pending: the next
// successful fast probe runs it.
func TestFirstAuditWaitsForTheFastProbe(t *testing.T) {
	p := &probes{version: 31, fastErr: database.ErrUnavailable}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: func() time.Time { return now }})
	_ = m.Refresh(t.Context())
	if _, audits := p.counts(); audits != 0 {
		t.Fatalf("audit ran after a failed fast probe: %d", audits)
	}
	if err := m.Check(t.Context()); !errors.Is(err, database.ErrUnavailable) {
		t.Fatalf("check after a failed fast probe: %v", err)
	}
	p.fastErr = nil
	_ = m.Refresh(t.Context())
	if err := m.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, audits := p.counts(); audits != 1 {
		t.Fatalf("first audit ran %d times", audits)
	}
}

func TestSentinelsAreUnavailable(t *testing.T) {
	if !errors.Is(readiness.Inconclusive(errors.New("busy")), readiness.ErrInconclusive) {
		t.Fatal("Inconclusive does not mark its error")
	}
	for _, sentinel := range []error{readiness.ErrNotReady, readiness.ErrStale, readiness.ErrInconclusive} {
		if !errors.Is(sentinel, database.ErrUnavailable) || !strings.HasPrefix(sentinel.Error(), "service_unavailable: ") {
			t.Fatalf("%v does not read as unavailable", sentinel)
		}
	}
	if errors.Is(readiness.ErrNotReady, readiness.ErrStale) || errors.Is(readiness.ErrStale, readiness.ErrNotReady) {
		t.Fatal("sentinels are not distinct")
	}
}

// A change of outcome is logged once, never once per refresh.
func TestOutcomeChangesAreLoggedOnce(t *testing.T) {
	p := &probes{version: 31}
	var logs bytes.Buffer
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, Now: time.Now, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	for range 3 {
		_ = m.Refresh(t.Context())
	}
	p.fastErr = errors.New("actor_id_key_mismatch")
	for range 3 {
		_ = m.Refresh(t.Context())
	}
	p.fastErr = nil
	for range 3 {
		_ = m.Refresh(t.Context())
	}
	if got := strings.Count(logs.String(), "node ready"); got != 2 {
		t.Fatalf("ready logged %d times: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "actor_id_key_mismatch"); got != 1 {
		t.Fatalf("refusal logged %d times: %s", got, logs.String())
	}
}

func TestNewRequiresBothProbes(t *testing.T) {
	p := &probes{}
	for _, options := range []readiness.Options{{Audit: p.audit}, {Fast: p.fast}, {Fast: p.fast, Audit: p.audit, FastInterval: -time.Second},
		{Fast: p.fast, Audit: p.audit, AuditInterval: -time.Second}, {Fast: p.fast, Audit: p.audit, Stale: -time.Second}} {
		if m, err := readiness.New(options); err == nil || m != nil {
			t.Fatalf("New(%+v) accepted", options)
		}
	}
}

// Run refreshes every FastInterval until its context ends, and returns.
func TestRunRefreshesUntilCancelled(t *testing.T) {
	p := &probes{version: 31}
	m, _ := readiness.New(readiness.Options{Fast: p.fast, Audit: p.audit, FastInterval: time.Millisecond, Now: time.Now})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fasts, _ := p.counts(); fasts >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not refresh")
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	fasts, _ := p.counts()
	time.Sleep(10 * time.Millisecond)
	if after, _ := p.counts(); after != fasts {
		t.Fatal("Run kept refreshing after it returned")
	}
}
