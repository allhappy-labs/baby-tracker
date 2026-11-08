package authority

import (
	"context"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

// The refresh routes in few round trips (Phase 5 Task 21, R3 and R4). A
// refresh and an acknowledgement were 21% of the requests of a 10,000-family
// run but 47% of its database round trips; each round trip inside a
// transaction holds the connection idle in transaction while the app turns
// around. Their statements are pipelined here in the order the commands ran
// them one by one, and Go evaluates the results in that same order, so every
// refusal, its code and its precedence are unchanged: a pipeline stops at its
// first failed statement, and a refusal decided by an earlier result is
// returned before any later result is looked at.

// RefreshParent is the session a refresh token names, with its device, as
// ResolveRefreshParent read it: unlocked, in a read-only transaction of its
// own. Refresh (Refresh.Parent) takes it instead of looking the session up
// again, and is as safe with it as with the unlocked read it made itself:
// it locks the parent's account and re-reads the parent FOR UPDATE, refusing
// the rotation (session_revoked) unless the locked row still has this
// session's refresh digest, token key, account and device. The runtime role
// cannot change a session's ID, account, device, refresh digest, token key or
// recovery epoch (it holds UPDATE on scope, family_id, role, status and the
// consumption and revocation columns only), so the identity read here is
// still the row's; every other field Refresh uses comes from the locked row.
type RefreshParent struct {
	session      SessionRow
	device       DeviceBinding
	deviceStatus string
}

// Device is the parent's device: its continuity binding and status (active
// or revoked).
func (p *RefreshParent) Device() (DeviceBinding, string) { return p.device, p.deviceStatus }

// sessionDeviceSQL reads sessions with each one's account status and the
// device of the session's account it is bound to; the caller appends the
// WHERE condition on sessions. The joined tables are aliased subqueries, so
// sessionColumns' unqualified names are the session's. A refresh subject
// reads by refresh digest what findParent, the account and device check and
// readDevice read in three statements; an acknowledgement reads by access
// digest (or session ID) what its account, session and device statements
// read.
const sessionDeviceSQL = `SELECT ` + sessionColumns + `, coalesce(a.a_status,''), d.d_id IS NOT NULL, coalesce(d.d_status,''),
		coalesce(d.d_origin,''), coalesce(d.d_key,''), d.d_digest
	FROM sessions
	LEFT JOIN (SELECT id AS a_id, status::text AS a_status FROM accounts) a ON a.a_id=sessions.account_id
	LEFT JOIN (SELECT id AS d_id, account_id AS d_account, status::text AS d_status, canonical_origin AS d_origin,
			continuity_key_id AS d_key, continuity_digest AS d_digest FROM devices) d
		ON d.d_id=sessions.device_id AND d.d_account=sessions.account_id
	WHERE `

// sessionDevice is one sessionDeviceSQL row. accountStatus is "" when the
// session's account row is missing; the device fields are zero when
// deviceFound is false.
type sessionDevice struct {
	session                     SessionRow
	accountStatus               string
	deviceFound                 bool
	deviceStatus, origin, keyID string
	deviceDigest                []byte
}

func scanSessionDevice(row pgx.Row) (sessionDevice, error) {
	var f sessionDevice
	session, err := scanSession(row, &f.accountStatus, &f.deviceFound, &f.deviceStatus, &f.origin, &f.keyID, &f.deviceDigest)
	f.session = session
	return f, err
}

// binding is the device row's continuity binding.
func (f sessionDevice) binding() DeviceBinding {
	b := DeviceBinding{DeviceID: f.session.DeviceID, AccountID: f.session.AccountID, CanonicalOrigin: f.origin, CredentialKeyID: f.keyID}
	copy(b.CredentialDigest[:], f.deviceDigest)
	return b
}

// deviceAuthority is the device part of the account's authority, as
// readAccountAuthority reads it (a device with a malformed digest is not
// found), for accountAuthority.proves.
func (f sessionDevice) deviceAuthority() accountAuthority {
	found := f.deviceFound && len(f.deviceDigest) == 32
	a := accountAuthority{deviceFound: found}
	if found {
		a.device, a.deviceStatus = f.binding(), f.deviceStatus
	}
	return a
}

// readRefreshSubject finds the session the candidates name with its
// account and device, with ResolveRefreshSubject's refusals in its order:
// findParent's, epoch_changed, then session_revoked for an inactive account
// or a missing device.
func (s *Service) readRefreshSubject(ctx context.Context, candidates []KeyedDigest) (sessionDevice, error) {
	var found sessionDevice
	matches := 0
	err := s.readOnce(ctx, "find refresh session", sessionDeviceSQL+`sessions.refresh_digest = ANY($1::bytea[])`, []any{refreshDigests(candidates)},
		func(rows pgx.Rows) error {
			for rows.Next() {
				row, err := scanSessionDevice(rows)
				if err != nil {
					return err
				}
				if _, n := s.candidateKey(row.session, candidates); n > 0 {
					found = row
					matches += n
				}
			}
			return nil
		})
	if err != nil {
		return sessionDevice{}, err
	}
	if _, _, err := parentMatch(found.session, identity.DigestKey{}, matches); err != nil {
		return sessionDevice{}, err
	}
	if found.session.RecoveryEpoch != s.epoch {
		return sessionDevice{}, ErrEpochChanged
	}
	if found.accountStatus != "active" || !found.deviceFound {
		return sessionDevice{}, ErrSessionRevoked
	}
	return found, nil
}

// readOnce runs one authority read as a read-only repeatable-read
// transaction in one round trip: BEGIN, the authority flag, sql and the
// rollback pipelined (readAuthority waits for the rows before it rolls
// back, two round trips). read sees sql's rows; any failure is returned as
// unavailable(what).
func (s *Service) readOnce(ctx context.Context, what, sql string, args []any, read func(pgx.Rows) error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return unavailable("acquire "+what, err)
	}
	tx := &requestTx{conn: conn}
	defer tx.Rollback(context.WithoutCancel(ctx))
	batch := &pgx.Batch{}
	batch.Queue(requestBeginRead)
	batch.Queue(authorityFlagSQL)
	batch.Queue(sql, args...)
	batch.Queue("rollback")
	results := conn.SendBatch(ctx, batch)
	err = func() error {
		for range 2 {
			if _, err := results.Exec(); err != nil {
				return err
			}
		}
		rows, err := results.Query()
		if err != nil {
			return err
		}
		err = read(rows)
		rows.Close()
		if err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = results.Exec()
		return err
	}()
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	releaseIfEnded(tx)
	if err != nil {
		return unavailable(what, err)
	}
	return nil
}

