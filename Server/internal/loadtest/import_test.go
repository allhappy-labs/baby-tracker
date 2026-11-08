package loadtest

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// The resolver's refusals (httpapi writeImportError) are named outcomes,
// not http_4xx_other, so a broken import cell can be read from its result.
func TestImportRefusalsAreNamedOutcomes(t *testing.T) {
	for status, codes := range map[int][]string{
		http.StatusConflict: {"import_history_limit", "import_unfinished_limit", "import_chunk_reused", "import_chunk_out_of_order",
			"import_manifest_changed", "import_manifest_incomplete", "import_resolution_stale", "import_target_already_assigned",
			"baby_uuid_collision", "command_id_reused", "import_page_limit", "family_storage_limit"},
		http.StatusGone:      {"import_expired"},
		http.StatusForbidden: {"import_access_denied", "owner_required"},
		http.StatusNotFound:  {"import_not_found"},
	} {
		for _, code := range codes {
			if got := classify(status, code); got != "http_4xx_"+code {
				t.Errorf("%d %s classified %s", status, code, got)
			}
		}
	}
}

// The plan follows the resolver's chunk rules: chunk 0 freezes the baby
// candidates, every later chunk sends "babies": [] (never null), and only
// the last finalizes, bringing the count to its declaration.
func TestImportPlanSendsTheBabiesOnlyOnChunkZero(t *testing.T) {
	plan, err := newImportPlan(newUUID(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	records := 0
	for i, chunk := range plan.chunks {
		raw, _ := json.Marshal(chunk)
		var wire struct {
			Babies json.RawMessage `json:"babies"`
		}
		_ = json.Unmarshal(raw, &wire)
		switch {
		case i == 0 && len(chunk.Babies) != 1:
			t.Fatalf("chunk 0 has %d babies", len(chunk.Babies))
		case i > 0 && string(wire.Babies) != "[]":
			t.Fatalf("chunk %d babies %s", i, wire.Babies)
		case chunk.Finalize != (i == len(plan.chunks)-1) || int(chunk.ChunkIndex) != i:
			t.Fatalf("chunk %d: index %d finalize %v", i, chunk.ChunkIndex, chunk.Finalize)
		}
		records += len(chunk.Records)
	}
	if len(plan.chunks) < 2 || records != importRecords || len(plan.mutations) != importRecords {
		t.Fatalf("%d chunks, %d manifest records, %d mutations", len(plan.chunks), records, len(plan.mutations))
	}
}

// An import whose final chunk leaves the mapping incomplete (conflicts an
// owner must decide) writes nothing: its records would be refused.
func TestImportWritesNothingUntilTheMappingIsComplete(t *testing.T) {
	f := newFakeV1(t)
	f.hook("POST /v1/family/import/resolve", func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
		var request protocol.ImportResolveRequest
		_ = json.Unmarshal(body, &request)
		writeJSONResponse(w, http.StatusOK, map[string]any{"import_id": request.ImportID, "resolver_revision": 1, "mappings": []any{},
			"conflicts": []any{}, "mapping_complete": false, "manifest_complete": request.Finalize})
		return true
	})
	g := testGenerator(t, f, 1)
	if g.importRecords(t.Context(), g.devices[0], time.Now(), &importStats{}) {
		t.Fatal("an unmapped import completed")
	}
	if f.count("POST /v1/sync") != 0 {
		t.Fatalf("%d writes of an unmapped import", f.count("POST /v1/sync"))
	}
	if got := g.outcomes(RouteImportResolve); got[OutcomeOK] != 1 || got[OutcomeImportUnmapped] != 1 {
		t.Fatalf("resolve outcomes %v", got)
	}
}
