package sync_test

import (
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// pumpMutation is pump.json as a create of id, running (ended nil) or
// finished at ended.
func pumpMutation(t *testing.T, h *testkit.Harness, id string, ended *string) protocol.Mutation {
	t.Helper()
	m := testkit.MustMutation(t, "pump.json")
	m.MutationID, m.EntityID = h.CommandID(), id
	p := m.Payload.(protocol.PumpPayload)
	p.EndedAt = ended
	if ended == nil {
		p.VolumeML = nil
	}
	m.Payload = p
	return m
}

// Review Focus 1. One running pump per family. A second open start loses as
// superseded and names the running one, writing nothing; a finished session
// is never in the way; once the first ends a new start goes through; and a
// finished session cannot be reopened.
func TestSecondOpenPumpIsSupersededAndAFinishedOneIsNot(t *testing.T) {
	h := testkit.New(t)
	const first, second, finished = "b0000000-0000-4000-8000-000000000011", "b0000000-0000-4000-8000-000000000012", "b0000000-0000-4000-8000-000000000013"
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, pumpMutation(t, h, first, nil)); err != nil || out.Status != "applied" {
		t.Fatalf("first %+v %v", out, err)
	}
	before := h.FamilySequence(t)
	out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, pumpMutation(t, h, second, nil))
	if err != nil || out.Status != "superseded" || out.Code == nil || *out.Code != "pump_running" ||
		out.CanonicalEntity == nil || out.CanonicalEntity.Meta.ID != first || out.CanonicalEntity.EntityKind != "pump" {
		t.Fatalf("second open start %+v %v", out, err)
	}
	if h.FamilySequence(t) != before || h.CareCount(t, "pump", h.FamilyID) != 1 {
		t.Fatal("a superseded start wrote something")
	}
	end := "2026-09-20T14:30:00.000000Z"
	if out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, pumpMutation(t, h, finished, &end)); err != nil || out.Status != "applied" {
		t.Fatalf("finished alongside a running one %+v %v", out, err)
	}
	// The running pump itself may be edited while it runs.
	edit := pumpMutation(t, h, first, nil)
	p := edit.Payload.(protocol.PumpPayload)
	p.Side = "left"
	edit.Payload, edit.Operation, edit.ObservedEntityVersion = p, "update", 1
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, edit); err != nil || out.Status != "applied" {
		t.Fatalf("editing the running pump %+v %v", out, err)
	}
	stop := pumpMutation(t, h, first, &end)
	stop.Operation, stop.ObservedEntityVersion = "update", 2
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop); err != nil || out.Status != "applied" {
		t.Fatalf("stop %+v %v", out, err)
	}
	if out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, pumpMutation(t, h, second, nil)); err != nil || out.Status != "applied" {
		t.Fatalf("second start after the stop %+v %v", out, err)
	}
	reopen := pumpMutation(t, h, first, nil)
	reopen.Operation, reopen.ObservedEntityVersion = "update", 3
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, reopen); err != nil || out.Status != "rejected" || out.Code == nil || *out.Code != "pump_reopened" {
		t.Fatalf("reopen %+v %v", out, err)
	}
	// Three pumps count toward the record quota; none is a baby or a timer.
	if count, _ := h.FamilyUsage(t); count != 3 {
		t.Fatalf("quota count %d", count)
	}
}

// Review Focus 1. Two open starts in one batch: the working set sees the
// first before the database does, so the second is superseded and only one
// row is written.
func TestTwoOpenStartsInOneBatchKeepOne(t *testing.T) {
	h := testkit.New(t)
	const first, second = "b0000000-0000-4000-8000-000000000021", "b0000000-0000-4000-8000-000000000022"
	request := h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil)
	request.Mutations = []protocol.Mutation{pumpMutation(t, h, first, nil), pumpMutation(t, h, second, nil)}
	response, err := applyAndPull(t, h.Service, h.Owner, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Outcomes) != 2 || response.Outcomes[0].Status != "applied" || response.Outcomes[1].Status != "superseded" ||
		response.Outcomes[1].Code == nil || *response.Outcomes[1].Code != "pump_running" ||
		response.Outcomes[1].CanonicalEntity == nil || response.Outcomes[1].CanonicalEntity.Meta.ID != first {
		t.Fatalf("outcomes %+v", response.Outcomes)
	}
	if n := h.CareCount(t, "pump", h.FamilyID); n != 1 {
		t.Fatalf("pump rows %d", n)
	}
}

