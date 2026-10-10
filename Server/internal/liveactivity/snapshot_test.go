package liveactivity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// main's snapshot tests (main:Server/internal/liveactivity/snapshot_test.go)
// on care_entities: care rows are written through care.Encode and
// caretest.Write, acknowledged changes through the sync service, and every
// Read runs in a read-only repeatable-read transaction with the owner's
// tenant context, as authority.WithLiveActivitySnapshot supplies it.

const metaLayout = "2006-01-02T15:04:05.000000Z"

// seeder writes care rows into the harness family as the owner, each at the
// next family sequence.
type seeder struct {
	h   *testkit.Harness
	seq uint64
}

func newSeeder(t *testing.T, h *testkit.Harness) *seeder {
	t.Helper()
	return &seeder{h: h, seq: h.FamilySequence(t)}
}

// row is a new live entity of kind accepted at h.Now.
func (s *seeder) row(t *testing.T, kind, id string, payload protocol.Payload) care.Row {
	t.Helper()
	now := s.h.Now.UTC().Format(metaLayout)
	return s.encode(t, protocol.CanonicalEntity{EntityKind: kind, Payload: payload, Meta: protocol.EntityMeta{ID: id, FamilyID: s.h.FamilyID,
		EntityVersion: 1, CreatorAccountID: s.h.Owner.AccountID, LastEditorAccountID: s.h.Owner.AccountID, ClientMutationTime: now, AcceptedAt: now}})
}

// encode is entity's first version.
func (s *seeder) encode(t *testing.T, entity protocol.CanonicalEntity) care.Row {
	t.Helper()
	s.seq++
	row, err := care.Encode(entity, nil, care.Change{Seq: s.seq, ActorAccountID: s.h.Owner.AccountID, DeviceID: s.h.Owner.DeviceID, Operation: "create"})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// update is the next version of r with payload.
func (s *seeder) update(t *testing.T, r care.Row, payload protocol.Payload) care.Row {
	t.Helper()
	entity, err := r.Entity()
	if err != nil {
		t.Fatal(err)
	}
	s.seq++
	entity.Payload = payload
	entity.Meta.EntityVersion++
	row, err := care.Encode(entity, &r, care.Change{Seq: s.seq, ActorAccountID: s.h.Owner.AccountID, DeviceID: s.h.Owner.DeviceID, Operation: "update"})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// tombstone is the deletion of r.
func (s *seeder) tombstone(t *testing.T, r care.Row) care.Row {
	t.Helper()
	s.seq++
	return caretest.Owner(s.h).Tombstone(t, r, s.seq, nil)
}

// write commits rows, a thousand per transaction.
func (s *seeder) write(t *testing.T, rows ...care.Row) {
	t.Helper()
	for len(rows) > 0 {
		n := min(len(rows), 1000)
		if err := caretest.Write(t, s.h, rows[:n]...); err != nil {
			t.Fatal(err)
		}
		rows = rows[n:]
	}
}

// snapshotTx is the transaction Read requires: read-only, repeatable read,
// with the owner's tenant context.
func snapshotTx(t *testing.T, h *testkit.Harness) pgx.Tx {
	t.Helper()
	tx, err := h.Runtime.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(t.Context()) })
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	return tx
}

func ownerGrant(h *testkit.Harness) access.Grant {
	return access.Grant{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID, Role: "owner"}
}

