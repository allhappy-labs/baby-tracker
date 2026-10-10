package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Sweep is one unit of retention work.
type Sweep interface {
	// Name names the sweep in logs and in Pass's counts.
	Name() string
	// Run does at most one bounded batch per kind it sweeps, each in its own
	// transaction, at the app clock now, and returns the rows it removed.
	// It is idempotent and safe beside requests and beside itself.
	Run(ctx context.Context, now time.Time) (int, error)
}

const (
	// defaultSweepTimeout bounds one sweep's run when Loop.Timeout is 0.
	defaultSweepTimeout = 30 * time.Second
	// defaultReadyRetry is how soon a pass that found the node not ready
	// looks again when Loop.ReadyRetry is 0.
	defaultReadyRetry = 2 * time.Second
)

// DefaultFailureRetry is how soon a failed pass runs again when
// Loop.FailureRetry is 0 (never later than the Interval).
const DefaultFailureRetry = time.Minute

// Loop runs its sweeps in order, one pass every Interval, while Ready
// allows.
type Loop struct {
	Name     string
	Sweeps   []Sweep
	Interval time.Duration // between passes; must be positive
	// Offset delays the first pass (0: at once). Loops that share a pool
	// start out of phase with each other.
	Offset time.Duration
	// ReadyRetry is how soon a pass that found the node not ready is tried
	// again (0: 2s; never later than Interval), so a pass due during a
	// short outage or at startup runs as soon as the node is ready rather
	// than one Interval later.
	ReadyRetry time.Duration
	// FailureRetry is how soon a pass with a failing sweep runs again (0:
	// DefaultFailureRetry; never later than Interval), so a roll that gave
	// up behind a lock (55P03) is retried within a minute, not an hour.
	FailureRetry time.Duration
	Timeout      time.Duration // per sweep; 0 means 30s
	// Ready is the node's cached readiness (nil: always ready). A pass runs
	// only while it returns nil.
	Ready  func(context.Context) error
	Now    func() time.Time // the app clock; nil means time.Now
	Logger *slog.Logger     // nil discards
}

// Run runs passes until ctx ends: the first after Offset, then one every
// Interval after the previous pass ends, or FailureRetry after a pass with a
// failing sweep. A pass the node is not ready for is retried every
// ReadyRetry until it runs.
func (l Loop) Run(ctx context.Context) {
	if l.Interval <= 0 {
		l.logger().Error("maintenance loop has no interval", slog.String("loop", l.Name))
		return
	}
	wait := l.Offset
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if l.Ready != nil && l.Ready(ctx) != nil {
			wait = l.readyRetry()
			continue
		}
		if _, err := l.Pass(ctx); err != nil {
			wait = l.failureRetry()
		} else {
			wait = l.Interval
		}
	}
}

// Pass runs one pass: every sweep in order, each bounded by Timeout and
// given the clock read just before it. A failing sweep is logged (its
// SQLSTATE and whether it timed out, never the database's text, which may
// name hosts or roles) and the others still run. It returns each sweep's
// count, a failing sweep's included (what it removed before failing), and
// the failing sweeps' errors joined.
func (l Loop) Pass(ctx context.Context) (map[string]int, error) {
	counts := make(map[string]int, len(l.Sweeps))
	var failures []error
	for _, sweep := range l.Sweeps {
		if ctx.Err() != nil {
			break
		}
		sweepContext, cancel := context.WithTimeout(ctx, l.timeout())
		n, err := sweep.Run(sweepContext, l.now())
		cancel()
		counts[sweep.Name()] = n
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", sweep.Name(), err))
		}
		switch {
		case err != nil && ctx.Err() == nil:
			var pgErr *pgconn.PgError
			sqlstate := ""
			if errors.As(err, &pgErr) {
				sqlstate = pgErr.Code
			}
			l.logger().Warn("maintenance sweep failed; the next pass retries it", slog.String("loop", l.Name), slog.String("sweep", sweep.Name()),
				slog.Int("removed", n), slog.String("sqlstate", sqlstate), slog.Bool("timeout", errors.Is(err, context.DeadlineExceeded)))
		case n > 0:
			l.logger().Debug("maintenance sweep", slog.String("loop", l.Name), slog.String("sweep", sweep.Name()), slog.Int("removed", n))
		}
	}
	if err := ctx.Err(); err != nil && len(counts) < len(l.Sweeps) {
		failures = append(failures, err)
	}
	return counts, errors.Join(failures...)
}

func (l Loop) timeout() time.Duration {
	if l.Timeout > 0 {
		return l.Timeout
	}
	return defaultSweepTimeout
}

func (l Loop) readyRetry() time.Duration {
	retry := l.ReadyRetry
	if retry <= 0 {
		retry = defaultReadyRetry
	}
	return min(retry, l.Interval)
}

func (l Loop) failureRetry() time.Duration {
	retry := l.FailureRetry
	if retry <= 0 {
		retry = DefaultFailureRetry
	}
	return min(retry, l.Interval)
}

func (l Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l Loop) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.New(slog.DiscardHandler)
}
