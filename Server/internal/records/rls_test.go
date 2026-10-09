package records_test

import (
	"errors"
	"fmt"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRuntimeCursorColumnGrantDoesNotRestoreStructuralWrites(t *testing.T) {
	h := testkit.New(t)
	if err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		if err := records.LockFamilyForUpdate(t.Context(), tx, h.FamilyID); err != nil {
			return err
		}
		tag, err := tx.Exec(t.Context(), `UPDATE families SET next_sequence=next_sequence+1 WHERE id=$1`, h.FamilyID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatal("cursor update missed family")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for name, query := range map[string]string{
		"status":  `UPDATE families SET status='deletion_pending' WHERE id=$1`,
		"display": `UPDATE families SET display_name='forbidden' WHERE id=$1`,
		"purge":   `UPDATE families SET purge_after=now() WHERE id=$1`,
		"insert":  `INSERT INTO families(id,display_name) VALUES($1,'forbidden')`,
		"delete":  `DELETE FROM families WHERE id=$1`,
	} {
		t.Run(name, func(t *testing.T) {
			err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error { _, err := tx.Exec(t.Context(), query, h.FamilyID); return err })
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("err=%v", err)
			}
		})
	}

	foreign, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "Foreign")
	if err != nil {
		t.Fatal(err)
	}
	h.Outsider = h.Refresh("outsider")
	if err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		tag, err := tx.Exec(t.Context(), `UPDATE families SET next_sequence=next_sequence+1 WHERE id=$1`, foreign.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatal("foreign cursor changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for column, want := range map[string]bool{"next_sequence": true, "status": false, "display_name": false, "purge_after": false} {
		var got bool
		if err := h.Migrator.QueryRow(t.Context(), `SELECT has_column_privilege('baby_tracker_runtime','families',$1,'UPDATE')`, column).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("column %s privilege=%t want=%t", column, got, want)
		}
	}
}

func TestDirectPutRoundTripsSubmittedParentInsideTransaction(t *testing.T) {
	h := testkit.New(t)
	first := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, first); err != nil {
		t.Fatal(err)
	}
	second := testkit.MustMutation(t, "baby.json")
	second.MutationID = "01000000-0000-4000-8000-000000000201"
	second.EntityID = "10000000-0000-4000-8000-000000000201"
	p := second.Payload.(protocol.BabyPayload)
	p.Name = "Second"
	second.Payload = p
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, second); err != nil {
		t.Fatal(err)
	}
	mutations := []protocol.Mutation{testkit.MustMutation(t, "feed-bottle.json"), testkit.MustMutation(t, "sleep.json"), testkit.MustMutation(t, "diaper.json")}
	for i := range mutations {
		mutations[i].MutationID = fmt.Sprintf("01000000-0000-4000-8000-%012d", 210+i)
		mutations[i].EntityID = fmt.Sprintf("20000000-0000-4000-8000-%012d", 210+i)
		if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, mutations[i]); err != nil {
			t.Fatal(err)
		}
	}
	repo := records.NewRepository()
	rollback := errors.New("rollback direct puts")
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "mutate_record", func(tx pgx.Tx, _ access.Grant) error {
		for _, m := range mutations {
			switch m.EntityKind {
			case "feed":
				payload, meta, err := repo.GetFeed(t.Context(), tx, h.FamilyID, m.EntityID)
				if err != nil {
					return err
				}
				payload.BabyID = second.EntityID
				meta.EntityVersion++
				if _, err = repo.PutFeed(t.Context(), tx, meta, payload); err != nil {
					return err
				}
				got, _, err := repo.GetFeed(t.Context(), tx, h.FamilyID, m.EntityID)
				if err != nil || got.BabyID != second.EntityID {
					t.Fatalf("feed parent=%q err=%v", got.BabyID, err)
				}
			case "sleep":
				payload, meta, err := repo.GetSleep(t.Context(), tx, h.FamilyID, m.EntityID)
				if err != nil {
					return err
				}
				payload.BabyID = second.EntityID
				meta.EntityVersion++
				if _, err = repo.PutSleep(t.Context(), tx, meta, payload); err != nil {
					return err
				}
				got, _, err := repo.GetSleep(t.Context(), tx, h.FamilyID, m.EntityID)
				if err != nil || got.BabyID != second.EntityID {
					t.Fatalf("sleep parent=%q err=%v", got.BabyID, err)
				}
			case "diaper":
				payload, meta, err := repo.GetDiaper(t.Context(), tx, h.FamilyID, m.EntityID)
				if err != nil {
					return err
				}
				payload.BabyID = second.EntityID
				meta.EntityVersion++
				if _, err = repo.PutDiaper(t.Context(), tx, meta, payload); err != nil {
					return err
				}
				got, _, err := repo.GetDiaper(t.Context(), tx, h.FamilyID, m.EntityID)
				if err != nil || got.BabyID != second.EntityID {
					t.Fatalf("diaper parent=%q err=%v", got.BabyID, err)
				}
			}
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("err=%v", err)
	}
}

