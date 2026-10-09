package identity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testAppleAudience = "com.example.babytracker"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type appleVerifierFixture struct {
	t        *testing.T
	key      *rsa.PrivateKey
	now      time.Time
	nonce    string
	client   *http.Client
	requests atomic.Int32
	header   string
	body     string
	status   int
}

func newAppleVerifierFixture(t *testing.T) *appleVerifierFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &appleVerifierFixture{
		t: t, key: key,
		now:    time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		nonce:  strings.Repeat("a", 64),
		header: "max-age=3600",
		status: http.StatusOK,
	}
	fixture.body = jwksDocument("apple-key", &key.PublicKey, `,"use":"sig","alg":"RS256"`)
	fixture.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		fixture.requests.Add(1)
		if request.URL.String() != appleJWKSURL {
			t.Fatalf("JWKS URL=%q", request.URL.String())
		}
		response := &http.Response{
			StatusCode: fixture.status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(fixture.body)),
			Request:    request,
		}
		if fixture.header != "" {
			response.Header.Set("Cache-Control", fixture.header)
		}
		return response, nil
	})}
	return fixture
}

func (fixture *appleVerifierFixture) verifier() IdentityVerifier {
	fixture.t.Helper()
	verifier, err := NewAppleVerifier(AppleOptions{
		Audience: testAppleAudience,
		HTTP:     fixture.client,
		Now:      func() time.Time { return fixture.now },
	})
	if err != nil {
		fixture.t.Fatal(err)
	}
	return verifier
}

func (fixture *appleVerifierFixture) claims(overrides string) string {
	base := fmt.Sprintf(`"iss":"https://appleid.apple.com","aud":%q,"sub":"apple-subject","exp":%d,"iat":%d,"nonce":%q`,
		testAppleAudience, fixture.now.Add(time.Hour).Unix(), fixture.now.Unix(), fixture.nonce)
	if overrides == "" {
		return "{" + base + "}"
	}
	return "{" + overrides + "}"
}

