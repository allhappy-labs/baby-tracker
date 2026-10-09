package security_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
)

func uint64Pointer(value uint64) *uint64 { return &value }

func TestRetentionWorkerActorIDVectorUsesUUIDv8(t *testing.T) {
	got, err := security.RetentionWorkerActorID("50000000-0000-4000-8000-000000000001")
	if err != nil || got != "7ca49471-6754-82b8-8c8e-2364ee9b1e45" {
		t.Fatalf("actor=%q err=%v", got, err)
	}
}

func TestIdentitySessionCompositeBindsOriginAndIdentityLossRevokesLineage(t *testing.T) {
	const (
		account  = "10000000-0000-4000-8000-000000000001"
		device   = "40000000-0000-4000-8000-000000000001"
		session  = "70000000-0000-4000-8000-000000000001"
		instance = "50000000-0000-4000-8000-000000000001"
	)
	prior := security.Head{Generation: 1}
	candidates := []security.Candidate{
		appendReplayEvent(t, &prior, 1, "identity_session_issued", []security.Effect{
			{Kind: "account_registered", AccountID: account, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "active"},
			{Kind: "device_registered", AccountID: account, DeviceID: device, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: "active"},
			{Kind: "session_granted", AccountID: account, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DeviceID: device, SessionID: session, LineageRootSessionID: session, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ProjectionMaterialDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"},
		}),
		appendReplayEvent(t, &prior, 2, "identity_unavailable", []security.Effect{{Kind: "identity_unavailable", AccountID: account, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "identity_unavailable"}}),
		appendReplayEvent(t, &prior, 3, "identity_restored", []security.Effect{{Kind: "identity_restored", AccountID: account, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "active"}}),
	}
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := state.Device(device); !ok || got.CanonicalOrigin != "https://family.example" {
		t.Fatalf("device=%+v present=%v", got, ok)
	}
	if got, ok := state.Session(session); !ok || got.Status != "revoked" {
		t.Fatalf("session=%+v present=%v", got, ok)
	}

	branch := prior
	changedOrigin := appendReplayEvent(t, &branch, 4, "device_binding_revoked", []security.Effect{{
		Kind: "device_binding_revoked", AccountID: account, DeviceID: device, ServerInstanceID: instance,
		CanonicalOrigin: "https://attacker.example", DeviceCredentialDigest: "device-key-digest", Status: "revoked",
	}})
	if _, err := security.Replay(append(candidates, changedOrigin)); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("changed origin code=%s err=%v", security.Code(err), err)
	}
}

