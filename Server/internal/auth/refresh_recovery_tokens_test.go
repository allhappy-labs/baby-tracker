package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func recoveryKnownAnswerInput(t *testing.T) RecoveryDerivationInput {
	t.Helper()
	input := RecoveryDerivationInput{RefreshDerivationInput: refreshKnownAnswerInput(t),
		AttemptID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", CanonicalOrigin: "https://sync.example.com", ContinuityKeyID: "device1"}
	copy(input.ContinuityDigest[:], mustHex(t, "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f"))
	return input
}

func TestRecoveryV3FrozenVectors(t *testing.T) {
	input := recoveryKnownAnswerInput(t)
	first, err := DeriveRecoverableRefreshRotation(input)
	if err != nil {
		t.Fatal(err)
	}
	want := RefreshDerivation{CommandID: "f6b15239-2980-8dae-b3a5-ced8c84dc516", SessionID: "52e84ff9-ca9f-8dfe-a017-5224877b805a", AccessToken: "IaNyJk5vXjEU5CXAxhsrUoOUzBxcCd9NfONNR7x6g24", RefreshToken: "cEtOHaI61kN-78MYcTlUuBLlBIl7pG7vsQAAZewFhSA"}
	if first != want {
		t.Fatalf("v3 rotation=%+v", first)
	}
	frozen := FreezeRecoveryInput(input)
	replayed, err := RestoreRecoveryInput(frozen, input.ParentKey)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveRecoverableRefreshRotation(replayed)
	if err != nil || second != want {
		t.Fatalf("frozen replay=%+v err=%v", second, err)
	}
}

func TestRecoveryV3BindsAttemptOriginAndProof(t *testing.T) {
	base := recoveryKnownAnswerInput(t)
	want, err := DeriveRecoverableRefreshRotation(base)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*RecoveryDerivationInput){
		"attempt":           func(i *RecoveryDerivationInput) { i.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff" },
		"origin":            func(i *RecoveryDerivationInput) { i.CanonicalOrigin = "https://other.example.com" },
		"continuity key":    func(i *RecoveryDerivationInput) { i.ContinuityKeyID = "device2" },
		"continuity digest": func(i *RecoveryDerivationInput) { i.ContinuityDigest[0]++ },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			altered := base
			change(&altered)
			got, err := DeriveRecoverableRefreshRotation(altered)
			if err != nil {
				t.Fatal(err)
			}
			if got.CommandID == want.CommandID || got.SessionID == want.SessionID || got.AccessToken == want.AccessToken || got.RefreshToken == want.RefreshToken {
				t.Fatalf("field did not bind every output: %+v", got)
			}
		})
	}
}

func TestRecoveryV3RejectsMalformedInputs(t *testing.T) {
	base := recoveryKnownAnswerInput(t)
	cases := map[string]func(*RecoveryDerivationInput){
		"zero attempt":           func(i *RecoveryDerivationInput) { i.AttemptID = "00000000-0000-4000-8000-000000000000" },
		"mixed case attempt":     func(i *RecoveryDerivationInput) { i.AttemptID = "EEEEEEEE-eeee-4eee-8eee-eeeeeeeeeeee" },
		"invalid origin":         func(i *RecoveryDerivationInput) { i.CanonicalOrigin = "https://sync.example.com/path" },
		"missing continuity key": func(i *RecoveryDerivationInput) { i.ContinuityKeyID = "" },
		"zero continuity digest": func(i *RecoveryDerivationInput) { i.ContinuityDigest = [32]byte{} },
		"invalid parent key":     func(i *RecoveryDerivationInput) { i.ParentKey.ID = "BAD" },
		"invalid output key":     func(i *RecoveryDerivationInput) { i.OutputKeyID = "BAD" },
		"invalid deadlines":      func(i *RecoveryDerivationInput) { i.AccessExpiresAt = i.RefreshExpiresAt.Add(time.Second) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			altered := base
			change(&altered)
			if _, err := DeriveRecoverableRefreshRotation(altered); err == nil {
				t.Fatal("accepted malformed recovery input")
			}
		})
	}
	frozen := FreezeRecoveryInput(base)
	wrongKey := base.ParentKey
	wrongKey.ID = "other"
	if _, err := RestoreRecoveryInput(frozen, wrongKey); err == nil {
		t.Fatal("restored with wrong key")
	}
}

