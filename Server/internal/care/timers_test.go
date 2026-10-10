package care_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// Every timer here starts at caretest.TimerAt, an hour before
// testkit.FixtureClock (2026-09-21T12:00:00Z), so a moved start has room on
// both sides. A test whose edits, stops or parts are judged against the
// service clock (a start a week back to a minute ahead, parts within the time
// elapsed) runs on testkit.NewAtFixtureClock.
const (
	at1100 = caretest.TimerAt
	partA  = "36000000-0000-4000-8000-000000000001"
	partB  = "36000000-0000-4000-8000-000000000002"
	partC  = "36000000-0000-4000-8000-000000000003"
	partD  = "36000000-0000-4000-8000-000000000004"
)

func breastPart(id, side string, seconds uint32) protocol.FeedComponent {
	return protocol.FeedComponent{ID: id, Kind: "breast", Side: &side, Seconds: &seconds}
}

func bottlePart(id, volume, contents string) protocol.FeedComponent {
	return protocol.FeedComponent{ID: id, Kind: "bottle", VolumeML: protocol.MustParseDecimal(volume), Contents: &contents}
}

// timerStartWith is the start of timer as its n-th command, with payload p
// (at 11:00 in Zurich, no finished parts, unless p says otherwise).
func timerStartWith(timer string, n int, p protocol.TimerStartPayload) protocol.Mutation {
	if p.StartedAt == "" {
		p.StartedAt = at1100
	}
	if p.TimeZoneID == "" {
		p.TimeZoneID = "Europe/Zurich"
	}
	if p.CompletedComponents == nil {
		p.CompletedComponents = []protocol.FeedComponent{}
	}
	return caretest.TimerCommand(timer, n, "timer_start", p.StartedAt, 0, p)
}

// timerEditOf is a caregiver's edit of timer sent at 11:59 (observed
// version 1): a new start when startedAt is set, and parts.
func timerEditOf(timer string, n int, baby string, startedAt *string, parts ...protocol.FeedComponent) protocol.Mutation {
	return caretest.TimerCommand(timer, n, "update", "2026-09-21T11:59:00.000000Z", 1,
		protocol.TimerEditPayload{BabyID: baby, StartedAt: startedAt, Components: parts})
}

// timerSessionEdit is an edit of timer that only sets its session.
func timerSessionEdit(timer string, n int, baby string, session *string) protocol.Mutation {
	return caretest.TimerCommand(timer, n, "update", "2026-09-21T11:59:00.000000Z", 1,
		protocol.TimerEditPayload{BabyID: baby, SessionID: session})
}

func timerFinishOf(timer string, n int, baby, at, part, volume string) protocol.Mutation {
	formula := "formula"
	return caretest.TimerCommand(timer, n, "timer_finish_bottle", at, 1, protocol.TimerBottleFinishPayload{BabyID: baby, FinishedAt: at,
		ComponentID: part, VolumeML: protocol.MustParseDecimal(volume), Contents: &formula})
}

// timerStopOf is the stop of timer at at into completion; part names the
// running side's part ("" none), and edit adjusts the payload.
func timerStopOf(timer string, n int, baby, completion, at, part string, edit func(*protocol.TimerStopPayload)) protocol.Mutation {
	p := protocol.TimerStopPayload{BabyID: baby, CompletedRecordID: completion, StoppedAt: at, ComponentID: part}
	if edit != nil {
		edit(&p)
	}
	return caretest.TimerCommand(timer, n, "timer_stop", at, 0, p)
}

func cleanup(p *protocol.TimerStopPayload) { p.CleanupShortBreastSegments = true }

// applyOne applies m alone as p, committed, and returns its outcome; a
// refused batch fails the test.
func applyOne(t *testing.T, h *testkit.Harness, p access.Principal, m protocol.Mutation) protocol.Outcome {
	t.Helper()
	result := caretest.ApplyCommitted(t, h, p, []protocol.Mutation{m})
	if result.Fatal != nil || len(result.Outcomes) != 1 {
		t.Fatalf("%s %s: %v, %d outcomes", m.EntityKind, m.Operation, result.Fatal, len(result.Outcomes))
	}
	return result.Outcomes[0]
}

// kindRows counts the family's rows of kind, tombstones included (the
// legacy per-kind table count), and the live ones.
func kindRows(t *testing.T, h *testkit.Harness, kind string) (all, live int) {
	t.Helper()
	for _, r := range caretest.Rows(t, h) {
		if r.Kind == kind {
			all++
			if r.Live() {
				live++
			}
		}
	}
	return all, live
}