func readSnapshot(t *testing.T, h *testkit.Harness, babyID string) CareSnapshot {
	t.Helper()
	tx := snapshotTx(t, h)
	defer func() { _ = tx.Rollback(t.Context()) }()
	s, err := (SnapshotReader{}).Read(t.Context(), tx, ownerGrant(h), babyID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// feedPayload is the feed of fixture for baby, with part IDs unique to n.
func feedPayload(t *testing.T, fixture, babyID string, n int) protocol.FeedPayload {
	t.Helper()
	p := testkit.MustMutation(t, fixture).Payload.(protocol.FeedPayload)
	p.BabyID = babyID
	components := append([]protocol.FeedComponent(nil), p.Components...)
	for j := range components {
		components[j].ID = fmt.Sprintf("3%07d-0000-4000-8000-%012d", n, j)
	}
	p.Components = components
	return p
}

// startFeed starts testkit.TimerA, a feed timer of baby, through the sync
// service as the owner.
func startFeed(t *testing.T, h *testkit.Harness, babyID string) protocol.Outcome {
	t.Helper()
	start := testkit.MustMutation(t, "timer-feed.json")
	start.MutationID, start.EntityID = h.CommandID(), testkit.TimerA
	timer := start.Payload.(protocol.TimerStartPayload)
	timer.BabyID = babyID
	start.Payload = timer
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("start feed: %+v %v", outcome, err)
	}
	return outcome
}

func at(h *testkit.Harness, offset time.Duration) string {
	return h.Now.Add(offset).UTC().Format(time.RFC3339Nano)
}

// queryPlan is what planIndexes reads from an executed plan.
type queryPlan struct {
	// indexes is the parent index of every index scanned (a partition's
	// index maps to the care_entities index it inherits).
	indexes map[string]bool
	// seq reports a sequential scan anywhere.
	seq bool
	// scanned is the most rows any scan node returned.
	scanned float64
}

// planIndexes explains (and runs) query in tx.
func planIndexes(t *testing.T, tx pgx.Tx, label, query string, args ...any) queryPlan {
	t.Helper()
	parents := map[string]string{}
	rows, err := tx.Query(t.Context(), `SELECT c.relname::text,p.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid JOIN pg_class p ON p.oid=i.inhparent WHERE p.relkind='I'`)
	if err != nil {
		t.Fatal(err)
	}
	var child, parent string
	if _, err := pgx.ForEachRow(rows, []any{&child, &parent}, func() error { parents[child] = parent; return nil }); err != nil {
		t.Fatal(err)
	}
	var plan json.RawMessage
	if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query, args...).Scan(&plan); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	t.Logf("%s query plan: %s", label, plan)
	var tree any
	if err := json.Unmarshal(plan, &tree); err != nil {
		t.Fatal(err)
	}
	result := queryPlan{indexes: map[string]bool{}}
	var walk func(any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if name, ok := v["Index Name"].(string); ok {
				if p, ok := parents[name]; ok {
					name = p
				}
				result.indexes[name] = true
			}
			if v["Node Type"] == "Seq Scan" {
				result.seq = true
			}
			if node, _ := v["Node Type"].(string); strings.HasSuffix(node, "Scan") {
				if rows, ok := v["Actual Rows"].(float64); ok {
					result.scanned = max(result.scanned, rows)
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(tree)
	return result
}

// expectIndex fails unless query scans one of indexes and nothing
// sequentially.
func expectIndex(t *testing.T, tx pgx.Tx, label, query string, indexes []string, args ...any) {
	t.Helper()
	plan := planIndexes(t, tx, label, query, args...)
	found := false
	for _, index := range indexes {
		found = found || plan.indexes[index]
	}
	if !found || plan.seq {
		t.Errorf("%s did not use bounded index %v (used %v, seq scan %t)", label, indexes, plan.indexes, plan.seq)
	}
}

// For every case of the shared projection fixture, the snapshot read from
// care_entities projects exactly the fixture's expected cards, and the same
// cards as the fixture's own in-memory snapshot (presentation_test.go): the
// reads pick the latest milk, its session and the latest breast feed, and
// sum a joined sleep, as main's legacy reads did. Each case is its own baby
// in one family, with the fixture's IDs moved to a case prefix (order within
// a case kept), its family and author the harness's.
func TestSnapshotMatchesTheSharedProjectionFixture(t *testing.T) {
	h := testkit.New(t)
	raw, err := os.ReadFile("../../api/fixtures/live-activity-v1/projection.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []json.RawMessage
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Fatalf("%d fixture cases", len(cases))
	}
	for i, c := range cases {
		text := strings.NewReplacer("10000000-0000-4000-8000-000000000002", h.FamilyID, "10000000-0000-4000-8000-000000000003", h.Owner.AccountID).Replace(string(c))
		text = strings.ReplaceAll(text, "10000000-0000-4000-8000-", fmt.Sprintf("6%07d-0000-4000-8000-", i+1))
		var f projectionFixture
		if err := json.Unmarshal([]byte(text), &f); err != nil {
			t.Fatal(err)
		}
		t.Run(f.Name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339Nano, f.Now)
			if err != nil {
				t.Fatal(err)
			}
			s := newSeeder(t, h)
			var live, deleted []care.Row
			entities := append([]protocol.CanonicalEntity{f.Baby}, f.Timers...)
			entities = append(append(entities, f.Feeds...), f.Sleeps...)
			for _, e := range entities {
				first := e
				first.Meta.TombstonedAt = nil
				if e.Payload == nil && e.EntityKind == "baby" {
					first.Payload = protocol.BabyPayload{Name: "Deleted", BirthDate: "2026-01-01"}
				}
				row := s.encode(t, first)
				live = append(live, row)
				if e.Meta.TombstonedAt != nil {
					deleted = append(deleted, row)
				}
			}
			s.write(t, live...)
			for _, row := range deleted {
				s.write(t, s.tombstone(t, row))
			}

			snapshot := readSnapshot(t, h, f.Baby.Meta.ID)
			if snapshot.Cursor != *h.Cursor(s.seq) || snapshot.Grant != ownerGrant(h) {
				t.Fatalf("snapshot cursor %+v grant %+v, want sequence %d", snapshot.Cursor, snapshot.Grant, s.seq)
			}
			if f.Baby.Meta.TombstonedAt != nil && (snapshot.Baby.Meta.TombstonedAt == nil || snapshot.Timers != nil || snapshot.LastMilk != nil) {
				t.Fatalf("a deleted baby's snapshot read on: %+v", snapshot)
			}
			if want := fixtureEarlierAsleep(t, f); snapshot.EarlierAsleepSeconds != want {
				t.Fatalf("earlier asleep %d, fixture rule %d", snapshot.EarlierAsleepSeconds, want)
			}
			cards, err := Project(snapshot, DevicePreferences{FeedBreakMinutes: f.Minutes}, now)
			if err != nil {
				t.Fatal(err)
			}
			got := []ContentState{}
			for _, card := range cards {
				got = append(got, card.Content)
				if !card.CanWrite || card.Sequence != s.seq || card.Scope.BabyID != f.Baby.Meta.ID || card.MayStart != (card.Phase == "running") {
					t.Fatalf("card identity %+v", card)
				}
			}
			if !reflect.DeepEqual(got, f.Expected) {
				t.Fatalf("got %+v want %+v", got, f.Expected)
			}
			memory := CareSnapshot{Baby: f.Baby, Timers: f.Timers, SessionMembers: f.Feeds, LastBreast: latestBreast(f.Feeds),
				EarlierAsleepSeconds: fixtureEarlierAsleep(t, f), Cursor: snapshot.Cursor, Grant: snapshot.Grant}
			want, err := Project(memory, DevicePreferences{FeedBreakMinutes: f.Minutes}, now)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cards, want) {
				t.Fatalf("stored cards %+v differ from the fixture snapshot's %+v", cards, want)
			}
		})
	}
}

