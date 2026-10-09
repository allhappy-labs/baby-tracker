package database_test

import (
	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
	"testing"
)

func TestRecoverySchemaRoleSeparation(t *testing.T) {
	fixture := testpg.New(t)
	var present bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT to_regclass('public.refresh_recovery_receipts') IS NOT NULL`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("missing refresh recovery receipt table")
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`SELECT * FROM public.refresh_recovery_receipts`, `DELETE FROM public.refresh_recovery_receipts`} {
		if _, err := fixture.Runtime.Exec(t.Context(), query); err == nil {
			t.Fatalf("runtime allowed %s", query)
		}
	}
}

func TestRecoveryAcknowledgementLookupRequiresDeviceRealm(t *testing.T) {
	fixture := testpg.New(t)
	var oldPresent, newPresent bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT to_regprocedure('app.lookup_refresh_recovery_ack(uuid,uuid)') IS NOT NULL,to_regprocedure('app.lookup_refresh_recovery_ack(uuid,uuid,uuid,bigint,uuid)') IS NOT NULL`).Scan(&oldPresent, &newPresent); err != nil {
		t.Fatal(err)
	}
	if oldPresent || !newPresent {
		t.Fatalf("ACK lookup signatures old=%t new=%t", oldPresent, newPresent)
	}
	signature := "app.lookup_refresh_recovery_ack(uuid,uuid,uuid,bigint,uuid)"
	assertFunctionPrivilege(t, fixture.Runtime, signature, true)
	assertFunctionPrivilege(t, fixture.Projector, signature, false)
	assertPublicCannotExecute(t, fixture, signature)
}
