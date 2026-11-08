package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCapabilityConventionMatchesIdentity(t *testing.T) {
	grammar := regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	for _, capability := range KnownCapabilities() {
		if !grammar.MatchString(capability) {
			t.Errorf("known capability %q violates shared grammar", capability)
		}
	}
	for _, invalid := range []string{"identity_v1", "records_v1"} {
		if err := ValidateCapabilities([]string{invalid}, KnownCapabilities()); ErrorCodeOf(err) != "unsupported_capability" {
			t.Errorf("invalid capability %q code = %q", invalid, ErrorCodeOf(err))
		}
	}
	if err := ValidateCapabilities([]string{"future-v1"}, IdentityCapabilities()); ErrorCodeOf(err) != "unsupported_capability" {
		t.Fatalf("identity negotiation code = %q", ErrorCodeOf(err))
	}
	if err := ValidateRouteCapabilities(SurfaceSync, []string{"future-v1"}, []string{"future-v1"}, KnownCapabilities()); ErrorCodeOf(err) != "unsupported_capability" {
		t.Fatalf("sync negotiation code = %q", ErrorCodeOf(err))
	}
}

func TestCapabilityMatrixIsRouteSpecific(t *testing.T) {
	for _, tc := range []struct {
		surface Surface
		header  []string
		body    []string
	}{
		{SurfaceSync, []string{CapabilityRecordsV1}, []string{CapabilityRecordsV1}},
		{SurfaceSyncTimer, []string{CapabilityRecordsV1, CapabilityTimerCommandsV1}, []string{CapabilityRecordsV1, CapabilityTimerCommandsV1}},
		{SurfaceSnapshot, []string{CapabilityRecordsV1, CapabilitySnapshotV1}, nil},
		{SurfaceImport, []string{CapabilityRecordsV1, CapabilityImportResolverV1}, []string{CapabilityRecordsV1, CapabilityImportResolverV1}},
		{SurfaceLive, []string{CapabilityRecordsV1, CapabilityLiveHintsV1}, nil},
		{SurfaceExport, []string{CapabilityExportV1}, []string{CapabilityExportV1}},
		{SurfaceLiveActivity, []string{CapabilityLiveActivityPushV1}, []string{CapabilityLiveActivityPushV1}},
	} {
		if err := ValidateRouteCapabilities(tc.surface, tc.header, tc.body, KnownCapabilities()); err != nil {
			t.Errorf("surface %q rejected valid capabilities: %v", tc.surface, err)
		}
	}
	if err := ValidateRouteCapabilities(SurfaceLive, []string{CapabilityRecordsV1}, nil, KnownCapabilities()); ErrorCodeOf(err) != "unsupported_capability" {
		t.Fatalf("live route accepted polling-only capability set: %v", err)
	}
	for name, err := range map[string]error{
		"missing":       ValidateRouteCapabilities(SurfaceSnapshot, []string{CapabilityRecordsV1}, nil, KnownCapabilities()),
		"body mismatch": ValidateRouteCapabilities(SurfaceSync, []string{CapabilityRecordsV1}, []string{CapabilityRecordsV1, CapabilitySnapshotV1}, KnownCapabilities()),
		"duplicate":     ValidateRouteCapabilities(SurfaceSync, []string{CapabilityRecordsV1, CapabilityRecordsV1}, []string{CapabilityRecordsV1, CapabilityRecordsV1}, KnownCapabilities()),
		"excess":        ValidateRouteCapabilities(SurfaceImport, []string{CapabilityRecordsV1, CapabilityImportResolverV1, CapabilitySnapshotV1}, []string{CapabilityRecordsV1, CapabilityImportResolverV1, CapabilitySnapshotV1}, KnownCapabilities()),
	} {
		if ErrorCodeOf(err) != "unsupported_capability" {
			t.Errorf("%s code = %q, error = %v", name, ErrorCodeOf(err), err)
		}
	}
	if err := ValidateRouteCapabilities(SurfaceSyncTimer, []string{CapabilityRecordsV1, CapabilityTimerCommandsV1}, []string{CapabilityRecordsV1, CapabilityTimerCommandsV1}, ServerCapabilities()); err != nil {
		t.Fatalf("advertised timer capability rejected: %v", err)
	}
}

