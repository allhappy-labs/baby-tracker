package security_test

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/security"
)

// Losing the authority commitment makes mutable receipt rows unverifiable.
func TestRecoveryAuthorityRetainsMaterialDigest(t *testing.T) {
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	_, digest := marshalStage(t, validExchangeStage(at, identitySession))
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000021", identitySession, digest, at)
	state, err := security.Replay([]security.Candidate{issue})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := state.Session(identitySession)
	if !ok {
		t.Fatal("missing granted authority")
	}
	if session.ProjectionMaterialDigest != hex.EncodeToString(digest[:]) {
		t.Fatal("authority lost committed material digest")
	}
}

func TestRecoveryLegacyEffectBytesUnchanged(t *testing.T) {
	effect := security.Effect{Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: "60000000-0000-4000-8000-000000000002", ParentSessionID: identitySession, LineageRootSessionID: identitySession, ProjectionMaterialDigest: repeatHex("cc"), Status: "active", ExpiresAt: "2026-09-21T12:00:00Z", RecoveryEpoch: 1}
	raw, err := json.Marshal(effect)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"kind":"session_rotated","account_id":"10000000-0000-4000-8000-000000000001","device_id":"40000000-0000-4000-8000-000000000001","session_id":"60000000-0000-4000-8000-000000000002","parent_session_id":"60000000-0000-4000-8000-000000000001","lineage_root_session_id":"60000000-0000-4000-8000-000000000001","projection_material_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","status":"active","expires_at":"2026-09-21T12:00:00Z","recovery_epoch":1}`
	if string(raw) != want {
		t.Fatalf("legacy effect bytes changed: %s", raw)
	}
}

// Removing outgoing binding or overwriting incoming binding breaks exact retry
// classification after receipt retirement, especially across multiple rotations.
func TestRecoveryAuthorityAttemptBinding(t *testing.T) {
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	_, digest := marshalStage(t, validExchangeStage(at, identitySession))
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000021", identitySession, digest, at)
	candidates := []security.Candidate{issue}
	parent := identitySession
	ids := []string{"60000000-0000-4000-8000-000000000002", "60000000-0000-4000-8000-000000000003"}
	attempts := []string{"70000000-0000-4000-8000-000000000001", "70000000-0000-4000-8000-000000000002"}
	head := security.Head{Generation: 1, Revision: 1, EventHash: issue.EventHash}
	for i, child := range ids {
		effect := security.Effect{Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: child, ParentSessionID: parent, LineageRootSessionID: identitySession, ProjectionMaterialDigest: repeatHex("dd"), RecoveryEpoch: 1, ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active"}
		effect.RefreshAttemptID = attempts[i]
		command := []string{"30000000-0000-4000-8000-000000000022", "30000000-0000-4000-8000-000000000023"}[i]
		candidate := projectionEventCandidate(t, head, command, "session_rotated", []security.Effect{effect}, at.Add(time.Duration(i+1)*time.Minute))
		candidates = append(candidates, candidate)
		head = security.Head{Generation: 1, Revision: uint64(i + 2), EventHash: candidate.EventHash}
		parent = child
	}
	state, err := security.Replay(candidates)
	if err != nil {
		t.Fatal(err)
	}
	middle, _ := state.Session(ids[0])
	if middle.RefreshAttemptID != attempts[0] || middle.RotationAttemptID != attempts[1] || middle.ChildSessionID != ids[1] || middle.ProjectionMaterialDigest != repeatHex("dd") {
		t.Fatalf("incorrect middle authority: %+v", middle)
	}
	middle.ProjectionMaterialDigest = "tampered"
	again, _ := state.Session(ids[0])
	if again.ProjectionMaterialDigest != repeatHex("dd") {
		t.Fatal("session view mutated authority")
	}
}

func TestRecoveryAttemptOnlyOnRotation(t *testing.T) {
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	_, digest := marshalStage(t, validExchangeStage(at, identitySession))
	issue := identityIssueCandidate(t, security.Head{Generation: 1}, "30000000-0000-4000-8000-000000000021", identitySession, digest, at)
	var event security.Event
	if err := json.Unmarshal(issue.EventJSON, &event); err != nil {
		t.Fatal(err)
	}
	event.Effects[0].RefreshAttemptID = "70000000-0000-4000-8000-000000000001"
	if _, err := security.EncodeEvent(event); err == nil {
		t.Fatal("attempt accepted on nonrotation event")
	}
	event.Kind = "session_rotated"
	event.Effects = []security.Effect{{Kind: "session_rotated", AccountID: identityAccount, DeviceID: identityDevice, SessionID: "60000000-0000-4000-8000-000000000002", ParentSessionID: identitySession, LineageRootSessionID: identitySession, ProjectionMaterialDigest: repeatHex("dd"), RecoveryEpoch: 1, ExpiresAt: at.Add(24 * time.Hour).Format(time.RFC3339Nano), Status: "active"}}
	for _, attempt := range []string{"00000000-0000-0000-0000-000000000000", "bad", strings.ToUpper("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")} {
		event.Effects[0].RefreshAttemptID = attempt
		if _, err := security.EncodeEvent(event); err == nil {
			t.Fatalf("invalid attempt accepted: %s", attempt)
		}
	}
}
