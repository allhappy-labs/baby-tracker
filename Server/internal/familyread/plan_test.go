package familyread

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// largeHistory is a realistic large history seeded in a rolled-back
// superuser transaction (foreign keys off), switched to the runtime role
// under the fixture family's tenant RLS.
type largeHistory struct {
	t   *testing.T
	f   *fixture
	tx  pgx.Tx
	end time.Time
	// preparedPlans names careExplain's prepared statements.
	preparedPlans int
}

// careWireWidth is the placeholder wire's width in bytes, that of a real
// stored row: in the golden corpus live feeds are 704-832 bytes, sleeps
// 622-648, diapers 610, supplements 601-623, measurements 662, timers
// 672-1072 and tombstones 427-437.
const careWireWidth = 700

// seedLargeHistory writes a large history to care_entities as care.Encode
// would type it: three years for the queried baby, a twin in the same
// family, and 10 other families with one year each.
//   - About 5% of the records are tombstones.
//   - Every feed has one bottle part, as its derived bottle columns (none on
//     a tombstone); a measurement's measured_at is its start; a supplement's
//     name is its label.
//   - Every fourth feed or sleep of a baby, in start order, is linked to the
//     previous record's session.
//   - Every baby is a live baby row and has 300 stopped timers (kept forever
//     as tombstones) and one running timer of each kind.
//
// Rows carry a placeholder wire that plans never decode, as wide as a real
// one (careWireWidth), so heap pages hold as many rows as they would in
// production. The session links are computed before the insert, so the heap
// holds no dead row versions.
func seedLargeHistory(t *testing.T, f *fixture) *largeHistory {
	t.Helper()
	ctx := t.Context()
	// The admin pool is a superuser on the bootstrap database; open the
	// fixture's own database with the same superuser.
	config := f.h.Admin.Config().Copy()
	config.ConnConfig.Database = f.h.Runtime.Config().ConnConfig.Database
	config.MaxConns = 1
	superuser, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(superuser.Close)
	tx, err := superuser.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql[:min(len(sql), 80)], err)
		}
	}
	exec(`SET LOCAL session_replication_role = replica`)
	end := day0
	owner := f.h.Owner.AccountID
	families, babies, days := []string{f.h.FamilyID, f.h.FamilyID}, []string{f.babyID, "10000000-0000-4000-8000-0000000000aa"}, []int{3 * 365, 3 * 365}
	for i := 0; i < 10; i++ {
		families, babies, days = append(families, f.id(0xfa000000)), append(babies, f.id(0xba000000)), append(days, 365)
	}
	// The fixture's baby is already a care row (the sync service wrote it);
	// the seed replaces the family's rows, in the rolled-back transaction.
	exec(`DELETE FROM care_entities`)
	exec(`WITH targets AS (
		SELECT * FROM unnest($5::uuid[], $6::uuid[], $7::int[]) AS t(family_id, baby_id, days)
	), records AS MATERIALIZED (
		SELECT family_id, gen_random_uuid() AS id, kind, baby_id, CASE WHEN random() < 0.05 THEN now() END AS tombstoned_at,
			started_at, ended_at, label, round((random() * 200)::numeric, 1) AS volume
		FROM (
			SELECT t.family_id, t.baby_id, 2::smallint AS kind, $3::timestamptz - make_interval(mins => n * 120) AS started_at,
				$3::timestamptz - make_interval(mins => n * 120 - 15) AS ended_at, NULL::text AS label
			FROM targets t, generate_series(1, t.days * 12) n
			UNION ALL
			SELECT t.family_id, t.baby_id, 3, $3::timestamptz - make_interval(mins => n * 288), $3::timestamptz - make_interval(mins => n * 288 - 90), NULL
			FROM targets t, generate_series(1, t.days * 5) n
			UNION ALL
			SELECT t.family_id, t.baby_id, 4, $3::timestamptz - make_interval(mins => n * 180), NULL, NULL
			FROM targets t, generate_series(1, t.days * 8) n
			UNION ALL
			SELECT t.family_id, t.baby_id, 6, $3::timestamptz - make_interval(days => n), NULL, 'Vitamin D'
			FROM targets t, generate_series(1, t.days) n
			UNION ALL
			SELECT t.family_id, t.baby_id, 8, $3::timestamptz - make_interval(days => n * 7), NULL, NULL
			FROM targets t, generate_series(1, t.days / 7) n
		) generated
	), linked AS (
		SELECT r.*, CASE WHEN r.kind IN (2, 3) AND row_number() OVER w % 4 = 0 THEN lag(r.id) OVER w END AS session_id
		FROM records r WINDOW w AS (PARTITION BY r.family_id, r.baby_id, r.kind ORDER BY r.started_at)
	)
	INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,
		operation,accepted_at,tombstoned_at,started_at,ended_at,session_id,timer_kind,sort_order,label,name_hash,bottle_volume_ml,
		bottle_observations,feed_kind,wire)
	SELECT family_id, id, kind, baby_id, 1, row_number() OVER (PARTITION BY family_id ORDER BY kind, id), $1::uuid, $1::uuid, $2::uuid,
		(CASE WHEN kind = 5 THEN 'timer_start' ELSE 'create' END)::app.care_operation, now(), tombstoned_at, started_at, ended_at,
		session_id, timer_kind, sort_order, label, name_hash, volume, observations, CASE WHEN kind = 2 THEN 2::smallint END,
		convert_to(repeat('x', $4), 'UTF8')
	FROM (
		SELECT family_id, baby_id AS id, 1::smallint AS kind, NULL::uuid AS baby_id, NULL::timestamptz AS tombstoned_at,
			NULL::timestamptz AS started_at, NULL::timestamptz AS ended_at, NULL::uuid AS session_id, NULL::smallint AS timer_kind,
			0 AS sort_order, NULL::text AS label, sha256(convert_to(baby_id::text, 'UTF8')) AS name_hash, NULL::numeric AS volume,
			0::smallint AS observations
		FROM targets
		UNION ALL
		SELECT family_id, id, kind, baby_id, tombstoned_at, started_at, ended_at, session_id, NULL, NULL, label, NULL,
			CASE WHEN kind = 2 AND tombstoned_at IS NULL THEN volume END, CASE WHEN kind = 2 AND tombstoned_at IS NULL THEN 1 ELSE 0 END
		FROM linked
		UNION ALL
		SELECT t.family_id, gen_random_uuid(), 5, t.baby_id, CASE WHEN g <= 300 THEN now() END,
			$3::timestamptz - make_interval(hours => g), NULL, NULL, (1 + g % 2)::smallint, NULL, NULL, NULL, NULL, 0
		FROM targets t, generate_series(1, 302) g
	) rows`, owner, f.h.Owner.DeviceID, end, careWireWidth, families, babies, days)
	exec(`ANALYZE care_entities`)
	var sizes string
	if err := tx.QueryRow(ctx, `SELECT format('babies=%s feeds=%s (linked %s, bottle parts %s) sleeps=%s (linked %s) diapers=%s supplements=%s measurements=%s timers=%s (running %s) tombstones=%s',
		count(*) FILTER (WHERE kind = 1), count(*) FILTER (WHERE kind = 2), count(session_id) FILTER (WHERE kind = 2),
		sum(bottle_observations) FILTER (WHERE kind = 2), count(*) FILTER (WHERE kind = 3), count(session_id) FILTER (WHERE kind = 3),
		count(*) FILTER (WHERE kind = 4), count(*) FILTER (WHERE kind = 6), count(*) FILTER (WHERE kind = 8),
		count(*) FILTER (WHERE kind = 5), count(*) FILTER (WHERE kind = 5 AND tombstoned_at IS NULL), count(tombstoned_at)) FROM care_entities`).Scan(&sizes); err != nil {
		t.Fatal(err)
	}
	t.Logf("synthetic care rows: %s", sizes)
	exec(`SET LOCAL ROLE baby_tracker_runtime`)
	exec(`SELECT set_config('app.account_id',$1,true),set_config('app.family_id',$2,true)`, owner, f.h.FamilyID)
	return &largeHistory{t: t, f: f, tx: tx, end: end}
}

// maxSummaryPlanBuffers bounds the shared buffers of one 744-hour summary
// aggregate over the seeded busy baby (measured about 2,400 on PostgreSQL 17).
const maxSummaryPlanBuffers = 6000