func TestResponseAllowsAdditiveFields(t *testing.T) {
	raw := readV1Fixture(t, "sync-page.json")
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["future_field"] = json.RawMessage(`true`)
	withFuture, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var response SyncResponse
	if err := json.Unmarshal(withFuture, &response); err != nil || response.ServerInstanceID == "" {
		t.Fatalf("additive response field rejected: %+v, %v", response, err)
	}
}

func TestKnownResponsePayloadAllowsAdditiveFields(t *testing.T) {
	raw := []byte(`{"entity_kind":"baby","meta":{"id":"10000000-0000-4000-8000-000000000001","family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","entity_version":1,"creator_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","last_editor_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-20T12:00:00.000000Z","tombstoned_at":null},"payload":{"name":"Élie","birth_date":"2026-01-12","sort_order":0,"future_field":true}}`)
	var entity CanonicalEntity
	if err := json.Unmarshal(raw, &entity); err != nil {
		t.Fatalf("additive known response payload rejected: %v", err)
	}
	if entity.Payload.(BabyPayload).Name != "Élie" {
		t.Fatalf("decoded entity = %+v", entity)
	}
}

func TestKnownResponsePayloadStillRequiresKnownFields(t *testing.T) {
	raw := []byte(`{"entity_kind":"baby","meta":{"id":"10000000-0000-4000-8000-000000000001","family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","entity_version":1,"creator_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","last_editor_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-20T12:00:00.000000Z","tombstoned_at":null},"payload":{"birth_date":"2026-01-12","sort_order":0,"future_field":true}}`)
	var entity CanonicalEntity
	if err := json.Unmarshal(raw, &entity); err == nil {
		t.Fatal("known response payload accepted missing required name")
	}
}

func TestCanonicalEntityRequiresNonNullMeta(t *testing.T) {
	for name, raw := range map[string][]byte{
		"missing": []byte(`{"entity_kind":"baby","payload":{"name":"Élie","birth_date":"2026-01-12","sort_order":0}}`),
		"null":    []byte(`{"entity_kind":"baby","meta":null,"payload":{"name":"Élie","birth_date":"2026-01-12","sort_order":0}}`),
	} {
		t.Run(name, func(t *testing.T) {
			var entity CanonicalEntity
			if err := json.Unmarshal(raw, &entity); err == nil {
				t.Fatal("canonical entity accepted absent metadata")
			}
		})
	}
}

func TestOutcomeAndLiveHintValidateCursorsOnUnmarshal(t *testing.T) {
	validCursor := `{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":9007199254740991}`
	invalid := map[string]string{
		"server UUID":    strings.Replace(validCursor, "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", "not-a-uuid", 1),
		"recovery epoch": strings.Replace(validCursor, `"recovery_epoch":1`, `"recovery_epoch":9007199254740992`, 1),
		"family UUID":    strings.Replace(validCursor, "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", "not-a-uuid", 1),
		"sequence":       strings.Replace(validCursor, "9007199254740991", "9007199254740992", 1),
	}
	outcome := func(cursor string) []byte {
		return []byte(`{"mutation_id":"01000000-0000-4000-8000-000000000001","status":"applied","original_status":null,"canonical_entity":null,"related_entities":[],"cursor":` + cursor + `,"code":null,"overwrote_newer_version":false}`)
	}
	hint := func(cursor string) []byte {
		return []byte(`{"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","cursor":` + cursor + `}`)
	}
	var validOutcome Outcome
	if err := json.Unmarshal(outcome(validCursor), &validOutcome); err != nil {
		t.Fatalf("maximum safe outcome cursor rejected: %v", err)
	}
	var validHint Hint
	if err := json.Unmarshal(hint(validCursor), &validHint); err != nil {
		t.Fatalf("maximum safe live hint cursor rejected: %v", err)
	}
	for name, cursor := range invalid {
		t.Run(name, func(t *testing.T) {
			var decodedOutcome Outcome
			if err := json.Unmarshal(outcome(cursor), &decodedOutcome); err == nil {
				t.Fatal("outcome accepted invalid cursor")
			}
			var decodedHint Hint
			if err := json.Unmarshal(hint(cursor), &decodedHint); err == nil {
				t.Fatal("live hint accepted invalid cursor")
			}
		})
	}
	nested := []byte(`{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"outcomes":[` + string(outcome(invalid["sequence"])) + `],"changes":[],"high_water":{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":1},"next_page_token":null}`)
	var response SyncResponse
	if err := json.Unmarshal(nested, &response); err == nil {
		t.Fatal("sync response accepted invalid nested outcome cursor")
	}
}

