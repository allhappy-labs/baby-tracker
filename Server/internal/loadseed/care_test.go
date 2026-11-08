//go:build identitytest

package loadseed

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// writersOf is the harness's owner and caregiver with their one device.
func writersOf(h *testkit.Harness) []writer {
	return []writer{{AccountID: h.Owner.AccountID, DeviceIDs: []string{h.Owner.DeviceID}},
		{AccountID: h.Caregiver.AccountID, DeviceIDs: []string{h.Caregiver.DeviceID}}}
}

// coverageSpec is Small with every kind and path planned: two babies, a
// running timer each, pumps and a running pump, edits and deletions.
func coverageSpec(asOf time.Time) Spec {
	spec := Small(1, 7, asOf)
	spec.TwinShare = 1
	spec.History.RunningTimerShare, spec.History.RunningPumpShare, spec.History.PumpFamilyShare = 1, 1, 1
	spec.History.UpdateShare, spec.History.TombstoneShare = 0.3, 0.1
	return spec
}

// longCoverageSpec is coverageSpec over 180 days at sparse rates: babies
// old enough for solids feeds and later immunization doses, and a history
// longer than the receipts the seed keeps (30 days).
func longCoverageSpec(asOf time.Time) Spec {
	spec := coverageSpec(asOf)
	spec.History.DaysWeights = map[int]float64{180: 1}
	spec.History.FeedsPerDay, spec.History.DiapersPerDay, spec.History.SleepsPerDay, spec.History.SupplementsPerDay = 1, 0.15, 0.1, 0.03
	spec.History.PumpsPerDay = 0.1
	return spec
}

// The rows Seed writes are the product's rows: the plan's mutations applied
// through the sync service leave the same care_entities rows (but for IDs
// and clocks the two harnesses do not share), the same counters and the
// same receipts as the seeded rows loaded with COPY.
func TestSeededCareRowsEqualTheWritePathsRows(t *testing.T) {
	t.Run("week, every receipt", func(t *testing.T) {
		assertSeededEqualsApplied(t, coverageSpec, 0)
	})
	t.Run("half a year, the last 30 days receipted", func(t *testing.T) {
		assertSeededEqualsApplied(t, longCoverageSpec, receiptRetention)
	})
}

