package authority_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
)

// The audit row hash is pinned byte for byte against the definition the
// verifier and the homelab's imported chains rely on:
// sha256(scope 0 scope_id 0 seq(8 BE) prev_hash kind 0 command_id 0 actor_id 0 at(RFC3339Nano, UTC) 0 payload).
func TestAuditHashIsDeterministicAndChains(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	account := f.Account(t)
	first, second := authoritytest.NewID(t), authoritytest.NewID(t)
	at1 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	at2 := time.Date(2026, 10, 4, 12, 0, 1, 123456789, time.UTC) // stored at microsecond precision
	payload1 := map[string]any{"z": "<b>& ", "a": 1, "n": map[string]any{"y": true, "x": nil}, "l": []any{"b", "a"}}
	// Strings and integers that jsonb re-renders differently from Go: the
	// recomputation from the stored jsonb below must still match.
	payload2 := struct {
		SessionID string `json:"session_id"`
		Revision  uint64 `json:"revision"`
		Big       uint64 `json:"big"`
		Note      string `json:"note"`
	}{SessionID: first, Revision: 9007199254740991, Big: math.MaxUint64, Note: "line\n\t\"q\"\\ <é> 😀 \x01 \u2029"}

	var seq1, seq2 uint64
	var hash1, hash2 [32]byte
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		head := lockAccount(t, tx, account)
		var err error
		if seq1, hash1, err = authority.AppendAudit(ctx, tx, head, authority.KindSessionGranted, first, account, at1, payload1); err != nil {
			t.Fatal(err)
		}
	})
	inAuthorityTx(t, f, func(tx pgx.Tx) {
		head := lockAccount(t, tx, account)
		if head.Seq != 1 || head.Hash != hash1 {
			t.Fatalf("locked head (%d,%x), want (1,%x)", head.Seq, head.Hash, hash1)
		}
		var err error
		if seq2, hash2, err = authority.AppendAudit(ctx, tx, head, authority.KindSessionRevoked, second, account, at2, payload2); err != nil {
			t.Fatal(err)
		}
		if head.Seq != 2 || head.Hash != hash2 {
			t.Fatalf("head not advanced: (%d,%x)", head.Seq, head.Hash)
		}
	})
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seqs = %d,%d, want 1,2", seq1, seq2)
	}

	// Known answer, built independently of the implementation.
	canonical1 := []byte(`{"a":1,"l":["b","a"],"n":{"x":null,"y":true},"z":"<b>&` + "\\u2028" + `"}`) // U+2028 stays escaped
	var be [8]byte
	binary.BigEndian.PutUint64(be[:], 1)
	var preimage []byte
	preimage = append(preimage, "account\x00"+account+"\x00"...)
	preimage = append(preimage, be[:]...)
	preimage = append(preimage, make([]byte, 32)...)
	preimage = append(preimage, "session_granted\x00"+first+"\x00"+account+"\x00"+"2026-10-04T10:00:00Z\x00"...)
	preimage = append(preimage, canonical1...)
	if want := sha256.Sum256(preimage); hash1 != want {
		t.Fatalf("row 1 hash %x, want known answer %x", hash1, want)
	}
	if got, err := authority.CanonicalJSON(payload1); err != nil || string(got) != string(canonical1) {
		t.Fatalf("canonical payload %s err=%v, want %s", got, err, canonical1)
	}

	rows := readAudit(t, f, authority.ScopeAccount, account)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Seq != 1 || rows[1].Seq != 2 {
		t.Fatalf("stored seqs %d,%d", rows[0].Seq, rows[1].Seq)
	}
	if rows[0].PrevHash != ([32]byte{}) {
		t.Fatalf("genesis prev_hash %x, want zeros", rows[0].PrevHash)
	}
	if rows[1].PrevHash != rows[0].Hash {
		t.Fatalf("row 2 prev_hash %x != row 1 hash %x", rows[1].PrevHash, rows[0].Hash)
	}
	if rows[0].Hash != hash1 || rows[1].Hash != hash2 {
		t.Fatal("stored hashes differ from the returned hashes")
	}
	for _, row := range rows {
		if got := recompute(t, row); got != row.Hash {
			t.Fatalf("seq %d recomputed %x, stored %x", row.Seq, got, row.Hash)
		}
		if row.ActorID != account || row.Scope != "account" || row.ScopeID != account {
			t.Fatalf("row identity %+v", row)
		}
	}
	if !rows[1].At.Equal(at2.Truncate(time.Microsecond)) {
		t.Fatalf("stored at %s, want %s", rows[1].At, at2.Truncate(time.Microsecond))
	}
	if rows[0].Kind != "session_granted" || rows[1].Kind != "session_revoked" || rows[0].CommandID != first || rows[1].CommandID != second {
		t.Fatalf("kinds/commands %+v", rows)
	}
	if seq, head := chainHead(t, f, authority.ScopeAccount, account); seq != 2 || head != hash2 {
		t.Fatalf("account head (%d,%x), want (2,%x)", seq, head, hash2)
	}

	t.Run("family scope chains on the family row", func(t *testing.T) {
		family := f.Family(t, account)
		var seq uint64
		var hash [32]byte
		inAuthorityTx(t, f, func(tx pgx.Tx) {
			lockAccount(t, tx, account)
			families, err := authority.LockFamilies(ctx, tx, family)
			if err != nil {
				t.Fatal(err)
			}
			if seq, hash, err = authority.AppendAudit(ctx, tx, families[family].Chain, authority.KindFamilyCreated, authoritytest.NewID(t), account, f.Now, map[string]string{"family_id": family}); err != nil {
				t.Fatal(err)
			}
		})
		rows := readAudit(t, f, authority.ScopeFamily, family)
		if seq != 1 || len(rows) != 1 || rows[0].Hash != hash || recompute(t, rows[0]) != hash {
			t.Fatalf("family chain seq=%d rows=%+v", seq, rows)
		}
		if headSeq, head := chainHead(t, f, authority.ScopeFamily, family); headSeq != 1 || head != hash {
			t.Fatalf("family head (%d,%x)", headSeq, head)
		}
		if accountSeq, _ := chainHead(t, f, authority.ScopeAccount, account); accountSeq != 2 {
			t.Fatalf("family append moved the account chain to %d", accountSeq)
		}
	})

	t.Run("several appends in one transaction chain from the advanced head", func(t *testing.T) {
		other := f.Account(t)
		var hashes [3][32]byte
		inAuthorityTx(t, f, func(tx pgx.Tx) {
			head := lockAccount(t, tx, other)
			for i := range hashes {
				seq, hash, err := authority.AppendAudit(ctx, tx, head, authority.KindSessionGranted, authoritytest.NewID(t), other, f.Now, map[string]int{"i": i})
				if err != nil || seq != uint64(i+1) {
					t.Fatalf("append %d: seq=%d err=%v", i, seq, err)
				}
				hashes[i] = hash
			}
		})
		rows := readAudit(t, f, authority.ScopeAccount, other)
		assertContiguous(t, rows, 3)
		for i, row := range rows {
			if row.Hash != hashes[i] || recompute(t, row) != row.Hash {
				t.Fatalf("row %d hash mismatch", i)
			}
		}
	})

	t.Run("a failed chain refuses appends", func(t *testing.T) {
		failed := f.Account(t)
		f.Exec(t, `UPDATE accounts SET chain_failed=true WHERE id=$1`, failed)
		tx, err := f.Service.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		head := lockAccount(t, tx, failed)
		if !head.Failed {
			t.Fatal("locked head does not report the failed chain")
		}
		_, _, err = authority.AppendAudit(ctx, tx, head, authority.KindSessionGranted, authoritytest.NewID(t), failed, f.Now, map[string]string{})
		if !errors.Is(err, authority.ErrScopeFailed) || authority.CodeOf(err) != "scope_failed" {
			t.Fatalf("append to failed chain err=%v code=%q", err, authority.CodeOf(err))
		}
		// A head read before the chain failed is refused by the guard too.
		stale := authority.NewChainHead(authority.ScopeAccount, failed, 0, [32]byte{}, false)
		_, _, err = authority.AppendAudit(ctx, tx, stale, authority.KindSessionGranted, authoritytest.NewID(t), failed, f.Now, map[string]string{})
		if !errors.Is(err, authority.ErrScopeFailed) {
			t.Fatalf("stale head on failed chain err=%v", err)
		}
		if rows := readAudit(t, f, authority.ScopeAccount, failed); len(rows) != 0 {
			t.Fatalf("failed chain gained rows: %d", len(rows))
		}
	})

	t.Run("malformed appends are invalid commands", func(t *testing.T) {
		head := func(scope authority.Scope, id string) *authority.LockedChainHead {
			seq, hash := chainHead(t, f, authority.ScopeAccount, account)
			return authority.NewChainHead(scope, id, seq, hash, false)
		}
		cases := map[string]func(tx pgx.Tx) error{
			"nil head": func(tx pgx.Tx) error {
				_, _, err := authority.AppendAudit(ctx, tx, nil, authority.KindSessionGranted, authoritytest.NewID(t), account, f.Now, map[string]string{})
				return err
			},
			"unknown kind": func(tx pgx.Tx) error {
				_, _, err := authority.AppendAudit(ctx, tx, head(authority.ScopeAccount, account), authority.Kind("bogus"), authoritytest.NewID(t), account, f.Now, map[string]string{})
				return err
			},
			"unknown scope": func(tx pgx.Tx) error {
				_, _, err := authority.AppendAudit(ctx, tx, head(authority.Scope("device"), account), authority.KindSessionGranted, authoritytest.NewID(t), account, f.Now, map[string]string{})
				return err
			},
			"non-canonical id": func(tx pgx.Tx) error {
				_, _, err := authority.AppendAudit(ctx, tx, head(authority.ScopeAccount, upper(account)), authority.KindSessionGranted, authoritytest.NewID(t), account, f.Now, map[string]string{})
				return err
			},
			"fractional number": func(tx pgx.Tx) error {
				_, _, err := authority.AppendAudit(ctx, tx, head(authority.ScopeAccount, account), authority.KindSessionGranted, authoritytest.NewID(t), account, f.Now, map[string]float64{"x": 1.5})
				return err
			},
			"NUL in payload": func(tx pgx.Tx) error {
				_, _, err := authority.AppendAudit(ctx, tx, head(authority.ScopeAccount, account), authority.KindSessionGranted, authoritytest.NewID(t), account, f.Now, map[string]string{"x": "a\x00b"})
				return err
			},
			"zero time": func(tx pgx.Tx) error {
				_, _, err := authority.AppendAudit(ctx, tx, head(authority.ScopeAccount, account), authority.KindSessionGranted, authoritytest.NewID(t), account, time.Time{}, map[string]string{})
				return err
			},
		}
		for name, run := range cases {
			tx, err := f.Service.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := run(tx); !errors.Is(err, authority.ErrInvalidCommand) {
				t.Errorf("%s: err=%v, want invalid_command", name, err)
			}
			_ = tx.Rollback(ctx)
		}
		if seq, _ := chainHead(t, f, authority.ScopeAccount, account); seq != 2 {
			t.Fatalf("malformed appends moved the chain to %d", seq)
		}
	})
}

