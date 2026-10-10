package protocol

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const storedMeta = `{"id":"50000000-0000-4000-8000-000000000001","family_id":"60000000-0000-4000-8000-000000000001",` +
	`"entity_version":1,"creator_account_id":"70000000-0000-4000-8000-000000000001","last_editor_account_id":"70000000-0000-4000-8000-000000000001",` +
	`"client_mutation_time":"2026-09-20T15:00:00.000000Z","accepted_at":"2026-09-20T15:00:00.000000Z","tombstoned_at":null}`

func storedEntity(kind string, payload Payload) CanonicalEntity {
	at := "2026-09-20T15:00:00.000000Z"
	return CanonicalEntity{EntityKind: kind, Payload: payload, Meta: EntityMeta{
		ID: "50000000-0000-4000-8000-000000000001", FamilyID: "60000000-0000-4000-8000-000000000001", EntityVersion: 1,
		CreatorAccountID: "70000000-0000-4000-8000-000000000001", LastEditorAccountID: "70000000-0000-4000-8000-000000000001",
		ClientMutationTime: at, AcceptedAt: at,
	}}
}

func roundTripStored(t *testing.T, entity CanonicalEntity) CanonicalEntity {
	t.Helper()
	raw, err := json.Marshal(entity)
	if err != nil {
		t.Fatal(err)
	}
	var read CanonicalEntity
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatalf("stored %s did not decode: %v\n%s", entity.EntityKind, err, raw)
	}
	return read
}

// A timer the server stored comes back out of a pull, a receipt or a
// snapshot exactly as stored, its session link included whether it is an id
// or an explicit null.
func TestStoredTimerRoundTrip(t *testing.T) {
	left, seconds := "left", uint32(240)
	formula, volume := "formula", MustParseDecimal("60")
	side := "right"
	startedAt := "2026-09-20T15:00:00.000000Z"
	session := "40000000-0000-4000-8000-000000000001"
	for name, payload := range map[string]TimerStartPayload{
		"sleep with no session": {BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "sleep", StartedAt: startedAt,
			TimeZoneID: "Europe/Zurich", CompletedComponents: []FeedComponent{}},
		"feed with a side, a bottle and a session": {BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "feed", StartedAt: startedAt,
			TimeZoneID: "Europe/Zurich", CurrentSide: &side, CurrentSideStartedAt: &startedAt, SessionID: &session,
			CompletedComponents: []FeedComponent{
				{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &seconds},
				{ID: "30000000-0000-4000-8000-000000000002", Kind: "bottle", VolumeML: volume, Contents: &formula},
			}},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := roundTripStored(t, storedEntity("timer", payload)).Payload.(TimerStartPayload)
			if !ok {
				t.Fatalf("payload=%+v", got)
			}
			if !reflect.DeepEqual(got.CompletedComponents, payload.CompletedComponents) {
				t.Fatalf("components=%+v want %+v", got.CompletedComponents, payload.CompletedComponents)
			}
			if !reflect.DeepEqual(got.SessionID, payload.SessionID) {
				t.Fatalf("session_id=%v want %v", got.SessionID, payload.SessionID)
			}
		})
	}
}

func TestStoredSleepSessionIDRoundTrip(t *testing.T) {
	startedAt := "2026-09-20T13:00:00.000000Z"
	session := "40000000-0000-4000-8000-000000000002"
	for name, payload := range map[string]SleepPayload{
		"session present": {BabyID: "10000000-0000-4000-8000-000000000001", StartedAt: startedAt,
			TimeZoneID: "Europe/Zurich", SleepSessionID: &session},
		"session explicitly null": {BabyID: "10000000-0000-4000-8000-000000000001", StartedAt: startedAt,
			TimeZoneID: "Europe/Zurich"},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := roundTripStored(t, storedEntity("sleep", payload)).Payload.(SleepPayload)
			if !ok || !reflect.DeepEqual(got.SleepSessionID, payload.SleepSessionID) {
				t.Fatalf("payload=%+v want session %v", got, payload.SleepSessionID)
			}
		})
	}
}

