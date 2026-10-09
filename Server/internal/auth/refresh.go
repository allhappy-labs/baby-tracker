package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
)

var (
	ErrAccessExpired            = codedError("access_expired")
	ErrSessionRevoked           = codedError("session_revoked")
	ErrRefreshReused            = codedError("refresh_reused")
	ErrReauthenticationRequired = codedError("reauthentication_required")
	ErrEpochChanged             = codedError("epoch_changed")
)

const maxRefreshPrepareAttempts = 3

type matchedAccess struct {
	row AccessSessionLookup
}

type matchedRefresh struct {
	row    RefreshSessionLookup
	key    identity.DigestKey
	digest [32]byte
}

func (service *Service) Authenticate(ctx context.Context, rawToken string) (access.Principal, error) {
	if service == nil || service.security == nil || service.repository == nil || !validBearerSecret(rawToken) {
		return access.Principal{}, ErrSessionRevoked
	}
	var principal access.Principal
	err := service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		matched, found, err := service.lookupAccessAcrossKeys(ctx, rawToken)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		if !found || matched.row.ServerInstanceID != service.instanceID {
			return ErrSessionRevoked
		}
		row := matched.row
		if row.RecoveryEpoch != snapshot.RecoveryEpoch {
			return ErrEpochChanged
		}
		if row.Status != "active" || row.RevokedAt != nil || row.ConsumedAt != nil {
			return ErrSessionRevoked
		}
		now := service.now().UTC().Truncate(time.Microsecond)
		deadline, err := service.sessionDeadline(ctx, row.AccountID, row.LineageRootSessionID, row.ReauthenticateAt, row.AbsoluteExpiresAt)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		if !now.Before(deadline) {
			return ErrReauthenticationRequired
		}
		if !now.Before(row.AccessExpiresAt) {
			return ErrAccessExpired
		}
		if !validSessionScope(row.Scope, row.FamilyID) {
			return ErrSessionRevoked
		}
		if _, err := snapshot.ResolveSessionActor(security.ExistingSessionActorInput{AccountID: row.AccountID, DeviceID: row.DeviceID,
			SessionID: row.SessionID, RecoveryEpoch: row.RecoveryEpoch, SecurityRevision: row.SecurityRevision}); err != nil {
			return ErrSessionRevoked
		}
		device, deviceOK := snapshot.View.Device(row.DeviceID)
		session, sessionOK := snapshot.View.Session(row.SessionID)
		if !deviceOK || device.Status != "active" || !sessionOK || session.Status != "active" ||
			session.SecurityRevision != row.SecurityRevision || session.RecoveryEpoch != row.RecoveryEpoch {
			return ErrSessionRevoked
		}
		scope, _, _ := authorityFamilySlot(snapshot.View, row.AccountID)
		// A fresh identity proof is deliberately account-scoped even when the
		// account owns a deletion-recovery claim. Only refresh upgrades that
		// persisted session to the narrow family_recovery scope, ensuring an
		// explicit second step before recovery authority can be exercised.
		if scope == "family_recovery" && row.Scope != "family_recovery" {
			scope = "account"
		}
		principal = access.Principal{AccountID: row.AccountID, DeviceID: row.DeviceID, SessionID: row.SessionID,
			RecoveryEpoch: row.RecoveryEpoch, SecurityRevision: row.SecurityRevision, Scope: scope, SessionScope: row.Scope,
			AccessExpiresAt: row.AccessExpiresAt}
		if row.FamilyID != nil {
			principal.SessionFamilyID = *row.FamilyID
		}
		return nil
	})
	return principal, err
}

func (service *Service) Refresh(ctx context.Context, rawToken string) (SessionBundle, error) {
	var lastErr error
	for attempt := 0; attempt < maxRefreshPrepareAttempts; attempt++ {
		bundle, err := service.refreshOnce(ctx, rawToken)
		if Code(err) != "security_busy" {
			return bundle, err
		}
		lastErr = err
		if ctx.Err() != nil {
			return SessionBundle{}, ErrUnavailable
		}
	}
	return SessionBundle{}, lastErr
}

