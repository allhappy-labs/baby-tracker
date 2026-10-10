package familyread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// The service's care data comes from care_entities (migration 000030): one
// row per entity, the typed columns for selection and the derived columns for
// the summary, the stored wire decoded for every entity it returns. Every read
// runs in the guard's read-only repeatable-read transaction and returns rows
// already checked against the invariants the service relies on (a broken one
// is ErrDataInvalid); the service owns validation of the query, paging,
// sizing and the cursor.
//
// Kinds are the smallint codes (care.Kind*) written as literals, so under
// forced RLS each key predicate stays an index condition. Every statement
// names family_id, so it reads one partition.

// careHistoryKinds is the care kind code of each history kind.
var careHistoryKinds = map[string]int16{
	"diaper": care.KindDiaper, "feed": care.KindFeed, "measurement": care.KindMeasurement,
	"sleep": care.KindSleep, "supplement": care.KindSupplement,
}

func kindPredicate(code int16) string { return fmt.Sprintf("kind=%d", code) }

// historyKindPredicate is the kind predicate of a history kind; any other
// kind is an error, never a predicate no row matches.
func historyKindPredicate(kind string) (string, error) {
	code, ok := careHistoryKinds[kind]
	if !ok {
		return "", fmt.Errorf("familyread: no care kind for history kind %q", kind)
	}
	return kindPredicate(code), nil
}

// mustHistoryKindPredicate is historyKindPredicate for the fixed kinds the
// package's statements name; an unknown one is a programming error.
func mustHistoryKindPredicate(kind string) string {
	predicate, err := historyKindPredicate(kind)
	if err != nil {
		panic(err)
	}
	return predicate
}

// careSelectionPredicates is selectionPredicates over care_entities: the
// kind's live rows of baby $2 in [$3,$4) by started_at (a measurement's
// measured_at), through care_entities_history_idx.
func careSelectionPredicates(kind string) ([]string, error) {
	predicate, err := historyKindPredicate(kind)
	if err != nil {
		return nil, err
	}
	where := []string{"family_id=$1", "baby_id=$2", predicate, "tombstoned_at IS NULL"}
	if kind == "sleep" {
		return append(where, "started_at > $3::timestamptz - interval '"+maxSleepInterval+"'", "started_at < $4",
			"(started_at >= $3 OR ended_at > $3)"), nil
	}
	return append(where, "started_at >= $3", "started_at < $4"), nil
}

// careSelection is careSelectionPredicates as one condition, for the
// package's fixed statements.
func careSelection(kind string) string {
	where, err := careSelectionPredicates(kind)
	if err != nil {
		panic(err)
	}
	return strings.Join(where, " AND ")
}

// careCandidateSQL is the history candidate page: one ascending range scan
// of care_entities_history_idx per kind, bounded by $5, after the (start,
// kind, id) keyset, in that order ($1 family, $2 baby, $3 and $4 the range,
// $6 and $7 the position).
func careCandidateSQL(kinds []string, after *position) (string, error) {
	branches := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		where, err := careSelectionPredicates(kind)
		if err != nil {
			return "", err
		}
		if after != nil {
			switch {
			case kind > after.kind:
				where = append(where, "started_at >= $6")
			case kind == after.kind:
				where = append(where, "(started_at, id) > ($6, $7::uuid)")
			default:
				where = append(where, "started_at > $6")
			}
		}
		branches = append(branches, fmt.Sprintf("(SELECT started_at AS event_start, '%s'::text AS kind, id FROM care_entities WHERE %s ORDER BY started_at, id LIMIT $5)",
			kind, strings.Join(where, " AND ")))
	}
	// page.id: the uuid column, not the text output.
	return `SELECT event_start, kind, id::text FROM (` + strings.Join(branches, " UNION ALL ") +
		`) page ORDER BY event_start, kind COLLATE "C", page.id LIMIT $5`, nil
}

