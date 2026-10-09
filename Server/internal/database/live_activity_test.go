package database

import (
	"babytracker/server/internal/testpg"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestLiveActivityRoleSeparation(t *testing.T) {
	f := testpg.New(t)
	if err := VerifyLiveActivityRole(t.Context(), f.LiveActivity, 29); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"live_activity_devices", "live_activity_tokens", "live_activity_delivery", "live_activity_installations", "live_activity_token_owners", "live_activity_registration_index", "babies", "families", "sessions"} {
		if _, err := f.LiveActivity.Exec(t.Context(), "SELECT * FROM "+table); err == nil {
			t.Fatalf("worker read %s", table)
		}
	}
	if _, err := f.Runtime.Exec(t.Context(), "SELECT * FROM app.claim_live_activity_work(now(),1)"); err == nil {
		t.Fatal("runtime claimed worker job")
	}
	// The worker's exact executable inventory, stated explicitly rather than
	// only derived from the manifest it is verified against.
	rows, err := f.Owner.Query(t.Context(), `SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='app' AND has_function_privilege('baby_tracker_live_activity',p.oid,'EXECUTE') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)",
		"app.claim_live_activity_work(timestamp with time zone,integer)",
		"app.finish_live_activity_work(jsonb,timestamp with time zone,text)",
		"app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)",
		"app.release_stale_live_activity_lease(jsonb)",
		"app.retain_live_activity_work()",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("worker app executables=%v want=%v", got, want)
	}
}
