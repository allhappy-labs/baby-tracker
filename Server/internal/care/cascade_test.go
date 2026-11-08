package care_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// cascadeOrder is the legacy cascade order of child kinds.
var cascadeOrder = map[string]int{"feed": 0, "sleep": 1, "diaper": 2, "supplement": 3, "growth_profile": 4, "measurement": 5, "immunization": 6, "timer": 7}

// sortedChildren is the live children of baby among rows in cascade order:
// by kind (feed, sleep, diaper, supplement, growth_profile, measurement,
// immunization, timer), then by id.
func sortedChildren(rows map[string]care.Row, baby string) []care.Row {
	var children []care.Row
	for _, r := range rows {
		if r.Live() && r.Kind != "baby" && r.BabyID != nil && *r.BabyID == baby {
			children = append(children, r)
		}
	}
	slices.SortFunc(children, func(x, y care.Row) int {
		if d := cascadeOrder[x.Kind] - cascadeOrder[y.Kind]; d != 0 {
			return d
		}
		return strings.Compare(x.ID, y.ID)
	})
	return children
}

// Review Focus 4: deleting a baby with 5,000 children (records of every
// kind, a growth profile and a running timer) is one transaction of two
// round trips. The root and every child take consecutive sequences in the
// legacy order, the reserves are released and the record count drops by the
// non-timer children, every tombstone reaches another phone across pull
// pages, and a later child mutation is refused baby_deleted.
func TestBabyDeleteCascadeIsSetBasedAndOrdered(t *testing.T) {
	h := testkit.New(t)
	a, b := caretest.BabyA, caretest.BabyB
	caretest.Seed(t, h, caretest.Baby(a), caretest.Baby(b), caretest.Feed(a, 1))
	const children = 5000
	kinds := []string{"feed", "sleep", "diaper", "supplement", "measurement", "immunization"}
	seed := []protocol.Mutation{caretest.GrowthProfile(b), caretest.StartTimer(b, caretest.TimerB, "feed")}
	for n := 0; len(seed) < children; n++ {
		seed = append(seed, caretest.Record(kinds[n%len(kinds)], b, 10+n))
	}
	for from := 0; from < len(seed); from += 500 {
		caretest.Seed(t, h, seed[from:min(from+500, len(seed))]...)
	}
	before := familyStateOf(t, h)
	rows := caretest.Rows(t, h)
	want := sortedChildren(rows, b)
	if len(want) != children {
		t.Fatalf("seeded %d children", len(want))
	}
	nonTimer := int64(0)
	released := int64(0)
	for _, r := range want {
		if r.Kind != "timer" {
			nonTimer++
		}
		released += r.ChargedBytes()
	}

	pool, wire := testkit.CountedPool(t, h.Runtime)
	cmd := caretest.Command(t, h, h.Owner, []protocol.Mutation{deleteBaby(b)})
	var result care.BatchResult
	for _, measure := range []bool{false, true} { // a rolled-back delete warms the statement cache
		tx := caretest.TenantTx(t, pool, h.Owner.AccountID, h.FamilyID)
		wire.Reset()
		var err error
		result, err = care.ApplyBatch(t.Context(), tx, cmd, &care.PullRequest{After: before.Sequence, Limit: 1000})
		if err != nil || result.Fatal != nil {
			t.Fatal(err, result.Fatal)
		}
		if trips := wire.Trips(); measure && trips != 2 {
			t.Fatalf("the cascade took %d round trips: %d statements", trips, len(wire.Statements()))
		}
		if !measure {
			_ = tx.Rollback(t.Context())
			continue
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	base := before.Sequence
	o := result.Outcomes[0]
	if o.Status != "applied" || o.CanonicalEntity.Meta.ID != b || o.CanonicalEntity.Payload != nil || len(o.RelatedEntities) != 0 ||
		o.Cursor.Sequence != base+1+children || result.HighWater != base+1+children {
		t.Fatalf("delete %+v, high water %d", o, result.HighWater)
	}
	if len(result.Changes) != 1000 || result.Changes[0].ID != b || result.Changes[0].Seq != base+1 {
		t.Fatalf("pull page %d changes", len(result.Changes))
	}

	after := caretest.Rows(t, h)
	root := after[b]
	if root.Live() || root.ChangedSeq != base+1 || root.EntityVersion != 2 || root.Operation != "delete" {
		t.Fatalf("root %+v", root)
	}
	for i, was := range want {
		r := after[was.ID]
		if r.Live() || r.ChangedSeq != base+2+uint64(i) || r.EntityVersion != was.EntityVersion+1 || r.Operation != "delete" ||
			r.CreatorAccountID != was.CreatorAccountID || r.ActorAccountID != h.Owner.AccountID || r.DeviceID != h.Owner.DeviceID ||
			r.CompletedRecordID != nil || r.Kind != was.Kind || *r.BabyID != b {
			t.Fatalf("child %d (%s %s): %+v", i, was.Kind, was.ID, r)
		}
	}
	if sibling := after[caretest.RecordID("feed", a, 1)]; !sibling.Live() || !after[a].Live() {
		t.Fatal("the cascade reached baby A")
	}
	state := familyStateOf(t, h)
	if int64(before.RecordCount)-int64(state.RecordCount) != nonTimer || nonTimer != children-1 {
		t.Fatalf("record count %d -> %d, want %d fewer", before.RecordCount, state.RecordCount, nonTimer)
	}
	tombstones := int64(0)
	for _, was := range want {
		tombstones += after[was.ID].ChargedBytes()
	}
	receipt, err := caretest.LegacyReceiptCharge(o)
	if err != nil {
		t.Fatal(err)
	}
	// The delete's receipt is charged (never refused) against the release.
	if delta := int64(before.ChargedBytes) - int64(state.ChargedBytes); delta != released-tombstones+(rows[b].ChargedBytes()-root.ChargedBytes())-receipt {
		t.Fatalf("charged bytes released %d", delta)
	}
	wantCountersMatchRows(t, h)

	// The caregiver's phone pulls every tombstone across pages.
	seen := 0
	for after := base; ; {
		tx := caretest.TenantTx(t, h.Runtime, h.Caregiver.AccountID, h.FamilyID)
		highWater, changes, err := care.ReadPull(t.Context(), tx, h.FamilyID, after, 700)
		_ = tx.Rollback(t.Context())
		if err != nil || highWater != base+1+children {
			t.Fatal(err, highWater)
		}
		for _, c := range changes {
			if c.Seq != base+1+uint64(seen) || c.Operation != "delete" || !strings.Contains(string(c.Wire), `"payload":null`) {
				t.Fatalf("change %d: %+v", seen, c)
			}
			seen++
		}
		if len(changes) == 0 {
			break
		}
		after = changes[len(changes)-1].Seq
	}
	if seen != children+1 {
		t.Fatalf("pulled %d tombstones", seen)
	}

	late := applyOne(t, h, h.Caregiver, caretest.Update(caretest.Record("feed", b, 10), mid(1), 1, caretest.WithNote("late")))
	if late.Status != "rejected" || caretest.Code(late) != "baby_deleted" || late.CanonicalEntity == nil || late.CanonicalEntity.Meta.ID != b ||
		late.CanonicalEntity.Meta.TombstonedAt == nil {
		t.Fatalf("late update %+v", late)
	}
}

// The cascade tombstones the baby's rows this batch created before the
// delete; a create after it is refused baby_deleted.
func TestCascadeIncludesRowsCreatedEarlierInBatch(t *testing.T) {
	h := testkit.New(t)
	b := caretest.BabyB
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Baby(b))
	first, last := caretest.Feed(b, 1), caretest.Feed(b, 2)
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{first, deleteBaby(b), last})
	if result.Fatal != nil || len(result.Outcomes) != 3 {
		t.Fatalf("%v %d", result.Fatal, len(result.Outcomes))
	}
	base := result.Base
	o := result.Outcomes
	if o[0].Status != "applied" || o[0].Cursor.Sequence != base+1 || o[1].Status != "applied" || o[1].Cursor.Sequence != base+3 {
		t.Fatalf("outcomes %+v", o[:2])
	}
	if o[2].Status != "rejected" || caretest.Code(o[2]) != "baby_deleted" || o[2].CanonicalEntity.Meta.ID != b {
		t.Fatalf("create after the delete %+v", o[2])
	}
	rows := caretest.Rows(t, h)
	feed := rows[first.EntityID]
	if feed.Live() || feed.ChangedSeq != base+3 || feed.EntityVersion != 2 || feed.Operation != "delete" || rows[b].ChangedSeq != base+2 {
		t.Fatalf("feed %+v", feed)
	}
	if _, ok := rows[last.EntityID]; ok {
		t.Fatal("the refused create wrote a row")
	}
	wantCountersMatchRows(t, h)
}

