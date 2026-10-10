package care_test

import (
	"errors"
	"strings"
	"testing"

	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5/pgconn"
)

// A timer's origin is its start: the caregiver's edit and stop leave it, and
// the completion the stop creates originates on the stopping device. The edit
// moves the start earlier (a start after caretest.Stop's 11:10 would refuse
// the stop as invalid_timer_time), and every step must apply.
func TestOriginIsTheFirstVersionAndNeverMoves(t *testing.T) {
	h := testkit.NewAtFixtureClock(t) // the edit's times are written against it
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	start := caretest.StartTimer(caretest.BabyA, testkit.TimerA, "feed")
	started := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{start})
	startSeq := started.Outcomes[0].Cursor.Sequence
	edit := caretest.TimerCommand(testkit.TimerA, 2, "update", "2026-09-21T12:05:00.000000Z", 1,
		protocol.TimerEditPayload{BabyID: caretest.BabyA, StartedAt: ptr("2026-09-21T10:59:00.000000Z")})
	if r := caretest.ApplyCommitted(t, h, h.Caregiver, []protocol.Mutation{edit}); r.Outcomes[0].Status != "applied" {
		t.Fatalf("edit: %s %s", r.Outcomes[0].Status, caretest.Code(r.Outcomes[0]))
	}
	row := caretest.Rows(t, h)[testkit.TimerA]
	if row.CreatedDeviceID != h.Owner.DeviceID || row.CreatedSeq != startSeq || row.ChangedSeq <= startSeq || row.DeviceID != h.Caregiver.DeviceID {
		t.Fatalf("origin %s/%d, changed %d by %s", row.CreatedDeviceID, row.CreatedSeq, row.ChangedSeq, row.DeviceID)
	}
	stop := caretest.Stop(caretest.BabyA, testkit.TimerA, testkit.RecordA)
	stop.ObservedEntityVersion = 2
	if r := caretest.ApplyCommitted(t, h, h.Caregiver, []protocol.Mutation{stop}); r.Outcomes[0].Status != "applied" {
		t.Fatalf("stop: %s %s", r.Outcomes[0].Status, caretest.Code(r.Outcomes[0]))
	}
	rows := caretest.Rows(t, h)
	if tomb := rows[testkit.TimerA]; tomb.Live() || tomb.CreatedDeviceID != h.Owner.DeviceID || tomb.CreatedSeq != startSeq {
		t.Fatalf("a tombstone moved the origin: %s/%d (live %t)", tomb.CreatedDeviceID, tomb.CreatedSeq, tomb.Live())
	}
	if rec := rows[testkit.RecordA]; rec.CreatedDeviceID != h.Caregiver.DeviceID || rec.CreatedSeq == 0 || rec.CreatedSeq != rec.ChangedSeq {
		t.Fatalf("the completion's origin is %s/%d at %d, want the stopping device at its own sequence", rec.CreatedDeviceID, rec.CreatedSeq, rec.ChangedSeq)
	}
}

