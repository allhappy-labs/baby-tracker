// Package caretest builds care rows and writes them through the care store
// for tests: tenant transactions, one canonical entity per kind, and
// committed writes whose family counters follow the rows.
package caretest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantTx begins a transaction on pool with the tenant context of account
// and family set, as the authority guard leaves it. It rolls back at cleanup
// unless the test commits it.
func TenantTx(t *testing.T, pool *pgxpool.Pool, accountID, familyID string) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(t.Context()) })
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	return tx
}

// Author is who writes rows: the family, the account and device of the
// change, and the acceptance time every built entity carries.
type Author struct {
	FamilyID, AccountID, DeviceID string
	Now                           time.Time
}

// Owner is the harness owner writing into the harness family.
func Owner(h *testkit.Harness) Author {
	return Author{FamilyID: h.FamilyID, AccountID: h.Owner.AccountID, DeviceID: h.Owner.DeviceID, Now: h.Now}
}

const (
	zone     = "Europe/Zurich"
	birth    = "2026-01-12"
	layout   = "2006-01-02T15:04:05.000000Z"
	startsAt = "2026-09-20T10:00:00.000000Z"
	endsAt   = "2026-09-20T10:30:00.000000Z"
)

// BabyRow is a live baby of the harness owner, named "Baby", at seq.
func BabyRow(t *testing.T, h *testkit.Harness, id string, seq uint64) care.Row {
	t.Helper()
	return Owner(h).Baby(t, id, "Baby", birth, seq)
}

// FeedRow is a live 60 ml bottle feed of baby by the harness owner at seq.
func FeedRow(t *testing.T, h *testkit.Harness, baby care.Row, id string, seq uint64) care.Row {
	t.Helper()
	return Owner(h).Row(t, "feed", id, &baby, seq)
}

// Baby is a live baby with name and birth date at seq.
func (a Author) Baby(t *testing.T, id, name, birthDate string, seq uint64) care.Row {
	t.Helper()
	return a.encode(t, "baby", id, protocol.BabyPayload{Name: name, BirthDate: birthDate}, nil, seq)
}

// Row is a live entity of kind (a baby when kind is "baby", whose baby must
// be nil; otherwise a record of baby) at seq, with a fixed valid payload.
func (a Author) Row(t *testing.T, kind, id string, baby *care.Row, seq uint64) care.Row {
	t.Helper()
	if kind == "baby" {
		if baby != nil {
			t.Fatal("caretest: a baby has no baby")
		}
		return a.Baby(t, id, "Baby", birth, seq)
	}
	if baby == nil {
		t.Fatalf("caretest: a %s needs a baby", kind)
	}
	end := endsAt
	var payload protocol.Payload
	switch kind {
	case "feed":
		contents := "formula"
		payload = protocol.FeedPayload{BabyID: baby.ID, Kind: "bottle", StartedAt: startsAt, TimeZoneID: zone,
			Components: []protocol.FeedComponent{{ID: partID(id), Kind: "bottle", Contents: &contents, VolumeML: protocol.MustParseDecimal("60")}}}
	case "sleep":
		payload = protocol.SleepPayload{BabyID: baby.ID, StartedAt: startsAt, EndedAt: &end, TimeZoneID: zone}
	case "diaper":
		payload = protocol.DiaperPayload{BabyID: baby.ID, StartedAt: startsAt, TimeZoneID: zone, Contents: "wet"}
	case "timer":
		payload = protocol.TimerStartPayload{BabyID: baby.ID, TimerKind: "feed", StartedAt: startsAt, TimeZoneID: zone,
			CompletedComponents: []protocol.FeedComponent{}}
	case "supplement":
		payload = protocol.SupplementPayload{BabyID: baby.ID, StartedAt: startsAt, TimeZoneID: zone, Name: "Vitamin D", LoggedAt: startsAt}
	case "growth_profile":
		payload = protocol.GrowthProfilePayload{BabyID: baby.ID, ReferenceSex: "girls"}
	case "measurement":
		payload = protocol.MeasurementPayload{BabyID: baby.ID, MeasuredAt: startsAt, LoggedAt: startsAt, TimeZoneID: zone,
			WeightKG: protocol.MustParseDecimal("7.125")}
	default:
		t.Fatalf("caretest: kind %q", kind)
	}
	return a.encode(t, kind, id, payload, nil, seq)
}

// Session is r moved into session (a feed's feeding session, a sleep's sleep
// session or a timer's session).
func (a Author) Session(t *testing.T, r care.Row, session string, seq uint64) care.Row {
	t.Helper()
	entity, err := r.Entity()
	if err != nil {
		t.Fatal(err)
	}
	switch payload := entity.Payload.(type) {
	case protocol.FeedPayload:
		payload.FeedingSessionID = &session
		entity.Payload = payload
	case protocol.SleepPayload:
		payload.SleepSessionID = &session
		entity.Payload = payload
	case protocol.TimerStartPayload:
		payload.SessionID = &session
		entity.Payload = payload
	default:
		t.Fatalf("caretest: a %s has no session", r.Kind)
	}
	return a.next(t, entity, &r, care.Change{Seq: seq, Operation: "update"})
}

