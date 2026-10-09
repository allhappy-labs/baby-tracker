package security

import (
	"sort"
	"time"
)

type effectShape struct {
	required []string
	allowed  []string
	status   string
	check    func(Event, Effect) bool
}

func validateEffects(event Event, shapes map[EventKind]effectShape) error {
	for _, effect := range event.Effects {
		shape, ok := shapes[effect.Kind]
		if !ok || (shape.status != "" && effect.Status != shape.status) || !exactEffectKeys(effect, shape.required, shape.allowed) {
			return ErrLedgerCorrupt
		}
		if shape.check != nil && !shape.check(event, effect) {
			return ErrLedgerCorrupt
		}
	}
	return nil
}

func oneEffect(event Event, shape effectShape) error {
	if len(event.Effects) != 1 {
		return ErrLedgerCorrupt
	}
	return validateEffects(event, map[EventKind]effectShape{event.Kind: shape})
}

func shape(required []string, status string) effectShape {
	return effectShape{required: required, allowed: required, status: status}
}

func shapeWithAllowed(required, allowed []string, status string) effectShape {
	return effectShape{required: required, allowed: allowed, status: status}
}

func effectOfKind(event Event, kind EventKind) *Effect {
	for i := range event.Effects {
		if event.Effects[i].Kind == kind {
			return &event.Effects[i]
		}
	}
	return nil
}

var (
	accountEffectKeys = []string{"kind", "account_id", "identity_key", "status"}
	deviceEffectKeys  = []string{"kind", "account_id", "device_id", "server_instance_id", "canonical_origin", "device_credential_digest", "status"}
	invitationKeys    = []string{"kind", "account_id", "family_id", "invitation_id", "token_digest", "role", "status", "expires_at"}
)

func validateAccountRegistered(event Event) error {
	return oneEffect(event, shape(accountEffectKeys, "active"))
}

func validateIdentityUnavailable(event Event) error {
	return oneEffect(event, shape(accountEffectKeys, "identity_unavailable"))
}

func validateIdentityRestored(event Event) error {
	return oneEffect(event, shape(accountEffectKeys, "active"))
}

func validateDeviceRegistered(event Event) error {
	return oneEffect(event, shape(deviceEffectKeys, "active"))
}

func validateDeviceBindingRevoked(event Event) error {
	return oneEffect(event, shape(deviceEffectKeys, "revoked"))
}

func validateSessionGranted(event Event) error {
	keys := []string{"kind", "account_id", "device_id", "session_id", "lineage_root_session_id", "recovery_epoch", "expires_at", "status"}
	entry := shape(keys, "active")
	entry.check = func(_ Event, effect Effect) bool {
		return effect.RecoveryEpoch != 0 && effect.LineageRootSessionID == effect.SessionID
	}
	return oneEffect(event, entry)
}

func validateSessionRotated(event Event) error {
	keys := []string{"kind", "account_id", "device_id", "session_id", "parent_session_id", "lineage_root_session_id", "recovery_epoch", "expires_at", "status", "projection_material_digest"}
	entry := shapeWithAllowed(keys, append(append([]string(nil), keys...), "refresh_attempt_id"), "active")
	entry.check = func(_ Event, effect Effect) bool {
		return effect.RecoveryEpoch != 0 && canonicalHash.MatchString(effect.ProjectionMaterialDigest)
	}
	return oneEffect(event, entry)
}

func validateSessionRevoked(event Event) error {
	keys := []string{"kind", "account_id", "device_id", "session_id", "lineage_root_session_id", "recovery_epoch", "expires_at", "status"}
	entry := shape(keys, "revoked")
	entry.check = func(_ Event, effect Effect) bool { return effect.RecoveryEpoch != 0 }
	return oneEffect(event, entry)
}

