package care_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// sameJSON compares two JSON documents byte for byte once compacted.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var ca, cb bytes.Buffer
	if err := json.Compact(&ca, a); err != nil {
		t.Fatalf("compact %s: %v", a, err)
	}
	if err := json.Compact(&cb, b); err != nil {
		t.Fatalf("compact %s: %v", b, err)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// wantReceipt requires the stored receipt of p's mutation to be outcome as
// encoded (jsonb decoded back into an outcome and re-encoded).
func wantReceipt(t *testing.T, h *testkit.Harness, p access.Principal, outcome protocol.Outcome) {
	t.Helper()
	stored, found := caretest.StoredReceipt(t, h, p, outcome.MutationID)
	if !found {
		t.Fatalf("no receipt for %s", outcome.MutationID)
	}
	var decoded protocol.Outcome
	if err := json.Unmarshal(stored, &decoded); err != nil {
		t.Fatal(err)
	}
	if got, want := mustJSON(t, decoded), mustJSON(t, outcome); !bytes.Equal(got, want) {
		t.Fatalf("receipt %s\n got %s\nwant %s", outcome.MutationID, got, want)
	}
}

func wantNoReceipt(t *testing.T, h *testkit.Harness, p access.Principal, mutationID string) {
	t.Helper()
	if _, found := caretest.StoredReceipt(t, h, p, mutationID); found {
		t.Fatalf("receipt for %s written", mutationID)
	}
}

func familyStateOf(t *testing.T, h *testkit.Harness) care.FamilyState {
	t.Helper()
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	defer func() { _ = tx.Rollback(t.Context()) }()
	state, err := care.ReadFamilyState(t.Context(), tx, h.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func deleteBaby(id string) protocol.Mutation { return caretest.DeleteBaby(id) }

// Review Focus 2: a batch refused at its k-th mutation commits mutations
// 1..k-1 with their receipts, as the legacy per-mutation transactions left
// them, and the retry replays them.
func TestBatchFatalErrorCommitsPrefix(t *testing.T) {
	t.Run("quota", func(t *testing.T) {
		h := testkit.New(t)
		h.Limits.MaxLiveBabies = 1
		caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
		batch := []protocol.Mutation{caretest.Feed(caretest.BabyA, 1), caretest.Baby(caretest.BabyB), caretest.Feed(caretest.BabyA, 2)}
		result := caretest.ApplyCommitted(t, h, h.Owner, batch)
		if !errors.Is(result.Fatal, care.ErrQuotaExceeded) || len(result.Outcomes) != 1 {
			t.Fatalf("%v %d", result.Fatal, len(result.Outcomes))
		}
		if result.Base != 1 || result.HighWater != 2 || result.Outcomes[0].Cursor == nil || result.Outcomes[0].Cursor.Sequence != 2 {
			t.Fatalf("base %d high water %d outcome %+v", result.Base, result.HighWater, result.Outcomes[0])
		}
		rows := caretest.Rows(t, h)
		if _, ok := rows[batch[0].EntityID]; !ok {
			t.Fatal("prefix feed not committed")
		}
		for _, m := range batch[1:] {
			if _, ok := rows[m.EntityID]; ok {
				t.Fatalf("%s after the refusal committed", m.EntityKind)
			}
			wantNoReceipt(t, h, h.Owner, m.MutationID)
		}
		wantReceipt(t, h, h.Owner, result.Outcomes[0])
		if state := familyStateOf(t, h); state.Sequence != 2 {
			t.Fatalf("family sequence %d", state.Sequence)
		}
		replay := caretest.ApplyCommitted(t, h, h.Owner, batch[:1])
		if len(replay.Outcomes) != 1 || replay.Outcomes[0].Status != "already_applied" || replay.Outcomes[0].OriginalStatus == nil ||
			*replay.Outcomes[0].OriginalStatus != "applied" || replay.Outcomes[0].Cursor.Sequence != 2 || replay.HighWater != 2 {
			t.Fatalf("retry %+v", replay)
		}
	})

	t.Run("mutation id reused", func(t *testing.T) {
		h := testkit.New(t)
		caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
		first := caretest.Feed(caretest.BabyA, 1)
		reused := caretest.Feed(caretest.BabyA, 2)
		reused.MutationID = first.MutationID
		result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{first, reused, caretest.Feed(caretest.BabyA, 3)})
		if !errors.Is(result.Fatal, care.ErrMutationIDReused) || len(result.Outcomes) != 1 {
			t.Fatalf("%v %d", result.Fatal, len(result.Outcomes))
		}
		wantReceipt(t, h, h.Owner, result.Outcomes[0])
		// Against the committed receipt, in a later batch.
		later := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{caretest.Feed(caretest.BabyA, 4), reused})
		if !errors.Is(later.Fatal, care.ErrMutationIDReused) || len(later.Outcomes) != 1 || later.Outcomes[0].Status != "applied" {
			t.Fatalf("%v %+v", later.Fatal, later.Outcomes)
		}
		rows := caretest.Rows(t, h)
		for id, want := range map[string]bool{first.EntityID: true, reused.EntityID: false, caretest.RecordID("feed", caretest.BabyA, 3): false,
			caretest.RecordID("feed", caretest.BabyA, 4): true} {
			if _, ok := rows[id]; ok != want {
				t.Fatalf("row %s present %t", id, ok)
			}
		}
	})

	t.Run("owner required", func(t *testing.T) {
		h := testkit.New(t)
		caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
		batch := []protocol.Mutation{caretest.Feed(caretest.BabyA, 1), deleteBaby(caretest.BabyA), caretest.Feed(caretest.BabyA, 2)}
		result := caretest.ApplyCommitted(t, h, h.Caregiver, batch)
		if !errors.Is(result.Fatal, authority.ErrOwnerRequired) || len(result.Outcomes) != 1 || result.HighWater != 2 {
			t.Fatalf("%v %d %d", result.Fatal, len(result.Outcomes), result.HighWater)
		}
		wantReceipt(t, h, h.Caregiver, result.Outcomes[0])
		wantNoReceipt(t, h, h.Caregiver, batch[1].MutationID)
		if rows := caretest.Rows(t, h); !rows[caretest.BabyA].Live() || len(rows) != 2 {
			t.Fatalf("rows %d", len(rows))
		}
		replay := caretest.ApplyCommitted(t, h, h.Caregiver, batch[:1])
		if replay.Outcomes[0].Status != "already_applied" {
			t.Fatalf("retry %+v", replay.Outcomes[0])
		}
	})

	t.Run("viewer writes nothing", func(t *testing.T) {
		h := testkit.New(t)
		caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
		viewer := h.AddMember(t, "viewer", "viewer")
		before := familyStateOf(t, h)
		batch := []protocol.Mutation{caretest.Feed(caretest.BabyA, 1)}
		result := caretest.ApplyCommitted(t, h, viewer, batch)
		if !errors.Is(result.Fatal, authority.ErrReadOnlyCaregiver) || len(result.Outcomes) != 0 || result.HighWater != result.Base {
			t.Fatalf("%v %d", result.Fatal, len(result.Outcomes))
		}
		if _, ok := caretest.Rows(t, h)[batch[0].EntityID]; ok {
			t.Fatal("viewer's feed written")
		}
		wantNoReceipt(t, h, viewer, batch[0].MutationID)
		if after := familyStateOf(t, h); after != before {
			t.Fatalf("family %+v, was %+v", after, before)
		}
	})
}

// Review Focus 5: one entity mutated several times in one offline batch
// chains through the working set and is written once, at its last
// sequence.
func TestBatchChainsMutationsOnOneEntity(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	const base = 1
	create := caretest.Feed(caretest.BabyA, 1)
	update := caretest.Update(create, "0e000001-0000-4000-8000-0000000000a1", 1, caretest.WithNote("chained"))
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{create, update, update})
	if result.Fatal != nil || len(result.Outcomes) != 3 || result.Base != base || result.HighWater != base+2 {
		t.Fatalf("%v %d outcomes, base %d high water %d", result.Fatal, len(result.Outcomes), result.Base, result.HighWater)
	}
	o := result.Outcomes
	for i, want := range []struct {
		status  string
		version uint64
		seq     uint64
	}{{"applied", 1, base + 1}, {"applied", 2, base + 2}, {"already_applied", 2, base + 2}} {
		if o[i].Status != want.status || o[i].CanonicalEntity == nil || o[i].CanonicalEntity.Meta.EntityVersion != want.version ||
			o[i].Cursor == nil || o[i].Cursor.Sequence != want.seq || o[i].OverwroteNewerVersion {
			t.Fatalf("outcome %d: %+v", i, o[i])
		}
	}
	if o[2].OriginalStatus == nil || *o[2].OriginalStatus != "applied" {
		t.Fatalf("replay %+v", o[2])
	}
	row := caretest.Rows(t, h)[create.EntityID]
	if row.EntityVersion != 2 || row.ChangedSeq != base+2 || row.Operation != "update" || !bytes.Equal(row.Wire, mustJSON(t, o[1].CanonicalEntity)) {
		t.Fatalf("row v%d seq %d %s: %s", row.EntityVersion, row.ChangedSeq, row.Operation, row.Wire)
	}
	if state := familyStateOf(t, h); state.Sequence != base+2 || state.EntityCount != 2 || state.RecordCount != 1 {
		t.Fatalf("family %+v", state)
	}
	wantReceipt(t, h, h.Owner, o[0])
	wantReceipt(t, h, h.Owner, o[1])
	// The in-batch replay is the stored receipt's replay, byte for byte.
	later := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{update})
	if !bytes.Equal(mustJSON(t, later.Outcomes[0]), mustJSON(t, o[2])) {
		t.Fatalf("replays differ\n in batch %s\n    later %s", mustJSON(t, o[2]), mustJSON(t, later.Outcomes[0]))
	}

	// The same mutation id with another payload in one batch stops the
	// batch after the two outcomes before it.
	second := caretest.Feed(caretest.BabyA, 2)
	edit := caretest.Update(second, "0e000002-0000-4000-8000-0000000000a1", 1, caretest.WithNote("x"))
	clash := caretest.Update(second, edit.MutationID, 1, caretest.WithNote("y"))
	result = caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{second, edit, clash})
	if !errors.Is(result.Fatal, care.ErrMutationIDReused) || len(result.Outcomes) != 2 || result.HighWater != base+4 {
		t.Fatalf("%v %d %d", result.Fatal, len(result.Outcomes), result.HighWater)
	}
	if row := caretest.Rows(t, h)[second.EntityID]; row.EntityVersion != 2 || row.ChangedSeq != base+4 || !strings.Contains(string(row.Wire), `"note":"x"`) {
		t.Fatalf("row v%d seq %d %s", row.EntityVersion, row.ChangedSeq, row.Wire)
	}
}