// Writers that lock the scope first serialize on the row lock: 20 concurrent
// transactions produce seq 1..20 exactly once, chained.
func TestAppendAuditFromLockedHeadSerializes(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	account := f.Account(t)
	const writers = 20
	errs := runWriters(t, f, writers, func(i int, tx pgx.Tx) error {
		rows, err := authority.LockAccounts(ctx, tx, account)
		if err != nil {
			return err
		}
		// Distinct timestamps so a duplicate seq would show up as a row
		// rather than as a primary-key collision.
		_, _, err = authority.AppendAudit(ctx, tx, rows[account].Chain, authority.KindSessionRotated, authoritytest.NewID(t), account,
			f.Now.Add(time.Duration(i)*time.Microsecond), map[string]int{"writer": i})
		return err
	})
	for _, err := range errs {
		if err != nil {
			t.Errorf("writer: %v", err)
		}
	}
	rows := readAudit(t, f, authority.ScopeAccount, account)
	assertContiguous(t, rows, writers)
	for _, row := range rows {
		if recompute(t, row) != row.Hash {
			t.Fatalf("seq %d hash does not recompute", row.Seq)
		}
	}
	if seq, head := chainHead(t, f, authority.ScopeAccount, account); seq != writers || head != rows[len(rows)-1].Hash {
		t.Fatalf("head (%d,%x), want (%d,last row)", seq, head, writers)
	}
}

