package protocol

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestGrowthAndMeasurementWire(t *testing.T) {
	for _, name := range []string{"growth-profile.json", "measurement.json"} {
		raw := readV1Fixture(t, name)
		m, err := DecodeMutation(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := json.Marshal(m.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]json.RawMessage
		json.Unmarshal(raw, &envelope)
		var want, got map[string]any
		json.Unmarshal(envelope["payload"], &want)
		json.Unmarshal(encoded, &got)
		if len(got) != len(want) {
			t.Fatalf("field mismatch: %s", encoded)
		}
		for key := range want {
			var fields map[string]json.RawMessage
			json.Unmarshal(envelope["payload"], &fields)
			delete(fields, key)
			envelope["payload"], _ = json.Marshal(fields)
			bad, _ := json.Marshal(envelope)
			if _, err := DecodeMutation(bad); err == nil {
				t.Fatalf("%s missing %s accepted", name, key)
			}
			json.Unmarshal(raw, &envelope)
		}
		h, err := CanonicalContentHash(m.EntityKind, m.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]json.RawMessage
		json.Unmarshal(envelope["payload"], &payload)
		payload["baby_id"] = json.RawMessage(`"10000000-0000-4000-8000-000000000002"`)
		envelope["payload"], _ = json.Marshal(payload)
		changed, _ := json.Marshal(envelope)
		other, err := DecodeMutation(changed)
		if err != nil {
			t.Fatal(err)
		}
		h2, err := CanonicalContentHash(other.EntityKind, other.Payload)
		if err != nil || h != h2 {
			t.Fatalf("baby remap changes content hash: %v", err)
		}
	}
}
func TestGrowthProfileIDVectors(t *testing.T) {
	for baby, want := range map[string]string{"00000000-0000-0000-0000-000000000000": "052ae0b7-e763-5d48-bf4c-b283a15c071f", "123e4567-e89b-12d3-a456-426614174000": "101dea4a-277e-597f-a0a7-aedaadde0cde", "ffffffff-ffff-ffff-ffff-ffffffffffff": "5793676f-fb9d-5024-b24d-6a3748cd8085"} {
		if got := GrowthProfileID(baby); got != want {
			t.Fatalf("%s => %s, want %s", baby, got, want)
		}
	}
}

func TestGrowthAndMeasurementGoldenContentHashes(t *testing.T) {
	for _, tt := range []struct{ fixture, hash string }{
		{"growth-profile.json", "3250e46c4d908a49a89e34d14ab78c3bb73d8cd225b497e64a763172e24fca6a"},
		{"measurement.json", "76464011b33e02e20fd0fc1a25f1190f100c533d8b6b0ad25b94d02090f9625b"},
	} {
		m, err := DecodeMutation(readV1Fixture(t, tt.fixture))
		if err != nil {
			t.Fatal(err)
		}
		hash, err := CanonicalContentHash(m.EntityKind, m.Payload)
		if err != nil || hex.EncodeToString(hash[:]) != tt.hash {
			t.Fatalf("golden %s hash %x %v", tt.fixture, hash, err)
		}
	}
}
func TestMeasurementRejectsInvalidWireTypes(t *testing.T) {
	raw := string(readV1Fixture(t, "measurement.json"))
	for _, bad := range []string{strings.Replace(raw, `"weight_kg":7.125`, `"weight_kg":"7.125"`, 1), strings.Replace(raw, `"length_position":"lying"`, `"length_position":true`, 1), strings.Replace(raw, `"logged_at":"2026-09-20T15:00:00Z"`, `"logged_at":null`, 1)} {
		if _, err := DecodeMutation([]byte(bad)); err == nil {
			t.Fatalf("invalid type accepted %s", bad)
		}
	}
}

func TestGrowthAndMeasurementGoldenMutationBytes(t *testing.T) {
	for _, tt := range []struct{ fixture, canonical, hash string }{
		{"growth-profile.json", `{"client_mutation_time":"2026-09-20T14:10:00.000000Z","entity_id":"2742cf11-d3ad-51bb-bbfa-24147f4b0aae","entity_kind":"growth_profile","import_id":null,"observed_entity_version":0,"operation":"create","payload":{"baby_id":"10000000-0000-4000-8000-000000000001","expected_due_date":null,"reference_sex":"girls"},"payload_version":1}`, "c5c274fb7b44c8f91f79a32a78def410d021bd7c074fb885c34ec02ab752c220"},
		{"measurement.json", `{"client_mutation_time":"2026-09-20T14:10:00.000000Z","entity_id":"70000000-0000-4000-8000-000000000001","entity_kind":"measurement","import_id":null,"observed_entity_version":0,"operation":"create","payload":{"baby_id":"10000000-0000-4000-8000-000000000001","head_circumference_cm":null,"length_cm":65.5,"length_position":"lying","logged_at":"2026-09-20T15:00:00.000000Z","measured_at":"2026-09-20T14:10:00.000000Z","time_zone_id":"Europe/Zurich","weight_kg":7.125},"payload_version":1}`, "e99207fb7e59b99f2629a2af12052fa690fc9266a9d057b11992b5fa7a86b510"},
	} {
		m, err := DecodeMutation(readV1Fixture(t, tt.fixture))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := CanonicalMutationBytes(m)
		if err != nil || string(raw) != tt.canonical {
			t.Fatalf("%s canonical bytes %s %v", tt.fixture, raw, err)
		}
		hash, err := CanonicalMutationHash(m)
		if err != nil || hex.EncodeToString(hash[:]) != tt.hash {
			t.Fatalf("%s hash %x %v", tt.fixture, hash, err)
		}
	}
}

func TestGrowthProfileIDRejectsMalformedNamespace(t *testing.T) {
	for _, value := range []string{"", "not-a-uuid", "123e4567e89b12d3a456426614174000"} {
		if got := GrowthProfileID(value); got != "" {
			t.Fatalf("malformed namespace %q returned %s", value, got)
		}
	}
}