func TestLastWriterWinsAndOverwroteNewer(t *testing.T) {
	h := testkit.New(t)
	feed := caretest.Feed(caretest.BabyA, 1)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), feed,
		caretest.Update(feed, "0e000001-0000-4000-8000-0000000000a1", 1, caretest.WithNote("v2")),
		caretest.Update(feed, "0e000002-0000-4000-8000-0000000000a1", 2, caretest.WithNote("v3")))
	for _, c := range []struct {
		observed uint64
		newer    bool
	}{{1, true}, {3, false}} {
		update := caretest.Update(feed, fmt.Sprintf("0e%06d-0000-4000-8000-0000000000a1", 10+c.observed), c.observed, caretest.WithNote("v4"))
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		result, err := care.ApplyBatch(t.Context(), tx, caretest.Command(t, h, h.Owner, []protocol.Mutation{update}), nil)
		if err != nil || result.Fatal != nil {
			t.Fatal(err, result.Fatal)
		}
		o := result.Outcomes[0]
		if o.Status != "applied" || o.CanonicalEntity.Meta.EntityVersion != 4 || o.OverwroteNewerVersion != c.newer ||
			o.CanonicalEntity.Meta.CreatorAccountID != h.Owner.AccountID || o.Cursor.Sequence != 5 {
			t.Fatalf("observed %d: %+v", c.observed, o)
		}
		_ = tx.Rollback(t.Context())
	}
	// Another account's update keeps the creator and names the editor.
	update := caretest.Update(feed, "0e000020-0000-4000-8000-0000000000a1", 3, caretest.WithNote("caregiver"))
	o := caretest.ApplyCommitted(t, h, h.Caregiver, []protocol.Mutation{update}).Outcomes[0]
	if meta := o.CanonicalEntity.Meta; meta.CreatorAccountID != h.Owner.AccountID || meta.LastEditorAccountID != h.Caregiver.AccountID ||
		meta.EntityVersion != 4 {
		t.Fatalf("meta %+v", meta)
	}
	if row := caretest.Rows(t, h)[feed.EntityID]; row.CreatorAccountID != h.Owner.AccountID || row.ActorAccountID != h.Caregiver.AccountID ||
		row.DeviceID != h.Caregiver.DeviceID {
		t.Fatalf("row %+v", row)
	}
}

