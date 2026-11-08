package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestImmunizationPayloadShape(t *testing.T) {
	raw := readV1Fixture(t, "immunization.json")
	m, err := DecodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := m.Payload.(ImmunizationPayload)
	if p.Vaccine != "Prevenar 13" || p.Dose == nil || *p.Dose != 2 || p.Note != nil {
		t.Fatalf("%+v", p)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(encoded, &fields)
	if len(fields) != 6 {
		t.Fatal(string(encoded))
	}
	without := func(key string) []byte {
		var envelope map[string]json.RawMessage
		json.Unmarshal(raw, &envelope)
		var payload map[string]json.RawMessage
		json.Unmarshal(envelope["payload"], &payload)
		delete(payload, key)
		envelope["payload"], _ = json.Marshal(payload)
		bad, _ := json.Marshal(envelope)
		return bad
	}
	// Every key is present on the wire; dose and note may be null, the rest may not.
	for _, key := range []string{"baby_id", "given_at", "time_zone_id", "vaccine", "dose", "note"} {
		if _, err := DecodeMutation(without(key)); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
	}
	// A required key set to null, with nothing else changed: the same payload
	// with the key restored decodes, so only the null is being refused.
	withNull := func(key string) []byte {
		var envelope map[string]json.RawMessage
		json.Unmarshal(raw, &envelope)
		var payload map[string]json.RawMessage
		json.Unmarshal(envelope["payload"], &payload)
		payload[key] = json.RawMessage("null")
		envelope["payload"], _ = json.Marshal(payload)
		bad, _ := json.Marshal(envelope)
		return bad
	}
	for _, key := range []string{"baby_id", "given_at", "time_zone_id", "vaccine"} {
		if _, err := DecodeMutation(withNull(key)); err == nil {
			t.Fatalf("accepted null %s", key)
		}
	}
	for _, key := range []string{"dose", "note"} {
		if _, err := DecodeMutation(withNull(key)); err != nil {
			t.Fatalf("refused null %s: %v", key, err)
		}
	}
	noDose := strings.Replace(string(raw), `"dose":2`, `"dose":null`, 1)
	if m, err := DecodeMutation([]byte(noDose)); err != nil || m.Payload.(ImmunizationPayload).Dose != nil {
		t.Fatalf("null dose: %v %v", m, err)
	}
	for _, bad := range []string{`"dose":2.5`, `"dose":"2"`, `"dose":true`} {
		if _, err := DecodeMutation([]byte(strings.Replace(string(raw), `"dose":2`, bad, 1))); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	padded := strings.Replace(string(raw), "Prevenar 13", "  Prevenar 13  ", 1)
	if trimmed, err := DecodeMutation([]byte(padded)); err != nil || trimmed.Payload.(ImmunizationPayload).Vaccine != "Prevenar 13" {
		t.Fatalf("trim %v %v", trimmed, err)
	}
	extra := strings.Replace(string(raw), `"note":null`, `"note":null,"logged_at":"2026-09-20T15:00:00Z"`, 1)
	if _, err := DecodeMutation([]byte(extra)); err == nil {
		t.Fatal("unknown field accepted")
	}
}