func TestLiveHintRejectsUnknownDuplicateAndMismatchedFamily(t *testing.T) {
	cursor := `{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":1}`
	for name, raw := range map[string]string{
		"unknown":    `{"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","cursor":` + cursor + `,"extra":true}`,
		"duplicate":  `{"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","cursor":` + cursor + `}`,
		"mismatched": `{"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c099","cursor":` + cursor + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			var hint Hint
			if err := json.Unmarshal([]byte(raw), &hint); err == nil {
				t.Fatal("invalid live hint accepted")
			}
		})
	}
}

func TestUnknownResourceRecovery(t *testing.T) {
	base := `{"cursor":{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":1},"entity_kind":"future","entity_id":"c1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","entity_version":1,"actor_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","device_id":"e1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","operation":"update","accepted_at":"2026-09-20T12:00:00.000000Z","optional":%s,"snapshot_or_tombstone":{"entity_kind":"future","meta":{},"payload":{}}}`
	changes, skipped, err := DecodeJournalChanges([]byte(`[` + sprintf(base, "true") + `]`))
	if err != nil || len(changes) != 0 || len(skipped) != 1 || skipped[0].Sequence != 1 {
		t.Fatalf("optional future resource = changes %d skipped %+v err %v", len(changes), skipped, err)
	}
	_, _, err = DecodeJournalChanges([]byte(`[` + sprintf(base, "false") + `]`))
	if !errors.Is(err, ErrUnsupportedResource) {
		t.Fatalf("required future resource error = %v", err)
	}
}

func TestSyncResponseAppliesUnknownEnvelopeRule(t *testing.T) {
	base := `{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"outcomes":[],"changes":[%s],"high_water":{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":2},"next_page_token":null}`
	change := `{"cursor":{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":1},"entity_kind":"future","entity_id":"c1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","entity_version":1,"actor_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","device_id":"e1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","operation":"delete","accepted_at":"2026-09-20T12:00:00.000000Z","optional":%s,"snapshot_or_tombstone":{"entity_kind":"future","meta":{},"payload":null}}`
	var optional SyncResponse
	if err := json.Unmarshal([]byte(sprintf(base, sprintf(change, "true"))), &optional); err != nil || len(optional.Changes) != 0 || len(optional.SkippedOptionalCursors) != 1 {
		t.Fatalf("optional sync response = %+v, err %v", optional, err)
	}
	var required SyncResponse
	err := json.Unmarshal([]byte(sprintf(base, sprintf(change, "false"))), &required)
	if !errors.Is(err, ErrUnsupportedResource) {
		t.Fatalf("required unknown response error = %v", err)
	}
	var unsupported UnsupportedResourceError
	if !errors.As(err, &unsupported) || unsupported.Cursor.Sequence != 1 {
		t.Fatalf("required unknown response did not retain cursor: %+v", unsupported)
	}
}

