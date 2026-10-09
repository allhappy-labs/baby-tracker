package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"github.com/jackc/pgx/v5"
)

var (
	ErrMutationIDReused       = errors.New("mutation_id_reused")
	ErrFamilyScopeRequired    = errors.New("family_scope_required")
	ErrSessionRefreshRequired = errors.New("session_refresh_required")
	// ErrReadOnlyCaregiver is returned for a viewer's mutation. It is raised
	// inside the authorized transaction so the rollback leaves no journal row
	// and no advanced cursor: a refused batch must not look like progress.
	ErrReadOnlyCaregiver  = errors.New("read_only_caregiver")
	ErrFeatureUnavailable = records.ErrFeatureUnavailable
	errReceiptCollision   = errors.New("receipt_collision")
)

type CommitObserver interface {
	Schedule(context.Context, pgx.Tx, access.Grant, string, protocol.Cursor) error
	// Publish is post-commit best effort. Its error is deliberately not returned
	// as a mutation failure because the durable receipt is already replayable.
	Publish(context.Context, string, protocol.Cursor) error
}

type ImportAdmission interface {
	AdmitRecord(context.Context, pgx.Tx, access.Grant, string, protocol.Mutation) (protocol.Outcome, bool, error)
}

func (s *Service) SetImportAdmission(admission ImportAdmission) { s.imports = admission }

type Service struct {
	guard    records.Guard
	repo     *records.Repository
	identity protocol.ServerInfo
	limits   protocol.Limits
	now      func() time.Time
	resolver access.FamilyResolver
	observer CommitObserver
	pageKeys identity.DigestKeyring
	imports  ImportAdmission
}

func NewServiceWithPaging(guard records.Guard, repo *records.Repository, server protocol.ServerInfo, limits protocol.Limits,
	now func() time.Time, resolver access.FamilyResolver, observer CommitObserver, pageKeys identity.DigestKeyring) *Service {
	service := NewServiceWithResolver(guard, repo, server, limits, now, resolver, observer)
	service.pageKeys = clonePageKeys(pageKeys)
	return service
}

func NewService(guard records.Guard, repo *records.Repository, identity protocol.ServerInfo, limits protocol.Limits, now func() time.Time) *Service {
	return &Service{guard: guard, repo: repo, identity: identity, limits: limits, now: now}
}

func NewServiceWithResolver(guard records.Guard, repo *records.Repository, identity protocol.ServerInfo, limits protocol.Limits,
	now func() time.Time, resolver access.FamilyResolver, observer CommitObserver) *Service {
	service := NewService(guard, repo, identity, limits, now)
	service.resolver = resolver
	service.observer = observer
	return service
}

func actionFor(m protocol.Mutation) (string, error) {
	if m.EntityKind == "baby" {
		switch m.Operation {
		case "create":
			return "add_baby", nil
		case "delete":
			return "delete_baby", nil
		case "update":
			return "mutate_record", nil
		}
	}
	if (m.EntityKind == "feed" || m.EntityKind == "sleep" || m.EntityKind == "diaper" || m.EntityKind == "supplement" || m.EntityKind == "growth_profile" || m.EntityKind == "measurement") && (m.Operation == "create" || m.Operation == "update" || m.Operation == "delete") {
		return "mutate_record", nil
	}
	if m.EntityKind == "timer" {
		return "mutate_record", nil
	}
	return "", protocol.ErrInvalidMutation
}