// releaseIfEnded returns tx's connection to the pool without a round trip
// when a pipelined COMMIT or ROLLBACK already ended the transaction (the
// connection is idle); otherwise tx stays open for its deferred Rollback.
func releaseIfEnded(tx *requestTx) {
	if !tx.closed && tx.conn.Conn().PgConn().TxStatus() == 'I' {
		tx.closed = true
		tx.conn.Release()
	}
}

// refreshLocks is what Refresh reads under its locks: the parent's account
// row (accountOK false when it has none), the parent as re-read FOR UPDATE,
// the configured key its refresh token is under and the account's
// authority.
type refreshLocks struct {
	account   accountRow
	accountOK bool
	parent    SessionRow
	key       identity.DigestKey
	authority accountAuthority
}

// lockRefresh opens a rotation's transaction and reads it under its locks,
// in Refresh's order: BEGIN and the authority flag, the parent (findParent,
// unless in.Parent is given) and its epoch, then the parent's account row
// lock, the parent re-read FOR UPDATE (session_revoked unless it is still the
// same credential) and the account's authority. Given in.Parent that is one
// round trip; otherwise findParent's row names the rows to lock, so two. The
// returned tx is nil only when no connection was acquired; otherwise it is
// the caller's to roll back.
func (s *Service) lockRefresh(ctx context.Context, in Refresh) (*requestTx, refreshLocks, error) {
	var found SessionRow
	var key identity.DigestKey
	if in.Parent != nil {
		found = in.Parent.session
		key, _ = s.candidateKey(found, in.ParentRefreshDigestCandidates)
		if found.RecoveryEpoch != s.epoch {
			return nil, refreshLocks{}, ErrEpochChanged
		}
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, refreshLocks{}, beginFailed(err)
	}
	tx := &requestTx{conn: conn}
	batch := &pgx.Batch{}
	batch.Queue(requestBegin)
	batch.Queue(authorityFlagSQL)
	if in.Parent == nil {
		batch.Queue(findParentSQL, refreshDigests(in.ParentRefreshDigestCandidates))
		results := conn.SendBatch(ctx, batch)
		found, key, err = s.beginFindParent(results, in.ParentRefreshDigestCandidates)
		if closeErr := results.Close(); err == nil && closeErr != nil {
			err = unavailable("find refresh session", closeErr)
		}
		if err != nil {
			return tx, refreshLocks{}, err
		}
		if found.RecoveryEpoch != s.epoch {
			return tx, refreshLocks{}, ErrEpochChanged
		}
		batch = &pgx.Batch{}
	}
	accountIDs, err := sortedIDs([]string{found.AccountID})
	if err != nil {
		return tx, refreshLocks{}, err
	}
	batch.Queue(selectAccountsSQL+rowLock, accountIDs)
	batch.Queue(`SELECT `+sessionColumns+` FROM sessions WHERE id=$1 FOR UPDATE`, found.ID)
	batch.Queue(accountAuthoritySQL, found.AccountID, found.DeviceID)
	results := conn.SendBatch(ctx, batch)
	locks, err := readRefreshLocks(results, in.Parent != nil, found, s.epoch)
	if closeErr := results.Close(); err == nil && closeErr != nil {
		err = unavailable("lock refresh", closeErr)
	}
	if err != nil {
		return tx, refreshLocks{}, err
	}
	locks.key = key
	return tx, locks, nil
}

