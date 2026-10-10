package identity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testHostedAppleClientID = "com.example.babytracker"
	testHostedAppleTeamID   = "TEAMID1234"
	testHostedAppleKeyID    = "ABCDEFGHIJ"
)

type hostedAppleFixture struct {
	t         *testing.T
	now       time.Time
	key       *ecdsa.PrivateKey
	status    int
	body      []byte
	requests  atomic.Int32
	requestFn func(*http.Request)
	client    *http.Client
}

func newHostedAppleFixture(t *testing.T) *hostedAppleFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &hostedAppleFixture{
		t:      t,
		now:    time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		key:    key,
		status: http.StatusOK,
		body:   []byte(`{"id_token":"identity-token","refresh_token":"refresh-token","access_token":"access-token","token_type":"Bearer","expires_in":3600}`),
	}
	fixture.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		fixture.requests.Add(1)
		if fixture.requestFn != nil {
			fixture.requestFn(request)
		}
		return &http.Response{
			StatusCode: fixture.status,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(fixture.body)),
			Request:    request,
		}, nil
	})}
	return fixture
}

func (fixture *hostedAppleFixture) options() HostedAppleOptions {
	fixture.t.Helper()
	return HostedAppleOptions{
		ClientID:   testHostedAppleClientID,
		TeamID:     testHostedAppleTeamID,
		KeyID:      testHostedAppleKeyID,
		SigningKey: fixture.key,
		HTTP:       fixture.client,
		Now:        func() time.Time { return fixture.now },
	}
}

func (fixture *hostedAppleFixture) apple() *HostedApple {
	fixture.t.Helper()
	authority, err := NewHostedApple(fixture.options())
	if err != nil {
		fixture.t.Fatal(err)
	}
	apple, ok := authority.(*HostedApple)
	if !ok {
		fixture.t.Fatalf("authority type=%T", authority)
	}
	return apple
}

func validAppleExchangeRequest() AppleCodeExchangeRequest {
	return AppleCodeExchangeRequest{
		AuthorizationCode: "code+/=& value",
		ExpectedNonce:     strings.Repeat("a", 64),
		ExpectedIdentity: VerifiedIdentity{
			Provider: "apple",
			Subject:  "apple-subject",
			Audience: testHostedAppleClientID,
		},
	}
}

