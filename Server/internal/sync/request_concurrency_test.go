package sync_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/family"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Review Focus 4: sync batches (which lock the family row), pulls and
// snapshot pages, each one authenticated transaction, run while ChangeRole
// (accounts, then the family) and Refresh (an account's session) commit.
// The refreshed account is itself sending batches, pulls and snapshot pages
// with its latest token while its parent session is consumed, and the
// snapshot legs page through many continuation pages (page size 3 over 60
// seeded rows). No request takes an account or session row lock, so no
// cycle can form: nothing deadlocks and nothing waits out a lock timeout.
func TestOneTransactionRequestsNeverDeadlockWithCommands(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 3
	h.RebuildSyncService(t, h.Limits)
	seed := []protocol.Mutation{caretest.Baby(caretest.BabyA)}
	for n := range 60 {
		seed = append(seed, caretest.Feed(caretest.BabyA, 900000+n))
	}
	caretest.Seed(t, h, seed...)
	viewer := h.AddMember(t, "viewer", "viewer")
	h.AddMember(t, "refresher", "caregiver")
	digests := func(subject string) []authority.KeyedDigest {
		d, err := h.Auth.AccessDigests(h.AccessToken(subject))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	// Read before any goroutine; the bundles map is not shared.
	owner, caregiver, viewerDigests := digests("owner"), digests("caregiver"), digests("viewer")
	var refresherDigests atomic.Pointer[[]authority.KeyedDigest]
	first := digests("refresher")
	refresherDigests.Store(&first)
	refreshToken := h.RefreshToken("refresher")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	var deadlocks, lockTimeouts, unexpected atomic.Int32
	const (
		batches = iota
		pulls
		snapshotPages
		continuationPages
		commands
		refresherRequests
		kinds
	)
	var served [kinds]atomic.Int32
	classify := func(kind int, err error) {
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			served[kind].Add(1)
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		case errors.As(err, &pgErr) && pgErr.Code == "40P01":
			deadlocks.Add(1)
		case errors.As(err, &pgErr) && pgErr.Code == "55P03":
			lockTimeouts.Add(1)
		default:
			t.Logf("unexpected: %v", err)
			unexpected.Add(1)
		}
	}
	// snapshot pages one snapshot through to its last page, or stops at the
	// account's snapshot cap (an expected refusal).
	snapshot := func(req authority.Request) {
		response, err := h.Service.ServeSnapshot(ctx, req, protocol.SnapshotRequest{})
		if errors.Is(err, familysync.ErrSnapshotLimit) {
			return
		}
		classify(snapshotPages, err)
		for err == nil && response.NextPageToken != nil && ctx.Err() == nil {
			response, err = h.Service.ServeSnapshot(ctx, req, protocol.SnapshotRequest{PageToken: response.NextPageToken})
			classify(continuationPages, err)
		}
	}
	var wg sync.WaitGroup
	for writer := range 4 {
		wg.Go(func() {
			for n := 0; ctx.Err() == nil; n++ {
				request := h.SyncRequest(nil, nil)
				request.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 100000*(writer+1)+n)}
				_, err := h.Service.ServeSync(ctx, authority.Request{Digests: owner}, request)
				classify(batches, err)
			}
		})
	}
	wg.Go(func() {
		for ctx.Err() == nil {
			_, err := h.Service.ServeSync(ctx, authority.Request{Digests: caregiver}, h.SyncRequest(nil, nil))
			classify(pulls, err)
		}
	})
	for _, reader := range [][]authority.KeyedDigest{caregiver, viewerDigests} {
		wg.Go(func() {
			for ctx.Err() == nil {
				snapshot(authority.Request{Digests: reader})
			}
		})
	}
	wg.Go(func() {
		for role := "caregiver"; ctx.Err() == nil; {
			_, err := h.Family.ChangeRole(ctx, h.Owner, h.CommandID(), viewer.AccountID, role)
			classify(commands, err)
			role = map[string]string{"caregiver": "viewer", "viewer": "caregiver"}[role]
		}
	})
	wg.Go(func() {
		for ctx.Err() == nil {
			bundle, err := h.Auth.Refresh(ctx, refreshToken)
			classify(commands, err)
			if err != nil {
				continue
			}
			refreshToken = bundle.RefreshToken
			next, err := h.Auth.AccessDigests(bundle.AccessToken)
			if err != nil {
				t.Error(err)
				return
			}
			refresherDigests.Store(&next)
		}
	})
	// The refreshed account sends requests with its latest token, racing
	// each refresh: a token whose session a refresh just consumed is
	// refused (session_revoked), which is the expected outcome, not a lock
	// failure.
	wg.Go(func() {
		for n := 0; ctx.Err() == nil; n++ {
			req := authority.Request{Digests: *refresherDigests.Load()}
			var err error
			switch n % 3 {
			case 0:
				request := h.SyncRequest(nil, nil)
				request.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 500000+n)}
				_, err = h.Service.ServeSync(ctx, req, request)
			case 1:
				_, err = h.Service.ServeSync(ctx, req, h.SyncRequest(nil, nil))
			default:
				_, err = h.Service.ServeSnapshot(ctx, req, protocol.SnapshotRequest{})
				if errors.Is(err, familysync.ErrSnapshotLimit) {
					err = nil
				}
			}
			if errors.Is(err, authority.ErrSessionRevoked) {
				continue
			}
			classify(refresherRequests, err)
		}
	})
	wg.Wait()
	t.Logf("served: %d batches, %d pulls, %d snapshots with %d continuation pages, %d commands, %d requests by the refreshed account",
		served[batches].Load(), served[pulls].Load(), served[snapshotPages].Load(), served[continuationPages].Load(), served[commands].Load(),
		served[refresherRequests].Load())
	if deadlocks.Load()+lockTimeouts.Load()+unexpected.Load() != 0 {
		t.Fatalf("deadlocks %d, lock timeouts %d, unexpected %d", deadlocks.Load(), lockTimeouts.Load(), unexpected.Load())
	}
	for kind := range served {
		if served[kind].Load() == 0 {
			t.Fatalf("kind %d never completed: the run proved nothing", kind)
		}
	}
	if served[continuationPages].Load() <= 2 {
		t.Fatalf("the snapshot legs served %d continuation pages, want more than 2", served[continuationPages].Load())
	}
}