func TestRecoveryStageCanonicalBytes(t *testing.T) {
	input := recoveryKnownAnswerInput(t)
	derived, err := DeriveRecoverableRefreshRotation(input)
	if err != nil {
		t.Fatal(err)
	}
	common := validRefreshStage().StageCommon
	common.Kind = "refresh"
	common.TokenKeyID = input.OutputKeyID
	common.LineageRootSessionID = input.LineageRootSessionID
	common.AccessExpiresAt = input.AccessExpiresAt.Format(time.RFC3339Nano)
	common.RefreshExpiresAt = input.RefreshExpiresAt.Format(time.RFC3339Nano)
	common.ReauthenticateAt = input.ReauthenticateAt.Format(time.RFC3339Nano)
	common.AbsoluteExpiresAt = input.AbsoluteExpiresAt.Format(time.RFC3339Nano)
	common.SessionID = derived.SessionID
	common.CanonicalOrigin = input.CanonicalOrigin
	stage := RefreshStage{StageCommon: common, ParentSessionID: input.ParentSessionID}
	material := RecoveryMaterial{Input: FreezeRecoveryInput(input), CreatedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), EffectiveDeadline: input.AbsoluteExpiresAt}
	first, digest, err := EncodeRecoveryRefreshStage(stage, material)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := EncodeRecoveryRefreshStage(stage, material)
	if err != nil || !bytes.Equal(first, second) || digest != secondDigest || digest != sha256.Sum256(first) {
		t.Fatalf("unstable canonical stage: %v", err)
	}
	if bytes.Contains(first, input.ParentKey.Secret[:]) || strings.Contains(string(first), derived.AccessToken) || strings.Contains(string(first), derived.RefreshToken) {
		t.Fatal("stage exposes raw secret")
	}
	if !strings.Contains(string(first), `"derivation_version":"v3"`) {
		t.Fatal("missing frozen v3 version")
	}
	if got := hex.EncodeToString(digest[:]); got != "b30fb01ab4e073b75e64924ac3315f765063420cc2929cfa9c4510b809a8d705" {
		t.Fatalf("canonical stage digest=%s", got)
	}
	other := material
	other.Input.ContinuityDigest[0]++
	_, changed, err := EncodeRecoveryRefreshStage(stage, other)
	if err != nil || changed == digest {
		t.Fatalf("recovery binding did not change digest: %v", err)
	}
	invalid := material
	invalid.Input.ParentSessionID = "invalid"
	if _, _, err := EncodeRecoveryRefreshStage(stage, invalid); err == nil {
		t.Fatal("accepted malformed frozen parent")
	}
}

func TestRecoveryFrozenFamilyDoesNotTrackMutableInput(t *testing.T) {
	input := recoveryKnownAnswerInput(t)
	family := "99999999-9999-4999-8999-999999999999"
	input.Scope, input.FamilyID = "family", &family
	frozen := FreezeRecoveryInput(input)
	family = "88888888-8888-4888-8888-888888888888"
	if frozen.FamilyID == nil || *frozen.FamilyID != "99999999-9999-4999-8999-999999999999" {
		t.Fatal("frozen family changed after input mutation")
	}
}

func TestRecoveryStageRejectsLegacyEncoderBypass(t *testing.T) {
	common := validRefreshStage().StageCommon
	common.Kind = "refresh"
	stage := RefreshStage{StageCommon: common, ParentSessionID: "11111111-2222-4333-8444-555555555555",
		Recovery: &RecoveryMaterial{}}
	if _, _, err := EncodeRefreshStage(stage); err == nil {
		t.Fatal("legacy encoder accepted unvalidated recovery material")
	}
}

func TestRecoveryRestoredFamilyDoesNotMutateFrozenInput(t *testing.T) {
	input := recoveryKnownAnswerInput(t)
	family := "99999999-9999-4999-8999-999999999999"
	input.Scope, input.FamilyID = "family", &family
	frozen := FreezeRecoveryInput(input)
	restored, err := RestoreRecoveryInput(frozen, input.ParentKey)
	if err != nil {
		t.Fatal(err)
	}
	*restored.FamilyID = "88888888-8888-4888-8888-888888888888"
	if *frozen.FamilyID != "99999999-9999-4999-8999-999999999999" {
		t.Fatal("restored family mutation changed frozen input")
	}
}
