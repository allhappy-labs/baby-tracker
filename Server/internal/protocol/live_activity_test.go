package protocol_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
)

const liveActivityScopeJSON = `{"server_instance_id":"22222222-2222-4222-8222-222222222222","recovery_epoch":1,"family_id":"33333333-3333-4333-8333-333333333333","baby_id":"44444444-4444-4444-8444-444444444444","kind_raw":"feed"}`
const liveActivityDeviceJSON = `{"protocol_version":"v1","capabilities":["live-activity-push-v1"],"installation_id":"11111111-1111-4111-8111-111111111111","registration_version":1,"apns_environment":"sandbox","payload_version":1,"push_to_start_token":"aabb","feed_break_minutes":30}`
const liveActivityTokenJSON = `{"protocol_version":"v1","capabilities":["live-activity-push-v1"],"installation_id":"11111111-1111-4111-8111-111111111111","activity_id":"activity-1","token":"aabb","token_version":1,"scope":` + liveActivityScopeJSON + `,"state":"active"}`

func TestLiveActivityRegistrationBounds(t *testing.T) {
	decode := func(raw string) (protocol.LiveActivityDeviceRegistration, error) {
		var input protocol.LiveActivityDeviceRegistration
		err := protocol.DecodeRequest(strings.NewReader(raw), 32768, &input)
		if err == nil {
			err = input.Validate()
		}
		return input, err
	}
	valid, err := decode(liveActivityDeviceJSON)
	if err != nil || valid.FeedBreakMinutes != 30 {
		t.Fatalf("valid/default: %+v, %v", valid, err)
	}
	withoutPreference := strings.Replace(liveActivityDeviceJSON, `,"feed_break_minutes":30`, ``, 1)
	valid, err = decode(withoutPreference)
	if err != nil || valid.FeedBreakMinutes != 30 {
		t.Fatalf("missing preference: %+v, %v", valid, err)
	}
	for _, raw := range []string{
		strings.Replace(liveActivityDeviceJSON, `"feed_break_minutes":30`, `"feed_break_minutes":5`, 1),
		strings.Replace(liveActivityDeviceJSON, `"feed_break_minutes":30`, `"feed_break_minutes":120`, 1),
		strings.Replace(liveActivityDeviceJSON, `aabb`, strings.Repeat("ab", 1024), 1),
		strings.Replace(liveActivityDeviceJSON, `"registration_version":1`, `"registration_version":9007199254740991`, 1),
	} {
		if _, err := decode(raw); err != nil {
			t.Fatalf("valid boundary: %v", err)
		}
	}
	for _, change := range [][2]string{
		{"aabb", ""}, {"aabb", "abc"}, {"aabb", "AABB"}, {"aabb", "zz"}, {"aabb", strings.Repeat("ab", 1025)},
		{`"feed_break_minutes":30`, `"feed_break_minutes":0`}, {`"feed_break_minutes":30`, `"feed_break_minutes":6`}, {`"feed_break_minutes":30`, `"feed_break_minutes":125`},
		{`"payload_version":1`, `"payload_version":2`}, {`"payload_version":1`, `"payload_version":1.5`},
		{"11111111-1111-4111-8111-111111111111", "not-a-uuid"}, {"11111111-1111-4111-8111-111111111111", "00000000-0000-0000-0000-000000000000"},
		{`"registration_version":1`, `"registration_version":0`}, {`"registration_version":1`, `"registration_version":1.5`},
		{`"registration_version":1`, `"registration_version":9007199254740992`}, {`"registration_version":1`, `"registration_version":1e0`},
		{"sandbox", "development"}, {"v1", "v2"}, {"live-activity-push-v1", "records-v1"},
		{`"feed_break_minutes":30`, `"feed_break_minutes":30,"account_id":"11111111-1111-4111-8111-111111111111"`},
		{`"feed_break_minutes":30`, `"feed_break_minutes":30,"feed_break_minutes":5`},
	} {
		if _, err := decode(strings.Replace(liveActivityDeviceJSON, change[0], change[1], 1)); err == nil {
			t.Fatalf("accepted invalid %q", change)
		}
	}
}

