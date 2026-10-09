package records_test

import (
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// seedSecondBaby mirrors seedBaby but creates a second, distinct baby in the
// same family: the ownership tests need two babies to prove a session ID
// bound to one cannot be reused by the other.
func seedSecondBaby(t *testing.T, h *testkit.Harness) string {
	t.Helper()
	m := testkit.MustMutation(t, "baby.json")
	m.MutationID, m.EntityID = mutationID(2500), "10000000-0000-4000-8000-000000000002"
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("seed second baby outcome=%+v err=%v", outcome, err)
	}
	return m.EntityID
}

// sessionTimerStart builds a v5 timer-start mutation that carries session, on
// top of the repo's timer-start fixtures.
func sessionTimerStart(t *testing.T, h *testkit.Harness, n int, timerID, kind string, session *string) protocol.Mutation {
	t.Helper()
	fixture := "timer-feed.json"
	if kind == "sleep" {
		fixture = "timer-sleep.json"
	}
	start := testkit.MustMutation(t, fixture)
	start.MutationID, start.EntityID, start.PayloadVersion = mutationID(n), timerID, 5
	p := start.Payload.(protocol.TimerStartPayload)
	p.SessionID = session
	start.Payload = p
	return start
}

// sessionTimerStop builds a v5 timer_stop mutation. There is no dedicated
// "stop" fixture in api/fixtures/v1 (confirmed via `ls`); the repo's own
// pattern (see stopFeedAs in timers_test.go) builds the stop payload by hand
// instead of decoding one from JSON, so this does the same at version 5.
func sessionTimerStop(n int, timerID string, payload protocol.TimerStopPayload) protocol.Mutation {
	return protocol.Mutation{MutationID: mutationID(n), EntityID: timerID, EntityKind: "timer", Operation: "timer_stop",
		PayloadVersion: 5, ClientMutationTime: payload.StoppedAt, Payload: payload}
}

// sessionSleepCreate builds a v2 sleep create mutation carrying a session id.
func sessionSleepCreate(n int, entityID, babyID string, session *string) protocol.Mutation {
	return protocol.Mutation{MutationID: mutationID(n), EntityID: entityID, EntityKind: "sleep", Operation: "create",
		PayloadVersion: 2, ClientMutationTime: "2026-09-20T13:00:00.000000Z",
		Payload: protocol.SleepPayload{BabyID: babyID, StartedAt: "2026-09-20T12:00:00.000000Z",
			EndedAt: stringPointer("2026-09-20T13:00:00.000000Z"), TimeZoneID: "Europe/Zurich",
			SleepSessionID: session}}
}

func lockedTimerPayload(t *testing.T, h *testkit.Harness, timerID string) protocol.TimerStartPayload {
	t.Helper()
	var locked protocol.TimerStartPayload
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "mutate_record", func(tx pgx.Tx, _ access.Grant) error {
		record, err := records.NewRepository().GetTimer(t.Context(), tx, h.FamilyID, timerID)
		locked = record.Payload
		return err
	})
	if err != nil {
		t.Fatalf("read timer %s: %v", timerID, err)
	}
	return locked
}

func TestStopCopiesTimerSessionIntoTheCompletedSleep(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	session := "aaaaaaaa-0000-4000-8000-000000000001"
	start := sessionTimerStart(t, h, 2501, "aaaaaaaa-0000-4000-8000-0000000000a1", "sleep", &session)
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start); err != nil || outcome.Status != "applied" {
		t.Fatalf("start outcome=%+v err=%v", outcome, err)
	}
	babyID := start.Payload.(protocol.TimerStartPayload).BabyID
	stop := sessionTimerStop(2502, start.EntityID, protocol.TimerStopPayload{BabyID: babyID,
		CompletedRecordID: "aaaaaaaa-0000-4000-8000-0000000000b1", StoppedAt: "2026-09-20T17:00:00.000000Z"})
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "applied" || len(outcome.RelatedEntities) != 1 {
		t.Fatalf("stop outcome=%+v", outcome)
	}
	sleep := outcome.RelatedEntities[0].Payload.(protocol.SleepPayload)
	if sleep.SleepSessionID == nil || *sleep.SleepSessionID != session {
		t.Fatalf("completed sleep lost its session: %+v", sleep)
	}
}

func TestStopCopiesTimerSessionIntoTheCompletedFeed(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	session := "aaaaaaaa-0000-4000-8000-000000000002"
	start := sessionTimerStart(t, h, 2503, "aaaaaaaa-0000-4000-8000-0000000000a2", "feed", &session)
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start); err != nil || outcome.Status != "applied" {
		t.Fatalf("start outcome=%+v err=%v", outcome, err)
	}
	babyID := start.Payload.(protocol.TimerStartPayload).BabyID
	startedAt := start.Payload.(protocol.TimerStartPayload).StartedAt
	// The breast side has been running since the timer started; stop 30s
	// later so the closing part survives even a cleanup pass.
	stop := sessionTimerStop(2504, start.EntityID, protocol.TimerStopPayload{BabyID: babyID,
		CompletedRecordID: "aaaaaaaa-0000-4000-8000-0000000000b2", StoppedAt: "2026-09-20T15:00:30.000000Z",
		ComponentID: "aaaaaaaa-0000-4000-8000-0000000000c2"})
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "applied" || len(outcome.RelatedEntities) != 1 {
		t.Fatalf("stop outcome=%+v startedAt=%s", outcome, startedAt)
	}
	feed := outcome.RelatedEntities[0].Payload.(protocol.FeedPayload)
	if feed.FeedingSessionID == nil || *feed.FeedingSessionID != session {
		t.Fatalf("completed feed lost its session: %+v", feed)
	}
}

