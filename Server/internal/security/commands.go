package security

type AccountRegisteredPayload struct {
	Version     string `json:"version"`
	AccountID   string `json:"account_id"`
	IdentityKey string `json:"identity_lookup_digest"`
}

type IdentityAuthorityPayload struct {
	Version            string `json:"version"`
	AccountID          string `json:"account_id"`
	IdentityKey        string `json:"identity_lookup_digest"`
	NotificationDigest string `json:"notification_digest,omitempty"`
}

type DevicePayload struct {
	Version                string `json:"version"`
	AccountID              string `json:"account_id"`
	DeviceID               string `json:"device_id"`
	ServerInstanceID       string `json:"server_instance_id"`
	CanonicalOrigin        string `json:"canonical_origin"`
	DeviceCredentialDigest string `json:"device_credential_digest"`
}

type SessionPayload struct {
	Version                  string `json:"version"`
	AccountID                string `json:"account_id"`
	DeviceID                 string `json:"device_id"`
	SessionID                string `json:"session_id"`
	ParentSessionID          string `json:"parent_session_id,omitempty"`
	LineageRootSessionID     string `json:"lineage_root_session_id"`
	RecoveryEpoch            uint64 `json:"recovery_epoch"`
	ExpiresAt                string `json:"expires_at"`
	Status                   string `json:"status"`
	ProjectionMaterialDigest string `json:"projection_material_digest,omitempty"`
	RefreshAttemptID         string `json:"refresh_attempt_id,omitempty"`
}

type IdentitySessionIssuedPayload struct {
	Version                  string `json:"version"`
	AccountID                string `json:"account_id"`
	IdentityKey              string `json:"identity_key"`
	RegisterAccount          bool   `json:"register_account"`
	DeviceID                 string `json:"device_id"`
	ServerInstanceID         string `json:"server_instance_id"`
	CanonicalOrigin          string `json:"canonical_origin"`
	DeviceCredentialDigest   string `json:"device_credential_digest"`
	RegisterDevice           bool   `json:"register_device"`
	SessionID                string `json:"session_id"`
	LineageRootSessionID     string `json:"lineage_root_session_id"`
	RecoveryEpoch            uint64 `json:"recovery_epoch"`
	ExpiresAt                string `json:"expires_at"`
	ProjectionMaterialDigest string `json:"projection_material_digest"`
}

type FamilyCreatedPayload struct {
	Version                  string `json:"version"`
	FamilyID                 string `json:"family_id"`
	OwnerAccountID           string `json:"owner_account_id"`
	ProjectionMaterialDigest string `json:"projection_material_digest"`
}

type FamilyBootstrappedPayload struct {
	Version                  string `json:"version"`
	FamilyID                 string `json:"family_id"`
	OwnerAccountID           string `json:"owner_account_id"`
	EntitlementKey           string `json:"entitlement_key"`
	EntitlementSource        string `json:"entitlement_source"`
	ProjectionMaterialDigest string `json:"projection_material_digest"`
}

type FamilyProfileRepairedPayload struct {
	Version                  string `json:"version"`
	FamilyID                 string `json:"family_id"`
	OwnerAccountID           string `json:"owner_account_id"`
	ProjectionMaterialDigest string `json:"projection_material_digest"`
}

type InvitationPayload struct {
	Version           string `json:"version"`
	InvitationID      string `json:"invitation_id"`
	FamilyID          string `json:"family_id"`
	CreatorAccountID  string `json:"creator_account_id"`
	RedeemerAccountID string `json:"redeemer_account_id,omitempty"`
	TokenDigest       string `json:"token_digest"`
	Role              string `json:"role"`
	Status            string `json:"status"`
	ExpiresAt         string `json:"expires_at"`
}

type MembershipPayload struct {
	Version   string `json:"version"`
	AccountID string `json:"account_id"`
	FamilyID  string `json:"family_id"`
}

// MemberRolePayload changes what a caregiver may do, without changing who is
// in the family. Owner promotion and demotion are `owner_transferred`, which
// is a different command because a family must always have exactly one owner.
type MemberRolePayload struct {
	Version   string `json:"version"`
	AccountID string `json:"account_id"`
	FamilyID  string `json:"family_id"`
	Role      string `json:"role"`
}

type OwnerChangePayload struct {
	Version                string `json:"version"`
	FamilyID               string `json:"family_id"`
	OldOwnerAccountID      string `json:"old_owner_account_id"`
	NewOwnerAccountID      string `json:"new_owner_account_id"`
	OperatorPolicyID       string `json:"operator_policy_id,omitempty"`
	OperatorEvidenceDigest string `json:"operator_evidence_digest,omitempty"`
	OperatorReasonCode     string `json:"operator_reason_code,omitempty"`
	ExpectedRevision       uint64 `json:"expected_revision,omitempty"`
}

type EntitlementPayload struct {
	Version        string `json:"version"`
	FamilyID       string `json:"family_id"`
	EntitlementKey string `json:"entitlement_key"`
	Source         string `json:"source"`
	Status         string `json:"status"`
	ExpiresAt      string `json:"expires_at,omitempty"`
}

type OperatorEntitlementPayload struct {
	Version                string `json:"version"`
	FamilyID               string `json:"family_id"`
	EntitlementKey         string `json:"entitlement_key"`
	Source                 string `json:"source"`
	Status                 string `json:"status"`
	ExpiresAt              string `json:"expires_at,omitempty"`
	OperatorPolicyID       string `json:"operator_policy_id"`
	OperatorEvidenceDigest string `json:"operator_evidence_digest"`
	OperatorReasonCode     string `json:"operator_reason_code"`
	ExpectedRevision       uint64 `json:"expected_revision"`
}

type FamilyLifecyclePayload struct {
	Version             string   `json:"version"`
	FamilyID            string   `json:"family_id"`
	ClaimOwnerAccountID string   `json:"claim_owner_account_id"`
	PreviousMemberIDs   []string `json:"previous_member_ids,omitempty"`
	PreviousRoles       []string `json:"previous_roles,omitempty"`
	PurgeAfter          string   `json:"purge_after,omitempty"`
}

type AccountDeletedPayload struct {
	Version     string `json:"version"`
	AccountID   string `json:"account_id"`
	IdentityKey string `json:"identity_lookup_digest"`
}

type FamilyPurgeStartedPayload struct {
	Version             string `json:"version"`
	FamilyID            string `json:"family_id"`
	PurgeAfter          string `json:"purge_after"`
	StartedAt           string `json:"started_at"`
	PurgeStartCommandID string `json:"purge_start_command_id"`
	PurgeStartRevision  uint64 `json:"purge_start_revision"`
	ServerInstanceID    string `json:"server_instance_id"`
}

type FamilyPurgeCompletedPayload struct {
	Version               string `json:"version"`
	FamilyID              string `json:"family_id"`
	PhysicalPurgeReceipt  string `json:"physical_purge_receipt"`
	CompletedAt           string `json:"completed_at"`
	ExpectedStartRevision uint64 `json:"expected_start_revision"`
	ServerInstanceID      string `json:"server_instance_id"`
}
