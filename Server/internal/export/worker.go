package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5"
)

// RunOnce expires old material, then processes at most limit queued jobs.
func (w *Worker) RunOnce(ctx context.Context, limit int) error {
	if w == nil || w.options.Authority == nil || w.options.Runtime == nil || w.options.WorkerPool == nil {
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
		err = w.options.Authority.CheckExportSubject(ctx, j.family, j.account, j.epoch)
		if j.instance != w.options.Identity.InstanceID {
			err = authority.ErrExportAuthorizationChanged
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, authority.ErrScopeFailed):
			// A failed audit chain fails that scope closed (503) without
			// changing it: the job is neither served nor destroyed here.
			continue
		case !errors.Is(err, authority.ErrExportAuthorizationChanged):
			return err
		}
		if err = w.failChanged(ctx, j.id); err != nil {
			return err
		}
	}
	return nil
}

// failChanged removes a job's material and fails it permanently with
// export_authorization_changed.
func (w *Worker) failChanged(ctx context.Context, jobID string) error {
	tx, err := w.options.WorkerPool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM public.export_chunks WHERE export_id=$1`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE public.export_jobs SET state='failed',last_code='export_authorization_changed',
		header=NULL,plaintext_bytes=NULL,plaintext_sha256=NULL,lease_id=NULL,lease_until=NULL
		WHERE id=$1 AND state IN ('queued','running','ready')`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	if err := retireJobs(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// sqlRetiredJobs selects at most 100 expired or failed jobs more than 7
// days past expires_at, oldest first (export_jobs_expiry_idx). Neither
// state is claimed, served or reset again (only the pass above turns a
// failed job past expires_at into an expired one), so the selected rows need
// no lock. expires_at is checked against the database clock, as the rest of
// the expiry pass checks it.
const sqlRetiredJobs = `SELECT id::text FROM public.export_jobs WHERE state IN ('expired','failed')
	AND expires_at < now() - interval '7 days' ORDER BY expires_at, id LIMIT 100`

// retireJobs deletes the retired jobs' rows (Phase 3 retention: expired or
// failed export jobs are kept 7 days after expires_at), their chunks first.
// Their chunks are normally long gone: the pass above deletes every expired
// job's chunks.
func retireJobs(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, sqlRetiredJobs)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.export_chunks WHERE export_id = ANY($1::uuid[])`, ids); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM public.export_jobs WHERE id = ANY($1::uuid[])`, ids)
	return err
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
		AND attempt<5 AND expires_at>now() ORDER BY next_attempt_at,export_jobs.id LIMIT 1 FOR UPDATE SKIP LOCKED`).
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
	// The subject is checked before the read and again after it, before the
	// archive is published: CheckExportSubject is its own authority read, so
	// an ownership change committed while the archive was being read is seen
	// by the second check and nothing is published.
	if err := w.options.Authority.CheckExportSubject(ctx, job.FamilyID, job.RequesterAccountID, job.RecoveryEpoch); err != nil {
		return err
	}
	archive, err := w.readArchive(ctx, job)
	if err != nil {
		return err
	}
	var plain bytes.Buffer
	if _, err := EncodeArchive(ctx, &plain, archive); err != nil {
		return err
	}
	if err := w.options.Authority.CheckExportSubject(ctx, job.FamilyID, job.RequesterAccountID, job.RecoveryEpoch); err != nil {
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
}

// readArchive reads a claimed job's archive in one read-only,
// repeatable-read snapshot under the requester's tenant context (the
// job's subject, never a request's), after confirming the job's lease is
// still held on this node's instance and epoch. The grant's revision is the
// family audit seq the snapshot saw; the audit section is read up to it.
func (w *Worker) readArchive(ctx context.Context, job Job) (ArchiveV1, error) {
	tx, err := w.options.Runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ArchiveV1{}, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: job.RequesterAccountID, FamilyID: job.FamilyID}); err != nil {
		return ArchiveV1{}, err
	}
	var live bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.export_jobs j JOIN public.server_state s ON s.singleton
		WHERE j.id=$1 AND j.family_id=$2 AND j.requester_account_id=$3 AND j.server_instance_id=$4
		AND j.recovery_epoch=$5 AND j.lease_id=$6 AND j.state='running' AND j.lease_until>now()
		AND j.expires_at>now() AND s.instance_id=j.server_instance_id AND s.recovery_epoch=j.recovery_epoch)`,
		job.ID, job.FamilyID, job.RequesterAccountID, job.InstanceID, job.RecoveryEpoch, job.LeaseID).Scan(&live); err != nil {
		return ArchiveV1{}, errors.Join(err, ErrUnavailable)
	}
	if !live {
		return ArchiveV1{}, ErrAuthorizationChanged
	}
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT audit_seq FROM public.families WHERE id=$1`, job.FamilyID).Scan(&revision); err != nil {
		return ArchiveV1{}, errors.Join(err, ErrUnavailable)
	}
	grant := access.Grant{AccountID: job.RequesterAccountID, FamilyID: job.FamilyID, Role: "owner", Revision: uint64(revision)}
	return collectArchive(ctx, tx, grant, w.options.Identity, w.options.Now(), w.options.Authority.FamilyAuditPage, careEntities{})
}

func (w *Worker) recordFailure(ctx context.Context, job Job, cause error) error {
	code := "export_generation_failed"
	permanent := false
	if errors.Is(cause, ErrArchiveTooLarge) || errors.Is(cause, ErrTooLarge) {
		code = "export_too_large"
		permanent = true
	}
	if errors.Is(cause, ErrAuthorizationChanged) || errors.Is(cause, authority.ErrExportAuthorizationChanged) {
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