func TestPumpInTheFutureIsRefused(t *testing.T) {
	h := testkit.New(t)
	future := pumpMutation(t, h, "b0000000-0000-4000-8000-000000000031", nil)
	p := future.Payload.(protocol.PumpPayload)
	p.StartedAt = "2099-01-01T00:00:00.000000Z"
	future.Payload = p
	out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, future)
	if err != nil || out.Status != "rejected" || out.Code == nil || *out.Code != "invalid_timestamp" {
		t.Fatalf("future start %+v %v", out, err)
	}
	end := "2099-01-01T00:00:00.000000Z"
	late := pumpMutation(t, h, "b0000000-0000-4000-8000-000000000032", &end)
	q := late.Payload.(protocol.PumpPayload)
	q.StartedAt = "2098-12-31T23:00:00.000000Z"
	late.Payload = q
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, late); err != nil || out.Status != "rejected" || *out.Code != "invalid_timestamp" {
		t.Fatalf("future end %+v %v", out, err)
	}
	if n := h.CareCount(t, "pump", h.FamilyID); n != 0 {
		t.Fatalf("pump rows %d", n)
	}
}

// Deleting a baby never deletes pumping history: the pump stays live in the
// table, in a fresh snapshot (last, by kind order), and in the changes other
// phones pull.
func TestPumpSurvivesABabyDeleteSnapshotAndPull(t *testing.T) {
	h := testkit.New(t)
	before := h.FamilySequence(t)
	baby := testkit.MustMutation(t, "baby.json")
	for _, fixture := range []string{"baby.json", "immunization.json"} {
		if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, fixture)); err != nil || out.Status != "applied" {
			t.Fatalf("%s %+v %v", fixture, out, err)
		}
	}
	created, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, testkit.MustMutation(t, "pump.json"))
	if err != nil || created.Status != "applied" {
		t.Fatalf("pump %+v %v", created, err)
	}
	got := created.CanonicalEntity.Payload.(protocol.PumpPayload)
	if got.Side != "both" || got.VolumeML == nil || got.VolumeML.String() != "90.5" || got.EndedAt == nil ||
		got.StartedAt != "2026-09-20T14:10:00.000000Z" || got.TimeZoneID != "Europe/Zurich" || got.Note != nil {
		t.Fatalf("created %+v", got)
	}
	gone := baby
	gone.MutationID, gone.Operation, gone.Payload, gone.ObservedEntityVersion = h.CommandID(), "delete", protocol.DeletePayload{}, 1
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, gone); err != nil || out.Status != "applied" {
		t.Fatalf("baby delete %+v %v", out, err)
	}
	// The shot went with its baby; the pump did not.
	if count, _ := h.FamilyUsage(t); count != 1 {
		t.Fatalf("quota count %d", count)
	}
	pull, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(before), nil))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range pull.Changes {
		if change.EntityKind != "pump" {
			continue
		}
		found = true
		if change.Operation != "create" || change.Optional || change.SnapshotOrTombstone.Meta.TombstonedAt != nil {
			t.Fatalf("pump change %+v", change)
		}
	}
	if !found {
		t.Fatal("pump create missing from the pull")
	}
	h.RebuildSyncService(t, h.Limits)
	snapshot, err := fullSnapshot(t, h.Service, h.Caregiver, protocol.SnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	last := snapshot.Entities[len(snapshot.Entities)-1]
	if last.EntityKind != "pump" || last.Meta.TombstonedAt != nil {
		t.Fatalf("snapshot ends with %s (tombstoned %v)", last.EntityKind, last.Meta.TombstonedAt)
	}
	if p := last.Payload.(protocol.PumpPayload); p.Side != "both" || p.VolumeML.String() != "90.5" {
		t.Fatalf("snapshot pump %+v", p)
	}
}

