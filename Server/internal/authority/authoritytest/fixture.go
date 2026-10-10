// Package authoritytest builds a disposable, migrated database and an
// authority.Service over its runtime pool, with helpers that seed authority
// rows directly as the table owner. It is shared by the authority package's
// tests and by caller packages that need real accounts, devices and sessions.
package authoritytest

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RecoveryEpoch is the server_state recovery epoch the fixture installs and
// configures the Service with.
const RecoveryEpoch uint64 = 1

// Fixture is a migrated database plus a Service over its runtime pool.
type Fixture struct {
	// Pool is the runtime pool the Service uses.
	Pool *pgxpool.Pool
	// Service is configured with InstanceID, RecoveryEpoch and TokenKeys.
	Service *authority.Service
	// Now is returned by the Service clock; tests may move it.
	Now time.Time
	// DB exposes the owner and admin pools for seeding and tampering.
	DB         *testpg.Fixture
	InstanceID string
	TokenKeys  identity.DigestKeyring
}

// New migrates a disposable database, writes the server_state row and
// constructs a Service over the runtime pool.
func New(t *testing.T) *Fixture {
	t.Helper()
	db := testpg.New(t)
	f := &Fixture{Pool: db.Runtime, DB: db, Now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), InstanceID: NewID(t), TokenKeys: keyring("session.v1", 0x22)}
	f.exec(t, `INSERT INTO server_state(singleton,instance_id,recovery_epoch) VALUES (true,$1,$2)`, f.InstanceID, int64(RecoveryEpoch))
	service, err := authority.New(authority.Options{
		Pool: db.Runtime, Now: func() time.Time { return f.Now }, InstanceID: f.InstanceID, RecoveryEpoch: RecoveryEpoch,
		TokenKeys: f.TokenKeys, MaxCaregivers: 10, MaxInvitations: 10, Allowed: Allowed,
		EntitlementExempt: EntitlementExempt,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Service = service
	return f
}

// Allowed is the role/action policy the fixture configures: every role reads,
// owners and caregivers write records, owners manage the family.
func Allowed(role, action string) bool {
	switch action {
	case "read", "read_receipt":
		return role == "owner" || role == "caregiver" || role == "viewer"
	case "mutate_record", "add_baby":
		return role == "owner" || role == "caregiver"
	case "delete_baby", "manage_members", "repair_profile", "transfer_owner", "export", "export_family", "delete_family":
		return role == "owner"
	default:
		return false
	}
}

// EntitlementExempt is the legacy list of actions that skip the
// family_sync entitlement check (authority.LegacyEntitlementExempt).
func EntitlementExempt(action string) bool { return authority.LegacyEntitlementExempt(action) }

// Account inserts an active account and returns its ID.
func (f *Fixture) Account(t *testing.T) string {
	t.Helper()
	id := NewID(t)
	f.exec(t, `INSERT INTO accounts(id,display_name,status,created_at,updated_at) VALUES ($1,'Fixture','active',$2,$2)`, id, f.Now)
	return id
}

// Device inserts an active device bound to account and returns its ID.
func (f *Fixture) Device(t *testing.T, account string) string {
	t.Helper()
	id := NewID(t)
	var continuity [32]byte
	copy(continuity[:], id)
	f.exec(t, `INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES ($1,$2,'https://tracker.example','continuity.v1',$3,$4,$4)`, id, account, continuity[:], f.Now)
	return id
}

// Family inserts an active family owned by owner, the owner's active
// membership and the owner's active family slot, and returns the family ID.
func (f *Fixture) Family(t *testing.T, owner string) string {
	t.Helper()
	id := NewID(t)
	f.exec(t, `INSERT INTO families(id,display_name,status,owner_account_id,created_at,updated_at) VALUES ($1,'Fixture Family','active',$2,$3,$3)`, id, owner, f.Now)
	f.exec(t, `INSERT INTO memberships(family_id,account_id,role,status,created_at,updated_at) VALUES ($1,$2,'owner','active',$3,$3)`, id, owner, f.Now)
	f.exec(t, `INSERT INTO family_slots(account_id,family_id,state) VALUES ($1,$2,'active')`, owner, id)
	return id
}

// Session inserts an active lineage-root session for account on device and
// returns its ID and raw bearer tokens. scope is "account" (family and role
// empty), "family" or "family_recovery". The digests are
// HMAC-SHA256(TokenKeys.Current, class || 0x00 || token), class "access" or
// "refresh", as the auth service derives them.
func (f *Fixture) Session(t *testing.T, account, device, scope, family, role string) (sessionID, rawAccess, rawRefresh string) {
	t.Helper()
	sessionID, rawAccess, rawRefresh = NewID(t), newToken(t), newToken(t)
	key := f.TokenKeys.Current
	access, refresh := TokenDigest(key, "access", rawAccess), TokenDigest(key, "refresh", rawRefresh)
	var familyID, roleValue any
	if scope != "account" {
		familyID, roleValue = family, role
	}
	f.exec(t, `INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,family_id,role,recovery_epoch,issued_seq,
			token_key_id,refresh_digest,access_expires_at,refresh_expires_at,deadline_at,status,created_at)
		SELECT $1,$2,$3,$4,$2,$5,$6::text::app.session_scope,$7,$8::text::app.member_role,$9,greatest(a.audit_seq,1),$10,$11,$12,$13,$14,'active',$5
		  FROM accounts a WHERE a.id=$3`,
		access[:], sessionID, account, device, f.Now, scope, familyID, roleValue, int64(RecoveryEpoch), key.ID, refresh[:],
		f.Now.Add(time.Hour), f.Now.Add(30*24*time.Hour), f.Now.Add(365*24*time.Hour))
	return sessionID, rawAccess, rawRefresh
}

// TokenDigest is HMAC-SHA256(key.Secret, class || 0x00 || token).
func TokenDigest(key identity.DigestKey, class, token string) [32]byte {
	mac := hmac.New(sha256.New, key.Secret[:])
	mac.Write([]byte(class))
	mac.Write([]byte{0})
	mac.Write([]byte(token))
	var sum [32]byte
	copy(sum[:], mac.Sum(nil))
	return sum
}

// NewID returns a random canonical lowercase version-4 UUID.
func NewID(t testing.TB) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// Exec runs statement as the table owner inside an authority transaction and
// returns the number of rows it affected.
func (f *Fixture) Exec(t *testing.T, statement string, args ...any) int64 {
	t.Helper()
	return f.run(t, statement, args...)
}

// exec is Exec for seeds, which must affect exactly one row.
func (f *Fixture) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if affected := f.run(t, statement, args...); affected != 1 {
		t.Fatalf("seed %q affected %d rows", statement, affected)
	}
}

func (f *Fixture) run(t *testing.T, statement string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := f.DB.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(ctx, statement, args...)
	if err != nil {
		t.Fatalf("seed %q: %v", statement, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return tag.RowsAffected()
}

func newToken(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func keyring(id string, fill byte) identity.DigestKeyring {
	key := identity.DigestKey{ID: id}
	for i := range key.Secret {
		key.Secret[i] = fill
	}
	return identity.DigestKeyring{Current: key}
}
