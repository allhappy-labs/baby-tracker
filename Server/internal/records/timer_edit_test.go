package records_test

import (
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// The harness clock reads 2026-09-21T12:00:00Z; every running timer here
// starts an hour earlier so a moved start has room on both sides.
const (
	editStartedAt = "2026-09-21T11:00:00.000000Z"
	editPartA     = "36000000-0000-4000-8000-000000000001"
	editPartB     = "36000000-0000-4000-8000-000000000002"
	editPartC     = "36000000-0000-4000-8000-000000000003"
	editPartD     = "36000000-0000-4000-8000-000000000004"
)

func timerCommand(mutation int, operation, clientTime string, payload protocol.Payload) protocol.Mutation {
	return protocol.Mutation{MutationID: mutationID(mutation), EntityID: timerA, EntityKind: "timer", Operation: operation,
		PayloadVersion: 5, ClientMutationTime: clientTime, Payload: payload}
}

func applyAs(t *testing.T, h *testkit.Harness, principal access.Principal, m protocol.Mutation) protocol.Outcome {
	t.Helper()
	outcome, err := h.Service.Apply(t.Context(), principal, h.FamilyID, m)
	if err != nil {
		t.Fatalf("%s: %v", m.Operation, err)
	}
	return outcome
}

func requireApplied(t *testing.T, outcome protocol.Outcome) protocol.TimerStartPayload {
	t.Helper()
	if outcome.Status != "applied" || outcome.CanonicalEntity == nil {
		t.Fatalf("outcome=%+v code=%v", outcome, outcome.Code)
	}
	payload, ok := outcome.CanonicalEntity.Payload.(protocol.TimerStartPayload)
	if !ok {
		t.Fatalf("canonical payload=%T", outcome.CanonicalEntity.Payload)
	}
	return payload
}

func startTiedFeed(t *testing.T, h *testkit.Harness, babyID string, mutation int) {
	t.Helper()
	left := "left"
	requireApplied(t, applyAs(t, h, h.Owner, timerCommand(mutation, "timer_start", editStartedAt, protocol.TimerStartPayload{
		BabyID: babyID, TimerKind: "feed", StartedAt: editStartedAt, TimeZoneID: "Europe/Zurich",
		CurrentSide: &left, CurrentSideStartedAt: stringPointer(editStartedAt),
		CompletedComponents: []protocol.FeedComponent{},
	})))
}

// startFeedWithParts seeds a running feed timer directly with whatever
// completed parts and open side a test needs, the way the one-part-timer
// client actually reaches that state: folded into the single `timer_start`
// it sends, never through a since-removed switch or attach-bottle command.
func startFeedWithParts(t *testing.T, h *testkit.Harness, babyID string, mutation int, currentSide, currentSideStartedAt *string, components ...protocol.FeedComponent) protocol.TimerStartPayload {
	t.Helper()
	if components == nil {
		components = []protocol.FeedComponent{}
	}
	return requireApplied(t, applyAs(t, h, h.Owner, timerCommand(mutation, "timer_start", editStartedAt, protocol.TimerStartPayload{
		BabyID: babyID, TimerKind: "feed", StartedAt: editStartedAt, TimeZoneID: "Europe/Zurich",
		CurrentSide: currentSide, CurrentSideStartedAt: currentSideStartedAt,
		CompletedComponents: components,
	})))
}

func editTimer(mutation int, babyID string, startedAt *string, components ...protocol.FeedComponent) protocol.Mutation {
	if components == nil {
		components = []protocol.FeedComponent{}
	}
	return timerCommand(mutation, "update", "2026-09-21T11:59:00.000000Z",
		protocol.TimerEditPayload{BabyID: babyID, StartedAt: startedAt, Components: components})
}

func breastPart(id, side string, seconds uint32) protocol.FeedComponent {
	return protocol.FeedComponent{ID: id, Kind: "breast", Side: &side, Seconds: &seconds}
}

func bottlePart(id, volume, contents string) protocol.FeedComponent {
	return protocol.FeedComponent{ID: id, Kind: "bottle", VolumeML: protocol.MustParseDecimal(volume), Contents: &contents}
}

func requireRefused(t *testing.T, outcome protocol.Outcome, status, code string) {
	t.Helper()
	if outcome.Status != status || outcome.Code == nil || *outcome.Code != code {
		t.Fatalf("outcome=%+v code=%v, want %s/%s", outcome, outcome.Code, status, code)
	}
	if outcome.CanonicalEntity == nil || outcome.CanonicalEntity.Meta.ID != timerA {
		t.Fatalf("refusal did not carry the canonical timer: %+v", outcome)
	}
}

// readStoredTimer reads the timer back the two ways a phone can see it: the
// locked row the next command is judged against, and the snapshot a new
// caregiver downloads.
func readStoredTimer(t *testing.T, h *testkit.Harness) (protocol.TimerStartPayload, protocol.TimerStartPayload) {
	t.Helper()
	var locked, snapshot protocol.TimerStartPayload
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "mutate_record", func(tx pgx.Tx, _ access.Grant) error {
		record, err := records.NewRepository().GetTimer(t.Context(), tx, h.FamilyID, timerA)
		if err != nil {
			return err
		}
		locked = record.Payload
		return records.NewRepository().StreamCanonical(t.Context(), tx, h.FamilyID, func(entity protocol.CanonicalEntity) error {
			if entity.EntityKind == "timer" && entity.Meta.ID == timerA {
				snapshot = entity.Payload.(protocol.TimerStartPayload)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("read timer: %v", err)
	}
	return locked, snapshot
}

func pulledTimerEdits(t *testing.T, h *testkit.Harness) []protocol.TimerStartPayload {
	t.Helper()
	var edits []protocol.TimerStartPayload
	for _, change := range h.Pull(t, h.Caregiver, nil, nil).Changes {
		if change.EntityID == timerA && change.Operation == "update" {
			edits = append(edits, change.SnapshotOrTombstone.Payload.(protocol.TimerStartPayload))
		}
	}
	return edits
}

func stopTimer(t *testing.T, h *testkit.Harness, babyID string, mutation int, at, componentID string) protocol.FeedPayload {
	t.Helper()
	outcome := applyAs(t, h, h.Owner, timerCommand(mutation, "timer_stop", at,
		protocol.TimerStopPayload{BabyID: babyID, CompletedRecordID: recordA, StoppedAt: at, ComponentID: componentID}))
	if outcome.Status != "applied" || len(outcome.RelatedEntities) != 1 {
		t.Fatalf("stop=%+v code=%v", outcome, outcome.Code)
	}
	return outcome.RelatedEntities[0].Payload.(protocol.FeedPayload)
}

func TestTimerEditMovesStartWithTiedSideAndTheStopUsesIt(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	startTiedFeed(t, h, babyID, 901)
	moved := "2026-09-21T10:55:00Z"
	edited := requireApplied(t, applyAs(t, h, h.Caregiver, editTimer(902, babyID, &moved)))
	want := "2026-09-21T10:55:00.000000Z"
	if edited.StartedAt != want || edited.CurrentSideStartedAt == nil || *edited.CurrentSideStartedAt != want ||
		edited.CurrentSide == nil || *edited.CurrentSide != "left" {
		t.Fatalf("edited=%+v", edited)
	}
	locked, snapshot := readStoredTimer(t, h)
	for name, stored := range map[string]protocol.TimerStartPayload{"locked row": locked, "snapshot": snapshot} {
		if stored.StartedAt != want || stored.CurrentSideStartedAt == nil || *stored.CurrentSideStartedAt != want {
			t.Fatalf("%s did not keep the edited start: %+v", name, stored)
		}
	}
	edits := pulledTimerEdits(t, h)
	if len(edits) != 1 || edits[0].StartedAt != want {
		t.Fatalf("journal edits=%+v", edits)
	}
	completion := stopTimer(t, h, babyID, 903, "2026-09-21T11:20:00.000000Z", editPartA)
	if completion.StartedAt != want || len(completion.Components) != 1 || *completion.Components[0].Seconds != 25*60 {
		t.Fatalf("completion ignored the edit: %+v", completion)
	}
}

func TestTimerEditLeavesAnUntiedSideAndRefusesAStartAfterIt(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	right, sideStart := "right", "2026-09-21T11:10:00.000000Z"
	startFeedWithParts(t, h, babyID, 911, &right, &sideStart, breastPart(editPartA, "left", 600))
	earlier := "2026-09-21T10:58:00.000000Z"
	edited := requireApplied(t, applyAs(t, h, h.Caregiver, editTimer(913, babyID, &earlier)))
	if edited.StartedAt != earlier || *edited.CurrentSideStartedAt != "2026-09-21T11:10:00.000000Z" {
		t.Fatalf("untied side moved: %+v", edited)
	}
	afterSide := "2026-09-21T11:12:00.000000Z"
	requireRefused(t, applyAs(t, h, h.Caregiver, editTimer(914, babyID, &afterSide)), "rejected", "invalid_timer_time")
	// Ten finished minutes cannot fit between 11:05 and the side that began at 11:10.
	tooLate := "2026-09-21T11:05:00.000000Z"
	refused := applyAs(t, h, h.Caregiver, editTimer(915, babyID, &tooLate))
	requireRefused(t, refused, "rejected", "invalid_timer_time")
	if refused.CanonicalEntity.Payload.(protocol.TimerStartPayload).StartedAt != earlier {
		t.Fatalf("refusal did not return the stored timer: %+v", refused.CanonicalEntity)
	}
}

func TestTimerEditReplacesPartsInPlace(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	startFeedWithParts(t, h, babyID, 921, nil, nil,
		breastPart(editPartA, "left", 600), breastPart(editPartB, "right", 300), bottlePart(editPartC, "60", "formula"))
	journal := h.Count(t, "mutation_journal", h.FamilyID)
	edited := requireApplied(t, applyAs(t, h, h.Caregiver, editTimer(924, babyID, nil,
		bottlePart(editPartC, "70", "mixed"), breastPart(editPartA, "left", 480))))
	parts := edited.CompletedComponents
	if len(parts) != 3 || parts[0].ID != editPartA || parts[1].ID != editPartB || parts[2].ID != editPartC ||
		*parts[0].Seconds != 480 || *parts[1].Seconds != 300 || parts[2].VolumeML.String() != "70" || *parts[2].Contents != "mixed" {
		t.Fatalf("parts=%+v", parts)
	}
	if edited.StartedAt != editStartedAt || edited.CurrentSide != nil {
		t.Fatalf("edit touched more than the parts: %+v", edited)
	}
	if got := h.Count(t, "mutation_journal", h.FamilyID); got != journal+1 {
		t.Fatalf("journal rows %d -> %d", journal, got)
	}
	locked, snapshot := readStoredTimer(t, h)
	if len(locked.CompletedComponents) != 3 || *locked.CompletedComponents[0].Seconds != 480 ||
		len(snapshot.CompletedComponents) != 3 || *snapshot.CompletedComponents[2].Contents != "mixed" {
		t.Fatalf("stored parts locked=%+v snapshot=%+v", locked.CompletedComponents, snapshot.CompletedComponents)
	}
	completion := stopTimer(t, h, babyID, 925, "2026-09-21T11:30:00.000000Z", "")
	if len(completion.Components) != 3 || *completion.Components[0].Seconds != 480 || completion.Components[2].VolumeML.String() != "70" {
		t.Fatalf("completion=%+v", completion.Components)
	}
}

func TestTimerEditRefusals(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	startFeedWithParts(t, h, babyID, 931, nil, nil,
		breastPart(editPartA, "left", 600), breastPart(editPartB, "right", 300), bottlePart(editPartC, "60", "formula"))
	future, tooOld := "2026-09-21T12:02:00.000000Z", "2026-09-14T11:59:00.000000Z"
	for name, test := range map[string]struct {
		mutation protocol.Mutation
		code     string
	}{
		"unknown part":            {editTimer(934, babyID, nil, breastPart(editPartD, "left", 60)), "timer_component_not_found"},
		"part changes kind":       {editTimer(935, babyID, nil, breastPart(editPartC, "left", 60)), "invalid_mutation"},
		"parts exceed elapsed":    {editTimer(936, babyID, nil, breastPart(editPartA, "left", 3600)), "invalid_timer_time"},
		"start in the future":     {editTimer(937, babyID, &future), "invalid_timer_time"},
		"start over a week early": {editTimer(938, babyID, &tooOld), "invalid_timer_time"},
	} {
		t.Run(name, func(t *testing.T) {
			requireRefused(t, applyAs(t, h, h.Caregiver, test.mutation), "rejected", test.code)
		})
	}
	t.Run("another baby", func(t *testing.T) {
		other := testkit.MustMutation(t, "baby.json")
		other.MutationID, other.EntityID = mutationID(939), "10000000-0000-4000-8000-000000000909"
		if outcome := applyAs(t, h, h.Owner, other); outcome.Status != "applied" {
			t.Fatalf("second baby=%+v", outcome)
		}
		requireRefused(t, applyAs(t, h, h.Caregiver, editTimer(940, other.EntityID, nil, breastPart(editPartA, "left", 60))),
			"rejected", "record_parent_mismatch")
	})
	// Only the current timer version is accepted; an older client's edit is
	// refused before it reaches the timer.
	for name, m := range map[string]protocol.Mutation{
		"payload version 1": func() protocol.Mutation {
			m := editTimer(941, babyID, nil, breastPart(editPartA, "left", 60))
			m.PayloadVersion = 1
			return m
		}(),
		"payload version 4": func() protocol.Mutation {
			m := editTimer(942, babyID, nil, breastPart(editPartA, "left", 60))
			m.PayloadVersion = 4
			return m
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			outcome := applyAs(t, h, h.Caregiver, m)
			if outcome.Status != "rejected" || outcome.Code == nil || *outcome.Code != "invalid_mutation" {
				t.Fatalf("outcome=%+v code=%v", outcome, outcome.Code)
			}
		})
	}
	t.Run("viewer", func(t *testing.T) {
		if _, err := h.Family.ChangeRole(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID, "viewer"); err != nil {
			t.Fatal(err)
		}
		_, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, editTimer(943, babyID, nil, breastPart(editPartA, "left", 60)))
		// The refusal is raised by the family guard or the sync gate, whichever
		// sees the role first; both spell it the same.
		if err == nil || err.Error() != "read_only_caregiver" {
			t.Fatalf("viewer edit err=%v", err)
		}
	})
	locked, _ := readStoredTimer(t, h)
	if locked.StartedAt != editStartedAt || *locked.CompletedComponents[0].Seconds != 600 || *locked.CompletedComponents[2].Contents != "formula" {
		t.Fatalf("a refused edit changed the timer: %+v", locked)
	}
}

func TestSleepTimerEditTakesAStartButNoParts(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	requireApplied(t, applyAs(t, h, h.Owner, timerCommand(951, "timer_start", editStartedAt, protocol.TimerStartPayload{
		BabyID: babyID, TimerKind: "sleep", StartedAt: editStartedAt, TimeZoneID: "Europe/Zurich",
		CompletedComponents: []protocol.FeedComponent{},
	})))
	requireRefused(t, applyAs(t, h, h.Caregiver, editTimer(952, babyID, nil, breastPart(editPartA, "left", 60))),
		"rejected", "invalid_timer_kind")
	earlier := "2026-09-21T10:30:00.000000Z"
	if edited := requireApplied(t, applyAs(t, h, h.Caregiver, editTimer(953, babyID, &earlier))); edited.StartedAt != earlier {
		t.Fatalf("sleep edit=%+v", edited)
	}
}

func TestTimerEditAfterStopIsSupersededWithTheCompletion(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	startTiedFeed(t, h, babyID, 961)
	stopTimer(t, h, babyID, 962, "2026-09-21T11:20:00.000000Z", editPartA)
	moved := "2026-09-21T10:55:00.000000Z"
	outcome := applyAs(t, h, h.Caregiver, editTimer(963, babyID, &moved))
	requireRefused(t, outcome, "superseded", "timer_ended")
	if len(outcome.RelatedEntities) != 1 || outcome.RelatedEntities[0].Meta.ID != recordA ||
		outcome.RelatedEntities[0].Payload.(protocol.FeedPayload).StartedAt != editStartedAt {
		t.Fatalf("related=%+v", outcome.RelatedEntities)
	}
}

func TestTimerEditThatChangesNothingWritesNoJournalRow(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	right, sideStart := "right", "2026-09-21T11:10:00.000000Z"
	startFeedWithParts(t, h, babyID, 971, &right, &sideStart, breastPart(editPartA, "left", 600))
	journal, sequence := h.Count(t, "mutation_journal", h.FamilyID), h.FamilySequence(t)
	same := "2026-09-21T11:00:00Z"
	outcome := applyAs(t, h, h.Caregiver, editTimer(973, babyID, &same, breastPart(editPartA, "left", 600)))
	requireApplied(t, outcome)
	if outcome.Cursor == nil || outcome.Cursor.Sequence != sequence || outcome.CanonicalEntity.Meta.EntityVersion != 1 {
		t.Fatalf("no-op cursor=%+v version=%d family sequence=%d", outcome.Cursor, outcome.CanonicalEntity.Meta.EntityVersion, sequence)
	}
	if got := h.Count(t, "mutation_journal", h.FamilyID); got != journal {
		t.Fatalf("no-op edit wrote a journal row: %d -> %d", journal, got)
	}
}

func TestResentTimerEditIsAlreadyApplied(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	startTiedFeed(t, h, babyID, 995)
	moved := "2026-09-21T10:55:00.000000Z"
	edit := editTimer(996, babyID, &moved)
	first := applyAs(t, h, h.Caregiver, edit)
	requireApplied(t, first)
	replay := applyAs(t, h, h.Caregiver, edit)
	if replay.Status != "already_applied" || replay.OriginalStatus == nil || *replay.OriginalStatus != "applied" ||
		replay.Cursor == nil || *replay.Cursor != *first.Cursor {
		t.Fatalf("replay=%+v", replay)
	}
	if edits := pulledTimerEdits(t, h); len(edits) != 1 {
		t.Fatalf("replay wrote another edit: %d", len(edits))
	}
}
