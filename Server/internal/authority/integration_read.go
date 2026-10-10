package authority

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/database"
	"babytracker/server/internal/diagnostics"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration credentials are "btmcp1." + canonical grant UUID + "." +
// unpadded base64url of the 32-byte secret: exactly 87 ASCII bytes
// (security/integration_read.go:17-26).
const (
	integrationCredentialPrefix = "btmcp1."
	integrationGrantIDLength    = 36
	integrationSecretLength     = 43
	integrationCredentialLength = len(integrationCredentialPrefix) + integrationGrantIDLength + 1 + integrationSecretLength
	integrationReadAction       = "read"

	// IntegrationSecretBytes is the credential secret's length.
	IntegrationSecretBytes = 32
	// integrationVerifierDomain separates integration verifiers from every
	// other digest (security/integration_events.go:23).
	integrationVerifierDomain = "babytracker-mcp-v1\x00"
)

// ErrIntegrationUnauthorized is the single, deliberately indistinguishable
// denial class: malformed, unknown, mismatched, expired, revoked, foreign or
// stale credentials and lost live family authority all return it. Every
// other failure is service_unavailable (or scope_failed), or the context's
// own error when caller cancellation or the credential deadline ended the
// read.
var ErrIntegrationUnauthorized error = Code("integration_unauthorized")

// IntegrationIdentity is informational transport metadata. It is never an
// authorization capability: every data read presents the credential again.
type IntegrationIdentity struct {
	GrantID, AccountID, FamilyID string
	ExpiresAt                    time.Time
}

// IntegrationReader is the only data guard for integration credentials. It
// owns a dedicated read-only pool, never the phone runtime pool, and never
// accepts a pool, family or principal from a request caller.
type IntegrationReader struct {
	authority *Service
	pool      *pgxpool.Pool
}

// NewIntegrationReader requires a dedicated pool, distinct from the
// authority's, whose sessions default to read-only transactions
// (database.OpenReadOnlyWithMax): even a raw COMMIT inside a read callback
// cannot start a writable transaction.
func NewIntegrationReader(a *Service, pool *pgxpool.Pool) (*IntegrationReader, error) {
	if a == nil || a.pool == nil || a.now == nil {
		return nil, errors.New("authority: integration reader requires a complete authority")
	}
	if pool == nil || pool == a.pool {
		return nil, errors.New("authority: integration reader requires its own dedicated pool")
	}
	if !database.ReadOnlyByDefault(pool) {
		return nil, errors.New("authority: integration reader pool must default to read-only transactions")
	}
	return &IntegrationReader{authority: a, pool: pool}, nil
}

// IntegrationVerifier is the stored verifier of a grant's secret:
// SHA-256(domain || grant ID || secret) (security/integration_events.go:87-96).
func IntegrationVerifier(grantID string, secret []byte) ([32]byte, error) {
	if !validID(grantID) || len(secret) != IntegrationSecretBytes {
		return [32]byte{}, ErrInvalidCommand
	}
	digest := sha256.New()
	digest.Write([]byte(integrationVerifierDomain))
	digest.Write([]byte(grantID))
	digest.Write(secret)
	var out [32]byte
	copy(out[:], digest.Sum(nil))
	return out, nil
}

// FormatIntegrationCredential renders the one canonical credential encoding
// (security/integration_read.go:67-73).
func FormatIntegrationCredential(grantID string, secret []byte) (string, error) {
	if !validID(grantID) || len(secret) != IntegrationSecretBytes {
		return "", errors.New("authority: invalid integration credential material")
	}
	return integrationCredentialPrefix + grantID + "." + base64.RawURLEncoding.EncodeToString(secret), nil
}