// careSessionMembersSQL is the session members of kind (feed or sleep):
// the live rows of baby $2 linked to a key in keys
// (care_entities_session_idx), plus the unlinked root whose own id is a key
// (by id: the planner reads care_entities_snapshot_idx's (kind, id) or the
// history index's (baby, kind) with the ids as an index condition). kind is
// one of the fixed kinds the package names (an unknown one panics).
func careSessionMembersSQL(kind string, keys, columns string) string {
	k := mustHistoryKindPredicate(kind)
	return `
		SELECT session_id AS key,` + columns + ` FROM care_entities
		 WHERE family_id=$1 AND session_id IS NOT NULL AND session_id=ANY(` + keys + `) AND baby_id=$2 AND ` + k + ` AND tombstoned_at IS NULL
		UNION ALL
		SELECT id,` + columns + ` FROM care_entities
		 WHERE family_id=$1 AND id=ANY(` + keys + `) AND baby_id=$2 AND ` + k + ` AND tombstoned_at IS NULL AND session_id IS NULL
		`
}

// careSessionSQL is the earliest live member start of each session key of
// kind ($1 family, $2 baby, $3 the session keys; rows of key and start).
func careSessionSQL(kind string) string {
	return `SELECT key::text,min(started_at) FROM (` + careSessionMembersSQL(kind, "$3::uuid[]", "started_at") + `) members GROUP BY key`
}

var (
	// care_entities_baby_match_idx (its predicate is live babies), or
	// care_entities_snapshot_idx (kind = 1).
	careBabiesSQL = `SELECT id::text, wire FROM care_entities WHERE family_id=$1 AND ` + kindPredicate(care.KindBaby) +
		` AND tombstoned_at IS NULL ORDER BY sort_order, id LIMIT $2`
	// care_entities_snapshot_idx (kind, id), or the primary key.
	careLiveBabySQL = `SELECT tombstoned_at IS NULL FROM care_entities WHERE family_id=$1 AND id=$2 AND ` + kindPredicate(care.KindBaby)
	// care_entities_one_live_timer.
	careTimersSQL = `SELECT id::text, wire FROM care_entities WHERE family_id=$1 AND ` + kindPredicate(care.KindTimer) +
		` AND baby_id=$2 AND tombstoned_at IS NULL ORDER BY timer_kind, id LIMIT 3`
)

