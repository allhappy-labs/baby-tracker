package security

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"babytracker/server/internal/access"
)

func familyCommandTestState() (State, access.Principal, access.Principal) {
	state := NewState()
	state.head = Head{Generation: 1, Revision: 1}
	owner := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", DeviceID: "40000000-0000-4000-8000-000000000001", SessionID: "70000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, SecurityRevision: 1, Scope: "account"}
	caregiver := access.Principal{AccountID: "10000000-0000-4000-8000-000000000002", DeviceID: "40000000-0000-4000-8000-000000000002", SessionID: "70000000-0000-4000-8000-000000000002", RecoveryEpoch: 1, SecurityRevision: 1, Scope: "account"}
	for _, principal := range []access.Principal{owner, caregiver} {
		state.accounts[principal.AccountID] = accountState{IdentityKey: "hmac-sha256:identity.v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "active", registeredRevision: 1}
		state.devices[principal.DeviceID] = deviceState{AccountID: principal.AccountID, Status: "active"}
		state.sessionLineages[principal.SessionID] = sessionState{AccountID: principal.AccountID, DeviceID: principal.DeviceID, LineageRootSessionID: principal.SessionID, Status: "active", RecoveryEpoch: 1, SecurityRevision: 1, ExpiresAt: "2026-09-21T14:00:00Z"}
	}
	return state, owner, caregiver
}