func validateIdentitySessionIssued(event Event) error {
	sessionKeys := []string{"kind", "account_id", "device_id", "session_id", "lineage_root_session_id", "recovery_epoch", "expires_at", "status", "identity_key", "server_instance_id", "canonical_origin", "device_credential_digest", "projection_material_digest"}
	sessionShape := shape(sessionKeys, "active")
	sessionShape.check = func(_ Event, effect Effect) bool {
		return effect.RecoveryEpoch != 0 && effect.LineageRootSessionID == effect.SessionID
	}
	if err := validateEffects(event, map[EventKind]effectShape{
		"account_registered": shape(accountEffectKeys, "active"),
		"device_registered":  shape(deviceEffectKeys, "active"),
		"session_granted":    sessionShape,
	}); err != nil {
		return err
	}
	counts := map[EventKind]int{}
	accountID, deviceID := "", ""
	for _, effect := range event.Effects {
		counts[effect.Kind]++
		if accountID == "" {
			accountID = effect.AccountID
		}
		if effect.AccountID != accountID {
			return ErrLedgerCorrupt
		}
		if effect.DeviceID != "" {
			if deviceID == "" {
				deviceID = effect.DeviceID
			}
			if effect.DeviceID != deviceID {
				return ErrLedgerCorrupt
			}
		}
	}
	session := effectOfKind(event, "session_granted")
	if counts["account_registered"] > 1 || counts["device_registered"] > 1 || counts["session_granted"] != 1 ||
		accountID != event.ActorAccountID || deviceID == "" || session == nil || !keyedDigest.MatchString(session.IdentityKey) ||
		!keyedDigest.MatchString(session.DeviceCredentialDigest) || !canonicalHash.MatchString(session.ProjectionMaterialDigest) {
		return ErrLedgerCorrupt
	}
	if account := effectOfKind(event, "account_registered"); account != nil && (account.AccountID != session.AccountID || account.IdentityKey != session.IdentityKey) {
		return ErrLedgerCorrupt
	}
	if device := effectOfKind(event, "device_registered"); device != nil &&
		(device.AccountID != session.AccountID || device.DeviceID != session.DeviceID || device.ServerInstanceID != session.ServerInstanceID ||
			device.CanonicalOrigin != session.CanonicalOrigin || device.DeviceCredentialDigest != session.DeviceCredentialDigest) {
		return ErrLedgerCorrupt
	}
	return nil
}

func familyCreatedShape() effectShape {
	keys := []string{"kind", "account_id", "family_id", "role", "status", "projection_material_digest"}
	entry := shape(keys, "active")
	entry.check = func(_ Event, effect Effect) bool {
		return effect.Role == "owner" && canonicalHash.MatchString(effect.ProjectionMaterialDigest)
	}
	return entry
}

func validateFamilyCreated(event Event) error { return oneEffect(event, familyCreatedShape()) }

func validateFamilyBootstrapped(event Event) error {
	entitlementRequired := []string{"kind", "family_id", "entitlement_key", "source", "status", "projection_material_digest"}
	entitlement := shapeWithAllowed(entitlementRequired, append(append([]string{}, entitlementRequired...), "expires_at"), "active")
	if err := validateEffects(event, map[EventKind]effectShape{"family_created": familyCreatedShape(), "entitlement_granted": entitlement}); err != nil {
		return err
	}
	if len(event.Effects) != 2 {
		return ErrLedgerCorrupt
	}
	family := effectOfKind(event, "family_created")
	grant := effectOfKind(event, "entitlement_granted")
	if family == nil || grant == nil || family.FamilyID != grant.FamilyID || family.AccountID != event.ActorAccountID ||
		family.ProjectionMaterialDigest != grant.ProjectionMaterialDigest || !canonicalHash.MatchString(family.ProjectionMaterialDigest) {
		return ErrLedgerCorrupt
	}
	eventAt, _ := time.Parse(time.RFC3339Nano, event.At)
	expiresAt, err := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if grant.EntitlementKey != "family_sync" || grant.Source != "hosted_beta" || err != nil || !expiresAt.Equal(eventAt.Truncate(time.Second).Add(366*24*time.Hour)) {
		return ErrLedgerCorrupt
	}
	return nil
}

func validateFamilyProfileRepaired(event Event) error {
	keys := []string{"kind", "account_id", "family_id", "projection_material_digest"}
	entry := shape(keys, "")
	entry.check = func(event Event, effect Effect) bool {
		return effect.AccountID == event.ActorAccountID && canonicalHash.MatchString(effect.ProjectionMaterialDigest)
	}
	return oneEffect(event, entry)
}

func validateInvitation(event Event, status string) error {
	entry := shape(invitationKeys, status)
	entry.check = func(_ Event, effect Effect) bool { return validInvitationRole(effect.Role) }
	return oneEffect(event, entry)
}

func validateInvitationCreated(event Event) error  { return validateInvitation(event, "active") }
func validateInvitationRevoked(event Event) error  { return validateInvitation(event, "revoked") }
func validateInvitationRedeemed(event Event) error { return validateInvitation(event, "redeemed") }

func validateMemberRemoved(event Event) error {
	return oneEffect(event, shape([]string{"kind", "account_id", "family_id", "status"}, "removed"))
}

func validateMemberRoleChanged(event Event) error {
	return oneEffect(event, shape([]string{"kind", "account_id", "family_id", "role", "status"}, "active"))
}

