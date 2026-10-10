package authority_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
)

// An exact retry finds the stored result even when every precondition a
// handler would check afterwards (account active, chain healthy) now fails,
// and the retry writes nothing.
func TestReceiptExactRetryReturnsStoredResultBeforeChecks(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	actor := f.Account(t)
	family := f.Family(t, actor)
	commandID := authoritytest.NewID(t)
	payload := map[string]any{"family_id": family, "display_name": "Ours", "count": 2}
	hash, err := authority.CanonicalCommandHash(actor, authority.KindFamilyCreated, payload)
	if err != nil {
		t.Fatal(err)
	}

	var stored authority.Result
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		head := lockAccount(t, tx, actor)
		if _, found, err := authority.LookupReceipt(ctx, tx, actor, commandID, hash); err != nil || found {
			t.Fatalf("first lookup found=%v err=%v", found, err)
		}
		seq, _, err := authority.AppendAudit(ctx, tx, head, authority.KindFamilyCreated, commandID, actor, f.Now, payload)
		if err != nil {
			t.Fatal(err)
		}
		stored = authority.Result{CommandID: commandID, Outcome: "committed", Code: "ok", Revision: seq}
		if err := authority.WriteReceipt(ctx, tx, actor, commandID, hash, authority.KindFamilyCreated, stored, []string{family}, f.Now); err != nil {
			t.Fatal(err)
		}
	})

	// Every later check would now refuse the command.
	f.Exec(t, `UPDATE accounts SET status='deleted', chain_failed=true WHERE id=$1`, actor)

	// The same payload with its keys in another order is the same command.
	retryPayload := struct {
		Count       int    `json:"count"`
		DisplayName string `json:"display_name"`
		FamilyID    string `json:"family_id"`
	}{2, "Ours", family}
	retryHash, err := authority.CanonicalCommandHash(actor, authority.KindFamilyCreated, retryPayload)
	if err != nil || retryHash != hash {
		t.Fatalf("retry hash %x err=%v, want %x", retryHash, err, hash)
	}
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		if _, err := authority.LockAccounts(ctx, tx, actor); err != nil {
			t.Fatal(err)
		}
		result, found, err := authority.LookupReceipt(ctx, tx, actor, commandID, retryHash)
		if err != nil || !found {
			t.Fatalf("retry lookup found=%v err=%v", found, err)
		}
		if result != stored {
			t.Fatalf("retry result %+v, want %+v", result, stored)
		}
	})
	if rows := readAudit(t, f, authority.ScopeAccount, actor); len(rows) != 1 {
		t.Fatalf("retry wrote audit rows: %d", len(rows))
	}
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		var count int
		var kind string
		var families []string
		if err := tx.QueryRow(ctx, `SELECT count(*) OVER (), kind::text, family_ids::text[] FROM command_receipts WHERE actor_account_id=$1`, actor).Scan(&count, &kind, &families); err != nil {
			t.Fatal(err)
		}
		if count != 1 || kind != "family_created" || len(families) != 1 || families[0] != family {
			t.Fatalf("receipt rows=%d kind=%s families=%v", count, kind, families)
		}
	})
}

func TestReceiptHashMismatchIsCommandIDReused(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	actor, other := f.Account(t), f.Account(t)
	commandID := authoritytest.NewID(t)
	original, err := authority.CanonicalCommandHash(actor, authority.KindInvitationCreated, map[string]string{"role": "caregiver"})
	if err != nil {
		t.Fatal(err)
	}
	result := authority.Result{CommandID: commandID, Outcome: "committed", Code: "ok", Revision: 1}
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		// No affected families: stored as the empty array.
		if err := authority.WriteReceipt(ctx, tx, actor, commandID, original, authority.KindInvitationCreated, result, nil, f.Now); err != nil {
			t.Fatal(err)
		}
	})

	differentPayload, _ := authority.CanonicalCommandHash(actor, authority.KindInvitationCreated, map[string]string{"role": "viewer"})
	differentKind, _ := authority.CanonicalCommandHash(actor, authority.KindInvitationRevoked, map[string]string{"role": "caregiver"})
	for name, hash := range map[string][32]byte{"different payload": differentPayload, "different kind": differentKind} {
		if hash == original {
			t.Fatalf("%s hashes like the original", name)
		}
		inAuthorityTx(t, f, func(tx pgx.Tx) {
			got, found, err := authority.LookupReceipt(ctx, tx, actor, commandID, hash)
			if !errors.Is(err, authority.ErrCommandIDReused) || authority.CodeOf(err) != "command_id_reused" || found || got != (authority.Result{}) {
				t.Fatalf("%s: result=%+v found=%v err=%v", name, got, found, err)
			}
		})
	}

	// Receipts are per actor: another account may use the same command ID.
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		if _, found, err := authority.LookupReceipt(ctx, tx, other, commandID, differentPayload); err != nil || found {
			t.Fatalf("other actor found=%v err=%v", found, err)
		}
	})

	// A second receipt with a different hash under the same actor and command
	// ID is refused as command_id_reused.
	tx, err := f.Service.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := authority.WriteReceipt(ctx, tx, actor, commandID, differentPayload, authority.KindInvitationCreated, result, nil, f.Now); !errors.Is(err, authority.ErrCommandIDReused) {
		t.Fatalf("duplicate receipt err=%v, want command_id_reused", err)
	}
}