// beginFailed is a failed BEGIN (or authority flag) of an authority
// transaction, as Service.begin reports it.
func beginFailed(err error) error {
	return errors.Join(fmt.Errorf("authority: begin: %w", err), ErrUnavailable)
}

// beginFindParent reads BEGIN, the flag and findParent's rows.
func (s *Service) beginFindParent(results pgx.BatchResults, candidates []KeyedDigest) (SessionRow, identity.DigestKey, error) {
	for range 2 {
		if _, err := results.Exec(); err != nil {
			return SessionRow{}, identity.DigestKey{}, beginFailed(err)
		}
	}
	rows, err := results.Query()
	if err != nil {
		return SessionRow{}, identity.DigestKey{}, fmt.Errorf("authority: find refresh session: %w", errors.Join(err, ErrUnavailable))
	}
	return s.matchParent(rows, candidates)
}

// readRefreshLocks reads the lock batch (after BEGIN and the flag when
// begun): the account lock, the parent FOR UPDATE (still found's credential,
// at epoch), the account's authority.
func readRefreshLocks(results pgx.BatchResults, begun bool, found SessionRow, epoch uint64) (refreshLocks, error) {
	if begun {
		for range 2 {
			if _, err := results.Exec(); err != nil {
				return refreshLocks{}, beginFailed(err)
			}
		}
	}
	rows, err := results.Query()
	if err != nil {
		return refreshLocks{}, fmt.Errorf("authority: lock accounts: %w", err)
	}
	accounts, err := scanAccounts(rows, 1)
	if err != nil {
		return refreshLocks{}, err
	}
	var locks refreshLocks
	locks.account, locks.accountOK = accounts[found.AccountID]
	// Re-read the parent under its lock: it may have been consumed or
	// revoked since the unlocked read.
	parent, err := scanSession(results.QueryRow())
	ok := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return refreshLocks{}, fmt.Errorf("authority: read session: %w", errors.Join(err, ErrUnavailable))
	}
	if !ok || parent.RefreshDigest != found.RefreshDigest || parent.TokenKeyID != found.TokenKeyID || parent.AccountID != found.AccountID ||
		parent.DeviceID != found.DeviceID {
		return refreshLocks{}, ErrSessionRevoked
	}
	// The unlocked read's epoch check, repeated on the locked row (the
	// runtime cannot change a session's epoch; this costs nothing).
	if parent.RecoveryEpoch != epoch {
		return refreshLocks{}, ErrEpochChanged
	}
	locks.parent = parent
	if locks.authority, err = scanAccountAuthority(results.QueryRow(), parent.AccountID); err != nil {
		return refreshLocks{}, err
	}
	return locks, nil
}

