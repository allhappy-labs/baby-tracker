package family

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const integrationFamilyGrant = "60000000-0000-4000-8000-000000000001"

type integrationFamilyFixture struct {
	*familyFixture
	reader     *authority.IntegrationReader
	familyA    string
	credential string
}

// newIntegrationFamilyFixture composes the real family policy (Allowed, and
// the entitlement requirement in hosted mode) behind the authority and gives
// the integration reader its own read-only pool, distinct from the phone
// runtime pool, as production composition does.
func newIntegrationFamilyFixture(t *testing.T, hosted bool) *integrationFamilyFixture {
	t.Helper()
	h := &integrationFamilyFixture{familyFixture: newFamilyFixtureMode(t, hosted)}
	h.familyA = h.familyID(h.Owner.AccountID)
	config := h.Runtime.Config().Copy()
	config.MaxConns = 2
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	h.reader, err = authority.NewIntegrationReader(h.Authority, pool)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *integrationFamilyFixture) issue(t *testing.T) {
	t.Helper()
	h.credential = h.issueFor(t, integrationFamilyGrant, h.Owner.AccountID, h.familyA)
}

// grantInput is an operator integration grant of grantID to accountID for
// familyID at the family's current revision.
func (h *integrationFamilyFixture) grantInput(t *testing.T, grantID, accountID, familyID string) (authority.OperatorIntegration, []byte) {
	t.Helper()
	secret := make([]byte, authority.IntegrationSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	verifier, err := authority.IntegrationVerifier(grantID, secret)
	if err != nil {
		t.Fatal(err)
	}
	return authority.OperatorIntegration{CommandID: h.CommandID(0), OperatorID: "90000000-0000-4000-8000-000000000001", Operation: "grant",
		GrantID: grantID, FamilyID: familyID, AccountID: accountID, Name: "Claude Desktop",
		Scope: authority.IntegrationScopeFamilyRead, Verifier: hex.EncodeToString(verifier[:]), ServerInstanceID: h.db.InstanceID,
		RecoveryEpoch: authoritytest.RecoveryEpoch, ValiditySeconds: authority.IntegrationValiditySeconds, PolicyID: "Integration.Policy_test-v1",
		EvidenceDigest: strings.Repeat("e", 64), ReasonCode: "integration_issued", ExpectedRevision: h.familySeq(familyID),
		ConfirmationExpiresAt: h.now.Add(10 * time.Minute).Truncate(time.Second)}, secret
}

// issueFor commits one owner grant and returns its credential.
func (h *integrationFamilyFixture) issueFor(t *testing.T, grantID, accountID, familyID string) string {
	t.Helper()
	input, secret := h.grantInput(t, grantID, accountID, familyID)
	if _, err := h.Authority.ExecuteOperatorIntegration(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	credential, err := authority.FormatIntegrationCredential(grantID, secret)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

// seedBaby writes one care baby row through the phone runtime path, as sync
// would, so the RLS-protected care_entities table holds data for that family.
func (h *integrationFamilyFixture) seedBaby(t *testing.T, accountID, familyID, babyID string) {
	t.Helper()
	tx, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`SELECT id FROM families WHERE id=$1::uuid FOR UPDATE`, []any{familyID}},
		{`UPDATE families SET next_sequence=next_sequence+1 WHERE id=$1::uuid`, []any{familyID}},
		{`INSERT INTO care_entities(family_id,id,kind,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,
			accepted_at,sort_order,name_hash,wire)
			SELECT $1::uuid,$2::uuid,1,1,next_sequence,$3::uuid,$3::uuid,'9b000000-0000-4000-8000-000000000001','create',now(),0,
			sha256(convert_to('Baby','UTF8')),convert_to(repeat('x',64),'UTF8')
			FROM families WHERE id=$1::uuid`, []any{familyID, babyID, accountID}},
	} {
		if _, err := tx.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatalf("seed baby: %v", err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("seed baby commit: %v", err)
	}
}

func (h *integrationFamilyFixture) assertAuthorized(t *testing.T, label string) {
	t.Helper()
	identity, err := h.reader.CheckIntegration(t.Context(), h.credential)
	if err != nil || identity.GrantID != integrationFamilyGrant || identity.AccountID != h.Owner.AccountID || identity.FamilyID != h.familyA {
		t.Fatalf("%s: identity=%+v err=%v", label, identity, err)
	}
	calls := 0
	if err := h.reader.WithIntegrationRead(t.Context(), h.credential, func(context.Context, pgx.Tx, access.Grant) error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("%s: read err=%v calls=%d", label, err, calls)
	}
}

func (h *integrationFamilyFixture) assertDenied(t *testing.T, label, credential string) {
	t.Helper()
	if identity, err := h.reader.CheckIntegration(t.Context(), credential); !errors.Is(err, authority.ErrIntegrationUnauthorized) || identity != (authority.IntegrationIdentity{}) {
		t.Fatalf("%s: check identity=%+v err=%v", label, identity, err)
	}
	calls := 0
	if err := h.reader.WithIntegrationRead(t.Context(), credential, func(context.Context, pgx.Tx, access.Grant) error { calls++; return nil }); !errors.Is(err, authority.ErrIntegrationUnauthorized) || calls != 0 {
		t.Fatalf("%s: read err=%v calls=%d", label, err, calls)
	}
}

func TestIntegrationReadUsesLiveFamilyPolicyAndTenantSnapshot(t *testing.T) {
	h := newIntegrationFamilyFixture(t, false)
	h.issue(t)
	h.assertAuthorized(t, "owner grant")
	var updatedAt time.Time
	err := h.reader.WithIntegrationRead(t.Context(), h.credential, func(ctx context.Context, tx pgx.Tx, grant access.Grant) error {
		if grant.AccountID != h.Owner.AccountID || grant.FamilyID != h.familyA || grant.Role != "owner" || grant.Revision != h.familySeq(h.familyA) {
			t.Errorf("grant=%+v", grant)
		}
		var families int
		if err := tx.QueryRow(ctx, `SELECT count(*), max(updated_at) FROM families`).Scan(&families, &updatedAt); err != nil {
			return err
		}
		if families != 1 {
			t.Errorf("tenant snapshot sees %d families", families)
		}
		var visible string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM families`).Scan(&visible); err != nil || visible != h.familyA {
			t.Errorf("visible family=%q err=%v", visible, err)
		}
		_, err := tx.Exec(ctx, `UPDATE families SET updated_at = updated_at + interval '1 second' WHERE id = $1::uuid`, h.familyA)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("mutation in integration read err=%v", err)
	}
	if err := h.reader.WithIntegrationRead(t.Context(), h.credential, func(ctx context.Context, tx pgx.Tx, _ access.Grant) error {
		var after time.Time
		if err := tx.QueryRow(ctx, `SELECT updated_at FROM families WHERE id = $1::uuid`, h.familyA).Scan(&after); err != nil {
			return err
		}
		if !after.Equal(updatedAt) {
			t.Errorf("family row changed: %s -> %s", updatedAt, after)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationAndPhoneCredentialsAreNotInterchangeable(t *testing.T) {
	h := newIntegrationFamilyFixture(t, false)
	h.issue(t)
	phone := h.bundles["owner-a"]
	if _, err := h.Auth.Authenticate(t.Context(), phone.AccessToken); err != nil {
		t.Fatalf("phone token is not live: %v", err)
	}
	h.assertDenied(t, "phone access token at MCP guard", phone.AccessToken)
	h.assertDenied(t, "phone refresh token at MCP guard", phone.RefreshToken)
	if principal, err := h.Auth.Authenticate(t.Context(), h.credential); auth.Code(err) != "session_revoked" || principal != (access.Principal{}) {
		t.Fatalf("app-token authentication accepted integration credential: principal=%+v code=%q err=%v", principal, auth.Code(err), err)
	}
	if bundle, err := h.Auth.Refresh(t.Context(), h.credential); err == nil || bundle.AccessToken != "" {
		t.Fatalf("app-token refresh accepted integration credential: %+v", bundle)
	}
	h.assertAuthorized(t, "integration credential after phone attempts")
}

func TestIntegrationReadDeniesAfterOwnershipTransferLeaveAndAccountDeletion(t *testing.T) {
	h := newIntegrationFamilyFixture(t, false)
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(0), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	h.issue(t)
	h.assertAuthorized(t, "owner before transfer")
	if _, err := h.Service.Transfer(t.Context(), h.Owner, h.CommandID(0), h.NewCaregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	h.assertDenied(t, "former owner after transfer", h.credential)
	former := h.refresh(h.Owner, "owner-a")
	if _, err := h.Service.Leave(t.Context(), former, h.CommandID(0)); err != nil {
		t.Fatal(err)
	}
	h.assertDenied(t, "former owner after leaving", h.credential)
	former = h.refresh(h.signIn("owner-a", "Owner A"), "owner-a")
	if _, err := (&account.Service{Authority: h.Authority}).Delete(t.Context(), former, h.CommandID(0), true); err != nil {
		t.Fatal(err)
	}
	h.assertDenied(t, "deleted account", h.credential)
}

func TestIntegrationReadDeniesIneligibleOrDeletedFamily(t *testing.T) {
	t.Run("entitlement lapses", func(t *testing.T) {
		h := newIntegrationFamilyFixture(t, true)
		h.now = h.now.Add(300 * 24 * time.Hour)
		h.issue(t)
		h.assertAuthorized(t, "entitled family")
		h.now = h.now.Add(67 * 24 * time.Hour)
		owner := h.signIn("owner-a", "Owner A")
		if _, err := h.Service.Current(t.Context(), owner); Code(err) != "entitlement_required" {
			t.Fatalf("family policy after entitlement expiry code=%q err=%v", Code(err), err)
		}
		h.assertDenied(t, "entitlement expired before grant", h.credential)
	})
	t.Run("deletion scheduled", func(t *testing.T) {
		h := newIntegrationFamilyFixture(t, false)
		h.issue(t)
		h.assertAuthorized(t, "active family")
		if _, err := h.Service.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(0), true); err != nil {
			t.Fatal(err)
		}
		h.assertDenied(t, "family scheduled for deletion", h.credential)
	})
}

// Ported from TestFamilyPolicyAuthorizesIntegrationOwnerWithoutSession: the
// integration read evaluates the family policy for the grant's account with
// no phone session at all, and the policy refuses a grant to an account that
// is not the family's owner.
func TestFamilyPolicyAuthorizesIntegrationOwnerWithoutSession(t *testing.T) {
	h := newIntegrationFamilyFixture(t, false)
	h.issue(t)
	if _, err := h.Auth.Logout(t.Context(), h.Owner, h.CommandID(0)); err != nil {
		t.Fatal(err)
	}
	var live int
	h.query(`SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active'`, []any{h.Owner.AccountID}, &live)
	if live != 0 {
		t.Fatalf("owner keeps %d live sessions", live)
	}
	if _, err := h.Auth.Authenticate(t.Context(), h.bundles["owner-a"].AccessToken); auth.Code(err) != "session_revoked" {
		t.Fatalf("owner phone session code=%q err=%v", auth.Code(err), err)
	}
	h.assertAuthorized(t, "owner grant without a phone session")
	foreign, _ := h.grantInput(t, "60000000-0000-4000-8000-000000000003", h.SecondOwner.AccountID, h.familyA)
	if _, err := h.Authority.ExecuteOperatorIntegration(t.Context(), foreign); authority.CodeOf(err) != "owner_required" {
		t.Fatalf("foreign owner grant err=%v", err)
	}
}

func integrationReadIDs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Two independent families, each with its own owner and a baby, share one
// database. Each grant reads only its bound family: the decided grant names
// that family, and the tenant snapshot exposes no row of the other family.
func TestIntegrationReadsAreTenantIsolated(t *testing.T) {
	h := newIntegrationFamilyFixture(t, false)
	familyB := h.familyID(h.SecondOwner.AccountID)
	if familyB == h.familyA {
		t.Fatal("fixture families are not independent")
	}
	const babyA, babyB = "9a000000-0000-4000-8000-0000000000a1", "9a000000-0000-4000-8000-0000000000b1"
	h.seedBaby(t, h.Owner.AccountID, h.familyA, babyA)
	h.seedBaby(t, h.SecondOwner.AccountID, familyB, babyB)
	credentialA := h.issueFor(t, integrationFamilyGrant, h.Owner.AccountID, h.familyA)
	credentialB := h.issueFor(t, "60000000-0000-4000-8000-000000000002", h.SecondOwner.AccountID, familyB)
	for _, test := range []struct {
		name, credential, account, family, baby, otherFamily, otherBaby string
	}{
		{"family A grant", credentialA, h.Owner.AccountID, h.familyA, babyA, familyB, babyB},
		{"family B grant", credentialB, h.SecondOwner.AccountID, familyB, babyB, h.familyA, babyA},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity, err := h.reader.CheckIntegration(t.Context(), test.credential)
			if err != nil || identity.AccountID != test.account || identity.FamilyID != test.family {
				t.Fatalf("identity=%+v err=%v", identity, err)
			}
			calls := 0
			err = h.reader.WithIntegrationRead(t.Context(), test.credential, func(ctx context.Context, tx pgx.Tx, grant access.Grant) error {
				calls++
				if grant.AccountID != test.account || grant.FamilyID != test.family || grant.Role != "owner" {
					t.Errorf("decided grant=%+v, want bound account %s family %s", grant, test.account, test.family)
				}
				var tenantFamily string
				if err := tx.QueryRow(ctx, `SELECT current_setting('app.family_id')`).Scan(&tenantFamily); err != nil || tenantFamily != test.family {
					t.Errorf("tenant family=%q err=%v", tenantFamily, err)
				}
				families, err := integrationReadIDs(ctx, tx, `SELECT id::text FROM families ORDER BY families.id`)
				if err != nil {
					return err
				}
				babies, err := integrationReadIDs(ctx, tx, `SELECT id::text FROM care_entities WHERE kind=1 ORDER BY care_entities.id`)
				if err != nil {
					return err
				}
				if len(families) != 1 || families[0] != test.family || len(babies) != 1 || babies[0] != test.baby {
					t.Errorf("visible families=%v babies=%v, want only %s/%s", families, babies, test.family, test.baby)
				}
				var otherFamilies, otherBabies int
				if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM families WHERE id=$1::uuid), (SELECT count(*) FROM care_entities WHERE family_id=$1::uuid OR id=$2::uuid)`,
					test.otherFamily, test.otherBaby).Scan(&otherFamilies, &otherBabies); err != nil {
					return err
				}
				if otherFamilies != 0 || otherBabies != 0 {
					t.Errorf("other family rows visible: families=%d babies=%d", otherFamilies, otherBabies)
				}
				return nil
			})
			if err != nil || calls != 1 {
				t.Fatalf("read err=%v calls=%d", err, calls)
			}
		})
	}
}
