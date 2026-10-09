package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
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

type provisionalIdentity struct {
	resolution security.IdentityAuthorityResolution
	head       security.Head
	epoch      uint64
	accountID  string
	generation uint64
}

type frozenExchange struct {
	commandID         string
	commandAt         time.Time
	verified          identity.VerifiedExchange
	candidates        []string
	provisional       provisionalIdentity
	device            DeviceBinding
	deviceCredential  string
	sessionID         string
	accessToken       string
	refreshToken      string
	accessExpiresAt   time.Time
	absoluteExpiresAt time.Time
	stageBytes        []byte
	stageDigest       [32]byte
	payload           security.IdentitySessionIssuedPayload
	commandHash       [32]byte
}

func New(options Options) (*Service, error) {
	if options.Identity == nil || options.Verifier == nil || options.Security == nil || options.Repository == nil || options.Now == nil ||
		!validUUID(options.InstanceID) || !validAuthKeyring(options.TokenKeys) || !validAuthKeyring(options.DeviceKeys) ||
		!validAuthKeyring(options.NotificationKeys) || allZero(options.ActorIDKey[:]) {
		return nil, errors.New("complete auth options are required")
	}
	if options.Mode != config.ModeSelfHosted && options.Mode != config.ModeHosted {
		return nil, errors.New("auth mode must be hosted or self-hosted")
	}
	canonical, err := config.CanonicalOrigin(options.Origin, options.Mode)
	if err != nil || canonical.String() != options.Origin {
		return nil, errors.New("canonical HTTPS origin is required")
	}
	if options.Mode == config.ModeHosted && (options.Apple == nil || options.Credentials == nil) {
		return nil, errors.New("hosted auth requires Apple exchange and credential rotation")
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
	return &Service{identity: options.Identity, verifier: options.Verifier, security: options.Security, repository: options.Repository,
		apple: options.Apple, credentials: options.Credentials, mode: options.Mode, origin: options.Origin, instanceID: options.InstanceID,
		tokenKeys: cloneKeyring(options.TokenKeys), deviceKeys: cloneKeyring(options.DeviceKeys), notificationKeys: cloneKeyring(options.NotificationKeys),
		actorIDKey: options.ActorIDKey, now: options.Now, accessTTL: accessTTL, refreshTTL: refreshTTL, absoluteAge: absoluteAge}, nil
}

func (service *Service) Exchange(ctx context.Context, request identity.ExchangeRequest) (SessionBundle, error) {
	if service == nil || service.identity == nil || service.security == nil || service.repository == nil {
		return SessionBundle{}, ErrUnavailable
	}
	verified, err := service.identity.VerifyExchangeWithProfile(ctx, request)
	if err != nil {
		return SessionBundle{}, err
	}
	candidates, err := service.identity.AuthorityKeyCandidates(verified.Identity)
	if err != nil {
		return SessionBundle{}, err
	}
	provisional, err := service.resolveProvisionalIdentity(ctx, verified.Identity, candidates)
	if err != nil {
		return SessionBundle{}, err
	}
	if service.mode == config.ModeHosted {
		return service.exchangeHosted(ctx, request, verified, candidates, provisional)
	}
	frozen, err := service.freezeSelfHostedExchange(ctx, request, verified, candidates, provisional)
	if err != nil {
		return SessionBundle{}, err
	}
	if err := service.preparePreliminary(ctx, &frozen); err != nil {
		return SessionBundle{}, err
	}
	if _, err := service.security.ExecutePrepared(ctx, func(callbackContext context.Context, snapshot security.AuthoritySnapshot) (security.PreparedCommand, error) {
		if snapshot.Head != frozen.provisional.head || snapshot.RecoveryEpoch != frozen.provisional.epoch {
			return security.PreparedCommand{}, authSecurityError("security_busy")
		}
		resolution, err := snapshot.View.ResolveIdentityAuthority(frozen.candidates)
		if err != nil || resolution != frozen.provisional.resolution {
			return security.PreparedCommand{}, authSecurityError("security_busy")
		}
		actor, err := snapshot.ResolveIdentityActor(frozen.candidates, frozen.provisional.accountID, frozen.provisional.generation)
		if err != nil {
			return security.PreparedCommand{}, err
		}
		device := frozen.device
		if request.DeviceContinuity != nil {
			proofContext := context.WithValue(callbackContext, verifiedDeviceContextKey{}, verifiedDeviceContext{accountID: frozen.provisional.accountID, identity: frozen.verified.Identity})
			device, _, err = service.ResolveDevice(proofContext, frozen.verified.Identity, request.DeviceContinuity)
			if err != nil || device != frozen.device {
				return security.PreparedCommand{}, ErrDeviceContinuityInvalid
			}
		}
		prepared, err := service.prepareIdentitySession(snapshot, actor, frozen, request.CommandID)
		if err != nil {
			return security.PreparedCommand{}, err
		}
		hash, err := snapshot.CommandHash(prepared)
		if err != nil || subtle.ConstantTimeCompare(hash[:], frozen.commandHash[:]) != 1 {
			return security.PreparedCommand{}, authSecurityError("security_busy")
		}
		if err := service.repository.StageAuthMaterial(callbackContext, AuthMaterialStage{CommandID: request.CommandID, CommandHash: hash,
			ProjectionMaterialDigest: frozen.stageDigest, Material: frozen.stageBytes, Now: frozen.commandAt}); err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		return prepared, nil
	}); err != nil {
		return SessionBundle{}, err
	}
	account, err := service.accountView(ctx, frozen)
	if err != nil {
		return SessionBundle{}, err
	}
	bundle := SessionBundle{AccessToken: frozen.accessToken, RefreshToken: frozen.refreshToken, DeviceID: frozen.device.DeviceID,
		DeviceCredential: frozen.deviceCredential, RecoveryEpoch: frozen.provisional.epoch, ExpiresAt: frozen.accessExpiresAt,
		ReauthenticateAt: frozen.absoluteExpiresAt, Account: account}
	bundle.FamilySlot, err = service.currentFamilySlot(ctx, frozen.provisional.accountID)
	if err != nil {
		return SessionBundle{}, err
	}
	return bundle, nil
}

var (
	errHostedAuthorityDrift = errors.New("hosted authority head drift")
	errHostedActorChanged   = errors.New("hosted provisional actor changed")
)

type hostedCredentialProjection struct {
	binding identity.CredentialBinding
	ref     identity.CredentialRef
}

func (service *Service) exchangeHosted(ctx context.Context, request identity.ExchangeRequest, verified identity.VerifiedExchange,
	candidates []string, provisional provisionalIdentity) (SessionBundle, error) {
	if service.apple == nil || service.credentials == nil {
		return SessionBundle{}, ErrHostedAdapterUnavailable
	}
	if request.AuthorizationCode == "" {
		return SessionBundle{}, identity.ErrInvalidRequest
	}
	binding, err := identity.NewAppleCredentialBinding(provisional.accountID, service.instanceID)
	if err != nil {
		return SessionBundle{}, ErrUnavailable
	}
	promoted, err := service.rotateHostedCredential(ctx, request, verified.Identity, binding)
	if err != nil {
		return SessionBundle{}, err
	}
	credential := hostedCredentialProjection{binding: binding, ref: promoted.Ref}
	frozen, err := service.freezeSelfHostedExchange(ctx, request, verified, candidates, provisional)
	if err != nil {
		_ = service.credentials.Abandon(ctx, binding, promoted.OperationID)
		return SessionBundle{}, err
	}
	if err := service.rebuildFrozenExchange(&frozen, &credential); err != nil {
		_ = service.credentials.Abandon(ctx, binding, promoted.OperationID)
		return SessionBundle{}, err
	}

	for attempt := 0; attempt < 8; attempt++ {
		if preliminaryErr := service.preparePreliminary(ctx, &frozen); preliminaryErr != nil {
			refreshed, refreshErr := service.refreshHostedFrozenExchange(ctx, verified.Identity, candidates, &frozen, credential)
			if refreshErr != nil {
				_ = service.credentials.Abandon(ctx, binding, promoted.OperationID)
				if errors.Is(refreshErr, errHostedActorChanged) {
					return SessionBundle{}, authSecurityError("security_busy")
				}
				return SessionBundle{}, refreshErr
			}
			if refreshed {
				continue
			}
			_ = service.credentials.Abandon(ctx, binding, promoted.OperationID)
			return SessionBundle{}, preliminaryErr
		}
		promotion, prepareErr := service.credentials.PreparePromotionCAS(ctx, binding, promoted, frozen.commandHash)
		if prepareErr != nil {
			_ = service.credentials.Abandon(ctx, binding, promoted.OperationID)
			return SessionBundle{}, prepareErr
		}

		var adoption identity.AdoptionCapability
		confirmed := false
		result, executeErr := service.security.ExecutePrepared(ctx, func(callbackContext context.Context, snapshot security.AuthoritySnapshot) (security.PreparedCommand, error) {
			resolution, resolveErr := snapshot.View.ResolveIdentityAuthority(frozen.candidates)
			if resolveErr != nil || resolution != frozen.provisional.resolution {
				return security.PreparedCommand{}, errHostedActorChanged
			}
			if snapshot.Head != frozen.provisional.head || snapshot.RecoveryEpoch != frozen.provisional.epoch {
				return security.PreparedCommand{}, errHostedAuthorityDrift
			}
			actor, resolveErr := snapshot.ResolveIdentityActor(frozen.candidates, frozen.provisional.accountID, frozen.provisional.generation)
			if resolveErr != nil {
				return security.PreparedCommand{}, errHostedActorChanged
			}
			device := frozen.device
			if request.DeviceContinuity != nil {
				proofContext := context.WithValue(callbackContext, verifiedDeviceContextKey{}, verifiedDeviceContext{accountID: frozen.provisional.accountID, identity: frozen.verified.Identity})
				device, _, resolveErr = service.ResolveDevice(proofContext, frozen.verified.Identity, request.DeviceContinuity)
				if resolveErr != nil || device != frozen.device {
					return security.PreparedCommand{}, errHostedActorChanged
				}
			}
			prepared, prepareErr := service.prepareIdentitySession(snapshot, actor, frozen, request.CommandID)
			if prepareErr != nil {
				return security.PreparedCommand{}, prepareErr
			}
			hash, hashErr := snapshot.CommandHash(prepared)
			if hashErr != nil || subtle.ConstantTimeCompare(hash[:], frozen.commandHash[:]) != 1 ||
				subtle.ConstantTimeCompare(hash[:], promotion.ExpectedCommandHash[:]) != 1 {
				return security.PreparedCommand{}, errHostedAuthorityDrift
			}
			adoption, prepareErr = service.credentials.ConfirmPromotion(callbackContext, promotion)
			if prepareErr != nil {
				return security.PreparedCommand{}, prepareErr
			}
			confirmed = true
			if prepareErr = service.repository.StageAuthMaterial(callbackContext, AuthMaterialStage{CommandID: request.CommandID, CommandHash: hash,
				ProjectionMaterialDigest: frozen.stageDigest, Material: frozen.stageBytes, Now: frozen.commandAt}); prepareErr != nil {
				return security.PreparedCommand{}, ErrUnavailable
			}
			return prepared, nil
		})
		if errors.Is(executeErr, errHostedAuthorityDrift) && !confirmed {
			refreshed, refreshErr := service.refreshHostedFrozenExchange(ctx, verified.Identity, candidates, &frozen, credential)
			if refreshErr != nil || !refreshed {
				_ = service.credentials.Abandon(ctx, binding, promoted.OperationID)
				if refreshErr != nil && !errors.Is(refreshErr, errHostedActorChanged) {
					return SessionBundle{}, refreshErr
				}
				return SessionBundle{}, authSecurityError("security_busy")
			}
			continue
		}
		if executeErr != nil {
			handled, _, reconcileErr := service.reconcileHostedAdoption(ctx, binding, promoted.CommandID, promotion.ExpectedCommandHash)
			if reconcileErr != nil {
				return SessionBundle{}, executeErr
			}
			if !handled {
				if confirmed && len(adoption.Opaque) != 0 {
					if abandonErr := service.credentials.AbandonAdoption(ctx, binding, adoption); abandonErr != nil {
						return SessionBundle{}, ErrUnavailable
					}
				} else if abandonErr := service.credentials.Abandon(ctx, binding, promoted.OperationID); abandonErr != nil {
					return SessionBundle{}, ErrUnavailable
				}
			}
			if errors.Is(executeErr, errHostedActorChanged) || errors.Is(executeErr, errHostedAuthorityDrift) {
				return SessionBundle{}, authSecurityError("security_busy")
			}
			return SessionBundle{}, executeErr
		}
		if result.CommandID != request.CommandID || result.Revision == 0 {
			if abandonErr := service.credentials.AbandonAdoption(ctx, binding, adoption); abandonErr != nil {
				return SessionBundle{}, ErrUnavailable
			}
			return SessionBundle{}, ErrUnavailable
		}
		if err := service.credentials.FinishAdoption(ctx, binding, adoption); err != nil {
			return SessionBundle{}, err
		}
		account, err := service.accountView(ctx, frozen)
		if err != nil {
			return SessionBundle{}, err
		}
		bundle := SessionBundle{AccessToken: frozen.accessToken, RefreshToken: frozen.refreshToken, DeviceID: frozen.device.DeviceID,
			DeviceCredential: frozen.deviceCredential, RecoveryEpoch: frozen.provisional.epoch, ExpiresAt: frozen.accessExpiresAt,
			ReauthenticateAt: frozen.absoluteExpiresAt, Account: account}
		bundle.FamilySlot, err = service.currentFamilySlot(ctx, frozen.provisional.accountID)
		if err != nil {
			return SessionBundle{}, err
		}
		return bundle, nil
	}
	_ = service.credentials.Abandon(ctx, binding, promoted.OperationID)
	return SessionBundle{}, authSecurityError("security_busy")
}

func (service *Service) currentFamilySlot(ctx context.Context, accountID string) (*FamilySlot, error) {
	var result *FamilySlot
	err := service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		_, _, result = authorityFamilySlot(snapshot.View, accountID)
		return nil
	})
	if err != nil {
		return nil, authSecurityError(security.Code(err))
	}
	return result, nil
}

