package records_test

import (
	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"errors"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

func TestGrowthChildCascadeOrderingRollbackAndLateEdit(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	for _, f := range []string{"growth-profile.json", "measurement.json"} {
		out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, testkit.MustMutation(t, f))
		if err != nil || out.Status != "applied" {
			t.Fatalf("seed %s %+v %v", f, out, err)
		}
	}
	before := h.FamilySequence(t)
	beforeCount, beforeBytes := h.FamilyUsage(t)
	beforeReceipts := h.Count(t, "mutation_receipts", h.FamilyID)
	guard := &cascadeBatchFaultGuard{inner: h.Security, failAt: 3}
	service := familysync.NewService(guard, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now })
	deletion := testkit.MustMutation(t, "baby.json")
	deletion.MutationID, deletion.Operation, deletion.Payload = h.CommandID(), "delete", protocol.DeletePayload{}
	if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, deletion); !errors.Is(err, errCascadeSecondBatch) {
		t.Fatalf("growth cascade did not reach injected measurement batch: %v", err)
	}
	if h.FamilySequence(t) != before || h.Count(t, "mutation_receipts", h.FamilyID) != beforeReceipts {
		t.Fatal("rollback changed sequence/receipts")
	}
	if count, bytes := h.FamilyUsage(t); count != beforeCount || bytes != beforeBytes {
		t.Fatal("rollback changed usage")
	}
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		for _, table := range []string{"babies", "growth_profiles", "measurement_entries"} {
			var count int
			if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE family_id=$1 AND tombstoned_at IS NULL", h.FamilyID).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				t.Fatalf("rollback lost live %s", table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, deletion); err == nil {
		t.Fatal("caregiver deleted child")
	}
	out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, deletion)
	if err != nil || out.Status != "applied" {
		t.Fatalf("delete %+v %v", out, err)
	}
	pull, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(before), nil))
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{"baby", "growth_profile", "measurement"}
	if len(pull.Changes) != len(kinds) {
		t.Fatalf("changes %+v", pull.Changes)
	}
	for i, kind := range kinds {
		c := pull.Changes[i]
		if c.EntityKind != kind || c.Cursor.Sequence != before+uint64(i)+1 || c.Operation != "delete" || c.Optional || c.SnapshotOrTombstone.Payload != nil {
			t.Fatalf("change %+v", c)
		}
	}
	if count, _ := h.FamilyUsage(t); count != 0 {
		t.Fatalf("usage count %d", count)
	}
	for _, f := range []string{"growth-profile.json", "measurement.json"} {
		edit := testkit.MustMutation(t, f)
		edit.MutationID, edit.Operation, edit.ObservedEntityVersion = h.CommandID(), "update", 1
		out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, edit)
		if err != nil || out.Code == nil || *out.Code != "baby_deleted" {
			t.Fatalf("late edit %+v %v", out, err)
		}
	}
}