// ResolveRefreshSubject performs the same keyed, epoch-bound authority lookup
// used by Refresh without consuming or rotating the credential. HTTP uses the
// returned opaque actor IDs only to charge the account/device quotas before the
// mutation enters its exclusive authority fence.
func (service *Service) ResolveRefreshSubject(ctx context.Context, rawToken string) (RefreshSubject, error) {
	if service == nil || service.security == nil || service.repository == nil || !validBearerSecret(rawToken) {
		return RefreshSubject{}, ErrSessionRevoked
	}
	var subject RefreshSubject
	err := service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		matched, found, err := service.lookupRefreshAcrossKeys(ctx, rawToken)
		if err != nil {
			return ErrUnavailable
		}
		if !found || matched.row.ServerInstanceID != service.instanceID {
			return ErrSessionRevoked
		}
		row := matched.row
		if row.RecoveryEpoch != snapshot.RecoveryEpoch {
			return ErrEpochChanged
		}
		if _, err := snapshot.ResolveSessionActor(security.ExistingSessionActorInput{
			AccountID: row.AccountID, DeviceID: row.DeviceID, SessionID: row.SessionID,
			RecoveryEpoch: row.RecoveryEpoch, SecurityRevision: row.SecurityRevision,
		}); err != nil {
			return ErrSessionRevoked
		}
		subject = RefreshSubject{AccountID: row.AccountID, DeviceID: row.DeviceID}
		return nil
	})
	return subject, err
}

