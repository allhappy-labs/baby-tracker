package security

import (
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"babytracker/server/internal/access"
)

type AccountView struct{ IdentityKey, Status string }
type FamilyView struct {
	OwnerAccountID, Status, PurgeAfter, PurgeStartCommandID, PurgeServerInstanceID, PhysicalPurgeReceipt, PurgedAt string
	PurgeStartRevision                                                                                             uint64
}
type FamilySlotView struct{ FamilyID, State string }
type MembershipView struct {
	AccountID, FamilyID, Role, Status string
	Generation                        uint64
}
type DeviceView struct{ AccountID, ServerInstanceID, CanonicalOrigin, CredentialDigest, Status string }
type InvitationView struct{ AccountID, FamilyID, TokenDigest, Role, Status, ExpiresAt string }
type SessionView struct {
	ProjectionMaterialDigest, RefreshAttemptID, RotationAttemptID, ChildSessionID string
	AccountID, DeviceID, ParentSessionID, LineageRootSessionID, Status, ExpiresAt string
	RecoveryEpoch, SecurityRevision                                               uint64
}
type EntitlementView struct{ Status, Source, ExpiresAt string }
type RecoveryClaimView struct{ AccountID, FamilyID, Status string }
type ReceiptView struct {
	CommandHash [32]byte
	Result      CommandResult
	Kind        EventKind
	FamilyIDs   []string
}

type accountState struct {
	IdentityKey        string
	Status             string
	registeredRevision uint64
}
type familyState FamilyView
type familySlotState FamilySlotView
type membershipState MembershipView
type deviceState DeviceView
type invitationState InvitationView
type sessionState SessionView
type entitlementState EntitlementView
type recoveryClaimState RecoveryClaimView
type receipt struct {
	CommandHash  [32]byte
	Result       CommandResult
	eventKind    EventKind
	preparedKind preparedKind
	familyIDs    []string
}

type State struct {
	head            Head
	accounts        map[string]accountState
	familySlots     map[string]familySlotState
	families        map[string]familyState
	memberships     map[string]membershipState
	devices         map[string]deviceState
	invitations     map[string]invitationState
	sessionLineages map[string]sessionState
	entitlements    map[string]entitlementState
	recoveryClaims  map[string]recoveryClaimState
	commandReceipts map[string]receipt
}

func NewState() State {
	return State{
		head: Head{Generation: 1}, accounts: map[string]accountState{}, familySlots: map[string]familySlotState{},
		families: map[string]familyState{}, memberships: map[string]membershipState{}, devices: map[string]deviceState{},
		invitations: map[string]invitationState{}, sessionLineages: map[string]sessionState{}, entitlements: map[string]entitlementState{},
		recoveryClaims: map[string]recoveryClaimState{}, commandReceipts: map[string]receipt{},
	}
}

func (state State) Head() Head { return state.head }
func (state State) Account(id string) (AccountView, bool) {
	item, ok := state.accounts[id]
	return AccountView{IdentityKey: item.IdentityKey, Status: item.Status}, ok
}

type CredentialKeyInventory struct {
	SessionToken     []string
	Identity         []string
	DeviceContinuity []string
	Invitation       []string
}

type IdentityAuthorityResolution struct {
	Status                                   string
	AccountID, IdentityKey                   string
	LifecycleGeneration, NextActorGeneration uint64
}

func (state State) ReferencedCredentialKeyIDs() CredentialKeyInventory {
	identity := map[string]struct{}{}
	devices := map[string]struct{}{}
	invitations := map[string]struct{}{}
	for _, account := range state.accounts {
		if keyID, ok := keyedDigestID(account.IdentityKey); ok {
			identity[keyID] = struct{}{}
		}
	}
	for _, device := range state.devices {
		if device.Status != "active" {
			continue
		}
		if keyID, ok := keyedDigestID(device.CredentialDigest); ok {
			devices[keyID] = struct{}{}
		}
	}
	for _, invitation := range state.invitations {
		if keyID, ok := keyedDigestID(invitation.TokenDigest); ok {
			invitations[keyID] = struct{}{}
		}
	}
	return CredentialKeyInventory{Identity: sortedStringSet(identity), DeviceContinuity: sortedStringSet(devices), Invitation: sortedStringSet(invitations)}
}