// assertSeededEqualsApplied seeds family 0 of spec(now) into one harness
// with COPY, keeping the receipts of the last retention (0: every
// receipt), applies the same mutations through the sync service in
// another, and compares rows, counters and receipts.
func assertSeededEqualsApplied(t *testing.T, spec func(time.Time) Spec, retention time.Duration) {
	seeded := testkit.New(t)
	applied := testkit.New(t)
	plan := planFamily(spec(seeded.Now), 0)
	if plan.RunningPumpID == "" || len(plan.Babies) != 2 || plan.Babies[0].RunningTimerID == "" {
		t.Fatalf("the coverage plan lacks a running pump or timer: %+v", plan.Babies)
	}
	if plan.Index%validateEvery != 0 {
		t.Fatal("the coverage family is not one encodeFamily validates")
	}
	var cutoff time.Time
	if retention > 0 {
		cutoff = seeded.Now.Add(-retention)
	}
	kept := map[string]bool{}
	solids, laterDoses := 0, 0
	for _, c := range plan.Changes {
		if !c.AcceptedAt.Before(cutoff) {
			kept[c.Mutation.MutationID] = true
		}
		switch payload := c.Mutation.Payload.(type) {
		case protocol.FeedPayload:
			if payload.Kind == "solids" {
				solids++
			}
		case protocol.ImmunizationPayload:
			if payload.Dose != nil && *payload.Dose > 1 {
				laterDoses++
			}
		}
	}
	if retention > 0 && (solids == 0 || laterDoses == 0 || len(kept) == 0 || len(kept) == len(plan.Changes)) {
		t.Fatalf("the long coverage plan has %d solids feeds, %d later doses, %d of %d changes receipted", solids, laterDoses, len(kept), len(plan.Changes))
	}
	encoded, err := encodeFamily(plan, seeded.FamilyID, writersOf(seeded), seeded.Info, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded.Rows) != plan.Entities() || len(encoded.Receipts) != len(kept) || encoded.State.Sequence != uint64(len(plan.Changes)) {
		t.Fatalf("%d rows, %d receipts, sequence %d for %d entities, %d changes and %d kept receipts", len(encoded.Rows), len(encoded.Receipts),
			encoded.State.Sequence, plan.Entities(), len(plan.Changes), len(kept))
	}
	seededAdmin := superuserOn(t, seeded.Runtime, 4)
	if _, err := copyCareRows(t.Context(), seededAdmin, "care_entities", encoded.Rows); err != nil {
		t.Fatal(err)
	}
	next := 0
	if _, err := copyFrom(t.Context(), seededAdmin, "care_receipts", receiptColumns, pgx.CopyFromFunc(func() ([]any, error) {
		if next == len(encoded.Receipts) {
			return nil, nil
		}
		next++
		return receiptValues(seeded.FamilyID, encoded.Receipts[next-1]), nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := setFamilyCounters(t.Context(), seededAdmin, []*familyState{{FamilyID: seeded.FamilyID}}, []care.FamilyState{encoded.State}); err != nil {
		t.Fatal(err)
	}

	for i, c := range plan.Changes {
		p := []access.Principal{applied.Owner, applied.Caregiver}[c.Actor]
		outcome, err := applied.Service.Apply(t.Context(), p, applied.FamilyID, c.Mutation)
		if err != nil {
			t.Fatalf("change %d (%s %s): %v", i, c.Mutation.EntityKind, c.Mutation.Operation, err)
		}
		if outcome.Status != "applied" {
			t.Fatalf("change %d (%s %s) %s: %v", i, c.Mutation.EntityKind, c.Mutation.Operation, outcome.Status, *outcome.Code)
		}
	}
	appliedAdmin := superuserOn(t, applied.Runtime, 4)

	appliedIDs, seededIDs := labelsOf(applied), labelsOf(seeded)
	want, got := careRows(t, appliedAdmin, applied.FamilyID, appliedIDs), careRows(t, seededAdmin, seeded.FamilyID, seededIDs)
	if len(want) != len(got) || len(got) != plan.Entities() {
		t.Fatalf("applied %d rows, seeded %d, planned %d", len(want), len(got), plan.Entities())
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("row %d differs:\napplied %s\nseeded  %s", i, want[i], got[i])
		}
	}

	// The counters: the seeded family's are what it holds, and the applied
	// family's less the charge of the receipts the seed did not keep.
	appliedState, seededState := readState(t, appliedAdmin, applied.FamilyID), readState(t, seededAdmin, seeded.FamilyID)
	appliedState.ChargedBytes -= uint64(unkeptReceiptCharge(t, appliedAdmin, applied.FamilyID, kept))
	if seededState != encoded.State || appliedState != seededState {
		t.Fatalf("counters: applied (less unkept receipts) %+v, seeded %+v, encoded %+v", appliedState, seededState, encoded.State)
	}
	for _, pool := range []struct {
		pool   *pgxpool.Pool
		family string
	}{{appliedAdmin, applied.FamilyID}, {seededAdmin, seeded.FamilyID}} {
		if err := verifyCounters(t.Context(), Environment{Superuser: pool.pool}, pool.family); err != nil {
			t.Fatal(err)
		}
	}

	// The receipts: one per kept mutation, the same mutation hash and the
	// same outcome but for IDs and clocks.
	wantReceipts, gotReceipts := receiptRows(t, appliedAdmin, applied.FamilyID, appliedIDs), receiptRows(t, seededAdmin, seeded.FamilyID, seededIDs)
	if len(wantReceipts) != len(plan.Changes) || len(gotReceipts) != len(kept) {
		t.Fatalf("applied %d receipts, seeded %d, planned %d changes, %d kept", len(wantReceipts), len(gotReceipts), len(plan.Changes), len(kept))
	}
	for id := range kept {
		if gotReceipts[id] != wantReceipts[id] {
			t.Fatalf("receipt of %s:\napplied %s\nseeded  %s", id, wantReceipts[id], gotReceipts[id])
		}
	}
}

// unkeptReceiptCharge is the charge of the family's receipts whose
// mutation is not kept.
func unkeptReceiptCharge(t *testing.T, pool *pgxpool.Pool, familyID string, kept map[string]bool) int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT mutation_id::text, outcome::text FROM care_receipts WHERE family_id = $1`, familyID)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	var id, outcome string
	if _, err := pgx.ForEachRow(rows, []any{&id, &outcome}, func() error {
		if kept[id] {
			return nil
		}
		charge, err := care.StoredReceiptCharge([]byte(outcome))
		total += charge
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

// labelsOf names the harness's family, accounts and devices by their
// role, so two harnesses' rows compare: they differ in those IDs.
func labelsOf(h *testkit.Harness) map[string]string {
	return map[string]string{h.FamilyID: "family", h.Owner.AccountID: "owner", h.Caregiver.AccountID: "caregiver",
		h.Owner.DeviceID: "owner-device", h.Caregiver.DeviceID: "caregiver-device"}
}

func label(labels map[string]string, id string) string {
	if name, ok := labels[id]; ok {
		return name
	}
	return "unknown:" + id
}

// labelled is entity with its family, creator and editor named by role and
// its clocks (accepted and tombstoned, which differ between harnesses) cut
// to whether they are set.
func labelled(entity protocol.CanonicalEntity, labels map[string]string) protocol.CanonicalEntity {
	m := &entity.Meta
	m.FamilyID, m.CreatorAccountID, m.LastEditorAccountID = label(labels, m.FamilyID), label(labels, m.CreatorAccountID), label(labels, m.LastEditorAccountID)
	m.AcceptedAt = "accepted"
	if m.TombstonedAt != nil {
		m.TombstonedAt = ptr("tombstoned")
	}
	return entity
}

// careRows is the family's care rows ordered by ID, each a line of its
// columns with the family, accounts and devices named by role and the
// acceptance clocks cut (the harnesses differ in both): kind, version,
// sequence, creator, actor, device and origin, operation, typed and
// derived columns, wire size and the wire.
func careRows(t *testing.T, pool *pgxpool.Pool, familyID string, labels map[string]string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT id::text, kind, baby_id::text, entity_version, changed_seq, operation::text,
		tombstoned_at IS NOT NULL, started_at, ended_at, session_id::text, timer_kind, completed_record_id::text, sort_order, label,
		name_hash, breast_seconds, bottle_volume_ml::text, bottle_observations, bottle_missing, shortest_part_seconds, feed_kind,
		created_seq, octet_length(wire), wire, accepted_at <= coalesce(tombstoned_at, accepted_at),
		creator_account_id::text, actor_account_id::text, device_id::text, created_device_id::text
		FROM care_entities WHERE family_id = $1 ORDER BY id`, familyID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		wire := values[23].([]byte)
		var entity protocol.CanonicalEntity
		if err := json.Unmarshal(wire, &entity); err != nil {
			t.Fatal(err)
		}
		blanked, err := json.Marshal(labelled(entity, labels))
		if err != nil {
			t.Fatal(err)
		}
		values[23] = string(blanked)
		for i := 25; i <= 28; i++ {
			values[i] = label(labels, values[i].(string))
		}
		if hash, ok := values[14].([]byte); ok {
			values[14] = hex.EncodeToString(hash)
		}
		for i, v := range values {
			if at, ok := v.(time.Time); ok {
				values[i] = at.UTC().Format(time.RFC3339Nano)
			}
		}
		out = append(out, fmt.Sprintln(values...))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func readState(t *testing.T, pool *pgxpool.Pool, familyID string) care.FamilyState {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	state, err := care.ReadFamilyState(t.Context(), tx, familyID)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// receiptRows is the family's receipts by mutation ID: the device and
// account by role, the semantic hash and the outcome, its entity labelled
// and its cursor's family by role.
func receiptRows(t *testing.T, pool *pgxpool.Pool, familyID string, labels map[string]string) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT mutation_id::text, device_id::text, account_id::text, semantic_hash, outcome::text
		FROM care_receipts WHERE family_id = $1`, familyID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var id, device, account, outcome string
	var hash []byte
	if _, err := pgx.ForEachRow(rows, []any{&id, &device, &account, &hash, &outcome}, func() error {
		var decoded protocol.Outcome
		if err := json.Unmarshal([]byte(outcome), &decoded); err != nil {
			return err
		}
		if decoded.CanonicalEntity == nil || decoded.Cursor == nil {
			return fmt.Errorf("receipt %s without its entity or cursor", id)
		}
		entity := labelled(*decoded.CanonicalEntity, labels)
		decoded.CanonicalEntity = &entity
		decoded.Cursor.FamilyID = label(labels, decoded.Cursor.FamilyID)
		raw, err := json.Marshal(decoded)
		if err != nil {
			return err
		}
		out[id] = label(labels, device) + " " + label(labels, account) + " " + hex.EncodeToString(hash) + " " + string(bytes.TrimSpace(raw))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
