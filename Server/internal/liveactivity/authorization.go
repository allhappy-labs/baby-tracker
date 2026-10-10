package liveactivity

import "babytracker/server/internal/authority"

// SubjectFromClaim carries only scheduling identity. The authority snapshot
// resolves the registration's lineage and membership from the database
// (app.live_activity_authorized) under the claim's tenant context.
func SubjectFromClaim(c Claim) authority.LiveActivitySubject {
	return authority.LiveActivitySubject{JobID: c.JobID, LeaseID: c.LeaseID, RegistrationID: c.RegistrationID,
		RegistrationVersion: c.RegistrationVersion, AccountID: c.AccountID, DeviceID: c.DeviceID, FamilyID: c.FamilyID,
		BabyID: c.BabyID, Kind: c.Kind, InstanceID: c.InstanceID, RecoveryEpoch: c.RecoveryEpoch, DirtyVersion: c.DirtyVersion, LeaseDeadline: c.LeaseDeadline}
}
