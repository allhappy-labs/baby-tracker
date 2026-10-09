package records_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

const (
	timerA  = "50000000-0000-4000-8000-000000000101"
	timerB  = "50000000-0000-4000-8000-000000000102"
	recordA = "20000000-0000-4000-8000-000000000101"
	recordB = "20000000-0000-4000-8000-000000000102"
)

func mutationID(value int) string {
	return fmt.Sprintf("01000000-0000-4000-8000-%012d", value)
}

func seedBaby(t *testing.T, h *testkit.Harness) string {
	t.Helper()
	m := testkit.MustMutation(t, "baby.json")
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("seed baby outcome=%+v err=%v", outcome, err)
	}
	return m.EntityID
}

func startFeed(t *testing.T, h *testkit.Harness, timerID string, mutation int) protocol.Outcome {
	t.Helper()
	m := testkit.MustMutation(t, "timer-feed.json")
	m.EntityID, m.MutationID = timerID, mutationID(mutation)
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil {
		t.Fatalf("start feed: %v", err)
	}
	return outcome
}

func stopFeedAs(t *testing.T, h *testkit.Harness, principalOwner bool, timerID, completedID string, mutation int) protocol.Outcome {
	t.Helper()
	m := testkit.MustMutation(t, "timer-feed.json")
	p := m.Payload.(protocol.TimerStartPayload)
	m.MutationID, m.EntityID, m.Operation = mutationID(mutation), timerID, "timer_stop"
	// The timer's running breast side (current_side in timer-feed.json) only
	// finalizes into a component at component_version>=2 if the stop names a
	// component id; the legacy v1 path (removed from the fixture corpus) used
	// to need none.
	m.Payload = protocol.TimerStopPayload{BabyID: p.BabyID, CompletedRecordID: completedID, StoppedAt: "2026-09-20T15:10:00.000000Z", ComponentID: "4" + completedID[1:]}
	principal := h.Caregiver
	if principalOwner {
		principal = h.Owner
	}
	outcome, err := h.Service.Apply(t.Context(), principal, h.FamilyID, m)
	if err != nil {
		t.Fatalf("stop feed: %v", err)
	}
	return outcome
}

func TestAllBriefFeedStopTombstonesTimerWithoutCreatingFeed(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	left := "left"
	startAt := "2026-09-28T12:00:00.000000Z"
	base := protocol.Mutation{EntityID: timerA, EntityKind: "timer", PayloadVersion: 5}
	start := base
	start.MutationID, start.Operation, start.ClientMutationTime = mutationID(901), "timer_start", startAt
	start.Payload = protocol.TimerStartPayload{BabyID: babyID, TimerKind: "feed", StartedAt: startAt,
		TimeZoneID: "UTC", CompletedComponents: []protocol.FeedComponent{},
		CurrentSide: &left, CurrentSideStartedAt: &startAt}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start); err != nil || outcome.Status != "applied" {
		t.Fatalf("start outcome=%+v err=%v", outcome, err)
	}
	stop := base
	stop.MutationID, stop.Operation, stop.ClientMutationTime = mutationID(903), "timer_stop", "2026-09-28T12:00:11.000000Z"
	stop.Payload = protocol.TimerStopPayload{BabyID: babyID, CompletedRecordID: recordA, StoppedAt: stop.ClientMutationTime,
		ComponentID: "30000000-0000-4000-8000-000000000902", CleanupShortBreastSegments: true}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop)
	if err != nil || outcome.Status != "applied" || outcome.CanonicalEntity == nil ||
		outcome.CanonicalEntity.Payload != nil || len(outcome.RelatedEntities) != 0 {
		t.Fatalf("discard outcome=%+v err=%v", outcome, err)
	}
	if count := h.Count(t, "feed_entries", h.FamilyID); count != 0 {
		t.Fatalf("discarded feed created %d records", count)
	}
	retry := stop
	retry.MutationID = mutationID(904)
	retry.Payload = protocol.TimerStopPayload{BabyID: babyID, CompletedRecordID: recordB,
		StoppedAt: "2026-09-28T12:00:12.000000Z", CleanupShortBreastSegments: true}
	repeated, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, retry)
	if err != nil || repeated.Status != "superseded" || len(repeated.RelatedEntities) != 0 || h.Count(t, "feed_entries", h.FamilyID) != 0 {
		t.Fatalf("repeat stop resurrected discarded feed: outcome=%+v err=%v", repeated, err)
	}
}

