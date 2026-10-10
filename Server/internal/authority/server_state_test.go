package authority_test

import (
	"encoding/hex"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
)

// The same known-answer vector as the legacy recovery sidecar
// (recovery/actor_key_test.go TestActorIDCommitmentKnownAnswer): a key that
// verified there verifies against server_state, which the homelab's
// commitment relies on: Phase 4's importer carried it across.
func TestActorKeyCommitmentKnownAnswer(t *testing.T) {
	decoded, err := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil || len(decoded) != 32 {
		t.Fatalf("decode actor key: %v", err)
	}
	var key [32]byte
	copy(key[:], decoded)
	commitment := authority.ActorKeyCommitment(key, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if got := hex.EncodeToString(commitment[:]); got != "1e288d423b480e3c0006a5488a0bd9861c076921a22ba1290771a54076381c4d" {
		t.Fatalf("commitment=%s", got)
	}
}

// A rotation empties device_sync_cursors in its transaction: every cursor
// recorded names the old epoch's deliveries, and the wake worker rebuilds the
// rows from the devices' next requests. A refused rotation leaves them.
func TestRotateEpochEmptiesDeviceCursors(t *testing.T) {
	f := authoritytest.New(t)
	family := f.Family(t, f.Account(t))
	cursors := func() int {
		t.Helper()
		var n int
		if err := f.DB.Maintenance.QueryRow(t.Context(), `SELECT count(*) FROM device_sync_cursors`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	seed := func() {
		t.Helper()
		if _, err := f.DB.Maintenance.Exec(t.Context(), `INSERT INTO device_sync_cursors(family_id,device_id,acked_sequence,delivered_sequence,seen_at)
			VALUES ($1,$2,3,3,$3)`, family, authoritytest.NewID(t), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	seed()
	if cursors() != 1 {
		t.Fatal("seeded cursor not visible to the maintenance pool")
	}
	rotated, err := authority.RotateRecoveryEpoch(t.Context(), f.DB.Owner, f.InstanceID, authoritytest.RecoveryEpoch)
	if err != nil || rotated.RecoveryEpoch != authoritytest.RecoveryEpoch+1 {
		t.Fatalf("rotate: %+v %v", rotated, err)
	}
	if n := cursors(); n != 0 {
		t.Fatalf("%d cursor rows after the rotation", n)
	}
	seed()
	if _, err := authority.RotateRecoveryEpoch(t.Context(), f.DB.Owner, f.InstanceID, authoritytest.RecoveryEpoch); err == nil {
		t.Fatal("a stale rotation was accepted")
	}
	if _, err := authority.RotateRecoveryEpoch(t.Context(), f.DB.Owner, authoritytest.NewID(t), authoritytest.RecoveryEpoch+1); err == nil {
		t.Fatal("a rotation of another instance was accepted")
	}
	if n := cursors(); n != 1 {
		t.Fatalf("%d cursor rows after refused rotations, want the 1 seeded", n)
	}
}
