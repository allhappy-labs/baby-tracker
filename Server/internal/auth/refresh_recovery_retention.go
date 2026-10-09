package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
)

func (service *Service) AcknowledgeRefresh(ctx context.Context, principal access.Principal, attemptID string, claim identity.DeviceContinuityClaim) error {
	if service == nil || service.security == nil || service.repository == nil || (!validUUID(attemptID) || attemptID == "00000000-0000-4000-8000-000000000000") {
		return ErrInvalidRequest
	}
	return service.security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		if principal.RecoveryEpoch != snapshot.RecoveryEpoch {
			return ErrEpochChanged
		}
		if !service.now().Before(principal.AccessExpiresAt) {
			return ErrAccessExpired
		}
		if _, err := snapshot.ResolveSessionActor(security.ExistingSessionActorInput{AccountID: principal.AccountID, DeviceID: principal.DeviceID, SessionID: principal.SessionID, RecoveryEpoch: principal.RecoveryEpoch, SecurityRevision: principal.SecurityRevision}); err != nil {
			return ErrSessionRevoked
		}
		session, ok := snapshot.View.Session(principal.SessionID)
		if !ok || session.Status != "active" {
			return ErrSessionRevoked
		}
		row, found, err := service.repository.LookupRecoveryChild(ctx, principal.SessionID)
		if err != nil {
			return ErrUnavailable
		}
		if !found {
			return ErrSessionRevoked
		}
		if err = service.validateRecoverySessionAuthority(ctx, snapshot, row); err != nil {
			return err
		}
		if !service.now().Before(row.AccessExpiresAt) {
			return ErrAccessExpired
		}
		binding, err := service.resolveExistingDevice(ctx, principal.AccountID, &claim)
		if err != nil {
			return err
		}
		device, ok := snapshot.View.Device(principal.DeviceID)
		if !ok || binding.DeviceID != principal.DeviceID || device.Status != "active" || device.AccountID != principal.AccountID || device.ServerInstanceID != service.instanceID || device.CanonicalOrigin != service.origin || device.CredentialDigest != authorityKeyedDigest(binding.CredentialKeyID, binding.CredentialDigest) {
			return ErrDeviceContinuityInvalid
		}
		receipts, err := service.repository.LookupRecoveryAcknowledgements(ctx, principal.AccountID, principal.DeviceID, service.instanceID, snapshot.RecoveryEpoch, attemptID)
		if err != nil {
			return ErrUnavailable
		}
		for _, receipt := range receipts {
			digest := sha256.Sum256(receipt.Material)
			child, ok := snapshot.View.Session(receipt.ChildSessionID)
			if !ok || digest != receipt.ProjectionMaterialDigest || hex.EncodeToString(digest[:]) != child.ProjectionMaterialDigest {
				return ErrUnavailable
			}
			var stage RefreshStage
			if json.Unmarshal(receipt.Material, &stage) != nil || stage.Recovery == nil {
				return ErrUnavailable
			}
			frozen := stage.Recovery.Input
			if frozen.AccountID != principal.AccountID || frozen.DeviceID != principal.DeviceID || frozen.ServerInstanceID != service.instanceID || frozen.RecoveryEpoch != snapshot.RecoveryEpoch || frozen.LineageRootSessionID != session.LineageRootSessionID || child.LineageRootSessionID != session.LineageRootSessionID || frozen.AttemptID != attemptID || frozen.ContinuityKeyID != binding.CredentialKeyID || frozen.ContinuityDigest != binding.CredentialDigest {
				return ErrSessionRevoked
			}
		}
		if len(receipts) == 0 {
			return nil
		}
		if err = service.repository.AcknowledgeRecovery(ctx, principal.AccountID, principal.DeviceID, service.instanceID, attemptID, snapshot.RecoveryEpoch, session.LineageRootSessionID); err != nil {
			return ErrUnavailable
		}
		return nil
	})
}

type RecoveryKeyInventory struct {
	SessionToken     []string
	DeviceContinuity []string
}

// RecoveryKeyCandidate carries no token material. Callers must retain unknown
// candidates conservatively; a SQL active status alone does not prove usability.
type RecoveryKeyCandidate struct {
	SessionID, AccountID, LineageID, TokenKeyID string
	ParentKeyID, ContinuityKeyID                *string
	RefreshExpiresAt                            *time.Time
	ReauthenticateAt, AbsoluteExpiresAt         time.Time
	EffectiveDeadline                           *time.Time
}