// applyIn applies batch in family as account/device with role, without a
// policy, and commits.
func applyIn(t *testing.T, h *testkit.Harness, grant access.Grant, device string, batch []protocol.Mutation) care.BatchResult {
	t.Helper()
	tx := caretest.TenantTx(t, h.Runtime, grant.AccountID, grant.FamilyID)
	result, err := care.ApplyBatch(t.Context(), tx, care.BatchCommand{Grant: grant, DeviceID: device, Mutations: batch, AcceptedAt: h.Now,
		Server: h.Info, Limits: h.Limits}, nil)
	if err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return result
}

// The receipt read admits the account's receipts from its other families;
// only this family's receipt of the same mutation replays.
func TestReceiptReplayAndCrossFamilyReuse(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	feed := caretest.Feed(caretest.BabyA, 1)
	applied := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{feed}).Outcomes[0]
	replay := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{feed})
	if want := applied; replay.Outcomes[0].Status != "already_applied" || replay.HighWater != 2 ||
		!bytes.Equal(mustJSON(t, protocol.Outcome{MutationID: want.MutationID, Status: "already_applied", OriginalStatus: &want.Status,
			CanonicalEntity: want.CanonicalEntity, RelatedEntities: want.RelatedEntities, Cursor: want.Cursor}), mustJSON(t, replay.Outcomes[0])) {
		t.Fatalf("replay %s", mustJSON(t, replay.Outcomes[0]))
	}

	// The outsider logs a feed in its own family B, then its device sends
	// the very same mutation in family A.
	foreign, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "Foreign")
	if err != nil {
		t.Fatal(err)
	}
	h.Outsider = h.Refresh("outsider")
	inB := access.Grant{AccountID: h.Outsider.AccountID, FamilyID: foreign.ID, Role: "owner"}
	babyB := caretest.Baby(caretest.BabyB)
	feedB := caretest.Feed(caretest.BabyB, 1)
	if r := applyIn(t, h, inB, h.Outsider.DeviceID, []protocol.Mutation{babyB, feedB}); r.Fatal != nil || r.Outcomes[1].Status != "applied" {
		t.Fatalf("family B: %+v", r)
	}
	inA := access.Grant{AccountID: h.Outsider.AccountID, FamilyID: h.FamilyID, Role: "caregiver"}
	before := familyStateOf(t, h)
	feedA := caretest.Feed(caretest.BabyA, 2)
	feedA.MutationID = feedB.MutationID
	for _, batch := range [][]protocol.Mutation{{feedA, caretest.Feed(caretest.BabyA, 3)}, {feedB}} {
		r := applyIn(t, h, inA, h.Outsider.DeviceID, batch)
		if !errors.Is(r.Fatal, care.ErrMutationIDReused) || len(r.Outcomes) != 0 || r.HighWater != before.Sequence {
			t.Fatalf("reuse in family A: %v %+v", r.Fatal, r.Outcomes)
		}
	}
	if after := familyStateOf(t, h); after != before {
		t.Fatalf("family A %+v, was %+v", after, before)
	}
	if rows := caretest.Rows(t, h); len(rows) != 2 {
		t.Fatalf("family A rows %d", len(rows))
	}
}

