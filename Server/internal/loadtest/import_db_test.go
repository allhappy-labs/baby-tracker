package loadtest

import (
	"encoding/json"
	"strings"
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// The import cell's plan, sent to the real resolver and sync service as the
// generator sends it: every chunk resolves (chunk 0 carries the baby, the
// later ones none, as the resolver requires), the final one completes the
// manifest and the mapping, and every record then applies.
func TestImportPlanCompletesOnTheRealResolver(t *testing.T) {
	h := testkit.New(t)
	plan, err := newImportPlan(newUUID(), h.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.chunks) != 2 {
		t.Fatalf("%d chunks", len(plan.chunks))
	}
	var last protocol.ImportResolveResponse
	for i, chunk := range plan.chunks {
		// Through the wire, as the handler reads it ("babies": null is refused).
		raw, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		var decoded protocol.ImportResolveRequest
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("chunk %d does not decode: %v", i, err)
		}
		last, err = h.Importer.Resolve(t.Context(), h.Owner, decoded)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	if !last.ManifestComplete || !last.MappingComplete || last.ConflictCount != 0 {
		t.Fatalf("final chunk: manifest %v mapping %v conflicts %d", last.ManifestComplete, last.MappingComplete, last.ConflictCount)
	}
	applied := 0
	for start := 0; start < len(plan.mutations); start += importBatch {
		request := h.SyncRequest(nil, nil)
		request.Mutations = plan.mutations[start:min(start+importBatch, len(plan.mutations))]
		response, err := h.Service.ApplyAndPull(t.Context(), h.Owner, request)
		if err != nil {
			t.Fatalf("batch at %d: %v", start, err)
		}
		for _, o := range testkit.Decoded(t, response).Outcomes {
			if o.Status != "applied" {
				t.Fatalf("batch at %d: outcome %s %v", start, o.Status, o.Code)
			}
			applied++
		}
	}
	if applied != importRecords {
		t.Fatalf("%d records applied", applied)
	}
}

// A run broken after chunk 0 leaves the owner's account an unfinished
// import, and the resolver refuses that account any new one
// (import_unfinished_limit: one per account) until the import finishes or
// its staging TTL passes. The next run rebuilds the frozen plan from the
// checkpointed id and time and resends it: chunk 0's replay is accepted,
// the final chunk completes the mapping, and the account may import again.
func TestAResumedImportFreesTheAccountOnTheRealResolver(t *testing.T) {
	h := testkit.New(t)
	pending := PendingImport{ImportID: newUUID(), FrozenAt: h.Now.UTC()}
	broken, err := newImportPlan(pending.ImportID, pending.FrozenAt)
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(chunk protocol.ImportResolveRequest) (protocol.ImportResolveResponse, error) {
		raw, _ := json.Marshal(chunk)
		var decoded protocol.ImportResolveRequest
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		return h.Importer.Resolve(t.Context(), h.Owner, decoded)
	}
	if _, err := resolve(broken.chunks[0]); err != nil {
		t.Fatal(err)
	}
	fresh, _ := newImportPlan(newUUID(), h.Now)
	if _, err := resolve(fresh.chunks[0]); err == nil || !strings.Contains(err.Error(), "import_unfinished_limit") {
		t.Fatalf("a new import beside the unfinished one: %v", err)
	}
	resumed, _ := newImportPlan(pending.ImportID, pending.FrozenAt)
	var last protocol.ImportResolveResponse
	for i, chunk := range resumed.chunks {
		if last, err = resolve(chunk); err != nil {
			t.Fatalf("resumed chunk %d: %v", i, err)
		}
	}
	if !last.MappingComplete {
		t.Fatal("the resumed import did not complete its mapping")
	}
	if _, err := resolve(fresh.chunks[0]); err != nil {
		t.Fatalf("a new import after the resumed one finished: %v", err)
	}
}