func TestLiveActivityTokenRegistrationBounds(t *testing.T) {
	decode := func(raw string) (protocol.LiveActivityTokenRegistration, error) {
		var input protocol.LiveActivityTokenRegistration
		err := protocol.DecodeRequest(strings.NewReader(raw), 32768, &input)
		if err == nil {
			err = input.Validate()
		}
		return input, err
	}
	for _, state := range []string{"active", "ended", "dismissed"} {
		if _, err := decode(strings.Replace(liveActivityTokenJSON, "active", state, 1)); err != nil {
			t.Fatalf("valid state: %v", err)
		}
	}
	for _, change := range [][2]string{
		{"activity-1", ""}, {"activity-1", strings.Repeat("a", 257)}, {"aabb", "abc"}, {"active", "unknown"},
		{`"token_version":1`, `"token_version":0`}, {`"token_version":1`, `"token_version":9007199254740992`},
		{`"recovery_epoch":1`, `"recovery_epoch":0`}, {"feed", "diaper"},
		{"11111111-1111-4111-8111-111111111111", "not-a-uuid"},
		{`"kind_raw":"feed"`, `"kind_raw":"feed","account_id":"11111111-1111-4111-8111-111111111111"`},
	} {
		if _, err := decode(strings.Replace(liveActivityTokenJSON, change[0], change[1], 1)); err == nil {
			t.Fatalf("accepted invalid %q", change)
		}
	}
	for _, field := range []string{"known_generation_id", "timer_id", "session_id"} {
		for _, value := range []string{"not-a-uuid", ""} {
			body := strings.TrimSuffix(liveActivityTokenJSON, "}") + `,"` + field + `":"` + value + `"}`
			if _, err := decode(body); err == nil {
				t.Fatalf("accepted invalid %s=%q", field, value)
			}
		}
	}
}

func TestLiveActivityScopeCannotCrossFamily(t *testing.T) {
	var input protocol.LiveActivityTokenRegistration
	if err := protocol.DecodeRequest(strings.NewReader(liveActivityTokenJSON), 32768, &input); err != nil {
		t.Fatal(err)
	}
	scope := input.Scope
	if !scope.MatchesRealm(scope.ServerInstanceID, 1, scope.FamilyID) || scope.MatchesRealm(scope.ServerInstanceID, 1, "66666666-6666-4666-8666-666666666666") ||
		scope.MatchesRealm("66666666-6666-4666-8666-666666666666", 1, scope.FamilyID) || scope.MatchesRealm(scope.ServerInstanceID, 2, scope.FamilyID) {
		t.Fatal("scope failed realm fence")
	}
}