// Encode takes a new entity's origin from its change and every later
// version's from the row it replaces, a tombstone's included; a row written
// before 32 has none, and its successors keep none.
func TestEncodeCarriesTheOrigin(t *testing.T) {
	const familyID, babyID = "f0000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000001"
	const first, second = "d0000000-0000-4000-8000-000000000001", "d0000000-0000-4000-8000-000000000002"
	entity := protocol.CanonicalEntity{EntityKind: "baby", Payload: protocol.BabyPayload{Name: "Baby", BirthDate: "2026-01-12"},
		Meta: protocol.EntityMeta{ID: babyID, FamilyID: familyID, EntityVersion: 1, CreatorAccountID: "a0000000-0000-4000-8000-000000000001",
			LastEditorAccountID: "a0000000-0000-4000-8000-000000000001", ClientMutationTime: "2026-09-21T12:00:00.000000Z",
			AcceptedAt: "2026-09-21T12:00:00.000000Z"}}
	created, err := care.Encode(entity, nil, care.Change{Seq: 7, DeviceID: first, Operation: "create"})
	if err != nil {
		t.Fatal(err)
	}
	if created.CreatedDeviceID != first || created.CreatedSeq != 7 {
		t.Fatalf("a new entity's origin is %s/%d, want %s/7", created.CreatedDeviceID, created.CreatedSeq, first)
	}
	entity.Meta.EntityVersion = 2
	edited, err := care.Encode(entity, &created, care.Change{Seq: 9, DeviceID: second, Operation: "update"})
	if err != nil {
		t.Fatal(err)
	}
	if edited.CreatedDeviceID != first || edited.CreatedSeq != 7 || edited.DeviceID != second {
		t.Fatalf("an edit's origin is %s/%d by %s, want %s/7", edited.CreatedDeviceID, edited.CreatedSeq, edited.DeviceID, first)
	}
	entity.Meta.EntityVersion, entity.Payload = 3, nil
	deleted := "2026-09-21T12:00:00.000000Z"
	entity.Meta.TombstonedAt = &deleted
	tomb, err := care.Encode(entity, &edited, care.Change{Seq: 11, DeviceID: second, Operation: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if tomb.CreatedDeviceID != first || tomb.CreatedSeq != 7 {
		t.Fatalf("a tombstone's origin is %s/%d, want %s/7", tomb.CreatedDeviceID, tomb.CreatedSeq, first)
	}
	legacy := created
	legacy.CreatedDeviceID, legacy.CreatedSeq = "", 0
	entity.Meta.EntityVersion, entity.Meta.TombstonedAt = 2, nil
	entity.Payload = protocol.BabyPayload{Name: "Baby", BirthDate: "2026-01-12"}
	after, err := care.Encode(entity, &legacy, care.Change{Seq: 9, DeviceID: second, Operation: "update"})
	if err != nil {
		t.Fatal(err)
	}
	if after.CreatedDeviceID != "" || after.CreatedSeq != 0 {
		t.Fatalf("an edit of a row without an origin invented %s/%d", after.CreatedDeviceID, after.CreatedSeq)
	}
}

// The upsert inserts the origin and never updates it: a later version whose
// row names another origin leaves the stored one as it was, and a row
// without one stores NULL.
func TestStoreNeverMovesAStoredOrigin(t *testing.T) {
	h := testkit.New(t)
	owner := caretest.Owner(h)
	baby := caretest.BabyRow(t, h, caretest.BabyA, 1)
	withOrigin := caretest.FeedRow(t, h, baby, testkit.RecordA, 2)
	without := caretest.FeedRow(t, h, baby, testkit.RecordB, 3)
	without.CreatedDeviceID, without.CreatedSeq = "", 0 // as written before 32
	if err := caretest.Write(t, h, baby, withOrigin, without); err != nil {
		t.Fatal(err)
	}
	rows := caretest.Rows(t, h)
	if got := rows[testkit.RecordA]; got.CreatedDeviceID != h.Owner.DeviceID || got.CreatedSeq != 2 {
		t.Fatalf("stored origin %s/%d, want %s/2", got.CreatedDeviceID, got.CreatedSeq, h.Owner.DeviceID)
	}
	if got := rows[testkit.RecordB]; got.CreatedDeviceID != "" || got.CreatedSeq != 0 {
		t.Fatalf("a row without an origin stored %s/%d", got.CreatedDeviceID, got.CreatedSeq)
	}

	// Later versions that name another origin (Encode never builds one;
	// the store must not trust that it does).
	movedA := owner.Session(t, rows[testkit.RecordA], "3a000000-0000-4000-8000-000000000001", 4)
	movedB := owner.Session(t, rows[testkit.RecordB], "3a000000-0000-4000-8000-000000000002", 5)
	movedA.CreatedDeviceID, movedA.CreatedSeq = h.Caregiver.DeviceID, 4
	movedB.CreatedDeviceID, movedB.CreatedSeq = h.Caregiver.DeviceID, 5
	if err := caretest.Write(t, h, movedA, movedB); err != nil {
		t.Fatal(err)
	}
	rows = caretest.Rows(t, h)
	if got := rows[testkit.RecordA]; got.EntityVersion != 2 || got.CreatedDeviceID != h.Owner.DeviceID || got.CreatedSeq != 2 {
		t.Fatalf("version %d moved the origin to %s/%d", got.EntityVersion, got.CreatedDeviceID, got.CreatedSeq)
	}
	if got := rows[testkit.RecordB]; got.EntityVersion != 2 || got.CreatedDeviceID != "" || got.CreatedSeq != 0 {
		t.Fatalf("version %d gave a pre-32 row the origin %s/%d", got.EntityVersion, got.CreatedDeviceID, got.CreatedSeq)
	}

	// A half-set origin is refused before it reaches the database.
	half := caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000009", 6)
	half.CreatedDeviceID = ""
	var pgErr *pgconn.PgError
	if err := caretest.Write(t, h, half); err == nil || errors.As(err, &pgErr) || !strings.Contains(err.Error(), "half set") {
		t.Fatalf("a half-set origin: %v", err)
	}
}