func validateOwnerChange(event Event, audited bool) error {
	keys := []string{"kind", "account_id", "family_id", "role", "status"}
	if audited {
		keys = append(keys, "operator_policy_id", "operator_evidence_digest", "operator_reason_code", "expected_revision")
	}
	entry := shape(keys, "active")
	entry.check = func(_ Event, effect Effect) bool { return validMemberRole(effect.Role) }
	if len(event.Effects) != 2 {
		return ErrLedgerCorrupt
	}
	if err := validateEffects(event, map[EventKind]effectShape{event.Kind: entry}); err != nil {
		return err
	}
	roles := []string{event.Effects[0].Role, event.Effects[1].Role}
	sort.Strings(roles)
	if roles[0] != "caregiver" || roles[1] != "owner" || event.Effects[0].FamilyID != event.Effects[1].FamilyID || event.Effects[0].AccountID == event.Effects[1].AccountID {
		return ErrLedgerCorrupt
	}
	if audited {
		first, second := event.Effects[0], event.Effects[1]
		if first.OperatorPolicyID != second.OperatorPolicyID || first.OperatorEvidenceDigest != second.OperatorEvidenceDigest ||
			first.OperatorReasonCode != second.OperatorReasonCode || first.ExpectedRevision == nil || second.ExpectedRevision == nil ||
			*first.ExpectedRevision == 0 || *first.ExpectedRevision != *second.ExpectedRevision || *first.ExpectedRevision != event.Revision-1 ||
			!operatorPolicyID.MatchString(first.OperatorPolicyID) || !canonicalHash.MatchString(first.OperatorEvidenceDigest) ||
			first.OperatorReasonCode != "owner_identity_unavailable" {
			return ErrLedgerCorrupt
		}
	}
	return nil
}

func validateOwnerTransferred(event Event) error { return validateOwnerChange(event, false) }
func validateOwnerRecovered(event Event) error   { return validateOwnerChange(event, true) }

func entitlementShape(status string, audited bool) effectShape {
	keys := []string{"kind", "family_id", "entitlement_key", "source", "status"}
	if audited {
		keys = append(keys, "operator_policy_id", "operator_evidence_digest", "operator_reason_code", "expected_revision")
	}
	return shapeWithAllowed(keys, append(append([]string{}, keys...), "expires_at"), status)
}

func validateEntitlement(event Event, status string, audited bool) error {
	if err := oneEffect(event, entitlementShape(status, audited)); err != nil {
		return err
	}
	if audited {
		effect := event.Effects[0]
		if effect.Source != "hosted_beta" || effect.ExpectedRevision == nil || *effect.ExpectedRevision == 0 || *effect.ExpectedRevision != event.Revision-1 || !operatorPolicyID.MatchString(effect.OperatorPolicyID) ||
			!canonicalHash.MatchString(effect.OperatorEvidenceDigest) || !operatorReason.MatchString(effect.OperatorReasonCode) {
			return ErrLedgerCorrupt
		}
	}
	return nil
}

func validateEntitlementGranted(event Event) error {
	return validateEntitlement(event, "active", false)
}
func validateEntitlementSuspended(event Event) error {
	return validateEntitlement(event, "suspended", false)
}
func validateOperatorEntitlementGranted(event Event) error {
	return validateEntitlement(event, "active", true)
}
func validateOperatorEntitlementSuspended(event Event) error {
	return validateEntitlement(event, "suspended", true)
}

func validateFamilyDeletionScheduled(event Event) error {
	keys := []string{"kind", "account_id", "family_id", "role", "status", "purge_after"}
	entry := shapeWithAllowed(keys, keys, "")
	entry.check = func(_ Event, effect Effect) bool {
		return (effect.Status == "deletion_pending" || effect.Status == "former") && validMemberRole(effect.Role) &&
			(effect.Status != "deletion_pending" || effect.Role == "owner")
	}
	if err := validateEffects(event, map[EventKind]effectShape{"family_deletion_scheduled": entry}); err != nil {
		return err
	}
	if len(event.Effects) == 0 {
		return ErrLedgerCorrupt
	}
	eventAt, err := time.Parse(time.RFC3339Nano, event.At)
	if err != nil {
		return ErrLedgerCorrupt
	}
	anchors := 0
	family, purge := event.Effects[0].FamilyID, event.Effects[0].PurgeAfter
	purgeAt, err := time.Parse(time.RFC3339Nano, purge)
	if err != nil || !purgeAt.Equal(eventAt.Add(30*24*time.Hour)) {
		return ErrLedgerCorrupt
	}
	accounts := map[string]bool{}
	for _, effect := range event.Effects {
		if effect.Status == "deletion_pending" {
			anchors++
			if effect.AccountID != event.ActorAccountID {
				return ErrLedgerCorrupt
			}
		}
		if effect.FamilyID != family || effect.PurgeAfter != purge || accounts[effect.AccountID] {
			return ErrLedgerCorrupt
		}
		accounts[effect.AccountID] = true
	}
	if anchors != 1 {
		return ErrLedgerCorrupt
	}
	return nil
}

func validateFamilyRecovered(event Event) error {
	entry := shape([]string{"kind", "account_id", "family_id", "role", "status"}, "active")
	entry.check = func(event Event, effect Effect) bool {
		return effect.Role == "owner" && effect.AccountID == event.ActorAccountID
	}
	return oneEffect(event, entry)
}

func validateRecoveryForfeited(event Event) error {
	entry := shape([]string{"kind", "account_id", "family_id", "status"}, "forfeited")
	entry.check = func(event Event, effect Effect) bool { return effect.AccountID == event.ActorAccountID }
	return oneEffect(event, entry)
}