// ackReceiptsSQL reads the receipts of account $1's device $2 for attempt $3
// with each child's lineage; ackReceiptsByDigestSQL reads those of the
// account and device of the sessions whose access digest is one of $1, for
// attempt $2, before the session is known (the caller keeps its own).
const (
	ackReceiptsSQL = `SELECT r.child_id::text, coalesce(c.lineage_root_id::text,''), r.account_id::text, r.device_id::text
		FROM refresh_receipts r LEFT JOIN sessions c ON c.id=r.child_id
		WHERE r.account_id=$1 AND r.device_id=$2 AND r.attempt_id=$3 ORDER BY r.parent_id`
	ackReceiptsByDigestSQL = `SELECT r.child_id::text, coalesce(c.lineage_root_id::text,''), r.account_id::text, r.device_id::text
		FROM refresh_receipts r LEFT JOIN sessions c ON c.id=r.child_id
		WHERE (r.account_id, r.device_id) IN (SELECT p.account_id, p.device_id FROM sessions p WHERE p.access_digest = ANY($1::bytea[]))
			AND r.attempt_id=$2 ORDER BY r.parent_id`
)

type ackReceipt struct{ child, lineage, account, device string }

// ackReads is an acknowledgement's two reads, each with its own error: an
// error is looked at only when the checks reach its read.
type ackReads struct {
	sessions                 []sessionDevice
	receipts                 []ackReceipt
	sessionsErr, receiptsErr error
}

// readAck reads the authority flag's result and the acknowledgement's two
// reads, in order.
func readAck(results pgx.BatchResults) ackReads {
	var read ackReads
	if _, err := results.Exec(); err != nil {
		read.sessionsErr = err
		read.receiptsErr = err
		return read
	}
	rows, err := results.Query()
	if err == nil {
		read.sessions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (sessionDevice, error) { return scanSessionDevice(row) })
	}
	if err != nil {
		read.sessionsErr, read.receiptsErr = err, err
		return read
	}
	rows, err = results.Query()
	if err == nil {
		read.receipts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ackReceipt, error) {
			var r ackReceipt
			err := row.Scan(&r.child, &r.lineage, &r.account, &r.device)
			return r, err
		})
	}
	read.receiptsErr = err
	return read
}

// ProveAndAcknowledgeRefresh is AcknowledgeRefresh with the device proof
// evaluated in the transaction: prove is called with the caller's device row
// binding after the session checks, so the acknowledgement is one
// transaction. A false (or nil) prove is device_continuity_invalid. It is two
// round trips: BEGIN, the authority flag, the caller's session with its
// account status and device, and the attempt's receipts, pipelined; then the
// DELETE with the COMMIT, or the rollback.
func (s *Service) ProveAndAcknowledgeRefresh(ctx context.Context, p access.Principal, attemptID string, prove func(stored DeviceBinding) bool) error {
	if !validID(attemptID) {
		return ErrInvalidRequest
	}
	if p.RecoveryEpoch != s.epoch {
		return ErrEpochChanged
	}
	if !validPrincipal(p) {
		return ErrSessionRevoked
	}
	now := s.at(time.Time{})
	if !now.Before(p.AccessExpiresAt) {
		return ErrAccessExpired
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return beginFailed(err)
	}
	tx := &requestTx{conn: conn}
	defer tx.Rollback(context.WithoutCancel(ctx))
	batch := &pgx.Batch{}
	batch.Queue(requestBegin)
	batch.Queue(authorityFlagSQL)
	batch.Queue(sessionDeviceSQL+`sessions.id=$1`, p.SessionID)
	batch.Queue(ackReceiptsSQL, p.AccountID, p.DeviceID, attemptID)
	results := conn.SendBatch(ctx, batch)
	var read ackReads
	_, err = results.Exec()
	if err == nil {
		read = readAck(results)
	}
	_ = results.Close() // every result was read; their errors are in err and read
	if err != nil {
		return beginFailed(err)
	}
	return s.acknowledge(ctx, tx, p, attemptID, prove, now, read)
}