func applyFamilyTestCommand(t *testing.T, state *State, principal access.Principal, command Command, now time.Time, policy FamilyPolicy) CommandResult {
	t.Helper()
	event, result, err := state.decide(principal, command, now, policy)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.apply(event, sha256.Sum256(encoded)); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFamilyCommandsCreateInviteRedeemAndReplay(t *testing.T) {
	state, owner, caregiver := familyCommandTestState()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	const familyID = "20000000-0000-4000-8000-000000000001"
	createPayload, _ := json.Marshal(FamilyCreatedPayload{Version: "v1", FamilyID: familyID, OwnerAccountID: owner.AccountID, ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	create := Command{ID: "30000000-0000-4000-8000-000000000001", Kind: "family_created", AffectedAccountIDs: []string{owner.AccountID}, AffectedFamilyIDs: []string{familyID}, Payload: createPayload}
	first := applyFamilyTestCommand(t, &state, owner, create, now, FamilyPolicy{MaxCaregivers: 2, MaxInvitations: 1})
	if slot, ok := state.FamilySlot(owner.AccountID); !ok || slot.FamilyID != familyID || slot.State != "active" {
		t.Fatalf("owner slot=%+v ok=%v", slot, ok)
	}
	invitationPayload, _ := json.Marshal(InvitationPayload{Version: "v1", InvitationID: "60000000-0000-4000-8000-000000000001", FamilyID: familyID, CreatorAccountID: owner.AccountID, TokenDigest: "hmac-sha256:invite.v1:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Role: "caregiver", Status: "active", ExpiresAt: "2026-09-28T12:00:00Z"})
	invite := Command{ID: "30000000-0000-4000-8000-000000000002", Kind: "invitation_created", AffectedAccountIDs: []string{owner.AccountID}, AffectedFamilyIDs: []string{familyID}, Payload: invitationPayload}
	applyFamilyTestCommand(t, &state, owner, invite, now, FamilyPolicy{MaxCaregivers: 2, MaxInvitations: 1})
	var redemption InvitationPayload
	_ = json.Unmarshal(invitationPayload, &redemption)
	redemption.RedeemerAccountID, redemption.Status = caregiver.AccountID, "redeemed"
	redeemPayload, _ := json.Marshal(redemption)
	redeem := Command{ID: "30000000-0000-4000-8000-000000000003", Kind: "invitation_redeemed", AffectedAccountIDs: []string{caregiver.AccountID}, AffectedFamilyIDs: []string{familyID}, Payload: redeemPayload}
	applyFamilyTestCommand(t, &state, caregiver, redeem, now, FamilyPolicy{MaxCaregivers: 2, MaxInvitations: 1})
	if slot, ok := state.FamilySlot(caregiver.AccountID); !ok || slot.FamilyID != familyID {
		t.Fatalf("caregiver slot=%+v ok=%v", slot, ok)
	}
	event, replay, err := state.decide(owner, create, now.Add(time.Hour), FamilyPolicy{MaxCaregivers: 2, MaxInvitations: 1})
	if err != nil || event.Revision != 0 || replay != first {
		t.Fatalf("replay=%+v event=%+v err=%v", replay, event, err)
	}
}

func TestFamilyCommandCapsAreCheckedInsideDecision(t *testing.T) {
	state, owner, _ := familyCommandTestState()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	const familyID = "20000000-0000-4000-8000-000000000001"
	state.families[familyID] = familyState{OwnerAccountID: owner.AccountID, Status: "active"}
	state.familySlots[owner.AccountID] = familySlotState{FamilyID: familyID, State: "active"}
	state.memberships[familyID+"/"+owner.AccountID] = membershipState{AccountID: owner.AccountID, FamilyID: familyID, Role: "owner", Status: "active"}
	for i := 1; i <= 2; i++ {
		payload, _ := json.Marshal(InvitationPayload{Version: "v1", InvitationID: []string{"60000000-0000-4000-8000-000000000001", "60000000-0000-4000-8000-000000000002"}[i-1], FamilyID: familyID, CreatorAccountID: owner.AccountID, TokenDigest: "hmac-sha256:invite.v1:" + []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}[i-1], Role: "caregiver", Status: "active", ExpiresAt: "2026-09-28T12:00:00Z"})
		command := Command{ID: []string{"30000000-0000-4000-8000-000000000011", "30000000-0000-4000-8000-000000000012"}[i-1], Kind: "invitation_created", AffectedAccountIDs: []string{owner.AccountID}, AffectedFamilyIDs: []string{familyID}, Payload: payload}
		event, _, err := state.decide(owner, command, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 1})
		if i == 1 {
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := EncodeEvent(event)
			if err := state.apply(event, sha256.Sum256(encoded)); err != nil {
				t.Fatal(err)
			}
		} else if Code(err) != "invitation_limit" {
			t.Fatalf("code=%q err=%v", Code(err), err)
		}
	}
}

func TestSelfHostedFamilyPolicyIgnoresLedgerEntitlementRows(t *testing.T) {
	state, owner, _ := familyCommandTestState()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	const familyID = "20000000-0000-4000-8000-000000000001"
	state.families[familyID] = familyState{OwnerAccountID: owner.AccountID, Status: "active"}
	state.familySlots[owner.AccountID] = familySlotState{FamilyID: familyID, State: "active"}
	state.memberships[familyID+"/"+owner.AccountID] = membershipState{AccountID: owner.AccountID, FamilyID: familyID, Role: "owner", Status: "active"}
	state.entitlements[familyID+"/family_sync"] = entitlementState{Status: "suspended", Source: "hosted_beta", ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}
	payload, _ := json.Marshal(InvitationPayload{Version: "v1", InvitationID: "60000000-0000-4000-8000-000000000001", FamilyID: familyID, CreatorAccountID: owner.AccountID,
		TokenDigest: "hmac-sha256:invite.v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Role: "caregiver", Status: "active", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano)})
	command := Command{ID: "30000000-0000-4000-8000-000000000021", Kind: "invitation_created", AffectedAccountIDs: []string{owner.AccountID}, AffectedFamilyIDs: []string{familyID}, Payload: payload}
	if _, _, err := state.decide(owner, command, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10}); err != nil {
		t.Fatalf("self-host denied: %v", err)
	}
	if _, _, err := state.decide(owner, command, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10, RequireFamilySyncEntitlement: true}); Code(err) != "entitlement_required" {
		t.Fatalf("hosted code=%q err=%v", Code(err), err)
	}
}

func TestFamilySemanticCommandHashSurvivesFreshSession(t *testing.T) {
	principal := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", DeviceID: "40000000-0000-4000-8000-000000000001",
		SessionID: "70000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, SecurityRevision: 1}
	payload, _ := json.Marshal(FamilyCreatedPayload{Version: "v1", FamilyID: "20000000-0000-4000-8000-000000000001", OwnerAccountID: principal.AccountID,
		ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	command := Command{ID: "30000000-0000-4000-8000-000000000001", Kind: "family_created", AffectedAccountIDs: []string{principal.AccountID},
		AffectedFamilyIDs: []string{"20000000-0000-4000-8000-000000000001"}, Payload: payload}
	first, err := CanonicalCommandHash(principal, command)
	if err != nil {
		t.Fatal(err)
	}
	principal.DeviceID = "40000000-0000-4000-8000-000000000009"
	principal.SessionID = "70000000-0000-4000-8000-000000000009"
	principal.RecoveryEpoch = 2
	principal.SecurityRevision = 99
	second, err := CanonicalCommandHash(principal, command)
	if err != nil || second != first {
		t.Fatalf("fresh-session hash changed: %x %x err=%v", first, second, err)
	}
	var changed FamilyCreatedPayload
	_ = json.Unmarshal(payload, &changed)
	changed.ProjectionMaterialDigest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	command.Payload, _ = json.Marshal(changed)
	third, err := CanonicalCommandHash(principal, command)
	if err != nil || third == first {
		t.Fatalf("changed payload hash=%x baseline=%x err=%v", third, first, err)
	}
}

func TestHostedBootstrapUsesAuthoritativeEventTimeAndReplaysLater(t *testing.T) {
	state, owner, _ := familyCommandTestState()
	const familyID = "20000000-0000-4000-8000-000000000001"
	payload, _ := json.Marshal(FamilyBootstrappedPayload{Version: "v1", FamilyID: familyID, OwnerAccountID: owner.AccountID,
		EntitlementKey: "family_sync", EntitlementSource: "hosted_beta", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	command := Command{ID: "30000000-0000-4000-8000-000000000001", Kind: "family_bootstrapped", AffectedAccountIDs: []string{owner.AccountID},
		AffectedFamilyIDs: []string{familyID}, Payload: payload}
	now := time.Date(2026, 9, 21, 12, 0, 0, 123456789, time.UTC)
	first := applyFamilyTestCommand(t, &state, owner, command, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10, RequireFamilySyncEntitlement: true})
	grant, ok := state.Entitlement(familyID, "family_sync")
	expectedExpiry := now.UTC().Truncate(time.Second).Add(366 * 24 * time.Hour).Format(time.RFC3339)
	if !ok || grant.ExpiresAt != expectedExpiry {
		t.Fatalf("grant=%+v ok=%v", grant, ok)
	}
	event, replay, err := state.decide(owner, command, now.Add(time.Hour), FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10, RequireFamilySyncEntitlement: true})
	if err != nil || event.Revision != 0 || replay != first {
		t.Fatalf("replay=%+v event=%+v err=%v", replay, event, err)
	}
}

func TestOwnerTransferRejectsIdentityUnavailableCaregiver(t *testing.T) {
	state, owner, caregiver := familyCommandTestState()
	const familyID = "20000000-0000-4000-8000-000000000001"
	state.families[familyID] = familyState{OwnerAccountID: owner.AccountID, Status: "active"}
	state.familySlots[owner.AccountID] = familySlotState{FamilyID: familyID, State: "active"}
	state.familySlots[caregiver.AccountID] = familySlotState{FamilyID: familyID, State: "active"}
	state.memberships[familyID+"/"+owner.AccountID] = membershipState{AccountID: owner.AccountID, FamilyID: familyID, Role: "owner", Status: "active"}
	state.memberships[familyID+"/"+caregiver.AccountID] = membershipState{AccountID: caregiver.AccountID, FamilyID: familyID, Role: "caregiver", Status: "active"}
	account := state.accounts[caregiver.AccountID]
	account.Status = "identity_unavailable"
	state.accounts[caregiver.AccountID] = account
	payload, _ := json.Marshal(OwnerChangePayload{Version: "v1", FamilyID: familyID, OldOwnerAccountID: owner.AccountID, NewOwnerAccountID: caregiver.AccountID})
	command := Command{ID: "30000000-0000-4000-8000-000000000001", Kind: "owner_transferred", AffectedAccountIDs: []string{owner.AccountID, caregiver.AccountID}, AffectedFamilyIDs: []string{familyID}, Payload: payload}
	if _, _, err := state.decide(owner, command, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10}); Code(err) != "membership_revoked" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
}

func TestHostedEntitlementExactExpiryDeniesButCommittedReplayBypassesLaterExpiry(t *testing.T) {
	state, owner, _ := familyCommandTestState()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	const familyID = "20000000-0000-4000-8000-000000000001"
	state.families[familyID] = familyState{OwnerAccountID: owner.AccountID, Status: "active"}
	state.familySlots[owner.AccountID] = familySlotState{FamilyID: familyID, State: "active"}
	state.memberships[familyID+"/"+owner.AccountID] = membershipState{AccountID: owner.AccountID, FamilyID: familyID, Role: "owner", Status: "active"}
	state.entitlements[familyID+"/family_sync"] = entitlementState{Status: "active", Source: "hosted_beta", ExpiresAt: now.Format(time.RFC3339Nano)}
	payload, _ := json.Marshal(InvitationPayload{Version: "v1", InvitationID: "60000000-0000-4000-8000-000000000001", FamilyID: familyID,
		CreatorAccountID: owner.AccountID, TokenDigest: "hmac-sha256:invite.v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Role: "caregiver", Status: "active", ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339Nano)})
	command := Command{ID: "30000000-0000-4000-8000-000000000001", Kind: "invitation_created", AffectedAccountIDs: []string{owner.AccountID}, AffectedFamilyIDs: []string{familyID}, Payload: payload}
	policy := FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10, RequireFamilySyncEntitlement: true}
	if _, _, err := state.decide(owner, command, now, policy); Code(err) != "entitlement_required" {
		t.Fatalf("boundary code=%q err=%v", Code(err), err)
	}
	entitlement := state.entitlements[familyID+"/family_sync"]
	entitlement.ExpiresAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	state.entitlements[familyID+"/family_sync"] = entitlement
	first := applyFamilyTestCommand(t, &state, owner, command, now, policy)
	event, replay, err := state.decide(owner, command, now.Add(2*time.Hour), policy)
	if err != nil || event.Revision != 0 || replay != first {
		t.Fatalf("expired replay=%+v event=%+v err=%v", replay, event, err)
	}
}

func TestFamilyProfileRepairEntitlementIsCheckedInsideAuthorityAndReplayWins(t *testing.T) {
	state, owner, _ := familyCommandTestState()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	const familyID = "20000000-0000-4000-8000-000000000001"
	state.families[familyID] = familyState{OwnerAccountID: owner.AccountID, Status: "active"}
	state.familySlots[owner.AccountID] = familySlotState{FamilyID: familyID, State: "active"}
	state.memberships[familyID+"/"+owner.AccountID] = membershipState{AccountID: owner.AccountID, FamilyID: familyID, Role: "owner", Status: "active"}
	payload, _ := json.Marshal(FamilyProfileRepairedPayload{Version: "v1", FamilyID: familyID, OwnerAccountID: owner.AccountID,
		ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	command := Command{ID: "30000000-0000-4000-8000-000000000031", Kind: "family_profile_repaired", AffectedAccountIDs: []string{owner.AccountID},
		AffectedFamilyIDs: []string{familyID}, Payload: payload}
	hosted := FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10, RequireFamilySyncEntitlement: true}
	if _, _, err := state.decide(owner, command, now, hosted); Code(err) != "entitlement_required" {
		t.Fatalf("hosted code=%q err=%v", Code(err), err)
	}
	if _, _, err := state.decide(owner, command, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10}); err != nil {
		t.Fatalf("self-host repair denied: %v", err)
	}
	state.entitlements[familyID+"/family_sync"] = entitlementState{Status: "active", Source: "hosted_beta", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano)}
	first := applyFamilyTestCommand(t, &state, owner, command, now, hosted)
	event, replay, err := state.decide(owner, command, now.Add(2*time.Hour), hosted)
	if err != nil || event.Revision != 0 || replay != first {
		t.Fatalf("expired replay=%+v event=%+v err=%v", replay, event, err)
	}
}
