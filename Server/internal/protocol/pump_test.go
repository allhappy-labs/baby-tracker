package protocol

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestPumpFixtureDecodes(t *testing.T) {
	m := mustMutation(t, readV1Fixture(t, "pump.json"))
	p, ok := m.Payload.(PumpPayload)
	if !ok || m.EntityKind != "pump" || p.Side != "both" || p.EndedAt == nil || p.VolumeML == nil || p.VolumeML.String() != "90.5" || p.Note != nil {
		t.Fatalf("payload %+v", m.Payload)
	}
	if p.StartedAt != "2026-09-20T14:10:00.000000Z" || *p.EndedAt != "2026-09-20T14:30:00.000000Z" {
		t.Fatalf("times not normalized: %s %s", p.StartedAt, *p.EndedAt)
	}
}

// ended_at, volume_ml and note are nullable but always sent, like a
// measurement's numbers: a missing key is a malformed request. The three
// required keys may not be null.
func TestPumpRequiresEveryKey(t *testing.T) {
	raw := readV1Fixture(t, "pump.json")
	for _, key := range []string{`"ended_at":"2026-09-20T14:30:00Z",`, `"volume_ml":90.5,`, `,"note":null`,
		`"started_at":"2026-09-20T14:10:00Z",`, `"time_zone_id":"Europe/Zurich",`, `"side":"both",`} {
		changed := bytes.Replace(raw, []byte(key), nil, 1)
		if bytes.Equal(changed, raw) {
			t.Fatalf("fixture no longer contains %s", key)
		}
		if _, err := DecodeMutation(changed); err == nil {
			t.Fatalf("decoded without %s", key)
		}
	}
	for _, null := range [][2]string{{`"started_at":"2026-09-20T14:10:00Z"`, `"started_at":null`},
		{`"time_zone_id":"Europe/Zurich"`, `"time_zone_id":null`}, {`"side":"both"`, `"side":null`}} {
		changed := strings.Replace(string(raw), null[0], null[1], 1)
		if changed == string(raw) {
			t.Fatalf("fixture no longer contains %s", null[0])
		}
		if _, err := DecodeMutation([]byte(changed)); err == nil {
			t.Fatalf("decoded with %s", null[1])
		}
	}
	running := strings.Replace(strings.Replace(string(raw), `"ended_at":"2026-09-20T14:30:00Z"`, `"ended_at":null`, 1), `"volume_ml":90.5`, `"volume_ml":null`, 1)
	m, err := DecodeMutation([]byte(running))
	if err != nil || m.Payload.(PumpPayload).EndedAt != nil || m.Payload.(PumpPayload).VolumeML != nil {
		t.Fatalf("a running pump: %+v %v", m.Payload, err)
	}
	extra := strings.Replace(string(raw), `"note":null`, `"note":null,"baby_id":"10000000-0000-4000-8000-000000000001"`, 1)
	if _, err := DecodeMutation([]byte(extra)); err == nil {
		t.Fatal("a pump naming a baby decoded")
	}
}

// A pump belongs to the family: its delete names nothing, like a baby's.
func TestPumpDeleteNamesNoBaby(t *testing.T) {
	deletion := func(payload string) []byte {
		return []byte(fmt.Sprintf(`{"mutation_id":"01000000-0000-4000-8000-0000000000b1","entity_id":"b0000000-0000-4000-8000-000000000001",`+
			`"entity_kind":"pump","operation":"delete","payload_version":1,"observed_entity_version":1,"client_mutation_time":"2026-10-02T09:00:00.000Z",`+
			`"payload":%s}`, payload))
	}
	if _, err := DecodeMutation(deletion(`{}`)); err != nil {
		t.Fatalf("empty pump delete: %v", err)
	}
	if _, err := DecodeMutation(deletion(`{"baby_id":"10000000-0000-4000-8000-000000000001"}`)); err == nil {
		t.Fatal("a pump delete naming a baby decoded")
	}
}

func TestPumpHashNeverCarriesABabyID(t *testing.T) {
	m := mustMutation(t, readV1Fixture(t, "pump.json"))
	for _, include := range []bool{false, true} {
		value, err := canonicalPayloadValue(m.Payload, include)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := value["baby_id"]; ok {
			t.Fatalf("includeBabyID=%t put baby_id in a pump hash", include)
		}
		if value["side"] != "both" || value["ended_at"] != "2026-09-20T14:30:00.000000Z" {
			t.Fatalf("canonical pump %v", value)
		}
	}
}

func TestDecimalScale(t *testing.T) {
	for raw, want := range map[string]int32{"90": 0, "90.5": 1, "90.50": 1, "88.72": 2, "88.721": 3, "0": 0} {
		if got := MustParseDecimal(raw).Scale(); got != want {
			t.Errorf("Scale(%s) = %d, want %d", raw, got, want)
		}
	}
}
