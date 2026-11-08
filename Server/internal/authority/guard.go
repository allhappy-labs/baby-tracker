package authority

import (
	"context"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Request transactions open with BEGIN and their one authority statement
// pipelined in a single round trip. pgx's own transactions cannot do that
// (Conn.BeginTx waits for BEGIN), so requestTx is a pgx.Tx over a pooled
// connection whose transaction was begun by that batch.
const (
	requestBegin     = `BEGIN ISOLATION LEVEL READ COMMITTED READ WRITE`
	requestBeginRead = `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`
)

// requestTx is a real (top-level) transaction on one acquired pool
// connection; Commit and Rollback end it and release the connection, which
// the pool destroys if the transaction did not end cleanly. Nested
// transactions and large objects are not supported (see the limits below).
type requestTx struct {
	conn   *pgxpool.Conn
	closed bool
	// atCommit is what PipelineWithCommit queued, sent with the COMMIT.
	atCommit []pipelinedWithCommit
}

type pipelinedWithCommit struct {
	queue func(*pgx.Batch)
	read  func(pgx.BatchResults) error
}

// CommitPipeline is a request transaction's commit seam (WithRequestTx,
// WithFamilyTx and WithAccountTx hand run one): statements that go with the
// COMMIT, in its round trip, instead of in a round trip of their own before
// it. It is for transactions that wrote nothing before them (a pull), and
// it enforces that: the batch first checks, in the server, that the
// transaction has no transaction id yet (pipelineReadOnlySQL: any write or
// row lock assigns one), and a transaction that has one fails there, so
// neither the queued statements nor the COMMIT run and it rolls back
// (ErrPipelineAfterWrite). A caller holding any other pgx.Tx sends its
// statements itself.
//
// read runs on the results after the COMMIT was sent. Its refusal is
// therefore a refusal of a committed transaction, and safe only because
// nothing before the queued statements wrote: the queued statements must
// be ones whose effect may stand when read refuses (an INSERT … ON
// CONFLICT DO NOTHING that read refuses only when it inserted nothing, a
// read). Never queue a write that read's refusal must undo.
type CommitPipeline interface {
	// PipelineWithCommit queues what queue adds to the batch, to run just
	// before the COMMIT in its round trip; read then reads exactly those
	// statements' results, in order. A statement's error skips the COMMIT,
	// and the transaction rolls back. read's error, a statement's or its
	// own, is the request's error, returned as is. Nothing is sent if the
	// transaction rolls back instead.
	PipelineWithCommit(queue func(*pgx.Batch), read func(pgx.BatchResults) error)
}

// ErrPipelineAfterWrite refuses a commit pipeline in a transaction that
// already wrote (CommitPipeline); the transaction rolls back.
var ErrPipelineAfterWrite = errors.New("authority: statements pipelined with the commit of a transaction that already wrote")

// pipelineReadOnlySQL fails (22012) when the transaction already has a
// transaction id, i.e. wrote or locked a row, and selects 1 otherwise. The
// divisor is volatile, so the planner cannot fold the division.
const pipelineReadOnlySQL = `SELECT 1 / (pg_catalog.pg_current_xact_id_if_assigned() IS NULL)::int`

var _ CommitPipeline = (*requestTx)(nil)

// PipelineWithCommit implements CommitPipeline; after Commit or Rollback it
// does nothing.
func (t *requestTx) PipelineWithCommit(queue func(*pgx.Batch), read func(pgx.BatchResults) error) {
	if t.closed {
		return
	}
	t.atCommit = append(t.atCommit, pipelinedWithCommit{queue: queue, read: read})
}

// Limits of requestTx, for request-path callers: Begin (nested savepoint
// transactions) returns errNestedRequestTx; LargeObjects panics with
// errLargeObjectsUnsupported; Conn after Commit or Rollback panics with
// errRequestTxReleased (the connection belongs to the pool again).
var (
	errNestedRequestTx         = errors.New("authority: nested transactions are not supported in a request transaction")
	errLargeObjectsUnsupported = errors.New("authority: large objects are not supported in a request transaction")
	errRequestTxReleased       = errors.New("authority: request transaction connection used after commit or rollback")
)

func (t *requestTx) Begin(context.Context) (pgx.Tx, error) {
	if t.closed {
		return nil, pgx.ErrTxClosed
	}
	return nil, errNestedRequestTx
}

// Commit commits and releases the connection (pgx dbTx.Commit semantics: a
// commit that rolled back is pgx.ErrTxCommitRollback), with what
// PipelineWithCommit queued in the commit's round trip.
func (t *requestTx) Commit(ctx context.Context) error {
	readErr, commitErr := t.commit(ctx)
	return errors.Join(readErr, commitErr)
}

// commit is Commit with the queued statements' read error (readErr) apart
// from the commit's own.
func (t *requestTx) commit(ctx context.Context) (readErr, commitErr error) {
	if t.closed {
		return nil, pgx.ErrTxClosed
	}
	t.closed = true
	defer t.conn.Release()
	var tag pgconn.CommandTag
	if len(t.atCommit) == 0 {
		tag, commitErr = t.conn.Exec(ctx, "commit")
	} else {
		batch := &pgx.Batch{}
		batch.Queue(pipelineReadOnlySQL)
		for _, p := range t.atCommit {
			p.queue(batch)
		}
		batch.Queue("commit")
		results := t.conn.SendBatch(ctx, batch)
		if _, err := results.Exec(); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "22012" {
				err = fmt.Errorf("%w (%w)", ErrPipelineAfterWrite, err)
			}
			readErr = err
		} else {
			for _, p := range t.atCommit {
				if err := p.read(results); err != nil && readErr == nil {
					readErr = err
				}
			}
		}
		tag, commitErr = results.Exec()
		if err := results.Close(); commitErr == nil {
			commitErr = err
		}
		// A statement's error skipped the COMMIT: the transaction is still
		// open (failed), so end it before the connection goes back.
		if t.conn.Conn().PgConn().TxStatus() != 'I' {
			_, _ = t.conn.Exec(context.WithoutCancel(ctx), "rollback")
		}
	}
	if commitErr == nil && tag.String() == "ROLLBACK" {
		commitErr = pgx.ErrTxCommitRollback
	}
	return readErr, commitErr
}

