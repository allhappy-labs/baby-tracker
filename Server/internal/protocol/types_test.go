package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/protocol"
	"gopkg.in/yaml.v3"
)

const fixturesDirectory = "../../api/fixtures"

func TestFixturesRoundTripThroughStrictWireTypes(t *testing.T) {
	t.Run("live hint", func(t *testing.T) {
		fixture, hint := decodeFixture[protocol.Hint](t, "v1/live-hint.json")
		if hint.FamilyID != hint.Cursor.FamilyID || hint.Cursor.Sequence != 42 {
			t.Fatalf("live hint=%+v", hint)
		}
		assertSemanticJSONEqual(t, fixture, hint)
	})

	t.Run("not ready error", func(t *testing.T) {
		fixture, response := decodeFixture[protocol.ErrorResponse](t, "error-not-ready.json")
		if err := response.Validate(); err != nil {
			t.Fatalf("validate error response: %v", err)
		}
		assertSemanticJSONEqual(t, fixture, response)
	})

	t.Run("server information", func(t *testing.T) {
		fixture, server := decodeFixture[protocol.ServerInfo](t, "server-info.json")
		if err := server.Validate(); err != nil {
			t.Fatalf("validate server information: %v", err)
		}
		assertSemanticJSONEqual(t, fixture, server)
	})

	t.Run("attachment metadata", func(t *testing.T) {
		fixture, metadata := decodeFixture[protocol.AttachmentMetadata](t, "attachment-metadata.json")
		if err := metadata.Validate(); err != nil {
			t.Fatalf("validate attachment metadata: %v", err)
		}
		if metadata.ID == "" || metadata.FamilyID == "" || metadata.CreatorAccountID == "" || metadata.MediaType == "" || metadata.SHA256 == "" || metadata.Lifecycle == "" {
			t.Fatal("attachment metadata omitted a required metadata field")
		}
		if (metadata.BabyID == "") == (metadata.RecordID == "") {
			t.Fatalf("attachment link = baby_id %q, record_id %q; want exactly one", metadata.BabyID, metadata.RecordID)
		}
		assertSemanticJSONEqual(t, fixture, metadata)

		encoded, err := json.Marshal(metadata)
		if err != nil {
			t.Fatalf("encode attachment metadata: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatalf("decode encoded attachment metadata: %v", err)
		}
		for _, forbidden := range []string{"bytes", "payload", "bucket", "object_key", "storage_key", "upload_url", "download_url"} {
			if _, found := fields[forbidden]; found {
				t.Errorf("attachment metadata exposed forbidden %q field", forbidden)
			}
		}
	})

	t.Run("family recovery", func(t *testing.T) {
		fixture, response := decodeFixture[protocol.FamilyRecoveryResponse](t, "family-recovery.json")
		if response.FamilyID == "" || response.State != "deletion_pending" || response.PurgeAfter == "" || !response.CanRecover {
			t.Fatalf("recovery response=%+v", response)
		}
		assertSemanticJSONEqual(t, fixture, response)
	})

	t.Run("family recovery command", func(t *testing.T) {
		data, err := os.ReadFile("../../api/fixtures/family-recovery-command.json")
		if err != nil {
			t.Fatal(err)
		}
		var request protocol.ConfirmedCommandRequest
		if err := protocol.DecodeRequest(bytes.NewReader(data), 64<<10, &request); err != nil {
			t.Fatal(err)
		}
		if err := protocol.ValidateRecoveryRequestCapabilities(&request); err != nil || !request.Confirmed {
			t.Fatalf("recovery request=%+v err=%v", request, err)
		}
	})

	t.Run("account deleted", func(t *testing.T) {
		fixture, response := decodeFixture[protocol.AccountDeletionResponse](t, "account-deleted.json")
		if response.CommandID == "" || response.Revision == 0 || response.AppleAuthorizationRevocation != "pending" {
			t.Fatalf("account deletion response=%+v", response)
		}
		assertSemanticJSONEqual(t, fixture, response)
	})
}

func TestServerInfoRejectsMissingHostnameAndEmptyQuery(t *testing.T) {
	for _, origin := range []string{"https://:443", "https://example.test?"} {
		t.Run(origin, func(t *testing.T) {
			_, server := decodeFixture[protocol.ServerInfo](t, "server-info.json")
			server.CanonicalOrigin = origin
			if !errors.Is(server.Validate(), protocol.ErrInvalidServerOrigin) {
				t.Fatalf("Validate accepted unsafe origin %q", origin)
			}
		})
	}
}

func TestOpenAPIBirthDateContract(t *testing.T) {
	schema := decodeOpenAPI(t).Components.Schemas["BirthDate"]
	if schema["type"] != "string" || schema["format"] != "date" || schema["example"] != "2026-09-20" {
		t.Fatalf("BirthDate must be a date-only string with a date-only example: %#v", schema)
	}
	pattern, ok := schema["pattern"].(string)
	if !ok {
		t.Fatal("BirthDate has no lexical pattern")
	}
	check := regexp.MustCompile(pattern)
	for value, want := range map[string]bool{"2026-09-20": true, "2026-9-20": false, "2026-09-2": false, "2026-09-20T00:00:00Z": false, " 2026-09-20": false} {
		if check.MatchString(value) != want {
			t.Errorf("BirthDate pattern match(%q) != %v", value, want)
		}
	}
}

func TestOpenAPIAdvertisesPersistentFamilyLimitRanges(t *testing.T) {
	schema := decodeOpenAPI(t).Components.Schemas["EffectiveSyncLimits"]
	properties := asMap(t, schema["properties"], "EffectiveSyncLimits.properties")
	for name, want := range map[string]struct{ minimum, maximum int }{
		"max_canonical_records_per_family":     {100, 1000000},
		"max_charged_storage_bytes_per_family": {16 << 20, 4 << 30},
	} {
		property := asMap(t, properties[name], "EffectiveSyncLimits.properties."+name)
		if property["type"] != "integer" || property["minimum"] != want.minimum || property["maximum"] != want.maximum {
			t.Errorf("%s=%#v, want integer range %d...%d", name, property, want.minimum, want.maximum)
		}
	}
}

func TestFixtureDecodingRejectsUnknownWireFields(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join(fixturesDirectory, "attachment-metadata.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(fixture, &object); err != nil {
		t.Fatalf("decode fixture object: %v", err)
	}
	object["unexpected"] = json.RawMessage(`true`)
	withUnknown, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("encode fixture with unknown field: %v", err)
	}

	var metadata protocol.AttachmentMetadata
	err = decodeStrictJSON(withUnknown, &metadata)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("strict decode error = %v, want unknown field rejection", err)
	}
}