func TestOpenAPIOriginHasNoBasePath(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^\s+basePath:`).Match(raw) {
		t.Fatal("OpenAPI server identity exposes a basePath variable")
	}
}

func TestOpenAPICanonicalSchemasAreStrictAtRequestBoundary(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range [][]byte{
		[]byte("Mutation:"), []byte("BabyPayload:"), []byte("FeedPayload:"), []byte("TimerStartPayload:"),
		[]byte("ImportResolveRequest:"), []byte("CanonicalEntity:"), []byte("records-v1"), []byte("timer-commands-v1"),
	} {
		if !bytes.Contains(raw, required) {
			t.Errorf("OpenAPI omitted %q", required)
		}
	}
	for _, forbidden := range [][]byte{[]byte("photo_data:"), []byte("live_activity_id:"), []byte("created_by:")} {
		if bytes.Contains(raw, forbidden) {
			t.Errorf("OpenAPI exposed local-only field %q", forbidden)
		}
	}
}

func TestOpenAPIRepresentsRawRequestsAndTypedImportResponses(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	schemas := document.Components.Schemas
	decimal := schemas["CanonicalDecimal"]
	if _, exists := decimal["multipleOf"]; exists {
		t.Fatal("raw decimal request schema rejects valid rounding inputs")
	}
	if decimal["minimum"] != -0.0000005 || decimal["maximum"] != 2000.0000005 {
		t.Fatalf("raw decimal bounds = %v...%v", decimal["minimum"], decimal["maximum"])
	}
	mutation := schemas["Mutation"]
	properties := mutation["properties"].(map[string]any)
	for _, forbidden := range []string{"protocol_version", "capabilities"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("nested mutation exposes request envelope field %q", forbidden)
		}
	}
	required := stringSet(mutation["required"].([]any))
	if required["import_id"] {
		t.Fatal("optional mutation import_id is required")
	}
	payloadSchema := properties["payload"].(map[string]any)
	if _, overlapsByPayloadOnly := payloadSchema["oneOf"]; overlapsByPayloadOnly {
		t.Fatal("mutation payload oneOf overlaps delete and timer payloads")
	}
	mutationAllOf := mutation["allOf"].([]any)
	operationVariants := mutationAllOf[0].(map[string]any)["oneOf"].([]any)
	// Record deletes split by kind, because each kind accepts deletes up to its
	// own current payload version. A pump adds its create/update arm; its
	// delete names no baby, so it shares the baby's empty-payload arm.
	if len(operationVariants) != 17 {
		t.Fatalf("mutation operation variants = %d, want 17", len(operationVariants))
	}
	editRequired := stringSet(schemas["TimerEditPayload"]["required"].([]any))
	if !editRequired["baby_id"] || !editRequired["components"] || editRequired["started_at"] {
		t.Fatalf("timer edit required fields = %v", editRequired)
	}
	if _, exists := schemas["BabyDeletePayload"]; !exists {
		t.Fatal("OpenAPI omitted exact empty baby-delete payload")
	}
	recordDeleteRequired := stringSet(schemas["RecordDeletePayload"]["required"].([]any))
	if !recordDeleteRequired["baby_id"] {
		t.Fatal("record-delete payload does not require baby_id")
	}
	// Each record kind has one shape: its parts and its session link are
	// always present, and no scalar form of the parts exists.
	feedRequired := stringSet(schemas["FeedPayload"]["required"].([]any))
	for _, optional := range []string{"ended_at", "note"} {
		if feedRequired[optional] {
			t.Errorf("optional feed field %q is required", optional)
		}
	}
	for kind, keys := range map[string][]string{
		"FeedPayload":       {"components", "feeding_session_id"},
		"SleepPayload":      {"sleep_session_id"},
		"TimerStartPayload": {"completed_components", "session_id"},
	} {
		required := stringSet(schemas[kind]["required"].([]any))
		for _, key := range keys {
			if !required[key] {
				t.Errorf("%s does not require %s", kind, key)
			}
		}
		properties := schemas[kind]["properties"].(map[string]any)
		for _, legacy := range []string{"sides", "volume_ml", "contents", "foods", "amount", "completed_sides", "pending_volume_ml", "pending_contents"} {
			if _, exists := properties[legacy]; exists {
				t.Errorf("%s still documents %s", kind, legacy)
			}
		}
	}
	if _, exists := schemas["SideInterval"]; exists {
		t.Error("OpenAPI still documents SideInterval")
	}
	babyProperties := schemas["BabyPayload"]["properties"].(map[string]any)
	if babyProperties["name"].(map[string]any)["maxLength"] != 100 {
		t.Fatalf("baby name scalar max = %v", babyProperties["name"].(map[string]any)["maxLength"])
	}
	diaperProperties := schemas["DiaperPayload"]["properties"].(map[string]any)
	consistencyOptions := diaperProperties["consistency"].(map[string]any)["oneOf"].([]any)
	if consistencyOptions[0].(map[string]any)["maxLength"] != 64 {
		t.Fatalf("consistency scalar max = %v", consistencyOptions[0].(map[string]any)["maxLength"])
	}
	syncCapabilities := schemas["SyncRequest"]["properties"].(map[string]any)["capabilities"].(map[string]any)
	if _, exists := syncCapabilities["oneOf"]; !exists {
		t.Fatal("sync request does not allow timer capabilities")
	}
	resolveProperties := schemas["ImportResolveResponse"]["properties"].(map[string]any)
	for field, want := range map[string]string{"mappings": "#/components/schemas/BabyMapping", "conflicts": "#/components/schemas/ImportConflict"} {
		items := resolveProperties[field].(map[string]any)["items"].(map[string]any)
		if items["$ref"] != want {
			t.Errorf("%s items ref = %v, want %s", field, items["$ref"], want)
		}
	}
	resolveRequired := stringSet(schemas["ImportResolveResponse"]["required"].([]any))
	for _, field := range []string{"conflict_count", "conflicts_complete", "next_conflict_page_token"} {
		if !resolveRequired[field] {
			t.Errorf("import response does not require %q", field)
		}
	}
	if resolveProperties["next_conflict_page_token"].(map[string]any)["oneOf"].([]any)[0].(map[string]any)["maxLength"] != 512 {
		t.Fatal("import conflict page token is not bounded to 512 bytes")
	}
	resolveRequestProperties := schemas["ImportResolveRequest"]["properties"].(map[string]any)
	if resolveRequestProperties["chunk_index"].(map[string]any)["maximum"] != 4294967295 ||
		resolveProperties["next_chunk_index"].(map[string]any)["maximum"] != 4294967295 {
		t.Fatalf("import chunk index schemas do not match uint32 DTOs")
	}
	effectiveLimitProperties := schemas["EffectiveSyncLimits"]["properties"].(map[string]any)
	for _, field := range []string{
		"active_snapshot_account_limit", "unfinished_import_family_limit",
		"sync_mutation_family_per_minute", "sync_pull_account_per_minute",
	} {
		if _, exists := effectiveLimitProperties[field]; !exists {
			t.Errorf("effective limits omitted %q", field)
		}
	}
}

func TestUnknownNullCanonicalEntityFailsOutsideOptionalJournalEnvelope(t *testing.T) {
	unknown := []byte(`{"entity_kind":"future","meta":{"id":"10000000-0000-4000-8000-000000000001","family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","entity_version":1,"creator_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","last_editor_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-20T12:00:00.000000Z","tombstoned_at":"2026-09-20T12:00:00.000000Z"},"payload":null}`)
	var entity CanonicalEntity
	if err := json.Unmarshal(unknown, &entity); !errors.Is(err, ErrUnsupportedResource) {
		t.Fatalf("unknown null canonical entity error = %v", err)
	}
	snapshot := []byte(`{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"snapshot_id":"60000000-0000-4000-8000-000000000001","high_water":{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":0},"entities":[` + string(unknown) + `],"next_page_token":null}`)
	var response SnapshotResponse
	if err := json.Unmarshal(snapshot, &response); !errors.Is(err, ErrUnsupportedResource) {
		t.Fatalf("snapshot unknown null entity error = %v", err)
	}
	liveNull := bytes.Replace(unknown, []byte(`"entity_kind":"future"`), []byte(`"entity_kind":"baby"`), 1)
	liveNull = bytes.Replace(liveNull, []byte(`"tombstoned_at":"2026-09-20T12:00:00.000000Z"`), []byte(`"tombstoned_at":null`), 1)
	if err := json.Unmarshal(liveNull, &entity); err == nil {
		t.Fatal("null payload without tombstone metadata accepted")
	}
	knownTombstone := bytes.Replace(unknown, []byte(`"entity_kind":"future"`), []byte(`"entity_kind":"baby"`), 1)
	if err := json.Unmarshal(knownTombstone, &entity); err != nil || entity.Payload != nil {
		t.Fatalf("known tombstone rejected: %+v, %v", entity, err)
	}
}

func TestResponseSafeIntegerBoundaries(t *testing.T) {
	knownEntity := []byte(`{"entity_kind":"baby","meta":{"id":"10000000-0000-4000-8000-000000000001","family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","entity_version":9007199254740992,"creator_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","last_editor_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-20T12:00:00.000000Z","tombstoned_at":null},"payload":{"name":"Élie","birth_date":"2026-01-12","sort_order":0}}`)
	var entity CanonicalEntity
	if err := json.Unmarshal(knownEntity, &entity); err == nil {
		t.Fatal("unsafe entity version accepted")
	}
	var meta EntityMeta
	var entityEnvelope struct {
		Meta json.RawMessage `json:"meta"`
	}
	if err := json.Unmarshal(knownEntity, &entityEnvelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(entityEnvelope.Meta, &meta); err == nil {
		t.Fatal("unsafe standalone entity metadata version accepted")
	}
	if err := json.Unmarshal(bytes.Replace(knownEntity, []byte("9007199254740992"), []byte("9007199254740991"), 1), &entity); err != nil {
		t.Fatalf("maximum safe entity version rejected: %v", err)
	}
	change := []byte(`[{"cursor":{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":1},"entity_kind":"baby","entity_id":"10000000-0000-4000-8000-000000000001","entity_version":9007199254740992,"actor_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","device_id":"e1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","operation":"update","accepted_at":"2026-09-20T12:00:00.000000Z","optional":false,"snapshot_or_tombstone":{"entity_kind":"baby","meta":{"id":"10000000-0000-4000-8000-000000000001","family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","entity_version":1,"creator_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","last_editor_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","client_mutation_time":"2026-09-20T12:00:00.000000Z","accepted_at":"2026-09-20T12:00:00.000000Z","tombstoned_at":null},"payload":{"name":"Élie","birth_date":"2026-01-12","sort_order":0}}}]`)
	if _, _, err := DecodeJournalChanges(change); err == nil {
		t.Fatal("unsafe journal entity version accepted")
	}
	var journal JournalChange
	if err := json.Unmarshal(change[1:len(change)-1], &journal); err == nil {
		t.Fatal("unsafe standalone journal entity version accepted")
	}
	if _, _, err := DecodeJournalChanges(bytes.Replace(change, []byte("9007199254740992"), []byte("9007199254740991"), 1)); err != nil {
		t.Fatalf("maximum safe journal entity version rejected: %v", err)
	}
	var conflict ImportConflict
	conflictRaw := []byte(`{"conflict_id":"72000000-0000-4000-8000-000000000001","kind":"ambiguous_baby","local_baby_id":"10000000-0000-4000-8000-000000000001","record_id":null,"candidate_baby_ids":["10000000-0000-4000-8000-000000000002"],"candidate_versions":[9007199254740992]}`)
	if err := json.Unmarshal(conflictRaw, &conflict); err == nil {
		t.Fatal("unsafe import candidate version accepted")
	}
	if err := json.Unmarshal(bytes.Replace(conflictRaw, []byte("9007199254740992"), []byte("9007199254740991"), 1), &conflict); err != nil {
		t.Fatalf("maximum safe import candidate version rejected: %v", err)
	}
	var resolve ImportResolveResponse
	resolveRaw := []byte(`{"import_id":"70000000-0000-4000-8000-000000000001","resolver_revision":9007199254740992,"normalization_version":"nfc-trim-fold-nfc-v1","mappings":[],"conflicts":[],"mapping_complete":true,"next_chunk_index":1,"manifest_complete":true}`)
	if err := json.Unmarshal(resolveRaw, &resolve); err == nil {
		t.Fatal("unsafe import resolver revision accepted")
	}
	if err := json.Unmarshal(bytes.Replace(resolveRaw, []byte("9007199254740992"), []byte("9007199254740991"), 1), &resolve); err != nil {
		t.Fatalf("maximum safe import resolver revision rejected: %v", err)
	}
	snapshot := bytes.Replace(readV1Fixture(t, "snapshot-page.json"), []byte(`"recovery_epoch":1`), []byte(`"recovery_epoch":9007199254740992`), 1)
	var snapshotResponse SnapshotResponse
	if err := json.Unmarshal(snapshot, &snapshotResponse); err == nil {
		t.Fatal("unsafe snapshot recovery epoch accepted")
	}
	if err := json.Unmarshal(bytes.Replace(snapshot, []byte("9007199254740992"), []byte("9007199254740991"), 1), &snapshotResponse); err != nil {
		t.Fatalf("maximum safe snapshot recovery epoch rejected: %v", err)
	}
	snapshot = bytes.Replace(readV1Fixture(t, "snapshot-page.json"), []byte(`"sequence":2`), []byte(`"sequence":9007199254740992`), 1)
	if err := json.Unmarshal(snapshot, &snapshotResponse); err == nil {
		t.Fatal("unsafe snapshot high-water accepted")
	}
	if err := json.Unmarshal(bytes.Replace(snapshot, []byte("9007199254740992"), []byte("9007199254740991"), 1), &snapshotResponse); err != nil {
		t.Fatalf("maximum safe snapshot high-water rejected: %v", err)
	}
}

func stringSet(values []any) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value.(string)] = true
	}
	return result
}

