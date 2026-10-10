package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
)

const (
	defaultAccessTTL = 10 * time.Minute
	// Self-hosted phones rotate refresh tokens during normal use. A short
	// absolute age forced a fresh Apple prompt every day even while the
	// device remained active. One sign-in now lasts up to one year, whether
	// or not the device is used during that time.
	defaultSelfHostedAge = 365 * 24 * time.Hour
	defaultHostedAge     = 30 * 24 * time.Hour
	minimumAbsoluteAge   = 5 * time.Minute
)

func New(options Options) (*Service, error) {
	if options.Identity == nil || options.Verifier == nil || options.Authority == nil || options.Now == nil ||
		!validUUID(options.InstanceID) || !validAuthKeyring(options.TokenKeys) || !validAuthKeyring(options.DeviceKeys) || allZero(options.ActorIDKey[:]) {
		return nil, errors.New("complete auth options are required")
	}
	if options.Authority.InstanceID() != options.InstanceID {
		return nil, errors.New("auth instance ID must be the authority's")
	}
	if !slices.Equal(authority.TokenKeyCommitment(options.TokenKeys), options.Authority.TokenKeyCommitment()) {
		return nil, errors.New("auth token keyring must be the authority's")
	}
	if options.Mode != config.ModeSelfHosted && options.Mode != config.ModeHosted {
		return nil, errors.New("auth mode must be hosted or self-hosted")
	}
	canonical, err := config.CanonicalOrigin(options.Origin, options.Mode)
	if err != nil || canonical.String() != options.Origin {
		return nil, errors.New("canonical HTTPS origin is required")
	}
	if options.Mode == config.ModeHosted && options.Apple == nil {
		return nil, errors.New("hosted auth requires Apple code exchange")
	}
	accessTTL := options.AccessTTL
	if accessTTL == 0 {
		accessTTL = defaultAccessTTL
	}
	absoluteAge := options.AbsoluteAge
	if absoluteAge == 0 {
		if options.Mode == config.ModeSelfHosted {
			absoluteAge = defaultSelfHostedAge
		} else {
			absoluteAge = defaultHostedAge
		}
	}
	if accessTTL <= 0 || absoluteAge < minimumAbsoluteAge || accessTTL > absoluteAge ||
		(options.Mode == config.ModeSelfHosted && absoluteAge > defaultSelfHostedAge) {
		return nil, errors.New("invalid auth session lifetimes")
	}
	refreshTTL := options.RefreshTTL
	if refreshTTL == 0 {
		if options.Mode == config.ModeSelfHosted {
			refreshTTL = defaultSelfHostedAge
		} else {
			refreshTTL = 30 * 24 * time.Hour
		}
	}
	if refreshTTL <= 0 {
		return nil, errors.New("invalid refresh lifetime")
	}
	return &Service{identity: options.Identity, verifier: options.Verifier, authority: options.Authority, apple: options.Apple, mode: options.Mode,
		origin: options.Origin, instanceID: options.InstanceID, tokenKeys: cloneKeyring(options.TokenKeys), deviceKeys: cloneKeyring(options.DeviceKeys),
		actorIDKey: options.ActorIDKey, now: options.Now, accessTTL: accessTTL, refreshTTL: refreshTTL, absoluteAge: absoluteAge}, nil
}