// The latest milk skips solids, however many are newer; with no breast feed
// there is no last breast, and an older one is found past every newer feed.
func TestSnapshotSkipsSolidsAndPagesPastThem(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	s := newSeeder(t, h)
	const bottleID, breastID = "20000000-0000-4000-8000-000000000001", "20000000-0000-4000-8000-000000000002"
	bottle := feedPayload(t, "feed-bottle.json", babyID, 1)
	bottle.StartedAt = at(h, -2*time.Hour)
	rows := []care.Row{s.row(t, "feed", bottleID, bottle)}
	for i := 0; i < 40; i++ {
		solids := feedPayload(t, "feed-solids.json", babyID, 100+i)
		solids.StartedAt = at(h, time.Duration(i-100)*time.Minute)
		rows = append(rows, s.row(t, "feed", fmt.Sprintf("20000000-0000-4000-8000-%012d", 100+i), solids))
	}
	s.write(t, rows...)

	snapshot := readSnapshot(t, h, babyID)
	if snapshot.LastMilk == nil || snapshot.LastMilk.Meta.ID != bottleID || snapshot.LastBreast != nil {
		t.Fatalf("last milk %+v, last breast %+v", snapshot.LastMilk, snapshot.LastBreast)
	}
	if len(snapshot.SessionMembers) != 1 || snapshot.SessionMembers[0].Meta.ID != bottleID || snapshot.TotalSessionParts != 1 {
		t.Fatalf("session %d members, %d parts", len(snapshot.SessionMembers), snapshot.TotalSessionParts)
	}

	breast := feedPayload(t, "feed-breast.json", babyID, 2)
	breast.StartedAt, breast.EndedAt = at(h, -3*time.Hour), ptr(at(h, -170*time.Minute))
	breastRow := s.row(t, "feed", breastID, breast)
	s.write(t, breastRow)
	snapshot = readSnapshot(t, h, babyID)
	if snapshot.LastMilk == nil || snapshot.LastMilk.Meta.ID != bottleID || snapshot.LastBreast == nil || snapshot.LastBreast.Meta.ID != breastID {
		t.Fatalf("last milk %+v, last breast %+v", snapshot.LastMilk, snapshot.LastBreast)
	}

	// An edit changes a feed's kind (the upsert rewrites feed_kind): the
	// breast feed recorded as solids is no longer the latest breast feed, and
	// the bottle recorded as breast becomes it and stays the latest milk.
	asSolids := breast
	asSolids.Kind = "solids"
	bottleRow := rows[0]
	asBreast := bottle
	asBreast.Kind = "breast"
	s.write(t, s.update(t, breastRow, asSolids), s.update(t, bottleRow, asBreast))
	snapshot = readSnapshot(t, h, babyID)
	if snapshot.LastMilk == nil || snapshot.LastMilk.Meta.ID != bottleID || snapshot.LastBreast == nil || snapshot.LastBreast.Meta.ID != bottleID {
		t.Fatalf("after the kind edits: last milk %+v, last breast %+v", snapshot.LastMilk, snapshot.LastBreast)
	}
}

// countingTx counts the statements sent through it.
type countingTx struct {
	pgx.Tx
	statements int
}

