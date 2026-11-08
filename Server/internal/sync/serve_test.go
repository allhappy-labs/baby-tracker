package sync_test

import (
	"context"
	"errors"
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
)

// accessDigests is subject's bearer access token's digests.
func accessDigests(t *testing.T, h *testkit.Harness, subject string) []authority.KeyedDigest {
	t.Helper()
	digests, err := h.Auth.AccessDigests(h.AccessToken(subject))
	if err != nil {
		t.Fatal(err)
	}
	return digests
}

// ServeSync and ServeSnapshot authorize from the session row alone: Admit
// sees the session's principal and Charge the grant of its family and live
// role. A refusal by either returns that refusal unchanged, runs nothing,
// writes nothing and publishes nothing; a committed batch publishes its
// high water once, after the commit.
func TestServeAuthorizesFromTheSessionAndPublishesOnlyAfterCommit(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	publisher := &committedPublisher{h: h, t: t}
	service := newService(h, h.Authority, h.Limits, func() time.Time { return h.Now }, publisher)
	owner := accessDigests(t, h, "owner")
	batch := h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil)
	batch.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 1)}
	held, limited := errors.New("held refusal"), errors.New("rate limited")
	var admitted []access.Principal
	var charged []access.Grant
	request := func(admit, charge error) authority.Request {
		return authority.Request{Digests: owner,
			Admit:  func(p access.Principal) error { admitted = append(admitted, p); return admit },
			Charge: func(_ access.Principal, g access.Grant) error { charged = append(charged, g); return charge }}
	}
	for _, c := range []struct {
		name          string
		admit, charge error
		admits, gets  int
	}{
		{"Admit refusal", held, nil, 1, 0},
		{"Charge refusal", nil, limited, 1, 1},
	} {
		admitted, charged = nil, nil
		before := h.FamilySequence(t)
		want := c.admit
		if want == nil {
			want = c.charge
		}
		if _, err := service.ServeSync(t.Context(), request(c.admit, c.charge), batch); !errors.Is(err, want) {
			t.Fatalf("%s: ServeSync %v, want %v", c.name, err, want)
		}
		if _, err := service.ServeSnapshot(t.Context(), request(c.admit, c.charge), protocol.SnapshotRequest{}); !errors.Is(err, want) {
			t.Fatalf("%s: ServeSnapshot %v, want %v", c.name, err, want)
		}
		if len(admitted) != 2*c.admits || len(charged) != 2*c.gets || h.FamilySequence(t) != before || publisher.publishes != 0 ||
			h.Count(t, "sync_snapshot_sessions", h.FamilyID) != 0 {
			t.Fatalf("%s: admitted %d, charged %d, sequence %d (was %d), publishes %d", c.name, len(admitted), len(charged),
				h.FamilySequence(t), before, publisher.publishes)
		}
	}
	admitted, charged = nil, nil
	response, err := service.ServeSync(t.Context(), request(nil, nil), batch)
	if err != nil || len(response.Outcomes) != 1 || response.Outcomes[0].Status != "applied" {
		t.Fatalf("batch: %+v %v", response, err)
	}
	if len(admitted) != 1 || admitted[0].AccountID != h.Owner.AccountID || admitted[0].DeviceID != h.Owner.DeviceID ||
		admitted[0].SessionFamilyID != h.FamilyID || len(charged) != 1 ||
		charged[0].AccountID != h.Owner.AccountID || charged[0].FamilyID != h.FamilyID || charged[0].Role != "owner" {
		t.Fatalf("hooks saw principal %+v and grant %+v", admitted, charged)
	}
	if publisher.publishes != 1 || publisher.cursors[0] != *response.Outcomes[0].Cursor || publisher.committed[0] != response.Outcomes[0].Cursor.Sequence {
		t.Fatalf("published %+v (committed sequence then %v) for the batch at %+v", publisher.cursors, publisher.committed, response.Outcomes[0].Cursor)
	}
	if _, err := service.ServeSync(t.Context(), request(nil, nil), h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil)); err != nil || publisher.publishes != 1 {
		t.Fatalf("a pull published (%d) or failed: %v", publisher.publishes, err)
	}
	snapshot, err := service.ServeSnapshot(t.Context(), request(nil, nil), protocol.SnapshotRequest{})
	if err != nil || len(testkit.DecodedSnapshot(t, snapshot).Entities) != 2 {
		t.Fatalf("snapshot: %+v %v", snapshot, err)
	}
	// Family policy is asked for "read", which every role holds: a viewer
	// pulls and pages a snapshot (its mutations are refused per mutation).
	h.AddMember(t, "viewer", "viewer")
	viewer := authority.Request{Digests: accessDigests(t, h, "viewer")}
	if pulled, err := service.ServeSync(t.Context(), viewer, h.SyncRequest(h.Cursor(0), nil)); err != nil || len(testkit.Decoded(t, pulled).Changes) != 2 {
		t.Fatalf("a viewer's pull: %+v %v", pulled, err)
	}
	if _, err := service.ServeSnapshot(t.Context(), viewer, protocol.SnapshotRequest{}); err != nil {
		t.Fatalf("a viewer's snapshot: %v", err)
	}
}

