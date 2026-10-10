package maintenance_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/maintenance"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
	"github.com/jackc/pgx/v5/pgconn"
)

// discardSender discards wakes.
type discardSender struct{}

func (discardSender) Wake(context.Context, []wake.Wake) error { return nil }

// Review Focus 3: workers and requests never deadlock and no request waits
// out a lock. Family A takes concurrent sync writes and role changes while
// the receipt sweep, the session sweep and the wake worker run over A; an
// account outside it rotates recoverably (each rotation a recovery receipt)
// and replays the consumed token (a reuse revocation deleting receipts)
// while the session sweep prunes those receipts (Task 9 M1). Family B is
// deletion pending with rows and receipts; its purge runs while the receipt
// sweep also works on B. No request fails at all (40P01, 55P03 or any
// refusal) and no worker sees a deadlock; a worker may yield behind a lock
// (55P03, its lock_timeout below deadlock_timeout), never a request. Both
// families' counters equal their rows after.
func TestMaintenanceAndRequestsNeverDeadlock(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	viewer := h.AddMember(t, "viewer", "viewer")
	// Family B: the outsider's own family, given 20 sync batches (rows and
	// receipts) and 1,000 device cursors, then scheduled for deletion, so the
	// purge (its cursor step locking cursors in the wake flush's key order
	// included) and the receipt sweep both work on it while family A takes
	// writes.
	familyB := deletionPendingFamily(t, h, 1000)
	owner := mustDigests(t, h, "owner")
	far := h.Now.Add(60 * 24 * time.Hour) // past B's purge_after and every receipt's 30 days
	// The recovery-receipt prune's clock: every recovery receipt minted
	// during the run (refresh expiry = now + the refresh TTL) is due there.
	// The rest of the session sweep runs at h.Now (a later clock would
	// prune the sign-ins' live challenges and the consumed parents the
	// reuse replays).
	refreshTTL := time.Duration(authorityCount(t, h, `SELECT extract(epoch FROM max(refresh_expires_at) - $2::timestamptz)::bigint FROM sessions
		WHERE account_id=$1 AND status='active'`, h.Owner.AccountID, h.Now)) * time.Second
	if refreshTTL <= 24*time.Hour {
		t.Fatalf("refresh TTL %v: the session sweep's clock cannot make receipts due without pruning family A's sessions", refreshTTL)
	}
	sessionClock := h.Now.Add(refreshTTL + time.Minute)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var failures, yields atomic.Int32
	done := map[string]*atomic.Int32{}
	for _, name := range []string{"writer A", "role change", "reuse revocation", "receipts", "sessions", "receipt prune", "purge", "wake"} {
		done[name] = &atomic.Int32{}
	}
	// classify records one call's outcome. Any request error is a failure.
	// A worker's 55P03 is a yield (its lock_timeout, below deadlock_timeout,
	// gave up first; the next run retries); any other worker error,
	// 40P01 included, is a failure.
	classify := func(where string, request bool, err error) {
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			done[where].Add(1)
			return
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil:
			return
		case !request && errors.As(err, &pgErr) && pgErr.Code == "55P03":
			yields.Add(1)
			t.Logf("%s yielded: %s", where, pgErr.Code)
			return
		case errors.As(err, &pgErr):
			t.Logf("%s: %s %s", where, pgErr.Code, pgErr.Message)
		default:
			t.Logf("%s: %v", where, err)
		}
		failures.Add(1)
	}
	tracker := wake.NewTracker(0)
	writerDevices := []string{h.Owner.DeviceID, h.Caregiver.DeviceID}
	var wg sync.WaitGroup
	for writer := range 2 {
		wg.Go(func() {
			for n := 0; ctx.Err() == nil; n++ {
				request := h.SyncRequest(nil, nil)
				request.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 200000*(writer+1)+n)}
				response, err := h.Service.ServeSync(ctx, authority.Request{Digests: owner}, request)
				if err == nil && (len(response.Outcomes) != 1 || response.Outcomes[0].Status != "applied") {
					err = fmt.Errorf("outcomes %+v", response.Outcomes)
				}
				classify("writer A", true, err)
				// The request path's progress, as production records it: the
				// wake worker flushes it into A's device cursors.
				tracker.Synced(h.FamilyID, writerDevices[writer], uint64(n), uint64(n), h.Now)
				tracker.Changed(h.FamilyID, uint64(n))
			}
		})
	}
	wg.Go(func() {
		for role := "caregiver"; ctx.Err() == nil; role = map[string]string{"caregiver": "viewer", "viewer": "caregiver"}[role] {
			_, err := h.Family.ChangeRole(ctx, h.Owner, h.CommandID(), viewer.AccountID, role)
			classify("role change", true, err)
		}
	})
	wg.Go(func() {
		for n := 0; ctx.Err() == nil; n++ {
			classify("reuse revocation", true, rotateAndReuse(ctx, h, n))
		}
	})
	receipts := &maintenance.ReceiptSweep{Pool: h.Maintenance, Batch: 50}
	sessions := &maintenance.SessionSweep{Authority: h.Authority}
	// B's purge spans the window: small statements, each holding B's family
	// row and the rows it deleted 60 ms before its commit, beside the
	// receipt sweep on B.
	purger := &maintenance.Purger{Authority: h.Authority, Pool: h.Maintenance, Batch: 25}
	var purgeStatements, cursorStatements atomic.Int32
	var completedAfter atomic.Int64
	began := time.Now()
	purger.FailAt(func(step string, _ int) error {
		switch step {
		case "start", "complete":
		case "completed":
			completedAfter.Store(int64(time.Since(began)))
		default:
			purgeStatements.Add(1)
			if step == "device_sync_cursors" {
				cursorStatements.Add(1)
			}
			time.Sleep(60 * time.Millisecond)
		}
		return nil
	})
	worker := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Now: func() time.Time { return far }}
	var receiptsRemoved, sessionsRemoved, purged atomic.Int64
	wg.Go(func() {
		for ctx.Err() == nil {
			n, err := receipts.Run(ctx, far)
			receiptsRemoved.Add(int64(n))
			classify("receipts", false, err)
		}
	})
	wg.Go(func() {
		for ctx.Err() == nil {
			n, err := sessions.Run(ctx, h.Now)
			sessionsRemoved.Add(int64(n))
			classify("sessions", false, err)
		}
	})
	// The session sweep's receipt prune at a clock where the reuser's
	// recovery receipts are due (at h.Now none is), beside the reuse
	// revocations deleting the same receipts.
	var receiptPrunes atomic.Int64
	wg.Go(func() {
		for ctx.Err() == nil {
			n, err := h.Authority.PruneRefreshReceipts(ctx, sessionClock, maintenance.RefreshReceiptBatch)
			receiptPrunes.Add(int64(n))
			classify("receipt prune", false, err)
		}
	})
	wg.Go(func() {
		for ctx.Err() == nil {
			n, err := purger.Run(ctx, far)
			purged.Add(int64(n))
			classify("purge", false, err)
		}
	})
	wg.Go(func() {
		for ctx.Err() == nil {
			_, err := worker.Tick(ctx)
			classify("wake", false, err)
		}
	})
	wg.Wait()
	for name, n := range done {
		t.Logf("%s: %d calls completed", name, n.Load())
		if n.Load() == 0 {
			t.Errorf("%s never completed a call: the run proves nothing about it", name)
		}
	}
	t.Logf("%d worker yields (55P03); removed: %d care receipts, %d session-sweep rows, %d recovery receipts, %d purged rows", yields.Load(),
		receiptsRemoved.Load(), sessionsRemoved.Load(), receiptPrunes.Load(), purged.Load())
	if receiptsRemoved.Load() == 0 || receiptPrunes.Load() == 0 || purged.Load() == 0 {
		t.Error("a sweep removed nothing during the run: it did not contend with the requests")
	}
	t.Logf("purge statements during the run: %d (%d on B's cursors); B completed after %v (0: not in the window)", purgeStatements.Load(),
		cursorStatements.Load(), time.Duration(completedAfter.Load()))
	if purgeStatements.Load() < 30 || cursorStatements.Load() == 0 || (completedAfter.Load() != 0 && completedAfter.Load() < int64(2*time.Second)) {
		t.Errorf("B's purge ran %d statements (%d on cursors), done after %v: want it spanning most of the run", purgeStatements.Load(),
			cursorStatements.Load(), time.Duration(completedAfter.Load()))
	}
	if n := maintenanceCount(t, h, "device_sync_cursors", h.FamilyID); n != len(writerDevices) {
		t.Errorf("A holds %d device cursors after the run, want the writers' %d: the wake leg flushed nothing", n, len(writerDevices))
	}
	if failures.Load() != 0 {
		t.Fatalf("%d failures (deadlocks, refused or failed requests, worker errors) between workers and requests", failures.Load())
	}
	if status := familyStatus(t, h, familyB); status == "deletion_pending" {
		t.Fatal("B's purge never started during the run")
	}
	caretest.AssertCounters(t, h, h.FamilyID)
	// The purge zeroes B's counters in the transaction that finds its last
	// row gone, so they equal B's rows once it completes, whichever worker
	// deleted which receipt first.
	for tick := 0; familyStatus(t, h, familyB) != "purge_completed"; tick++ {
		if tick > 500 {
			t.Fatal("B's purge did not finish")
		}
		if _, err := purger.Run(t.Context(), far); err != nil {
			t.Fatal(err)
		}
	}
	caretest.AssertCounters(t, h, familyB)
	for _, table := range purgeTables {
		if n := maintenanceCount(t, h, table, familyB); n != 0 {
			t.Fatalf("%s holds %d rows of purged B", table, n)
		}
	}
}