func (c *countingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.statements++
	return c.Tx.Query(ctx, sql, args...)
}

func (c *countingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.statements++
	return c.Tx.QueryRow(ctx, sql, args...)
}

func (c *countingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.statements++
	return c.Tx.Exec(ctx, sql, args...)
}

// The latest milk and the latest breast feed are one index row each, as
// main's were, whatever the baby's feed history holds: a bottle-fed baby
// with ten thousand feeds and no breast feed, and a weaned child whose ten
// thousand newer feeds are all solids. Read is six statements (cursor, baby,
// timers, latest milk, its session, latest breast), and each latest read
// scans its own partial index for at most one row.
func TestSnapshotLatestFeedsReadOneIndexRow(t *testing.T) {
	h := testkit.New(t)
	babies := h.SeedBabies(t, 2)
	bottleFed, weaned := babies[0].CanonicalEntity.Meta.ID, babies[1].CanonicalEntity.Meta.ID
	s := newSeeder(t, h)
	var rows []care.Row
	for g := 1; g <= 10000; g++ {
		bottle := feedPayload(t, "feed-bottle.json", bottleFed, g)
		bottle.StartedAt = at(h, -time.Duration(g)*time.Hour)
		rows = append(rows, s.row(t, "feed", fmt.Sprintf("2b000000-0000-4000-8000-%012d", g), bottle))
		solids := feedPayload(t, "feed-solids.json", weaned, 20000+g)
		solids.StartedAt = at(h, -time.Duration(g)*time.Hour)
		rows = append(rows, s.row(t, "feed", fmt.Sprintf("2c000000-0000-4000-8000-%012d", g), solids))
	}
	const lastBottle, weanedBottle = "2b000000-0000-4000-8000-000000000001", "2d000000-0000-4000-8000-000000000001"
	old := feedPayload(t, "feed-bottle.json", weaned, 30001)
	old.StartedAt = at(h, -20000*time.Hour)
	rows = append(rows, s.row(t, "feed", weanedBottle, old))
	s.write(t, rows...)
	h.ExecFamily(t, `ANALYZE care_entities`)

	for _, c := range []struct{ name, baby, milk string }{{"bottle-fed", bottleFed, lastBottle}, {"weaned", weaned, weanedBottle}} {
		tx := &countingTx{Tx: snapshotTx(t, h)}
		snapshot, err := (SnapshotReader{}).Read(t.Context(), tx, ownerGrant(h), c.baby)
		if err != nil {
			t.Fatal(err)
		}
		if tx.statements != 6 || snapshot.LastMilk == nil || snapshot.LastMilk.Meta.ID != c.milk || snapshot.LastBreast != nil {
			t.Fatalf("%s: %d statements, last milk %+v, last breast %+v", c.name, tx.statements, snapshot.LastMilk, snapshot.LastBreast)
		}
		for _, q := range []struct {
			name, sql, index string
			rows             float64
		}{{"latest milk", latestMilkSQL, "care_entities_latest_milk_idx", 1}, {"latest breast", latestBreastSQL, "care_entities_latest_breast_idx", 0}} {
			plan := planIndexes(t, tx.Tx, c.name+" "+q.name, q.sql, h.FamilyID, c.baby)
			if !plan.indexes[q.index] || len(plan.indexes) != 1 || plan.seq || plan.scanned != q.rows {
				t.Errorf("%s %s: indexes %v, seq scan %t, %v rows scanned; want %s alone, %v rows", c.name, q.name, plan.indexes, plan.seq, plan.scanned, q.index, q.rows)
			}
		}
		_ = tx.Rollback(t.Context())
	}
}