func (repository *Repository) RecoveryKeyCandidates(ctx context.Context) ([]RecoveryKeyCandidate, error) {
	if repository == nil || repository.runtime == nil {
		return nil, errRepositoryUnavailable
	}
	rows, err := repository.runtime.Query(ctx, `SELECT session_id,account_id,lineage_id,token_key_id,parent_key_id,continuity_key_id,refresh_expires_at,reauthenticate_at,absolute_expires_at,effective_deadline FROM app.refresh_recovery_key_candidates()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RecoveryKeyCandidate
	for rows.Next() {
		var candidate RecoveryKeyCandidate
		if err = rows.Scan(&candidate.SessionID, &candidate.AccountID, &candidate.LineageID, &candidate.TokenKeyID, &candidate.ParentKeyID, &candidate.ContinuityKeyID, &candidate.RefreshExpiresAt, &candidate.ReauthenticateAt, &candidate.AbsoluteExpiresAt, &candidate.EffectiveDeadline); err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, rows.Err()
}

// ReferencedRecoveryKeyIDs is the conservative data-only inventory. Readiness
// supplies a current validated authority view to Service.ReferencedRecoveryKeyIDs to
// exclude safely retired rows without reentering the authority fence.
func (repository *Repository) ReferencedRecoveryKeyIDs(ctx context.Context) (RecoveryKeyInventory, error) {
	candidates, err := repository.RecoveryKeyCandidates(ctx)
	if err != nil {
		return RecoveryKeyInventory{}, err
	}
	return recoveryCandidateKeys(candidates), nil
}

type RecoveryKeyEligibility struct {
	Inventory    RecoveryKeyInventory
	RevalidateAt time.Time
	Cacheable    bool
}

func (service *Service) ReferencedRecoveryKeyIDs(ctx context.Context, view security.AuthorityView) (RecoveryKeyInventory, error) {
	result, err := service.ReferencedRecoveryKeyEligibility(ctx, view)
	return result.Inventory, err
}

// NewRecoveryKeyReferences binds only repository, policy and clock. The caller
// supplies its current authority capability; this provider never opens a fence.
func NewRecoveryKeyReferences(repository *Repository, mode config.Mode, now func() time.Time) (func(context.Context, security.AuthorityView) (RecoveryKeyEligibility, error), error) {
	if repository == nil || repository.runtime == nil || now == nil || (mode != config.ModeHosted && mode != config.ModeSelfHosted) {
		return nil, errors.New("recovery key references require repository, mode and clock")
	}
	return func(ctx context.Context, view security.AuthorityView) (RecoveryKeyEligibility, error) {
		return referencedRecoveryKeyEligibility(ctx, repository, mode, now(), view)
	}, nil
}

func (service *Service) ReferencedRecoveryKeyEligibility(ctx context.Context, view security.AuthorityView) (RecoveryKeyEligibility, error) {
	if service == nil || service.repository == nil || service.now == nil {
		return RecoveryKeyEligibility{}, ErrUnavailable
	}
	return referencedRecoveryKeyEligibility(ctx, service.repository, service.mode, service.now(), view)
}

func referencedRecoveryKeyEligibility(ctx context.Context, repository *Repository, mode config.Mode, now time.Time, view security.AuthorityView) (RecoveryKeyEligibility, error) {
	candidates, err := repository.RecoveryKeyCandidates(ctx)
	if err != nil {
		return RecoveryKeyEligibility{}, err
	}
	usable := make([]RecoveryKeyCandidate, 0, len(candidates))
	result := RecoveryKeyEligibility{Cacheable: true}
	for _, candidate := range candidates {
		// Unknown authority/policy keeps the references. Known terminal authority or
		// a proven effective deadline releases them even if SQL status remains active.
		session, known := view.Session(candidate.SessionID)
		if !known || view.RecoveryEpoch() == 0 {
			result.Cacheable = false
			usable = append(usable, candidate)
			continue
		}
		if known && (session.Status == "revoked" || session.Status == "rotated" || session.RecoveryEpoch != view.RecoveryEpoch()) {
			continue
		}
		if known {
			account, ok := view.Account(session.AccountID)
			if !ok {
				result.Cacheable = false
				usable = append(usable, candidate)
				continue
			}
			if ok && account.Status != "active" {
				continue
			}
			device, ok := view.Device(session.DeviceID)
			if !ok {
				result.Cacheable = false
				usable = append(usable, candidate)
				continue
			}
			if ok && device.Status != "active" {
				continue
			}
		}
		deadline := minDeadline(candidate.ReauthenticateAt, candidate.AbsoluteExpiresAt)
		if candidate.RefreshExpiresAt != nil {
			deadline = minDeadline(deadline, *candidate.RefreshExpiresAt)
		}
		if candidate.EffectiveDeadline != nil {
			deadline = minDeadline(deadline, *candidate.EffectiveDeadline)
		}
		if mode == config.ModeSelfHosted {
			started, lookupErr := repository.LineageStartedAt(ctx, candidate.AccountID, candidate.LineageID)
			if lookupErr != nil || started.IsZero() {
				result.Cacheable = false
				usable = append(usable, candidate)
				continue
			}
			deadline = minDeadline(deadline, started.Add(defaultSelfHostedAge))
		}
		if deadline.IsZero() || now.Before(deadline) {
			usable = append(usable, candidate)
			if deadline.IsZero() {
				result.Cacheable = false
			} else {
				if result.RevalidateAt.IsZero() || deadline.Before(result.RevalidateAt) {
					result.RevalidateAt = deadline
				}
			}
		}
	}
	result.Inventory = recoveryCandidateKeys(usable)
	return result, nil
}

func recoveryCandidateKeys(candidates []RecoveryKeyCandidate) RecoveryKeyInventory {
	token, device := map[string]struct{}{}, map[string]struct{}{}
	for _, candidate := range candidates {
		if candidate.TokenKeyID != "" {
			token[candidate.TokenKeyID] = struct{}{}
		}
		if candidate.ParentKeyID != nil && *candidate.ParentKeyID != "" {
			token[*candidate.ParentKeyID] = struct{}{}
		}
		if candidate.ContinuityKeyID != nil && *candidate.ContinuityKeyID != "" {
			device[*candidate.ContinuityKeyID] = struct{}{}
		}
	}
	result := RecoveryKeyInventory{SessionToken: make([]string, 0, len(token)), DeviceContinuity: make([]string, 0, len(device))}
	for id := range token {
		result.SessionToken = append(result.SessionToken, id)
	}
	for id := range device {
		result.DeviceContinuity = append(result.DeviceContinuity, id)
	}
	sort.Strings(result.SessionToken)
	sort.Strings(result.DeviceContinuity)
	return result
}
