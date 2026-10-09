package security

import "testing"

// The role vocabulary the ledger will accept. Anything else is a corrupt
// event, not a role to guess at.
func TestMemberRoleEventAcceptsOnlyCaregiverAndViewer(t *testing.T) {
	for _, tc := range []struct {
		role string
		want bool
	}{
		{"caregiver", true},
		{"viewer", true},
		{"owner", false},
		{"", false},
		{"admin", false},
	} {
		state := &State{memberships: map[string]membershipState{
			"f/a": {AccountID: "a", FamilyID: "f", Role: "caregiver", Status: "active"},
		}}
		event := Event{Effects: []Effect{{
			Kind: "member_role_changed", AccountID: "a", FamilyID: "f", Role: tc.role, Status: "active",
		}}}
		err := applyMemberRoleChanged(state, event)
		if (err == nil) != tc.want {
			t.Errorf("role %q: err=%v, want accepted=%v", tc.role, err, tc.want)
		}
		if tc.want && state.memberships["f/a"].Role != tc.role {
			t.Errorf("role %q was not applied", tc.role)
		}
	}
}

// A family must always have exactly one owner. Demoting one through this
// command would leave it with none, so the ledger refuses it outright —
// stepping down is owner_transferred, which names a successor.
func TestMemberRoleEventRefusesToDemoteAnOwner(t *testing.T) {
	state := &State{memberships: map[string]membershipState{
		"f/a": {AccountID: "a", FamilyID: "f", Role: "owner", Status: "active"},
	}}
	event := Event{Effects: []Effect{{
		Kind: "member_role_changed", AccountID: "a", FamilyID: "f", Role: "viewer", Status: "active",
	}}}

	if err := applyMemberRoleChanged(state, event); err == nil {
		t.Fatal("an owner was demoted by a role change")
	}
	if state.memberships["f/a"].Role != "owner" {
		t.Fatal("the owner's role was mutated by a refused event")
	}
}

// A removed member is not a member. Changing their role would quietly
// resurrect them.
func TestMemberRoleEventRefusesARemovedMember(t *testing.T) {
	state := &State{memberships: map[string]membershipState{
		"f/a": {AccountID: "a", FamilyID: "f", Role: "caregiver", Status: "removed"},
	}}
	event := Event{Effects: []Effect{{
		Kind: "member_role_changed", AccountID: "a", FamilyID: "f", Role: "viewer", Status: "active",
	}}}

	if err := applyMemberRoleChanged(state, event); err == nil {
		t.Fatal("a removed member's role was changed")
	}
}

func TestMemberRoleEventRequiresAKnownMembership(t *testing.T) {
	state := &State{memberships: map[string]membershipState{}}
	event := Event{Effects: []Effect{{
		Kind: "member_role_changed", AccountID: "a", FamilyID: "f", Role: "viewer", Status: "active",
	}}}

	if err := applyMemberRoleChanged(state, event); err == nil {
		t.Fatal("a role was set for a membership that does not exist")
	}
}
