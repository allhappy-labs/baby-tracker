package family_test

import (
	"context"
	"testing"
	"time"

	"babytracker/server/internal/account"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/family"
	"babytracker/server/internal/testkit"
)

// raceDeletion runs a command of the caregiver's against the caregiver's
// account deletion, the deletion first, deterministically: a transaction
// holding the caregiver's family row stops the deletion after its account
// lock; racer starts once the deletion waits there and must then wait on
// the account itself; the family lock is released and the deletion commits
// first. It returns racer's error.
func raceDeletion(t *testing.T, h *testkit.Harness, racer func(context.Context) error) error {
	t.Helper()
	ctx := t.Context()
	var database string
	if err := h.Migrator.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	waiting := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var count int
			if err := h.Admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock'`, database).
				Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d lock waits, want %d", what, count, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	holder, err := h.Migrator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.WithoutCancel(ctx))
	if _, err := holder.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM families WHERE id=$1 FOR NO KEY UPDATE`, h.FamilyID); err != nil {
		t.Fatal(err)
	}

	deleted := make(chan error, 1)
	go func() {
		_, err := (&account.Service{Authority: h.Authority}).Delete(ctx, h.Caregiver, h.CommandID(), true)
		deleted <- err
	}()
	waiting(1, "the deletion waits on the family after locking the account")
	raced := make(chan error, 1)
	go func() { raced <- racer(ctx) }()
	waiting(2, "the racing command waits on the account")
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete: %v", err)
	}
	return <-raced
}

// accountRow is the account's display name and status, read as their owner.
func accountRow(t *testing.T, h *testkit.Harness, accountID string) (name, status string) {
	t.Helper()
	ctx := t.Context()
	read, err := h.Migrator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(context.WithoutCancel(ctx))
	if _, err := read.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	if err := read.QueryRow(ctx, `SELECT display_name, status::text FROM accounts WHERE id=$1`, accountID).Scan(&name, &status); err != nil {
		t.Fatal(err)
	}
	return name, status
}

// A profile update that passed its session check while the account's
// deletion held the account row is refused as session_revoked once the
// deletion commits, as a request arriving after it would be: it never
// reaches the profile write and answers service_unavailable (found by
// TestRandomFamilyAndAccountCommandsNeverDeadlock, seed 7).
func TestProfileUpdateRacingAccountDeletionIsSessionRevoked(t *testing.T) {
	h := testkit.New(t)
	err := raceDeletion(t, h, func(ctx context.Context) error {
		_, err := h.Family.UpdateProfile(ctx, h.Caregiver, h.CommandID(), "Renamed", true)
		return err
	})
	if family.Code(err) != "session_revoked" {
		t.Fatalf("profile update after the deletion committed: code %q, err %v; want session_revoked", family.Code(err), err)
	}
	if name, status := accountRow(t, h, h.Caregiver.AccountID); name != "" || status != "deleted" {
		t.Fatalf("account after both: name %q status %q, want the deletion's", name, status)
	}
}

// A sign-in that resolved the subject's live account before the account's
// deletion committed is refused as security_busy (a retryable lost race),
// not invalid_command (a server fault on the wire), and the subject's next
// sign-in registers the next generation (found by
// TestRandomFamilyAndAccountCommandsNeverDeadlock, seeds 16, 32 and 42).
func TestSignInRacingAccountDeletionIsSecurityBusy(t *testing.T) {
	h := testkit.New(t)
	err := raceDeletion(t, h, func(ctx context.Context) error {
		_, err := h.SignInDevice(ctx, "caregiver")
		return err
	})
	if auth.Code(err) != "security_busy" {
		t.Fatalf("sign-in after the deletion committed: code %q, err %v; want security_busy", auth.Code(err), err)
	}
	if _, status := accountRow(t, h, h.Caregiver.AccountID); status != "deleted" {
		t.Fatalf("account after both: status %q, want deleted", status)
	}
	retry, err := h.SignInDevice(t.Context(), "caregiver")
	if err != nil {
		t.Fatalf("sign-in again: %v", err)
	}
	if retry.Account.ID == "" || retry.Account.ID == h.Caregiver.AccountID {
		t.Fatalf("sign-in again: account %q, want a new generation's account", retry.Account.ID)
	}
}