func TestPumpDeleteIsAnEmptyPayloadTombstone(t *testing.T) {
	h := testkit.New(t)
	m := testkit.MustMutation(t, "pump.json")
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || out.Status != "applied" {
		t.Fatalf("create %+v %v", out, err)
	}
	baby := "10000000-0000-4000-8000-000000000001"
	m.MutationID, m.Operation, m.ObservedEntityVersion, m.Payload = h.CommandID(), "delete", 1, protocol.DeletePayload{BabyID: &baby}
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || out.Status != "rejected" || *out.Code != "invalid_mutation" {
		t.Fatalf("a pump delete naming a baby %+v %v", out, err)
	}
	m.MutationID, m.Payload = h.CommandID(), protocol.DeletePayload{}
	out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if err != nil || out.Status != "applied" || out.CanonicalEntity.Meta.TombstonedAt == nil {
		t.Fatalf("delete %+v %v", out, err)
	}
	if count, _ := h.FamilyUsage(t); count != 0 {
		t.Fatalf("quota count %d", count)
	}
	m.MutationID, m.Operation, m.ObservedEntityVersion = h.CommandID(), "update", 2
	m.Payload = testkit.MustMutation(t, "pump.json").Payload
	if late, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || late.Status != "superseded" || *late.Code != "entity_deleted" {
		t.Fatalf("an update after the delete %+v %v", late, err)
	}
}

// A phone that has not synced still shows the session running; the other
// parent has stopped it and entered the amount. Its late Stop must not
// replace the real end and wipe the amount: it is superseded and told the
// session as it stands.
func TestAStaleStopDoesNotReplaceAFinishedPump(t *testing.T) {
	h := testkit.New(t)
	const id = "b0000000-0000-4000-8000-000000000041"
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, pumpMutation(t, h, id, nil)); err != nil || out.Status != "applied" {
		t.Fatalf("start %+v %v", out, err)
	}
	end := "2026-09-20T14:30:00.000000Z"
	stop := pumpMutation(t, h, id, &end) // 90.5 ml, as the fixture
	stop.Operation, stop.ObservedEntityVersion = "update", 1
	if out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, stop); err != nil || out.Status != "applied" {
		t.Fatalf("the other parent's stop %+v %v", out, err)
	}
	before := h.FamilySequence(t)
	later := "2026-09-20T14:50:00.000000Z"
	stale := pumpMutation(t, h, id, &later)
	p := stale.Payload.(protocol.PumpPayload)
	p.VolumeML = nil
	stale.Payload, stale.Operation, stale.ObservedEntityVersion = p, "update", 1
	out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stale)
	if err != nil || out.Status != "superseded" || out.Code == nil || *out.Code != "pump_already_stopped" || out.CanonicalEntity == nil {
		t.Fatalf("stale stop %+v %v", out, err)
	}
	kept := out.CanonicalEntity.Payload.(protocol.PumpPayload)
	if kept.EndedAt == nil || *kept.EndedAt != end || kept.VolumeML == nil || kept.VolumeML.String() != "90.5" || out.CanonicalEntity.Meta.EntityVersion != 2 {
		t.Fatalf("the finished session changed: %+v v%d", kept, out.CanonicalEntity.Meta.EntityVersion)
	}
	if h.FamilySequence(t) != before {
		t.Fatal("a superseded stop wrote something")
	}
	// A phone that has seen it finished may still correct it.
	edit := pumpMutation(t, h, id, &end)
	q := edit.Payload.(protocol.PumpPayload)
	q.VolumeML = protocol.MustParseDecimal("80")
	edit.Payload, edit.Operation, edit.ObservedEntityVersion = q, "update", 2
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, edit); err != nil || out.Status != "applied" {
		t.Fatalf("an edit of the finished session by a phone that saw it %+v %v", out, err)
	}
}

