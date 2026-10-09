package auth

import (
	"babytracker/server/internal/access"
	"babytracker/server/internal/config"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecoveryDeadlineMatrix(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeSelfHosted, config.ModeHosted} {
		t.Run(string(mode), func(t *testing.T) {
			var h *sessionFixture
			var original SessionBundle
			if mode == config.ModeHosted {
				hosted := newHostedSessionFixture(t)
				h = hosted.sessionFixture
				var err error
				original, _, err = hosted.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent", Confirmed: true}, nil)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				h = newSessionFixture(t, mode)
				original = h.Issue()
			}
			request := recoveryRequest(original)
			child, err := h.Service.RefreshRecoverable(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if !child.ReauthenticateAt.Equal(original.ReauthenticateAt) {
				t.Fatal("deadline extended")
			}
			h.Clock.Advance(original.ReauthenticateAt.Sub(h.Clock.Now()))
			if _, err = h.Service.RefreshRecoverable(t.Context(), request); Code(err) != "reauthentication_required" {
				t.Fatalf("deadline: %v", err)
			}
		})
	}
	t.Run("parent expired child valid", func(t *testing.T) {
		h := newSessionFixture(t, config.ModeSelfHosted)
		h.Service.refreshTTL = time.Hour
		original := h.Issue()
		h.Clock.Advance(30 * time.Minute)
		request := recoveryRequest(original)
		child, err := h.Service.RefreshRecoverable(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		h.Clock.Advance(31 * time.Minute)
		recovered, err := h.Service.RefreshRecoverable(t.Context(), request)
		if err != nil || recovered.RefreshToken != child.RefreshToken {
			t.Fatalf("child-valid: %v", err)
		}
		h.Clock.Advance(30 * time.Minute)
		if _, err = h.Service.RefreshRecoverable(t.Context(), request); Code(err) != "reauthentication_required" {
			t.Fatalf("child expiry: %v", err)
		}
	})
}

func TestRecoveryCurrentAuthorityOverridesFrozenScope(t *testing.T) {
	for _, action := range []string{"logout", "epoch", "consumed child", "account deletion", "scope revocation"} {
		t.Run(action, func(t *testing.T) {
			h := newSessionFixture(t, config.ModeSelfHosted)
			original := h.Issue()
			if action == "scope revocation" {
				recoveryFamily(t, h, original)
			}
			request := recoveryRequest(original)
			child, err := h.Service.RefreshRecoverable(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "logout":
				principal, err := h.Service.Authenticate(t.Context(), child.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = h.Service.Logout(t.Context(), principal, h.nextID(7)); err != nil {
					t.Fatal(err)
				}
			case "account deletion", "scope revocation":
				principal, err := h.Service.Authenticate(t.Context(), child.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				state, err := h.Service.security.ReadState(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				command := security.Command{ID: h.nextID(7), AffectedAccountIDs: []string{original.Account.ID}}
				if action == "account deletion" {
					account, _ := state.Account(original.Account.ID)
					command.Kind = "account_deleted"
					command.Payload, _ = json.Marshal(security.AccountDeletedPayload{Version: "v1", AccountID: original.Account.ID, IdentityKey: account.IdentityKey})
				} else {
					slot, _ := state.FamilySlot(original.Account.ID)
					command.Kind = "family_deletion_scheduled"
					command.AffectedFamilyIDs = []string{slot.FamilyID}
					command.Payload, _ = json.Marshal(security.FamilyLifecyclePayload{Version: "v1", FamilyID: slot.FamilyID, ClaimOwnerAccountID: original.Account.ID, PreviousMemberIDs: []string{original.Account.ID}, PreviousRoles: []string{"owner"}})
				}
				if _, err = h.Service.security.Execute(t.Context(), principal, command); err != nil {
					t.Fatal(err)
				}
			case "epoch":
				h.SetEpoch(2)
			case "consumed child":
				if _, err = h.Service.Refresh(t.Context(), child.RefreshToken); err != nil {
					t.Fatal(err)
				}
			}
			before := h.authMutationSnapshot()
			result, err := h.Service.RefreshRecoverable(t.Context(), request)
			if err == nil || result.AccessToken != "" || result.RefreshToken != "" {
				t.Fatalf("revoked returned credentials: %v", err)
			}
			if h.authMutationSnapshot() != before {
				t.Fatal("denial mutated authority")
			}
		})
	}
}

func TestRecoveryAcknowledgement(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := h.Service.Authenticate(t.Context(), child.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	bad := request.DeviceContinuity
	bad.Credential = child.RefreshToken
	if err = h.Service.AcknowledgeRefresh(t.Context(), principal, request.AttemptID, bad); Code(err) != "device_continuity_invalid" {
		t.Fatalf("bad ack: %v", err)
	}
	recoveryReceiptCount(t, h, 1)
	if err = h.Service.AcknowledgeRefresh(t.Context(), principal, request.AttemptID, request.DeviceContinuity); err != nil {
		t.Fatal(err)
	}
	recoveryReceiptCount(t, h, 0)
	if err = h.Service.AcknowledgeRefresh(t.Context(), principal, request.AttemptID, request.DeviceContinuity); err != nil {
		t.Fatal(err)
	}
	if err = h.Service.AcknowledgeRefresh(t.Context(), principal, request.AttemptID, bad); Code(err) != "device_continuity_invalid" {
		t.Fatalf("absent bad proof: %v", err)
	}
	h.Clock.Advance(11 * time.Minute)
	if err = h.Service.AcknowledgeRefresh(t.Context(), principal, request.AttemptID, request.DeviceContinuity); Code(err) != "access_expired" {
		t.Fatalf("expired ack: %v", err)
	}
}

func TestRecoveryRetention(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(30 * 24 * time.Hour)
	h.RestartAndReconcile()
	recovered, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil || recovered.RefreshToken != child.RefreshToken {
		t.Fatalf("restart recovery: %v", err)
	}
	successor := recoveryRequest(child.SessionBundle)
	successor.DeviceContinuity = request.DeviceContinuity
	successor.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if _, err = h.Service.RefreshRecoverable(t.Context(), successor); err != nil {
		t.Fatal(err)
	}
	recoveryReceiptCount(t, h, 1)
}

func TestRecoveryExpiredRowsReleaseUnneededKeyReferences(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	before, err := recoveryInventory(t, h)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.SessionToken, []string{h.tokenKeys.Current.ID}) {
		t.Fatalf("active references=%+v", before)
	}
	h.Clock.Advance(original.ReauthenticateAt.Sub(h.Clock.Now()))
	after, err := recoveryInventory(t, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.SessionToken) != 0 {
		t.Fatalf("expired references=%+v", after)
	}
}

func recoveryReceiptCount(t *testing.T, h *sessionFixture, want int) {
	t.Helper()
	var count int
	if err := h.db.Owner.QueryRow(t.Context(), "SELECT count(*) FROM refresh_recovery_receipts").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("receipt count=%d want=%d", count, want)
	}
}

func recoveryInventory(t *testing.T, h *sessionFixture) (RecoveryKeyInventory, error) {
	t.Helper()
	var keys RecoveryKeyInventory
	err := h.Service.security.WithAuthoritySnapshot(t.Context(), func(snapshot security.AuthoritySnapshot) error {
		var err error
		keys, err = h.Service.ReferencedRecoveryKeyIDs(t.Context(), snapshot.View)
		return err
	})
	return keys, err
}

func TestRecoveryKeyRetentionAcrossRestart(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	old := h.tokenKeys.Current
	next := testKeyring("session.next", 0x69).Current
	h.tokenKeys.Current = next
	h.tokenKeys.Previous = []identity.DigestKey{old}
	h.Service.tokenKeys = cloneKeyring(h.tokenKeys)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	h.RestartAndReconcile()
	keys, err := recoveryInventory(t, h)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys.SessionToken, []string{old.ID, next.ID}) && !reflect.DeepEqual(keys.SessionToken, []string{next.ID, old.ID}) {
		t.Fatalf("receipt keys=%+v", keys)
	}
	if !reflect.DeepEqual(keys.DeviceContinuity, []string{h.deviceKeys.Current.ID}) {
		t.Fatalf("device refs=%+v", keys)
	}
	recovered, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil || recovered.AccessToken != child.AccessToken || recovered.RefreshToken != child.RefreshToken {
		t.Fatalf("restart: %v", err)
	}
	keys.SessionToken[0] = "mutated"
	again, err := recoveryInventory(t, h)
	if err != nil || reflect.DeepEqual(keys, again) {
		t.Fatal("inventory aliases caller slices")
	}
}

func TestRecoveryCleanupAndFreshProofAreDeviceScoped(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	first := h.Issue()
	firstRequest := recoveryRequest(first)
	if _, err := h.Service.RefreshRecoverable(t.Context(), firstRequest); err != nil {
		t.Fatal(err)
	}
	second := h.Issue()
	secondRequest := recoveryRequest(second)
	secondRequest.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if _, err := h.Service.RefreshRecoverable(t.Context(), secondRequest); err != nil {
		t.Fatal(err)
	}
	recoveryReceiptCount(t, h, 2)
	challenge, err := h.Identity.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Service.Exchange(t.Context(), identity.ExchangeRequest{CommandID: h.nextID(7), Challenge: challenge.Challenge, IdentityToken: "subject-one", InstallationID: "installation-subject-one", DeviceContinuity: &firstRequest.DeviceContinuity})
	if err != nil {
		t.Fatal(err)
	}
	recoveryReceiptCount(t, h, 1)
	if _, err = h.Service.RefreshRecoverable(t.Context(), secondRequest); err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.Runtime.Exec(t.Context(), "SELECT app.cleanup_refresh_recovery_receipts($1)", h.Clock.Now()); err == nil {
		t.Fatal("runtime obtained maintenance cleanup")
	}
	if _, err = h.db.Maintenance.Exec(t.Context(), "SELECT app.cleanup_refresh_recovery_receipts($1)", h.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	recoveryReceiptCount(t, h, 1)
	h.Clock.Advance(defaultSelfHostedAge)
	if _, err = h.db.Maintenance.Exec(t.Context(), "SELECT app.cleanup_refresh_recovery_receipts($1)", h.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	recoveryReceiptCount(t, h, 0)
}

func TestRecoveryWrongLineageAcknowledgementKeepsReceipt(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	challenge, err := h.Identity.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.Service.Exchange(t.Context(), identity.ExchangeRequest{CommandID: h.nextID(7), Challenge: challenge.Challenge, IdentityToken: "subject-one", InstallationID: "installation-subject-one", DeviceContinuity: &request.DeviceContinuity})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Service.RefreshRecoverable(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	principal, err := h.Service.Authenticate(t.Context(), other.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Service.AcknowledgeRefresh(t.Context(), principal, request.AttemptID, request.DeviceContinuity); Code(err) != "session_revoked" {
		t.Fatalf("wrong lineage ack: %v", err)
	}
	recoveryReceiptCount(t, h, 1)
}

func recoveryFamily(t *testing.T, h *sessionFixture, bundle SessionBundle) {
	t.Helper()
	repository, err := family.NewRepository(h.db.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	families, err := family.New(family.Options{Security: h.Service.security, Repository: repository, Entitlements: family.SelfHostedEntitlementPolicy{}, InvitationKeys: testKeyring("invitation.v1", 0x75), Origin: h.origin, Now: h.Clock.Now, MaxCaregivers: 10, MaxInvitations: 10, InvitationTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := h.Service.Authenticate(t.Context(), bundle.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = families.Create(t.Context(), principal, h.nextID(7), "Family"); err != nil && family.Code(err) != "membership_revoked" {
		t.Fatal(err)
	}
	state, err := h.Service.security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.FamilySlot(bundle.Account.ID); !ok {
		t.Fatal("family creation did not commit")
	}

}

func TestRecoveryFrozenFamilyRequiresCurrentEntitlementButAckIsAccountScoped(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	recoveryFamily(t, h, original)
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	// The existing family has no hosted grant; changing the policy makes that
	// current entitlement unusable without changing the frozen child scope.
	h.Service.mode = config.ModeHosted
	if _, err = h.Service.RefreshRecoverable(t.Context(), request); Code(err) != "session_revoked" {
		t.Fatalf("missing current grant: %v", err)
	}
	principal, err := h.Service.Authenticate(t.Context(), child.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Service.AcknowledgeRefresh(t.Context(), principal, request.AttemptID, request.DeviceContinuity); err != nil {
		t.Fatalf("account-scoped ack required family entitlement: %v", err)
	}
	recoveryReceiptCount(t, h, 0)
}

func TestRecoveryUnknownAuthorityRetainsKeyReferences(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	h.Clock.Advance(original.ReauthenticateAt.Sub(h.Clock.Now()))
	keys, err := h.Service.ReferencedRecoveryKeyIDs(t.Context(), security.AuthorityView{})
	if err != nil || len(keys.SessionToken) != 1 || keys.SessionToken[0] != h.tokenKeys.Current.ID {
		t.Fatalf("unknown authority retired key: %+v %v", keys, err)
	}
}

func TestRecoveryFirstRotationRequiresCurrentFamilyAuthority(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	recoveryFamily(t, h, original)
	h.Service.mode = config.ModeHosted
	before := h.authMutationSnapshot()
	if result, err := h.Service.RefreshRecoverable(t.Context(), recoveryRequest(original)); Code(err) != "session_revoked" || result.RefreshToken != "" {
		t.Fatalf("issued without current hosted entitlement: %v", err)
	}
	if h.authMutationSnapshot() != before {
		t.Fatal("denied first rotation mutated authority")
	}
}

func TestRecoveryAcknowledgementRejectsZeroAttempt(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	principal, err := h.Service.Authenticate(t.Context(), original.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Service.AcknowledgeRefresh(t.Context(), principal, "00000000-0000-4000-8000-000000000000", recoveryRequest(original).DeviceContinuity); Code(err) != "invalid_request" {
		t.Fatalf("zero ack: %v", err)
	}
}

func TestRecoveryHistoricalLongLifetimeKeysExpireAtAnnualCap(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	h.Service.absoluteAge = 100 * 365 * 24 * time.Hour
	h.Service.refreshTTL = h.Service.absoluteAge
	h.Issue()
	h.Clock.Advance(defaultSelfHostedAge)
	keys, err := recoveryInventory(t, h)
	if err != nil || len(keys.SessionToken) != 0 {
		t.Fatalf("historical row retained beyond annual cap: %+v %v", keys, err)
	}
}

func TestRecoveryAcknowledgementSameAttemptIsDeviceScoped(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	first, second := h.Issue(), h.Issue()
	a, b := recoveryRequest(first), recoveryRequest(second)
	if a.AttemptID != b.AttemptID || first.DeviceID == second.DeviceID {
		t.Fatal("fixture requires distinct devices and shared attempt")
	}
	firstChild, err := h.Service.RefreshRecoverable(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	secondChild, err := h.Service.RefreshRecoverable(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, deviceID, instanceID string
		epoch                      uint64
		want                       int
	}{
		{"first device", first.DeviceID, h.instance, h.epoch, 1},
		{"second device", second.DeviceID, h.instance, h.epoch, 1},
		{"wrong instance", first.DeviceID, "ffffffff-ffff-4fff-8fff-ffffffffffff", h.epoch, 0},
		{"wrong epoch", first.DeviceID, h.instance, h.epoch + 1, 0},
	} {
		receipts, err := h.Service.repository.LookupRecoveryAcknowledgements(t.Context(), first.Account.ID, test.deviceID, test.instanceID, test.epoch, a.AttemptID)
		if err != nil || len(receipts) != test.want {
			t.Fatalf("%s typed lookup rows=%d want=%d err=%v", test.name, len(receipts), test.want, err)
		}
	}
	firstPrincipal, err := h.Service.Authenticate(t.Context(), firstChild.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	secondPrincipal, err := h.Service.Authenticate(t.Context(), secondChild.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	before := h.authMutationSnapshot()
	if err = h.Service.AcknowledgeRefresh(t.Context(), firstPrincipal, a.AttemptID, a.DeviceContinuity); err != nil {
		t.Fatalf("other device blocked ACK: %v", err)
	}
	recoveryReceiptCount(t, h, 1)
	if err = h.Service.AcknowledgeRefresh(t.Context(), firstPrincipal, a.AttemptID, a.DeviceContinuity); err != nil {
		t.Fatalf("other device blocked absent ACK: %v", err)
	}
	recoveryReceiptCount(t, h, 1)
	recovered, err := h.Service.RefreshRecoverable(t.Context(), b)
	if err != nil || recovered.RefreshToken != secondChild.RefreshToken {
		t.Fatalf("other device receipt lost: %v", err)
	}
	if err = h.Service.AcknowledgeRefresh(t.Context(), secondPrincipal, b.AttemptID, b.DeviceContinuity); err != nil {
		t.Fatal(err)
	}
	recoveryReceiptCount(t, h, 0)
	if h.authMutationSnapshot() != before {
		t.Fatal("ACK changed authority head")
	}
}

func assertRecoveryDeniedWithoutMutation(t *testing.T, h *sessionFixture, input RecoverableRefreshInput) {
	t.Helper()
	before := h.authMutationSnapshot()
	recovered, err := h.Service.RefreshRecoverable(t.Context(), input)
	if err == nil || recovered.AccessToken != "" || recovered.RefreshToken != "" {
		t.Fatalf("revoked authority returned credentials: %v", err)
	}
	if _, err = h.Service.ResolveRecoverableRefreshSubject(t.Context(), input); err == nil {
		t.Fatal("revoked authority supplied quota subject")
	}
	if h.authMutationSnapshot() != before {
		t.Fatal("denied recovery changed authority head")
	}
}

func TestRecoveryDeviceBindingRevocationOverridesReceipt(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	input := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := h.Service.Authenticate(t.Context(), child.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Service.security.ExecutePrepared(t.Context(), func(_ context.Context, snapshot security.AuthoritySnapshot) (security.PreparedCommand, error) {
		actor, err := snapshot.ResolveSessionActor(security.ExistingSessionActorInput{AccountID: principal.AccountID, DeviceID: principal.DeviceID, SessionID: principal.SessionID, RecoveryEpoch: principal.RecoveryEpoch, SecurityRevision: principal.SecurityRevision})
		if err != nil {
			return security.PreparedCommand{}, err
		}
		device, _ := snapshot.View.Device(principal.DeviceID)
		return snapshot.PrepareDeviceRevocation(actor, security.DeviceRevocationCommandInput{Meta: security.CommandMeta{CommandID: h.nextID(7), At: h.Clock.Now()}, Payload: security.DevicePayload{Version: "v1", AccountID: principal.AccountID, DeviceID: principal.DeviceID, ServerInstanceID: device.ServerInstanceID, CanonicalOrigin: device.CanonicalOrigin, DeviceCredentialDigest: device.CredentialDigest}})
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := h.Service.security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	device, ok := state.Device(original.DeviceID)
	if !ok || device.Status != "revoked" {
		t.Fatal("binding revocation did not commit")
	}
	assertRecoveryDeniedWithoutMutation(t, h, input)
}

func executeRecoveryFamilyCommand(t *testing.T, h *sessionFixture, principal access.Principal, kind security.EventKind, payload any, accounts []string, familyID string) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Service.security.Execute(t.Context(), principal, security.Command{ID: h.nextID(7), Kind: kind, Payload: raw, AffectedAccountIDs: accounts, AffectedFamilyIDs: []string{familyID}})
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
}

func TestRecoveryMembershipRemovalOverridesReceipt(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	owner := h.Issue()
	recoveryFamily(t, h, owner)
	caregiver, err := h.exchange("caregiver-subject", &identity.ProfileInput{DisplayName: "Caregiver", Confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := h.Service.security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slot, ok := state.FamilySlot(owner.Account.ID)
	if !ok {
		t.Fatal("owner family missing")
	}
	ownerPrincipal, err := h.Service.Authenticate(t.Context(), owner.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	caregiverPrincipal, err := h.Service.Authenticate(t.Context(), caregiver.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	invitation := security.InvitationPayload{Version: "v1", InvitationID: h.nextID(8), FamilyID: slot.FamilyID, CreatorAccountID: owner.Account.ID, TokenDigest: "hmac-sha256:invitation.v1:" + strings.Repeat("a", 64), Role: "caregiver", Status: "active", ExpiresAt: h.Clock.Now().Add(time.Hour).Format(time.RFC3339Nano)}
	executeRecoveryFamilyCommand(t, h, ownerPrincipal, "invitation_created", invitation, []string{owner.Account.ID}, slot.FamilyID)
	invitation.RedeemerAccountID = caregiver.Account.ID
	invitation.Status = "redeemed"
	executeRecoveryFamilyCommand(t, h, caregiverPrincipal, "invitation_redeemed", invitation, []string{caregiver.Account.ID}, slot.FamilyID)
	input := recoveryRequest(caregiver)
	child, err := h.Service.RefreshRecoverable(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if child.FamilySlot == nil || child.FamilySlot.FamilyID != slot.FamilyID {
		t.Fatal("child did not inherit authorized family")
	}
	executeRecoveryFamilyCommand(t, h, ownerPrincipal, "member_removed", security.MembershipPayload{Version: "v1", AccountID: caregiver.Account.ID, FamilyID: slot.FamilyID}, []string{caregiver.Account.ID}, slot.FamilyID)
	state, err = h.Service.security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	membership, ok := state.Membership(slot.FamilyID, caregiver.Account.ID)
	if !ok || membership.Status != "removed" {
		t.Fatalf("membership removal missing: %+v", membership)
	}
	assertRecoveryDeniedWithoutMutation(t, h, input)
}

type recoveryTestOperatorGuard struct{ head security.Head }

func (guard recoveryTestOperatorGuard) ValidateOperatorAuthorityUnderFence(_ context.Context, head security.Head) error {
	if head != guard.head {
		return ErrUnavailable
	}
	return nil
}
func recoveryEntitlementCommand(t *testing.T, h *sessionFixture, familyID, operation string, expiry time.Time) {
	t.Helper()
	head, err := h.store.Heads.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reason := "beta_extension"
	if operation == "suspend" {
		reason = "abuse_suspension"
	}
	_, err = h.Service.security.ExecuteOperatorEntitlement(t.Context(), security.OperatorEntitlementInput{CommandID: h.nextID(7), OperatorID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", FamilyID: familyID, Operation: operation, Key: "family_sync", Source: "hosted_beta", ExpiresAt: expiry, PolicyID: "Recovery.Test_v1", EvidenceDigest: strings.Repeat("e", 64), ReasonCode: reason, ExpectedRevision: head.Revision, ConfirmationExpiresAt: h.Clock.Now().Add(5 * time.Minute)}, recoveryTestOperatorGuard{head: head})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryHostedGrantedEntitlementLaterInvalidates(t *testing.T) {
	for _, transition := range []string{"suspended", "expired"} {
		t.Run(transition, func(t *testing.T) {
			hosted := newHostedSessionFixture(t)
			h := hosted.sessionFixture
			original, _, err := hosted.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent", Confirmed: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			recoveryFamily(t, h, original)
			state, err := h.Service.security.ReadState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			slot, ok := state.FamilySlot(original.Account.ID)
			if !ok {
				t.Fatal("family missing")
			}
			expiry := h.Clock.Now().Add(time.Hour)
			recoveryEntitlementCommand(t, h, slot.FamilyID, "grant", expiry)
			state, err = h.Service.security.ReadState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			grant, ok := state.Entitlement(slot.FamilyID, "family_sync")
			if !ok || grant.Status != "active" || grant.ExpiresAt != expiry.Format(time.RFC3339) {
				t.Fatal("valid grant did not commit")
			}
			input := recoveryRequest(original)
			child, err := h.Service.RefreshRecoverable(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.Service.RefreshRecoverable(t.Context(), input); err != nil {
				t.Fatalf("granted recovery: %v", err)
			}
			if transition == "suspended" {
				recoveryEntitlementCommand(t, h, slot.FamilyID, "suspend", expiry)
			} else {
				h.Clock.Advance(time.Hour)
			}
			row, found, err := h.Service.lookupRefreshAcrossKeys(t.Context(), child.RefreshToken)
			if err != nil || !found || !h.Clock.Now().Before(row.row.RefreshExpiresAt) || !h.Clock.Now().Before(child.ReauthenticateAt) {
				t.Fatal("entitlement transition overlapped credential expiry")
			}
			assertRecoveryDeniedWithoutMutation(t, h, input)
		})
	}
}

func TestRecoveryActiveDescendantAcknowledgesSupersededAttempt(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	input := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := h.Service.lookupRefreshAcrossKeys(t.Context(), original.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	next := input
	next.RefreshToken = child.RefreshToken
	next.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	descendant, err := h.Service.RefreshRecoverable(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := h.Service.repository.LookupRecoveryReceipt(t.Context(), parent.row.SessionID, input.AttemptID); err != nil || found {
		t.Fatal("successor retained predecessor receipt")
	}
	recoveryReceiptCount(t, h, 1)
	principal, err := h.Service.Authenticate(t.Context(), descendant.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	before := h.authMutationSnapshot()
	if err = h.Service.AcknowledgeRefresh(t.Context(), principal, input.AttemptID, input.DeviceContinuity); err != nil {
		t.Fatalf("descendant ACK: %v", err)
	}
	recoveryReceiptCount(t, h, 1)
	recovered, err := h.Service.RefreshRecoverable(t.Context(), next)
	if err != nil || recovered.RefreshToken != descendant.RefreshToken {
		t.Fatalf("predecessor ACK removed successor: %v", err)
	}
	if h.authMutationSnapshot() != before {
		t.Fatal("descendant ACK mutated authority")
	}
}

func TestRecoveryKeyEligibilityDeadlineAndUncertainty(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	var eligibility RecoveryKeyEligibility
	read := func() {
		t.Helper()
		if err := h.Service.security.WithAuthorityView(t.Context(), func(v security.AuthorityView) error {
			var err error
			eligibility, err = h.Service.ReferencedRecoveryKeyEligibility(t.Context(), v)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if !eligibility.Cacheable || !eligibility.RevalidateAt.Equal(original.ReauthenticateAt) || len(eligibility.Inventory.SessionToken) != 1 {
		t.Fatalf("eligibility=%+v deadline=%v", eligibility, original.ReauthenticateAt)
	}
	h.Clock.Advance(original.ReauthenticateAt.Sub(h.Clock.Now()))
	read()
	if !eligibility.Cacheable || !eligibility.RevalidateAt.IsZero() || len(eligibility.Inventory.SessionToken) != 0 {
		t.Fatalf("expired=%+v", eligibility)
	}
	unknown, err := h.Service.ReferencedRecoveryKeyEligibility(t.Context(), security.AuthorityView{})
	if err != nil || unknown.Cacheable || len(unknown.Inventory.SessionToken) != 1 {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
}

func TestRecoveryKeyEligibilityLineageFailureRetainsAndRetries(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	provider, err := NewRecoveryKeyReferences(h.Service.repository, h.mode, h.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	read := func() (RecoveryKeyEligibility, error) {
		var result RecoveryKeyEligibility
		err := h.Service.security.WithAuthorityView(t.Context(), func(v security.AuthorityView) error {
			var err error
			result, err = provider(t.Context(), v)
			return err
		})
		return result, err
	}
	h.Clock.Advance(original.ReauthenticateAt.Sub(h.Clock.Now()))
	if _, err = h.db.Owner.Exec(t.Context(), "REVOKE EXECUTE ON FUNCTION app.session_lineage_started_at(uuid,uuid) FROM baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err := read()
		if err != nil || result.Cacheable || len(result.Inventory.SessionToken) != 1 {
			t.Fatalf("uncertain result=%+v err=%v", result, err)
		}
	}
	if _, err = h.db.Owner.Exec(t.Context(), "GRANT EXECUTE ON FUNCTION app.session_lineage_started_at(uuid,uuid) TO baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	result, err := read()
	if err != nil || !result.Cacheable || len(result.Inventory.SessionToken) != 0 {
		t.Fatalf("resolved result=%+v err=%v", result, err)
	}
	if _, err = h.db.Owner.Exec(t.Context(), "REVOKE EXECUTE ON FUNCTION app.refresh_recovery_key_candidates() FROM baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if _, err = read(); err == nil {
		t.Fatal("candidate query failure authorized inventory")
	}
}

func TestRecoveryKeyEligibilityNearestRefreshDeadline(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	h.Service.refreshTTL = 2 * time.Hour
	original := h.Issue()
	var result RecoveryKeyEligibility
	if err := h.Service.security.WithAuthorityView(t.Context(), func(v security.AuthorityView) error {
		var err error
		result, err = h.Service.ReferencedRecoveryKeyEligibility(t.Context(), v)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := h.Clock.Now().Add(2 * time.Hour)
	if !result.RevalidateAt.Equal(want) || !want.Before(original.ReauthenticateAt) || !result.Cacheable {
		t.Fatalf("result=%+v want=%v", result, want)
	}
}

func TestReceiptInventoryCompleteAfterRecoverableRotation(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	provider, err := NewRecoveryKeyReferences(h.Service.repository, h.mode, h.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	authority, err := security.New(security.Options{Ledger: h.store.Ledger, Heads: h.heads, Projection: h.projection, Fence: h.store.Fence, Runtime: h.db.Runtime, Now: h.Clock.Now,
		Epoch: func(context.Context) (uint64, error) { return h.epoch, nil },
		Binding: func(context.Context) (security.AuthorityBinding, error) {
			return security.AuthorityBinding{InstanceID: h.instance, Epoch: h.epoch, TrustedHead: security.Head{Generation: 1}}, nil
		},
		Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, nil
		},
		RecoveryKeyReferences: func(ctx context.Context, v security.AuthorityView) (security.RecoveryKeyReferencesResult, error) {
			calls++
			r, err := provider(ctx, v)
			return security.RecoveryKeyReferencesResult{Inventory: security.CredentialKeyInventory{SessionToken: r.Inventory.SessionToken, DeviceContinuity: r.Inventory.DeviceContinuity}, RevalidateAt: r.RevalidateAt, Cacheable: r.Cacheable}, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Service.security = authority
	original := h.Issue()
	old := h.tokenKeys.Current
	next := testKeyring("session.next", 0x69).Current
	h.tokenKeys.Current = next
	h.tokenKeys.Previous = []identity.DigestKey{old}
	h.Service.tokenKeys = cloneKeyring(h.tokenKeys)
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	afterCommit := calls
	for i := 0; i < 5; i++ {
		keys, err := authority.ReferencedCredentialKeyIDs(t.Context())
		if err != nil || !reflect.DeepEqual(keys.SessionToken, []string{next.ID, old.ID}) || !reflect.DeepEqual(keys.DeviceContinuity, []string{h.deviceKeys.Current.ID}) {
			t.Fatalf("union=%+v err=%v", keys, err)
		}
		keys.SessionToken[0] = "tamper"
	}
	if calls != afterCommit {
		t.Fatalf("warm readiness rescanned candidates: before=%d after=%d", afterCommit, calls)
	}
	principal, err := h.Service.Authenticate(t.Context(), child.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	// A ring retirement concurrent with ACK must still see the parent reference.
	// Each read models readiness with a ring that has dropped that required key.
	start := make(chan struct{})
	results := make(chan error, 17)
	for i := 0; i < 16; i++ {
		go func() {
			<-start
			keys, err := authority.ReferencedCredentialKeyIDs(t.Context())
			if err == nil && !reflect.DeepEqual(keys.SessionToken, []string{next.ID, old.ID}) {
				err = ErrUnavailable
			}
			results <- err
		}()
	}
	go func() {
		<-start
		results <- h.Service.AcknowledgeRefresh(t.Context(), principal, request.AttemptID, request.DeviceContinuity)
	}()
	close(start)
	for i := 0; i < 17; i++ {
		if err := <-results; err != nil {
			t.Fatalf("retirement/ACK race exposed incomplete inventory: %v", err)
		}
	}
	keys, err := authority.ReferencedCredentialKeyIDs(t.Context())
	if err != nil || len(keys.SessionToken) != 2 {
		t.Fatalf("ack must retain cached receipt refs conservatively: %+v %v", keys, err)
	}
	h.Clock.Advance(child.ReauthenticateAt.Sub(h.Clock.Now()))
	keys, err = authority.ReferencedCredentialKeyIDs(t.Context())
	if err != nil || len(keys.SessionToken) != 0 || calls != afterCommit+1 {
		t.Fatalf("expiry union=%+v err=%v calls=%d", keys, err, calls)
	}
}

func TestRecoveryKeyEligibilitySelfHostedLineageBoundary(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	// Projected policy dates can be later than the server's hard lineage ceiling.
	later := original.ReauthenticateAt.Add(24 * time.Hour)
	if _, err := h.db.Owner.Exec(t.Context(), "UPDATE sessions SET refresh_expires_at=$1,reauthenticate_at=$1,absolute_expires_at=$1", later); err != nil {
		t.Fatal(err)
	}
	if err := h.Service.security.WithAuthorityView(t.Context(), func(v security.AuthorityView) error {
		result, err := h.Service.ReferencedRecoveryKeyEligibility(t.Context(), v)
		if err == nil && (!result.Cacheable || !result.RevalidateAt.Equal(original.ReauthenticateAt)) {
			t.Fatalf("lineage boundary=%+v want=%v", result, original.ReauthenticateAt)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
