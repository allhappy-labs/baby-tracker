package protocol

import "encoding/hex"

// Push-token bounds: an APNs device token is opaque bytes, 32 today; the
// route accepts 16 through 256 decoded bytes (the APNs client's own bounds
// and device_push_tokens' CHECK), sent as lowercase hex.
const (
	MinPushTokenBytes = 16
	MaxPushTokenBytes = 256
	// PushTokenBodyBytes bounds a PUT /v1/devices/push-token body.
	PushTokenBodyBytes = 4096
)

// ErrInvalidPushToken is a push token or environment the route refuses
// (400 invalid_push_token).
var ErrInvalidPushToken error = protocolError("invalid_push_token")

// PushTokenRequest is the body of PUT /v1/devices/push-token. It names no
// device: the token is always the authenticated session's own device's.
type PushTokenRequest struct {
	RequestMeta
	Token       string `json:"token"`       // lowercase hex, even length, 16–256 decoded bytes
	Environment string `json:"environment"` // "sandbox" | "production"
}

// Decode validates r and returns the raw token: ErrInvalidPushToken for the
// token or the environment.
func (r PushTokenRequest) Decode() ([]byte, error) {
	if r.Environment != "sandbox" && r.Environment != "production" {
		return nil, ErrInvalidPushToken
	}
	n := len(r.Token)
	if n%2 != 0 || n < 2*MinPushTokenBytes || n > 2*MaxPushTokenBytes {
		return nil, ErrInvalidPushToken
	}
	for i := 0; i < n; i++ {
		if c := r.Token[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, ErrInvalidPushToken
		}
	}
	token, err := hex.DecodeString(r.Token)
	if err != nil {
		return nil, ErrInvalidPushToken
	}
	return token, nil
}