// A phone that was offline sends "end the running pump, start another" — or
// "delete it, start another" — in one batch. Both go through, and the family
// has exactly one running pump.
func TestEndingOrDeletingTheRunningPumpAndStartingAnotherInOneBatch(t *testing.T) {
	for name, close := range map[string]func(m *protocol.Mutation){
		"ended": func(m *protocol.Mutation) {
			end := "2026-09-20T14:30:00.000000Z"
			p := m.Payload.(protocol.PumpPayload)
			p.EndedAt = &end
			m.Payload, m.Operation = p, "update"
		},
		"deleted": func(m *protocol.Mutation) { m.Payload, m.Operation = protocol.DeletePayload{}, "delete" },
	} {
		t.Run(name, func(t *testing.T) {
			h := testkit.New(t)
			const first, second = "b0000000-0000-4000-8000-000000000051", "b0000000-0000-4000-8000-000000000052"
			if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, pumpMutation(t, h, first, nil)); err != nil || out.Status != "applied" {
				t.Fatalf("first %+v %v", out, err)
			}
			closing := pumpMutation(t, h, first, nil)
			closing.ObservedEntityVersion = 1
			close(&closing)
			request := h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil)
			request.Mutations = []protocol.Mutation{closing, pumpMutation(t, h, second, nil)}
			response, err := applyAndPull(t, h.Service, h.Owner, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Outcomes) != 2 || response.Outcomes[0].Status != "applied" || response.Outcomes[1].Status != "applied" {
				t.Fatalf("outcomes %+v", response.Outcomes)
			}
			// A third open start now loses to the second.
			out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, pumpMutation(t, h, "b0000000-0000-4000-8000-000000000053", nil))
			if err != nil || out.Status != "superseded" || out.CanonicalEntity == nil || out.CanonicalEntity.Meta.ID != second {
				t.Fatalf("the running pump after the batch %+v %v", out, err)
			}
		})
	}
}

// A phone stops its session and enters the amount before the server has
// answered the stop: the amount's update still names the version before the
// stop. That is this phone's own next step, not a stale view, and it applies.
func TestAPhonesOwnAmountAfterItsOwnStopApplies(t *testing.T) {
	h := testkit.New(t)
	const id = "b0000000-0000-4000-8000-000000000061"
	if out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, pumpMutation(t, h, id, nil)); err != nil || out.Status != "applied" {
		t.Fatalf("start %+v %v", out, err)
	}
	end := "2026-09-20T14:30:00.000000Z"
	stop := pumpMutation(t, h, id, &end)
	p := stop.Payload.(protocol.PumpPayload)
	p.VolumeML = nil
	stop.Payload, stop.Operation, stop.ObservedEntityVersion = p, "update", 1
	amount := pumpMutation(t, h, id, &end)
	q := amount.Payload.(protocol.PumpPayload)
	q.VolumeML = protocol.MustParseDecimal("90")
	amount.Payload, amount.Operation, amount.ObservedEntityVersion = q, "update", 1
	request := h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil)
	request.Mutations = []protocol.Mutation{stop, amount}
	response, err := applyAndPull(t, h.Service, h.Caregiver, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Outcomes) != 2 || response.Outcomes[0].Status != "applied" || response.Outcomes[1].Status != "applied" {
		t.Fatalf("outcomes %+v", response.Outcomes)
	}
	got := response.Outcomes[1].CanonicalEntity.Payload.(protocol.PumpPayload)
	if got.VolumeML == nil || got.VolumeML.String() != "90" {
		t.Fatalf("the amount after the stop %+v", got)
	}
}