func sprintf(format, value string) string {
	result := make([]byte, 0, len(format)+len(value))
	for index := 0; index < len(format); index++ {
		if index+1 < len(format) && format[index:index+2] == "%s" {
			result = append(result, value...)
			index++
			continue
		}
		result = append(result, format[index])
	}
	return string(result)
}

func TestFrozenV1ResponseAndImportFixturesDecode(t *testing.T) {
	decode := func(name string, target any) []byte {
		t.Helper()
		raw := readV1Fixture(t, name)
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		return raw
	}
	var outcomes struct {
		Outcomes []Outcome `json:"outcomes"`
	}
	decode("outcomes.json", &outcomes)
	if len(outcomes.Outcomes) != 5 {
		t.Fatalf("outcome fixture count = %d", len(outcomes.Outcomes))
	}
	var syncResponse SyncResponse
	decode("sync-page.json", &syncResponse)
	var snapshot SnapshotResponse
	decode("snapshot-page.json", &snapshot)
	var importFixture struct {
		Request  json.RawMessage       `json:"request"`
		Response ImportResolveResponse `json:"response"`
	}
	decode("import-resolve.json", &importFixture)
	if importFixture.Response.ConflictCount != 0 || !importFixture.Response.ConflictsComplete || importFixture.Response.NextConflictPageToken != nil {
		t.Fatalf("import conflict paging fixture = %+v", importFixture.Response)
	}
	var importRequest ImportResolveRequest
	if err := DecodeRequest(bytes.NewReader(importFixture.Request), 1<<20, &importRequest); err != nil {
		t.Fatalf("strict import request: %v", err)
	}
	if err := ValidateRouteCapabilities(SurfaceImport, importRequest.Capabilities, importRequest.Capabilities, KnownCapabilities()); err != nil {
		t.Fatalf("import capabilities: %v", err)
	}
	var conflictFixture struct {
		Request   json.RawMessage `json:"request"`
		KeepLocal json.RawMessage `json:"keep_local"`
	}
	decode("import-conflict.json", &conflictFixture)
	for _, raw := range []json.RawMessage{conflictFixture.Request, conflictFixture.KeepLocal} {
		var request ImportConflictRequest
		if err := DecodeRequest(bytes.NewReader(raw), 1<<20, &request); err != nil {
			t.Fatalf("strict import conflict request: %v", err)
		}
	}
	var hint Hint
	decode("live-hint.json", &hint)
	var errorsByCode map[string]ErrorResponse
	decode("errors.json", &errorsByCode)
	for code, response := range errorsByCode {
		if string(response.Error.Code) != code || response.Validate() != nil {
			t.Errorf("error fixture %q = %+v", code, response)
		}
	}
}

