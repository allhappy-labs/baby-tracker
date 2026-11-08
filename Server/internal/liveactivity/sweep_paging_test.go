package liveactivity

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The work sweep runs in pages (app.sweep_live_activity_work_page, 000037),
// each committed on its own, so no family's dirty row stays locked for the
// whole pass (Task 21 C2: the one-transaction sweep held every family's row
// for 25.6 s and the device upsert, whose trigger locks that row first,
// waited up to 9.7 s).

// sweepHarness is the harness with one baby and the caregiver's live
// registration, plus a superuser pool on its database (row security
// bypassed) for arranging and reading the sweep's tables.
type sweepHarness struct {
	*testkit.Harness
	su *pgxpool.Pool
}

func newSweepHarness(t *testing.T) sweepHarness {
	t.Helper()
	h := harness(t)
	seedBaby(t, h)
	if _, err := register(t, h, "caregiver", deviceRequest()); err != nil {
		t.Fatal(err)
	}
	var database string
	if err := h.Migrator.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	config := h.Admin.Config().Copy()
	config.ConnConfig.Database = database
	su, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(su.Close)
	return sweepHarness{Harness: h, su: su}
}

// clone adds family id beside the harness's: a copy of its family row and
// babies and of its caregiver's live registration, so the sweep has more
// than one family to page over. The registration's insert runs its trigger
// (the index row and the family's dirty row), as a request's does.
func (h sweepHarness) clone(t *testing.T, id string) {
	t.Helper()
	ctx := t.Context()
	tx, err := h.su.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, statement := range []string{
		`INSERT INTO families SELECT (jsonb_populate_record(NULL::families, to_jsonb(f) || jsonb_build_object('id', $2::uuid))).* FROM families f WHERE id=$1`,
		`INSERT INTO care_entities SELECT (jsonb_populate_record(NULL::care_entities, to_jsonb(c) || jsonb_build_object('family_id', $2::uuid, 'id', gen_random_uuid()))).*
			FROM care_entities c WHERE family_id=$1 AND kind=1`,
		`INSERT INTO live_activity_devices SELECT (jsonb_populate_record(NULL::live_activity_devices, to_jsonb(d) || jsonb_build_object('family_id', $2::uuid,
			'registration_id', gen_random_uuid(), 'installation_id', gen_random_uuid(), 'push_token', NULL))).*
			FROM live_activity_devices d WHERE family_id=$1 AND NOT revoked ORDER BY registration_id LIMIT 1`,
	} {
		if tag, err := tx.Exec(ctx, statement, h.FamilyID, id); err != nil || tag.RowsAffected() == 0 {
			t.Fatalf("clone %s: %v (%d rows)", statement, err, tag.RowsAffected())
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func (h sweepHarness) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if _, err := h.su.Exec(t.Context(), statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func (h sweepHarness) scalar(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := h.su.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (h sweepHarness) now(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := h.su.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func (h sweepHarness) journal(t *testing.T, family string) int64 {
	t.Helper()
	return h.scalar(t, `SELECT journal_sequence FROM live_activity_family_dirty WHERE family_id=$1`, family)
}

func (h sweepHarness) sequence(t *testing.T, family string) int64 {
	t.Helper()
	return h.scalar(t, `SELECT next_sequence FROM families WHERE id=$1`, family)
}

// sweepState is what a sweep pass writes: each family's dirty row (its
// version, cursor and realm, and whether its authority check moved past
// mark) and each delivery row's versions.
type sweepState struct {
	Dirty      []string
	Deliveries []string
}

func readSweepState(t *testing.T, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, mark time.Time) sweepState {
	t.Helper()
	collect := func(query string, args ...any) []string {
		rows, err := q.Query(t.Context(), query, args...)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	return sweepState{
		Dirty: collect(`SELECT concat_ws('|', family_id, dirty_version, journal_sequence, instance_id, recovery_epoch, authority_checked_at > $1)
			FROM live_activity_family_dirty ORDER BY family_id`, mark),
		Deliveries: collect(`SELECT concat_ws('|', registration_id, baby_id, kind, registration_version, dirty_version, completed_version, family_id)
			FROM live_activity_delivery ORDER BY registration_id, baby_id, kind`),
	}
}

// sweepPass runs one paged pass on q and returns its page count.
func sweepPass(t *testing.T, q *Queue) int {
	t.Helper()
	pages := 0
	for after := ""; ; {
		next, err := q.SweepWorkPage(t.Context(), after)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if next == "" {
			return pages
		}
		if next <= after {
			t.Fatalf("page cursor went from %s to %s", after, next)
		}
		after = next
	}
}

// A paged pass writes exactly what the one-transaction pass
// (app.sweep_live_activity_work, unchanged) writes: the same families
// dirtied with the same versions, cursors and delivery rows, the same
// authority checks moved. Five families cover each branch: a sequence past
// the cursor, an authority check over a minute old, a cursor of another
// recovery epoch, a family with nothing due, and one whose only
// registration is revoked (never swept).
func TestSweepWorkPagesWriteWhatTheSinglePassWrites(t *testing.T) {
	h := newSweepHarness(t)
	stale, epoch, idle, revoked := "00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000a2", "00000000-0000-4000-8000-0000000000a3", "00000000-0000-4000-8000-0000000000a4"
	for _, id := range []string{stale, epoch, idle, revoked} {
		h.clone(t, id)
	}
	// The harness's family: its sequence moved past the cursor (a lost feed).
	h.exec(t, `UPDATE families SET next_sequence=next_sequence+3 WHERE id=$1`, h.FamilyID)
	h.exec(t, `UPDATE live_activity_family_dirty d SET journal_sequence=f.next_sequence-3 FROM families f WHERE f.id=d.family_id AND d.family_id=$1`, h.FamilyID)
	// Up to date, but the authority was last checked an hour ago.
	h.exec(t, `UPDATE live_activity_family_dirty d SET journal_sequence=f.next_sequence, authority_checked_at=clock_timestamp()-interval '1 hour' FROM families f WHERE f.id=d.family_id AND d.family_id=$1`, stale)
	// A cursor written under another recovery epoch counts as none.
	h.exec(t, `UPDATE live_activity_family_dirty d SET journal_sequence=f.next_sequence, recovery_epoch=d.recovery_epoch+7 FROM families f WHERE f.id=d.family_id AND d.family_id=$1`, epoch)
	// Nothing due: the cursor at the sequence, checked just now.
	h.exec(t, `UPDATE live_activity_family_dirty d SET journal_sequence=f.next_sequence FROM families f WHERE f.id=d.family_id AND d.family_id=$1`, idle)
	// Revoked: due by both rules, but no live registration.
	h.exec(t, `UPDATE live_activity_devices SET revoked=true WHERE family_id=$1`, revoked)
	h.exec(t, `UPDATE live_activity_family_dirty SET authority_checked_at=clock_timestamp()-interval '1 hour', journal_sequence=0 WHERE family_id=$1`, revoked)
	before := readSweepState(t, h.su, h.now(t))
	if len(before.Dirty) != 5 {
		t.Fatalf("dirty rows %v, want the five families'", before.Dirty)
	}
	dirtyVersion := func(id string) int64 {
		return h.scalar(t, `SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1`, id)
	}
	versionsBefore := map[string]int64{}
	for _, id := range []string{h.FamilyID, stale, epoch, idle, revoked} {
		versionsBefore[id] = dirtyVersion(id)
	}

	// The single pass, rolled back.
	ctx := t.Context()
	tx, err := h.su.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mark := h.now(t)
	if _, err := tx.Exec(ctx, `SELECT app.sweep_live_activity_work()`); err != nil {
		t.Fatal(err)
	}
	single := readSweepState(t, tx, mark)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(readSweepState(t, h.su, mark), before) {
		t.Fatal("the rolled-back single pass left state behind")
	}

	// The paged pass on the maintenance pool, two families a page: 2 + 2,
	// then an empty page ends it (the revoked family is not listed).
	mark = h.now(t)
	q := NewQueue(h.Maintenance)
	q.workPage = 2
	if pages := sweepPass(t, q); pages != 3 {
		t.Fatalf("a pass over four families took %d pages of two, want 3", pages)
	}
	paged := readSweepState(t, h.su, mark)
	if !reflect.DeepEqual(paged, single) {
		t.Fatalf("the paged pass wrote\n%v\nthe single pass\n%v", paged, single)
	}

	// And what both wrote is the rule: the first three dirtied once (each
	// baby's two deliveries at the new version), the idle and revoked
	// families untouched, the other epoch's cursor reset to the sequence.
	for _, id := range []string{h.FamilyID, stale, epoch} {
		if got := dirtyVersion(id); got != versionsBefore[id]+1 {
			t.Errorf("family %s dirty version %d -> %d, want one dirtying", id, versionsBefore[id], got)
		}
		if got := h.scalar(t, `SELECT count(*) FROM live_activity_delivery d JOIN live_activity_family_dirty f USING(family_id) WHERE d.family_id=$1 AND d.dirty_version=f.dirty_version`, id); got != 2 {
			t.Errorf("family %s has %d deliveries at its dirty version, want the baby's feed and sleep", id, got)
		}
	}
	for _, id := range []string{idle, revoked} {
		if got := dirtyVersion(id); got != versionsBefore[id] {
			t.Errorf("family %s swept: dirty version %d -> %d", id, versionsBefore[id], got)
		}
	}
	if journal, sequence := h.journal(t, epoch), h.sequence(t, epoch); journal != sequence {
		t.Errorf("other-epoch cursor %d, want the sequence %d", journal, sequence)
	}

	// A page is 1 to 1000 families.
	for _, limit := range []any{0, 1001, nil} {
		_, err := h.Maintenance.Exec(ctx, `SELECT app.sweep_live_activity_work_page(NULL,$1::integer)`, limit)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "22023" {
			t.Fatalf("page limit %v: %v, want 22023", limit, err)
		}
	}
}

// While a page's transaction is open, only its own families' dirty rows are
// locked: a device upsert on a family outside the page commits at once
// (under the one-transaction sweep it waited for the whole pass).
func TestSweepWorkPageLeavesOtherFamiliesUnlocked(t *testing.T) {
	h := newSweepHarness(t)
	// The clone sorts before any random family id, so a one-family page is
	// the clone alone; both families are due.
	first := "00000000-0000-4000-8000-000000000001"
	h.clone(t, first)
	h.exec(t, `UPDATE live_activity_family_dirty SET authority_checked_at=clock_timestamp()-interval '1 hour'`)

	ctx := t.Context()
	page, err := h.Maintenance.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Rollback(context.Background())
	var next string
	if err := page.QueryRow(ctx, `SELECT coalesce(app.sweep_live_activity_work_page(NULL,1)::text, 'NULL')`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != first {
		t.Fatalf("first page ended at %s, want %s", next, first)
	}
	locked := func(family string) bool {
		t.Helper()
		probe, err := h.su.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Rollback(context.Background())
		_, err = probe.Exec(ctx, `SELECT 1 FROM live_activity_family_dirty WHERE family_id=$1 FOR UPDATE NOWAIT`, family)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return true
		}
		if err != nil {
			t.Fatal(err)
		}
		return false
	}
	if !locked(first) {
		t.Fatal("the open page does not hold its family's dirty row")
	}
	if locked(h.FamilyID) {
		t.Fatal("the open page holds the dirty row of a family outside it")
	}
	// A registration update on the other family: its trigger locks that
	// family's dirty row first, then dirties it.
	req := deviceRequest()
	req.RegistrationVersion = 2
	done := make(chan error, 1)
	go func() {
		_, err := register(t, h.Harness, "caregiver", req)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		_ = page.Rollback(context.Background())
		<-done
		t.Fatal("the device upsert waited for a page that does not hold its family")
	}
	if err := page.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// The worker's minute maintenance walks the work sweep page after page, each
// in its own slot, until a page comes back short, and logs the page count:
// with one family a page, a full page and then an empty one. (Its
// registration sweep runs first and would revoke clone() families, which
// have no membership or session of their own, so this one has none.)
func TestWorkerSweepsEveryWorkPage(t *testing.T) {
	h := newSweepHarness(t)
	families := []string{h.FamilyID}
	h.exec(t, `UPDATE live_activity_family_dirty SET authority_checked_at=clock_timestamp()-interval '1 hour'`)
	versions := map[string]int64{}
	for _, id := range families {
		versions[id] = h.scalar(t, `SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1`, id)
	}
	w := newTestWorker(t, h.Harness, NewDirty(), h.Authority, transportFunc(func(context.Context, PushRequest) (PushResult, error) {
		return PushResult{}, errors.New("no push in this test")
	}))
	w.Queue.workPage = 1
	logs := &logCapture{}
	w.Logger = slog.New(slog.NewTextHandler(logs, nil))
	if err := w.maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range families {
		if got := h.scalar(t, `SELECT dirty_version FROM live_activity_family_dirty WHERE family_id=$1`, id); got != versions[id]+1 {
			t.Errorf("family %s dirty version %d -> %d, want one dirtying", id, versions[id], got)
		}
	}
	if !strings.Contains(logs.String(), " revoked=0 ") || !strings.Contains(logs.String(), " work_pages=2 ") {
		t.Fatalf("sweep log %q, want nothing revoked and work_pages=2", logs.String())
	}
}

// A family whose sequence moves during a pass is never lost: ahead of the
// cursor, this pass dirties it; behind it, the next pass does (its cursor
// lags the sequence), as a change during the one-transaction pass was
// caught by the next one.
func TestSweepWorkPassCatchesAFamilyDirtiedMidPass(t *testing.T) {
	h := newSweepHarness(t)
	for _, id := range []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"} {
		h.clone(t, id)
	}
	// Every cursor at its sequence, checked just now: nothing is due.
	h.exec(t, `UPDATE live_activity_family_dirty d SET journal_sequence=f.next_sequence FROM families f WHERE f.id=d.family_id`)
	rows, err := h.su.Query(t.Context(), `SELECT DISTINCT family_id::text FROM live_activity_registration_index WHERE NOT revoked ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	families, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(families) != 3 || !slices.IsSorted(families) {
		t.Fatalf("families %v (%v)", families, err)
	}
	behind, ahead := families[0], families[2]

	q := NewQueue(h.Maintenance)
	q.workPage = 1
	after, err := q.SweepWorkPage(t.Context(), "")
	if err != nil || after != behind {
		t.Fatalf("first page ended at %q (%v), want %s", after, err, behind)
	}
	// Both move now: one already swept, one not yet.
	h.exec(t, `UPDATE families SET next_sequence=next_sequence+4 WHERE id=$1 OR id=$2`, behind, ahead)
	for pages := 1; after != ""; pages++ {
		if pages > 4 {
			t.Fatal("the pass did not end")
		}
		if after, err = q.SweepWorkPage(t.Context(), after); err != nil {
			t.Fatal(err)
		}
	}
	if journal, sequence := h.journal(t, ahead), h.sequence(t, ahead); journal != sequence {
		t.Fatalf("family ahead of the cursor: cursor %d, sequence %d, want this pass to dirty it", journal, sequence)
	}
	if h.journal(t, behind) == h.sequence(t, behind) {
		t.Fatal("family behind the cursor dirtied by a pass already past it")
	}
	sweepPass(t, q)
	if journal, sequence := h.journal(t, behind), h.sequence(t, behind); journal != sequence {
		t.Fatalf("family behind the cursor: cursor %d, sequence %d after the next pass", journal, sequence)
	}
}