func TestCursorValidatesOpaqueRecoveryScope(t *testing.T) {
	fixture := []byte(`{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":0}`)
	var cursor protocol.Cursor
	if err := decodeStrictJSON(fixture, &cursor); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if err := cursor.Validate(); err != nil {
		t.Fatalf("validate cursor: %v", err)
	}
	if cursor.ServerInstanceID == "" || cursor.FamilyID == "" || cursor.RecoveryEpoch == 0 {
		t.Fatalf("cursor = %#v, want server instance, family, and positive recovery epoch", cursor)
	}
	if cursor.Sequence != 0 {
		t.Fatalf("cursor sequence = %d, want non-negative zero fixture", cursor.Sequence)
	}

	cursor.RecoveryEpoch = 0
	if !errors.Is(cursor.Validate(), protocol.ErrInvalidRecoveryEpoch) {
		t.Fatalf("zero recovery epoch error = %v, want ErrInvalidRecoveryEpoch", cursor.Validate())
	}
	cursor.RecoveryEpoch = 9007199254740992
	if !errors.Is(cursor.Validate(), protocol.ErrInvalidRecoveryEpoch) {
		t.Fatalf("unsafe integer recovery epoch error = %v", cursor.Validate())
	}

	negativeSequence := []byte(`{"server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":1,"family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","sequence":-1}`)
	if err := decodeStrictJSON(negativeSequence, &cursor); err == nil {
		t.Fatal("negative cursor sequence decoded into a non-negative wire type")
	}
}

