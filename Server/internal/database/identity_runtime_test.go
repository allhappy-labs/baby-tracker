package database_test

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

// Schema 29 account_identities: the Apple credential quartet is gone with the
// hosted vault (hosted ruling); a row binds one live actor to a complete,
// bounded identity and an account holds at most one identity row.
func TestAccountIdentityConstraintRejectsIncompleteMaterialAndCredentialQuartets(t *testing.T) {
	fixture := testpg.New(t)
	const accountID, otherID = "10000000-0000-4000-8000-000000000099", "10000000-0000-4000-8000-000000000098"
	if err := authorityExec(t.Context(), fixture.Owner, `INSERT INTO accounts(id,display_name) VALUES($1,'Constraint'),($2,'Other')`, accountID, otherID); err != nil {
		t.Fatal(err)
	}
	var credentialColumns int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema='public'
		AND table_name='account_identities' AND column_name LIKE 'apple_credential%'`).Scan(&credentialColumns); err != nil || credentialColumns != 0 {
		t.Fatalf("credential columns=%d err=%v", credentialColumns, err)
	}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	insert := func(account, provider, keyID string, digest []byte, generation int64, audience, subject any) error {
		return authorityExec(t.Context(), fixture.Owner, `INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,
			lifecycle_generation,audience,subject,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			provider, keyID, digest, account, generation, audience, subject, now)
	}
	digest := hashBytes("identity")
	for name, test := range map[string]struct {
		provider, keyID   string
		digest            []byte
		generation        int64
		audience, subject any
		state             string
	}{
		"missing audience":  {"apple", "identity.v1", digest, 1, nil, "subject", "23502"},
		"missing subject":   {"apple", "identity.v1", digest, 1, "audience", nil, "23502"},
		"empty audience":    {"apple", "identity.v1", digest, 1, "", "subject", "23514"},
		"empty subject":     {"apple", "identity.v1", digest, 1, "audience", "", "23514"},
		"other provider":    {"google", "identity.v1", digest, 1, "audience", "subject", "23514"},
		"malformed key id":  {"apple", "Identity", digest, 1, "audience", "subject", "23514"},
		"short digest":      {"apple", "identity.v1", digest[:31], 1, "audience", "subject", "23514"},
		"zero generation":   {"apple", "identity.v1", digest, 0, "audience", "subject", "23514"},
		"unsafe generation": {"apple", "identity.v1", digest, 9007199254740992, "audience", "subject", "23514"},
	} {
		t.Run(name, func(t *testing.T) {
			assertSQLState(t, insert(accountID, test.provider, test.keyID, test.digest, test.generation, test.audience, test.subject), test.state)
		})
	}
	if err := insert(accountID, "apple", "identity.v1", digest, 1, "audience", "subject"); err != nil {
		t.Fatalf("complete identity rejected: %v", err)
	}
	// One identity row per account, whatever the identity.
	assertSQLState(t, insert(accountID, "apple", "identity.v1", hashBytes("second"), 1, "audience", "subject"), "23505")
	// Account deletion kills the generation; the identity's next live
	// generation belongs to a new account.
	if err := authorityExec(t.Context(), fixture.Owner, `UPDATE account_identities SET live=false WHERE account_id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	if err := insert(otherID, "apple", "identity.v1", digest, 2, "audience", "subject"); err != nil {
		t.Fatalf("next generation rejected: %v", err)
	}
}

func hashBytes(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}

func assertSQLState(t *testing.T, err error, want string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != want {
		t.Fatalf("error=%v, want SQLSTATE %s", err, want)
	}
}
