//go:build identitytest

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"babytracker/server/internal/protocol"
)

func TestValidatePreservedMappingsRequiresCanonicalLocalIDs(t *testing.T) {
	localIDs := []string{
		"12000000-0000-4000-8000-000000000201",
		"12000000-0000-4000-8000-000000000202",
	}
	response := protocol.ImportResolveResponse{MappingComplete: true, Mappings: []protocol.BabyMapping{
		{LocalBabyID: localIDs[0], SharedBabyID: localIDs[0]},
		{LocalBabyID: localIDs[1], SharedBabyID: localIDs[1]},
	}}
	if err := validatePreservedMappings(response, localIDs); err != nil {
		t.Fatal(err)
	}
	response.Mappings[1].SharedBabyID = "12000000-0000-4000-8000-000000000299"
	if err := validatePreservedMappings(response, localIDs); err == nil {
		t.Fatal("distinct but remapped preserve_local_ids response was accepted")
	}
}

func TestValidateCompletedFeedRequiresExactSingleMaterialization(t *testing.T) {
	const timerID = "51000000-0000-4000-8000-000000000201"
	const recordID = "21000000-0000-4000-8000-000000000201"
	const babyID = "11000000-0000-4000-8000-000000000101"
	ended := "2026-09-20T15:10:00.000000Z"
	side, seconds := "left", uint32(600)
	expected := protocol.FeedPayload{BabyID: babyID, Kind: "breast", StartedAt: "2026-09-20T15:00:00.000000Z", EndedAt: &ended,
		TimeZoneID: "Europe/Zurich", Components: []protocol.FeedComponent{
			{ID: "21100000-0000-4000-8000-000000000201", Kind: "breast", Side: &side, Seconds: &seconds}}}
	entity := protocol.CanonicalEntity{EntityKind: "feed", Meta: protocol.EntityMeta{ID: recordID}, Payload: expected}
	outcome := protocol.Outcome{RelatedEntities: []protocol.CanonicalEntity{entity}}
	changes := []protocol.JournalChange{
		{EntityKind: "timer", EntityID: timerID, Operation: "timer_stop"},
		{EntityKind: "feed", EntityID: recordID, Operation: "timer_stop", SnapshotOrTombstone: entity},
	}
	if err := validateCompletedFeed(outcome, changes, timerID, recordID, expected); err != nil {
		t.Fatal(err)
	}
	changes = append(changes, changes[1])
	if err := validateCompletedFeed(outcome, changes, timerID, recordID, expected); err == nil {
		t.Fatal("duplicate completed-feed materialization was accepted")
	}
	changes = changes[:2]
	wrong := expected
	wrong.BabyID = "11000000-0000-4000-8000-000000000199"
	changes[1].SnapshotOrTombstone.Payload = wrong
	if err := validateCompletedFeed(outcome, changes, timerID, recordID, expected); err == nil {
		t.Fatal("completed feed with wrong baby reference was accepted")
	}
}

func TestRecoveryVerifyRefusesMissingExpectation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeState(path, savedState{}); err != nil {
		t.Fatal(err)
	}
	if err := (client{}).recoveryVerify(context.Background(), path); err == nil {
		t.Fatal("missing process expectation accepted")
	}
}
func TestRecoveryVerifyRefusesIncompleteExpectation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeState(path, savedState{Recovery: &recoveryExpectation{AttemptID: "80000000-0000-4000-8000-000000000001"}}); err != nil {
		t.Fatal(err)
	}
	if err := (client{}).recoveryVerify(context.Background(), path); err == nil {
		t.Fatal("incomplete process expectation accepted")
	}
}

func TestExchangeUsesAdvertisedAppleAuthorizationMode(t *testing.T) {
	for _, mode := range []string{"identity_token_only", "code_exchange_required"} {
		t.Run(mode, func(t *testing.T) {
			seen := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/server" {
					json.NewEncoder(w).Encode(protocol.ServerInfo{AppleAuthorizationMode: mode})
					return
				}
				var request protocol.AppleExchangeRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				seen = request.AuthorizationCode
				json.NewEncoder(w).Encode(protocol.SessionResponse{})
			}))
			defer server.Close()
			c := client{base: server.URL, http: server.Client()}
			if _, err := c.exchange(t.Context(), protocol.AppleExchangeRequest{AuthorizationCode: "synthetic-code"}); err != nil {
				t.Fatal(err)
			}
			want := "synthetic-code"
			if mode == "identity_token_only" {
				want = ""
			}
			if seen != want {
				t.Fatalf("mode=%s sent authorization code=%q want=%q", mode, seen, want)
			}
		})
	}
}