// A start resent under a new mutation id (the phone lost the first
// response) finds the timer it already started and is superseded by it, not
// refused as a collision. The resend arrives through the wire decoder, like
// the first.
func TestResentStartOfTheSameTimerIsSuperseded(t *testing.T) {
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
			seedBaby(t, h)
			start := testkit.MustMutation(t, "timer-feed.json")
			payload := start.Payload.(protocol.TimerStartPayload)
			payload.CompletedComponents = parts
			start.Payload, start.EntityID = payload, timerA
			wire, err := json.Marshal(start)
			if err != nil {
				t.Fatal(err)
			}
			for index, want := range []string{"applied", "superseded"} {
				sent, err := protocol.DecodeMutation(wire)
				if err != nil {
					t.Fatal(err)
				}
				sent.MutationID = mutationID(131 + index)
				outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, sent)
				if err != nil || outcome.Status != want || outcome.CanonicalEntity == nil || outcome.CanonicalEntity.Meta.ID != timerA {
					t.Fatalf("send %d: outcome=%+v code=%v err=%v", index+1, outcome, outcome.Code, err)
				}
			}
			if got := h.Count(t, "active_timers", h.FamilyID); got != 1 {
				t.Fatalf("timers=%d", got)
			}
		})
	}
}

func TestLostStartCollisionCannotRetargetCommands(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	if got := startFeed(t, h, timerA, 111); got.Status != "applied" {
		t.Fatalf("start A=%+v", got)
	}
	loserMutation := testkit.MustMutation(t, "timer-feed.json")
	loserMutation.MutationID, loserMutation.EntityID = mutationID(112), timerB
	loser, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, loserMutation)
	if err != nil || loser.Status != "superseded" || loser.CanonicalEntity == nil || loser.CanonicalEntity.Meta.ID != timerA {
		t.Fatalf("loser=%+v err=%v", loser, err)
	}
	replay, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, loserMutation)
	if err != nil || replay.Status != "already_applied" || replay.OriginalStatus == nil || *replay.OriginalStatus != "superseded" || replay.CanonicalEntity.Meta.ID != timerA {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	stop := stopFeedAs(t, h, false, timerB, recordB, 115)
	if stop.Status != "rejected" || stop.Code == nil || *stop.Code != "entity_not_found" {
		t.Fatalf("losing stop=%+v", stop)
	}
	if h.Count(t, "active_timers", h.FamilyID) != 1 || h.Count(t, "feed_entries", h.FamilyID) != 0 {
		t.Fatal("losing commands retargeted canonical timer")
	}
}