func (service *Service) refreshHostedFrozenExchange(ctx context.Context, verified identity.VerifiedIdentity, candidates []string,
	frozen *frozenExchange, credential hostedCredentialProjection) (bool, error) {
	updated, err := service.resolveProvisionalIdentity(ctx, verified, candidates)
	if err != nil {
		return false, err
	}
	if updated.resolution != frozen.provisional.resolution || updated.accountID != frozen.provisional.accountID ||
		updated.generation != frozen.provisional.generation {
		return false, errHostedActorChanged
	}
	if updated.head == frozen.provisional.head && updated.epoch == frozen.provisional.epoch {
		return false, nil
	}
	frozen.provisional = updated
	if err := service.rebuildFrozenExchange(frozen, &credential); err != nil {
		return false, err
	}
	return true, nil
}

func (service *Service) rotateHostedCredential(ctx context.Context, request identity.ExchangeRequest, expected identity.VerifiedIdentity,
	binding identity.CredentialBinding) (identity.CredentialPromotion, error) {
	resume, err := service.credentials.Resume(ctx, binding)
	if err != nil {
		return identity.CredentialPromotion{}, err
	}
	operationID := ""
	switch resume.State.Phase {
	case "absent", "ready":
		receiptExists, receiptErr := service.hostedCommandReceiptExists(ctx, binding.AccountID, request.CommandID)
		if receiptErr != nil {
			return identity.CredentialPromotion{}, receiptErr
		}
		if receiptExists {
			return identity.CredentialPromotion{}, authSecurityError("command_id_reused")
		}
		operationID, err = newUUID()
		if err != nil {
			return identity.CredentialPromotion{}, ErrUnavailable
		}
	case "reserved", "candidate", "retiring", "promoted":
		if resume.State.CommandID != request.CommandID {
			return identity.CredentialPromotion{}, identity.ErrCredentialRotationBusy
		}
		operationID = resume.State.OperationID
	case "adopting":
		if resume.State.CommandID != request.CommandID || resume.Adoption == nil {
			return identity.CredentialPromotion{}, identity.ErrCredentialRotationBusy
		}
		_, _, reconcileErr := service.reconcileHostedAdoption(ctx, binding, request.CommandID, resume.ExpectedCommandHash)
		if reconcileErr != nil {
			return identity.CredentialPromotion{}, reconcileErr
		}
		return identity.CredentialPromotion{}, ErrUnavailable
	default:
		return identity.CredentialPromotion{}, identity.ErrCredentialRotationBusy
	}
	nonce, err := identity.AppleNonce(service.origin, service.instanceID, request.Challenge)
	if err != nil {
		return identity.CredentialPromotion{}, identity.ErrInvalidRequest
	}
	return service.credentials.RotateForSession(ctx, binding, operationID, request.CommandID, func(exchangeContext context.Context) (identity.AppleTokens, error) {
		tokens, exchangeErr := service.apple.ExchangeCode(exchangeContext, identity.AppleCodeExchangeRequest{AuthorizationCode: request.AuthorizationCode,
			ExpectedNonce: nonce, ExpectedIdentity: expected})
		if exchangeErr != nil {
			return identity.AppleTokens{}, exchangeErr
		}
		returned, verifyErr := service.verifier.Verify(exchangeContext, tokens.IdentityToken, nonce)
		if verifyErr != nil || !sameAppleIdentity(returned, expected) {
			return identity.AppleTokens{}, identity.ErrInvalidAppleProof
		}
		return tokens, nil
	})
}