func (service *Service) refreshOnce(ctx context.Context, rawToken string) (SessionBundle, error) {
	if service == nil || service.security == nil || service.repository == nil || !validBearerSecret(rawToken) {
		return SessionBundle{}, ErrSessionRevoked
	}
	var bundle SessionBundle
	var reused bool
	_, err := service.security.ExecutePrepared(ctx, func(callbackContext context.Context, snapshot security.AuthoritySnapshot) (security.PreparedCommand, error) {
		matched, found, err := service.lookupRefreshAcrossKeys(callbackContext, rawToken)
		if err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		if !found || matched.row.ServerInstanceID != service.instanceID {
			return security.PreparedCommand{}, ErrSessionRevoked
		}
		row := matched.row
		if row.RecoveryEpoch != snapshot.RecoveryEpoch {
			return security.PreparedCommand{}, ErrEpochChanged
		}
		now := service.now().UTC().Truncate(time.Microsecond)
		if !validSessionScope(row.Scope, row.FamilyID) {
			return security.PreparedCommand{}, ErrSessionRevoked
		}
		actor, err := snapshot.ResolveSessionActor(security.ExistingSessionActorInput{AccountID: row.AccountID, DeviceID: row.DeviceID,
			SessionID: row.SessionID, RecoveryEpoch: row.RecoveryEpoch, SecurityRevision: row.SecurityRevision})
		if err != nil {
			return security.PreparedCommand{}, ErrSessionRevoked
		}
		scope, familyID, familySlot := authorityFamilySlot(snapshot.View, row.AccountID)
		row.Scope, row.FamilyID = scope, familyID
		matched.row = row
		input := service.refreshInput(snapshot, matched, now)
		if row.RefreshConsumedAt != nil {
			commandID, err := DeriveRefreshReuseCommand(input)
			if err != nil {
				return security.PreparedCommand{}, ErrUnavailable
			}
			reused = true
			return snapshot.PrepareRefreshReuseRevocation(actor, security.RefreshReuseCommandInput{Meta: security.CommandMeta{CommandID: commandID, At: now},
				Payload: security.SessionPayload{Version: "v1", AccountID: row.AccountID, DeviceID: row.DeviceID, SessionID: row.SessionID,
					LineageRootSessionID: row.LineageRootSessionID, RecoveryEpoch: row.RecoveryEpoch,
					ExpiresAt: row.AbsoluteExpiresAt.Format(time.RFC3339Nano), Status: "revoked"}})
		}
		deadline, err := service.sessionDeadline(callbackContext, row.AccountID, row.LineageRootSessionID, row.ReauthenticateAt, row.AbsoluteExpiresAt)
		if err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		if !now.Before(deadline) || !now.Before(row.RefreshExpiresAt) {
			return security.PreparedCommand{}, ErrReauthenticationRequired
		}
		if row.Status != "active" || row.RevokedAt != nil || row.ReuseDetectedAt != nil {
			return security.PreparedCommand{}, ErrSessionRevoked
		}
		account, err := service.refreshAccountView(callbackContext, snapshot, row)
		if err != nil {
			return security.PreparedCommand{}, err
		}
		derived, err := DeriveRefreshRotation(input)
		if err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		stage, stageBytes, stageDigest, err := service.refreshStage(row, derived, snapshot.Head.Revision+1, now)
		if err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		payload := security.SessionPayload{Version: "v1", AccountID: row.AccountID, DeviceID: row.DeviceID, SessionID: derived.SessionID,
			ParentSessionID: row.SessionID, LineageRootSessionID: row.LineageRootSessionID, RecoveryEpoch: row.RecoveryEpoch,
			ExpiresAt: row.AbsoluteExpiresAt.Format(time.RFC3339Nano), Status: "active", ProjectionMaterialDigest: hex.EncodeToString(stageDigest[:])}
		prepared, err := snapshot.PrepareSessionRotation(actor, security.SessionRotationCommandInput{Meta: security.CommandMeta{CommandID: derived.CommandID, At: now}, Payload: payload})
		if err != nil {
			return security.PreparedCommand{}, err
		}
		commandHash, err := snapshot.CommandHash(prepared)
		if err != nil {
			return security.PreparedCommand{}, err
		}
		if err := service.repository.StageAuthMaterial(callbackContext, AuthMaterialStage{CommandID: derived.CommandID, CommandHash: commandHash,
			ProjectionMaterialDigest: stageDigest, Material: stageBytes, Now: now}); err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		bundle = SessionBundle{AccessToken: derived.AccessToken, RefreshToken: derived.RefreshToken, DeviceID: row.DeviceID,
			RecoveryEpoch: row.RecoveryEpoch, ExpiresAt: minDeadline(mustParseDeadline(stage.AccessExpiresAt), deadline),
			ReauthenticateAt: deadline, Account: account}
		bundle.FamilySlot = familySlot
		return prepared, nil
	})
	if err != nil {
		return SessionBundle{}, err
	}
	if reused {
		return SessionBundle{}, ErrRefreshReused
	}
	return bundle, nil
}

func (service *Service) sessionDeadline(ctx context.Context, accountID, lineageRootID string, reauthenticateAt, absoluteExpiresAt time.Time) (time.Time, error) {
	deadline := minDeadline(reauthenticateAt, absoluteExpiresAt)
	if service.mode != config.ModeSelfHosted {
		return deadline, nil
	}
	startedAt, err := service.repository.LineageStartedAt(ctx, accountID, lineageRootID)
	if err != nil {
		return time.Time{}, err
	}
	return minDeadline(deadline, startedAt.Add(defaultSelfHostedAge)), nil
}

func minDeadline(first, second time.Time) time.Time {
	if second.Before(first) {
		return second
	}
	return first
}