// Equal times are ordered as main ordered them: the latest milk by end,
// then effective session, then id, all newest first; the latest breast by
// start, then id. The tied rows are written in the opposite order, so the
// heap never answers for the ORDER BY.
func TestSnapshotLatestFeedTieBreakers(t *testing.T) {
	h := testkit.New(t)
	babies := h.SeedBabies(t, 2)
	milkBaby, breastBaby := babies[0].CanonicalEntity.Meta.ID, babies[1].CanonicalEntity.Meta.ID
	s := newSeeder(t, h)
	const (
		rootLow    = "20000000-0000-4000-8000-0000000000a1" // its own session, the lowest
		linkedLow  = "20000000-0000-4000-8000-0000000000a2" // session b0, the lower id
		linkedHigh = "20000000-0000-4000-8000-0000000000a3" // session b0, the higher id: the latest milk
		session    = "20000000-0000-4000-8000-0000000000b0"
		solidsTop  = "20000000-0000-4000-8000-0000000000ff" // session ff, solids: skipped
		breastLow  = "20000000-0000-4000-8000-0000000000c1"
		breastHigh = "20000000-0000-4000-8000-0000000000c2" // the latest breast
	)
	ended := at(h, -time.Hour)
	milk := func(id, kind string, session *string, n int) care.Row {
		p := feedPayload(t, "feed-bottle.json", milkBaby, n)
		if kind == "solids" {
			p = feedPayload(t, "feed-solids.json", milkBaby, n)
		}
		p.StartedAt, p.EndedAt, p.FeedingSessionID = at(h, -2*time.Hour), &ended, session
		return s.row(t, "feed", id, p)
	}
	breast := func(id string, n int) care.Row {
		p := feedPayload(t, "feed-breast.json", breastBaby, n)
		p.StartedAt, p.EndedAt = at(h, -2*time.Hour), &ended
		return s.row(t, "feed", id, p)
	}
	s.write(t, milk(solidsTop, "solids", nil, 5), milk(linkedHigh, "bottle", ptr(session), 3), milk(linkedLow, "bottle", ptr(session), 2),
		milk(rootLow, "bottle", nil, 1), breast(breastHigh, 7), breast(breastLow, 6))

	if got := readSnapshot(t, h, milkBaby); got.LastMilk == nil || got.LastMilk.Meta.ID != linkedHigh || got.LastBreast != nil {
		t.Fatalf("latest milk %+v, latest breast %+v; want %s", got.LastMilk, got.LastBreast, linkedHigh)
	}
	if got := readSnapshot(t, h, breastBaby); got.LastBreast == nil || got.LastBreast.Meta.ID != breastHigh {
		t.Fatalf("latest breast %+v, want %s", got.LastBreast, breastHigh)
	}
}

