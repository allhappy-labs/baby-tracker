//go:build identitytest

package testidentity

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"time"

	"babytracker/server/internal/identity"
)

type Issuer struct {
	key *rsa.PrivateKey
	kid string
	now func() time.Time
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

// Verifier returns the production Apple JWT verifier with its key transport
// pinned to this compile-time test issuer. No runtime setting can select it.
func (issuer *Issuer) Verifier(audience string) (identity.IdentityVerifier, error) {
	jwks, err := issuer.JWKS()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(jwks)), Request: request}, nil
	})}
	return identity.NewAppleVerifier(identity.AppleOptions{Audience: audience, Now: issuer.now, HTTP: client})
}

func New(kid string, now func() time.Time) (*Issuer, error) {
	if kid == "" || now == nil {
		return nil, errors.New("test issuer requires a key ID and clock")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &Issuer{key: key, kid: kid, now: now}, nil
}

func (issuer *Issuer) JWKS() ([]byte, error) {
	if issuer == nil || issuer.key == nil {
		return nil, errors.New("test issuer is unavailable")
	}
	exponent := big.NewInt(int64(issuer.key.PublicKey.E)).Bytes()
	return json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": issuer.kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(issuer.key.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(exponent),
	}}})
}

func (issuer *Issuer) Sign(audience, subject, nonce string) (string, error) {
	if issuer == nil || issuer.key == nil || audience == "" || subject == "" || nonce == "" {
		return "", errors.New("test identity proof inputs are required")
	}
	now := issuer.now().UTC()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": issuer.kid})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": "https://appleid.apple.com", "aud": audience, "sub": subject,
		"nonce": nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, issuer.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
