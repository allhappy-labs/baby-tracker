//go:build identitytest

package liveactivity

import (
	"errors"
	"net/http"

	"babytracker/server/internal/config"
)

// Hosts that cannot resolve: an identitytest recorder answers before any dial.
const (
	IdentityTestAPNsSandboxURL    = "https://sandbox.apns.identitytest.invalid"
	IdentityTestAPNsProductionURL = "https://production.apns.identitytest.invalid"
)

// NewIdentityTestAPNSTransport is the production APNs transport — provider
// token, headers, payload encoding, response classification — with Apple's
// hosts replaced by an in-process round tripper. It exists only in identitytest
// builds (the disposable protocol harness); production builds cannot reach it.
func NewIdentityTestAPNSTransport(configuration config.LiveActivityConfig, recorder http.RoundTripper) (*APNSTransport, error) {
	if !configuration.Enabled || recorder == nil {
		return nil, errors.New("identitytest APNs transport requires an enabled configuration and a recorder")
	}
	key, err := config.ReadAPNSPrivateKey(configuration.KeyFile)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: apnsRequestTimeout, Transport: recorder, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return newAPNSTransport(configuration.TeamID, configuration.KeyID, configuration.BundleID, key, client, IdentityTestAPNsSandboxURL, IdentityTestAPNsProductionURL), nil
}
