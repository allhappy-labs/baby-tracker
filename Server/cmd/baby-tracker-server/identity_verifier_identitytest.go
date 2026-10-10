//go:build identitytest

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/config"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/testidentity"
	"babytracker/server/internal/testkit"
)

var identityTestState struct {
	sync.RWMutex
	issuer *testidentity.Issuer
}

func newConfiguredIdentityVerifier(cfg config.Config) (identity.IdentityVerifier, error) {
	issuer, err := testidentity.New("protocol-e2e", time.Now)
	if err != nil {
		return nil, err
	}
	verifier, err := issuer.Verifier(cfg.AppleAudience)
	if err != nil {
		return nil, err
	}
	identityTestState.Lock()
	identityTestState.issuer = issuer
	identityTestState.Unlock()
	return verifier, nil
}

func wrapIdentityTestHandler(handler http.Handler, audience string) http.Handler {
	mux := http.NewServeMux()
	faults := &testkit.RecoveryFaults{}
	mux.Handle("/", httpapi.RecoveryFaultHandler(handler, func(point, attempt string) error { return faults.FailOnce(testkit.RecoveryFaultPoint(point), attempt) }))
	mux.HandleFunc("POST /__identitytest/proof", func(response http.ResponseWriter, request *http.Request) {
		var input struct {
			Subject string `json:"subject"`
			Nonce   string `json:"nonce"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 1024))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || input.Subject == "" || len(input.Nonce) != 64 {
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		identityTestState.RLock()
		issuer := identityTestState.issuer
		identityTestState.RUnlock()
		if issuer == nil {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
			return
		}
		token, err := issuer.Sign(audience, input.Subject, input.Nonce)
		if err != nil {
			http.Error(response, "unavailable", http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(map[string]string{"identity_token": token})
	})
	registerIdentityTestAPNS(mux)
	return mux
}

func identityTestHostedAdapterEnabled(cfg config.Config) bool {
	return cfg.Mode == config.ModeHosted && os.Getenv(identityTestHostedAdapterEnv) == "1"
}

func loadServerConfig() (config.Config, error) {
	if os.Getenv("BT_MODE") != string(config.ModeHosted) || os.Getenv(identityTestHostedAdapterEnv) != "1" {
		return config.Load(os.Getenv)
	}
	cfg, err := config.Load(func(name string) string {
		if name == "BT_MODE" {
			return string(config.ModeSelfHosted)
		}
		return os.Getenv(name)
	})
	if err != nil {
		return config.Config{}, err
	}
	cfg.Mode = config.ModeHosted
	return cfg, nil
}

type identityTestApple struct {
	issuer   *testidentity.Issuer
	audience string
}

func (apple *identityTestApple) ExchangeCode(_ context.Context, request identity.AppleCodeExchangeRequest) (identity.AppleTokens, error) {
	if apple == nil || apple.issuer == nil || request.AuthorizationCode == "" {
		return identity.AppleTokens{}, identity.ErrInvalidAppleProof
	}
	proof, err := apple.issuer.Sign(apple.audience, request.ExpectedIdentity.Subject, request.ExpectedNonce)
	if err != nil {
		return identity.AppleTokens{}, err
	}
	return identity.AppleTokens{IdentityToken: proof, RefreshToken: "identitytest-refresh-" + request.AuthorizationCode}, nil
}

// configureIdentityTestHostedAuth installs the identitytest Apple code
// exchanger on hosted auth (BT_IDENTITYTEST_HOSTED_ADAPTER=1). The returned
// Apple refresh token is not persisted (hosted-mode ruling, Phase 1).
func configureIdentityTestHostedAuth(cfg config.Config, options *auth.Options) (bool, error) {
	if !identityTestHostedAdapterEnabled(cfg) {
		return false, nil
	}
	identityTestState.RLock()
	issuer := identityTestState.issuer
	identityTestState.RUnlock()
	if issuer == nil || options == nil {
		return false, errHostedAdapterUnavailable
	}
	options.Apple = &identityTestApple{issuer: issuer, audience: cfg.AppleAudience}
	return true, nil
}