func validateAccountDeleted(event Event) error {
	if err := oneEffect(event, shape(accountEffectKeys, "deleted")); err != nil {
		return err
	}
	if event.Effects[0].AccountID != event.ActorAccountID {
		return ErrLedgerCorrupt
	}
	return nil
}

func validateFamilyPurgeStarted(event Event) error {
	keys := []string{"kind", "family_id", "server_instance_id", "status", "purge_after", "purge_start_command_id", "purge_start_revision"}
	entry := shape(keys, "purge_started")
	entry.check = func(event Event, effect Effect) bool {
		return effect.PurgeStartCommandID == event.CommandID && effect.PurgeStartRevision != nil && *effect.PurgeStartRevision == event.Revision && retentionActorMatches(event.ActorAccountID, effect.ServerInstanceID)
	}
	return oneEffect(event, entry)
}

func validateFamilyPurgeCompleted(event Event) error {
	keys := []string{"kind", "family_id", "server_instance_id", "status", "physical_purge_receipt", "purged_at", "expected_start_revision"}
	entry := shape(keys, "purge_completed")
	entry.check = func(event Event, effect Effect) bool {
		return effect.PurgedAt == event.At && retentionActorMatches(event.ActorAccountID, effect.ServerInstanceID)
	}
	return oneEffect(event, entry)
}

func dispatchEventValidation(registry map[EventKind]eventDescriptor, event Event) error {
	descriptor, ok := registry[event.Kind]
	if !ok || descriptor.validate == nil {
		return ErrUnsupportedEvent
	}
	return descriptor.validate(event)
}

func dispatchEventApplication(registry map[EventKind]eventDescriptor, state *State, event Event) error {
	descriptor, ok := registry[event.Kind]
	if !ok || descriptor.apply == nil {
		return ErrUnsupportedEvent
	}
	return descriptor.apply(state, event)
}

func applyAccountRegistered(state *State, event Event) error {
	e := event.Effects[0]
	if _, exists := state.accounts[e.AccountID]; exists {
		return ErrLedgerCorrupt
	}
	for _, account := range state.accounts {
		if account.IdentityKey == e.IdentityKey && account.Status != "deleted" {
			return ErrLedgerCorrupt
		}
	}
	state.accounts[e.AccountID] = accountState{IdentityKey: e.IdentityKey, Status: "active", registeredRevision: event.Revision}
	return nil
}

func applyIdentityAuthority(state *State, event Event, expectedStatus string, revoke bool) error {
	e := event.Effects[0]
	account, exists := state.accounts[e.AccountID]
	if !exists || account.IdentityKey != e.IdentityKey || account.Status != expectedStatus {
		return ErrLedgerCorrupt
	}
	account.Status = e.Status
	state.accounts[e.AccountID] = account
	if revoke {
		state.revokeAccountSessions(e.AccountID)
	}
	return nil
}

func applyIdentityUnavailable(state *State, event Event) error {
	return applyIdentityAuthority(state, event, "active", true)
}
func applyIdentityRestored(state *State, event Event) error {
	return applyIdentityAuthority(state, event, "identity_unavailable", false)
}

func applyDeviceRegistered(state *State, event Event) error {
	e := event.Effects[0]
	account, ok := state.accounts[e.AccountID]
	if !ok || account.Status != "active" {
		return ErrLedgerCorrupt
	}
	if _, exists := state.devices[e.DeviceID]; exists {
		return ErrLedgerCorrupt
	}
	state.devices[e.DeviceID] = deviceState{AccountID: e.AccountID, ServerInstanceID: e.ServerInstanceID, CanonicalOrigin: e.CanonicalOrigin, CredentialDigest: e.DeviceCredentialDigest, Status: "active"}
	return nil
}

func applyDeviceBindingRevoked(state *State, event Event) error {
	e := event.Effects[0]
	device, ok := state.devices[e.DeviceID]
	if !ok || device.Status != "active" || device.AccountID != e.AccountID || device.ServerInstanceID != e.ServerInstanceID || device.CanonicalOrigin != e.CanonicalOrigin || device.CredentialDigest != e.DeviceCredentialDigest {
		return ErrLedgerCorrupt
	}
	device.Status = "revoked"
	state.devices[e.DeviceID] = device
	for id, session := range state.sessionLineages {
		if session.DeviceID == e.DeviceID {
			session.Status = "revoked"
			state.sessionLineages[id] = session
		}
	}
	return nil
}

func activeSessionAuthority(state *State, effect Effect) bool {
	account, accountOK := state.accounts[effect.AccountID]
	device, deviceOK := state.devices[effect.DeviceID]
	return accountOK && account.Status == "active" && deviceOK && device.Status == "active" && device.AccountID == effect.AccountID
}

