package auth

import (
	"encoding/json"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
)

func recoveryRequest(bundle SessionBundle) RecoverableRefreshInput {
	return RecoverableRefreshInput{RefreshToken: bundle.RefreshToken, AttemptID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", DeviceContinuity: identity.DeviceContinuityClaim{DeviceID: bundle.DeviceID, Credential: bundle.DeviceCredential}}
}

func TestRecoveryExactRetryOneChild(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	first, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.exchange("unrelated-subject", &identity.ProfileInput{DisplayName: "Other", Confirmed: true}, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := h.db.DB.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", original.Account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), "UPDATE accounts SET display_name='Current Parent' WHERE id=$1", original.Account.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(11 * time.Minute)
	before := h.authMutationSnapshot()
	second, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccessToken != second.AccessToken || first.RefreshToken != second.RefreshToken || first.RefreshAttemptID != request.AttemptID || second.RefreshAttemptID != request.AttemptID || !first.ExpiresAt.Equal(second.ExpiresAt) || !first.ReauthenticateAt.Equal(second.ReauthenticateAt) {
		t.Fatal("recovery changed committed output")
	}
	if second.Account.DisplayName != "Current Parent" {
		t.Fatal("recovery returned frozen presentation")
	}
	if h.authMutationSnapshot() != before {
		t.Fatal("exact retry mutated authority")
	}
	if _, err = h.Service.Authenticate(t.Context(), second.AccessToken); Code(err) != "access_expired" {
		t.Fatalf("expired frozen access: %v", err)
	}
	if _, err = h.Service.Refresh(t.Context(), second.RefreshToken); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryConsumedParentQuotaSubject(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	if _, err := h.Service.RefreshRecoverable(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	before := h.authMutationSnapshot()
	for _, attempt := range []string{request.AttemptID, "ffffffff-ffff-4fff-8fff-ffffffffffff"} {
		request.AttemptID = attempt
		subject, err := h.Service.ResolveRecoverableRefreshSubject(t.Context(), request)
		if err != nil || subject.AccountID != original.Account.ID || subject.DeviceID != original.DeviceID {
			t.Fatalf("subject=%+v err=%v", subject, err)
		}
	}
	if h.authMutationSnapshot() != before {
		t.Fatal("quota lookup mutated authority")
	}
}

func TestRecoveryWrongProofNeverRevokes(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.DeviceContinuity.Credential = child.RefreshToken
	request.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	before := h.authMutationSnapshot()
	if _, err = h.Service.RefreshRecoverable(t.Context(), request); Code(err) != "device_continuity_invalid" {
		t.Fatalf("wrong proof: %v", err)
	}
	if _, err = h.Service.ResolveRecoverableRefreshSubject(t.Context(), request); Code(err) != "device_continuity_invalid" {
		t.Fatalf("quota proof: %v", err)
	}
	if h.authMutationSnapshot() != before {
		t.Fatal("wrong proof changed authority")
	}
	if _, err = h.Service.Authenticate(t.Context(), child.AccessToken); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryDifferentAttemptRevokes(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if _, err = h.Service.RefreshRecoverable(t.Context(), request); Code(err) != "refresh_attempt_conflict" {
		t.Fatalf("conflict: %v", err)
	}
	if _, err = h.Service.Authenticate(t.Context(), child.AccessToken); Code(err) != "session_revoked" {
		t.Fatalf("child survived: %v", err)
	}
}

func TestRecoveryMissingReceiptNoReuseRevocation(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if n := h.exec("DELETE FROM refresh_receipts"); n != 1 {
		t.Fatalf("deleted %d receipts", n)
	}
	before := h.authMutationSnapshot()
	if _, err = h.Service.RefreshRecoverable(t.Context(), request); Code(err) != "refresh_receipt_unavailable" {
		t.Fatalf("missing: %v", err)
	}
	if h.authMutationSnapshot() != before {
		t.Fatal("missing receipt changed authority")
	}
	if _, err = h.Service.Authenticate(t.Context(), child.AccessToken); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRemovedReceiptConflictStillChargesAndRevokes(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if n := h.exec("DELETE FROM refresh_receipts"); n != 1 {
		t.Fatalf("deleted %d receipts", n)
	}
	request.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	subject, err := h.Service.ResolveRecoverableRefreshSubject(t.Context(), request)
	if err != nil || subject.AccountID != original.Account.ID {
		t.Fatalf("removed conflict subject: %v", err)
	}
	if _, err = h.Service.RefreshRecoverable(t.Context(), request); Code(err) != "refresh_attempt_conflict" {
		t.Fatalf("conflict: %v", err)
	}
	if _, err = h.Service.Authenticate(t.Context(), child.AccessToken); Code(err) != "session_revoked" {
		t.Fatalf("conflict child: %v", err)
	}
}

func TestRecoveryReceiptTamperingAndCrossAccountFailClosed(t *testing.T) {
	for _, attack := range []string{"canonical material", "child digest", "cross account"} {
		t.Run(attack, func(t *testing.T) {
			h := newSessionFixture(t, config.ModeSelfHosted)
			original := h.Issue()
			request := recoveryRequest(original)
			if _, err := h.Service.RefreshRecoverable(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			parentID := h.sessionByRefreshToken(original.RefreshToken)
			switch attack {
			case "canonical material":
				var material []byte
				h.query(`SELECT material FROM refresh_receipts WHERE parent_id=$1`, []any{parentID}, &material)
				var stage RefreshStage
				if err := json.Unmarshal(material, &stage); err != nil {
					t.Fatal(err)
				}
				frozen := *stage.Recovery
				stage.Recovery = nil
				frozen.CreatedAt = frozen.CreatedAt.Add(-time.Hour)
				changed, digest, err := EncodeRecoveryRefreshStage(stage, frozen)
				if err != nil {
					t.Fatal(err)
				}
				if n := h.exec("UPDATE refresh_receipts SET material=$1, material_digest=$2 WHERE parent_id=$3", changed, digest[:], parentID); n != 1 {
					t.Fatalf("tampered %d receipts", n)
				}
			case "child digest":
				if n := h.exec("UPDATE sessions SET access_digest=decode(repeat('ab',32),'hex') WHERE parent_id=$1", parentID); n != 1 {
					t.Fatalf("tampered %d children", n)
				}
			case "cross account":
				other, err := h.exchange("another-subject", &identity.ProfileInput{DisplayName: "Another", Confirmed: true}, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.DeviceContinuity = identity.DeviceContinuityClaim{DeviceID: other.DeviceID, Credential: other.DeviceCredential}
			}
			before := h.authMutationSnapshot()
			if result, err := h.Service.RefreshRecoverable(t.Context(), request); err == nil || result.RefreshToken != "" {
				t.Fatalf("attack returned credentials: %v", err)
			}
			// The quota subject is the token's account and device once the
			// device proof matches; a tampered receipt is refused by the
			// rotation, not by quota charging.
			if _, err := h.Service.ResolveRecoverableRefreshSubject(t.Context(), request); (err == nil) == (attack == "cross account") {
				t.Fatalf("quota subject err=%v", err)
			}
			if h.authMutationSnapshot() != before {
				t.Fatal("attack revoked authority")
			}
		})
	}
}
