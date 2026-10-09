package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
)

type RecoverableRefreshInput struct {
	RefreshToken, AttemptID string
	DeviceContinuity        identity.DeviceContinuityClaim
}
type RecoveredSessionBundle struct {
	SessionBundle
	RefreshAttemptID string
}

var ErrRefreshAttemptConflict = codedError("refresh_attempt_conflict")
var ErrRefreshReceiptUnavailable = codedError("refresh_receipt_unavailable")
var errRecovered = errors.New("recovered committed refresh")

type recoveryMatch struct {
	parent    matchedRefresh
	binding   DeviceBinding
	authority security.SessionView
	recovered RecoveredSessionBundle
	consumed  bool
}

func (service *Service) ResolveRecoverableRefreshSubject(ctx context.Context, input RecoverableRefreshInput) (RefreshSubject, error) {
	if !service.validRecoveryRequest(input) {
		return RefreshSubject{}, ErrInvalidRequest
	}
	var subject RefreshSubject
	err := service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		match, err := service.matchRecovery(ctx, snapshot, input)
		if err != nil {
			return err
		}
		subject = RefreshSubject{AccountID: match.parent.row.AccountID, DeviceID: match.parent.row.DeviceID}
		return nil
	})
	return subject, err
}

func (service *Service) RefreshRecoverable(ctx context.Context, input RecoverableRefreshInput) (RecoveredSessionBundle, error) {
	if !service.validRecoveryRequest(input) {
		return RecoveredSessionBundle{}, ErrInvalidRequest
	}
	var last error
	for attempt := 0; attempt < maxRefreshPrepareAttempts; attempt++ {
		bundle, err := service.refreshRecoverableOnce(ctx, input)
		if Code(err) != "security_busy" {
			return bundle, err
		}
		last = err
		if ctx.Err() != nil {
			return RecoveredSessionBundle{}, ErrUnavailable
		}
	}
	return RecoveredSessionBundle{}, last
}

func (service *Service) validRecoveryRequest(input RecoverableRefreshInput) bool {
	return service != nil && service.security != nil && service.repository != nil && validBearerSecret(input.RefreshToken) && validUUID(input.AttemptID) && input.AttemptID != "00000000-0000-4000-8000-000000000000"
}