// main's TestLiveActivitySnapshotBoundedHistory on care_entities: the latest
// session's members are its latest eight nonempty feeds, the part count is
// exact across that bound, and each read stays on its index under ten
// thousand aged sessions and the runtime's row-level security. The session
// also holds a bottle part, and the newest feed is solids in a session of
// its own, which the latest milk skips.
func TestLiveActivitySnapshotBoundedHistory(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	s := newSeeder(t, h)
	var rows []care.Row
	firstID := "20000000-0000-4000-8000-000000000100"
	for i := 0; i < 20; i++ {
		p := feedPayload(t, "feed-breast.json", babyID, 100+i)
		if i > 0 {
			p.FeedingSessionID = &firstID
		} // Legacy nil-ID root plus explicitly linked members.
		p.StartedAt = at(h, time.Duration(i-60)*time.Minute)
		p.EndedAt = ptr(at(h, time.Duration(i-59)*time.Minute))
		// Last breast component has a recognizable duration; every member has two
		// parts, so a row count would get omittedParts wrong.
		seconds := uint32(100 + i)
		p.Components[1].Seconds = &seconds
		rows = append(rows, s.row(t, "feed", fmt.Sprintf("20000000-0000-4000-8000-%012d", 100+i), p))
	}
	// Milk-kind records can legally contain solids only. They contribute no
	// visible parts and must not crowd the last-eight nonempty member window.
	var lastBreastID string
	for i := 0; i < 12; i++ {
		p := feedPayload(t, "feed-solids.json", babyID, 200+i)
		p.Kind = "breast"
		p.FeedingSessionID = &firstID
		p.StartedAt = at(h, time.Duration(i-30)*time.Minute)
		p.EndedAt = ptr(p.StartedAt)
		lastBreastID = fmt.Sprintf("20000000-0000-4000-8000-%012d", 200+i)
		rows = append(rows, s.row(t, "feed", lastBreastID, p))
	}
	// A bottle before the root: one more part, never in the last eight.
	bottle := feedPayload(t, "feed-bottle.json", babyID, 300)
	bottle.FeedingSessionID, bottle.StartedAt = &firstID, at(h, -61*time.Minute)
	rows = append(rows, s.row(t, "feed", "20000000-0000-4000-8000-000000000300", bottle))
	// The newest feed is solids, in its own session.
	solids := feedPayload(t, "feed-solids.json", babyID, 301)
	solids.StartedAt = at(h, -time.Minute)
	rows = append(rows, s.row(t, "feed", "20000000-0000-4000-8000-000000000301", solids))
	// A solids feed in the session holding a bottle part: main's members and
	// count skip solids feeds whatever their parts, so it is neither the
	// newest member nor counted.
	solidsWithBottle := feedPayload(t, "feed-solids.json", babyID, 302)
	solidsWithBottle.FeedingSessionID, solidsWithBottle.StartedAt = &firstID, at(h, -10*time.Minute)
	solidsWithBottle.Components = append(solidsWithBottle.Components, feedPayload(t, "feed-bottle.json", babyID, 303).Components[0])
	rows = append(rows, s.row(t, "feed", "20000000-0000-4000-8000-000000000302", solidsWithBottle))

	// Ten thousand aged, unrelated sessions. The same indexed paths must work
	// with accumulated history and real runtime RLS predicates enabled.
	for g := 1; g <= 10000; g++ {
		p := feedPayload(t, "feed-breast.json", babyID, 10000+g)
		p.Components = p.Components[:1]
		p.StartedAt = at(h, -365*24*time.Hour-time.Duration(g)*time.Minute)
		p.EndedAt = ptr(at(h, -365*24*time.Hour-time.Duration(g)*time.Minute+10*time.Minute))
		rows = append(rows, s.row(t, "feed", fmt.Sprintf("2a000000-0000-4000-8000-%012d", g), p))
	}
	s.write(t, rows...)

	// A real mutation after the synthetic history supplies the acknowledged
	// cursor; do not infer it from the queue or manufacture expected sequence.
	outcome := h.RenameBaby(t, babyID, "Snapshot baby")
	h.ExecFamily(t, `ANALYZE care_entities`)
	tx := snapshotTx(t, h)
	snapshot, err := (SnapshotReader{}).Read(t.Context(), tx, ownerGrant(h), babyID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor != *outcome.Cursor {
		t.Fatalf("snapshot cursor %+v differs from acknowledged %+v", snapshot.Cursor, *outcome.Cursor)
	}
	if len(snapshot.SessionMembers) != 8 || snapshot.TotalSessionParts != 41 || snapshot.LastMilk == nil || effectiveSession(*snapshot.LastMilk) != firstID {
		t.Fatalf("unbounded/wrong snapshot: members=%d parts=%d", len(snapshot.SessionMembers), snapshot.TotalSessionParts)
	}
	if snapshot.LastBreast == nil || snapshot.LastBreast.Meta.ID != lastBreastID {
		t.Fatalf("last breast %+v, want %s", snapshot.LastBreast, lastBreastID)
	}
	cards, err := Project(snapshot, DevicePreferences{120}, h.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].Content.OmittedParts != 33 || cards[0].Content.Parts[7].Seconds != 119 || *cards[0].SessionID != firstID {
		t.Fatalf("wrong summary %+v", cards)
	}
	for _, q := range []struct {
		name, sql string
		indexes   []string
		args      []any
	}{
		{"latest milk", latestMilkSQL, []string{"care_entities_latest_milk_idx"}, []any{h.FamilyID, babyID}},
		{"latest breast", latestBreastSQL, []string{"care_entities_latest_breast_idx"}, []any{h.FamilyID, babyID}},
		{"session members", sessionFeedsSQL, []string{"care_entities_session_idx"}, []any{h.FamilyID, babyID, firstID}},
		{"running timers", timersSQL, []string{"care_entities_one_live_timer", "care_entities_history_idx"}, []any{h.FamilyID, babyID}},
	} {
		expectIndex(t, tx, q.name, q.sql, q.indexes, q.args...)
	}
	_ = tx.Rollback(t.Context())

	// Running cards read only current timers, and preserve both active kinds.
	startFeed(t, h, babyID)
	snapshot = readSnapshot(t, h, babyID)
	if len(snapshot.Timers) != 1 || len(snapshot.SessionMembers) != 0 || snapshot.LastMilk != nil || snapshot.LastBreast != nil || snapshot.EarlierAsleepSeconds != 0 {
		t.Fatal("running snapshot fetched history")
	}
}

// main's TestLiveActivitySnapshotJoinedSleep on care_entities: a running
// sleep that continues an earlier sleep reads that sleep's completed parts —
// and only those — in the snapshot transaction, so the pushed card counts
// the whole sleep exactly as the phone's does (commit e736df5).
func TestLiveActivitySnapshotJoinedSleep(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	s := newSeeder(t, h)
	sleepAt := func(id string, start, end time.Duration, session *string) care.Row {
		p := testkit.MustMutation(t, "sleep.json").Payload.(protocol.SleepPayload)
		p.BabyID = babyID
		p.StartedAt = at(h, start)
		p.EndedAt = ptr(at(h, end))
		p.SleepSessionID = session
		return s.row(t, "sleep", id, p)
	}
	root := "30000000-0000-4000-8000-000000000301"
	// The sleep: 20 minutes, a 5-minute awake gap, then 900.7 seconds (900 whole).
	rows := []care.Row{
		sleepAt(root, -3*time.Hour, -160*time.Minute, nil),
		sleepAt("30000000-0000-4000-8000-000000000302", -155*time.Minute, -140*time.Minute+700*time.Millisecond, &root),
	}
	// A part the family deleted, and an earlier nap that is its own sleep.
	deleted := sleepAt("30000000-0000-4000-8000-000000000303", -138*time.Minute, -135*time.Minute, &root)
	rows = append(rows, deleted, sleepAt("30000000-0000-4000-8000-000000000304", -5*time.Hour, -270*time.Minute, nil))
	// Ten thousand aged, unrelated sleeps of the same baby: the session read
	// must stay on the session index under real RLS predicates.
	for g := 1; g <= 10000; g++ {
		rows = append(rows, sleepAt(fmt.Sprintf("3a000000-0000-4000-8000-%012d", g), -365*24*time.Hour-time.Duration(g)*time.Minute,
			-365*24*time.Hour-time.Duration(g)*time.Minute+30*time.Second, nil))
	}
	s.write(t, rows...)
	s.write(t, s.tombstone(t, deleted))
	h.ExecFamily(t, `ANALYZE care_entities`)

	// The running sleep continues the root's session; its start is the
	// acknowledged cursor the snapshot must report.
	start := testkit.MustMutation(t, "timer-sleep.json")
	start.MutationID, start.EntityID = h.CommandID(), testkit.TimerB
	timer := start.Payload.(protocol.TimerStartPayload)
	timer.BabyID = babyID
	timer.StartedAt = at(h, -10*time.Minute)
	timer.SessionID = &root
	start.Payload = timer
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("start joined sleep: %+v %v", outcome, err)
	}
	tx := snapshotTx(t, h)
	snapshot, err := (SnapshotReader{}).Read(t.Context(), tx, ownerGrant(h), babyID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor != *outcome.Cursor {
		t.Fatalf("snapshot cursor %+v differs from acknowledged %+v", snapshot.Cursor, *outcome.Cursor)
	}
	// 20 minutes plus 900 whole seconds: the deleted part, the earlier
	// separate nap and the 0.7 s remainder are not counted, as on the phone.
	if len(snapshot.Timers) != 1 || snapshot.EarlierAsleepSeconds != 2100 || snapshot.LastMilk != nil || len(snapshot.SessionMembers) != 0 {
		t.Fatalf("wrong joined-sleep snapshot: timers=%d asleep=%d", len(snapshot.Timers), snapshot.EarlierAsleepSeconds)
	}
	cards, err := Project(snapshot, DevicePreferences{120}, h.Now)
	if err != nil {
		t.Fatal(err)
	}
	wholeSleep := h.Now.Add(-10*time.Minute - 2100*time.Second)
	if len(cards) != 1 || cards[0].Content.ActivityKind != "sleep" || cards[0].Content.StartedAt != foundationTime(wholeSleep) || *cards[0].SessionID != root {
		t.Fatalf("card counts the part, not the sleep: %+v", cards)
	}
	expectIndex(t, tx, "earlier asleep", earlierAsleepSQL, []string{"care_entities_session_idx"}, h.FamilyID, babyID, root)
	expectIndex(t, tx, "running timers", timersSQL, []string{"care_entities_one_live_timer", "care_entities_history_idx"}, h.FamilyID, babyID)
}

// A running timer is served as main's timer tables rendered it: every
// timestamp at whole microseconds, as care.NormalizeTimerEntity renders the
// stored wire (which keeps the phone's form).
func TestSnapshotNormalizesRunningTimers(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	s := newSeeder(t, h)
	// The decoder normalizes the start itself; a running bottle's start is
	// decoded as sent.
	timer := protocol.TimerStartPayload{BabyID: babyID, TimerKind: "feed", StartedAt: "2026-09-21T11:00:00Z", TimeZoneID: "UTC",
		CurrentBottleStartedAt: ptr("2026-09-21T11:00:00.5Z"), CompletedComponents: []protocol.FeedComponent{}}
	row := s.row(t, "timer", testkit.TimerA, timer)
	s.write(t, row)
	stored, err := row.Entity()
	if err != nil {
		t.Fatal(err)
	}
	want, err := care.NormalizeTimerEntity(stored)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := readSnapshot(t, h, babyID)
	if len(snapshot.Timers) != 1 || !reflect.DeepEqual(snapshot.Timers[0], want) || reflect.DeepEqual(stored, want) {
		t.Fatalf("timer %+v, want %+v", snapshot.Timers, want)
	}
	if p := snapshot.Timers[0].Payload.(protocol.TimerStartPayload); p.CurrentBottleStartedAt == nil || *p.CurrentBottleStartedAt != "2026-09-21T11:00:00.500000Z" {
		t.Fatalf("timer payload %+v", p)
	}
}

// A missing baby and a deleted one read as no cards: the cursor and grant
// only, and a deleted baby's tombstone, as main's reader returned it.
func TestSnapshotOfAMissingOrDeletedBaby(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	startFeed(t, h, babyID)
	if running := readSnapshot(t, h, babyID); len(running.Timers) != 1 {
		t.Fatalf("running timers %+v", running.Timers)
	}
	missing := readSnapshot(t, h, "10000000-0000-4000-8000-0000000000ff")
	if missing.Cursor != h.FamilyHighWater(t) || missing.Baby.Meta.ID != "" || missing.Timers != nil {
		t.Fatalf("missing baby %+v", missing)
	}
	caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{caretest.DeleteBaby(babyID)})
	gone := readSnapshot(t, h, babyID)
	if gone.Baby.Meta.ID != babyID || gone.Baby.Meta.TombstonedAt == nil || gone.Timers != nil || gone.LastMilk != nil {
		t.Fatalf("deleted baby %+v", gone)
	}
	if cards, err := Project(gone, DevicePreferences{30}, h.Now); err != nil || len(cards) != 0 {
		t.Fatalf("deleted baby cards %+v %v", cards, err)
	}
}

