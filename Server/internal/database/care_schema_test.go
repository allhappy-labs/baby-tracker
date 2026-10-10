package database_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
)

// Schema 30 adds the Phase 2 care storage beside the legacy care tables:
// care_entities, hash-partitioned by family into 16 partitions, each with
// fill factor 90 and its own forced RLS, and no runtime grant on a partition
// (the runtime reaches the rows through the parent only).
func TestCareSchemaIsVersionThirty(t *testing.T) {
	f := testpg.New(t)
	var v int64
	if err := f.Runtime.QueryRow(t.Context(), `SELECT current_version FROM schema_compatibility WHERE singleton`).Scan(&v); err != nil || v != 32 {
		t.Fatalf("version=%d err=%v", v, err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), f.Runtime, 32); err != nil {
		t.Fatal(err)
	}
	var parts int
	var strategy string
	if err := f.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_inherits WHERE inhparent='public.care_entities'::regclass`).Scan(&parts); err != nil {
		t.Fatal(err)
	}
	if err := f.Owner.QueryRow(t.Context(), `SELECT partstrat::text FROM pg_partitioned_table WHERE partrelid='public.care_entities'::regclass`).Scan(&strategy); err != nil {
		t.Fatal(err)
	}
	if parts != 16 || strategy != "h" {
		t.Fatalf("partitions=%d strategy=%s", parts, strategy)
	}
	var unfilled, unprotected, granted int
	if err := f.Owner.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE NOT coalesce(c.reloptions @> ARRAY['fillfactor=90'],false)),
		count(*) FILTER (WHERE NOT (c.relrowsecurity AND c.relforcerowsecurity)),
		count(*) FILTER (WHERE has_table_privilege('baby_tracker_runtime', c.oid, 'SELECT,INSERT,UPDATE,DELETE'))
		FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid WHERE i.inhparent='public.care_entities'::regclass`).Scan(&unfilled, &unprotected, &granted); err != nil {
		t.Fatal(err)
	}
	if unfilled+unprotected+granted != 0 {
		t.Fatalf("partitions: fillfactor %d, rls %d, runtime-granted %d", unfilled, unprotected, granted)
	}
	// Every other new table is forced-RLS too.
	var unprotectedTables []string
	rows, err := f.Owner.Query(t.Context(), `SELECT relname::text FROM pg_class
		WHERE oid IN ('public.care_entities'::regclass,'public.care_receipts'::regclass,'public.sync_page_bindings'::regclass,'public.sync_snapshot_sessions'::regclass)
		  AND NOT (relrowsecurity AND relforcerowsecurity) ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if unprotectedTables, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(unprotectedTables) != 0 {
		t.Fatalf("tables without forced RLS %v err=%v", unprotectedTables, err)
	}
}

// Every care_entities index serves a named request-path query (see the
// comments in 000030 and 000032); nothing else is indexed.
func TestCareSchemaIndexesAreNamedQueries(t *testing.T) {
	f := testpg.New(t)
	want := []string{"care_entities_baby_match_idx", "care_entities_history_idx", "care_entities_latest_breast_idx", "care_entities_latest_milk_idx", "care_entities_one_growth_profile", "care_entities_one_live_timer",
		"care_entities_pkey", "care_entities_seq_key", "care_entities_session_idx", "care_entities_snapshot_idx"}
	rows, err := f.Owner.Query(t.Context(), `SELECT indexrelid::regclass::text FROM pg_index WHERE indrelid='public.care_entities'::regclass ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("care_entities indexes %v, want %v (err=%v)", got, want, err)
	}
	// care_receipts: the receipt lookup (pkey), the retention sweep, and a
	// family_id-leading index for the families cascade and purge.
	rows, err = f.Owner.Query(t.Context(), `SELECT indexrelid::regclass::text || ':' || pg_get_indexdef(indexrelid, 1, true)
		FROM pg_index WHERE indrelid='public.care_receipts'::regclass ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if want := []string{"care_receipts_family_idx:family_id", "care_receipts_pkey:device_id", "care_receipts_retention_idx:accepted_at"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("care_receipts indexes %v, want %v (err=%v)", got, want, err)
	}
}

func TestCareSchemaRuntimeGrantsAreExact(t *testing.T) {
	f := testpg.New(t)
	for _, c := range []struct {
		sql  string
		want bool
	}{
		{`has_table_privilege(current_user,'public.care_entities','SELECT,INSERT')`, true},
		{`has_table_privilege(current_user,'public.care_entities','DELETE') OR has_table_privilege(current_user,'public.care_entities','TRUNCATE')`, false},
		{`has_column_privilege(current_user,'public.care_entities','wire','UPDATE') AND has_column_privilege(current_user,'public.care_entities','changed_seq','UPDATE') AND has_column_privilege(current_user,'public.care_entities','feed_kind','UPDATE')`, true},
		{`has_column_privilege(current_user,'public.care_entities','created_device_id','UPDATE') OR has_column_privilege(current_user,'public.care_entities','created_seq','UPDATE')`, false},
		{`has_column_privilege(current_user,'public.care_entities','kind','UPDATE') OR has_column_privilege(current_user,'public.care_entities','family_id','UPDATE') OR has_column_privilege(current_user,'public.care_entities','creator_account_id','UPDATE') OR has_column_privilege(current_user,'public.care_entities','timer_kind','UPDATE')`, false},
		{`has_table_privilege(current_user,'public.care_receipts','UPDATE') OR has_table_privilege(current_user,'public.care_receipts','DELETE')`, false},
		{`has_column_privilege(current_user,'public.families','care_charged_bytes','UPDATE') AND has_column_privilege(current_user,'public.families','care_wire_bytes','UPDATE')`, true},
	} {
		var got bool
		if err := f.Runtime.QueryRow(t.Context(), `SELECT `+c.sql).Scan(&got); err != nil || got != c.want {
			t.Fatalf("%s = %v (%v)", c.sql, got, err)
		}
	}
}

// The care write path updates the usage counters on the family row it has
// locked, in the tenant context: the four counters are outside the
// families_authority_columns trigger, so the tenant may write them, while the
// authority columns stay refused.
func TestCareSchemaFamilyCountersAreTenantWritable(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	var records, charged, entities, wire int64
	if err := tx.QueryRow(t.Context(), `UPDATE families SET next_sequence=next_sequence+2, care_record_count=care_record_count+1,
		care_charged_bytes=care_charged_bytes+300, care_entity_count=care_entity_count+2, care_wire_bytes=care_wire_bytes+400, updated_at=now()
		WHERE id=$1 RETURNING care_record_count, care_charged_bytes, care_entity_count, care_wire_bytes`, ids.familyA).Scan(&records, &charged, &entities, &wire); err != nil {
		t.Fatalf("tenant counter update: %v", err)
	}
	if records != 1 || charged != 300 || entities != 2 || wire != 400 {
		t.Fatalf("counters %d %d %d %d", records, charged, entities, wire)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE families SET care_charged_bytes=-1 WHERE id=$1`, ids.familyA); sqlState(err) != "23514" {
		t.Fatalf("negative charged bytes err=%v", err)
	}
	// Release the row lock before the next transaction reaches the same row.
	_ = tx.Rollback(t.Context())
	other := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if _, err := other.Exec(t.Context(), `UPDATE families SET care_wire_bytes=1, entitlement_required=true WHERE id=$1`, ids.familyA); sqlState(err) != "42501" {
		t.Fatalf("authority column beside a counter err=%v", err)
	}
}

