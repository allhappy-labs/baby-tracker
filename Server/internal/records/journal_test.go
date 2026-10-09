package records_test

import (
	"errors"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

func TestAppendJournalBatchesRejectsIncompleteReservationAndRollsBackCursor(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	beforeSequence := h.FamilySequence(t)
	beforeJournal := h.Count(t, "mutation_journal", h.FamilyID)
	mutation := testkit.MustMutation(t, "baby.json")
	mutation.MutationID, mutation.Operation, mutation.Payload = mutationID(224), "delete", protocol.DeletePayload{}
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "mutate_record", func(tx pgx.Tx, grant access.Grant) error {
		if err := records.LockFamilyForUpdate(t.Context(), tx, grant.FamilyID); err != nil {
			return err
		}
		_, _, err := records.AppendJournalBatches(t.Context(), tx, grant, h.Owner.DeviceID, mutation, 2,
			h.Info.InstanceID, h.Info.RecoveryEpoch, func(records.JournalBatchAppender) error { return nil })
		return err
	})
	if err == nil || !errors.Is(err, records.ErrIncompleteJournalReservation) {
		t.Fatalf("incomplete reservation error=%v", err)
	}
	if h.FamilySequence(t) != beforeSequence || h.Count(t, "mutation_journal", h.FamilyID) != beforeJournal {
		t.Fatal("incomplete reservation advanced the cursor or wrote journal rows")
	}
}