// committedPublisher records each publish and the family sequence another
// connection saw committed at that moment.
type committedPublisher struct {
	capturePublisher
	h         *testkit.Harness
	t         *testing.T
	committed []uint64
}

func (p *committedPublisher) Publish(ctx context.Context, familyID string, cursor protocol.Cursor) error {
	p.committed = append(p.committed, p.h.FamilySequence(p.t))
	return p.capturePublisher.Publish(ctx, familyID, cursor)
}

// An incompletely composed service refuses before it opens a transaction;
// an unknown credential is refused by the authority before any hook.
func TestServeRefusesBeforeAnyStatement(t *testing.T) {
	h := testkit.New(t)
	pool, wire := testkit.CountedPool(t, h.Runtime)
	guard := h.AuthorityOn(t, pool)
	noPolicy := familysync.New(familysync.Options{Guard: guard, Server: h.Info, Limits: h.Limits, Now: func() time.Time { return h.Now },
		PageKeys: testkit.PageKeyring("page.v1", 0x77)})
	owner := accessDigests(t, h, "owner")
	wire.Reset()
	if _, err := noPolicy.ServeSync(t.Context(), authority.Request{Digests: owner}, h.SyncRequest(nil, nil)); err == nil {
		t.Fatal("an unready service served a sync")
	}
	if _, err := noPolicy.ServeSnapshot(t.Context(), authority.Request{Digests: owner}, protocol.SnapshotRequest{}); err == nil {
		t.Fatal("an unready service served a snapshot")
	}
	if trips := wire.Trips(); trips != 0 {
		t.Fatalf("an unready service made %d round trips", trips)
	}
	service := testkit.SyncServiceOn(t, h, guard)
	hooked := authority.Request{Admit: func(access.Principal) error { t.Fatal("Admit ran without a credential"); return nil }}
	if _, err := service.ServeSync(t.Context(), hooked, h.SyncRequest(nil, nil)); family.Code(err) != "session_revoked" {
		t.Fatalf("no credential: %v", err)
	}
	if trips := wire.Trips(); trips != 0 {
		t.Fatalf("no credential made %d round trips", trips)
	}
}

// The principal methods serve the principal's session family without
// reading the family first: a pull is the one transaction's 2 round trips,
// and a principal that is not a refreshed family session is refused in Go,
// before any statement. The guard's grant decides the rest: a session
// family that is not the session row's is refused.
func TestPrincipalMethodsServeTheSessionFamilyWithoutReadingIt(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	pool, wire := testkit.CountedPool(t, h.Runtime)
	service := testkit.SyncServiceOn(t, h, h.AuthorityOn(t, pool))
	pull := h.SyncRequest(h.Cursor(0), nil)
	if _, err := service.ApplyAndPull(t.Context(), h.Owner, pull); err != nil { // prepares
		t.Fatal(err)
	}
	wire.Reset()
	if response, err := service.ApplyAndPull(t.Context(), h.Owner, pull); err != nil || len(testkit.Decoded(t, response).Changes) != 1 {
		t.Fatalf("pull: %+v %v", response, err)
	}
	if trips := wire.Trips(); trips != 2 {
		t.Fatalf("a principal pull made %d round trips, want 2: %q", trips, wire.Statements())
	}
	if _, err := service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{}); err != nil {
		t.Fatal(err)
	}
	account, unrefreshed, foreign := h.Owner, h.Owner, h.Owner
	account.Scope = "account"
	unrefreshed.SessionScope, unrefreshed.SessionFamilyID = "account", ""
	foreign.SessionFamilyID = "30000000-0000-4000-8000-0000000000ff"
	wire.Reset()
	for _, c := range []struct {
		name string
		p    access.Principal
		want error
	}{
		{"account scope", account, familysync.ErrFamilyScopeRequired},
		{"unrefreshed session", unrefreshed, familysync.ErrSessionRefreshRequired},
	} {
		if _, err := service.ApplyAndPull(t.Context(), c.p, pull); !errors.Is(err, c.want) {
			t.Fatalf("%s: ApplyAndPull %v, want %v", c.name, err, c.want)
		}
		if _, err := service.FullSnapshot(t.Context(), c.p, protocol.SnapshotRequest{}); !errors.Is(err, c.want) {
			t.Fatalf("%s: FullSnapshot %v, want %v", c.name, err, c.want)
		}
	}
	if trips := wire.Trips(); trips != 0 {
		t.Fatalf("refusals in Go made %d round trips", trips)
	}
	if _, err := service.ApplyAndPull(t.Context(), foreign, h.SyncRequest(nil, nil)); !errors.Is(err, authority.ErrMembershipRevoked) {
		t.Fatalf("another session family: %v", err)
	}
}

