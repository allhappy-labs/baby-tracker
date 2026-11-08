package care_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	storeBaby    = "11000000-0000-4000-8000-000000000001"
	storeBabyTwo = "11000000-0000-4000-8000-000000000002"
)

// sameRow reports whether a stored row reads back as the row written.
func sameRow(a, b care.Row) bool {
	volume := func(r care.Row) string {
		if r.Derived.BottleVolumeML == nil {
			return "null"
		}
		return r.Derived.BottleVolumeML.String()
	}
	if volume(a) != volume(b) {
		return false
	}
	a.Derived.BottleVolumeML, b.Derived.BottleVolumeML = nil, nil
	return reflect.DeepEqual(a, b)
}

func wantSameRow(t *testing.T, got, want care.Row) {
	t.Helper()
	if !sameRow(got, want) {
		t.Fatalf("row %s read back\n got %+v\nwant %+v", want.ID, got, want)
	}
}

// familyState is the family row as the counters should hold it after rows
// were written from an empty family at sequence 0.
func familyState(sequence uint64, rows ...care.Row) care.FamilyState {
	state := care.FamilyState{Sequence: sequence}
	for _, r := range rows {
		if r.CountsAsRecord() {
			state.RecordCount++
		}
		state.ChargedBytes += uint64(r.ChargedBytes())
		state.EntityCount++
		state.WireBytes += uint64(r.WireBytes)
	}
	return state
}

func TestCareEntitiesSecondLineChecks(t *testing.T) {
	h := testkit.New(t)
	baby := caretest.BabyRow(t, h, storeBaby, 1)
	if err := caretest.Write(t, h, baby); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		edit func(*care.Row)
		code string
	}{
		{"eight-day feed", func(r *care.Row) { end := r.StartedAt.Add(8 * 24 * time.Hour); r.EndedAt = &end }, "23514"},
		{"zero-second part", func(r *care.Row) { zero := int32(0); r.Derived.ShortestPartSeconds = &zero }, "23514"},
		{"negative bottle", func(r *care.Row) {
			r.Derived.BottleVolumeML, r.Derived.BottleObservations = protocol.MustParseDecimal("-1"), 1
		}, "23514"},
		{"feed without baby", func(r *care.Row) { r.BabyID = nil }, "23514"},
		{"timer kind on a feed", func(r *care.Row) { k := "feed"; r.TimerKind = &k }, "23514"},
		{"unknown baby", func(r *care.Row) { other := "11000000-0000-4000-8000-0000000000ff"; r.BabyID = &other }, "23503"},
		{"origin after its change", func(r *care.Row) { r.CreatedSeq = r.ChangedSeq + 1 }, "23514"},
		{"reused sequence", func(r *care.Row) { r.ChangedSeq, r.CreatedSeq = baby.ChangedSeq, baby.ChangedSeq }, "23505"},
	} {
		row := caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000001", 2)
		c.edit(&row)
		write := caretest.Write
		if c.name == "reused sequence" {
			write = writeUnguarded // QueueWrites refuses the sequence in Go first
		}
		var pgErr *pgconn.PgError
		if err := write(t, h, row); !errors.As(err, &pgErr) || pgErr.Code != c.code {
			t.Fatalf("%s: %v, want %s", c.name, err, c.code)
		}
	}
	// The valid row is accepted, so each refusal above is its edit's.
	if err := caretest.Write(t, h, caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000001", 2)); err != nil {
		t.Fatal(err)
	}
}

// The family counters and sequence follow what a write batch queued, and a
// conflicting row or a moved family sequence fails the check, writing
// nothing.
func TestQueueWritesGuardsVersionsKindsAndSequence(t *testing.T) {
	h := testkit.New(t)
	owner := caretest.Owner(h)
	baby := caretest.BabyRow(t, h, storeBaby, 1)
	feed := caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000001", 2)
	if err := caretest.Write(t, h, baby, feed); err != nil {
		t.Fatal(err)
	}
	// Two updates of one entity chained in one batch reach the store as one
	// row two versions ahead.
	session := "51000000-0000-4000-8000-000000000001"
	v2 := owner.Session(t, feed, session, 3)
	v3 := owner.Session(t, v2, session, 3)
	if v3.EntityVersion != 3 {
		t.Fatalf("chained version %d", v3.EntityVersion)
	}
	if err := caretest.Write(t, h, v3); err != nil {
		t.Fatalf("chained update: %v", err)
	}
	rows := caretest.Rows(t, h)
	wantSameRow(t, rows[feed.ID], v3)
	before := readState(t, h)
	if want := familyState(3, baby, v3); before != want {
		t.Fatalf("family %+v, want %+v", before, want)
	}

	stale := v3
	stale.ChangedSeq = 4
	older := v2
	older.ChangedSeq = 4
	sleep := owner.Row(t, "sleep", feed.ID, &baby, 4)
	sleep.EntityVersion = 4
	for name, row := range map[string]care.Row{"same version": stale, "older version": older, "other kind": sleep} {
		if err := caretest.Write(t, h, row); !errors.Is(err, care.ErrConflict) {
			t.Fatalf("%s: %v, want ErrConflict", name, err)
		}
	}
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	moved := before
	moved.Sequence--
	batch := &pgx.Batch{}
	next := owner.Row(t, "diaper", "41000000-0000-4000-8000-000000000001", &baby, 4)
	check := care.QueueWrites(batch, care.Writes{FamilyID: h.FamilyID, Base: moved, Sequence: 4, Rows: []care.Row{next},
		Delta: care.CounterDelta{}.Replace(nil, next)})
	results := tx.SendBatch(t.Context(), batch)
	if err := check(results); !errors.Is(err, care.ErrConflict) {
		t.Fatalf("moved family sequence: %v, want ErrConflict", err)
	}
	_ = results.Close()
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := readState(t, h); after != before {
		t.Fatalf("a refused write changed the family %+v -> %+v", before, after)
	}
	if got := caretest.Rows(t, h); len(got) != 2 || !sameRow(got[feed.ID], v3) {
		t.Fatalf("a refused write changed rows: %d", len(got))
	}
}