func (service *Service) hostedCommandReceiptExists(ctx context.Context, accountID, commandID string) (bool, error) {
	exists := false
	err := service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		_, exists = snapshot.View.Receipt(accountID, commandID)
		return nil
	})
	return exists, err
}

// reconcileHostedAdoption resolves an ambiguous promotion result only after the
// authority fence has been released. It returns handled when the credential was
// in adopting and therefore must not be abandoned through the generic rotation
// path. An unreadable authority receipt is intentionally left adopting for a
// later restart/reconciler pass.
func (service *Service) reconcileHostedAdoption(ctx context.Context, binding identity.CredentialBinding, commandID string,
	expectedHash [32]byte) (handled, committed bool, err error) {
	resume, err := service.credentials.Resume(ctx, binding)
	if err != nil {
		return false, false, err
	}
	if resume.State.Phase != "adopting" {
		return false, false, nil
	}
	if resume.State.CommandID != commandID || resume.ExpectedCommandHash != expectedHash || resume.Adoption == nil {
		return true, false, identity.ErrCredentialConflict
	}
	receiptPresent := false
	receiptMatches := false
	err = service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		receipt, exists := snapshot.View.Receipt(binding.AccountID, commandID)
		receiptPresent = exists
		if exists {
			receiptMatches = subtle.ConstantTimeCompare(receipt.CommandHash[:], expectedHash[:]) == 1
		}
		return nil
	})
	if err != nil {
		return true, false, err
	}
	if receiptPresent {
		if !receiptMatches {
			return true, false, identity.ErrCredentialConflict
		}
		if err := service.credentials.FinishAdoption(ctx, binding, *resume.Adoption); err != nil {
			return true, false, err
		}
		return true, true, nil
	}
	if err := service.credentials.AbandonAdoption(ctx, binding, *resume.Adoption); err != nil {
		return true, false, err
	}
	return true, false, nil
}