// rotateAndReuse signs the reuser in on two new devices and rotates each
// new session recoverably (a recovery receipt each, due at the session
// sweep's clock), then replays the first consumed refresh token: the reuse
// revokes that lineage and deletes its receipts and the account's dead ones,
// while the second receipt is left for the prune. The replay's
// refresh_reused is the expected answer.
func rotateAndReuse(ctx context.Context, h *testkit.Harness, n int) error {
	var first string
	for device := range 2 {
		bundle, err := h.SignInDevice(ctx, "reuser")
		if err != nil {
			return fmt.Errorf("sign in: %w", err)
		}
		if _, err := h.Auth.RefreshRecoverable(ctx, auth.RecoverableRefreshInput{RefreshToken: bundle.RefreshToken,
			AttemptID:        fmt.Sprintf("7a000000-0000-4000-8000-%011d%d", n+1, device),
			DeviceContinuity: identity.DeviceContinuityClaim{DeviceID: bundle.DeviceID, Credential: bundle.DeviceCredential}}); err != nil {
			return fmt.Errorf("recoverable rotation: %w", err)
		}
		if device == 0 {
			first = bundle.RefreshToken
		}
	}
	if _, err := h.Auth.Refresh(ctx, first); !errors.Is(err, authority.ErrRefreshReused) && !errors.Is(err, auth.ErrRefreshReused) {
		return fmt.Errorf("reuse: %v, want refresh_reused", err)
	}
	return nil
}

