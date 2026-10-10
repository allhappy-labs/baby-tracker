package testkit_test

import (
	"testing"
	"time"

	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// TestHarnessSessionsOutliveTheirFixtureDate pins the harness clock to the
// database's. The database compares sessions with its own clock
// (app.push_targets, app.prune_push_tokens and app.live_activity_authorized
// read refresh_expires_at > now() or clock_timestamp()), so sessions issued
// against a fixed calendar date stop counting once the real calendar passes
// that date plus the refresh window, and every test that needs a live
// session starts failing on that day.
func TestHarnessSessionsOutliveTheirFixtureDate(t *testing.T) {
	h := testkit.New(t)
	database := h.DatabaseNow()
	// The harness starts at the latest noon UTC at or before the database
	// clock: less than a day behind it, never ahead.
	if lag := database.Sub(h.Now); lag < 0 || lag >= 24*time.Hour || !h.Now.Equal(h.Now.UTC().Truncate(24*time.Hour).Add(12*time.Hour)) {
		t.Fatalf("harness clock %s, database clock %s: want the latest noon UTC at or before the database's", h.Now.UTC().Format(time.RFC3339), database.UTC().Format(time.RFC3339))
	}
	var sessions int
	var outlive bool
	err := pgx.BeginFunc(t.Context(), h.Migrator, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
			return err
		}
		return tx.QueryRow(t.Context(), `SELECT count(*), coalesce(min(refresh_expires_at) > clock_timestamp() + interval '7 days', false)
			FROM sessions WHERE status='active'`).Scan(&sessions, &outlive)
	})
	if err != nil {
		t.Fatal(err)
	}
	if sessions == 0 || !outlive {
		t.Fatalf("%d active harness sessions, all refreshable for another week by the database clock: %t", sessions, outlive)
	}
}
