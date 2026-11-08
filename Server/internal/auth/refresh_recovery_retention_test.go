package auth

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
)

func TestRecoveryDeadlineMatrix(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeSelfHosted, config.ModeHosted} {
		t.Run(string(mode), func(t *testing.T) {
			h := newSessionFixture(t, mode)
			original := h.Issue()
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
				if _, err = h.Service.Logout(t.Context(), h.principal(child.SessionBundle), h.nextID(7)); err != nil {
					t.Fatal(err)
				}
			case "account deletion":
				principal := h.principal(child.SessionBundle)
				candidates, err := h.Identity.AuthorityKeyCandidates(identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: "subject-one"})
				if err != nil {
					t.Fatal(err)
				}
				resolution, err := h.Authority.ResolveIdentity(t.Context(), candidates)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = h.Authority.DeleteAccount(t.Context(), authority.DeleteAccount{CommandID: h.nextID(7), AccountID: principal.AccountID,
					IdentityKey: resolution.IdentityKey, Principal: principal}); err != nil {
					t.Fatal(err)
				}
			case "scope revocation":
				principal := h.principal(child.SessionBundle)
				if _, err = h.Authority.ScheduleDeletion(t.Context(), authority.ScheduleDeletion{CommandID: h.nextID(7), OwnerAccountID: principal.AccountID,
					Principal: principal}); err != nil {
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

// A lost-ack retry after ForfeitRecovery moved the child from
// family_recovery to account scope re-derives the committed child from the
// scope frozen in the receipt, through the real v3 derivation and recovery
// material; another attempt then revokes the lineage.
func TestRecoveryExactRetryAfterForfeitUsesTheFrozenScope(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	recoveryFamily(t, h, original)
	if _, err := h.Authority.ScheduleDeletion(t.Context(), authority.ScheduleDeletion{CommandID: h.nextID(7), OwnerAccountID: original.Account.ID,
		Principal: h.principal(original)}); err != nil {
		t.Fatal(err)
	}
	signedIn, err := h.Reauthenticate(original)
	if err != nil {
		t.Fatal(err)
	}
	request := recoveryRequest(signedIn)
	if signedIn.DeviceCredential == "" {
		request.DeviceContinuity = recoveryRequest(original).DeviceContinuity // the same device keeps its credential
	}
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil || child.FamilySlot == nil || child.FamilySlot.State != "family_recovery" {
		t.Fatalf("rotation into the recovery scope: %+v %v", child.FamilySlot, err)
	}
	if _, err = h.Authority.ForfeitRecovery(t.Context(), authority.ForfeitRecovery{CommandID: h.nextID(7), AccountID: original.Account.ID,
		Principal: h.principal(child.SessionBundle)}); err != nil {
		t.Fatal(err)
	}
	before := h.authMutationSnapshot()
	retry, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil || retry.AccessToken != child.AccessToken || retry.RefreshToken != child.RefreshToken || retry.RefreshAttemptID != request.AttemptID {
		t.Fatalf("exact retry after forfeit: %v", err)
	}
	if retry.FamilySlot != nil {
		t.Fatalf("recovered slot %+v, want none after forfeit", retry.FamilySlot)
	}
	if h.authMutationSnapshot() != before {
		t.Fatal("exact retry mutated authority")
	}
	other := request
	other.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if _, err = h.Service.RefreshRecoverable(t.Context(), other); Code(err) != "refresh_attempt_conflict" {
		t.Fatalf("another attempt: %v", err)
	}
	if _, err = h.Service.Authenticate(t.Context(), child.AccessToken); Code(err) != "session_revoked" {
		t.Fatalf("child after the conflict: %v", err)
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

// AcknowledgeRefreshToken is Authenticate then AcknowledgeRefresh in one
// transaction: the same outcomes in the same order, the bearer's refusals
// first (an unknown or expired bearer is refused before the attempt ID and
// the device proof are looked at).
func TestRecoveryAcknowledgementByToken(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	bad := request.DeviceContinuity
	bad.Credential = child.RefreshToken
	const zero = "00000000-0000-4000-8000-000000000000"
	unknown := strings.Repeat("A", 43)
	for _, step := range []struct {
		token, attempt string
		claim          identity.DeviceContinuityClaim
		code           string
		receipts       int
	}{
		{unknown, request.AttemptID, bad, "session_revoked", 1},
		{unknown, zero, bad, "session_revoked", 1},
		{"not a token", request.AttemptID, request.DeviceContinuity, "session_revoked", 1},
		{child.AccessToken, zero, bad, "invalid_request", 1},
		{child.AccessToken, request.AttemptID, bad, "device_continuity_invalid", 1},
		{child.AccessToken, request.AttemptID, request.DeviceContinuity, "", 0},
		{child.AccessToken, request.AttemptID, request.DeviceContinuity, "", 0},
		{child.AccessToken, request.AttemptID, bad, "device_continuity_invalid", 0},
	} {
		if err := h.Service.AcknowledgeRefreshToken(t.Context(), step.token, step.attempt, step.claim); Code(err) != step.code && !(step.code == "" && err == nil) {
			t.Fatalf("ack(%.8s, %s): %v, want %q", step.token, step.attempt, err, step.code)
		}
		recoveryReceiptCount(t, h, step.receipts)
	}
	h.Clock.Advance(11 * time.Minute)
	if err = h.Service.AcknowledgeRefreshToken(t.Context(), child.AccessToken, request.AttemptID, bad); Code(err) != "access_expired" {
		t.Fatalf("expired ack: %v", err)
	}
	if err = h.Service.AcknowledgeRefreshToken(t.Context(), child.AccessToken, zero, bad); Code(err) != "access_expired" {
		t.Fatalf("expired zero ack: %v", err)
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
	h.Restart()
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

func recoveryReceiptCount(t *testing.T, h *sessionFixture, want int) {
	t.Helper()
	var count int
	h.query("SELECT count(*) FROM refresh_receipts", nil, &count)
	if count != want {
		t.Fatalf("receipt count=%d want=%d", count, want)
	}
}

func recoveryInventory(t *testing.T, h *sessionFixture) authority.KeyInventory {
	t.Helper()
	keys, err := h.Authority.ReferencedKeyIDs(t.Context(), authority.Mode(h.mode))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// A recovery receipt pins its consumed parent's token key: a lost-ack retry
// presents the parent's refresh token, so the old key stays referenced while
// the receipt recovers an active child.
func TestRecoveryKeyRetentionAcrossRestart(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	next := testKeyring("session.next", 0x69).Current
	old := h.RotateTokenKeys(next)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	h.Restart()
	keys := recoveryInventory(t, h)
	if !reflect.DeepEqual(keys.SessionToken, []string{next.ID, old.ID}) {
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
	if again := recoveryInventory(t, h); reflect.DeepEqual(keys, again) {
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
	recoveryReceiptCount(t, h, 1)
	var remaining string
	h.query("SELECT device_id::text FROM refresh_receipts", nil, &remaining)
	if remaining != second.DeviceID {
		t.Fatalf("fresh proof retired device %s's receipt", remaining)
	}
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

func recoveryFamily(t *testing.T, h *sessionFixture, bundle SessionBundle) string {
	t.Helper()
	principal := h.principal(bundle)
	result, err := h.Authority.CreateFamily(t.Context(), authority.CreateFamily{CommandID: h.nextID(7), OwnerAccountID: principal.AccountID,
		FamilyID: h.nextID(6), DisplayName: "Family", Principal: principal})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := h.Authority.FamilySlot(t.Context(), bundle.Account.ID)
	if err != nil || slot == nil || slot.FamilyID != result.FamilyID {
		t.Fatalf("family creation did not commit: slot=%+v err=%v", slot, err)
	}
	return result.FamilyID
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
	for _, deviceID := range []string{first.DeviceID, second.DeviceID} {
		var receipts int
		h.query(`SELECT count(*) FROM refresh_receipts WHERE account_id=$1 AND device_id=$2 AND attempt_id=$3`, []any{first.Account.ID, deviceID, a.AttemptID}, &receipts)
		if receipts != 1 {
			t.Fatalf("device %s receipts=%d want 1", deviceID, receipts)
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
	want := before
	want.RefreshReceipts -= 2
	if after := h.authMutationSnapshot(); after != want {
		t.Fatalf("ACK changed more than its receipts: before=%+v after=%+v", before, after)
	}
}

// assertRecoveryDeniedWithoutMutation requires the recovery to be refused
// without any authority change. subjectRefused also requires the quota
// subject to be refused (it is refused when the device proof no longer
// matches an active device; scope and entitlement are the rotation's).
func assertRecoveryDeniedWithoutMutation(t *testing.T, h *sessionFixture, input RecoverableRefreshInput, subjectRefused bool) {
	t.Helper()
	before := h.authMutationSnapshot()
	recovered, err := h.Service.RefreshRecoverable(t.Context(), input)
	if err == nil || recovered.AccessToken != "" || recovered.RefreshToken != "" {
		t.Fatalf("revoked authority returned credentials: %v", err)
	}
	if _, err = h.Service.ResolveRecoverableRefreshSubject(t.Context(), input); (err != nil) != subjectRefused {
		t.Fatalf("quota subject err=%v, want refused=%t", err, subjectRefused)
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
	if _, err = h.Service.Logout(t.Context(), h.principal(child.SessionBundle), h.nextID(7)); err != nil {
		t.Fatal(err)
	}
	var status string
	h.query(`SELECT status::text FROM devices WHERE id=$1`, []any{original.DeviceID}, &status)
	if status != "revoked" {
		t.Fatal("binding revocation did not commit")
	}
	assertRecoveryDeniedWithoutMutation(t, h, input, true)
}

func TestRecoveryMembershipRemovalOverridesReceipt(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	owner := h.Issue()
	familyID := recoveryFamily(t, h, owner)
	caregiver, err := h.exchange("caregiver-subject", &identity.ProfileInput{DisplayName: "Caregiver", Confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	joinFamily(t, h, owner, caregiver, familyID)
	input := recoveryRequest(caregiver)
	child, err := h.Service.RefreshRecoverable(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if child.FamilySlot == nil || child.FamilySlot.FamilyID != familyID {
		t.Fatal("child did not inherit authorized family")
	}
	if _, err = h.Authority.RemoveMember(t.Context(), authority.RemoveMember{CommandID: h.nextID(7), OwnerAccountID: owner.Account.ID,
		AccountID: caregiver.Account.ID, Principal: h.principal(owner)}); err != nil {
		t.Fatal(err)
	}
	var status string
	h.query(`SELECT status::text FROM memberships WHERE family_id=$1 AND account_id=$2`, []any{familyID, caregiver.Account.ID}, &status)
	if status != "removed" {
		t.Fatalf("membership removal missing: %s", status)
	}
	assertRecoveryDeniedWithoutMutation(t, h, input, false)
}

// joinFamily makes member a caregiver of the owner's family through an
// invitation.
func joinFamily(t *testing.T, h *sessionFixture, owner, member SessionBundle, familyID string) {
	t.Helper()
	digest := "hmac-sha256:invitation.v1:" + strings.Repeat("a", 64)
	if _, err := h.Authority.CreateInvitation(t.Context(), authority.CreateInvitation{CommandID: h.nextID(7), CreatorAccountID: owner.Account.ID,
		InvitationID: h.nextID(8), FamilyID: familyID, TokenDigest: digest, Role: "caregiver", ExpiresAt: h.Clock.Now().Add(time.Hour),
		Principal: h.principal(owner)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Authority.RedeemInvitation(t.Context(), authority.RedeemInvitation{CommandID: h.nextID(7), RedeemerAccountID: member.Account.ID,
		TokenDigestCandidates: []string{digest}, Principal: h.principal(member)}); err != nil {
		t.Fatal(err)
	}
}

func recoveryEntitlementCommand(t *testing.T, h *sessionFixture, familyID, operation string, expiry time.Time) {
	t.Helper()
	var revision uint64
	h.query(`SELECT audit_seq FROM families WHERE id=$1`, []any{familyID}, &revision)
	reason := "beta_extension"
	if operation == "suspend" {
		reason = "abuse_suspension"
	}
	if _, err := h.Authority.ExecuteOperatorEntitlement(t.Context(), authority.OperatorEntitlement{CommandID: h.nextID(7), OperatorID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		FamilyID: familyID, Operation: operation, Key: "family_sync", Source: "hosted_beta", ExpiresAt: expiry, PolicyID: "Recovery.Test_v1",
		EvidenceDigest: strings.Repeat("e", 64), ReasonCode: reason, ExpectedRevision: revision, ConfirmationExpiresAt: h.Clock.Now().Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryHostedGrantedEntitlementLaterInvalidates(t *testing.T) {
	for _, transition := range []string{"suspended", "expired"} {
		t.Run(transition, func(t *testing.T) {
			h := newHostedSessionFixture(t)
			original := h.Issue()
			familyID := recoveryFamily(t, h, original)
			expiry := h.Clock.Now().Add(time.Hour)
			recoveryEntitlementCommand(t, h, familyID, "grant", expiry)
			var status string
			var expires time.Time
			h.query(`SELECT status::text, expires_at FROM family_entitlements WHERE family_id=$1 AND entitlement_key='family_sync'`, []any{familyID}, &status, &expires)
			if status != "active" || !expires.Equal(expiry) {
				t.Fatalf("valid grant did not commit: %s %s", status, expires)
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
				recoveryEntitlementCommand(t, h, familyID, "suspend", expiry)
			} else {
				h.Clock.Advance(time.Hour)
			}
			var refreshExpires time.Time
			h.query(`SELECT refresh_expires_at FROM sessions WHERE id=$1`, []any{h.sessionByRefreshToken(child.RefreshToken)}, &refreshExpires)
			if !h.Clock.Now().Before(refreshExpires) || !h.Clock.Now().Before(child.ReauthenticateAt) {
				t.Fatal("entitlement transition overlapped credential expiry")
			}
			assertRecoveryDeniedWithoutMutation(t, h, input, false)
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
	parentID := h.sessionByRefreshToken(original.RefreshToken)
	next := input
	next.RefreshToken = child.RefreshToken
	next.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	descendant, err := h.Service.RefreshRecoverable(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	var predecessor int
	h.query(`SELECT count(*) FROM refresh_receipts WHERE parent_id=$1 AND attempt_id=$2`, []any{parentID, input.AttemptID}, &predecessor)
	if predecessor != 0 {
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

// After a recoverable rotation across a token key rotation the inventory
// holds both keys (the receipt pins the consumed parent's); acknowledging
// the receipt releases the old key, which no credential can use any more.
func TestReceiptInventoryCompleteAfterRecoverableRotation(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	next := testKeyring("session.next", 0x69).Current
	old := h.RotateTokenKeys(next)
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		keys := recoveryInventory(t, h)
		if !reflect.DeepEqual(keys.SessionToken, []string{next.ID, old.ID}) || !reflect.DeepEqual(keys.DeviceContinuity, []string{h.deviceKeys.Current.ID}) {
			t.Fatalf("inventory=%+v", keys)
		}
		keys.SessionToken[0] = "tamper"
	}
	if err := h.Service.AcknowledgeRefresh(t.Context(), h.principal(child.SessionBundle), request.AttemptID, request.DeviceContinuity); err != nil {
		t.Fatal(err)
	}
	if keys := recoveryInventory(t, h); !reflect.DeepEqual(keys.SessionToken, []string{next.ID}) {
		t.Fatalf("acknowledged receipt still pins the old key: %+v", keys)
	}
}