func TestEveryRepositoryDeniesForeignFamily(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	foreign, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "Foreign")
	if err != nil {
		t.Fatal(err)
	}
	h.Outsider = h.Refresh("outsider")
	baby := testkit.MustMutation(t, "baby.json")
	baby.EntityID = "10000000-0000-4000-8000-000000000099"
	baby.MutationID = "01000000-0000-4000-8000-000000000099"
	if _, err = h.Service.Apply(t.Context(), h.Outsider, foreign.ID, baby); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{"baby": baby.EntityID}
	for index, fixture := range []string{"feed-bottle.json", "sleep.json", "diaper.json", "supplement.json"} {
		m := testkit.MustMutation(t, fixture)
		m.MutationID = fmt.Sprintf("01000000-0000-4000-8000-%012d", 110+index)
		m.EntityID = fmt.Sprintf("20000000-0000-4000-8000-%012d", 110+index)
		switch p := m.Payload.(type) {
		case protocol.FeedPayload:
			// Several parts, so the foreign feed has child rows to hide.
			left, right, some := "left", "right", "some"
			leftSeconds, rightSeconds := uint32(30), uint32(45)
			p.BabyID = baby.EntityID
			p.Kind = "breast"
			p.FeedingSessionID = nil
			p.Components = []protocol.FeedComponent{
				{ID: "31000000-0000-4000-8000-000000000110", Kind: "breast", Side: &left, Seconds: &leftSeconds},
				{ID: "31000000-0000-4000-8000-000000000111", Kind: "breast", Side: &right, Seconds: &rightSeconds},
				{ID: "31000000-0000-4000-8000-000000000112", Kind: "solids", Foods: []string{"banana", "banana", "yogurt"}, Amount: &some},
			}
			m.Payload = p
		case protocol.SleepPayload:
			p.BabyID = baby.EntityID
			m.Payload = p
		case protocol.SupplementPayload:
			p.BabyID = baby.EntityID
			m.Payload = p
		case protocol.DiaperPayload:
			p.BabyID = baby.EntityID
			m.Payload = p
		}
		if _, err = h.Service.Apply(t.Context(), h.Outsider, foreign.ID, m); err != nil {
			t.Fatal(err)
		}
		ids[m.EntityKind] = m.EntityID
	}
	if parts := h.Count(t, "feed_components", foreign.ID); parts != 3 {
		t.Fatalf("foreign feed parts=%d", parts)
	}
	tables := []string{"babies", "feed_entries", "sleep_entries", "diaper_entries", "supplement_entries", "feed_components", "mutation_receipts", "mutation_journal", "family_record_usage"}
	for _, table := range tables {
		var rawCount int
		if err = h.Runtime.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&rawCount); err != nil || rawCount != 0 {
			t.Fatalf("missing-context %s count=%d err=%v", table, rawCount, err)
		}
	}
	repo := records.NewRepository()
	localBaby := testkit.MustMutation(t, "baby.json").Payload.(protocol.BabyPayload)
	foreignBaby := baby.Payload.(protocol.BabyPayload)
	localBabyID := testkit.MustMutation(t, "baby.json").EntityID
	foreignFeed := testkit.MustMutation(t, "feed-breast.json").Payload.(protocol.FeedPayload)
	foreignFeed.BabyID = baby.EntityID
	foreignSleep := testkit.MustMutation(t, "sleep.json").Payload.(protocol.SleepPayload)
	foreignSleep.BabyID = baby.EntityID
	foreignDiaper := testkit.MustMutation(t, "diaper.json").Payload.(protocol.DiaperPayload)
	foreignDiaper.BabyID = baby.EntityID

	missingContextWrites := []struct {
		name string
		run  func(pgx.Tx) error
	}{
		{"babies repository", func(tx pgx.Tx) error {
			_, err := repo.PutBaby(t.Context(), tx, testEntityMeta(h.FamilyID, "10000000-0000-4000-8000-000000000701", h.Owner.AccountID), localBaby)
			return err
		}},
		{"feeds repository", func(tx pgx.Tx) error {
			payload := foreignFeed
			payload.BabyID = localBabyID
			_, err := repo.PutFeed(t.Context(), tx, testEntityMeta(h.FamilyID, "20000000-0000-4000-8000-000000000701", h.Owner.AccountID), payload)
			return err
		}},
		{"sleeps repository", func(tx pgx.Tx) error {
			payload := foreignSleep
			payload.BabyID = localBabyID
			_, err := repo.PutSleep(t.Context(), tx, testEntityMeta(h.FamilyID, "30000000-0000-4000-8000-000000000701", h.Owner.AccountID), payload)
			return err
		}},
		{"diapers repository", func(tx pgx.Tx) error {
			payload := foreignDiaper
			payload.BabyID = localBabyID
			_, err := repo.PutDiaper(t.Context(), tx, testEntityMeta(h.FamilyID, "40000000-0000-4000-8000-000000000701", h.Owner.AccountID), payload)
			return err
		}},
		{"feed parts table", func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds)
				VALUES($1,$2,'31000000-0000-4000-8000-000000000113',3,'breast','left',1)`, foreign.ID, ids["feed"])
			return err
		}},
	}
	for _, test := range missingContextWrites {
		t.Run("missing context write "+test.name, func(t *testing.T) {
			tx, beginErr := h.Runtime.Begin(t.Context())
			if beginErr != nil {
				t.Fatal(beginErr)
			}
			writeErr := test.run(tx)
			_ = tx.Rollback(t.Context())
			assertPGCode(t, writeErr, "42501")
		})
	}

	foreignContextWrites := []struct {
		name string
		run  func(pgx.Tx) error
	}{
		{"babies repository", func(tx pgx.Tx) error {
			_, err := repo.PutBaby(t.Context(), tx, testEntityMeta(foreign.ID, "10000000-0000-4000-8000-000000000702", h.Owner.AccountID), foreignBaby)
			return err
		}},
		{"feeds repository", func(tx pgx.Tx) error {
			_, err := repo.PutFeed(t.Context(), tx, testEntityMeta(foreign.ID, "20000000-0000-4000-8000-000000000702", h.Owner.AccountID), foreignFeed)
			return err
		}},
		{"sleeps repository", func(tx pgx.Tx) error {
			_, err := repo.PutSleep(t.Context(), tx, testEntityMeta(foreign.ID, "30000000-0000-4000-8000-000000000702", h.Owner.AccountID), foreignSleep)
			return err
		}},
		{"diapers repository", func(tx pgx.Tx) error {
			_, err := repo.PutDiaper(t.Context(), tx, testEntityMeta(foreign.ID, "40000000-0000-4000-8000-000000000702", h.Owner.AccountID), foreignDiaper)
			return err
		}},
		{"feed parts table", func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds)
				VALUES($1,$2,'31000000-0000-4000-8000-000000000113',3,'breast','left',1)`, foreign.ID, ids["feed"])
			return err
		}},
	}
	for _, test := range foreignContextWrites {
		t.Run("foreign context write "+test.name, func(t *testing.T) {
			writeErr := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "mutate_record", func(tx pgx.Tx, _ access.Grant) error {
				return test.run(tx)
			})
			assertPGCode(t, writeErr, "42501")
		})
	}

	err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		for _, table := range tables {
			var count int
			if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE family_id=$1`, foreign.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("foreign-context %s count=%d err=%v", table, count, err)
			}
		}
		if _, _, err := repo.GetBaby(t.Context(), tx, foreign.ID, ids["baby"]); !errors.Is(err, records.ErrNotFound) {
			t.Fatalf("foreign baby err=%v", err)
		}
		if _, _, err := repo.GetFeed(t.Context(), tx, foreign.ID, ids["feed"]); !errors.Is(err, records.ErrNotFound) {
			t.Fatalf("foreign feed err=%v", err)
		}
		if _, _, err := repo.GetSleep(t.Context(), tx, foreign.ID, ids["sleep"]); !errors.Is(err, records.ErrNotFound) {
			t.Fatalf("foreign sleep err=%v", err)
		}
		if _, _, err := repo.GetDiaper(t.Context(), tx, foreign.ID, ids["diaper"]); !errors.Is(err, records.ErrNotFound) {
			t.Fatalf("foreign diaper err=%v", err)
		}
		_, insertErr := tx.Exec(t.Context(), `INSERT INTO family_record_usage(family_id,canonical_record_count,charged_storage_bytes,updated_at) VALUES($1,0,0,now()) ON CONFLICT DO NOTHING`, foreign.ID)
		var pgErr *pgconn.PgError
		if !errors.As(insertErr, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("foreign insert err=%v", insertErr)
		}
		return nil
	})
	if err == nil {
		t.Fatal("transaction with RLS violation unexpectedly committed")
	}
	for _, table := range tables {
		t.Run("foreign update "+table, func(t *testing.T) {
			err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
				tag, err := tx.Exec(t.Context(), `UPDATE `+table+` SET family_id=family_id WHERE family_id=$1`, foreign.ID)
				if err == nil && tag.RowsAffected() != 0 {
					t.Fatalf("foreign %s updated %d", table, tag.RowsAffected())
				}
				return err
			})
			if err == nil {
				if table == "feed_components" || table == "mutation_receipts" || table == "mutation_journal" {
					t.Fatalf("%s unexpectedly update-capable", table)
				}
			} else {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
					t.Fatalf("%s err=%v", table, err)
				}
			}
		})
	}
	crossFamilyFKWrites := []struct {
		name string
		run  func(pgx.Tx) error
	}{
		{"baby journal cursor", func(tx pgx.Tx) error {
			meta := testEntityMeta(h.FamilyID, baby.EntityID, h.Owner.AccountID)
			if _, err := repo.PutBaby(t.Context(), tx, meta, foreignBaby); err != nil {
				return err
			}
			if _, err := tx.Exec(t.Context(), `UPDATE babies SET last_sequence=1 WHERE family_id=$1 AND id=$2`, h.FamilyID, baby.EntityID); err != nil {
				return err
			}
			_, err := tx.Exec(t.Context(), `SET CONSTRAINTS babies_last_sequence_fk IMMEDIATE`)
			return err
		}},
		{"feed baby", func(tx pgx.Tx) error {
			_, err := repo.PutFeed(t.Context(), tx, testEntityMeta(h.FamilyID, "20000000-0000-4000-8000-000000000703", h.Owner.AccountID), foreignFeed)
			return err
		}},
		{"sleep baby", func(tx pgx.Tx) error {
			_, err := repo.PutSleep(t.Context(), tx, testEntityMeta(h.FamilyID, "30000000-0000-4000-8000-000000000703", h.Owner.AccountID), foreignSleep)
			return err
		}},
		{"diaper baby", func(tx pgx.Tx) error {
			_, err := repo.PutDiaper(t.Context(), tx, testEntityMeta(h.FamilyID, "40000000-0000-4000-8000-000000000703", h.Owner.AccountID), foreignDiaper)
			return err
		}},
		{"feed part parent", func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), `INSERT INTO feed_components(family_id,feed_id,component_id,position,kind,side,seconds)
				VALUES($1,$2,'31000000-0000-4000-8000-000000000114',4,'breast','left',1)`, h.FamilyID, ids["feed"])
			return err
		}},
	}
	for _, test := range crossFamilyFKWrites {
		t.Run("cross-family FK "+test.name, func(t *testing.T) {
			writeErr := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "mutate_record", func(tx pgx.Tx, _ access.Grant) error {
				return test.run(tx)
			})
			assertPGCode(t, writeErr, "23503")
		})
	}
	for _, attribute := range []string{"rolsuper", "rolcreaterole", "rolcreatedb", "rolreplication", "rolbypassrls"} {
		var value bool
		if err := h.Migrator.QueryRow(t.Context(), `SELECT `+attribute+` FROM pg_roles WHERE rolname='baby_tracker_runtime'`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value {
			t.Fatalf("runtime %s=true", attribute)
		}
	}
}

func testEntityMeta(familyID, entityID, accountID string) protocol.EntityMeta {
	return protocol.EntityMeta{
		ID: entityID, FamilyID: familyID, EntityVersion: 1,
		CreatorAccountID: accountID, LastEditorAccountID: accountID,
		ClientMutationTime: "2026-09-20T12:00:00.000000Z",
		AcceptedAt:         "2026-09-20T12:00:00.000000Z",
	}
}

func assertPGCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("err=%v code=%q want=%q", err, pgErrCode(pgErr), code)
	}
}

func pgErrCode(err *pgconn.PgError) string {
	if err == nil {
		return ""
	}
	return err.Code
}