// requestPausingGuard holds a request inside its transaction after
// authorization, before any care statement.
type requestPausingGuard struct {
	authority.RequestGuard
	entered, release chan struct{}
}

func (g requestPausingGuard) WithRequestTx(ctx context.Context, req authority.Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	return g.RequestGuard.WithRequestTx(ctx, req, action, func(tx pgx.Tx, p access.Principal, gr access.Grant) error {
		close(g.entered)
		<-g.release
		return run(tx, p, gr)
	})
}

// Review Focus 4: a request already past authorization completes although
// the member is removed meanwhile; the next request, whose transaction
// begins after the removal committed, is refused as session_revoked.
func TestRevocationRefusesTheNextRequestAndLetsInFlightFinish(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1))
	caregiver, err := h.Auth.AccessDigests(h.AccessToken("caregiver"))
	if err != nil {
		t.Fatal(err)
	}
	guard := requestPausingGuard{RequestGuard: h.Authority, entered: make(chan struct{}), release: make(chan struct{})}
	service := testkit.SyncServiceOn(t, h, guard)
	done := make(chan error, 1)
	var inFlight protocol.SyncResponse
	go func() {
		var err error
		inFlight, err = service.ServeSync(context.Background(), authority.Request{Digests: caregiver}, h.SyncRequest(h.Cursor(0), nil))
		done <- err
	}()
	<-guard.entered
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err) // a pull holds no family lock, so removal does not wait for it
	}
	close(guard.release)
	if err := <-done; err != nil || len(testkit.Decoded(t, inFlight).Changes) != 2 {
		t.Fatalf("the in-flight pull did not complete: %v", err)
	}
	if _, err := h.Service.ServeSync(t.Context(), authority.Request{Digests: caregiver}, h.SyncRequest(h.Cursor(0), nil)); family.Code(err) != "session_revoked" {
		t.Fatalf("a request after the removal committed: %v", err)
	}
}