func (s *Service) Apply(ctx context.Context, p access.Principal, familyID string, m protocol.Mutation) (protocol.Outcome, error) {
	if s == nil || s.guard == nil || s.repo == nil || s.now == nil {
		return protocol.Outcome{}, fmt.Errorf("sync service unavailable")
	}
	if p.Scope != "family" {
		return protocol.Outcome{}, ErrFamilyScopeRequired
	}
	if p.SessionScope != "family" || p.SessionFamilyID != familyID {
		return protocol.Outcome{}, ErrSessionRefreshRequired
	}
	if !protocol.ValidUUID(familyID) || familyID == "" {
		return protocol.Outcome{}, ErrFamilyScopeRequired
	}
	if s.resolver != nil {
		resolved, err := s.resolver.Current(ctx, p)
		if err != nil {
			return protocol.Outcome{}, err
		}
		if resolved != familyID {
			return protocol.Outcome{}, ErrFamilyScopeRequired
		}
	}
	if err := s.identity.Validate(); err != nil {
		return protocol.Outcome{}, err
	}
	if err := s.limits.Validate(); err != nil {
		return protocol.Outcome{}, err
	}
	action, err := actionFor(m)
	if err != nil {
		return protocol.Outcome{}, err
	}
	hash, err := protocol.CanonicalMutationHash(m)
	if err != nil {
		return protocol.Outcome{}, err
	}
	var acceptedAt time.Time
	var result protocol.Outcome
	var publish *protocol.Cursor
	var journalAdvanced bool
	var repositoryElapsed, observerElapsed time.Duration
	var measuredRepository, measuredObserver bool
	err = s.guard.WithAuthorizedTransaction(ctx, p, familyID, action, func(tx pgx.Tx, g access.Grant) error {
		if g.AccountID != p.AccountID || g.FamilyID != familyID {
			return ErrFamilyScopeRequired
		}
		// A read-only caregiver reads everything and originates nothing. This
		// is the gate; the client hiding its log buttons is only a courtesy,
		// and an older build, a widget or a replayed frozen mutation would
		// walk straight past it.
		if !access.RoleMayWrite(g.Role) {
			return ErrReadOnlyCaregiver
		}
		// The family lock serializes every write to a family: the receipt
		// lookup, the journal sequence and the commit order all happen under
		// it, so two copies of one mutation cannot both miss the receipt.
		if err := records.LockFamilyForUpdate(ctx, tx, g.FamilyID); err != nil {
			return err
		}
		var journalBefore uint64
		if err := tx.QueryRow(ctx, `SELECT next_sequence FROM families WHERE id=$1`, g.FamilyID).Scan(&journalBefore); err != nil {
			return err
		}
		acceptedAt = s.now().UTC()
		receipt, found, err := readReceipt(ctx, tx, p.DeviceID, m.MutationID)
		if err != nil {
			return err
		}
		if found {
			if receipt.Hash != hash {
				return errReceiptCollision
			}
			original := receipt.Outcome.Status
			result = receipt.Outcome
			result.Status = "already_applied"
			result.OriginalStatus = &original
			return nil
		}
		handled := false
		if m.ImportID != nil {
			if s.imports == nil {
				return ErrFeatureUnavailable
			}
			result, handled, err = s.imports.AdmitRecord(ctx, tx, g, p.DeviceID, m)
			if err != nil {
				return err
			}
		}
		if !handled {
			databaseStart := time.Now()
			result, err = s.repo.Apply(ctx, tx, records.ApplyCommand{Grant: g, DeviceID: p.DeviceID, Mutation: m, AcceptedAt: acceptedAt,
				ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch, Limits: s.limits})
			repositoryElapsed = time.Since(databaseStart)
			measuredRepository = true
		}
		if err != nil {
			return err
		}
		deletionReleasedReserve := m.Operation == "delete" && result.Status == "applied" && result.Cursor != nil &&
			result.CanonicalEntity != nil && result.CanonicalEntity.Meta.TombstonedAt != nil
		if err = s.repo.ChargeReceipt(ctx, tx, g.FamilyID, result, s.limits, deletionReleasedReserve); err != nil {
			return err
		}
		if err = saveReceipt(ctx, tx, g, p.DeviceID, m, hash, result, acceptedAt); err != nil {
			return err
		}
		var journalAfter uint64
		if err = tx.QueryRow(ctx, `SELECT next_sequence FROM families WHERE id=$1`, g.FamilyID).Scan(&journalAfter); err != nil {
			return err
		}
		journalAdvanced = journalAfter > journalBefore
		if journalAdvanced && result.Cursor != nil && s.observer != nil {
			observerStart := time.Now()
			err = s.observer.Schedule(ctx, tx, g, p.DeviceID, *result.Cursor)
			observerElapsed = time.Since(observerStart)
			measuredObserver = true
			if err != nil {
				return err
			}
			cursor := *result.Cursor
			publish = &cursor
		}
		return nil
	})
	// The transaction guard has invalidated its lease and released the fence.
	// An opt-in recorder may reenter authority without prolonging that boundary.
	if measuredRepository {
		diagnostics.Record(ctx, nil, diagnostics.Database, repositoryElapsed)
	}
	if measuredObserver {
		diagnostics.Record(ctx, nil, diagnostics.Observer, observerElapsed)
	}
	if errors.Is(err, errReceiptCollision) {
		return protocol.Outcome{}, ErrMutationIDReused
	}
	if err != nil {
		return protocol.Outcome{}, err
	}
	if publish != nil && s.observer != nil {
		_ = s.observer.Publish(ctx, familyID, *publish)
	}
	return result, nil
}
