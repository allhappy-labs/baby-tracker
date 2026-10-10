package pushtoken_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/pushtoken"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func mustDigests(t *testing.T, h *testkit.Harness, subject string) []authority.KeyedDigest {
	t.Helper()
	digests, err := h.Auth.AccessDigests(h.AccessToken(subject))
	if err != nil {
		t.Fatal(err)
	}
	return digests
}

type stored struct {
	found       bool
	token       []byte
	environment string
	account     string
	registered  time.Time
}

// storedToken reads deviceID's row as the table owner (the migrator pool)
// under the authority flag: maintenance holds no privilege on
// device_push_tokens (000031), and the tenant policy would hide the row.
func storedToken(t *testing.T, h *testkit.Harness, deviceID string) stored {
	t.Helper()
	var got stored
	err := pgx.BeginFunc(t.Context(), h.Migrator, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
			return err
		}
		err := tx.QueryRow(t.Context(), `SELECT token, environment::text, account_id::text, registered_at FROM device_push_tokens WHERE device_id=$1`,
			deviceID).Scan(&got.token, &got.environment, &got.account, &got.registered)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		got.found = err == nil
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// tokenRows is the number of rows in device_push_tokens (table owner).
func tokenRows(t *testing.T, h *testkit.Harness) int {
	t.Helper()
	var n int
	err := pgx.BeginFunc(t.Context(), h.Migrator, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
			return err
		}
		return tx.QueryRow(t.Context(), `SELECT count(*) FROM device_push_tokens`).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRegisterBindsTheTokenToTheSessionsOwnDevice(t *testing.T) {
	h := testkit.New(t)
	service := pushtoken.New(h.Authority, func() time.Time { return h.Now })
	owner := mustDigests(t, h, "owner")
	first, second := bytes.Repeat([]byte{0x01}, 32), bytes.Repeat([]byte{0x02}, 32)
	if err := service.Register(t.Context(), authority.Request{Digests: owner}, "sandbox", first); err != nil {
		t.Fatal(err)
	}
	if got := storedToken(t, h, h.Owner.DeviceID); !got.found || !bytes.Equal(got.token, first) || got.environment != "sandbox" || !got.registered.Equal(h.Now) {
		t.Fatalf("stored %+v", got)
	}
	if err := service.Register(t.Context(), authority.Request{Digests: owner}, "sandbox", first); err != nil {
		t.Fatalf("the same token again is not idempotent: %v", err)
	}
	h.Now = h.Now.Add(time.Minute)
	if err := service.Register(t.Context(), authority.Request{Digests: owner}, "production", second); err != nil {
		t.Fatal(err) // a replacement
	}
	if got := storedToken(t, h, h.Owner.DeviceID); !bytes.Equal(got.token, second) || got.environment != "production" || got.account != h.Owner.AccountID ||
		!got.registered.Equal(h.Now) {
		t.Fatalf("stored %+v", got)
	}
	if n := tokenRows(t, h); n != 1 {
		t.Fatalf("%d rows, want one per device", n)
	}
	if err := service.Unregister(t.Context(), authority.Request{Digests: owner}); err != nil {
		t.Fatal(err)
	}
	if got := storedToken(t, h, h.Owner.DeviceID); got.found {
		t.Fatalf("unregistered token kept: %+v", got)
	}
	if err := service.Unregister(t.Context(), authority.Request{Digests: owner}); err != nil {
		t.Fatalf("a second unregister is not idempotent: %v", err)
	}
	viewer := h.AddMember(t, "viewer", "viewer")
	if err := service.Register(t.Context(), authority.Request{Digests: mustDigests(t, h, "viewer")}, "sandbox", first); err != nil {
		t.Fatalf("a viewer's phone needs wakes too: %v", err)
	}
	if got := storedToken(t, h, viewer.DeviceID); !got.found || got.account != viewer.AccountID {
		t.Fatalf("viewer's token %+v", got)
	}
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), viewer.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := service.Register(t.Context(), authority.Request{Digests: mustDigests(t, h, "viewer")}, "sandbox", first); family.Code(err) != "session_revoked" {
		t.Fatalf("a removed member registered: %v", err)
	}
	if err := service.Unregister(t.Context(), authority.Request{Digests: mustDigests(t, h, "viewer")}); family.Code(err) != "session_revoked" {
		t.Fatalf("a removed member unregistered: %v", err)
	}
	if err := service.Register(t.Context(), authority.Request{Digests: mustDigests(t, h, "viewer")}, "staging", nil); family.Code(err) != "session_revoked" {
		t.Fatalf("a removed member's bad token was judged before the session: %v", err)
	}
	for _, bad := range []struct {
		environment string
		token       []byte
	}{{"staging", first}, {"sandbox", first[:15]}, {"production", bytes.Repeat([]byte{1}, 257)}} {
		if err := service.Register(t.Context(), authority.Request{Digests: owner}, bad.environment, bad.token); !errors.Is(err, protocol.ErrInvalidPushToken) {
			t.Fatalf("%q %d bytes: %v", bad.environment, len(bad.token), err)
		}
	}
	if got := storedToken(t, h, h.Owner.DeviceID); got.found {
		t.Fatalf("a refused token was stored: %+v", got)
	}
	if err := service.Register(t.Context(), authority.Request{Digests: mustDigests(t, h, "outsider")}, "sandbox", first); family.Code(err) != "family_unavailable" {
		t.Fatalf("an account with no family registered: %v", err)
	}
}

// The request names no device: one account's request never reaches
// another account's row, whatever it sends.
func TestRegisterNeverReachesAnotherAccountsDevice(t *testing.T) {
	h := testkit.New(t)
	service := pushtoken.New(h.Authority, func() time.Time { return h.Now })
	caregiverToken := bytes.Repeat([]byte{0xc0}, 32)
	if err := service.Register(t.Context(), authority.Request{Digests: mustDigests(t, h, "caregiver")}, "production", caregiverToken); err != nil {
		t.Fatal(err)
	}
	owner := authority.Request{Digests: mustDigests(t, h, "owner")}
	if err := service.Register(t.Context(), owner, "sandbox", bytes.Repeat([]byte{0x0e}, 32)); err != nil {
		t.Fatal(err)
	}
	if err := service.Unregister(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	if got := storedToken(t, h, h.Caregiver.DeviceID); !got.found || !bytes.Equal(got.token, caregiverToken) || got.environment != "production" ||
		got.account != h.Caregiver.AccountID {
		t.Fatalf("another account's token changed: %+v", got)
	}
	// Nor another device of the same account: each device has its own row.
	tablet, tabletAccess, err := h.NewDeviceSession("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	tabletDigests, err := h.Auth.AccessDigests(tabletAccess)
	if err != nil {
		t.Fatal(err)
	}
	tabletToken := bytes.Repeat([]byte{0x7a}, 32)
	if err := service.Register(t.Context(), authority.Request{Digests: tabletDigests}, "sandbox", tabletToken); err != nil {
		t.Fatal(err)
	}
	if err := service.Unregister(t.Context(), authority.Request{Digests: mustDigests(t, h, "caregiver")}); err != nil {
		t.Fatal(err)
	}
	if got := storedToken(t, h, tablet.DeviceID); !got.found || !bytes.Equal(got.token, tabletToken) {
		t.Fatalf("the phone's DELETE removed the tablet's token: %+v", got)
	}
	if got := storedToken(t, h, h.Caregiver.DeviceID); got.found {
		t.Fatalf("the phone's token was kept: %+v", got)
	}
}

// The tenant policy alone scopes the table: inside the owner's request
// transaction an unqualified read sees, and an unqualified delete removes,
// only the owner's rows, and a row for another account's device cannot be
// written.
func TestTheTenantPolicyAloneScopesPushTokens(t *testing.T) {
	h := testkit.New(t)
	service := pushtoken.New(h.Authority, func() time.Time { return h.Now })
	for _, subject := range []string{"owner", "caregiver"} {
		if err := service.Register(t.Context(), authority.Request{Digests: mustDigests(t, h, subject)}, "sandbox", bytes.Repeat([]byte{0x11}, 32)); err != nil {
			t.Fatal(err)
		}
	}
	owner := authority.Request{Digests: mustDigests(t, h, "owner")}
	var visible, deleted int64
	err := h.Authority.WithRequestTx(t.Context(), owner, "read", func(tx pgx.Tx, _ access.Principal, _ access.Grant) error {
		if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM device_push_tokens`).Scan(&visible); err != nil {
			return err
		}
		tag, err := tx.Exec(context.Background(), `DELETE FROM device_push_tokens`)
		deleted = tag.RowsAffected()
		return err
	})
	if err != nil || visible != 1 || deleted != 1 {
		t.Fatalf("visible %d deleted %d: %v", visible, deleted, err)
	}
	if got := storedToken(t, h, h.Caregiver.DeviceID); !got.found {
		t.Fatal("the owner's unqualified delete removed the caregiver's token")
	}
	err = h.Authority.WithRequestTx(t.Context(), owner, "read", func(tx pgx.Tx, _ access.Principal, _ access.Grant) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO device_push_tokens(device_id, account_id, environment, token, registered_at)
			VALUES ($1, $2, 'sandbox', $3, $4) ON CONFLICT (device_id) DO UPDATE SET token = EXCLUDED.token`,
			h.Caregiver.DeviceID, h.Caregiver.AccountID, bytes.Repeat([]byte{0x22}, 32), h.Now)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("the owner wrote the caregiver's row: %v", err)
	}
	if got := storedToken(t, h, h.Caregiver.DeviceID); !bytes.Equal(got.token, bytes.Repeat([]byte{0x11}, 32)) {
		t.Fatalf("caregiver's token changed: %+v", got)
	}
}
