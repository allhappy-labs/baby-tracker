package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"babytracker/server/internal/config"
)

const (
	challengeLifetime    = 5 * time.Minute
	maxIdentityToken     = 16 << 10
	maxAuthorizationCode = 4096
	maxInstallationID    = 128
)

type Service struct {
	verifier           IdentityVerifier
	repository         *Repository
	lookupIdentity     func(context.Context, string, KeyedDigest) (identityLookup, bool, error)
	origin, instanceID string
	identityKeys       DigestKeyring
	now                func() time.Time
}

func New(options Options) (*Service, error) {
	if options.Verifier == nil || options.Repository == nil || options.Now == nil || !validNonzeroUUID(options.InstanceID) || !validateKeyring(options.IdentityKeys) {
		return nil, errors.New("complete identity options are required")
	}
	origin, err := config.CanonicalOrigin(options.Origin, config.ModeHosted)
	if err != nil {
		return nil, errors.New("canonical HTTPS origin is required")
	}
	return &Service{verifier: options.Verifier, repository: options.Repository, lookupIdentity: options.Repository.lookupIdentity, origin: origin.String(),
		instanceID: options.InstanceID, identityKeys: options.IdentityKeys, now: options.Now}, nil
}

func (service *Service) Challenge(ctx context.Context) (Challenge, error) {
	if service == nil {
		return Challenge{}, ErrUnavailable
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return Challenge{}, ErrUnavailable
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(token))
	expiresAt := service.now().UTC().Add(challengeLifetime)
	if err := service.repository.CreateChallenge(ctx, digest, OriginBinding{Origin: service.origin, InstanceID: service.instanceID}, expiresAt); err != nil {
		return Challenge{}, err
	}
	return Challenge{Challenge: token, ServerInstanceID: service.instanceID, CanonicalOrigin: service.origin, ExpiresAt: expiresAt}, nil
}

func (service *Service) VerifyExchange(ctx context.Context, request ExchangeRequest) (VerifiedIdentity, error) {
	verified, err := service.VerifyExchangeWithProfile(ctx, request)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	return verified.Identity, nil
}

func (service *Service) VerifyExchangeWithProfile(ctx context.Context, request ExchangeRequest) (VerifiedExchange, error) {
	profile, err := validateExchangeRequest(request)
	if err != nil {
		return VerifiedExchange{}, err
	}
	nonce, err := AppleNonce(service.origin, service.instanceID, request.Challenge)
	if err != nil {
		return VerifiedExchange{}, ErrInvalidRequest
	}
	digest := sha256.Sum256([]byte(request.Challenge))
	if err := service.repository.ConsumeChallenge(ctx, digest, OriginBinding{Origin: service.origin, InstanceID: service.instanceID}, service.now().UTC()); err != nil {
		return VerifiedExchange{}, err
	}
	verified, err := service.verifier.Verify(ctx, request.IdentityToken, nonce)
	if err != nil {
		return VerifiedExchange{}, ErrInvalidAppleProof
	}
	if verified.Provider != "apple" || verified.Subject == "" || verified.Audience == "" {
		return VerifiedExchange{}, ErrInvalidAppleProof
	}

	match, matchedDigest, found, err := service.lookupIdentityCandidates(ctx, verified)
	if err != nil {
		return VerifiedExchange{}, err
	}
	if (!found || match.ProfileRequired) && profile == nil {
		return VerifiedExchange{}, ErrDisplayNameRequired
	}
	verified.IdentityKey = authorityDigest(matchedDigest)
	return VerifiedExchange{Identity: verified, Profile: profile}, nil
}

func (service *Service) lookupIdentityCandidates(ctx context.Context, verified VerifiedIdentity) (identityLookup, KeyedDigest, bool, error) {
	if service == nil || service.lookupIdentity == nil {
		return identityLookup{}, KeyedDigest{}, false, ErrUnavailable
	}
	keys := append([]DigestKey{service.identityKeys.Current}, service.identityKeys.Previous...)
	var match identityLookup
	var matchedDigest KeyedDigest
	found, ambiguous := false, false
	for _, key := range keys {
		candidate := identityDigest(key, verified.Provider, verified.Audience, verified.Subject)
		row, exists, err := service.lookupIdentity(ctx, verified.Provider, candidate)
		if err != nil {
			return identityLookup{}, KeyedDigest{}, false, err
		}
		if !exists {
			continue
		}
		if found {
			ambiguous = true
			continue
		}
		found, match, matchedDigest = true, row, candidate
	}
	if ambiguous {
		return identityLookup{}, KeyedDigest{}, false, ErrUnavailable
	}
	if !found {
		matchedDigest = identityDigest(service.identityKeys.Current, verified.Provider, verified.Audience, verified.Subject)
	}
	return match, matchedDigest, found, nil
}

func (service *Service) AuthorityKeyCandidates(verified VerifiedIdentity) ([]string, error) {
	if service == nil || verified.Provider != "apple" || verified.Audience == "" || verified.Subject == "" || !validateKeyring(service.identityKeys) {
		return nil, ErrInvalidAppleProof
	}
	keys := append([]DigestKey{service.identityKeys.Current}, service.identityKeys.Previous...)
	candidates := make([]string, 0, len(keys))
	for _, key := range keys {
		candidates = append(candidates, authorityDigest(identityDigest(key, verified.Provider, verified.Audience, verified.Subject)))
	}
	return candidates, nil
}

func validateExchangeRequest(request ExchangeRequest) (*VerifiedProfile, error) {
	if !validNonzeroUUID(request.CommandID) || len(request.IdentityToken) == 0 || len(request.IdentityToken) > maxIdentityToken ||
		len(request.AuthorizationCode) > maxAuthorizationCode || len(request.InstallationID) == 0 || len(request.InstallationID) > maxInstallationID ||
		strings.ContainsRune(request.InstallationID, 0) {
		return nil, ErrInvalidRequest
	}
	decoded, err := base64.RawURLEncoding.DecodeString(request.Challenge)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != request.Challenge {
		return nil, ErrInvalidRequest
	}
	if request.DeviceContinuity != nil {
		credential, decodeErr := base64.RawURLEncoding.DecodeString(request.DeviceContinuity.Credential)
		if !validNonzeroUUID(request.DeviceContinuity.DeviceID) || decodeErr != nil || len(credential) != 32 ||
			base64.RawURLEncoding.EncodeToString(credential) != request.DeviceContinuity.Credential {
			return nil, ErrInvalidRequest
		}
	}
	if request.Profile == nil {
		return nil, nil
	}
	name := strings.TrimSpace(request.Profile.DisplayName)
	if !request.Profile.Confirmed || name == "" || len(name) > 320 || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 80 {
		return nil, ErrInvalidRequest
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return nil, ErrInvalidRequest
		}
	}
	return &VerifiedProfile{DisplayName: name, Confirmed: true}, nil
}

func identityDigest(key DigestKey, provider, audience, subject string) KeyedDigest {
	digest := hmac.New(sha256.New, key.Secret[:])
	digest.Write([]byte(provider))
	digest.Write([]byte{0})
	digest.Write([]byte(audience))
	digest.Write([]byte{0})
	digest.Write([]byte(subject))
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return KeyedDigest{KeyID: key.ID, Sum: sum}
}

func authorityDigest(digest KeyedDigest) string {
	return "hmac-sha256:" + digest.KeyID + ":" + hex.EncodeToString(digest.Sum[:])
}