func readForm(t *testing.T, request *http.Request) url.Values {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func TestHostedAppleClientSecretUsesExactES256JOSEEncoding(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	apple := fixture.apple()

	secret, err := apple.clientSecret()
	if err != nil {
		t.Fatal(err)
	}
	segments := strings.Split(secret, ".")
	if len(segments) != 3 {
		t.Fatalf("segments=%d", len(segments))
	}
	const wantHeader = "eyJhbGciOiJFUzI1NiIsImtpZCI6IkFCQ0RFRkdISUoifQ"
	const wantClaims = "eyJpc3MiOiJURUFNSUQxMjM0IiwiaWF0IjoxNzg5OTA1NjAwLCJleHAiOjE3ODk5MDU5MDAsImF1ZCI6Imh0dHBzOi8vYXBwbGVpZC5hcHBsZS5jb20iLCJzdWIiOiJjb20uZXhhbXBsZS5iYWJ5dHJhY2tlciJ9"
	if segments[0] != wantHeader || segments[1] != wantClaims {
		t.Fatalf("signing input=%q.%q", segments[0], segments[1])
	}
	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil || len(signature) != 64 {
		t.Fatalf("signature bytes=%d err=%v", len(signature), err)
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	if !ecdsa.Verify(&fixture.key.PublicKey, digest[:], newBigInt(signature[:32]), newBigInt(signature[32:])) {
		t.Fatal("raw JOSE signature did not verify")
	}
}

func TestNewHostedAppleRejectsUnsafeConfigurationAndClonesKey(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	wrongCurve, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	inconsistentKey := *fixture.key
	inconsistentKey.PublicKey = ecdsa.PublicKey{Curve: elliptic.P256(), X: big.NewInt(1), Y: big.NewInt(1)}
	tests := []struct {
		name   string
		mutate func(*HostedAppleOptions)
	}{
		{name: "empty client ID", mutate: func(options *HostedAppleOptions) { options.ClientID = "" }},
		{name: "unsafe client ID", mutate: func(options *HostedAppleOptions) { options.ClientID = "client id" }},
		{name: "leading client separator", mutate: func(options *HostedAppleOptions) { options.ClientID = ".example.app" }},
		{name: "trailing client separator", mutate: func(options *HostedAppleOptions) { options.ClientID = "com.example." }},
		{name: "empty client segment", mutate: func(options *HostedAppleOptions) { options.ClientID = "com..app" }},
		{name: "hyphenated client edge", mutate: func(options *HostedAppleOptions) { options.ClientID = "com.-example.app" }},
		{name: "empty team ID", mutate: func(options *HostedAppleOptions) { options.TeamID = "" }},
		{name: "unsafe team ID", mutate: func(options *HostedAppleOptions) { options.TeamID = "TEAM-ID" }},
		{name: "empty key ID", mutate: func(options *HostedAppleOptions) { options.KeyID = "" }},
		{name: "unsafe key ID", mutate: func(options *HostedAppleOptions) { options.KeyID = "KEY/ID" }},
		{name: "nil signing key", mutate: func(options *HostedAppleOptions) { options.SigningKey = nil }},
		{name: "wrong curve", mutate: func(options *HostedAppleOptions) { options.SigningKey = wrongCurve }},
		{name: "inconsistent public key", mutate: func(options *HostedAppleOptions) { options.SigningKey = &inconsistentKey }},
		{name: "http redirect URI", mutate: func(options *HostedAppleOptions) { options.RedirectURI = "http://example.test/callback" }},
		{name: "relative redirect URI", mutate: func(options *HostedAppleOptions) { options.RedirectURI = "/callback" }},
		{name: "redirect URI user info", mutate: func(options *HostedAppleOptions) { options.RedirectURI = "https://user@example.test/callback" }},
		{name: "redirect URI fragment", mutate: func(options *HostedAppleOptions) { options.RedirectURI = "https://example.test/callback#fragment" }},
		{name: "non ASCII redirect URI", mutate: func(options *HostedAppleOptions) { options.RedirectURI = "https://exämple.test/callback" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := fixture.options()
			test.mutate(&options)
			if _, err := NewHostedApple(options); Code(err) != "invalid_request" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
		})
	}

	options := fixture.options()
	options.RedirectURI = "https://example.test/apple/callback?channel=ios"
	authority, err := NewHostedApple(options)
	if err != nil {
		t.Fatal(err)
	}
	apple := authority.(*HostedApple)
	originalD := newBigInt(fixture.key.D.Bytes())
	fixture.key.D.SetInt64(1)
	secret, err := apple.clientSecret()
	if err != nil {
		t.Fatal(err)
	}
	segments := strings.Split(secret, ".")
	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	x, y := elliptic.P256().ScalarBaseMult(originalD.Bytes())
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, digest[:], newBigInt(signature[:32]), newBigInt(signature[32:])) {
		t.Fatal("constructed authority retained mutable caller key")
	}
}

func TestHostedAppleExchangeUsesFixedEndpointAndExactForm(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	fixture.requestFn = func(request *http.Request) {
		if request.Method != http.MethodPost || request.URL.String() != appleTokenURL {
			t.Fatalf("request=%s %s", request.Method, request.URL)
		}
		if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || request.Header.Get("Accept") != "application/json" {
			t.Fatalf("headers=%v", request.Header)
		}
		form := readForm(t, request)
		if len(form) != 4 || form.Get("client_id") != testHostedAppleClientID || form.Get("code") != "code+/=& value" ||
			form.Get("grant_type") != "authorization_code" || form.Get("client_secret") == "" || form.Has("redirect_uri") {
			t.Fatalf("form=%v", form)
		}
	}
	tokens, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if tokens != (AppleTokens{IdentityToken: "identity-token", RefreshToken: "refresh-token"}) {
		t.Fatalf("tokens=%+v", tokens)
	}

	options := fixture.options()
	options.RedirectURI = "https://example.test/apple/callback?channel=ios"
	authority, err := NewHostedApple(options)
	if err != nil {
		t.Fatal(err)
	}
	fixture.requestFn = func(request *http.Request) {
		form := readForm(t, request)
		if len(form) != 5 || form.Get("redirect_uri") != options.RedirectURI {
			t.Fatalf("form=%v", form)
		}
	}
	if _, err := authority.ExchangeCode(t.Context(), validAppleExchangeRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestHostedAppleDeniesRedirectWithoutMutatingInjectedClient(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	var originalRedirectCalled atomic.Bool
	var targetCalled atomic.Bool
	fixture.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		originalRedirectCalled.Store(true)
		return nil
	}
	fixture.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		fixture.requests.Add(1)
		if request.URL.Host == "attacker.test" {
			targetCalled.Store(true)
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://attacker.test/stolen"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    request,
		}, nil
	})
	_, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest())
	if Code(err) != "apple_unavailable" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
	if originalRedirectCalled.Load() || targetCalled.Load() || fixture.requests.Load() != 1 {
		t.Fatalf("original=%v target=%v requests=%d", originalRedirectCalled.Load(), targetCalled.Load(), fixture.requests.Load())
	}
}

