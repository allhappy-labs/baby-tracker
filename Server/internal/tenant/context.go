// Package tenant establishes the transaction-local database isolation boundary.
// Callers must authorize the account and family before installing this context.
package tenant

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type Context struct {
	AccountID string
	FamilyID  string
}

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SetLocal sets both identifiers for the lifetime of tx, including its queries
// and deferred constraints. Commit and rollback both discard these settings.
func SetLocal(ctx context.Context, tx pgx.Tx, tenant Context) error {
	for name, value := range map[string]string{"account": tenant.AccountID, "family": tenant.FamilyID} {
		if !canonicalUUID.MatchString(value) {
			return fmt.Errorf("tenant %s ID must be a canonical lowercase UUID", name)
		}
		var id pgtype.UUID
		if err := id.Scan(value); err != nil || !id.Valid || id.Bytes == [16]byte{} {
			return fmt.Errorf("tenant %s ID must be a nonzero UUID", name)
		}
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.account_id', $1, true), set_config('app.family_id', $2, true)", tenant.AccountID, tenant.FamilyID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	return nil
}

func SetAccountLocal(ctx context.Context, tx pgx.Tx, accountID string) error {
	if !canonicalUUID.MatchString(accountID) {
		return fmt.Errorf("tenant account ID must be a canonical lowercase UUID")
	}
	var id pgtype.UUID
	if err := id.Scan(accountID); err != nil || !id.Valid || id.Bytes == [16]byte{} {
		return fmt.Errorf("tenant account ID must be a nonzero UUID")
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.account_id', $1, true), set_config('app.family_id', '', true)", accountID); err != nil {
		return fmt.Errorf("set account context: %w", err)
	}
	return nil
}
