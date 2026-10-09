package protocol

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRecoveryRequestStrictContract(t *testing.T) {
	valid := `{"protocol_version":"v1","capabilities":["identity-v1","refresh-recovery-v1"],"refresh_token":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","refresh_attempt_id":"11111111-1111-4111-8111-111111111111","device_continuity":{"device_id":"22222222-2222-4222-8222-222222222222","credential":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"}}`
	var input RecoverableRefreshRequest
	if err := DecodeRequest(strings.NewReader(valid), 32768, &input); err != nil || ValidateRefreshRecoveryRequest(input) != nil {
		t.Fatalf("valid request: %+v, %v", input, err)
	}
	for _, body := range []string{
		strings.Replace(valid, `"v1"`, `"v2"`, 1),
		strings.Replace(valid, `"refresh-recovery-v1"`, `"identity-v1"`, 1),
		strings.Replace(valid, `,"refresh-recovery-v1"`, ``, 1),
		strings.Replace(valid, `"11111111-1111-4111-8111-111111111111"`, `"11111111-1111-4111-8111-11111111111A"`, 1),
		strings.Replace(valid, `"11111111-1111-4111-8111-111111111111"`, `"00000000-0000-0000-0000-000000000000"`, 1),
		strings.Replace(valid, `"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`, `"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","extra":true`, 1),
		strings.Replace(valid, `"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`, `"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","credential":"again"`, 1),
		strings.Replace(valid, `"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`, `"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","other":{}`, 1),
	} {
		var request RecoverableRefreshRequest
		if err := DecodeRequest(strings.NewReader(body), 32768, &request); err == nil {
			err = ValidateRefreshRecoveryRequest(request)
			if err == nil {
				t.Fatalf("accepted %s", body)
			}
		}
	}
}

func TestRecoveryFixturesMatchWireTypes(t *testing.T) {
	data, err := os.ReadFile("../../api/fixtures/auth-refresh-recovery.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Request    json.RawMessage `json:"request"`
		Response   json.RawMessage `json:"response"`
		AckRequest json.RawMessage `json:"ack_request"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var request RecoverableRefreshRequest
	if err := DecodeRequest(strings.NewReader(string(fixture.Request)), 32768, &request); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRefreshRecoveryRequest(request); err != nil {
		t.Fatal(err)
	}
	var ack RefreshAckRequest
	if err := DecodeRequest(strings.NewReader(string(fixture.AckRequest)), 32768, &ack); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRefreshAckRequest(ack); err != nil {
		t.Fatal(err)
	}
	var response RecoverableRefreshResponse
	if err := json.Unmarshal(fixture.Response, &response); err != nil {
		t.Fatal(err)
	}
	if response.RefreshAttemptID != request.RefreshAttemptID || response.ServerInstanceID == "" || response.DeviceID != request.DeviceContinuity.DeviceID {
		t.Fatalf("response=%+v", response)
	}
	data, err = os.ReadFile("../../api/fixtures/auth-refresh-recovery-errors.json")
	if err != nil {
		t.Fatal(err)
	}
	var errors map[string]ErrorResponse
	if err := json.Unmarshal(data, &errors); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"refresh_attempt_conflict", "refresh_receipt_unavailable"} {
		if value, ok := errors[code]; !ok || string(value.Error.Code) != code || value.Error.Retryable {
			t.Fatalf("error fixture %s: %+v", code, value)
		}
	}
}
