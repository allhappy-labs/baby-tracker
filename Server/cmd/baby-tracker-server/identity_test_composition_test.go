//go:build identitytest

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babytracker/server/internal/config"
	"babytracker/server/internal/family"
	"babytracker/server/internal/identity"
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
	if deliveryQueueHosted(selfHosted.Mode) || composeSecurityFamilySettings(selfHosted).requireEntitlement {
		t.Fatal("identitytest changed self-hosted delivery or entitlement semantics")
	}
	if _, ok := familyServiceOptions(selfHosted, nil, nil).Entitlements.(family.SelfHostedEntitlementPolicy); !ok {
		t.Fatalf("self-host entitlement policy=%T", familyServiceOptions(selfHosted, nil, nil).Entitlements)
	}

	t.Setenv(identityTestHostedAdapterEnv, "")
	hosted := config.Config{Mode: config.ModeHosted, LedgerHeadDir: t.TempDir()}
	if identityTestHostedAdapterEnabled(hosted) {
		t.Fatal("identitytest adapter enabled without explicit flag")
	}
	if !deliveryQueueHosted(hosted.Mode) || !composeSecurityFamilySettings(hosted).requireEntitlement {
		t.Fatal("hosted delivery or entitlement semantics were disabled")
	}
	if _, ok := familyServiceOptions(hosted, nil, nil).Entitlements.(family.HostedEntitlementPolicy); !ok {
		t.Fatalf("hosted entitlement policy=%T", familyServiceOptions(hosted, nil, nil).Entitlements)
	}

	t.Setenv(identityTestHostedAdapterEnv, "1")
	if !identityTestHostedAdapterEnabled(hosted) {
		t.Fatal("explicit hosted identitytest adapter was not enabled")
	}
	if _, err := newConfiguredIdentityVerifier(config.Config{AppleAudience: "com.example.protocol"}); err != nil {
		t.Fatal(err)
	}
	options := sessionServiceOptions(hosted, nil, nil, nil, nil)
	enabled, err := configureIdentityTestHostedAuth(hosted, &options)
	if err != nil || !enabled || options.Apple == nil || options.Credentials == nil {
		t.Fatalf("hosted test adapter enabled=%t apple=%T credentials=%T err=%v", enabled, options.Apple, options.Credentials, err)
	}
}

func TestIdentityTestHostedCredentialStatePersistsAcrossAdapterRestart(t *testing.T) {
	t.Setenv(identityTestHostedAdapterEnv, "1")
	cfg := config.Config{Mode: config.ModeHosted, AppleAudience: "com.example.protocol", LedgerHeadDir: t.TempDir()}
	cfg.Auth.DeviceContinuityKeys.Current.Secret[0] = 1
	if _, err := newConfiguredIdentityVerifier(cfg); err != nil {
		t.Fatal(err)
	}
	first := sessionServiceOptions(cfg, nil, nil, nil, nil)
	if enabled, err := configureIdentityTestHostedAuth(cfg, &first); err != nil || !enabled {
		t.Fatalf("first adapter enabled=%t err=%v", enabled, err)
	}
	binding, err := identity.NewAppleCredentialBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	promotion, err := first.Credentials.RotateForSession(t.Context(), binding,
		"70000000-0000-4000-8000-000000000001", "70000000-0000-4000-8000-000000000101", func(context.Context) (identity.AppleTokens, error) {
			return identity.AppleTokens{IdentityToken: "signed", RefreshToken: "durable-refresh"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("accepted-command"))
	promotion, err = first.Credentials.PreparePromotionCAS(t.Context(), binding, promotion, hash)
	if err != nil {
		t.Fatal(err)
	}
	adoption, err := first.Credentials.ConfirmPromotion(t.Context(), promotion)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Credentials.FinishAdoption(t.Context(), binding, adoption); err != nil {
		t.Fatal(err)
	}

	second := sessionServiceOptions(cfg, nil, nil, nil, nil)
	if enabled, err := configureIdentityTestHostedAuth(cfg, &second); err != nil || !enabled {
		t.Fatalf("restarted adapter enabled=%t err=%v", enabled, err)
	}
	resume, err := second.Credentials.Resume(t.Context(), binding)
	if err != nil || resume.State.Phase != identity.CredentialPhaseReady || !resume.State.HasCurrent || resume.State.HasPending {
		t.Fatalf("restarted credential state=%+v err=%v", resume.State, err)
	}
}