// Writers that skip the lock all read the same head; exactly one append wins
// and every other one fails closed (service_unavailable) without writing a
// duplicate seq or forking the chain.
func TestAppendAuditStaleHeadFailsClosed(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	account := f.Account(t)
	const writers = 20
	seq, hash := chainHead(t, f, authority.ScopeAccount, account)
	errs := runWriters(t, f, writers, func(i int, tx pgx.Tx) error {
		head := authority.NewChainHead(authority.ScopeAccount, account, seq, hash, false) // read without a lock
		_, _, err := authority.AppendAudit(ctx, tx, head, authority.KindSessionRotated, authoritytest.NewID(t), account,
			f.Now.Add(time.Duration(i)*time.Microsecond), map[string]int{"writer": i})
		return err
	})
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, authority.ErrUnavailable) && authority.CodeOf(err) == "service_unavailable":
		default:
			t.Errorf("stale writer: %v (code %q)", err, authority.CodeOf(err))
		}
	}
	if won != 1 {
		t.Fatalf("%d stale-head writers won, want exactly 1", won)
	}
	rows := readAudit(t, f, authority.ScopeAccount, account)
	assertContiguous(t, rows, 1)
	if headSeq, head := chainHead(t, f, authority.ScopeAccount, account); headSeq != 1 || head != rows[0].Hash {
		t.Fatalf("head (%d,%x) does not match the single row", headSeq, head)
	}
}

