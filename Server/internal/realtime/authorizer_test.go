package realtime

import (
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/testkit"
)

func TestAuthorityAuthorizerDeniesRemovedMemberInOneBatch(t *testing.T) {
	h := testkit.New(t)
	authorizer, err := NewAuthorityAuthorizer(h.Security, false, func() time.Time { return h.Now })
	if err != nil {
		t.Fatal(err)
	}
	leasing := []AuthorizationLease{
		{Principal: h.Owner, Grant: access.Grant{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID, Role: "owner", Revision: h.Owner.SecurityRevision}},
		{Principal: h.Caregiver, Grant: access.Grant{AccountID: h.Caregiver.AccountID, FamilyID: h.FamilyID, Role: "caregiver", Revision: h.Caregiver.SecurityRevision}},
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
	authorizer, _ := NewAuthorityAuthorizer(h.Security, false, func() time.Time { return h.Now })
	grant := access.Grant{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID, Role: "owner", Revision: h.Owner.SecurityRevision}
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
