package maintenance_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/maintenance"
	"github.com/jackc/pgx/v5/pgconn"
)

// The retention bounds, pinned by value: batches start at 1,000 rows per
// statement, halve to no fewer than 100 after a statement timeout, and a
// pass re-runs full batches up to its cap (the capacity of the former
// single 5,000/5,000/2,000-row batches).
func TestRetentionBoundsArePinned(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"SessionBatch", maintenance.SessionBatch, 1000},
		{"ChallengeBatch", maintenance.ChallengeBatch, 1000},
		{"RefreshReceiptBatch", maintenance.RefreshReceiptBatch, 1000},
		{"MinBatch", maintenance.MinBatch, 100},
		{"SessionPassCap", maintenance.SessionPassCap, 15000},
		{"ChallengePassCap", maintenance.ChallengePassCap, 5000},
		{"RefreshReceiptPassCap", maintenance.RefreshReceiptPassCap, 2000},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if maintenance.DefaultFailureRetry != time.Minute {
		t.Errorf("DefaultFailureRetry = %v, want 1m", maintenance.DefaultFailureRetry)
	}
}

func statementTimeout() error {
	return fmt.Errorf("prune: %w", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"})
}

// A batch that hits the statement timeout halves the next one, down to the
// floor; a pass ends at the failure. A cancellation (the same SQLSTATE, sent
// for a cancelled context) does not shrink it.
func TestAdaptiveBatchHalvesAfterAStatementTimeout(t *testing.T) {
	batch := &maintenance.AdaptiveBatch{Start: 1000, Min: 100, PassCap: 15000}
	var limits []int
	timeout := func(_ context.Context, limit int) (int, error) {
		limits = append(limits, limit)
		return 0, statementTimeout()
	}
	for _, want := range []int{500, 250, 125, 100, 100} {
		if _, _, err := batch.Drain(t.Context(), timeout); err == nil {
			t.Fatal("a timed-out batch reported success")
		}
		if got := batch.Size(); got != want {
			t.Fatalf("after a statement timeout the batch is %d, want %d (limits so far %v)", got, want, limits)
		}
	}
	if want := []int{1000, 500, 250, 125, 100}; fmt.Sprint(limits) != fmt.Sprint(want) {
		t.Fatalf("one statement per failing pass, at limits %v, want %v", limits, want)
	}
	// A cancelled pass is not a slow statement.
	other := &maintenance.AdaptiveBatch{Start: 1000, Min: 100, PassCap: 15000}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := other.Drain(ctx, func(context.Context, int) (int, error) { return 0, statementTimeout() }); err == nil {
		t.Fatal("a cancelled batch reported success")
	}
	if got := other.Size(); got != 1000 {
		t.Fatalf("a cancelled pass shrank the batch to %d", got)
	}
	// Other failures leave the size alone too.
	if _, _, err := other.Drain(t.Context(), func(context.Context, int) (int, error) { return 0, errors.New("boom") }); err == nil || other.Size() != 1000 {
		t.Fatalf("a non-timeout failure: %v, size %d", err, other.Size())
	}
}

// A pass re-runs at once while batches come back full, and stops at the
// first short batch or at its cap (reported, so the caller can log the
// backlog).
func TestAdaptiveBatchRerunsWhileFullUpToItsCap(t *testing.T) {
	batch := &maintenance.AdaptiveBatch{Start: 1000, Min: 100, PassCap: 5000}
	backlog := 3500
	calls := 0
	drain := func(_ context.Context, limit int) (int, error) {
		calls++
		n := min(limit, backlog)
		backlog -= n
		return n, nil
	}
	removed, capped, err := batch.Drain(t.Context(), drain)
	if err != nil || removed != 3500 || capped || calls != 4 {
		t.Fatalf("removed %d, capped %v, %d statements (%v); want the 3,500 backlog in 4, under the cap", removed, capped, calls, err)
	}
	endless := func(_ context.Context, limit int) (int, error) { return limit, nil }
	removed, capped, err = batch.Drain(t.Context(), endless)
	if err != nil || removed != 5000 || !capped {
		t.Fatalf("an endless backlog: removed %d, capped %v (%v); want the 5,000 cap", removed, capped, err)
	}
	// A failure part-way keeps what the committed batches removed.
	failing := 0
	removed, _, err = batch.Drain(t.Context(), func(_ context.Context, limit int) (int, error) {
		if failing++; failing == 3 {
			return 0, errors.New("boom")
		}
		return limit, nil
	})
	if err == nil || removed != 2000 {
		t.Fatalf("a failure on the third batch: removed %d (%v), want the first two's 2,000", removed, err)
	}
}

// A halved batch grows back: after four full batches without a timeout the
// size doubles, up to Start (C18).
func TestAdaptiveBatchGrowsBackAfterSuccesses(t *testing.T) {
	b := &maintenance.AdaptiveBatch{Start: 1000, Min: 50, PassCap: 1 << 30}
	// statementTimedOut (batch.go) matches SQLSTATE 57014 while ctx is live.
	pgTimeout := func() error { return &pgconn.PgError{Code: "57014"} }
	b.Drain(t.Context(), func(ctx context.Context, limit int) (int, error) { return 0, pgTimeout() })
	if b.Size() != 500 {
		t.Fatalf("after a timeout %d", b.Size())
	}
	full := 0
	b.Drain(t.Context(), func(ctx context.Context, limit int) (int, error) {
		full++
		if full > 12 {
			return 0, nil
		}
		return limit, nil
	})
	if b.Size() != 1000 {
		t.Fatalf("after 12 full batches the size is %d, want back at Start 1000", b.Size())
	}
}

// A timeout resets the streak: full batches before it do not count towards
// the next growth. Otherwise the streak carries across passes, and
// GrowAfter overrides the default four.
func TestAdaptiveBatchTimeoutResetsTheGrowStreak(t *testing.T) {
	b := &maintenance.AdaptiveBatch{Start: 1000, Min: 50, PassCap: 1 << 30}
	if _, _, err := b.Drain(t.Context(), func(context.Context, int) (int, error) { return 0, statementTimeout() }); err == nil {
		t.Fatal("a timed-out batch reported success")
	}
	// fullThen runs `full` full batches, then ends the pass with `last`.
	fullThen := func(b *maintenance.AdaptiveBatch, full int, last error) {
		t.Helper()
		calls := 0
		_, _, err := b.Drain(t.Context(), func(_ context.Context, limit int) (int, error) {
			if calls++; calls > full {
				return 0, last
			}
			return limit, nil
		})
		if (err != nil) != (last != nil) {
			t.Fatalf("pass after %d full batches: %v", full, err)
		}
	}
	fullThen(b, 3, statementTimeout()) // three full at 500, then a timeout: 250, streak 0
	if b.Size() != 250 {
		t.Fatalf("after the second timeout %d, want 250", b.Size())
	}
	fullThen(b, 3, nil) // three full: the streak is 3, not 6
	if b.Size() != 250 {
		t.Fatalf("three full batches after a timeout grew the batch to %d; the timeout must reset the streak", b.Size())
	}
	fullThen(b, 1, nil) // the fourth, in the next pass: the streak carries across passes
	if b.Size() != 500 {
		t.Fatalf("the fourth full batch (across passes) left the size at %d, want 500", b.Size())
	}
	// GrowAfter 1 grows on every full batch; the growth stops at Start even
	// when doubling would pass it (300 → 600 → 1,000, not 1,200).
	quick := &maintenance.AdaptiveBatch{Start: 1000, Min: 300, PassCap: 1 << 30, GrowAfter: 1}
	fullThen(quick, 0, statementTimeout())
	fullThen(quick, 0, statementTimeout())
	if quick.Size() != 300 {
		t.Fatalf("two timeouts with Min 300 left %d", quick.Size())
	}
	fullThen(quick, 1, nil)
	if quick.Size() != 600 {
		t.Fatalf("GrowAfter 1: one full batch left the size at %d, want 600", quick.Size())
	}
	fullThen(quick, 3, nil)
	if quick.Size() != 1000 {
		t.Fatalf("growth past Start: the size is %d, want 1000", quick.Size())
	}
}