func TestConcurrentStopExactlyOnce(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	startFeed(t, h, timerA, 121)
	type result struct {
		outcome protocol.Outcome
	}
	ready := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for index, record := range []string{recordA, recordB} {
		wg.Add(1)
		go func(index int, record string) {
			defer wg.Done()
			<-ready
			m := testkit.MustMutation(t, "timer-feed.json")
			p := m.Payload.(protocol.TimerStartPayload)
			m.MutationID, m.EntityID, m.Operation = mutationID(122+index), timerA, "timer_stop"
			m.Payload = protocol.TimerStopPayload{BabyID: p.BabyID, CompletedRecordID: record, StoppedAt: "2026-09-20T15:10:00.000000Z", ComponentID: "4" + record[1:]}
			o, err := h.Service.Apply(context.Background(), h.Caregiver, h.FamilyID, m)
			if err != nil {
				t.Errorf("stop %d: %v", index, err)
				return
			}
			results <- result{o}
		}(index, record)
	}
	close(ready)
	wg.Wait()
	close(results)
	statuses := map[string]int{}
	for result := range results {
		statuses[result.outcome.Status]++
	}
	if statuses["applied"] != 1 || statuses["superseded"] != 1 || h.Count(t, "feed_entries", h.FamilyID) != 1 ||
		h.Count(t, "active_timers", h.FamilyID) != 1 || h.Count(t, "mutation_receipts", h.FamilyID) != 4 {
		t.Fatalf("statuses=%v feeds=%d timers=%d receipts=%d", statuses, h.Count(t, "feed_entries", h.FamilyID),
			h.Count(t, "active_timers", h.FamilyID), h.Count(t, "mutation_receipts", h.FamilyID))
	}
	// Baby start + timer start + exactly one timer tombstone and completion.
	if h.Count(t, "mutation_journal", h.FamilyID) != 4 {
		t.Fatalf("journal=%d", h.Count(t, "mutation_journal", h.FamilyID))
	}
}

func TestStaleStopDoesNotEndNewTimer(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	startFeed(t, h, timerA, 281)
	stopFeedAs(t, h, true, timerA, recordA, 282)
	if outcome := startFeed(t, h, timerB, 283); outcome.Status != "applied" {
		t.Fatalf("new timer=%+v", outcome)
	}
	stale := stopFeedAs(t, h, false, timerA, recordB, 284)
	if stale.Status != "superseded" || stale.Code == nil || *stale.Code != "timer_ended" || h.Count(t, "feed_entries", h.FamilyID) != 1 {
		t.Fatalf("stale=%+v", stale)
	}
	newStop := stopFeedAs(t, h, true, timerB, recordB, 285)
	if newStop.Status != "applied" || h.Count(t, "feed_entries", h.FamilyID) != 2 {
		t.Fatalf("new stop=%+v", newStop)
	}
}

func TestConcurrentStopExactlyOnceAtBatchScale(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	startFeed(t, h, timerA, 301)
	mutations := make([]protocol.Mutation, 100)
	for index := range mutations {
		m := testkit.MustMutation(t, "timer-feed.json")
		p := m.Payload.(protocol.TimerStartPayload)
		m.MutationID, m.EntityID, m.Operation = mutationID(302+index), timerA, "timer_stop"
		m.Payload = protocol.TimerStopPayload{BabyID: p.BabyID, CompletedRecordID: fmt.Sprintf("20000000-0000-4000-8000-%012d", 302+index),
			StoppedAt: "2026-09-20T15:10:00.000000Z", ComponentID: fmt.Sprintf("40000000-0000-4000-8000-%012d", 302+index)}
		mutations[index] = m
	}
	ready := make(chan struct{})
	results := make(chan protocol.Outcome, len(mutations))
	var wg sync.WaitGroup
	for index := range mutations {
		wg.Add(1)
		go func(m protocol.Mutation) {
			defer wg.Done()
			<-ready
			outcome, err := h.Service.Apply(context.Background(), h.Caregiver, h.FamilyID, m)
			if err != nil {
				t.Errorf("stop: %v", err)
				return
			}
			results <- outcome
		}(mutations[index])
	}
	close(ready)
	wg.Wait()
	close(results)
	statuses := map[string]int{}
	for outcome := range results {
		statuses[outcome.Status]++
	}
	if statuses["applied"] != 1 || statuses["superseded"] != 99 || h.Count(t, "feed_entries", h.FamilyID) != 1 ||
		h.Count(t, "active_timers", h.FamilyID) != 1 || h.Count(t, "mutation_journal", h.FamilyID) != 4 ||
		h.Count(t, "mutation_receipts", h.FamilyID) != 102 {
		t.Fatalf("statuses=%v feeds=%d timers=%d journal=%d receipts=%d", statuses, h.Count(t, "feed_entries", h.FamilyID),
			h.Count(t, "active_timers", h.FamilyID), h.Count(t, "mutation_journal", h.FamilyID), h.Count(t, "mutation_receipts", h.FamilyID))
	}
}

