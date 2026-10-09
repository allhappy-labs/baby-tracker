package security

import (
	"testing"
	"time"

	"babytracker/server/internal/access"
)

func TestLiveActivityMembershipReplayLifetime(t *testing.T) {
	s, owner, caregiver := familyCommandTestState()
	family := "f"
	event := Event{Revision: 12, Effects: []Effect{{AccountID: owner.AccountID, FamilyID: family}}}
	if err := applyFamilyCreated(&s, event); err != nil {
		t.Fatal(err)
	}
	if s.MembershipGeneration(family, owner.AccountID) != 12 {
		t.Fatal("create lost event revision")
	}
	s.invitations["i"] = invitationState{FamilyID: family, Role: "caregiver", Status: "active"}
	join := Event{Revision: 15, Effects: []Effect{{AccountID: caregiver.AccountID, FamilyID: family, InvitationID: "i", Role: "caregiver"}}}
	if err := applyInvitationRedeemed(&s, join); err != nil {
		t.Fatal(err)
	}
	role := Event{Revision: 16, Effects: []Effect{{AccountID: caregiver.AccountID, FamilyID: family, Role: "viewer"}}}
	if err := applyMemberRoleChanged(&s, role); err != nil {
		t.Fatal(err)
	}
	if s.Clone().MembershipGeneration(family, caregiver.AccountID) != 15 {
		t.Fatal("role or clone reset lifetime")
	}
	if err := applyMemberRemoved(&s, role); err != nil {
		t.Fatal(err)
	}
	if s.MembershipGeneration(family, caregiver.AccountID) != 0 {
		t.Fatal("removed lifetime still active")
	}
	s.invitations["j"] = invitationState{FamilyID: family, Role: "caregiver", Status: "active"}
	join.Revision = 19
	join.Effects[0].InvitationID = "j"
	if err := applyInvitationRedeemed(&s, join); err != nil {
		t.Fatal(err)
	}
	if s.MembershipGeneration(family, caregiver.AccountID) != 19 {
		t.Fatal("rejoin reused lifetime")
	}
	transfer := Event{Revision: 20, Effects: []Effect{{AccountID: owner.AccountID, FamilyID: family, Role: "caregiver"}, {AccountID: caregiver.AccountID, FamilyID: family, Role: "owner"}}}
	if err := applyOwnerTransferred(&s, transfer); err != nil {
		t.Fatal(err)
	}
	if s.MembershipGeneration(family, owner.AccountID) != 12 || s.MembershipGeneration(family, caregiver.AccountID) != 19 {
		t.Fatal("transfer reset lifetime")
	}
}

func TestLiveActivityAuthorityLineageFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	base := NewState()
	base.accounts["a"] = accountState{Status: "active"}
	base.devices["d"] = deviceState{AccountID: "a", Status: "active", ServerInstanceID: "i"}
	base.familySlots["a"] = familySlotState{FamilyID: "f", State: "active"}
	base.families["f"] = familyState{Status: "active"}
	base.memberships["f/a"] = membershipState{AccountID: "a", FamilyID: "f", Role: "viewer", Status: "active", Generation: 5}
	base.sessionLineages["root"] = sessionState{AccountID: "a", DeviceID: "d", LineageRootSessionID: "root", Status: "rotated", ChildSessionID: "child", RecoveryEpoch: 1, ExpiresAt: expires.Format(time.RFC3339Nano)}
	child := base.sessionLineages["root"]
	child.Status = "active"
	child.ParentSessionID = "root"
	child.ChildSessionID = ""
	base.sessionLineages["child"] = child
	subject := LiveActivitySubject{AccountID: "a", DeviceID: "d", FamilyID: "f", InstanceID: "i", RecoveryEpoch: 1}
	binding := liveActivityBinding{sessionID: "root", rootID: "root", membership: 5, expires: expires, currentRealm: true}
	grant, err := base.liveActivityGrant(subject, binding, 1, now, false)
	if err != nil || access.RoleMayWrite(grant.Role) {
		t.Fatalf("viewer rotated authority: %+v %v", grant, err)
	}
	for _, name := range []string{"cycle", "foreign_child", "wrong_epoch", "revoked", "expired", "missing_child", "new_membership", "deleted", "revoked_device", "family_switch", "entitlement"} {
		t.Run(name, func(t *testing.T) {
			s := base.Clone()
			c := s.sessionLineages["child"]
			switch name {
			case "cycle":
				c.Status = "rotated"
				c.ChildSessionID = "root"
				r := s.sessionLineages["root"]
				r.ParentSessionID = "child"
				s.sessionLineages["root"] = r
			case "foreign_child":
				c.AccountID = "other"
			case "wrong_epoch":
				c.RecoveryEpoch = 2
			case "revoked":
				c.Status = "revoked"
			case "expired":
				c.ExpiresAt = now.Format(time.RFC3339Nano)
			case "missing_child":
				r := s.sessionLineages["root"]
				r.ChildSessionID = "absent"
				s.sessionLineages["root"] = r
			case "new_membership":
				m := s.memberships["f/a"]
				m.Generation = 8
				s.memberships["f/a"] = m
			case "deleted":
				s.accounts["a"] = accountState{Status: "deleted"}
			case "revoked_device":
				d := s.devices["d"]
				d.Status = "revoked"
				s.devices["d"] = d
			case "family_switch":
				s.familySlots["a"] = familySlotState{FamilyID: "other", State: "active"}
			}
			s.sessionLineages["child"] = c
			if _, err := s.liveActivityGrant(subject, binding, 1, now, name == "entitlement"); err == nil {
				t.Fatal("stale authority accepted")
			}
		})
	}
}
