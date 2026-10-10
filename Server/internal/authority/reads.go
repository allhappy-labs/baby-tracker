package authority

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Read helpers for the family, account and operator services. Each runs one
// read-only, repeatable-read authority transaction (readOnly) and locks
// nothing; a command that acts on what it read re-checks it under its own
// locks. They read authority-only rows with the authority flag raised, so
// RLS does not scope them: callers must pass IDs they have authenticated
// (the caller's own account from its principal, a family its grant or
// receipt names, or an operator's confirmed proposal), never IDs taken from
// request input on behalf of another account.

// readOnly runs fn in one read-only authority transaction and rolls back.
func (s *Service) readOnly(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	return fn(tx)
}

// FamilyRevision is familyID's audit seq: the ExpectedRevision every
// operator command compares against (stale_security_revision otherwise). An
// unknown family is family_unavailable. familyID must be one the caller is
// entitled to name (an operator's confirmed proposal).
func (s *Service) FamilyRevision(ctx context.Context, familyID string) (uint64, error) {
	if !validID(familyID) {
		return 0, ErrInvalidCommand
	}
	var seq int64
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT audit_seq FROM families WHERE id=$1`, familyID).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFamilyUnavailable
		}
		if err != nil {
			return unavailable("read family revision", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return uint64(seq), nil
}

// ReceiptView is a stored command receipt: its kind, the families it
// touched and its result.
type ReceiptView struct {
	Kind      Kind
	FamilyIDs []string
	Result    Result
}

// CommandReceipt returns actorID's receipt for commandID, of any kind. The
// family service uses it where the legacy service looked a receipt up before
// its other checks: a command ID already spent on another kind is
// command_id_reused, and an exact retry is answered before the
// preconditions of a first attempt are evaluated. actorID must be the
// authenticated caller's own account.
func (s *Service) CommandReceipt(ctx context.Context, actorID, commandID string) (ReceiptView, bool, error) {
	if !validID(actorID) || !validID(commandID) {
		return ReceiptView{}, false, ErrInvalidCommand
	}
	var view ReceiptView
	var found bool
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		var kind string
		var encoded []byte
		err := tx.QueryRow(ctx, `SELECT kind::text, coalesce(family_ids::text[], '{}'), result FROM command_receipts
			WHERE actor_account_id=$1 AND command_id=$2`, actorID, commandID).Scan(&kind, &view.FamilyIDs, &encoded)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return unavailable("read receipt", err)
		}
		result, err := decodeResult(encoded)
		if err != nil {
			return err
		}
		view.Kind, view.Result, found = Kind(kind), result, true
		return nil
	})
	if err != nil {
		return ReceiptView{}, false, err
	}
	return view, found, nil
}

// MaxAuditPageRows bounds one FamilyAuditPage.
const MaxAuditPageRows = 1000

// AuditEntry is one audit row as a family archive shows it: no payload, no
// hashes.
type AuditEntry struct {
	Seq            uint64
	Kind           string
	ActorAccountID string
	At             time.Time
}

// FamilyAuditPage returns familyID's family-scope audit rows with
// afterSeq < seq <= throughSeq, in seq order, at most limit (1 to
// MaxAuditPageRows) of them: the family archive's audit section, which
// replaces app.export_family_audit. It is one ordered range of the
// (scope_id, seq, at, scope) key per partition. familyID must be one the
// caller is entitled to name (an export job whose subject CheckExportSubject
// passed).
func (s *Service) FamilyAuditPage(ctx context.Context, familyID string, afterSeq, throughSeq uint64, limit int) ([]AuditEntry, error) {
	if !validID(familyID) || limit < 1 || limit > MaxAuditPageRows || afterSeq > maxWireInteger || throughSeq > maxWireInteger {
		return nil, ErrInvalidCommand
	}
	entries := []AuditEntry{}
	if throughSeq <= afterSeq {
		return entries, nil
	}
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT seq, kind::text, actor_account_id::text, at FROM security_audit_events
			WHERE scope_id=$1 AND scope='family' AND seq>$2 AND seq<=$3 ORDER BY seq, at LIMIT $4`,
			familyID, int64(afterSeq), int64(throughSeq), limit)
		if err != nil {
			return unavailable("read family audit", err)
		}
		defer rows.Close()
		for rows.Next() {
			var entry AuditEntry
			var seq int64
			if err := rows.Scan(&seq, &entry.Kind, &entry.ActorAccountID, &entry.At); err != nil {
				return unavailable("scan family audit", err)
			}
			entry.Seq = uint64(seq)
			entries = append(entries, entry)
		}
		if err := rows.Err(); err != nil {
			return unavailable("read family audit", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// AccountIdentityKey is accountID's Apple identity key
// (hmac-sha256:<key id>:<hex>), live or not, as DeleteAccount expects it;
// found is false when the account has no identity row. A deleted account
// keeps its row (live=false), so an exact retry of the deletion hashes the
// same key. accountID must be the authenticated caller's own account.
func (s *Service) AccountIdentityKey(ctx context.Context, accountID string) (string, bool, error) {
	if !validID(accountID) {
		return "", false, ErrInvalidCommand
	}
	var key string
	var found bool
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT 'hmac-sha256:'||identity_key_id||':'||encode(identity_digest,'hex') FROM account_identities
			WHERE account_id=$1`, accountID).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return unavailable("read identity key", err)
		}
		found = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return key, found, nil
}