// Task 4 carry m3: the principal methods report a family that moved into
// recovery as the legacy family read did (family_unavailable), never as
// the guard's recovery_only.
func TestPrincipalMethodsReportRecoveryAsTheLegacyFamilyRead(t *testing.T) {
	h := testkit.New(t)
	service := serviceGuarded(h, refusingGuard{RequestGuard: h.Authority, err: authority.ErrRecoveryOnly})
	if _, err := service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(nil, nil)); family.Code(err) != "family_unavailable" {
		t.Fatalf("ApplyAndPull: %v", err)
	}
	if _, err := service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{}); family.Code(err) != "family_unavailable" {
		t.Fatalf("FullSnapshot: %v", err)
	}
	other := serviceGuarded(h, refusingGuard{RequestGuard: h.Authority, err: authority.ErrScopeFailed})
	if _, err := other.ApplyAndPull(context.Background(), h.Owner, h.SyncRequest(nil, nil)); !errors.Is(err, authority.ErrScopeFailed) {
		t.Fatalf("another refusal is returned as is: %v", err)
	}
}

var errInjectedCommit = errors.New("injected commit failure")

// commitFailGuard ends every request transaction as a failed commit would:
// run's work all succeeds (the batch is written and Exchange returns a high
// water to publish), then the transaction rolls back with an error.
type commitFailGuard struct{ authority.RequestGuard }

func (g commitFailGuard) WithRequestTx(ctx context.Context, req authority.Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	return g.RequestGuard.WithRequestTx(ctx, req, action, func(tx pgx.Tx, p access.Principal, grant access.Grant) error {
		if err := run(tx, p, grant); err != nil {
			return err
		}
		return errInjectedCommit
	})
}

// A request whose transaction fails, at the write batch or at its commit,
// returns the failure, leaves nothing durable and publishes nothing: the
// high water is announced only after a commit that succeeded.
func TestServeFailedTransactionPublishesNothing(t *testing.T) {
	for _, c := range []struct {
		name  string
		guard func(*testkit.Harness) authority.RequestGuard
		want  error
	}{
		{"fault at the write batch", func(h *testkit.Harness) authority.RequestGuard { return faultGuard{RequestGuard: h.Authority} }, errInjectedCommitBoundary},
		{"failed commit", func(h *testkit.Harness) authority.RequestGuard { return commitFailGuard{RequestGuard: h.Authority} }, errInjectedCommit},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := testkit.New(t)
			caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
			publisher := &capturePublisher{}
			service := newService(h, c.guard(h), h.Limits, func() time.Time { return h.Now }, publisher)
			owner := authority.Request{Digests: accessDigests(t, h, "owner")}
			before := h.FamilySequence(t)
			batch := h.SyncRequest(h.Cursor(before), nil)
			batch.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 1)}
			if _, err := service.ServeSync(t.Context(), owner, batch); !errors.Is(err, c.want) {
				t.Fatalf("ServeSync: %v, want %v", err, c.want)
			}
			if h.FamilySequence(t) != before || h.CareCount(t, "feed", h.FamilyID) != 0 || h.Count(t, "care_receipts", h.FamilyID) != 1 || publisher.publishes != 0 {
				t.Fatalf("a failed transaction left sequence %d (was %d) or published %d", h.FamilySequence(t), before, publisher.publishes)
			}
			if c.want == errInjectedCommit {
				if _, err := service.ServeSnapshot(t.Context(), owner, protocol.SnapshotRequest{}); !errors.Is(err, c.want) {
					t.Fatalf("ServeSnapshot: %v, want %v", err, c.want)
				}
				if h.Count(t, "sync_snapshot_sessions", h.FamilyID) != 0 || publisher.publishes != 0 {
					t.Fatal("a failed snapshot transaction stored its session or published")
				}
			}
		})
	}
}

type progressCall struct {
	family, device   string
	acked, delivered uint64
	at               time.Time
}

type recordingProgress struct{ calls []progressCall }

