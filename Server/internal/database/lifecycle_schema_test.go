package database_test

import (
	"testing"

	"babytracker/server/internal/testpg"
)

// Schema 29 drops the Apple revocation and credential cleanup workers (their
// jobs tables and the thirteen worker functions; hosted ruling), and the
// baseline drops their Apple-revoker group (Phase 4 Task 27).
func TestLifecycleMigrationFreezesWorkerFunctionSignatures(t *testing.T) {
	fixture := testpg.New(t)
	for _, gone := range []string{"public.apple_revocation_jobs", "public.credential_cleanup_jobs"} {
		var exists bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, gone).Scan(&exists); err != nil || exists {
			t.Fatalf("%s survived=%t err=%v", gone, exists, err)
		}
	}
	var workerFunctions int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='app' AND (p.proname LIKE '%apple%' OR p.proname LIKE '%credential_cleanup%')`).Scan(&workerFunctions); err != nil || workerFunctions != 0 {
		t.Fatalf("worker functions=%d err=%v", workerFunctions, err)
	}
}

func TestFamilyRecoveryClaimsAreUniqueOnlyWhileActive(t *testing.T) {
	fixture := testpg.New(t)
	var accountUniqueConstraints int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint c
		WHERE c.conrelid='public.family_recovery_claims'::regclass AND c.contype='u'
		  AND pg_get_constraintdef(c.oid) LIKE 'UNIQUE (account_id)%'`).Scan(&accountUniqueConstraints); err != nil {
		t.Fatal(err)
	}
	if accountUniqueConstraints != 0 {
		t.Fatalf("global account uniqueness constraints=%d", accountUniqueConstraints)
	}
	var definition string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes WHERE schemaname='public'
		AND tablename='family_recovery_claims' AND indexname='family_recovery_claims_one_active_per_account_idx'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if definition != "CREATE UNIQUE INDEX family_recovery_claims_one_active_per_account_idx ON public.family_recovery_claims USING btree (account_id) WHERE (status = 'active'::app.claim_status)" {
		t.Fatalf("active-claim index=%q", definition)
	}
}