func (service *Service) matchRecovery(ctx context.Context, snapshot security.AuthoritySnapshot, input RecoverableRefreshInput) (recoveryMatch, error) {
	matched, found, err := service.lookupRefreshAcrossKeys(ctx, input.RefreshToken)
	if err != nil {
		return recoveryMatch{}, ErrUnavailable
	}
	if !found || matched.row.ServerInstanceID != service.instanceID {
		return recoveryMatch{}, ErrSessionRevoked
	}
	row := matched.row
	if row.RecoveryEpoch != snapshot.RecoveryEpoch {
		return recoveryMatch{}, ErrEpochChanged
	}
	binding, err := service.resolveExistingDevice(ctx, row.AccountID, &input.DeviceContinuity)
	if err != nil {
		return recoveryMatch{}, err
	}
	device, ok := snapshot.View.Device(row.DeviceID)
	if !ok || binding.DeviceID != row.DeviceID || device.Status != "active" || device.AccountID != row.AccountID || device.ServerInstanceID != service.instanceID || device.CanonicalOrigin != service.origin || device.CredentialDigest != authorityKeyedDigest(binding.CredentialKeyID, binding.CredentialDigest) {
		return recoveryMatch{}, ErrDeviceContinuityInvalid
	}
	parent, ok := snapshot.View.Session(row.SessionID)
	if !ok || parent.AccountID != row.AccountID || parent.DeviceID != row.DeviceID || parent.LineageRootSessionID != row.LineageRootSessionID || parent.RecoveryEpoch != row.RecoveryEpoch || parent.SecurityRevision != row.SecurityRevision {
		return recoveryMatch{}, ErrSessionRevoked
	}
	if _, err = snapshot.ResolveSessionActor(security.ExistingSessionActorInput{AccountID: row.AccountID, DeviceID: row.DeviceID, SessionID: row.SessionID, RecoveryEpoch: row.RecoveryEpoch, SecurityRevision: row.SecurityRevision}); err != nil {
		return recoveryMatch{}, ErrSessionRevoked
	}
	result := recoveryMatch{parent: matched, binding: binding, authority: parent, consumed: row.RefreshConsumedAt != nil}
	if !result.consumed {
		if parent.Status != "active" {
			return recoveryMatch{}, ErrSessionRevoked
		}
		if err = service.validateRecoverySession(ctx, snapshot, row); err != nil {
			return recoveryMatch{}, err
		}
		return result, nil
	}
	if parent.Status != "rotated" || parent.RotationAttemptID == "" || parent.ChildSessionID == "" {
		return recoveryMatch{}, ErrRefreshReceiptUnavailable
	}
	child, ok := snapshot.View.Session(parent.ChildSessionID)
	if !ok || child.ParentSessionID != row.SessionID || child.RefreshAttemptID != parent.RotationAttemptID || child.AccountID != row.AccountID || child.DeviceID != row.DeviceID || child.LineageRootSessionID != row.LineageRootSessionID || child.RecoveryEpoch != row.RecoveryEpoch {
		return recoveryMatch{}, ErrSessionRevoked
	}
	if child.Status != "active" {
		return recoveryMatch{}, ErrRefreshReceiptUnavailable
	}
	receipt, found, err := service.repository.LookupRecoveryReceipt(ctx, row.SessionID, parent.RotationAttemptID)
	if err != nil {
		return recoveryMatch{}, ErrUnavailable
	}
	if !found {
		if input.AttemptID == parent.RotationAttemptID {
			return recoveryMatch{}, ErrRefreshReceiptUnavailable
		}
		row, exists, lookupErr := service.repository.LookupRecoveryChild(ctx, parent.ChildSessionID)
		if lookupErr != nil {
			return recoveryMatch{}, ErrUnavailable
		}
		if !exists {
			return recoveryMatch{}, ErrSessionRevoked
		}
		if row.SessionID != parent.ChildSessionID || row.AccountID != matched.row.AccountID || row.DeviceID != matched.row.DeviceID || row.LineageRootSessionID != matched.row.LineageRootSessionID {
			return recoveryMatch{}, ErrSessionRevoked
		}
		if err := service.validateRecoverySession(ctx, snapshot, row); err != nil {
			return recoveryMatch{}, err
		}
		return result, nil
	}
	recovered, err := service.restoreRecovery(ctx, snapshot, matched, binding, child, receipt)
	if err != nil {
		return recoveryMatch{}, err
	}
	result.recovered = recovered
	return result, nil
}

func (service *Service) validateRecoverySession(ctx context.Context, snapshot security.AuthoritySnapshot, row RefreshSessionLookup) error {
	if err := service.validateRecoverySessionAuthority(ctx, snapshot, row); err != nil {
		return err
	}
	scope, family, _ := authorityFamilySlot(snapshot.View, row.AccountID)
	if row.Scope != "account" && (row.Scope != scope || !sameFamily(row.FamilyID, family)) {
		return ErrSessionRevoked
	}
	if row.Scope == "family" {
		if service.mode == config.ModeHosted {
			entitlement, ok := snapshot.View.Entitlement(*row.FamilyID, "family_sync")
			if !ok || entitlement.Status != "active" {
				return ErrSessionRevoked
			}
			if entitlement.ExpiresAt != "" {
				expiry, err := time.Parse(time.RFC3339Nano, entitlement.ExpiresAt)
				if err != nil {
					return ErrUnavailable
				}
				if !service.now().Before(expiry) {
					return ErrSessionRevoked
				}
			}
		}
		membership, ok := snapshot.View.Membership(*row.FamilyID, row.AccountID)
		if !ok || membership.Status != "active" {
			return ErrSessionRevoked
		}
		family, ok := snapshot.View.Family(*row.FamilyID)
		if !ok || family.Status != "active" {
			return ErrSessionRevoked
		}
	}
	return nil
}