func (state State) ResolveIdentityAuthority(candidates []string) (IdentityAuthorityResolution, error) {
	if len(candidates) == 0 || len(candidates) > 8 {
		return IdentityAuthorityResolution{}, errorCode("invalid_command")
	}
	candidateSet := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if !keyedDigest.MatchString(candidate) {
			return IdentityAuthorityResolution{}, errorCode("invalid_command")
		}
		if _, exists := candidateSet[candidate]; exists {
			return IdentityAuthorityResolution{}, errorCode("invalid_command")
		}
		candidateSet[candidate] = struct{}{}
	}
	type match struct {
		accountID string
		account   accountState
	}
	matches := make([]match, 0)
	for accountID, account := range state.accounts {
		if _, ok := candidateSet[account.IdentityKey]; ok {
			matches = append(matches, match{accountID: accountID, account: account})
		}
	}
	if len(matches) == 0 {
		return IdentityAuthorityResolution{Status: "absent", NextActorGeneration: 1}, nil
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].account.registeredRevision == matches[j].account.registeredRevision {
			return matches[i].accountID < matches[j].accountID
		}
		return matches[i].account.registeredRevision < matches[j].account.registeredRevision
	})
	if uint64(len(matches)) > maxWireInteger {
		return IdentityAuthorityResolution{}, errorCode("authorization_quarantine")
	}
	live := -1
	for index, item := range matches {
		if item.account.registeredRevision == 0 || item.account.registeredRevision > maxWireInteger ||
			(index > 0 && item.account.registeredRevision <= matches[index-1].account.registeredRevision) {
			return IdentityAuthorityResolution{}, errorCode("authorization_quarantine")
		}
		switch item.account.Status {
		case "deleted":
		case "active", "identity_unavailable":
			if live >= 0 {
				return IdentityAuthorityResolution{}, errorCode("authorization_quarantine")
			}
			live = index
		default:
			return IdentityAuthorityResolution{}, errorCode("authorization_quarantine")
		}
	}
	if live >= 0 && live != len(matches)-1 {
		return IdentityAuthorityResolution{}, errorCode("authorization_quarantine")
	}
	selected := matches[len(matches)-1]
	generation := uint64(len(matches))
	return IdentityAuthorityResolution{Status: selected.account.Status, AccountID: selected.accountID, IdentityKey: selected.account.IdentityKey,
		LifecycleGeneration: generation, NextActorGeneration: generation + 1}, nil
}

func keyedDigestID(value string) (string, bool) {
	if !keyedDigest.MatchString(value) {
		return "", false
	}
	parts := strings.SplitN(value, ":", 3)
	return parts[1], true
}

func sortedStringSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func (state State) Family(id string) (FamilyView, bool) {
	item, ok := state.families[id]
	return FamilyView(item), ok
}
func (state State) FamilySlot(accountID string) (FamilySlotView, bool) {
	item, ok := state.familySlots[accountID]
	return FamilySlotView(item), ok
}
func (state State) Membership(familyID, accountID string) (MembershipView, bool) {
	item, ok := state.memberships[familyID+"/"+accountID]
	return MembershipView(item), ok
}

// MembershipGeneration is the ledger revision which began this membership lifetime.
func (state State) MembershipGeneration(familyID, accountID string) uint64 {
	member, ok := state.memberships[familyID+"/"+accountID]
	if !ok || member.Status != "active" {
		return 0
	}
	return member.Generation
}
func (state State) Device(id string) (DeviceView, bool) {
	item, ok := state.devices[id]
	return DeviceView(item), ok
}
func (state State) Invitation(id string) (InvitationView, bool) {
	item, ok := state.invitations[id]
	return InvitationView(item), ok
}
func (state State) Session(id string) (SessionView, bool) {
	item, ok := state.sessionLineages[id]
	return SessionView(item), ok
}
func (state State) Entitlement(familyID, entitlementKey string) (EntitlementView, bool) {
	item, ok := state.entitlements[familyID+"/"+entitlementKey]
	return EntitlementView(item), ok
}
func (state State) RecoveryClaim(familyID, accountID string) (RecoveryClaimView, bool) {
	item, ok := state.recoveryClaims[familyID+"/"+accountID]
	return RecoveryClaimView(item), ok
}
func (state State) Memberships(familyID string) []MembershipView {
	items := make([]MembershipView, 0)
	for _, item := range state.memberships {
		if item.FamilyID == familyID {
			items = append(items, MembershipView(item))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].AccountID < items[j].AccountID })
	return items
}
func (state State) Receipt(accountID, commandID string) (ReceiptView, bool) {
	item, ok := state.commandReceipts[accountID+"/"+commandID]
	return ReceiptView{CommandHash: item.CommandHash, Result: item.Result, Kind: item.eventKind, FamilyIDs: append([]string(nil), item.familyIDs...)}, ok
}