func sameAppleIdentity(left, right identity.VerifiedIdentity) bool {
	return subtle.ConstantTimeCompare([]byte(left.Provider), []byte(right.Provider)) == 1 &&
		subtle.ConstantTimeCompare([]byte(left.Subject), []byte(right.Subject)) == 1 &&
		subtle.ConstantTimeCompare([]byte(left.Audience), []byte(right.Audience)) == 1
}

func (service *Service) resolveProvisionalIdentity(ctx context.Context, verified identity.VerifiedIdentity, candidates []string) (provisionalIdentity, error) {
	var result provisionalIdentity
	err := service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		resolution, err := snapshot.View.ResolveIdentityAuthority(candidates)
		if err != nil {
			return err
		}
		result = provisionalIdentity{resolution: resolution, head: snapshot.Head, epoch: snapshot.RecoveryEpoch}
		switch resolution.Status {
		case "absent", "deleted":
			result.accountID, err = ActorID(service.actorIDKey, service.instanceID, verified, resolution.NextActorGeneration)
			result.generation = resolution.NextActorGeneration
		case "active", "identity_unavailable":
			result.accountID, result.generation = resolution.AccountID, resolution.LifecycleGeneration
		default:
			return authSecurityError("authorization_quarantine")
		}
		return err
	})
	return result, err
}