// Exchange signs in with a verified Apple proof. Everything that needs the
// network or a separate read happens before the sign-in transaction: the
// challenge is consumed and the identity token verified, the identity
// resolved, (hosted) the authorization code exchanged and the returned
// identity verified, and an existing device's continuity proof checked.
// Authority.SignIn then registers or restores the account and device and
// issues the account-scope lineage root session in one transaction,
// refusing a resolution or device that changed meanwhile.
func (service *Service) Exchange(ctx context.Context, request identity.ExchangeRequest) (SessionBundle, error) {
	if service == nil || service.identity == nil || service.authority == nil {
		return SessionBundle{}, ErrUnavailable
	}
	verified, err := service.identity.VerifyExchangeWithProfile(ctx, request)
	if err != nil {
		return SessionBundle{}, err
	}
	accountID, generation, err := service.proposedActor(verified)
	if err != nil {
		return SessionBundle{}, err
	}
	if service.mode == config.ModeHosted {
		if err := service.exchangeAppleCode(ctx, request, verified.Identity); err != nil {
			return SessionBundle{}, err
		}
	}
	var device authority.DeviceBinding
	deviceCredential := ""
	if request.DeviceContinuity == nil {
		device, deviceCredential, err = service.newDevice(accountID)
	} else {
		device, err = service.resolveExistingDevice(ctx, accountID, request.DeviceContinuity)
	}
	if err != nil {
		return SessionBundle{}, err
	}
	sessionID, err := newUUID()
	if err != nil {
		return SessionBundle{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	accessToken, err := newToken()
	if err != nil {
		return SessionBundle{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	refreshToken, err := newToken()
	if err != nil {
		return SessionBundle{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	now := service.now().UTC().Truncate(time.Microsecond)
	deadline := now.Add(service.absoluteAge)
	refreshExpires := minDeadline(now.Add(service.refreshTTL), deadline)
	accessExpires := minDeadline(now.Add(service.accessTTL), refreshExpires)
	in := authority.SignIn{CommandID: request.CommandID, At: now, Candidates: verified.Candidates, Audience: verified.Identity.Audience,
		Subject: verified.Identity.Subject, ProposedAccountID: accountID, ProposedGeneration: generation, Device: device,
		RegisterDevice: deviceCredential != "", SessionID: sessionID, TokenKeyID: service.tokenKeys.Current.ID,
		AccessDigest: tokenDigest(service.tokenKeys.Current, "access", accessToken).Sum, RefreshDigest: tokenDigest(service.tokenKeys.Current, "refresh", refreshToken).Sum,
		AccessExpiresAt: accessExpires, RefreshExpiresAt: refreshExpires, DeadlineAt: deadline, CanonicalOrigin: service.origin,
		Mode: authority.Mode(service.mode)}
	if verified.Profile != nil {
		in.DisplayName, in.ProfileConfirmed = stringPointer(verified.Profile.DisplayName), verified.Profile.Confirmed
	}
	result, err := service.authority.SignIn(ctx, in)
	if err != nil {
		return SessionBundle{}, err
	}
	return SessionBundle{AccessToken: accessToken, RefreshToken: refreshToken, DeviceID: device.DeviceID, DeviceCredential: deviceCredential,
		RecoveryEpoch: service.authority.RecoveryEpoch(), ExpiresAt: accessExpires, ReauthenticateAt: deadline, Account: AccountView(result.Account),
		FamilySlot: familySlot(result.FamilySlot)}, nil
}

// proposedActor is the account a verified identity signs into: the live
// account it resolved to, or for an absent or deleted identity a new actor
// at the next lifecycle generation.
func (service *Service) proposedActor(verified identity.VerifiedExchange) (string, uint64, error) {
	resolution := verified.Authority
	switch resolution.Status {
	case "absent", "deleted":
		accountID, err := ActorID(service.actorIDKey, service.instanceID, verified.Identity, resolution.NextActorGeneration)
		if err != nil {
			return "", 0, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		return accountID, resolution.NextActorGeneration, nil
	case "active", "identity_unavailable":
		return resolution.AccountID, resolution.LifecycleGeneration, nil
	default:
		return "", 0, authority.ErrAuthorizationQuarantine
	}
}

// exchangeAppleCode exchanges a hosted sign-in's authorization code and
// verifies that the returned identity token is the same Apple identity under
// the same nonce. The returned Apple refresh token is not kept (Phase 1:
// account deletion reports manual Apple revocation).
func (service *Service) exchangeAppleCode(ctx context.Context, request identity.ExchangeRequest, expected identity.VerifiedIdentity) error {
	if service.apple == nil {
		return ErrHostedAdapterUnavailable
	}
	if request.AuthorizationCode == "" {
		return identity.ErrInvalidRequest
	}
	nonce, err := identity.AppleNonce(service.origin, service.instanceID, request.Challenge)
	if err != nil {
		return identity.ErrInvalidRequest
	}
	tokens, err := service.apple.ExchangeCode(ctx, identity.AppleCodeExchangeRequest{AuthorizationCode: request.AuthorizationCode,
		ExpectedNonce: nonce, ExpectedIdentity: expected})
	if err != nil {
		return err
	}
	returned, err := service.verifier.Verify(ctx, tokens.IdentityToken, nonce)
	if err != nil || !sameAppleIdentity(returned, expected) {
		return identity.ErrInvalidAppleProof
	}
	return nil
}

func sameAppleIdentity(left, right identity.VerifiedIdentity) bool {
	return subtle.ConstantTimeCompare([]byte(left.Provider), []byte(right.Provider)) == 1 &&
		subtle.ConstantTimeCompare([]byte(left.Subject), []byte(right.Subject)) == 1 &&
		subtle.ConstantTimeCompare([]byte(left.Audience), []byte(right.Audience)) == 1
}

func familySlot(slot *authority.FamilySlot) *FamilySlot {
	if slot == nil {
		return nil
	}
	return &FamilySlot{FamilyID: slot.FamilyID, State: slot.State}
}

func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

func validAuthKeyring(keyring identity.DigestKeyring) bool {
	keys := append([]identity.DigestKey{keyring.Current}, keyring.Previous...)
	if len(keys) == 0 || len(keys) > 8 {
		return false
	}
	ids := make(map[string]struct{}, len(keys))
	secrets := make(map[[32]byte]struct{}, len(keys))
	for _, key := range keys {
		if !digestKeyIDPattern.MatchString(key.ID) || allZero(key.Secret[:]) {
			return false
		}
		if _, exists := ids[key.ID]; exists {
			return false
		}
		if _, exists := secrets[key.Secret]; exists {
			return false
		}
		ids[key.ID] = struct{}{}
		secrets[key.Secret] = struct{}{}
	}
	return true
}

func cloneKeyring(keyring identity.DigestKeyring) identity.DigestKeyring {
	return identity.DigestKeyring{Current: keyring.Current, Previous: append([]identity.DigestKey(nil), keyring.Previous...)}
}

func stringPointer(value string) *string { return &value }
