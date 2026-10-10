package authority_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mcpPool is a dedicated pool over the runtime role whose sessions default
// to read-only transactions, as database.OpenReadOnlyWithMax opens it.
func mcpPool(t *testing.T, f *authoritytest.Fixture) *pgxpool.Pool {
	t.Helper()
	config := f.Pool.Config().Copy()
	config.MaxConns = 2
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newReader(t *testing.T, f *authoritytest.Fixture) *authority.IntegrationReader {
	t.Helper()
	r, err := authority.NewIntegrationReader(f.Service, mcpPool(t, f))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// issueCredential grants owner a family:read integration through the
// operator command and returns the grant and its credential.
func issueCredential(t *testing.T, f *authoritytest.Fixture, family, owner string) (authority.OperatorIntegration, string) {
	t.Helper()
	in := grantOf(t, f, family, owner)
	secret := make([]byte, authority.IntegrationSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	verifier, err := authority.IntegrationVerifier(in.GrantID, secret)
	if err != nil {
		t.Fatal(err)
	}
	in.Verifier = hex.EncodeToString(verifier[:])
	if _, err := f.Service.ExecuteOperatorIntegration(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	credential, err := authority.FormatIntegrationCredential(in.GrantID, secret)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := authority.IntegrationCredentialGrantID(credential); !ok || id != in.GrantID {
		t.Fatalf("credential grant %q %v", id, ok)
	}
	return in, credential
}

func TestIntegrationReadDeniesAfterRevocation(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	grant, credential := issueCredential(t, f, family, owner.account)
	r := newReader(t, f)

	identity, err := r.CheckIntegration(t.Context(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if identity.GrantID != grant.GrantID || identity.AccountID != owner.account || identity.FamilyID != family ||
		!identity.ExpiresAt.Equal(f.Now.Truncate(time.Second).Add(integrationValidity)) {
		t.Fatalf("identity %+v", identity)
	}
	callbacks := 0
	read := func() error {
		return r.WithIntegrationRead(t.Context(), credential, func(ctx context.Context, tx pgx.Tx, g access.Grant) error {
			callbacks++
			if g.AccountID != owner.account || g.FamilyID != family || g.Role != "owner" || g.Revision != familyRevision(t, f, family) {
				t.Errorf("grant %+v", g)
			}
			var account, current string
			var authorityFlag bool
			if err := tx.QueryRow(ctx, `SELECT coalesce(app.current_account_id()::text,''), coalesce(app.current_family_id()::text,''), app.authority_context()`).
				Scan(&account, &current, &authorityFlag); err != nil {
				return err
			}
			if account != owner.account || current != family || authorityFlag {
				t.Errorf("tenant context %s %s %v", account, current, authorityFlag)
			}
			// Nested use of the reader from the callback is refused.
			if _, err := r.CheckIntegration(ctx, credential); authority.CodeOf(err) != "service_unavailable" {
				t.Errorf("nested CheckIntegration: %v", err)
			}
			return nil
		})
	}
	if err := read(); err != nil || callbacks != 1 {
		t.Fatalf("read before revocation: %v (%d callbacks)", err, callbacks)
	}
	if _, err := f.Service.ExecuteOperatorIntegration(t.Context(), revokeOf(t, f, grant)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, read(), "integration_unauthorized")
	if callbacks != 1 {
		t.Fatalf("revoked credential reached the callback (%d callbacks)", callbacks)
	}
	_, err = r.CheckIntegration(t.Context(), credential)
	wantCode(t, err, "integration_unauthorized")
}

// Grants stay active across ownership changes; the reader rechecks the live
// owner, membership and slot on every use.
func TestIntegrationReadRechecksLiveOwner(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := joined(t, f, f.Service, owner, family, "caregiver")
	_, credential := issueCredential(t, f, family, owner.account)
	r := newReader(t, f)
	if _, err := r.CheckIntegration(t.Context(), credential); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.TransferOwner(t.Context(), authority.TransferOwner{CommandID: authoritytest.NewID(t), OldOwnerID: owner.account,
		NewOwnerID: caregiver.account, At: f.Now, Principal: owner.principal(t, f)}); err != nil {
		t.Fatal(err)
	}
	_, err := r.CheckIntegration(t.Context(), credential)
	wantCode(t, err, "integration_unauthorized")
	wantCode(t, r.WithIntegrationRead(t.Context(), credential, func(context.Context, pgx.Tx, access.Grant) error {
		t.Fatal("callback ran for a former owner")
		return nil
	}), "integration_unauthorized")

	// A failed family chain is a 503, not a denial.
	_, credential = issueCredential(t, f, family, caregiver.account)
	f.Exec(t, `UPDATE families SET chain_failed=true WHERE id=$1`, family)
	_, err = r.CheckIntegration(t.Context(), credential)
	wantCode(t, err, "scope_failed")
}

func TestIntegrationReadRejectsSQLMutation(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	_, credential := issueCredential(t, f, family, owner.account)
	r := newReader(t, f)
	for _, statements := range [][]string{
		{`UPDATE families SET next_sequence=next_sequence WHERE id=$1`},
		// Ending the read-only transaction does not open a writable one.
		{`COMMIT`, `UPDATE families SET next_sequence=next_sequence WHERE id=$1`},
	} {
		err := r.WithIntegrationRead(t.Context(), credential, func(ctx context.Context, tx pgx.Tx, g access.Grant) error {
			for _, statement := range statements {
				var args []any
				if statement != `COMMIT` {
					args = []any{g.FamilyID}
				}
				if _, err := tx.Exec(ctx, statement, args...); err != nil {
					return err
				}
			}
			return nil
		})
		if sqlState(err) != "25006" {
			t.Fatalf("%q: err = %v, want read-only refusal", statements, err)
		}
	}
	// The reader refuses the runtime pool and a writable pool.
	if _, err := authority.NewIntegrationReader(f.Service, f.Pool); err == nil {
		t.Fatal("reader accepted the runtime pool")
	}
	writable := f.Pool.Config().Copy()
	pool, err := pgxpool.NewWithConfig(t.Context(), writable)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := authority.NewIntegrationReader(f.Service, pool); err == nil {
		t.Fatal("reader accepted a writable pool")
	}
}

func TestIntegrationReadExpiryAtExactDeadline(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	_, credential := issueCredential(t, f, family, owner.account)
	expires := f.Now.Truncate(time.Second).Add(integrationValidity)
	r := newReader(t, f)

	f.Now = expires.Add(-time.Microsecond)
	if _, err := r.CheckIntegration(t.Context(), credential); err != nil {
		t.Fatalf("a microsecond before expiry: %v", err)
	}
	// The read's context ends no later than the credential.
	f.Now = expires.Add(-time.Hour)
	before := time.Now()
	if err := r.WithIntegrationRead(t.Context(), credential, func(ctx context.Context, _ pgx.Tx, _ access.Grant) error {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(time.Now().Add(time.Hour)) || deadline.Before(before.Add(59*time.Minute)) {
			t.Errorf("read deadline %v (ok=%v), want about an hour", deadline, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.Now = expires
	callbacks := 0
	_, err := r.CheckIntegration(t.Context(), credential)
	wantCode(t, err, "integration_unauthorized")
	wantCode(t, r.WithIntegrationRead(t.Context(), credential, func(context.Context, pgx.Tx, access.Grant) error { callbacks++; return nil }),
		"integration_unauthorized")
	if callbacks != 0 {
		t.Fatal("expired credential reached the callback")
	}
}

func TestPhoneTokenAndIntegrationCredentialAreNotInterchangeable(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	phone := seated(t, f, owner, family, "owner")
	_, credential := issueCredential(t, f, family, owner.account)
	r := newReader(t, f)

	// The phone access token is not an integration credential.
	_, err := r.CheckIntegration(t.Context(), phone.access)
	wantCode(t, err, "integration_unauthorized")
	wantCode(t, r.WithIntegrationRead(t.Context(), phone.access, func(context.Context, pgx.Tx, access.Grant) error {
		t.Fatal("phone token reached an integration read")
		return nil
	}), "integration_unauthorized")
	// The integration credential is not a phone access token.
	_, err = f.Service.Authenticate(t.Context(), accessDigests(f.TokenKeys, credential))
	wantCode(t, err, "session_revoked")
	wantCode(t, f.Service.WithRequestTx(t.Context(), authority.Request{Digests: accessDigests(f.TokenKeys, credential)}, "read",
		func(pgx.Tx, access.Principal, access.Grant) error {
			t.Fatal("integration credential reached a phone transaction")
			return nil
		}), "session_revoked")
	// Both still work for their own path.
	if _, err := f.Service.Authenticate(t.Context(), phone.digests(f)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CheckIntegration(t.Context(), credential); err != nil {
		t.Fatal(err)
	}
	// A credential with a flipped secret byte is refused.
	tampered := []byte(credential)
	if tampered[len(tampered)-10] == 'A' {
		tampered[len(tampered)-10] = 'B'
	} else {
		tampered[len(tampered)-10] = 'A'
	}
	_, err = r.CheckIntegration(t.Context(), string(tampered))
	wantCode(t, err, "integration_unauthorized")
}
