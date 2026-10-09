package protocol

import "slices"

func FamilyCapabilities() []string {
	return []string{CapabilityIdentityV1, CapabilityFamilyLifecycleV1}
}

func RecoveryCapabilities() []string { return []string{CapabilityRecoveryV1} }

func ServerCapabilities() []string {
	return []string{CapabilityIdentityV1, CapabilityFamilyLifecycleV1, CapabilityRecoveryV1, CapabilityRecordsV1, CapabilitySnapshotV1, CapabilityTimerCommandsV1, CapabilityImportResolverV1, CapabilityLiveHintsV1, CapabilityExportV1}
}

func ValidateFamilyRequestCapabilities(request any) error {
	var capabilities []string
	switch value := request.(type) {
	case *CommandRequest:
		capabilities = value.Capabilities
	case *ConfirmedCommandRequest:
		capabilities = value.Capabilities
	case *UpdateAccountRequest:
		capabilities = value.Capabilities
	case *RepairFamilyProfileRequest:
		capabilities = value.Capabilities
	case *CreateFamilyRequest:
		capabilities = value.Capabilities
	case *PreviewInvitationRequest:
		capabilities = value.Capabilities
	case *RedeemInvitationRequest:
		capabilities = value.Capabilities
	case *TransferOwnershipRequest:
		capabilities = value.Capabilities
	// Absent until now, which made POST /v1/family/members/{id}/role
	// unreachable: an unlisted type falls to the default below and every
	// request to that route was rejected as invalid_request, however well
	// formed and whoever sent it.
	case *MemberRoleRequest:
		capabilities = value.Capabilities
	case *InviteRequest:
		capabilities = value.Capabilities
	default:
		return errInvalidRequest
	}
	return ValidateCapabilities(capabilities, FamilyCapabilities())
}

func ValidateRecoveryRequestCapabilities(request any) error {
	value, ok := request.(*ConfirmedCommandRequest)
	if !ok || !slices.Contains(value.Capabilities, CapabilityRecoveryV1) {
		return errUnsupportedCapability
	}
	return ValidateCapabilities(value.Capabilities, RecoveryCapabilities())
}

type CommandRequest struct {
	RequestMeta
	CommandID string `json:"command_id"`
}

// InviteRequest mints an invitation for a named role.
//
// `role` is optional and defaults to caregiver, so a client built before
// view-only existed keeps working unchanged — and, more to the point, so does
// one built after it that simply does not offer the choice.
type InviteRequest struct {
	CommandRequest
	Role string `json:"role,omitempty"`
}

type ConfirmedCommandRequest struct {
	RequestMeta
	CommandID string `json:"command_id"`
	Confirmed bool   `json:"confirmed"`
}

type UpdateAccountRequest struct {
	ConfirmedCommandRequest
	DisplayName string `json:"display_name"`
}

type RepairFamilyProfileRequest struct {
	ConfirmedCommandRequest
	DisplayName string `json:"display_name"`
}

type CreateFamilyRequest struct {
	CommandRequest
	DisplayName string `json:"display_name"`
}

type PreviewInvitationRequest struct {
	RequestMeta
	Token string `json:"token"`
}

type RedeemInvitationRequest struct {
	ConfirmedCommandRequest
	Token string `json:"token"`
}

type TransferOwnershipRequest struct {
	CommandRequest
	NewOwnerAccountID string `json:"new_owner_account_id"`
}

// MemberRoleRequest changes what a caregiver may do. Owner promotion and
// demotion go through TransferOwnershipRequest, which names a successor.
type MemberRoleRequest struct {
	CommandRequest
	Role string `json:"role"`
}

type FamilySlotResponse struct {
	FamilyID string `json:"family_id"`
	State    string `json:"state"`
}

type AccountResponse struct {
	ProtocolVersion string              `json:"protocol_version"`
	ID              string              `json:"id"`
	DisplayName     string              `json:"display_name"`
	Status          string              `json:"status"`
	ProfileRequired bool                `json:"profile_required"`
	FamilySlot      *FamilySlotResponse `json:"family_slot,omitempty"`
}

type FamilyMemberResponse struct {
	AccountID   string `json:"account_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Status      string `json:"status"`
}

type FamilyResponse struct {
	ProtocolVersion string                 `json:"protocol_version"`
	ID              string                 `json:"id"`
	DisplayName     string                 `json:"display_name"`
	Status          string                 `json:"status"`
	ProfileRequired bool                   `json:"profile_required"`
	Members         []FamilyMemberResponse `json:"members"`
}

type FamilyActorResponse struct {
	AccountID   string `json:"account_id"`
	DisplayName string `json:"display_name"`
	Former      bool   `json:"former"`
}

type FamilyActorPageResponse struct {
	ProtocolVersion string                `json:"protocol_version"`
	Actors          []FamilyActorResponse `json:"actors"`
	HighWater       string                `json:"high_water"`
	NextCursor      string                `json:"next_cursor,omitempty"`
}

type InvitationResponse struct {
	ProtocolVersion   string `json:"protocol_version"`
	ID                string `json:"id"`
	FamilyID          string `json:"family_id"`
	Role              string `json:"role"`
	Status            string `json:"status"`
	ExpiresAt         string `json:"expires_at"`
	CreatedAt         string `json:"created_at"`
	Token             string `json:"token,omitempty"`
	Link              string `json:"link,omitempty"`
	SecretUnavailable bool   `json:"secret_unavailable"`
}

// InvitationSummary is the list-view sibling of InvitationResponse: it never
// carries a token or link, so it is safe to return in bulk.
type InvitationSummary struct {
	ID        string `json:"id"`
	FamilyID  string `json:"family_id"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
}

type PendingInvitationsResponse struct {
	Invitations []InvitationSummary `json:"invitations"`
}

type InvitationPreviewResponse struct {
	ProtocolVersion   string `json:"protocol_version"`
	FamilyDisplayName string `json:"family_display_name"`
	ExpiresAt         string `json:"expires_at"`
}

type CommandResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	CommandID       string `json:"command_id"`
	Outcome         string `json:"outcome"`
	Code            string `json:"code"`
	Revision        uint64 `json:"revision"`
}

type FamilyRecoveryResponse struct {
	ProtocolVersion string `json:"protocol_version"`
	FamilyID        string `json:"family_id"`
	State           string `json:"state"`
	PurgeAfter      string `json:"purge_after"`
	CanRecover      bool   `json:"can_recover"`
}

type AccountDeletionResponse struct {
	CommandResponse
	AppleAuthorizationRevocation string `json:"apple_authorization_revocation"`
}
