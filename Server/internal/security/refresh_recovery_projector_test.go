package security_test

import (
	"babytracker/server/internal/auth"
	"babytracker/server/internal/security"
	"babytracker/server/internal/testpg"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

// A stage cannot be used as proof that a child was committed.
func TestRecoveryStageIsNotReceipt(t *testing.T) {
	fixture, _, _, raw, digest := recoveryProjectionSetup(t)
	stageMaterial(t, fixture, recoveryCommand, raw, digest, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
	repository, err := auth.NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := repository.LookupRecoveryReceipt(t.Context(), identitySession, recoveryAttempt); err != nil || found {
		t.Fatalf("stage became receipt: found=%t err=%v", found, err)
	}
}

const recoveryAttempt = "70000000-0000-4000-8000-000000000001"
const recoveryChild = "60000000-0000-4000-8000-000000000002"
const recoveryCommand = "30000000-0000-4000-8000-000000000022"

func recoveryProjectionSetup(t *testing.T) (*testpg.Fixture, security.Candidate, security.Candidate, []byte, [32]byte) {
	t.Helper()
	fixture := testpg.New(t)
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	issueStage := validExchangeStage(at, identitySession)
	issueBytes, issueDigest := marshalStage(t, issueStage)
	stageMaterial(t, fixture, "30000000-0000-4000-8000-000000000021", issueBytes, issueDigest, at)
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000021", identitySession, issueDigest, at)
	if err := security.NewProjector(fixture.Projector).Apply(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	stage := auth.RefreshStage{StageCommon: auth.StageCommon{Version: "v1", Kind: "refresh", AccountID: identityAccount, DeviceID: identityDevice, SessionID: recoveryChild, LineageRootSessionID: identitySession, ServerInstanceID: identityInstance, CanonicalOrigin: "https://sync.example.test", TokenKeyID: "session.v1", AccessDigest: repeatHex("33"), RefreshDigest: repeatHex("44"), RecoveryEpoch: 1, SecurityRevision: 2, Scope: "account", AccessExpiresAt: at.Add(20 * time.Minute).Format(time.RFC3339Nano), RefreshExpiresAt: at.Add(2 * time.Hour).Format(time.RFC3339Nano), ReauthenticateAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), AbsoluteExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano)}, ParentSessionID: identitySession}
	parentBytes, _ := hex.DecodeString(issueStage.RefreshDigest)
	var parentDigest, continuityDigest [32]byte
	copy(parentDigest[:], parentBytes)
	copy(continuityDigest[:], bytes.Repeat([]byte{0xbb}, 32))
	frozen := auth.FrozenRecoveryInput{DerivationVersion: "v3", ServerInstanceID: identityInstance, AccountID: identityAccount, DeviceID: identityDevice, ParentSessionID: identitySession, LineageRootSessionID: identitySession, RecoveryEpoch: 1, ParentKeyID: issueStage.TokenKeyID, ParentDigest: parentDigest, OutputKeyID: "session.v1", Scope: "account", AccessExpiresAt: at.Add(20 * time.Minute), RefreshExpiresAt: at.Add(2 * time.Hour), ReauthenticateAt: at.Add(24 * time.Hour), AbsoluteExpiresAt: at.Add(24 * time.Hour), HeadGeneration: 1, HeadRevision: 1, HeadHash: issue.EventHash, TargetRevision: 2, AttemptID: recoveryAttempt, CanonicalOrigin: stage.CanonicalOrigin, ContinuityKeyID: "device.v1", ContinuityDigest: continuityDigest}
	raw, digest, err := auth.EncodeRecoveryRefreshStage(stage, auth.RecoveryMaterial{Input: frozen, CreatedAt: at.Add(time.Minute), EffectiveDeadline: at.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash}
	rotate := projectionEventCandidate(t, head, recoveryCommand, "session_rotated", []security.Effect{{Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: recoveryChild, ParentSessionID: identitySession, LineageRootSessionID: identitySession, ProjectionMaterialDigest: hex.EncodeToString(digest[:]), RefreshAttemptID: recoveryAttempt, RecoveryEpoch: 1, ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active"}}, at.Add(time.Minute))
	return fixture, issue, rotate, raw, digest
}

// Receipt insertion failure must roll back both parent consumption and child creation.
func TestRecoveryProjectionAtomicReceipt(t *testing.T) {
	fixture, issue, rotate, raw, digest := recoveryProjectionSetup(t)
	stageMaterial(t, fixture, recoveryCommand, raw, digest, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
	if _, err := fixture.Owner.Exec(t.Context(), `ALTER TABLE public.refresh_recovery_receipts ADD CONSTRAINT injected_failure CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), rotate); err == nil {
		t.Fatal("injected receipt failure was ignored")
	}
	var children, consumed, receipts int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions WHERE parent_session_id=$1),(SELECT count(*) FROM sessions WHERE session_id=$1 AND status='consumed'),(SELECT count(*) FROM refresh_recovery_receipts)`, identitySession).Scan(&children, &consumed, &receipts); err != nil {
		t.Fatal(err)
	}
	if children != 0 || consumed != 0 || receipts != 0 {
		t.Fatalf("partial rotation: children=%d consumed=%d receipts=%d", children, consumed, receipts)
	}
	if _, err := fixture.Owner.Exec(t.Context(), `ALTER TABLE public.refresh_recovery_receipts DROP CONSTRAINT injected_failure`); err != nil {
		t.Fatal(err)
	}
	if err := projector.Apply(t.Context(), rotate); err != nil {
		t.Fatal(err)
	}
	if err := projector.Apply(t.Context(), rotate); err != nil {
		t.Fatal(err)
	}
	assertRecoveryReceipt(t, fixture, raw, digest)
	// Retirement removes secret derivation material, never the authority attempt binding.
	if _, err := fixture.Owner.Exec(t.Context(), `DELETE FROM public.refresh_recovery_receipts`); err != nil {
		t.Fatal(err)
	}
	authority, err := security.Replay([]security.Candidate{issue, rotate})
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := authority.Session(identitySession)
	child, _ := authority.Session(recoveryChild)
	if parent.ChildSessionID != recoveryChild || parent.RotationAttemptID != recoveryAttempt || child.RefreshAttemptID != recoveryAttempt || child.ProjectionMaterialDigest != hex.EncodeToString(digest[:]) {
		t.Fatal("retirement lost authority binding")
	}
}

func assertRecoveryReceipt(t *testing.T, fixture *testpg.Fixture, want []byte, digest [32]byte) {
	t.Helper()
	repository, err := auth.NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := repository.LookupRecoveryReceipt(t.Context(), identitySession, recoveryAttempt)
	if err != nil || !found {
		t.Fatalf("receipt lookup found=%t err=%v", found, err)
	}
	if !bytes.Equal(receipt.Material, want) || receipt.ProjectionMaterialDigest != digest || receipt.ChildSessionID != recoveryChild || receipt.ParentSessionID != identitySession || receipt.AttemptID != recoveryAttempt {
		t.Fatal("receipt differs from committed material")
	}
	if _, found, err := repository.LookupRecoveryReceipt(t.Context(), identitySession, "70000000-0000-4000-8000-000000000002"); err != nil || found {
		t.Fatalf("wrong attempt receipt found=%t err=%v", found, err)
	}

	var children, consumed, receipts, cleared int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM sessions WHERE parent_session_id=$1),(SELECT count(*) FROM sessions WHERE session_id=$1 AND status='consumed'),(SELECT count(*) FROM refresh_recovery_receipts),(SELECT count(*) FROM auth_exchange_material WHERE command_id=$2 AND projected_at IS NOT NULL AND token_key_id IS NULL AND recovery_material_bytes IS NULL)`, identitySession, recoveryCommand).Scan(&children, &consumed, &receipts, &cleared); err != nil {
		t.Fatal(err)
	}
	if children != 1 || consumed != 1 || receipts != 1 || cleared != 1 {
		t.Fatalf("children=%d consumed=%d receipts=%d cleared=%d", children, consumed, receipts, cleared)
	}
}

func TestRecoveryProjectionRepair(t *testing.T) {
	fixture, issue, rotate, raw, digest := recoveryProjectionSetup(t)
	// Committed authority survives absent projection; a missing stage fails closed.
	if _, err := security.Replay([]security.Candidate{issue, rotate}); err != nil {
		t.Fatal(err)
	}
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), rotate); err == nil {
		t.Fatal("recovery projected without receipt material")
	}
	stageMaterial(t, fixture, recoveryCommand, raw, digest, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
	var count int
	if err := fixture.Runtime.QueryRow(t.Context(), `SELECT count(*) FROM app.lookup_refresh_recovery_receipt($1::uuid,$2::uuid)`, identitySession, recoveryAttempt).Scan(&count); err != nil || count != 0 {
		t.Fatalf("staged receipt count=%d err=%v", count, err)
	}
	if err := projector.Apply(t.Context(), rotate); err != nil {
		t.Fatal(err)
	}
	assertRecoveryReceipt(t, fixture, raw, digest)
}

func TestRecoveryStageRejectsNoncanonicalMaterial(t *testing.T) {
	fixture, _, _, raw, _ := recoveryProjectionSetup(t)
	for _, mutate := range []func([]byte) []byte{
		func(b []byte) []byte { return append(b, ' ') },
		func(b []byte) []byte {
			return bytes.ReplaceAll(b, []byte("2026-09-20T12:20:00Z"), []byte("2026-09-20T12:20:00.0Z"))
		},
		func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"attempt_id":"`+recoveryAttempt+`"`), []byte(`"attempt_id":"00000000-0000-0000-0000-000000000000"`), 1)
		},
		func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"derivation_version":"v3"`), []byte(`"derivation_version":"v3","unknown":true`), 1)
		},
		func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"target_revision":2`), []byte(`"target_revision":3`), 1)
		},
	} {
		changed := mutate(bytes.Clone(raw))
		hash := sha256.Sum256(changed)
		if _, err := fixture.Runtime.Exec(t.Context(), `SELECT app.stage_auth_material($1,$2,$3,$4,$5)`, recoveryCommand, bytes.Repeat([]byte{0xaa}, 32), hash[:], changed, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC)); err == nil {
			t.Fatal("invalid recovery material accepted")
		}
	}
	var decoded auth.RefreshStage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryProjectionRejectsAlteredStagedColumns(t *testing.T) {
	fixture, _, rotate, raw, digest := recoveryProjectionSetup(t)
	stageMaterial(t, fixture, recoveryCommand, raw, digest, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
	if _, err := fixture.Owner.Exec(t.Context(), `UPDATE auth_exchange_material SET access_digest=decode(repeat('ee',32),'hex') WHERE command_id=$1`, recoveryCommand); err != nil {
		t.Fatal(err)
	}
	if err := security.NewProjector(fixture.Projector).Apply(t.Context(), rotate); err == nil {
		t.Fatal("altered child token material projected")
	}
	var count int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE parent_session_id=$1`, identitySession).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial projection count=%d err=%v", count, err)
	}
}

// The frozen bytes, not mutable staging columns, bind the authority child.
func TestRecoveryProjectionRejectsAlteredChildBinding(t *testing.T) {
	fixture, _, rotate, raw, digest := recoveryProjectionSetup(t)
	stageMaterial(t, fixture, recoveryCommand, raw, digest, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
	const other = "60000000-0000-4000-8000-000000000003"
	if _, err := fixture.Owner.Exec(t.Context(), `UPDATE auth_exchange_material SET session_id=$1 WHERE command_id=$2`, other, recoveryCommand); err != nil {
		t.Fatal(err)
	}
	var event security.Event
	if err := json.Unmarshal(rotate.EventJSON, &event); err != nil {
		t.Fatal(err)
	}
	event.Effects[0].SessionID = other
	var err error
	rotate.EventJSON, err = security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	rotate.EventHash = sha256.Sum256(rotate.EventJSON)
	if err := security.NewProjector(fixture.Projector).Apply(t.Context(), rotate); err == nil {
		t.Fatal("frozen child binding was ignored")
	}
}
