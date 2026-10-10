package authority_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The fixture clock (2026-10-04) trails the database clock, so every prune
// below that keeps or deletes a row at an app-clock cutoff also fails a prune
// that compared the column with the database's now(): the columns are
// stamped with the app clock (retention ruling).

func TestPruneSessionsKeepsWhatARetryNeeds(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, rawA := f.Session(t, account, device, "account", "", "")
	attempt := newAttempt(t)
	recoverable, err := f.Service.Refresh(t.Context(), refreshOf(f, rawA, attempt, binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	_, _, rawB := f.Session(t, account, device, "account", "", "")
	plain, err := f.Service.Refresh(t.Context(), refreshOf(f, rawB, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	cutoff := f.Now.Add(time.Hour)
	if n, err := f.Service.PruneSessions(t.Context(), cutoff, 100); err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want only the receipt-free consumed parent", n, err)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE id=$1`, plain.Parent.ID); n != 0 {
		t.Fatal("the receipt-free consumed parent survived the prune")
	}
	retry, err := f.Service.Refresh(t.Context(), refreshOf(f, rawA, attempt, binding(account, device)))
	if err != nil || !retry.Recovered || retry.AccessToken != recoverable.AccessToken {
		t.Fatalf("the lost-ack retry after a prune: %+v %v", retry, err)
	}
	f.Exec(t, `DELETE FROM refresh_receipts WHERE child_id=$1`, recoverable.Child.ID) // acknowledged
	if n, err := f.Service.PruneSessions(t.Context(), cutoff, 100); err != nil || n != 1 {
		t.Fatalf("second pass pruned %d (%v)", n, err)
	}
	_, err = f.Service.Refresh(t.Context(), refreshOf(f, rawA, attempt, binding(account, device)))
	wantCode(t, err, "session_revoked") // past the 24-hour window a replay is refused, not answered with reuse revocation (decision D2)
	if row, _ := sessionRow(t, f, recoverable.Child.ID); row.Status != "active" {
		t.Fatalf("an active child was pruned: %s", row.Status)
	}
	if row, _ := sessionRow(t, f, plain.Child.ID); row.Status != "active" {
		t.Fatalf("an active child was pruned: %s", row.Status)
	}
}

// A session a refresh receipt names, as parent or as child, is never pruned
// whatever its status and age: the receipt is what a lost-acknowledgement
// retry recovers from, and deleting either session would cascade it away.
func TestPruneSessionsNeverTakesASessionAReceiptNames(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	_, _, raw := f.Session(t, account, device, "account", "", "")
	rotated, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), binding(account, device)))
	if err != nil {
		t.Fatal(err)
	}
	// Age both ends past every cutoff: the parent consumed, the child
	// revoked and refresh-expired. Only the receipt keeps them.
	f.Exec(t, `UPDATE sessions SET consumed_at=$2 WHERE id=$1`, rotated.Parent.ID, f.Now.Add(-48*time.Hour))
	far := f.Now.Add(400 * 24 * time.Hour)
	if n, err := f.Service.PruneSessions(t.Context(), far, 100); err != nil || n != 0 {
		t.Fatalf("pruned %d (%v) beside a live receipt", n, err)
	}
	f.Exec(t, `UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='device_revoked' WHERE id=$1`, rotated.Child.ID, f.Now)
	if n, err := f.Service.PruneSessions(t.Context(), far, 100); err != nil || n != 0 {
		t.Fatalf("pruned %d (%v) beside a receipt naming a revoked child", n, err)
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts WHERE parent_id=$1 AND child_id=$2`, rotated.Parent.ID, rotated.Child.ID); n != 1 {
		t.Fatal("a prune cascaded into the refresh receipt")
	}
	f.Exec(t, `DELETE FROM refresh_receipts WHERE parent_id=$1`, rotated.Parent.ID)
	if n, err := f.Service.PruneSessions(t.Context(), far, 100); err != nil || n != 2 {
		t.Fatalf("pruned %d (%v) once the receipt went, want parent and child", n, err)
	}
}

// Each kind is pruned strictly before the cutoff, by the column that
// stamped it: consumed_at, revoked_at and, for an active session,
// refresh_expires_at. An active session still refreshable is kept.
func TestPruneSessionsCutsEachKindAtItsOwnColumn(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	stamp := f.Now
	consumed, _, _ := f.Session(t, account, device, "account", "", "")
	f.Exec(t, `UPDATE sessions SET status='consumed', consumed_at=$2, child_id=gen_random_uuid() WHERE id=$1`, consumed, stamp)
	revoked, _, _ := f.Session(t, account, device, "account", "", "")
	f.Exec(t, `UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='device_revoked' WHERE id=$1`, revoked, stamp.Add(time.Minute))
	expired, _, _ := f.Session(t, account, device, "account", "", "")
	f.Exec(t, `UPDATE sessions SET access_expires_at=$2, refresh_expires_at=$2 WHERE id=$1`, expired, stamp.Add(2*time.Minute))
	live, _, _ := f.Session(t, account, device, "account", "", "") // refresh_expires_at = stamp + 30 days
	present := func(id string) bool { return count(t, f, `SELECT count(*) FROM sessions WHERE id=$1`, id) == 1 }
	for _, step := range []struct {
		cutoff time.Time
		gone   []string
	}{
		{stamp, nil}, // strictly before: a session stamped at the cutoff is kept
		{stamp.Add(time.Microsecond), []string{consumed}},
		{stamp.Add(time.Minute), []string{consumed}},
		{stamp.Add(time.Minute + time.Microsecond), []string{consumed, revoked}},
		{stamp.Add(2 * time.Minute), []string{consumed, revoked}},
		{stamp.Add(2*time.Minute + time.Microsecond), []string{consumed, revoked, expired}},
		{stamp.Add(29 * 24 * time.Hour), []string{consumed, revoked, expired}},
	} {
		if _, err := f.Service.PruneSessions(t.Context(), step.cutoff, 100); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{consumed, revoked, expired, live} {
			want := true
			for _, g := range step.gone {
				want = want && g != id
			}
			if present(id) != want {
				t.Fatalf("cutoff %s: session %s present=%v, want %v", step.cutoff.Format(time.RFC3339Nano), id, present(id), want)
			}
		}
	}
}

func TestPruneSessionsIsBoundedAndSkipsLockedRows(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	parents := consumedParents(t, f, account, device, 10)
	held := parents[3]
	holder, err := f.Service.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM sessions WHERE id=$1 FOR UPDATE`, held); err != nil {
		t.Fatal(err)
	}
	cutoff := f.Now.Add(time.Hour)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if n, err := f.Service.PruneSessions(ctx, cutoff, 4); err != nil || n != 4 {
		t.Fatalf("first bounded pass pruned %d (%v), want 4", n, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the prune waited %v behind a held session", elapsed)
	}
	total := 4
	for range 5 {
		n, err := f.Service.PruneSessions(ctx, cutoff, 4)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total != 9 {
		t.Fatalf("repeated passes pruned %d, want the 9 not held", total)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE id=$1`, held); n != 1 {
		t.Fatal("the held session was pruned")
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Service.PruneSessions(ctx, cutoff, 4); err != nil || n != 1 {
		t.Fatalf("after the release pruned %d (%v), want the held one", n, err)
	}
}

// A prune cancelled after its delete and before its COMMIT changes nothing;
// the next one deletes the same rows.
func TestPrunesRolledBackBeforeCommitDeleteNothing(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	consumedParents(t, f, account, device, 3)
	seedChallenge(t, f, f.Now)
	seedReceipt(t, f, account, device)
	later := f.Now.Add(365 * 24 * time.Hour)
	for _, prune := range []struct {
		name  string
		table string
		run   func(context.Context) (int, error)
		want  int
	}{
		{"sessions", `SELECT count(*) FROM sessions WHERE status='consumed'`,
			func(ctx context.Context) (int, error) { return f.Service.PruneSessions(ctx, f.Now.Add(time.Hour), 100) }, 3},
		{"challenges", `SELECT count(*) FROM auth_challenges`,
			func(ctx context.Context) (int, error) { return f.Service.PruneChallenges(ctx, later, 100) }, 1},
		{"refresh receipts", `SELECT count(*) FROM refresh_receipts`,
			func(ctx context.Context) (int, error) { return f.Service.PruneRefreshReceipts(ctx, later, 100) }, 1},
	} {
		t.Run(prune.name, func(t *testing.T) {
			before := count(t, f, prune.table)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ran := false
			authority.SetAfterPruneBatch(f.Service, func() { ran = true; cancel() })
			if _, err := prune.run(ctx); err == nil || !ran {
				t.Fatalf("a prune cancelled before COMMIT reported success (hook ran %v)", ran)
			}
			authority.SetAfterPruneBatch(f.Service, nil)
			if got := count(t, f, prune.table); got != before {
				t.Fatalf("the rolled-back prune deleted %d rows", before-got)
			}
			if n, err := prune.run(t.Context()); err != nil || n != prune.want {
				t.Fatalf("the next prune deleted %d (%v), want %d", n, err, prune.want)
			}
		})
	}
}

// Concurrent prunes of one kind are harmless: each row is deleted once and
// neither errs (sessions and challenges skip each other's rows; receipt
// prunes, which cannot lock, wait and find the row gone).
func TestConcurrentPrunesDeleteEachRowOnce(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	consumedParents(t, f, account, device, 12)
	for i := range 12 {
		seedChallenge(t, f, f.Now.Add(time.Duration(i)*time.Second))
	}
	for range 12 {
		seedReceipt(t, f, account, device)
	}
	later := f.Now.Add(365 * 24 * time.Hour)
	for _, prune := range []struct {
		name, table string
		run         func() (int, error)
	}{
		// Receipts first: their consumed parents then join the session prune.
		{"refresh receipts", `SELECT count(*) FROM refresh_receipts`, func() (int, error) { return f.Service.PruneRefreshReceipts(t.Context(), later, 3) }},
		{"challenges", `SELECT count(*) FROM auth_challenges`, func() (int, error) { return f.Service.PruneChallenges(t.Context(), later, 3) }},
		{"sessions", `SELECT count(*) FROM sessions WHERE status='consumed'`,
			func() (int, error) { return f.Service.PruneSessions(t.Context(), f.Now.Add(time.Hour), 3) }},
	} {
		t.Run(prune.name, func(t *testing.T) {
			before := count(t, f, prune.table)
			var mu sync.Mutex
			deleted := 0
			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					for range 10 {
						n, err := prune.run()
						if err != nil {
							t.Errorf("concurrent prune: %v", err)
							return
						}
						mu.Lock()
						deleted += n
						mu.Unlock()
					}
				})
			}
			wg.Wait()
			if left := count(t, f, prune.table); left != 0 || deleted != before {
				t.Fatalf("%d of %d rows reported deleted, %d left", deleted, before, left)
			}
		})
	}
}

func TestPruneChallengesAndRefreshReceipts(t *testing.T) {
	f := authoritytest.New(t)
	// A fixed clock months behind the database's, so every cutoff below,
	// receipts' included, is in the database's past: a prune comparing with
	// now() deletes at the "kept" steps, whatever day the test runs.
	f.Now = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	expiring := seedChallenge(t, f, f.Now)            // expires f.Now + 5 min
	live := seedChallenge(t, f, f.Now.Add(time.Hour)) // expires f.Now + 65 min
	due := f.Now.Add(5 * time.Minute)
	if n, err := f.Service.PruneChallenges(t.Context(), due.Add(-time.Microsecond), 100); err != nil || n != 0 {
		t.Fatalf("pruned %d challenges (%v) before their expiry", n, err)
	}
	if n, err := f.Service.PruneChallenges(t.Context(), due, 100); err != nil || n != 1 {
		t.Fatalf("pruned %d challenges (%v) at the expiry, want 1", n, err)
	}
	if count(t, f, `SELECT count(*) FROM auth_challenges WHERE token_digest=$1`, expiring[:]) != 0 ||
		count(t, f, `SELECT count(*) FROM auth_challenges WHERE token_digest=$1`, live[:]) != 1 {
		t.Fatal("the prune took the wrong challenge")
	}
	// Receipts fall due at LEAST(refresh_expires_at, effective_deadline):
	// one whose lineage's year cap ends in 10 days (effective_deadline
	// first) and one whose child refresh expiry, 30 days, comes first.
	capped := seedReceiptWith(t, f, account, device, f.Now.Add(-355*24*time.Hour))
	plain := seedReceipt(t, f, account, device)
	cappedDue, plainDue := f.Now.Add(10*24*time.Hour), f.Now.Add(30*24*time.Hour)
	if got := receiptDue(t, f, capped); !got.Equal(cappedDue) {
		t.Fatalf("capped receipt due %s, want %s", got, cappedDue)
	}
	for _, step := range []struct {
		now  time.Time
		want int
		gone []string
	}{
		{cappedDue.Add(-time.Microsecond), 0, nil},
		{cappedDue, 1, []string{capped}},
		{plainDue.Add(-time.Microsecond), 0, []string{capped}},
		{plainDue, 1, []string{capped, plain}},
	} {
		if n, err := f.Service.PruneRefreshReceipts(t.Context(), step.now, 100); err != nil || n != step.want {
			t.Fatalf("at %s pruned %d receipts (%v), want %d", step.now.Format(time.RFC3339Nano), n, err, step.want)
		}
		for _, parent := range []string{capped, plain} {
			want := 1
			for _, g := range step.gone {
				if g == parent {
					want = 0
				}
			}
			if got := count(t, f, `SELECT count(*) FROM refresh_receipts WHERE parent_id=$1`, parent); got != want {
				t.Fatalf("at %s receipt of %s: %d rows, want %d", step.now.Format(time.RFC3339Nano), parent, got, want)
			}
		}
	}
}

// No prune locks an account, family or device row, or waits on one: while
// each prune's transaction is open (after its statements, before COMMIT) the
// only relation locks it holds are on the swept tables, and a writer holding
// the account row does not delay it.
func TestPrunesLockNoAccountOrFamily(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	f.Family(t, account)
	consumedParents(t, f, account, device, 2)
	seedChallenge(t, f, f.Now)
	seedReceipt(t, f, account, device)
	holder, err := f.Service.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM accounts WHERE id=$1 FOR UPDATE`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM families WHERE owner_account_id=$1 FOR UPDATE`, account); err != nil {
		t.Fatal(err)
	}
	var holderPID int
	if err := holder.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	later := f.Now.Add(365 * 24 * time.Hour)
	for _, prune := range []struct {
		name  string
		swept string
		run   func(context.Context) (int, error)
	}{
		{"sessions", "sessions", func(ctx context.Context) (int, error) { return f.Service.PruneSessions(ctx, f.Now.Add(time.Hour), 100) }},
		{"challenges", "auth_challenges", func(ctx context.Context) (int, error) { return f.Service.PruneChallenges(ctx, later, 100) }},
		{"refresh receipts", "refresh_receipts", func(ctx context.Context) (int, error) { return f.Service.PruneRefreshReceipts(ctx, later, 100) }},
	} {
		t.Run(prune.name, func(t *testing.T) {
			var locked []string
			authority.SetAfterPruneBatch(f.Service, func() {
				rows, err := f.DB.Owner.Query(context.Background(), `SELECT DISTINCT c.relname::text FROM pg_locks l JOIN pg_class c ON c.oid=l.relation
					WHERE l.locktype='relation' AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND l.pid NOT IN (pg_backend_pid(), $1) AND c.relkind IN ('r','p') AND c.relnamespace='public'::regnamespace
					  AND l.pid IN (SELECT pid FROM pg_stat_activity WHERE backend_type='client backend') ORDER BY 1`, holderPID)
				if err != nil {
					t.Error(err)
					return
				}
				locked, err = pgx.CollectRows(rows, pgx.RowTo[string])
				if err != nil {
					t.Error(err)
				}
			})
			defer authority.SetAfterPruneBatch(f.Service, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			start := time.Now()
			if n, err := prune.run(ctx); err != nil || n == 0 {
				t.Fatalf("pruned %d (%v)", n, err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("the prune waited %v behind an account writer", elapsed)
			}
			if !strings.Contains(strings.Join(locked, ","), prune.swept) {
				t.Fatalf("the hook saw no lock on %s (%v): the check is vacuous", prune.swept, locked)
			}
			for _, relation := range locked {
				if relation != prune.swept && relation != "refresh_receipts" {
					t.Fatalf("the %s prune holds a lock on %s (all: %v)", prune.name, relation, locked)
				}
			}
		})
	}
}

// Every prune runs with the authority flag, statement_timeout 10s and a
// lock_timeout of 200ms, below the server's 1s deadlock_timeout: a prune
// that would close a lock cycle with a request gives up (55P03) before the
// deadlock detector runs, so the request never sees 40P01. All set in its
// BEGIN round trip and local to its transaction.
func TestPrunesRunUnderTheWorkerTimeouts(t *testing.T) {
	f := authoritytest.New(t)
	tx, err := f.Pool.BeginTx(t.Context(), pgx.TxOptions{BeginQuery: authority.PruneBegin})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var lock, statement, flag string
	if err := tx.QueryRow(t.Context(), `SELECT current_setting('lock_timeout'), current_setting('statement_timeout'), current_setting('app.authority')`).
		Scan(&lock, &statement, &flag); err != nil {
		t.Fatal(err)
	}
	if lock != "200ms" || statement != "10s" || flag != "1" {
		t.Fatalf("prune transaction: lock_timeout %s, statement_timeout %s, authority %q", lock, statement, flag)
	}
	// A receipt another transaction is deleting is waited for at most the
	// lock timeout (the receipt prune cannot skip it).
	account := liveAccount(t, f)
	device := f.Device(t, account)
	parent := seedReceipt(t, f, account, device)
	holder, err := f.Service.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(t.Context(), `DELETE FROM refresh_receipts WHERE parent_id=$1`, parent); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	start := time.Now()
	_, err = f.Service.PruneRefreshReceipts(ctx, f.Now.Add(365*24*time.Hour), 100)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("behind a held receipt: %v, want 55P03", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 900*time.Millisecond {
		t.Fatalf("the receipt prune gave up after %v, want its 200ms lock timeout (under the 1s deadlock_timeout)", elapsed)
	}
}

// The receipt prune cannot skip locked receipts, so it can close a lock
// cycle with a request deleting due receipts in the opposite order (a reuse
// revocation deletes the lineage's receipts, then the account's dead ones,
// in two statements). The prune yields: it gives up with 55P03 at its 200ms
// lock timeout, before the 1s deadlock detector would abort either side, and
// the request completes.
//
// The prune's lock order follows the hash of the parent IDs (its plan
// de-duplicates the due IDs with a HashAggregate, which is also why no ORDER
// BY can fix the order), so each attempt checks that the prune already holds
// the other receipt before the request closes the cycle, and reseeds if the
// prune reached the request's receipt first.
func TestReceiptPruneYieldsToARequestInsteadOfDeadlocking(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	for attempt := 1; ; attempt++ {
		if attempt > 20 {
			t.Fatal("the prune never held the other receipt while waiting on the request's")
		}
		if receiptCycleYields(t, f, account, device) {
			return
		}
	}
}

// receiptCycleYields runs one attempt: false when the prune reached the
// request's receipt before the other one (no cycle could form).
func receiptCycleYields(t *testing.T, f *authoritytest.Fixture, account, device string) bool {
	t.Helper()
	other := seedReceiptWith(t, f, account, device, f.Now.Add(-355*24*time.Hour))
	held := seedReceipt(t, f, account, device)
	request, err := f.Service.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer request.Rollback(context.Background())
	if _, err := request.Exec(t.Context(), `DELETE FROM refresh_receipts WHERE parent_id=$1`, held); err != nil {
		t.Fatal(err)
	}
	pruned := make(chan error, 1)
	go func() {
		_, err := f.Service.PruneRefreshReceipts(context.Background(), f.Now.Add(365*24*time.Hour), 100)
		pruned <- err
	}()
	// Wait until the prune waits on the request's receipt.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := f.DB.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'DELETE FROM refresh_receipts WHERE parent_id IN%'`).
			Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the prune never waited on the request's receipt")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !lockedByAnother(t, f, other) {
		// The prune reached the request's receipt first: no cycle. Let it
		// finish (it deletes both once the request is gone).
		if err := request.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-pruned; err != nil {
			t.Fatalf("the prune after the request rolled back: %v", err)
		}
		return false
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := request.Exec(ctx, `DELETE FROM refresh_receipts WHERE parent_id=$1`, other); err != nil {
		t.Fatalf("the request closing the cycle: %v, want it to complete", err)
	}
	if err := request.Commit(ctx); err != nil {
		t.Fatalf("the request's commit: %v", err)
	}
	var pgErr *pgconn.PgError
	if err := <-pruned; !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("the prune: %v, want 55P03 (it yields), never 40P01", err)
	}
	if n := count(t, f, `SELECT count(*) FROM refresh_receipts`); n != 0 {
		t.Fatalf("%d receipts left after the request deleted both", n)
	}
	return true
}

// lockedByAnother reports whether another transaction holds the receipt's
// row lock (here: the prune, which deleted it). It asks as the table owner
// with NOWAIT and releases at once.
func lockedByAnother(t *testing.T, f *authoritytest.Fixture, parent string) bool {
	t.Helper()
	probe, err := f.DB.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Rollback(context.Background())
	if _, err := probe.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	_, err = probe.Exec(t.Context(), `SELECT 1 FROM refresh_receipts WHERE parent_id=$1 FOR UPDATE NOWAIT`, parent)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	return false
}

// The prune statements read their retention indexes, in custom and generic
// plans, and never scan the swept table whole.
func TestPruneStatementsUseTheirIndexes(t *testing.T) {
	f := authoritytest.New(t)
	account := liveAccount(t, f)
	device := f.Device(t, account)
	base := f.Now
	// 20,000 sessions: 1% consumed, 1% revoked, 1% refresh-expired, the rest
	// live; 2,000 receipts (session i is the parent, i + 10,000 the child),
	// 1% due; 20,000 challenges, 1% expired.
	f.Exec(t, `INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,recovery_epoch,issued_seq,
			token_key_id,refresh_digest,access_expires_at,refresh_expires_at,deadline_at,status,consumed_at,child_id,revoked_at,revoke_reason,created_at)
		SELECT sha256(('a'||i)::bytea), md5('s'||i)::uuid, $1, $2, md5('s'||i)::uuid, $3::timestamptz, 'account', 1, 1, 'session.v1', sha256(('r'||i)::bytea),
			$3::timestamptz + CASE WHEN i % 100 = 2 THEN interval '1 minute' ELSE interval '1 hour' END,
			$3::timestamptz + CASE WHEN i % 100 = 2 THEN interval '1 minute' ELSE interval '30 days' END + (i * interval '1 second'),
			$3::timestamptz + interval '365 days',
			CASE i % 100 WHEN 0 THEN 'consumed' WHEN 1 THEN 'revoked' ELSE 'active' END::app.session_status,
			CASE WHEN i % 100 = 0 THEN $3::timestamptz - (i * interval '1 second') END,
			CASE WHEN i % 100 = 0 THEN md5('c'||i)::uuid END,
			CASE WHEN i % 100 = 1 THEN $3::timestamptz - (i * interval '1 second') END,
			CASE WHEN i % 100 = 1 THEN 'device_revoked'::app.revoke_reason END,
			$3::timestamptz
		FROM generate_series(1, 20000) i`, account, device, base)
	f.Exec(t, `INSERT INTO refresh_receipts(parent_id,child_id,attempt_id,account_id,device_id,material,material_digest,created_at,effective_deadline,refresh_expires_at)
		SELECT md5('s'||i)::uuid, md5('s'||(i+10000))::uuid, md5('t'||i)::uuid, $1, $2, 'm'::bytea, sha256('m'::bytea), $3::timestamptz, $3::timestamptz + interval '365 days',
			$3::timestamptz + CASE WHEN i % 100 = 0 THEN interval '1 minute' ELSE interval '30 days' END
		FROM generate_series(1, 2000) i`, account, device, base)
	f.Exec(t, `INSERT INTO auth_challenges(token_digest,expires_at,created_at)
		SELECT sha256(('ch'||i)::bytea), $1::timestamptz + CASE WHEN i % 100 = 0 THEN interval '1 minute' ELSE interval '1 day' END, $1::timestamptz
		FROM generate_series(1, 20000) i`, base)
	for _, table := range []string{"sessions", "refresh_receipts", "auth_challenges"} {
		f.Exec(t, `ANALYZE `+table)
	}
	cutoff := base.Add(time.Hour)
	for _, c := range []struct {
		name, sql, table, index string
		args                    []any
	}{
		{"consumed", authority.PruneSQL["consumed"], "sessions", "sessions_consumed_idx", []any{cutoff, 5000}},
		{"revoked", authority.PruneSQL["revoked"], "sessions", "sessions_revoked_idx", []any{cutoff, 5000}},
		{"refresh-expired", authority.PruneSQL["expired"], "sessions", "sessions_expired_idx", []any{cutoff, 5000}},
		{"challenges", authority.PruneSQL["challenges"], "auth_challenges", "auth_challenges_expiry_idx", []any{cutoff, 5000}},
		{"refresh receipts", authority.PruneSQL["receipts"], "refresh_receipts", "refresh_receipts_expiry_idx", []any{cutoff, 2000}},
	} {
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				inAuthorityTx(t, f, func(tx pgx.Tx) {
					if _, err := tx.Exec(t.Context(), `SELECT set_config('plan_cache_mode', $1, true)`, mode); err != nil {
						t.Fatal(err)
					}
					var raw string
					if err := tx.QueryRow(t.Context(), `EXPLAIN (FORMAT JSON) `+c.sql, c.args...).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var plans []struct{ Plan pruneNode }
					if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) != 1 {
						t.Fatalf("plan %s: %v", raw, err)
					}
					usesIndex := false
					plans[0].Plan.walk(func(n pruneNode) {
						if n.RelationName == c.table && n.NodeType == "Seq Scan" {
							t.Fatalf("a sequential scan of %s: %s", c.table, raw)
						}
						if n.IndexName == c.index {
							usesIndex = true
						}
					})
					if !usesIndex {
						t.Fatalf("the plan does not read %s: %s", c.index, raw)
					}
				})
			})
		}
	}
}

