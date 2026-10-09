package protocol

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCapabilityConvention(t *testing.T) {
	reserved := ReservedCapabilities()
	if err := ValidateCapabilities(reserved, reserved); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"identity_v1", "Identity-v1", "identity--v1", "future-required-v1"} {
		if err := ValidateCapabilities([]string{bad}, reserved); ErrorCodeOf(err) != "unsupported_capability" {
			t.Fatalf("capability=%q code=%q", bad, ErrorCodeOf(err))
		}
	}
}

func TestAuthFixturesUseSharedCapabilitiesAndValidErrors(t *testing.T) {
	data, err := os.ReadFile("../../api/fixtures/auth-challenge.json")
	if err != nil {
		t.Fatal(err)
	}
	var challenge struct {
		Request              ChallengeRequest `json:"request"`
		ReservedCapabilities []string         `json:"reserved_capabilities"`
	}
	if err := json.Unmarshal(data, &challenge); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(challenge.ReservedCapabilities, ReservedCapabilities()) ||
		ValidateCapabilities(challenge.Request.Capabilities, IdentityCapabilities()) != nil {
		t.Fatalf("challenge fixture capabilities=%v reserved=%v", challenge.Request.Capabilities, challenge.ReservedCapabilities)
	}
	data, err = os.ReadFile("../../api/fixtures/auth-errors.json")
	if err != nil {
		t.Fatal(err)
	}
	var errorFixtures map[string]ErrorResponse
	if err := json.Unmarshal(data, &errorFixtures); err != nil {
		t.Fatal(err)
	}
	for name, response := range errorFixtures {
		if string(response.Error.Code) != name || response.Validate() != nil {
			t.Errorf("error fixture %q=%+v", name, response)
		}
	}
}

func TestErrorResponseRejectsNonpositiveRetryAfter(t *testing.T) {
	zero := 0
	response := ErrorResponse{Error: ErrorDetail{Code: "rate_limited", Message: "Rate limit exceeded", RetryAfterSeconds: &zero}}
	if response.Validate() == nil {
		t.Fatal("zero retry_after_seconds accepted")
	}
}