func readState(t *testing.T, h *testkit.Harness) care.FamilyState {
	t.Helper()
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	state, err := care.ReadFamilyState(t.Context(), tx, h.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// fetchFixture is a family with every kind of reference Fetch resolves.
type fetchFixture struct {
	baby, babyTwo, feed, member, liveTimer, stopped, completion care.Row
	session, mutation                                           string
	hash                                                        [32]byte
}

func seedFetch(t *testing.T, h *testkit.Harness) fetchFixture {
	t.Helper()
	owner := caretest.Owner(h)
	f := fetchFixture{session: "51000000-0000-4000-8000-000000000001", mutation: "01000000-0000-4000-8000-000000000001"}
	f.baby = caretest.BabyRow(t, h, storeBaby, 1)
	f.babyTwo = caretest.BabyRow(t, h, storeBabyTwo, 2)
	f.feed = owner.Session(t, caretest.FeedRow(t, h, f.baby, "21000000-0000-4000-8000-000000000001", 3), f.session, 3)
	f.member = owner.Session(t, caretest.FeedRow(t, h, f.baby, "21000000-0000-4000-8000-000000000002", 4), f.session, 4)
	f.liveTimer = owner.Row(t, "timer", "61000000-0000-4000-8000-000000000001", &f.baby, 5)
	completion := "21000000-0000-4000-8000-000000000003"
	f.stopped = owner.Tombstone(t, owner.Row(t, "timer", "61000000-0000-4000-8000-000000000002", &f.babyTwo, 6), 6, &completion)
	f.completion = caretest.FeedRow(t, h, f.babyTwo, completion, 7)
	f.hash = [32]byte{1, 2, 3}
	receipt := care.ReceiptRow{DeviceID: h.Owner.DeviceID, MutationID: f.mutation, AccountID: h.Owner.AccountID, Hash: f.hash,
		Outcome: protocol.Outcome{MutationID: f.mutation, Status: "applied"}, AcceptedAt: h.Now}
	if err := owner.Commit(t, h.Runtime, []care.ReceiptRow{receipt}, f.baby, f.babyTwo, f.feed, f.member, f.liveTimer, f.stopped, f.completion); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fetchFixture) refs(device string) care.Refs {
	return care.Refs{
		IDs:             []string{f.feed.ID, f.stopped.ID, "21000000-0000-4000-8000-0000000000ff"},
		StoppedTimerIDs: []string{f.stopped.ID}, SessionIDs: []string{f.session}, TimerBabyIDs: []string{f.baby.ID},
		CascadeBabyIDs: []string{f.babyTwo.ID}, MutationIDs: []string{f.mutation, "01000000-0000-4000-8000-0000000000ff"},
		DeviceID: device, CountLiveBabies: true,
	}
}

func TestFetchIsOneRoundTrip(t *testing.T) {
	h := testkit.New(t)
	f := seedFetch(t, h)
	pool, wire := testkit.CountedPool(t, h.Runtime)
	var trips int
	var statements []string
	fetch := func() care.Fetched {
		tx := caretest.TenantTx(t, pool, h.Owner.AccountID, h.FamilyID)
		defer func() { _ = tx.Rollback(t.Context()) }()
		wire.Reset()
		fetched, err := care.Fetch(t.Context(), tx, h.FamilyID, f.refs(h.Owner.DeviceID))
		if err != nil {
			t.Fatal(err)
		}
		trips, statements = wire.Trips(), wire.Statements()
		return fetched
	}
	fetch() // prepares every statement on the one connection
	fetched := fetch()
	if trips != 1 {
		t.Fatalf("Fetch took %d round trips, want 1: %q", trips, statements)
	}
	if len(statements) != 7 {
		t.Fatalf("Fetch sent %d statements, want 7: %q", len(statements), statements)
	}
	if !strings.Contains(statements[0], "FOR NO KEY UPDATE") {
		t.Fatalf("the family lock is not first: %q", statements[0])
	}

	if want := familyState(7, f.baby, f.babyTwo, f.feed, f.member, f.liveTimer, f.stopped, f.completion); fetched.Family != want {
		t.Fatalf("family %+v, want %+v", fetched.Family, want)
	}
	if len(fetched.Rows) != 3 {
		t.Fatalf("rows %v", slices.Collect(mapsKeys(fetched.Rows)))
	}
	for _, want := range []care.Row{f.feed, f.stopped, f.completion} {
		wantSameRow(t, fetched.Rows[want.ID], want)
	}
	withoutWire := func(rows []care.Row) []string {
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			if r.Wire != nil || r.WireBytes == 0 {
				t.Fatalf("%s: wire %d bytes loaded, size %d", r.ID, len(r.Wire), r.WireBytes)
			}
			ids = append(ids, r.ID)
		}
		slices.Sort(ids)
		return ids
	}
	if got := withoutWire(fetched.SessionMembers); !slices.Equal(got, []string{f.feed.ID, f.member.ID}) {
		t.Fatalf("session members %v", got)
	}
	if got := withoutWire(fetched.CascadeTargets); !slices.Equal(got, []string{f.completion.ID}) {
		t.Fatalf("cascade targets %v", got)
	}
	if len(fetched.LiveTimers) != 1 {
		t.Fatalf("live timers %d", len(fetched.LiveTimers))
	}
	wantSameRow(t, fetched.LiveTimers[0], f.liveTimer)
	if fetched.LiveBabies != 2 {
		t.Fatalf("live babies %d", fetched.LiveBabies)
	}
	receipt, ok := fetched.Receipts[f.mutation]
	if len(fetched.Receipts) != 1 || !ok || !receipt.SameFamily || receipt.Hash != f.hash || receipt.Outcome.MutationID != f.mutation ||
		receipt.Outcome.Status != "applied" {
		t.Fatalf("receipts %+v", fetched.Receipts)
	}

	// Refs that ask for nothing beyond the lock send only the lock.
	tx := caretest.TenantTx(t, pool, h.Owner.AccountID, h.FamilyID)
	wire.Reset()
	only, err := care.Fetch(t.Context(), tx, h.FamilyID, care.Refs{})
	if err != nil || only.Family != fetched.Family || len(only.Rows) != 0 || only.Receipts == nil {
		t.Fatalf("lock-only fetch %+v %v", only, err)
	}
	if trips, n := wire.Trips(), len(wire.Statements()); trips != 1 || n != 1 {
		t.Fatalf("lock-only fetch: %d round trips, %d statements", trips, n)
	}
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// One pipelined batch carries the upsert, the receipts, the family row and
// the pull read.
func TestQueueWritesIsOneRoundTrip(t *testing.T) {
	h := testkit.New(t)
	owner := caretest.Owner(h)
	baby := caretest.BabyRow(t, h, storeBaby, 1)
	if err := caretest.Write(t, h, baby); err != nil {
		t.Fatal(err)
	}
	pool, wire := testkit.CountedPool(t, h.Runtime)
	feed := caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000001", 2)
	diaper := owner.Row(t, "diaper", "41000000-0000-4000-8000-000000000001", &baby, 3)
	mutation := "01000000-0000-4000-8000-000000000001"
	receipt := care.ReceiptRow{DeviceID: h.Owner.DeviceID, MutationID: mutation, AccountID: h.Owner.AccountID, Hash: [32]byte{9},
		Outcome: protocol.Outcome{MutationID: mutation, Status: "applied"}, AcceptedAt: h.Now}
	var trips int
	var statements []string
	write := func(commit bool) []care.ChangeRow {
		tx := caretest.TenantTx(t, pool, h.Owner.AccountID, h.FamilyID)
		base, err := care.LockFamily(t.Context(), tx, h.FamilyID)
		if err != nil {
			t.Fatal(err)
		}
		w := care.Writes{FamilyID: h.FamilyID, Base: base, Sequence: 3, Rows: []care.Row{feed, diaper}, Receipts: []care.ReceiptRow{receipt},
			Delta: care.CounterDelta{}.Replace(nil, feed).Replace(nil, diaper)}
		batch := &pgx.Batch{}
		check := care.QueueWrites(batch, w)
		read := care.QueueChanges(batch, h.FamilyID, 0, 3, 10)
		wire.Reset()
		results := tx.SendBatch(t.Context(), batch)
		if err := check(results); err != nil {
			t.Fatal(err)
		}
		changes, err := read(results)
		if err != nil {
			t.Fatal(err)
		}
		if err := results.Close(); err != nil {
			t.Fatal(err)
		}
		trips, statements = wire.Trips(), wire.Statements()
		if commit {
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
		} else if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		return changes
	}
	write(false) // prepares every statement on the one connection
	changes := write(true)
	if trips != 1 {
		t.Fatalf("the write batch took %d round trips, want 1: %q", trips, statements)
	}
	if len(statements) != 4 {
		t.Fatalf("the write batch sent %d statements, want 4: %q", len(statements), statements)
	}
	if len(changes) != 3 || changes[0].Seq != 1 || changes[1].Seq != 2 || changes[2].Seq != 3 ||
		!bytes.Equal(changes[1].Wire, feed.Wire) || changes[2].Kind != "diaper" || changes[2].ID != diaper.ID {
		t.Fatalf("pull read %+v", changes)
	}
	if got, want := readState(t, h), familyState(3, baby, feed, diaper); got != want {
		t.Fatalf("family %+v, want %+v", got, want)
	}
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	fetched, err := care.Fetch(t.Context(), tx, h.FamilyID, care.Refs{MutationIDs: []string{mutation}, DeviceID: h.Owner.DeviceID})
	if err != nil || fetched.Receipts[mutation].Hash != [32]byte{9} || !fetched.Receipts[mutation].SameFamily {
		t.Fatalf("receipt %+v %v", fetched.Receipts, err)
	}
	var accepted time.Time
	if err := tx.QueryRow(t.Context(), `SELECT accepted_at FROM care_receipts WHERE device_id=$1 AND mutation_id=$2`,
		h.Owner.DeviceID, mutation).Scan(&accepted); err != nil || !accepted.Equal(h.Now) {
		t.Fatalf("receipt accepted at %s %v", accepted, err)
	}
}

func TestReadPullIsOneStatementSnapshot(t *testing.T) {
	h := testkit.New(t)
	baby := caretest.BabyRow(t, h, storeBaby, 1)
	written := []care.Row{baby}
	for seq := uint64(2); seq <= 4; seq++ {
		written = append(written, caretest.FeedRow(t, h, baby, fmt.Sprintf("21000000-0000-4000-8000-%012d", seq), seq))
	}
	if err := caretest.Write(t, h, written...); err != nil {
		t.Fatal(err)
	}
	reader := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	pull := func(after uint64, limit int, wantHW uint64, wantSeqs ...uint64) {
		t.Helper()
		hw, rows, err := care.ReadPull(t.Context(), reader, h.FamilyID, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		seqs := make([]uint64, len(rows))
		for i, r := range rows {
			seqs[i] = r.Seq
			if r.Seq > hw || !bytes.Equal(r.Wire, written[r.Seq-1].Wire) || r.ID != written[r.Seq-1].ID {
				t.Fatalf("row %d (hw %d) is not the written row", r.Seq, hw)
			}
		}
		if hw != wantHW || !slices.Equal(seqs, wantSeqs) {
			t.Fatalf("pull after %d: hw %d rows %v, want hw %d rows %v", after, hw, seqs, wantHW, wantSeqs)
		}
	}
	pull(0, 100, 4, 1, 2, 3, 4)
	// A writer commits two more changes between the reader's statements.
	for seq := uint64(5); seq <= 6; seq++ {
		written = append(written, caretest.FeedRow(t, h, baby, fmt.Sprintf("21000000-0000-4000-8000-%012d", seq), seq))
	}
	if err := caretest.Write(t, h, written[4:]...); err != nil {
		t.Fatal(err)
	}
	pull(0, 100, 6, 1, 2, 3, 4, 5, 6)
	pull(4, 1, 6, 5)
	pull(6, 10, 6)
	// An uncommitted writer holding sequence 7 is invisible to both the high
	// water and the rows.
	pending := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if err := caretest.Owner(h).Queue(t, pending, nil, caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000007", 7)); err != nil {
		t.Fatal(err)
	}
	pull(4, 10, 6, 5, 6)
	if err := pending.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	other := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, _, err := care.ReadPull(t.Context(), other, "f0000000-0000-4000-8000-0000000000ff", 0, 10); !errors.Is(err, care.ErrNotFound) {
		t.Fatalf("pull of an invisible family: %v", err)
	}
}

// The pull's cached generic plan survives a VACUUM or ANALYZE of families.
// Either updates families' pg_class row (or its statistics), which
// invalidates every cached plan that names families; under load every
// backend then re-plans the pull over all sixteen care_entities partitions
// at once (vm-v2-analysis.md, the stall at ~40 s past every minute). The
// test sees a re-plan by changing the planner settings after the plan is
// cached: a cached plan keeps the index scans it was built with, a new plan
// takes the sequential scans the new settings force. A control statement
// that names families must be re-planned by each event (so the event did
// invalidate); the pull must not be.
func TestReadPullPlanSurvivesFamiliesVacuumAndAnalyze(t *testing.T) {
	h := testkit.New(t)
	ctx := t.Context()
	baby := caretest.BabyRow(t, h, storeBaby, 1)
	if err := caretest.Write(t, h, baby, caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000002", 2)); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, h.Runtime.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `SELECT set_config('app.account_id', $1, false), set_config('app.family_id', $2, false)`,
		h.Owner.AccountID, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	settings := func(t *testing.T, indexes bool) {
		t.Helper()
		on, off := "on", "off"
		if !indexes {
			on, off = off, on
		}
		for _, setting := range []string{"SET plan_cache_mode = force_generic_plan", "SET enable_seqscan = " + off, "SET enable_bitmapscan = off",
			"SET enable_indexscan = " + on, "SET enable_indexonlyscan = " + on} {
			if _, err := conn.Exec(ctx, setting); err != nil {
				t.Fatal(err)
			}
		}
	}
	statements := map[string]string{"pull": care.Statements()["read_pull"], "control": `SELECT next_sequence FROM families WHERE id = $1`}
	args := map[string]string{"pull": "'" + h.FamilyID + "'::uuid, 0, 10", "control": "'" + h.FamilyID + "'::uuid"}
	// cachedPlan reports whether the prepared statement still runs the plan
	// it was prepared with (index scans), or was planned again (sequential
	// scans).
	cachedPlan := func(t *testing.T, name, prepared string) bool {
		t.Helper()
		rows, err := conn.Query(ctx, "EXPLAIN (COSTS OFF) EXECUTE "+prepared+"("+args[name]+")")
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		plan := strings.Join(lines, "\n")
		index, seq := strings.Contains(plan, "Index Scan") || strings.Contains(plan, "Index Only Scan"), strings.Contains(plan, "Seq Scan")
		if index == seq {
			t.Fatalf("%s plan is neither the cached nor a new one:\n%s", name, plan)
		}
		return index
	}
	for round, event := range []string{"ANALYZE public.families", "VACUUM public.families"} {
		t.Run(event, func(t *testing.T) {
			prepared := func(name string) string { return fmt.Sprintf("%s_%d", name, round) }
			settings(t, true)
			for name, sql := range statements {
				if _, err := conn.Exec(ctx, "PREPARE "+prepared(name)+" AS "+sql); err != nil {
					t.Fatal(err)
				}
				if !cachedPlan(t, name, prepared(name)) {
					t.Fatalf("%s did not plan with index scans", name)
				}
			}
			settings(t, false)
			for name := range statements {
				if !cachedPlan(t, name, prepared(name)) {
					t.Fatalf("%s was planned again by a settings change alone", name)
				}
			}
			// Dead versions, as the write load leaves them.
			for i := 0; i < 3; i++ {
				if _, err := h.Migrator.Exec(ctx, `UPDATE public.families SET next_sequence = next_sequence WHERE id = $1`, h.FamilyID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.Migrator.Exec(ctx, event); err != nil {
				t.Fatal(err)
			}
			if cachedPlan(t, "control", prepared("control")) {
				t.Fatalf("%s did not invalidate a plan that names families", event)
			}
			if !cachedPlan(t, "pull", prepared("pull")) {
				t.Errorf("%s invalidated the pull's cached plan: every backend plans it again", event)
			}
		})
	}
}

// A snapshot pages every row, tombstones included, by kind code then id.
func TestSnapshotPageKeyOrder(t *testing.T) {
	h := testkit.New(t)
	owner := caretest.Owner(h)
	baby := caretest.BabyRow(t, h, "1f000000-0000-4000-8000-000000000001", 1)
	second := caretest.BabyRow(t, h, "1e000000-0000-4000-8000-000000000001", 2)
	rows := []care.Row{baby, second}
	kinds := []string{"feed", "sleep", "diaper", "timer", "supplement", "growth_profile", "measurement"}
	for i, kind := range kinds {
		// ids descend as kinds ascend, so only the kind code orders them
		rows = append(rows, owner.Row(t, kind, fmt.Sprintf("%x0000000-0000-4000-8000-000000000001", 0xd-i), &baby, uint64(3+i)))
	}
	sleep := rows[3]
	rows[3] = owner.Tombstone(t, sleep, 10, nil)
	if err := caretest.Write(t, h, rows...); err != nil {
		t.Fatal(err)
	}
	want := []care.Row{second, baby}
	want = append(want, rows[2:]...)

	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	var got []care.SnapshotRow
	after := care.FirstSnapshotKey
	for pages := 0; ; pages++ {
		if pages > len(want) {
			t.Fatal("snapshot paging does not end")
		}
		batch := &pgx.Batch{}
		read := care.QueueSnapshotPage(batch, h.FamilyID, after, 2)
		results := tx.SendBatch(t.Context(), batch)
		page, err := read(results)
		if err != nil {
			t.Fatal(err)
		}
		if err := results.Close(); err != nil {
			t.Fatal(err)
		}
		if len(page) > 2 {
			t.Fatalf("page of %d", len(page))
		}
		got = append(got, page...)
		if len(page) < 2 {
			break
		}
		after = page[len(page)-1].Key
	}
	if len(got) != len(want) {
		t.Fatalf("snapshot of %d rows, want %d", len(got), len(want))
	}
	for i, r := range want {
		if got[i].Key != (care.SnapshotKey{Kind: r.Kind, ID: r.ID}) || !bytes.Equal(got[i].Wire, r.Wire) {
			t.Fatalf("snapshot row %d is %+v, want %s %s", i, got[i].Key, r.Kind, r.ID)
		}
	}
}

// Snapshot pages, Stream and MatchBabies order by the uuid, never by its
// text, whatever the database's collation. Under ICU's numeric collation
// ("und-u-kn-true", which a self-hoster's database may use) digit runs sort
// by value, so the text order of these ids differs from their uuid order;
// a page sorted by text would resume, in uuid order, past rows it never
// served.
func TestKeyOrderIsUUIDOrderUnderANumericCollation(t *testing.T) {
	h := testkit.NewWithICUCollation(t, "und-u-kn-true")
	owner := caretest.Owner(h)
	random := rand.New(rand.NewPCG(11, 13))
	id := func() string {
		var b [16]byte
		for i := range b {
			b[i] = byte(random.IntN(256))
		}
		b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	var rows []care.Row
	var babies []care.Row
	for i := range 6 {
		// one name, so MatchBabies returns all six
		babies = append(babies, owner.Baby(t, id(), "Noa", "2026-01-12", uint64(1+i)))
	}
	rows = append(rows, babies...)
	for i := range 40 {
		rows = append(rows, owner.Row(t, []string{"sleep", "diaper"}[i%2], id(), &babies[0], uint64(7+i)))
	}
	if err := caretest.Write(t, h, rows...); err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(rows)
	slices.SortFunc(want, func(a, b care.Row) int {
		ka, _ := care.KindCode(a.Kind)
		kb, _ := care.KindCode(b.Kind)
		if ka != kb {
			return int(ka) - int(kb)
		}
		return strings.Compare(a.ID, b.ID) // lowercase canonical text compares as the uuid bytes
	})

	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	// Not vacuous: this database sorts these ids' text otherwise.
	var textOrderDiffers bool
	if err := tx.QueryRow(t.Context(), `SELECT array_agg(id::text ORDER BY kind, id::text) <> array_agg(id::text ORDER BY kind, id)
		FROM care_entities WHERE family_id = $1`, h.FamilyID).Scan(&textOrderDiffers); err != nil {
		t.Fatal(err)
	}
	if !textOrderDiffers {
		t.Fatal("the numeric collation sorts these ids' text in uuid order: the test proves nothing")
	}

	var paged []care.SnapshotKey
	after := care.FirstSnapshotKey
	for pages := 0; ; pages++ {
		if pages > len(want) {
			t.Fatal("snapshot paging does not end")
		}
		batch := &pgx.Batch{}
		read := care.QueueSnapshotPage(batch, h.FamilyID, after, 4)
		results := tx.SendBatch(t.Context(), batch)
		page, err := read(results)
		if err = errors.Join(err, results.Close()); err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			paged = append(paged, r.Key)
		}
		if len(page) < 4 {
			break
		}
		after = page[len(page)-1].Key
	}
	restore := care.SetStreamPageSize(4)
	defer restore()
	var streamed []care.SnapshotKey
	if err := care.Stream(t.Context(), tx, h.FamilyID, func(r care.Row) error {
		streamed = append(streamed, care.SnapshotKey{Kind: r.Kind, ID: r.ID})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var wantKeys []care.SnapshotKey
	for _, r := range want {
		wantKeys = append(wantKeys, care.SnapshotKey{Kind: r.Kind, ID: r.ID})
	}
	if !slices.Equal(paged, wantKeys) {
		t.Fatalf("snapshot pages served %d rows\n%v\nwant %d in uuid order\n%v", len(paged), paged, len(wantKeys), wantKeys)
	}
	if !slices.Equal(streamed, wantKeys) {
		t.Fatalf("Stream visited %d rows\n%v\nwant %d in uuid order\n%v", len(streamed), streamed, len(wantKeys), wantKeys)
	}

	hash, err := care.BabyNameHash("Noa", "2026-01-12")
	if err != nil {
		t.Fatal(err)
	}
	matches, err := care.MatchBabies(t.Context(), tx, h.FamilyID, hash)
	if err != nil {
		t.Fatal(err)
	}
	var matched []string
	for _, r := range matches {
		matched = append(matched, r.ID)
	}
	var wantBabies []string
	for _, r := range want[:len(babies)] {
		wantBabies = append(wantBabies, r.ID)
	}
	if !slices.Equal(matched, wantBabies) {
		t.Fatalf("MatchBabies order %v, want uuid order %v", matched, wantBabies)
	}
}

func TestChargeImportEnforcesQuota(t *testing.T) {
	h := testkit.New(t)
	limits := h.Limits
	limits.MaxChargedStorageBytesPerFamily = 1000
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := care.LockFamily(t.Context(), tx, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE families SET care_charged_bytes=990 WHERE id=$1`, h.FamilyID); err != nil {
		t.Fatal(err)
	}
	charged := func() uint64 {
		state, err := care.ReadFamilyState(t.Context(), tx, h.FamilyID)
		if err != nil {
			t.Fatal(err)
		}
		return state.ChargedBytes
	}
	if err := care.ChargeImport(t.Context(), tx, h.FamilyID, 20, limits); !errors.Is(err, care.ErrQuotaExceeded) {
		t.Fatalf("charge past the quota: %v", err)
	}
	if got := charged(); got != 990 {
		t.Fatalf("a refused charge left %d bytes", got)
	}
	if err := care.ChargeImport(t.Context(), tx, h.FamilyID, 5, limits); err != nil {
		t.Fatal(err)
	}
	if err := care.ChargeImport(t.Context(), tx, h.FamilyID, 5, limits); err != nil {
		t.Fatalf("charge up to the quota: %v", err)
	}
	if got := charged(); got != 1000 {
		t.Fatalf("charged %d", got)
	}
	// A release is never refused.
	limits.MaxChargedStorageBytesPerFamily = 10
	if err := care.ChargeImport(t.Context(), tx, h.FamilyID, -995, limits); err != nil || charged() != 5 {
		t.Fatalf("release: %v, %d bytes", err, charged())
	}
	if err := care.ChargeImport(t.Context(), tx, h.FamilyID, 1, limits); err != nil {
		t.Fatal(err)
	}
	if err := care.ChargeImport(t.Context(), tx, "f0000000-0000-4000-8000-0000000000ff", 1, limits); !errors.Is(err, care.ErrNotFound) {
		t.Fatalf("charge of an invisible family: %v", err)
	}
}

func TestMatchBabiesUsesNameHash(t *testing.T) {
	h := testkit.New(t)
	owner := caretest.Owner(h)
	upper := owner.Baby(t, "11000000-0000-4000-8000-000000000003", "Élie", "2026-01-12", 1)
	shout := owner.Baby(t, "11000000-0000-4000-8000-000000000002", "ÉLIE", "2026-01-12", 2)
	deleted := owner.Tombstone(t, owner.Baby(t, "11000000-0000-4000-8000-000000000001", "élie", "2026-01-12", 3), 3, nil)
	otherDay := owner.Baby(t, "11000000-0000-4000-8000-000000000004", "Élie", "2026-01-13", 4)
	otherName := owner.Baby(t, "11000000-0000-4000-8000-000000000005", "Noa", "2026-01-12", 5)
	if err := caretest.Write(t, h, upper, shout, deleted, otherDay, otherName); err != nil {
		t.Fatal(err)
	}
	hash, err := care.BabyNameHash(" élie ", "2026-01-12")
	if err != nil {
		t.Fatal(err)
	}
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	matches, err := care.MatchBabies(t.Context(), tx, h.FamilyID, hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("%d matches", len(matches))
	}
	wantSameRow(t, matches[0], shout)
	wantSameRow(t, matches[1], upper)
}

// Get, GetMany, Stream and StreamChanges read rows back exactly as written,
// and a stored change encodes as the legacy journal served it.
func TestStoreReadsReturnRowsAsWritten(t *testing.T) {
	h := testkit.New(t)
	owner := caretest.Owner(h)
	baby := caretest.BabyRow(t, h, storeBaby, 1)
	rows := []care.Row{baby}
	for i, kind := range []string{"feed", "sleep", "diaper", "timer", "supplement", "growth_profile", "measurement", "feed", "sleep"} {
		rows = append(rows, owner.Row(t, kind, fmt.Sprintf("2%07d-0000-4000-8000-000000000001", 9-i), &baby, uint64(2+i)))
	}
	completion := rows[8].ID
	rows[4] = owner.Tombstone(t, rows[4], 11, &completion) // a stopped timer
	rows[2] = owner.Tombstone(t, rows[2], 12, nil)         // a deleted sleep
	if err := caretest.Write(t, h, rows...); err != nil {
		t.Fatal(err)
	}
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	for _, want := range rows {
		got, err := care.Get(t.Context(), tx, h.FamilyID, want.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantSameRow(t, got, want)
	}
	if _, err := care.Get(t.Context(), tx, h.FamilyID, "21000000-0000-4000-8000-0000000000ff"); !errors.Is(err, care.ErrNotFound) {
		t.Fatalf("missing row: %v", err)
	}
	many, err := care.GetMany(t.Context(), tx, h.FamilyID, []string{rows[1].ID, rows[4].ID, "21000000-0000-4000-8000-0000000000ff"})
	if err != nil || len(many) != 2 {
		t.Fatalf("GetMany %d %v", len(many), err)
	}
	wantSameRow(t, many[rows[4].ID], rows[4])

	restore := care.SetStreamPageSize(3)
	defer restore()
	var streamed []care.Row
	if err := care.Stream(t.Context(), tx, h.FamilyID, func(r care.Row) error {
		// visit may use the transaction between pages
		if _, err := care.Get(t.Context(), tx, h.FamilyID, r.ID); err != nil {
			return err
		}
		streamed = append(streamed, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ordered := slices.Clone(rows)
	slices.SortFunc(ordered, func(a, b care.Row) int {
		ka, _ := care.KindCode(a.Kind)
		kb, _ := care.KindCode(b.Kind)
		if ka != kb {
			return int(ka) - int(kb)
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(streamed) != len(ordered) {
		t.Fatalf("streamed %d rows, want %d", len(streamed), len(ordered))
	}
	for i := range ordered {
		wantSameRow(t, streamed[i], ordered[i])
	}
	stop := errors.New("stop")
	if err := care.Stream(t.Context(), tx, h.FamilyID, func(care.Row) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("Stream did not return the visit error: %v", err)
	}

	var changes []care.ChangeRow
	if err := care.StreamChanges(t.Context(), tx, h.FamilyID, 11, func(c care.ChangeRow) error {
		changes = append(changes, c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, c := range changes {
		seqs = append(seqs, c.Seq)
	}
	if !slices.Equal(seqs, []uint64{1, 2, 4, 6, 7, 8, 9, 10, 11}) {
		t.Fatalf("changes through 11: %v", seqs)
	}
	for _, c := range changes {
		var want care.Row
		for _, r := range rows {
			if r.ID == c.ID {
				want = r
			}
		}
		raw, err := c.Encode(h.Info, h.FamilyID)
		if err != nil {
			t.Fatal(err)
		}
		entity, err := want.Entity()
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := json.Marshal(protocol.JournalChange{
			Cursor:     protocol.Cursor{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, FamilyID: h.FamilyID, Sequence: want.ChangedSeq},
			EntityKind: want.Kind, EntityID: want.ID, EntityVersion: want.EntityVersion, ActorAccountID: want.ActorAccountID,
			DeviceID: want.DeviceID, Operation: want.Operation, AcceptedAt: entity.Meta.AcceptedAt, SnapshotOrTombstone: entity})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, legacy) {
			t.Fatalf("change %d\n got %s\nwant %s", c.Seq, raw, legacy)
		}
	}
}

// Every store read, as the runtime in a tenant transaction with generic
// plans, scans exactly one care_entities partition through the index its
// query names, with every key predicate an index condition. Under custom
// and generic plans alike, no read sorts by an id's text, and the key-order
// pages (snapshot pages, Stream, pulls) do not sort at all: their index
// returns them in (kind, uuid) or sequence order, so a page reads at most
// its limit and its order is the uuid order the continuation resumes in.
func TestCareQueriesUseOnePartitionAndIndexes(t *testing.T) {
	h := testkit.New(t)
	ctx := t.Context()
	foreign, err := h.Family.Create(ctx, h.Outsider, h.CommandID(), "Foreign")
	if err != nil {
		t.Fatal(err)
	}
	h.Outsider = h.Refresh("outsider")
	// 40 babies, so one baby's rows are a small part of the family, and 1000
	// stopped timers next to the 40 live ones.
	var babies []string
	for i := 1; i <= 40; i++ {
		babies = append(babies, fmt.Sprintf("71000000-0000-4000-8000-%012d", i))
	}
	for _, family := range []struct{ account, family, device string }{{h.Owner.AccountID, h.FamilyID, h.Owner.DeviceID}, {h.Outsider.AccountID, foreign.ID, h.Outsider.DeviceID}} {
		tx := caretest.TenantTx(t, h.Runtime, family.account, family.family)
		for _, step := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO care_entities(family_id,id,kind,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,sort_order,name_hash,wire)
			 SELECT $1, b, 1, 1, i, $2, $2, $3, 'create', now(), i, sha256(convert_to(b::text,'UTF8')), convert_to(repeat('x',80),'UTF8')
			 FROM unnest($4::uuid[]) WITH ORDINALITY AS u(b, i)`, []any{family.family, family.account, family.device, babies}},
			{`INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,started_at,ended_at,session_id,feed_kind,wire)
			 SELECT $1, gen_random_uuid(), k, ($4::uuid[])[1 + (g/4)%40], 1, 100 + g, $2, $2, $3, 'create', now(),
			   timestamptz '2026-10-01' - g * interval '10 minutes',
			   CASE WHEN k IN (2,3,4) THEN timestamptz '2026-10-01' - g * interval '10 minutes' + interval '5 minutes' END,
			   CASE WHEN k IN (2,3) AND g % 7 = 0 THEN gen_random_uuid() END, CASE WHEN k = 2 THEN 2::smallint END,
			   convert_to(repeat('x',80),'UTF8')
			 FROM generate_series(1, 4000) g, LATERAL (SELECT (ARRAY[2,3,4,8])[1 + g%4]::smallint AS k) x`, []any{family.family, family.account, family.device, babies}},
			{`INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,started_at,timer_kind,wire)
			 SELECT $1, gen_random_uuid(), 5, b, 1, 9000 + i, $2, $2, $3, 'timer_start', now(), timestamptz '2026-10-01', 1, convert_to(repeat('x',80),'UTF8')
			 FROM unnest($4::uuid[]) WITH ORDINALITY AS u(b, i)`, []any{family.family, family.account, family.device, babies}},
			{`INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,tombstoned_at,started_at,timer_kind,wire)
			 SELECT $1, gen_random_uuid(), 5, ($4::uuid[])[1 + g%40], 2, 10000 + g, $2, $2, $3, 'timer_stop', now(), now(), timestamptz '2026-10-01', 1, convert_to(repeat('x',80),'UTF8')
			 FROM generate_series(1, 1000) g`, []any{family.family, family.account, family.device, babies}},
			{`INSERT INTO care_receipts(device_id,mutation_id,family_id,account_id,semantic_hash,outcome,accepted_at)
			 SELECT $3, gen_random_uuid(), $1, $2, sha256(convert_to(g::text,'UTF8')), '{}'::jsonb, now() FROM generate_series(1, 2000) g`,
				[]any{family.family, family.account, family.device}},
			{`UPDATE families SET next_sequence=11000 WHERE id=$1`, []any{family.family}},
		} {
			if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Migrator.Exec(ctx, "ANALYZE public.care_entities; ANALYZE public.care_receipts"); err != nil {
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
	if rows.Err() != nil || len(parents) != 16*11 {
		t.Fatalf("partition indexes %d err=%v", len(parents), rows.Err())
	}

	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	for _, setting := range []string{"SET LOCAL plan_cache_mode = force_generic_plan", "SET LOCAL enable_seqscan = off", "SET LOCAL enable_bitmapscan = off"} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatal(err)
		}
	}
	family := "'" + h.FamilyID + "'::uuid"
	uuids := "ARRAY['" + babies[0] + "','" + babies[1] + "']::uuid[]"
	statements := care.Statements()
	cases := []struct {
		name, args, index string
		scans             int
		conds             []string
		indexOrder        bool // the index provides ORDER BY: no Sort node
	}{
		{"fetch_rows", family + "," + uuids + "," + uuids, "care_entities_pkey", 2, []string{"family_id = $1", "id = ANY ($3)", "id = ANY ("}, false},
		{"fetch_session_members", family + "," + uuids, "care_entities_session_idx", 1, []string{"family_id = $1", "session_id = ANY ($2)"}, false},
		{"fetch_live_timers", family + "," + uuids, "care_entities_one_live_timer", 1, []string{"family_id = $1", "baby_id = ANY ($2)"}, false},
		{"fetch_cascade_targets", family + "," + uuids, "care_entities_history_idx", 1, []string{"family_id = $1", "baby_id = ANY ($2)"}, false},
		{"fetch_live_babies", family, "care_entities_baby_match_idx", 1, []string{"family_id = $1"}, false},
		{"fetch_open_pump", family, "care_entities_one_open_pump", 1, []string{"family_id = $1"}, false},
		{"read_pull", family + ",100,500", "care_entities_seq_key", 1, []string{"family_id = $1", "changed_seq > $2", "changed_seq <= "}, true},
		{"changes", family + ",100,2000,500", "care_entities_seq_key", 1, []string{"family_id = $1", "changed_seq > $2", "changed_seq <= $3"}, true},
		{"snapshot_page", family + ",2::smallint,'00000000-0000-0000-0000-000000000001'::uuid,201", "care_entities_snapshot_idx", 1,
			[]string{"family_id = $1", "ROW(kind, id) > ROW($2, $3)"}, true},
		{"stream_page", family + ",2::smallint,'00000000-0000-0000-0000-000000000001'::uuid,1000", "care_entities_snapshot_idx", 1,
			[]string{"family_id = $1", "ROW(kind, id) > ROW($2, $3)"}, true},
		{"get", family + ",'" + babies[0] + "'::uuid", "care_entities_pkey", 1, []string{"family_id = $1", "id = $2"}, false},
		{"get_many", family + "," + uuids, "care_entities_pkey", 1, []string{"family_id = $1", "id = ANY ($2)"}, false},
		{"match_babies", family + ",sha256(convert_to('" + babies[0] + "','UTF8'))", "care_entities_baby_match_idx", 1, []string{"family_id = $1", "name_hash = $2"}, false},
		{"fetch_receipts", "'" + h.Owner.DeviceID + "'::uuid," + family + "," + uuids, "care_receipts_pkey", 0, []string{"device_id = $1", "mutation_id = ANY ($3)"}, false},
	}
	if len(cases) != len(statements) {
		t.Fatalf("%d plan cases for %d read statements", len(cases), len(statements))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, ok := statements[c.name]
			if !ok {
				t.Fatalf("no statement %s", c.name)
			}
			if _, err := tx.Exec(ctx, "PREPARE care_"+c.name+" AS "+sql); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := tx.QueryRow(ctx, fmt.Sprintf("EXPLAIN (FORMAT JSON) EXECUTE care_%s(%s)", c.name, c.args)).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct{ Plan planNode }
			if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
				t.Fatalf("plan %s: %v", raw, err)
			}
			partition := regexp.MustCompile(`^care_entities_p\d\d$`)
			var scans []planNode
			var other []string
			pruned := 0
			plans[0].Plan.walk(func(n planNode) {
				switch {
				case partition.MatchString(n.RelationName):
					scans = append(scans, n)
				case n.RelationName != "":
					other = append(other, n.RelationName+"/"+n.IndexName)
				}
				if n.SubplansRemoved != nil && *n.SubplansRemoved == 15 {
					pruned++
				}
			})
			plans[0].Plan.walk(func(n planNode) {
				if n.IndexName != "" {
					t.Logf("%s %s using %s: Index Cond %s, Filter %q", n.NodeType, n.RelationName, n.IndexName, n.IndexCond, n.Filter)
				}
			})
			conds, filters := "", ""
			if c.scans == 0 {
				if len(scans) != 0 || len(other) != 1 || other[0] != "care_receipts/"+c.index {
					t.Fatalf("scans %v, want %s only: %s", other, c.index, raw)
				}
				plans[0].Plan.walk(func(n planNode) { conds, filters = conds+n.IndexCond, filters+n.Filter })
			} else {
				if len(scans) != c.scans || pruned != c.scans {
					t.Fatalf("%d partition scans, %d pruned to one partition, want %d: %s", len(scans), pruned, c.scans, raw)
				}
				// No other table: the pull reads its high water through
				// app.family_high_water, so its plan does not name families
				// (TestReadPullPlanSurvivesFamiliesVacuumAndAnalyze).
				if len(other) != 0 {
					t.Fatalf("scans %v beside care_entities: %s", other, raw)
				}
				for _, scan := range scans {
					if parents[scan.IndexName] != c.index {
						t.Fatalf("%s scans %s (%s), want %s: %s", scan.NodeType, scan.IndexName, parents[scan.IndexName], c.index, raw)
					}
					conds, filters = conds+" "+scan.IndexCond, filters+" "+scan.Filter
				}
			}
			for _, cond := range c.conds {
				if !strings.Contains(conds, cond) || strings.Contains(filters, cond) {
					t.Fatalf("%q not an index condition: Index Cond %q, Filter %q: %s", cond, conds, filters, raw)
				}
			}
			for _, mode := range []string{"force_generic_plan", "force_custom_plan"} {
				if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
					t.Fatal(err)
				}
				var raw string
				if err := tx.QueryRow(ctx, fmt.Sprintf("EXPLAIN (FORMAT JSON) EXECUTE care_%s(%s)", c.name, c.args)).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var plans []struct{ Plan planNode }
				if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
					t.Fatalf("plan %s: %v", raw, err)
				}
				plans[0].Plan.walk(func(n planNode) {
					if !strings.Contains(n.NodeType, "Sort") {
						return
					}
					if c.indexOrder {
						t.Fatalf("%s plan sorts (%s by %v): %s", mode, n.NodeType, n.SortKey, raw)
					}
					for _, key := range n.SortKey {
						if strings.Contains(key, "::text") {
							t.Fatalf("%s plan sorts by text (%s by %v): %s", mode, n.NodeType, n.SortKey, raw)
						}
					}
				})
			}
			if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
				t.Fatal(err)
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
	SortKey         []string   `json:"Sort Key"`
	SubplansRemoved *int       `json:"Subplans Removed"`
	Plans           []planNode `json:"Plans"`
}

func (n planNode) walk(visit func(planNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}

// writeUnguarded is caretest.Write through QueueWritesUnguarded, which
// skips QueueWrites' Go checks of the write set, so the database's own
// constraints answer.
func writeUnguarded(t *testing.T, h *testkit.Harness, rows ...care.Row) error {
	t.Helper()
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	defer func() { _ = tx.Rollback(t.Context()) }()
	base, err := care.LockFamily(t.Context(), tx, h.FamilyID)
	if err != nil {
		return err
	}
	w := care.Writes{FamilyID: h.FamilyID, Base: base, Sequence: base.Sequence, Rows: rows}
	for _, r := range rows {
		w.Sequence = max(w.Sequence, r.ChangedSeq)
		w.Delta = w.Delta.Replace(nil, r)
	}
	batch := &pgx.Batch{}
	check := care.QueueWritesUnguarded(batch, w)
	results := tx.SendBatch(t.Context(), batch)
	err = check(results)
	if err = errors.Join(err, results.Close()); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}

// QueueWrites refuses, before queuing anything, a write set the batch
// planner could only produce by mistake: a row outside the sequence range
// it allocates, two rows at one sequence, two rows of one entity, or two
// receipts of one mutation.
func TestQueueWritesRefusesMalformedWriteSets(t *testing.T) {
	h := testkit.New(t)
	baby := caretest.BabyRow(t, h, storeBaby, 4)
	feed := caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000001", 5)
	other := caretest.FeedRow(t, h, baby, "21000000-0000-4000-8000-000000000002", 6)
	receipt := care.ReceiptRow{DeviceID: h.Owner.DeviceID, MutationID: "01000000-0000-4000-8000-000000000001", AccountID: h.Owner.AccountID,
		Outcome: protocol.Outcome{MutationID: "01000000-0000-4000-8000-000000000001", Status: "applied"}, AcceptedAt: h.Now}
	at := func(r care.Row, seq uint64) care.Row { r.ChangedSeq = seq; return r }
	base := care.FamilyState{Sequence: 3}
	for _, c := range []struct {
		name string
		w    care.Writes
		want string
	}{
		{"row at the base sequence", care.Writes{Sequence: 6, Rows: []care.Row{at(baby, 3), feed}}, "outside"},
		{"row above the new sequence", care.Writes{Sequence: 5, Rows: []care.Row{baby, at(feed, 6)}}, "outside"},
		{"two rows at one sequence", care.Writes{Sequence: 6, Rows: []care.Row{baby, at(feed, 4)}}, "sequence 4 twice"},
		{"two rows of one entity", care.Writes{Sequence: 6, Rows: []care.Row{feed, at(feed, 6)}}, "twice"},
		{"two receipts of one mutation", care.Writes{Sequence: 3, Receipts: []care.ReceiptRow{receipt, receipt}}, "twice"},
	} {
		c.w.FamilyID, c.w.Base = h.FamilyID, base
		batch := &pgx.Batch{}
		check := care.QueueWrites(batch, c.w)
		if err := check(nil); err == nil || !strings.Contains(err.Error(), c.want) || batch.Len() != 0 {
			t.Fatalf("%s: %v, %d statements queued", c.name, err, batch.Len())
		}
	}
	// A well-formed set passes the checks (the database is not reached).
	batch := &pgx.Batch{}
	care.QueueWrites(batch, care.Writes{FamilyID: h.FamilyID, Base: base, Sequence: 6, Rows: []care.Row{baby, feed, other}, Receipts: []care.ReceiptRow{receipt}})
	if batch.Len() != 3 {
		t.Fatalf("%d statements queued", batch.Len())
	}
}