func TestBoundedAccountDeletionRevokesUnboundedSessionHistory(t *testing.T) {
	const (
		account  = "10000000-0000-4000-8000-000000000001"
		device   = "40000000-0000-4000-8000-000000000001"
		instance = "50000000-0000-4000-8000-000000000001"
		family   = "20000000-0000-4000-8000-000000000001"
	)
	prior := security.Head{Generation: 1}
	firstSession := "70000000-0000-4000-8000-000000000001"
	candidates := []security.Candidate{
		appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: account, IdentityKey: "apple-proof-digest", Status: "active"}}),
		appendReplayEvent(t, &prior, 2, "device_registered", []security.Effect{{Kind: "device_registered", AccountID: account, DeviceID: device, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: "device-digest", Status: "active"}}),
		appendReplayEvent(t, &prior, 3, "session_granted", []security.Effect{{Kind: "session_granted", AccountID: account, DeviceID: device, SessionID: firstSession, LineageRootSessionID: firstSession, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"}}),
	}
	parent := firstSession
	for index := 2; index <= 400; index++ {
		next := fmt.Sprintf("70000000-0000-4000-8000-%012d", index)
		candidates = append(candidates, appendReplayEvent(t, &prior, index+2, "session_rotated", []security.Effect{{Kind: "session_rotated", AccountID: account, DeviceID: device, SessionID: next, ParentSessionID: parent, LineageRootSessionID: firstSession, ProjectionMaterialDigest: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"}}))
		parent = next
	}
	candidates = append(candidates, appendReplayEvent(t, &prior, 403, "family_created", []security.Effect{{Kind: "family_created", AccountID: account, FamilyID: family, Role: "owner", Status: "active", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}))
	candidates = append(candidates, appendReplayEvent(t, &prior, 404, "family_deletion_scheduled", []security.Effect{{Kind: "family_deletion_scheduled", AccountID: account, FamilyID: family, Role: "owner", Status: "deletion_pending", PurgeAfter: "2026-10-20T12:06:44Z"}}))
	deletion := appendReplayEvent(t, &prior, 405, "account_deleted", []security.Effect{{Kind: "account_deleted", AccountID: account, IdentityKey: "apple-proof-digest", Status: "deleted"}})
	if len(deletion.EventJSON) >= 64<<10 {
		t.Fatalf("bounded deletion bytes=%d", len(deletion.EventJSON))
	}
	candidates = append(candidates, deletion)
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{firstSession, "70000000-0000-4000-8000-000000000200", parent} {
		if session, ok := state.Session(id); !ok || session.Status != "revoked" {
			t.Fatalf("session %s=%+v present=%v", id, session, ok)
		}
	}
	if deviceState, _ := state.Device(device); deviceState.Status != "revoked" {
		t.Fatalf("device=%+v", deviceState)
	}
	if membership, _ := state.Membership(family, account); membership.Status != "removed" {
		t.Fatalf("membership=%+v", membership)
	}
	if claim, _ := state.RecoveryClaim(family, account); claim.Status != "forfeited" {
		t.Fatalf("claim=%+v", claim)
	}
}

func TestFamilyBootstrapAndActiveOwnerRecoveryAreClosedComposites(t *testing.T) {
	const (
		oldOwner = "10000000-0000-4000-8000-000000000001"
		newOwner = "10000000-0000-4000-8000-000000000002"
		family   = "20000000-0000-4000-8000-000000000001"
		invite   = "60000000-0000-4000-8000-000000000001"
	)
	prior := security.Head{Generation: 1}
	candidates := []security.Candidate{
		appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: oldOwner, IdentityKey: "old-digest", Status: "active"}}),
		appendReplayEvent(t, &prior, 2, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: newOwner, IdentityKey: "new-digest", Status: "active"}}),
		appendReplayEvent(t, &prior, 3, "family_bootstrapped", []security.Effect{
			{Kind: "entitlement_granted", FamilyID: family, EntitlementKey: "family_sync", Source: "hosted_beta", Status: "active", ExpiresAt: "2027-09-21T12:00:03Z", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			{Kind: "family_created", AccountID: oldOwner, FamilyID: family, Role: "owner", Status: "active", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		}),
		appendReplayEvent(t, &prior, 4, "invitation_created", []security.Effect{{Kind: "invitation_created", AccountID: oldOwner, FamilyID: family, InvitationID: invite, TokenDigest: "invite-digest", Role: "caregiver", Status: "active", ExpiresAt: "2026-09-21T12:00:00Z"}}),
		appendReplayEvent(t, &prior, 5, "invitation_redeemed", []security.Effect{{Kind: "invitation_redeemed", AccountID: newOwner, FamilyID: family, InvitationID: invite, TokenDigest: "invite-digest", Role: "caregiver", Status: "redeemed", ExpiresAt: "2026-09-21T12:00:00Z"}}),
		appendReplayEvent(t, &prior, 6, "identity_unavailable", []security.Effect{{Kind: "identity_unavailable", AccountID: oldOwner, IdentityKey: "old-digest", Status: "identity_unavailable"}}),
		appendReplayEvent(t, &prior, 7, "owner_recovered", []security.Effect{
			{Kind: "owner_recovered", AccountID: oldOwner, FamilyID: family, Role: "caregiver", Status: "active", OperatorPolicyID: "test-policy-v1", OperatorEvidenceDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", OperatorReasonCode: "owner_identity_unavailable", ExpectedRevision: uint64Pointer(6)},
			{Kind: "owner_recovered", AccountID: newOwner, FamilyID: family, Role: "owner", Status: "active", OperatorPolicyID: "test-policy-v1", OperatorEvidenceDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", OperatorReasonCode: "owner_identity_unavailable", ExpectedRevision: uint64Pointer(6)},
		}),
	}
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := state.Entitlement(family, "family_sync"); !ok || got.Source != "hosted_beta" || got.Status != "active" {
		t.Fatalf("entitlement=%+v present=%v", got, ok)
	}
	if got, _ := state.Family(family); got.OwnerAccountID != newOwner || got.Status != "active" {
		t.Fatalf("family=%+v", got)
	}
	if got, _ := state.Membership(family, oldOwner); got.Role != "caregiver" || got.Status != "active" {
		t.Fatalf("old owner membership=%+v", got)
	}
	if _, ok := state.RecoveryClaim(family, oldOwner); ok {
		t.Fatal("owner recovery created a deletion recovery claim")
	}
}

func TestOperatorEntitlementRequiresCompleteAuditBinding(t *testing.T) {
	base := security.Event{
		Version: "v1", Generation: 1, Revision: 2,
		PreviousHash: "0000000000000000000000000000000000000000000000000000000000000000",
		CommandID:    "30000000-0000-4000-8000-000000000001", CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", Kind: "operator_entitlement_granted", At: "2026-09-20T12:00:00Z",
		Effects: []security.Effect{{Kind: "operator_entitlement_granted", FamilyID: "20000000-0000-4000-8000-000000000001", EntitlementKey: "family_sync", Source: "hosted_beta", Status: "active", OperatorPolicyID: "Entitlement.Policy_test-v1", OperatorEvidenceDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", OperatorReasonCode: "support_approved", ExpectedRevision: uint64Pointer(1)}},
	}
	if _, err := security.EncodeEvent(base); err != nil {
		t.Fatalf("valid operator event: %v", err)
	}
	base.Effects[0].OperatorEvidenceDigest = ""
	if _, err := security.EncodeEvent(base); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("missing evidence code=%s", security.Code(err))
	}
}

func TestOperatorEntitlementRejectsNoncanonicalSourceZeroRevisionAndPolicyID(t *testing.T) {
	base := security.Event{
		Version: "v1", Generation: 1, Revision: 2,
		PreviousHash: "0000000000000000000000000000000000000000000000000000000000000000",
		CommandID:    "30000000-0000-4000-8000-000000000001", CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", Kind: "operator_entitlement_granted", At: "2026-09-20T12:00:00Z",
		Effects: []security.Effect{{Kind: "operator_entitlement_granted", FamilyID: "20000000-0000-4000-8000-000000000001", EntitlementKey: "family_sync", Source: "hosted_beta", Status: "active", OperatorPolicyID: "Entitlement.Policy_test-v1", OperatorEvidenceDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", OperatorReasonCode: "support_approved", ExpectedRevision: uint64Pointer(1)}},
	}
	for name, mutate := range map[string]func(*security.Effect){
		"source":   func(effect *security.Effect) { effect.Source = "operator" },
		"revision": func(effect *security.Effect) { effect.ExpectedRevision = uint64Pointer(0) },
		"policy":   func(effect *security.Effect) { effect.OperatorPolicyID = "1invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			event := base
			event.Effects = append([]security.Effect(nil), base.Effects...)
			mutate(&event.Effects[0])
			if _, err := security.EncodeEvent(event); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%q err=%v", security.Code(err), err)
			}
		})
	}
}

func TestOperatorEntitlementGrantExtendsOnlyMatchingExistingGrant(t *testing.T) {
	const (
		account = "10000000-0000-4000-8000-000000000001"
		family  = "20000000-0000-4000-8000-000000000001"
	)
	base := func(t *testing.T, expiresAt string) ([]security.Candidate, security.Head) {
		t.Helper()
		prior := security.Head{Generation: 1}
		candidates := []security.Candidate{
			appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: account, IdentityKey: "owner-digest", Status: "active"}}),
			appendReplayEvent(t, &prior, 2, "family_created", []security.Effect{{Kind: "family_created", AccountID: account, FamilyID: family, Role: "owner", Status: "active", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}),
			appendReplayEvent(t, &prior, 3, "entitlement_granted", []security.Effect{{Kind: "entitlement_granted", FamilyID: family, EntitlementKey: "family_sync", Source: "hosted_beta", Status: "active", ExpiresAt: expiresAt}}),
		}
		return candidates, prior
	}
	extension := func(expiresAt, source string, expectedRevision uint64) security.Effect {
		return security.Effect{Kind: "operator_entitlement_granted", FamilyID: family, EntitlementKey: "family_sync", Source: source, Status: "active", ExpiresAt: expiresAt,
			OperatorPolicyID: "entitlement-policy-test-v1", OperatorEvidenceDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			OperatorReasonCode: "beta_extension", ExpectedRevision: uint64Pointer(expectedRevision)}
	}

	for _, initialExpiry := range []string{"2027-09-20T12:00:00Z", "2026-09-20T11:00:00Z"} {
		t.Run(initialExpiry, func(t *testing.T) {
			candidates, prior := base(t, initialExpiry)
			candidates = append(candidates, appendReplayEvent(t, &prior, 4, "operator_entitlement_granted", []security.Effect{extension("2028-09-20T12:00:00Z", "hosted_beta", 3)}))
			state, err := security.Replay(candidates)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := state.Entitlement(family, "family_sync"); !ok || got.Status != "active" || got.Source != "hosted_beta" || got.ExpiresAt != "2028-09-20T12:00:00Z" {
				t.Fatalf("entitlement=%+v present=%v", got, ok)
			}
		})
	}

	for name, effect := range map[string]security.Effect{
		"same expiry":    extension("2027-09-20T12:00:00Z", "hosted_beta", 3),
		"earlier expiry": extension("2027-09-20T11:59:59Z", "hosted_beta", 3),
	} {
		t.Run(name, func(t *testing.T) {
			candidates, prior := base(t, "2027-09-20T12:00:00Z")
			candidates = append(candidates, appendReplayEvent(t, &prior, 4, "operator_entitlement_granted", []security.Effect{effect}))
			if _, err := security.Replay(candidates); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%s err=%v", security.Code(err), err)
			}
		})
	}

	candidates, prior := base(t, "2027-09-20T12:00:00Z")
	candidates = append(candidates, appendReplayEvent(t, &prior, 4, "entitlement_granted", []security.Effect{{Kind: "entitlement_granted", FamilyID: family, EntitlementKey: "family_sync", Source: "hosted_beta", Status: "active", ExpiresAt: "2028-09-20T12:00:00Z"}}))
	if _, err := security.Replay(candidates); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("ordinary regrant code=%s err=%v", security.Code(err), err)
	}
}

func TestSessionRotationRequiresFreshSuccessorAndRevocationClosesLineage(t *testing.T) {
	const (
		account   = "10000000-0000-4000-8000-000000000001"
		device    = "40000000-0000-4000-8000-000000000001"
		instance  = "50000000-0000-4000-8000-000000000001"
		root      = "70000000-0000-4000-8000-000000000001"
		next      = "70000000-0000-4000-8000-000000000002"
		unrelated = "70000000-0000-4000-8000-000000000003"
	)
	base := func(t *testing.T) ([]security.Candidate, security.Head) {
		t.Helper()
		prior := security.Head{Generation: 1}
		candidates := []security.Candidate{
			appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: account, IdentityKey: "owner-digest", Status: "active"}}),
			appendReplayEvent(t, &prior, 2, "device_registered", []security.Effect{{Kind: "device_registered", AccountID: account, DeviceID: device, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: "device-digest", Status: "active"}}),
			appendReplayEvent(t, &prior, 3, "session_granted", []security.Effect{{Kind: "session_granted", AccountID: account, DeviceID: device, SessionID: root, LineageRootSessionID: root, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"}}),
		}
		return candidates, prior
	}

	candidates, prior := base(t)
	candidates = append(candidates,
		appendReplayEvent(t, &prior, 4, "session_granted", []security.Effect{{Kind: "session_granted", AccountID: account, DeviceID: device, SessionID: next, LineageRootSessionID: next, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"}}),
		appendReplayEvent(t, &prior, 5, "session_rotated", []security.Effect{{Kind: "session_rotated", AccountID: account, DeviceID: device, SessionID: next, ParentSessionID: root, LineageRootSessionID: root, ProjectionMaterialDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"}}),
	)
	if _, err := security.Replay(candidates); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("successor collision code=%s err=%v", security.Code(err), err)
	}

	candidates, prior = base(t)
	candidates = append(candidates,
		appendReplayEvent(t, &prior, 4, "session_granted", []security.Effect{{Kind: "session_granted", AccountID: account, DeviceID: device, SessionID: unrelated, LineageRootSessionID: unrelated, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"}}),
		appendReplayEvent(t, &prior, 5, "session_rotated", []security.Effect{{Kind: "session_rotated", AccountID: account, DeviceID: device, SessionID: next, ParentSessionID: root, LineageRootSessionID: root, ProjectionMaterialDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"}}),
		appendReplayEvent(t, &prior, 6, "session_revoked", []security.Effect{{Kind: "session_revoked", AccountID: account, DeviceID: device, SessionID: root, LineageRootSessionID: root, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "revoked"}}),
	)
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{root, next} {
		if got, ok := state.Session(id); !ok || got.Status != "revoked" {
			t.Fatalf("session %s=%+v present=%v", id, got, ok)
		}
	}
	if got, ok := state.Session(unrelated); !ok || got.Status != "active" {
		t.Fatalf("unrelated session=%+v present=%v", got, ok)
	}
}

func TestOperatorCommandHashesBindEveryAuditField(t *testing.T) {
	principal := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, SecurityRevision: 9}
	owner := security.OwnerChangePayload{Version: "v1", FamilyID: "20000000-0000-4000-8000-000000000001",
		OldOwnerAccountID: principal.AccountID, NewOwnerAccountID: "10000000-0000-4000-8000-000000000002",
		OperatorPolicyID: "test-policy-v1", OperatorEvidenceDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		OperatorReasonCode: "owner_identity_unavailable", ExpectedRevision: 9}
	basePayload, _ := json.Marshal(owner)
	base := security.Command{ID: "30000000-0000-4000-8000-000000000001", Kind: "owner_recovered",
		AffectedAccountIDs: []string{owner.OldOwnerAccountID, owner.NewOwnerAccountID}, AffectedFamilyIDs: []string{owner.FamilyID}, Payload: basePayload}
	baseHash, err := security.CanonicalCommandHash(principal, base)
	if err != nil {
		t.Fatal(err)
	}
	owner.OperatorReasonCode = "different_reason"
	base.Payload, _ = json.Marshal(owner)
	changed, err := security.CanonicalCommandHash(principal, base)
	if err != nil || changed == baseHash {
		t.Fatalf("owner audit mutation hash=%x err=%v", changed, err)
	}

	entitlement := security.OperatorEntitlementPayload{Version: "v1", FamilyID: owner.FamilyID, EntitlementKey: "family_sync", Source: "hosted_beta", Status: "active",
		OperatorPolicyID: "entitlement-policy-test-v1", OperatorEvidenceDigest: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		OperatorReasonCode: "support_approved", ExpectedRevision: 9}
	payload, _ := json.Marshal(entitlement)
	command := security.Command{ID: "30000000-0000-4000-8000-000000000002", Kind: "operator_entitlement_granted", AffectedFamilyIDs: []string{owner.FamilyID}, Payload: payload}
	first, err := security.CanonicalCommandHash(principal, command)
	if err != nil {
		t.Fatal(err)
	}
	entitlement.OperatorEvidenceDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	command.Payload, _ = json.Marshal(entitlement)
	second, err := security.CanonicalCommandHash(principal, command)
	if err != nil || second == first {
		t.Fatalf("entitlement audit mutation hash=%x err=%v", second, err)
	}
}

func TestCompositeCommandHashesBindProjectionAnchors(t *testing.T) {
	principal := access.Principal{AccountID: "10000000-0000-4000-8000-000000000001", RecoveryEpoch: 7, SecurityRevision: 9}
	issued := security.IdentitySessionIssuedPayload{Version: "v1", AccountID: principal.AccountID, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RegisterAccount: true,
		DeviceID: "40000000-0000-4000-8000-000000000001", ServerInstanceID: "50000000-0000-4000-8000-000000000001", CanonicalOrigin: "https://family.example",
		DeviceCredentialDigest: "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RegisterDevice: true,
		SessionID: "70000000-0000-4000-8000-000000000001", LineageRootSessionID: "70000000-0000-4000-8000-000000000001", RecoveryEpoch: 7,
		ExpiresAt: "2026-09-21T12:00:00Z", ProjectionMaterialDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
	payload, _ := json.Marshal(issued)
	command := security.Command{ID: "30000000-0000-4000-8000-000000000010", Kind: "identity_session_issued", AffectedAccountIDs: []string{principal.AccountID}, Payload: payload}
	baseline, err := security.CanonicalCommandHash(principal, command)
	if err != nil {
		t.Fatal(err)
	}
	issued.ProjectionMaterialDigest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	command.Payload, _ = json.Marshal(issued)
	changed, err := security.CanonicalCommandHash(principal, command)
	if err != nil || changed == baseline {
		t.Fatalf("identity material mutation hash=%x err=%v", changed, err)
	}

	bootstrapped := security.FamilyBootstrappedPayload{Version: "v1", FamilyID: "20000000-0000-4000-8000-000000000001", OwnerAccountID: principal.AccountID,
		EntitlementKey: "family_sync", EntitlementSource: "hosted_beta", ProjectionMaterialDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}
	payload, _ = json.Marshal(bootstrapped)
	command = security.Command{ID: "30000000-0000-4000-8000-000000000011", Kind: "family_bootstrapped", AffectedAccountIDs: []string{principal.AccountID}, AffectedFamilyIDs: []string{bootstrapped.FamilyID}, Payload: payload}
	baseline, err = security.CanonicalCommandHash(principal, command)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapped.EntitlementSource = "hosted_beta_changed"
	command.Payload, _ = json.Marshal(bootstrapped)
	changed, err = security.CanonicalCommandHash(principal, command)
	if err != nil || changed == baseline {
		t.Fatalf("family entitlement mutation hash=%x err=%v", changed, err)
	}

	created := security.FamilyCreatedPayload{Version: "v1", FamilyID: bootstrapped.FamilyID, OwnerAccountID: principal.AccountID,
		ProjectionMaterialDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	payload, _ = json.Marshal(created)
	command = security.Command{ID: "30000000-0000-4000-8000-000000000012", Kind: "family_created", AffectedAccountIDs: []string{principal.AccountID}, AffectedFamilyIDs: []string{created.FamilyID}, Payload: payload}
	baseline, err = security.CanonicalCommandHash(principal, command)
	if err != nil {
		t.Fatal(err)
	}
	created.ProjectionMaterialDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	command.Payload, _ = json.Marshal(created)
	changed, err = security.CanonicalCommandHash(principal, command)
	if err != nil || changed == baseline {
		t.Fatalf("standalone family material mutation hash=%x err=%v", changed, err)
	}
}

func TestIdentitySessionCompositeRestoresUnavailableAndBindsExistingAuthority(t *testing.T) {
	const (
		account  = "10000000-0000-4000-8000-000000000001"
		device   = "40000000-0000-4000-8000-000000000001"
		instance = "50000000-0000-4000-8000-000000000001"
		first    = "70000000-0000-4000-8000-000000000001"
		next     = "70000000-0000-4000-8000-000000000002"
	)
	const identity = "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const credential = "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const material = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	prior := security.Head{Generation: 1}
	candidates := []security.Candidate{
		appendReplayEvent(t, &prior, 1, "identity_session_issued", []security.Effect{
			{Kind: "account_registered", AccountID: account, IdentityKey: identity, Status: "active"},
			{Kind: "device_registered", AccountID: account, DeviceID: device, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: credential, Status: "active"},
			{Kind: "session_granted", AccountID: account, IdentityKey: identity, DeviceID: device, SessionID: first, LineageRootSessionID: first, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: credential, ProjectionMaterialDigest: material, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"},
		}),
		appendReplayEvent(t, &prior, 2, "identity_unavailable", []security.Effect{{Kind: "identity_unavailable", AccountID: account, IdentityKey: identity, Status: "identity_unavailable"}}),
	}
	unavailableHead := prior
	restored := appendReplayEvent(t, &prior, 3, "identity_session_issued", []security.Effect{{Kind: "session_granted", AccountID: account, IdentityKey: identity, DeviceID: device, SessionID: next, LineageRootSessionID: next, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: credential, ProjectionMaterialDigest: material, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T13:00:00Z", Status: "active"}})
	state, err := security.Replay(append(candidates, restored))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := state.Account(account); got.Status != "active" {
		t.Fatalf("restored account=%+v", got)
	}
	if got, _ := state.Session(first); got.Status != "revoked" {
		t.Fatalf("old session=%+v", got)
	}
	if got, _ := state.Session(next); got.Status != "active" {
		t.Fatalf("new session=%+v", got)
	}

	for name, mutate := range map[string]func(*security.Effect){
		"identity": func(effect *security.Effect) {
			effect.IdentityKey = "hmac-sha256:identity-v1:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		},
		"instance": func(effect *security.Effect) { effect.ServerInstanceID = "50000000-0000-4000-8000-000000000002" },
		"origin":   func(effect *security.Effect) { effect.CanonicalOrigin = "https://other.example" },
		"credential": func(effect *security.Effect) {
			effect.DeviceCredentialDigest = "hmac-sha256:device-v1:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		},
	} {
		t.Run(name, func(t *testing.T) {
			branch := unavailableHead
			effect := security.Effect{Kind: "session_granted", AccountID: account, IdentityKey: identity, DeviceID: device, SessionID: next, LineageRootSessionID: next, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: credential, ProjectionMaterialDigest: material, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T13:00:00Z", Status: "active"}
			mutate(&effect)
			candidate := appendReplayEvent(t, &branch, 3, "identity_session_issued", []security.Effect{effect})
			if _, err := security.Replay(append(candidates, candidate)); security.Code(err) != "ledger_corrupt" {
				t.Fatalf("code=%s err=%v", security.Code(err), err)
			}
		})
	}

	deletedHead := unavailableHead
	deleted := appendReplayEvent(t, &deletedHead, 3, "account_deleted", []security.Effect{{Kind: "account_deleted", AccountID: account, IdentityKey: identity, Status: "deleted"}})
	deletedCandidates := append(append([]security.Candidate{}, candidates...), deleted)
	blocked := appendReplayEvent(t, &deletedHead, 4, "identity_session_issued", []security.Effect{{Kind: "session_granted", AccountID: account, IdentityKey: identity, DeviceID: device, SessionID: next, LineageRootSessionID: next, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: credential, ProjectionMaterialDigest: material, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T13:00:00Z", Status: "active"}})
	if _, err := security.Replay(append(deletedCandidates, blocked)); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("deleted restore code=%s err=%v", security.Code(err), err)
	}
}

func TestDeletedIdentityTombstoneDoesNotReactivateOldActor(t *testing.T) {
	const (
		oldActor = "10000000-0000-4000-8000-000000000001"
		newActor = "10000000-0000-4000-8000-000000000002"
	)
	prior := security.Head{Generation: 1}
	candidates := []security.Candidate{
		appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: oldActor, IdentityKey: "apple-proof-digest", Status: "active"}}),
		appendReplayEvent(t, &prior, 2, "account_deleted", []security.Effect{{Kind: "account_deleted", AccountID: oldActor, IdentityKey: "apple-proof-digest", Status: "deleted"}}),
		appendReplayEvent(t, &prior, 3, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: newActor, IdentityKey: "apple-proof-digest", Status: "active"}}),
	}
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if old, _ := state.Account(oldActor); old.Status != "deleted" || old.IdentityKey != "apple-proof-digest" {
		t.Fatalf("old tombstone=%+v", old)
	}
	if fresh, _ := state.Account(newActor); fresh.Status != "active" || fresh.IdentityKey != "apple-proof-digest" {
		t.Fatalf("fresh actor=%+v", fresh)
	}
	branch := prior
	restoreOld := appendReplayEvent(t, &branch, 4, "identity_restored", []security.Effect{{Kind: "identity_restored", AccountID: oldActor, IdentityKey: "apple-proof-digest", Status: "active"}})
	if _, err := security.Replay(append(candidates, restoreOld)); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("old actor restore code=%s err=%v", security.Code(err), err)
	}
}

func TestAccountDeletionCannotOrphanActiveFamilyOwner(t *testing.T) {
	const (
		account = "10000000-0000-4000-8000-000000000001"
		family  = "20000000-0000-4000-8000-000000000001"
	)
	prior := security.Head{Generation: 1}
	candidates := []security.Candidate{
		appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: account, IdentityKey: "owner-digest", Status: "active"}}),
		appendReplayEvent(t, &prior, 2, "family_created", []security.Effect{{Kind: "family_created", AccountID: account, FamilyID: family, Role: "owner", Status: "active", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}),
		appendReplayEvent(t, &prior, 3, "account_deleted", []security.Effect{{Kind: "account_deleted", AccountID: account, IdentityKey: "owner-digest", Status: "deleted"}}),
	}
	if _, err := security.Replay(candidates); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("active owner deletion code=%s err=%v", security.Code(err), err)
	}

	prior = security.Head{Generation: 1}
	candidates = []security.Candidate{
		appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: account, IdentityKey: "owner-digest", Status: "active"}}),
		appendReplayEvent(t, &prior, 2, "account_deleted", []security.Effect{{Kind: "account_deleted", AccountID: account, IdentityKey: "owner-digest", Status: "deleted"}}),
		appendReplayEvent(t, &prior, 3, "account_deleted", []security.Effect{{Kind: "account_deleted", AccountID: account, IdentityKey: "owner-digest", Status: "deleted"}}),
	}
	if _, err := security.Replay(candidates); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("second deletion code=%s err=%v", security.Code(err), err)
	}
}

func TestFamilyRecoveryRequiresFreshlyActiveIdentity(t *testing.T) {
	const (
		account    = "10000000-0000-4000-8000-000000000001"
		family     = "20000000-0000-4000-8000-000000000001"
		device     = "40000000-0000-4000-8000-000000000001"
		instance   = "50000000-0000-4000-8000-000000000001"
		session    = "70000000-0000-4000-8000-000000000001"
		identity   = "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		credential = "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	prior := security.Head{Generation: 1}
	base := []security.Candidate{
		appendReplayEvent(t, &prior, 1, "account_registered", []security.Effect{{Kind: "account_registered", AccountID: account, IdentityKey: identity, Status: "active"}}),
		appendReplayEvent(t, &prior, 2, "family_created", []security.Effect{{Kind: "family_created", AccountID: account, FamilyID: family, Role: "owner", Status: "active", ProjectionMaterialDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}}),
		appendReplayEvent(t, &prior, 3, "family_deletion_scheduled", []security.Effect{{Kind: "family_deletion_scheduled", AccountID: account, FamilyID: family, Role: "owner", Status: "deletion_pending", PurgeAfter: "2026-10-20T12:00:03Z"}}),
		appendReplayEvent(t, &prior, 4, "identity_unavailable", []security.Effect{{Kind: "identity_unavailable", AccountID: account, IdentityKey: identity, Status: "identity_unavailable"}}),
	}
	blockedHead := prior
	blocked := appendReplayEvent(t, &blockedHead, 5, "family_recovered", []security.Effect{{Kind: "family_recovered", AccountID: account, FamilyID: family, Role: "owner", Status: "active"}})
	if _, err := security.Replay(append(base, blocked)); security.Code(err) != "ledger_corrupt" {
		t.Fatalf("identity-unavailable recovery code=%s err=%v", security.Code(err), err)
	}

	restored := appendReplayEvent(t, &prior, 5, "identity_session_issued", []security.Effect{
		{Kind: "device_registered", AccountID: account, DeviceID: device, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: credential, Status: "active"},
		{Kind: "session_granted", AccountID: account, IdentityKey: identity, DeviceID: device, SessionID: session, LineageRootSessionID: session, ServerInstanceID: instance, CanonicalOrigin: "https://family.example", DeviceCredentialDigest: credential, ProjectionMaterialDigest: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"},
	})
	recovered := appendReplayEvent(t, &prior, 6, "family_recovered", []security.Effect{{Kind: "family_recovered", AccountID: account, FamilyID: family, Role: "owner", Status: "active"}})
	state, err := security.Replay(append(base, restored, recovered))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := state.Family(family); got.Status != "active" || got.OwnerAccountID != account {
		t.Fatalf("family=%+v", got)
	}
}