func TestHostedAppleImposesFiveSecondWholeCallDeadline(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	fixture.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining > 5*time.Second || remaining < 4*time.Second {
			t.Fatalf("deadline=%v remaining=%v ok=%v", deadline, remaining, ok)
		}
		return nil, context.DeadlineExceeded
	})
	_, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest())
	if Code(err) != "apple_unavailable" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}

	shortContext, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	fixture.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining > time.Second || remaining < 500*time.Millisecond {
			t.Fatalf("deadline=%v remaining=%v ok=%v", deadline, remaining, ok)
		}
		return nil, context.DeadlineExceeded
	})
	// Build after replacing the injected transport: construction clones the
	// client value, not the mutable client pointer.
	_, err = fixture.apple().ExchangeCode(shortContext, validAppleExchangeRequest())
	if Code(err) != "apple_unavailable" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
}

func TestHostedAppleStrictTokenResponseValidation(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	longToken := strings.Repeat("x", 16<<10+1)
	tests := []struct {
		name string
		body []byte
	}{
		{name: "non object", body: []byte(`[]`)},
		{name: "invalid UTF-8", body: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}},
		{name: "trailing JSON", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":3600}{}`)},
		{name: "duplicate escaped key", body: []byte(`{"id_token":"id","\u0069d_token":"other","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":3600}`)},
		{name: "duplicate escaped unknown key", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":3600,"future":1,"\u0066uture":2}`)},
		{name: "malformed unknown value", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":3600,"future":[}`)},
		{name: "missing identity token", body: []byte(`{"refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":3600}`)},
		{name: "empty refresh token", body: []byte(`{"id_token":"id","refresh_token":"","access_token":"access","token_type":"Bearer","expires_in":3600}`)},
		{name: "oversized access token", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"` + longToken + `","token_type":"Bearer","expires_in":3600}`)},
		{name: "wrong token type", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"bearer","expires_in":3600}`)},
		{name: "string expiry", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":"3600"}`)},
		{name: "fractional expiry", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":1.5}`)},
		{name: "exponent expiry", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":1e3}`)},
		{name: "zero expiry", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":0}`)},
		{name: "too large expiry", body: []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":86401}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture.body = test.body
			if _, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest()); Code(err) != "apple_unavailable" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
		})
	}

	fixture.body = []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":86400,"future":{"nested":true}}`)
	tokens, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest())
	if err != nil || tokens != (AppleTokens{IdentityToken: "id", RefreshToken: "refresh"}) {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
}

func TestHostedAppleTokenResponseToleratesArbitraryAdditiveUnknownValues(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	var unknowns strings.Builder
	for index := 0; index < 80; index++ {
		unknowns.WriteString(`,"future_`)
		unknowns.WriteString(strconv.Itoa(index))
		unknowns.WriteString(`":{"nested":[null,true,false,123,"value"]}`)
	}
	deepValue := strings.Repeat(`{"nested":`, 128) + `true` + strings.Repeat(`}`, 128)
	fixture.body = []byte(`{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":3600,"deep":` + deepValue + unknowns.String() + `}`)
	if len(fixture.body) >= 64<<10 {
		t.Fatalf("fixture unexpectedly exceeds response cap: %d", len(fixture.body))
	}
	tokens, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest())
	if err != nil || tokens != (AppleTokens{IdentityToken: "id", RefreshToken: "refresh"}) {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
}

func TestHostedAppleResponseBodyBoundaries(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	prefix := `{"id_token":"id","refresh_token":"refresh","access_token":"access","token_type":"Bearer","expires_in":3600,"padding":"`
	suffix := `"}`
	fixture.body = []byte(prefix + strings.Repeat("x", 64<<10-len(prefix)-len(suffix)) + suffix)
	if len(fixture.body) != 64<<10 {
		t.Fatalf("fixture bytes=%d", len(fixture.body))
	}
	if _, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest()); err != nil {
		t.Fatal(err)
	}
	fixture.body = append(fixture.body, ' ')
	if _, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest()); Code(err) != "apple_unavailable" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
}

