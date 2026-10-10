package authority

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// verifyPageRows bounds one keyset page of a chain walk, so a walk holds at
// most this many rows in memory however long the chain is. Tests shrink it
// to cross page boundaries.
var verifyPageRows = 512

// verifyListScopes bounds one keyset page of VerifyAll's scope listing.
const verifyListScopes = 1024

// runBatchScopes is the number of chains Run verifies per tick.
const runBatchScopes = 1000

// ScopeRef names one audit chain: an account's or a family's.
type ScopeRef struct {
	Scope Scope
	ID    string
}

// VerifyReport summarises a verification pass. Scopes is the number of
// chains examined, Rows the number of audit rows whose hash was recomputed
// and matched, and Failed the chains that are failed after the pass: chains
// this pass found broken and, for VerifyAll, chains already marked failed.
type VerifyReport struct {
	Scopes, Rows int
	Failed       []ScopeRef
}

// Verifier checks the per-scope audit chains against their heads and fails
// a broken chain closed: it sets chain_failed on the account or family row,
// after which every transaction of that scope is refused with scope_failed.
// Only a broken chain's own scope is failed. The verifier never clears
// chain_failed; that is an operator decision.
type Verifier struct {
	s      *Service
	logger *slog.Logger
}

// NewVerifier returns a verifier over s's runtime pool. A nil logger
// discards the mismatch alerts.
func NewVerifier(s *Service, logger *slog.Logger) *Verifier {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Verifier{s: s, logger: logger}
}

// NewAuditVerifier returns a verifier over pool alone, for `baby-tracker-admin
// authority verify`, which holds no token keys or role policy: the verifier
// only reads audit chains, writes checkpoints and fails broken chains, all
// in authority transactions on pool.
func NewAuditVerifier(pool *pgxpool.Pool, now func() time.Time, logger *slog.Logger) (*Verifier, error) {
	if pool == nil || now == nil {
		return nil, errors.New("authority: audit verifier requires a pool and a clock")
	}
	return NewVerifier(&Service{pool: pool, now: now}, logger), nil
}

// chainCheckpoint is a verified prefix of a chain: every row up to seq was
// verified and row seq's hash is hash. seq 0 is genesis.
type chainCheckpoint struct {
	seq  uint64
	hash [32]byte
}

// chainMismatch is the first broken seq of a chain and why.
type chainMismatch struct {
	seq    uint64
	reason string
}

// scopeOutcome is one chain's verification result: the head as of the
// verification snapshot, the rows verified, whether the chain was already
// failed, and the first mismatch, if any.
type scopeOutcome struct {
	head     chainCheckpoint
	rows     int
	failed   bool
	mismatch *chainMismatch
}

