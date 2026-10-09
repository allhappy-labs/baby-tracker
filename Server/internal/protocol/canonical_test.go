package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalVectors(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"  E\u0301LIE\u00a0", "élie"}, {"Straße", "strasse"},
		{"A  B", "a  b"}, {"İ", "i\u0307"},
	} {
		got, err := NormalizeBabyName(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("NormalizeBabyName(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}

	raw := readV1Fixture(t, "feed-bottle.json")
	a := mustMutation(t, raw)
	b := mustMutation(t, bytes.Replace(raw, []byte(`"volume_ml":1`), []byte(`"volume_ml":1.0`), 1))
	c := mustMutation(t, bytes.Replace(raw, []byte(`"volume_ml":1`), []byte(`"volume_ml":1e0`), 1))
	ah, err := CanonicalMutationHash(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []Mutation{b, c} {
		got, err := CanonicalMutationHash(mutation)
		if err != nil {
			t.Fatal(err)
		}
		if got != ah {
			t.Fatal("equal decimal semantics produced different receipts")
		}
	}
	const wantHash = "4d28b2f9e3d1d1e59f45de30bd7a1aa362adcc6f1b3e06efb7f3b8a97b5fde11"
	if hex.EncodeToString(ah[:]) != wantHash {
		t.Fatalf("canonical hash = %x, want committed independent oracle %s", ah, wantHash)
	}
}

func TestFeedComponentCanonicalOrderAndStrictShape(t *testing.T) {
	raw := []byte(`{"mutation_id":"01000000-0000-4000-8000-000000000099","entity_id":"20000000-0000-4000-8000-000000000001","entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,"client_mutation_time":"2026-09-24T10:01:02Z","import_id":null,"payload":{"baby_id":"10000000-0000-4000-8000-000000000001","kind":"breast","started_at":"2026-09-24T10:00:00Z","ended_at":null,"time_zone_id":"Europe/Zurich","note":null,"feeding_session_id":null,"components":[{"id":"30000000-0000-4000-8000-000000000001","kind":"breast","side":"left","seconds":480},{"id":"30000000-0000-4000-8000-000000000002","kind":"bottle","volume_ml":50.125000,"contents":"breast_milk"}]}}`)
	mutation := mustMutation(t, raw)
	feed := mutation.Payload.(FeedPayload)
	if len(feed.Components) != 2 || feed.Components[0].Kind != "breast" || feed.Components[1].Kind != "bottle" {
		t.Fatalf("components=%+v", feed.Components)
	}
	canonical, err := CanonicalMutationBytes(mutation)
	if err != nil {
		t.Fatal(err)
	}
	// v4 canonical form: step bounds and bottle seconds are explicit nulls,
	// and the session key is always present.
	want := `{"client_mutation_time":"2026-09-24T10:01:02.000000Z","entity_id":"20000000-0000-4000-8000-000000000001","entity_kind":"feed","import_id":null,"observed_entity_version":0,"operation":"create","payload":{"baby_id":"10000000-0000-4000-8000-000000000001","components":[{"ended_at":null,"id":"30000000-0000-4000-8000-000000000001","kind":"breast","seconds":480,"side":"left","started_at":null},{"contents":"breast_milk","ended_at":null,"id":"30000000-0000-4000-8000-000000000002","kind":"bottle","seconds":null,"started_at":null,"volume_ml":50.125}],"ended_at":null,"feeding_session_id":null,"kind":"breast","note":null,"started_at":"2026-09-24T10:00:00.000000Z","time_zone_id":"Europe/Zurich"},"payload_version":4}`
	if string(canonical) != want {
		t.Fatalf("canonical=%s\nwant=%s", canonical, want)
	}

	for name, changed := range map[string][]byte{
		"mixed breast fields":     bytes.Replace(raw, []byte(`"seconds":480`), []byte(`"seconds":480,"volume_ml":20`), 1),
		"missing bottle contents": bytes.Replace(raw, []byte(`,"contents":"breast_milk"`), nil, 1),
		"unknown component":       bytes.Replace(raw, []byte(`"kind":"breast","side":"left"`), []byte(`"kind":"unknown","side":"left"`), 1),
		"v1 fields in v4":         bytes.Replace(raw, []byte(`"components":`), []byte(`"sides":[],"components":`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMutation(changed); ErrorCodeOf(err) != "invalid_request" {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestNormalizedBabyNameExpansionFitsSchemaStorageBound(t *testing.T) {
	// U+0130 occupies two input bytes and expands during case folding. Repeating
	// it 100 times exercises the maximum valid Unicode-scalar name length.
	input := strings.Repeat("İ", 100)
	if len(input) != 200 {
		t.Fatalf("test input bytes=%d", len(input))
	}
	normalized, err := NormalizeBabyName(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 300 || len(normalized) > 1600 {
		t.Fatalf("normalized bytes=%d input=%d schema_bound=1600", len(normalized), len(input))
	}
}

func TestCanonicalTimestampRoundsTiesToEven(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"2026-09-20T12:00:00.1234564Z", "2026-09-20T12:00:00.123456Z"},
		{"2026-09-20T12:00:00.1234565Z", "2026-09-20T12:00:00.123456Z"},
		{"2026-09-20T12:00:00.1234575Z", "2026-09-20T12:00:00.123458Z"},
		{"2026-09-20T14:00:00+02:00", "2026-09-20T12:00:00.000000Z"},
		{"1899-12-31T23:59:59.9999995Z", "1900-01-01T00:00:00.000000Z"},
	} {
		got, err := NormalizeTimestamp(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("NormalizeTimestamp(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, invalid := range []string{"2100-12-31T23:59:59.9999995Z", "1899-12-31T23:59:59.999998Z"} {
		if got, err := NormalizeTimestamp(invalid); err == nil {
			t.Errorf("NormalizeTimestamp(%q) = %q, want post-rounding range error", invalid, got)
		}
	}
}

func TestCanonicalJSONEmitsLineSeparatorsLiterally(t *testing.T) {
	mutation := mustMutation(t, readV1Fixture(t, "feed-solids.json"))
	payload := mutation.Payload.(FeedPayload)
	note := "before\u2028middle\u2029after"
	payload.Note = &note
	mutation.Payload = payload
	canonical, err := canonicalMutationBytes(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte(`\u2028`)) || bytes.Contains(canonical, []byte(`\u2029`)) ||
		!bytes.Contains(canonical, []byte("before\u2028middle\u2029after")) {
		t.Fatalf("canonical JSON escaped line separators: %s", canonical)
	}
	literal := `before\u2028middle\u2029after`
	payload.Note = &literal
	mutation.Payload = payload
	literalCanonical, err := canonicalMutationBytes(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(literalCanonical, []byte(`before\\u2028middle\\u2029after`)) ||
		bytes.Contains(literalCanonical, []byte("before\u2028middle\u2029after")) {
		t.Fatalf("canonical JSON corrupted literal backslash escapes: %s", literalCanonical)
	}
	literalHash, _ := CanonicalMutationHash(mutation)
	payload.Note = &note
	mutation.Payload = payload
	separatorHash, _ := CanonicalMutationHash(mutation)
	if literalHash == separatorHash {
		t.Fatal("literal backslash escapes and line separators produced the same hash")
	}
}

func TestCanonicalJSONMatchesIndependentFixtureLiteral(t *testing.T) {
	var vectors struct {
		Mutation struct {
			CanonicalJSON string `json:"canonical_json"`
			SHA256        string `json:"sha256"`
		} `json:"mutation"`
		Decimals []struct {
			Input     string `json:"input"`
			Canonical string `json:"canonical"`
		} `json:"decimals"`
		Timestamps []struct {
			Input     string `json:"input"`
			Canonical string `json:"canonical"`
			Valid     bool   `json:"valid"`
		} `json:"timestamps"`
		Unicode struct {
			JSON struct {
				Actual        string `json:"actual"`
				Literal       string `json:"literal"`
				CanonicalJSON string `json:"canonical_json"`
				SHA256        string `json:"sha256"`
			} `json:"json"`
		} `json:"unicode"`
		MinimumLimits       Limits `json:"minimum_effective_limits"`
		EncodedPageBoundary struct {
			MaximumBytes           int   `json:"maximum_bytes"`
			EncodedItemBytes       []int `json:"encoded_item_bytes"`
			ExpectedPageItemCounts []int `json:"expected_page_item_counts"`
		} `json:"encoded_page_boundary"`
		SessionContentHashes struct {
			SleepV2 sessionContentHashVector `json:"sleep_v2"`
			TimerV5 sessionContentHashVector `json:"timer_v5"`
		} `json:"session_content_hashes"`
	}
	if err := json.Unmarshal(readV1Fixture(t, "canonical-vectors.json"), &vectors); err != nil {
		t.Fatal(err)
	}
	mutation := mustMutation(t, readV1Fixture(t, "feed-bottle.json"))
	canonical, err := canonicalMutationBytes(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != vectors.Mutation.CanonicalJSON {
		t.Fatalf("canonical JSON = %s, want literal %s", canonical, vectors.Mutation.CanonicalJSON)
	}
	for _, vector := range vectors.Decimals {
		decimal, err := ParseDecimal(vector.Input)
		if err != nil || decimal.String() != vector.Canonical {
			t.Errorf("decimal %s = %v, %v; want %s", vector.Input, decimal, err, vector.Canonical)
		}
	}
	for _, vector := range vectors.Timestamps {
		if got := normalizeTimestampForHash(vector.Input); got != vector.Canonical {
			t.Errorf("timestamp hash normalization %q = %q, want %q", vector.Input, got, vector.Canonical)
		}
		got, err := NormalizeTimestamp(vector.Input)
		if vector.Valid && (err != nil || got != vector.Canonical) {
			t.Errorf("valid timestamp %q = %q, %v", vector.Input, got, err)
		}
		if !vector.Valid && err == nil {
			t.Errorf("invalid-domain timestamp %q accepted as %q", vector.Input, got)
		}
	}
	unicodeCanonical, err := marshalCanonicalJSON(map[string]any{
		"literal": vectors.Unicode.JSON.Literal,
		"actual":  vectors.Unicode.JSON.Actual,
	})
	if err != nil {
		t.Fatal(err)
	}
	unicodeHash := sha256.Sum256(unicodeCanonical)
	if string(unicodeCanonical) != vectors.Unicode.JSON.CanonicalJSON || hex.EncodeToString(unicodeHash[:]) != vectors.Unicode.JSON.SHA256 {
		t.Fatalf("unicode vector = %q/%x, want %q/%s", unicodeCanonical, unicodeHash, vectors.Unicode.JSON.CanonicalJSON, vectors.Unicode.JSON.SHA256)
	}
	if err := vectors.MinimumLimits.Validate(); err != nil {
		t.Fatalf("minimum custom-server limits: %v", err)
	}
	if vectors.EncodedPageBoundary.MaximumBytes != 1<<20 || len(vectors.EncodedPageBoundary.EncodedItemBytes) != 2 || len(vectors.EncodedPageBoundary.ExpectedPageItemCounts) != 2 {
		t.Fatalf("encoded-page boundary vector = %+v", vectors.EncodedPageBoundary)
	}
	for name, vector := range map[string]sessionContentHashVector{
		"sleep_v2": vectors.SessionContentHashes.SleepV2, "timer_v5": vectors.SessionContentHashes.TimerV5,
	} {
		mutation, err := DecodeMutation([]byte(vector.MutationJSON))
		if err != nil {
			t.Fatalf("%s: decode mutation: %v", name, err)
		}
		if mutation.EntityKind != vector.EntityKind || uint32(vector.PayloadVersion) != mutation.PayloadVersion {
			t.Fatalf("%s: mutation kind/version = %s/%d, want %s/%d", name, mutation.EntityKind, mutation.PayloadVersion, vector.EntityKind, vector.PayloadVersion)
		}
		hash, err := CanonicalContentHash(mutation.EntityKind, mutation.Payload)
		if err != nil {
			t.Fatalf("%s: content hash: %v", name, err)
		}
		if got := hex.EncodeToString(hash[:]); got != vector.SHA256 {
			t.Fatalf("%s: content hash = %s, want %s (Swift's ContentHash must agree)", name, got, vector.SHA256)
		}
	}
}

// sessionContentHashVector is the shared sleep v2 / timer v5 content-hash
// vector in canonical-vectors.json. Swift's
// ProtocolFixtureConformanceTests.sleepV2AndTimerV5ContentHashesMatchTheServerVectors
// checks the same fixture against ProtocolCanonicalizer.contentSHA256.
type sessionContentHashVector struct {
	EntityKind     string `json:"entity_kind"`
	PayloadVersion int    `json:"payload_version"`
	MutationJSON   string `json:"mutation_json"`
	SHA256         string `json:"sha256"`
}

func TestCanonicalNullAndCollectionNormalization(t *testing.T) {
	raw := readV1Fixture(t, "feed-bottle.json")
	omitted := bytes.Replace(raw, []byte(`,"note":null,`), []byte(`,`), 1)
	a := mustMutation(t, raw)
	b := mustMutation(t, omitted)
	ah, _ := CanonicalMutationHash(a)
	bh, _ := CanonicalMutationHash(b)
	if ah != bh {
		t.Fatal("omitted optional note did not normalize to null")
	}
	withNullComponents := bytes.Replace(raw, []byte(`"components":[{"id":"31000000-0000-4000-8000-000000000001","kind":"bottle","volume_ml":1,"contents":"formula","seconds":null,"started_at":null,"ended_at":null}]`), []byte(`"components":null`), 1)
	if _, err := DecodeMutation(withNullComponents); ErrorCodeOf(err) != "invalid_request" {
		t.Fatalf("explicit null components error = %v", err)
	}
}

func TestDecodeMutationNormalizesValidPayloadBeforeAdmission(t *testing.T) {
	raw := readV1Fixture(t, "baby.json")
	raw = bytes.Replace(raw, []byte(`"name":"Élie"`), []byte(`"name":"  E\u0301lie\u00a0"`), 1)
	mutation := mustMutation(t, raw)
	if got := mutation.Payload.(BabyPayload).Name; got != "Élie" {
		t.Fatalf("decoded display name = %q, want NFC-trimmed Élie", got)
	}
	raw = readV1Fixture(t, "feed-solids.json")
	raw = bytes.Replace(raw, []byte(`"started_at":"2026-09-20T13:00:00Z"`), []byte(`"started_at":"2026-09-20T15:00:00+02:00"`), 1)
	raw = bytes.Replace(raw, []byte(`"note":"Lunch"`), []byte(`"note":"Cafe\u0301"`), 1)
	mutation = mustMutation(t, raw)
	feed := mutation.Payload.(FeedPayload)
	if feed.StartedAt != "2026-09-20T13:00:00.000000Z" || feed.Note == nil || *feed.Note != "Café" {
		t.Fatalf("decoded feed = %+v, want canonical timestamp and NFC note", feed)
	}
}

func TestCanonicalContentHashExcludesBabyRelationshipOnly(t *testing.T) {
	mutation := mustMutation(t, readV1Fixture(t, "feed-bottle.json"))
	payload := mutation.Payload.(FeedPayload)
	first, err := CanonicalContentHash("feed", payload)
	if err != nil {
		t.Fatal(err)
	}
	payload.BabyID = "10000000-0000-4000-8000-000000000099"
	second, _ := CanonicalContentHash("feed", payload)
	if first != second {
		t.Fatal("content hash included mapped baby relationship")
	}
	differentKind, _ := CanonicalContentHash("sleep", payload)
	if first == differentKind {
		t.Fatal("content hash omitted entity kind")
	}
}

func TestRejectDuplicateNestedKeys(t *testing.T) {
	raw := readV1Fixture(t, "feed-bottle.json")
	needle := []byte(`"baby_id":"10000000-0000-4000-8000-000000000001"`)
	duplicate := append(append([]byte{}, needle...), append([]byte(","), needle...)...)
	raw = bytes.Replace(raw, needle, duplicate, 1)
	if _, err := DecodeMutation(raw); ErrorCodeOf(err) != "invalid_request" {
		t.Fatalf("nested duplicate code = %q, error = %v", ErrorCodeOf(err), err)
	}
}

func TestRejectPresentationAndActorFields(t *testing.T) {
	raw := readV1Fixture(t, "baby.json")
	for _, field := range []string{`"created_by":"caregiver"`, `"live_activity_id":"activity"`, `"photo_data":"AA=="`} {
		withField := bytes.Replace(raw, []byte(`"sort_order":0`), []byte(`"sort_order":0,`+field), 1)
		if _, err := DecodeMutation(withField); ErrorCodeOf(err) != "invalid_request" {
			t.Errorf("field %s code = %q, error = %v", field, ErrorCodeOf(err), err)
		}
	}
}

func TestRejectMalformedMutationShapeBeforeReceipt(t *testing.T) {
	raw := readV1Fixture(t, "feed-bottle.json")
	for name, changed := range map[string][]byte{
		"exponent integer":        bytes.Replace(raw, []byte(`"observed_entity_version":0`), []byte(`"observed_entity_version":0e0`), 1),
		"unsafe integer":          bytes.Replace(raw, []byte(`"observed_entity_version":0`), []byte(`"observed_entity_version":9007199254740992`), 1),
		"unknown payload version": bytes.Replace(raw, []byte(`"payload_version":4`), []byte(`"payload_version":5`), 1),
		"trailing value":          append(append([]byte{}, raw...), []byte(`{}`)...),
		"unpaired surrogate":      bytes.Replace(raw, []byte(`"note":null`), []byte(`"note":"\ud800"`), 1),
		"zero mutation UUID":      bytes.Replace(raw, []byte(`"mutation_id":"01000000-0000-4000-8000-000000000003"`), []byte(`"mutation_id":"00000000-0000-0000-0000-000000000000"`), 1),
		"envelope metadata":       bytes.Replace(raw, []byte(`{"mutation_id"`), []byte(`{"protocol_version":"v1","capabilities":["records-v1"],"mutation_id"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMutation(changed); ErrorCodeOf(err) != "invalid_request" {
				t.Fatalf("code = %q, error = %v", ErrorCodeOf(err), err)
			}
		})
	}
}

// TestRemovedLegacyTimerOperationsAreInvalidRequest pins that the operations
// only an older client ever sent — switch side, pause, attach/begin a
// bottle — no longer decode at all. The payload shape stays
// `timer_start`-shaped; the point is that the operation name alone, now
// unmatched for any payload version, fails before the payload is even
// inspected.
func TestRemovedLegacyTimerOperationsAreInvalidRequest(t *testing.T) {
	raw := readV1Fixture(t, "timer-feed.json")
	for _, operation := range []string{"timer_switch_side", "timer_pause", "timer_attach_bottle", "timer_start_bottle"} {
		t.Run(operation, func(t *testing.T) {
			changed := bytes.Replace(raw, []byte(`"operation":"timer_start"`), []byte(`"operation":"`+operation+`"`), 1)
			if _, err := DecodeMutation(changed); ErrorCodeOf(err) != "invalid_request" {
				t.Fatalf("code = %q, error = %v", ErrorCodeOf(err), err)
			}
		})
	}
}

func TestDecodeMutationRejectsInvalidUTF8Explicitly(t *testing.T) {
	raw := readV1Fixture(t, "feed-bottle.json")
	invalid := bytes.Replace(raw, []byte(`"note":null`), append([]byte(`"note":"`), append([]byte{0xff}, []byte(`"`)...)...), 1)
	if _, err := DecodeMutation(invalid); ErrorCodeOf(err) != "invalid_request" {
		t.Fatalf("invalid UTF-8 code = %q, error = %v", ErrorCodeOf(err), err)
	}
}

func TestRejectNestedMissingOrNullRequiredFields(t *testing.T) {
	feed := readV1Fixture(t, "feed-breast.json")
	for name, raw := range map[string][]byte{
		"missing side":         bytes.Replace(feed, []byte(`"side":"left","seconds":300`), []byte(`"seconds":300`), 1),
		"null seconds":         bytes.Replace(feed, []byte(`"seconds":300`), []byte(`"seconds":null`), 1),
		"null baby sort order": bytes.Replace(readV1Fixture(t, "baby.json"), []byte(`"sort_order":0`), []byte(`"sort_order":null`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMutation(raw); ErrorCodeOf(err) != "invalid_request" {
				t.Fatalf("code = %q, error = %v", ErrorCodeOf(err), err)
			}
		})
	}
	omittedDefault := bytes.Replace(readV1Fixture(t, "baby.json"), []byte(`,"sort_order":0`), nil, 1)
	mutation := mustMutation(t, omittedDefault)
	if mutation.Payload.(BabyPayload).SortOrder != 0 {
		t.Fatal("omitted baby sort_order did not default to zero")
	}
}

func TestDeletePayloadShapeDependsOnEntityKind(t *testing.T) {
	var fixture map[string]json.RawMessage
	if err := json.Unmarshal(readV1Fixture(t, "mutations.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"baby null parent":      bytes.Replace(fixture["delete_baby"], []byte(`"payload":{}`), []byte(`"payload":{"baby_id":null}`), 1),
		"baby unknown key":      bytes.Replace(fixture["delete_baby"], []byte(`"payload":{}`), []byte(`"payload":{"future":true}`), 1),
		"record missing parent": bytes.Replace(fixture["delete_record"], []byte(`"payload":{"baby_id":"10000000-0000-4000-8000-000000000001"}`), []byte(`"payload":{}`), 1),
		"record null parent":    bytes.Replace(fixture["delete_record"], []byte(`"payload":{"baby_id":"10000000-0000-4000-8000-000000000001"}`), []byte(`"payload":{"baby_id":null}`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMutation(raw); ErrorCodeOf(err) != "invalid_request" {
				t.Fatalf("code = %q, error = %v", ErrorCodeOf(err), err)
			}
		})
	}
}

// TestCanonicalMutationHashOfEveryFixture pins the receipt hash of every
// fixture mutation, one per payload type. The Swift port
// (ProtocolFixtureConformanceTests.mutationHashOfEveryFixtureMatchesTheServer)
// asserts the same literals, so a projection that drifts on either side, such
// as a part's absent step bounds, fails its own test.
func TestCanonicalMutationHashOfEveryFixture(t *testing.T) {
	var batch map[string]json.RawMessage
	if err := json.Unmarshal(readV1Fixture(t, "mutations.json"), &batch); err != nil {
		t.Fatal(err)
	}
	for _, vector := range []struct{ fixture, key, sha256 string }{
		{"baby.json", "", "3ed382f4152d6fa67d95e5a784db2f1bcdc64c7962dfb666a9bbcb14c0eb1574"},
		{"feed-bottle.json", "", "4d28b2f9e3d1d1e59f45de30bd7a1aa362adcc6f1b3e06efb7f3b8a97b5fde11"},
		{"feed-breast.json", "", "6c2031e5dce8b7244b069e91ac47e0a1af39881b3c95d6c23674d186c3a20af5"},
		{"feed-combination.json", "", "8eafe1234e1f54264882fe0e1e719d5a94c8996387ed436536abd7fd86962052"},
		{"feed-solids.json", "", "97ed3d0ca27e29f40e72d62c42ecec0d353cb0b3f266c7c57f9a282a9b5341e7"},
		{"sleep.json", "", "6f4dac1e58bf7297ee069a6f764a18dcbf5beba4dd133eaae4e7c05b1c10addd"},
		{"diaper.json", "", "5970a6698e951a13edad6cad5acc1ae352da289fc62c636535d077635f41ad27"},
		{"supplement.json", "", "5fcc3ae7f9374061cfa8f72b225fe6f8e52ca73e9241bfa25736f970421ce30e"},
		{"growth-profile.json", "", "c5c274fb7b44c8f91f79a32a78def410d021bd7c074fb885c34ec02ab752c220"},
		{"measurement.json", "", "e99207fb7e59b99f2629a2af12052fa690fc9266a9d057b11992b5fa7a86b510"},
		{"timer-feed.json", "", "4b3df663c3674fc2b755e8a35d09752ede55c0088446bc4e20a3e7b9d5949c77"},
		{"timer-sleep.json", "", "5454d2982ab7d0a1a57d7ca8301bc70faae2d347c2f3d75d79770cfa2a2d8a21"},
		{"mutations.json", "delete_baby", "e7f1d0a8928958c575ae858747f808acc12daea4ce85d5bb67662367afe43cf4"},
		{"mutations.json", "delete_record", "182467c6d9c2ba3282bd3fbc530d6069070cc35f6e113da6241aa3af3f951941"},
		{"mutations.json", "timer_bottle", "78bb831d5a7a25a6fd22f89d6bb3fe967dc5ca6c48807aff0114f31b1989cc24"},
		{"mutations.json", "timer_edit", "2722b6c67c424c981c7a41c252942b0aae5bc3291745bbccb288ebd985128e27"},
		{"mutations.json", "timer_stop", "59a59902d50e8359bc94488e267a0da55721d8be8eb77123962b09b27a372dc8"},
	} {
		raw := []byte(batch[vector.key])
		if vector.key == "" {
			raw = readV1Fixture(t, vector.fixture)
		}
		hash, err := CanonicalMutationHash(mustMutation(t, raw))
		if err != nil {
			t.Fatalf("%s %s: %v", vector.fixture, vector.key, err)
		}
		if got := hex.EncodeToString(hash[:]); got != vector.sha256 {
			t.Errorf("%s %s: sha256 = %s, want %s", vector.fixture, vector.key, got, vector.sha256)
		}
	}
}

func mustMutation(t *testing.T, raw []byte) Mutation {
	t.Helper()
	mutation, err := DecodeMutation(raw)
	if err != nil {
		t.Fatalf("DecodeMutation: %v", err)
	}
	return mutation
}

func readV1Fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../api/fixtures/v1", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return raw
}

func TestBabyMutationHashOmitsAbsentIcon(t *testing.T) {
	without := BabyPayload{Name: "Élie", BirthDate: "2026-01-12"}
	value, err := canonicalPayloadValue(without, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value["icon"]; ok {
		t.Fatal("absent icon must not appear in the canonical payload")
	}
	icon := "boy.2"
	with := BabyPayload{Name: "Élie", BirthDate: "2026-01-12", Icon: &icon}
	value, err = canonicalPayloadValue(with, false)
	if err != nil {
		t.Fatal(err)
	}
	if value["icon"] != "boy.2" {
		t.Fatalf("icon missing from canonical payload: %v", value)
	}
}

func TestDecodeMutationAcceptsOptionalBabyIcon(t *testing.T) {
	raw := readV1Fixture(t, "baby.json")
	withIcon := bytes.Replace(raw, []byte(`"sort_order":0}`), []byte(`"sort_order":0,"icon":"girl.3"}`), 1)
	if bytes.Equal(withIcon, raw) {
		t.Fatal("fixture shape changed; icon was not inserted")
	}
	payload := mustMutation(t, withIcon).Payload.(BabyPayload)
	if payload.Icon == nil || *payload.Icon != "girl.3" {
		t.Fatalf("decoded icon = %v, want girl.3", payload.Icon)
	}
	withNull := bytes.Replace(raw, []byte(`"sort_order":0}`), []byte(`"sort_order":0,"icon":null}`), 1)
	payload = mustMutation(t, withNull).Payload.(BabyPayload)
	if payload.Icon != nil {
		t.Fatalf("decoded null icon = %v, want nil", *payload.Icon)
	}
}
