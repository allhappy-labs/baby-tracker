package liveactivity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"babytracker/server/internal/apns"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrStaleClaim = errors.New("live_activity_stale_claim")

// Claim is a lease-bound scheduling coordinate, never a care payload or token.
type Claim struct {
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
type Queue struct {
	pool *pgxpool.Pool
	// workPage overrides SweepWorkPageLimit (tests); zero keeps it.
	workPage int
}

func NewQueue(pool *pgxpool.Pool) *Queue { return &Queue{pool: pool} }
func (q *Queue) Claim(ctx context.Context, now time.Time, limit int) ([]Claim, error) {
	rows, err := q.pool.Query(ctx, `SELECT * FROM app.claim_live_activity_work($1,$2)`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Claim{}
	for rows.Next() {
		var raw []byte
		var claim Claim
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &claim); err != nil {
			return nil, err
		}
		result = append(result, claim)
	}
	return result, rows.Err()
}
func (q *Queue) Complete(ctx context.Context, claim Claim) error {
	return q.finish(ctx, claim, nil, nil)
}
func (q *Queue) Retry(ctx context.Context, claim Claim, notBefore time.Time, reason string) error {
	return q.finish(ctx, claim, &notBefore, &reason)
}
func (q *Queue) finish(ctx context.Context, claim Claim, notBefore *time.Time, reason *string) error {
	raw, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	var accepted bool
	if err = q.pool.QueryRow(ctx, `SELECT app.finish_live_activity_work($1,$2,$3)`, raw, notBefore, reason).Scan(&accepted); err != nil {
		return err
	}
	if !accepted {
		return ErrStaleClaim
	}
	return nil
}

// Retain removes expired authority rows and their token/queue children. Version
// tombstones remain until absolute lineage authority expires.
func (q *Queue) Retain(ctx context.Context) (int64, error) {
	var count int64
	err := q.pool.QueryRow(ctx, `SELECT app.retain_live_activity_work()`).Scan(&count)
	return count, err
}

// TokenFence identifies the exact registration/token version observed before
// I/O. No raw token is accepted or returned by the worker persistence function.
type TokenFence struct {
	ActivityID string `json:"activity_id"`
	Version    uint64 `json:"version"`
}

// PushOutcome is APNs' answer to one attempt as the history keeps it: the
// response class, the HTTP status when one arrived and an allowlisted APNs
// reason. Never a token, payload or care content.
type PushOutcome struct {
	Result     PushClassification `json:"result"`
	HTTPStatus *int               `json:"http_status,omitempty"`
	Reason     string             `json:"reason,omitempty"`
}

func outcomeOf(result PushResult) *PushOutcome {
	outcome := &PushOutcome{Result: result.Classification}
	if result.HTTPStatus >= 100 && result.HTTPStatus <= 599 {
		status := result.HTTPStatus
		outcome.HTTPStatus = &status
	}
	if apns.KnownReason(result.Reason) {
		outcome.Reason = result.Reason
	}
	return outcome
}

func (q *Queue) LoadTarget(ctx context.Context, c Claim) (TargetState, error) {
	raw, _ := json.Marshal(c)
	var state []byte
	err := q.pool.QueryRow(ctx, `SELECT app.live_activity_target($1,NULL,false,NULL,NULL,NULL)`, raw).Scan(&state)
	if err != nil {
		return TargetState{}, err
	}
	if state == nil {
		return TargetState{}, ErrStaleClaim
	}
	var s TargetState
	err = json.Unmarshal(state, &s)
	return s, err
}

func (q *Queue) SaveTarget(ctx context.Context, c Claim, s TargetState, finish bool, deadline time.Time, invalidate *TokenFence) error {
	return q.saveTarget(ctx, c, s, finish, deadline, invalidate, nil)
}

// SaveOutcome finishes the lease after a send, recording APNs' answer against
// this attempt's history row together with the resulting lifecycle state.
func (q *Queue) SaveOutcome(ctx context.Context, c Claim, s TargetState, deadline time.Time, invalidate *TokenFence, outcome *PushOutcome) error {
	return q.saveTarget(ctx, c, s, true, deadline, invalidate, outcome)
}

func (q *Queue) saveTarget(ctx context.Context, c Claim, s TargetState, finish bool, deadline time.Time, invalidate *TokenFence, outcome *PushOutcome) error {
	claim, _ := json.Marshal(c)
	state, err := json.Marshal(s)
	if err != nil {
		return err
	}
	var at *time.Time
	if !deadline.IsZero() {
		at = &deadline
	}
	var fence []byte
	if invalidate != nil {
		fence, _ = json.Marshal(invalidate)
	}
	var answer []byte
	if outcome != nil {
		answer, _ = json.Marshal(outcome)
	}
	var result []byte
	err = q.pool.QueryRow(ctx, `SELECT app.live_activity_target($1,$2,$3,$4,$5,$6)`, claim, state, finish, at, fence, answer).Scan(&result)
	if err == nil && result == nil {
		return ErrStaleClaim
	}
	return err
}

// ReleaseStale clears this exact lease only when newer dirty work has made it
// unacknowledgeable. Lifecycle intent and completed work are left untouched, so
// the next lease re-derives from the persisted state under a fresh guard.
func (q *Queue) ReleaseStale(ctx context.Context, c Claim) error {
	claim, _ := json.Marshal(c)
	var released bool
	err := q.pool.QueryRow(ctx, `SELECT app.release_stale_live_activity_lease($1)`, claim).Scan(&released)
	if err == nil && !released {
		return ErrStaleClaim
	}
	return err
}

// AbandonStart releases only this exact known-unsent intent, even if newer
// dirty work or a registration rotation invalidated its snapshot. It never
// completes dirty work or authorizes transport; the next lease needs a guard.
func (q *Queue) AbandonStart(ctx context.Context, c Claim, intent, previous TargetState) error {
	claim, _ := json.Marshal(c)
	allowed, used := false, false
	if previous.GenerationID == intent.GenerationID {
		allowed, used = previous.AbsenceRetryAllowed, previous.AbsenceRetryUsed
	}
	var accepted bool
	err := q.pool.QueryRow(ctx, `SELECT app.abandon_live_activity_start($1,$2,$3,$4,$5)`, claim, intent.GenerationID, intent.AttemptID, allowed, used).Scan(&accepted)
	if err == nil && !accepted {
		return ErrStaleClaim
	}
	return err
}

// The maintenance calls below replace main's journal trigger, the claim's
// own sweep and the security guard's registration sweep: the worker runs
// them on the maintenance pool.

// SweepRegistrationsLimit is one registration sweep's page: a sweep that
// returns this many may have left more.
const SweepRegistrationsLimit = 100

// SweepRegistrations revokes up to SweepRegistrationsLimit registrations
// app.live_activity_authorized no longer admits (requireEntitlement is the
// hosted family_sync rule) and returns how many it found; their delivery
// rows go with them.
func (q *Queue) SweepRegistrations(ctx context.Context, requireEntitlement bool) (int, error) {
	var n int
	err := q.pool.QueryRow(ctx, `SELECT app.sweep_live_activity_registrations($1,$2)`, requireEntitlement, SweepRegistrationsLimit).Scan(&n)
	return n, err
}

// SweepWorkPageLimit is one work-sweep page: the families one call of
// app.sweep_live_activity_work_page dirties in its own short transaction
// (about 2.5 ms a family under the 2x load, so a page holds its families'
// dirty rows for a fraction of a second).
const SweepWorkPageLimit = 100

// SweepWorkPage is one page of the work sweep. Of the registered families
// after after ("" for the first page), in family order, it takes up to a
// page and dirties each whose sequence moved past its last flush, or whose
// authority was last checked over a minute ago: the backstop for a change
// the post-commit feed lost (main's claim ran it). It returns the next
// page's after, or "" once the pass is over. Each call commits on its own,
// so a family's dirty row is locked only while its page runs.
func (q *Queue) SweepWorkPage(ctx context.Context, after string) (string, error) {
	var cursor any
	if after != "" {
		cursor = after
	}
	limit := q.workPage
	if limit <= 0 {
		limit = SweepWorkPageLimit
	}
	var next *string
	if err := q.pool.QueryRow(ctx, `SELECT app.sweep_live_activity_work_page($1::uuid,$2)::text`, cursor, limit).Scan(&next); err != nil {
		return "", err
	}
	if next == nil {
		return "", nil
	}
	return *next, nil
}

// MaximumDirtyBatch bounds one FlushDirty call (app.dirty_live_activity_families).
const MaximumDirtyBatch = 1000

// FlushDirty dirties those of families (each at its sequence) that hold a
// live registration, and returns how many did.
func (q *Queue) FlushDirty(ctx context.Context, families []string, sequences []uint64) (int, error) {
	if len(families) != len(sequences) || len(families) > MaximumDirtyBatch {
		return 0, errors.New("live_activity_invalid_dirty_batch")
	}
	values := make([]int64, len(sequences))
	for i, sequence := range sequences {
		values[i] = int64(sequence)
	}
	var n int
	err := q.pool.QueryRow(ctx, `SELECT app.dirty_live_activity_families($1::uuid[],$2::bigint[])`, families, values).Scan(&n)
	return n, err
}
