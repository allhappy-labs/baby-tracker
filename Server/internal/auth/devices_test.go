package auth

import (
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
)

func TestCrossAccountDeviceClaimDenied(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	if _, err := h.exchange("subject-two", &identity.ProfileInput{DisplayName: "Parent Two", Confirmed: true}, nil); err != nil {
		t.Fatal(err)
	}
	before := h.authMutationSnapshot()
	_, err := h.exchange("subject-two", nil, &identity.DeviceContinuityClaim{DeviceID: original.DeviceID, Credential: original.DeviceCredential})
	if Code(err) != "device_continuity_invalid" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	after := h.authMutationSnapshot()
	if after != before {
		t.Fatalf("invalid claim mutated state: before=%+v after=%+v", before, after)
	}
}

func TestRevokedDeviceClaimDenied(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	if _, err := h.db.Owner.Exec(t.Context(), `UPDATE devices SET binding_revoked_at=$2,updated_at=$2 WHERE id=$1`, original.DeviceID, h.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	before := h.authMutationSnapshot()
	_, err := h.Reauthenticate(original)
	if Code(err) != "device_continuity_invalid" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	after := h.authMutationSnapshot()
	if after != before {
		t.Fatalf("revoked claim mutated auth state: %+v -> %+v", before, after)
	}
}

func TestChangedOriginOrInstanceDeviceClaimDenied(t *testing.T) {
	for _, test := range []struct{ name, origin, instance string }{
		{name: "origin", origin: "https://other.example.test", instance: sessionInstance},
		{name: "instance", origin: sessionOrigin, instance: "50000000-0000-4000-8000-000000000092"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newSessionFixture(t, config.ModeSelfHosted)
			original := h.Issue()
			h.restartWith(test.origin, test.instance)
			before := h.authMutationSnapshot()
			_, err := h.exchange("subject-one", nil, &identity.DeviceContinuityClaim{DeviceID: original.DeviceID, Credential: original.DeviceCredential})
			if Code(err) != "device_continuity_invalid" {
				t.Fatalf("code=%s err=%v", Code(err), err)
			}
			after := h.authMutationSnapshot()
			if after != before {
				t.Fatalf("changed binding mutated auth state: %+v -> %+v", before, after)
			}
		})
	}
}

func TestLostReauthResponseCanRetryContinuity(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	h.Clock.Advance(24 * time.Hour)
	h.Restart()
	first, err := h.Reauthenticate(original)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Reauthenticate(original)
	if err != nil {
		t.Fatal(err)
	}
	if first.DeviceID != original.DeviceID || second.DeviceID != original.DeviceID || first.DeviceCredential != "" || second.DeviceCredential != "" {
		t.Fatalf("continuity changed: original=%+v first=%+v second=%+v", original, first, second)
	}
	if first.AccessToken == second.AccessToken || first.RefreshToken == second.RefreshToken {
		t.Fatal("lost-response retry reused the new session")
	}
}

func TestDeviceContinuityPreservesReceiptOwner(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	if _, err := h.db.Owner.Exec(t.Context(), `CREATE TABLE test_device_receipts(
		device_id uuid NOT NULL,mutation_id uuid NOT NULL,result text NOT NULL,PRIMARY KEY(device_id,mutation_id))`); err != nil {
		t.Fatal(err)
	}
	mutationID := h.nextID(9)
	if _, err := h.db.Owner.Exec(t.Context(), `INSERT INTO test_device_receipts(device_id,mutation_id,result) VALUES($1,$2,'applied')`, original.DeviceID, mutationID); err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(24 * time.Hour)
	h.Restart()
	next, err := h.Reauthenticate(original)
	if err != nil {
		t.Fatal(err)
	}
	var result string
	if err := h.db.Owner.QueryRow(t.Context(), `SELECT result FROM test_device_receipts WHERE device_id=$1 AND mutation_id=$2`, next.DeviceID, mutationID).Scan(&result); err != nil || result != "applied" {
		t.Fatalf("receipt result=%q err=%v", result, err)
	}
	if _, err := h.exchange("subject-two", &identity.ProfileInput{DisplayName: "Parent Two", Confirmed: true}, nil); err != nil {
		t.Fatal(err)
	}
	before := h.authMutationSnapshot()
	if _, err := h.exchange("subject-two", nil, &identity.DeviceContinuityClaim{DeviceID: original.DeviceID, Credential: original.DeviceCredential}); Code(err) != "device_continuity_invalid" {
		t.Fatalf("cross-account code=%s err=%v", Code(err), err)
	}
	after := h.authMutationSnapshot()
	if after != before {
		t.Fatal("cross-account claim reached receipt ownership state")
	}
}

type authMutationState struct {
	Head                         [32]byte
	Revision                     uint64
	Devices, Sessions, Materials int
}

func (h *sessionFixture) authMutationSnapshot() authMutationState {
	h.t.Helper()
	head, err := h.store.Heads.Load(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	state := authMutationState{Head: head.EventHash, Revision: head.Revision}
	if err := h.db.Owner.QueryRow(h.t.Context(), `SELECT (SELECT count(*) FROM devices),(SELECT count(*) FROM sessions),
		(SELECT count(*) FROM auth_exchange_material)`).Scan(&state.Devices, &state.Sessions, &state.Materials); err != nil {
		h.t.Fatal(err)
	}
	return state
}