// runWriters begins n authority transactions, then runs write in all of them
// at once and commits each that succeeded; it returns each writer's error.
func runWriters(t *testing.T, f *authoritytest.Fixture, n int, write func(i int, tx pgx.Tx) error) []error {
	t.Helper()
	ctx := t.Context()
	errs := make([]error, n)
	var begun, done sync.WaitGroup
	begun.Add(n)
	start := make(chan struct{})
	for i := range n {
		done.Add(1)
		go func() {
			defer done.Done()
			tx, err := f.Service.Begin(ctx)
			begun.Done()
			if err != nil {
				errs[i] = err
				return
			}
			defer tx.Rollback(context.Background())
			<-start
			if errs[i] = write(i, tx); errs[i] == nil {
				errs[i] = tx.Commit(ctx)
			}
		}()
	}
	begun.Wait()
	close(start)
	done.Wait()
	return errs
}

func lockAccount(t *testing.T, tx pgx.Tx, id string) *authority.LockedChainHead {
	t.Helper()
	rows, err := authority.LockAccounts(t.Context(), tx, id)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows[id]
	if !ok {
		t.Fatalf("account %s not locked", id)
	}
	return row.Chain
}

func upper(id string) string {
	out := []byte(id)
	for i, c := range out {
		if c >= 'a' && c <= 'f' {
			out[i] = c - 'a' + 'A'
		}
	}
	return string(out)
}