type pruneNode struct {
	NodeType     string      `json:"Node Type"`
	RelationName string      `json:"Relation Name"`
	IndexName    string      `json:"Index Name"`
	Plans        []pruneNode `json:"Plans"`
}

func (n pruneNode) walk(visit func(pruneNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}

// consumedParents rotates n fresh sessions without recovery (no receipt) and
// returns their consumed parents' IDs.
func consumedParents(t *testing.T, f *authoritytest.Fixture, account, device string, n int) []string {
	t.Helper()
	parents := make([]string, n)
	for i := range parents {
		id, _, raw := f.Session(t, account, device, "account", "", "")
		if _, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, "", nil)); err != nil {
			t.Fatal(err)
		}
		parents[i] = id
	}
	return parents
}

// seedChallenge stores a challenge created at at (it expires 5 minutes later).
func seedChallenge(t *testing.T, f *authoritytest.Fixture, at time.Time) [32]byte {
	t.Helper()
	digest := sha256.Sum256([]byte(authoritytest.NewID(t)))
	if err := f.Service.CreateChallenge(t.Context(), digest, at.Add(5*time.Minute), at); err != nil {
		t.Fatal(err)
	}
	return digest
}

// seedReceipt rotates a fresh session recoverably and returns the parent ID
// its receipt is keyed by.
func seedReceipt(t *testing.T, f *authoritytest.Fixture, account, device string) string {
	t.Helper()
	return seedReceiptWith(t, f, account, device, time.Time{})
}