// The receipt insert keeps the legacy definer's device rule: the device is
// the account's and active, or nothing is written.
func TestReceiptWriteRequiresTheAccountsActiveDevice(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	for name, device := range map[string]string{"another account's device": h.Caregiver.DeviceID,
		"unknown device": "d1000000-0000-4000-8000-0000000000ff"} {
		cmd := caretest.Command(t, h, h.Owner, []protocol.Mutation{caretest.Feed(caretest.BabyA, 1)})
		cmd.DeviceID = device
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		if _, err := care.ApplyBatch(t.Context(), tx, cmd, nil); !errors.Is(err, care.ErrConflict) {
			t.Fatalf("%s: %v, want ErrConflict", name, err)
		}
		_ = tx.Rollback(t.Context())
	}
}

// Fetch takes the family lock first, so a batch waiting on a concurrent
// one reads its committed rows: no lost update, consecutive sequences.
func TestConcurrentBatchesSerializeOnTheFamilyLock(t *testing.T) {
	h := testkit.New(t)
	feed := caretest.Feed(caretest.BabyA, 1)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), feed)
	first := caretest.Update(feed, "0e000001-0000-4000-8000-0000000000a1", 1, caretest.WithNote("first"))
	second := caretest.Update(feed, "0e000002-0000-4000-8000-0000000000a1", 1, caretest.WithNote("second"))

	tx1 := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	r1, err := care.ApplyBatch(t.Context(), tx1, caretest.Command(t, h, h.Owner, []protocol.Mutation{first}), nil)
	if err != nil || r1.Fatal != nil {
		t.Fatal(err, r1.Fatal)
	}
	cmd2 := caretest.Command(t, h, h.Caregiver, []protocol.Mutation{second})
	type applied struct {
		result care.BatchResult
		err    error
	}
	done := make(chan applied, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		tx2, err := h.Runtime.Begin(ctx)
		if err != nil {
			done <- applied{err: err}
			return
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		if err := tenant.SetLocal(ctx, tx2, tenant.Context{AccountID: h.Caregiver.AccountID, FamilyID: h.FamilyID}); err != nil {
			done <- applied{err: err}
			return
		}
		result, err := care.ApplyBatch(ctx, tx2, cmd2, &care.PullRequest{After: 2, Limit: 10})
		if err == nil {
			err = tx2.Commit(ctx)
		}
		done <- applied{result, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("the second batch did not wait for the family lock: %+v %v", got.result, got.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx1.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.result.Fatal != nil {
		t.Fatal(got.err, got.result.Fatal)
	}
	o := got.result.Outcomes[0]
	if got.result.Base != 3 || got.result.HighWater != 4 || o.CanonicalEntity.Meta.EntityVersion != 3 || o.Cursor.Sequence != 4 || !o.OverwroteNewerVersion {
		t.Fatalf("second batch saw a stale row: base %d %+v", got.result.Base, o)
	}
	if len(got.result.Changes) != 1 || got.result.Changes[0].Seq != 4 || got.result.Changes[0].Version != 3 {
		t.Fatalf("pull %+v", got.result.Changes)
	}
	row := caretest.Rows(t, h)[feed.EntityID]
	if row.EntityVersion != 3 || row.ChangedSeq != 4 || !strings.Contains(string(row.Wire), `"note":"second"`) {
		t.Fatalf("row v%d seq %d", row.EntityVersion, row.ChangedSeq)
	}
}

type stubImports struct {
	calls   []string
	handled map[string]protocol.Outcome
	seen    map[string]bool
}

func (s *stubImports) AdmitRecord(_ context.Context, _ pgx.Tx, _ access.Grant, _ string, m protocol.Mutation, view care.View) (protocol.Outcome, bool, error) {
	s.calls = append(s.calls, m.MutationID)
	_, found := view.Lookup(caretest.BabyA)
	s.seen[m.MutationID] = found
	outcome, ok := s.handled[m.MutationID]
	return outcome, ok, nil
}

func TestImportTaggedMutationsGoThroughAdmission(t *testing.T) {
	h := testkit.New(t)
	importID := "0c000000-0000-4000-8000-000000000001"
	tag := func(m protocol.Mutation) protocol.Mutation { m.ImportID = &importID; return m }
	baby := caretest.Baby(caretest.BabyA)
	admitted, plain := tag(caretest.Feed(caretest.BabyA, 1)), tag(caretest.Feed(caretest.BabyA, 2))

	// Without an admission an import-tagged mutation stops the batch.
	result := caretest.ApplyCommitted(t, h, h.Owner, []protocol.Mutation{baby, admitted})
	if !errors.Is(result.Fatal, care.ErrFeatureUnavailable) || len(result.Outcomes) != 1 {
		t.Fatalf("%v %d", result.Fatal, len(result.Outcomes))
	}
	code := "import_not_found"
	stub := &stubImports{seen: map[string]bool{}, handled: map[string]protocol.Outcome{admitted.MutationID: {MutationID: admitted.MutationID,
		Status: "rejected", Code: &code, RelatedEntities: []protocol.CanonicalEntity{}}}}
	cmd := caretest.Command(t, h, h.Owner, []protocol.Mutation{admitted, plain})
	cmd.Imports = stub
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	result, err := care.ApplyBatch(t.Context(), tx, cmd, nil)
	if err != nil || result.Fatal != nil {
		t.Fatal(err, result.Fatal)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 2 || !stub.seen[admitted.MutationID] {
		t.Fatalf("admission calls %v seen %v", stub.calls, stub.seen)
	}
	if o := result.Outcomes; caretest.Code(o[0]) != code || o[1].Status != "applied" || o[1].Cursor.Sequence != 2 || result.HighWater != 2 {
		t.Fatalf("outcomes %+v", o)
	}
	wantReceipt(t, h, h.Owner, result.Outcomes[0])
	wantReceipt(t, h, h.Owner, result.Outcomes[1])
}

// The family counters are the aggregates of the family's rows after any
// mix of creates, updates, deletes and refusals, and every row's charge is
// the legacy entity charge plus the live reserve. Charged bytes also count
// retained import staging, decisions and ID markers (care.ChargeImport),
// which wantCountersMatchRows reconstructs from the import tables.
func TestCountersMatchRows(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Baby(caretest.BabyB))
	random := rand.New(rand.NewPCG(5, 7))
	kinds := []string{"feed", "sleep", "diaper", "supplement", "measurement"}
	type entity struct{ create protocol.Mutation }
	var entities []*entity
	mutations := 0
	for mutations < 200 {
		size := 1 + random.IntN(20)
		var batch []protocol.Mutation
		for range size {
			mutations++
			id := fmt.Sprintf("0f%06d-0000-4000-8000-%012d", mutations, mutations)
			switch r := random.IntN(10); {
			case r < 4 || len(entities) == 0:
				baby := []string{caretest.BabyA, caretest.BabyB}[random.IntN(2)]
				m := caretest.Record(kinds[random.IntN(len(kinds))], baby, mutations)
				batch = append(batch, m)
				entities = append(entities, &entity{create: m})
			case r < 8:
				e := entities[random.IntN(len(entities))]
				edit := caretest.WithNote(strings.Repeat("n", random.IntN(300)))
				if e.create.EntityKind == "measurement" {
					edit = nil
				}
				batch = append(batch, caretest.Update(e.create, id, uint64(random.IntN(3)), edit))
			default:
				e := entities[random.IntN(len(entities))]
				batch = append(batch, caretest.Delete(e.create, id, 1))
			}
		}
		result := caretest.ApplyCommitted(t, h, h.Owner, batch)
		if result.Fatal != nil || len(result.Outcomes) != len(batch) {
			t.Fatalf("batch: %v, %d of %d outcomes", result.Fatal, len(result.Outcomes), len(batch))
		}
	}

	rows := wantCountersMatchRows(t, h)
	tombstones := 0
	for _, r := range rows {
		if !r.Live() {
			tombstones++
		}
	}
	if tombstones == 0 || len(rows) < 50 {
		t.Fatalf("%d rows, %d tombstones: the mix did not exercise deletes", len(rows), tombstones)
	}

	// Timer lifecycles (a completion takes the reserve the stopped timer
	// releases) and a cascade (every child releases its reserve and leaves
	// the record count) keep the counters on the rows.
	start, finish, edit, stop := caretest.FeedTimerLifecycle(caretest.BabyA, caretest.TimerA, caretest.CompletionA)
	sleepTimer := "50000000-0000-4000-8000-0000000000b2"
	for _, batch := range [][]protocol.Mutation{
		{start, finish, edit, stop},
		{caretest.StartTimer(caretest.BabyB, sleepTimer, "sleep"), caretest.StartTimer(caretest.BabyB, caretest.TimerB, "feed"),
			caretest.Stop(caretest.BabyB, sleepTimer, "31000000-0000-4000-8000-0000000000b2")},
		{deleteBaby(caretest.BabyB)},
	} {
		result := caretest.ApplyCommitted(t, h, h.Owner, batch)
		if result.Fatal != nil || len(result.Outcomes) != len(batch) {
			t.Fatalf("batch: %v, %d of %d outcomes", result.Fatal, len(result.Outcomes), len(batch))
		}
		for i, o := range result.Outcomes {
			if o.Status != "applied" {
				t.Fatalf("%s %s: %+v %s", batch[i].EntityKind, batch[i].Operation, o, caretest.Code(o))
			}
		}
	}
	rows = wantCountersMatchRows(t, h)
	if r := rows[caretest.TimerB]; r.Live() || r.Operation != "delete" {
		t.Fatalf("baby B's running timer after the cascade: %+v", r)
	}
}

// legacyEntityCharge and legacyDeletionReserve are the legacy storage's
// charge of an entity (its encoded canonical entity plus 256 bytes) and the
// reserve it held for a live entity's deletion, copied from the legacy
// records package when it was removed so the row charge keeps an independent
// oracle.
func legacyEntityCharge(entity protocol.CanonicalEntity) (int64, error) {
	raw, err := json.Marshal(entity)
	if err != nil {
		return 0, err
	}
	return int64(len(raw)) + 256, nil
}

const legacyDeletionReserve int64 = 4096

// withReceipt is state charged for outcome's receipt, as a mutation that
// changes no row (a rejection, a no-op) leaves the family.
func withReceipt(t *testing.T, state care.FamilyState, outcome protocol.Outcome) care.FamilyState {
	t.Helper()
	charge, err := caretest.LegacyReceiptCharge(outcome)
	if err != nil {
		t.Fatal(err)
	}
	state.ChargedBytes += uint64(charge)
	return state
}

// wantCountersMatchRows requires the family counters to equal the charge
// recomputed from the family's rows, retained import charges and live
// receipts (caretest.AssertCounters), the family's next sequence to be its
// rows' latest change, every row's charge to be the legacy entity charge
// plus the live reserve, and every wire to be canonical; it returns the
// rows.
func wantCountersMatchRows(t *testing.T, h *testkit.Harness) map[string]care.Row {
	t.Helper()
	caretest.AssertCounters(t, h, h.FamilyID)
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	state, err := care.ReadFamilyState(t.Context(), tx, h.FamilyID)
	if err != nil {
		t.Fatal(err)
	}
	var latest uint64
	if err := tx.QueryRow(t.Context(), `SELECT max(changed_seq) FROM care_entities WHERE family_id = $1`, h.FamilyID).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(t.Context())
	if state.Sequence != latest {
		t.Fatalf("family sequence %d, latest row change %d", state.Sequence, latest)
	}
	rows := caretest.Rows(t, h)
	for _, r := range rows {
		entity, err := r.Entity()
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := legacyEntityCharge(entity)
		if err != nil {
			t.Fatal(err)
		}
		if r.Live() {
			legacy += legacyDeletionReserve
		}
		if r.ChargedBytes() != legacy {
			t.Fatalf("%s %s charged %d, legacy entity charge %d", r.Kind, r.ID, r.ChargedBytes(), legacy)
		}
	}
	wantWireRoundTrips(t, rows)
	return rows
}

type goldenSyncCorpus struct {
	Outcomes  [][]json.RawMessage `json:"outcomes"`
	HighWater []uint64            `json:"high_water"`
	FullPull  struct {
		Changes []json.RawMessage `json:"changes"`
	} `json:"full_pull"`
}

// The legacy corpus applied one batch per step, the import step (11)
// through the real importer (its resolve, then the batch with the
// importer's admission): the outcomes and high waters are the recorded ones
// byte for byte (records, the timed bottle and the sleep timer with their
// completions, refusals and a stale write, the receipt replay, baby B's
// cascade, the import's new and equal records, and the timers left
// running), and the pull is the latest entry per entity of the recorded
// pull.
func TestApplyBatchReproducesLegacyCorpus(t *testing.T) {
	raw, err := os.ReadFile("../sync/testdata/golden/legacy-sync.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus goldenSyncCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	h := testkit.NewAtFixtureClock(t)
	steps := testkit.GoldenSteps(t, h)
	principal := map[string]access.Principal{"owner": h.Owner, "caregiver": h.Caregiver}
	for i, step := range steps {
		cmd := caretest.Command(t, h, principal[step.Subject], step.Mutations)
		if step.Resolve != nil {
			if _, err := h.Importer.Resolve(t.Context(), principal[step.Subject], *step.Resolve); err != nil {
				t.Fatalf("%s: resolve: %v", step.Name, err)
			}
			cmd.Imports = h.Importer
		}
		result := caretest.ApplyCommandCommitted(t, h, principal[step.Subject], cmd)
		if result.Fatal != nil || len(result.Outcomes) != len(corpus.Outcomes[i]) {
			t.Fatalf("%s: %v, %d outcomes", step.Name, result.Fatal, len(result.Outcomes))
		}
		for j, outcome := range result.Outcomes {
			if got := h.Normalize(mustJSON(t, outcome)); !sameJSON(t, got, corpus.Outcomes[i][j]) {
				t.Fatalf("%s outcome %d\n got %s\nwant %s", step.Name, j, got, corpus.Outcomes[i][j])
			}
		}
		if want := corpus.HighWater[i]; result.HighWater != want {
			t.Fatalf("%s: high water %d, want %d", step.Name, result.HighWater, want)
		}
	}
	want := testkit.LatestPerEntity(t, corpus.FullPull.Changes)
	through := corpus.HighWater[len(corpus.HighWater)-1]
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	highWater, changes, err := care.ReadPull(t.Context(), tx, h.FamilyID, 0, 100)
	if err != nil || highWater != through || len(changes) != len(want) {
		t.Fatalf("pull: high water %d, %d changes (want %d), %v", highWater, len(changes), len(want), err)
	}
	for i, c := range changes {
		encoded, err := c.Encode(h.Info, h.FamilyID)
		if err != nil {
			t.Fatal(err)
		}
		if got := h.Normalize(encoded); !sameJSON(t, got, want[i]) {
			t.Fatalf("change %d\n got %s\nwant %s", i, got, want[i])
		}
	}
	wantCountersMatchRows(t, h)
}

// The importer's baby publication is a server-built batch (NoReceipts):
// it writes no receipt, so it charges none. After a publication that
// creates a baby, the counters are the rows plus the retained import
// charges, with no receipt term.
func TestImporterPublicationChargesNoReceipt(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA), caretest.Feed(caretest.BabyA, 1))
	r := testkit.ImportRequest(0, "Published", "2026-01-12")
	receipts := h.Count(t, "care_receipts", h.FamilyID)
	out, err := h.Importer.Resolve(t.Context(), h.Owner, r)
	if err != nil || !out.MappingComplete || len(out.Mappings) != 1 || out.Mappings[0].SharedBabyID != r.Babies[0].LocalBabyID {
		t.Fatalf("publication %+v %v", out, err)
	}
	if baby, ok := caretest.Rows(t, h)[r.Babies[0].LocalBabyID]; !ok || baby.Kind != "baby" || !baby.Live() {
		t.Fatalf("published baby %+v", baby)
	}
	if after := h.Count(t, "care_receipts", h.FamilyID); after != receipts {
		t.Fatalf("publication wrote %d receipts", after-receipts)
	}
	wantCountersMatchRows(t, h)
}

// The family sequence ends at 2^53-1, the largest a cursor carries: a batch
// with room for one more change applies its first mutation and stops at the
// second with ErrSequenceExhausted, the first written with its receipt.
func TestBatchStopsWhenTheFamilySequenceIsExhausted(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	if _, err := tx.Exec(t.Context(), `UPDATE families SET next_sequence = $2 WHERE id = $1`, h.FamilyID, int64(protocol.MaxSafeInteger-1)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	batch := []protocol.Mutation{caretest.Feed(caretest.BabyA, 1), caretest.Feed(caretest.BabyA, 2)}
	result := caretest.ApplyCommitted(t, h, h.Owner, batch)
	if !errors.Is(result.Fatal, care.ErrSequenceExhausted) || len(result.Outcomes) != 1 {
		t.Fatalf("%v, %d outcomes", result.Fatal, len(result.Outcomes))
	}
	if o := result.Outcomes[0]; o.Status != "applied" || o.Cursor.Sequence != protocol.MaxSafeInteger ||
		result.Base != protocol.MaxSafeInteger-1 || result.HighWater != protocol.MaxSafeInteger {
		t.Fatalf("base %d high water %d outcome %+v", result.Base, result.HighWater, o)
	}
	rows := caretest.Rows(t, h)
	if r, ok := rows[batch[0].EntityID]; !ok || r.ChangedSeq != protocol.MaxSafeInteger {
		t.Fatalf("first feed row %+v present %t", r, ok)
	}
	if _, ok := rows[batch[1].EntityID]; ok {
		t.Fatal("second feed written")
	}
	wantReceipt(t, h, h.Owner, result.Outcomes[0])
	wantNoReceipt(t, h, h.Owner, batch[1].MutationID)
	if state := familyStateOf(t, h); state.Sequence != protocol.MaxSafeInteger {
		t.Fatalf("family sequence %d, want one past %d", state.Sequence, protocol.MaxSafeInteger-1)
	}
}

// ActionFor runs for every mutation, policy or not: a kind and operation
// with no action stops the batch with ErrInvalidMutation, as the legacy
// actionFor did, rather than becoming an invalid_mutation outcome.
func TestUnknownActionStopsTheBatchWithoutAPolicy(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	bad := caretest.Feed(caretest.BabyA, 2)
	bad.Operation = "timer_stop" // a feed has no timer_stop; DecodeMutation would refuse it
	if _, err := care.ActionFor(bad); !errors.Is(err, protocol.ErrInvalidMutation) {
		t.Fatalf("ActionFor: %v", err)
	}
	for _, policy := range []care.Policy{nil, caretest.Policy} {
		cmd := caretest.Command(t, h, h.Owner, []protocol.Mutation{caretest.Feed(caretest.BabyA, 1), bad})
		cmd.Policy = policy
		tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
		result, err := care.ApplyBatch(t.Context(), tx, cmd, nil)
		_ = tx.Rollback(t.Context())
		if err != nil || !errors.Is(result.Fatal, protocol.ErrInvalidMutation) || len(result.Outcomes) != 1 || result.Outcomes[0].Status != "applied" {
			t.Fatalf("policy %t: %v %v %+v", policy != nil, err, result.Fatal, result.Outcomes)
		}
	}
}
