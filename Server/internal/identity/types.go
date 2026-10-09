package identity

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"
)

type Challenge struct {
	Challenge        string
	ServerInstanceID string
	CanonicalOrigin  string
	ExpiresAt        time.Time
}

type ProfileInput struct {
	DisplayName string
	Confirmed   bool
}

type VerifiedProfile struct {
	DisplayName string
	Confirmed   bool
}

type ExchangeRequest struct {
	CommandID         string
	Challenge         string
	IdentityToken     string
	AuthorizationCode string
	InstallationID    string
	Profile           *ProfileInput
	DeviceContinuity  *DeviceContinuityClaim
}

type DeviceContinuityClaim struct {
	DeviceID   string
	Credential string
}

type VerifiedIdentity struct {
	Provider, Subject, Audience, IdentityKey string
	IssuedAt, ExpiresAt                      time.Time
}

type VerifiedExchange struct {
	Identity VerifiedIdentity
	Profile  *VerifiedProfile
}

type DigestKey struct {
	ID     string
	Secret [32]byte
}

type DigestKeyring struct {
	Current  DigestKey
	Previous []DigestKey
}

type KeyedDigest struct {
	KeyID string
	Sum   [32]byte
}

type IdentityVerifier interface {
	Verify(context.Context, string, string) (VerifiedIdentity, error)
}

type IdentityVerifierFunc func(context.Context, string, string) (VerifiedIdentity, error)

func (function IdentityVerifierFunc) Verify(ctx context.Context, token, nonce string) (VerifiedIdentity, error) {
	return function(ctx, token, nonce)
}

type OriginBinding struct {
	Origin, InstanceID string
}

type Options struct {
	Verifier     IdentityVerifier
	Repository   *Repository
	Origin       string
	InstanceID   string
	IdentityKeys DigestKeyring
	Now          func() time.Time
}

type AppleOptions struct {
	Audience string
	HTTP     *http.Client
	Now      func() time.Time
}

var keyIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

var (
	ErrInvalidAppleProof         = codedError("invalid_apple_proof")
	ErrInvalidOrExpiredChallenge = codedError("invalid_or_expired_challenge")
	ErrDisplayNameRequired       = codedError("display_name_required")
	ErrInvalidRequest            = codedError("invalid_request")
	ErrUnavailable               = codedError("service_unavailable")
)

type codedError string

func (e codedError) Error() string { return string(e) }

func Code(err error) string {
	if err == nil {
		return ""
	}
	var coded codedError
	if errors.As(err, &coded) {
		return string(coded)
	}
	return "service_unavailable"
}

func validDigestKey(key DigestKey) bool {
	if !keyIDPattern.MatchString(key.ID) {
		return false
	}
	var zero [32]byte
	return key.Secret != zero
}

func validateKeyring(keyring DigestKeyring) bool {
	if !validDigestKey(keyring.Current) || len(keyring.Previous) > 7 {
		return false
	}
	seen := map[string]struct{}{keyring.Current.ID: {}}
	seenSecrets := map[[32]byte]struct{}{keyring.Current.Secret: {}}
	for _, key := range keyring.Previous {
		if !validDigestKey(key) {
			return false
		}
		if _, exists := seen[key.ID]; exists {
			return false
		}
		if _, exists := seenSecrets[key.Secret]; exists {
			return false
		}
		seen[key.ID] = struct{}{}
		seenSecrets[key.Secret] = struct{}{}
	}
	return true
}
