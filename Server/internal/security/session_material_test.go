package security_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"babytracker/server/internal/security"
)

// Removing the projection-material digest from a rotation must make the
// authority event invalid before it can be committed or projected.
func TestSessionRotationRequiresProjectionMaterialDigest(t *testing.T) {
	const (
		account  = "10000000-0000-4000-8000-000000000001"
		device   = "40000000-0000-4000-8000-000000000001"
		root     = "60000000-0000-4000-8000-000000000001"
		next     = "60000000-0000-4000-8000-000000000002"
		instance = "50000000-0000-4000-8000-000000000001"
	)
	prior := security.Head{Generation: 1}
	issued := appendReplayEvent(t, &prior, 1, "identity_session_issued", []security.Effect{
		{Kind: "account_registered", AccountID: account, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "active"},
		{Kind: "device_registered", AccountID: account, DeviceID: device, ServerInstanceID: instance, CanonicalOrigin: "https://sync.example.test", DeviceCredentialDigest: "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: "active"},
		{Kind: "session_granted", AccountID: account, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DeviceID: device, SessionID: root, LineageRootSessionID: root, ServerInstanceID: instance, CanonicalOrigin: "https://sync.example.test", DeviceCredentialDigest: "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ProjectionMaterialDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"},
	})
	rotation := security.Event{
		Version: "v1", Generation: 1, Revision: 2, PreviousHash: hex.EncodeToString(prior.EventHash[:]),
		CommandID: "30000000-0000-4000-8000-000000000002", CommandHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		ActorAccountID: account, ActorDeviceID: device, Kind: "session_rotated", At: time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC).Format(time.RFC3339Nano),
		Effects: []security.Effect{{Kind: "session_rotated", AccountID: account, DeviceID: device, SessionID: next, ParentSessionID: root, LineageRootSessionID: root, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"}},
	}
	raw, err := security.EncodeEvent(rotation)
	if err == nil {
		candidate := security.Candidate{CommandID: rotation.CommandID, Prior: prior, EventJSON: raw, EventHash: sha256.Sum256(raw)}
		_, err = security.Replay([]security.Candidate{issued, candidate})
	}
	if security.Code(err) != "ledger_corrupt" {
		t.Fatalf("rotation without material digest code=%q err=%v", security.Code(err), err)
	}
}
