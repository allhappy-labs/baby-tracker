package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestTimedBottleFinishMutationAndStoredTimerRoundTrip(t *testing.T) {
	raw := []byte(`{"mutation_id":"01000000-0000-4000-8000-000000000980","entity_id":"50000000-0000-4000-8000-000000000980","entity_kind":"timer","operation":"timer_finish_bottle","payload_version":5,"observed_entity_version":2,"client_mutation_time":"2026-09-28T12:05:00Z","payload":{"baby_id":"10000000-0000-4000-8000-000000000001","finished_at":"2026-09-28T12:05:00Z","component_id":"30000000-0000-4000-8000-000000000980","volume_ml":50,"contents":"formula"}}`)
	mutation, err := DecodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if mutation.PayloadVersion != 5 {
		t.Fatalf("mutation=%+v", mutation)
	}
	if _, ok := mutation.Payload.(TimerBottleFinishPayload); !ok {
		t.Fatalf("payload=%T", mutation.Payload)
	}
	for _, older := range []string{`"payload_version":3`, `"payload_version":4`} {
		if _, err := DecodeMutation(bytes.Replace(raw, []byte(`"payload_version":5`), []byte(older), 1)); ErrorCodeOf(err) != "invalid_request" {
			t.Fatalf("%s finish bottle err=%v", older, err)
		}
	}
	at := "2026-09-28T12:00:00.000000Z"
	entity := CanonicalEntity{EntityKind: "timer", Meta: EntityMeta{ID: "50000000-0000-4000-8000-000000000980", FamilyID: "60000000-0000-4000-8000-000000000001", EntityVersion: 2, CreatorAccountID: "70000000-0000-4000-8000-000000000001", LastEditorAccountID: "70000000-0000-4000-8000-000000000001", ClientMutationTime: at, AcceptedAt: at}, Payload: TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed", StartedAt: at, TimeZoneID: "UTC", CurrentBottleStartedAt: &at, CompletedComponents: []FeedComponent{}}}
	encoded, err := json.Marshal(entity)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CanonicalEntity
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("readback: %v: %s", err, encoded)
	}
	payload := decoded.Payload.(TimerStartPayload)
	if payload.CurrentBottleStartedAt == nil || *payload.CurrentBottleStartedAt != at {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestTimedBottleFeedIsAcceptedOnlyAtTheCurrentVersion(t *testing.T) {
	raw := []byte(`{"mutation_id":"01000000-0000-4000-8000-000000000981","entity_id":"20000000-0000-4000-8000-000000000981","entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,"client_mutation_time":"2026-09-28T12:05:00Z","payload":{"baby_id":"10000000-0000-4000-8000-000000000001","kind":"bottle","started_at":"2026-09-28T12:00:00Z","ended_at":"2026-09-28T12:05:00Z","time_zone_id":"UTC","feeding_session_id":null,"components":[{"id":"30000000-0000-4000-8000-000000000981","kind":"bottle","seconds":300,"volume_ml":50,"contents":"formula"}]}}`)
	if _, err := DecodeMutation(raw); err != nil {
		t.Fatal(err)
	}
	// The same timed bottle as an older client sent it: v3, without the
	// session key, and v2, which never allowed bottle seconds at all.
	withoutSession := bytes.Replace(raw, []byte(`"feeding_session_id":null,`), nil, 1)
	for _, older := range []string{`"payload_version":3`, `"payload_version":2`} {
		legacy := bytes.Replace(withoutSession, []byte(`"payload_version":4`), []byte(older), 1)
		if _, err := DecodeMutation(legacy); ErrorCodeOf(err) != "invalid_request" {
			t.Fatalf("%s accepted: %v", older, err)
		}
	}
}

func TestV4FeedPreservesClearedSessionLinkAndStepBounds(t *testing.T) {
	raw := []byte(`{"mutation_id":"01000000-0000-4000-8000-000000000982","entity_id":"20000000-0000-4000-8000-000000000982","entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,"client_mutation_time":"2026-09-28T12:05:00Z","payload":{"baby_id":"10000000-0000-4000-8000-000000000001","kind":"bottle","started_at":"2026-09-28T12:00:00Z","ended_at":"2026-09-28T12:02:00Z","time_zone_id":"UTC","feeding_session_id":null,"components":[{"id":"30000000-0000-4000-8000-000000000982","kind":"bottle","seconds":120,"volume_ml":60,"contents":"formula","started_at":"2026-09-28T12:00:00Z","ended_at":"2026-09-28T12:02:00Z"}]}}`)
	mutation, err := DecodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	feed := mutation.Payload.(FeedPayload)
	if feed.FeedingSessionID != nil || len(feed.Components) != 1 || feed.Components[0].StartedAt == nil || *feed.Components[0].StartedAt != "2026-09-28T12:00:00.000000Z" {
		t.Fatalf("decoded=%+v", feed)
	}
	canonical, err := CanonicalMutationBytes(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(canonical, []byte(`"feeding_session_id":null`)) || !bytes.Contains(canonical, []byte(`"ended_at":"2026-09-28T12:02:00.000000Z"`)) {
		t.Fatalf("canonical=%s", canonical)
	}
}
