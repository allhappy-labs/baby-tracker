package security

import (
	"encoding/json"
	"testing"
	"time"

	"babytracker/server/internal/access"
)

func lifecycleCommandState(t *testing.T) (State, access.Principal, access.Principal, string) {
	t.Helper()
	state, owner, caregiver := familyCommandTestState()
	familyID := "20000000-0000-4000-8000-000000000001"
	state.families[familyID] = familyState{OwnerAccountID: owner.AccountID, Status: "active"}
	for _, item := range []struct {
		principal access.Principal
		role      string
	}{{owner, "owner"}, {caregiver, "caregiver"}} {
		state.familySlots[item.principal.AccountID] = familySlotState{FamilyID: familyID, State: "active"}
		state.memberships[familyID+"/"+item.principal.AccountID] = membershipState{AccountID: item.principal.AccountID, FamilyID: familyID, Role: item.role, Status: "active"}
	}
	return state, owner, caregiver, familyID
}

func lifecycleCommand(t *testing.T, id string, kind EventKind, principal access.Principal, familyID string, payload any, accounts []string) Command {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	families := []string(nil)
	if familyID != "" {
		families = []string{familyID}
	}
	return Command{ID: id, Kind: kind, AffectedAccountIDs: accounts, AffectedFamilyIDs: families, Payload: raw}
}

func TestLifecycleScheduleReservesOnlyOwnerRecoverySlot(t *testing.T) {
	state, owner, caregiver, familyID := lifecycleCommandState(t)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	payload := FamilyLifecyclePayload{Version: "v1", FamilyID: familyID, ClaimOwnerAccountID: owner.AccountID,
		PreviousMemberIDs: []string{owner.AccountID, caregiver.AccountID}, PreviousRoles: []string{"owner", "caregiver"}}
	command := lifecycleCommand(t, "30000000-0000-4000-8000-000000000051", "family_deletion_scheduled", owner, familyID, payload,
		[]string{owner.AccountID, caregiver.AccountID})
	applyFamilyTestCommand(t, &state, owner, command, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10})

	family, _ := state.Family(familyID)
	if family.Status != "deletion_pending" || family.PurgeAfter != now.Add(30*24*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("family=%+v", family)
	}
	if slot, ok := state.FamilySlot(owner.AccountID); !ok || slot.State != "family_recovery" || slot.FamilyID != familyID {
		t.Fatalf("owner slot=%+v present=%v", slot, ok)
	}
	if _, ok := state.FamilySlot(caregiver.AccountID); ok {
		t.Fatal("former caregiver retained family slot")
	}
	if claim, ok := state.RecoveryClaim(familyID, owner.AccountID); !ok || claim.Status != "active" {
		t.Fatalf("claim=%+v present=%v", claim, ok)
	}
	for _, principal := range []access.Principal{owner, caregiver} {
		if session, _ := state.Session(principal.SessionID); session.Status != "revoked" {
			t.Fatalf("session %s=%+v", principal.SessionID, session)
		}
	}
}

