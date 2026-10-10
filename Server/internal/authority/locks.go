package authority

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// accountRow is a locked account's authority state. Chain is the account's
// audit chain head as locked; pass it to appendAudit, which advances it.
type accountRow struct {
	ID                  string
	Status              string
	LifecycleGeneration uint64
	Chain               *chainHead
}

// familyRow is a locked family's authority state.
type familyRow struct {
	ID                  string
	Status              string
	OwnerAccountID      string // "" when the family has no owner
	DisplayName         string
	ProfileRequired     bool
	PurgeAfter          *time.Time
	PurgeStartedAt      *time.Time
	PurgeCommandID      string // "" when unset
	PurgeReceipt        string // "" when unset
	PurgedAt            *time.Time
	EntitlementRequired bool
	// Chain is the family's audit chain head as locked; pass it to
	// appendAudit, which advances it.
	Chain *chainHead
}

// rowLock is the row lock authority commands take on accounts and
// families. No command changes a key column (id, the only key other tables
// reference) or deletes the row, so FOR NO KEY UPDATE serializes every
// command exactly as FOR UPDATE did while leaving the FOR KEY SHARE locks of
// foreign key checks unblocked: an insert referencing a locked family or
// account (export_jobs, care rows) neither waits on a command nor joins a
// lock cycle with one.
const rowLock = " FOR NO KEY UPDATE"

// lockAccounts locks the given accounts FOR NO KEY UPDATE in ID order and
// returns their rows keyed by ID; IDs with no row are absent from the map.
// Every command locks its accounts before its families, and each set in ID
// order, so two commands never wait on each other in a cycle.
func lockAccounts(ctx context.Context, tx pgx.Tx, ids ...string) (map[string]accountRow, error) {
	return selectAccounts(ctx, tx, rowLock, ids)
}

// readAccounts is lockAccounts without the lock, for checks that write
// nothing (operator preflight).
func readAccounts(ctx context.Context, tx pgx.Tx, ids ...string) (map[string]accountRow, error) {
	return selectAccounts(ctx, tx, "", ids)
}

func selectAccounts(ctx context.Context, tx pgx.Tx, lock string, ids []string) (map[string]accountRow, error) {
	sorted, err := sortedIDs(ids)
	if err != nil || len(sorted) == 0 {
		return map[string]accountRow{}, err
	}
	// ORDER BY under a row lock: LockRows sits above the ordered scan, so
	// rows are locked one at a time in ID order.
	rows, err := tx.Query(ctx, `SELECT id::text, status::text, lifecycle_generation, audit_seq, audit_hash, chain_failed
		FROM accounts WHERE id = ANY($1::uuid[]) ORDER BY accounts.id`+lock, sorted)
	if err != nil {
		return nil, fmt.Errorf("authority: lock accounts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]accountRow, len(sorted))
	for rows.Next() {
		var row accountRow
		var seq uint64
		var hash []byte
		var failed bool
		if err := rows.Scan(&row.ID, &row.Status, &row.LifecycleGeneration, &seq, &hash, &failed); err != nil {
			return nil, fmt.Errorf("authority: lock accounts: %w", err)
		}
		if row.Chain, err = newChainHead(accountScope(row.ID), seq, hash, failed); err != nil {
			return nil, err
		}
		out[row.ID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authority: lock accounts: %w", err)
	}
	return out, nil
}

// lockFamilies locks the given families FOR NO KEY UPDATE in ID order and
// returns their rows keyed by ID; IDs with no row are absent from the map.
// Callers lock the involved accounts first.
func lockFamilies(ctx context.Context, tx pgx.Tx, ids ...string) (map[string]familyRow, error) {
	return selectFamilies(ctx, tx, rowLock, ids)
}

// readFamilies is lockFamilies without the lock, for checks that write
// nothing (operator preflight).
func readFamilies(ctx context.Context, tx pgx.Tx, ids ...string) (map[string]familyRow, error) {
	return selectFamilies(ctx, tx, "", ids)
}

func selectFamilies(ctx context.Context, tx pgx.Tx, lock string, ids []string) (map[string]familyRow, error) {
	sorted, err := sortedIDs(ids)
	if err != nil || len(sorted) == 0 {
		return map[string]familyRow{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text, status::text, coalesce(owner_account_id::text,''), display_name, profile_required,
			purge_after, purge_started_at, coalesce(purge_command_id::text,''), coalesce(purge_receipt::text,''), purged_at,
			audit_seq, audit_hash, chain_failed, entitlement_required
		FROM families WHERE id = ANY($1::uuid[]) ORDER BY families.id`+lock, sorted)
	if err != nil {
		return nil, fmt.Errorf("authority: lock families: %w", err)
	}
	defer rows.Close()
	out := make(map[string]familyRow, len(sorted))
	for rows.Next() {
		var row familyRow
		var seq uint64
		var hash []byte
		var failed bool
		if err := rows.Scan(&row.ID, &row.Status, &row.OwnerAccountID, &row.DisplayName, &row.ProfileRequired,
			&row.PurgeAfter, &row.PurgeStartedAt, &row.PurgeCommandID, &row.PurgeReceipt, &row.PurgedAt,
			&seq, &hash, &failed, &row.EntitlementRequired); err != nil {
			return nil, fmt.Errorf("authority: lock families: %w", err)
		}
		if row.Chain, err = newChainHead(familyScope(row.ID), seq, hash, failed); err != nil {
			return nil, err
		}
		out[row.ID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authority: lock families: %w", err)
	}
	return out, nil
}

// sortedIDs validates ids as canonical UUIDs and returns them sorted and
// deduplicated. Lowercase canonical text sorts like PostgreSQL's uuid order.
func sortedIDs(ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !validID(id) {
			return nil, ErrInvalidCommand
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