func storeSession(state *State, event Event, effect Effect) {
	state.sessionLineages[effect.SessionID] = sessionState{AccountID: effect.AccountID, DeviceID: effect.DeviceID, ParentSessionID: effect.ParentSessionID, LineageRootSessionID: effect.LineageRootSessionID, RecoveryEpoch: effect.RecoveryEpoch, SecurityRevision: event.Revision, ProjectionMaterialDigest: effect.ProjectionMaterialDigest, RefreshAttemptID: effect.RefreshAttemptID, ExpiresAt: effect.ExpiresAt, Status: effect.Status}
}

func applySessionGranted(state *State, event Event) error {
	e := event.Effects[0]
	if !activeSessionAuthority(state, e) {
		return ErrLedgerCorrupt
	}
	if _, exists := state.sessionLineages[e.SessionID]; exists || e.ParentSessionID != "" {
		return ErrLedgerCorrupt
	}
	storeSession(state, event, e)
	return nil
}

func applySessionRotated(state *State, event Event) error {
	e := event.Effects[0]
	if !activeSessionAuthority(state, e) {
		return ErrLedgerCorrupt
	}
	parent, exists := state.sessionLineages[e.ParentSessionID]
	if !exists || parent.AccountID != e.AccountID || parent.DeviceID != e.DeviceID || parent.Status != "active" ||
		parent.RecoveryEpoch != e.RecoveryEpoch || parent.ExpiresAt != e.ExpiresAt || parent.LineageRootSessionID != e.LineageRootSessionID || e.SessionID == e.ParentSessionID {
		return ErrLedgerCorrupt
	}
	if _, exists := state.sessionLineages[e.SessionID]; exists {
		return ErrLedgerCorrupt
	}
	parent.Status = "rotated"
	parent.RotationAttemptID = e.RefreshAttemptID
	parent.ChildSessionID = e.SessionID
	state.sessionLineages[e.ParentSessionID] = parent
	storeSession(state, event, e)
	return nil
}

func applySessionRevoked(state *State, event Event) error {
	e := event.Effects[0]
	if !activeSessionAuthority(state, e) {
		return ErrLedgerCorrupt
	}
	target, exists := state.sessionLineages[e.SessionID]
	if !exists || target.AccountID != e.AccountID || target.DeviceID != e.DeviceID || (target.Status != "active" && target.Status != "rotated") ||
		target.RecoveryEpoch != e.RecoveryEpoch || target.ExpiresAt != e.ExpiresAt || target.LineageRootSessionID != e.LineageRootSessionID {
		return ErrLedgerCorrupt
	}
	activeLineage := false
	for _, session := range state.sessionLineages {
		if session.AccountID == e.AccountID && session.DeviceID == e.DeviceID && session.LineageRootSessionID == e.LineageRootSessionID && session.Status == "active" {
			activeLineage = true
		}
	}
	if !activeLineage {
		return ErrLedgerCorrupt
	}
	for id, session := range state.sessionLineages {
		if session.AccountID == e.AccountID && session.DeviceID == e.DeviceID && session.LineageRootSessionID == e.LineageRootSessionID {
			session.Status = "revoked"
			state.sessionLineages[id] = session
		}
	}
	return nil
}

func applyIdentitySessionIssued(state *State, event Event) error {
	accountEffect := effectOfKind(event, "account_registered")
	deviceEffect := effectOfKind(event, "device_registered")
	sessionEffect := effectOfKind(event, "session_granted")
	if accountEffect != nil {
		if err := applyAccountRegistered(state, Event{Revision: event.Revision, Effects: []Effect{*accountEffect}}); err != nil {
			return err
		}
	}
	account := state.accounts[sessionEffect.AccountID]
	if account.IdentityKey != sessionEffect.IdentityKey || (account.Status != "active" && account.Status != "identity_unavailable") {
		return ErrLedgerCorrupt
	}
	if account.Status == "identity_unavailable" {
		account.Status = "active"
		state.accounts[sessionEffect.AccountID] = account
	}
	if deviceEffect != nil {
		if err := applyDeviceRegistered(state, Event{Effects: []Effect{*deviceEffect}}); err != nil {
			return err
		}
	}
	device := state.devices[sessionEffect.DeviceID]
	if device.Status != "active" || device.AccountID != sessionEffect.AccountID || device.ServerInstanceID != sessionEffect.ServerInstanceID ||
		device.CanonicalOrigin != sessionEffect.CanonicalOrigin || device.CredentialDigest != sessionEffect.DeviceCredentialDigest {
		return ErrLedgerCorrupt
	}
	if _, exists := state.sessionLineages[sessionEffect.SessionID]; exists {
		return ErrLedgerCorrupt
	}
	storeSession(state, event, *sessionEffect)
	return nil
}