func TestSleepTimerStopCreatesSleepCompletion(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	start := testkit.MustMutation(t, "timer-sleep.json")
	start.MutationID, start.EntityID = mutationID(411), timerA
	if outcome, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, start); err != nil || outcome.Status != "applied" {
		t.Fatalf("start=%+v err=%v", outcome, err)
	}
	p := start.Payload.(protocol.TimerStartPayload)
	kind := "nap"
	stop := start
	stop.MutationID, stop.Operation = mutationID(412), "timer_stop"
	stop.Payload = protocol.TimerStopPayload{BabyID: p.BabyID, CompletedRecordID: recordA, StoppedAt: "2026-09-20T16:00:00.000000Z", SleepKindOverride: &kind}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop)
	if err != nil || outcome.Status != "applied" || len(outcome.RelatedEntities) != 1 || outcome.RelatedEntities[0].EntityKind != "sleep" {
		t.Fatalf("stop=%+v err=%v", outcome, err)
	}
	completion, ok := outcome.RelatedEntities[0].Payload.(protocol.SleepPayload)
	if !ok || completion.EndedAt == nil || *completion.EndedAt != "2026-09-20T16:00:00.000000Z" || completion.KindOverride == nil || *completion.KindOverride != kind {
		t.Fatalf("completion=%+v", outcome.RelatedEntities[0])
	}
}

func TestTimerCompletionCannotReuseItsOwnID(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	startFeed(t, h, timerA, 131)
	before := h.FamilySequence(t)
	result := stopFeedAs(t, h, true, timerA, timerA, 132)
	if result.Status != "rejected" || result.Code == nil || *result.Code != "completed_record_exists" {
		t.Fatalf("self-ID stop=%+v", result)
	}
	if h.FamilySequence(t) != before || h.Count(t, "feed_entries", h.FamilyID) != 0 {
		t.Fatal("completion collision partially changed canonical state")
	}
	if !h.TimerIsLive(t, timerA) || h.TimerVersion(t, timerA) != 1 {
		t.Fatal("completion collision changed active timer")
	}
}

func TestTimerCompletionRejectsEveryFamilyUUIDCollision(t *testing.T) {
	for _, stoppingKind := range []string{"feed", "sleep"} {
		t.Run(stoppingKind, func(t *testing.T) {
			h := testkit.New(t)
			seedBaby(t, h)
			targets := seedCollisionTargets(t, h, stoppingKind)
			fixture, stoppingID := "timer-feed.json", timerA
			if stoppingKind == "sleep" {
				fixture, stoppingID = "timer-sleep.json", timerB
			}
			start := testkit.MustMutation(t, fixture)
			start.MutationID, start.EntityID = mutationID(600), stoppingID
			if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start); err != nil || outcome.Status != "applied" {
				t.Fatalf("start=%+v err=%v", outcome, err)
			}
			for index, target := range targets {
				t.Run(target.name, func(t *testing.T) {
					beforeSequence := h.FamilySequence(t)
					beforeJournal := h.Count(t, "mutation_journal", h.FamilyID)
					beforeReceipts := h.Count(t, "mutation_receipts", h.FamilyID)
					beforeVersion := h.TimerVersion(t, stoppingID)
					beforeRows := map[string]int{}
					for _, table := range []string{"babies", "feed_entries", "sleep_entries", "diaper_entries", "active_timers"} {
						beforeRows[table] = h.Count(t, table, h.FamilyID)
					}
					stop := timerStopForFixture(start, mutationID(610+index), target.id)
					outcome, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, stop)
					if err != nil || outcome.Status != "rejected" || outcome.Code == nil || *outcome.Code != "completed_record_exists" || outcome.Cursor != nil || len(outcome.RelatedEntities) != 0 {
						t.Fatalf("outcome=%+v err=%v", outcome, err)
					}
					if h.FamilySequence(t) != beforeSequence || h.Count(t, "mutation_journal", h.FamilyID) != beforeJournal || h.Count(t, "mutation_receipts", h.FamilyID) != beforeReceipts+1 {
						t.Fatal("collision changed canonical or journal state")
					}
					if !h.TimerIsLive(t, stoppingID) || h.TimerVersion(t, stoppingID) != beforeVersion {
						t.Fatal("collision changed timer state")
					}
					for table, before := range beforeRows {
						if after := h.Count(t, table, h.FamilyID); after != before {
							t.Fatalf("collision changed %s rows: before=%d after=%d", table, before, after)
						}
					}
					replay, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, stop)
					if err != nil || replay.Status != "already_applied" || replay.OriginalStatus == nil || *replay.OriginalStatus != "rejected" || replay.Code == nil || *replay.Code != "completed_record_exists" {
						t.Fatalf("replay=%+v err=%v", replay, err)
					}
				})
			}
		})
	}
}

