package protocol

import (
	"bytes"
	"fmt"
	"testing"
)

// The inbound version rule: creates, updates and timer commands at exactly
// the kind's current version; deletes at any version from 1 to the current
// one, permanently (build 2961ca1 sends feed deletes at 2, sleep deletes at 1).
func TestInboundPayloadVersionRule(t *testing.T) {
	current := map[string]uint32{"baby": 1, "feed": 4, "sleep": 2, "diaper": 1, "supplement": 1,
		"growth_profile": 1, "measurement": 1, "timer": 5}
	for kind, want := range current {
		if got := CurrentPayloadVersion(kind); got != want {
			t.Fatalf("%s current=%d want=%d", kind, got, want)
		}
		for version := uint32(0); version <= 6; version++ {
			for _, operation := range []string{"create", "update", "timer_start", "timer_finish_bottle", "timer_stop"} {
				if got := AcceptedPayloadVersion(kind, operation, version); got != (version == want) {
					t.Fatalf("%s %s v%d accepted=%t", kind, operation, version, got)
				}
			}
			if got := AcceptedPayloadVersion(kind, "delete", version); got != (version >= 1 && version <= want) {
				t.Fatalf("%s delete v%d accepted=%t", kind, version, got)
			}
		}
	}
}

// Every shared fixture is a current-version write, so it decodes as sent and
// is refused one version either side of it.
func TestFixtureWritesDecodeOnlyAtTheirCurrentVersion(t *testing.T) {
	for _, fixture := range []string{"baby.json", "feed-bottle.json", "feed-breast.json", "feed-combination.json",
		"feed-solids.json", "sleep.json", "diaper.json", "supplement.json", "growth-profile.json", "measurement.json",
		"timer-feed.json", "timer-sleep.json"} {
		t.Run(fixture, func(t *testing.T) {
			raw := readV1Fixture(t, fixture)
			mutation := mustMutation(t, raw)
			if mutation.PayloadVersion != CurrentPayloadVersion(mutation.EntityKind) {
				t.Fatalf("fixture %s is at v%d", mutation.EntityKind, mutation.PayloadVersion)
			}
			sent := []byte(fmt.Sprintf(`"payload_version":%d`, mutation.PayloadVersion))
			for _, version := range []uint32{0, mutation.PayloadVersion - 1, mutation.PayloadVersion + 1} {
				changed := bytes.Replace(raw, sent, []byte(fmt.Sprintf(`"payload_version":%d`, version)), 1)
				if _, err := DecodeMutation(changed); ErrorCodeOf(err) != "invalid_request" {
					t.Fatalf("v%d err=%v", version, err)
				}
			}
		})
	}
}

// A delete names only its baby, so the interim client's older delete
// versions decode; zero and anything above the current version do not.
func TestDeletesDecodeAtEveryVersionUpToTheCurrentOne(t *testing.T) {
	for kind, current := range map[string]uint32{"baby": 1, "feed": 4, "sleep": 2, "diaper": 1, "supplement": 1, "growth_profile": 1, "measurement": 1} {
		payload := `{"baby_id":"10000000-0000-4000-8000-000000000001"}`
		if kind == "baby" {
			payload = `{}`
		}
		for version := uint32(0); version <= current+1; version++ {
			raw := []byte(fmt.Sprintf(`{"mutation_id":"01000000-0000-4000-8000-000000000071","entity_id":"20000000-0000-4000-8000-000000000071",`+
				`"entity_kind":%q,"operation":"delete","payload_version":%d,"observed_entity_version":3,"client_mutation_time":"2026-10-02T09:00:00.000Z",`+
				`"payload":%s}`, kind, version, payload))
			_, err := DecodeMutation(raw)
			if accepted := version >= 1 && version <= current; accepted != (err == nil) {
				t.Fatalf("%s delete v%d err=%v", kind, version, err)
			}
		}
	}
}