// AuthenticateAndAcknowledgeRefresh is Authenticate followed by
// ProveAndAcknowledgeRefresh for the principal it resolves, in one
// transaction of two round trips: BEGIN, app.request_authority by the access
// digests, the authority flag (request_authority restores the flag it
// raised), the session with its account status and device and the attempt's
// receipts, pipelined; then the DELETE with the COMMIT, or the rollback.
// Authenticate's refusals come first, in its order, then
// ProveAndAcknowledgeRefresh's (an invalid attempt ID is invalid_request
// after Authenticate's refusals); the session is re-read in the transaction
// as ProveAndAcknowledgeRefresh re-reads it.
func (s *Service) AuthenticateAndAcknowledgeRefresh(ctx context.Context, accessDigests []KeyedDigest, attemptID string, prove func(stored DeviceBinding) bool) error {
	args, ok := digestArgs(accessDigests)
	if !ok {
		return ErrSessionRevoked
	}
	if !validID(attemptID) {
		if _, err := s.Authenticate(ctx, accessDigests); err != nil {
			return err
		}
		return ErrInvalidRequest
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return unavailable("authenticate", err)
	}
	tx := &requestTx{conn: conn}
	defer tx.Rollback(context.WithoutCancel(ctx))
	batch := &pgx.Batch{}
	batch.Queue(ackBegin)
	batch.Queue(requestAuthoritySQL, args, nil, false, int64(s.epoch))
	batch.Queue(authorityFlagSQL)
	batch.Queue(sessionDeviceSQL+`sessions.access_digest = ANY($1::bytea[])`, args)
	batch.Queue(ackReceiptsByDigestSQL, args, attemptID)
	results := conn.SendBatch(ctx, batch)
	if _, beginErr := results.Exec(); beginErr != nil {
		// A database that refuses a read-write transaction (a hot
		// standby) refused the BEGIN before the bearer was looked at.
		// The two calls authenticated first, in an implicit read-only
		// transaction, and refused BEGIN only after that: authenticate the
		// same way, so a refused bearer keeps its code, then report the
		// BEGIN.
		_ = results.Close()
		_ = tx.Rollback(context.WithoutCancel(ctx))
		if _, err := s.Authenticate(ctx, accessDigests); err != nil {
			return err
		}
		return beginFailed(beginErr)
	}
	p, now, read, err := s.readAuthenticatedAck(results, accessDigests)
	_ = results.Close() // the results read have their errors in err and read; the rest are not needed
	if err != nil {
		return err
	}
	return s.acknowledge(ctx, tx, p, attemptID, prove, now, read)
}

// ackBegin opens AuthenticateAndAcknowledgeRefresh's transaction (a
// variable only so a test can make the BEGIN fail).
var ackBegin = requestBegin