// The cascade decides every case as the legacy Repository.DeleteBaby decided
// it, through the differential harness of TestRecordRulesMatchLegacy.
// Ports the cases of records/{child_delete,growth_cascade}_test.go at 342973dc.
func TestBabyDeleteRulesMatchLegacy(t *testing.T) {
	a, b := caretest.BabyA, caretest.BabyB
	babyA, babyB := caretest.Baby(a), caretest.Baby(b)
	feedA := caretest.Feed(a, 1)
	feedB := caretest.Feed(b, 1)
	fullFamily := []protocol.Mutation{babyA, babyB, feedB} // 100 records (the smallest record quota)
	for n := 1; n <= 99; n++ {
		fullFamily = append(fullFamily, caretest.Feed(a, n))
	}
	cases := []legacyCase{
		{name: "every kind in legacy order", setup: []protocol.Mutation{babyA, babyB, caretest.Feed(b, 2), feedB, caretest.Record("sleep", b, 1),
			caretest.Record("diaper", b, 1), caretest.Record("supplement", b, 1), caretest.GrowthProfile(b), caretest.Record("measurement", b, 1),
			caretest.StartTimer(b, caretest.TimerB, "feed"), caretest.StartTimer(b, "50000000-0000-4000-8000-0000000000b2", "sleep"), feedA},
			probe: []protocol.Mutation{deleteBaby(b)}, status: "applied"},
		{name: "a baby without children", setup: []protocol.Mutation{babyA}, probe: []protocol.Mutation{deleteBaby(a)}, status: "applied"},
		{name: "delete refusals", setup: []protocol.Mutation{babyA, babyB, feedA, deleteBaby(b)},
			probe: []protocol.Mutation{deleteBaby("11000000-0000-4000-8000-0000000000c1"), renamed(deleteBaby(a), mid(2), feedA.EntityID),
				renamed(deleteBaby(b), mid(3), b)}, code: "entity_deleted"},
		{name: "rows created and changed earlier in the batch", setup: []protocol.Mutation{babyA, babyB, feedB},
			probe: []protocol.Mutation{caretest.Feed(b, 2), caretest.Update(feedB, mid(1), 1, caretest.WithNote("edited")),
				caretest.StartTimer(b, caretest.TimerB, "sleep"), deleteBaby(b), caretest.Feed(b, 3)}, code: "baby_deleted"},
		{name: "child mutations after the cascade", setup: []protocol.Mutation{babyA, feedA, caretest.StartTimer(a, caretest.TimerA, "feed"),
			caretest.GrowthProfile(a), caretest.Record("measurement", a, 1), deleteBaby(a)},
			probe: []protocol.Mutation{caretest.Feed(a, 2), caretest.Update(feedA, mid(1), 1, nil), caretest.StartTimer(a, caretest.TimerB, "sleep"),
				caretest.Stop(a, caretest.TimerA, caretest.CompletionA), caretest.Update(caretest.GrowthProfile(a), mid(2), 1, nil),
				caretest.Update(caretest.Record("measurement", a, 1), mid(3), 1, nil)}, code: "baby_deleted"},
		{name: "tombstones and stopped timers stay as they are", setup: []protocol.Mutation{babyA, babyB, feedB, caretest.Delete(feedB, mid(1), 1),
			caretest.StartTimer(b, caretest.TimerB, "feed"), caretest.Stop(b, caretest.TimerB, "21000000-0000-4000-8000-0000000000b1")},
			probe: []protocol.Mutation{deleteBaby(b)}, status: "applied"},
		{name: "a delete frees the live baby quota", limits: func(l *protocol.Limits) { l.MaxLiveBabies = 1 }, setup: []protocol.Mutation{babyA},
			probe: []protocol.Mutation{deleteBaby(a), babyB}, status: "applied"},
		{name: "a delete under a full record quota", limits: func(l *protocol.Limits) { l.MaxCanonicalRecordsPerFamily = 100 },
			setup: fullFamily, probe: []protocol.Mutation{deleteBaby(b), caretest.Feed(a, 100)},
			status: "applied"},
	}
	runLegacyCases(t, cases)
}