func (service *Service) validateRecoverySessionAuthority(ctx context.Context, snapshot security.AuthoritySnapshot, row RefreshSessionLookup) error {
	if row.Status != "active" || row.RevokedAt != nil || row.RefreshConsumedAt != nil || row.ReuseDetectedAt != nil || !validSessionScope(row.Scope, row.FamilyID) {
		return ErrSessionRevoked
	}
	account, ok := snapshot.View.Account(row.AccountID)
	if !ok || account.Status != "active" {
		return ErrSessionRevoked
	}
	current, ok := snapshot.View.Session(row.SessionID)
	if !ok || current.Status != "active" || current.AccountID != row.AccountID || current.DeviceID != row.DeviceID || current.RecoveryEpoch != snapshot.RecoveryEpoch || current.SecurityRevision != row.SecurityRevision {
		return ErrSessionRevoked
	}
	deadline, err := service.sessionDeadline(ctx, row.AccountID, row.LineageRootSessionID, row.ReauthenticateAt, row.AbsoluteExpiresAt)
	if err != nil {
		return ErrUnavailable
	}
	now := service.now().UTC().Truncate(time.Microsecond)
	if !now.Before(deadline) || !now.Before(row.RefreshExpiresAt) {
		return ErrReauthenticationRequired
	}
	return nil
}

