package identity

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	appleIssuer           = "https://appleid.apple.com"
	maxIdentityTokenBytes = 16 << 10
	maxJWTJSONDepth       = 8
	maxJWTJSONKeys        = 64
)

type AppleVerifier struct {
	keys     *JWKSCache
	audience string
	now      func() time.Time
}

func NewAppleVerifier(options AppleOptions) (IdentityVerifier, error) {
	if !validBoundedText(options.Audience, 255) {
		return nil, ErrInvalidRequest
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &AppleVerifier{
		keys:     newJWKSCache(options.HTTP, now),
		audience: options.Audience,
		now:      now,
	}, nil
}

func (verifier *AppleVerifier) Verify(ctx context.Context, identityToken, expectedNonce string) (VerifiedIdentity, error) {
	if verifier == nil || verifier.keys == nil || verifier.now == nil || len(identityToken) == 0 || len(identityToken) > maxIdentityTokenBytes || !validAppleNonce(expectedNonce) {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	segments := strings.Split(identityToken, ".")
	if len(segments) != 3 {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	headerBytes, err := decodeCanonicalBase64URL(segments[0])
	if err != nil {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	claimsBytes, err := decodeCanonicalBase64URL(segments[1])
	if err != nil {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	signature, err := decodeCanonicalBase64URL(segments[2])
	if err != nil || len(signature) == 0 {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}

	header, err := strictJSONObject(headerBytes, maxJWTJSONDepth, maxJWTJSONKeys)
	if err != nil {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	algorithm, err := exactJSONString(header, "alg")
	if err != nil || algorithm != "RS256" {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	kid, err := exactJSONString(header, "kid")
	if err != nil || !validASCIIIdentifier(kid, 128) {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	if _, present := header["jku"]; present {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	if _, present := header["x5u"]; present {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}

	claims, err := strictJSONObject(claimsBytes, maxJWTJSONDepth, maxJWTJSONKeys)
	if err != nil {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	issuer, err := exactJSONString(claims, "iss")
	if err != nil || issuer != appleIssuer {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	audience, err := exactJSONString(claims, "aud")
	if err != nil || audience != verifier.audience {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	subject, err := exactJSONString(claims, "sub")
	if err != nil || !validBoundedText(subject, 255) {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	nonce, err := exactJSONString(claims, "nonce")
	if err != nil || !constantTimeStringEqual(nonce, expectedNonce) {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	expiresUnix, err := exactIntegralNumericDate(claims, "exp")
	if err != nil {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	issuedUnix, err := exactIntegralNumericDate(claims, "iat")
	if err != nil {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	now := verifier.now()
	expiresAt := time.Unix(expiresUnix, 0).UTC()
	issuedAt := time.Unix(issuedUnix, 0).UTC()
	if !expiresAt.After(now) || issuedAt.After(now.Add(time.Minute)) || issuedAt.Before(now.Add(-10*time.Minute)) {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}

	key, err := verifier.keys.Key(ctx, kid)
	if err != nil {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return VerifiedIdentity{}, ErrInvalidAppleProof
	}
	return VerifiedIdentity{
		Provider:  "apple",
		Subject:   subject,
		Audience:  audience,
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}, nil
}

func decodeCanonicalBase64URL(segment string) ([]byte, error) {
	if segment == "" {
		return nil, errors.New("empty base64url segment")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != segment {
		return nil, errors.New("noncanonical base64url segment")
	}
	return decoded, nil
}

func strictJSONObject(data []byte, maxDepth, maxKeys int) (map[string]json.RawMessage, error) {
	if len(data) == 0 || !utf8.Valid(data) {
		return nil, errors.New("invalid json encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("json value is not an object")
	}
	keyCount := 0
	if err := walkJSONObject(decoder, 1, maxDepth, maxKeys, &keyCount); err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, errors.New("trailing json data")
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil || value == nil {
		return nil, errors.New("invalid json object")
	}
	return value, nil
}

func walkJSONObject(decoder *json.Decoder, depth, maxDepth, maxKeys int, keyCount *int) error {
	if depth > maxDepth {
		return errors.New("json nesting exceeds bound")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("json object key is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("duplicate json key")
		}
		seen[key] = struct{}{}
		*keyCount = *keyCount + 1
		if *keyCount > maxKeys {
			return errors.New("json key count exceeds bound")
		}
		if err := walkJSONValue(decoder, depth, maxDepth, maxKeys, keyCount); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("invalid json object")
	}
	return nil
}

func walkJSONArray(decoder *json.Decoder, depth, maxDepth, maxKeys int, keyCount *int) error {
	if depth > maxDepth {
		return errors.New("json nesting exceeds bound")
	}
	for decoder.More() {
		if err := walkJSONValue(decoder, depth, maxDepth, maxKeys, keyCount); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim(']') {
		return errors.New("invalid json array")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, depth, maxDepth, maxKeys int, keyCount *int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		return walkJSONObject(decoder, depth+1, maxDepth, maxKeys, keyCount)
	case '[':
		return walkJSONArray(decoder, depth+1, maxDepth, maxKeys, keyCount)
	default:
		return errors.New("unexpected json delimiter")
	}
}

func exactJSONString(object map[string]json.RawMessage, name string) (string, error) {
	raw, present := object[name]
	if !present {
		return "", errors.New("missing string claim")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("claim is not a string")
	}
	return value, nil
}

func exactIntegralNumericDate(object map[string]json.RawMessage, name string) (int64, error) {
	raw, present := object[name]
	if !present || len(raw) == 0 {
		return 0, errors.New("missing numeric date")
	}
	text := string(raw)
	start := 0
	if text[0] == '-' {
		start = 1
	}
	if start == len(text) {
		return 0, errors.New("invalid numeric date")
	}
	for _, character := range text[start:] {
		if character < '0' || character > '9' {
			return 0, errors.New("numeric date is not integral")
		}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, errors.New("numeric date is out of range")
	}
	return value, nil
}

func validASCIIIdentifier(value string, maxBytes int) bool {
	if len(value) == 0 || len(value) > maxBytes {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func validBoundedText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validAppleNonce(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func constantTimeStringEqual(left, right string) bool {
	leftDigest := sha256.Sum256([]byte(left))
	rightDigest := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftDigest[:], rightDigest[:]) == 1
}