func TestTimerEditCanDetachTheSession(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	session := "aaaaaaaa-0000-4000-8000-000000000003"
	timerID := "aaaaaaaa-0000-4000-8000-0000000000a3"
	start := sessionTimerStart(t, h, 2505, timerID, "feed", &session)
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start); err != nil || outcome.Status != "applied" {
		t.Fatalf("start outcome=%+v err=%v", outcome, err)
	}
	babyID := start.Payload.(protocol.TimerStartPayload).BabyID
	edit := protocol.Mutation{MutationID: mutationID(2506), EntityID: timerID, EntityKind: "timer", Operation: "update",
		PayloadVersion: 5, ClientMutationTime: "2026-09-20T15:00:30.000000Z",
		Payload: protocol.TimerEditPayload{BabyID: babyID, Components: []protocol.FeedComponent{}, SessionID: nil}}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "applied" {
		t.Fatalf("edit outcome=%+v", outcome)
	}
	if locked := lockedTimerPayload(t, h, timerID); locked.SessionID != nil {
		t.Fatalf("edit did not detach the session: %+v", locked)
	}
	stop := sessionTimerStop(2507, timerID, protocol.TimerStopPayload{BabyID: babyID,
		CompletedRecordID: "aaaaaaaa-0000-4000-8000-0000000000b3", StoppedAt: "2026-09-20T15:10:00.000000Z",
		ComponentID: "aaaaaaaa-0000-4000-8000-0000000000c3"})
	stopOutcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop)
	if err != nil {
		t.Fatal(err)
	}
	if stopOutcome.Status != "applied" || len(stopOutcome.RelatedEntities) != 1 {
		t.Fatalf("stop outcome=%+v", stopOutcome)
	}
	feed := stopOutcome.RelatedEntities[0].Payload.(protocol.FeedPayload)
	if feed.FeedingSessionID != nil {
		t.Fatalf("stopped feed kept a detached session: %+v", feed)
	}
}

func TestASessionIDOwnedByAnotherBabyOrKindIsRefused(t *testing.T) {
	h := testkit.New(t)
	babyA := seedBaby(t, h)
	babyB := seedSecondBaby(t, h)
	session := "aaaaaaaa-0000-4000-8000-000000000004"

	sleepA := sessionSleepCreate(2508, "aaaaaaaa-0000-4000-8000-0000000000d1", babyA, &session)
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, sleepA); err != nil || outcome.Status != "applied" {
		t.Fatalf("seed sleep outcome=%+v err=%v", outcome, err)
	}

	// 1. A feed timer start for the same baby A, reusing a session id that
	// already belongs to a live sleep: refused, same baby or not, because the
	// kind differs.
	feedStart := sessionTimerStart(t, h, 2509, "aaaaaaaa-0000-4000-8000-0000000000a4", "feed", &session)
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, feedStart)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "rejected" || outcome.Code == nil || *outcome.Code != "record_parent_mismatch" {
		t.Fatalf("feed timer start with a sleep's session: outcome=%+v", outcome)
	}

	// 2. A sleep v2 create for baby B, reusing baby A's session id: refused,
	// same kind but a different baby.
	sleepB := sessionSleepCreate(2510, "aaaaaaaa-0000-4000-8000-0000000000d2", babyB, &session)
	outcome, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, sleepB)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "rejected" || outcome.Code == nil || *outcome.Code != "record_parent_mismatch" {
		t.Fatalf("sleep create with another baby's session: outcome=%+v", outcome)
	}

	// 3. A sleep v2 create for baby A, same baby and same kind: applied.
	sleepA2 := sessionSleepCreate(2511, "aaaaaaaa-0000-4000-8000-0000000000d3", babyA, &session)
	outcome, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, sleepA2)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "applied" {
		t.Fatalf("sleep create with its own baby's session: outcome=%+v", outcome)
	}
}

// A timer edit's session id is held to the same owner rule as a start's: one
// owned by another kind (here a sleep's) is refused, and the timer keeps the
// session it had.
func TestATimerEditToASessionOwnedByAnotherKindIsRefused(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	sleepSession := "aaaaaaaa-0000-4000-8000-000000000005"
	sleep := sessionSleepCreate(2512, "aaaaaaaa-0000-4000-8000-0000000000d4", babyID, &sleepSession)
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, sleep); err != nil || outcome.Status != "applied" {
		t.Fatalf("seed sleep outcome=%+v err=%v", outcome, err)
	}
	timerID := "aaaaaaaa-0000-4000-8000-0000000000a5"
	start := sessionTimerStart(t, h, 2513, timerID, "feed", nil)
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start); err != nil || outcome.Status != "applied" {
		t.Fatalf("start outcome=%+v err=%v", outcome, err)
	}

	edit := protocol.Mutation{MutationID: mutationID(2514), EntityID: timerID, EntityKind: "timer", Operation: "update",
		PayloadVersion: 5, ClientMutationTime: "2026-09-20T15:00:30.000000Z",
		Payload: protocol.TimerEditPayload{BabyID: babyID, Components: []protocol.FeedComponent{}, SessionID: &sleepSession}}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "rejected" || outcome.Code == nil || *outcome.Code != "record_parent_mismatch" {
		t.Fatalf("edit to a sleep's session: outcome=%+v", outcome)
	}
	if outcome.CanonicalEntity == nil {
		t.Fatalf("the refusal carries no stored timer to restore: %+v", outcome)
	}
	if locked := lockedTimerPayload(t, h, timerID); locked.SessionID != nil {
		t.Fatalf("a refused edit changed the session: %+v", locked)
	}
}