// A timer's origin is its start: the owner starts it, the caregiver edits
// and stops it, and ReadTimerOrigin stays the owner's device at the start's
// sequence. The stop links the timer to its completion and that feed's
// session (ReadCompletedSession); a running or unknown timer has no link,
// and an unknown timer, or one written before 000032, no origin.
func TestTimerOriginIsTheStartingDevice(t *testing.T) {
	h := testkit.NewAtFixtureClock(t) // the edit's times are written against it
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	started := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{caretest.StartTimer(caretest.BabyA, testkit.TimerA, "feed")})
	startSeq := started.Outcomes[0].Cursor.Sequence
	edit := caretest.TimerCommand(testkit.TimerA, 2, "update", "2026-09-21T12:05:00.000000Z", 1,
		protocol.TimerEditPayload{BabyID: caretest.BabyA, StartedAt: ptr("2026-09-21T10:59:00.000000Z")})
	if r := caretest.ApplyCommitted(t, h, h.Caregiver, []protocol.Mutation{edit}); r.Outcomes[0].Status != "applied" {
		t.Fatalf("edit: %s %s", r.Outcomes[0].Status, caretest.Code(r.Outcomes[0]))
	}
	read := func() (TimerOrigin, *string, *string) {
		t.Helper()
		tx := snapshotTx(t, h)
		defer func() { _ = tx.Rollback(t.Context()) }()
		origin, err := ReadTimerOrigin(t.Context(), tx, h.FamilyID, testkit.TimerA)
		if err != nil {
			t.Fatal(err)
		}
		completed, session, err := ReadCompletedSession(t.Context(), tx, h.FamilyID, testkit.TimerA)
		if err != nil {
			t.Fatal(err)
		}
		return origin, completed, session
	}
	want := TimerOrigin{DeviceID: h.Owner.DeviceID, Sequence: startSeq}
	if origin, completed, session := read(); origin != want || completed != nil || session != nil || startSeq == 0 || h.Owner.DeviceID == h.Caregiver.DeviceID {
		t.Fatalf("running timer: origin %+v (want %+v), completion %v/%v", origin, want, completed, session)
	}

	stop := caretest.Stop(caretest.BabyA, testkit.TimerA, testkit.RecordA)
	stop.ObservedEntityVersion = 2
	if r := caretest.ApplyCommitted(t, h, h.Caregiver, []protocol.Mutation{stop}); r.Outcomes[0].Status != "applied" {
		t.Fatalf("stop: %s %s", r.Outcomes[0].Status, caretest.Code(r.Outcomes[0]))
	}
	if origin, completed, session := read(); origin != want || completed == nil || *completed != testkit.RecordA || session == nil || *session != testkit.RecordA {
		t.Fatalf("stopped timer: origin %+v (want %+v), completion %v/%v", origin, want, completed, session)
	}
	// The completion deleted: the link stays, the session goes.
	completion := caretest.Feed(caretest.BabyA, 0)
	completion.EntityID = testkit.RecordA
	if r := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{caretest.Delete(completion, h.CommandID(), 1)}); r.Outcomes[0].Status != "applied" {
		t.Fatalf("delete completion: %s %s", r.Outcomes[0].Status, caretest.Code(r.Outcomes[0]))
	}
	if origin, completed, session := read(); origin != want || completed == nil || *completed != testkit.RecordA || session != nil {
		t.Fatalf("deleted completion: origin %+v, completion %v/%v", origin, completed, session)
	}

	tx := snapshotTx(t, h)
	unknown, err := ReadTimerOrigin(t.Context(), tx, h.FamilyID, testkit.TimerB)
	if err != nil || unknown != (TimerOrigin{}) {
		t.Fatalf("unknown timer origin %+v %v", unknown, err)
	}
	completed, session, err := ReadCompletedSession(t.Context(), tx, h.FamilyID, testkit.TimerB)
	if err != nil || completed != nil || session != nil {
		t.Fatalf("unknown timer completion %v/%v %v", completed, session, err)
	}
	_ = tx.Rollback(t.Context())
	h.ExecFamily(t, `UPDATE care_entities SET created_device_id=NULL,created_seq=NULL WHERE family_id=$1 AND id=$2`, h.FamilyID, testkit.TimerA)
	if origin, _, _ := read(); origin != (TimerOrigin{}) {
		t.Fatalf("a timer written before 000032 has origin %+v", origin)
	}
}