func (state State) Clone() State {
	clone := NewState()
	clone.head = state.head
	for key, value := range state.accounts {
		clone.accounts[key] = value
	}
	for key, value := range state.familySlots {
		clone.familySlots[key] = value
	}
	for key, value := range state.families {
		clone.families[key] = value
	}
	for key, value := range state.memberships {
		clone.memberships[key] = value
	}
	for key, value := range state.devices {
		clone.devices[key] = value
	}
	for key, value := range state.invitations {
		clone.invitations[key] = value
	}
	for key, value := range state.sessionLineages {
		clone.sessionLineages[key] = value
	}
	for key, value := range state.entitlements {
		clone.entitlements[key] = value
	}
	for key, value := range state.recoveryClaims {
		clone.recoveryClaims[key] = value
	}
	for key, value := range state.commandReceipts {
		value.familyIDs = append([]string(nil), value.familyIDs...)
		clone.commandReceipts[key] = value
	}
	return clone
}

func (state State) Decide(principal access.Principal, command Command, now time.Time) (Event, CommandResult, error) {
	return state.decide(principal, command, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10})
}

func (state State) decide(principal access.Principal, command Command, now time.Time, policy FamilyPolicy) (Event, CommandResult, error) {
	commandHash, err := CanonicalCommandHash(principal, command)
	if err != nil {
		return Event{}, CommandResult{}, err
	}
	receiptKey := principal.AccountID + "/" + command.ID
	if existing, ok := state.commandReceipts[receiptKey]; ok {
		if existing.CommandHash != commandHash {
			return Event{}, CommandResult{}, errorCode("command_id_reused")
		}
		return Event{}, existing.Result, nil
	}
	if command.Kind == "account_registered" {
		if !command.verifiedIdentity {
			return Event{}, CommandResult{}, errorCode("invalid_command")
		}
		return state.decideAccountRegistration(principal, command, commandHash, now)
	}
	switch command.Kind {
	case "family_created", "family_bootstrapped", "family_profile_repaired", "invitation_created", "invitation_redeemed", "invitation_revoked", "member_left", "member_removed", "member_role_changed", "owner_transferred",
		"family_deletion_scheduled", "family_recovered", "recovery_forfeited", "account_deleted":
	default:
		return Event{}, CommandResult{}, errorCode("invalid_command")
	}
	if principal.DeviceID == "" || principal.SessionID == "" {
		return Event{}, CommandResult{}, errorCode("invalid_command")
	}
	if !state.validFamilyPrincipal(principal, now) || policy.MaxCaregivers < 1 || policy.MaxInvitations < 1 {
		return Event{}, CommandResult{}, errorCode("session_revoked")
	}
	effects, err := state.decideFamilyEffects(principal, command, now, policy)
	if err != nil {
		return Event{}, CommandResult{}, err
	}
	if !affectedIDsMatch(command, effects) {
		return Event{}, CommandResult{}, errorCode("invalid_command")
	}
	sortedEffects(effects)
	result := CommandResult{CommandID: command.ID, Outcome: "committed", Code: "ok", Revision: state.head.Revision + 1}
	eventAt := now.UTC()
	if command.Kind == "family_bootstrapped" {
		// The bootstrap entitlement is defined on whole-second boundaries, and
		// the projector requires its expiry to be exactly 366 days after the
		// event envelope time.
		eventAt = eventAt.Truncate(time.Second)
	}
	event := Event{Version: "v1", Generation: state.head.Generation, Revision: result.Revision,
		PreviousHash: hex.EncodeToString(state.head.EventHash[:]), CommandID: command.ID, CommandHash: hex.EncodeToString(commandHash[:]),
		ActorAccountID: principal.AccountID, ActorDeviceID: principal.DeviceID, Kind: command.Kind, At: eventAt.Format(time.RFC3339Nano), Effects: effects}
	return event, result, nil
}

func (state State) decideAccountRegistration(principal access.Principal, command Command, commandHash [32]byte, now time.Time) (Event, CommandResult, error) {
	var payload AccountRegisteredPayload
	if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" ||
		payload.AccountID != principal.AccountID || payload.IdentityKey == "" || len(payload.IdentityKey) > 4096 {
		return Event{}, CommandResult{}, errorCode("invalid_command")
	}
	if _, exists := state.accounts[payload.AccountID]; exists {
		return Event{}, CommandResult{}, errorCode("invalid_command")
	}
	effects := []Effect{{Kind: command.Kind, AccountID: payload.AccountID, IdentityKey: payload.IdentityKey, Status: "active"}}
	if !affectedIDsMatch(command, effects) {
		return Event{}, CommandResult{}, errorCode("invalid_command")
	}
	result := CommandResult{CommandID: command.ID, Outcome: "committed", Code: "ok", Revision: state.head.Revision + 1}
	event := Event{Version: "v1", Generation: state.head.Generation, Revision: result.Revision,
		PreviousHash: hex.EncodeToString(state.head.EventHash[:]), CommandID: command.ID, CommandHash: hex.EncodeToString(commandHash[:]),
		ActorAccountID: principal.AccountID, ActorDeviceID: principal.DeviceID, Kind: command.Kind, At: now.UTC().Format(time.RFC3339Nano), Effects: effects}
	return event, result, nil
}

