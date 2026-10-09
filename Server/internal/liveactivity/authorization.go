package liveactivity

import "babytracker/server/internal/security"

// SubjectFromClaim carries only scheduling identity. The guard resolves the
// registering session and membership lifetime from runtime tenant RLS.
func SubjectFromClaim(c Claim) security.LiveActivitySubject {
	return security.LiveActivitySubject{JobID: c.JobID, LeaseID: c.LeaseID, RegistrationID: c.RegistrationID,
		RegistrationVersion: c.RegistrationVersion, AccountID: c.AccountID, DeviceID: c.DeviceID, FamilyID: c.FamilyID,
		BabyID: c.BabyID, Kind: c.Kind, InstanceID: c.InstanceID, RecoveryEpoch: c.RecoveryEpoch, DirtyVersion: c.DirtyVersion, LeaseDeadline: c.LeaseDeadline}
}