func TestServerInfoRejectsUnsafeIntegerRecoveryEpoch(t *testing.T) {
	server := protocol.ServerInfo{InstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 9007199254740991,
		CanonicalOrigin: "https://sync.example.test", APIVersions: []string{"v1"}, Capabilities: []string{}, AppleAuthorizationMode: "identity_token_only"}
	if err := server.Validate(); err != nil {
		t.Fatalf("maximum safe recovery epoch rejected: %v", err)
	}
	server.RecoveryEpoch++
	if !errors.Is(server.Validate(), protocol.ErrInvalidRecoveryEpoch) {
		t.Fatalf("unsafe recovery epoch error=%v", server.Validate())
	}
}

func TestServerInfoCarriesValidatedEffectiveSyncLimitsAdditively(t *testing.T) {
	limits := protocol.DefaultLimits()
	server := protocol.ServerInfo{
		InstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 1,
		CanonicalOrigin: "https://sync.example.test", APIVersions: []string{"v1"},
		Capabilities: []string{}, AppleAuthorizationMode: "identity_token_only", Limits: &limits,
	}
	if err := server.Validate(); err != nil {
		t.Fatalf("valid effective limits rejected: %v", err)
	}
	server.Limits.SyncBatchSize = 0
	if err := server.Validate(); err == nil {
		t.Fatal("invalid effective sync limits accepted")
	}
}

func TestAttachmentValidationRequiresOneTypedLinkAndCanonicalMetadata(t *testing.T) {
	_, fixture := decodeFixture[protocol.AttachmentMetadata](t, "attachment-metadata.json")
	fixture.BabyID = ""
	fixture.RecordID = ""
	if !errors.Is(fixture.Validate(), protocol.ErrInvalidAttachmentLink) {
		t.Fatalf("no link error = %v, want ErrInvalidAttachmentLink", fixture.Validate())
	}

	_, fixture = decodeFixture[protocol.AttachmentMetadata](t, "attachment-metadata.json")
	fixture.RecordID = "c1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
	if !errors.Is(fixture.Validate(), protocol.ErrInvalidAttachmentLink) {
		t.Fatalf("two links error = %v, want ErrInvalidAttachmentLink", fixture.Validate())
	}

	_, fixture = decodeFixture[protocol.AttachmentMetadata](t, "attachment-metadata.json")
	fixture.SHA256 = strings.ToUpper(fixture.SHA256)
	if !errors.Is(fixture.Validate(), protocol.ErrInvalidSHA256) {
		t.Fatalf("uppercase checksum error = %v, want ErrInvalidSHA256", fixture.Validate())
	}

	negativeByteSize := []byte(`{"id":"c1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","family_id":"b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","creator_account_id":"d1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","media_type":"image/jpeg","byte_size":-1,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","lifecycle":"pending","created_at":"2026-09-20T12:00:00Z","baby_id":"e1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"}`)
	if err := decodeStrictJSON(negativeByteSize, &fixture); err == nil {
		t.Fatal("negative byte size decoded into a non-negative wire type")
	}
}

func TestAttachmentValidationRequiresCanonicalMediaType(t *testing.T) {
	for _, mediaType := range []string{
		"attachment",
		"image",
		"Image/JPEG",
		"image/jpeg; charset=utf-8",
		"image /jpeg",
		"image/jpeg ",
	} {
		t.Run(mediaType, func(t *testing.T) {
			_, attachment := decodeFixture[protocol.AttachmentMetadata](t, "attachment-metadata.json")
			attachment.MediaType = mediaType
			if err := attachment.Validate(); err == nil {
				t.Fatalf("Validate accepted non-canonical media type %q", mediaType)
			}
		})
	}
}