// Rollback rolls back and releases the connection; a failed rollback leaves
// the connection outside the idle state, so the pool destroys it.
func (t *requestTx) Rollback(ctx context.Context) error {
	if t.closed {
		return pgx.ErrTxClosed
	}
	t.closed = true
	defer t.conn.Release()
	_, err := t.conn.Exec(ctx, "rollback")
	return err
}

func (t *requestTx) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error) {
	if t.closed {
		return 0, pgx.ErrTxClosed
	}
	return t.conn.CopyFrom(ctx, table, columns, src)
}

func (t *requestTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	if t.closed {
		return closedBatch{}
	}
	return t.conn.SendBatch(ctx, b)
}

// LargeObjects is unsupported (see requestTx); it panics with
// errLargeObjectsUnsupported rather than return a value that would
// nil-dereference on use.
func (t *requestTx) LargeObjects() pgx.LargeObjects { panic(errLargeObjectsUnsupported) }

func (t *requestTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	if t.closed {
		return nil, pgx.ErrTxClosed
	}
	return t.conn.Conn().Prepare(ctx, name, sql)
}

func (t *requestTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if t.closed {
		return pgconn.CommandTag{}, pgx.ErrTxClosed
	}
	return t.conn.Exec(ctx, sql, args...)
}

func (t *requestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if t.closed {
		return nil, pgx.ErrTxClosed
	}
	return t.conn.Query(ctx, sql, args...)
}

func (t *requestTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.closed {
		return errRow{pgx.ErrTxClosed}
	}
	return t.conn.QueryRow(ctx, sql, args...)
}

