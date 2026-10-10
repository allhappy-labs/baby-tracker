package authority

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5"
)

// ErrLiveActivityAuthorizationChanged is a Live Activity job whose lease is
// no longer live or whose registration may no longer be pushed to: the
// worker releases the lease and sends nothing.
var ErrLiveActivityAuthorizationChanged error = Code("live_activity_authorization_changed")

// LiveActivitySubject is a lease-bound scheduling coordinate (main's
// security.LiveActivitySubject): the worker's claim, never a token or care
// payload. Its JSON is the claim's (app.claim_live_activity_work).
type LiveActivitySubject struct {
	JobID               string    `json:"job_id"`
	LeaseID             string    `json:"lease_id"`
	RegistrationID      string    `json:"registration_id"`
	RegistrationVersion uint64    `json:"registration_version"`
	AccountID           string    `json:"account_id"`
	DeviceID            string    `json:"device_id"`
	FamilyID            string    `json:"family_id"`
	BabyID              string    `json:"baby_id"`
	Kind                string    `json:"kind"`
	InstanceID          string    `json:"instance_id"`
	RecoveryEpoch       uint64    `json:"recovery_epoch"`
	DirtyVersion        uint64    `json:"dirty_version"`
	LeaseDeadline       time.Time `json:"lease_deadline"`
}

// liveActivitySnapshotSQL is the snapshot's one check: the lease is live
// (the exact claim, under the subject's tenant context) and the
// registration may still be pushed to (its member's role, or NULL).
const liveActivitySnapshotSQL = `SELECT app.live_activity_claim_live($1), app.live_activity_authorized($2,$3,$4)::text`

// WithLiveActivitySnapshot runs run in one read-only repeatable-read runtime
// transaction with the subject's tenant context, after one pipelined round
// trip (BEGIN, the tenant context and liveActivitySnapshotSQL) checks that
// the lease is live (app.live_activity_claim_live) and the registration is
// authorized (app.live_activity_authorized, with the node's entitlement
// policy from Options). Anything else is
// ErrLiveActivityAuthorizationChanged, and run is not called. The grant
// carries the member's role, so CanWrite follows access.RoleMayWrite (the
// rule family.Allowed applies to "mutate_record"). The transaction is
// rolled back when run returns: run builds a detached payload, and APNs I/O
// belongs afterwards.
func (s *Service) WithLiveActivitySnapshot(ctx context.Context, subject LiveActivitySubject, run func(pgx.Tx, access.Grant) error) error {
	if run == nil || !validID(subject.RegistrationID) || !validID(subject.AccountID) || !validID(subject.FamilyID) ||
		subject.RegistrationVersion == 0 || subject.RegistrationVersion > maxWireInteger {
		return ErrInvalidCommand
	}
	claim, err := json.Marshal(subject)
	if err != nil {
		return ErrInvalidCommand
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return unavailable("acquire live activity snapshot", err)
	}
	tx := &requestTx{conn: conn}
	defer tx.Rollback(context.WithoutCancel(ctx))
	batch := &pgx.Batch{}
	batch.Queue(requestBeginRead)
	batch.Queue(tenant.SetLocalSQL, subject.AccountID, subject.FamilyID)
	batch.Queue(liveActivitySnapshotSQL, claim, subject.RegistrationID, int64(subject.RegistrationVersion), s.requireFamilySyncEntitlement)
	results := conn.SendBatch(ctx, batch)
	var live bool
	var role *string
	_, beginErr := results.Exec()
	var contextErr, checkErr error
	if beginErr == nil {
		if _, contextErr = results.Exec(); contextErr == nil {
			checkErr = results.QueryRow().Scan(&live, &role)
		}
	}
	if err := errors.Join(beginErr, contextErr, checkErr, results.Close()); err != nil {
		return unavailable("live activity snapshot", err)
	}
	if !live || role == nil {
		return ErrLiveActivityAuthorizationChanged
	}
	return run(tx, access.Grant{AccountID: subject.AccountID, FamilyID: subject.FamilyID, Role: *role})
}