func (service *Service) Logout(ctx context.Context, principal access.Principal, commandID string) (security.CommandResult, error) {
	if service == nil || service.security == nil || !validUUID(commandID) {
		return security.CommandResult{}, ErrInvalidRequest
	}
	return service.security.ExecutePrepared(ctx, func(_ context.Context, snapshot security.AuthoritySnapshot) (security.PreparedCommand, error) {
		if principal.RecoveryEpoch != snapshot.RecoveryEpoch {
			return security.PreparedCommand{}, ErrEpochChanged
		}
		actor, err := snapshot.ResolveSessionActor(security.ExistingSessionActorInput{AccountID: principal.AccountID, DeviceID: principal.DeviceID,
			SessionID: principal.SessionID, RecoveryEpoch: principal.RecoveryEpoch, SecurityRevision: principal.SecurityRevision})
		if err != nil {
			return security.PreparedCommand{}, ErrSessionRevoked
		}
		device, ok := snapshot.View.Device(principal.DeviceID)
		if !ok {
			return security.PreparedCommand{}, ErrSessionRevoked
		}
		return snapshot.PrepareDeviceRevocation(actor, security.DeviceRevocationCommandInput{Meta: security.CommandMeta{CommandID: commandID, At: service.now().UTC().Truncate(time.Microsecond)},
			Payload: security.DevicePayload{Version: "v1", AccountID: principal.AccountID, DeviceID: principal.DeviceID,
				ServerInstanceID: device.ServerInstanceID, CanonicalOrigin: device.CanonicalOrigin, DeviceCredentialDigest: device.CredentialDigest}})
	})
}

func (service *Service) refreshAccountView(ctx context.Context, snapshot security.AuthoritySnapshot, session RefreshSessionLookup) (AccountView, error) {
	authority, ok := snapshot.View.Account(session.AccountID)
	if !ok || authority.Status != "active" {
		return AccountView{}, ErrSessionRevoked
	}
	keyID, digestHex, ok := splitAuthorityDigest(authority.IdentityKey)
	if !ok {
		return AccountView{}, ErrUnavailable
	}
	rawDigest, err := hex.DecodeString(digestHex)
	if err != nil || len(rawDigest) != 32 {
		return AccountView{}, ErrUnavailable
	}
	var digest [32]byte
	copy(digest[:], rawDigest)
	projected, found, err := service.repository.LookupAuthIdentity(ctx, "apple", keyID, digest)
	if err != nil {
		return AccountView{}, ErrUnavailable
	}
	if !found || projected.AccountID != session.AccountID || projected.AuthorityStatus != "active" {
		return AccountView{}, ErrUnavailable
	}
	return AccountView{ID: projected.AccountID, DisplayName: projected.DisplayName, ProfileRequired: projected.ProfileRequired}, nil
}

func (service *Service) lookupAccessAcrossKeys(ctx context.Context, raw string) (matchedAccess, bool, error) {
	keys := append([]identity.DigestKey{service.tokenKeys.Current}, service.tokenKeys.Previous...)
	var result matchedAccess
	matches := 0
	for _, key := range keys {
		digest := tokenDigest(key, "access", raw)
		row, found, err := service.repository.LookupAccessSession(ctx, key.ID, digest.Sum)
		if err != nil {
			return matchedAccess{}, false, err
		}
		if found {
			matches++
			result = matchedAccess{row: row}
		}
	}
	if matches > 1 {
		return matchedAccess{}, false, errors.New("ambiguous access credential")
	}
	return result, matches == 1, nil
}

func (service *Service) lookupRefreshAcrossKeys(ctx context.Context, raw string) (matchedRefresh, bool, error) {
	keys := append([]identity.DigestKey{service.tokenKeys.Current}, service.tokenKeys.Previous...)
	var result matchedRefresh
	matches := 0
	for _, key := range keys {
		digest := tokenDigest(key, "refresh", raw)
		row, found, err := service.repository.LookupRefreshSession(ctx, key.ID, digest.Sum)
		if err != nil {
			return matchedRefresh{}, false, err
		}
		if found {
			matches++
			result = matchedRefresh{row: row, key: key, digest: digest.Sum}
		}
	}
	if matches > 1 {
		return matchedRefresh{}, false, errors.New("ambiguous refresh credential")
	}
	return result, matches == 1, nil
}