// The care kind codes are fixed (000030 section 1): 1 baby, 2 feed, 3 sleep,
// 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement, in the
// legacy snapshot key order; timer kinds 1 feed, 2 sleep. The column comments
// state the mapping, and the kind-shape CHECK pins it: each record shape is
// accepted by exactly the codes of the kinds that may take that shape.
func TestCareSchemaKindCodesArePinned(t *testing.T) {
	f := testpg.New(t)
	const careKinds = "care kind: 1 baby, 2 feed, 3 sleep, 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement"
	for column, want := range map[string]string{
		"care_entities.kind": careKinds, "sync_page_bindings.resume_kind": careKinds, "care_entities.timer_kind": "timer kind: 1 feed, 2 sleep",
		"care_entities.feed_kind": "feed kind: 1 breast, 2 bottle, 3 solids",
	} {
		table, name, _ := strings.Cut(column, ".")
		var comment, typ string
		if err := f.Owner.QueryRow(t.Context(), `SELECT coalesce(col_description(a.attrelid, a.attnum),''), format_type(a.atttypid, a.atttypmod)
			FROM pg_attribute a WHERE a.attrelid=('public.'||$1)::regclass AND a.attname=$2`, table, name).Scan(&comment, &typ); err != nil {
			t.Fatal(err)
		}
		if comment != want || typ != "smallint" {
			t.Fatalf("%s: %s %q, want smallint %q", column, typ, comment, want)
		}
	}

	ids := seedAuthorityFamilies(t, f)
	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	const baby = "71000000-0000-4000-8000-000000000001"
	insert := `INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,
		started_at,ended_at,session_id,timer_kind,sort_order,label,name_hash,breast_seconds,shortest_part_seconds,feed_kind,wire)
		VALUES ($1,$2,$3,$4,1,$5,$6,$6,$7,'create',now(),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,convert_to(repeat('x',80),'UTF8'))`
	hash := make([]byte, 32)
	started := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	session := "72000000-0000-4000-8000-000000000001"
	type shape struct {
		baby               any
		started, ended     any
		session, timerKind any
		sortOrder, label   any
		nameHash           any
		breast             int32
		shortestPart       any
		// feedKind is the feed kind written; unset, a feed (code 2) gets
		// bottle and any other code none, unless noFeedKind.
		feedKind   any
		noFeedKind bool
	}
	if _, err := tx.Exec(t.Context(), insert, ids.familyA, baby, int16(1), nil, int64(1), ids.accountA, ids.deviceA, nil, nil, nil, nil, int32(0), nil, hash, int32(0), nil, nil); err != nil {
		t.Fatalf("baby row: %v", err)
	}
	for _, c := range []struct {
		name  string
		shape shape
		want  []int16
	}{
		{"baby", shape{sortOrder: int32(1), nameHash: hash}, []int16{1}},
		{"breast feed", shape{baby: baby, started: started, ended: ended, breast: 60, shortestPart: int32(60)}, []int16{2}},
		{"breast feed without a part", shape{baby: baby, started: started, ended: ended, breast: 60}, nil},
		{"ended with a part", shape{baby: baby, started: started, ended: ended, shortestPart: int32(30)}, []int16{2}},
		{"ended in a session", shape{baby: baby, started: started, ended: ended, session: session}, []int16{2, 3}},
		{"ended", shape{baby: baby, started: started, ended: ended}, []int16{2, 3, 4}},
		{"feed timer", shape{baby: baby, started: started, session: session, timerKind: int16(1)}, []int16{5}},
		{"sleep timer", shape{baby: baby, started: started, timerKind: int16(2)}, []int16{5}},
		{"feed timer with a part", shape{baby: baby, started: started, timerKind: int16(1), shortestPart: int32(30)}, []int16{5}},
		{"sleep timer with a part", shape{baby: baby, started: started, timerKind: int16(2), shortestPart: int32(30)}, nil},
		{"timer kind 3", shape{baby: baby, started: started, timerKind: int16(3)}, nil},
		{"labelled", shape{baby: baby, started: started, label: "Vitamin D"}, []int16{6}},
		{"undated", shape{baby: baby}, []int16{7}},
		{"dated", shape{baby: baby, started: started}, []int16{2, 3, 4, 8}},
		// 000032: a live feed has a feed kind (1..3); nothing else does.
		{"ended with a feed kind", shape{baby: baby, started: started, ended: ended, feedKind: int16(1)}, []int16{2}},
		{"ended with solids", shape{baby: baby, started: started, ended: ended, feedKind: int16(3)}, []int16{2}},
		{"feed kind 4", shape{baby: baby, started: started, ended: ended, feedKind: int16(4)}, nil},
		{"ended without a feed kind", shape{baby: baby, started: started, ended: ended, noFeedKind: true}, []int16{3, 4}},
	} {
		var accepted []int16
		for code := int16(0); code <= 9; code++ {
			sp, err := tx.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			feedKind := c.shape.feedKind
			if feedKind == nil && code == 2 && !c.shape.noFeedKind {
				feedKind = int16(2)
			}
			_, err = sp.Exec(t.Context(), insert, ids.familyA, "71000000-0000-4000-8000-000000000002", code, c.shape.baby, int64(2), ids.accountA, ids.deviceA,
				c.shape.started, c.shape.ended, c.shape.session, c.shape.timerKind, c.shape.sortOrder, c.shape.label, c.shape.nameHash, c.shape.breast, c.shape.shortestPart,
				feedKind)
			if err == nil {
				accepted = append(accepted, code)
			} else if state := sqlState(err); state != "23514" {
				t.Fatalf("%s as %d: %v", c.name, code, err)
			}
			if err := sp.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(accepted, c.want) {
			t.Errorf("%s accepted as kinds %v, want %v", c.name, accepted, c.want)
		}
	}
}

// Under forced RLS the runtime's generic prepared plans prune to one
// partition and keep every key predicate, kind included, as an index
// condition (not a Filter on top of a family-wide scan). This is why kind is
// a smallint: enum comparisons are not LEAKPROOF and would become Filters.
func TestCareSchemaPlansKeepIndexConditionsUnderRLS(t *testing.T) {
	f := testpg.New(t)
	ctx := t.Context()
	ids := seedAuthorityFamilies(t, f)
	babies := []string{"71000000-0000-4000-8000-000000000001", "71000000-0000-4000-8000-000000000002", "71000000-0000-4000-8000-000000000003", "71000000-0000-4000-8000-000000000004"}
	for _, family := range []struct{ account, family, device string }{{ids.accountA, ids.familyA, ids.deviceA}, {ids.accountB, ids.familyB, ids.deviceA}} {
		tx := tenantTx(t, f.Runtime, family.account, family.family)
		for _, step := range []string{
			`INSERT INTO care_entities(family_id,id,kind,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,sort_order,name_hash,wire)
			 SELECT $1, b, 1, 1, i, $2, $2, $3, 'create', now(), i, sha256(convert_to(b::text,'UTF8')), convert_to(repeat('x',80),'UTF8')
			 FROM unnest($4::uuid[]) WITH ORDINALITY AS u(b, i)`,
			`INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,started_at,ended_at,session_id,feed_kind,wire)
			 SELECT $1, gen_random_uuid(), k, ($4::uuid[])[1 + (g/4)%4], 1, 100 + g, $2, $2, $3, 'create', now(),
			   timestamptz '2026-10-01' - g * interval '10 minutes',
			   CASE WHEN k IN (2,3,4) THEN timestamptz '2026-10-01' - g * interval '10 minutes' + interval '5 minutes' END,
			   CASE WHEN k IN (2,3) AND g % 7 = 0 THEN gen_random_uuid() END, CASE WHEN k = 2 THEN 2::smallint END,
			   convert_to(repeat('x',80),'UTF8')
			 FROM generate_series(1, 4000) g, LATERAL (SELECT (ARRAY[2,3,4,8])[1 + g%4]::smallint AS k) x`,
			`INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,started_at,timer_kind,wire)
			 SELECT $1, gen_random_uuid(), 5, b, 1, 9000 + i, $2, $2, $3, 'timer_start', now(), timestamptz '2026-10-01', 1, convert_to(repeat('x',80),'UTF8')
			 FROM unnest($4::uuid[]) WITH ORDINALITY AS u(b, i)`,
		} {
			if _, err := tx.Exec(ctx, step, family.family, family.account, family.device, babies); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Owner.Exec(ctx, "ANALYZE public.care_entities"); err != nil {
		t.Fatal(err)
	}
	parents := map[string]string{}
	rows, err := f.Owner.Query(ctx, `SELECT c.relname::text, p.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid JOIN pg_class p ON p.oid=i.inhparent
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
	if rows.Err() != nil || len(parents) != 16*10 {
		t.Fatalf("partition indexes %d err=%v", len(parents), rows.Err())
	}

	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	for _, setting := range []string{"SET LOCAL plan_cache_mode = force_generic_plan", "SET LOCAL enable_seqscan = off", "SET LOCAL enable_bitmapscan = off"} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatal(err)
		}
	}
	family := "'" + ids.familyA + "'"
	for _, c := range []struct {
		name, prepare, execute, index string
		conds                         []string
	}{
		{"pull", `(uuid,bigint,bigint,int) AS SELECT id, changed_seq, wire FROM care_entities WHERE family_id=$1 AND changed_seq > $2 AND changed_seq <= $3 ORDER BY changed_seq LIMIT $4`,
			family + ",100,2000,500", "care_entities_seq_key", []string{"family_id = $1", "changed_seq > $2", "changed_seq <= $3"}},
		{"snapshot_page", `(uuid,smallint,uuid,int) AS SELECT kind, id, wire FROM care_entities WHERE family_id=$1 AND tombstoned_at IS NULL AND (kind, id) > ($2, $3) ORDER BY kind, id LIMIT $4`,
			family + ",2,'00000000-0000-0000-0000-000000000001',500", "care_entities_snapshot_idx", []string{"family_id = $1", "ROW(kind, id) > ROW($2, $3)"}},
		{"fetch", `(uuid,uuid[]) AS SELECT id, kind, entity_version FROM care_entities WHERE family_id=$1 AND id = ANY($2)`,
			family + ",ARRAY['" + babies[0] + "'::uuid]", "care_entities_pkey", []string{"family_id = $1", "id = ANY ($2)"}},
		{"history", `(uuid,uuid,smallint,timestamptz,int) AS SELECT id, started_at, ended_at FROM care_entities
			WHERE family_id=$1 AND baby_id=$2 AND kind=$3 AND tombstoned_at IS NULL AND started_at < $4 ORDER BY started_at DESC, id DESC LIMIT $5`,
			family + ",'" + babies[0] + "',2,'2026-10-01',50", "care_entities_history_idx", []string{"family_id = $1", "baby_id = $2", "kind = $3", "started_at < $4"}},
		{"session", `(uuid,uuid[]) AS SELECT id, baby_id, kind, started_at FROM care_entities
			WHERE family_id=$1 AND session_id = ANY($2) AND session_id IS NOT NULL AND tombstoned_at IS NULL`,
			family + ",ARRAY['72000000-0000-4000-8000-000000000001'::uuid]", "care_entities_session_idx", []string{"family_id = $1", "session_id = ANY ($2)"}},
		{"match", `(uuid,bytea) AS SELECT id FROM care_entities WHERE family_id=$1 AND kind = 1 AND tombstoned_at IS NULL AND name_hash = $2`,
			family + ",sha256(convert_to('" + babies[0] + "','UTF8'))", "care_entities_baby_match_idx", []string{"family_id = $1", "name_hash = $2"}},
		{"live_timer", `(uuid,uuid,smallint) AS SELECT id, entity_version FROM care_entities WHERE family_id=$1 AND kind = 5 AND tombstoned_at IS NULL AND baby_id = $2 AND timer_kind = $3`,
			family + ",'" + babies[0] + "',1", "care_entities_one_live_timer", []string{"family_id = $1", "baby_id = $2", "timer_kind = $3"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "PREPARE care_"+c.name+c.prepare); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := tx.QueryRow(ctx, fmt.Sprintf("EXPLAIN (FORMAT JSON) EXECUTE care_%s(%s)", c.name, c.execute)).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct{ Plan planNode }
			if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
				t.Fatalf("plan %s: %v", raw, err)
			}
			var scans []planNode
			removed := -1
			plans[0].Plan.walk(func(n planNode) {
				if n.RelationName != "" {
					scans = append(scans, n)
				}
				if n.SubplansRemoved != nil {
					removed = *n.SubplansRemoved
				}
			})
			if len(scans) != 1 || !regexp.MustCompile(`^care_entities_p\d\d$`).MatchString(scans[0].RelationName) || removed != 15 {
				t.Fatalf("not pruned to one partition (scans %d, subplans removed %d): %s", len(scans), removed, raw)
			}
			scan := scans[0]
			if parents[scan.IndexName] != c.index {
				t.Fatalf("%s scans %s (%s), want %s: %s", scan.NodeType, scan.IndexName, parents[scan.IndexName], c.index, raw)
			}
			for _, cond := range c.conds {
				if !strings.Contains(scan.IndexCond, cond) || strings.Contains(scan.Filter, cond) {
					t.Fatalf("%q not an index condition: Index Cond %q, Filter %q", cond, scan.IndexCond, scan.Filter)
				}
			}
		})
	}
}

type planNode struct {
	NodeType        string     `json:"Node Type"`
	RelationName    string     `json:"Relation Name"`
	IndexName       string     `json:"Index Name"`
	IndexCond       string     `json:"Index Cond"`
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

// The stored wire is the canonical entity: the mutation (up to the 64 KiB
// BT_SYNC_MUTATION_BYTES ceiling) plus entity meta, so the bound sits well
// above 64 KiB; 1 MiB is the hard second line.
func TestCareSchemaWireBoundSitsAboveTheMutationLimit(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	insert := `INSERT INTO care_entities(family_id,id,kind,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,sort_order,name_hash,wire)
		VALUES ($1,$2,1,1,$3,$4,$4,$5,'create',now(),0,decode(repeat('ab',32),'hex'),$6)`
	for _, c := range []struct {
		size  int
		state string
	}{{63, "23514"}, {64, ""}, {65536 + 512, ""}, {1 << 20, ""}, {1<<20 + 1, "23514"}} {
		tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
		_, err := tx.Exec(t.Context(), insert, ids.familyA, "71000000-0000-4000-8000-000000000001", int64(1), ids.accountA, ids.deviceA, []byte(strings.Repeat("x", c.size)))
		if sqlState(err) != c.state {
			t.Errorf("wire of %d bytes: %v, want %q", c.size, err, c.state)
		}
		_ = tx.Rollback(t.Context())
	}
}

// Snapshot session ids follow the file's zero-UUID convention.
func TestCareSchemaSnapshotSessionsRefuseZeroIDs(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	const zero = "00000000-0000-0000-0000-000000000000"
	insert := `INSERT INTO sync_snapshot_sessions(family_id,id,account_id,device_id,session_id,server_instance_id,recovery_epoch,security_revision,
		high_water_sequence,captured_at,expires_at) VALUES ($1,'73000000-0000-4000-8000-000000000001',$2,$3,$4,$5,1,1,0,now(),now()+interval '15 minutes')`
	valid := map[string]string{"account_id": ids.accountA, "device_id": ids.deviceA, "session_id": "74000000-0000-4000-8000-000000000001", "server_instance_id": "75000000-0000-4000-8000-000000000001"}
	for _, column := range []string{"", "account_id", "device_id", "session_id", "server_instance_id"} {
		values := map[string]string{}
		for k, v := range valid {
			values[k] = v
		}
		want := ""
		if column != "" {
			values[column], want = zero, "23514"
		}
		tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
		if column == "account_id" {
			// The policy admits only the context's account; give the context the
			// zero id so the CHECK, not the policy, is what refuses the row.
			if _, err := tx.Exec(t.Context(), `SELECT set_config('app.account_id',$1,true)`, zero); err != nil {
				t.Fatal(err)
			}
		}
		_, err := tx.Exec(t.Context(), insert, ids.familyA, values["account_id"], values["device_id"], values["session_id"], values["server_instance_id"])
		if sqlState(err) != want {
			t.Errorf("zero %q: %v, want %q", column, err, want)
		}
		_ = tx.Rollback(t.Context())
	}
}

// legacyCareTables are the 18 legacy care tables 000030 section 8 drops, and
// legacyCareFunctions the four helpers that served them.
var (
	legacyCareTables = []string{"babies", "feed_entries", "sleep_entries", "diaper_entries", "supplement_entries", "growth_profiles",
		"measurement_entries", "active_timers", "feed_components", "timer_components", "mutation_receipts", "mutation_journal",
		"family_record_usage", "sync_page_sessions", "sync_snapshots", "sync_snapshot_entities", "sync_snapshot_admissions", "delivery_pending"}
	legacyCareFunctions = []string{"schedule_family_wakes", "save_mutation_receipt", "sync_snapshot_admission_counts", "stage_sync_snapshot_admission"}
)

// Schema 30 is the care storage alone: the per-kind tables, the journal,
// the receipts, the usage table, the snapshot staging and the delivery queue
// are gone with their helper functions, and the current manifest names none of them.
func TestLegacyCareStorageIsGone(t *testing.T) {
	f := testpg.New(t)
	for _, table := range legacyCareTables {
		var present bool
		if err := f.Owner.QueryRow(t.Context(), `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if present {
			t.Errorf("legacy table %s survived", table)
		}
	}
	for _, function := range legacyCareFunctions {
		var n int
		if err := f.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'app' AND p.proname = $1`, function).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("legacy function app.%s survived", function)
		}
	}
	var listed []string
	rows, err := f.Owner.Query(t.Context(), `SELECT object_kind || ' ' || object_identity FROM schema_privilege_manifest
		WHERE schema_version = (SELECT current_version FROM schema_compatibility WHERE singleton) AND (object_identity ~ ('^public\.(' || $1 || ')(\.|$)') OR object_identity ~ ('^app\.(' || $2 || ')\(')) ORDER BY 1`,
		strings.Join(legacyCareTables, "|"), strings.Join(legacyCareFunctions, "|"))
	if err != nil {
		t.Fatal(err)
	}
	if listed, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(listed) != 0 {
		t.Fatalf("the current manifest still lists %v (err=%v)", listed, err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), f.Runtime, 32); err != nil {
		t.Fatal(err)
	}
}
