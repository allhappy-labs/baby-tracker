package loadtest

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// importResolver is a fake resolver that maps every import it is sent and
// records the chunks in order; refuse, when set, answers a chunk instead.
type importResolver struct {
	mu     sync.Mutex
	chunks []protocol.ImportResolveRequest
	refuse func(request protocol.ImportResolveRequest, bearer string) (status int, code string)
}

func (r *importResolver) install(f *fakeV1) {
	f.hook("POST /v1/family/import/resolve", func(w http.ResponseWriter, hr *http.Request, body []byte) bool {
		var request protocol.ImportResolveRequest
		_ = json.Unmarshal(body, &request)
		r.mu.Lock()
		r.chunks = append(r.chunks, request)
		refuse := r.refuse
		r.mu.Unlock()
		if refuse != nil {
			if status, code := refuse(request, strings.TrimPrefix(hr.Header.Get("Authorization"), "Bearer ")); status != 0 {
				writeErrorResponse(w, status, code)
				return true
			}
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"import_id": request.ImportID, "resolver_revision": 1, "mappings": []any{},
			"conflicts": []any{}, "mapping_complete": request.Finalize, "manifest_complete": request.Finalize})
		return true
	})
}

func (r *importResolver) sent() []protocol.ImportResolveRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]protocol.ImportResolveRequest(nil), r.chunks...)
}

// An import's plan is a function of its id and freeze time, as the iOS
// importer's frozen manifest is its bytes: a resumed import resends the
// very chunks and mutations the broken run sent.
func TestImportPlanIsFrozenByItsIDAndTime(t *testing.T) {
	id, at := newUUID(), time.Now().UTC().Truncate(time.Microsecond)
	first, err := newImportPlan(id, at)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := newImportPlan(id, at)
	a, _ := json.Marshal(first.chunks)
	b, _ := json.Marshal(again.chunks)
	m, _ := json.Marshal(first.mutations)
	n, _ := json.Marshal(again.mutations)
	if string(a) != string(b) || string(m) != string(n) {
		t.Fatal("the same import planned twice differs")
	}
	other, _ := newImportPlan(newUUID(), at)
	if other.mutations[0].EntityID == first.mutations[0].EntityID || other.mutations[0].MutationID == first.mutations[0].MutationID {
		t.Fatal("two imports share record or mutation ids")
	}
	if first.importID != id || first.chunks[0].ImportID != id || *first.mutations[0].ImportID != id {
		t.Fatal("the plan does not carry its import id")
	}
}

// The import is checkpointed (the device's PendingImport) before its first
// chunk is sent and cleared once its last record is applied; an import that
// fails keeps it for the next run.
func TestImportIsCheckpointedUntilItsRecordsApply(t *testing.T) {
	f := newFakeV1(t)
	r := &importResolver{}
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	var pendingAtChunk0 *PendingImport
	r.refuse = func(request protocol.ImportResolveRequest, _ string) (int, string) {
		if request.ChunkIndex == 0 {
			pendingAtChunk0 = d.state().PendingImport
		}
		if request.ChunkIndex == 1 {
			return http.StatusServiceUnavailable, "service_unavailable"
		}
		return 0, ""
	}
	r.install(f)
	if g.importRecords(t.Context(), d, time.Now(), &importStats{}) {
		t.Fatal("an import whose final chunk failed completed")
	}
	pending := d.state().PendingImport
	if pendingAtChunk0 == nil || pending == nil || pending.ImportID != pendingAtChunk0.ImportID || pending.ImportID != r.sent()[0].ImportID {
		t.Fatalf("pending at chunk 0 %+v, after the failure %+v", pendingAtChunk0, pending)
	}
	if err := g.checkpoint(); err != nil {
		t.Fatal(err)
	}
	_, states, err := ReadFleet(g.cfg.Fleet)
	if err != nil {
		t.Fatal(err)
	}
	if states[0].PendingImport == nil || *states[0].PendingImport != *pending {
		t.Fatalf("the checkpointed fleet's pending import %+v, want %+v", states[0].PendingImport, pending)
	}
}