func TestTimerCompletionConstraintFailureAfterAdmissionRollsBack(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	startFeed(t, h, timerA, 701)
	if _, err := h.Migrator.Exec(t.Context(), `CREATE FUNCTION reject_timer_completion() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '20000000-0000-4000-8000-000000000701'::uuid THEN
				RAISE EXCEPTION 'injected completion constraint' USING ERRCODE = '23514';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER reject_timer_completion BEFORE INSERT ON feed_entries
		FOR EACH ROW EXECUTE FUNCTION reject_timer_completion()`); err != nil {
		t.Fatal(err)
	}
	beforeSequence := h.FamilySequence(t)
	beforeJournal, beforeReceipts := h.Count(t, "mutation_journal", h.FamilyID), h.Count(t, "mutation_receipts", h.FamilyID)
	stop := testkit.MustMutation(t, "timer-feed.json")
	p := stop.Payload.(protocol.TimerStartPayload)
	stop.MutationID, stop.EntityID, stop.Operation = mutationID(702), timerA, "timer_stop"
	stop.Payload = protocol.TimerStopPayload{BabyID: p.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000701", StoppedAt: "2026-09-20T15:10:00.000000Z", ComponentID: "40000000-0000-4000-8000-000000000701"}
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop); err == nil {
		t.Fatal("injected completion constraint was accepted")
	}
	if !h.TimerIsLive(t, timerA) || h.TimerVersion(t, timerA) != 1 || h.Count(t, "feed_entries", h.FamilyID) != 0 ||
		h.FamilySequence(t) != beforeSequence || h.Count(t, "mutation_journal", h.FamilyID) != beforeJournal || h.Count(t, "mutation_receipts", h.FamilyID) != beforeReceipts {
		t.Fatal("constraint failure left partial timer state or receipt")
	}
}

type collisionTarget struct{ name, id string }