func TestHostedAppleMapsStatusesAndSuppressesProviderDetails(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	const providerDetail = "DISTINCTIVE-PROVIDER-SECRET"
	fixture.body = []byte(`{"error":"invalid_grant","error_description":"` + providerDetail + `"}`)
	for _, status := range []int{400, 401, 403, 404, 409, 422, 429} {
		fixture.status = status
		_, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest())
		if Code(err) != "invalid_apple_authorization" || strings.Contains(err.Error(), providerDetail) {
			t.Fatalf("status=%d code=%q err=%v", status, Code(err), err)
		}
	}
	fixture.status = http.StatusBadRequest
	fixture.body = bytes.Repeat([]byte{'x'}, 64<<10+1)
	if _, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest()); Code(err) != "invalid_apple_authorization" {
		t.Fatalf("oversized 4xx code=%q err=%v", Code(err), err)
	}
	for _, status := range []int{300, 301, 307, 500, 502, 503, 504} {
		fixture.status = status
		_, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest())
		if Code(err) != "apple_unavailable" || strings.Contains(err.Error(), providerDetail) {
			t.Fatalf("status=%d code=%q err=%v", status, Code(err), err)
		}
	}

	networkDetail := errors.New("network " + providerDetail)
	fixture.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, networkDetail })
	_, err := fixture.apple().ExchangeCode(t.Context(), validAppleExchangeRequest())
	if Code(err) != "apple_unavailable" || strings.Contains(err.Error(), providerDetail) {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
}

func TestHostedAppleRejectsInvalidInputsWithoutNetwork(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	apple := fixture.apple()
	request := validAppleExchangeRequest()
	request.AuthorizationCode = ""
	if _, err := apple.ExchangeCode(t.Context(), request); Code(err) != "invalid_apple_authorization" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
	request.AuthorizationCode = strings.Repeat("x", 16<<10+1)
	if _, err := apple.ExchangeCode(t.Context(), request); Code(err) != "invalid_apple_authorization" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
	if fixture.requests.Load() != 0 {
		t.Fatalf("requests=%d", fixture.requests.Load())
	}
}

func TestHostedAppleSupportsConcurrentExchanges(t *testing.T) {
	fixture := newHostedAppleFixture(t)
	fixture.requestFn = func(request *http.Request) {
		_ = readForm(t, request)
	}
	apple := fixture.apple()
	var wait sync.WaitGroup
	for index := 0; index < 40; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := apple.ExchangeCode(t.Context(), validAppleExchangeRequest()); err != nil {
				t.Errorf("exchange: %v", err)
			}
		}()
	}
	wait.Wait()
	if fixture.requests.Load() != 40 {
		t.Fatalf("requests=%d", fixture.requests.Load())
	}
}

func newBigInt(encoded []byte) *big.Int {
	return new(big.Int).SetBytes(encoded)
}