func (state State) validFamilyPrincipal(principal access.Principal, now time.Time) bool {
	if !validUUID(principal.AccountID) || !validUUID(principal.DeviceID) || !validUUID(principal.SessionID) ||
		principal.RecoveryEpoch == 0 || principal.SecurityRevision == 0 {
		return false
	}
	account, accountOK := state.accounts[principal.AccountID]
	device, deviceOK := state.devices[principal.DeviceID]
	session, sessionOK := state.sessionLineages[principal.SessionID]
	if !accountOK || account.Status != "active" || !deviceOK || device.Status != "active" || device.AccountID != principal.AccountID ||
		!sessionOK || session.Status != "active" || session.AccountID != principal.AccountID || session.DeviceID != principal.DeviceID ||
		session.RecoveryEpoch != principal.RecoveryEpoch || session.SecurityRevision != principal.SecurityRevision {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, session.ExpiresAt)
	return err == nil && now.UTC().Before(expiresAt)
}

func (state State) owner(familyID, accountID string) bool {
	family, familyOK := state.families[familyID]
	membership, memberOK := state.memberships[familyID+"/"+accountID]
	return familyOK && family.Status == "active" && family.OwnerAccountID == accountID && memberOK && membership.Status == "active" && membership.Role == "owner"
}

func (state State) activeCaregivers(familyID string) int {
	count := 0
	for _, member := range state.memberships {
		if member.FamilyID == familyID && member.Status == "active" {
			count++
		}
	}
	return count
}

func (state State) activeInvitations(familyID string, now time.Time) int {
	count := 0
	for _, invitation := range state.invitations {
		expiresAt, err := time.Parse(time.RFC3339Nano, invitation.ExpiresAt)
		if invitation.FamilyID == familyID && invitation.Status == "active" && err == nil && now.UTC().Before(expiresAt) {
			count++
		}
	}
	return count
}

func (state State) familyEntitled(familyID string, now time.Time, required bool) bool {
	if !required {
		return true
	}
	entitlement, exists := state.entitlements[familyID+"/family_sync"]
	if !exists {
		return false
	}
	if entitlement.Status != "active" {
		return false
	}
	if entitlement.ExpiresAt == "" {
		return true
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, entitlement.ExpiresAt)
	return err == nil && now.UTC().Before(expiresAt)
}

