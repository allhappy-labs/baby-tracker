package authority_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/care"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5/pgconn"
)

// Property: 200 concurrent commands, each locking a random subset of 5
// accounts and then a random subset of 3 families (subsets passed in random
// order) and appending to every locked scope, never deadlock and never lose
// an append.
func TestLockOrderingNeverDeadlocks(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	accounts := make([]string, 5)
	for i := range accounts {
		accounts[i] = f.Account(t)
	}
	families := make([]string, 3)
	for i := range families {
		families[i] = f.Family(t, accounts[i])
	}
	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	type command struct{ accounts, families []string }
	const commands = 200
	plan := make([]command, commands)
	touched := map[string]uint64{}
	for i := range plan {
		pick := func(ids []string, min int) []string {
			var out []string
			for len(out) < min {
				out = out[:0]
				for _, id := range ids {
					if rng.IntN(2) == 0 {
						out = append(out, id)
					}
				}
			}
			rng.Shuffle(len(out), func(a, b int) { out[a], out[b] = out[b], out[a] })
			return out
		}
		plan[i] = command{accounts: pick(accounts, 1), families: pick(families, 0)}
		for _, id := range plan[i].accounts {
			touched[id]++
		}
		for _, id := range plan[i].families {
			touched[id]++
		}
	}

	start := make(chan struct{})
	errs := make(chan error, commands)
	var done sync.WaitGroup
	for i, cmd := range plan {
		done.Add(1)
		go func() {
			defer done.Done()
			<-start
			errs <- runLockedCommand(ctx, f, i, cmd.accounts, cmd.families)
		}()
	}
	close(start)
	done.Wait()
	close(errs)
	deadlocks, failures := 0, 0
	for err := range errs {
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
		case errors.As(err, &pgErr) && pgErr.Code == "40P01":
			deadlocks++
		default:
			failures++
			t.Errorf("command: %v", err)
		}
	}
	if deadlocks > 0 || failures > 0 {
		t.Fatalf("%d deadlocks (40P01), %d other failures out of %d commands", deadlocks, failures, commands)
	}
	for _, id := range accounts {
		if seq, _ := chainHead(t, f, authority.ScopeAccount, id); seq != touched[id] {
			t.Errorf("account %s audit_seq %d, want %d", id, seq, touched[id])
		}
		assertContiguous(t, readAudit(t, f, authority.ScopeAccount, id), touched[id])
	}
	for _, id := range families {
		if seq, _ := chainHead(t, f, authority.ScopeFamily, id); seq != touched[id] {
			t.Errorf("family %s audit_seq %d, want %d", id, seq, touched[id])
		}
		assertContiguous(t, readAudit(t, f, authority.ScopeFamily, id), touched[id])
	}
}