func signJWT(t *testing.T, key *rsa.PrivateKey, header, claims string) string {
	t.Helper()
	encodedHeader := base64.RawURLEncoding.EncodeToString([]byte(header))
	encodedClaims := base64.RawURLEncoding.EncodeToString([]byte(claims))
	signingInput := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func jwksDocument(kid string, key *rsa.PublicKey, extra string) string {
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	eBytes := []byte{byte(key.E >> 16), byte(key.E >> 8), byte(key.E)}
	for len(eBytes) > 1 && eBytes[0] == 0 {
		eBytes = eBytes[1:]
	}
	e := base64.RawURLEncoding.EncodeToString(eBytes)
	return fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":%q,"n":%q,"e":%q%s}]}`, kid, n, e, extra)
}

func TestAppleVerifierAcceptsStrictRS256Proof(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	token := signJWT(t, fixture.key, `{"alg":"RS256","kid":"apple-key","typ":"JWT"}`, fixture.claims(""))
	verified, err := fixture.verifier().Verify(t.Context(), token, fixture.nonce)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Provider != "apple" || verified.Subject != "apple-subject" || verified.Audience != testAppleAudience ||
		!verified.IssuedAt.Equal(fixture.now) || !verified.ExpiresAt.Equal(fixture.now.Add(time.Hour)) || verified.IdentityKey != "" {
		t.Fatalf("verified identity=%+v", verified)
	}
	if fixture.requests.Load() != 1 {
		t.Fatalf("JWKS requests=%d", fixture.requests.Load())
	}
}

func TestAppleVerifierRejectsMalformedOrUntrustedProofs(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	validHeader := `{"alg":"RS256","kid":"apple-key"}`
	validClaims := fixture.claims("")
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	manyClaims := make([]string, 65)
	for index := range manyClaims {
		manyClaims[index] = fmt.Sprintf(`"x%d":%d`, index, index)
	}
	deep := `{"iss":"https://appleid.apple.com","aud":"com.example.babytracker","sub":"apple-subject","exp":1790000000,"iat":1789905600,"nonce":"` + fixture.nonce + `","x":{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":1}}}}}}}}}`
	tests := []struct {
		name  string
		token func() string
		nonce string
	}{
		{name: "two segments", token: func() string { return "a.b" }, nonce: fixture.nonce},
		{name: "four segments", token: func() string { return "a.b.c.d" }, nonce: fixture.nonce},
		{name: "oversized", token: func() string { return strings.Repeat("a", maxIdentityTokenBytes+1) }, nonce: fixture.nonce},
		{name: "padded header", token: func() string { return base64.URLEncoding.EncodeToString([]byte(validHeader)) + ".e30.x" }, nonce: fixture.nonce},
		{name: "invalid header utf8", token: func() string { return signJWT(t, fixture.key, string([]byte{0xff}), validClaims) }, nonce: fixture.nonce},
		{name: "invalid claims utf8", token: func() string { return signJWT(t, fixture.key, validHeader, string([]byte{0xff})) }, nonce: fixture.nonce},
		{name: "duplicate escaped header key", token: func() string {
			return signJWT(t, fixture.key, `{"alg":"RS256","\u0061lg":"RS256","kid":"apple-key"}`, validClaims)
		}, nonce: fixture.nonce},
		{name: "duplicate escaped claim key", token: func() string {
			return signJWT(t, fixture.key, validHeader, strings.TrimSuffix(validClaims, "}")+`,"\u0061ud":"other"}`)
		}, nonce: fixture.nonce},
		{name: "non object header", token: func() string { return signJWT(t, fixture.key, `[]`, validClaims) }, nonce: fixture.nonce},
		{name: "non object claims", token: func() string { return signJWT(t, fixture.key, validHeader, `[]`) }, nonce: fixture.nonce},
		{name: "trailing header", token: func() string { return signJWT(t, fixture.key, validHeader+` {}`, validClaims) }, nonce: fixture.nonce},
		{name: "alg none", token: func() string { return signJWT(t, fixture.key, `{"alg":"none","kid":"apple-key"}`, validClaims) }, nonce: fixture.nonce},
		{name: "alg hs256", token: func() string { return signJWT(t, fixture.key, `{"alg":"HS256","kid":"apple-key"}`, validClaims) }, nonce: fixture.nonce},
		{name: "jku", token: func() string {
			return signJWT(t, fixture.key, `{"alg":"RS256","kid":"apple-key","jku":"https://attacker.test"}`, validClaims)
		}, nonce: fixture.nonce},
		{name: "x5u", token: func() string {
			return signJWT(t, fixture.key, `{"alg":"RS256","kid":"apple-key","x5u":"https://attacker.test"}`, validClaims)
		}, nonce: fixture.nonce},
		{name: "empty kid", token: func() string { return signJWT(t, fixture.key, `{"alg":"RS256","kid":""}`, validClaims) }, nonce: fixture.nonce},
		{name: "non ascii kid", token: func() string { return signJWT(t, fixture.key, `{"alg":"RS256","kid":"äpple"}`, validClaims) }, nonce: fixture.nonce},
		{name: "long kid", token: func() string {
			return signJWT(t, fixture.key, `{"alg":"RS256","kid":"`+strings.Repeat("a", 129)+`"}`, validClaims)
		}, nonce: fixture.nonce},
		{name: "wrong issuer", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://attacker.test","aud":"`+testAppleAudience+`","sub":"apple-subject","exp":`+strconv.FormatInt(fixture.now.Add(time.Hour).Unix(), 10)+`,"iat":`+strconv.FormatInt(fixture.now.Unix(), 10)+`,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "substring audience", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://appleid.apple.com","aud":"prefix.`+testAppleAudience+`","sub":"apple-subject","exp":`+strconv.FormatInt(fixture.now.Add(time.Hour).Unix(), 10)+`,"iat":`+strconv.FormatInt(fixture.now.Unix(), 10)+`,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "audience array", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://appleid.apple.com","aud":["`+testAppleAudience+`"],"sub":"apple-subject","exp":`+strconv.FormatInt(fixture.now.Add(time.Hour).Unix(), 10)+`,"iat":`+strconv.FormatInt(fixture.now.Unix(), 10)+`,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "empty subject", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://appleid.apple.com","aud":"`+testAppleAudience+`","sub":"","exp":`+strconv.FormatInt(fixture.now.Add(time.Hour).Unix(), 10)+`,"iat":`+strconv.FormatInt(fixture.now.Unix(), 10)+`,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "long subject", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://appleid.apple.com","aud":"`+testAppleAudience+`","sub":"`+strings.Repeat("s", 256)+`","exp":`+strconv.FormatInt(fixture.now.Add(time.Hour).Unix(), 10)+`,"iat":`+strconv.FormatInt(fixture.now.Unix(), 10)+`,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "expired", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://appleid.apple.com","aud":"`+testAppleAudience+`","sub":"apple-subject","exp":`+strconv.FormatInt(fixture.now.Unix(), 10)+`,"iat":`+strconv.FormatInt(fixture.now.Unix(), 10)+`,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "future iat", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://appleid.apple.com","aud":"`+testAppleAudience+`","sub":"apple-subject","exp":`+strconv.FormatInt(fixture.now.Add(time.Hour).Unix(), 10)+`,"iat":`+strconv.FormatInt(fixture.now.Add(61*time.Second).Unix(), 10)+`,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "old iat", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://appleid.apple.com","aud":"`+testAppleAudience+`","sub":"apple-subject","exp":`+strconv.FormatInt(fixture.now.Add(time.Hour).Unix(), 10)+`,"iat":`+strconv.FormatInt(fixture.now.Add(-10*time.Minute-time.Second).Unix(), 10)+`,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "fractional exp", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://appleid.apple.com","aud":"`+testAppleAudience+`","sub":"apple-subject","exp":1790000000.5,"iat":`+strconv.FormatInt(fixture.now.Unix(), 10)+`,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "exponent iat", token: func() string {
			return signJWT(t, fixture.key, validHeader, fixture.claims(`"iss":"https://appleid.apple.com","aud":"`+testAppleAudience+`","sub":"apple-subject","exp":1790000000,"iat":1e9,"nonce":"`+fixture.nonce+`"`))
		}, nonce: fixture.nonce},
		{name: "wrong nonce", token: func() string { return signJWT(t, fixture.key, validHeader, validClaims) }, nonce: strings.Repeat("b", 64)},
		{name: "empty expected nonce", token: func() string { return signJWT(t, fixture.key, validHeader, validClaims) }, nonce: ""},
		{name: "noncanonical expected nonce", token: func() string { return signJWT(t, fixture.key, validHeader, validClaims) }, nonce: strings.Repeat("A", 64)},
		{name: "missing nonce", token: func() string {
			return signJWT(t, fixture.key, validHeader, strings.Replace(validClaims, `,"nonce":"`+fixture.nonce+`"`, "", 1))
		}, nonce: fixture.nonce},
		{name: "wrong signature", token: func() string { return signJWT(t, otherKey, validHeader, validClaims) }, nonce: fixture.nonce},
		{name: "unknown kid", token: func() string { return signJWT(t, fixture.key, `{"alg":"RS256","kid":"unknown"}`, validClaims) }, nonce: fixture.nonce},
		{name: "too deep", token: func() string { return signJWT(t, fixture.key, validHeader, deep) }, nonce: fixture.nonce},
		{name: "too many keys", token: func() string { return signJWT(t, fixture.key, validHeader, "{"+strings.Join(manyClaims, ",")+"}") }, nonce: fixture.nonce},
	}
	verifier := fixture.verifier()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := verifier.Verify(t.Context(), test.token(), test.nonce)
			if Code(err) != "invalid_apple_proof" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
		})
	}
}