// Ported from records/child_delete_test.go at 342973dc: the cascade tombstones every
// child (both feeds by id, the sleep, the diaper and the running timer with
// its finished part) in order at consecutive sequences, keeps the rows, and
// releases the record count and storage.
func TestBabyDeleteCascadeIsAtomicAndOrdered(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a))
	right, sideAt := "right", "2026-09-21T11:10:00.000000Z"
	second := caretest.Feed(a, 2)
	second.EntityID, second.MutationID = "20000000-0000-4000-8000-000000000000", mid(4)
	caretest.Seed(t, h, caretest.Feed(a, 1), caretest.Record("sleep", a, 1), caretest.Record("diaper", a, 1), caretest.Edit(second, nil),
		timerStartWith(caretest.TimerA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed", CurrentSide: &right, CurrentSideStartedAt: &sideAt,
			CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 600)}}))
	before := familyStateOf(t, h)
	if before.RecordCount != 4 {
		t.Fatalf("record count %d", before.RecordCount)
	}
	o := applyOne(t, h, h.Owner, deleteBaby(a))
	if o.Status != "applied" || o.CanonicalEntity.EntityKind != "baby" || o.CanonicalEntity.Payload != nil || len(o.RelatedEntities) != 0 ||
		o.Cursor.Sequence != before.Sequence+6 {
		t.Fatalf("delete %+v", o)
	}
	tx := caretest.TenantTx(t, h.Runtime, h.Caregiver.AccountID, h.FamilyID)
	_, changes, err := care.ReadPull(t.Context(), tx, h.FamilyID, before.Sequence, 100)
	_ = tx.Rollback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var kinds, ids []string
	for i, c := range changes {
		if c.Operation != "delete" || c.Seq != before.Sequence+uint64(i)+1 || !strings.Contains(string(c.Wire), `"payload":null`) {
			t.Fatalf("change %d %+v", i, c)
		}
		kinds, ids = append(kinds, c.Kind), append(ids, c.ID)
	}
	if !reflect.DeepEqual(kinds, []string{"baby", "feed", "feed", "sleep", "diaper", "timer"}) || ids[1] != second.EntityID ||
		ids[2] != caretest.RecordID("feed", a, 1) {
		t.Fatalf("changes %v %v", kinds, ids)
	}
	rows := caretest.Rows(t, h)
	if len(rows) != 6 {
		t.Fatalf("the cascade removed rows: %d", len(rows))
	}
	if timer := rows[caretest.TimerA]; timer.Derived.ShortestPartSeconds != nil || timer.CompletedRecordID != nil {
		t.Fatalf("the timer tombstone kept its parts: %+v", timer)
	}
	if after := familyStateOf(t, h); after.RecordCount != 0 || after.ChargedBytes >= before.ChargedBytes {
		t.Fatalf("usage %+v, was %+v", after, before)
	}
	wantCountersMatchRows(t, h)
}

