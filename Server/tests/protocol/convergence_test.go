//go:build identitytest

package protocol_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"github.com/coder/websocket"
)

func TestTwoClientHTTPSAndLiveHintConverge(t *testing.T) {
	fixture := newProtocolFixture(t)
	header := http.Header{"Authorization": []string{"Bearer " + fixture.h.AccessToken("caregiver")}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	connection, _, err := websocket.Dial(t.Context(), fixture.server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	applied, err := fixture.Owner().Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())})
	if err != nil || len(applied.Outcomes) != 1 || applied.Outcomes[0].Cursor == nil {
		t.Fatalf("apply=%+v err=%v", applied, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, raw, err := connection.Read(ctx)
	if err != nil || len(raw) == 0 {
		t.Fatalf("hint bytes=%d err=%v", len(raw), err)
	}
	pulled, err := fixture.Caregiver().Sync(nil)
	if err != nil || len(pulled.Changes) != 1 || pulled.HighWater != *applied.Outcomes[0].Cursor {
		t.Fatalf("pull=%+v err=%v", pulled, err)
	}
}

func TestAllRecordTypesConvergeAndSnapshotThroughHTTP(t *testing.T) {
	fixture := newProtocolFixture(t)
	mutations := make([]protocol.Mutation, 0, 7)
	for _, name := range []string{"baby.json", "feed-bottle.json", "feed-breast.json", "feed-combination.json", "feed-solids.json", "sleep.json", "diaper.json"} {
		value := mutation(t, name, fixture.h.CommandID())
		// The canonical combination fixture is an update vector. This scenario
		// creates one entity of every supported record shape from an empty realm.
		if name == "feed-combination.json" {
			value.Operation = "create"
		}
		mutations = append(mutations, value)
	}
	applied, err := fixture.Owner().Sync(mutations)
	if err != nil || len(applied.Outcomes) != len(mutations) {
		t.Fatalf("outcomes=%d err=%v", len(applied.Outcomes), err)
	}
	pulled, err := fixture.Caregiver().Sync(nil)
	if err != nil || len(pulled.Changes) != len(mutations) {
		t.Fatalf("changes=%d err=%v", len(pulled.Changes), err)
	}
	kinds := map[string]int{}
	for _, change := range pulled.Changes {
		kinds[change.EntityKind]++
	}
	if kinds["baby"] != 1 || kinds["feed"] != 4 || kinds["sleep"] != 1 || kinds["diaper"] != 1 {
		t.Fatalf("kinds=%v", kinds)
	}
	snapshot, err := fixture.Caregiver().Snapshot()
	if err != nil || snapshot.NextPageToken != nil || len(snapshot.Entities) != len(mutations) || snapshot.HighWater != pulled.HighWater {
		t.Fatalf("snapshot entities=%d high_water=%+v err=%v", len(snapshot.Entities), snapshot.HighWater, err)
	}
}

func TestReorderedAndLostHintsRemainAdvisory(t *testing.T) {
	fixture := newProtocolFixture(t)
	header := http.Header{"Authorization": []string{"Bearer " + fixture.h.AccessToken("caregiver")}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	connection, _, err := websocket.Dial(t.Context(), fixture.server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	high := realtime.Hint{FamilyID: fixture.h.FamilyID, Cursor: *fixture.h.Cursor(2)}
	fixture.hub.Publish(t.Context(), high)
	fixture.hub.Publish(t.Context(), realtime.Hint{FamilyID: fixture.h.FamilyID, Cursor: *fixture.h.Cursor(1)})
	readContext, cancel := context.WithTimeout(t.Context(), time.Second)
	_, raw, err := connection.Read(readContext)
	cancel()
	connection.CloseNow()
	var got realtime.Hint
	if err != nil || json.Unmarshal(raw, &got) != nil || got.Cursor.Sequence != 2 {
		t.Fatalf("hint=%s decoded=%+v err=%v", raw, got, err)
	}
	// No socket is open for the actual commit. Ordinary sync must recover it.
	if _, err = fixture.Owner().Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}
	fixture.caregiver.cursor = nil
	pulled, err := fixture.Caregiver().Sync(nil)
	if err != nil || len(pulled.Changes) != 1 || pulled.Changes[0].Cursor.Sequence != 1 {
		t.Fatalf("lost-hint reconciliation=%+v err=%v", pulled, err)
	}
}

func TestMemberRemovalClosesLiveLeaseAndDeniesFurtherSync(t *testing.T) {
	fixture := newProtocolFixture(t)
	header := http.Header{"Authorization": []string{"Bearer " + fixture.h.AccessToken("caregiver")}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	connection, _, err := websocket.Dial(t.Context(), fixture.server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	if _, err = fixture.h.Family.Remove(t.Context(), fixture.h.Owner, fixture.h.CommandID(), fixture.h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	readContext, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err = connection.Read(readContext)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close=%d err=%v", websocket.CloseStatus(err), err)
	}
	if _, err = fixture.Caregiver().Sync(nil); err == nil {
		t.Fatal("removed caregiver retained sync access")
	}
}