// seedReceiptWith is seedReceipt for a lineage that started at
// lineageStarted (zero: now), which caps the self-hosted effective deadline.
func seedReceiptWith(t *testing.T, f *authoritytest.Fixture, account, device string, lineageStarted time.Time) string {
	t.Helper()
	id, _, raw := f.Session(t, account, device, "account", "", "")
	if !lineageStarted.IsZero() {
		f.Exec(t, `UPDATE sessions SET lineage_started_at=$2 WHERE id=$1`, id, lineageStarted)
	}
	if _, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, newAttempt(t), binding(account, device))); err != nil {
		t.Fatal(err)
	}
	return id
}

func receiptDue(t *testing.T, f *authoritytest.Fixture, parent string) time.Time {
	t.Helper()
	var due time.Time
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		if err := tx.QueryRow(t.Context(), `SELECT LEAST(refresh_expires_at, effective_deadline) FROM refresh_receipts WHERE parent_id=$1`, parent).Scan(&due); err != nil {
			t.Fatal(err)
		}
	})
	return due.UTC()
}

// FamilyPurgeWork lists the purges to drive: started ones first, each with
// its persisted purge_command_id (a crash resumes a started purge from it),
// then deletion-pending families whose purge_after is at or before now, by
// family ID; never an active, a not-yet-due or a completed one; at most
// limit.
func TestFamilyPurgeWork(t *testing.T) {
	f := authoritytest.New(t)
	scheduled := f.Now
	var pending []string
	for range 3 {
		owner := newMember(t, f)
		family := createFamily(t, f, f.Service, owner)
		if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
			t.Fatal(err)
		}
		pending = append(pending, family)
	}
	createFamily(t, f, f.Service, newMember(t, f)) // active: never listed
	f.Now = scheduled.Add(time.Hour)
	late := newMember(t, f)
	lateFamily := createFamily(t, f, f.Service, late) // due an hour after the others
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, late.account, late.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	slices.Sort(pending)
	due := scheduled.Add(purgeDelay)

	if work, err := f.Service.FamilyPurgeWork(t.Context(), due.Add(-time.Microsecond), 10); err != nil || len(work) != 0 {
		t.Fatalf("work before any purge_after: %+v %v", work, err)
	}
	work, err := f.Service.FamilyPurgeWork(t.Context(), due, 10)
	want := []authority.PurgeWork{{FamilyID: pending[0]}, {FamilyID: pending[1]}, {FamilyID: pending[2]}}
	if err != nil || !slices.Equal(work, want) {
		t.Fatalf("work at purge_after %+v %v, want the three due families by ID, none started", work, err)
	}
	if work, err := f.Service.FamilyPurgeWork(t.Context(), due, 2); err != nil || !slices.Equal(work, want[:2]) {
		t.Fatalf("work limited to 2: %+v %v", work, err)
	}
	// The purge's quarantine is left out.
	if work, err := f.Service.FamilyPurgeWork(t.Context(), due, 2, pending[0], pending[2]); err != nil || !slices.Equal(work, want[1:2]) {
		t.Fatalf("work without the quarantined families: %+v %v, want %+v", work, err, want[1:2])
	}
	if _, err := f.Service.FamilyPurgeWork(t.Context(), due, 2, "not-a-uuid"); !errors.Is(err, authority.ErrInvalidCommand) {
		t.Fatalf("an invalid excluded ID: %v, want invalid_command", err)
	}

	// The highest ID starts first: a started purge leads, with its command.
	start := authority.StartFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: pending[2], At: due}
	if _, err := f.Service.StartFamilyPurge(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	work, err = f.Service.FamilyPurgeWork(t.Context(), due.Add(time.Hour), 10)
	want = []authority.PurgeWork{{FamilyID: pending[2], StartCommandID: start.CommandID}, {FamilyID: pending[0]}, {FamilyID: pending[1]}, {FamilyID: lateFamily}}
	slices.SortFunc(want[1:], func(a, b authority.PurgeWork) int { return strings.Compare(a.FamilyID, b.FamilyID) })
	if err != nil || !slices.Equal(work, want) {
		t.Fatalf("work with a started purge %+v %v, want %+v", work, err, want)
	}
	// A started purge is listed whatever the clock: it resumes.
	if work, err := f.Service.FamilyPurgeWork(t.Context(), scheduled, 1); err != nil || !slices.Equal(work, want[:1]) {
		t.Fatalf("started purge before any purge_after %+v %v", work, err)
	}
	// A completed purge is never listed.
	if _, err := f.Service.CompleteFamilyPurge(t.Context(), authority.CompleteFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: pending[2],
		StartCommandID: start.CommandID, Receipt: authoritytest.NewID(t), At: due}); err != nil {
		t.Fatal(err)
	}
	if work, err := f.Service.FamilyPurgeWork(t.Context(), due.Add(time.Hour), 10); err != nil || !slices.Equal(work, want[1:]) {
		t.Fatalf("work after the purge completed %+v %v, want %+v", work, err, want[1:])
	}
	for _, limit := range []int{0, -1, 1001} {
		if _, err := f.Service.FamilyPurgeWork(t.Context(), due, limit); !errors.Is(err, authority.ErrInvalidCommand) {
			t.Errorf("limit %d: %v, want invalid_command", limit, err)
		}
	}
	if _, err := f.Service.FamilyPurgeWork(t.Context(), time.Time{}, 10); !errors.Is(err, authority.ErrInvalidCommand) {
		t.Errorf("zero clock: %v, want invalid_command", err)
	}
}
