package security

import (
	"babytracker/server/internal/access"
	"context"
	"testing"
)

type successorFence struct {
	Fence
	beforeRelease func()
}

func (f *successorFence) Write(ctx context.Context) (func(), error) {
	release, err := f.Fence.Write(ctx)
	if err != nil {
		return nil, err
	}
	return func() { f.beforeRelease(); release() }, nil
}
func TestAuthoritySuccessorPublishesBeforeFenceRelease(t *testing.T) {
	h, _, _ := cacheFixture(t)
	readView(t, h)
	old := h.Service.authorityProof
	p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000002", Scope: "account", RecoveryEpoch: 1}
	h.Service.fence = &successorFence{Fence: h.Service.fence, beforeRelease: func() {
		owner := h.Service.authorityProof
		if owner == old || owner.head != h.Head() {
			t.Error("durable successor not published before exclusive fence release")
		}
		if _, ok := owner.state.Account(p.AccountID); !ok {
			t.Error("successor missing committed account")
		}
	}}
	if _, err := h.Service.Execute(t.Context(), p, newVerifiedIdentityRegistration(p.AccountID, "sha256:second-identity", "30000000-0000-4000-8000-000000000002")); err != nil {
		t.Fatal(err)
	}
	if _, ok := old.state.Account(p.AccountID); ok {
		t.Fatal("published prior owner mutated")
	}
	owner := h.Service.authorityProof
	readView(t, h)
	if h.Service.authorityProof != owner {
		t.Fatal("read reconstructed committed successor")
	}
}
func TestAuthoritySuccessorCASLossFailsClosed(t *testing.T) {
	for _, failure := range []string{"cas", "projection"} {
		t.Run(failure, func(t *testing.T) {
			h, _, _ := cacheFixture(t)
			readView(t, h)
			old := h.Service.authorityProof
			h.heads.rejectOnce = failure == "cas"
			h.Projection.failOnce = failure == "projection"
			if failure == "cas" {
				h.Service.heads = rejectingSuccessorHeads{h.heads}
			}
			p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000002", Scope: "account", RecoveryEpoch: 1}
			_, err := h.Service.Execute(t.Context(), p, newVerifiedIdentityRegistration(p.AccountID, "sha256:second-identity", "30000000-0000-4000-8000-000000000002"))
			if err == nil {
				t.Fatal("failed write succeeded")
			}
			if h.Service.authorityProof != old {
				t.Fatal("failed write published successor")
			}
			if failure == "projection" {
				if h.Service.authorityFloor != h.Head() {
					t.Fatal("durable floor lost")
				}
				if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { t.Error("failed projection authorized"); return nil }); err == nil {
					t.Fatal("failed projection accepted")
				}
				h.heads.head = old.head
				h.Projection.head = old.head
				h.Service.setQuarantine(false)
				if err := h.Service.WithAuthorityView(t.Context(), func(AuthorityView) error { return nil }); err == nil {
					t.Fatal("rollback accepted after projection failure")
				}
			}
		})
	}
}

type rejectingSuccessorHeads struct{ LedgerHeadStore }

func (rejectingSuccessorHeads) CompareAndSwap(context.Context, Head, Head) (bool, error) {
	return false, nil
}

func TestStalePreparedCapabilityCannotCommit(t *testing.T) {
	h := newAuthority(t)
	var stale PreparedCommand
	var expired AuthoritySnapshot
	if err := h.Service.WithAuthoritySnapshot(t.Context(), func(snapshot AuthoritySnapshot) error {
		expired = snapshot
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return err
		}
		stale, err = snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000021"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := expired.CommandHash(stale); Code(err) != "invalid_command" {
		t.Fatalf("expired hash: %v", err)
	}
	before := h.Head()
	if _, err := h.Service.ExecutePrepared(t.Context(), func(context.Context, AuthoritySnapshot) (PreparedCommand, error) { return stale, nil }); Code(err) != "invalid_command" {
		t.Fatalf("stale commit: %v", err)
	}
	if h.Head() != before || h.AppendCount() != 0 {
		t.Fatal("stale capability wrote authority")
	}
}

func TestAuthoritySuccessorUnboundWriteRejectsKnownRollback(t *testing.T) {
	h := newAuthority(t)
	p := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", Scope: "account", RecoveryEpoch: 1}
	if _, err := h.Service.Execute(t.Context(), p, h.Register(p.AccountID, "30000000-0000-4000-8000-000000000001")); err != nil {
		t.Fatal(err)
	}
	h.heads.head = Head{Generation: 1}
	h.Projection.head = h.heads.head
	p.AccountID = "10000000-0000-4000-8000-000000000002"
	if _, err := h.Service.Execute(t.Context(), p, newVerifiedIdentityRegistration(p.AccountID, "sha256:second-identity", "30000000-0000-4000-8000-000000000002")); err == nil {
		t.Fatal("unbound write accepted known rollback")
	}
}

func TestAuthoritySuccessorPreparedPublishesBeforeFenceRelease(t *testing.T) {
	h := newAuthority(t)
	readView(t, h)
	old := h.Service.authorityProof
	h.Service.fence = &successorFence{Fence: h.Service.fence, beforeRelease: func() {
		owner := h.Service.authorityProof
		if owner == old || owner.head != h.Head() {
			t.Error("prepared successor missing before release")
		}
		if _, ok := owner.state.Session(preparedSession); !ok {
			t.Error("prepared session missing")
		}
	}}
	_, err := h.Service.ExecutePrepared(t.Context(), func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		actor, err := snapshot.ResolveIdentityActor([]string{preparedIdentity}, preparedAccount, 1)
		if err != nil {
			return PreparedCommand{}, err
		}
		return snapshot.PrepareIdentitySessionIssued(actor, preparedIdentitySessionInput("30000000-0000-4000-8000-000000000024"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := old.state.Session(preparedSession); ok {
		t.Fatal("prepared write mutated prior owner")
	}
}