// careSummaryAggregateSQL is the summary's per-day totals ($1 family, $2
// baby, $3 and $4 the range, $5 the day bounds; one row per day). Bottle
// totals come from each selected feed's derived columns (bottle_observations,
// bottle_volume_ml, bottle_missing: the count, sum and missing count of its
// bottle parts' volumes), so no part is read.
var careSummaryAggregateSQL = `WITH
bounds AS (
  SELECT (ord - 1)::int AS day, at AS lo, lead(at) OVER (ORDER BY ord) AS hi
    FROM unnest($5::timestamptz[]) WITH ORDINALITY AS b(at, ord)
),
days AS (SELECT day, lo, hi FROM bounds WHERE hi IS NOT NULL),
feeds AS MATERIALIZED (
  SELECT id, started_at, session_id, bottle_observations, bottle_volume_ml, bottle_missing FROM care_entities WHERE ` + careSelection("feed") + `
),
feed_groups AS (
  SELECT min(started_at) AS first FROM (` + careSessionMembersSQL("feed",
	"ARRAY(SELECT DISTINCT coalesce(session_id, id) FROM feeds)", "started_at") + `) members GROUP BY key
),
sleeps AS MATERIALIZED (
  SELECT id, started_at, ended_at, session_id FROM care_entities WHERE ` + careSelection("sleep") + `
),
sleep_groups AS (
  SELECT min(started_at) AS first, bool_or(ended_at IS NOT NULL) AS completed FROM (` + careSessionMembersSQL("sleep",
	"ARRAY(SELECT DISTINCT coalesce(session_id, id) FROM sleeps WHERE started_at >= $3)", "started_at, ended_at") + `) members GROUP BY key
),
sleep_parts AS (
  SELECT d.day, s.id, greatest(s.started_at, d.lo) AS lo, least(s.ended_at, d.hi) AS hi
    FROM sleeps s JOIN days d ON s.started_at < d.hi AND s.ended_at > d.lo
   WHERE s.ended_at > s.started_at
),
sleep_reach AS (
  SELECT day, lo, hi, max(hi) OVER (PARTITION BY day ORDER BY lo, hi, id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS reach
    FROM sleep_parts
),
sleep_days AS (
  SELECT day, sum(greatest(0, ` + microseconds("hi") + ` - ` + microseconds("greatest(lo, coalesce(reach, lo))") + `)) AS micros
    FROM sleep_reach GROUP BY day
),
feed_days AS (
  SELECT width_bucket(started_at, $5::timestamptz[]) - 1 AS day, count(*) AS logged FROM feeds GROUP BY 1
),
feed_session_days AS (
  SELECT width_bucket(first, $5::timestamptz[]) - 1 AS day, count(*) AS sessions FROM feed_groups WHERE first >= $3 GROUP BY 1
),
bottle_days AS (
  SELECT width_bucket(started_at, $5::timestamptz[]) - 1 AS day, sum(bottle_observations)::bigint AS observations,
         (sum(bottle_volume_ml) * 1000000)::bigint AS micro_ml, sum(bottle_missing)::bigint AS missing
    FROM feeds WHERE bottle_observations + bottle_missing > 0 GROUP BY 1
),
sleep_session_days AS (
  SELECT width_bucket(first, $5::timestamptz[]) - 1 AS day, count(*) AS sessions FROM sleep_groups
   WHERE first >= $3 AND completed GROUP BY 1
),
unfinished_days AS (
  SELECT width_bucket(started_at, $5::timestamptz[]) - 1 AS day, count(*) AS unfinished FROM sleeps WHERE ended_at IS NULL GROUP BY 1
),
diaper_days AS (
  SELECT width_bucket(started_at, $5::timestamptz[]) - 1 AS day, count(*) AS n FROM care_entities WHERE ` + careSelection("diaper") + ` GROUP BY 1
),
supplement_days AS (
  SELECT width_bucket(started_at, $5::timestamptz[]) - 1 AS day, count(*) AS n FROM care_entities WHERE ` + careSelection("supplement") + ` GROUP BY 1
),
measurement_days AS (
  SELECT width_bucket(started_at, $5::timestamptz[]) - 1 AS day, count(*) AS n FROM care_entities WHERE ` + careSelection("measurement") + ` GROUP BY 1
)
SELECT d.day, coalesce(fs.sessions, 0), coalesce(fd.logged, 0), coalesce(b.observations, 0), b.micro_ml, coalesce(b.missing, 0),
       coalesce(ss.sessions, 0), coalesce(sd.micros, 0), coalesce(u.unfinished, 0),
       coalesce(dd.n, 0), coalesce(pd.n, 0), coalesce(md.n, 0)
  FROM days d
  LEFT JOIN feed_session_days fs ON fs.day = d.day
  LEFT JOIN feed_days fd ON fd.day = d.day
  LEFT JOIN bottle_days b ON b.day = d.day
  LEFT JOIN sleep_session_days ss ON ss.day = d.day
  LEFT JOIN sleep_days sd ON sd.day = d.day
  LEFT JOIN unfinished_days u ON u.day = d.day
  LEFT JOIN diaper_days dd ON dd.day = d.day
  LEFT JOIN supplement_days pd ON pd.day = d.day
  LEFT JOIN measurement_days md ON md.day = d.day
 ORDER BY d.day`

// careSupplementsSQL is the selected supplement dose count per name: label
// is the supplement name.
var careSupplementsSQL = `SELECT label, count(*) FROM care_entities WHERE ` + careSelection("supplement") +
	` GROUP BY label ORDER BY label COLLATE "C"`

// careMeasurementsSQL is the selected measurements with their stored wire.
var careMeasurementsSQL = `SELECT id::text, wire FROM care_entities WHERE ` + careSelection("measurement") +
	` ORDER BY started_at, id LIMIT $5`

// decodeWire is the stored entity kind id as the readers serve it: the wire
// decoded, and a live timer as the legacy readers rendered it from the timer
// tables (care.NormalizeTimerEntity: every timestamp to whole microseconds).
// A wire that does not decode to that entity is ErrDataInvalid.
func decodeWire(kind, id string, wire []byte) (protocol.CanonicalEntity, error) {
	var entity protocol.CanonicalEntity
	err := json.Unmarshal(wire, &entity)
	if err != nil || entity.EntityKind != kind || entity.Meta.ID != id {
		return protocol.CanonicalEntity{}, dataInvalid(kind, id)
	}
	if entity, err = care.NormalizeTimerEntity(entity); err != nil {
		return protocol.CanonicalEntity{}, dataInvalid(kind, id)
	}
	return entity, nil
}

// idWire is a selected row: its id and stored wire.
type idWire struct {
	id   string
	wire []byte
}