// Tombstone is the deletion of r at seq. A timer stop names its completion.
func (a Author) Tombstone(t *testing.T, r care.Row, seq uint64, completion *string) care.Row {
	t.Helper()
	entity, err := r.Entity()
	if err != nil {
		t.Fatal(err)
	}
	accepted := a.Now.UTC().Format(layout)
	entity.Meta.TombstonedAt, entity.Payload = &accepted, nil
	operation := "delete"
	if completion != nil {
		operation = "timer_stop"
	}
	return a.next(t, entity, &r, care.Change{Seq: seq, Operation: operation, CompletedRecordID: completion})
}

func (a Author) next(t *testing.T, entity protocol.CanonicalEntity, previous *care.Row, change care.Change) care.Row {
	t.Helper()
	entity.Meta.EntityVersion++
	entity.Meta.LastEditorAccountID, entity.Meta.AcceptedAt = a.AccountID, a.Now.UTC().Format(layout)
	change.ActorAccountID, change.DeviceID = a.AccountID, a.DeviceID
	row, err := care.Encode(entity, previous, change)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (a Author) encode(t *testing.T, kind, id string, payload protocol.Payload, previous *care.Row, seq uint64) care.Row {
	t.Helper()
	now := a.Now.UTC().Format(layout)
	entity := protocol.CanonicalEntity{EntityKind: kind, Payload: payload, Meta: protocol.EntityMeta{ID: id, FamilyID: a.FamilyID, EntityVersion: 1,
		CreatorAccountID: a.AccountID, LastEditorAccountID: a.AccountID, ClientMutationTime: now, AcceptedAt: now}}
	row, err := care.Encode(entity, previous, care.Change{Seq: seq, ActorAccountID: a.AccountID, DeviceID: a.DeviceID, Operation: "create"})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// partID is a feed part id derived from the feed id.
func partID(id string) string { return "3" + id[1:] }

// Write writes rows into the harness family as the owner and commits: one
// tenant transaction, LockFamily, then QueueWrites with the family sequence
// raised to the highest row sequence and counters following the rows (each
// row replaces the family's current row of that id).
func Write(t *testing.T, h *testkit.Harness, rows ...care.Row) error {
	t.Helper()
	return Owner(h).Commit(t, h.Runtime, nil, rows...)
}

// Commit writes rows and receipts as a and commits; see Write.
func (a Author) Commit(t *testing.T, pool *pgxpool.Pool, receipts []care.ReceiptRow, rows ...care.Row) error {
	t.Helper()
	tx := TenantTx(t, pool, a.AccountID, a.FamilyID)
	if err := a.Queue(t, tx, receipts, rows...); err != nil {
		// release the family lock now, not at cleanup
		return errors.Join(err, tx.Rollback(t.Context()))
	}
	return tx.Commit(t.Context())
}

// Queue writes rows and receipts as a inside tx without committing.
func (a Author) Queue(t *testing.T, tx pgx.Tx, receipts []care.ReceiptRow, rows ...care.Row) error {
	t.Helper()
	ctx := t.Context()
	base, err := care.LockFamily(ctx, tx, a.FamilyID)
	if err != nil {
		return err
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	current, err := care.GetMany(ctx, tx, a.FamilyID, ids)
	if err != nil {
		return err
	}
	w := care.Writes{FamilyID: a.FamilyID, Base: base, Sequence: base.Sequence, Rows: rows, Receipts: receipts}
	for _, row := range rows {
		w.Sequence = max(w.Sequence, row.ChangedSeq)
		var previous *care.Row
		if stored, ok := current[row.ID]; ok {
			previous = &stored
		}
		w.Delta = w.Delta.Replace(previous, row)
	}
	batch := &pgx.Batch{}
	check := care.QueueWrites(batch, w)
	results := tx.SendBatch(ctx, batch)
	err = check(results)
	return errors.Join(err, results.Close())
}

// Rows is every care row of the harness family, read through the store in
// a tenant transaction of the owner.
func Rows(t *testing.T, h *testkit.Harness) map[string]care.Row {
	t.Helper()
	tx := TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	defer func() { _ = tx.Rollback(t.Context()) }() // hold no lock past the read
	rows := map[string]care.Row{}
	if err := care.Stream(t.Context(), tx, h.FamilyID, func(r care.Row) error {
		if _, ok := rows[r.ID]; ok {
			return fmt.Errorf("row %s streamed twice", r.ID)
		}
		rows[r.ID] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}