func (state State) decideFamilyEffects(principal access.Principal, command Command, now time.Time, policy FamilyPolicy) ([]Effect, error) {
	switch command.Kind {
	case "family_deletion_scheduled", "family_recovered", "recovery_forfeited", "account_deleted":
		return state.decideLifecycleEffects(principal, command, now)
	case "family_created":
		var payload FamilyCreatedPayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" || payload.OwnerAccountID != principal.AccountID ||
			!validUUID(payload.FamilyID) || !canonicalHash.MatchString(payload.ProjectionMaterialDigest) {
			return nil, errorCode("invalid_command")
		}
		if _, occupied := state.familySlots[principal.AccountID]; occupied {
			return nil, errorCode("family_slot_occupied")
		}
		if _, exists := state.families[payload.FamilyID]; exists {
			return nil, errorCode("invalid_command")
		}
		return []Effect{{Kind: "family_created", AccountID: principal.AccountID, FamilyID: payload.FamilyID, Role: "owner", Status: "active", ProjectionMaterialDigest: payload.ProjectionMaterialDigest}}, nil
	case "family_bootstrapped":
		var payload FamilyBootstrappedPayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" || payload.OwnerAccountID != principal.AccountID ||
			!validUUID(payload.FamilyID) || payload.EntitlementKey != "family_sync" || payload.EntitlementSource != "hosted_beta" ||
			!canonicalHash.MatchString(payload.ProjectionMaterialDigest) {
			return nil, errorCode("invalid_command")
		}
		if _, occupied := state.familySlots[principal.AccountID]; occupied {
			return nil, errorCode("family_slot_occupied")
		}
		if _, exists := state.families[payload.FamilyID]; exists {
			return nil, errorCode("invalid_command")
		}
		return []Effect{
			{Kind: "family_created", AccountID: principal.AccountID, FamilyID: payload.FamilyID, Role: "owner", Status: "active", ProjectionMaterialDigest: payload.ProjectionMaterialDigest},
			{Kind: "entitlement_granted", FamilyID: payload.FamilyID, EntitlementKey: "family_sync", Source: "hosted_beta", Status: "active", ExpiresAt: now.UTC().Truncate(time.Second).Add(366 * 24 * time.Hour).Format(time.RFC3339), ProjectionMaterialDigest: payload.ProjectionMaterialDigest},
		}, nil
	case "family_profile_repaired":
		var payload FamilyProfileRepairedPayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" || payload.OwnerAccountID != principal.AccountID ||
			!validUUID(payload.FamilyID) || !canonicalHash.MatchString(payload.ProjectionMaterialDigest) {
			return nil, errorCode("invalid_command")
		}
		if !state.owner(payload.FamilyID, principal.AccountID) {
			return nil, errorCode("owner_required")
		}
		if !state.familyEntitled(payload.FamilyID, now, policy.RequireFamilySyncEntitlement) {
			return nil, errorCode("entitlement_required")
		}
		return []Effect{{Kind: "family_profile_repaired", AccountID: principal.AccountID, FamilyID: payload.FamilyID,
			ProjectionMaterialDigest: payload.ProjectionMaterialDigest}}, nil
	case "invitation_created":
		var payload InvitationPayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" || payload.CreatorAccountID != principal.AccountID ||
			!validUUID(payload.InvitationID) || !validUUID(payload.FamilyID) || !keyedDigest.MatchString(payload.TokenDigest) ||
			!validInvitationRole(payload.Role) || payload.Status != "active" {
			return nil, errorCode("invalid_command")
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
		if err != nil || !now.UTC().Before(expiresAt) {
			return nil, errorCode("invalid_command")
		}
		if !state.owner(payload.FamilyID, principal.AccountID) {
			return nil, errorCode("owner_required")
		}
		if !state.familyEntitled(payload.FamilyID, now, policy.RequireFamilySyncEntitlement) {
			return nil, errorCode("entitlement_required")
		}
		if state.activeInvitations(payload.FamilyID, now) >= policy.MaxInvitations {
			return nil, errorCode("invitation_limit")
		}
		if _, exists := state.invitations[payload.InvitationID]; exists {
			return nil, errorCode("invalid_command")
		}
		for _, invitation := range state.invitations {
			if invitation.TokenDigest == payload.TokenDigest {
				return nil, errorCode("invalid_command")
			}
		}
		return []Effect{{Kind: "invitation_created", AccountID: principal.AccountID, FamilyID: payload.FamilyID, InvitationID: payload.InvitationID, TokenDigest: payload.TokenDigest, Role: payload.Role, Status: "active", ExpiresAt: payload.ExpiresAt}}, nil
	case "invitation_redeemed":
		var payload InvitationPayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" || payload.RedeemerAccountID != principal.AccountID ||
			!validUUID(payload.InvitationID) || !validUUID(payload.FamilyID) || !keyedDigest.MatchString(payload.TokenDigest) ||
			!validInvitationRole(payload.Role) || payload.Status != "redeemed" {
			return nil, errorCode("invalid_command")
		}
		if _, occupied := state.familySlots[principal.AccountID]; occupied {
			return nil, errorCode("family_slot_occupied")
		}
		invitation, exists := state.invitations[payload.InvitationID]
		if !exists || invitation.FamilyID != payload.FamilyID || invitation.TokenDigest != payload.TokenDigest || invitation.Role != payload.Role || invitation.ExpiresAt != payload.ExpiresAt {
			return nil, errorCode("invitation_invalid")
		}
		switch invitation.Status {
		case "redeemed":
			return nil, errorCode("invitation_consumed")
		case "revoked":
			return nil, errorCode("invitation_revoked")
		case "active":
		default:
			return nil, errorCode("invitation_invalid")
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, invitation.ExpiresAt)
		if err != nil || !now.UTC().Before(expiresAt) {
			return nil, errorCode("invitation_expired")
		}
		family, ok := state.families[payload.FamilyID]
		if !ok || family.Status != "active" {
			return nil, errorCode("family_unavailable")
		}
		if state.activeCaregivers(payload.FamilyID) >= policy.MaxCaregivers {
			return nil, errorCode("caregiver_limit")
		}
		if !state.familyEntitled(payload.FamilyID, now, policy.RequireFamilySyncEntitlement) {
			return nil, errorCode("entitlement_required")
		}
		return []Effect{{Kind: "invitation_redeemed", AccountID: principal.AccountID, FamilyID: payload.FamilyID, InvitationID: payload.InvitationID, TokenDigest: payload.TokenDigest, Role: payload.Role, Status: "redeemed", ExpiresAt: payload.ExpiresAt}}, nil
	case "invitation_revoked":
		var payload InvitationPayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" || payload.Status != "revoked" {
			return nil, errorCode("invalid_command")
		}
		if !state.owner(payload.FamilyID, principal.AccountID) {
			return nil, errorCode("owner_required")
		}
		if !state.familyEntitled(payload.FamilyID, now, policy.RequireFamilySyncEntitlement) {
			return nil, errorCode("entitlement_required")
		}
		invitation, exists := state.invitations[payload.InvitationID]
		if !exists || invitation.FamilyID != payload.FamilyID || invitation.AccountID != payload.CreatorAccountID {
			return nil, errorCode("invitation_invalid")
		}
		switch invitation.Status {
		case "redeemed":
			return nil, errorCode("invitation_consumed")
		case "revoked":
			return nil, errorCode("invitation_revoked")
		case "active":
		default:
			return nil, errorCode("invitation_invalid")
		}
		if invitation.TokenDigest != payload.TokenDigest || invitation.Role != payload.Role || invitation.ExpiresAt != payload.ExpiresAt {
			return nil, errorCode("invalid_command")
		}
		return []Effect{{Kind: "invitation_revoked", AccountID: payload.CreatorAccountID, FamilyID: payload.FamilyID, InvitationID: payload.InvitationID, TokenDigest: payload.TokenDigest, Role: payload.Role, Status: "revoked", ExpiresAt: payload.ExpiresAt}}, nil
	case "member_left", "member_removed":
		var payload MembershipPayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" || !validUUID(payload.AccountID) || !validUUID(payload.FamilyID) {
			return nil, errorCode("invalid_command")
		}
		if command.Kind == "member_left" && payload.AccountID != principal.AccountID {
			return nil, errorCode("invalid_command")
		}
		if command.Kind == "member_removed" && !state.owner(payload.FamilyID, principal.AccountID) {
			return nil, errorCode("owner_required")
		}
		if command.Kind == "member_removed" && !state.familyEntitled(payload.FamilyID, now, policy.RequireFamilySyncEntitlement) {
			return nil, errorCode("entitlement_required")
		}
		member, ok := state.memberships[payload.FamilyID+"/"+payload.AccountID]
		if !ok || member.Status != "active" {
			return nil, errorCode("membership_revoked")
		}
		if member.Role == "owner" {
			return nil, errorCode("owner_required")
		}
		return []Effect{{Kind: command.Kind, AccountID: payload.AccountID, FamilyID: payload.FamilyID, Status: "removed"}}, nil
	case "member_role_changed":
		var payload MemberRolePayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" ||
			!validUUID(payload.FamilyID) || !validUUID(payload.AccountID) ||
			(payload.Role != "caregiver" && payload.Role != "viewer") {
			return nil, errorCode("invalid_command")
		}
		if !state.owner(payload.FamilyID, principal.AccountID) {
			return nil, errorCode("owner_required")
		}
		if payload.AccountID == principal.AccountID {
			// An owner cannot demote themselves: the family would be left
			// without one. Stepping down names a successor instead.
			return nil, errorCode("owner_required")
		}
		if !state.familyEntitled(payload.FamilyID, now, policy.RequireFamilySyncEntitlement) {
			return nil, errorCode("entitlement_required")
		}
		target, targetOK := state.memberships[payload.FamilyID+"/"+payload.AccountID]
		if !targetOK || target.Status != "active" || target.Role == "owner" {
			return nil, errorCode("membership_revoked")
		}
		return []Effect{{
			Kind: "member_role_changed", AccountID: payload.AccountID,
			FamilyID: payload.FamilyID, Role: payload.Role, Status: "active",
		}}, nil
	case "owner_transferred":
		var payload OwnerChangePayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" || payload.OldOwnerAccountID != principal.AccountID ||
			payload.NewOwnerAccountID == principal.AccountID || !validUUID(payload.FamilyID) || !validUUID(payload.NewOwnerAccountID) {
			return nil, errorCode("invalid_command")
		}
		if !state.owner(payload.FamilyID, principal.AccountID) {
			return nil, errorCode("owner_required")
		}
		if !state.familyEntitled(payload.FamilyID, now, policy.RequireFamilySyncEntitlement) {
			return nil, errorCode("entitlement_required")
		}
		target, ok := state.memberships[payload.FamilyID+"/"+payload.NewOwnerAccountID]
		targetAccount, accountOK := state.accounts[payload.NewOwnerAccountID]
		if !ok || target.Status != "active" || target.Role != "caregiver" || !accountOK || targetAccount.Status != "active" {
			return nil, errorCode("membership_revoked")
		}
		return []Effect{
			{Kind: "owner_transferred", AccountID: principal.AccountID, FamilyID: payload.FamilyID, Role: "caregiver", Status: "active"},
			{Kind: "owner_transferred", AccountID: payload.NewOwnerAccountID, FamilyID: payload.FamilyID, Role: "owner", Status: "active"},
		}, nil
	default:
		return nil, errorCode("invalid_command")
	}
}

