package familyread

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/testkit"
)

// mustJSON is value as the MCP tools send it (json.Marshal).
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// sameBytes fails unless care's encoding is legacy's, byte for byte.
func sameBytes(t *testing.T, label string, legacy, care []byte) {
	t.Helper()
	if bytes.Equal(legacy, care) {
		return
	}
	i := 0
	for i < len(legacy) && i < len(care) && legacy[i] == care[i] {
		i++
	}
	excerpt := func(b []byte) string { return string(b[max(0, i-120):min(len(b), i+120)]) }
	t.Fatalf("%s: care differs from legacy at byte %d\nlegacy …%s…\n  care …%s…", label, i, excerpt(legacy), excerpt(care))
}

// TestFamilyReadMatchesLegacyCorpus runs the golden scenario through the
// care-backed services (the import step through the real importer) and
// reads it through the default reads: the recorded legacy familyread
// corpus (testdata/golden/legacy-familyread.json, never rewritten) is
// reproduced byte for byte, revisions and active timers included.
func TestFamilyReadMatchesLegacyCorpus(t *testing.T) {
	f := newGoldenFixture(t)
	f.h.RunGolden(t)
	f.credential = f.issue(t, grantA)
	encode := func(value any) json.RawMessage { return f.h.Normalize(mustJSON(t, value)) }
	babies, err := f.service.ListBabies(t.Context(), f.credential)
	if err != nil {
		t.Fatal(err)
	}
	query := HistoryQuery{BabyID: testkit.GoldenBabyA, Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		End: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), Limit: goldenHistoryLimit}
	first := f.history(t, query)
	if first.NextCursor == "" {
		t.Fatalf("history has one page: %d entries", len(first.Entries))
	}
	query.Cursor = first.NextCursor
	second := f.history(t, query)
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	summary, err := f.service.Summary(t.Context(), f.credential, SummaryQuery{BabyID: testkit.GoldenBabyA,
		Start: time.Date(2026, 9, 19, 0, 0, 0, 0, zurich), End: time.Date(2026, 9, 22, 0, 0, 0, 0, zurich), Timezone: "Europe/Zurich"})
	if err != nil {
		t.Fatal(err)
	}
	matchGolden(t, "testdata/golden/legacy-familyread.json", goldenFamilyRead{Babies: encode(babies),
		History: encode(first), HistoryPage: encode(second), Summary: encode(summary)})
}