// Ported from records/child_delete_test.go at 342973dc: after the cascade, a delayed
// child create, update, timer start and timer stop are refused
// baby_deleted.
func TestBabyDeleteDelayedChildMutationsGetBabyDeleted(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	feed := caretest.Feed(a, 1)
	caretest.Seed(t, h, caretest.Baby(a))
	applyOne(t, h, h.Caregiver, feed)
	caretest.Seed(t, h, caretest.StartTimer(a, caretest.TimerA, "feed"), deleteBaby(a))
	for _, m := range []protocol.Mutation{caretest.Feed(a, 2), caretest.Update(feed, mid(1), 1, nil), caretest.StartTimer(a, caretest.TimerB, "feed"),
		caretest.Stop(a, caretest.TimerA, caretest.CompletionA)} {
		if o := applyOne(t, h, h.Caregiver, m); o.Status != "rejected" || caretest.Code(o) != "baby_deleted" || o.CanonicalEntity.Meta.ID != a {
			t.Fatalf("%s %s: %+v", m.EntityKind, m.Operation, o)
		}
	}
}

// Ported from records/child_delete_test.go at 342973dc: a caregiver's baby delete stops
// the batch before any receipt or sequence.
func TestBabyDeleteByCaregiverFailsBeforeReceipt(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	before, receipts := familyStateOf(t, h), receiptCount(t, h)
	result := caretest.ApplyCommitted(t, h, h.Caregiver, []protocol.Mutation{deleteBaby(caretest.BabyA)})
	if !errors.Is(result.Fatal, authority.ErrOwnerRequired) || len(result.Outcomes) != 0 {
		t.Fatalf("%v %+v", result.Fatal, result.Outcomes)
	}
	if familyStateOf(t, h) != before || receiptCount(t, h) != receipts {
		t.Fatal("the refused delete changed the receipts or the cursor")
	}
}