func TestDecodeRequestIsStrictAndBounded(t *testing.T) {
	valid := `{"protocol_version":"v1","capabilities":[]}`
	var request ChallengeRequest
	if err := DecodeRequest(strings.NewReader(valid), int64(len(valid)), &request); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"missing capabilities": `{"protocol_version":"v1"}`,
		"null capabilities":    `{"protocol_version":"v1","capabilities":null}`,
		"unknown":              `{"protocol_version":"v1","capabilities":[],"future":true}`,
		"duplicate":            `{"protocol_version":"v1","\u0063apabilities":[],"capabilities":[]}`,
		"case alias":           `{"Protocol_Version":"v1","capabilities":[]}`,
		"case alias duplicate": `{"protocol_version":"v1","PROTOCOL_VERSION":"v1","capabilities":[]}`,
		"trailing":             valid + `{}`,
		"array":                `[]`,
		"bad version":          `{"protocol_version":"v2","capabilities":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var got ChallengeRequest
			if err := DecodeRequest(strings.NewReader(body), int64(len(body)), &got); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	invalidUTF8 := append([]byte(`{"protocol_version":"v1","capabilities":[],"x":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	if err := DecodeRequest(strings.NewReader(string(invalidUTF8)), 1<<10, &request); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	if err := DecodeRequest(strings.NewReader(valid+" "), int64(len(valid)), &request); ErrorCodeOf(err) != "invalid_request" {
		t.Fatalf("max-plus-one code=%q err=%v", ErrorCodeOf(err), err)
	}
	var unregistered struct {
		ProtocolVersion string   `json:"protocol_version"`
		Capabilities    []string `json:"capabilities"`
	}
	if err := DecodeRequest(strings.NewReader(valid), int64(len(valid)), &unregistered); ErrorCodeOf(err) != "invalid_request" {
		t.Fatalf("unregistered request type code=%q err=%v", ErrorCodeOf(err), err)
	}
}

func TestDecodeRequestClassifiesNegotiationFailures(t *testing.T) {
	for name, test := range map[string]struct {
		body string
		code string
	}{
		"missing version":      {`{"capabilities":[]}`, "unsupported_protocol"},
		"unsupported version":  {`{"protocol_version":"v2","capabilities":[]}`, "unsupported_protocol"},
		"missing capabilities": {`{"protocol_version":"v1"}`, "unsupported_capability"},
		"null capabilities":    {`{"protocol_version":"v1","capabilities":null}`, "unsupported_capability"},
	} {
		t.Run(name, func(t *testing.T) {
			var request ChallengeRequest
			err := DecodeRequest(strings.NewReader(test.body), int64(len(test.body)), &request)
			if got := ErrorCodeOf(err); got != test.code {
				t.Fatalf("code=%q want=%q error=%v", got, test.code, err)
			}
		})
	}
}

func TestResponseAdditionDoesNotBreakDecoder(t *testing.T) {
	var got ChallengeResponse
	err := json.Unmarshal([]byte(`{"protocol_version":"v1","challenge":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8","server_instance_id":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d","canonical_origin":"https://sync.example.test","expires_at":"2026-09-20T12:05:00Z","future_field":true}`), &got)
	if err != nil || got.Challenge == "" {
		t.Fatal("additive response rejected")
	}
}

func TestAppleExchangeDTOCarriesContinuityWithoutAuthenticatingIt(t *testing.T) {
	body := `{"protocol_version":"v1","capabilities":["identity-v1"],"command_id":"30000000-0000-4000-8000-000000000001","challenge":"challenge","identity_token":"token","authorization_code":"","installation_id":"install","profile":{"display_name":"Parent","confirmed":true},"device_continuity":{"device_id":"40000000-0000-4000-8000-000000000001","credential":"opaque"}}`
	var request AppleExchangeRequest
	if err := DecodeRequest(strings.NewReader(body), 32<<10, &request); err != nil {
		t.Fatal(err)
	}
	if request.DeviceContinuity == nil || request.Profile == nil || request.DeviceContinuity.Credential != "opaque" {
		t.Fatalf("decoded request=%+v", request)
	}
}

func TestAuthSessionAndContinuityFixturesMatchWireDTOs(t *testing.T) {
	data, err := os.ReadFile("../../api/fixtures/auth-session.json")
	if err != nil {
		t.Fatal(err)
	}
	var sessions struct {
		Initial SessionResponse `json:"initial_response"`
		Refresh SessionResponse `json:"refresh_response"`
	}
	if err := json.Unmarshal(data, &sessions); err != nil {
		t.Fatal(err)
	}
	if sessions.Initial.ProtocolVersion != "v1" || sessions.Initial.RecoveryEpoch != 7 || sessions.Initial.DeviceCredential == "" ||
		sessions.Refresh.RecoveryEpoch != sessions.Initial.RecoveryEpoch || sessions.Refresh.DeviceID != sessions.Initial.DeviceID || sessions.Refresh.DeviceCredential != "" ||
		sessions.Initial.Account.DisplayName != "Parent" {
		t.Fatalf("session fixtures=%+v", sessions)
	}

	data, err = os.ReadFile("../../api/fixtures/auth-reauth-continuity.json")
	if err != nil {
		t.Fatal(err)
	}
	var continuity struct {
		Request  AppleExchangeRequest `json:"request"`
		Response SessionResponse      `json:"response"`
	}
	if err := json.Unmarshal(data, &continuity); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(continuity.Request)
	if err != nil {
		t.Fatal(err)
	}
	var strict AppleExchangeRequest
	if err := DecodeRequest(strings.NewReader(string(encoded)), 32<<10, &strict); err != nil {
		t.Fatal(err)
	}
	if strict.DeviceContinuity == nil || len(strict.DeviceContinuity.Credential) != 43 || continuity.Response.DeviceCredential != "" ||
		continuity.Response.DeviceID != strict.DeviceContinuity.DeviceID || continuity.Response.RecoveryEpoch != sessions.Initial.RecoveryEpoch {
		t.Fatalf("continuity fixture=%+v", continuity)
	}

	data, err = os.ReadFile("../../api/fixtures/auth-device-continuity-invalid.json")
	if err != nil {
		t.Fatal(err)
	}
	var invalidContinuity map[string]struct {
		Request  AppleExchangeRequest `json:"request"`
		Response ErrorResponse        `json:"response"`
	}
	if err := json.Unmarshal(data, &invalidContinuity); err != nil || len(invalidContinuity) != 2 {
		t.Fatalf("invalid continuity fixture=%v err=%v", invalidContinuity, err)
	}
	for name, fixture := range invalidContinuity {
		encoded, marshalErr := json.Marshal(fixture.Request)
		var request AppleExchangeRequest
		decodeErr := DecodeRequest(strings.NewReader(string(encoded)), 32<<10, &request)
		if marshalErr != nil || decodeErr != nil || fixture.Response.Validate() != nil || fixture.Response.Error.Code != "device_continuity_invalid" {
			t.Fatalf("continuity case %s=%+v marshal=%v decode=%v", name, fixture, marshalErr, decodeErr)
		}
	}
	if !reflect.DeepEqual(invalidContinuity["missing_proof"].Response, invalidContinuity["cross_account"].Response) {
		t.Fatal("continuity failures disclose which binding check failed")
	}

	data, err = os.ReadFile("../../api/fixtures/auth-refresh-reused.json")
	if err != nil {
		t.Fatal(err)
	}
	var reused ErrorResponse
	if err := json.Unmarshal(data, &reused); err != nil || reused.Validate() != nil || reused.Error.Code != "refresh_reused" {
		t.Fatalf("refresh reuse fixture=%+v err=%v", reused, err)
	}

	data, err = os.ReadFile("../../api/fixtures/apple-notification.json")
	if err != nil {
		t.Fatal(err)
	}
	var notification map[string]json.RawMessage
	if err := json.Unmarshal(data, &notification); err != nil || len(notification) != 1 || notification["payload"] == nil {
		t.Fatalf("notification fixture keys=%v err=%v", notification, err)
	}
}

func TestServerInfoRequiresAppleAuthorizationModeAndImplementedCapability(t *testing.T) {
	server := ServerInfo{InstanceID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RecoveryEpoch: 1,
		CanonicalOrigin: "https://sync.example.test", APIVersions: []string{"v1"}, Capabilities: IdentityCapabilities(),
		AppleAuthorizationMode: "identity_token_only"}
	if err := server.Validate(); err != nil {
		t.Fatal(err)
	}
	server.AppleAuthorizationMode = ""
	if err := server.Validate(); err == nil {
		t.Fatal("missing Apple authorization mode accepted")
	}
	server.AppleAuthorizationMode = "identity_token_only"
	server.Capabilities = ReservedCapabilities()
	if err := server.Validate(); err == nil {
		t.Fatal("unimplemented capabilities advertised")
	}
}