func (p *recordingProgress) Synced(familyID, deviceID string, acked, delivered uint64, at time.Time) {
	p.calls = append(p.calls, progressCall{familyID, deviceID, acked, delivered, at})
}

// ServeSync and ServeSnapshot report each completed request's progress
// after the commit, in memory: the cursor the device presented (acked) and,
// on a final page, the high water served (delivered). A page with more
// reports no delivery; a refused request reports nothing; a snapshot reports
// its high water on its final page only.
func TestServeSyncReportsProgress(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	for n := range 5 {
		caretest.Seed(t, h, caretest.Feed(caretest.BabyA, n))
	}
	high := h.FamilySequence(t) // 6
	limits := h.Limits
	limits.SyncPageSize = 3
	info := h.Info
	info.Limits = &limits
	progress := &recordingProgress{}
	service := familysync.New(familysync.Options{Guard: h.Authority, Server: info, Limits: limits, Now: func() time.Time { return h.Now },
		PageKeys: testkit.PageKeyring("page.v1", 0x77), Allowed: family.Allowed, Progress: progress})
	service.SetImportAdmission(h.Importer)
	caregiver := authority.Request{Digests: accessDigests(t, h, "caregiver")}
	device := h.Caregiver.DeviceID
	expect := func(label string, want ...progressCall) {
		t.Helper()
		if len(progress.calls) != len(want) {
			t.Fatalf("%s: progress %+v, want %+v", label, progress.calls, want)
		}
		for i := range want {
			if progress.calls[i] != want[i] {
				t.Fatalf("%s: progress %+v, want %+v", label, progress.calls, want)
			}
		}
	}
	first, err := service.ServeSync(t.Context(), caregiver, h.SyncRequest(h.Cursor(1), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first page: %+v %v", first, err)
	}
	expect("a page with more", progressCall{h.FamilyID, device, 1, 0, h.Now})
	last, err := service.ServeSync(t.Context(), caregiver, h.SyncRequest(nil, first.NextPageToken))
	if err != nil || last.NextPageToken != nil || last.HighWater.Sequence != high {
		t.Fatalf("last page: %+v %v", last, err)
	}
	expect("the final page", progressCall{h.FamilyID, device, 1, 0, h.Now}, progressCall{h.FamilyID, device, 0, high, h.Now})
	progress.calls = nil
	if _, err := service.ServeSync(t.Context(), caregiver, h.SyncRequest(h.Cursor(high), nil)); err != nil {
		t.Fatal(err)
	}
	batch := h.SyncRequest(h.Cursor(high), nil)
	batch.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 10)}
	if _, err := service.ServeSync(t.Context(), caregiver, batch); err != nil {
		t.Fatal(err)
	}
	expect("a pull at the high water and a batch", progressCall{h.FamilyID, device, high, high, h.Now},
		progressCall{h.FamilyID, device, high, high + 1, h.Now})
	// Refusals report nothing: a cursor past the high water, a held refusal.
	progress.calls = nil
	if _, err := service.ServeSync(t.Context(), caregiver, h.SyncRequest(h.Cursor(high+100), nil)); !errors.Is(err, familysync.ErrSnapshotRequired) {
		t.Fatalf("a cursor past the high water: %v", err)
	}
	// A batch with a stale cursor commits its mutations, then is refused
	// after the commit (Fatal): it reports nothing either.
	stale := h.SyncRequest(h.Cursor(high+100), nil)
	stale.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 11)}
	if _, err := service.ServeSync(t.Context(), caregiver, stale); !errors.Is(err, familysync.ErrSnapshotRequired) {
		t.Fatalf("a batch with a stale cursor: %v", err)
	}
	high++
	held := authority.Request{Digests: caregiver.Digests, Admit: func(access.Principal) error { return errors.New("held") }}
	if _, err := service.ServeSync(t.Context(), held, h.SyncRequest(h.Cursor(high), nil)); err == nil {
		t.Fatal("a held refusal succeeded")
	}
	if _, err := service.ServeSnapshot(t.Context(), held, protocol.SnapshotRequest{}); err == nil {
		t.Fatal("a held snapshot succeeded")
	}
	expect("refusals")
	// A snapshot (8 entities at 3 a page) reports once, on its final page.
	var token *string
	for pages := 0; ; pages++ {
		page, err := service.ServeSnapshot(t.Context(), caregiver, protocol.SnapshotRequest{PageToken: token})
		if err != nil || pages > 5 {
			t.Fatalf("snapshot page %d: %v", pages, err)
		}
		if token = page.NextPageToken; token == nil {
			expect("a snapshot", progressCall{h.FamilyID, device, 0, high + 1, h.Now})
			break
		}
		expect("a snapshot page with more")
	}
}