// VerifyIncremental verifies at most maxScopes chains that moved since
// their checkpoint (or that have rows and no checkpoint), never-verified
// chains first and then the oldest checkpoint first. Each chain is walked
// from its checkpointed row, which is re-verified and must still carry the
// checkpointed hash, to its head. Clean chains get their checkpoint advanced
// to the head; broken chains are failed closed and alerted. Chains already
// failed are skipped.
func (v *Verifier) VerifyIncremental(ctx context.Context, maxScopes int) (VerifyReport, error) {
	if maxScopes < 1 {
		return VerifyReport{}, errors.New("authority: verify: maxScopes must be positive")
	}
	type candidate struct {
		ref    ScopeRef
		anchor chainCheckpoint
	}
	var candidates []candidate
	err := v.read(ctx, func(tx pgx.Tx) error {
		// A chain moved when its head differs from its checkpoint; a chain
		// with no checkpoint is due once it has rows. Genesis-only chains
		// without a checkpoint are left to VerifyAll.
		rows, err := tx.Query(ctx, `SELECT s.scope, s.id::text, coalesce(c.verified_seq, 0), coalesce(c.verified_hash, $2)
			FROM (SELECT 'account' AS scope, id, audit_seq, audit_hash FROM accounts WHERE NOT chain_failed
			      UNION ALL
			      SELECT 'family', id, audit_seq, audit_hash FROM families WHERE NOT chain_failed) s
			LEFT JOIN audit_checkpoints c ON c.scope = s.scope::app.audit_scope AND c.scope_id = s.id
			WHERE CASE WHEN c.scope_id IS NULL THEN s.audit_seq > 0
			           ELSE c.verified_seq <> s.audit_seq OR c.verified_hash <> s.audit_hash END
			ORDER BY c.verified_at NULLS FIRST, s.scope, s.id
			LIMIT $1`, maxScopes, make([]byte, sha256.Size))
		if err != nil {
			return err
		}
		candidates, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
			var c candidate
			var scope string
			var hash []byte
			if err := row.Scan(&scope, &c.ref.ID, &c.anchor.seq, &hash); err != nil {
				return c, err
			}
			if len(hash) != sha256.Size {
				return c, fmt.Errorf("checkpoint of %s %s has a %d-byte hash", scope, c.ref.ID, len(hash))
			}
			c.ref.Scope = Scope(scope)
			copy(c.anchor.hash[:], hash)
			return c, nil
		})
		return err
	})
	if err != nil {
		return VerifyReport{}, fmt.Errorf("authority: verify: list due chains: %w", errors.Join(err, ErrUnavailable))
	}
	var report VerifyReport
	var verified []ScopeRef
	var heads []chainCheckpoint
	var errs []error
	for _, c := range candidates {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		// One chain's failure to verify (or to be marked failed) is logged
		// and returned, never allowed to stop the other chains of the pass.
		outcome, err := v.verifyScope(ctx, c.ref, c.anchor, false)
		if err != nil {
			errs = append(errs, v.unverified(ctx, c.ref, err))
			continue
		}
		report.Scopes++
		report.Rows += outcome.rows
		switch {
		case outcome.mismatch != nil:
			report.Failed = append(report.Failed, c.ref)
			if err := v.fail(ctx, c.ref, *outcome.mismatch); err != nil {
				errs = append(errs, v.unverified(ctx, c.ref, err))
			}
		case outcome.failed:
			// Failed since it was listed: leave it failed and unchecked.
		default:
			verified, heads = append(verified, c.ref), append(heads, outcome.head)
		}
	}
	errs = append(errs, v.writeCheckpoints(ctx, verified, heads))
	return report, errors.Join(errs...)
}

// unverified logs a chain the pass could not verify or fail and returns the
// error for the pass's result.
func (v *Verifier) unverified(ctx context.Context, ref ScopeRef, err error) error {
	if ctx.Err() == nil {
		v.logger.Warn("audit chain not verified", "scope", string(ref.Scope), "id", ref.ID, "error", err)
	}
	return err
}