// Conn returns the transaction's connection; after Commit or Rollback the
// connection is back in the pool, so Conn panics with errRequestTxReleased.
func (t *requestTx) Conn() *pgx.Conn {
	if t.closed {
		panic(errRequestTxReleased)
	}
	return t.conn.Conn()
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type closedBatch struct{}

func (closedBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, pgx.ErrTxClosed }
func (closedBatch) Query() (pgx.Rows, error)         { return nil, pgx.ErrTxClosed }
func (closedBatch) QueryRow() pgx.Row                { return errRow{pgx.ErrTxClosed} }
func (closedBatch) Close() error                     { return pgx.ErrTxClosed }

// beginRequest acquires a connection from pool and sends begin and the
// authority statement sql in one pipelined round trip (a statement new to
// the connection costs one prepare round trip first); scan reads the
// statement's row. On any failure the transaction is rolled back and the
// connection released.
func beginRequest(ctx context.Context, pool *pgxpool.Pool, begin string, scan func(pgx.Row) error, sql string, args ...any) (*requestTx, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, unavailable("acquire request connection", err)
	}
	tx := &requestTx{conn: conn}
	batch := &pgx.Batch{}
	batch.Queue(begin)
	batch.Queue(sql, args...)
	results := conn.SendBatch(ctx, batch)
	_, beginErr := results.Exec()
	var scanErr error
	if beginErr == nil {
		scanErr = scan(results.QueryRow())
	}
	if err := errors.Join(beginErr, scanErr, results.Close()); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, unavailable("begin request", err)
	}
	return tx, nil
}

// finish runs run inside tx and commits; any error or panic rolls back. An
// error of what run pipelined with the commit (CommitPipeline) is returned
// as is, as run's own would be.
func finish(ctx context.Context, tx *requestTx, run func() error) error {
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := run(); err != nil {
		return err
	}
	readErr, commitErr := tx.commit(ctx)
	if readErr != nil {
		return readErr
	}
	if commitErr != nil {
		return unavailable("commit request", commitErr)
	}
	return nil
}

// authorityFlagSQL raises the transaction-local authority flag
// (app.authority_context()).
const authorityFlagSQL = `SELECT set_config('app.authority','1',true)`

// readAuthority runs one authority read in a read-only repeatable-read
// transaction on the runtime pool: BEGIN, the authority flag and sql
// pipelined in one round trip, read over its rows, then the rollback (two
// round trips in all). read's error is returned as is.
func (s *Service) readAuthority(ctx context.Context, what, sql string, args []any, read func(pgx.Rows) error) error {
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
	results := conn.SendBatch(ctx, batch)
	defer results.Close()
	for range 2 {
		if _, err := results.Exec(); err != nil {
			return unavailable(what, err)
		}
	}
	rows, err := results.Query()
	if err != nil {
		return unavailable(what, err)
	}
	defer rows.Close()
	if err := read(rows); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return unavailable(what, err)
	}
	return nil
}

// refuse rolls back a transaction whose authorization failed and returns err.
func refuse(ctx context.Context, tx *requestTx, err error) error {
	_ = tx.Rollback(context.WithoutCancel(ctx))
	return err
}

// requestByPrincipal opens a request transaction for an authenticated
// principal: BEGIN and app.request_authority by session ID in one round trip.
// The principal must be this epoch's (epoch_changed) and well formed; the
// session must still be the one authenticated, live, and before its deadline
// (checkLiveSession). Revocation after commit wins: the session row is read
// in this transaction, so a revocation committed before it began refuses it.
func (s *Service) requestByPrincipal(ctx context.Context, p access.Principal, begin string, family bool) (*requestTx, requestRow, error) {
	if p.RecoveryEpoch != s.epoch {
		return nil, requestRow{}, ErrEpochChanged
	}
	if !validPrincipal(p) {
		return nil, requestRow{}, ErrSessionRevoked
	}
	var row requestRow
	tx, err := beginRequest(ctx, s.pool, begin, func(r pgx.Row) (err error) { row, err = scanRequest(r); return err },
		requestAuthoritySQL, nil, p.SessionID, family, int64(s.epoch))
	if err != nil {
		return nil, requestRow{}, err
	}
	if !row.matchesPrincipal(p) {
		return nil, requestRow{}, refuse(ctx, tx, ErrSessionRevoked)
	}
	if err := s.checkLiveSession(row, s.at(time.Time{})); err != nil {
		return nil, requestRow{}, refuse(ctx, tx, err)
	}
	return tx, row, nil
}