func (state State) decideLifecycleEffects(principal access.Principal, command Command, now time.Time) ([]Effect, error) {
	switch command.Kind {
	case "family_deletion_scheduled":
		var payload FamilyLifecyclePayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" ||
			payload.ClaimOwnerAccountID != principal.AccountID || !validUUID(payload.FamilyID) || payload.PurgeAfter != "" ||
			len(payload.PreviousMemberIDs) == 0 || len(payload.PreviousMemberIDs) != len(payload.PreviousRoles) {
			return nil, errorCode("invalid_command")
		}
		if !state.owner(payload.FamilyID, principal.AccountID) {
			return nil, errorCode("owner_required")
		}
		type memberRole struct{ accountID, role string }
		members := make([]memberRole, 0)
		for _, member := range state.memberships {
			if member.FamilyID == payload.FamilyID && member.Status == "active" {
				members = append(members, memberRole{member.AccountID, member.Role})
			}
		}
		sort.Slice(members, func(i, j int) bool { return members[i].accountID < members[j].accountID })
		if len(members) != len(payload.PreviousMemberIDs) {
			return nil, errorCode("membership_revoked")
		}
		purgeAfter := now.UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
		effects := make([]Effect, len(members))
		for index, member := range members {
			if payload.PreviousMemberIDs[index] != member.accountID || payload.PreviousRoles[index] != member.role || !validUUID(member.accountID) || !validMemberRole(member.role) {
				return nil, errorCode("membership_revoked")
			}
			status := "former"
			if member.accountID == principal.AccountID {
				if member.role != "owner" {
					return nil, errorCode("owner_required")
				}
				status = "deletion_pending"
			}
			effects[index] = Effect{Kind: command.Kind, AccountID: member.accountID, FamilyID: payload.FamilyID, Role: member.role, Status: status, PurgeAfter: purgeAfter}
		}
		return effects, nil
	case "family_recovered", "recovery_forfeited":
		var payload FamilyLifecyclePayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" ||
			payload.ClaimOwnerAccountID != principal.AccountID || !validUUID(payload.FamilyID) || payload.PurgeAfter != "" ||
			len(payload.PreviousMemberIDs) != 0 || len(payload.PreviousRoles) != 0 || principal.Scope != "family_recovery" {
			return nil, errorCode("recovery_only")
		}
		claim, claimOK := state.recoveryClaims[payload.FamilyID+"/"+principal.AccountID]
		slot, slotOK := state.familySlots[principal.AccountID]
		family, familyOK := state.families[payload.FamilyID]
		if !claimOK || !slotOK || slot.FamilyID != payload.FamilyID || slot.State != "family_recovery" || !familyOK || family.Status != "deletion_pending" {
			if claimOK && claim.Status == "forfeited" {
				return nil, errorCode("recovery_forfeited")
			}
			return nil, errorCode("family_unavailable")
		}
		if claim.Status != "active" {
			if claim.Status == "forfeited" {
				return nil, errorCode("recovery_forfeited")
			}
			return nil, errorCode("family_unavailable")
		}
		if command.Kind == "family_recovered" {
			purgeAfter, err := time.Parse(time.RFC3339Nano, family.PurgeAfter)
			if err != nil || !now.UTC().Before(purgeAfter) {
				return nil, errorCode("recovery_expired")
			}
			return []Effect{{Kind: command.Kind, AccountID: principal.AccountID, FamilyID: payload.FamilyID, Role: "owner", Status: "active"}}, nil
		}
		return []Effect{{Kind: command.Kind, AccountID: principal.AccountID, FamilyID: payload.FamilyID, Status: "forfeited"}}, nil
	case "account_deleted":
		var payload AccountDeletedPayload
		if err := decodeStrict(command.Payload, &payload, maxEventBytes); err != nil || payload.Version != "v1" ||
			payload.AccountID != principal.AccountID || !keyedDigest.MatchString(payload.IdentityKey) {
			return nil, errorCode("invalid_command")
		}
		account, ok := state.accounts[principal.AccountID]
		if !ok || account.Status != "active" || account.IdentityKey != payload.IdentityKey {
			return nil, errorCode("session_revoked")
		}
		for _, family := range state.families {
			if family.OwnerAccountID == principal.AccountID && family.Status == "active" {
				return nil, errorCode("ownership_transfer_required")
			}
		}
		return []Effect{{Kind: command.Kind, AccountID: principal.AccountID, IdentityKey: payload.IdentityKey, Status: "deleted"}}, nil
	default:
		return nil, errorCode("invalid_command")
	}
}