// VerifyAll verifies every account and family chain from genesis and
// fails closed every broken chain. Checkpoints are read, never written: the
// walk also requires the checkpointed row to still carry the checkpointed
// hash (and the head not to be behind it), so a consistent rewrite of a
// checkpointed row is reported here too; writing them would replace that
// evidence with whatever the chain says now. Chains already failed are
// walked (a mismatch still present is alerted again) and reported failed
// whatever the walk finds. A chain that cannot be verified is logged and
// returned in the error after the other chains are verified. Used by
// `baby-tracker-admin authority verify`.
func (v *Verifier) VerifyAll(ctx context.Context) (VerifyReport, error) {
	var report VerifyReport
	var errs []error
	for _, scope := range []Scope{ScopeAccount, ScopeFamily} {
		table, _ := scopeTable(scope)
		after := "00000000-0000-0000-0000-000000000000"
		for {
			var ids []string
			err := v.read(ctx, func(tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT id::text FROM `+table+` WHERE id > $1 ORDER BY `+table+`.id LIMIT $2`, after, verifyListScopes)
				if err != nil {
					return err
				}
				ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
				return err
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("authority: verify: list %s: %w", table, errors.Join(err, ErrUnavailable)))
				return report, errors.Join(errs...)
			}
			for _, id := range ids {
				if ctx.Err() != nil {
					return report, errors.Join(append(errs, ctx.Err())...)
				}
				ref := ScopeRef{Scope: scope, ID: id}
				outcome, err := v.verifyScope(ctx, ref, chainCheckpoint{}, true)
				if err != nil {
					errs = append(errs, v.unverified(ctx, ref, err))
					continue
				}
				report.Scopes++
				report.Rows += outcome.rows
				switch {
				case outcome.mismatch != nil:
					report.Failed = append(report.Failed, ref)
					if err := v.fail(ctx, ref, *outcome.mismatch); err != nil {
						errs = append(errs, v.unverified(ctx, ref, err))
					}
				case outcome.failed:
					report.Failed = append(report.Failed, ref)
				}
			}
			if len(ids) < verifyListScopes {
				break
			}
			after = ids[len(ids)-1]
		}
	}
	return report, errors.Join(errs...)
}

// Run verifies incrementally (runBatchScopes chains per pass) at once and
// then every interval until ctx ends. Failures to verify are logged; they
// do not stop the loop.
func (v *Verifier) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := v.VerifyIncremental(ctx, runBatchScopes); err != nil && ctx.Err() == nil {
			v.logger.Warn("audit chain verification incomplete", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// verifyScope walks one chain from anchor (genesis when anchor.seq is 0)
// to its head inside one repeatable-read snapshot, so the head and the rows
// are those of one instant: appends committing meanwhile (which move the
// head and insert rows in one transaction) are invisible, never a
// mismatch. With crossCheck (a walk from genesis), the chain's checkpoint is
// read in the same snapshot and held to as well: checkpoints are written
// only after a verified snapshot, so a legitimate one is never ahead of the
// head nor different from its row. The walk is broken when:
//   - the head is behind the anchor or the checkpoint;
//   - the anchored row is missing, duplicated, or no longer hashes to the
//     anchored hash, or the checkpointed row no longer carries the
//     checkpointed hash;
//   - a row cannot be decoded (a non-finite at, a malformed hash): such a
//     row is tampered, not an infrastructure failure;
//   - a seq is missing (a gap, a deleted first row or a deleted tail) or
//     duplicated (the schema's key includes at, so it cannot refuse one);
//   - a row's prev_hash is not the previous row's hash;
//   - a row's hash is not the hash recomputed from its columns (payload
//     re-canonicalised from its jsonb text), or its payload is not
//     canonical JSON;
//   - a row lies beyond the head's seq;
//   - the head's hash is not the last row's hash.
//
// Rows are read by keyset pages over the primary key (scope_id, seq, at,
// scope) in (seq, at) order, so duplicates arrive adjacently.
func (v *Verifier) verifyScope(ctx context.Context, ref ScopeRef, anchor chainCheckpoint, crossCheck bool) (scopeOutcome, error) {
	table, ok := scopeTable(ref.Scope)
	if !ok || !validID(ref.ID) {
		return scopeOutcome{}, fmt.Errorf("authority: verify: bad scope %q %q: %w", ref.Scope, ref.ID, ErrInvalidCommand)
	}
	var out scopeOutcome
	err := v.read(ctx, func(tx pgx.Tx) error {
		out = scopeOutcome{}
		var headSeq uint64
		var headHash []byte
		if err := tx.QueryRow(ctx, `SELECT audit_seq, audit_hash, chain_failed FROM `+table+` WHERE id=$1`, ref.ID).
			Scan(&headSeq, &headHash, &out.failed); err != nil {
			return err
		}
		if len(headHash) != sha256.Size {
			return fmt.Errorf("head of %s %s has a %d-byte hash", ref.Scope, ref.ID, len(headHash))
		}
		out.head.seq = headSeq
		copy(out.head.hash[:], headHash)
		walk := chainWalk{scope: ref, anchor: anchor, expected: max(anchor.seq, 1)}
		floor := anchor.seq // the head may not be behind the anchor or the checkpoint
		if crossCheck {
			var cp chainCheckpoint
			var hash []byte
			err := tx.QueryRow(ctx, `SELECT verified_seq, verified_hash FROM audit_checkpoints WHERE scope_id=$2 AND scope=$1::text::app.audit_scope`,
				string(ref.Scope), ref.ID).Scan(&cp.seq, &hash)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return err
			case len(hash) != sha256.Size:
				return fmt.Errorf("checkpoint of %s %s has a %d-byte hash", ref.Scope, ref.ID, len(hash))
			case cp.seq > 0:
				copy(cp.hash[:], hash)
				walk.checkpoint = &cp
				floor = max(floor, cp.seq)
			}
		}
		if floor > headSeq {
			out.mismatch = &chainMismatch{seq: floor, reason: "head behind checkpoint"}
			return nil
		}

		pageRows := verifyPageRows
		var cursorSeq uint64
		var cursorAt time.Time
		first := true
		for {
			var rows pgx.Rows
			var err error
			if first {
				rows, err = tx.Query(ctx, `SELECT seq, prev_hash, hash, kind::text, command_id::text, actor_account_id::text, at, payload::text
					FROM security_audit_events WHERE scope=$1::text::app.audit_scope AND scope_id=$2 AND seq >= $3
					ORDER BY seq, at LIMIT $4`, string(ref.Scope), ref.ID, int64(walk.expected), pageRows)
			} else {
				rows, err = tx.Query(ctx, `SELECT seq, prev_hash, hash, kind::text, command_id::text, actor_account_id::text, at, payload::text
					FROM security_audit_events WHERE scope=$1::text::app.audit_scope AND scope_id=$2 AND (seq, at) > ($3, $4)
					ORDER BY seq, at LIMIT $5`, string(ref.Scope), ref.ID, int64(cursorSeq), cursorAt, pageRows)
			}
			if err != nil {
				return err
			}
			page := 0
			for rows.Next() {
				// Every column is scanned into a type that accepts any value
				// the column can hold, so a tampered value is classified by
				// the walk instead of failing the scan.
				var row storedAuditRow
				var prev, hash []byte
				var at pgtype.Timestamptz
				if err := rows.Scan(&row.seq, &prev, &hash, &row.kind, &row.commandID, &row.actorID, &at, &row.payload); err != nil {
					rows.Close()
					out.rows, out.mismatch = walk.rows, &chainMismatch{seq: walk.expected, reason: "row not decodable"}
					return nil
				}
				page++
				switch {
				case !at.Valid || at.InfinityModifier != pgtype.Finite:
					row.undecodable = "non-finite at"
				case len(prev) != sha256.Size || len(hash) != sha256.Size:
					row.undecodable = "malformed hash"
				default:
					row.at = at.Time
					copy(row.prev[:], prev)
					copy(row.hash[:], hash)
				}
				cursorSeq, cursorAt = row.seq, row.at
				if mismatch := walk.next(row, headSeq); mismatch != nil {
					rows.Close()
					out.rows, out.mismatch = walk.rows, mismatch
					return nil
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if page < pageRows {
				break
			}
			first = false
		}
		out.rows, out.mismatch = walk.rows, walk.end(headSeq, out.head.hash)
		return nil
	})
	if err != nil {
		return scopeOutcome{}, fmt.Errorf("authority: verify %s %s: %w", ref.Scope, ref.ID, errors.Join(err, ErrUnavailable))
	}
	return out, nil
}

type storedAuditRow struct {
	seq                      uint64
	prev, hash               [32]byte
	kind, commandID, actorID string
	at                       time.Time
	payload                  string
	// undecodable names why the row's stored values cannot be hashed.
	undecodable string
}

// chainWalk is the state of one chain walk: the next seq expected, the hash
// the next row must link to, and the rows verified so far.
type chainWalk struct {
	scope  ScopeRef
	anchor chainCheckpoint
	// checkpoint, when set, is a checkpoint a walk from genesis holds the
	// chain to: its row must still carry its hash.
	checkpoint *chainCheckpoint
	expected   uint64
	running    [32]byte
	rows       int
}

// next verifies one row (rows arrive in (seq, at) order) and returns the
// first mismatch it reveals.
func (w *chainWalk) next(row storedAuditRow, headSeq uint64) *chainMismatch {
	switch {
	case row.seq < w.expected:
		return &chainMismatch{seq: row.seq, reason: "duplicate seq"}
	case row.seq > w.expected:
		return &chainMismatch{seq: w.expected, reason: "missing row"}
	case row.seq > headSeq:
		return &chainMismatch{seq: row.seq, reason: "row beyond head"}
	case row.undecodable != "":
		return &chainMismatch{seq: row.seq, reason: "row not decodable: " + row.undecodable}
	}
	isAnchor := w.anchor.seq > 0 && row.seq == w.anchor.seq
	if !isAnchor && row.prev != w.running {
		return &chainMismatch{seq: row.seq, reason: "prev_hash does not link"}
	}
	payload, err := canonicalJSON(json.RawMessage(row.payload))
	if err != nil {
		return &chainMismatch{seq: row.seq, reason: "payload not canonical"}
	}
	if auditRowHash(w.scope.Scope, w.scope.ID, row.seq, row.prev, Kind(row.kind), row.commandID, row.actorID, row.at, payload) != row.hash {
		return &chainMismatch{seq: row.seq, reason: "row hash"}
	}
	if (isAnchor && row.hash != w.anchor.hash) || (w.checkpoint != nil && row.seq == w.checkpoint.seq && row.hash != w.checkpoint.hash) {
		return &chainMismatch{seq: row.seq, reason: "checkpoint hash"}
	}
	w.running = row.hash
	w.expected++
	w.rows++
	return nil
}

// end checks the walked chain reaches the head: no missing tail, and the
// head's hash is the last row's.
func (w *chainWalk) end(headSeq uint64, headHash [32]byte) *chainMismatch {
	if last := w.expected - 1; last < headSeq {
		return &chainMismatch{seq: last + 1, reason: "missing row"}
	}
	if w.running != headHash {
		return &chainMismatch{seq: headSeq, reason: "head hash"}
	}
	return nil
}

// fail raises the alert and marks ref's chain failed (idempotently; the
// verifier never clears it). The alert comes first so a failed write still
// leaves it; the next pass finds the mismatch again.
func (v *Verifier) fail(ctx context.Context, ref ScopeRef, mismatch chainMismatch) error {
	v.logger.Error("audit chain mismatch", "scope", string(ref.Scope), "id", ref.ID, "seq", mismatch.seq, "reason", mismatch.reason)
	table, _ := scopeTable(ref.Scope)
	tx, err := v.s.begin(ctx)
	if err != nil {
		return errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `UPDATE `+table+` SET chain_failed=true WHERE id=$1 AND NOT chain_failed`, ref.ID); err != nil {
		return fmt.Errorf("authority: verify: fail %s %s: %w", ref.Scope, ref.ID, errors.Join(err, ErrUnavailable))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("authority: verify: fail %s %s: %w", ref.Scope, ref.ID, errors.Join(err, ErrUnavailable))
	}
	return nil
}

// writeCheckpoints records verified heads in one statement. A checkpoint
// never moves backwards (a slower concurrent verifier cannot regress it).
func (v *Verifier) writeCheckpoints(ctx context.Context, refs []ScopeRef, heads []chainCheckpoint) error {
	if len(refs) == 0 {
		return nil
	}
	scopes, ids, seqs, hashes := make([]string, len(refs)), make([]string, len(refs)), make([]int64, len(refs)), make([][]byte, len(refs))
	for i, ref := range refs {
		scopes[i], ids[i], seqs[i], hashes[i] = string(ref.Scope), ref.ID, int64(heads[i].seq), heads[i].hash[:]
	}
	tx, err := v.s.begin(ctx)
	if err != nil {
		return errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `INSERT INTO audit_checkpoints(scope, scope_id, verified_seq, verified_hash, verified_at)
		SELECT c.scope::app.audit_scope, c.id, c.seq, c.hash, $5 FROM unnest($1::text[], $2::uuid[], $3::bigint[], $4::bytea[]) AS c(scope, id, seq, hash)
		ON CONFLICT (scope, scope_id) DO UPDATE SET verified_seq=excluded.verified_seq, verified_hash=excluded.verified_hash, verified_at=excluded.verified_at
		WHERE audit_checkpoints.verified_seq <= excluded.verified_seq`,
		scopes, ids, seqs, hashes, v.s.now().UTC().Truncate(time.Microsecond)); err != nil {
		return fmt.Errorf("authority: verify: write checkpoints: %w", errors.Join(err, ErrUnavailable))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("authority: verify: write checkpoints: %w", errors.Join(err, ErrUnavailable))
	}
	return nil
}

// read runs fn in a read-only repeatable-read authority transaction.
func (v *Verifier) read(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := v.s.beginReadOnly(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
