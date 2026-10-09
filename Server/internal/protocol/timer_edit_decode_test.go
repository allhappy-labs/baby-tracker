package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
)

func timerEditFixture(t *testing.T) []byte {
	t.Helper()
	var fixture map[string]json.RawMessage
	if err := json.Unmarshal(readV1Fixture(t, "mutations.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	raw := fixture["timer_edit"]
	if raw == nil {
		t.Fatal("mutations.json has no timer_edit fixture")
	}
	return raw
}

func TestTimerEditDecodesAsANormalizedPatch(t *testing.T) {
	mutation := mustMutation(t, timerEditFixture(t))
	payload, ok := mutation.Payload.(TimerEditPayload)
	if !ok || mutation.Operation != "update" || mutation.EntityKind != "timer" || mutation.PayloadVersion != 5 {
		t.Fatalf("mutation=%+v", mutation)
	}
	left, seconds, formula := "left", uint32(300), "formula"
	want := TimerEditPayload{BabyID: "10000000-0000-4000-8000-000000000001", StartedAt: stringPointerForTest("2026-09-20T14:58:00.000000Z"),
		Components: []FeedComponent{
			{ID: "30000000-0000-4000-8000-000000000011", Kind: "breast", Side: &left, Seconds: &seconds},
			{ID: "30000000-0000-4000-8000-000000000012", Kind: "bottle", VolumeML: MustParseDecimal("70"), Contents: &formula},
		}}
	if !reflect.DeepEqual(payload, want) {
		t.Fatalf("payload=%+v\nwant   %+v", payload, want)
	}
	for name, raw := range map[string][]byte{
		"null start":    bytes.Replace(timerEditFixture(t), []byte(`"started_at":"2026-09-20T14:58:00Z"`), []byte(`"started_at":null`), 1),
		"omitted start": bytes.Replace(timerEditFixture(t), []byte(`"started_at":"2026-09-20T14:58:00Z",`), nil, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if got := mustMutation(t, raw).Payload.(TimerEditPayload); got.StartedAt != nil || len(got.Components) != 2 {
				t.Fatalf("payload=%+v", got)
			}
		})
	}
	sent, err := json.Marshal(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if again := mustMutation(t, sent); !reflect.DeepEqual(again, mutation) {
		t.Fatalf("round trip=%+v want %+v", again, mutation)
	}
	startOnly := mutation
	startOnly.Payload = TimerEditPayload{BabyID: payload.BabyID, StartedAt: payload.StartedAt}
	if sent, err = json.Marshal(startOnly); err != nil || !bytes.Contains(sent, []byte(`"components":[]`)) {
		t.Fatalf("start-only edit encoded as %s (%v)", sent, err)
	}
}

func TestTimerEditRefusesMalformedPayloads(t *testing.T) {
	raw := timerEditFixture(t)
	components := `"components":[{"id":"30000000-0000-4000-8000-000000000011","kind":"breast","side":"left","seconds":300},{"id":"30000000-0000-4000-8000-000000000012","kind":"bottle","volume_ml":70,"contents":"formula"}]`
	for name, broken := range map[string][]byte{
		"missing parts":      bytes.Replace(raw, []byte(","+components), nil, 1),
		"null parts":         bytes.Replace(raw, []byte(components), []byte(`"components":null`), 1),
		"missing baby":       bytes.Replace(raw, []byte(`"baby_id":"10000000-0000-4000-8000-000000000001",`), nil, 1),
		"unknown field":      bytes.Replace(raw, []byte(`"started_at"`), []byte(`"current_side":"left","started_at"`), 1),
		"part without kind":  bytes.Replace(raw, []byte(`"kind":"breast",`), nil, 1),
		"breast with volume": bytes.Replace(raw, []byte(`"seconds":300`), []byte(`"seconds":300,"volume_ml":1`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMutation(broken); ErrorCodeOf(err) != "invalid_request" {
				t.Fatalf("code=%q err=%v\n%s", ErrorCodeOf(err), err, broken)
			}
		})
	}
}

func TestTimerEditCanonicalVector(t *testing.T) {
	var vectors struct {
		TimerEdit struct {
			Fixture       string `json:"fixture"`
			Key           string `json:"key"`
			CanonicalJSON string `json:"canonical_json"`
			SHA256        string `json:"sha256"`
		} `json:"timer_edit_mutation"`
	}
	if err := json.Unmarshal(readV1Fixture(t, "canonical-vectors.json"), &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.TimerEdit.Fixture != "mutations.json" || vectors.TimerEdit.Key != "timer_edit" {
		t.Fatalf("vector points at %q/%q", vectors.TimerEdit.Fixture, vectors.TimerEdit.Key)
	}
	mutation := mustMutation(t, timerEditFixture(t))
	canonical, err := canonicalMutationBytes(mutation)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := CanonicalMutationHash(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != vectors.TimerEdit.CanonicalJSON || hex.EncodeToString(hash[:]) != vectors.TimerEdit.SHA256 {
		t.Fatalf("canonical=%s\nsha256=%s\nwant %s / %s", canonical, hex.EncodeToString(hash[:]), vectors.TimerEdit.CanonicalJSON, vectors.TimerEdit.SHA256)
	}
	// A null start and an omitted one are the same edit, so they are the same
	// receipt: a client that drops the key on resend is not refused as a reused id.
	omitted := mustMutation(t, bytes.Replace(timerEditFixture(t), []byte(`"started_at":"2026-09-20T14:58:00Z",`), nil, 1))
	null := mustMutation(t, bytes.Replace(timerEditFixture(t), []byte(`"started_at":"2026-09-20T14:58:00Z"`), []byte(`"started_at":null`), 1))
	omittedHash, _ := CanonicalMutationHash(omitted)
	nullHash, _ := CanonicalMutationHash(null)
	if omittedHash != nullHash || omittedHash == hash {
		t.Fatal("null and omitted starts hash differently, or the start is not in the hash")
	}
}

func stringPointerForTest(value string) *string { return &value }
