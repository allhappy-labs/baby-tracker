package care_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/care"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
)

// replayCorpus folds the legacy full pull through Encode, one row per
// entity holding its latest change, as the write path keeps them. A timer
// stop names the completion record the following change created.
func replayCorpus(t *testing.T, c corpus) map[string]care.Row {
	t.Helper()
	envelopes := make([]corpusChange, len(c.FullPull.Changes))
	for index, raw := range c.FullPull.Changes {
		if err := json.Unmarshal(raw, &envelopes[index]); err != nil {
			t.Fatal(err)
		}
	}
	rows := map[string]care.Row{}
	for index, envelope := range envelopes {
		accepted, err := time.Parse(time.RFC3339Nano, envelope.AcceptedAt)
		if err != nil {
			t.Fatal(err)
		}
		change := care.Change{Seq: envelope.Cursor.Sequence, ActorAccountID: envelope.ActorAccountID, DeviceID: envelope.DeviceID,
			Operation: envelope.Operation, AcceptedAt: accepted}
		if envelope.EntityKind == "timer" && envelope.Operation == "timer_stop" {
			next := envelopes[index+1]
			if next.Operation != "timer_stop" || next.EntityKind == "timer" {
				t.Fatalf("timer stop at %d has no completion after it", envelope.Cursor.Sequence)
			}
			change.CompletedRecordID = &next.EntityID
		}
		var previous *care.Row
		if row, ok := rows[envelope.EntityID]; ok {
			previous = &row
		}
		row, err := care.Encode(decodeEntity(t, envelope.SnapshotOrTombstone), previous, change)
		if err != nil {
			t.Fatalf("change %d: %v", envelope.Cursor.Sequence, err)
		}
		rows[envelope.EntityID] = row
	}
	return rows
}

// Every row the encoder produced for the legacy corpus is accepted by the
// care_entities CHECKs (kind shape, derived columns, tombstones, the wire
// bound), and the latest rows are exactly the legacy snapshot.
func TestEncodedCorpusRowsSatisfyTheSchema(t *testing.T) {
	c := loadCorpus(t)
	rows := replayCorpus(t, c)
	if len(rows) != len(c.Snapshot.Entities) {
		t.Fatalf("%d rows, snapshot has %d entities", len(rows), len(c.Snapshot.Entities))
	}
	for _, raw := range c.Snapshot.Entities {
		entity := decodeEntity(t, raw)
		if row := rows[entity.Meta.ID]; !bytes.Equal(row.Wire, raw) {
			t.Fatalf("latest row of %s:\n got %s\nwant %s", entity.Meta.ID, row.Wire, raw)
		}
	}

	f := testpg.New(t)
	seed, err := f.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seed.Rollback(t.Context()) }()
	for _, statement := range []string{
		`SELECT set_config('app.authority','1',true)`,
		`INSERT INTO accounts(id,display_name,status) VALUES ('` + accountA + `','Owner','active')`,
		`INSERT INTO families(id,display_name,status,owner_account_id) VALUES ('` + familyID + `','Golden','active','` + accountA + `')`,
	} {
		if _, err := seed.Exec(t.Context(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	if err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountA, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	ordered := make([]care.Row, 0, len(rows))
	for _, row := range rows {
		ordered = append(ordered, row)
	}
	// Babies first: every record's baby is a row of the family.
	slices.SortFunc(ordered, func(a, b care.Row) int {
		ka, _ := care.KindCode(a.Kind)
		kb, _ := care.KindCode(b.Kind)
		if ka != kb {
			return int(ka) - int(kb)
		}
		return strings.Compare(a.ID, b.ID)
	})
	const insert = `INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,
		operation,accepted_at,tombstoned_at,started_at,ended_at,session_id,timer_kind,completed_record_id,sort_order,label,name_hash,
		breast_seconds,bottle_volume_ml,bottle_observations,bottle_missing,shortest_part_seconds,wire,feed_kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::text::app.care_operation,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22::numeric,$23,$24,$25,$26,$27)`
	for _, row := range ordered {
		kind, ok := care.KindCode(row.Kind)
		if !ok {
			t.Fatalf("%s: kind %q", row.ID, row.Kind)
		}
		var timerKind *int16
		if row.TimerKind != nil {
			code, ok := care.TimerKindCode(*row.TimerKind)
			if !ok {
				t.Fatalf("%s: timer kind %q", row.ID, *row.TimerKind)
			}
			timerKind = &code
		}
		var feedKind *int16
		if row.FeedKind != nil {
			code, ok := care.FeedKindCode(*row.FeedKind)
			if !ok {
				t.Fatalf("%s: feed kind %q", row.ID, *row.FeedKind)
			}
			feedKind = &code
		}
		var volume *string
		if row.Derived.BottleVolumeML != nil {
			value := row.Derived.BottleVolumeML.String()
			volume = &value
		}
		if _, err := tx.Exec(t.Context(), insert, row.FamilyID, row.ID, kind, row.BabyID, int64(row.EntityVersion), int64(row.ChangedSeq),
			row.CreatorAccountID, row.ActorAccountID, row.DeviceID, row.Operation, row.AcceptedAt, row.TombstonedAt, row.StartedAt,
			row.EndedAt, row.SessionID, timerKind, row.CompletedRecordID, row.SortOrder, row.Label, row.NameHash,
			row.Derived.BreastSeconds, volume, row.Derived.BottleObservations, row.Derived.BottleMissing, row.Derived.ShortestPartSeconds,
			row.Wire, feedKind); err != nil {
			t.Fatalf("insert %s %s (seq %d): %v", row.Kind, row.ID, row.ChangedSeq, err)
		}
	}
	// The stored wire is the bytes that went in, and the derived columns
	// read back as encoded.
	for _, row := range ordered {
		var wire []byte
		var breast int32
		var volume *string
		var observations, missing int16
		if err := tx.QueryRow(t.Context(), `SELECT wire, breast_seconds, bottle_volume_ml::text, bottle_observations, bottle_missing
			FROM care_entities WHERE family_id=$1 AND id=$2`, familyID, row.ID).Scan(&wire, &breast, &volume, &observations, &missing); err != nil {
			t.Fatal(err)
		}
		wantVolume := "null"
		if row.Derived.BottleVolumeML != nil {
			wantVolume = row.Derived.BottleVolumeML.String()
		}
		gotVolume := "null"
		if volume != nil {
			gotVolume = strings.TrimRight(strings.TrimRight(*volume, "0"), ".")
		}
		if !bytes.Equal(wire, row.Wire) || breast != row.Derived.BreastSeconds || gotVolume != wantVolume ||
			observations != row.Derived.BottleObservations || missing != row.Derived.BottleMissing {
			t.Fatalf("%s read back breast=%d volume=%s observations=%d missing=%d", row.ID, breast, gotVolume, observations, missing)
		}
	}
}