func runLockedCommand(ctx context.Context, f *authoritytest.Fixture, i int, accounts, families []string) error {
	tx, err := f.Service.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	lockedAccounts, err := authority.LockAccounts(ctx, tx, accounts...)
	if err != nil {
		return err
	}
	lockedFamilies, err := authority.LockFamilies(ctx, tx, families...)
	if err != nil {
		return err
	}
	if len(lockedAccounts) != len(accounts) || len(lockedFamilies) != len(families) {
		return errors.New("locked rows missing")
	}
	actor := accounts[0]
	at := f.Now.Add(time.Duration(i) * time.Millisecond)
	for _, id := range accounts {
		if _, _, err := authority.AppendAudit(ctx, tx, lockedAccounts[id].Chain, authority.KindMemberRoleChanged, newCommandID(i, 1), actor, at, map[string]int{"command": i}); err != nil {
			return err
		}
	}
	for _, id := range families {
		if _, _, err := authority.AppendAudit(ctx, tx, lockedFamilies[id].Chain, authority.KindMemberRoleChanged, newCommandID(i, 2), actor, at, map[string]int{"command": i}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func newCommandID(i, part int) string {
	const hex = "0123456789abcdef"
	id := []byte("70000000-0000-4000-8000-000000000000")
	n := i*4 + part
	for pos := len(id) - 1; n > 0; pos-- {
		id[pos] = hex[n%16]
		n /= 16
	}
	return string(id)
}

func assertContiguous(t *testing.T, rows []auditRow, want uint64) {
	t.Helper()
	if uint64(len(rows)) != want {
		t.Errorf("audit rows %d, want %d", len(rows), want)
		return
	}
	var prev [32]byte
	for i, row := range rows {
		if row.Seq != uint64(i+1) || row.PrevHash != prev {
			t.Errorf("row %d: seq %d breaks the chain", i, row.Seq)
			return
		}
		prev = row.Hash
	}
}

func TestLockAccountsAndFamiliesReturnLockedRows(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	a, b := f.Account(t), f.Account(t)
	family := f.Family(t, a)
	missing := authoritytest.NewID(t)

	tx, err := f.Service.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	rows, err := authority.LockAccounts(ctx, tx, b, a, missing, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows %v, want exactly %s and %s", rows, a, b)
	}
	if _, ok := rows[missing]; ok {
		t.Fatal("missing account present in the map")
	}
	got := rows[a]
	if got.ID != a || got.Status != "active" || got.LifecycleGeneration != 1 || got.Chain == nil || got.Chain.Seq != 0 || got.Chain.Hash != ([32]byte{}) || got.Chain.Failed {
		t.Fatalf("account row %+v", got)
	}
	families, err := authority.LockFamilies(ctx, tx, family, missing)
	if err != nil {
		t.Fatal(err)
	}
	fam, ok := families[family]
	if len(families) != 1 || !ok || fam.ID != family || fam.Status != "active" || fam.OwnerAccountID != a || fam.Chain == nil || fam.Chain.Seq != 0 || fam.Chain.Failed || fam.EntitlementRequired {
		t.Fatalf("family rows %+v", families)
	}

	// The rows really are locked: a NOWAIT lock from another transaction fails.
	for table, id := range map[string]string{"accounts": a, "families": family} {
		other, err := f.Service.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = other.Exec(ctx, `SELECT 1 FROM `+table+` WHERE id=$1 FOR UPDATE NOWAIT`, id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Errorf("%s row not locked: %v", table, err)
		}
		_ = other.Rollback(ctx)
	}

	if empty, err := authority.LockAccounts(ctx, tx); err != nil || len(empty) != 0 {
		t.Fatalf("empty lock = %v, %v", empty, err)
	}
	for _, bad := range []string{"", "not-a-uuid", upper(a), "00000000-0000-0000-0000-000000000000"} {
		if _, err := authority.LockAccounts(ctx, tx, a, bad); !errors.Is(err, authority.ErrInvalidCommand) {
			t.Errorf("LockAccounts(%q) err=%v, want invalid_command", bad, err)
		}
	}
	if _, err := authority.LockFamilies(ctx, tx, "nope"); !errors.Is(err, authority.ErrInvalidCommand) {
		t.Errorf("LockFamilies(nope) err=%v", err)
	}
}

// The account lock order is the ID order, independent of the argument order.
func TestLockAccountsLocksInIDOrder(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	ids := []string{f.Account(t), f.Account(t), f.Account(t)}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	// Hold the smallest ID; a locker of {largest, smallest} must block on the
	// smallest before it has taken the largest.
	holder, err := f.Service.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := authority.LockAccounts(ctx, holder, sorted[0]); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() {
		tx, err := f.Service.Begin(ctx)
		if err != nil {
			blocked <- err
			return
		}
		defer tx.Rollback(context.Background())
		_, err = authority.LockAccounts(ctx, tx, sorted[2], sorted[0])
		blocked <- err
	}()
	time.Sleep(200 * time.Millisecond)
	probe, err := f.Service.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = probe.Exec(ctx, `SELECT 1 FROM accounts WHERE id=$1 FOR UPDATE NOWAIT`, sorted[2])
	_ = probe.Rollback(ctx)
	if err != nil {
		t.Fatalf("largest ID was locked before the smallest: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("locker never acquired the released row")
	}
}

// An export request's insert checks its foreign keys by taking FOR KEY SHARE
// on the family and then on the requesting owner's account, without locking
// either. ScheduleDeletion locks the owner's account and then the family.
// Under FOR UPDATE the interleaving "export holds the family key share,
// ScheduleDeletion holds the account and waits on the family, export's
// account check waits on ScheduleDeletion" was a 40P01 cycle. Authority
// locks are FOR NO KEY UPDATE, which key share does not conflict with, so
// ScheduleDeletion completes while an export holds the family's key share
// (or both, after its whole insert), and the export then commits; nothing
// waits on anything. The records family fence behaves the same.
func TestAuthorityLocksDoNotBlockExportForeignKeys(t *testing.T) {
	f := authoritytest.New(t)
	ctx := t.Context()
	for _, stage := range []string{"family key share", "whole export insert"} {
		t.Run(stage, func(t *testing.T) {
			owner := newMember(t, f)
			family := createFamily(t, f, f.Service, owner)
			export, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer export.Rollback(context.Background())
			if err := tenant.SetLocal(ctx, export, tenant.Context{AccountID: owner.account, FamilyID: family}); err != nil {
				t.Fatal(err)
			}
			// A lock wait here would be the bug: fail fast instead of hanging.
			if _, err := export.Exec(ctx, `SET LOCAL lock_timeout='5s'`); err != nil {
				t.Fatal(err)
			}
			insertExport := func() error {
				_, err := export.Exec(ctx, `INSERT INTO export_jobs(id,family_id,requester_account_id,request_id,server_instance_id,recovery_epoch,security_revision,state,created_at,expires_at,next_attempt_at)
					VALUES(gen_random_uuid(),$1,$2,gen_random_uuid(),$3,$4,1,'queued',now(),now()+interval '24 hours',now())`,
					family, owner.account, f.InstanceID, int64(authoritytest.RecoveryEpoch))
				return err
			}
			if stage == "family key share" {
				// The insert's first foreign key check, alone.
				if _, err := export.Exec(ctx, `SELECT 1 FROM families WHERE id=$1 FOR KEY SHARE`, family); err != nil {
					t.Fatal(err)
				}
			} else if err := insertExport(); err != nil {
				t.Fatal(err)
			}

			// The care family fence (the write batch's lock) does not wait on the key share either.
			fence, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer fence.Rollback(context.Background())
			if err := tenant.SetLocal(ctx, fence, tenant.Context{AccountID: owner.account, FamilyID: family}); err != nil {
				t.Fatal(err)
			}
			if _, err := fence.Exec(ctx, `SET LOCAL lock_timeout='5s'`); err != nil {
				t.Fatal(err)
			}
			if _, err := care.LockFamily(ctx, fence, family); err != nil {
				t.Fatalf("family fence behind an export's key share: %v", err)
			}
			_ = fence.Rollback(ctx)

			done := make(chan error, 1)
			go func() {
				_, err := f.Service.ScheduleDeletion(ctx, scheduleOf(t, f, owner.account, owner.principal(t, f)))
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("schedule deletion beside an export: %v", err)
				}
			case <-time.After(10 * time.Second):
				// Release the export so the blocked command and the pool
				// can finish, then fail.
				_ = export.Rollback(context.Background())
				<-done
				t.Fatal("schedule deletion waited on an export's foreign key locks")
			}
			if stage == "family key share" {
				if err := insertExport(); err != nil {
					t.Fatalf("export account check after the deletion: %v", err)
				}
			}
			if err := export.Commit(ctx); err != nil {
				t.Fatalf("export commit: %v", err)
			}
			if status, _, _ := familyState(t, f, family); status != "deletion_pending" {
				t.Fatalf("family %s", status)
			}
		})
	}
}