// WithAccountTx runs run in a read-committed transaction with the tenant
// context of p's account (app.family_id cleared), after BEGIN and the
// session re-check in one round trip (security/transaction.go
// WithAccountTransaction): a family_recovery session is refused
// (session_revoked).
func (s *Service) WithAccountTx(ctx context.Context, p access.Principal, run func(pgx.Tx) error) error {
	if run == nil {
		return ErrUnavailable
	}
	tx, row, err := s.requestByPrincipal(ctx, p, requestBegin, false)
	if err != nil {
		return err
	}
	if row.scope != "account" && row.scope != "family" {
		return refuse(ctx, tx, ErrSessionRevoked)
	}
	return finish(ctx, tx, func() error { return run(tx) })
}

// FamilyGuard is the care-data request seam (sync, importer): one guarded
// transaction per call, read-committed or (the read form) a repeatable-read
// read-only snapshot, the family and role taken from the grant. *Service
// implements it; tests wrap it to inject faults.
type FamilyGuard interface {
	WithFamilyTx(ctx context.Context, p access.Principal, action string, run func(pgx.Tx, access.Grant) error) error
	WithFamilyReadTx(ctx context.Context, p access.Principal, action string, run func(pgx.Tx, access.Grant) error) error
}

var _ FamilyGuard = (*Service)(nil)

// WithFamilyTx runs run in a read-committed transaction with the tenant
// context of p's account and family once family policy grants action
// (familyGrant), after BEGIN and the authority read in one round trip. It
// locks nothing: a mutating caller locks the family row itself.
func (s *Service) WithFamilyTx(ctx context.Context, p access.Principal, action string, run func(pgx.Tx, access.Grant) error) error {
	return s.withFamily(ctx, p, action, requestBegin, run)
}

// WithFamilyReadTx is WithFamilyTx in a repeatable-read, read-only
// transaction: the authority read and every read of run share one snapshot.
func (s *Service) WithFamilyReadTx(ctx context.Context, p access.Principal, action string, run func(pgx.Tx, access.Grant) error) error {
	return s.withFamily(ctx, p, action, requestBeginRead, run)
}

func (s *Service) withFamily(ctx context.Context, p access.Principal, action, begin string, run func(pgx.Tx, access.Grant) error) error {
	if run == nil {
		return ErrUnavailable
	}
	tx, row, err := s.requestByPrincipal(ctx, p, begin, true)
	if err != nil {
		return err
	}
	grant, err := s.familyGrant(row, action, s.at(time.Time{}))
	if err != nil {
		return refuse(ctx, tx, err)
	}
	return finish(ctx, tx, func() error { return run(tx, grant) })
}

// anyRoleAllowed reports whether some role may perform action.
func (s *Service) anyRoleAllowed(action string) bool {
	return s.allowed("owner", action) || s.allowed("caregiver", action) || s.allowed("viewer", action)
}

