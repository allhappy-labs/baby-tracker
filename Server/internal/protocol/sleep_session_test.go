package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func mutationJSON(kind, op string, version int, payload string) []byte {
	return []byte(`{"mutation_id":"11111111-1111-4111-8111-111111111111","entity_id":"22222222-2222-4222-8222-222222222222",` +
		`"entity_kind":"` + kind + `","operation":"` + op + `","payload_version":` + strings.TrimSpace(json.Number(itoa(version)).String()) +
		`,"observed_entity_version":0,"client_mutation_time":"2026-10-01T10:00:00.000000Z","payload":` + payload + `}`)
}

func itoa(v int) string { b, _ := json.Marshal(v); return string(b) }

const babyID = "33333333-3333-4333-8333-333333333333"
const sessionID = "44444444-4444-4444-8444-444444444444"

func TestTimerStartV5CarriesSessionID(t *testing.T) {
	raw := mutationJSON("timer", "timer_start", 5, `{"baby_id":"`+babyID+`","timer_kind":"feed","started_at":"2026-10-01T10:00:00.000000Z",`+
		`"time_zone_id":"Europe/Zurich","completed_components":[],"current_side":"left","current_side_started_at":"2026-10-01T10:00:00.000000Z",`+
		`"current_bottle_started_at":null,"session_id":"`+sessionID+`"}`)
	m, err := DecodeMutation(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	p := m.Payload.(TimerStartPayload)
	if p.SessionID == nil || *p.SessionID != sessionID {
		t.Fatalf("session not decoded: %+v", p)
	}
	out, _ := json.Marshal(p)
	if !strings.Contains(string(out), `"session_id":"`+sessionID+`"`) {
		t.Fatalf("session not encoded: %s", out)
	}
}

func TestTimerStartV5RequiresSessionKey(t *testing.T) {
	raw := mutationJSON("timer", "timer_start", 5, `{"baby_id":"`+babyID+`","timer_kind":"sleep","started_at":"2026-10-01T10:00:00.000000Z",`+
		`"time_zone_id":"Europe/Zurich","completed_components":[],"current_side":null,"current_side_started_at":null,"current_bottle_started_at":null}`)
	if _, err := DecodeMutation(raw); err == nil {
		t.Fatal("v5 start without session_id must be invalid")
	}
}

func TestSleepV2CarriesSleepSessionIDAndV1ForbidsIt(t *testing.T) {
	body := `{"baby_id":"` + babyID + `","started_at":"2026-10-01T10:00:00.000000Z","ended_at":"2026-10-01T11:00:00.000000Z",` +
		`"time_zone_id":"Europe/Zurich","note":null,"kind_override":null,"sleep_session_id":"` + sessionID + `"}`
	m, err := DecodeMutation(mutationJSON("sleep", "create", 2, body))
	if err != nil {
		t.Fatalf("decode v2: %v", err)
	}
	p := m.Payload.(SleepPayload)
	if p.SleepSessionID == nil || *p.SleepSessionID != sessionID {
		t.Fatalf("sleep session not decoded: %+v", p)
	}
	if _, err := DecodeMutation(mutationJSON("sleep", "create", 1, body)); err == nil {
		t.Fatal("v1 sleep must not carry sleep_session_id")
	}
	if _, err := DecodeMutation(mutationJSON("diaper", "create", 2, `{}`)); err == nil {
		t.Fatal("diaper v2 stays invalid")
	}
	if _, err := DecodeMutation(mutationJSON("feed", "create", 5, `{}`)); err == nil {
		t.Fatal("feed v5 stays invalid")
	}
}

// The hash has one formula: every session key is in it, null or not, whatever
// the Go value's version fields hold.
func TestCanonicalHashAlwaysIncludesSessionFields(t *testing.T) {
	s := sessionID
	for name, test := range map[string]struct {
		payload Payload
		key     string
		want    any
	}{
		"sleep without a session": {SleepPayload{BabyID: babyID, StartedAt: "2026-10-01T10:00:00.000000Z", TimeZoneID: "UTC"}, "sleep_session_id", nil},
		"sleep in a session":      {SleepPayload{BabyID: babyID, StartedAt: "2026-10-01T10:00:00.000000Z", TimeZoneID: "UTC", SleepSessionID: &s}, "sleep_session_id", s},
		"feed without a session":  {FeedPayload{BabyID: babyID, Kind: "bottle", StartedAt: "2026-10-01T10:00:00.000000Z", TimeZoneID: "UTC"}, "feeding_session_id", nil},
		"timer in a session":      {TimerStartPayload{BabyID: babyID, TimerKind: "feed", StartedAt: "2026-10-01T10:00:00.000000Z", TimeZoneID: "UTC", SessionID: &s}, "session_id", s},
		"timer without a bottle":  {TimerStartPayload{BabyID: babyID, TimerKind: "feed", StartedAt: "2026-10-01T10:00:00.000000Z", TimeZoneID: "UTC"}, "current_bottle_started_at", nil},
		"timer edit":              {TimerEditPayload{BabyID: babyID, SessionID: &s}, "session_id", s},
	} {
		t.Run(name, func(t *testing.T) {
			canonical, err := canonicalPayloadValue(test.payload, true)
			if err != nil {
				t.Fatal(err)
			}
			if got, present := canonical[test.key]; !present || got != test.want {
				t.Fatalf("%s = %#v (present %v), want %#v", test.key, got, present, test.want)
			}
		})
	}
}