func TestLiveActivityFixtureParity(t *testing.T) {
	read := func(name string) []byte {
		data, err := os.ReadFile("../../api/fixtures/live-activity-v1/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var device protocol.LiveActivityDeviceRegistration
	if err := protocol.DecodeRequest(strings.NewReader(string(read("device-registration.json"))), 32768, &device); err != nil {
		t.Fatal(err)
	}
	if err := device.Validate(); err != nil {
		t.Fatal(err)
	}
	var token protocol.LiveActivityTokenRegistration
	if err := protocol.DecodeRequest(strings.NewReader(string(read("token-registration.json"))), 32768, &token); err != nil {
		t.Fatal(err)
	}
	if err := token.Validate(); err != nil {
		t.Fatal(err)
	}
	var receipt protocol.LiveActivityRegistrationReceipt
	if err := json.Unmarshal(read("registration-receipt.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	if token.KnownGenerationID == nil || receipt.Generations[0].GenerationID != *token.KnownGenerationID || receipt.AcceptedTokenVersion == nil || *receipt.AcceptedTokenVersion != token.TokenVersion {
		t.Fatal("receipt mapping mismatch")
	}
	var states map[string]liveactivity.ContentState
	raw := read("content-states.json")
	if err := json.Unmarshal(raw, &states); err != nil {
		t.Fatal(err)
	}
	if len(states) != 8 {
		t.Fatalf("states=%d", len(states))
	}
	for name, state := range states {
		if err := state.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if states["breast-left"].StartedAt != 812678400 || states["breast-left"].Remote.PresentationTimestamp != 1790985600 {
		t.Fatal("Foundation/APNs date epochs differ from golden literals")
	}
	if liveactivity.FoundationDate(1790985600) != 812678400 {
		t.Fatal("Unix-to-Foundation date conversion is incorrect")
	}
	revision := *states["breast-left"].Remote
	if !reflect.DeepEqual(liveactivity.NewRevision(revision.Protocol()), revision) {
		t.Fatal("payload/registration adapter changed revision identity")
	}
	encoded, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("Go JSON mapping lost or changed golden fields")
	}
	var legacy liveactivity.ContentState
	if err := json.Unmarshal(read("legacy-content.json"), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Remote != nil || legacy.OmittedParts != 0 || legacy.Phase != "running" {
		t.Fatalf("legacy defaults: %+v", legacy)
	}
}

func TestLiveActivityCapabilityIsOptional(t *testing.T) {
	if err := protocol.ValidateCapabilities([]string{protocol.CapabilityLiveActivityPushV1}, protocol.KnownCapabilities()); err != nil {
		t.Fatal(err)
	}
	for _, value := range protocol.ServerCapabilities() {
		if value == protocol.CapabilityLiveActivityPushV1 {
			t.Fatal("unconfigured default advertises push")
		}
	}
	raw, err := os.ReadFile("../../api/fixtures/server-info.json")
	if err != nil {
		t.Fatal(err)
	}
	var server protocol.ServerInfo
	if err := json.Unmarshal(raw, &server); err != nil {
		t.Fatal(err)
	}
	server.Capabilities = append(server.Capabilities, protocol.CapabilityLiveActivityPushV1)
	if err := server.Validate(); err != nil {
		t.Fatalf("configured capability advertisement: %v", err)
	}
}

func TestLiveActivityRevisionAndReceiptBounds(t *testing.T) {
	var token protocol.LiveActivityTokenRegistration
	if err := protocol.DecodeRequest(strings.NewReader(liveActivityTokenJSON), 32768, &token); err != nil {
		t.Fatal(err)
	}
	revision := protocol.LiveActivityRevision{Scope: token.Scope, GenerationID: "55555555-5555-4555-8555-555555555555", PresentationVersion: 1, FamilySequence: 0, PresentationTimestamp: 1790985600}
	if err := revision.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"generation", "timer", "session", "sequence", "version", "timestamp"} {
		bad := revision
		invalidID := "not-a-uuid"
		switch field {
		case "generation":
			bad.GenerationID = "not-a-uuid"
		case "timer":
			bad.TimerID = &invalidID
		case "session":
			bad.SessionID = &invalidID
		case "sequence":
			bad.FamilySequence = protocol.MaxSafeInteger + 1
		case "version":
			bad.PresentationVersion = 0
		case "timestamp":
			bad.PresentationTimestamp = protocol.MaxSafeInteger + 1
		}
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid revision %s", field)
		}
	}
	receipt := protocol.LiveActivityRegistrationReceipt{ProtocolVersion: "v1", ServerInstanceID: token.Scope.ServerInstanceID,
		RecoveryEpoch: 1, AcceptedRegistrationVersion: 1,
		Generations: []protocol.LiveActivityGenerationMapping{{ActivityID: "activity-1", Scope: token.Scope, GenerationID: revision.GenerationID}}}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"realm", "epoch", "registration_version", "token_version", "duplicate", "generation"} {
		bad := receipt
		bad.Generations = append([]protocol.LiveActivityGenerationMapping{}, receipt.Generations...)
		switch field {
		case "realm":
			bad.Generations[0].Scope.ServerInstanceID = "66666666-6666-4666-8666-666666666666"
		case "epoch":
			bad.RecoveryEpoch = 2
		case "registration_version":
			bad.AcceptedRegistrationVersion = 0
		case "token_version":
			value := protocol.MaxSafeInteger + 1
			bad.AcceptedTokenVersion = &value
		case "duplicate":
			bad.Generations = append(bad.Generations, bad.Generations[0])
		case "generation":
			bad.Generations[0].GenerationID = "not-a-uuid"
		}
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid receipt %s", field)
		}
	}
}

func TestLiveActivityForegroundInventory(t *testing.T) {
	valid := protocol.LiveActivityForegroundInventory{InventoryVersion: 1, AbsentGenerations: []protocol.LiveActivityAbsentGeneration{{GenerationID: "11111111-1111-4111-8111-111111111111", ObservedAttemptID: "22222222-2222-4222-8222-222222222222"}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := valid
	bad.InventoryVersion = 0
	if bad.Validate() == nil {
		t.Fatal("zero inventory accepted")
	}
	bad = valid
	bad.AbsentGenerations = append(bad.AbsentGenerations, bad.AbsentGenerations[0])
	if bad.Validate() == nil {
		t.Fatal("duplicate generation accepted")
	}
	bad = valid
	bad.AbsentGenerations = make([]protocol.LiveActivityAbsentGeneration, 17)
	if bad.Validate() == nil {
		t.Fatal("unbounded proof accepted")
	}
}
