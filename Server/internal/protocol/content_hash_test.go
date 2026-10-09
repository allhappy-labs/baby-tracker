package protocol

import (
	"encoding/hex"
	"testing"
)

// contentHashVector pins CanonicalContentHash for payloads the iOS import
// sends, so the Swift port (ContentHashTests in the BabyProtocol tests) can
// assert the same literals. The import's manifest carries this hash and
// AdmitRecord refuses a record whose recomputed hash differs, so any drift
// between the two implementations fails every imported record.
type contentHashVector struct {
	name          string
	raw           []byte
	canonicalJSON string
	sha256        string
}

func contentHashVectors(t *testing.T) []contentHashVector {
	return []contentHashVector{
		{name: "supplement fixture", raw: readV1Fixture(t, "supplement.json"), canonicalJSON: `{"entity_kind":"supplement","payload":{"logged_at":"2026-09-20T15:00:00.000000Z","name":"Vitamin D","note":null,"started_at":"2026-09-20T14:10:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":1}`, sha256: "ada019a1605551c30b8a3926f283b062b0939f229fbc3be9f1133df9ad5307c9"},
		{
			name:          "sleep fixture",
			raw:           readV1Fixture(t, "sleep.json"),
			canonicalJSON: `{"entity_kind":"sleep","payload":{"ended_at":"2026-09-20T14:00:00.000000Z","kind_override":"nap","note":null,"sleep_session_id":null,"started_at":"2026-09-20T13:00:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":1}`,
			sha256:        "d3fd9350402e119e2bf99e814b8e963702f039660e1a2c8eb87198c3e61c5e5e",
		},
		{
			name:          "diaper fixture",
			raw:           readV1Fixture(t, "diaper.json"),
			canonicalJSON: `{"entity_kind":"diaper","payload":{"color":"mustard","consistency":"soft","contents":"mixed","ended_at":null,"note":null,"started_at":"2026-09-20T14:10:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":1}`,
			sha256:        "32d53d1dd1b2ac16c08953f678d5f163d4804d243d5be00f1514e114552aa029",
		},
		{
			// The shape the client freezes: millisecond times, a decomposed
			// accent and a raw U+2028 in the note, a fractional bottle volume
			// and a solids component.
			name:          "feed v4 composite",
			raw:           []byte("{\"mutation_id\":\"01000000-0000-4000-8000-0000000000a1\",\"entity_id\":\"20000000-0000-4000-8000-0000000000a1\",\"entity_kind\":\"feed\",\"operation\":\"create\",\"payload_version\":4,\"observed_entity_version\":0,\"client_mutation_time\":\"2026-09-24T10:31:00.000Z\",\"import_id\":\"cccccccc-cccc-4ccc-8ccc-cccccccccccc\",\"payload\":{\"baby_id\":\"10000000-0000-4000-8000-000000000001\",\"kind\":\"breast\",\"started_at\":\"2026-09-24T10:00:00.250Z\",\"ended_at\":\"2026-09-24T10:30:00.000Z\",\"time_zone_id\":\"Europe/Zurich\",\"note\":\"Cafe\u0301 \u2028top-up\",\"feeding_session_id\":null,\"components\":[{\"id\":\"30000000-0000-4000-8000-000000000001\",\"kind\":\"breast\",\"side\":\"left\",\"seconds\":480},{\"id\":\"30000000-0000-4000-8000-000000000002\",\"kind\":\"bottle\",\"volume_ml\":50.125,\"contents\":\"breast_milk\"},{\"id\":\"30000000-0000-4000-8000-000000000003\",\"kind\":\"solids\",\"foods\":[\"banana\",\"yogurt\"],\"amount\":\"some\"}]}}"),
			canonicalJSON: "{\"entity_kind\":\"feed\",\"payload\":{\"components\":[{\"ended_at\":null,\"id\":\"30000000-0000-4000-8000-000000000001\",\"kind\":\"breast\",\"seconds\":480,\"side\":\"left\",\"started_at\":null},{\"contents\":\"breast_milk\",\"ended_at\":null,\"id\":\"30000000-0000-4000-8000-000000000002\",\"kind\":\"bottle\",\"seconds\":null,\"started_at\":null,\"volume_ml\":50.125},{\"amount\":\"some\",\"foods\":[\"banana\",\"yogurt\"],\"id\":\"30000000-0000-4000-8000-000000000003\",\"kind\":\"solids\"}],\"ended_at\":\"2026-09-24T10:30:00.000000Z\",\"feeding_session_id\":null,\"kind\":\"breast\",\"note\":\"Caf\u00e9 \u2028top-up\",\"started_at\":\"2026-09-24T10:00:00.250000Z\",\"time_zone_id\":\"Europe/Zurich\"},\"payload_version\":1}",
			sha256:        "27afa828b79af603d09a39f2ad57cef405a70f9f795741998fd83583857f355a",
		},
		{
			// The client omits an empty note and end rather than sending null.
			name:          "feed v4 sparse",
			raw:           []byte(`{"mutation_id":"01000000-0000-4000-8000-0000000000a2","entity_id":"20000000-0000-4000-8000-0000000000a2","entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,"client_mutation_time":"2026-09-24T11:00:00.000Z","import_id":null,"payload":{"baby_id":"10000000-0000-4000-8000-000000000001","kind":"bottle","started_at":"2026-09-24T11:00:00.000Z","time_zone_id":"Europe/Zurich","feeding_session_id":null,"components":[{"id":"30000000-0000-4000-8000-000000000004","kind":"bottle","volume_ml":60,"contents":"formula"}]}}`),
			canonicalJSON: `{"entity_kind":"feed","payload":{"components":[{"contents":"formula","ended_at":null,"id":"30000000-0000-4000-8000-000000000004","kind":"bottle","seconds":null,"started_at":null,"volume_ml":60}],"ended_at":null,"feeding_session_id":null,"kind":"bottle","note":null,"started_at":"2026-09-24T11:00:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":1}`,
			sha256:        "67659c4328a6ffe8b5c8d1d017f95ba35866aec597f58d4df4c63107a30c99a9",
		},
		{
			name:          "timer v5 running feed",
			raw:           []byte(`{"mutation_id":"01000000-0000-4000-8000-0000000000a3","entity_id":"50000000-0000-4000-8000-0000000000a3","entity_kind":"timer","operation":"timer_start","payload_version":5,"observed_entity_version":0,"client_mutation_time":"2026-09-24T12:00:00.000Z","import_id":null,"payload":{"baby_id":"10000000-0000-4000-8000-000000000001","timer_kind":"feed","started_at":"2026-09-24T11:50:00.000Z","time_zone_id":"Europe/Zurich","completed_components":[{"id":"30000000-0000-4000-8000-000000000005","kind":"breast","side":"left","seconds":300}],"current_side":"right","current_side_started_at":"2026-09-24T11:55:00.000Z","current_bottle_started_at":null,"session_id":null}}`),
			canonicalJSON: `{"entity_kind":"timer","payload":{"completed_components":[{"ended_at":null,"id":"30000000-0000-4000-8000-000000000005","kind":"breast","seconds":300,"side":"left","started_at":null}],"current_bottle_started_at":null,"current_side":"right","current_side_started_at":"2026-09-24T11:55:00.000000Z","session_id":null,"started_at":"2026-09-24T11:50:00.000000Z","time_zone_id":"Europe/Zurich","timer_kind":"feed"},"payload_version":1}`,
			sha256:        "e604e2e693e8e56bf6a90b736efe1bc77f73ac435a3813b5338cef0b675d8b05",
		},
	}
}

