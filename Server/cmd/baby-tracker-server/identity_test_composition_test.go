//go:build identitytest

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIdentityTestCompositionSignsOnlyThroughCompileTimeHandler(t *testing.T) {
	verifier, err := newConfiguredIdentityVerifier(config.Config{AppleAudience: "com.example.protocol"})
	if err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("a", 64)
	request := httptest.NewRequest(http.MethodPost, "/__identitytest/proof", bytes.NewBufferString(`{"subject":"owner","nonce":"`+nonce+`"}`))
	response := httptest.NewRecorder()
	wrapIdentityTestHandler(http.NotFoundHandler(), "com.example.protocol").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		IdentityToken string `json:"identity_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.IdentityToken == "" {
		t.Fatalf("body=%s err=%v", response.Body.String(), err)
	}
	if _, err := verifier.Verify(t.Context(), body.IdentityToken, nonce); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityTestHostedAdapterRequiresHostedModeAndExplicitFlag(t *testing.T) {
	t.Setenv(identityTestHostedAdapterEnv, "1")
	selfHosted := config.Config{Mode: config.ModeSelfHosted}
	if identityTestHostedAdapterEnabled(selfHosted) {
		t.Fatal("identitytest adapter enabled in self-hosted mode")
	}
	if requireFamilySyncEntitlement(selfHosted.Mode) || authorityOptions(selfHosted, &pgxpool.Pool{}).RequireFamilySyncEntitlement {
		t.Fatal("identitytest changed self-hosted entitlement semantics")
	}

	t.Setenv(identityTestHostedAdapterEnv, "")
	hosted := config.Config{Mode: config.ModeHosted, AppleAudience: "com.example.protocol"}
	if identityTestHostedAdapterEnabled(hosted) {
		t.Fatal("identitytest adapter enabled without explicit flag")
	}
	if !requireFamilySyncEntitlement(hosted.Mode) || !authorityOptions(hosted, &pgxpool.Pool{}).RequireFamilySyncEntitlement {
		t.Fatal("hosted entitlement semantics were disabled")
	}

	t.Setenv(identityTestHostedAdapterEnv, "1")
	if !identityTestHostedAdapterEnabled(hosted) {
		t.Fatal("explicit hosted identitytest adapter was not enabled")
	}
	if _, err := newConfiguredIdentityVerifier(config.Config{AppleAudience: "com.example.protocol"}); err != nil {
		t.Fatal(err)
	}
	options := sessionServiceOptions(hosted, nil, nil, nil)
	enabled, err := configureIdentityTestHostedAuth(hosted, &options)
	if err != nil || !enabled || options.Apple == nil {
		t.Fatalf("hosted test adapter enabled=%t apple=%T err=%v", enabled, options.Apple, err)
	}
	// The exchanger proves the expected identity with the expected nonce;
	// its Apple refresh token is never persisted (Phase 1 hosted ruling).
	tokens, err := options.Apple.ExchangeCode(t.Context(), identity.AppleCodeExchangeRequest{AuthorizationCode: "code",
		ExpectedIdentity: identity.VerifiedIdentity{Subject: "owner"}, ExpectedNonce: strings.Repeat("b", 64)})
	if err != nil || tokens.IdentityToken == "" {
		t.Fatalf("identitytest exchange: %+v %v", tokens, err)
	}
	if _, err := options.Apple.ExchangeCode(t.Context(), identity.AppleCodeExchangeRequest{}); err == nil {
		t.Fatal("identitytest exchanger accepted an empty authorization code")
	}
}