// The next run's cell resumes the device's unfinished import first, as the
// iOS importer resumes its frozen manifest: the same chunks (their replays
// answer as before) and the same mutations, then the cell's own import.
// Only then is the checkpoint cleared.
func TestTheNextRunResumesAnUnfinishedImport(t *testing.T) {
	f := newFakeV1(t)
	r := &importResolver{}
	r.install(f)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	left := PendingImport{ImportID: newUUID(), FrozenAt: time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)}
	d.PendingImport = &left
	stats := &importStats{}
	if !g.importRecords(t.Context(), d, time.Now(), stats) {
		t.Fatalf("resolve %v write %v", g.outcomes(RouteImportResolve), g.outcomes(RouteImportWrite))
	}
	sent := r.sent()
	if len(sent) != 4 || sent[0].ImportID != left.ImportID || sent[1].ImportID != left.ImportID || sent[2].ImportID == left.ImportID {
		t.Fatalf("chunks sent: %d, ids %v", len(sent), importIDs(sent))
	}
	plan, _ := newImportPlan(left.ImportID, left.FrozenAt)
	want, _ := json.Marshal(plan.chunks[0])
	got, _ := json.Marshal(sent[0])
	if string(want) != string(got) {
		t.Fatal("the resumed chunk 0 is not the frozen one")
	}
	if stats.resumed.Load() != 1 || d.state().PendingImport != nil || g.ledgered.Load() != 2*importRecords {
		t.Fatalf("resumed %d, pending %+v, ledgered %d", stats.resumed.Load(), d.state().PendingImport, g.ledgered.Load())
	}
}

// An unfinished import past the server's staging TTL has expired there (it
// no longer counts as unfinished): it is dropped unsent.
func TestAnExpiredUnfinishedImportIsDropped(t *testing.T) {
	f := newFakeV1(t)
	r := &importResolver{}
	r.install(f)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	left := PendingImport{ImportID: newUUID(), FrozenAt: time.Now().Add(-importStagingTTL - time.Minute).UTC()}
	d.PendingImport = &left
	stats := &importStats{}
	if !g.importRecords(t.Context(), d, time.Now(), stats) {
		t.Fatal("the import failed")
	}
	for _, chunk := range r.sent() {
		if chunk.ImportID == left.ImportID {
			t.Fatal("the expired import was resent")
		}
	}
	if stats.resumed.Load() != 0 || len(r.sent()) != 2 {
		t.Fatalf("resumed %d, %d chunks", stats.resumed.Load(), len(r.sent()))
	}
}

// An owner whose account holds an unfinished import this generator cannot
// resume (left by a run before the checkpoint, or by another client) is
// refused import_unfinished_limit on a new import's chunk 0: the cell
// records that as import_leftover (expected), marks the owner for the
// staging TTL, and imports with the next owner, so a valid cell needs no
// fresh seed. Later cells skip the marked owner without asking.
func TestTheCellMovesPastAnOwnerWithALeftoverImport(t *testing.T) {
	f := newFakeV1(t)
	r := &importResolver{}
	g := testGenerator(t, f, 4)
	extras := importExtras(g)
	cell := extras[0]
	owners := importCandidates(g)
	blocked := owners[0]
	blockedBearer := blocked.access()
	r.refuse = func(request protocol.ImportResolveRequest, bearer string) (int, string) {
		if request.ChunkIndex == 0 && bearer == blockedBearer {
			return http.StatusConflict, "import_unfinished_limit"
		}
		return 0, ""
	}
	r.install(f)
	cell.op(t.Context(), time.Now())
	if got := g.outcomes(RouteImportResolve); got[OutcomeImportLeftover] != 1 || got[OutcomeOK] != 2 {
		t.Fatalf("resolve outcomes %v", got)
	}
	state := blocked.state()
	if state.ImportBlockedUntil == nil || state.ImportBlockedUntil.Before(time.Now().Add(importStagingTTL-time.Minute)) || state.PendingImport != nil {
		t.Fatalf("blocked owner %+v", state)
	}
	if g.ledgered.Load() != importRecords {
		t.Fatalf("ledgered %d", g.ledgered.Load())
	}
	if v := EvaluateWindow(RunSummary{Routes: g.phases.measure.Snapshot()}, SpecThresholds()); unexpectedReason(v) {
		t.Fatalf("the leftover counted as unexpected: %v", v.Reasons)
	}
	// A later cell (the next run) does not ask the marked owner again.
	g2 := testGenerator(t, f, 4)
	for _, d := range g2.devices {
		if d.AccountID == blocked.AccountID && d.DeviceID == blocked.DeviceID {
			d.ImportBlockedUntil = state.ImportBlockedUntil
		}
	}
	if first := importCandidates(g2)[0]; first.DeviceID == blocked.DeviceID {
		t.Fatal("the marked owner is still the first candidate")
	}
}

func importIDs(chunks []protocol.ImportResolveRequest) []string {
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.ImportID
	}
	return ids
}
