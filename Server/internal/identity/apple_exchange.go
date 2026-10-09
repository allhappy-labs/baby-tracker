package identity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	appleTokenURL             = "https://appleid.apple.com/auth/token"
	appleRevokeURL            = "https://appleid.apple.com/auth/revoke"
	appleClientSecretLifetime = 5 * time.Minute
	maxAppleResponseBytes     = 64 << 10
	maxAppleAuthorizationCode = 16 << 10
	maxAppleCredentialBytes   = 16 << 10
)

var (
	appleDeveloperIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)

	ErrInvalidAppleAuthorization = codedError("invalid_apple_authorization")
	ErrAppleUnavailable          = codedError("apple_unavailable")
)

type HostedAppleOptions struct {
	ClientID    string
	TeamID      string
	KeyID       string
	RedirectURI string
	SigningKey  *ecdsa.PrivateKey
	HTTP        *http.Client
	Now         func() time.Time
}

type HostedApple struct {
	http        *http.Client
	clientID    string
	teamID      string
	keyID       string
	redirectURI string
	signingKey  *ecdsa.PrivateKey
	now         func() time.Time
}

func NewHostedApple(options HostedAppleOptions) (AppleAuthority, error) {
	if !validAppleClientID(options.ClientID) ||
		!appleDeveloperIDPattern.MatchString(options.TeamID) ||
		!appleDeveloperIDPattern.MatchString(options.KeyID) ||
		!validAppleRedirectURI(options.RedirectURI) {
		return nil, ErrInvalidRequest
	}
	signingKey, ok := cloneAppleSigningKey(options.SigningKey)
	if !ok {
		return nil, ErrInvalidRequest
	}
	client := options.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	clientClone := *client
	clientClone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &HostedApple{
		http:        &clientClone,
		clientID:    options.ClientID,
		teamID:      options.TeamID,
		keyID:       options.KeyID,
		redirectURI: options.RedirectURI,
		signingKey:  signingKey,
		now:         now,
	}, nil
}

func (apple *HostedApple) ExchangeCode(ctx context.Context, request AppleCodeExchangeRequest) (AppleTokens, error) {
	if apple == nil || apple.http == nil || apple.signingKey == nil || apple.now == nil ||
		!validAppleOpaqueString(request.AuthorizationCode, maxAppleAuthorizationCode) {
		return AppleTokens{}, ErrInvalidAppleAuthorization
	}
	clientSecret, err := apple.clientSecret()
	if err != nil {
		return AppleTokens{}, ErrAppleUnavailable
	}
	form := url.Values{
		"client_id":     []string{apple.clientID},
		"client_secret": []string{clientSecret},
		"code":          []string{request.AuthorizationCode},
		"grant_type":    []string{"authorization_code"},
	}
	if apple.redirectURI != "" {
		form.Set("redirect_uri", apple.redirectURI)
	}
	status, body, err := apple.postForm(ctx, appleTokenURL, form)
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return AppleTokens{}, ErrInvalidAppleAuthorization
	}
	if err != nil {
		return AppleTokens{}, ErrAppleUnavailable
	}
	if status != http.StatusOK {
		return AppleTokens{}, ErrAppleUnavailable
	}
	tokens, err := parseAppleTokenResponse(body)
	if err != nil {
		return AppleTokens{}, ErrAppleUnavailable
	}
	return tokens, nil
}

func (apple *HostedApple) Revoke(ctx context.Context, request AppleRevocationRequest) error {
	if apple == nil || apple.http == nil || apple.signingKey == nil || apple.now == nil ||
		!validAppleOpaqueBytes(request.RefreshToken, maxAppleCredentialBytes) {
		return ErrInvalidAppleAuthorization
	}
	clientSecret, err := apple.clientSecret()
	if err != nil {
		return ErrAppleUnavailable
	}
	form := url.Values{
		"client_id":       []string{apple.clientID},
		"client_secret":   []string{clientSecret},
		"token":           []string{string(request.RefreshToken)},
		"token_type_hint": []string{"refresh_token"},
	}
	status, _, err := apple.postForm(ctx, appleRevokeURL, form)
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return ErrInvalidAppleAuthorization
	}
	if err != nil {
		return ErrAppleUnavailable
	}
	if status != http.StatusOK {
		return ErrAppleUnavailable
	}
	return nil
}