// deletionPendingFamily is the outsider's own family B with 20 sync
// batches of babies and feeds (rows and receipts) and cursors device cursors
// (flushed by a wake worker while B is active), scheduled for deletion.
func deletionPendingFamily(t *testing.T, h *testkit.Harness, cursors int) string {
	t.Helper()
	created, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "B")
	if err != nil {
		t.Fatal(err)
	}
	outsider := h.Refresh("outsider")
	digests := mustDigests(t, h, "outsider")
	for batch := range 20 {
		baby := fmt.Sprintf("1b000000-0000-4000-8000-%012d", batch+1)
		request := h.SyncRequest(nil, nil)
		request.Mutations = []protocol.Mutation{caretest.Baby(baby), caretest.Feed(baby, 1), caretest.Feed(baby, 2), caretest.Feed(baby, 3)}
		response, err := h.Service.ServeSync(t.Context(), authority.Request{Digests: digests}, request)
		if err != nil {
			t.Fatal(err)
		}
		for _, outcome := range response.Outcomes {
			if outcome.Status != "applied" {
				t.Fatalf("B batch %d: %+v", batch, outcome)
			}
		}
	}
	if n := maintenanceCount(t, h, "care_receipts", created.ID); n != 80 {
		t.Fatalf("B holds %d receipts, want 80", n)
	}
	caretest.AssertCounters(t, h, created.ID)
	if cursors > 0 {
		tracker := wake.NewTracker(0)
		for i := range cursors {
			tracker.Synced(created.ID, fmt.Sprintf("d1000000-0000-4000-8000-%012d", i+1), 1, 1, h.Now)
		}
		worker := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Now: func() time.Time { return h.Now }}
		if _, err := worker.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if n := maintenanceCount(t, h, "device_sync_cursors", created.ID); n != cursors {
			t.Fatalf("B holds %d cursors, want %d", n, cursors)
		}
	}
	if _, err := h.Family.ScheduleDeletion(t.Context(), outsider, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// mustDigests are subject's access token digests, as a request presents
// them.
func mustDigests(t *testing.T, h *testkit.Harness, subject string) []authority.KeyedDigest {
	t.Helper()
	digests, err := h.Auth.AccessDigests(h.AccessToken(subject))
	if err != nil {
		t.Fatal(err)
	}
	return digests
}
