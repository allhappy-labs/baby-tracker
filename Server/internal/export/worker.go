package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
	"github.com/jackc/pgx/v5"
)

// RunOnce expires old material, then processes at most limit queued jobs.
func (w *Worker) RunOnce(ctx context.Context, limit int) error {
	if w == nil || w.guard == nil || w.options.WorkerPool == nil {
		return ErrUnavailable
	}
	if limit < 1 || limit > 4 {
		return ErrInvalidRequest
	}
	if err := w.expire(ctx); err != nil {
		return err
	}
	if err := w.invalidateChanged(ctx); err != nil {
		return err
	}
	for i := 0; i < limit; i++ {
		job, ok, err := w.claim(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := w.process(ctx, job); err != nil {
			if failure := w.recordFailure(ctx, job, err); failure != nil {
				return errors.Join(err, failure)
			}
			return err
		}
	}
	return nil
}

// Reconcile authority changes before claiming work. Request endpoints refuse
// former owners immediately; this pass also removes their encrypted material.
func (w *Worker) invalidateChanged(ctx context.Context) error {
	rows, err := w.options.WorkerPool.Query(ctx, `SELECT id::text,family_id::text,requester_account_id::text,
		server_instance_id::text,recovery_epoch FROM public.export_jobs WHERE state IN ('queued','running','ready')`)
	if err != nil {
		return err
	}
	type subject struct {
		id, family, account, instance string
		epoch                         uint64
	}
	var jobs []subject
	for rows.Next() {
		var j subject
		if err := rows.Scan(&j.id, &j.family, &j.account, &j.instance, &j.epoch); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		err = w.options.Security.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
			state := snapshot.View
			account, accountOK := state.Account(j.account)
			slot, slotOK := state.FamilySlot(j.account)
			family, familyOK := state.Family(j.family)
			member, memberOK := state.Membership(j.family, j.account)
			valid := j.instance == w.options.Identity.InstanceID && j.epoch == snapshot.RecoveryEpoch &&
				accountOK && account.Status == "active" && slotOK && slot.FamilyID == j.family && slot.State == "active" &&
				familyOK && family.Status == "active" && family.OwnerAccountID == j.account && memberOK && member.Status == "active" && member.Role == "owner"
			if valid {
				return nil
			}
			tx, err := w.options.WorkerPool.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `DELETE FROM public.export_chunks WHERE export_id=$1`, j.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE public.export_jobs SET state='failed',last_code='export_authorization_changed',
				header=NULL,plaintext_bytes=NULL,plaintext_sha256=NULL,lease_id=NULL,lease_until=NULL
				WHERE id=$1 AND state IN ('queued','running','ready')`, j.id); err != nil {
				return err
			}
			return tx.Commit(ctx)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) expire(ctx context.Context) error {
	tx, err := w.options.WorkerPool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM public.export_chunks WHERE export_id IN
		(SELECT id FROM public.export_jobs WHERE expires_at<=now())`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE public.export_jobs SET state='expired',header=NULL,plaintext_bytes=NULL,plaintext_sha256=NULL,
		lease_id=NULL,lease_until=NULL WHERE expires_at<=now() AND state<>'expired'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE public.export_jobs SET state='failed',last_code='export_retry_exhausted',
		lease_id=NULL,lease_until=NULL WHERE state='running' AND lease_until<=now() AND attempt>=5`); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM public.export_rate_reservations WHERE accepted_at<now()-interval '6 hours'`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (w *Worker) claim(ctx context.Context) (Job, bool, error) {
	var job Job
	tx, err := w.options.WorkerPool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return job, false, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT id::text,family_id::text,requester_account_id::text,request_id::text,
		server_instance_id::text,recovery_epoch,security_revision,state,created_at,expires_at,attempt
		FROM public.export_jobs WHERE ((state='queued' AND next_attempt_at<=now()) OR (state='running' AND lease_until<=now()))
		AND attempt<5 AND expires_at>now() ORDER BY next_attempt_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`).
		Scan(&job.ID, &job.FamilyID, &job.RequesterAccountID, &job.RequestID, &job.InstanceID, &job.RecoveryEpoch, &job.SecurityRevision,
			&job.State, &job.CreatedAt, &job.ExpiresAt, &job.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.export_chunks WHERE export_id=$1`, job.ID); err != nil {
		return Job{}, false, err
	}
	lease, err := newUUID()
	if err != nil {
		return Job{}, false, err
	}
	job.Attempt++
	job.LeaseID = lease
	job.State = "running"
	if _, err := tx.Exec(ctx, `UPDATE public.export_jobs SET state='running',attempt=$2,lease_id=$3,
		lease_until=now()+interval '60 seconds',header=NULL,plaintext_bytes=NULL,plaintext_sha256=NULL,last_code=NULL
		WHERE id=$1`, job.ID, job.Attempt, job.LeaseID); err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func (w *Worker) process(parent context.Context, job Job) error {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	if job.InstanceID != w.options.Identity.InstanceID || job.RecoveryEpoch != w.options.Identity.RecoveryEpoch {
		return ErrAuthorizationChanged
	}
	key := w.options.Keys[w.options.ActiveKeyID]
	return w.guard.WithExportSnapshot(ctx, security.ExportSubject{JobID: job.ID, LeaseID: job.LeaseID, FamilyID: job.FamilyID,
		RequesterAccountID: job.RequesterAccountID, InstanceID: job.InstanceID, RecoveryEpoch: job.RecoveryEpoch}, func(readTx pgx.Tx, grant access.Grant) error {
		archive, err := collectArchive(ctx, readTx, grant, w.options.Identity, w.options.Now())
		if err != nil {
			return err
		}
		var plain bytes.Buffer
		if _, err := EncodeArchive(ctx, &plain, archive); err != nil {
			return err
		}
		writeTx, err := w.options.WorkerPool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		defer writeTx.Rollback(ctx)
		core := HeaderCore{ExportID: job.ID, InstanceID: job.InstanceID, FamilyID: job.FamilyID, RequesterAccountID: job.RequesterAccountID,
			CreatedAt: job.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000Z"), ExpiresAt: job.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000000Z"), KeyID: w.options.ActiveKeyID}
		header, err := SealArchive(ctx, core, key[:], &plain, dbChunkSink{tx: writeTx, job: job})
		if err != nil {
			return err
		}
		raw, err := json.Marshal(header)
		if err != nil {
			return err
		}
		result, err := writeTx.Exec(ctx, `UPDATE public.export_jobs SET state='ready',header=$4::jsonb,plaintext_bytes=$5,
			plaintext_sha256=$6,lease_id=NULL,lease_until=NULL WHERE id=$1 AND attempt=$2 AND lease_id=$3
			AND state='running' AND lease_until>now() AND expires_at>now()`, job.ID, job.Attempt, job.LeaseID, raw, header.Core.PlaintextBytes, header.Core.PlaintextSHA256)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrAuthorizationChanged
		}
		return writeTx.Commit(ctx)
	})
}

func (w *Worker) recordFailure(ctx context.Context, job Job, cause error) error {
	code := "export_generation_failed"
	permanent := false
	if errors.Is(cause, ErrArchiveTooLarge) || errors.Is(cause, ErrTooLarge) {
		code = "export_too_large"
		permanent = true
	}
	if errors.Is(cause, ErrAuthorizationChanged) || security.Code(cause) == "export_authorization_changed" {
		code = "export_authorization_changed"
		permanent = true
	}
	if errors.Is(cause, ErrEnvelopeIntegrity) {
		code = "export_integrity"
		permanent = true
	}
	if job.Attempt >= 5 {
		code = "export_retry_exhausted"
		permanent = true
	}
	tx, err := w.options.WorkerPool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM public.export_chunks WHERE export_id=$1 AND attempt=$2`, job.ID, job.Attempt); err != nil {
		return err
	}
	state := "queued"
	if permanent {
		state = "failed"
	}
	// Retry delay grows with each attempt and is bounded by five minutes.
	delay := time.Duration(1<<min(job.Attempt, 8)) * time.Second
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	result, err := tx.Exec(ctx, `UPDATE public.export_jobs SET state=$4,last_code=$5,lease_id=NULL,lease_until=NULL,
		next_attempt_at=now()+$6::interval WHERE id=$1 AND attempt=$2 AND lease_id=$3 AND state='running'`,
		job.ID, job.Attempt, job.LeaseID, state, code, fmt.Sprintf("%f seconds", delay.Seconds()))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return nil
	}
	return tx.Commit(ctx)
}
