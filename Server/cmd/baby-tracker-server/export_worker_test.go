package main

import (
	"testing"

	"babytracker/server/internal/testpg"
)

func TestExportWorkerRequiresIsolatedMaintenanceRole(t *testing.T) {
	fixture := testpg.New(t)
	var maintenanceRole string
	if err := fixture.Maintenance.QueryRow(t.Context(), "SELECT current_user").Scan(&maintenanceRole); err != nil {
		t.Fatal(err)
	}
	if err := verifyExportPool(t.Context(), fixture.Maintenance, maintenanceRole); err != nil {
		t.Fatalf("maintenance role rejected: %v", err)
	}
	if err := verifyExportPool(t.Context(), fixture.Runtime, maintenanceRole); err == nil {
		t.Fatal("runtime credential was accepted as export worker")
	}
	if err := verifyExportPool(t.Context(), fixture.Owner, maintenanceRole); err == nil {
		t.Fatal("owner credential was accepted as export worker")
	}
}