func (service *Service) freezeSelfHostedExchange(ctx context.Context, request identity.ExchangeRequest, verified identity.VerifiedExchange,
	candidates []string, provisional provisionalIdentity) (frozenExchange, error) {
	now := service.now().UTC().Truncate(time.Microsecond)
	sessionID, err := newUUID()
	if err != nil {
		return frozenExchange{}, ErrUnavailable
	}
	accessToken, err := newToken()
	if err != nil {
		return frozenExchange{}, ErrUnavailable
	}
	refreshToken, err := newToken()
	if err != nil {
		return frozenExchange{}, ErrUnavailable
	}
	var device DeviceBinding
	deviceCredential := ""
	if request.DeviceContinuity == nil {
		device, deviceCredential, err = service.newDevice(provisional.accountID)
	} else {
		device, err = service.resolveExistingDevice(ctx, provisional.accountID, request.DeviceContinuity)
	}
	if err != nil {
		return frozenExchange{}, err
	}
	accessExpires := now.Add(service.accessTTL)
	absoluteExpires := now.Add(service.absoluteAge)
	frozen := frozenExchange{commandID: request.CommandID, commandAt: now, verified: verified, candidates: append([]string(nil), candidates...), provisional: provisional,
		device: device, deviceCredential: deviceCredential, sessionID: sessionID, accessToken: accessToken, refreshToken: refreshToken,
		accessExpiresAt: accessExpires, absoluteExpiresAt: absoluteExpires}
	if err := service.rebuildFrozenExchange(&frozen, nil); err != nil {
		return frozenExchange{}, err
	}
	return frozen, nil
}