func receiptCount(t *testing.T, h *testkit.Harness) int {
	t.Helper()
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	defer func() { _ = tx.Rollback(t.Context()) }()
	var n int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM care_receipts WHERE family_id = $1`, h.FamilyID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func timerPayload(t *testing.T, o protocol.Outcome) protocol.TimerStartPayload {
	t.Helper()
	if o.Status != "applied" || o.CanonicalEntity == nil {
		t.Fatalf("outcome %+v code %q", o, caretest.Code(o))
	}
	p, ok := o.CanonicalEntity.Payload.(protocol.TimerStartPayload)
	if !ok {
		t.Fatalf("canonical payload %T", o.CanonicalEntity.Payload)
	}
	return p
}

func wantRefused(t *testing.T, o protocol.Outcome, status, code, timer string) {
	t.Helper()
	if o.Status != status || caretest.Code(o) != code {
		t.Fatalf("outcome %+v code %q, want %s/%s", o, caretest.Code(o), status, code)
	}
	if o.CanonicalEntity == nil || o.CanonicalEntity.Meta.ID != timer {
		t.Fatalf("refusal did not carry the timer: %+v", o)
	}
}

// storedTimer is the timer's row decoded, as the next command is judged
// against it and as a snapshot serves it.
func storedTimer(t *testing.T, h *testkit.Harness, id string) (care.Row, protocol.CanonicalEntity) {
	t.Helper()
	row, ok := caretest.Rows(t, h)[id]
	if !ok {
		t.Fatalf("timer %s has no row", id)
	}
	entity, err := row.Entity()
	if err != nil {
		t.Fatal(err)
	}
	return row, entity
}

// Review Focus 5: a timer started, its bottle finished, edited and stopped
// in one offline batch chains through the working set: each command reads
// the row the one before it wrote, the timer is written once (its stop
// tombstone) and the completion once, at the batch's last two sequences.
func TestTimerLifecycleInOneBatch(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	start, finish, edit, stop := caretest.FeedTimerLifecycle(caretest.BabyA, caretest.TimerA, caretest.CompletionA)
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{start, finish, edit, stop})
	if result.Fatal != nil || len(result.Outcomes) != 4 {
		t.Fatalf("%v, %d outcomes", result.Fatal, len(result.Outcomes))
	}
	base := result.Base
	want := []uint64{base + 1, base + 2, base + 3, base + 5} // start, finish, edit, stop's final cursor (tombstone base+4, completion base+5)
	for i, o := range result.Outcomes {
		if o.Status != "applied" || o.Cursor.Sequence != want[i] {
			t.Fatalf("outcome %d %+v", i, o)
		}
		if o.CanonicalEntity.Meta.EntityVersion != uint64(i+1) {
			t.Fatalf("outcome %d version %d", i, o.CanonicalEntity.Meta.EntityVersion)
		}
		wantReceipt(t, h, h.Owner, o)
	}
	if result.HighWater != base+5 {
		t.Fatalf("high water %d", result.HighWater)
	}
	edited := timerPayload(t, result.Outcomes[2])
	if len(edited.CompletedComponents) != 1 || edited.CompletedComponents[0].VolumeML.String() != "95" || edited.CurrentBottleStartedAt != nil {
		t.Fatalf("edited %+v", edited)
	}
	if related := result.Outcomes[3].RelatedEntities; len(related) != 1 || related[0].Meta.ID != caretest.CompletionA ||
		result.Outcomes[3].CanonicalEntity.Payload != nil {
		t.Fatalf("stop %+v", result.Outcomes[3])
	}
	rows := caretest.Rows(t, h)
	timer, completion := rows[caretest.TimerA], rows[caretest.CompletionA]
	if timer.ChangedSeq != base+4 || timer.TombstonedAt == nil || timer.CompletedRecordID == nil || *timer.CompletedRecordID != caretest.CompletionA ||
		timer.Operation != "timer_stop" || timer.EntityVersion != 4 {
		t.Fatalf("timer %+v", timer)
	}
	if completion.ChangedSeq != base+5 || completion.Kind != "feed" || completion.Operation != "timer_stop" || completion.Derived.BottleObservations != 1 ||
		completion.Derived.BottleVolumeML.String() != "95" || completion.EntityVersion != 1 {
		t.Fatalf("completion %+v", completion)
	}
	if string(completion.Wire) != string(mustJSON(t, result.Outcomes[3].RelatedEntities[0])) ||
		string(timer.Wire) != string(mustJSON(t, result.Outcomes[3].CanonicalEntity)) {
		t.Fatal("rows are not the stop's entities")
	}
	wantCountersMatchRows(t, h)

	late := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{caretest.Stop(caretest.BabyA, caretest.TimerA, "21000000-0000-4000-8000-0000000000aa")})
	if o := late.Outcomes[0]; o.Status != "superseded" || *o.Code != "timer_ended" || o.Cursor.Sequence != base+5 || len(o.RelatedEntities) != 1 ||
		o.RelatedEntities[0].Meta.ID != caretest.CompletionA || late.HighWater != base+5 {
		t.Fatalf("late stop %+v", o)
	}
	// The same commands sent again replay their receipts.
	replay := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{start, finish, edit, stop})
	for i, o := range replay.Outcomes {
		if o.Status != "already_applied" || *o.OriginalStatus != "applied" || o.Cursor.Sequence != want[i] {
			t.Fatalf("replay %d %+v", i, o)
		}
	}
}

// A start is superseded by the baby's live timer of its kind, committed or
// started earlier in the batch, until that timer stops; the other kind
// starts beside it. The one-live-timer index never sees two.
func TestTimerStartRules(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"))
	second, third, fourth := "50000000-0000-4000-8000-0000000000a2", "50000000-0000-4000-8000-0000000000a3", "50000000-0000-4000-8000-0000000000a4"
	restart := caretest.StartTimer(a, second, "feed")
	restart.MutationID = "05000009-0000-4000-8000-0000000000a2"
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{
		caretest.StartTimer(a, second, "feed"),  // superseded by the committed feed timer
		caretest.StartTimer(a, third, "sleep"),  // the other kind starts
		caretest.StartTimer(a, fourth, "sleep"), // superseded by the sleep timer of this batch
		caretest.Stop(a, caretest.TimerA, caretest.CompletionA),
		restart, // the feed kind is free again
	})
	if result.Fatal != nil || len(result.Outcomes) != 5 {
		t.Fatalf("%v %d", result.Fatal, len(result.Outcomes))
	}
	o := result.Outcomes
	base := result.Base
	if o[0].Status != "superseded" || o[0].CanonicalEntity.Meta.ID != caretest.TimerA || o[0].Cursor.Sequence != base || o[0].Code != nil {
		t.Fatalf("second feed timer %+v", o[0])
	}
	if o[1].Status != "applied" || o[1].Cursor.Sequence != base+1 {
		t.Fatalf("sleep timer %+v", o[1])
	}
	if o[2].Status != "superseded" || o[2].CanonicalEntity.Meta.ID != third || o[2].Cursor.Sequence != base+1 {
		t.Fatalf("second sleep timer %+v", o[2])
	}
	if o[3].Status != "applied" || o[3].Cursor.Sequence != base+3 || o[4].Status != "applied" || o[4].Cursor.Sequence != base+4 {
		t.Fatalf("stop %+v, restart %+v", o[3], o[4])
	}
	rows := caretest.Rows(t, h)
	live := map[string]string{}
	for _, r := range rows {
		if r.Kind == "timer" && r.Live() {
			if other, ok := live[*r.TimerKind]; ok {
				t.Fatalf("two live %s timers: %s and %s", *r.TimerKind, other, r.ID)
			}
			live[*r.TimerKind] = r.ID
		}
	}
	if live["feed"] != second || live["sleep"] != third {
		t.Fatalf("live timers %v", live)
	}
	if _, ok := rows[fourth]; ok {
		t.Fatal("a superseded start wrote a row")
	}
	// A later batch's start of a kind already running is superseded too.
	late := applyOne(t, h, h.Caregiver, caretest.StartTimer(a, "50000000-0000-4000-8000-0000000000a5", "sleep"))
	if late.Status != "superseded" || late.CanonicalEntity.Meta.ID != third || late.Cursor.Sequence != base+1 {
		t.Fatalf("late start %+v", late)
	}
	wantCountersMatchRows(t, h)
}

// A timer batch is two round trips like any other: the start's live-timer
// lookup, the stop's completion probe and an ended timer's completion are
// all read by the one Fetch.
func TestTimerBatchIsTwoRoundTrips(t *testing.T) {
	h := testkit.New(t)
	a, b := caretest.BabyA, caretest.BabyB
	caretest.Seed(t, h, caretest.Baby(a), caretest.Baby(b), caretest.StartTimer(b, caretest.TimerB, "feed"),
		caretest.StartTimer(b, "50000000-0000-4000-8000-0000000000b3", "sleep"))
	caretest.Seed(t, h, caretest.Stop(b, "50000000-0000-4000-8000-0000000000b3", "31000000-0000-4000-8000-0000000000b3"))
	start, finish, edit, stop := caretest.FeedTimerLifecycle(a, caretest.TimerA, caretest.CompletionA)
	batch := []protocol.Mutation{start, finish, edit, stop,
		caretest.StartTimer(b, "50000000-0000-4000-8000-0000000000b2", "feed"),                           // superseded by the live feed timer
		caretest.Stop(b, caretest.TimerB, "21000000-0000-4000-8000-0000000000b1"),                        // stops it
		caretest.Stop(b, "50000000-0000-4000-8000-0000000000b3", "21000000-0000-4000-8000-0000000000b4"), // ended earlier
		caretest.Stop(a, caretest.TimerA, "21000000-0000-4000-8000-0000000000a9"),                        // ended in this batch
	}
	pool, wire := testkit.CountedPool(t, h.Runtime)
	cmd := caretest.Command(t, h, h.Owner, batch)
	var result care.BatchResult
	for _, measure := range []bool{false, true} {
		tx := caretest.TenantTx(t, pool, h.Owner.AccountID, h.FamilyID)
		wire.Reset()
		var err error
		result, err = care.ApplyBatch(t.Context(), tx, cmd, &care.PullRequest{After: 0, Limit: 100})
		if err != nil || result.Fatal != nil {
			t.Fatal(err, result.Fatal)
		}
		if trips := wire.Trips(); measure && trips != 2 {
			t.Fatalf("%d round trips: %q", trips, wire.Statements())
		}
		_ = tx.Rollback(t.Context())
	}
	statuses := make([]string, len(result.Outcomes))
	for i, o := range result.Outcomes {
		statuses[i] = o.Status + "/" + caretest.Code(o)
	}
	if want := []string{"applied/", "applied/", "applied/", "applied/", "superseded/", "applied/", "superseded/timer_ended", "superseded/timer_ended"}; !reflect.DeepEqual(statuses, want) {
		t.Fatalf("outcomes %v", statuses)
	}
	rows := map[string]care.Row{}
	for _, c := range result.Changes {
		rows[c.ID] = care.Row{ID: c.ID, Wire: c.Wire}
	}
	wantWireRoundTrips(t, rows)
}

// The timer rules decide every case as the legacy Repository.ApplyTimer decided
// it: each case's probe batch gives the legacy outcomes byte for byte, the
// same refusal, the same sequences and the same pulled state (the
// differential harness of TestRecordRulesMatchLegacy). Ports the cases of
// records/{timers,timer_edit,timed_bottle}_test.go at 342973dc.
func TestTimerRulesMatchLegacy(t *testing.T) {
	runLegacyCases(t, timerLegacyCases(t))
}

func timerLegacyCases(t *testing.T) []legacyCase {
	t.Helper()
	a, b := caretest.BabyA, caretest.BabyB
	babyA, babyB := caretest.Baby(a), caretest.Baby(b)
	tA, tB, tC := caretest.TimerA, caretest.TimerB, "50000000-0000-4000-8000-0000000000a2"
	cA, cB, cC := caretest.CompletionA, "21000000-0000-4000-8000-0000000000a2", "21000000-0000-4000-8000-0000000000a3"
	left, right := "left", "right"
	start, finish, edit, stop := caretest.FeedTimerLifecycle(a, tA, cA)
	feedStart := caretest.StartTimer(a, tA, "feed") // the left side running since 11:00
	sleepStart := caretest.StartTimer(a, tB, "sleep")
	bottleAt := at1100
	bottleStart := timerStartWith(tA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed", CurrentBottleStartedAt: &bottleAt})
	rawBottleAt := "2026-09-21T11:00:00Z" // the wire decoder leaves a bottle start as sent
	rawBottleStart := timerStartWith(tA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed", CurrentBottleStartedAt: &rawBottleAt})
	rawPart := func(seconds uint32) protocol.FeedComponent { // bounds as sent: the wire decoder leaves part bounds raw
		part := breastPart(partA, "left", seconds)
		part.StartedAt, part.EndedAt = ptr("2026-09-21T11:00:00Z"), ptr("2026-09-21T11:05:00Z")
		return part
	}
	rawPartStart := timerStartWith(tA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed",
		CompletedComponents: []protocol.FeedComponent{rawPart(300)}})
	sideAt := "2026-09-21T11:10:00.000000Z"
	withPart := timerStartWith(tA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed", CurrentSide: &right, CurrentSideStartedAt: &sideAt,
		CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 600)}})
	threeParts := timerStartWith(tA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed",
		CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 600), breastPart(partB, "right", 300), bottlePart(partC, "60", "formula")}})
	feed := caretest.Feed(a, 1)
	session, otherSession := "aaaaaaaa-0000-4000-8000-000000000002", "aaaaaaaa-0000-4000-8000-000000000003"
	inSession := func(m protocol.Mutation, s string) protocol.Mutation {
		p := m.Payload.(protocol.TimerStartPayload)
		p.SessionID = &s
		m.Payload = p
		return caretest.Edit(m, nil)
	}
	at := func(clock string) *string { v := "2026-09-21T" + clock; return &v }
	completionFeed := protocol.Mutation{EntityID: cA, EntityKind: "feed", Payload: protocol.FeedPayload{BabyID: a}}
	olderVersion := func(m protocol.Mutation, version uint32) protocol.Mutation {
		m.PayloadVersion = version // as an older client sends it, past the wire decoder
		return m
	}
	hundredRecords := []protocol.Mutation{babyA}
	for n := 1; n <= 100; n++ {
		hundredRecords = append(hundredRecords, caretest.Feed(a, n))
	}
	briefAt := at("11:00:11.000000Z")
	briefStop := timerStopOf(tA, 2, a, cA, *briefAt, partD, cleanup)

	return []legacyCase{
		// The lifecycle, in one batch and across batches.
		{name: "timer lifecycle in one batch", setup: []protocol.Mutation{babyA}, probe: []protocol.Mutation{start, finish, edit, stop}, status: "applied"},
		{name: "timer lifecycle across batches, then ended", setup: []protocol.Mutation{babyA, start, finish},
			probe: []protocol.Mutation{edit, stop, caretest.Stop(a, tA, cB), timerEditOf(tA, 9, a, at("10:50:00Z")), timerFinishOf(tA, 10, a, *at("11:06:00.000000Z"), partD, "60")},
			code:  "timer_ended"},

		// Start.
		{name: "a resent start is superseded by its timer", setup: []protocol.Mutation{babyA, feedStart},
			probe: []protocol.Mutation{renamed(feedStart, mid(1), tA)}, status: "superseded"},
		{name: "a resent start with finished parts is superseded by its timer", setup: []protocol.Mutation{babyA, threeParts},
			probe: []protocol.Mutation{renamed(threeParts, mid(1), tA)}, status: "superseded"},
		{name: "a stored bottle start reads back normalized", setup: []protocol.Mutation{babyA, rawBottleStart},
			probe: []protocol.Mutation{renamed(rawBottleStart, mid(1), tA), timerFinishOf(tA, 2, a, *at("11:05:00.000000Z"), partD, "80"),
				timerStopOf(tA, 3, a, cA, *at("11:06:00.000000Z"), "", nil)}, status: "applied"},
		{name: "stored part bounds read back normalized", setup: []protocol.Mutation{babyA, rawPartStart},
			probe: []protocol.Mutation{renamed(rawPartStart, mid(1), tA), timerEditOf(tA, 2, a, nil, rawPart(300))}, status: "applied"},
		{name: "a start on another entity's id", setup: []protocol.Mutation{babyA, feed, feedStart},
			probe: []protocol.Mutation{renamed(sleepStart, mid(1), feed.EntityID), renamed(sleepStart, mid(2), tA)}, code: "entity_exists"},
		{name: "a start on a stopped timer's id", setup: []protocol.Mutation{babyA, feedStart, caretest.Stop(a, tA, cA)},
			probe: []protocol.Mutation{renamed(feedStart, mid(1), tA)}, code: "entity_exists"},
		{name: "the live timer of a kind supersedes a start; the other kind starts", setup: []protocol.Mutation{babyA, feedStart},
			probe:  []protocol.Mutation{caretest.StartTimer(a, tC, "feed"), sleepStart, caretest.StartTimer(a, "50000000-0000-4000-8000-0000000000a3", "sleep")},
			status: "superseded"},
		{name: "a stop earlier in the batch frees the kind", setup: []protocol.Mutation{babyA, feedStart},
			probe: []protocol.Mutation{caretest.Stop(a, tA, cA), caretest.StartTimer(a, tC, "feed")}, status: "applied"},
		{name: "a start for a deleted, missing or non-baby baby", setup: []protocol.Mutation{babyA, babyB, feed, deleteBaby(b)},
			probe: []protocol.Mutation{caretest.StartTimer(b, tA, "feed"), caretest.StartTimer("11000000-0000-4000-8000-0000000000c1", tB, "sleep"),
				caretest.StartTimer(feed.EntityID, tC, "sleep")}, code: "baby_not_found"},
		{name: "a start in a session another baby or kind owns", setup: []protocol.Mutation{babyA, babyB, inSessionOf(caretest.Feed(b, 1), session)},
			probe: []protocol.Mutation{inSession(feedStart, session), inSession(caretest.StartTimer(b, tB, "sleep"), session),
				inSession(caretest.StartTimer(b, tC, "feed"), session)}, status: "applied"},

		// Finish a bottle.
		{name: "a finish on a timer without a running bottle", setup: []protocol.Mutation{babyA, sleepStart, feedStart},
			probe: []protocol.Mutation{timerFinishOf(tB, 2, a, *at("11:05:00.000000Z"), partD, "60"), timerFinishOf(tA, 2, a, *at("11:05:00.000000Z"), partD, "60")},
			code:  "invalid_timer_kind"},
		{name: "a finish's bottle time", setup: []protocol.Mutation{babyA, timerStartWith(tA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed",
			CurrentBottleStartedAt: &bottleAt, CompletedComponents: []protocol.FeedComponent{bottlePart(partA, "30", "formula")}})},
			probe: []protocol.Mutation{timerFinishOf(tA, 2, a, at1100, partB, "60"), timerFinishOf(tA, 3, a, "2026-09-28T11:00:01.000000Z", partB, "60"),
				timerFinishOf(tA, 4, a, *at("11:05:00.000000Z"), partA, "60")}, code: "invalid_timer_time"},

		// Stop.
		{name: "stop refusals", setup: []protocol.Mutation{babyA, babyB, bottleStart, sleepStart, caretest.StartTimer(b, tC, "feed")},
			probe: []protocol.Mutation{caretest.Stop(a, tA, cA), timerStopOf(tB, 2, a, cA, *at("10:59:00.000000Z"), "", nil),
				timerStopOf(tB, 3, a, cA, "2026-09-28T11:00:01.000000Z", "", nil), timerStopOf(tB, 4, a, cA, *at("11:10:00.000000Z"), "", cleanup),
				timerStopOf(tC, 2, b, cA, *at("11:10:00.000000Z"), "", nil)}, code: "invalid_timer_time"},
		{name: "a stop a week after the start with nothing to complete", setup: []protocol.Mutation{babyA, timerStartWith(tA, 1,
			protocol.TimerStartPayload{BabyID: a, TimerKind: "feed", CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 3)}})},
			probe: []protocol.Mutation{timerStopOf(tA, 2, a, cA, "2026-09-28T11:00:01.000000Z", "", cleanup)}, code: "invalid_timer_time"},
		{name: "a completion id the family already uses", setup: []protocol.Mutation{babyA, babyB, feed, caretest.Delete(feed, mid(50), 1),
			caretest.Record("sleep", a, 1), feedStart, caretest.StartTimer(b, tC, "sleep"), caretest.Stop(b, tC, cB)},
			probe: []protocol.Mutation{caretest.Stop(a, tA, feed.EntityID), caretest.Stop(a, tA, caretest.RecordID("sleep", a, 1)), caretest.Stop(a, tA, tA),
				caretest.Stop(a, tA, tC), caretest.Stop(a, tA, b), caretest.Stop(a, tA, cB), caretest.Feed(a, 2), caretest.Stop(a, tA, caretest.RecordID("feed", a, 2))},
			code: "completed_record_exists"},
		{name: "a sleep stop with a note and a kind", setup: []protocol.Mutation{babyA, sleepStart},
			probe: []protocol.Mutation{timerStopOf(tB, 2, a, cB, *at("11:45:00.000000Z"), "", func(p *protocol.TimerStopPayload) {
				note, kind := "Woke up <happy> & 😴", "nap"
				p.Note, p.SleepKindOverride = &note, &kind
			})}, status: "applied"},
		{name: "an all-brief feed stop leaves no completion", setup: []protocol.Mutation{babyA, feedStart},
			probe: []protocol.Mutation{briefStop, timerStopOf(tA, 3, a, cB, *at("11:00:12.000000Z"), "", cleanup), timerFinishOf(tA, 4, a, *at("11:00:13.000000Z"), partC, "60")},
			code:  "timer_ended"},
		{name: "cleanup drops a brief switch and joins a side", setup: []protocol.Mutation{babyA, timerStartWith(tA, 1, protocol.TimerStartPayload{BabyID: a,
			TimerKind: "feed", CurrentSide: &left, CurrentSideStartedAt: at("11:07:03.000000Z"),
			CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 300), breastPart(partB, "right", 3), breastPart(partC, "left", 120)}})},
			probe: []protocol.Mutation{timerStopOf(tA, 2, a, cA, *at("11:09:03.000000Z"), partD, cleanup)}, status: "applied"},
		{name: "a brief breast before a bottle completes as a bottle", setup: []protocol.Mutation{babyA, timerStartWith(tA, 1, protocol.TimerStartPayload{BabyID: a,
			TimerKind: "feed", CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 3), bottlePart(partB, "60", "formula")}})},
			probe: []protocol.Mutation{timerStopOf(tA, 2, a, cA, *at("11:00:04.000000Z"), "", cleanup)}, status: "applied"},
		{name: "a timer not found or of another baby", setup: []protocol.Mutation{babyA, babyB, feed, feedStart},
			probe: []protocol.Mutation{caretest.Stop(a, tB, cA), caretest.Stop(a, feed.EntityID, cB), caretest.Stop(b, tA, cC),
				timerFinishOf(tA, 3, b, *at("11:05:00.000000Z"), partD, "60"), timerEditOf(tA, 4, b, nil, breastPart(partA, "left", 60))},
			code: "record_parent_mismatch"},
		{name: "commands on a stopped timer", setup: []protocol.Mutation{babyA, babyB, feedStart, caretest.Stop(a, tA, cA)},
			probe: []protocol.Mutation{caretest.Stop(b, tA, cB), timerEditOf(tA, 5, b, nil), timerFinishOf(tA, 6, a, *at("11:12:00.000000Z"), partD, "60"),
				caretest.Stop(a, tA, cC)}, code: "timer_ended"},
		{name: "a stopped timer whose completion was deleted", setup: []protocol.Mutation{babyA, feedStart, caretest.Stop(a, tA, cA),
			caretest.Delete(completionFeed, mid(60), 1)}, probe: []protocol.Mutation{caretest.Stop(a, tA, cB)}, code: "timer_ended"},

		// Edit.
		{name: "an edit moves the start with its tied side", setup: []protocol.Mutation{babyA, feedStart},
			probe: []protocol.Mutation{timerEditOf(tA, 2, a, at("10:55:00Z")), timerStopOf(tA, 3, a, cA, *at("11:20:00.000000Z"), partA, nil)}, status: "applied"},
		{name: "an edit leaves an untied side and refuses a start after it", setup: []protocol.Mutation{babyA, withPart},
			probe: []protocol.Mutation{timerEditOf(tA, 3, a, at("10:58:00.000000Z")), timerEditOf(tA, 4, a, at("11:12:00.000000Z")),
				timerEditOf(tA, 5, a, at("11:05:00.000000Z"))}, code: "invalid_timer_time"},
		{name: "an edit replaces parts in place", setup: []protocol.Mutation{babyA, threeParts},
			probe: []protocol.Mutation{timerEditOf(tA, 4, a, nil, bottlePart(partC, "70", "mixed"), breastPart(partA, "left", 480)),
				timerStopOf(tA, 5, a, cA, *at("11:30:00.000000Z"), "", nil)}, status: "applied"},
		{name: "edit refusals", setup: []protocol.Mutation{babyA, babyB, threeParts},
			probe: []protocol.Mutation{timerEditOf(tA, 4, a, nil, breastPart(partD, "left", 60)), timerEditOf(tA, 5, a, nil, breastPart(partC, "left", 60)),
				timerEditOf(tA, 6, a, nil, breastPart(partA, "left", 3600)), timerEditOf(tA, 7, a, at("12:02:00.000000Z")),
				timerEditOf(tA, 8, a, ptr("2026-09-14T11:59:00.000000Z")), timerEditOf(tA, 9, b, nil, breastPart(partA, "left", 60)),
				olderVersion(timerEditOf(tA, 10, a, nil, breastPart(partA, "left", 60)), 1), olderVersion(timerEditOf(tA, 11, a, nil, breastPart(partA, "left", 60)), 4)},
			code: "invalid_mutation"},
		{name: "a sleep edit takes a start but no parts", setup: []protocol.Mutation{babyA, sleepStart},
			probe: []protocol.Mutation{timerEditOf(tB, 2, a, nil, breastPart(partA, "left", 60)), timerEditOf(tB, 3, a, at("10:30:00.000000Z"))}, status: "applied"},
		{name: "an edit that changes nothing is applied at the current cursor", setup: []protocol.Mutation{babyA, withPart},
			probe: []protocol.Mutation{timerEditOf(tA, 3, a, at("11:00:00Z"), breastPart(partA, "left", 600))}, status: "applied"},
		{name: "a resent edit is already applied", setup: []protocol.Mutation{babyA, feedStart},
			probe: []protocol.Mutation{timerEditOf(tA, 2, a, at("10:55:00Z")), timerEditOf(tA, 2, a, at("10:55:00Z"))}, status: "already_applied"},
		{name: "an edit's session", setup: []protocol.Mutation{babyA, babyB, inSessionOf(caretest.Feed(b, 1), session), feedStart},
			probe:  []protocol.Mutation{timerSessionEdit(tA, 2, a, &session), timerSessionEdit(tA, 3, a, &otherSession), timerSessionEdit(tA, 4, a, nil)},
			status: "applied"},
		{name: "a stale edit overwrote a newer version", setup: []protocol.Mutation{babyA, feedStart, timerEditOf(tA, 2, a, at("10:55:00Z"))},
			probe: []protocol.Mutation{timerEditOf(tA, 3, a, at("10:50:00Z"))}, status: "applied"},

		// Timed bottles.
		{name: "a standalone timed bottle stops only once finished", setup: []protocol.Mutation{babyA, bottleStart},
			probe: []protocol.Mutation{caretest.Stop(a, tA, cB), timerFinishOf(tA, 2, a, *at("11:04:00.000000Z"), partD, "80"),
				timerStopOf(tA, 3, a, cB, *at("11:04:00.000000Z"), "", nil)}, status: "applied"},
		{name: "a breast completion keeps its part's bounds", setup: []protocol.Mutation{babyA, feedStart},
			probe: []protocol.Mutation{timerStopOf(tA, 2, a, cB, *at("11:04:00.000000Z"), partD, nil)}, status: "applied"},
		{name: "a timed bottle's start edit cannot conflict with its measured duration", setup: []protocol.Mutation{babyA, bottleStart,
			timerFinishOf(tA, 2, a, *at("11:05:00.000000Z"), partD, "60")},
			probe: []protocol.Mutation{timerEditOf(tA, 3, a, at("11:58:00.000000Z")), timerEditOf(tA, 4, a, nil, protocol.FeedComponent{ID: partD, Kind: "bottle",
				Seconds: ptr(uint32(301)), VolumeML: protocol.MustParseDecimal("60"), Contents: ptr("formula")})}, code: "invalid_timer_time"},

		// Quotas: a stop's completion is a record; a timer is not.
		{name: "a stop under a full record quota", limits: func(l *protocol.Limits) { l.MaxCanonicalRecordsPerFamily = 100 },
			setup: append(append([]protocol.Mutation{}, hundredRecords...), feedStart),
			probe: []protocol.Mutation{timerEditOf(tA, 2, a, at("10:55:00Z")), caretest.StartTimer(a, tB, "sleep"), caretest.Stop(a, tA, cA)},
			fatal: "family_record_quota_exceeded"},

		// The minimum page: solids parts whose foods escape on the wire.
		{name: "a timer start too large for the minimum page", setup: []protocol.Mutation{babyA},
			probe: []protocol.Mutation{timerStartWith(tA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed", CompletedComponents: solidsParts()})},
			code:  "mutation_too_large"},
	}
}

// solidsParts are three solids parts of 50 foods of 100 '<' each: about 15
// KB as sent, over 64 KiB once escaped on the wire.
func solidsParts() []protocol.FeedComponent {
	foods := make([]string, 50)
	for i := range foods {
		foods[i] = strings.Repeat("<", 100)
	}
	amount := "some"
	parts := make([]protocol.FeedComponent, 3)
	for i := range parts {
		parts[i] = protocol.FeedComponent{ID: fmt.Sprintf("3b%06d-0000-4000-8000-000000000001", i), Kind: "solids", Foods: foods, Amount: &amount}
	}
	return parts
}

// A stop takes two sequences and a cascade one per row: with fewer left
// below 2^53-1, the largest a cursor carries, the batch stops at that
// mutation with ErrSequenceExhausted and writes nothing of it.
func TestTimerStopAndCascadeClaimEverySequence(t *testing.T) {
	h := testkit.New(t)
	a, b := caretest.BabyA, caretest.BabyB
	caretest.Seed(t, h, caretest.Baby(a), caretest.Baby(b), caretest.StartTimer(a, caretest.TimerA, "feed"), caretest.Feed(b, 1))
	setSequence := func(sequence uint64) {
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		if _, err := tx.Exec(t.Context(), `UPDATE families SET next_sequence = $2 WHERE id = $1`, h.FamilyID, int64(sequence)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	stop, deletion := caretest.Stop(a, caretest.TimerA, caretest.CompletionA), deleteBaby(b)
	setSequence(protocol.MaxSafeInteger - 1) // one sequence left
	rows := caretest.Rows(t, h)
	for _, m := range []protocol.Mutation{stop, deletion} {
		result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{m})
		if !errors.Is(result.Fatal, care.ErrSequenceExhausted) || len(result.Outcomes) != 0 || result.HighWater != protocol.MaxSafeInteger-1 {
			t.Fatalf("%s %s: %v %+v", m.EntityKind, m.Operation, result.Fatal, result.Outcomes)
		}
		wantNoReceipt(t, h, h.Owner, m.MutationID)
	}
	if !reflect.DeepEqual(caretest.Rows(t, h), rows) {
		t.Fatal("a refused mutation wrote rows")
	}
	setSequence(protocol.MaxSafeInteger - 2) // two left: the stop takes both, the delete none
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{stop, deletion})
	if !errors.Is(result.Fatal, care.ErrSequenceExhausted) || len(result.Outcomes) != 1 || result.Outcomes[0].Cursor.Sequence != protocol.MaxSafeInteger {
		t.Fatalf("%v %+v", result.Fatal, result.Outcomes)
	}
	if after := caretest.Rows(t, h); after[caretest.CompletionA].ChangedSeq != protocol.MaxSafeInteger || !after[b].Live() {
		t.Fatalf("completion %+v, baby B live %t", after[caretest.CompletionA], after[b].Live())
	}
}

// Ported from records/timers_test.go at 342973dc: an all-brief feed stopped with
// cleanup leaves the timer tombstone and no feed, and a repeated stop is
// superseded without resurrecting one.
func TestTimerAllBriefFeedStopCreatesNoFeed(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"))
	stop := timerStopOf(caretest.TimerA, 2, a, caretest.CompletionA, "2026-09-21T11:00:11.000000Z", partD, cleanup)
	o := applyOne(t, h, h.Owner, stop)
	if o.Status != "applied" || o.CanonicalEntity.Payload != nil || len(o.RelatedEntities) != 0 {
		t.Fatalf("discard %+v", o)
	}
	if all, _ := kindRows(t, h, "feed"); all != 0 {
		t.Fatalf("discarded feed created %d records", all)
	}
	if timer := caretest.Rows(t, h)[caretest.TimerA]; timer.Live() || timer.CompletedRecordID != nil || timer.Operation != "timer_stop" {
		t.Fatalf("timer %+v", timer)
	}
	retry := timerStopOf(caretest.TimerA, 3, a, "21000000-0000-4000-8000-0000000000a2", "2026-09-21T11:00:12.000000Z", "", cleanup)
	repeated := applyOne(t, h, h.Owner, retry)
	if repeated.Status != "superseded" || len(repeated.RelatedEntities) != 0 || repeated.Cursor.Sequence != o.Cursor.Sequence {
		t.Fatalf("repeat stop %+v", repeated)
	}
	if all, _ := kindRows(t, h, "feed"); all != 0 {
		t.Fatal("repeat stop resurrected the discarded feed")
	}
	wantCountersMatchRows(t, h)
}

// Ported from records/timers_test.go at 342973dc: a start resent under a new mutation
// id (the first response was lost) is superseded by the timer it started.
func TestTimerResentStartIsSuperseded(t *testing.T) {
	left, seconds, formula := "left", uint32(60), "formula"
	for name, parts := range map[string][]protocol.FeedComponent{
		"no finished parts": {},
		"a finished breast and bottle": {
			{ID: "30000000-0000-4000-8000-000000000131", Kind: "breast", Side: &left, Seconds: &seconds},
			{ID: "30000000-0000-4000-8000-000000000132", Kind: "bottle", VolumeML: protocol.MustParseDecimal("60"), Contents: &formula},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := testkit.New(t)
			caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
			start := timerStartWith(caretest.TimerA, 1, protocol.TimerStartPayload{BabyID: caretest.BabyA, TimerKind: "feed", CompletedComponents: parts})
			for index, want := range []string{"applied", "superseded"} {
				sent := start
				sent.MutationID = fmt.Sprintf("05%06d-0000-4000-8000-0000000000a1", 131+index)
				o := applyOne(t, h, h.Owner, sent)
				if o.Status != want || o.CanonicalEntity == nil || o.CanonicalEntity.Meta.ID != caretest.TimerA {
					t.Fatalf("send %d: %+v %s", index+1, o, caretest.Code(o))
				}
			}
			if all, _ := kindRows(t, h, "timer"); all != 1 {
				t.Fatalf("timers %d", all)
			}
		})
	}
}

// Ported from records/timers_test.go at 342973dc: a losing start is superseded by the
// winner, its replay keeps that outcome, and a stop of the losing id never
// reaches the winning timer.
func TestTimerLostStartCannotRetargetCommands(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"))
	loser := caretest.StartTimer(a, caretest.TimerB, "feed")
	o := applyOne(t, h, h.Caregiver, loser)
	if o.Status != "superseded" || o.CanonicalEntity.Meta.ID != caretest.TimerA {
		t.Fatalf("loser %+v", o)
	}
	replay := applyOne(t, h, h.Caregiver, loser)
	if replay.Status != "already_applied" || *replay.OriginalStatus != "superseded" || replay.CanonicalEntity.Meta.ID != caretest.TimerA {
		t.Fatalf("replay %+v", replay)
	}
	if stop := applyOne(t, h, h.Caregiver, caretest.Stop(a, caretest.TimerB, caretest.CompletionA)); caretest.Code(stop) != "entity_not_found" {
		t.Fatalf("losing stop %+v", stop)
	}
	if timers, _ := kindRows(t, h, "timer"); timers != 1 {
		t.Fatalf("timers %d", timers)
	}
	if feeds, _ := kindRows(t, h, "feed"); feeds != 0 {
		t.Fatal("the losing stop retargeted the winning timer")
	}
}

// Ported from records/timers_test.go at 342973dc: concurrent stops of one timer
// serialize on the family lock, so exactly one completes it; the others are
// superseded with its completion. Also at batch scale: 100 stops in 20
// concurrent batches.
func TestTimerConcurrentStopExactlyOnce(t *testing.T) {
	for _, shape := range []struct{ batches, perBatch int }{{2, 1}, {20, 5}} {
		t.Run(fmt.Sprintf("%dx%d", shape.batches, shape.perBatch), func(t *testing.T) {
			h := testkit.New(t)
			a := caretest.BabyA
			caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"))
			before := receiptCount(t, h)
			ready := make(chan struct{})
			outcomes := make(chan protocol.Outcome, shape.batches*shape.perBatch)
			var wg sync.WaitGroup
			for batchIndex := range shape.batches {
				batch := make([]protocol.Mutation, shape.perBatch)
				for i := range batch {
					n := batchIndex*shape.perBatch + i
					batch[i] = caretest.Stop(a, caretest.TimerA, fmt.Sprintf("21000000-0000-4000-8000-%012d", 300+n))
				}
				cmd := caretest.Command(t, h, h.Caregiver, batch)
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-ready
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					tx, err := h.Runtime.Begin(ctx)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = tx.Rollback(ctx) }()
					if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: h.Caregiver.AccountID, FamilyID: h.FamilyID}); err != nil {
						t.Error(err)
						return
					}
					result, err := care.ApplyBatch(ctx, tx, cmd, nil)
					if err == nil && result.Fatal != nil {
						err = result.Fatal
					}
					if err == nil {
						err = tx.Commit(ctx)
					}
					if err != nil {
						t.Error(err)
						return
					}
					for _, o := range result.Outcomes {
						outcomes <- o
					}
				}()
			}
			close(ready)
			wg.Wait()
			close(outcomes)
			statuses := map[string]int{}
			for o := range outcomes {
				statuses[o.Status+"/"+caretest.Code(o)]++
			}
			total := shape.batches * shape.perBatch
			if statuses["applied/"] != 1 || statuses["superseded/timer_ended"] != total-1 {
				t.Fatalf("statuses %v", statuses)
			}
			feeds, _ := kindRows(t, h, "feed")
			timers, live := kindRows(t, h, "timer")
			if feeds != 1 || timers != 1 || live != 0 || receiptCount(t, h) != before+total {
				t.Fatalf("feeds %d timers %d/%d receipts %d", feeds, timers, live, receiptCount(t, h)-before)
			}
			// Baby, start, and exactly one stop's tombstone and completion.
			if seq := familyStateOf(t, h).Sequence; seq != 4 {
				t.Fatalf("family sequence %d", seq)
			}
		})
	}
}

// Ported from records/timers_test.go at 342973dc: a stale stop of an ended timer does
// not end the timer that replaced it.
func TestTimerStaleStopDoesNotEndNewTimer(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"), caretest.Stop(a, caretest.TimerA, caretest.CompletionA))
	if o := applyOne(t, h, h.Owner, caretest.StartTimer(a, caretest.TimerB, "feed")); o.Status != "applied" {
		t.Fatalf("new timer %+v", o)
	}
	stale := applyOne(t, h, h.Caregiver, caretest.Stop(a, caretest.TimerA, "21000000-0000-4000-8000-0000000000a2"))
	if stale.Status != "superseded" || caretest.Code(stale) != "timer_ended" {
		t.Fatalf("stale %+v", stale)
	}
	if feeds, _ := kindRows(t, h, "feed"); feeds != 1 {
		t.Fatalf("feeds %d", feeds)
	}
	if o := applyOne(t, h, h.Owner, caretest.Stop(a, caretest.TimerB, "21000000-0000-4000-8000-0000000000a3")); o.Status != "applied" {
		t.Fatalf("new stop %+v", o)
	}
	if feeds, _ := kindRows(t, h, "feed"); feeds != 2 {
		t.Fatalf("feeds %d", feeds)
	}
}

// Ported from records/timers_test.go at 342973dc: a sleep timer stops into a sleep with
// the stop's end and kind.
func TestTimerSleepStopCreatesSleepCompletion(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a))
	if o := applyOne(t, h, h.Caregiver, caretest.StartTimer(a, caretest.TimerA, "sleep")); o.Status != "applied" {
		t.Fatalf("start %+v", o)
	}
	o := applyOne(t, h, h.Owner, timerStopOf(caretest.TimerA, 2, a, caretest.CompletionA, "2026-09-21T11:50:00.000000Z", "",
		func(p *protocol.TimerStopPayload) { p.SleepKindOverride = ptr("nap") }))
	if o.Status != "applied" || len(o.RelatedEntities) != 1 || o.RelatedEntities[0].EntityKind != "sleep" {
		t.Fatalf("stop %+v", o)
	}
	sleep, ok := o.RelatedEntities[0].Payload.(protocol.SleepPayload)
	if !ok || sleep.EndedAt == nil || *sleep.EndedAt != "2026-09-21T11:50:00.000000Z" || sleep.KindOverride == nil || *sleep.KindOverride != "nap" {
		t.Fatalf("completion %+v", o.RelatedEntities[0])
	}
	if row := caretest.Rows(t, h)[caretest.CompletionA]; row.Kind != "sleep" || row.EndedAt == nil || row.Operation != "timer_stop" {
		t.Fatalf("completion row %+v", row)
	}
}

// Ported from records/timers_test.go at 342973dc: a stop naming the timer itself as its
// completion is refused and changes nothing.
func TestTimerCompletionCannotReuseItsOwnID(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"))
	before := familyStateOf(t, h)
	o := applyOne(t, h, h.Owner, caretest.Stop(a, caretest.TimerA, caretest.TimerA))
	if o.Status != "rejected" || caretest.Code(o) != "completed_record_exists" {
		t.Fatalf("self-id stop %+v", o)
	}
	if after := familyStateOf(t, h); after != withReceipt(t, before, o) {
		t.Fatalf("family %+v, was %+v plus the receipt", after, before)
	}
	if timer := caretest.Rows(t, h)[caretest.TimerA]; !timer.Live() || timer.EntityVersion != 1 {
		t.Fatalf("timer %+v", timer)
	}
}

// Ported from records/timers_test.go at 342973dc: a completion id any family entity
// already holds (a baby, a record or a timer, live or tombstoned) refuses the
// stop without a change; the receipt replays the refusal.
func TestTimerCompletionRejectsEveryFamilyUUIDCollision(t *testing.T) {
	for _, stopping := range []string{"feed", "sleep"} {
		t.Run(stopping, func(t *testing.T) {
			h := testkit.New(t)
			a := caretest.BabyA
			caretest.Seed(t, h, caretest.Baby(a))
			var targets []string
			for i, tombstoned := range []bool{false, true} {
				id := fmt.Sprintf("11000000-0000-4000-8000-%012d", 500+i)
				caretest.Seed(t, h, caretest.Baby(id))
				if tombstoned {
					caretest.Seed(t, h, deleteBaby(id))
				}
				targets = append(targets, id)
			}
			for i, kind := range []string{"feed", "sleep", "diaper"} {
				for j, tombstoned := range []bool{false, true} {
					m := caretest.Record(kind, a, 510+2*i+j)
					caretest.Seed(t, h, m)
					if tombstoned {
						caretest.Seed(t, h, caretest.Delete(m, fmt.Sprintf("0e%06d-0000-4000-8000-0000000000a1", 510+2*i+j), 1))
					}
					targets = append(targets, m.EntityID)
				}
			}
			other := map[string]string{"feed": "sleep", "sleep": "feed"}[stopping]
			ended, running := "50000000-0000-4000-8000-000000000550", "50000000-0000-4000-8000-000000000551"
			caretest.Seed(t, h, caretest.StartTimer(a, ended, stopping), caretest.Stop(a, ended, "21000000-0000-4000-8000-000000000552"),
				caretest.StartTimer(a, running, other))
			targets = append(targets, ended, running)
			stoppingID := caretest.TimerA
			caretest.Seed(t, h, caretest.StartTimer(a, stoppingID, stopping))
			for i, target := range targets {
				before, rows, receipts := familyStateOf(t, h), caretest.Rows(t, h), receiptCount(t, h)
				stop := caretest.Stop(a, stoppingID, target)
				stop.MutationID = fmt.Sprintf("08%06d-0000-4000-8000-0000000000a1", 610+i)
				o := applyOne(t, h, h.Caregiver, stop)
				if o.Status != "rejected" || caretest.Code(o) != "completed_record_exists" || o.Cursor != nil || len(o.RelatedEntities) != 0 || o.CanonicalEntity != nil {
					t.Fatalf("target %s: %+v", target, o)
				}
				if after := familyStateOf(t, h); after != withReceipt(t, before, o) || receiptCount(t, h) != receipts+1 {
					t.Fatalf("target %s changed the family: %+v, was %+v plus the receipt", target, after, before)
				}
				if after := caretest.Rows(t, h); !reflect.DeepEqual(after, rows) {
					t.Fatalf("target %s changed rows", target)
				}
				replay := applyOne(t, h, h.Caregiver, stop)
				if replay.Status != "already_applied" || *replay.OriginalStatus != "rejected" || caretest.Code(replay) != "completed_record_exists" {
					t.Fatalf("replay %+v", replay)
				}
			}
		})
	}
}

// Ported from records/timers_test.go at 342973dc: a database refusal of the completion
// after every Go check admitted the stop rolls the whole batch back.
func TestTimerCompletionConstraintFailureRollsBack(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"))
	completion := "21000000-0000-4000-8000-000000000701"
	if _, err := h.Migrator.Exec(t.Context(), `CREATE FUNCTION reject_timer_completion() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '`+completion+`'::uuid THEN
				RAISE EXCEPTION 'injected completion constraint' USING ERRCODE = '23514';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_timer_completion BEFORE INSERT ON care_entities
		FOR EACH ROW EXECUTE FUNCTION reject_timer_completion()`); err != nil {
		t.Fatal(err)
	}
	before, rows, receipts := familyStateOf(t, h), caretest.Rows(t, h), receiptCount(t, h)
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := care.ApplyBatch(t.Context(), tx, caretest.Command(t, h, h.Owner, []protocol.Mutation{caretest.Stop(a, caretest.TimerA, completion)}), nil); err == nil ||
		!strings.Contains(err.Error(), "injected completion constraint") {
		t.Fatalf("the injected constraint was not reported: %v", err)
	}
	_ = tx.Rollback(t.Context())
	if after := familyStateOf(t, h); after != before || receiptCount(t, h) != receipts || !reflect.DeepEqual(caretest.Rows(t, h), rows) {
		t.Fatal("the refused completion left a partial stop")
	}
}

// Ported from records/timer_edit_test.go at 342973dc: an edit moves the start with the
// side tied to it, the stored timer keeps it, and the stop completes from
// it.
func TestTimerEditMovesStartWithTiedSideAndTheStopUsesIt(t *testing.T) {
	h := testkit.NewAtFixtureClock(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"))
	before := familyStateOf(t, h).Sequence
	edited := timerPayload(t, applyOne(t, h, h.Caregiver, timerEditOf(caretest.TimerA, 2, a, ptr("2026-09-21T10:55:00Z"))))
	want := "2026-09-21T10:55:00.000000Z"
	if edited.StartedAt != want || edited.CurrentSideStartedAt == nil || *edited.CurrentSideStartedAt != want || *edited.CurrentSide != "left" {
		t.Fatalf("edited %+v", edited)
	}
	row, stored := storedTimer(t, h, caretest.TimerA)
	if p := stored.Payload.(protocol.TimerStartPayload); p.StartedAt != want || *p.CurrentSideStartedAt != want || row.StartedAt.Format(time.RFC3339) != "2026-09-21T10:55:00Z" {
		t.Fatalf("stored %+v", p)
	}
	if row.ChangedSeq != before+1 || row.Operation != "update" || familyStateOf(t, h).Sequence != before+1 {
		t.Fatalf("the edit is not one change: row seq %d, family %d", row.ChangedSeq, familyStateOf(t, h).Sequence)
	}
	o := applyOne(t, h, h.Owner, timerStopOf(caretest.TimerA, 3, a, caretest.CompletionA, "2026-09-21T11:20:00.000000Z", partA, nil))
	completion := o.RelatedEntities[0].Payload.(protocol.FeedPayload)
	if completion.StartedAt != want || len(completion.Components) != 1 || *completion.Components[0].Seconds != 25*60 {
		t.Fatalf("completion ignored the edit: %+v", completion)
	}
}

// Ported from records/timer_edit_test.go at 342973dc: an untied side stays put, and a
// start after it (or too late for the finished parts) is refused with the
// stored timer.
func TestTimerEditLeavesAnUntiedSideAndRefusesAStartAfterIt(t *testing.T) {
	h := testkit.NewAtFixtureClock(t)
	a := caretest.BabyA
	right, sideAt := "right", "2026-09-21T11:10:00.000000Z"
	caretest.Seed(t, h, caretest.Baby(a), timerStartWith(caretest.TimerA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed",
		CurrentSide: &right, CurrentSideStartedAt: &sideAt, CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 600)}}))
	earlier := "2026-09-21T10:58:00.000000Z"
	edited := timerPayload(t, applyOne(t, h, h.Caregiver, timerEditOf(caretest.TimerA, 3, a, &earlier)))
	if edited.StartedAt != earlier || *edited.CurrentSideStartedAt != sideAt {
		t.Fatalf("untied side moved: %+v", edited)
	}
	wantRefused(t, applyOne(t, h, h.Caregiver, timerEditOf(caretest.TimerA, 4, a, ptr("2026-09-21T11:12:00.000000Z"))), "rejected", "invalid_timer_time", caretest.TimerA)
	refused := applyOne(t, h, h.Caregiver, timerEditOf(caretest.TimerA, 5, a, ptr("2026-09-21T11:05:00.000000Z")))
	wantRefused(t, refused, "rejected", "invalid_timer_time", caretest.TimerA)
	if refused.CanonicalEntity.Payload.(protocol.TimerStartPayload).StartedAt != earlier {
		t.Fatalf("the refusal did not carry the stored timer: %+v", refused.CanonicalEntity)
	}
}

// Ported from records/timer_edit_test.go at 342973dc: an edit replaces the named parts
// in place and only them; the stop completes from the edited parts.
func TestTimerEditReplacesPartsInPlace(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), timerStartWith(caretest.TimerA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed",
		CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 600), breastPart(partB, "right", 300), bottlePart(partC, "60", "formula")}}))
	sequence := familyStateOf(t, h).Sequence
	edited := timerPayload(t, applyOne(t, h, h.Caregiver, timerEditOf(caretest.TimerA, 4, a, nil, bottlePart(partC, "70", "mixed"), breastPart(partA, "left", 480))))
	parts := edited.CompletedComponents
	if len(parts) != 3 || parts[0].ID != partA || parts[1].ID != partB || parts[2].ID != partC || *parts[0].Seconds != 480 || *parts[1].Seconds != 300 ||
		parts[2].VolumeML.String() != "70" || *parts[2].Contents != "mixed" {
		t.Fatalf("parts %+v", parts)
	}
	if edited.StartedAt != at1100 || edited.CurrentSide != nil {
		t.Fatalf("the edit touched more than the parts: %+v", edited)
	}
	if got := familyStateOf(t, h).Sequence; got != sequence+1 {
		t.Fatalf("sequence %d -> %d", sequence, got)
	}
	row, stored := storedTimer(t, h, caretest.TimerA)
	if p := stored.Payload.(protocol.TimerStartPayload); len(p.CompletedComponents) != 3 || *p.CompletedComponents[0].Seconds != 480 ||
		*p.CompletedComponents[2].Contents != "mixed" || row.Derived.ShortestPartSeconds == nil || *row.Derived.ShortestPartSeconds != 300 {
		t.Fatalf("stored %+v", p)
	}
	o := applyOne(t, h, h.Owner, timerStopOf(caretest.TimerA, 5, a, caretest.CompletionA, "2026-09-21T11:30:00.000000Z", "", nil))
	completion := o.RelatedEntities[0].Payload.(protocol.FeedPayload)
	if len(completion.Components) != 3 || *completion.Components[0].Seconds != 480 || completion.Components[2].VolumeML.String() != "70" {
		t.Fatalf("completion %+v", completion.Components)
	}
}

// Ported from records/timer_edit_test.go at 342973dc: every refusal carries the stored
// timer and leaves it unchanged; an older payload version and a viewer are
// refused before the timer.
func TestTimerEditRefusals(t *testing.T) {
	h := testkit.NewAtFixtureClock(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.Baby(caretest.BabyB), timerStartWith(caretest.TimerA, 1, protocol.TimerStartPayload{BabyID: a,
		TimerKind: "feed", CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 600), breastPart(partB, "right", 300), bottlePart(partC, "60", "formula")}}))
	for name, test := range map[string]struct {
		m    protocol.Mutation
		code string
	}{
		"unknown part":            {timerEditOf(caretest.TimerA, 4, a, nil, breastPart(partD, "left", 60)), "timer_component_not_found"},
		"part changes kind":       {timerEditOf(caretest.TimerA, 5, a, nil, breastPart(partC, "left", 60)), "invalid_mutation"},
		"parts exceed elapsed":    {timerEditOf(caretest.TimerA, 6, a, nil, breastPart(partA, "left", 3600)), "invalid_timer_time"},
		"start in the future":     {timerEditOf(caretest.TimerA, 7, a, ptr("2026-09-21T12:02:00.000000Z")), "invalid_timer_time"},
		"start over a week early": {timerEditOf(caretest.TimerA, 8, a, ptr("2026-09-14T11:59:00.000000Z")), "invalid_timer_time"},
		"another baby":            {timerEditOf(caretest.TimerA, 9, caretest.BabyB, nil, breastPart(partA, "left", 60)), "record_parent_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			wantRefused(t, applyOne(t, h, h.Caregiver, test.m), "rejected", test.code, caretest.TimerA)
		})
	}
	for _, version := range []uint32{1, 4} {
		m := timerEditOf(caretest.TimerA, 10+int(version), a, nil, breastPart(partA, "left", 60))
		m.PayloadVersion = version
		if o := applyOne(t, h, h.Caregiver, m); o.Status != "rejected" || caretest.Code(o) != "invalid_mutation" {
			t.Fatalf("payload version %d: %+v", version, o)
		}
	}
	viewer := h.AddMember(t, "viewer", "viewer")
	result := caretest.ApplyCommitted(t, h, viewer, []protocol.Mutation{timerEditOf(caretest.TimerA, 20, a, nil, breastPart(partA, "left", 60))})
	if !errors.Is(result.Fatal, authority.ErrReadOnlyCaregiver) || len(result.Outcomes) != 0 {
		t.Fatalf("viewer edit %v %+v", result.Fatal, result.Outcomes)
	}
	_, stored := storedTimer(t, h, caretest.TimerA)
	if p := stored.Payload.(protocol.TimerStartPayload); p.StartedAt != at1100 || *p.CompletedComponents[0].Seconds != 600 || *p.CompletedComponents[2].Contents != "formula" ||
		stored.Meta.EntityVersion != 1 {
		t.Fatalf("a refused edit changed the timer: %+v", p)
	}
}

// Ported from records/timer_edit_test.go at 342973dc: a sleep timer takes a new start
// but no parts.
func TestTimerSleepEditTakesAStartButNoParts(t *testing.T) {
	h := testkit.NewAtFixtureClock(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "sleep"))
	wantRefused(t, applyOne(t, h, h.Caregiver, timerEditOf(caretest.TimerA, 2, a, nil, breastPart(partA, "left", 60))), "rejected", "invalid_timer_kind", caretest.TimerA)
	earlier := "2026-09-21T10:30:00.000000Z"
	if edited := timerPayload(t, applyOne(t, h, h.Caregiver, timerEditOf(caretest.TimerA, 3, a, &earlier))); edited.StartedAt != earlier {
		t.Fatalf("sleep edit %+v", edited)
	}
}

// Ported from records/timer_edit_test.go at 342973dc: an edit after the stop is
// superseded with the completion.
func TestTimerEditAfterStopIsSupersededWithTheCompletion(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"),
		timerStopOf(caretest.TimerA, 2, a, caretest.CompletionA, "2026-09-21T11:20:00.000000Z", partA, nil))
	completionSeq := caretest.Rows(t, h)[caretest.CompletionA].ChangedSeq
	o := applyOne(t, h, h.Caregiver, timerEditOf(caretest.TimerA, 3, a, ptr("2026-09-21T10:55:00.000000Z")))
	wantRefused(t, o, "superseded", "timer_ended", caretest.TimerA)
	if len(o.RelatedEntities) != 1 || o.RelatedEntities[0].Meta.ID != caretest.CompletionA || o.Cursor.Sequence != completionSeq ||
		o.RelatedEntities[0].Payload.(protocol.FeedPayload).StartedAt != at1100 {
		t.Fatalf("related %+v", o.RelatedEntities)
	}
}

// Ported from records/timer_edit_test.go at 342973dc: an edit that changes nothing is
// applied at the current cursor and writes no change.
func TestTimerEditThatChangesNothingWritesNoChange(t *testing.T) {
	h := testkit.NewAtFixtureClock(t)
	a := caretest.BabyA
	right, sideAt := "right", "2026-09-21T11:10:00.000000Z"
	caretest.Seed(t, h, caretest.Baby(a), timerStartWith(caretest.TimerA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed",
		CurrentSide: &right, CurrentSideStartedAt: &sideAt, CompletedComponents: []protocol.FeedComponent{breastPart(partA, "left", 600)}}))
	before := familyStateOf(t, h)
	row := caretest.Rows(t, h)[caretest.TimerA]
	o := applyOne(t, h, h.Caregiver, timerEditOf(caretest.TimerA, 3, a, ptr("2026-09-21T11:00:00Z"), breastPart(partA, "left", 600)))
	timerPayload(t, o)
	if o.Cursor.Sequence != before.Sequence || o.CanonicalEntity.Meta.EntityVersion != 1 {
		t.Fatalf("no-op %+v", o)
	}
	if after := familyStateOf(t, h); after != withReceipt(t, before, o) || !reflect.DeepEqual(caretest.Rows(t, h)[caretest.TimerA], row) {
		t.Fatal("a no-op edit wrote a change")
	}
	wantReceipt(t, h, h.Caregiver, o)
}

// Ported from records/timer_edit_test.go at 342973dc: a resent edit replays its receipt
// and writes nothing more.
func TestTimerResentEditIsAlreadyApplied(t *testing.T) {
	h := testkit.NewAtFixtureClock(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"))
	edit := timerEditOf(caretest.TimerA, 2, a, ptr("2026-09-21T10:55:00.000000Z"))
	first := applyOne(t, h, h.Caregiver, edit)
	timerPayload(t, first)
	sequence := familyStateOf(t, h).Sequence
	replay := applyOne(t, h, h.Caregiver, edit)
	if replay.Status != "already_applied" || *replay.OriginalStatus != "applied" || *replay.Cursor != *first.Cursor {
		t.Fatalf("replay %+v", replay)
	}
	if familyStateOf(t, h).Sequence != sequence || caretest.Rows(t, h)[caretest.TimerA].EntityVersion != 2 {
		t.Fatal("the replay wrote another edit")
	}
}

// Ported from records/timed_bottle_test.go at 342973dc: a timed bottle cannot stop
// before it is finished; once finished it stops into a bottle feed of the
// measured seconds.
func TestTimedBottleCannotStopBeforeFinish(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	bottleAt := at1100
	caretest.Seed(t, h, caretest.Baby(a), timerStartWith(caretest.TimerA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed", CurrentBottleStartedAt: &bottleAt}))
	if o := applyOne(t, h, h.Owner, timerStopOf(caretest.TimerA, 2, a, caretest.CompletionA, "2026-09-21T11:03:00.000000Z", "", nil)); caretest.Code(o) != "active_bottle_unfinished" {
		t.Fatalf("stop %+v", o)
	}
	if o := applyOne(t, h, h.Owner, timerFinishOf(caretest.TimerA, 3, a, "2026-09-21T11:04:00.000000Z", partD, "80")); o.Status != "applied" {
		t.Fatalf("finish %+v", o)
	}
	o := applyOne(t, h, h.Owner, timerStopOf(caretest.TimerA, 4, a, caretest.CompletionA, "2026-09-21T11:04:00.000000Z", "", nil))
	if o.Status != "applied" || len(o.RelatedEntities) != 1 {
		t.Fatalf("final stop %+v", o)
	}
	feed := o.RelatedEntities[0].Payload.(protocol.FeedPayload)
	if feed.Kind != "bottle" || len(feed.Components) != 1 || *feed.Components[0].Seconds != 240 {
		t.Fatalf("standalone feed %+v", feed)
	}
}

// Ported from records/timed_bottle_test.go at 342973dc: the running side becomes a
// breast part bounded by its start and the stop.
func TestTimedBreastCompletionRetainsStepBounds(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a), caretest.StartTimer(a, caretest.TimerA, "feed"))
	stopAt := "2026-09-21T11:04:00.000000Z"
	if o := applyOne(t, h, h.Owner, timerStopOf(caretest.TimerA, 2, a, caretest.CompletionA, stopAt, partD, nil)); o.Status != "applied" {
		t.Fatalf("stop %+v", o)
	}
	_, entity := storedTimer(t, h, caretest.CompletionA)
	feed := entity.Payload.(protocol.FeedPayload)
	if len(feed.Components) != 1 || *feed.Components[0].StartedAt != at1100 || *feed.Components[0].EndedAt != stopAt {
		t.Fatalf("completion %+v", feed)
	}
}

// Ported from records/timed_bottle_test.go at 342973dc: a start edit or a part edit
// cannot contradict a finished bottle's measured duration.
func TestTimedBottleStartEditCannotConflictWithMeasuredDuration(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	bottleAt := at1100
	caretest.Seed(t, h, caretest.Baby(a), timerStartWith(caretest.TimerA, 1, protocol.TimerStartPayload{BabyID: a, TimerKind: "feed", CurrentBottleStartedAt: &bottleAt}),
		timerFinishOf(caretest.TimerA, 2, a, "2026-09-21T11:05:00.000000Z", partD, "60"))
	if o := applyOne(t, h, h.Owner, timerEditOf(caretest.TimerA, 3, a, ptr("2026-09-21T11:58:00.000000Z"))); caretest.Code(o) != "invalid_timer_time" {
		t.Fatalf("start edit %+v", o)
	}
	longer := protocol.FeedComponent{ID: partD, Kind: "bottle", Seconds: ptr(uint32(301)), VolumeML: protocol.MustParseDecimal("60"), Contents: ptr("formula")}
	if o := applyOne(t, h, h.Owner, timerEditOf(caretest.TimerA, 4, a, nil, longer)); caretest.Code(o) != "invalid_timer_time" {
		t.Fatalf("duration edit %+v", o)
	}
}

// Ported from records/timer_stop_cleanup_test.go at 342973dc (CompletedTimerPayload
// moved into care): cleanup drops a brief switch and joins the untimed
// parts of one side it leaves adjacent; the running side stays its own part.
func TestTimerStopCleanupRemovesBriefSwitchAndCombinesMatchingBreasts(t *testing.T) {
	left, right := "left", "right"
	long, brief, short := uint32(300), uint32(3), uint32(120)
	current := "2026-09-28T12:07:03.000000Z"
	timer := protocol.TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed", StartedAt: "2026-09-28T12:00:00.000000Z",
		TimeZoneID: "UTC", CurrentSide: &left, CurrentSideStartedAt: &current,
		CompletedComponents: []protocol.FeedComponent{
			{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &long},
			{ID: "30000000-0000-4000-8000-000000000002", Kind: "breast", Side: &right, Seconds: &brief},
			{ID: "30000000-0000-4000-8000-000000000003", Kind: "breast", Side: &left, Seconds: &short},
		}}
	stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
		StoppedAt: "2026-09-28T12:09:03.000000Z", ComponentID: "30000000-0000-4000-8000-000000000004", CleanupShortBreastSegments: true}
	payload, kind, err := care.CompletedTimerPayload(timer, stop, time.Date(2026, 9, 28, 12, 9, 3, 0, time.UTC), protocol.DefaultLimits())
	if err != nil || kind != "feed" {
		t.Fatalf("kind %q err %v", kind, err)
	}
	feed := payload.(protocol.FeedPayload)
	if len(feed.Components) != 2 || feed.Components[0].ID != timer.CompletedComponents[0].ID || *feed.Components[0].Seconds != 420 || feed.Components[0].StartedAt != nil {
		t.Fatalf("cleaned %+v", feed)
	}
	running := feed.Components[1]
	if running.ID != stop.ComponentID || *running.Seconds != 120 || *running.StartedAt != current || *running.EndedAt != stop.StoppedAt {
		t.Fatalf("running side part %+v", running)
	}
}

func TestTimerStopCleanupDiscardsAllBriefFeed(t *testing.T) {
	left, right := "left", "right"
	brief := uint32(3)
	current := "2026-09-28T12:00:03.000000Z"
	timer := protocol.TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed", StartedAt: "2026-09-28T12:00:00.000000Z",
		TimeZoneID: "UTC", CurrentSide: &right, CurrentSideStartedAt: &current,
		CompletedComponents: []protocol.FeedComponent{{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &brief}}}
	stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
		StoppedAt: "2026-09-28T12:00:11.000000Z", ComponentID: "30000000-0000-4000-8000-000000000002", CleanupShortBreastSegments: true}
	payload, _, err := care.CompletedTimerPayload(timer, stop, time.Date(2026, 9, 28, 12, 0, 11, 0, time.UTC), protocol.DefaultLimits())
	if err != nil || payload != nil {
		t.Fatalf("an all-brief feed has no completion: %+v %v", payload, err)
	}
}

func TestTimerStopCleanupCannotChangeSleepStop(t *testing.T) {
	timer := protocol.TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "sleep", StartedAt: "2026-09-28T12:00:00.000000Z", TimeZoneID: "UTC"}
	stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
		StoppedAt: "2026-09-28T12:01:00.000000Z", CleanupShortBreastSegments: true}
	if _, _, err := care.CompletedTimerPayload(timer, stop, time.Date(2026, 9, 28, 12, 1, 0, 0, time.UTC), protocol.DefaultLimits()); err == nil {
		t.Fatal("a sleep stop accepted breast cleanup")
	}
}

func TestTimerStopCleanupBriefBreastBeforeBottleCompletesAsBottle(t *testing.T) {
	left, formula := "left", "formula"
	brief := uint32(3)
	timer := protocol.TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed", StartedAt: "2026-09-28T12:00:00.000000Z",
		TimeZoneID: "UTC", CompletedComponents: []protocol.FeedComponent{
			{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &brief},
			{ID: "30000000-0000-4000-8000-000000000002", Kind: "bottle", VolumeML: protocol.MustParseDecimal("60"), Contents: &formula},
		}}
	stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
		StoppedAt: "2026-09-28T12:00:04.000000Z", CleanupShortBreastSegments: true}
	payload, _, err := care.CompletedTimerPayload(timer, stop, time.Date(2026, 9, 28, 12, 0, 4, 0, time.UTC), protocol.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if feed := payload.(protocol.FeedPayload); feed.Kind != "bottle" || len(feed.Components) != 1 || feed.Components[0].Kind != "bottle" {
		t.Fatalf("completion %+v", feed)
	}
}

// The side still running at the stop becomes one more breast part, so a
// timer already holding MaxSideIntervals breast parts cannot stop into a
// feed, while one part fewer stops into exactly MaxSideIntervals parts.
func TestTimerStopRefusesABreastPartBeyondTheSideIntervalLimit(t *testing.T) {
	limits := protocol.DefaultLimits()
	left, seconds := "left", uint32(1)
	current := "2026-09-28T12:30:00.000000Z"
	stopped := time.Date(2026, 9, 28, 12, 31, 0, 0, time.UTC)
	for _, test := range []struct {
		held    int
		wantErr error
	}{{limits.MaxSideIntervals - 1, nil}, {limits.MaxSideIntervals, protocol.ErrInvalidTimestamp}} {
		parts := make([]protocol.FeedComponent, test.held)
		for i := range parts {
			parts[i] = protocol.FeedComponent{ID: fmt.Sprintf("30000000-0000-4000-8000-%012d", i+1), Kind: "breast", Side: &left, Seconds: &seconds}
		}
		timer := protocol.TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed", StartedAt: "2026-09-28T12:00:00.000000Z",
			TimeZoneID: "UTC", CurrentSide: &left, CurrentSideStartedAt: &current, CompletedComponents: parts}
		stop := protocol.TimerStopPayload{BabyID: timer.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000001",
			StoppedAt: "2026-09-28T12:31:00.000000Z", ComponentID: "31000000-0000-4000-8000-000000000001"}
		payload, kind, err := care.CompletedTimerPayload(timer, stop, stopped, limits)
		if !errors.Is(err, test.wantErr) {
			t.Fatalf("held %d: %v, want %v", test.held, err, test.wantErr)
		}
		if test.wantErr != nil {
			continue
		}
		if feed, ok := payload.(protocol.FeedPayload); kind != "feed" || !ok || len(feed.Components) != limits.MaxSideIntervals ||
			feed.Components[len(feed.Components)-1].ID != stop.ComponentID {
			t.Fatalf("held %d: %q %+v", test.held, kind, payload)
		}
	}
}

// Ported from records/read_timer_test.go at 342973dc: there is no row lock to take, so
// a timer row reads in a read-only transaction exactly as the write path
// stored it, and a missing timer is ErrNotFound.
func TestReadTimerRowInAReadOnlyTransaction(t *testing.T) {
	h := testkit.New(t)
	a := caretest.BabyA
	caretest.Seed(t, h, caretest.Baby(a))
	o := applyOne(t, h, h.Owner, caretest.StartTimer(a, caretest.TimerA, "feed"))
	written := caretest.Rows(t, h)[caretest.TimerA]
	tx, err := h.Runtime.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	read, err := care.Get(t.Context(), tx, h.FamilyID, caretest.TimerA)
	if err != nil {
		t.Fatalf("Get in a read-only transaction: %v", err)
	}
	if !reflect.DeepEqual(read, written) || string(read.Wire) != string(mustJSON(t, o.CanonicalEntity)) {
		t.Fatalf("read %+v\nwritten %+v", read, written)
	}
	if _, err := care.Get(t.Context(), tx, h.FamilyID, caretest.TimerB); !errors.Is(err, care.ErrNotFound) {
		t.Fatalf("missing timer: %v", err)
	}
}
