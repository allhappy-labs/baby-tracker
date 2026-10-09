package security

import (
	"context"
	"errors"

	"babytracker/server/internal/access"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExportSubject is constructed from a claimed server job, never from an HTTP
// request. Its lease and authority are rechecked while the read fence is held.
type ExportSubject struct {
	JobID, LeaseID, FamilyID, RequesterAccountID, InstanceID string
	RecoveryEpoch                                            uint64
}

type ExportSnapshotGuard struct {
	authority *Service
	runtime   *pgxpool.Pool
}

func NewExportSnapshotGuard(authority *Service, runtime *pgxpool.Pool) (*ExportSnapshotGuard, error) {
	if authority == nil || runtime == nil {
		return nil, errors.New("export snapshot requires authority and runtime pool")
	}
	return &ExportSnapshotGuard{authority: authority, runtime: runtime}, nil
}

func (guard *ExportSnapshotGuard) WithExportSnapshot(ctx context.Context, subject ExportSubject, run func(pgx.Tx, access.Grant) error) error {
	if guard == nil || guard.authority == nil || guard.runtime == nil || run == nil {
		return ErrUnavailable
	}
	if err := guard.authority.rejectCallbackEntry(ctx); err != nil {
		return err
	}
	return guard.authority.WithAuthoritySnapshot(ctx, func(snapshot AuthoritySnapshot) error {
		state := snapshot.View
		account, accountOK := state.Account(subject.RequesterAccountID)
		slot, slotOK := state.FamilySlot(subject.RequesterAccountID)
		family, familyOK := state.Family(subject.FamilyID)
		membership, memberOK := state.Membership(subject.FamilyID, subject.RequesterAccountID)
		if subject.RecoveryEpoch != snapshot.RecoveryEpoch || !accountOK || account.Status != "active" ||
			!slotOK || slot.FamilyID != subject.FamilyID || slot.State != "active" ||
			!familyOK || family.Status != "active" || family.OwnerAccountID != subject.RequesterAccountID ||
			!memberOK || membership.Status != "active" || membership.Role != "owner" {
			return errorCode("export_authorization_changed")
		}
		tx, err := guard.runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return ErrUnavailable
		}
		defer tx.Rollback(ctx)
		if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: subject.RequesterAccountID, FamilyID: subject.FamilyID}); err != nil {
			return err
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.export_jobs j JOIN public.server_state s ON s.singleton
			WHERE j.id=$1 AND j.family_id=$2 AND j.requester_account_id=$3 AND j.server_instance_id=$4
			AND j.recovery_epoch=$5 AND j.lease_id=$6 AND j.state='running' AND j.lease_until>now()
			AND j.expires_at>now() AND s.instance_id=j.server_instance_id AND s.recovery_epoch=j.recovery_epoch)`,
			subject.JobID, subject.FamilyID, subject.RequesterAccountID, subject.InstanceID, subject.RecoveryEpoch, subject.LeaseID).Scan(&live); err != nil {
			return ErrUnavailable
		}
		if !live {
			return errorCode("export_authorization_changed")
		}
		grant := access.Grant{AccountID: subject.RequesterAccountID, FamilyID: subject.FamilyID, Role: "owner", Revision: snapshot.Head.Revision}
		if err := run(tx, grant); err != nil {
			return err
		}
		// Read-only snapshot: rollback releases it without introducing a commit
		// failure after the worker has durably published the encrypted archive.
		return nil
	})
}

// WithAuthorizedSnapshot provides the request path's held authority fence and
// immutable family view for ready archive downloads.
func (service *Service) WithAuthorizedSnapshot(ctx context.Context, principal access.Principal, familyID, action string, run func(pgx.Tx, access.Grant) error) error {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return err
	}
	if service == nil || service.decide == nil || run == nil {
		return ErrUnavailable
	}
	callbackCtx := ctx
	ctx, head, release, err := service.beginGuardedTransaction(ctx, principal)
	if err != nil {
		return err
	}
	defer release()
	grant, err := service.decideWithAuthorityRead(callbackCtx, ctx, principal, familyID, action)
	if err != nil {
		return err
	}
	if grant.AccountID != principal.AccountID || grant.FamilyID != familyID || grant.Revision != head.Revision {
		return ErrMembershipRevoked
	}
	tx, err := service.runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrUnavailable
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: grant.AccountID, FamilyID: grant.FamilyID}); err != nil {
		return err
	}
	if err := run(tx, grant); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}
