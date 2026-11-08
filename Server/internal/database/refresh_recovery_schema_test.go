package database_test

import (
	"strings"
	"testing"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
)

// Schema 29 replaces refresh_recovery_receipts and its runtime functions with
// refresh_receipts, an authority-only table: the runtime reaches it only
// inside an authority transaction and every worker role holds nothing on it.
func TestRecoverySchemaRoleSeparation(t *testing.T) {
	fixture := testpg.New(t)
	var legacy, present bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT to_regclass('public.refresh_recovery_receipts') IS NOT NULL,
		to_regclass('public.refresh_receipts') IS NOT NULL`).Scan(&legacy, &present); err != nil {
		t.Fatal(err)
	}
	if legacy || !present {
		t.Fatalf("legacy receipts=%t refresh receipts=%t", legacy, present)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 37); err != nil {
		t.Fatal(err)
	}
	ids := seedAuthorityFamilies(t, fixture)
	rows := authorityOnlyRows(ids, 1)
	for _, table := range []string{"sessions", "refresh_receipts"} {
		if err := authorityExec(t.Context(), fixture.Owner, rows[table].sql, rows[table].args...); err != nil {
			t.Fatalf("seed %s: %v", table, err)
		}
	}
	var visible int
	if err := fixture.Runtime.QueryRow(t.Context(), `SELECT count(*) FROM public.refresh_receipts`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("runtime outside authority reads %d receipts err=%v", visible, err)
	}
	deleted, err := fixture.Runtime.Exec(t.Context(), `DELETE FROM public.refresh_receipts`)
	if err != nil || deleted.RowsAffected() != 0 {
		t.Fatalf("runtime outside authority deleted %d receipts err=%v", deleted.RowsAffected(), err)
	}
	for _, role := range []string{"baby_tracker_maintenance"} {
		var any bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT has_table_privilege($1,'public.refresh_receipts','SELECT,INSERT,UPDATE,DELETE,TRUNCATE')`, role).Scan(&any); err != nil || any {
			t.Fatalf("%s holds refresh_receipts privileges=%t err=%v", role, any, err)
		}
	}
}

// The acknowledgement lookup is keyed by the device realm (account, device,
// attempt), not by the attempt alone: the index the authority's
// AcknowledgeRefresh uses. Behaviour: auth
// TestRecoveryAcknowledgementSameAttemptIsDeviceScoped and authority
// TestAcknowledgeRefreshDeletesOnlyTheBoundReceipt.
func TestRecoveryAcknowledgementLookupRequiresDeviceRealm(t *testing.T) {
	fixture := testpg.New(t)
	var legacyLookup bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT to_regprocedure('app.lookup_refresh_recovery_ack(uuid,uuid,uuid,bigint,uuid)') IS NOT NULL`).Scan(&legacyLookup); err != nil || legacyLookup {
		t.Fatalf("legacy ack lookup survived=%t err=%v", legacyLookup, err)
	}
	var definition string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes WHERE schemaname='public'
		AND indexname='refresh_receipts_device_attempt_idx'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(definition, "ON public.refresh_receipts USING btree (account_id, device_id, attempt_id)") {
		t.Fatalf("ack index=%q", definition)
	}
}