func seedCollisionTargets(t *testing.T, h *testkit.Harness, stoppingKind string) []collisionTarget {
	t.Helper()
	targets := []collisionTarget{}
	baseBaby := testkit.MustMutation(t, "baby.json")
	for index, tombstoned := range []bool{false, true} {
		m := baseBaby
		m.MutationID = mutationID(500 + index*2)
		m.EntityID = fmt.Sprintf("10000000-0000-4000-8000-%012d", 500+index)
		p := m.Payload.(protocol.BabyPayload)
		p.Name = fmt.Sprintf("Collision %d", index)
		m.Payload = p
		if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Status != "applied" {
			t.Fatalf("baby target=%+v err=%v", outcome, err)
		}
		if tombstoned {
			del := m
			del.MutationID, del.Operation, del.Payload = mutationID(501+index*2), "delete", protocol.DeletePayload{}
			if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, del); err != nil || outcome.Status != "applied" {
				t.Fatalf("delete baby target=%+v err=%v", outcome, err)
			}
		}
		targets = append(targets, collisionTarget{fmt.Sprintf("baby/tombstone=%t", tombstoned), m.EntityID})
	}
	fixtures := []struct{ kind, name string }{{"feed", "feed-bottle.json"}, {"sleep", "sleep.json"}, {"diaper", "diaper.json"}}
	sequence := 510
	for _, fixture := range fixtures {
		for _, tombstoned := range []bool{false, true} {
			m := testkit.MustMutation(t, fixture.name)
			m.MutationID = mutationID(sequence)
			m.EntityID = fmt.Sprintf("%d0000000-0000-4000-8000-%012d", map[string]int{"feed": 2, "sleep": 3, "diaper": 4}[fixture.kind], sequence)
			if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Status != "applied" {
				t.Fatalf("%s target=%+v err=%v", fixture.kind, outcome, err)
			}
			if tombstoned {
				babyID := mutationBabyIDForTest(m)
				del := m
				del.MutationID, del.Operation, del.Payload = mutationID(sequence+1), "delete", protocol.DeletePayload{BabyID: &babyID}
				if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, del); err != nil || outcome.Status != "applied" {
					t.Fatalf("delete %s target=%+v err=%v", fixture.kind, outcome, err)
				}
			}
			targets = append(targets, collisionTarget{fmt.Sprintf("%s/tombstone=%t", fixture.kind, tombstoned), m.EntityID})
			sequence += 2
		}
	}
	tombFixture, tombID := "timer-feed.json", "50000000-0000-4000-8000-000000000550"
	liveFixture, liveID := "timer-sleep.json", "50000000-0000-4000-8000-000000000551"
	if stoppingKind == "sleep" {
		tombFixture, liveFixture = liveFixture, tombFixture
		tombID, liveID = liveID, tombID
	}
	tomb := testkit.MustMutation(t, tombFixture)
	tomb.MutationID, tomb.EntityID = mutationID(550), tombID
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, tomb); err != nil || outcome.Status != "applied" {
		t.Fatalf("tomb timer start=%+v err=%v", outcome, err)
	}
	end := timerStopForFixture(tomb, mutationID(552), "20000000-0000-4000-8000-000000000552")
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, end); err != nil || outcome.Status != "applied" {
		t.Fatalf("tomb timer stop=%+v err=%v", outcome, err)
	}
	targets = append(targets, collisionTarget{"timer/tombstone=true", tombID})
	live := testkit.MustMutation(t, liveFixture)
	live.MutationID, live.EntityID = mutationID(553), liveID
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, live); err != nil || outcome.Status != "applied" {
		t.Fatalf("live timer start=%+v err=%v", outcome, err)
	}
	targets = append(targets, collisionTarget{"timer/tombstone=false", liveID})
	return targets
}

func timerStopForFixture(start protocol.Mutation, mutationID, completionID string) protocol.Mutation {
	p := start.Payload.(protocol.TimerStartPayload)
	stopped := "2026-09-20T15:10:00.000000Z"
	if p.TimerKind == "sleep" {
		stopped = "2026-09-20T16:10:00.000000Z"
	}
	start.MutationID, start.Operation = mutationID, "timer_stop"
	// Only a feed timer's running breast side needs a component id to
	// finalize at component_version>=2; harmless to always pass one since the
	// sleep completion path never looks at it.
	start.Payload = protocol.TimerStopPayload{BabyID: p.BabyID, CompletedRecordID: completionID, StoppedAt: stopped, ComponentID: "4" + completionID[1:]}
	return start
}

func mutationBabyIDForTest(m protocol.Mutation) string {
	switch p := m.Payload.(type) {
	case protocol.FeedPayload:
		return p.BabyID
	case protocol.SleepPayload:
		return p.BabyID
	case protocol.DiaperPayload:
		return p.BabyID
	}
	return ""
}
