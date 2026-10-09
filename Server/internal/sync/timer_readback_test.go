package sync_test

import (
	"fmt"
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// Timers, in the current payload version the iOS client sends, stored and
// then read back through every read path a phone uses: an incremental pull
// while the timer runs, a full pull after it stopped, and a snapshot.
//
// Their stored form carries `completed_components` and no `completed_sides`,
// and the journal/snapshot decoder once validated every stored payload as v1
// first — so the write committed and every later pull for the family failed
// with a retryable 503. Found by the two-caregiver iOS gate (protocol-e2e.sh).
func TestTimersReadBackThroughJournalAndSnapshot(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	babyID := baby.EntityID
	const feedTimer = "51000000-0000-4000-8000-000000000301"
	const sleepTimer = "51000000-0000-4000-8000-000000000302"
	const leftComponent = "31000000-0000-4000-8000-000000000301"
	const bottleComponent = "31000000-0000-4000-8000-000000000302"
	sequence := 900
	mutation := func(entity, operation string, observed int, payload string) protocol.Mutation {
		t.Helper()
		sequence++
		raw := fmt.Sprintf(`{"mutation_id":"01000000-0000-4000-8000-000000000%d","entity_id":"%s","entity_kind":"timer","operation":"%s",`+
			`"payload_version":5,"observed_entity_version":%d,"client_mutation_time":"2026-09-20T15:00:00.000Z","import_id":null,"payload":%s}`,
			sequence, entity, operation, observed, payload)
		decoded, err := protocol.DecodeMutation([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		return decoded
	}
	apply := func(m protocol.Mutation) {
		t.Helper()
		outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
		if err != nil || outcome.Status != "applied" {
			t.Fatalf("%s %s: outcome=%+v err=%v", m.Operation, m.EntityID, outcome, err)
		}
	}

	// The feed timer's completed components are folded directly into its
	// `timer_start`, the way the one-part-timer client actually reaches this
	// state — there is no separate attach-bottle command anymore.
	apply(mutation(feedTimer, "timer_start", 0, fmt.Sprintf(`{"baby_id":"%s","timer_kind":"feed","started_at":"2026-09-20T15:00:00.000Z",`+
		`"time_zone_id":"Europe/Zurich","completed_components":[{"id":"%s","kind":"breast","side":"left","seconds":240},`+
		`{"id":"%s","kind":"bottle","volume_ml":60,"contents":"formula"}],"current_side":null,"current_side_started_at":null,"current_bottle_started_at":null,"session_id":null}`,
		babyID, leftComponent, bottleComponent)))
	apply(mutation(sleepTimer, "timer_start", 0, fmt.Sprintf(`{"baby_id":"%s","timer_kind":"sleep","started_at":"2026-09-20T15:00:00.000Z",`+
		`"time_zone_id":"Europe/Zurich","completed_components":[],"session_id":null}`, babyID)))

	// Running: an incremental pull from the baby's cursor and a snapshot
	// both carry the live timers.
	afterBaby := h.Pull(t, h.Owner, nil, nil).HighWater
	afterBaby.Sequence = 1
	running := h.Pull(t, h.Owner, &afterBaby, nil)
	if len(running.Changes) != 2 {
		t.Fatalf("running changes=%d", len(running.Changes))
	}
	snapshot, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil {
		t.Fatalf("snapshot with live timers: %v", err)
	}
	live := map[string]protocol.TimerStartPayload{}
	for _, entity := range snapshot.Entities {
		if payload, ok := entity.Payload.(protocol.TimerStartPayload); ok && entity.Meta.TombstonedAt == nil {
			live[entity.Meta.ID] = payload
		}
	}
	if len(live) != 2 || len(live[feedTimer].CompletedComponents) != 2 || live[feedTimer].CompletedComponents[1].ID != bottleComponent {
		t.Fatalf("live timers=%+v", live)
	}

	// Stopped: the timers end and their records exist, and everything still
	// reads back from the very start of the journal.
	apply(mutation(feedTimer, "timer_stop", 1, fmt.Sprintf(`{"baby_id":"%s","completed_record_id":"21000000-0000-4000-8000-000000000301",`+
		`"stopped_at":"2026-09-20T15:10:00.000Z","note":null,"sleep_kind_override":null}`, babyID)))
	apply(mutation(sleepTimer, "timer_stop", 1, fmt.Sprintf(`{"baby_id":"%s","completed_record_id":"22000000-0000-4000-8000-000000000302",`+
		`"stopped_at":"2026-09-20T15:45:00.000Z","note":null,"sleep_kind_override":null}`, babyID)))
	full := h.Pull(t, h.Owner, nil, nil)
	kinds := map[string]int{}
	for _, change := range full.Changes {
		kinds[change.EntityKind]++
	}
	if kinds["feed"] != 1 || kinds["sleep"] != 1 || kinds["timer"] < 4 {
		t.Fatalf("full pull kinds=%v", kinds)
	}
	feed := h.ReadFeed(t, "21000000-0000-4000-8000-000000000301").Payload.(protocol.FeedPayload)
	if len(feed.Components) != 2 || feed.Components[0].ID != leftComponent || feed.Components[1].ID != bottleComponent {
		t.Fatalf("completed feed components=%+v", feed.Components)
	}
	if _, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{}); err != nil {
		t.Fatalf("snapshot after stops: %v", err)
	}
}
