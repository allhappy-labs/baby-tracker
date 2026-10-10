package sync

import (
	"errors"
	"testing"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/family"
	"babytracker/server/internal/protocol"
)

func TestActionForMutationUsesNarrowAuthority(t *testing.T) {
	for _, test := range []struct{ kind, operation, want string }{
		{"baby", "create", "add_baby"}, {"baby", "update", "mutate_record"},
		{"baby", "delete", "delete_baby"}, {"feed", "delete", "mutate_record"},
	} {
		got, err := care.ActionFor(protocol.Mutation{EntityKind: test.kind, Operation: test.operation})
		if err != nil || got != test.want {
			t.Fatalf("%s/%s action=%q err=%v", test.kind, test.operation, got, err)
		}
	}
}

// The batch policy refuses as the authority's family grant does: a viewer
// where a caregiver may act is read_only_caregiver, another non-owner
// owner_required, an unknown role membership_revoked.
func TestPolicyRefusesLikeTheFamilyGrant(t *testing.T) {
	policy := policyOf(family.Allowed)
	for _, test := range []struct {
		role, action string
		want         error
	}{
		{"owner", "delete_baby", nil}, {"caregiver", "mutate_record", nil}, {"caregiver", "add_baby", nil},
		{"viewer", "mutate_record", care.ErrReadOnlyCaregiver}, {"viewer", "add_baby", care.ErrReadOnlyCaregiver},
		{"viewer", "delete_baby", authority.ErrOwnerRequired}, {"caregiver", "delete_baby", authority.ErrOwnerRequired},
		{"", "mutate_record", authority.ErrMembershipRevoked},
	} {
		if err := policy(test.role, test.action); !errors.Is(err, test.want) || (test.want == nil && err != nil) {
			t.Fatalf("%s %s: %v, want %v", test.role, test.action, err, test.want)
		}
	}
	if policyOf(nil) != nil {
		t.Fatal("a service without the family policy has a policy")
	}
}