func TestCanonicalContentHashCrossPlatformVectors(t *testing.T) {
	for _, vector := range contentHashVectors(t) {
		mutation := mustMutation(t, vector.raw)
		payload, err := canonicalPayloadValue(mutation.Payload, false)
		if err != nil {
			t.Fatalf("%s: %v", vector.name, err)
		}
		canonical, err := marshalCanonicalJSON(map[string]any{
			"entity_kind": mutation.EntityKind, "payload": payload, "payload_version": uint32(1),
		})
		if err != nil {
			t.Fatalf("%s: %v", vector.name, err)
		}
		hash, err := CanonicalContentHash(mutation.EntityKind, mutation.Payload)
		if err != nil {
			t.Fatalf("%s: %v", vector.name, err)
		}
		if string(canonical) != vector.canonicalJSON || hex.EncodeToString(hash[:]) != vector.sha256 {
			t.Errorf("%s:\n canonical = %s\n sha256    = %x", vector.name, canonical, hash)
		}
	}
}

// A composite feed (two breast parts and a bottle) as the client sends it at
// the current version. The Swift side asserts the same hash for the same feed
// frozen into an import manifest
// (ImportStoreTests.compositeFeedManifestHashMatchesTheServer).
func TestCompositeFeedV4ContentHashMatchesClient(t *testing.T) {
	const components = `[{"id":"c9625297-288b-54c2-8e3b-6b80d0b1f9ff","kind":"breast","side":"left","seconds":300},{"id":"5a0487fb-fcc3-5e2f-a7b5-a3b990f33dcd","kind":"breast","side":"right","seconds":240},{"id":"01b7623d-08d1-53d5-b0df-65221e4079ef","kind":"bottle","volume_ml":60,"contents":"formula"}]`
	v4 := `{"mutation_id":"01000000-0000-4000-8000-0000000000b2","entity_id":"20000000-0000-4000-8000-0000000000b1","entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,"client_mutation_time":"2026-09-24T08:20:00.000Z","import_id":null,"payload":{"baby_id":"10000000-0000-4000-8000-000000000001","kind":"breast","started_at":"2026-09-24T08:00:00.000Z","ended_at":"2026-09-24T08:20:00.000Z","time_zone_id":"Europe/Zurich","feeding_session_id":null,"components":` + components + `}}`
	mutation := mustMutation(t, []byte(v4))
	hash, err := CanonicalContentHash(mutation.EntityKind, mutation.Payload)
	if err != nil {
		t.Fatal(err)
	}
	const want = "253a09be435d9c90eae64d6904bbdc97ee49149f3c39d0540eb1c2b85a133be5"
	if hex.EncodeToString(hash[:]) != want {
		t.Fatalf("v4 composite hash=%x want=%s", hash, want)
	}
}

// Go's RE2 `$` never matches before a trailing newline, so such a string is
// not a timestamp and is hashed as text. The Swift port must agree.
func TestTimestampWithTrailingNewlineIsHashedAsText(t *testing.T) {
	const raw = "2026-09-20T12:00:00Z\n"
	if got := normalizeTimestampForHash(raw); got != raw {
		t.Fatalf("normalizeTimestampForHash(%q) = %q", raw, got)
	}
}
