package protocol

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestPushTokenRequestDecode(t *testing.T) {
	hexOf := func(n int, b string) string { return strings.Repeat(b, n) }
	meta := RequestMeta{ProtocolVersion: "v1", Capabilities: []string{CapabilityPushTokenV1}}
	for _, c := range []struct {
		name        string
		token       string
		environment string
		want        []byte // nil: invalid_push_token
	}{
		{"32 bytes, sandbox", hexOf(32, "0a"), "sandbox", bytes.Repeat([]byte{0x0a}, 32)},
		{"32 bytes, production", hexOf(32, "ff"), "production", bytes.Repeat([]byte{0xff}, 32)},
		{"16 bytes", hexOf(16, "01"), "sandbox", bytes.Repeat([]byte{0x01}, 16)},
		{"256 bytes", hexOf(256, "9c"), "production", bytes.Repeat([]byte{0x9c}, 256)},
		{"upper-case hex", hexOf(32, "AB"), "sandbox", nil},
		{"mixed-case hex", hexOf(31, "ab") + "aB", "sandbox", nil},
		{"odd length", hexOf(32, "ab") + "a", "sandbox", nil},
		{"15 bytes", hexOf(15, "ab"), "sandbox", nil},
		{"257 bytes", hexOf(257, "ab"), "sandbox", nil},
		{"non-hex", hexOf(31, "ab") + "zz", "sandbox", nil},
		{"a space inside", hexOf(15, "ab") + " " + hexOf(16, "ab")[1:], "sandbox", nil},
		{"empty token", "", "sandbox", nil},
		{"environment staging", hexOf(32, "ab"), "staging", nil},
		{"environment upper-case", hexOf(32, "ab"), "Sandbox", nil},
		{"empty environment", hexOf(32, "ab"), "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := PushTokenRequest{RequestMeta: meta, Token: c.token, Environment: c.environment}.Decode()
			if c.want == nil {
				if !errors.Is(err, ErrInvalidPushToken) || got != nil {
					t.Fatalf("Decode = %x, %v; want invalid_push_token", got, err)
				}
				if ErrorCodeOf(err) != "invalid_push_token" {
					t.Fatalf("code %q", ErrorCodeOf(err))
				}
				return
			}
			if err != nil || !bytes.Equal(got, c.want) {
				t.Fatalf("Decode = %x, %v; want %x", got, err, c.want)
			}
		})
	}
}

// The push-token route's surface is exactly push-token-v1, a known
// capability; ServerCapabilities lists it (the composition drops it when
// push is off, as it drops export-v1).
func TestPushTokenCapabilityAndSurface(t *testing.T) {
	if CapabilityPushTokenV1 != "push-token-v1" || SurfacePushToken != "push-token" {
		t.Fatalf("names %q %q", CapabilityPushTokenV1, SurfacePushToken)
	}
	if !slices.Contains(KnownCapabilities(), CapabilityPushTokenV1) || !slices.Contains(ServerCapabilities(), CapabilityPushTokenV1) {
		t.Fatal("push-token-v1 is not a known server capability")
	}
	if got := RequiredCapabilities(SurfacePushToken); !slices.Equal(got, []string{CapabilityPushTokenV1}) {
		t.Fatalf("route capabilities %v", got)
	}
	exact := []string{CapabilityPushTokenV1}
	if err := ValidateRouteCapabilities(SurfacePushToken, exact, exact, ServerCapabilities()); err != nil {
		t.Fatal(err)
	}
	for name, header := range map[string][]string{
		"records-v1": {CapabilityRecordsV1},
		"excess":     {CapabilityPushTokenV1, CapabilityRecordsV1},
	} {
		if err := ValidateRouteCapabilities(SurfacePushToken, header, header, ServerCapabilities()); ErrorCodeOf(err) != "unsupported_capability" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	withoutPush := slices.DeleteFunc(ServerCapabilities(), func(c string) bool { return c == CapabilityPushTokenV1 })
	if err := ValidateRouteCapabilities(SurfacePushToken, exact, exact, withoutPush); ErrorCodeOf(err) != "unsupported_capability" {
		t.Fatalf("not advertised: %v", err)
	}
}

// The request decodes strictly: an unknown field is invalid_request.
func TestPushTokenRequestDecodesStrictly(t *testing.T) {
	var input PushTokenRequest
	ok := `{"protocol_version":"v1","capabilities":["push-token-v1"],"token":"` + strings.Repeat("ab", 32) + `","environment":"sandbox"}`
	if err := DecodeRequest(strings.NewReader(ok), 4096, &input); err != nil || input.Environment != "sandbox" {
		t.Fatalf("%+v %v", input, err)
	}
	extra := strings.Replace(ok, `"environment"`, `"device_id":"x","environment"`, 1)
	if err := DecodeRequest(strings.NewReader(extra), 4096, &PushTokenRequest{}); ErrorCodeOf(err) != "invalid_request" {
		t.Fatalf("unknown field: %v", err)
	}
}
