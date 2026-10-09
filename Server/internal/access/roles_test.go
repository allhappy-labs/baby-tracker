package access

import "testing"

func TestRoleMayWrite(t *testing.T) {
	for _, tc := range []struct {
		role string
		want bool
	}{
		{"owner", true},
		{"caregiver", true},
		{"viewer", false},
		// Fails closed. An unrecognised role must never be assumed harmless:
		// guessing upwards is a silent unauthorized write.
		{"", false},
		{"admin", false},
		{"Owner", false},
		{"superadmin", false},
	} {
		if got := RoleMayWrite(tc.role); got != tc.want {
			t.Errorf("RoleMayWrite(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}
}
