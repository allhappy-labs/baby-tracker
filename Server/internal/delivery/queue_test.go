package delivery

import (
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

func TestHostedScheduleFamilyExcludesActorAndIncludesOtherActiveDevices(t *testing.T) {
	h := testkit.NewHosted(t)
	otherOwner, err := h.NewDevice("owner")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json"))
	if err != nil || outcome.Cursor == nil {
		t.Fatalf("apply outcome=%+v err=%v", outcome, err)
	}
	repository := NewRepository(true)
	err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "mutate_record", func(tx pgx.Tx, grant access.Grant) error {
		return repository.ScheduleFamily(t.Context(), tx, grant, h.Owner.DeviceID, *outcome.Cursor)
	})
	if err != nil {
		t.Fatal(err)
	}
	wakes := h.PendingWakes(t)
	if len(wakes) != 2 || wakes[0].DeviceID == h.Owner.DeviceID || wakes[1].DeviceID == h.Owner.DeviceID {
		t.Fatalf("pending=%+v", wakes)
	}
	want := map[string]bool{h.Caregiver.DeviceID: true, otherOwner.DeviceID: true}
	for _, wake := range wakes {
		if !want[wake.DeviceID] || wake.Cursor != *outcome.Cursor || wake.Generation != 1 {
			t.Fatalf("pending=%+v", wakes)
		}
	}
}

func TestSelfHostedScheduleFamilyIsNoop(t *testing.T) {
	h := testkit.New(t)
	repository := NewRepository(false)
	cursor := protocol.Cursor{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, FamilyID: h.FamilyID, Sequence: h.FamilySequence(t)}
	if err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "mutate_record", func(tx pgx.Tx, grant access.Grant) error {
		return repository.ScheduleFamily(t.Context(), tx, grant, h.Owner.DeviceID, cursor)
	}); err != nil {
		t.Fatal(err)
	}
	if wakes := h.PendingWakes(t); len(wakes) != 0 {
		t.Fatalf("self-host scheduled wakes=%+v", wakes)
	}
}
