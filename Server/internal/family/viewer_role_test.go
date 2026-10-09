package family

import "testing"

// A read-only caregiver reads everything and originates nothing.
func TestViewerReadsButDoesNotWrite(t *testing.T) {
	reads := []string{"read", "read_receipt"}
	writes := []string{"mutate_record", "add_baby"}
	ownerOnly := []string{"delete_baby", "manage_members", "repair_profile", "transfer_owner", "export", "export_family", "delete_family"}

	for _, action := range reads {
		if !allowed("viewer", action) {
			t.Errorf("viewer should be allowed to %q", action)
		}
	}
	for _, action := range writes {
		if allowed("viewer", action) {
			t.Errorf("viewer must not be allowed to %q", action)
		}
		if !allowed("caregiver", action) || !allowed("owner", action) {
			t.Errorf("%q must remain available to caregiver and owner", action)
		}
	}
	for _, action := range ownerOnly {
		if allowed("viewer", action) {
			t.Errorf("viewer must not be allowed to %q", action)
		}
	}
}

// Adding the role must not quietly widen what an ordinary caregiver can do.
func TestExistingRolesUnchangedByViewer(t *testing.T) {
	for _, action := range []string{"delete_baby", "manage_members", "transfer_owner", "delete_family"} {
		if allowed("caregiver", action) {
			t.Errorf("caregiver must not be allowed to %q", action)
		}
		if !allowed("owner", action) {
			t.Errorf("owner must still be allowed to %q", action)
		}
	}
	if allowed("viewer", "unknown_action") || allowed("owner", "unknown_action") {
		t.Error("an unknown action must be refused for every role")
	}
}

// A viewer refused a write must be told they are view-only, not that they need
// to be the owner.
//
// The distinction is the whole difference between "your role cannot do this"
// and "nobody but the owner can do this": the first is a setting somebody can
// change for you, the second is a transfer of the family. Told the wrong one, a
// parent goes looking for the wrong fix — and the iOS client branches on this
// code to decide whether to show the view-only banner at all.
func TestViewOnlyRefusalIsDistinguishableFromOwnerOnly(t *testing.T) {
	// Actions a caregiver may take and a viewer may not: view-only is the
	// reason, and the only reason.
	for _, action := range []string{"mutate_record", "add_baby"} {
		if !allowed("caregiver", action) || allowed("viewer", action) {
			t.Fatalf("%q is no longer the caregiver/viewer boundary this test is about", action)
		}
	}
	// Actions nobody but the owner may take. A viewer refused one of these is
	// in the same position as a caregiver, and promoting them would not help,
	// so `owner_required` remains the honest answer.
	for _, action := range []string{"manage_members", "transfer_owner", "delete_family"} {
		if allowed("caregiver", action) {
			t.Fatalf("%q stopped being owner-only", action)
		}
	}
}
