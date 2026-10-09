package access

import "time"

type Principal struct {
	AccountID        string
	DeviceID         string
	SessionID        string
	RecoveryEpoch    uint64
	SecurityRevision uint64
	Scope            string
	SessionScope     string
	SessionFamilyID  string
	AccessExpiresAt  time.Time
}

type Grant struct {
	// MembershipRevision identifies the current membership lifetime in authority replay; zero is unavailable.
	MembershipRevision uint64
	AccountID          string
	FamilyID           string
	Role               string
	Revision           uint64
}

// RoleMayWrite is the single server-side answer to whether a membership role
// may originate a mutation.
//
// It fails closed. An unrecognised role — one written by a newer binary during
// a rolling deploy, or by hand — is refused rather than assumed harmless,
// because the failure mode of guessing upwards is silent unauthorized writes.
func RoleMayWrite(role string) bool {
	switch role {
	case "owner", "caregiver":
		return true
	default:
		return false
	}
}
