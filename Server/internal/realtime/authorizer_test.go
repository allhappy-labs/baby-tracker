package realtime

import (
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// liveGrant is the grant the live route admits a principal with: family
// policy's read grant for its session.
func liveGrant(t *testing.T, h *testkit.Harness, p access.Principal) access.Grant {
	t.Helper()
	var g access.Grant
	if err := h.Authority.WithFamilyReadTx(t.Context(), p, "read", func(_ pgx.Tx, grant access.Grant) error {
		g = grant
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if g.FamilyID != h.FamilyID {
		t.Fatalf("grant %+v, want family %s", g, h.FamilyID)
	}
	return g
}

func TestAuthorityAuthorizerDeniesRemovedMemberInOneBatch(t *testing.T) {
	h := testkit.New(t)
	authorizer, err := NewAuthorityAuthorizer(h.Authority)
	if err != nil {
		t.Fatal(err)
	}
	leasing := []AuthorizationLease{
		{Principal: h.Owner, Grant: liveGrant(t, h, h.Owner)},
		{Principal: h.Caregiver, Grant: liveGrant(t, h, h.Caregiver)},
	}
	allowed, err := authorizer.CheckMany(t.Context(), leasing)
	if err != nil || len(allowed) != 2 || !allowed[0] || !allowed[1] {
		t.Fatalf("before allowed=%v err=%v", allowed, err)
	}
	if _, err = h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	allowed, err = authorizer.CheckMany(t.Context(), leasing)
	if err != nil || len(allowed) != 2 || !allowed[0] || allowed[1] {
		t.Fatalf("after allowed=%v err=%v", allowed, err)
	}
}

func TestAuthorityAuthorizerDeniesExpiredRevokedAndWrongEpochSessions(t *testing.T) {
	h := testkit.New(t)
	authorizer, _ := NewAuthorityAuthorizer(h.Authority)
	grant := liveGrant(t, h, h.Owner)
	expired := h.Owner
	expired.AccessExpiresAt = h.Now
	wrongEpoch := h.Owner
	wrongEpoch.RecoveryEpoch++
	_ = h.Refresh("owner")
	allowed, err := authorizer.CheckMany(t.Context(), []AuthorizationLease{
		{Principal: expired, Grant: grant}, {Principal: wrongEpoch, Grant: grant}, {Principal: h.Owner, Grant: grant},
	})
	if err != nil || len(allowed) != 3 || allowed[0] || allowed[1] || allowed[2] {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
}

// The hub subscribed to the real authority closes a live connection as soon
// as the revoking command commits, for both invalidation classes: a member
// removal (family seq) and a logout (account seq, one session). The owner's
// grant revision (a family seq) is pushed ahead of the logout's account seq,
// so the logout would be skipped if its revision were compared with the
// grant instead of the session's issued seq.
func TestHubClosesLiveConnectionsOnCommittedRevocations(t *testing.T) {
	h := testkit.New(t)
	authorizer, err := NewAuthorityAuthorizer(h.Authority)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := NewHub(t.Context(), HubOptions{Limits: protocol.DefaultLimits(), Authorizer: authorizer, Invalidations: h.Authority,
		Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	var _ authority.InvalidationOverflowSink = hub
	viewer := h.AddMember(t, "viewer", "viewer")
	for range 6 {
		if _, err := h.Family.Invite(t.Context(), h.Owner, h.CommandID(), "caregiver"); err != nil {
			t.Fatal(err)
		}
	}
	ownerGrant := liveGrant(t, h, h.Owner)
	caregiver := boundLease(t, hub, h.Caregiver, liveGrant(t, h, h.Caregiver))
	owner := boundLease(t, hub, h.Owner, ownerGrant)
	kept := boundLease(t, hub, viewer, liveGrant(t, h, viewer))

	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	waitCloses(t, caregiver, ClosePolicyViolation)

	logout, err := h.Auth.Logout(t.Context(), h.Owner, h.CommandID())
	if err != nil {
		t.Fatal(err)
	}
	if logout.Revision > ownerGrant.Revision || logout.Revision <= h.Owner.SecurityRevision {
		t.Fatalf("precondition: logout account seq %d must be at or below the grant's family seq %d and above the session's %d",
			logout.Revision, ownerGrant.Revision, h.Owner.SecurityRevision)
	}
	waitCloses(t, owner, ClosePolicyViolation)

	if closeCount(kept) != 0 {
		t.Fatal("unaffected member's connection closed")
	}
	if n := leaseCount(hub); n != 1 {
		t.Fatalf("bound leases=%d, want the viewer's only", n)
	}
}