func newVerifiedIdentityRegistration(accountID, identityKey, commandID string) Command {
	payload, _ := json.Marshal(AccountRegisteredPayload{Version: "v1", AccountID: accountID, IdentityKey: identityKey})
	return Command{ID: commandID, Kind: "account_registered", AffectedAccountIDs: []string{accountID}, Payload: payload, verifiedIdentity: true}
}

func (state *State) apply(event Event, hash [32]byte) error {
	if err := validateEventEffects(event); err != nil {
		return err
	}
	commandHash, err := decodeHash(event.CommandHash)
	if err != nil {
		return ErrLedgerCorrupt
	}
	receiptKey := event.ActorAccountID + "/" + event.CommandID
	if _, exists := state.commandReceipts[receiptKey]; exists {
		return ErrLedgerCorrupt
	}
	receiptKind := replayPreparedKind(*state, event)
	if err := dispatchEventApplication(eventDescriptors, state, event); err != nil {
		return err
	}
	result := CommandResult{CommandID: event.CommandID, Outcome: "committed", Code: "ok", Revision: event.Revision}
	familySet := map[string]struct{}{}
	for _, effect := range event.Effects {
		if effect.FamilyID != "" {
			familySet[effect.FamilyID] = struct{}{}
		}
	}
	familyIDs := make([]string, 0, len(familySet))
	for familyID := range familySet {
		familyIDs = append(familyIDs, familyID)
	}
	sort.Strings(familyIDs)
	state.commandReceipts[receiptKey] = receipt{CommandHash: commandHash, Result: result, eventKind: event.Kind, preparedKind: receiptKind, familyIDs: familyIDs}
	state.head = Head{Generation: event.Generation, Revision: event.Revision, EventHash: hash}
	return nil
}

