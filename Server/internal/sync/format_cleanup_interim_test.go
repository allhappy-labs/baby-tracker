package sync_test

import (
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/migrate"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
	"babytracker/server/migrations"
)

// A phone that synced at schema 26 keeps its cursor across migration 27. Its
// next ordinary pull, through the same paging path the interim build uses,
// must deliver the past entries 27 converted and joined, carrying their
// session ids, as plain updates after that cursor. Production appends about
// 180 heads against a page of 200, so this family has more than a page of
// them and the pull must follow the page token.
func TestFormatCleanupPublishesPastSessionsToAnInterimCursor(t *testing.T) {
	h := testkit.NewThrough(t, 26)
	// This server writes the icon column 000029 adds. Lend it to the
	// schema-26 tables while seeding; it is dropped again before 000027 runs.
	if _, err := h.Migrator.Exec(t.Context(), `ALTER TABLE babies ADD COLUMN icon text`); err != nil {
		t.Fatal(err)
	}
	baby := testkit.MustMutation(t, "baby.json")
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil || out.Status != "applied" {
		t.Fatalf("seed baby %+v %v", out, err)
	}
	const (
		feedFirst  = "fc400000-0000-4000-8000-000000000001"
		feedJoined = "fc400000-0000-4000-8000-000000000002"
		feedApart  = "fc400000-0000-4000-8000-000000000003"
		sleepFirst = "fc400000-0000-4000-8000-000000000011"
		sleepNext  = "fc400000-0000-4000-8000-000000000012"
	)
	feed := func(id, started, ended string) string {
		return fmt.Sprintf(`{"mutation_id":"%s","entity_id":"%s","entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,`+
			`"client_mutation_time":"%s","import_id":null,"payload":{"baby_id":"%s","kind":"breast","started_at":"%s","ended_at":"%s",`+
			`"time_zone_id":"Europe/Zurich","note":null,"components":[{"id":"%s","kind":"breast","side":"left","seconds":600}],"feeding_session_id":null}}`,
			"ab"+id[2:], id, ended, baby.EntityID, started, ended, "cd"+id[2:])
	}
	sleep := func(id, started, ended string) string {
		return fmt.Sprintf(`{"mutation_id":"%s","entity_id":"%s","entity_kind":"sleep","operation":"create","payload_version":2,"observed_entity_version":0,`+
			`"client_mutation_time":"%s","import_id":null,"payload":{"baby_id":"%s","started_at":"%s","ended_at":"%s",`+
			`"time_zone_id":"Europe/Zurich","note":null,"kind_override":null,"sleep_session_id":null}}`,
			"ab"+id[2:], id, ended, baby.EntityID, started, ended)
	}
	// One past feed a day through 2025: each starts a session of its own.
	// Their ids sort after the three feeds above and before nothing else of
	// their kind, so their heads come between those feeds and the sleeps.
	const pastDays = 205
	seeds := []string{
		feed(feedFirst, "2026-09-20T08:00:00Z", "2026-09-20T08:10:00Z"),
		feed(feedJoined, "2026-09-20T08:25:00Z", "2026-09-20T08:35:00Z"),
		feed(feedApart, "2026-09-20T10:00:00Z", "2026-09-20T10:10:00Z"),
	}
	type wanted struct {
		kind, id, session string
		version           uint64
	}
	want := []wanted{{"feed", feedFirst, "", 2}, {"feed", feedJoined, feedFirst, 2}, {"feed", feedApart, "", 2}}
	firstDay := time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
	for day := range pastDays {
		id := fmt.Sprintf("fc410000-0000-4000-8000-%012d", day+1)
		started := firstDay.AddDate(0, 0, day)
		seeds = append(seeds, feed(id, started.Format(time.RFC3339), started.Add(10*time.Minute).Format(time.RFC3339)))
		want = append(want, wanted{"feed", id, "", 2})
	}
	seeds = append(seeds,
		sleep(sleepFirst, "2026-09-20T13:00:00Z", "2026-09-20T14:00:00Z"),
		sleep(sleepNext, "2026-09-20T14:10:00Z", "2026-09-20T15:00:00Z"))
	want = append(want, wanted{"sleep", sleepFirst, "", 2}, wanted{"sleep", sleepNext, sleepFirst, 2})
	for _, raw := range seeds {
		mutation, err := protocol.DecodeMutation([]byte(raw))
		if err != nil {
			t.Fatalf("%v\n%s", err, raw)
		}
		if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, mutation); err != nil || out.Status != "applied" {
			t.Fatalf("seed %s %+v %v", mutation.EntityID, out, err)
		}
	}
	pull := func(cursor *protocol.Cursor) ([]protocol.JournalChange, protocol.Cursor, int) {
		t.Helper()
		var changes []protocol.JournalChange
		page, err := h.Service.ApplyAndPull(t.Context(), h.Caregiver, h.SyncRequest(cursor, nil))
		for pages := 1; ; pages++ {
			if err != nil {
				t.Fatal(err)
			}
			changes = append(changes, page.Changes...)
			if page.NextPageToken == nil {
				return changes, page.HighWater, pages
			}
			page, err = h.Service.ApplyAndPull(t.Context(), h.Caregiver, h.SyncRequest(nil, page.NextPageToken))
		}
	}
	// The phone's cursor at schema 26. This server reads only the current
	// shapes, which migration 27 guarantees before it serves, so the pull runs
	// before the rows are put back in their schema-26 shape; that rewrite is in
	// place and moves no sequence.
	seen, cursor, _ := pull(nil)
	if cursor.Sequence != uint64(len(seeds))+1 || len(seen) != len(seeds)+1 {
		t.Fatalf("schema-26 pull high_water=%d changes=%d", cursor.Sequence, len(seen))
	}

	// Put them back in the shape a schema-26 server held for entries logged
	// before sessions existed: feed v2 and sleep v1, with no session key in
	// their records or their journal rows.
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE feed_entries SET component_version=2 WHERE family_id=$1`,
		`UPDATE sleep_entries SET payload_version=1 WHERE family_id=$1`,
		`UPDATE mutation_journal SET canonical_entity=canonical_entity #- '{payload,feeding_session_id}' WHERE family_id=$1 AND entity_kind='feed'`,
		`UPDATE mutation_journal SET canonical_entity=canonical_entity #- '{payload,sleep_session_id}' WHERE family_id=$1 AND entity_kind='sleep'`,
	} {
		if _, err := tx.Exec(t.Context(), statement, h.FamilyID); err != nil {
			t.Fatalf("%v\n%s", err, statement)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Migrator.Exec(t.Context(), `ALTER TABLE babies DROP COLUMN icon`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.New(testpg.Through(migrations.Files, 27)).Apply(t.Context(), h.Migrator); err != nil {
		t.Fatal(err)
	}

	changes, highWater, pages := pull(&cursor)
	if highWater.Sequence != cursor.Sequence+uint64(len(want)) || len(changes) != len(want) {
		t.Fatalf("pull after 27 high_water=%d changes=%d, want %d and %d", highWater.Sequence, len(changes), cursor.Sequence+uint64(len(want)), len(want))
	}
	if limit := h.Limits.SyncPageSize; len(want) <= limit || pages < 2 {
		t.Fatalf("pull after 27 took %d page(s) for %d heads; the case needs more than one page of %d", pages, len(want), limit)
	}
	for index, change := range changes {
		expected := want[index]
		if change.Cursor.Sequence != cursor.Sequence+uint64(index)+1 || change.EntityKind != expected.kind || change.EntityID != expected.id ||
			change.EntityVersion != expected.version || change.Operation != "update" || change.Optional ||
			change.ActorAccountID != h.Owner.AccountID || change.DeviceID != h.Owner.DeviceID {
			t.Errorf("change %d = seq %d %s %s v%d %s actor=%s device=%s", index, change.Cursor.Sequence, change.EntityKind, change.EntityID,
				change.EntityVersion, change.Operation, change.ActorAccountID, change.DeviceID)
		}
		var session *string
		switch payload := change.SnapshotOrTombstone.Payload.(type) {
		case protocol.FeedPayload:
			if len(payload.Components) != 1 {
				t.Errorf("change %d feed components=%d", index, len(payload.Components))
			}
			session = payload.FeedingSessionID
		case protocol.SleepPayload:
			session = payload.SleepSessionID
		default:
			t.Fatalf("change %d payload %T", index, payload)
		}
		got := ""
		if session != nil {
			got = *session
		}
		if got != expected.session {
			t.Errorf("change %d (%s) session=%q, want %q", index, expected.id, got, expected.session)
		}
	}

	// The cursor the phone now holds is current: nothing more to pull.
	if rest, again, _ := pull(&highWater); len(rest) != 0 || again.Sequence != highWater.Sequence {
		t.Errorf("second pull changes=%d high_water=%d", len(rest), again.Sequence)
	}
}