// TestReadsRenderMillisecondTimersAndSessionRoots reads a family whose
// running timers' stored parts and bottle start carry millisecond times and
// whose bottle session lost its root and its earliest linked member (Phase
// 2 Task 7's legacy parity cases, kept care-only): running timers are
// rendered normalized as the legacy timer tables rendered them, and the
// session's earliest live member is the second linked feed.
func TestReadsRenderMillisecondTimersAndSessionRoots(t *testing.T) {
	f := newGoldenFixture(t)
	caretest.Seed(t, f.h, caretest.MillisecondFamily()...)
	f.credential = f.issue(t, grantA)
	ctx, credential := t.Context(), f.credential
	babies, err := f.service.ListBabies(ctx, credential)
	if err != nil || len(babies.Babies) != 2 {
		t.Fatalf("babies: %v %+v", err, babies)
	}
	from19 := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	sessionPage, err := f.service.History(ctx, credential, HistoryQuery{BabyID: caretest.BabyC, Start: from19, End: from19.Add(72 * time.Hour),
		Kinds: []string{"feed"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionPage.Entries) != 2 || sessionPage.Entries[0].ID != "2c000000-0000-4000-8000-000000000004" ||
		sessionPage.Entries[0].Session.ID != "2c000000-0000-4000-8000-000000000001" ||
		!sessionPage.Entries[0].Session.EarliestMemberStart.Equal(time.Date(2026, 9, 20, 7, 30, 0, 875000000, time.UTC)) {
		t.Fatalf("baby C feed sessions: %+v", sessionPage.Entries)
	}
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	summary, err := f.service.Summary(ctx, credential, SummaryQuery{BabyID: caretest.BabyC, Start: time.Date(2026, 9, 19, 0, 0, 0, 0, zurich),
		End: time.Date(2026, 9, 22, 0, 0, 0, 0, zurich), Timezone: "Europe/Zurich"})
	if err != nil || len(summary.ActiveTimers) != 2 {
		t.Fatalf("baby C timers: %v %+v", err, summary.ActiveTimers)
	}
	if raw := mustJSON(t, summary.ActiveTimers); !bytes.Contains(raw, []byte(`"current_bottle_started_at":"2026-09-21T11:00:00.456000Z"`)) ||
		!bytes.Contains(raw, []byte(`"ended_at":"2026-09-21T10:57:00.250000Z"`)) {
		t.Fatalf("baby C timers not normalized: %s", raw)
	}
	page, err := f.service.History(ctx, credential, HistoryQuery{BabyID: caretest.BabyD, Start: from19, End: from19.Add(72 * time.Hour)})
	if err != nil || !bytes.Contains(mustJSON(t, page), []byte(`"ended_at":"2026-09-21T11:20:00.789000Z"`)) {
		t.Fatalf("baby D timer not normalized: %v %s", err, mustJSON(t, page))
	}
	// Refusals: a record id that is not a baby.
	q := HistoryQuery{BabyID: "2c000000-0000-4000-8000-000000000004", Start: from19, End: from19.Add(24 * time.Hour)}
	if _, err := f.service.History(ctx, credential, q); !errors.Is(err, ErrBabyNotFound) {
		t.Fatalf("history of a feed id: %v", err)
	}
}

// An unknown history kind is an error, never a kind predicate no row
// matches (kinds are validated before any read, so this is a second line).
func TestCareCandidateSQLRefusesUnknownKind(t *testing.T) {
	if sql, err := careCandidateSQL([]string{"feed", "timer"}, nil); err == nil {
		t.Fatalf("unknown kind built %s", sql)
	}
	if _, err := careCandidateSQL(historyKinds[:], nil); err != nil {
		t.Fatal(err)
	}
}

// matchGolden compares got, encoded as testkit.AssertGolden encodes it, with
// the recorded file, which it never rewrites.
func matchGolden(t *testing.T, path string, got any) {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(got); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, path, want, buffer.Bytes())
}

// carePlanNode is one node of EXPLAIN (FORMAT JSON).
type carePlanNode struct {
	NodeType        string         `json:"Node Type"`
	RelationName    string         `json:"Relation Name"`
	IndexName       string         `json:"Index Name"`
	IndexCond       string         `json:"Index Cond"`
	Filter          string         `json:"Filter"`
	SubplansRemoved *int           `json:"Subplans Removed"`
	SharedHit       int            `json:"Shared Hit Blocks"`
	SharedRead      int            `json:"Shared Read Blocks"`
	Plans           []carePlanNode `json:"Plans"`
}

func (n carePlanNode) walk(visit func(carePlanNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}

// careIndexRule is what a scan through a care_entities index must look
// like: each key predicate an index condition, none of the listed ones
// checked by a filter instead.
type careIndexRule struct{ conds, unfiltered []string }

var careIndexRules = map[string]careIndexRule{
	"care_entities_history_idx": {conds: []string{`family_id = `, `baby_id = `, `kind = \d`, `started_at [<>]|[( ]id = ANY`},
		unfiltered: []string{`baby_id = `, `kind = \d`}},
	// The planner may read babies, or ids of one kind, by (kind, id).
	"care_entities_snapshot_idx": {conds: []string{`family_id = `, `kind = \d`}, unfiltered: []string{`kind = \d`, `[( ]id = `}},
	"care_entities_session_idx": {conds: []string{`family_id = `, `session_id = ANY`, `baby_id = `, `kind = \d`},
		unfiltered: []string{`session_id = ANY`, `baby_id = `, `kind = \d`}},
	"care_entities_pkey":           {conds: []string{`family_id = `, `[^_]id = `}, unfiltered: []string{`[( ]id = `}},
	"care_entities_one_live_timer": {conds: []string{`family_id = `, `baby_id = `}, unfiltered: []string{`baby_id = `}},
	"care_entities_baby_match_idx": {conds: []string{`family_id = `}},
}

var carePartition = regexp.MustCompile(`^care_entities_p\d\d$`)

// sqlLiteral renders a query argument as a text literal PREPARE's
// parameter types coerce.
func sqlLiteral(arg any) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	switch v := arg.(type) {
	case string:
		return quote(v)
	case int:
		return strconv.Itoa(v)
	case time.Time:
		return quote(v.UTC().Format(time.RFC3339Nano))
	case []string:
		return quote("{" + strings.Join(v, ",") + "}")
	case []time.Time:
		parts := make([]string, len(v))
		for i, at := range v {
			parts[i] = `"` + at.UTC().Format(time.RFC3339Nano) + `"`
		}
		return quote("{" + strings.Join(parts, ",") + "}")
	}
	panic(fmt.Sprintf("sqlLiteral %T", arg))
}

// careExplain runs EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) on sql as the
// runtime role under tenant RLS, once as the custom plan a parameterized
// statement gets and once as the generic plan a cached prepared statement
// may switch to, and checks each: no sequential scan; every care_entities
// scan in one partition and through an index, with each key predicate an
// index condition (careIndexRules); the indexes in want used, and an index
// condition kind = k for every k in kinds; at most maxBuffers shared
// buffers. A want entry "a|b" is met by either index.
func (l *largeHistory) careExplain(parents map[string]string, label, sql string, want []string, kinds []int16, maxBuffers int, args ...any) {
	t := l.t
	t.Helper()
	ctx := t.Context()
	run := func(mode, statement string, args ...any) {
		t.Helper()
		var raw string
		if err := l.tx.QueryRow(ctx, statement, args...).Scan(&raw); err != nil {
			t.Fatalf("%s (%s): %v", label, mode, err)
		}
		var plans []struct{ Plan carePlanNode }
		if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
			t.Fatalf("%s (%s): plan %s: %v", label, mode, raw, err)
		}
		root := plans[0].Plan
		partitions, used := map[string]bool{}, map[string]bool{}
		var historyConds, lines []string
		root.walk(func(n carePlanNode) {
			if n.NodeType == "Seq Scan" {
				t.Errorf("%s (%s): sequential scan on %s", label, mode, n.RelationName)
			}
			if carePartition.MatchString(n.RelationName) {
				partitions[n.RelationName] = true
				if n.IndexName == "" && n.NodeType != "Bitmap Heap Scan" {
					t.Errorf("%s (%s): %s on %s without an index", label, mode, n.NodeType, n.RelationName)
				}
			}
			if n.IndexName == "" {
				return
			}
			parent := parents[n.IndexName]
			lines = append(lines, fmt.Sprintf("%s %s using %s (%s): Index Cond %s Filter %q", n.NodeType, n.RelationName, n.IndexName, parent, n.IndexCond, n.Filter))
			rule, ok := careIndexRules[parent]
			if !ok {
				t.Errorf("%s (%s): unexpected index %s (%s)", label, mode, n.IndexName, parent)
				return
			}
			used[parent] = true
			for _, cond := range rule.conds {
				if !regexp.MustCompile(cond).MatchString(n.IndexCond) {
					t.Errorf("%s (%s): %s: %q is not an index condition: Index Cond %q Filter %q", label, mode, n.IndexName, cond, n.IndexCond, n.Filter)
				}
			}
			for _, cond := range rule.unfiltered {
				if regexp.MustCompile(cond).MatchString(n.Filter) {
					t.Errorf("%s (%s): %s filters %q: Index Cond %q Filter %q", label, mode, n.IndexName, cond, n.IndexCond, n.Filter)
				}
			}
			if parent == "care_entities_history_idx" {
				historyConds = append(historyConds, n.IndexCond)
			}
		})
		t.Logf("%s (%s): %d shared buffers, partitions %v\n  %s", label, mode, root.SharedHit+root.SharedRead, partitions, strings.Join(lines, "\n  "))
		if len(partitions) != 1 {
			t.Errorf("%s (%s): reads %d partitions %v, want one", label, mode, len(partitions), partitions)
		}
		for _, alternatives := range want {
			if !slices.ContainsFunc(strings.Split(alternatives, "|"), func(index string) bool { return used[index] }) {
				t.Errorf("%s (%s): does not use %s", label, mode, alternatives)
			}
		}
		for _, kind := range kinds {
			if !strings.Contains(strings.Join(historyConds, " "), fmt.Sprintf("kind = %d)", kind)) {
				t.Errorf("%s (%s): no history index scan with kind = %d: %v", label, mode, kind, historyConds)
			}
		}
		if buffers := root.SharedHit + root.SharedRead; buffers == 0 || buffers > maxBuffers {
			t.Errorf("%s (%s): %d shared buffers, want 1..%d", label, mode, buffers, maxBuffers)
		}
	}
	run("custom plan", `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+sql, args...)
	l.preparedPlans++
	name := fmt.Sprintf("care_plan_%d", l.preparedPlans)
	literals := make([]string, len(args))
	for i, arg := range args {
		literals[i] = sqlLiteral(arg)
	}
	for _, statement := range []string{`SET LOCAL plan_cache_mode = force_generic_plan`, `PREPARE ` + name + ` AS ` + sql} {
		if _, err := l.tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %s: %v", label, statement[:min(len(statement), 60)], err)
		}
	}
	run("generic plan", `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE `+name+`(`+strings.Join(literals, ",")+`)`)
	if _, err := l.tx.Exec(ctx, `SET LOCAL plan_cache_mode = auto`); err != nil {
		t.Fatal(err)
	}
}

// TestCareQueryPlansStayBounded explains every care read statement on the
// large history of seedLargeHistory (about 1.3 million rows over 11
// families, 5% tombstones, 300 stopped timers per baby, 700-byte wires):
// each reads one partition through the history, session, primary key,
// live-timer or baby-match index with every key predicate (family, baby,
// kind code, start range, session, id) an index condition, in custom and
// generic plans, with its shared buffers bounded.
func TestCareQueryPlansStayBounded(t *testing.T) {
	f := newFixture(t)
	large := seedLargeHistory(t, f)
	ctx, tx := t.Context(), large.tx
	parents := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT c.relname::text, p.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent WHERE p.relkind = 'I'`)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var live, minWire, maxWire int
	var heap string
	if err := tx.QueryRow(ctx, `SELECT count(*), min(octet_length(wire)), max(octet_length(wire)),
			pg_size_pretty(pg_relation_size(tableoid::regclass)) FROM care_entities WHERE family_id=$1 AND baby_id=$2 AND tombstoned_at IS NULL
			GROUP BY tableoid`, f.h.FamilyID, f.babyID).Scan(&live, &minWire, &maxWire, &heap); err != nil || live < 20000 ||
		minWire != careWireWidth || maxWire != careWireWidth {
		t.Fatalf("queried baby has %d live care rows, wire %d..%d bytes: %v", live, minWire, maxWire, err)
	}
	t.Logf("queried baby: %d live care rows, %d-byte wires; its partition's heap is %s", live, careWireWidth, heap)

	history := []int16{2, 3, 4, 6, 8}
	// A row by id: the primary key, or (kind, id) or (baby, kind) with the
	// ids as an index condition.
	const byID = "care_entities_pkey|care_entities_snapshot_idx|care_entities_history_idx"
	start := large.end.Add(-400 * 24 * time.Hour)
	q := historyQuery{babyID: f.babyID, start: start, end: start.Add(MaxRangeElapsed), kinds: historyKinds[:], limit: MaxHistoryLimit}
	firstPage, err := careCandidateSQL(q.kinds, nil)
	if err != nil {
		t.Fatal(err)
	}
	large.careExplain(parents, "first page, all kinds, 31 days, limit 100", firstPage,
		[]string{"care_entities_history_idx"}, history, 1000, candidateArgs(f.h.FamilyID, q, nil)...)
	after := &position{start: start.Add(10 * 24 * time.Hour), kind: "feed", id: "ffffffff-ffff-4fff-bfff-ffffffffffff"}
	continuation, err := careCandidateSQL(q.kinds, after)
	if err != nil {
		t.Fatal(err)
	}
	large.careExplain(parents, "continuation page, all kinds", continuation,
		[]string{"care_entities_history_idx"}, history, 1000, candidateArgs(f.h.FamilyID, q, after)...)

	sessionKeys := func(kind int16) []string {
		var keys []string
		if err := tx.QueryRow(ctx, `SELECT array_agg(coalesce(session_id, id)::text) FROM (SELECT session_id, id FROM care_entities
			WHERE family_id=$1 AND baby_id=$2 AND kind=$3 AND tombstoned_at IS NULL AND started_at >= $4 ORDER BY started_at, id LIMIT 100) page`,
			f.h.FamilyID, f.babyID, kind, start).Scan(&keys); err != nil {
			t.Fatal(err)
		}
		return keys
	}
	for _, kind := range []string{"feed", "sleep"} {
		large.careExplain(parents, "earliest member for 100 "+kind+" sessions", careSessionSQL(kind),
			[]string{"care_entities_session_idx", byID}, nil, 1000, f.h.FamilyID, f.babyID, sessionKeys(careHistoryKinds[kind]))
	}
	large.careExplain(parents, "running timers", careTimersSQL, []string{"care_entities_one_live_timer"}, nil, 100, f.h.FamilyID, f.babyID)
	large.careExplain(parents, "live babies", careBabiesSQL, []string{"care_entities_baby_match_idx|care_entities_snapshot_idx"}, nil, 100,
		f.h.FamilyID, MaxBabies+1)
	large.careExplain(parents, "live baby", careLiveBabySQL, []string{byID}, nil, 100, f.h.FamilyID, f.babyID)

	zone, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	end := start.Add(MaxRangeElapsed)
	days, err := localDays(start, end, zone)
	if err != nil {
		t.Fatal(err)
	}
	args := summaryArgs(f.h.FamilyID, f.babyID, start, end, days)
	bounds := args[4].([]time.Time)
	large.careExplain(parents, "summary aggregate, 744 hours", careSummaryAggregateSQL,
		[]string{"care_entities_history_idx", "care_entities_session_idx", byID}, history, maxSummaryPlanBuffers,
		args[0], args[1], args[2], args[3], bounds)
	large.careExplain(parents, "supplement doses by name", careSupplementsSQL, []string{"care_entities_history_idx"}, []int16{6}, 1000,
		f.h.FamilyID, f.babyID, start, end)
	large.careExplain(parents, "measurements", careMeasurementsSQL, []string{"care_entities_history_idx"}, []int16{8}, 1000,
		f.h.FamilyID, f.babyID, start, end, MaxSummaryMeasurements+1)
}
