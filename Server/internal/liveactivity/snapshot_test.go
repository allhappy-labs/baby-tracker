package liveactivity

import (
	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"
)

func TestLiveActivitySnapshotBoundedHistory(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	var outcome protocol.Outcome
	firstID := ""
	for i := 0; i < 20; i++ {
		m := testkit.MustMutation(t, "feed-breast.json")
		m.MutationID = h.CommandID()
		m.EntityID = fmt.Sprintf("20000000-0000-4000-8000-%012d", 100+i)
		if i == 0 {
			firstID = m.EntityID
		}
		p := m.Payload.(protocol.FeedPayload)
		if i > 0 {
			p.FeedingSessionID = &firstID
		} // Legacy nil-ID root plus explicitly linked members.
		p.StartedAt = h.Now.Add(time.Duration(i-60) * time.Minute).UTC().Format(time.RFC3339Nano)
		p.EndedAt = ptr(h.Now.Add(time.Duration(i-59) * time.Minute).UTC().Format(time.RFC3339Nano))
		// Last breast component has a recognizable duration; every member has two
		// parts, so a row count would get omittedParts wrong.
		seconds := uint32(100 + i)
		p.Components[1].Seconds = &seconds
		m.Payload = p
		var err error
		outcome, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
		if err != nil || outcome.Status != "applied" {
			t.Fatalf("seed %+v %v", outcome, err)
		}
	}
	// Milk-kind records can legally contain solids only. They contribute no
	// visible parts and must not crowd the last-eight nonempty member window.
	for i := 0; i < 12; i++ {
		m := testkit.MustMutation(t, "feed-solids.json")
		m.MutationID = h.CommandID()
		m.EntityID = fmt.Sprintf("20000000-0000-4000-8000-%012d", 200+i)
		p := m.Payload.(protocol.FeedPayload)
		p.Kind = "breast"
		p.FeedingSessionID = &firstID
		p.StartedAt = h.Now.Add(time.Duration(i-30) * time.Minute).UTC().Format(time.RFC3339Nano)
		p.EndedAt = ptr(p.StartedAt)
		m.Payload = p
		out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
		if err != nil || out.Status != "applied" {
			t.Fatalf("empty visible member: %+v %v", out, err)
		}
	}

	// Ten thousand aged, unrelated sessions. The same indexed paths must work
	// with accumulated history and real runtime RLS predicates enabled.
	h.ExecFamily(t, `WITH seed AS (
 SELECT f.*,j.actor_account_id,j.device_id FROM feed_entries f JOIN mutation_journal j ON j.family_id=f.family_id AND j.sequence=f.last_sequence WHERE f.family_id=$1 AND f.id=$2
 ), advance AS (UPDATE families SET next_sequence=next_sequence+10000 WHERE id=$1 RETURNING next_sequence), generated AS (
 SELECT seed.*,g,md5('aged-live-activity-'||g)::uuid generated_id,advance.next_sequence-10000+g generated_sequence FROM seed CROSS JOIN advance CROSS JOIN generate_series(1,10000) g
 ), journal AS (
 INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
 SELECT family_id,generated_sequence,'feed',generated_id,1,actor_account_id,device_id,'create',jsonb_build_object('entity_kind','feed'),accepted_at FROM generated
 )
 INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,last_sequence,storage_bytes,kind,started_at,ended_at,time_zone_id)
 SELECT family_id,generated_id,baby_id,1,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,generated_sequence,1,'breast',started_at-interval '1 year'-g*interval '1 minute',ended_at-interval '1 year'-g*interval '1 minute',time_zone_id FROM generated`, h.FamilyID, firstID)
	h.ExecFamily(t, `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds)
 SELECT $1,md5('aged-live-activity-'||g)::uuid,md5('aged-component-'||g)::uuid,0,'breast','left',60 FROM generate_series(1,10000) g`, h.FamilyID)
	h.ExecFamily(t, `ANALYZE feed_components`)

	// A real mutation after the synthetic history supplies the acknowledged
	// cursor; do not infer it from the queue or manufacture expected sequence.
	outcome = h.RenameBaby(t, babyID, "Snapshot baby")
	h.ExecFamily(t, `ANALYZE feed_entries`)
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error {
		s, err := (SnapshotReader{}).Read(t.Context(), tx, g, babyID)
		if err != nil {
			return err
		}
		if s.Cursor != *outcome.Cursor {
			t.Fatalf("snapshot cursor %+v differs from acknowledged %+v", s.Cursor, *outcome.Cursor)
		}
		if len(s.SessionMembers) != 8 || s.TotalSessionParts != 40 || s.LastMilk == nil || effectiveSession(*s.LastMilk) != firstID {
			t.Fatalf("unbounded/wrong snapshot: members=%d parts=%d", len(s.SessionMembers), s.TotalSessionParts)
		}
		cards, err := Project(s, DevicePreferences{120}, h.Now)
		if err != nil {
			return err
		}
		if len(cards) != 1 || cards[0].Content.OmittedParts != 32 || cards[0].Content.Parts[7].Seconds != 119 || *cards[0].SessionID != firstID {
			t.Fatalf("wrong summary %+v", cards)
		}
		for _, q := range []struct {
			name, sql, index string
			args             []any
		}{
			{"latest milk", latestMilkSQL, "live_activity_latest_milk", []any{h.FamilyID, babyID}},
			{"latest breast", latestBreastSQL, "live_activity_latest_breast", []any{h.FamilyID, babyID}},
			{"session members", sessionMembersSQL, "live_activity_session_members", []any{h.FamilyID, babyID, firstID}},
			{"session count", sessionCountSQL, "live_activity_session_members", []any{h.FamilyID, babyID, firstID}},
		} {
			var plan json.RawMessage
			if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+q.sql, q.args...).Scan(&plan); err != nil {
				return err
			}
			t.Logf("%s query plan: %s", q.name, plan)
			if !strings.Contains(string(plan), q.index) || strings.Contains(string(plan), `"Node Type": "Seq Scan"`) {
				t.Errorf("%s did not use bounded index %s", q.name, q.index)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Running cards read only current timers, and preserve both active kinds.
	h.StartFeed(t, h.Owner, testkit.TimerA)
	err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error {
		s, err := (SnapshotReader{}).Read(t.Context(), tx, g, babyID)
		if err != nil {
			return err
		}
		if len(s.Timers) != 1 || len(s.SessionMembers) != 0 || s.LastMilk != nil || s.EarlierAsleepSeconds != 0 {
			t.Fatal("running snapshot fetched history")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A running sleep that continues an earlier sleep reads that sleep's completed
// parts — and only those — in the snapshot transaction, so the pushed card
// counts the whole sleep exactly as the phone's does (commit e736df5).
func TestLiveActivitySnapshotJoinedSleep(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	sleepAt := func(id string, start, end time.Duration, session *string) {
		m := testkit.MustMutation(t, "sleep.json")
		m.MutationID, m.EntityID = h.CommandID(), id
		p := m.Payload.(protocol.SleepPayload)
		p.StartedAt = h.Now.Add(start).UTC().Format(time.RFC3339Nano)
		p.EndedAt = ptr(h.Now.Add(end).UTC().Format(time.RFC3339Nano))
		p.SleepSessionID = session
		m.Payload = p
		out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
		if err != nil || out.Status != "applied" {
			t.Fatalf("seed sleep %s: %+v %v", id, out, err)
		}
	}
	root := "30000000-0000-4000-8000-000000000301"
	deleted := "30000000-0000-4000-8000-000000000303"
	// The sleep: 20 minutes, a 5-minute awake gap, then 900.7 seconds (900 whole).
	sleepAt(root, -3*time.Hour, -160*time.Minute, nil)
	sleepAt("30000000-0000-4000-8000-000000000302", -155*time.Minute, -140*time.Minute+700*time.Millisecond, &root)
	// A part the family deleted, and an earlier nap that is its own sleep.
	sleepAt(deleted, -138*time.Minute, -135*time.Minute, &root)
	sleepAt("30000000-0000-4000-8000-000000000304", -5*time.Hour, -270*time.Minute, nil)
	del := testkit.MustMutation(t, "sleep.json")
	del.MutationID, del.EntityID, del.Operation, del.ObservedEntityVersion = h.CommandID(), deleted, "delete", 1
	del.Payload = protocol.DeletePayload{BabyID: &babyID}
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, del); err != nil || out.Status != "applied" {
		t.Fatalf("delete part: %+v code=%v %v", out, out.Code, err)
	}

	// Ten thousand aged, unrelated sleeps of the same baby: the session read
	// must stay on the session index under real RLS predicates.
	h.ExecFamily(t, `WITH seed AS (
 SELECT s.*,j.actor_account_id,j.device_id FROM sleep_entries s JOIN mutation_journal j ON j.family_id=s.family_id AND j.sequence=s.last_sequence WHERE s.family_id=$1 AND s.id=$2
 ), advance AS (UPDATE families SET next_sequence=next_sequence+10000 WHERE id=$1 RETURNING next_sequence), generated AS (
 SELECT seed.*,g,md5('aged-sleep-'||g)::uuid generated_id,advance.next_sequence-10000+g generated_sequence FROM seed CROSS JOIN advance CROSS JOIN generate_series(1,10000) g
 ), journal AS (
 INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
 SELECT family_id,generated_sequence,'sleep',generated_id,1,actor_account_id,device_id,'create',jsonb_build_object('entity_kind','sleep'),accepted_at FROM generated
 )
 INSERT INTO sleep_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,last_sequence,storage_bytes,started_at,ended_at,time_zone_id)
 SELECT family_id,generated_id,baby_id,1,creator_account_id,last_editor_account_id,client_mutation_time,accepted_at,generated_sequence,1,started_at-interval '1 year'-g*interval '1 minute',ended_at-interval '1 year'-g*interval '1 minute',time_zone_id FROM generated`, h.FamilyID, root)
	h.ExecFamily(t, `ANALYZE sleep_entries`)

	// The running sleep continues the root's session; its start is the
	// acknowledged cursor the snapshot must report.
	start := testkit.MustMutation(t, "timer-sleep.json")
	start.MutationID, start.EntityID = h.CommandID(), testkit.TimerB
	timer := start.Payload.(protocol.TimerStartPayload)
	timer.StartedAt = h.Now.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	timer.SessionID = &root
	start.Payload = timer
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("start joined sleep: %+v %v", outcome, err)
	}
	err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, g access.Grant) error {
		s, err := (SnapshotReader{}).Read(t.Context(), tx, g, babyID)
		if err != nil {
			return err
		}
		if s.Cursor != *outcome.Cursor {
			t.Fatalf("snapshot cursor %+v differs from acknowledged %+v", s.Cursor, *outcome.Cursor)
		}
		// 20 minutes plus 900 whole seconds: the deleted part, the earlier
		// separate nap and the 0.7 s remainder are not counted, as on the phone.
		if len(s.Timers) != 1 || s.EarlierAsleepSeconds != 2100 || s.LastMilk != nil || len(s.SessionMembers) != 0 {
			t.Fatalf("wrong joined-sleep snapshot: timers=%d asleep=%d", len(s.Timers), s.EarlierAsleepSeconds)
		}
		cards, err := Project(s, DevicePreferences{120}, h.Now)
		if err != nil {
			return err
		}
		wholeSleep := h.Now.Add(-10*time.Minute - 2100*time.Second)
		if len(cards) != 1 || cards[0].Content.ActivityKind != "sleep" || cards[0].Content.StartedAt != foundationTime(wholeSleep) || *cards[0].SessionID != root {
			t.Fatalf("card counts the part, not the sleep: %+v", cards)
		}
		var plan json.RawMessage
		if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+earlierAsleepSQL, h.FamilyID, babyID, root).Scan(&plan); err != nil {
			return err
		}
		t.Logf("earlier asleep query plan: %s", plan)
		if !strings.Contains(string(plan), "sleep_entries_session") || strings.Contains(string(plan), `"Node Type": "Seq Scan"`) {
			t.Error("earlier asleep sum did not use bounded index sleep_entries_session")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