func TestLifecycleRecoveryDeadlineAndForfeitAreExclusive(t *testing.T) {
	state, owner, caregiver, familyID := lifecycleCommandState(t)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	schedule := lifecycleCommand(t, "30000000-0000-4000-8000-000000000052", "family_deletion_scheduled", owner, familyID,
		FamilyLifecyclePayload{Version: "v1", FamilyID: familyID, ClaimOwnerAccountID: owner.AccountID,
			PreviousMemberIDs: []string{owner.AccountID, caregiver.AccountID}, PreviousRoles: []string{"owner", "caregiver"}},
		[]string{owner.AccountID, caregiver.AccountID})
	applyFamilyTestCommand(t, &state, owner, schedule, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10})

	// A fresh proof creates a new live session while preserving the recovery slot.
	recovery := owner
	recovery.DeviceID = "40000000-0000-4000-8000-000000000009"
	recovery.SessionID = "70000000-0000-4000-8000-000000000009"
	recovery.SecurityRevision = state.head.Revision
	recovery.Scope = "family_recovery"
	state.devices[recovery.DeviceID] = deviceState{AccountID: owner.AccountID, Status: "active"}
	state.sessionLineages[recovery.SessionID] = sessionState{AccountID: owner.AccountID, DeviceID: recovery.DeviceID,
		LineageRootSessionID: recovery.SessionID, Status: "active", RecoveryEpoch: 1, SecurityRevision: recovery.SecurityRevision,
		ExpiresAt: now.Add(31 * 24 * time.Hour).Format(time.RFC3339Nano)}

	recoverCommand := lifecycleCommand(t, "30000000-0000-4000-8000-000000000053", "family_recovered", recovery, familyID,
		FamilyLifecyclePayload{Version: "v1", FamilyID: familyID, ClaimOwnerAccountID: owner.AccountID}, []string{owner.AccountID})
	if _, _, err := state.decide(recovery, recoverCommand, now.Add(30*24*time.Hour), FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10}); Code(err) != "recovery_expired" {
		t.Fatalf("deadline code=%q err=%v", Code(err), err)
	}

	forfeit := lifecycleCommand(t, "30000000-0000-4000-8000-000000000054", "recovery_forfeited", recovery, familyID,
		FamilyLifecyclePayload{Version: "v1", FamilyID: familyID, ClaimOwnerAccountID: owner.AccountID}, []string{owner.AccountID})
	applyFamilyTestCommand(t, &state, recovery, forfeit, now.Add(30*24*time.Hour), FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10})
	if _, _, err := state.decide(recovery, recoverCommand, now.Add(29*24*time.Hour), FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10}); Code(err) != "recovery_forfeited" {
		t.Fatalf("post-forfeit code=%q err=%v", Code(err), err)
	}
}

func TestAccountDeletionRejectsActiveOwnerAndForfeitsRecovery(t *testing.T) {
	state, owner, caregiver, familyID := lifecycleCommandState(t)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	deleteCommand := lifecycleCommand(t, "30000000-0000-4000-8000-000000000055", "account_deleted", owner, "",
		AccountDeletedPayload{Version: "v1", AccountID: owner.AccountID, IdentityKey: state.accounts[owner.AccountID].IdentityKey}, []string{owner.AccountID})
	if _, _, err := state.decide(owner, deleteCommand, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10}); Code(err) != "ownership_transfer_required" {
		t.Fatalf("active owner code=%q err=%v", Code(err), err)
	}

	schedule := lifecycleCommand(t, "30000000-0000-4000-8000-000000000056", "family_deletion_scheduled", owner, familyID,
		FamilyLifecyclePayload{Version: "v1", FamilyID: familyID, ClaimOwnerAccountID: owner.AccountID,
			PreviousMemberIDs: []string{owner.AccountID, caregiver.AccountID}, PreviousRoles: []string{"owner", "caregiver"}},
		[]string{owner.AccountID, caregiver.AccountID})
	applyFamilyTestCommand(t, &state, owner, schedule, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10})
	deletedPrincipal := owner
	deletedPrincipal.DeviceID = "40000000-0000-4000-8000-000000000008"
	deletedPrincipal.SessionID = "70000000-0000-4000-8000-000000000008"
	deletedPrincipal.SecurityRevision = state.head.Revision
	deletedPrincipal.Scope = "family_recovery"
	state.devices[deletedPrincipal.DeviceID] = deviceState{AccountID: owner.AccountID, Status: "active"}
	state.sessionLineages[deletedPrincipal.SessionID] = sessionState{AccountID: owner.AccountID, DeviceID: deletedPrincipal.DeviceID,
		LineageRootSessionID: deletedPrincipal.SessionID, Status: "active", RecoveryEpoch: 1, SecurityRevision: deletedPrincipal.SecurityRevision,
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano)}
	deleteCommand = lifecycleCommand(t, deleteCommand.ID, "account_deleted", deletedPrincipal, "",
		AccountDeletedPayload{Version: "v1", AccountID: owner.AccountID, IdentityKey: state.accounts[owner.AccountID].IdentityKey}, []string{owner.AccountID})
	applyFamilyTestCommand(t, &state, deletedPrincipal, deleteCommand, now, FamilyPolicy{MaxCaregivers: 10, MaxInvitations: 10})
	if account, _ := state.Account(owner.AccountID); account.Status != "deleted" {
		t.Fatalf("account=%+v", account)
	}
	if claim, _ := state.RecoveryClaim(familyID, owner.AccountID); claim.Status != "forfeited" {
		t.Fatalf("claim=%+v", claim)
	}
}
