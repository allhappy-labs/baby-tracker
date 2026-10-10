package care_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"babytracker/server/internal/care"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// wantWireRoundTrips requires every row's wire to be canonical: decoded and
// encoded again, it is the same bytes (Encode trusts its input to be
// canonical; the write path must only ever hand it canonical entities).
func wantWireRoundTrips(t *testing.T, rows map[string]care.Row) {
	t.Helper()
	for id, r := range rows {
		var entity protocol.CanonicalEntity
		if err := json.Unmarshal(r.Wire, &entity); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		again, err := json.Marshal(entity)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again, r.Wire) {
			t.Fatalf("%s wire is not canonical\n stored %s\nre-encoded %s", id, r.Wire, again)
		}
	}
}

// A sync batch is two round trips whatever its size: Fetch, then the
// pipelined writes with the pull read.
func TestApplyBatchIsTwoRoundTripsAtAnySize(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	pool, wire := testkit.CountedPool(t, h.Runtime)
	for _, size := range []int{1, 20, 100} {
		batch := caretest.MixedBatch(caretest.BabyA, size)
		cmd := caretest.Command(t, h, h.Owner, batch)
		var trips int
		var statements []string
		run := func(measure bool) care.BatchResult {
			tx := caretest.TenantTx(t, pool, h.Owner.AccountID, h.FamilyID)
			defer func() { _ = tx.Rollback(t.Context()) }()
			if measure {
				wire.Reset()
			}
			result, err := care.ApplyBatch(t.Context(), tx, cmd, &care.PullRequest{After: 0, Limit: 201})
			if err != nil || result.Fatal != nil {
				t.Fatalf("size %d: %v %v", size, err, result.Fatal)
			}
			trips, statements = wire.Trips(), wire.Statements()
			if measure {
				wantWireRoundTrips(t, caretest.Rows(t, h)) // committed rows
				rows := map[string]care.Row{}
				for _, c := range result.Changes {
					rows[c.ID] = care.Row{ID: c.ID, Wire: c.Wire}
				}
				wantWireRoundTrips(t, rows) // this batch's rows, as the pull read them
			}
			return result
		}
		run(false) // warms the per-connection statement cache with every statement this shape queues
		result := run(true)
		if trips != 2 || len(result.Outcomes) != size {
			t.Fatalf("size %d: %d round trips, %d outcomes: %q", size, trips, len(result.Outcomes), statements)
		}
		for i, o := range result.Outcomes {
			if o.Status != "applied" {
				t.Fatalf("size %d mutation %d (%s %s): %+v %s", size, i, batch[i].EntityKind, batch[i].Operation, o, caretest.Code(o))
			}
		}
		// Every change of the batch comes back in the pull, each entity once,
		// at its last sequence.
		if want := 1 + size - min(size-1, 1); len(result.Changes) != want || result.HighWater != 1+uint64(size) {
			t.Fatalf("size %d: %d changes (want %d), high water %d", size, len(result.Changes), want, result.HighWater)
		}
	}
}
