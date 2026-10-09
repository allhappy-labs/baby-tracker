package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSupplementPayloadShape(t *testing.T) {
	raw := readV1Fixture(t, "supplement.json")
	m, err := DecodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := m.Payload.(SupplementPayload)
	if p.Name != "Vitamin D" {
		t.Fatal(p)
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
	for _, key := range []string{"baby_id", "started_at", "time_zone_id", "name", "logged_at"} {
		var envelope map[string]json.RawMessage
		json.Unmarshal(raw, &envelope)
		var payload map[string]json.RawMessage
		json.Unmarshal(envelope["payload"], &payload)
		delete(payload, key)
		envelope["payload"], _ = json.Marshal(payload)
		bad, _ := json.Marshal(envelope)
		if _, err := DecodeMutation(bad); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
	}
	padded := strings.Replace(string(raw), "Vitamin D", "  Vitamin D  ", 1)
	trimmed, err := DecodeMutation([]byte(padded))
	if err != nil || trimmed.Payload.(SupplementPayload).Name != "Vitamin D" {
		t.Fatalf("trim %v %v", trimmed, err)
	}
	bad := strings.Replace(string(raw), `"name":"Vitamin D"`, `"name":"Vitamin D","ended_at":null`, 1)
	if _, err := DecodeMutation([]byte(bad)); err == nil {
		t.Fatal("unknown field accepted")
	}
}