// parseIntegrationCredential accepts only the canonical encoding. The length
// check precedes every decode, so oversized input costs nothing
// (security/integration_read.go:75-91, verbatim).
func parseIntegrationCredential(credential string) (string, []byte, bool) {
	if len(credential) != integrationCredentialLength || credential[:len(integrationCredentialPrefix)] != integrationCredentialPrefix {
		return "", nil, false
	}
	rest := credential[len(integrationCredentialPrefix):]
	grantID, encoded := rest[:integrationGrantIDLength], rest[integrationGrantIDLength+1:]
	if rest[integrationGrantIDLength] != '.' || !validID(grantID) {
		return "", nil, false
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(secret) != IntegrationSecretBytes || base64.RawURLEncoding.EncodeToString(secret) != encoded {
		return "", nil, false
	}
	return grantID, secret, true
}

// NewIntegrationSecret returns a fresh random integration secret. Only the
// operator CLI calls it, when it prepares an issue.
func NewIntegrationSecret() ([]byte, error) {
	secret := make([]byte, IntegrationSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, errors.New("authority: integration secret generation failed")
	}
	return secret, nil
}

// IntegrationCredentialVerifier returns the grant ID and the lowercase hex
// verifier of a canonical credential, or false for any non-canonical
// credential. The operator CLI uses it to bind a prepared secret file to its
// confirmed proposal (security/integration_operator.go:49-59).
func IntegrationCredentialVerifier(credential string) (string, string, bool) {
	grantID, secret, ok := parseIntegrationCredential(credential)
	if !ok {
		return "", "", false
	}
	verifier, err := IntegrationVerifier(grantID, secret)
	if err != nil {
		return "", "", false
	}
	return grantID, hex.EncodeToString(verifier[:]), true
}

// IntegrationCredentialGrantID returns the grant a canonical credential names,
// or false for any non-canonical credential. It authorizes nothing: callers
// use it only after WithIntegrationRead accepted the same credential, to bind
// derived state (such as a history continuation) to that grant
// (security/integration_read.go:93-100).
func IntegrationCredentialGrantID(credential string) (string, bool) {
	grantID, _, ok := parseIntegrationCredential(credential)
	return grantID, ok
}

// integrationAuthoritySQL is app.integration_authority (000029): the grant
// and the live authority it is bound to, in the caller's snapshot.
const integrationAuthoritySQL = `SELECT grant_found, coalesce(account_id::text,''), coalesce(family_id::text,''), coalesce(recovery_epoch,0),
	coalesce(scope,''), verifier, coalesce(status,''), expires_at, coalesce(account_status,''), coalesce(account_chain_failed,false),
	coalesce(family_status,''), coalesce(family_owner_id::text,''), coalesce(family_audit_seq,0), coalesce(family_chain_failed,false),
	coalesce(family_entitlement_required,false), coalesce(member_role,''), coalesce(member_status,''), coalesce(slot_family_id::text,''),
	coalesce(slot_state,''), coalesce(entitlement_status,''), entitlement_expires_at
	FROM app.integration_authority($1,$2)`

type integrationRow struct {
	found                                  bool
	accountID, familyID                    string
	recoveryEpoch                          uint64
	scope                                  string
	verifier                               []byte
	status                                 string
	expiresAt                              *time.Time
	accountStatus                          string
	accountChainFailed                     bool
	familyStatus, familyOwnerID            string
	familyAuditSeq                         uint64
	familyChainFailed, entitlementRequired bool
	memberRole, memberStatus               string
	slotFamilyID, slotState                string
	entitlementStatus                      string
	entitlementExpiresAt                   *time.Time
}

func scanIntegration(row pgx.Row) (integrationRow, error) {
	var r integrationRow
	err := row.Scan(&r.found, &r.accountID, &r.familyID, &r.recoveryEpoch, &r.scope, &r.verifier, &r.status, &r.expiresAt, &r.accountStatus,
		&r.accountChainFailed, &r.familyStatus, &r.familyOwnerID, &r.familyAuditSeq, &r.familyChainFailed, &r.entitlementRequired, &r.memberRole,
		&r.memberStatus, &r.slotFamilyID, &r.slotState, &r.entitlementStatus, &r.entitlementExpiresAt)
	return r, err
}

// integrationDecision is an authorized integration read.
type integrationDecision struct {
	grant     access.Grant
	identity  IntegrationIdentity
	remaining time.Duration
}

// authorize checks the stored grant and the live authority it is bound to
// (security/integration_read.go:243-277). Grants stay active across
// ownership, membership, account, family and epoch changes, so each of those
// is rechecked: the verifier matches in constant time (derived even for an
// unknown grant, so misses cost the same work); the grant is active,
// family:read, of this epoch and unexpired; the account is active; the
// family is active and owned by the account through an active owner
// membership; the account's slot is the family's and active; family policy
// grants "read" to the owner, entitled when the node or the family requires
// it. A failed audit chain is scope_failed.
func (r *IntegrationReader) authorize(row integrationRow, grantID string, secret []byte) (integrationDecision, error) {
	a := r.authority
	derived, err := IntegrationVerifier(grantID, secret)
	stored := make([]byte, 32)
	if len(row.verifier) == 32 {
		copy(stored, row.verifier)
	}
	matches := err == nil && subtle.ConstantTimeCompare(derived[:], stored) == 1
	now := a.at(time.Time{})
	if !row.found || !matches || len(row.verifier) != 32 || row.status != "active" || row.scope != IntegrationScopeFamilyRead ||
		row.recoveryEpoch == 0 || row.recoveryEpoch != a.epoch || row.expiresAt == nil || !now.Before(*row.expiresAt) {
		return integrationDecision{}, ErrIntegrationUnauthorized
	}
	if row.accountChainFailed || row.familyChainFailed {
		return integrationDecision{}, ErrScopeFailed
	}
	if row.accountStatus != "active" || row.familyStatus != "active" || row.familyOwnerID != row.accountID || row.memberRole != "owner" ||
		row.memberStatus != "active" || row.slotFamilyID != row.familyID || row.slotState != "active" || !a.allowed("owner", integrationReadAction) {
		return integrationDecision{}, ErrIntegrationUnauthorized
	}
	if !a.entitlementExempt(integrationReadAction) && (a.requireFamilySyncEntitlement || row.entitlementRequired) &&
		!entitled(row.entitlementStatus, row.entitlementExpiresAt, now) {
		return integrationDecision{}, ErrIntegrationUnauthorized
	}
	// Resample after policy so the transaction cannot outlive the credential.
	remaining := row.expiresAt.Sub(a.now())
	if remaining <= 0 {
		return integrationDecision{}, ErrIntegrationUnauthorized
	}
	expires := row.expiresAt.UTC()
	return integrationDecision{
		grant:     access.Grant{AccountID: row.accountID, FamilyID: row.familyID, Role: "owner", Revision: row.familyAuditSeq},
		identity:  IntegrationIdentity{GrantID: grantID, AccountID: row.accountID, FamilyID: row.familyID, ExpiresAt: expires},
		remaining: remaining,
	}, nil
}

type integrationReadKey struct{}

// entry refuses nested or escaped use: a callback context re-entering the
// reader would hold a second connection of its small pool.
func (r *IntegrationReader) entry(ctx context.Context) error {
	if r == nil || r.authority == nil || r.pool == nil || ctx == nil {
		return ErrUnavailable
	}
	if reader, _ := ctx.Value(integrationReadKey{}).(*IntegrationReader); reader == r {
		return ErrUnavailable
	}
	return nil
}

// CheckIntegration performs the complete live authorization with one query
// on the dedicated pool and no data transaction, so a transport can refuse a
// request before reading its body.
func (r *IntegrationReader) CheckIntegration(ctx context.Context, credential string) (IntegrationIdentity, error) {
	if err := r.entry(ctx); err != nil {
		return IntegrationIdentity{}, err
	}
	grantID, secret, ok := parseIntegrationCredential(credential)
	if !ok {
		return IntegrationIdentity{}, ErrIntegrationUnauthorized
	}
	start := time.Now()
	row, err := scanIntegration(r.pool.QueryRow(ctx, integrationAuthoritySQL, grantID, int64(r.authority.epoch)))
	diagnostics.Record(ctx, nil, diagnostics.Authority, time.Since(start))
	if err != nil {
		return IntegrationIdentity{}, integrationFailure(ctx, err)
	}
	decision, err := r.authorize(row, grantID, secret)
	if err != nil {
		return IntegrationIdentity{}, err
	}
	return decision.identity, nil
}

// WithIntegrationRead authorizes credential and runs run inside a read-only
// repeatable-read transaction on the dedicated pool, with the grant's
// account and family as tenant context. BEGIN and the authority statement
// share one round trip and the transaction's snapshot, so a revocation
// committed before the read began refuses it and an authorized read
// completes. run's context ends at the earlier of the caller's deadline and
// the credential expiry, and refuses re-entry into the reader.
func (r *IntegrationReader) WithIntegrationRead(ctx context.Context, credential string, run func(context.Context, pgx.Tx, access.Grant) error) error {
	if err := r.entry(ctx); err != nil {
		return err
	}
	if run == nil {
		return ErrUnavailable
	}
	grantID, secret, ok := parseIntegrationCredential(credential)
	if !ok {
		return ErrIntegrationUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var row integrationRow
	// Diagnostics (opt-in per request): the connection acquisition, the
	// authorization (BEGIN plus the authority statement, one round trip)
	// and the caller's read, recorded after the transaction ends.
	start, acquireStart := time.Now(), diagnostics.Start(ctx)
	tx, err := beginRequest(ctx, r.pool, requestBeginRead, func(rw pgx.Row) (err error) { row, err = scanIntegration(rw); return err },
		integrationAuthoritySQL, grantID, int64(r.authority.epoch))
	diagnostics.CaptureSince(ctx, diagnostics.TransactionAcquire, acquireStart)
	diagnostics.Record(ctx, nil, diagnostics.Authority, time.Since(start))
	if err != nil {
		return integrationFailure(ctx, err)
	}
	decision, err := r.authorize(row, grantID, secret)
	if err != nil {
		return refuse(ctx, tx, err)
	}
	databaseStart := time.Now()
	defer func() { diagnostics.Record(ctx, nil, diagnostics.Database, time.Since(databaseStart)) }()
	txCtx, cancel := context.WithTimeout(context.WithValue(ctx, integrationReadKey{}, r), decision.remaining)
	defer cancel()
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := run(txCtx, tx, decision.grant); err != nil {
		return err
	}
	if err := tx.Commit(txCtx); err != nil {
		return integrationFailure(txCtx, err)
	}
	return nil
}

// integrationFailure is the context's own error when it ended the read,
// else service_unavailable.
func integrationFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return unavailable("integration read", err)
}
