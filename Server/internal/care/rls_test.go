package care_test

import (
	"errors"
	"slices"
	"testing"

	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Row-level security on the four care tables (ported from records/rls_test.go at 342973dc):
// a family's tenant reads and writes none of another family's rows, and an
// account sees its own receipts from a previous family but never another
// account's.
func TestCareRLSIsolatesFamilies(t *testing.T) {
	h := testkit.New(t)
	ctx := t.Context()
	foreign, err := h.Family.Create(ctx, h.Outsider, h.CommandID(), "Foreign")
	if err != nil {
		t.Fatal(err)
	}
	h.Outsider = h.Refresh("outsider")
	ownerA := caretest.Owner(h)
	outsiderB := caretest.Author{FamilyID: foreign.ID, AccountID: h.Outsider.AccountID, DeviceID: h.Outsider.DeviceID, Now: h.Now}
	// A synthetic tenant context: another account writing family B's receipt.
	otherB := caretest.Author{FamilyID: foreign.ID, AccountID: h.Caregiver.AccountID, DeviceID: h.Caregiver.DeviceID, Now: h.Now}
	receipt := func(a caretest.Author, mutation string) care.ReceiptRow {
		return care.ReceiptRow{DeviceID: a.DeviceID, MutationID: mutation, AccountID: a.AccountID, Hash: [32]byte{1},
			Outcome: protocol.Outcome{MutationID: mutation, Status: "applied"}, AcceptedAt: h.Now}
	}
	const (
		receiptA      = "01000000-0000-4000-8000-00000000000a"
		receiptB      = "01000000-0000-4000-8000-00000000000b"
		receiptOtherB = "01000000-0000-4000-8000-00000000000c"
	)
	babyA := ownerA.Baby(t, "11000000-0000-4000-8000-00000000000a", "Ada", "2026-01-12", 1)
	babyB := outsiderB.Baby(t, "11000000-0000-4000-8000-00000000000b", "Bea", "2026-01-12", 1)
	if err := ownerA.Commit(t, h.Runtime, []care.ReceiptRow{receipt(ownerA, receiptA)}, babyA, ownerA.Row(t, "feed", "21000000-0000-4000-8000-00000000000a", &babyA, 2)); err != nil {
		t.Fatal(err)
	}
	if err := outsiderB.Commit(t, h.Runtime, []care.ReceiptRow{receipt(outsiderB, receiptB)}, babyB, outsiderB.Row(t, "feed", "21000000-0000-4000-8000-00000000000b", &babyB, 2)); err != nil {
		t.Fatal(err)
	}
	if err := otherB.Commit(t, h.Runtime, []care.ReceiptRow{receipt(otherB, receiptOtherB)}); err != nil {
		t.Fatal(err)
	}
	for session, a := range map[string]caretest.Author{"52000000-0000-4000-8000-00000000000a": ownerA, "52000000-0000-4000-8000-00000000000b": outsiderB} {
		tx := caretest.TenantTx(t, h.Runtime, a.AccountID, a.FamilyID)
		if err := insertSessionRows(t, tx, a, session); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	tables := []string{"care_entities", "care_receipts", "sync_page_bindings", "sync_snapshot_sessions"}
	t.Run("reads", func(t *testing.T) {
		tx := caretest.TenantTx(t, h.Runtime, ownerA.AccountID, ownerA.FamilyID)
		for _, table := range tables {
			var foreignRows, all, own int
			if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE family_id=$1), count(*), count(*) FILTER (WHERE family_id=$2) FROM `+table,
				foreign.ID, h.FamilyID).Scan(&foreignRows, &all, &own); err != nil {
				t.Fatal(err)
			}
			if foreignRows != 0 || all != own || own == 0 {
				t.Fatalf("%s: family B rows %d, all %d, family A %d", table, foreignRows, all, own)
			}
		}
		if _, err := care.Get(ctx, tx, foreign.ID, babyB.ID); !errors.Is(err, care.ErrNotFound) {
			t.Fatalf("family B's baby through the store: %v", err)
		}
		if _, err := care.LockFamily(ctx, tx, foreign.ID); !errors.Is(err, care.ErrNotFound) {
			t.Fatalf("family B's lock: %v", err)
		}
		if rows := caretest.Rows(t, h); len(rows) != 2 {
			t.Fatalf("family A rows %d", len(rows))
		}
	})

	t.Run("writes", func(t *testing.T) {
		foreignBaby := outsiderB.Baby(t, "11000000-0000-4000-8000-0000000000bb", "Cy", "2026-01-12", 3)
		writes := map[string]func(pgx.Tx) error{
			"care_entities": func(tx pgx.Tx) error {
				batch := &pgx.Batch{}
				check := care.QueueWrites(batch, care.Writes{FamilyID: foreign.ID, Base: care.FamilyState{Sequence: 2}, Sequence: 3, Rows: []care.Row{foreignBaby}})
				results := tx.SendBatch(ctx, batch)
				defer results.Close()
				return check(results)
			},
			"care_receipts": func(tx pgx.Tx) error {
				r := receipt(ownerA, "01000000-0000-4000-8000-0000000000dd")
				batch := &pgx.Batch{}
				check := care.QueueWrites(batch, care.Writes{FamilyID: foreign.ID, Base: care.FamilyState{Sequence: 2}, Sequence: 2, Receipts: []care.ReceiptRow{r}})
				results := tx.SendBatch(ctx, batch)
				defer results.Close()
				return check(results)
			},
			"sync_page_bindings": func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, insertBinding, foreign.ID, ownerA.AccountID, ownerA.DeviceID, "52000000-0000-4000-8000-0000000000dd")
				return err
			},
			"sync_snapshot_sessions": func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, insertSnapshot, foreign.ID, ownerA.AccountID, ownerA.DeviceID, "52000000-0000-4000-8000-0000000000dd")
				return err
			},
		}
		for _, table := range tables {
			tx := caretest.TenantTx(t, h.Runtime, ownerA.AccountID, ownerA.FamilyID)
			var pgErr *pgconn.PgError
			if err := writes[table](tx); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("%s insert into family B: %v, want 42501", table, err)
			}
		}
		// Updating family B's rows from family A touches nothing.
		tx := caretest.TenantTx(t, h.Runtime, ownerA.AccountID, ownerA.FamilyID)
		tag, err := tx.Exec(ctx, `UPDATE care_entities SET wire=wire WHERE family_id=$1`, foreign.ID)
		if err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("update of family B's rows: %d %v", tag.RowsAffected(), err)
		}
		if _, err := care.ReadFamilyState(ctx, tx, foreign.ID); !errors.Is(err, care.ErrNotFound) {
			t.Fatalf("family B's state: %v", err)
		}
	})

	t.Run("receipts", func(t *testing.T) {
		visible := func(account caretest.Author, family string) []string {
			tx := caretest.TenantTx(t, h.Runtime, account.AccountID, family)
			rows, err := tx.Query(ctx, `SELECT mutation_id::text FROM care_receipts ORDER BY care_receipts.mutation_id`)
			if err != nil {
				t.Fatal(err)
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			return ids
		}
		// The outsider, now in family A, still sees its own receipt from family
		// B, but not the other account's.
		if got := visible(outsiderB, h.FamilyID); !slices.Equal(got, []string{receiptA, receiptB}) {
			t.Fatalf("outsider in family A sees %v", got)
		}
		if got := visible(ownerA, h.FamilyID); !slices.Equal(got, []string{receiptA}) {
			t.Fatalf("owner in family A sees %v", got)
		}
		// Fetch reports the outsider's family B receipt as another family's.
		tx := caretest.TenantTx(t, h.Runtime, outsiderB.AccountID, h.FamilyID)
		fetched, err := care.Fetch(ctx, tx, h.FamilyID, care.Refs{MutationIDs: []string{receiptB, receiptOtherB}, DeviceID: outsiderB.DeviceID})
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := fetched.Receipts[receiptB]; len(fetched.Receipts) != 1 || !ok || r.SameFamily {
			t.Fatalf("receipts %+v", fetched.Receipts)
		}
	})
}

const (
	insertBinding = `INSERT INTO sync_page_bindings(family_id,token_digest,token_key_id,account_id,device_id,session_id,server_instance_id,recovery_epoch,
		security_revision,kind,high_water_sequence,resume_sequence,item_limit,byte_limit,created_at,expires_at)
		VALUES ($1,sha256(convert_to($4::uuid::text,'UTF8')),'page.v1',$2,$3,$4::uuid,'a1000000-0000-4000-8000-000000000001',1,1,'sync',0,0,100,65536,now(),now()+interval '1 hour')`
	insertSnapshot = `INSERT INTO sync_snapshot_sessions(family_id,id,account_id,device_id,session_id,server_instance_id,recovery_epoch,security_revision,
		high_water_sequence,captured_at,expires_at)
		VALUES ($1,$4::uuid,$2,$3,$4::uuid,'a1000000-0000-4000-8000-000000000001',1,1,0,now(),now()+interval '1 hour')`
)

func insertSessionRows(t *testing.T, tx pgx.Tx, a caretest.Author, session string) error {
	if _, err := tx.Exec(t.Context(), insertBinding, a.FamilyID, a.AccountID, a.DeviceID, session); err != nil {
		return err
	}
	_, err := tx.Exec(t.Context(), insertSnapshot, a.FamilyID, a.AccountID, a.DeviceID, session)
	return err
}