// familyGrant is family.Authorize (family/service.go:134-189) over a family
// request row, with the session row standing in for the membership: a
// family-scope session carries its live role (role changes update it, and
// removal, deletion and identity loss revoke it in the same transaction). An
// account-scope session (signed in, not yet refreshed) is resolved through
// the account's slot and membership, as the legacy resolver did.
//
// Order: an action no role may perform (membership_revoked); the family's
// audit chain failed (scope_failed, 503); a family_recovery session or slot
// (recovery_only); an account-scope session without a slot
// (family_unavailable); then, unless the account, the family and the
// membership are active and the role is allowed: a non-owner member's
// disallowed action (read_only_caregiver for a viewer where a caregiver
// would be allowed, else owner_required), otherwise membership_revoked; the
// family_sync entitlement, when the node or the family requires it and the
// action is not exempt (entitlement_required).
func (s *Service) familyGrant(row requestRow, action string, now time.Time) (access.Grant, error) {
	if !s.anyRoleAllowed(action) {
		return access.Grant{}, ErrMembershipRevoked
	}
	if row.familyID != "" && row.familyChainFailed {
		return access.Grant{}, ErrScopeFailed
	}
	var familyID, role, memberStatus string
	switch row.scope {
	case "family_recovery":
		return access.Grant{}, ErrRecoveryOnly
	case "family":
		familyID, role, memberStatus = row.sessionFamilyID, row.sessionRole, "active"
	case "account":
		if row.slotFamilyID == "" {
			return access.Grant{}, ErrFamilyUnavailable
		}
		if row.slotState == "family_recovery" {
			return access.Grant{}, ErrRecoveryOnly
		}
		familyID, role, memberStatus = row.slotFamilyID, row.memberRole, row.memberStatus
	default:
		return access.Grant{}, ErrSessionRevoked
	}
	if row.accountStatus != "active" || row.familyID != familyID || row.familyStatus != "active" || memberStatus != "active" || !s.allowed(role, action) {
		if role != "" && role != "owner" && !s.allowed(role, action) {
			if role == "viewer" && s.allowed("caregiver", action) {
				return access.Grant{}, ErrReadOnlyCaregiver
			}
			return access.Grant{}, ErrOwnerRequired
		}
		return access.Grant{}, ErrMembershipRevoked
	}
	if !s.entitlementExempt(action) && (s.requireFamilySyncEntitlement || row.entitlementRequired) && !entitled(row.entitlementStatus, row.entitlementExpiresAt, now) {
		return access.Grant{}, ErrEntitlementRequired
	}
	return access.Grant{AccountID: row.accountID, FamilyID: familyID, Role: role, Revision: row.familyAuditSeq}, nil
}

// entitled reports an active family_sync entitlement unexpired at now.
func entitled(status string, expiresAt *time.Time, now time.Time) bool {
	return status == "active" && (expiresAt == nil || now.Before(*expiresAt))
}

// LiveLease is a live connection's authorization: the principal it
// authenticated as and the grant it was admitted with.
type LiveLease struct {
	Principal access.Principal
	Grant     access.Grant
}

// checkManySQL reads every lease's session with its account, family and
// family_sync entitlement in one statement.
const checkManySQL = `SELECT l.ord, s.status::text, s.account_id::text, s.device_id::text, s.recovery_epoch, s.issued_seq, s.scope::text,
	coalesce(s.family_id::text,''), coalesce(s.role::text,''), s.deadline_at, s.lineage_started_at, a.status::text, a.chain_failed,
	f.status::text, f.audit_seq, f.chain_failed, f.entitlement_required, coalesce(e.status::text,''), e.expires_at
	FROM unnest($1::uuid[]) WITH ORDINALITY AS l(id, ord)
	JOIN sessions s ON s.id = l.id
	JOIN accounts a ON a.id = s.account_id
	JOIN families f ON f.id = s.family_id
	LEFT JOIN family_entitlements e ON e.family_id = f.id AND e.entitlement_key = 'family_sync'`