func TestV1FixtureDigestManifestCoversExactCorpus(t *testing.T) {
	directory := "../../api/fixtures/v1"
	var manifest struct {
		Version   string            `json:"version"`
		Algorithm string            `json:"algorithm"`
		Files     map[string]string `json:"files"`
	}
	raw, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatalf("decode fixture manifest: %v", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode fixture manifest: %v", err)
	}
	if manifest.Version != "v1" || manifest.Algorithm != "sha256" || len(manifest.Files) != 23 {
		t.Fatalf("manifest header/count = %q/%q/%d", manifest.Version, manifest.Algorithm, len(manifest.Files))
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	corpusCount := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == "manifest.json" {
			continue
		}
		corpusCount++
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(contents)
		if got, want := hex.EncodeToString(digest[:]), manifest.Files[entry.Name()]; got != want {
			t.Errorf("%s digest = %s, want %s", entry.Name(), got, want)
		}
	}
	if corpusCount != 23 {
		t.Fatalf("corpus file count = %d, want 23", corpusCount)
	}
}

func TestRejectExponentCountersAtAnyRequestDepth(t *testing.T) {
	raw := readV1Fixture(t, "import-resolve.json")
	var fixture struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture.Request = bytes.Replace(fixture.Request, []byte(`"sort_order":0`), []byte(`"sort_order":0e0`), 1)
	var request ImportResolveRequest
	if err := DecodeRequest(bytes.NewReader(fixture.Request), 1<<20, &request); ErrorCodeOf(err) != "invalid_request" {
		t.Fatalf("exponent counter code = %q, error = %v", ErrorCodeOf(err), err)
	}
}

func TestRejectUnsafeCursorAndResolverCounters(t *testing.T) {
	cursor := Cursor{ServerInstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 1, FamilyID: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", Sequence: MaxSafeInteger + 1}
	if err := cursor.Validate(); err == nil {
		t.Fatal("unsafe cursor sequence accepted")
	}
	raw := readV1Fixture(t, "import-conflict.json")
	var fixture struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture.Request = bytes.Replace(fixture.Request, []byte(`"resolver_revision":2`), []byte(`"resolver_revision":9007199254740992`), 1)
	var request ImportConflictRequest
	if err := DecodeRequest(bytes.NewReader(fixture.Request), 1<<20, &request); ErrorCodeOf(err) != "invalid_request" {
		t.Fatalf("unsafe resolver revision code = %q, error = %v", ErrorCodeOf(err), err)
	}
}