func (service *Service) refreshInput(snapshot security.AuthoritySnapshot, matched matchedRefresh, now time.Time) RefreshDerivationInput {
	row := matched.row
	refreshExpires := now.Add(service.refreshTTL)
	if refreshExpires.After(row.AbsoluteExpiresAt) {
		refreshExpires = row.AbsoluteExpiresAt
	}
	accessExpires := now.Add(service.accessTTL)
	if accessExpires.After(refreshExpires) {
		accessExpires = refreshExpires
	}
	return RefreshDerivationInput{ServerInstanceID: service.instanceID, AccountID: row.AccountID, DeviceID: row.DeviceID,
		ParentSessionID: row.SessionID, LineageRootSessionID: row.LineageRootSessionID, RecoveryEpoch: row.RecoveryEpoch,
		ParentKey: matched.key, ParentDigest: matched.digest, OutputKeyID: service.tokenKeys.Current.ID, Scope: row.Scope, FamilyID: row.FamilyID,
		AccessExpiresAt: accessExpires, RefreshExpiresAt: refreshExpires, ReauthenticateAt: row.ReauthenticateAt,
		AbsoluteExpiresAt: row.AbsoluteExpiresAt, HeadGeneration: snapshot.Head.Generation, HeadRevision: snapshot.Head.Revision,
		HeadHash: snapshot.Head.EventHash, TargetRevision: snapshot.Head.Revision + 1}
}

func (service *Service) refreshStage(row RefreshSessionLookup, derived RefreshDerivation, securityRevision uint64, now time.Time) (RefreshStage, []byte, [32]byte, error) {
	refreshExpires := now.Add(service.refreshTTL)
	if refreshExpires.After(row.AbsoluteExpiresAt) {
		refreshExpires = row.AbsoluteExpiresAt
	}
	accessExpires := now.Add(service.accessTTL)
	if accessExpires.After(refreshExpires) {
		accessExpires = refreshExpires
	}
	accessDigest := tokenDigest(service.tokenKeys.Current, "access", derived.AccessToken)
	refreshDigest := tokenDigest(service.tokenKeys.Current, "refresh", derived.RefreshToken)
	stage := RefreshStage{StageCommon: StageCommon{Version: "v1", Kind: "refresh", AccountID: row.AccountID, DeviceID: row.DeviceID,
		SessionID: derived.SessionID, LineageRootSessionID: row.LineageRootSessionID, ServerInstanceID: service.instanceID,
		CanonicalOrigin: service.origin, TokenKeyID: service.tokenKeys.Current.ID, AccessDigest: hex.EncodeToString(accessDigest.Sum[:]),
		RefreshDigest: hex.EncodeToString(refreshDigest.Sum[:]), RecoveryEpoch: row.RecoveryEpoch, SecurityRevision: securityRevision,
		Scope: row.Scope, FamilyID: row.FamilyID, AccessExpiresAt: accessExpires.Format(time.RFC3339Nano),
		RefreshExpiresAt: refreshExpires.Format(time.RFC3339Nano), ReauthenticateAt: row.ReauthenticateAt.Format(time.RFC3339Nano),
		AbsoluteExpiresAt: row.AbsoluteExpiresAt.Format(time.RFC3339Nano)}, ParentSessionID: row.SessionID}
	encoded, digest, err := EncodeRefreshStage(stage)
	return stage, encoded, digest, err
}

func validBearerSecret(raw string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == raw
}

func validSessionScope(scope string, familyID *string) bool {
	if scope == "account" {
		return familyID == nil
	}
	return (scope == "family" || scope == "family_recovery") && familyID != nil && validUUID(*familyID)
}

func authorityFamilySlot(state security.AuthorityView, accountID string) (string, *string, *FamilySlot) {
	slot, ok := state.FamilySlot(accountID)
	if !ok {
		return "account", nil, nil
	}
	familyID := slot.FamilyID
	scope := "family"
	if slot.State == "family_recovery" {
		scope = "family_recovery"
	}
	return scope, &familyID, &FamilySlot{FamilyID: slot.FamilyID, State: slot.State}
}

func mustParseDeadline(raw string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, raw)
	return parsed
}