func replayPreparedKind(state State, event Event) preparedKind {
	switch event.Kind {
	case "identity_session_issued":
		if account, exists := state.accounts[event.ActorAccountID]; exists && account.Status == "identity_unavailable" && effectOfKind(event, "account_registered") == nil {
			return preparedIdentityRestored
		}
		return preparedIdentityIssued
	case "session_rotated":
		return preparedSessionRotation
	case "session_revoked":
		return preparedRefreshReuse
	case "device_binding_revoked":
		return preparedDeviceRevocation
	case "identity_unavailable":
		return preparedIdentityUnavailable
	case "owner_recovered":
		return preparedOperatorRecovery
	case "operator_entitlement_granted":
		return preparedOperatorEntitlementGrant
	case "operator_entitlement_suspended":
		return preparedOperatorEntitlementSuspend
	default:
		return 0
	}
}

func (state *State) revokeAccountSessions(accountID string) {
	for id, session := range state.sessionLineages {
		if session.AccountID == accountID {
			session.Status = "revoked"
			state.sessionLineages[id] = session
		}
	}
}

func sortedEffects(effects []Effect) {
	sort.Slice(effects, func(i, j int) bool {
		a, _ := json.Marshal(effects[i])
		b, _ := json.Marshal(effects[j])
		return string(a) < string(b)
	})
}

func validTime(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.Location() == time.UTC && parsed.Format(time.RFC3339Nano) == value
}

func affectedIDsMatch(command Command, effects []Effect) bool {
	accountSet, familySet := map[string]bool{}, map[string]bool{}
	for _, effect := range effects {
		if effect.AccountID != "" {
			accountSet[effect.AccountID] = true
		}
		if effect.FamilyID != "" {
			familySet[effect.FamilyID] = true
		}
	}
	accounts := make([]string, 0, len(accountSet))
	for id := range accountSet {
		accounts = append(accounts, id)
	}
	families := make([]string, 0, len(familySet))
	for id := range familySet {
		families = append(families, id)
	}
	sort.Strings(accounts)
	sort.Strings(families)
	wantAccounts, accountOK := sortedCanonicalIDs(command.AffectedAccountIDs)
	wantFamilies, familyOK := sortedCanonicalIDs(command.AffectedFamilyIDs)
	return accountOK && familyOK && slices.Equal(accounts, wantAccounts) && slices.Equal(families, wantFamilies)
}