func (apple *HostedApple) clientSecret() (string, error) {
	issuedAt := apple.now().Unix()
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}{Algorithm: "ES256", KeyID: apple.keyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(struct {
		Issuer    string `json:"iss"`
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Audience  string `json:"aud"`
		Subject   string `json:"sub"`
	}{
		Issuer:    apple.teamID,
		IssuedAt:  issuedAt,
		ExpiresAt: issuedAt + int64(appleClientSecretLifetime/time.Second),
		Audience:  appleIssuer,
		Subject:   apple.clientID,
	})
	if err != nil {
		return "", err
	}
	headerSegment := base64.RawURLEncoding.EncodeToString(header)
	claimsSegment := base64.RawURLEncoding.EncodeToString(claims)
	signingInput := headerSegment + "." + claimsSegment
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, apple.signingKey, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (apple *HostedApple) postForm(ctx context.Context, endpoint string, form url.Values) (int, []byte, error) {
	requestContext, cancel := context.WithTimeout(ctx, appleHTTPDeadline)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := apple.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAppleResponseBytes+1))
	if err != nil || len(body) > maxAppleResponseBytes {
		return response.StatusCode, nil, ErrAppleUnavailable
	}
	return response.StatusCode, body, nil
}

func parseAppleTokenResponse(body []byte) (AppleTokens, error) {
	object, err := appleTokenResponseObject(body)
	if err != nil {
		return AppleTokens{}, err
	}
	identityToken, err := exactJSONString(object, "id_token")
	if err != nil || !validAppleOpaqueString(identityToken, maxAppleCredentialBytes) {
		return AppleTokens{}, ErrAppleUnavailable
	}
	refreshToken, err := exactJSONString(object, "refresh_token")
	if err != nil || !validAppleOpaqueString(refreshToken, maxAppleCredentialBytes) {
		return AppleTokens{}, ErrAppleUnavailable
	}
	accessToken, err := exactJSONString(object, "access_token")
	if err != nil || !validAppleOpaqueString(accessToken, maxAppleCredentialBytes) {
		return AppleTokens{}, ErrAppleUnavailable
	}
	tokenType, err := exactJSONString(object, "token_type")
	if err != nil || tokenType != "Bearer" {
		return AppleTokens{}, ErrAppleUnavailable
	}
	expiresIn, err := exactIntegralNumericDate(object, "expires_in")
	if err != nil || expiresIn < 1 || expiresIn > 86400 {
		return AppleTokens{}, ErrAppleUnavailable
	}
	return AppleTokens{IdentityToken: identityToken, RefreshToken: refreshToken}, nil
}

func appleTokenResponseObject(body []byte) (map[string]json.RawMessage, error) {
	if len(body) == 0 || !utf8.Valid(body) {
		return nil, ErrAppleUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrAppleUnavailable
	}
	known := map[string]struct{}{
		"id_token": {}, "refresh_token": {}, "access_token": {}, "token_type": {}, "expires_in": {},
	}
	result := make(map[string]json.RawMessage, len(known))
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return nil, ErrAppleUnavailable
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, ErrAppleUnavailable
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrAppleUnavailable
		}
		if _, keep := known[key]; keep {
			result[key] = value
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, ErrAppleUnavailable
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, ErrAppleUnavailable
	}
	return result, nil
}

func validAppleRedirectURI(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 2048 || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.Hostname() != "" &&
		parsed.User == nil && parsed.Fragment == "" && parsed.Opaque == ""
}

func validAppleClientID(value string) bool {
	if len(value) == 0 || len(value) > 255 {
		return false
	}
	for _, segment := range strings.Split(value, ".") {
		if len(segment) == 0 || segment[0] == '-' || segment[len(segment)-1] == '-' {
			return false
		}
		for index := range len(segment) {
			character := segment[index]
			if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') &&
				(character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func validAppleOpaqueString(value string, maximum int) bool {
	return len(value) > 0 && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validAppleOpaqueBytes(value []byte, maximum int) bool {
	return len(value) > 0 && len(value) <= maximum && bytes.IndexByte(value, 0) < 0 && utf8.Valid(value)
}

func cloneAppleSigningKey(source *ecdsa.PrivateKey) (*ecdsa.PrivateKey, bool) {
	if source == nil || source.Curve != elliptic.P256() || source.D == nil || source.X == nil || source.Y == nil {
		return nil, false
	}
	curve := elliptic.P256()
	if source.D.Sign() <= 0 || source.D.Cmp(curve.Params().N) >= 0 || !curve.IsOnCurve(source.X, source.Y) {
		return nil, false
	}
	x, y := curve.ScalarBaseMult(source.D.Bytes())
	if x.Cmp(source.X) != 0 || y.Cmp(source.Y) != 0 {
		return nil, false
	}
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: new(big.Int).Set(x), Y: new(big.Int).Set(y)},
		D:         new(big.Int).Set(source.D),
	}, true
}