// Ported from records/{child_delete,growth_cascade}_test.go at 342973dc: a database
// failure on any tombstone of the cascade (here the last measurement's)
// rolls the whole cascade back, sequence, counters and receipt included;
// the owner's retry then cascades in order (baby, growth profile,
// measurements), and late edits of the growth profile and a measurement are
// refused baby_deleted.
func TestBabyDeleteFailureRollsBackTheWholeCascade(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a))
	children := []protocol.Mutation{caretest.GrowthProfile(a)}
	for n := 1; n <= 4100; n++ { // past the legacy 4,096-row cascade batch
		children = append(children, caretest.Record("measurement", a, n))
	}
	for from := 0; from < len(children); from += 500 {
		caretest.Seed(t, h, children[from:min(from+500, len(children))]...)
	}
	last := caretest.RecordID("measurement", a, 4100)
	if _, err := h.Migrator.Exec(t.Context(), `CREATE FUNCTION reject_cascade_tombstone() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '`+last+`'::uuid AND NEW.tombstoned_at IS NOT NULL THEN
				RAISE EXCEPTION 'injected cascade failure' USING ERRCODE = '23514';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_cascade_tombstone BEFORE UPDATE ON care_entities
		FOR EACH ROW EXECUTE FUNCTION reject_cascade_tombstone()`); err != nil {
		t.Fatal(err)
	}
	before, rows, receipts := familyStateOf(t, h), caretest.Rows(t, h), receiptCount(t, h)
	deletion := deleteBaby(a)
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := care.ApplyBatch(t.Context(), tx, caretest.Command(t, h, h.Owner, []protocol.Mutation{deletion}), nil); err == nil ||
		!strings.Contains(err.Error(), "injected cascade failure") {
		t.Fatalf("the injected failure was not reported: %v", err)
	}
	_ = tx.Rollback(t.Context())
	if familyStateOf(t, h) != before || receiptCount(t, h) != receipts || !reflect.DeepEqual(caretest.Rows(t, h), rows) {
		t.Fatal("the failed cascade left a change")
	}
	if _, err := h.Migrator.Exec(t.Context(), `DROP TRIGGER reject_cascade_tombstone ON care_entities`); err != nil {
		t.Fatal(err)
	}
	if r := caretest.ApplyCommitted(t, h, h.Caregiver, []protocol.Mutation{deletion}); !errors.Is(r.Fatal, authority.ErrOwnerRequired) {
		t.Fatalf("caregiver delete %v", r.Fatal)
	}
	if o := applyOne(t, h, h.Owner, deletion); o.Status != "applied" || o.Cursor.Sequence != before.Sequence+uint64(len(children))+1 {
		t.Fatalf("delete %+v", o)
	}
	after := caretest.Rows(t, h)
	if g := after[protocol.GrowthProfileID(a)]; g.ChangedSeq != before.Sequence+2 || g.Live() {
		t.Fatalf("growth profile %+v", g)
	}
	if m := after[caretest.RecordID("measurement", a, 1)]; m.ChangedSeq != before.Sequence+3 || m.Live() {
		t.Fatalf("first measurement %+v", m)
	}
	if state := familyStateOf(t, h); state.RecordCount != 0 {
		t.Fatalf("record count %d", state.RecordCount)
	}
	for i, m := range []protocol.Mutation{caretest.GrowthProfile(a), caretest.Record("measurement", a, 1)} {
		edit := caretest.Update(m, fmt.Sprintf("0e%06d-0000-4000-8000-0000000000a1", 900+i), 1, nil)
		if o := applyOne(t, h, h.Caregiver, edit); caretest.Code(o) != "baby_deleted" {
			t.Fatalf("late %s edit %+v", m.EntityKind, o)
		}
	}
	wantCountersMatchRows(t, h)
}