func TestAppleVerifierAcceptsNumericDateBoundaries(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	claims := fixture.claims(`"iss":"https://appleid.apple.com","aud":"` + testAppleAudience + `","sub":"apple-subject","exp":` + strconv.FormatInt(fixture.now.Add(time.Nanosecond).Unix()+1, 10) + `,"iat":` + strconv.FormatInt(fixture.now.Add(-10*time.Minute).Unix(), 10) + `,"nonce":"` + fixture.nonce + `"`)
	token := signJWT(t, fixture.key, `{"alg":"RS256","kid":"apple-key"}`, claims)
	if _, err := fixture.verifier().Verify(t.Context(), token, fixture.nonce); err != nil {
		t.Fatal(err)
	}
}

func TestJWKSRejectsInvalidResponsesAndKeys(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	validN := base64.RawURLEncoding.EncodeToString(fixture.key.PublicKey.N.Bytes())
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "status", status: http.StatusInternalServerError, body: fixture.body},
		{name: "invalid json", status: http.StatusOK, body: `{"keys":`},
		{name: "invalid utf8", status: http.StatusOK, body: string([]byte{0xff})},
		{name: "duplicate escaped keys", status: http.StatusOK, body: `{"keys":[],"\u006beys":[]}`},
		{name: "not object", status: http.StatusOK, body: `[]`},
		{name: "too many keys", status: http.StatusOK, body: `{"keys":[` + strings.TrimSuffix(strings.Repeat(fmt.Sprintf(`{"kty":"RSA","kid":"x","n":%q,"e":"AQAB","use":"sig","alg":"RS256"},`, validN), maxJWKSKeys+1), ",") + `]}`},
		{name: "wrong kty", status: http.StatusOK, body: strings.Replace(fixture.body, `"kty":"RSA"`, `"kty":"EC"`, 1)},
		{name: "wrong use", status: http.StatusOK, body: strings.Replace(fixture.body, `"use":"sig"`, `"use":"enc"`, 1)},
		{name: "wrong alg", status: http.StatusOK, body: strings.Replace(fixture.body, `"alg":"RS256"`, `"alg":"RS512"`, 1)},
		{name: "wrong key ops", status: http.StatusOK, body: strings.Replace(fixture.body, `,"use":"sig"`, `,"use":"sig","key_ops":["sign"]`, 1)},
		{name: "duplicate key ops", status: http.StatusOK, body: strings.Replace(fixture.body, `,"use":"sig"`, `,"use":"sig","key_ops":["verify","verify"]`, 1)},
		{name: "invalid n", status: http.StatusOK, body: strings.Replace(fixture.body, `"n":"`+validN+`"`, `"n":"***"`, 1)},
		{name: "padded n", status: http.StatusOK, body: strings.Replace(fixture.body, `"n":"`+validN+`"`, `"n":"`+validN+`="`, 1)},
		{name: "small modulus", status: http.StatusOK, body: jwksWithRawValues("apple-key", base64.RawURLEncoding.EncodeToString(makeModulus(2047)), "AQAB", `,"use":"sig","alg":"RS256"`)},
		{name: "large modulus", status: http.StatusOK, body: jwksWithRawValues("apple-key", base64.RawURLEncoding.EncodeToString(makeModulus(4097)), "AQAB", `,"use":"sig","alg":"RS256"`)},
		{name: "leading zero modulus", status: http.StatusOK, body: jwksWithRawValues("apple-key", base64.RawURLEncoding.EncodeToString(append([]byte{0}, fixture.key.PublicKey.N.Bytes()...)), "AQAB", `,"use":"sig","alg":"RS256"`)},
		{name: "exponent one", status: http.StatusOK, body: jwksWithRawValues("apple-key", validN, "AQ", `,"use":"sig","alg":"RS256"`)},
		{name: "even exponent", status: http.StatusOK, body: jwksWithRawValues("apple-key", validN, "BA", `,"use":"sig","alg":"RS256"`)},
		{name: "leading zero exponent", status: http.StatusOK, body: jwksWithRawValues("apple-key", validN, "AAEAAQ", `,"use":"sig","alg":"RS256"`)},
		{name: "duplicate kid", status: http.StatusOK, body: `{"keys":[` + strings.TrimPrefix(strings.TrimSuffix(fixture.body, `]}`), `{"keys":[`) + `,` + strings.TrimPrefix(strings.TrimSuffix(fixture.body, `]}`), `{"keys":[`) + `]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)), Request: request}, nil
			})}
			verifier, err := NewAppleVerifier(AppleOptions{Audience: testAppleAudience, HTTP: client, Now: func() time.Time { return fixture.now }})
			if err != nil {
				t.Fatal(err)
			}
			token := signJWT(t, fixture.key, `{"alg":"RS256","kid":"apple-key"}`, fixture.claims(""))
			if _, err := verifier.Verify(t.Context(), token, fixture.nonce); Code(err) != "invalid_apple_proof" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
		})
	}
}

func TestJWKSAccepts2048And4096BitRSAKeys(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	for _, bits := range []int{2048, 4096} {
		key := &rsa.PublicKey{N: newRSAInteger(bits), E: 65537}
		body := jwksDocument("key", key, `,"use":"sig","alg":"RS256","key_ops":["verify"]`)
		cache := newTestJWKSCache(t, &fixture.now, body, "max-age=60", nil)
		got, err := cache.Key(t.Context(), "key")
		if err != nil || got.N.BitLen() != bits {
			t.Fatalf("bits=%d got=%v err=%v", bits, got, err)
		}
	}
}

func TestJWKSBodyLimitRedirectAndDeadline(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	t.Run("body limit", func(t *testing.T) {
		body := strings.Repeat(" ", maxJWKSBodyBytes+1)
		cache := newTestJWKSCache(t, &fixture.now, body, "", nil)
		if _, err := cache.Key(t.Context(), "key"); err == nil {
			t.Fatal("oversized body accepted")
		}
	})
	t.Run("redirect denied without mutating client", func(t *testing.T) {
		var originalCalled atomic.Bool
		client := &http.Client{
			Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://attacker.test/keys"}}, Body: io.NopCloser(strings.NewReader("redirect")), Request: request}, nil
			}),
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { originalCalled.Store(true); return nil },
		}
		verifier, err := NewAppleVerifier(AppleOptions{Audience: testAppleAudience, HTTP: client, Now: func() time.Time { return fixture.now }})
		if err != nil {
			t.Fatal(err)
		}
		token := signJWT(t, fixture.key, `{"alg":"RS256","kid":"apple-key"}`, fixture.claims(""))
		if _, err := verifier.Verify(t.Context(), token, fixture.nonce); Code(err) != "invalid_apple_proof" {
			t.Fatalf("code=%q err=%v", Code(err), err)
		}
		if originalCalled.Load() {
			t.Fatal("injected client's redirect function was used or mutated")
		}
	})
	t.Run("five second deadline", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			deadline, ok := request.Context().Deadline()
			if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) < 4*time.Second {
				t.Fatalf("deadline=%v ok=%v", deadline, ok)
			}
			return nil, context.DeadlineExceeded
		})}
		verifier, err := NewAppleVerifier(AppleOptions{Audience: testAppleAudience, HTTP: client, Now: func() time.Time { return fixture.now }})
		if err != nil {
			t.Fatal(err)
		}
		token := signJWT(t, fixture.key, `{"alg":"RS256","kid":"apple-key"}`, fixture.claims(""))
		if _, err := verifier.Verify(t.Context(), token, fixture.nonce); Code(err) != "invalid_apple_proof" {
			t.Fatalf("code=%q err=%v", Code(err), err)
		}
	})
}

func TestJWKSCacheTTLAndExpiredFailure(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	tests := []struct {
		name      string
		cache     string
		freshFor  time.Duration
		expiredAt time.Duration
	}{
		{name: "default", cache: "", freshFor: 59 * time.Minute, expiredAt: time.Hour},
		{name: "max age", cache: "max-age=60", freshFor: 59 * time.Second, expiredAt: time.Minute},
		{name: "six hour cap", cache: "max-age=999999", freshFor: 6*time.Hour - time.Second, expiredAt: 6 * time.Hour},
		{name: "no cache", cache: "no-cache, max-age=3600", freshFor: 0, expiredAt: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := fixture.now
			var requests atomic.Int32
			fail := atomic.Bool{}
			cache := newTestJWKSCache(t, &now, fixture.body, test.cache, func() error {
				requests.Add(1)
				if fail.Load() {
					return context.DeadlineExceeded
				}
				return nil
			})
			if _, err := cache.Key(t.Context(), "apple-key"); err != nil {
				t.Fatal(err)
			}
			first := requests.Load()
			if test.freshFor > 0 {
				now = now.Add(test.freshFor)
				fail.Store(true)
				if _, err := cache.Key(t.Context(), "apple-key"); err != nil {
					t.Fatalf("fresh cached key rejected: %v", err)
				}
				if requests.Load() != first {
					t.Fatalf("fresh cache fetched: %d -> %d", first, requests.Load())
				}
				now = fixture.now.Add(test.expiredAt)
			} else {
				fail.Store(true)
			}
			if _, err := cache.Key(t.Context(), "apple-key"); err == nil {
				t.Fatal("expired cache survived refresh failure")
			}
		})
	}
}

func TestJWKSExpiredShortTTLRefreshesInsideUnknownKeyCooldown(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	now := fixture.now
	var requests atomic.Int32
	cache := newTestJWKSCache(t, &now, fixture.body, "max-age=1", func() error {
		requests.Add(1)
		return nil
	})
	if _, err := cache.Key(t.Context(), "apple-key"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := cache.Key(t.Context(), "apple-key"); err != nil {
		t.Fatalf("expired healthy cache did not refresh: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("JWKS requests=%d, want one request per expired lookup", requests.Load())
	}
}

func TestJWKSUnknownKidCooldownSurvivesZeroTTL(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	now := fixture.now
	var requests atomic.Int32
	cache := newTestJWKSCache(t, &now, fixture.body, "no-cache", func() error {
		requests.Add(1)
		return nil
	})
	if _, err := cache.Key(t.Context(), "missing-key"); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := cache.Key(t.Context(), "missing-key"); err == nil {
		t.Fatal("unknown key accepted inside cooldown")
	}
	if requests.Load() != 1 {
		t.Fatalf("JWKS requests=%d, want one unknown-key refresh inside cooldown", requests.Load())
	}
	now = now.Add(unknownKidCooldown)
	if _, err := cache.Key(t.Context(), "missing-key"); err == nil {
		t.Fatal("unknown key accepted after cooldown")
	}
	if requests.Load() != 2 {
		t.Fatalf("JWKS requests=%d, want refresh after cooldown", requests.Load())
	}
}

func TestJWKSFailedUnknownKidRefreshIsCoalescedAndCooled(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	now := fixture.now
	var requests atomic.Int32
	cache := newTestJWKSCache(t, &now, fixture.body, "no-cache", func() error {
		requests.Add(1)
		return context.DeadlineExceeded
	})
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := cache.Key(t.Context(), "missing-key"); err == nil {
				t.Error("unknown key accepted after failed refresh")
			}
		}()
	}
	wait.Wait()
	if requests.Load() != 1 {
		t.Fatalf("JWKS requests=%d, want one failed refresh inside cooldown", requests.Load())
	}
	if _, err := cache.Key(t.Context(), "another-missing-key"); err == nil {
		t.Fatal("second unknown key accepted inside cooldown")
	}
	if requests.Load() != 1 {
		t.Fatalf("JWKS requests=%d after cooldown check, want one", requests.Load())
	}
	now = now.Add(unknownKidCooldown)
	if _, err := cache.Key(t.Context(), "missing-key"); err == nil {
		t.Fatal("unknown key accepted after cooldown")
	}
	if requests.Load() != 2 {
		t.Fatalf("JWKS requests=%d, want retry after cooldown", requests.Load())
	}
}

func TestJWKSUnknownKidRefreshIsCoalescedForOneHundredCallers(t *testing.T) {
	fixture := newAppleVerifierFixture(t)
	now := fixture.now
	var requests atomic.Int32
	cache := newTestJWKSCache(t, &now, fixture.body, "max-age=3600", func() error {
		requests.Add(1)
		return nil
	})
	if _, err := cache.Key(t.Context(), "apple-key"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	var wait sync.WaitGroup
	var unexpected atomic.Int32
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := cache.Key(t.Context(), "unknown"); err == nil {
				unexpected.Add(1)
			}
		}()
	}
	wait.Wait()
	if unexpected.Load() != 0 {
		t.Fatalf("unknown key successes=%d", unexpected.Load())
	}
	if requests.Load() != 2 {
		t.Fatalf("JWKS requests=%d, want initial plus one coalesced refresh", requests.Load())
	}
}

func TestNewAppleVerifierValidatesOptions(t *testing.T) {
	for _, audience := range []string{"", strings.Repeat("a", 256), "bad\x00audience", string([]byte{0xff})} {
		if _, err := NewAppleVerifier(AppleOptions{Audience: audience}); err == nil {
			t.Fatalf("audience %q accepted", audience)
		}
	}
	if _, err := NewAppleVerifier(AppleOptions{Audience: testAppleAudience}); err != nil {
		t.Fatal(err)
	}
}

func newTestJWKSCache(t *testing.T, now *time.Time, body, cacheControl string, hook func() error) *JWKSCache {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if hook != nil {
			if err := hook(); err != nil {
				return nil, err
			}
		}
		header := make(http.Header)
		if cacheControl != "" {
			header.Set("Cache-Control", cacheControl)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	return newJWKSCache(client, func() time.Time { return *now })
}

func jwksWithRawValues(kid, n, e, extra string) string {
	return fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":%q,"n":%q,"e":%q%s}]}`, kid, n, e, extra)
}

func makeModulus(bits int) []byte {
	value := make([]byte, (bits+7)/8)
	value[0] = byte(1 << ((bits - 1) % 8))
	return value
}

func newRSAInteger(bits int) *big.Int {
	return new(big.Int).SetBytes(makeModulus(bits))
}
