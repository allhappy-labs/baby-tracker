package auth

import (
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"
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
	tx, err := h.db.Owner.Begin(t.Context())
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
	if _, err = h.db.Owner.Exec(t.Context(), "DELETE FROM refresh_recovery_receipts"); err != nil {
		t.Fatal(err)
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
	if _, err = h.db.Owner.Exec(t.Context(), "DELETE FROM refresh_recovery_receipts"); err != nil {
		t.Fatal(err)
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
			child, err := h.Service.RefreshRecoverable(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			parent, _, err := h.Service.lookupRefreshAcrossKeys(t.Context(), original.RefreshToken)
			if err != nil {
				t.Fatal(err)
			}
			switch attack {
			case "canonical material":
				receipt, found, err := h.Service.repository.LookupRecoveryReceipt(t.Context(), parent.row.SessionID, request.AttemptID)
				if err != nil || !found {
					t.Fatal(err)
				}
				var stage RefreshStage
				if err = json.Unmarshal(receipt.Material, &stage); err != nil {
					t.Fatal(err)
				}
				material := *stage.Recovery
				stage.Recovery = nil
				material.CreatedAt = material.CreatedAt.Add(-time.Hour)
				changed, _, err := EncodeRecoveryRefreshStage(stage, material)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(changed)
				if _, err = h.db.Owner.Exec(t.Context(), "UPDATE refresh_recovery_receipts SET material_bytes=$1,projection_material_digest=$2 WHERE parent_session_id=$3", changed, digest[:], parent.row.SessionID); err != nil {
					t.Fatal(err)
				}
			case "child digest":
				if _, err = h.db.Owner.Exec(t.Context(), "UPDATE sessions SET access_digest=decode(repeat('ab',32),'hex') WHERE parent_session_id=$1", parent.row.SessionID); err != nil {
					t.Fatal(err)
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
			if _, err = h.Service.ResolveRecoverableRefreshSubject(t.Context(), request); err == nil {
				t.Fatal("tampered quota subject accepted")
			}
			if h.authMutationSnapshot() != before {
				t.Fatal("attack revoked authority")
			}
			_ = child
		})
	}
}

func TestRecoveryRepairsCommittedProjectionAfterLostReply(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	request := recoveryRequest(original)
	h.FailProjectionOnce()
	if _, err := h.Service.RefreshRecoverable(t.Context(), request); Code(err) != "service_unavailable" {
		t.Fatalf("injected projection: %v", err)
	}
	head, err := h.store.Heads.Load(t.Context())
	if err != nil || head.Revision != 2 {
		t.Fatalf("rotation did not commit: %+v %v", head, err)
	}
	recoveryReceiptCount(t, h, 0)
	h.RestartAndReconcile()
	child, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	recoveryReceiptCount(t, h, 1)
	after, err := h.store.Heads.Load(t.Context())
	if err != nil || after != head {
		t.Fatalf("repair re-rotated: %+v %v", after, err)
	}
	if _, err = h.Service.Authenticate(t.Context(), child.AccessToken); err != nil {
		t.Fatal(err)
	}
	again, err := h.Service.RefreshRecoverable(t.Context(), request)
	if err != nil || again.RefreshToken != child.RefreshToken {
		t.Fatalf("repair unstable: %v", err)
	}
}
