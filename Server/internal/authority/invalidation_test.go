package authority_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
)

// observed is an invalidation and the audit seq its scope had committed when
// the sink received it.
type observed struct {
	item      authority.SecurityInvalidation
	committed uint64
	err       error
}

// observingSink reads, as each invalidation arrives, the committed audit head
// of the scope it names (the family's when FamilyID is set, else the
// account's). An invalidation published before its commit sees an older head.
type observingSink struct {
	f  *authoritytest.Fixture
	ch chan observed
}

func (sink observingSink) Invalidate(ctx context.Context, item authority.SecurityInvalidation) {
	table, id := "accounts", item.AccountID
	if item.FamilyID != "" {
		table, id = "families", item.FamilyID
	}
	var seq uint64
	tx, err := sink.f.Service.Begin(ctx)
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT audit_seq FROM `+table+` WHERE id=$1`, id).Scan(&seq)
		_ = tx.Rollback(ctx)
	}
	sink.ch <- observed{item: item, committed: seq, err: err}
}

// slowCommits makes every transaction that moves an audit head sleep 300 ms
// inside COMMIT (a deferred constraint trigger), so an invalidation published
// before the commit returns is observed against the old head.
func slowCommits(t *testing.T, f *authoritytest.Fixture) {
	t.Helper()
	for _, statement := range []string{
		`CREATE FUNCTION public.test_slow_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF current_setting('test.slept', true) IS DISTINCT FROM '1' THEN
		     PERFORM set_config('test.slept', '1', true);
		     PERFORM pg_sleep(0.3);
		   END IF;
		   RETURN NULL;
		 END $$`,
		`CREATE CONSTRAINT TRIGGER test_slow_commit AFTER UPDATE ON public.accounts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.test_slow_commit()`,
		`CREATE CONSTRAINT TRIGGER test_slow_commit AFTER UPDATE ON public.families DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.test_slow_commit()`,
	} {
		if _, err := f.DB.Owner.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func subscribe(t *testing.T, f *authoritytest.Fixture) observingSink {
	t.Helper()
	sink := observingSink{f: f, ch: make(chan observed, 64)}
	t.Cleanup(f.Service.Subscribe(sink))
	return sink
}

func invalidationKey(item authority.SecurityInvalidation) string {
	return fmt.Sprintf("%s|%s|%s|%d", item.AccountID, item.FamilyID, item.SessionID, item.Revision)
}

// expect receives exactly want (in any order), each observed after its
// commit. Anything published earlier and not yet consumed (a rolled-back
// command or an exact retry) is received first and fails the comparison.
func (sink observingSink) expect(t *testing.T, step string, want ...authority.SecurityInvalidation) {
	t.Helper()
	var got []string
	for range want {
		select {
		case o := <-sink.ch:
			if o.err != nil {
				t.Fatalf("%s: observe %+v: %v", step, o.item, o.err)
			}
			if o.committed < o.item.Revision {
				t.Fatalf("%s: %+v delivered while the committed head was %d (published before commit)", step, o.item, o.committed)
			}
			got = append(got, invalidationKey(o.item))
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: received %v, missing %d of %d", step, got, len(want)-len(got), len(want))
		}
	}
	wanted := make([]string, len(want))
	for i, item := range want {
		wanted[i] = invalidationKey(item)
	}
	slices.Sort(got)
	slices.Sort(wanted)
	if !slices.Equal(got, wanted) {
		t.Fatalf("%s: invalidations\n got %v\nwant %v", step, got, wanted)
	}
}

// quiet requires that nothing further was published.
func (sink observingSink) quiet(t *testing.T, step string) {
	t.Helper()
	select {
	case o := <-sink.ch:
		t.Fatalf("%s: unexpected invalidation %+v", step, o.item)
	case <-time.After(300 * time.Millisecond):
	}
}

func accountSeq(t *testing.T, f *authoritytest.Fixture, account string) uint64 {
	t.Helper()
	seq, _ := chainHead(t, f, authority.ScopeAccount, account)
	return seq
}

func TestInvalidationsPublishedOnlyAfterCommit(t *testing.T) {
	t.Run("session commands", func(t *testing.T) {
		f := authoritytest.New(t)
		s := f.Service
		slowCommits(t, f)
		sink := subscribe(t, f)

		// Logout: every session of the device, at the account's revision.
		m := newMember(t, f)
		second, _, _ := f.Session(t, m.account, m.device, "account", "", "")
		otherDevice, _, _ := f.Session(t, m.account, f.Device(t, m.account), "account", "", "")
		command := authoritytest.NewID(t)
		result, err := s.Logout(t.Context(), m.principal(t, f), command, f.Now)
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "logout",
			authority.SecurityInvalidation{AccountID: m.account, SessionID: m.session, Revision: result.Revision},
			authority.SecurityInvalidation{AccountID: m.account, SessionID: second, Revision: result.Revision})
		if row, _ := sessionRow(t, f, otherDevice); row.Status != "active" {
			t.Fatalf("other device's session %s", row.Status)
		}
		// Exact retry: nothing.
		if again, err := s.Logout(t.Context(), principalOf(f, t, m.session), command, f.Now); err != nil || again != result {
			t.Fatalf("logout retry %+v %v", again, err)
		}

		// A refresh rotation publishes nothing; a reuse names every session
		// of the lineage it revoked.
		r := newMember(t, f)
		rotated, err := s.Refresh(t.Context(), refreshOf(f, r.refresh, "", nil))
		if err != nil {
			t.Fatal(err)
		}
		sink.quiet(t, "logout retry and rotation")
		_, err = s.Refresh(t.Context(), refreshOf(f, r.refresh, "", nil))
		wantCode(t, err, "refresh_reused")
		sink.expect(t, "reuse", authority.SecurityInvalidation{AccountID: r.account, SessionID: rotated.Child.ID, Revision: accountSeq(t, f, r.account)})
		// A replay into the revoked lineage commits nothing.
		_, err = s.Refresh(t.Context(), refreshOf(f, r.refresh, "", nil))
		wantCode(t, err, "session_revoked")

		// Identity loss: the whole account.
		u, key := keyedMember(t, f)
		in := authority.IdentityUnavailable{CommandID: authoritytest.NewID(t), AccountID: u.account, Candidates: []string{key}, At: f.Now}
		lost, err := s.MarkIdentityUnavailable(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "identity unavailable", authority.SecurityInvalidation{AccountID: u.account, Revision: lost.Revision})
		if again, err := s.MarkIdentityUnavailable(t.Context(), in); err != nil || again != lost {
			t.Fatalf("identity retry %+v %v", again, err)
		}
		// A refused command (rolled back) publishes nothing.
		_, err = s.Logout(t.Context(), principalOf(f, t, u.session), authoritytest.NewID(t), f.Now)
		wantCode(t, err, "session_revoked")
		sink.quiet(t, "retries and refusals")
	})

	t.Run("family commands", func(t *testing.T) {
		f := authoritytest.New(t)
		s := f.Service
		slowCommits(t, f)
		owner := newMember(t, f)
		family := createFamily(t, f, s, owner)
		a := joined(t, f, s, owner, family, "caregiver")
		b := joined(t, f, s, owner, family, "caregiver")
		c := joined(t, f, s, owner, family, "caregiver")
		sink := subscribe(t, f)

		remove := authority.RemoveMember{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, AccountID: a.account, At: f.Now, Principal: owner.principal(t, f)}
		removed, err := s.RemoveMember(t.Context(), remove)
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "remove", authority.SecurityInvalidation{AccountID: a.account, FamilyID: family, Revision: removed.Revision})
		if again, err := s.RemoveMember(t.Context(), remove); err != nil || again != removed {
			t.Fatalf("remove retry %+v %v", again, err)
		}
		left, err := s.LeaveFamily(t.Context(), authority.LeaveFamily{CommandID: authoritytest.NewID(t), AccountID: b.account, At: f.Now, Principal: b.principal(t, f)})
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "leave", authority.SecurityInvalidation{AccountID: b.account, FamilyID: family, Revision: left.Revision})
		changed, err := s.ChangeRole(t.Context(), authority.ChangeRole{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, AccountID: c.account,
			Role: "viewer", At: f.Now, Principal: owner.principal(t, f)})
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "change role", authority.SecurityInvalidation{AccountID: c.account, FamilyID: family, Revision: changed.Revision})
		// Refused: a viewer cannot become owner; nothing is published.
		_, err = s.TransferOwner(t.Context(), authority.TransferOwner{CommandID: authoritytest.NewID(t), OldOwnerID: owner.account, NewOwnerID: c.account,
			At: f.Now, Principal: owner.principal(t, f)})
		wantCode(t, err, "membership_revoked")
		if _, err := s.ChangeRole(t.Context(), authority.ChangeRole{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, AccountID: c.account,
			Role: "caregiver", At: f.Now, Principal: owner.principal(t, f)}); err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "change role back", authority.SecurityInvalidation{AccountID: c.account, FamilyID: family, Revision: familyRevision(t, f, family)})
		transferred, err := s.TransferOwner(t.Context(), authority.TransferOwner{CommandID: authoritytest.NewID(t), OldOwnerID: owner.account, NewOwnerID: c.account,
			At: f.Now, Principal: owner.principal(t, f)})
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "transfer",
			authority.SecurityInvalidation{AccountID: owner.account, FamilyID: family, Revision: transferred.Revision},
			authority.SecurityInvalidation{AccountID: c.account, FamilyID: family, Revision: transferred.Revision})
		// Commands that grant rather than reduce authority publish nothing.
		joined(t, f, s, c, family, "viewer")
		sink.quiet(t, "retries, refusals and grants")
	})

	t.Run("lifecycle commands", func(t *testing.T) {
		f := authoritytest.New(t)
		s := f.Service
		slowCommits(t, f)
		owner := newMember(t, f)
		family := createFamily(t, f, s, owner)
		member := joined(t, f, s, owner, family, "caregiver")
		leaver, key := keyedMember(t, f)
		sink := subscribe(t, f)

		schedule := scheduleOf(t, f, owner.account, owner.principal(t, f))
		scheduled, err := s.ScheduleDeletion(t.Context(), schedule)
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "schedule deletion",
			authority.SecurityInvalidation{AccountID: owner.account, FamilyID: family, Revision: scheduled.Revision},
			authority.SecurityInvalidation{AccountID: member.account, FamilyID: family, Revision: scheduled.Revision})
		if _, err := s.ScheduleDeletion(t.Context(), schedule); err != nil {
			t.Fatalf("schedule retry: %v", err)
		}
		deleted, err := s.DeleteAccount(t.Context(), authority.DeleteAccount{CommandID: authoritytest.NewID(t), AccountID: leaver.account, IdentityKey: key,
			At: f.Now, Principal: leaver.principal(t, f)})
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "delete account", authority.SecurityInvalidation{AccountID: leaver.account, Revision: deleted.Revision})
		f.Now = f.Now.Add(purgeDelay)
		start := authority.StartFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family, At: f.Now}
		started, err := s.StartFamilyPurge(t.Context(), start)
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "purge start", authority.SecurityInvalidation{FamilyID: family, Revision: started.Revision})
		completed, err := s.CompleteFamilyPurge(t.Context(), authority.CompleteFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family,
			StartCommandID: start.CommandID, Receipt: authoritytest.NewID(t), At: f.Now})
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "purge complete", authority.SecurityInvalidation{FamilyID: family, Revision: completed.Revision})
		if again, err := s.StartFamilyPurge(t.Context(), start); err != nil || again != started {
			t.Fatalf("purge retry %+v %v", again, err)
		}
		sink.quiet(t, "retries")
	})

	t.Run("operator commands", func(t *testing.T) {
		f := authoritytest.New(t)
		s := f.Service
		slowCommits(t, f)
		hosted := serviceWith(t, f, func(o *authority.Options) { o.RequireFamilySyncEntitlement = true })
		r := newRecoveryFamily(t, f)
		other := newMember(t, f)
		otherFamily := createFamily(t, f, hosted, other)
		grant := grantOf(t, f, otherFamily, other.account)
		if _, err := s.ExecuteOperatorIntegration(t.Context(), grant); err != nil {
			t.Fatal(err)
		}
		sink := subscribe(t, f)

		recovery := recoveryOf(t, f, r, r.caregiver.account)
		recovered, err := s.ExecuteOperatorRecoverOwner(t.Context(), recovery)
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "owner recovery",
			authority.SecurityInvalidation{AccountID: r.owner.account, FamilyID: r.family, Revision: recovered.Revision},
			authority.SecurityInvalidation{AccountID: r.caregiver.account, FamilyID: r.family, Revision: recovered.Revision})
		if again, err := s.ExecuteOperatorRecoverOwner(t.Context(), recovery); err != nil || again != recovered {
			t.Fatalf("recovery retry %+v %v", again, err)
		}
		revoke := revokeOf(t, f, grant)
		revoked, err := s.ExecuteOperatorIntegration(t.Context(), revoke)
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "integration revoke", authority.SecurityInvalidation{AccountID: other.account, FamilyID: otherFamily, Revision: revoked.Revision})
		_, err = s.ExecuteOperatorIntegration(t.Context(), revokeOf(t, f, grant))
		wantCode(t, err, "integration_revoked")
		_, _, current := entitlementState(t, f, otherFamily)
		suspended, err := s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, otherFamily, "suspend", current.UTC()))
		if err != nil {
			t.Fatal(err)
		}
		sink.expect(t, "entitlement suspension", authority.SecurityInvalidation{FamilyID: otherFamily, Revision: suspended.Revision})
		// Grants publish nothing: an integration grant, and entitlement grants
		// (a first grant and an extension).
		if _, err := s.ExecuteOperatorIntegration(t.Context(), grantOf(t, f, r.family, r.caregiver.account)); err != nil {
			t.Fatal(err)
		}
		sink.quiet(t, "integration grant")
		expires := f.Now.Add(24 * time.Hour).Truncate(time.Second)
		if _, err := s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, r.family, "grant", expires)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ExecuteOperatorEntitlement(t.Context(), entitlementOf(t, f, r.family, "grant", expires.Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
		sink.quiet(t, "retries, refusals and grants")
	})
}

type countingSink struct {
	mu sync.Mutex
	n  int
}

func (sink *countingSink) Invalidate(context.Context, authority.SecurityInvalidation) {
	sink.mu.Lock()
	sink.n++
	sink.mu.Unlock()
}

func TestInvalidationUnsubscribeIsIdempotent(t *testing.T) {
	f := authoritytest.New(t)
	sink := &countingSink{}
	unsubscribe := f.Service.Subscribe(sink)
	if n := f.Service.InvalidationSubscriberCount(); n != 1 {
		t.Fatalf("subscribers %d", n)
	}
	unsubscribe()
	unsubscribe()
	if n := f.Service.InvalidationSubscriberCount(); n != 0 {
		t.Fatalf("subscribers after unsubscribe %d", n)
	}
	// Publishing with no subscriber, and to an unsubscribed sink, is a no-op.
	m, key := keyedMember(t, f)
	if _, err := f.Service.MarkIdentityUnavailable(t.Context(), authority.IdentityUnavailable{CommandID: authoritytest.NewID(t), AccountID: m.account,
		Candidates: []string{key}, At: f.Now}); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.n != 0 {
		t.Fatalf("unsubscribed sink received %d", sink.n)
	}
	for range 70 {
		f.Service.Subscribe(&countingSink{})
	}
	if n := f.Service.InvalidationSubscriberCount(); n != 64 {
		t.Fatalf("subscribers capped at %d", n)
	}
}

// blockingOverflowSink holds its delivery goroutine on release and records
// what it was given: items and overflow signals.
type blockingOverflowSink struct {
	release   chan struct{}
	entered   chan struct{}
	mu        sync.Mutex
	delivered []authority.SecurityInvalidation
	dropped   int
}

func (sink *blockingOverflowSink) Invalidate(_ context.Context, item authority.SecurityInvalidation) {
	select {
	case sink.entered <- struct{}{}:
	default:
	}
	<-sink.release
	sink.mu.Lock()
	sink.delivered = append(sink.delivered, item)
	sink.mu.Unlock()
}

func (sink *blockingOverflowSink) InvalidationsDropped(context.Context) {
	sink.mu.Lock()
	sink.dropped++
	sink.mu.Unlock()
}

// An invalidation that does not fit a subscriber's queue is not lost
// silently: an InvalidationOverflowSink is told, once per burst of drops,
// after the queued items, so it can re-check every grant it holds.
func TestInvalidationOverflowIsSignalledNotSilentlyDropped(t *testing.T) {
	f := authoritytest.New(t)
	sink := &blockingOverflowSink{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	unsubscribe := f.Service.Subscribe(sink)
	defer unsubscribe()
	item := func(i int) authority.SecurityInvalidation {
		return authority.SecurityInvalidation{AccountID: authoritytest.NewID(t), Revision: uint64(i + 1)}
	}
	// The first item occupies the delivery goroutine; 32 more fill the queue;
	// the remaining ten are dropped.
	f.Service.PublishForTest(item(0))
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first invalidation was not delivered")
	}
	for i := 1; i <= 42; i++ {
		f.Service.PublishForTest(item(i))
	}
	close(sink.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		sink.mu.Lock()
		delivered, dropped := len(sink.delivered), sink.dropped
		sink.mu.Unlock()
		if delivered == 33 && dropped >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivered=%d dropped signals=%d, want 33 and at least one", delivered, dropped)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Coalesced: ten drops in one burst, at most two signals (one may be
	// consumed while the burst is still being published).
	time.Sleep(50 * time.Millisecond)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.dropped > 2 {
		t.Fatalf("dropped signals=%d for one burst", sink.dropped)
	}
}