func TestAttachmentValidationRequiresLexicalRFC3339Timestamp(t *testing.T) {
	valid := []string{
		"2026-09-20T12:00:00Z",
		"2026-09-20T12:00:00.123+02:30",
		"2026-09-20T12:00:00+23:59",
		"2026-09-20T12:00:00-23:59",
	}
	for _, timestamp := range valid {
		t.Run("accept "+timestamp, func(t *testing.T) {
			_, attachment := decodeFixture[protocol.AttachmentMetadata](t, "attachment-metadata.json")
			attachment.CreatedAt = timestamp
			if err := attachment.Validate(); err != nil {
				t.Fatalf("Validate rejected canonical RFC3339 timestamp %q: %v", timestamp, err)
			}
		})
	}

	for _, timestamp := range []string{
		"2026-09-20T2:00:00Z",
		"2026-09-20T02:00:00,123Z",
		"2026-09-20T02:00:00+2:30",
		"2026-09-20T02:00:00+24:00",
		"2026-09-20T02:00:00-24:00",
		"2026-09-20T02:00:00+02:60",
		"2026-09-20T02:00:00-02:60",
	} {
		t.Run("reject "+timestamp, func(t *testing.T) {
			_, attachment := decodeFixture[protocol.AttachmentMetadata](t, "attachment-metadata.json")
			attachment.CreatedAt = timestamp
			if err := attachment.Validate(); err == nil {
				t.Fatalf("Validate accepted lexically invalid RFC3339 timestamp %q", timestamp)
			}
		})
	}
}

func TestValidatedRequiredCollectionsSerializeAsArrays(t *testing.T) {
	manifest := protocol.AttachmentManifest{Version: "v1"}
	if err := manifest.Validate(); err == nil {
		t.Fatal("Validate accepted manifest with nil attachments")
	}

	manifest.Attachments = []protocol.AttachmentMetadata{}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("Validate empty attachment manifest: %v", err)
	}
	assertSemanticJSONEqual(t, []byte(`{"version":"v1","attachments":[]}`), manifest)

	_, server := decodeFixture[protocol.ServerInfo](t, "server-info.json")
	server.Capabilities = nil
	if err := server.Validate(); err == nil {
		t.Fatal("Validate accepted server information with nil capabilities")
	}

	server.Capabilities = []string{}
	if err := server.Validate(); err != nil {
		t.Fatalf("Validate server information with empty capabilities: %v", err)
	}
	server.Limits = nil
	assertSemanticJSONEqual(t, []byte(`{"instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","recovery_epoch":7,"canonical_origin":"https://sync.example.test","api_versions":["v1"],"capabilities":[],"apple_authorization_mode":"identity_token_only","source_version":"unknown","source_url":"https://github.com/allhappy-labs/baby-tracker","max_items_per_page":200,"max_encoded_bytes":1048576}`), server)
}

func TestOpenAPIRouteSetMatchesRegisteredRoutes(t *testing.T) {
	document := decodeOpenAPI(t)
	got := documentedRoutes(t, document)
	want := make([]string, 0, len(httpapi.Routes()))
	for _, route := range httpapi.Routes() {
		want = append(want, strings.ToUpper(route.Method)+" "+route.Pattern)
	}
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("documented routes = %v, registered routes = %v", got, want)
	}
	for _, required := range []string{"PUT /v1/live-activities/device", "PUT /v1/live-activities/activity", "DELETE /v1/live-activities/device/{installation_id}"} {
		found := false
		for _, route := range got {
			if route == required {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing Live Activity route %s", required)
		}
	}
	for _, route := range got {
		if strings.Contains(route, "attachment") {
			t.Fatalf("attachment endpoint documented before attachment delivery exists: %q", route)
		}
	}
}