func (service *Service) rebuildFrozenExchange(frozen *frozenExchange, credential *hostedCredentialProjection) error {
	if frozen == nil {
		return ErrUnavailable
	}
	refreshExpires := frozen.commandAt.Add(service.refreshTTL)
	if refreshExpires.After(frozen.absoluteExpiresAt) {
		refreshExpires = frozen.absoluteExpiresAt
	}
	accessExpires := frozen.accessExpiresAt
	if accessExpires.After(refreshExpires) {
		accessExpires = refreshExpires
	}
	frozen.accessExpiresAt = accessExpires
	accessDigest := tokenDigest(service.tokenKeys.Current, "access", frozen.accessToken)
	refreshDigest := tokenDigest(service.tokenKeys.Current, "refresh", frozen.refreshToken)
	identityKey := frozen.candidates[0]
	registerAccount := frozen.provisional.resolution.Status == "absent" || frozen.provisional.resolution.Status == "deleted"
	if !registerAccount {
		identityKey = frozen.provisional.resolution.IdentityKey
	}
	identityKeyID, identityDigest, ok := splitAuthorityDigest(identityKey)
	if !ok {
		return ErrUnavailable
	}
	registerDevice := frozen.deviceCredential != ""
	var continuityKeyID, continuityDigest *string
	if registerDevice {
		continuityKeyID = stringPointer(frozen.device.CredentialKeyID)
		continuityDigest = stringPointer(hex.EncodeToString(frozen.device.CredentialDigest[:]))
	}
	var displayName *string
	profileConfirmed := false
	if frozen.verified.Profile != nil {
		displayName = stringPointer(frozen.verified.Profile.DisplayName)
		profileConfirmed = frozen.verified.Profile.Confirmed
	}
	stage := ExchangeStage{StageCommon: StageCommon{Version: "v1", Kind: "exchange", AccountID: frozen.provisional.accountID,
		DeviceID: frozen.device.DeviceID, SessionID: frozen.sessionID, LineageRootSessionID: frozen.sessionID, ServerInstanceID: service.instanceID,
		CanonicalOrigin: service.origin, TokenKeyID: service.tokenKeys.Current.ID, AccessDigest: hex.EncodeToString(accessDigest.Sum[:]),
		RefreshDigest: hex.EncodeToString(refreshDigest.Sum[:]), RecoveryEpoch: frozen.provisional.epoch, SecurityRevision: frozen.provisional.head.Revision + 1,
		Scope: "account", AccessExpiresAt: accessExpires.Format(time.RFC3339Nano), RefreshExpiresAt: refreshExpires.Format(time.RFC3339Nano),
		ReauthenticateAt: frozen.absoluteExpiresAt.Format(time.RFC3339Nano), AbsoluteExpiresAt: frozen.absoluteExpiresAt.Format(time.RFC3339Nano)},
		IdentityProvider: frozen.verified.Identity.Provider, IdentityKeyID: identityKeyID, IdentityDigest: identityDigest,
		IdentityAudience: frozen.verified.Identity.Audience, IdentitySubject: frozen.verified.Identity.Subject, DisplayName: displayName,
		ProfileConfirmed: profileConfirmed, RegisterAccount: registerAccount, RegisterDevice: registerDevice,
		ContinuityKeyID: continuityKeyID, ContinuityDigest: continuityDigest}
	if credential != nil {
		stage.AppleCredentialID = stringPointer(credential.ref.ID)
		stage.AppleCredentialKeyID = stringPointer(credential.ref.KeyID)
		stage.AppleCredentialPurpose = stringPointer(credential.binding.Purpose)
		stage.AppleCredentialObjectID = stringPointer(credential.binding.ObjectID)
	}
	stageBytes, stageDigest, err := EncodeExchangeStage(stage)
	if err != nil {
		return ErrUnavailable
	}
	frozen.stageBytes, frozen.stageDigest = stageBytes, stageDigest
	frozen.payload = security.IdentitySessionIssuedPayload{Version: "v1", AccountID: frozen.provisional.accountID, IdentityKey: identityKey,
		RegisterAccount: registerAccount, DeviceID: frozen.device.DeviceID, ServerInstanceID: service.instanceID, CanonicalOrigin: service.origin,
		DeviceCredentialDigest: authorityKeyedDigest(frozen.device.CredentialKeyID, frozen.device.CredentialDigest), RegisterDevice: registerDevice,
		SessionID: frozen.sessionID, LineageRootSessionID: frozen.sessionID, RecoveryEpoch: frozen.provisional.epoch,
		ExpiresAt: frozen.absoluteExpiresAt.Format(time.RFC3339Nano), ProjectionMaterialDigest: hex.EncodeToString(stageDigest[:])}
	frozen.commandHash = [32]byte{}
	return nil
}