func applyFamilyCreated(state *State, event Event) error {
	e := event.Effects[0]
	account, ok := state.accounts[e.AccountID]
	if _, occupied := state.familySlots[e.AccountID]; !ok || account.Status != "active" || occupied {
		return ErrLedgerCorrupt
	}
	if _, exists := state.families[e.FamilyID]; exists {
		return ErrLedgerCorrupt
	}
	state.families[e.FamilyID] = familyState{OwnerAccountID: e.AccountID, Status: "active"}
	state.familySlots[e.AccountID] = familySlotState{FamilyID: e.FamilyID, State: "active"}
	state.memberships[e.FamilyID+"/"+e.AccountID] = membershipState{AccountID: e.AccountID, FamilyID: e.FamilyID, Role: "owner", Status: "active", Generation: event.Revision}
	return nil
}

func applyFamilyBootstrapped(state *State, event Event) error {
	family := effectOfKind(event, "family_created")
	grant := effectOfKind(event, "entitlement_granted")
	if err := applyFamilyCreated(state, Event{Revision: event.Revision, Effects: []Effect{*family}}); err != nil {
		return err
	}
	state.entitlements[grant.FamilyID+"/"+grant.EntitlementKey] = entitlementState{Status: "active", Source: grant.Source, ExpiresAt: grant.ExpiresAt}
	return nil
}

func applyFamilyProfileRepaired(state *State, event Event) error {
	e := event.Effects[0]
	family, ok := state.families[e.FamilyID]
	member, memberOK := state.memberships[e.FamilyID+"/"+e.AccountID]
	if !ok || family.Status != "active" || family.OwnerAccountID != e.AccountID || !memberOK || member.Status != "active" || member.Role != "owner" {
		return ErrLedgerCorrupt
	}
	return nil
}

func applyInvitationCreated(state *State, event Event) error {
	e := event.Effects[0]
	family, ok := state.families[e.FamilyID]
	member := state.memberships[e.FamilyID+"/"+e.AccountID]
	if !ok || family.Status != "active" || member.Role != "owner" || member.Status != "active" {
		return ErrLedgerCorrupt
	}
	if _, exists := state.invitations[e.InvitationID]; exists {
		return ErrLedgerCorrupt
	}
	state.invitations[e.InvitationID] = invitationState{AccountID: e.AccountID, FamilyID: e.FamilyID, TokenDigest: e.TokenDigest, Role: e.Role, Status: "active", ExpiresAt: e.ExpiresAt}
	return nil
}

func applyInvitationRevoked(state *State, event Event) error {
	e := event.Effects[0]
	invitation, ok := state.invitations[e.InvitationID]
	if !ok || invitation.AccountID != e.AccountID || invitation.FamilyID != e.FamilyID || invitation.TokenDigest != e.TokenDigest || invitation.Role != e.Role || invitation.ExpiresAt != e.ExpiresAt || invitation.Status != "active" {
		return ErrLedgerCorrupt
	}
	invitation.Status = "revoked"
	state.invitations[e.InvitationID] = invitation
	return nil
}

func applyInvitationRedeemed(state *State, event Event) error {
	e := event.Effects[0]
	invitation, ok := state.invitations[e.InvitationID]
	account, accountOK := state.accounts[e.AccountID]
	_, occupied := state.familySlots[e.AccountID]
	if !ok || invitation.FamilyID != e.FamilyID || invitation.TokenDigest != e.TokenDigest || invitation.Role != e.Role || invitation.ExpiresAt != e.ExpiresAt || invitation.Status != "active" || !accountOK || account.Status != "active" || occupied {
		return ErrLedgerCorrupt
	}
	invitation.Status = "redeemed"
	state.invitations[e.InvitationID] = invitation
	state.familySlots[e.AccountID] = familySlotState{FamilyID: e.FamilyID, State: "active"}
	state.memberships[e.FamilyID+"/"+e.AccountID] = membershipState{AccountID: e.AccountID, FamilyID: e.FamilyID, Role: e.Role, Status: "active", Generation: event.Revision}
	return nil
}

func applyMemberRemoved(state *State, event Event) error {
	e := event.Effects[0]
	member, ok := state.memberships[e.FamilyID+"/"+e.AccountID]
	if !ok || member.Status != "active" || member.Role == "owner" {
		return ErrLedgerCorrupt
	}
	member.Status = "removed"
	state.memberships[e.FamilyID+"/"+e.AccountID] = member
	delete(state.familySlots, e.AccountID)
	state.revokeAccountSessions(e.AccountID)
	return nil
}

