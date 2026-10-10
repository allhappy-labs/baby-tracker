package sync_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
)

type planNode struct {
	NodeType        string     `json:"Node Type"`
	RelationName    string     `json:"Relation Name"`
	IndexName       string     `json:"Index Name"`
	IndexCond       string     `json:"Index Cond"`
	RecheckCond     string     `json:"Recheck Cond"`
	Filter          string     `json:"Filter"`
	SubplansRemoved *int       `json:"Subplans Removed"`
	Plans           []planNode `json:"Plans"`
}

func (n planNode) walk(visit func(planNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}

// TestSyncStatementsUseOnePartitionAndIndexes explains the request-path
// statements sync sends itself (the two continuations and the snapshot
// admission count) as the runtime role under the family's RLS, with generic
// plans, over a family of 20,000 care rows with real bindings, live
// snapshot sessions and 2,000 expired ones: each care scan is pruned to one
// partition and reads its named index, and every key predicate is an Index
// Cond, not a Filter.
//
// The expired sessions' IDs sort above every v4 UUID, so the live snapshot
// session (the one every snapshot binding names) is always at rank 0 of
// sync_snapshot_sessions_pkey's id histogram: the adversarial case. There a
// plain join to the session prices a merge join over the pkey scanned from
// the family's first id (id only a Merge Cond); with random IDs that plan
// came up about one run in a hundred.
func TestSyncStatementsUseOnePartitionAndIndexes(t *testing.T) {
	h := testkit.New(t)
	ctx := t.Context()
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	seed := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,started_at,feed_kind,wire)
		 SELECT $1, gen_random_uuid(), (ARRAY[2,3,4,8])[1 + g%4]::smallint, $4::uuid, 1, 1 + g, $2, $2, $3, 'create', now(),
		   timestamptz '2026-10-01' - g * interval '10 minutes', CASE WHEN g % 4 = 0 THEN 2::smallint END, convert_to('{"x":"' || repeat('x',690) || '"}','UTF8')
		 FROM generate_series(1, 20000) g`, []any{h.FamilyID, h.Owner.AccountID, h.Owner.DeviceID, caretest.BabyA}},
		{`UPDATE families SET next_sequence=20001 WHERE id=$1`, []any{h.FamilyID}},
		{`INSERT INTO sync_snapshot_sessions(family_id,id,account_id,device_id,session_id,server_instance_id,recovery_epoch,security_revision,
		   high_water_sequence,captured_at,expires_at)
		 SELECT $1, ('ffffffff-0000-4000-8000-' || lpad(g::text, 12, '0'))::uuid, $2, $3, $4, $5, 1, 1, 1, timestamptz '2026-01-01' + g * interval '1 minute',
		   timestamptz '2026-01-01' + g * interval '1 minute' + interval '15 minutes'
		 FROM generate_series(1, 2000) g`, []any{h.FamilyID, h.Owner.AccountID, h.Owner.DeviceID, h.Owner.SessionID, h.Info.InstanceID}},
	} {
		if _, err := seed.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Real bindings, many pages deep: a pull and a snapshot of pages of 50.
	// (The seeded wire is placeholder JSON the service serves verbatim.)
	h.Limits.SyncPageSize = 50
	h.RebuildSyncService(t, h.Limits)
	pull, err := h.Service.ApplyAndPull(ctx, h.Owner, h.SyncRequest(h.Cursor(0), nil))
	for range 100 {
		if err != nil || pull.NextPageToken == nil {
			t.Fatalf("pull: %v", err)
		}
		pull, err = h.Service.ApplyAndPull(ctx, h.Owner, h.SyncRequest(nil, pull.NextPageToken))
	}
	snapshot, err := h.Service.FullSnapshot(ctx, h.Owner, protocol.SnapshotRequest{})
	for range 100 {
		if err != nil || snapshot.NextPageToken == nil {
			t.Fatalf("snapshot: %v", err)
		}
		snapshot, err = h.Service.FullSnapshot(ctx, h.Owner, protocol.SnapshotRequest{PageToken: snapshot.NextPageToken})
	}
	if _, err := h.Migrator.Exec(ctx, "ANALYZE public.care_entities; ANALYZE public.sync_page_bindings; ANALYZE public.sync_snapshot_sessions"); err != nil {
		t.Fatal(err)
	}
	parents := map[string]string{}
	rows, err := h.Migrator.Query(ctx, `SELECT c.relname::text, p.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid JOIN pg_class p ON p.oid=i.inhparent
		WHERE p.relkind='I' AND i.inhparent IN (SELECT indexrelid FROM pg_index WHERE indrelid='public.care_entities'::regclass)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			t.Fatal(err)
		}
		parents[child] = parent
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}

	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	defer func() { _ = tx.Rollback(ctx) }()
	digest := func(kind string) string {
		var hex string
		if err := tx.QueryRow(ctx, `SELECT encode(token_digest,'hex') FROM sync_page_bindings WHERE family_id=$1 AND kind=$2::text::app.page_kind
			ORDER BY coalesce(resume_sequence, 0) DESC, resume_id DESC LIMIT 1`, h.FamilyID, kind).Scan(&hex); err != nil {
			t.Fatal(err)
		}
		return "decode('" + hex + "','hex')"
	}
	family := "'" + h.FamilyID + "'::uuid"
	cases := []struct {
		name, args string
		care       string   // the care_entities index (one pruned partition scan), or ""
		others     []string // the other relations' indexes
		conds      []string
	}{
		{"sync_continuation", family + "," + digest("sync"), "care_entities_seq_key", []string{"sync_page_bindings/sync_page_bindings_pkey"},
			[]string{"family_id = $1", "token_digest = $2", "changed_seq > b.resume_sequence", "changed_seq <= b.high_water_sequence"}},
		{"snapshot_continuation", family + "," + digest("snapshot"), "care_entities_snapshot_idx",
			[]string{"sync_page_bindings/sync_page_bindings_pkey", "sync_snapshot_sessions/sync_snapshot_sessions_pkey"},
			[]string{"family_id = $1", "token_digest = $2", "ROW(kind, id) > ROW(b.resume_kind, b.resume_id)", "id = b.snapshot_id"}},
		{"snapshot_admission", "'" + h.Owner.AccountID + "'::uuid," + family + ",timestamptz '2026-09-21 12:00:00+00'", "",
			[]string{"sync_snapshot_sessions/sync_snapshot_sessions_account_idx", "sync_snapshot_sessions/sync_snapshot_sessions_family_idx"},
			[]string{"(account_id = $1) AND (expires_at > $3)", "(family_id = $2) AND (expires_at > $3)"}},
	}
	statements := familysync.Statements()
	if len(cases) != len(statements) {
		t.Fatalf("%d plan cases for %d statements", len(cases), len(statements))
	}
	for _, setting := range []string{"SET LOCAL plan_cache_mode = force_generic_plan", "SET LOCAL enable_seqscan = off"} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatal(err)
		}
	}
	partition := regexp.MustCompile(`^care_entities_p\d\d$`)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "PREPARE sync_"+c.name+" AS "+statements[c.name]); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := tx.QueryRow(ctx, fmt.Sprintf("EXPLAIN (FORMAT JSON) EXECUTE sync_%s(%s)", c.name, c.args)).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct{ Plan planNode }
			if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
				t.Fatalf("plan %s: %v", raw, err)
			}
			var careScans []planNode
			others := map[string]bool{}
			pruned := 0
			conds, filters := "", ""
			plans[0].Plan.walk(func(n planNode) {
				if n.NodeType == "Seq Scan" {
					t.Errorf("sequential scan on %s: %s", n.RelationName, raw)
				}
				switch {
				case partition.MatchString(n.RelationName):
					careScans = append(careScans, n)
				case n.RelationName != "" && n.IndexName != "":
					others[n.RelationName+"/"+n.IndexName] = true
				}
				if n.NodeType == "Bitmap Index Scan" {
					others["sync_snapshot_sessions/"+n.IndexName] = true
				}
				if n.SubplansRemoved != nil && *n.SubplansRemoved == 15 {
					pruned++
				}
				conds, filters = conds+" "+n.IndexCond+" "+n.RecheckCond, filters+" "+n.Filter
				if n.IndexName != "" {
					t.Logf("%s %s using %s: Index Cond %s, Filter %q", n.NodeType, n.RelationName, n.IndexName, n.IndexCond, n.Filter)
				}
			})
			if c.care != "" {
				if len(careScans) != 1 || pruned != 1 || parents[careScans[0].IndexName] != c.care {
					t.Fatalf("%d care scans, %d pruned to one partition, want one on %s: %s", len(careScans), pruned, c.care, raw)
				}
			} else if len(careScans) != 0 {
				t.Fatalf("care scans in %s", raw)
			}
			for _, other := range c.others {
				if !others[other] {
					t.Fatalf("plan does not read %s (reads %v): %s", other, others, raw)
				}
			}
			for _, cond := range c.conds {
				if !strings.Contains(conds, cond) || strings.Contains(filters, cond) {
					t.Fatalf("%q not an index condition: Index Cond %q, Filter %q: %s", cond, conds, filters, raw)
				}
			}
		})
	}
}