func (service *Service) preparePreliminary(ctx context.Context, frozen *frozenExchange) error {
	return service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		if snapshot.Head != frozen.provisional.head || snapshot.RecoveryEpoch != frozen.provisional.epoch {
			return authSecurityError("security_busy")
		}
		resolution, err := snapshot.View.ResolveIdentityAuthority(frozen.candidates)
		if err != nil || resolution != frozen.provisional.resolution {
			return authSecurityError("security_busy")
		}
		actor, err := snapshot.ResolveIdentityActor(frozen.candidates, frozen.provisional.accountID, frozen.provisional.generation)
		if err != nil {
			return err
		}
		prepared, err := service.prepareIdentitySession(snapshot, actor, *frozen, frozen.commandID)
		if err != nil {
			return err
		}
		frozen.commandHash, err = snapshot.CommandHash(prepared)
		return err
	})
}

func (service *Service) prepareIdentitySession(snapshot security.AuthoritySnapshot, actor security.IdentityActor, frozen frozenExchange, commandID string) (security.PreparedCommand, error) {
	input := security.IdentitySessionCommandInput{Meta: security.CommandMeta{CommandID: commandID, At: frozen.commandAt}, Payload: frozen.payload}
	if frozen.provisional.resolution.Status == "identity_unavailable" {
		return snapshot.PrepareIdentityRestored(actor, input)
	}
	return snapshot.PrepareIdentitySessionIssued(actor, input)
}

func (service *Service) accountView(ctx context.Context, frozen frozenExchange) (AccountView, error) {
	keyID, digestHex, ok := splitAuthorityDigest(frozen.payload.IdentityKey)
	if !ok {
		return AccountView{}, ErrUnavailable
	}
	digestBytes, _ := hex.DecodeString(digestHex)
	var digest [32]byte
	copy(digest[:], digestBytes)
	row, found, err := service.repository.LookupAuthIdentity(ctx, "apple", keyID, digest)
	if err != nil || !found || row.AccountID != frozen.provisional.accountID || row.AuthorityStatus != "active" {
		return AccountView{}, ErrUnavailable
	}
	return AccountView{ID: row.AccountID, DisplayName: row.DisplayName, ProfileRequired: row.ProfileRequired}, nil
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

func splitAuthorityDigest(value string) (string, string, bool) {
	const prefix = "hmac-sha256:"
	if len(value) <= len(prefix) || value[:len(prefix)] != prefix {
		return "", "", false
	}
	rest := value[len(prefix):]
	separator := -1
	for index := range rest {
		if rest[index] == ':' {
			separator = index
			break
		}
	}
	if separator <= 0 || separator+1 >= len(rest) || !digestKeyIDPattern.MatchString(rest[:separator]) || !lowercaseDigestPattern.MatchString(rest[separator+1:]) {
		return "", "", false
	}
	return rest[:separator], rest[separator+1:], true
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

// security's coded errors are deliberately private. Return the stable auth
// equivalent for errors that cross this module boundary.
func authSecurityError(code string) error { return codedError(code) }