func applyOwnerChange(state *State, event Event, recovery bool) error {
	familyID := event.Effects[0].FamilyID
	family, ok := state.families[familyID]
	if !ok || family.Status != "active" {
		return ErrLedgerCorrupt
	}
	previousOwnerSeen, nextOwnerSeen := false, false
	for _, e := range event.Effects {
		member, exists := state.memberships[familyID+"/"+e.AccountID]
		if !exists || member.Status != "active" {
			return ErrLedgerCorrupt
		}
		if member.Role == "owner" && e.Role == "caregiver" && e.AccountID == family.OwnerAccountID {
			if recovery && state.accounts[e.AccountID].Status != "identity_unavailable" {
				return ErrLedgerCorrupt
			}
			previousOwnerSeen = true
		}
		if member.Role == "caregiver" && e.Role == "owner" {
			if recovery && state.accounts[e.AccountID].Status != "active" {
				return ErrLedgerCorrupt
			}
			nextOwnerSeen = true
		}
		state.memberships[familyID+"/"+e.AccountID] = membershipState{AccountID: e.AccountID, FamilyID: familyID, Role: e.Role, Status: "active", Generation: member.Generation}
		state.familySlots[e.AccountID] = familySlotState{FamilyID: familyID, State: "active"}
		if e.Role == "owner" {
			family.OwnerAccountID = e.AccountID
		}
	}
	if !previousOwnerSeen || !nextOwnerSeen {
		return ErrLedgerCorrupt
	}
	state.families[familyID] = family
	return nil
}

// applyMemberRoleChanged moves a caregiver between writing and view-only.
//
// It refuses to touch an owner: a family must always have exactly one, and
// demoting them here would leave it with none. Stepping down is
// `owner_transferred`, which names a successor in the same command.
func applyMemberRoleChanged(state *State, event Event) error {
	e := event.Effects[0]
	member, ok := state.memberships[e.FamilyID+"/"+e.AccountID]
	if !ok || member.Status != "active" || member.Role == "owner" {
		return ErrLedgerCorrupt
	}
	if e.Role != "caregiver" && e.Role != "viewer" {
		return ErrLedgerCorrupt
	}
	member.Role = e.Role
	state.memberships[e.FamilyID+"/"+e.AccountID] = member
	return nil
}

func applyOwnerTransferred(state *State, event Event) error {
	return applyOwnerChange(state, event, false)
}
func applyOwnerRecovered(state *State, event Event) error {
	return applyOwnerChange(state, event, true)
}

func applyEntitlement(state *State, event Event, suspension, extension bool) error {
	e := event.Effects[0]
	family, ok := state.families[e.FamilyID]
	key := e.FamilyID + "/" + e.EntitlementKey
	if !ok || family.Status != "active" {
		return ErrLedgerCorrupt
	}
	current, exists := state.entitlements[key]
	if suspension {
		if !exists || current.Status != "active" || current.Source != e.Source || current.ExpiresAt != e.ExpiresAt {
			return ErrLedgerCorrupt
		}
	} else if extension && exists {
		currentExpiry, currentErr := time.Parse(time.RFC3339Nano, current.ExpiresAt)
		nextExpiry, nextErr := time.Parse(time.RFC3339Nano, e.ExpiresAt)
		if current.Status != "active" || current.Source != e.Source || currentErr != nil || nextErr != nil || !nextExpiry.After(currentExpiry) {
			return ErrLedgerCorrupt
		}
	} else if exists {
		return ErrLedgerCorrupt
	}
	state.entitlements[key] = entitlementState{Status: e.Status, Source: e.Source, ExpiresAt: e.ExpiresAt}
	return nil
}

func applyEntitlementGranted(state *State, event Event) error {
	return applyEntitlement(state, event, false, false)
}
func applyEntitlementSuspended(state *State, event Event) error {
	return applyEntitlement(state, event, true, false)
}
func applyOperatorEntitlementGranted(state *State, event Event) error {
	return applyEntitlement(state, event, false, true)
}
func applyOperatorEntitlementSuspended(state *State, event Event) error {
	return applyEntitlement(state, event, true, false)
}

func applyFamilyDeletionScheduled(state *State, event Event) error {
	var anchor Effect
	for _, effect := range event.Effects {
		if effect.Status == "deletion_pending" {
			anchor = effect
			break
		}
	}
	family, ok := state.families[anchor.FamilyID]
	if !ok || family.Status != "active" || family.OwnerAccountID != anchor.AccountID {
		return ErrLedgerCorrupt
	}
	activeMembers := 0
	for _, member := range state.memberships {
		if member.FamilyID == anchor.FamilyID && member.Status == "active" {
			activeMembers++
		}
	}
	if activeMembers != len(event.Effects) {
		return ErrLedgerCorrupt
	}
	family.Status, family.PurgeAfter = "deletion_pending", anchor.PurgeAfter
	state.families[anchor.FamilyID] = family
	for _, e := range event.Effects {
		member := state.memberships[e.FamilyID+"/"+e.AccountID]
		if member.AccountID == "" || (e.Role != "" && member.Role != e.Role) || (e.Status == "deletion_pending" && member.Role != "owner") {
			return ErrLedgerCorrupt
		}
		if member.Role != "owner" {
			delete(state.familySlots, e.AccountID)
		} else {
			state.familySlots[e.AccountID] = familySlotState{FamilyID: e.FamilyID, State: "family_recovery"}
		}
		member.Status = "former"
		state.memberships[e.FamilyID+"/"+e.AccountID] = member
		state.revokeAccountSessions(e.AccountID)
	}
	state.recoveryClaims[anchor.FamilyID+"/"+anchor.AccountID] = recoveryClaimState{AccountID: anchor.AccountID, FamilyID: anchor.FamilyID, Status: "active"}
	return nil
}