func (service *Service) restoreRecovery(ctx context.Context, snapshot security.AuthoritySnapshot, parent matchedRefresh, binding DeviceBinding, child security.SessionView, receipt RecoveryReceipt) (RecoveredSessionBundle, error) {
	digest := sha256.Sum256(receipt.Material)
	if digest != receipt.ProjectionMaterialDigest || hex.EncodeToString(digest[:]) != child.ProjectionMaterialDigest || receipt.ParentSessionID != parent.row.SessionID || receipt.ChildSessionID == "" || receipt.AttemptID != child.RefreshAttemptID {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	var stage RefreshStage
	if json.Unmarshal(receipt.Material, &stage) != nil || stage.Recovery == nil {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	material := *stage.Recovery
	stage.Recovery = nil
	canonical, canonicalDigest, err := EncodeRecoveryRefreshStage(stage, material)
	if err != nil || canonicalDigest != digest || !bytes.Equal(canonical, receipt.Material) {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	frozen := material.Input
	if frozen.ParentKeyID != parent.key.ID || frozen.ParentDigest != parent.digest || frozen.ParentSessionID != parent.row.SessionID || frozen.AccountID != parent.row.AccountID || frozen.DeviceID != parent.row.DeviceID || frozen.LineageRootSessionID != parent.row.LineageRootSessionID || frozen.RecoveryEpoch != snapshot.RecoveryEpoch || frozen.ServerInstanceID != service.instanceID || frozen.CanonicalOrigin != service.origin || frozen.ContinuityKeyID != binding.CredentialKeyID || frozen.ContinuityDigest != binding.CredentialDigest || frozen.AttemptID != receipt.AttemptID || stage.SessionID != receipt.ChildSessionID || stage.SecurityRevision != child.SecurityRevision {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	restored, err := RestoreRecoveryInput(frozen, parent.key)
	if err != nil {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	derived, err := DeriveRecoverableRefreshRotation(restored)
	if err != nil || derived.SessionID != receipt.ChildSessionID {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	outputKey, ok := recoveryKey(service.tokenKeys, frozen.OutputKeyID)
	if !ok {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	accessDigest := tokenDigest(outputKey, "access", derived.AccessToken)
	refreshDigest := tokenDigest(outputKey, "refresh", derived.RefreshToken)
	if hex.EncodeToString(accessDigest.Sum[:]) != stage.AccessDigest || hex.EncodeToString(refreshDigest.Sum[:]) != stage.RefreshDigest {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	accessRow, found, err := service.repository.LookupAccessSession(ctx, outputKey.ID, accessDigest.Sum)
	if err != nil || !found || accessRow.SessionID != derived.SessionID {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	row, found, err := service.repository.LookupRefreshSession(ctx, outputKey.ID, refreshDigest.Sum)
	if err != nil || !found || row.SessionID != derived.SessionID || row.AccountID != frozen.AccountID || row.DeviceID != frozen.DeviceID || row.ServerInstanceID != frozen.ServerInstanceID || row.LineageRootSessionID != frozen.LineageRootSessionID || row.RecoveryEpoch != frozen.RecoveryEpoch || row.SecurityRevision != frozen.TargetRevision || row.Scope != frozen.Scope || !sameFamily(row.FamilyID, frozen.FamilyID) || !row.AccessExpiresAt.Equal(frozen.AccessExpiresAt) || !row.RefreshExpiresAt.Equal(frozen.RefreshExpiresAt) || !row.ReauthenticateAt.Equal(frozen.ReauthenticateAt) || !row.AbsoluteExpiresAt.Equal(frozen.AbsoluteExpiresAt) {
		return RecoveredSessionBundle{}, ErrUnavailable
	}
	if err = service.validateRecoverySession(ctx, snapshot, row); err != nil {
		return RecoveredSessionBundle{}, err
	}
	if !service.now().Before(material.EffectiveDeadline) {
		return RecoveredSessionBundle{}, ErrReauthenticationRequired
	}
	account, err := service.refreshAccountView(ctx, snapshot, row)
	if err != nil {
		return RecoveredSessionBundle{}, err
	}
	_, _, slot := authorityFamilySlot(snapshot.View, row.AccountID)
	return RecoveredSessionBundle{SessionBundle: SessionBundle{AccessToken: derived.AccessToken, RefreshToken: derived.RefreshToken, DeviceID: row.DeviceID, RecoveryEpoch: row.RecoveryEpoch, ExpiresAt: minDeadline(frozen.AccessExpiresAt, material.EffectiveDeadline), ReauthenticateAt: material.EffectiveDeadline, Account: account, FamilySlot: slot}, RefreshAttemptID: receipt.AttemptID}, nil
}

func recoveryKey(keys identity.DigestKeyring, id string) (identity.DigestKey, bool) {
	if keys.Current.ID == id {
		return keys.Current, true
	}
	for _, key := range keys.Previous {
		if key.ID == id {
			return key, true
		}
	}
	return identity.DigestKey{}, false
}

func (service *Service) refreshRecoverableOnce(ctx context.Context, input RecoverableRefreshInput) (RecoveredSessionBundle, error) {
	var bundle RecoveredSessionBundle
	conflict := false
	_, err := service.security.ExecutePrepared(ctx, func(callbackContext context.Context, snapshot security.AuthoritySnapshot) (security.PreparedCommand, error) {
		match, err := service.matchRecovery(callbackContext, snapshot, input)
		if err != nil {
			return security.PreparedCommand{}, err
		}
		row := match.parent.row
		now := service.now().UTC().Truncate(time.Microsecond)
		actor, err := snapshot.ResolveSessionActor(security.ExistingSessionActorInput{AccountID: row.AccountID, DeviceID: row.DeviceID, SessionID: row.SessionID, RecoveryEpoch: row.RecoveryEpoch, SecurityRevision: row.SecurityRevision})
		if err != nil {
			return security.PreparedCommand{}, ErrSessionRevoked
		}
		if match.consumed {
			if match.authority.RotationAttemptID == input.AttemptID {
				bundle = match.recovered
				return security.PreparedCommand{}, errRecovered
			}
			commandID, err := DeriveRefreshReuseCommand(service.refreshInput(snapshot, match.parent, now))
			if err != nil {
				return security.PreparedCommand{}, ErrUnavailable
			}
			conflict = true
			return snapshot.PrepareRefreshReuseRevocation(actor, security.RefreshReuseCommandInput{Meta: security.CommandMeta{CommandID: commandID, At: now}, Payload: security.SessionPayload{Version: "v1", AccountID: row.AccountID, DeviceID: row.DeviceID, SessionID: row.SessionID, LineageRootSessionID: row.LineageRootSessionID, RecoveryEpoch: row.RecoveryEpoch, ExpiresAt: row.AbsoluteExpiresAt.Format(time.RFC3339Nano), Status: "revoked"}})
		}
		scope, family, slot := authorityFamilySlot(snapshot.View, row.AccountID)
		row.Scope, row.FamilyID = scope, family
		if err := service.validateRecoverySession(callbackContext, snapshot, row); err != nil {
			return security.PreparedCommand{}, err
		}
		match.parent.row = row
		deadline, err := service.sessionDeadline(callbackContext, row.AccountID, row.LineageRootSessionID, row.ReauthenticateAt, row.AbsoluteExpiresAt)
		if err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		derivation := RecoveryDerivationInput{RefreshDerivationInput: service.refreshInput(snapshot, match.parent, now), AttemptID: input.AttemptID, CanonicalOrigin: service.origin, ContinuityKeyID: match.binding.CredentialKeyID, ContinuityDigest: match.binding.CredentialDigest}
		derived, err := DeriveRecoverableRefreshRotation(derivation)
		if err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		stage, _, _, err := service.refreshStage(row, derived, snapshot.Head.Revision+1, now)
		if err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		material := RecoveryMaterial{Input: FreezeRecoveryInput(derivation), CreatedAt: now, EffectiveDeadline: deadline}
		stageBytes, stageDigest, err := EncodeRecoveryRefreshStage(stage, material)
		if err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		payload := security.SessionPayload{Version: "v1", AccountID: row.AccountID, DeviceID: row.DeviceID, SessionID: derived.SessionID, ParentSessionID: row.SessionID, LineageRootSessionID: row.LineageRootSessionID, RecoveryEpoch: row.RecoveryEpoch, ExpiresAt: row.AbsoluteExpiresAt.Format(time.RFC3339Nano), Status: "active", ProjectionMaterialDigest: hex.EncodeToString(stageDigest[:]), RefreshAttemptID: input.AttemptID}
		prepared, err := snapshot.PrepareSessionRotation(actor, security.SessionRotationCommandInput{Meta: security.CommandMeta{CommandID: derived.CommandID, At: now}, Payload: payload})
		if err != nil {
			return security.PreparedCommand{}, err
		}
		hash, err := snapshot.CommandHash(prepared)
		if err != nil {
			return security.PreparedCommand{}, err
		}
		account, err := service.refreshAccountView(callbackContext, snapshot, row)
		if err != nil {
			return security.PreparedCommand{}, err
		}
		if err = service.repository.StageAuthMaterial(callbackContext, AuthMaterialStage{CommandID: derived.CommandID, CommandHash: hash, ProjectionMaterialDigest: stageDigest, Material: stageBytes, Now: now}); err != nil {
			return security.PreparedCommand{}, ErrUnavailable
		}
		bundle = RecoveredSessionBundle{SessionBundle: SessionBundle{AccessToken: derived.AccessToken, RefreshToken: derived.RefreshToken, DeviceID: row.DeviceID, RecoveryEpoch: row.RecoveryEpoch, ExpiresAt: minDeadline(derivation.AccessExpiresAt, deadline), ReauthenticateAt: deadline, Account: account, FamilySlot: slot}, RefreshAttemptID: input.AttemptID}
		return prepared, nil
	})
	if errors.Is(err, errRecovered) {
		return bundle, nil
	}
	if err != nil {
		return RecoveredSessionBundle{}, err
	}
	if conflict {
		return RecoveredSessionBundle{}, ErrRefreshAttemptConflict
	}
	return bundle, nil
}