// readAuthenticatedAck reads request_authority (after BEGIN), applies
// Authenticate's refusals and ProveAndAcknowledgeRefresh's principal checks,
// then reads the acknowledgement's reads.
func (s *Service) readAuthenticatedAck(results pgx.BatchResults, digests []KeyedDigest) (access.Principal, time.Time, ackReads, error) {
	row, err := scanRequest(results.QueryRow())
	if err != nil {
		return access.Principal{}, time.Time{}, ackReads{}, unavailable("authenticate", err)
	}
	now := s.at(time.Time{})
	if err := s.checkAuthenticated(row, digests, now); err != nil {
		return access.Principal{}, time.Time{}, ackReads{}, err
	}
	p := row.principal()
	switch {
	case p.RecoveryEpoch != s.epoch:
		return access.Principal{}, time.Time{}, ackReads{}, ErrEpochChanged
	case !validPrincipal(p):
		return access.Principal{}, time.Time{}, ackReads{}, ErrSessionRevoked
	case !now.Before(p.AccessExpiresAt):
		return access.Principal{}, time.Time{}, ackReads{}, ErrAccessExpired
	}
	return p, now, readAck(results), nil
}

// acknowledge is the acknowledgement's checks over its reads, in
// ProveAndAcknowledgeRefresh's order, then the receipt's DELETE pipelined
// with the COMMIT. With no receipt it changes nothing (the caller rolls
// back).
func (s *Service) acknowledge(ctx context.Context, tx *requestTx, p access.Principal, attemptID string, prove func(DeviceBinding) bool, now time.Time,
	read ackReads) error {
	if read.sessionsErr != nil {
		return unavailable("read session", read.sessionsErr)
	}
	var found sessionDevice
	ok := false
	for _, row := range read.sessions {
		if row.session.ID == p.SessionID {
			found, ok = row, true
		}
	}
	// principalSession (ResolveSessionActor): the account is active, the
	// session is p's as it was authenticated and its device row exists.
	session := found.session
	if !ok || found.accountStatus != "active" || session.AccountID != p.AccountID || session.DeviceID != p.DeviceID ||
		session.RecoveryEpoch != p.RecoveryEpoch || session.IssuedSeq != p.SecurityRevision || !found.deviceFound {
		return ErrSessionRevoked
	}
	if session.Status != "active" {
		return ErrSessionRevoked
	}
	// The policy deadline, with the one-year lineage cap applied whatever the
	// mode (AcknowledgeRefresh has no mode). Self-hosted this is exactly
	// Refresh's effective deadline; hosted it equals deadline_at whenever the
	// hosted absolute age is at most a year (default 30 days), and is only
	// stricter otherwise, which merely keeps a receipt until cleanup.
	if !now.Before(effectiveDeadline(session, ModeSelfHosted)) || !now.Before(session.RefreshExpiresAt) {
		return ErrReauthenticationRequired
	}
	if !now.Before(session.AccessExpiresAt) {
		return ErrAccessExpired
	}
	if !found.deviceAuthority().proves(p.AccountID, p.DeviceID, prove) {
		return ErrDeviceContinuityInvalid
	}
	if read.receiptsErr != nil {
		return fmt.Errorf("authority: read refresh receipts: %w", errors.Join(read.receiptsErr, ErrUnavailable))
	}
	receipts := 0
	for _, r := range read.receipts {
		if r.account != p.AccountID || r.device != p.DeviceID {
			continue
		}
		receipts++
		if r.lineage != session.LineageRootID {
			return ErrSessionRevoked
		}
	}
	if receipts == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM refresh_receipts r USING sessions c WHERE r.child_id=c.id AND r.account_id=$1 AND r.device_id=$2
			AND r.attempt_id=$3 AND c.lineage_root_id=$4`, p.AccountID, p.DeviceID, attemptID, session.LineageRootID)
	batch.Queue("commit")
	results := tx.conn.SendBatch(ctx, batch)
	if _, err := results.Exec(); err != nil {
		_ = results.Close()
		return fmt.Errorf("authority: delete refresh receipt: %w", errors.Join(err, ErrUnavailable))
	}
	tag, err := results.Exec()
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	if err == nil && tag.String() == "ROLLBACK" {
		err = pgx.ErrTxCommitRollback
	}
	releaseIfEnded(tx)
	if err != nil {
		return fmt.Errorf("authority: commit acknowledgement: %w", errors.Join(err, ErrUnavailable))
	}
	return nil
}
