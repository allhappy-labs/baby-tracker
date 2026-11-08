package loadtest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
)

// A rejected_mutation records each refused mutation's status and code in
// the route's rejections, so a report can judge them (the hot family's
// 759 on the load VM had no reason).
func TestRejectedMutationsRecordTheirReasons(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.writers[0]
	f.hook("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
		var request struct {
			Mutations []json.RawMessage `json:"mutations"`
		}
		_ = json.Unmarshal(body, &request)
		outcomes := []protocol.Outcome{}
		codes := []string{"record_content_conflict", "baby_deleted", "Not A Code!"}
		for i, raw := range request.Mutations {
			m, _ := protocol.DecodeMutation(raw)
			code := codes[i%len(codes)]
			outcomes = append(outcomes, protocol.Outcome{MutationID: m.MutationID, Status: "rejected", Code: &code,
				RelatedEntities: []protocol.CanonicalEntity{}})
		}
		writeJSONResponse(w, http.StatusOK, protocol.SyncResponse{ServerInstanceID: f.instance, RecoveryEpoch: 1, Outcomes: outcomes,
			Changes: []protocol.JournalChange{}, HighWater: g.cursor(d.FamilyID, 0)})
		return true
	})
	g.write(t.Context(), d, time.Now(), RouteSyncWrite, 3)
	route := g.phases.measure.Snapshot()[RouteSyncWrite]
	if route.Outcomes[OutcomeRejected] != 1 {
		t.Fatalf("outcomes %v", route.Outcomes)
	}
	want := map[string]uint64{"rejected:record_content_conflict": 1, "rejected:baby_deleted": 1, "rejected:other": 1}
	if len(route.Rejections) != len(want) {
		t.Fatalf("rejections %v, want %v", route.Rejections, want)
	}
	for reason, n := range want {
		if route.Rejections[reason] != n {
			t.Fatalf("rejections %v, want %v", route.Rejections, want)
		}
	}
	raw, _ := json.Marshal(route)
	var decoded struct {
		Rejections map[string]uint64 `json:"rejections"`
	}
	if json.Unmarshal(raw, &decoded) != nil || decoded.Rejections["rejected:baby_deleted"] != 1 {
		t.Fatalf("result JSON %s", raw)
	}
	if ok, _ := json.Marshal(RouteSummary{Outcomes: map[string]uint64{OutcomeOK: 1}}); strings.Contains(string(ok), "rejections") {
		t.Fatalf("a route with no rejection writes the key: %s", ok)
	}
}

// Rejection reasons reconcile with the outcomes: they are filed only for
// a request recorded rejected_mutation, never for one whose outcome was
// rewritten (a protocol answer, another family's entity, the restart
// window).
func TestRejectionReasonsAreFiledOnlyForRejectedMutations(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	g.record(RouteSyncWrite, time.Now(), OutcomeProtocol, "rejected:baby_deleted")
	g.record(RouteSyncWrite, time.Now(), OutcomeCrossFamily, "rejected:baby_deleted")
	g.phases.restartFrom, g.phases.restartTo = time.Now().Add(-time.Minute), time.Now().Add(time.Minute)
	g.record(RouteSyncWrite, time.Now(), OutcomeRejected, "rejected:baby_deleted")
	g.phases.restartFrom, g.phases.restartTo = time.Time{}, time.Time{}
	g.record(RouteSyncWrite, time.Now(), OutcomeRejected, "rejected:record_deleted_conflict")
	route := g.phases.measure.Snapshot()[RouteSyncWrite]
	if route.Outcomes[OutcomeRestartWindow] != 1 || len(route.Rejections) != 1 || route.Rejections["rejected:record_deleted_conflict"] != 1 {
		t.Fatalf("outcomes %v, rejections %v", route.Outcomes, route.Rejections)
	}
}

// A request answered with a refused mutation and a missing outcome is a
// protocol outcome, and files no reason.
func TestAProtocolWriteFilesNoRejectionReason(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.writers[0]
	f.hook("POST /v1/sync", func(w http.ResponseWriter, _ *http.Request, body []byte) bool {
		var request struct {
			Mutations []json.RawMessage `json:"mutations"`
		}
		_ = json.Unmarshal(body, &request)
		m, _ := protocol.DecodeMutation(request.Mutations[0])
		code := "baby_deleted"
		writeJSONResponse(w, http.StatusOK, protocol.SyncResponse{ServerInstanceID: f.instance, RecoveryEpoch: 1,
			Outcomes: []protocol.Outcome{{MutationID: m.MutationID, Status: "rejected", Code: &code, RelatedEntities: []protocol.CanonicalEntity{}}},
			Changes:  []protocol.JournalChange{}, HighWater: g.cursor(d.FamilyID, 0)})
		return true
	})
	g.write(t.Context(), d, time.Now(), RouteSyncWrite, 2)
	route := g.phases.measure.Snapshot()[RouteSyncWrite]
	if route.Outcomes[OutcomeProtocol] != 1 || len(route.Rejections) != 0 {
		t.Fatalf("outcomes %v, rejections %v", route.Outcomes, route.Rejections)
	}
}