func TestOpenAPIAuthErrorMatrixIsStatusSpecific(t *testing.T) {
	document := decodeOpenAPI(t)
	wantResponses := map[string]string{
		"400": "AuthBadRequest", "401": "AuthUnauthorized", "409": "AuthConflict", "429": "AuthRateLimited", "503": "AuthUnavailable",
	}
	for _, path := range []string{"/v1/auth/apple", "/v1/auth/refresh"} {
		operation := asMap(t, document.Paths[path]["post"], path+" post")
		responses := asMap(t, operation["responses"], path+" responses")
		for status, component := range wantResponses {
			response := asMap(t, responses[status], path+" "+status)
			if response["$ref"] != "#/components/responses/"+component {
				t.Fatalf("%s %s response=%v", path, status, response)
			}
		}
	}
	for name, codes := range map[string][]string{
		"AuthBadRequestError":   {"invalid_request", "device_continuity_invalid"},
		"AuthUnauthorizedError": {"invalid_apple_proof", "refresh_reused", "epoch_changed"},
		"AuthConflictError":     {"command_id_reused", "credential_conflict"},
		"AuthRateLimitedError":  {"rate_limited", "retry_after_seconds"},
		"AuthUnavailableError":  {"security_busy", "credential_rotation_busy", "service_unavailable"},
	} {
		encoded, err := json.Marshal(document.Components.Schemas[name])
		if err != nil || len(encoded) == 0 {
			t.Fatalf("schema %s missing: %v", name, err)
		}
		for _, code := range codes {
			if !bytes.Contains(encoded, []byte(code)) {
				t.Errorf("schema %s omitted %s: %s", name, code, encoded)
			}
		}
	}
	rate := document.Components.Schemas["AuthRateLimitedError"]
	encoded, _ := json.Marshal(rate)
	if !bytes.Contains(encoded, []byte(`"retryable":{"const":true}`)) || !bytes.Contains(encoded, []byte(`"required":["retry_after_seconds"]`)) {
		t.Fatalf("429 schema does not freeze retryability/extension: %s", encoded)
	}
	unavailable, err := json.Marshal(document.Components.Responses["AuthUnavailable"])
	if err != nil || !bytes.Contains(unavailable, []byte(`"x-required-for-error-codes":["security_busy","credential_rotation_busy"]`)) ||
		!bytes.Contains(unavailable, []byte(`"const":1`)) {
		t.Fatalf("503 response does not freeze conditional Retry-After: %s err=%v", unavailable, err)
	}
}

func TestOpenAPIFamilyErrorMatrixAndSecretReplayAreExplicit(t *testing.T) {
	document := decodeOpenAPI(t)
	for _, path := range []string{"/v1/family/invitations/{id}/revoke", "/v1/family/invitations/preview", "/v1/family/invitations/redeem"} {
		operation := asMap(t, document.Paths[path]["post"], path+" post")
		responses := asMap(t, operation["responses"], path+" responses")
		gone := asMap(t, responses["410"], path+" 410")
		if gone["$ref"] != "#/components/responses/InvitationGone" {
			t.Fatalf("%s 410 response=%v", path, gone)
		}
	}
	for schema, codes := range map[string][]string{
		"LiveActivityBadRequestError":  {"invalid_request", "unsupported_protocol"},
		"LiveActivityForbiddenError":   {"live_activity_registration_denied", "membership_revoked", "entitlement_required"},
		"LiveActivityConflictError":    {"unsupported_capability", "live_activity_version_conflict"},
		"LiveActivityUnavailableError": {"service_unavailable", "security_busy"},
		"FamilyBadRequestError":        {"invalid_request", "unsupported_capability", "confirmation_required", "invalid_display_name"},
		"FamilyForbiddenError":         {"owner_required", "membership_revoked", "entitlement_required"},
		"FamilyNotFoundError":          {"family_unavailable", "invitation_invalid"},
		"FamilyConflictError":          {"family_slot_occupied", "caregiver_limit", "invitation_limit", "command_id_reused"},
		"InvitationGoneError":          {"invitation_expired", "invitation_consumed", "invitation_revoked"},
	} {
		encoded, err := json.Marshal(document.Components.Schemas[schema])
		if err != nil || len(encoded) == 0 {
			t.Fatalf("schema %s missing: %v", schema, err)
		}
		for _, code := range codes {
			if !bytes.Contains(encoded, []byte(code)) {
				t.Errorf("schema %s omitted %s: %s", schema, code, encoded)
			}
		}
	}
	invitation, err := json.Marshal(document.Components.Schemas["InvitationResponse"])
	if err != nil || !bytes.Contains(invitation, []byte("secret_unavailable")) || !bytes.Contains(invitation, []byte(`"const":true`)) || !bytes.Contains(invitation, []byte(`"const":false`)) {
		t.Fatalf("invitation secret replay contract=%s err=%v", invitation, err)
	}
	capabilities, err := json.Marshal(document.Components.Schemas["FamilyCapabilities"])
	if err != nil || !bytes.Contains(capabilities, []byte("family-lifecycle-v1")) || bytes.Contains(capabilities, []byte("sync-v1")) || bytes.Contains(capabilities, []byte("family_sync")) {
		t.Fatalf("family capabilities=%s err=%v", capabilities, err)
	}
	recoveryCapabilities, err := json.Marshal(document.Components.Schemas["RecoveryCapabilities"])
	if err != nil || !bytes.Contains(recoveryCapabilities, []byte("recovery-v1")) || bytes.Contains(recoveryCapabilities, []byte("family-lifecycle-v1")) {
		t.Fatalf("recovery capabilities=%s err=%v", recoveryCapabilities, err)
	}
	serverInfo, err := json.Marshal(document.Components.Schemas["ServerInfo"])
	if err != nil || !bytes.Contains(serverInfo, []byte(`"$ref":"#/components/schemas/ServerCapabilities"`)) {
		t.Fatalf("ServerInfo does not use the implemented family capability registry: %s err=%v", serverInfo, err)
	}
	for _, path := range []string{"/v1/family/recover", "/v1/family/recovery/forfeit"} {
		operation := asMap(t, document.Paths[path]["post"], path+" post")
		requestBody := asMap(t, operation["requestBody"], path+" request body")
		content := asMap(t, requestBody["content"], path+" request content")
		jsonContent := asMap(t, content["application/json"], path+" request JSON")
		schema := asMap(t, jsonContent["schema"], path+" request schema")
		if schema["$ref"] != "#/components/schemas/RecoveryCommandRequest" {
			t.Fatalf("%s request schema=%v", path, schema)
		}
	}
}

