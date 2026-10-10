package testkit

import (
	"context"
	"time"
)

// FixtureClock is the instant the fixed test data was written against: the
// legacy corpora and goldens (accepted_at 2026-09-21T12:00:00Z), the
// recorded legacy decisions, and the testdata mutations and caretest's timer
// times, which sit an hour or a day before it. The care rules judge such
// times against the service clock (a timer edit reaches a week back and a
// minute ahead; parts may not outrun the time elapsed), so a test that
// replays that data runs on NewAtFixtureClock.
var FixtureClock = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// DatabaseNow is the PostgreSQL server's clock_timestamp(). Workers that stamp
// rows with a service clock and later select them against the database's
// now() (the export queue's next_attempt_at<=now()) disagree whenever the
// test database runs in a container whose clock trails the host's: a job
// stamped a few milliseconds "in the future" is not claimed. Tests that
// expect one worker pass to make progress use this as the service clock so
// stamping and claiming read one clock. A failed read reports the error and
// falls back to the host clock.
func (h *Harness) DatabaseNow() time.Time {
	var now time.Time
	if err := h.Admin.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		h.t.Errorf("database clock: %v", err)
		return time.Now()
	}
	return now
}