// Two identical first attempts race: the handler order (lock the actor's
// account, look up, write) makes the loser wait and then find the winner's
// receipt. One audit row, one receipt, equal results.
func TestReceiptConcurrentIdenticalFirstAttemptsAgree(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	actor := f.Account(t)
	commandID := authoritytest.NewID(t)
	payload := map[string]string{"display_name": "Ours"}
	hash, err := authority.CanonicalCommandHash(actor, authority.KindFamilyProfileRepaired, payload)
	if err != nil {
		t.Fatal(err)
	}
	results := make([]authority.Result, 2)
	found := make([]bool, 2)
	errs := runWriters(t, f, 2, func(i int, tx pgx.Tx) error {
		head := lockAccount(t, tx, actor)
		stored, ok, err := authority.LookupReceipt(ctx, tx, actor, commandID, hash)
		if err != nil || ok {
			results[i], found[i] = stored, ok
			return err
		}
		seq, _, err := authority.AppendAudit(ctx, tx, head, authority.KindFamilyProfileRepaired, commandID, actor, f.Now, payload)
		if err != nil {
			return err
		}
		results[i] = authority.Result{CommandID: commandID, Outcome: "committed", Code: "ok", Revision: seq}
		return authority.WriteReceipt(ctx, tx, actor, commandID, hash, authority.KindFamilyProfileRepaired, results[i], nil, f.Now)
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if found[0] == found[1] {
		t.Fatalf("found=%v: want exactly one writer and one replay", found)
	}
	if results[0] != results[1] || results[0].Revision != 1 {
		t.Fatalf("results differ: %+v vs %+v", results[0], results[1])
	}
	if rows := readAudit(t, f, authority.ScopeAccount, actor); len(rows) != 1 {
		t.Fatalf("audit rows %d, want 1", len(rows))
	}
}

// A caller that skips the actor lock and loses the race gets a deterministic,
// documented error from writeReceipt, never a raw unique violation:
// security_busy for the identical command, command_id_reused otherwise.
func TestReceiptWriteRaceWithoutActorLockIsMapped(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	actor := f.Account(t)
	commandID := authoritytest.NewID(t)
	hash, _ := authority.CanonicalCommandHash(actor, authority.KindInvitationRevoked, map[string]string{"id": "a"})
	other, _ := authority.CanonicalCommandHash(actor, authority.KindInvitationRevoked, map[string]string{"id": "b"})
	result := authority.Result{CommandID: commandID, Outcome: "committed", Code: "ok", Revision: 1}

	begin := func() pgx.Tx {
		tx, err := f.Service.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, found, err := authority.LookupReceipt(ctx, tx, actor, commandID, hash); err != nil || found {
			t.Fatalf("lookup found=%v err=%v", found, err)
		}
		return tx
	}
	winner, identical, different := begin(), begin(), begin()
	if err := authority.WriteReceipt(ctx, winner, actor, commandID, hash, authority.KindInvitationRevoked, result, nil, f.Now); err != nil {
		t.Fatal(err)
	}
	// The loser's insert waits on the uncommitted winner, then resolves.
	blocked := make(chan error, 1)
	go func() {
		blocked <- authority.WriteReceipt(ctx, identical, actor, commandID, hash, authority.KindInvitationRevoked, result, nil, f.Now)
	}()
	select {
	case err := <-blocked:
		t.Fatalf("loser did not wait for the winner: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := winner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-blocked; !errors.Is(err, authority.ErrSecurityBusy) || authority.CodeOf(err) != "security_busy" {
		t.Fatalf("identical loser err=%v code=%q", err, authority.CodeOf(err))
	}
	if err := authority.WriteReceipt(ctx, different, actor, commandID, other, authority.KindInvitationRevoked, result, nil, f.Now); !errors.Is(err, authority.ErrCommandIDReused) {
		t.Fatalf("different loser err=%v", err)
	}
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		if stored, found, err := authority.LookupReceipt(ctx, tx, actor, commandID, hash); err != nil || !found || stored != result {
			t.Fatalf("retry after lost race: %+v found=%v err=%v", stored, found, err)
		}
	})
}

func TestCanonicalCommandHashIsPinned(t *testing.T) {
	actor := "11111111-1111-4111-8111-111111111111"
	got, err := authority.CanonicalCommandHash(actor, authority.KindFamilyCreated, map[string]any{"b": "<x>", "a": 1})
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(`{"version":"v2","kind":"family_created","actor_account_id":"` + actor + `","payload":{"a":1,"b":"<x>"}}`))
	if got != want {
		t.Fatalf("hash %x, want %x", got, want)
	}
	for name, run := range map[string]func() error{
		"actor": func() error {
			_, err := authority.CanonicalCommandHash("ACTOR", authority.KindFamilyCreated, nil)
			return err
		},
		"kind": func() error { _, err := authority.CanonicalCommandHash(actor, authority.Kind("nope"), nil); return err },
		"payload": func() error {
			_, err := authority.CanonicalCommandHash(actor, authority.KindFamilyCreated, 0.5)
			return err
		},
		"marshal": func() error {
			_, err := authority.CanonicalCommandHash(actor, authority.KindFamilyCreated, make(chan int))
			return err
		},
	} {
		if err := run(); !errors.Is(err, authority.ErrInvalidCommand) {
			t.Errorf("%s: err=%v, want invalid_command", name, err)
		}
	}
}