func TestOpenAPIExpressesCurrentAndAttachmentWireContracts(t *testing.T) {
	document := decodeOpenAPI(t)
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("OpenAPI version = %q, want 3.1.0", document.OpenAPI)
	}
	if len(document.Servers) == 0 {
		t.Fatal("OpenAPI declares no HTTPS server")
	}
	for _, server := range document.Servers {
		if !strings.HasPrefix(server.URL, "https://") || len(server.Variables) == 0 {
			t.Fatalf("server = %#v, want HTTPS URL with variables", server)
		}
	}

	attachment := document.Components.Schemas["AttachmentMetadata"]
	if attachment == nil {
		t.Fatal("AttachmentMetadata schema is missing")
	}
	properties := asMap(t, attachment["properties"], "AttachmentMetadata.properties")
	for _, required := range []string{"id", "family_id", "creator_account_id", "media_type", "byte_size", "sha256", "lifecycle", "baby_id", "record_id"} {
		if _, found := properties[required]; !found {
			t.Errorf("AttachmentMetadata omitted %q", required)
		}
	}
	for _, forbidden := range []string{"bytes", "payload", "bucket", "object_key", "storage_key", "upload_url", "download_url"} {
		if _, found := properties[forbidden]; found {
			t.Errorf("AttachmentMetadata exposed forbidden %q property", forbidden)
		}
	}
	if _, found := attachment["oneOf"]; !found {
		t.Fatal("AttachmentMetadata does not model baby_id xor record_id")
	}
	if got := asMap(t, properties["sha256"], "AttachmentMetadata.properties.sha256")["pattern"]; got != "^[0-9a-f]{64}$" {
		t.Fatalf("sha256 pattern = %v, want lowercase SHA-256", got)
	}
	if got := asMap(t, document.Components.Schemas["Cursor"], "Cursor")["properties"]; got == nil {
		t.Fatal("Cursor schema has no properties")
	}
	requestID := asMap(t, document.Components.Schemas["ErrorDetail"], "ErrorDetail")
	requestIDProperties := asMap(t, requestID["properties"], "ErrorDetail.properties")
	requestIDPattern := asMap(t, requestIDProperties["request_id"], "ErrorDetail.properties.request_id")["pattern"].(string)
	if !regexp.MustCompile(requestIDPattern).MatchString("") {
		t.Fatal("ErrorResponse request_id schema rejects the empty request ID in its checked-in fixture")
	}
}