// Migration 000027 rewrote every journal row and receipt into the current
// shape, so a stored entity in an older shape is no longer read: there is no
// version guess from the keys present and no upgrade.
func TestStoredEntityInAnOlderShapeIsRefused(t *testing.T) {
	const baby = `"baby_id":"10000000-0000-4000-8000-000000000001"`
	const breast = `{"id":"30000000-0000-4000-8000-000000000001","kind":"breast","side":"left","seconds":240}`
	for name, payload := range map[string]struct{ kind, body string }{
		"feed v1 scalars": {"feed", `{` + baby + `,"kind":"breast","started_at":"2026-09-20T15:00:00Z","ended_at":null,"time_zone_id":"UTC","note":null,` +
			`"sides":[{"side":"left","seconds":240}],"volume_ml":null,"contents":null,"foods":[],"amount":null}`},
		"feed v1 scalars beside components, no session key": {"feed", `{` + baby + `,"kind":"breast","started_at":"2026-09-20T15:00:00Z","ended_at":null,"time_zone_id":"UTC","note":null,` +
			`"sides":[{"side":"left","seconds":240}],"volume_ml":null,"contents":null,"foods":[],"amount":null,"components":[` + breast + `]}`},
		"feed v2 without session key": {"feed", `{` + baby + `,"kind":"breast","started_at":"2026-09-20T15:00:00Z","ended_at":null,"time_zone_id":"UTC","note":null,` +
			`"components":[` + breast + `]}`},
		"sleep v1 without session key": {"sleep", `{` + baby + `,"started_at":"2026-09-20T15:00:00Z","ended_at":null,"time_zone_id":"UTC","note":null,"kind_override":null}`},
		"timer v1 completed sides": {"timer", `{` + baby + `,"timer_kind":"feed","started_at":"2026-09-20T15:00:00Z","time_zone_id":"UTC",` +
			`"current_side":"left","current_side_started_at":"2026-09-20T15:00:00Z","completed_sides":[],"pending_volume_ml":null,"pending_contents":null}`},
		"timer v4 without session key": {"timer", `{` + baby + `,"timer_kind":"feed","started_at":"2026-09-20T15:00:00Z","time_zone_id":"UTC",` +
			`"current_side":"left","current_side_started_at":"2026-09-20T15:00:00Z","current_bottle_started_at":null,"completed_components":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			raw := `{"entity_kind":"` + payload.kind + `","meta":` + storedMeta + `,"payload":` + payload.body + `}`
			var read CanonicalEntity
			if err := json.Unmarshal([]byte(raw), &read); err == nil {
				t.Fatalf("older shape decoded: %+v", read.Payload)
			}
		})
	}
}

// Every payload is written in its kind's current shape, whatever the Go
// value's version fields hold, and reads back.
func TestPayloadsMarshalOnlyTheCurrentShape(t *testing.T) {
	at := "2026-09-20T15:00:00.000000Z"
	left, seconds := "left", uint32(240)
	breast := []FeedComponent{{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &seconds}}
	for name, test := range map[string]struct {
		kind    string
		payload Payload
		keys    []string
	}{
		"feed": {"feed", FeedPayload{BabyID: "10000000-0000-4000-8000-000000000001", Kind: "breast", StartedAt: at, TimeZoneID: "UTC", Components: breast},
			[]string{"baby_id", "components", "ended_at", "feeding_session_id", "kind", "note", "started_at", "time_zone_id"}},
		"sleep": {"sleep", SleepPayload{BabyID: "10000000-0000-4000-8000-000000000001", StartedAt: at, TimeZoneID: "UTC"},
			[]string{"baby_id", "ended_at", "kind_override", "note", "sleep_session_id", "started_at", "time_zone_id"}},
		"timer": {"timer", TimerStartPayload{BabyID: "10000000-0000-4000-8000-000000000001", TimerKind: "sleep", StartedAt: at, TimeZoneID: "UTC", CompletedComponents: []FeedComponent{}},
			[]string{"baby_id", "completed_components", "current_bottle_started_at", "current_side", "current_side_started_at", "session_id", "started_at", "time_zone_id", "timer_kind"}},
		"timer edit": {"", TimerEditPayload{BabyID: "10000000-0000-4000-8000-000000000001"},
			[]string{"baby_id", "components", "session_id", "started_at"}},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			if strings.Join(keys, ",") != strings.Join(test.keys, ",") {
				t.Fatalf("keys=%v want %v\n%s", keys, test.keys, raw)
			}
			if test.kind != "" {
				roundTripStored(t, storedEntity(test.kind, test.payload))
			}
		})
	}
}

// A part writes the fields its kind has: a solids part always carries its
// foods list, `[]` when empty, and a breast or bottle part never carries one.
func TestFeedComponentWritesItsKindsFields(t *testing.T) {
	left, seconds, formula, taste := "left", uint32(60), "formula", "taste"
	for name, test := range map[string]struct {
		part FeedComponent
		want string
	}{
		"solids with no foods":    {FeedComponent{ID: "30000000-0000-4000-8000-000000000001", Kind: "solids", Amount: &taste}, `{"id":"30000000-0000-4000-8000-000000000001","kind":"solids","foods":[],"amount":"taste"}`},
		"solids with empty foods": {FeedComponent{ID: "30000000-0000-4000-8000-000000000001", Kind: "solids", Foods: []string{}, Amount: &taste}, `{"id":"30000000-0000-4000-8000-000000000001","kind":"solids","foods":[],"amount":"taste"}`},
		"solids with foods":       {FeedComponent{ID: "30000000-0000-4000-8000-000000000001", Kind: "solids", Foods: []string{"pear"}, Amount: &taste}, `{"id":"30000000-0000-4000-8000-000000000001","kind":"solids","foods":["pear"],"amount":"taste"}`},
		"breast":                  {FeedComponent{ID: "30000000-0000-4000-8000-000000000002", Kind: "breast", Side: &left, Seconds: &seconds, Foods: []string{}}, `{"id":"30000000-0000-4000-8000-000000000002","kind":"breast","side":"left","seconds":60}`},
		"bottle":                  {FeedComponent{ID: "30000000-0000-4000-8000-000000000003", Kind: "bottle", VolumeML: MustParseDecimal("60"), Contents: &formula, Foods: []string{}}, `{"id":"30000000-0000-4000-8000-000000000003","kind":"bottle","volume_ml":60,"contents":"formula"}`},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(test.part)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != test.want {
				t.Fatalf("part=%s\nwant %s", raw, test.want)
			}
		})
	}
	at := "2026-09-20T15:00:00.000000Z"
	feed := FeedPayload{BabyID: "10000000-0000-4000-8000-000000000001", Kind: "solids", StartedAt: at, TimeZoneID: "UTC",
		Components: []FeedComponent{{ID: "30000000-0000-4000-8000-000000000001", Kind: "solids", Foods: []string{}, Amount: &taste}}}
	got := roundTripStored(t, storedEntity("feed", feed)).Payload.(FeedPayload)
	if len(got.Components) != 1 || got.Components[0].Foods == nil || len(got.Components[0].Foods) != 0 {
		t.Fatalf("stored solids part=%+v", got.Components)
	}
}