// CheckMany reports, per lease, whether its live connection may stay open
// (realtime/authorizer.go AuthorityAuthorizer.CheckMany): one read-only
// transaction and one statement for every lease. A lease is allowed only if
// its principal is a family session of this epoch whose access has not
// expired and whose grant names its own account and session family at a
// revision the family has reached, and the session is still that one,
// active, with the grant's role, before its deadline, on an active account
// and an active family whose chains have not failed, entitled when the node
// or the family requires it.
func (s *Service) CheckMany(ctx context.Context, leases []LiveLease) ([]bool, error) {
	allowed := make([]bool, len(leases))
	now := s.at(time.Time{})
	ids := make([]string, len(leases))
	for i, l := range leases {
		p, g := l.Principal, l.Grant
		if validPrincipal(p) && p.AccountID == g.AccountID && validID(g.FamilyID) && p.SessionFamilyID == g.FamilyID && p.Scope == "family" &&
			p.SessionScope == "family" && !p.AccessExpiresAt.IsZero() && now.Before(p.AccessExpiresAt) && p.RecoveryEpoch == s.epoch && g.Revision != 0 {
			ids[i] = p.SessionID
		}
	}
	query := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			query = append(query, id)
		}
	}
	if len(query) == 0 {
		return allowed, nil
	}
	// query[k] is lease index positions[k].
	positions := make([]int, 0, len(query))
	for i, id := range ids {
		if id != "" {
			positions = append(positions, i)
		}
	}
	err := s.readAuthority(ctx, "check live leases", checkManySQL, []any{query}, func(rows pgx.Rows) error {
		for rows.Next() {
			var ord int
			var status, accountID, deviceID, scope, familyID, role, accountStatus, familyStatus, entitlementStatus string
			var epoch, issued, familySeq uint64
			var deadline, lineage time.Time
			var accountFailed, familyFailed, entitlementRequired bool
			var entitlementExpires *time.Time
			if err := rows.Scan(&ord, &status, &accountID, &deviceID, &epoch, &issued, &scope, &familyID, &role, &deadline, &lineage, &accountStatus,
				&accountFailed, &familyStatus, &familySeq, &familyFailed, &entitlementRequired, &entitlementStatus, &entitlementExpires); err != nil {
				return unavailable("scan live lease", err)
			}
			if ord < 1 || ord > len(positions) {
				return fmt.Errorf("authority: live lease ordinal %d out of range: %w", ord, ErrUnavailable)
			}
			i := positions[ord-1]
			p, g := leases[i].Principal, leases[i].Grant
			allowed[i] = status == "active" && accountID == p.AccountID && deviceID == p.DeviceID && epoch == p.RecoveryEpoch && issued == p.SecurityRevision &&
				scope == "family" && familyID == g.FamilyID && role == g.Role && now.Before(minTime(deadline, lineage.Add(selfHostedLineageAge))) &&
				accountStatus == "active" && !accountFailed && familyStatus == "active" && !familyFailed && g.Revision <= familySeq &&
				(!(s.requireFamilySyncEntitlement || entitlementRequired) || entitled(entitlementStatus, entitlementExpires, now))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return allowed, nil
}

// CheckExportSubject rechecks a claimed export job's authority
// (security/export_snapshot.go:40-49): the epoch is this node's, the
// requester's account is active and holds familyID's active slot, and the
// family is active and owned by the requester through an active owner
// membership (export_authorization_changed otherwise). A failed audit chain
// on the account or the family is scope_failed.
func (s *Service) CheckExportSubject(ctx context.Context, familyID, requesterID string, epoch uint64) error {
	if epoch != s.epoch || !validID(familyID) || !validID(requesterID) {
		return ErrExportAuthorizationChanged
	}
	var found bool
	var accountStatus, slotFamily, slotState, familyStatus, owner, role, memberStatus string
	var accountFailed, familyFailed bool
	err := s.readAuthority(ctx, "read export subject", `SELECT a.status::text, a.chain_failed, coalesce(fs.family_id::text,''), coalesce(fs.state::text,''),
			coalesce(f.status::text,''), coalesce(f.owner_account_id::text,''), coalesce(f.chain_failed,false), coalesce(m.role::text,''), coalesce(m.status::text,'')
		FROM accounts a
		LEFT JOIN family_slots fs ON fs.account_id = a.id
		LEFT JOIN families f ON f.id = $2
		LEFT JOIN memberships m ON m.family_id = $2 AND m.account_id = a.id
		WHERE a.id = $1`, []any{requesterID, familyID}, func(rows pgx.Rows) error {
		if !rows.Next() {
			return nil
		}
		found = true
		if err := rows.Scan(&accountStatus, &accountFailed, &slotFamily, &slotState, &familyStatus, &owner, &familyFailed, &role, &memberStatus); err != nil {
			return unavailable("scan export subject", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return ErrExportAuthorizationChanged
	}
	if accountFailed || familyFailed {
		return ErrScopeFailed
	}
	if accountStatus != "active" || slotFamily != familyID || slotState != "active" || familyStatus != "active" || owner != requesterID ||
		role != "owner" || memberStatus != "active" {
		return ErrExportAuthorizationChanged
	}
	return nil
}