func TestOpenAPIAttachmentMediaTypePatternMatchesWireContract(t *testing.T) {
	document := decodeOpenAPI(t)
	attachment := document.Components.Schemas["AttachmentMetadata"]
	properties := asMap(t, attachment["properties"], "AttachmentMetadata.properties")
	mediaType := asMap(t, properties["media_type"], "AttachmentMetadata.properties.media_type")
	pattern, ok := mediaType["pattern"].(string)
	if !ok {
		t.Fatal("AttachmentMetadata media_type schema has no canonical token pattern")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("compile media_type pattern: %v", err)
	}
	for _, value := range []string{"image/jpeg", "application/vnd.baby-tracker+json"} {
		if !re.MatchString(value) {
			t.Errorf("media_type pattern rejected %q", value)
		}
	}
	for _, value := range []string{"attachment", "Image/JPEG", "image/jpeg; charset=utf-8", "image /jpeg"} {
		if re.MatchString(value) {
			t.Errorf("media_type pattern accepted %q", value)
		}
	}
}

func TestOpenAPICurrentResponsesUseVersionedWireSchemas(t *testing.T) {
	document := decodeOpenAPI(t)
	tests := []struct {
		path       string
		statusCode string
		wantSchema string
	}{
		{path: "/healthz", statusCode: "200", wantSchema: "StatusResponse"},
		{path: "/readyz", statusCode: "200", wantSchema: "StatusResponse"},
		{path: "/readyz", statusCode: "503", wantSchema: "ErrorResponse"},
		{path: "/v1/server", statusCode: "200", wantSchema: "ServerInfo"},
	}
	for _, tt := range tests {
		t.Run(tt.path+" "+tt.statusCode, func(t *testing.T) {
			operation := asMap(t, document.Paths[tt.path]["get"], tt.path+" get")
			responses := asMap(t, operation["responses"], tt.path+" responses")
			response := asMap(t, responses[tt.statusCode], tt.path+" response "+tt.statusCode)
			content := asMap(t, response["content"], tt.path+" response content")
			jsonContent := asMap(t, content["application/json"], tt.path+" JSON response")
			schema := asMap(t, jsonContent["schema"], tt.path+" response schema")
			if schema["$ref"] != "#/components/schemas/"+tt.wantSchema {
				t.Fatalf("response schema = %v, want %s", schema["$ref"], tt.wantSchema)
			}
		})
	}
}

type openAPIDocument struct {
	OpenAPI    string                    `yaml:"openapi"`
	Servers    []openAPIServer           `yaml:"servers"`
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas   map[string]map[string]any `yaml:"schemas"`
		Responses map[string]map[string]any `yaml:"responses"`
	} `yaml:"components"`
}

type openAPIServer struct {
	URL       string         `yaml:"url"`
	Variables map[string]any `yaml:"variables"`
}

func decodeFixture[T any](t *testing.T, name string) ([]byte, T) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join(fixturesDirectory, name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	var value T
	if err := decodeStrictJSON(fixture, &value); err != nil {
		t.Fatalf("strict decode fixture %q: %v", name, err)
	}
	return fixture, value
}

func decodeStrictJSON(input []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func assertSemanticJSONEqual(t *testing.T, fixture []byte, value any) {
	t.Helper()
	var want any
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatalf("decode fixture semantics: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode fixture value: %v", err)
	}
	var got any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decode encoded semantics: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("re-encoded value = %s, want semantic fixture %s", encoded, fixture)
	}
}

func decodeOpenAPI(t *testing.T) openAPIDocument {
	t.Helper()
	contents, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI document: %v", err)
	}
	var document openAPIDocument
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse OpenAPI document: %v", err)
	}
	return document
}

func documentedRoutes(t *testing.T, document openAPIDocument) []string {
	t.Helper()
	validMethods := map[string]struct{}{"get": {}, "put": {}, "post": {}, "delete": {}, "options": {}, "head": {}, "patch": {}, "trace": {}}
	var routes []string
	for path, operations := range document.Paths {
		for method := range operations {
			if _, valid := validMethods[strings.ToLower(method)]; valid {
				routes = append(routes, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(routes)
	return routes
}

func asMap(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", name, value)
	}
	return result
}