func selectWires(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]idWire, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (idWire, error) {
		var r idWire
		err := row.Scan(&r.id, &r.wire)
		return r, err
	})
}

// readBabies is the family's live babies in (sort order, id) order, at
// most limit.
func readBabies(ctx context.Context, tx pgx.Tx, familyID string, limit int) ([]protocol.CanonicalEntity, error) {
	rows, err := selectWires(ctx, tx, careBabiesSQL, familyID, limit)
	if err != nil {
		return nil, err
	}
	babies := make([]protocol.CanonicalEntity, 0, len(rows))
	for _, r := range rows {
		entity, err := decodeWire("baby", r.id, r.wire)
		if err != nil {
			return nil, err
		}
		if _, ok := entity.Payload.(protocol.BabyPayload); !ok || entity.Meta.TombstonedAt != nil {
			return nil, dataInvalid("baby", r.id)
		}
		babies = append(babies, entity)
	}
	return babies, nil
}

// requireLiveBaby fails with ErrBabyNotFound unless babyID is a live baby
// of the family.
func requireLiveBaby(ctx context.Context, tx pgx.Tx, familyID, babyID string) error {
	var live bool
	err := tx.QueryRow(ctx, careLiveBabySQL, familyID, babyID).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
		return ErrBabyNotFound
	}
	return err
}

// readCandidates is at most q.limit+1 history keys after the position, in
// canonical order (HistorySelection).
func readCandidates(ctx context.Context, tx pgx.Tx, familyID string, q historyQuery, after *position) ([]position, error) {
	sql, err := careCandidateSQL(q.kinds, after)
	if err != nil {
		return nil, err
	}
	return selectCandidates(ctx, tx, sql, candidateArgs(familyID, q, after))
}

// readEntries reads the root records of picked, in that order, with one
// care.GetMany.
func readEntries(ctx context.Context, tx pgx.Tx, familyID, babyID string, picked []position) ([]HistoryEntry, error) {
	ids := make([]string, len(picked))
	for i, candidate := range picked {
		ids[i] = candidate.id
	}
	rows, err := care.GetMany(ctx, tx, familyID, ids)
	if err != nil {
		return nil, err
	}
	entries := make([]HistoryEntry, 0, len(picked))
	for _, candidate := range picked {
		row, ok := rows[candidate.id]
		if !ok {
			return nil, dataInvalid(candidate.kind, candidate.id)
		}
		entity, err := decodeWire(candidate.kind, candidate.id, row.Wire)
		if err != nil {
			return nil, err
		}
		entry, err := historyEntry(candidate, babyID, entity)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// readTimers is the baby's running timers (at most one per kind) in timer
// kind order.
func readTimers(ctx context.Context, tx pgx.Tx, familyID, babyID string) ([]ActiveTimer, error) {
	rows, err := selectWires(ctx, tx, careTimersSQL, familyID, babyID)
	if err != nil {
		return nil, err
	}
	if len(rows) > 2 {
		return nil, dataInvalid("timer", babyID)
	}
	timers := make([]ActiveTimer, 0, len(rows))
	for _, r := range rows {
		entity, err := decodeWire("timer", r.id, r.wire)
		if err != nil {
			return nil, err
		}
		timer, err := activeTimer(r.id, babyID, entity)
		if err != nil {
			return nil, err
		}
		timers = append(timers, timer)
	}
	return timers, nil
}

// readMeasurements is the selected measurements in (measured_at, id) order;
// more than limit is ErrResultTooLarge.
func readMeasurements(ctx context.Context, tx pgx.Tx, familyID string, q summaryQuery, limit int) ([]MeasurementObservation, error) {
	rows, err := selectWires(ctx, tx, careMeasurementsSQL, familyID, q.babyID, q.start, q.end, limit+1)
	if err != nil {
		return nil, err
	}
	if len(rows) > limit {
		return nil, ErrResultTooLarge
	}
	observations := make([]MeasurementObservation, 0, len(rows))
	for _, r := range rows {
		entity, err := decodeWire("measurement", r.id, r.wire)
		if err != nil {
			return nil, err
		}
		observation, err := measurementObservation(r.id, q, entity)
		if err != nil {
			return nil, err
		}
		observations = append(observations, observation)
	}
	return observations, nil
}