func applyFamilyRecovered(state *State, event Event) error {
	e := event.Effects[0]
	family, ok := state.families[e.FamilyID]
	claim, claimOK := state.recoveryClaims[e.FamilyID+"/"+e.AccountID]
	account, accountOK := state.accounts[e.AccountID]
	if !ok || family.Status != "deletion_pending" || !claimOK || claim.Status != "active" || !accountOK || account.Status != "active" {
		return ErrLedgerCorrupt
	}
	family.Status, family.OwnerAccountID, family.PurgeAfter = "active", e.AccountID, ""
	state.families[e.FamilyID] = family
	state.familySlots[e.AccountID] = familySlotState{FamilyID: e.FamilyID, State: "active"}
	state.memberships[e.FamilyID+"/"+e.AccountID] = membershipState{AccountID: e.AccountID, FamilyID: e.FamilyID, Role: "owner", Status: "active", Generation: state.memberships[e.FamilyID+"/"+e.AccountID].Generation}
	claim.Status = "recovered"
	state.recoveryClaims[e.FamilyID+"/"+e.AccountID] = claim
	return nil
}

func applyRecoveryForfeited(state *State, event Event) error {
	e := event.Effects[0]
	claim, ok := state.recoveryClaims[e.FamilyID+"/"+e.AccountID]
	if !ok || claim.Status != "active" {
		return ErrLedgerCorrupt
	}
	claim.Status = "forfeited"
	state.recoveryClaims[e.FamilyID+"/"+e.AccountID] = claim
	if slot, ok := state.familySlots[e.AccountID]; ok && slot.FamilyID == e.FamilyID {
		delete(state.familySlots, e.AccountID)
	}
	return nil
}

func applyAccountDeleted(state *State, event Event) error {
	anchor := event.Effects[0]
	account, ok := state.accounts[anchor.AccountID]
	if !ok || account.IdentityKey != anchor.IdentityKey || account.Status == "deleted" {
		return ErrLedgerCorrupt
	}
	for _, family := range state.families {
		if family.OwnerAccountID == anchor.AccountID && family.Status == "active" {
			return ErrLedgerCorrupt
		}
	}
	account.Status = "deleted"
	state.accounts[anchor.AccountID] = account
	delete(state.familySlots, anchor.AccountID)
	state.revokeAccountSessions(anchor.AccountID)
	for id, device := range state.devices {
		if device.AccountID == anchor.AccountID {
			device.Status = "revoked"
			state.devices[id] = device
		}
	}
	for key, member := range state.memberships {
		if member.AccountID == anchor.AccountID {
			member.Status = "removed"
			state.memberships[key] = member
		}
	}
	for key, claim := range state.recoveryClaims {
		if claim.AccountID == anchor.AccountID {
			claim.Status = "forfeited"
			state.recoveryClaims[key] = claim
		}
	}
	return nil
}

func applyFamilyPurgeStarted(state *State, event Event) error {
	e := event.Effects[0]
	family, ok := state.families[e.FamilyID]
	at, _ := time.Parse(time.RFC3339Nano, event.At)
	due, _ := time.Parse(time.RFC3339Nano, family.PurgeAfter)
	if !ok || family.Status != "deletion_pending" || family.PurgeAfter != e.PurgeAfter || at.Before(due) {
		return ErrLedgerCorrupt
	}
	family.Status = "purge_started"
	family.PurgeStartCommandID = e.PurgeStartCommandID
	family.PurgeStartRevision = *e.PurgeStartRevision
	family.PurgeServerInstanceID = e.ServerInstanceID
	state.families[e.FamilyID] = family
	if slot, ok := state.familySlots[family.OwnerAccountID]; ok && slot.FamilyID == e.FamilyID {
		delete(state.familySlots, family.OwnerAccountID)
	}
	return nil
}

func applyFamilyPurgeCompleted(state *State, event Event) error {
	e := event.Effects[0]
	family, ok := state.families[e.FamilyID]
	if !ok || family.Status != "purge_started" || e.ExpectedStartRevision == nil || *e.ExpectedStartRevision != family.PurgeStartRevision || e.ServerInstanceID != family.PurgeServerInstanceID {
		return ErrLedgerCorrupt
	}
	family.Status = "purge_completed"
	family.PhysicalPurgeReceipt = e.PhysicalPurgeReceipt
	family.PurgedAt = e.PurgedAt
	state.families[e.FamilyID] = family
	if slot, ok := state.familySlots[family.OwnerAccountID]; ok && slot.FamilyID == e.FamilyID {
		delete(state.familySlots, family.OwnerAccountID)
	}
	for key, claim := range state.recoveryClaims {
		if claim.FamilyID == e.FamilyID && claim.Status == "active" {
			claim.Status = "forfeited"
			state.recoveryClaims[key] = claim
		}
	}
	return nil
}
