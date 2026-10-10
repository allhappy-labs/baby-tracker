package authority

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// KeyedDigest is a token digest under one named token key.
type KeyedDigest = identity.KeyedDigest

// Mode is the deployment mode; its values equal config.Mode's, so callers
// convert with authority.Mode(cfg.Mode). (config is not imported: it depends
// on the legacy security package.)
type Mode string

const (
	ModeSelfHosted Mode = "self_hosted"
	ModeHosted     Mode = "hosted"
)

// selfHostedLineageAge caps a self-hosted lineage at one year from its
// original sign-in (auth/sessions.go defaultSelfHostedAge).
const selfHostedLineageAge = 365 * 24 * time.Hour

// maxRefreshMaterial bounds a recovery receipt's material (refresh_receipts CHECK).
const maxRefreshMaterial = 32768

// DeviceBinding is a device's continuity binding: the devices row's ID,
// account, canonical origin and continuity key ID and digest.
type DeviceBinding struct {
	DeviceID, AccountID, CanonicalOrigin, CredentialKeyID string
	CredentialDigest                                      [32]byte
}

// SessionRow is a sessions row. FamilyID and Role are "" for account scope;
// ParentID, ConsumedAttemptID and ChildID are "" when NULL.
type SessionRow struct {
	ID, AccountID, DeviceID, LineageRootID, ParentID, FamilyID      string
	Scope, Role, Status                                             string
	RecoveryEpoch, IssuedSeq                                        uint64
	TokenKeyID                                                      string
	AccessDigest, RefreshDigest                                     [32]byte
	AccessExpiresAt, RefreshExpiresAt, DeadlineAt, LineageStartedAt time.Time
	CreatedAt                                                       time.Time
	ConsumedAt, RevokedAt                                           *time.Time
	ConsumedAttemptID, ChildID                                      string
}

// AccountView is the account presentation returned with a session
// (auth.AccountView).
type AccountView struct {
	ID, DisplayName string
	ProfileRequired bool
}

// FamilySlot is the account's family slot, returned for presentation only.
type FamilySlot struct{ FamilyID, State string }

// RefreshDerivationInput is everything a child session's ID, command ID and
// raw tokens are derived from. On a lost-acknowledgement retry it is rebuilt
// from the committed rows, so a derivation that reads only these fields
// reproduces the committed child exactly. Parent is the parent as it was
// before consumption (status active, no consumption fields).
type RefreshDerivationInput struct {
	Parent                            SessionRow
	ParentKey                         identity.DigestKey
	ParentDigest                      [32]byte
	OutputKeyID                       string
	Scope, FamilyID, Role             string
	AccessExpiresAt, RefreshExpiresAt time.Time
	AttemptID                         string
	Continuity                        *DeviceBinding
}

// RefreshDerivation is a derived child: the rotation's command ID, the child
// session ID and the raw child tokens.
type RefreshDerivation struct{ CommandID, SessionID, AccessToken, RefreshToken string }

// RefreshStage is what a rotation's material commits to: the frozen
// derivation input, the child row as inserted (IssuedSeq included) and the
// effective policy deadline. Material must not depend on anything else.
type RefreshStage struct {
	Input             RefreshDerivationInput
	Child             SessionRow
	EffectiveDeadline time.Time
}

// Refresh rotates the session whose refresh token digests (one per token
// key) are ParentRefreshDigestCandidates. AttemptID "" is the legacy
// non-recoverable refresh; otherwise the rotation commits a recovery receipt
// holding Material's bytes and needs exactly one device proof: Continuity,
// the caller's binding compared with the device row, or ProveContinuity,
// called under the locks with the device row's binding (the caller recomputes
// its keyed proof under the stored key ID). A failed proof is
// device_continuity_invalid and never revokes. Derive and Material see the
// proven binding as Continuity.
type Refresh struct {
	ParentRefreshDigestCandidates []KeyedDigest
	At                            time.Time // zero: the service clock after the locks
	AttemptID                     string
	Continuity                    *DeviceBinding
	ProveContinuity               func(stored DeviceBinding) bool
	Derive                        func(RefreshDerivationInput) (RefreshDerivation, error)
	OutputKeyID                   string // "" means the current token key
	AccessTTL, RefreshTTL         time.Duration
	Mode                          Mode
	Material                      func(RefreshStage) ([]byte, [32]byte, error)
}

// RefreshResult is a rotation's outcome, or the committed rotation recovered
// by an exact lost-acknowledgement retry (Recovered). EffectiveDeadline is
// the mode's reauthentication deadline (the receipt's, on recovery).
type RefreshResult struct {
	Result
	Child, Parent             SessionRow
	AccessToken, RefreshToken string
	Recovered                 bool
	EffectiveDeadline         time.Time
	FamilySlot                *FamilySlot
	Account                   AccountView
}

const sessionColumns = `id::text, account_id::text, device_id::text, lineage_root_id::text, coalesce(parent_id::text,''), coalesce(family_id::text,''),
	scope::text, coalesce(role::text,''), status::text, recovery_epoch, issued_seq, token_key_id, access_digest, refresh_digest,
	access_expires_at, refresh_expires_at, deadline_at, lineage_started_at, created_at, consumed_at, revoked_at,
	coalesce(consumed_attempt_id::text,''), coalesce(child_id::text,'')`

const insertSessionSQL = `INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,parent_id,lineage_started_at,scope,family_id,role,
		recovery_epoch,issued_seq,token_key_id,refresh_digest,access_expires_at,refresh_expires_at,deadline_at,status,created_at)
	VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,$7,$8::text::app.session_scope,NULLIF($9,'')::uuid,NULLIF($10,'')::text::app.member_role,
		$11,$12,$13,$14,$15,$16,$17,'active',$18)`

func insertSessionArgs(row SessionRow) []any {
	return []any{row.AccessDigest[:], row.ID, row.AccountID, row.DeviceID, row.LineageRootID, row.ParentID, row.LineageStartedAt, row.Scope, row.FamilyID,
		row.Role, int64(row.RecoveryEpoch), int64(row.IssuedSeq), row.TokenKeyID, row.RefreshDigest[:], row.AccessExpiresAt, row.RefreshExpiresAt,
		row.DeadlineAt, row.CreatedAt}
}

func scanSession(row pgx.Row) (SessionRow, error) {
	var s SessionRow
	var access, refresh []byte
	if err := row.Scan(&s.ID, &s.AccountID, &s.DeviceID, &s.LineageRootID, &s.ParentID, &s.FamilyID, &s.Scope, &s.Role, &s.Status, &s.RecoveryEpoch,
		&s.IssuedSeq, &s.TokenKeyID, &access, &refresh, &s.AccessExpiresAt, &s.RefreshExpiresAt, &s.DeadlineAt, &s.LineageStartedAt, &s.CreatedAt,
		&s.ConsumedAt, &s.RevokedAt, &s.ConsumedAttemptID, &s.ChildID); err != nil {
		return SessionRow{}, err
	}
	if len(access) != 32 || len(refresh) != 32 {
		return SessionRow{}, fmt.Errorf("authority: corrupt session digests: %w", ErrUnavailable)
	}
	copy(s.AccessDigest[:], access)
	copy(s.RefreshDigest[:], refresh)
	for _, at := range []*time.Time{&s.AccessExpiresAt, &s.RefreshExpiresAt, &s.DeadlineAt, &s.LineageStartedAt, &s.CreatedAt, s.ConsumedAt, s.RevokedAt} {
		if at != nil {
			*at = at.UTC()
		}
	}
	return s, nil
}

// sessionByID reads one session; lock is "" or a locking clause.
func sessionByID(ctx context.Context, tx pgx.Tx, id, lock string) (SessionRow, bool, error) {
	row, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id=$1 `+lock, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionRow{}, false, nil
	}
	if err != nil {
		return SessionRow{}, false, fmt.Errorf("authority: read session: %w", errors.Join(err, ErrUnavailable))
	}
	return row, true, nil
}

// tokenDigest is HMAC-SHA256(secret, class || 0x00 || token) (auth/tokens.go).
func tokenDigest(key identity.DigestKey, class, token string) [32]byte {
	mac := hmac.New(sha256.New, key.Secret[:])
	mac.Write([]byte(class))
	mac.Write([]byte{0})
	mac.Write([]byte(token))
	var sum [32]byte
	copy(sum[:], mac.Sum(nil))
	return sum
}

// tokenKey returns the configured token key named id.
func (s *Service) tokenKey(id string) (identity.DigestKey, bool) {
	if s.tokenKeys.Current.ID == id {
		return s.tokenKeys.Current, true
	}
	for _, key := range s.tokenKeys.Previous {
		if key.ID == id {
			return key, true
		}
	}
	return identity.DigestKey{}, false
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// at is the command time: the caller's, or the service clock (read after the
// command's locks), at microsecond precision.
func (s *Service) at(at time.Time) time.Time {
	if at.IsZero() {
		at = s.now()
	}
	return at.UTC().Truncate(time.Microsecond)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// effectiveDeadline is the reauthentication deadline: deadline_at, and for a
// self-hosted node at most one year from the lineage's sign-in
// (auth/refresh.go sessionDeadline).
func effectiveDeadline(row SessionRow, mode Mode) time.Time {
	if mode == ModeSelfHosted {
		return minTime(row.DeadlineAt, row.LineageStartedAt.Add(selfHostedLineageAge))
	}
	return row.DeadlineAt
}

func validBinding(b *DeviceBinding) bool {
	return b != nil && validID(b.DeviceID) && validID(b.AccountID) && keyIDPattern.MatchString(b.CredentialKeyID) && validOrigin(b.CanonicalOrigin)
}

// validOrigin is the devices.canonical_origin shape: 1..2048 bytes of ASCII.
func validOrigin(origin string) bool {
	if len(origin) == 0 || len(origin) > 2048 {
		return false
	}
	for i := 0; i < len(origin); i++ {
		if origin[i] < 0x21 || origin[i] > 0x7e {
			return false
		}
	}
	return true
}

// accountAuthority is what a session's account currently grants: the
// device, the family slot with the account's membership in that family, the
// family's status and family_sync entitlement, and the presentation.
type accountAuthority struct {
	deviceFound              bool
	device                   DeviceBinding
	deviceStatus             string
	displayName              string
	profileRequired          bool
	identityLive             bool
	slot                     *FamilySlot
	memberRole, memberStatus string
	familyStatus             string
	entitlementStatus        string
	entitlementExpiresAt     *time.Time
}

// readAccountAuthority reads accountID's authority in one statement.
func readAccountAuthority(ctx context.Context, tx pgx.Tx, accountID, deviceID string) (accountAuthority, error) {
	var a accountAuthority
	var deviceID2, origin, keyID, slotFamily, slotState, role, memberStatus, familyStatus, deviceStatus, entitlement *string
	var digest []byte
	err := tx.QueryRow(ctx, `SELECT d.id::text, d.status::text, d.canonical_origin, d.continuity_key_id, d.continuity_digest,
			a.display_name, a.profile_required, EXISTS (SELECT 1 FROM account_identities i WHERE i.account_id=a.id AND i.live),
			s.family_id::text, s.state::text, m.role::text, m.status::text, f.status::text, e.status::text, e.expires_at
		FROM accounts a
		LEFT JOIN devices d ON d.id=$2 AND d.account_id=a.id
		LEFT JOIN family_slots s ON s.account_id=a.id
		LEFT JOIN memberships m ON m.family_id=s.family_id AND m.account_id=a.id
		LEFT JOIN families f ON f.id=s.family_id
		LEFT JOIN family_entitlements e ON e.family_id=s.family_id AND e.entitlement_key='family_sync'
		WHERE a.id=$1`, accountID, deviceID).Scan(&deviceID2, &deviceStatus, &origin, &keyID, &digest, &a.displayName, &a.profileRequired, &a.identityLive,
		&slotFamily, &slotState, &role, &memberStatus, &familyStatus, &entitlement, &a.entitlementExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return accountAuthority{}, ErrSessionRevoked
	}
	if err != nil {
		return accountAuthority{}, fmt.Errorf("authority: read account authority: %w", errors.Join(err, ErrUnavailable))
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if deviceID2 != nil && len(digest) == 32 {
		a.deviceFound = true
		a.deviceStatus = deref(deviceStatus)
		a.device = DeviceBinding{DeviceID: *deviceID2, AccountID: accountID, CanonicalOrigin: deref(origin), CredentialKeyID: deref(keyID)}
		copy(a.device.CredentialDigest[:], digest)
	}
	if slotFamily != nil {
		a.slot = &FamilySlot{FamilyID: *slotFamily, State: deref(slotState)}
	}
	a.memberRole, a.memberStatus, a.familyStatus, a.entitlementStatus = deref(role), deref(memberStatus), deref(familyStatus), deref(entitlement)
	return a, nil
}

// scope is the session scope the account's slot grants (auth/refresh.go
// authorityFamilySlot): no slot is account scope; an active slot is family
// scope with the active membership's role; a deletion-recovery slot is
// family_recovery with the (former) owner membership's role.
func (a accountAuthority) scope() (scope, familyID, role string, err error) {
	if a.slot == nil {
		return "account", "", "", nil
	}
	switch a.slot.State {
	case "active":
		if a.memberStatus != "active" || a.memberRole == "" {
			return "", "", "", ErrSessionRevoked
		}
		return "family", a.slot.FamilyID, a.memberRole, nil
	case "family_recovery":
		if a.memberRole == "" {
			return "", "", "", ErrSessionRevoked
		}
		return "family_recovery", a.slot.FamilyID, a.memberRole, nil
	default:
		return "", "", "", ErrSessionRevoked
	}
}

// validateRecoverableScope is refresh_recovery.go validateRecoverySession's
// scope part for a session of sessionScope/sessionFamily: a family-bound
// session must still match the account's slot, and a family-scope session
// needs an active membership, an active family and, hosted, an unexpired
// active family_sync entitlement.
func (a accountAuthority) validateRecoverableScope(sessionScope, sessionFamily string, now time.Time, mode Mode) error {
	if sessionScope == "account" {
		return nil
	}
	current, family, _, err := a.scope()
	if err != nil || sessionScope != current || sessionFamily != family {
		return ErrSessionRevoked
	}
	if sessionScope != "family" {
		return nil
	}
	if mode == ModeHosted && (a.entitlementStatus != "active" || (a.entitlementExpiresAt != nil && !now.Before(*a.entitlementExpiresAt))) {
		return ErrSessionRevoked
	}
	if a.memberStatus != "active" || a.familyStatus != "active" {
		return ErrSessionRevoked
	}
	return nil
}

// view is the account presentation; the account's identity must be live
// (auth/refresh.go refreshAccountView).
func (a accountAuthority) view(accountID string) (AccountView, error) {
	if !a.identityLive {
		return AccountView{}, fmt.Errorf("authority: account %s has no live identity: %w", accountID, ErrUnavailable)
	}
	return AccountView{ID: accountID, DisplayName: a.displayName, ProfileRequired: a.profileRequired}, nil
}

// proves checks a device proof against the session's device row
// (auth/refresh_recovery.go:88-95): the row exists, is active and is the
// session's account and device, and prove accepts its binding. prove gets a
// copy of the binding.
func (a accountAuthority) proves(accountID, deviceID string, prove func(DeviceBinding) bool) bool {
	return prove != nil && a.deviceFound && a.deviceStatus == "active" && a.device.DeviceID == deviceID && a.device.AccountID == accountID &&
		prove(a.device)
}

// oneProof reports whether exactly one device proof form is given, and a
// given binding is well formed.
func oneProof(binding *DeviceBinding, prove func(DeviceBinding) bool) bool {
	if binding != nil {
		return prove == nil && validBinding(binding)
	}
	return prove != nil
}

// proofOf is the proof function of either form: a binding proves by equality
// with the device row.
func proofOf(binding *DeviceBinding, prove func(DeviceBinding) bool) func(DeviceBinding) bool {
	if binding == nil {
		return prove
	}
	expected := *binding
	return func(stored DeviceBinding) bool { return stored == expected }
}

// findParent resolves the session a refresh token names, without a lock:
// exactly one row whose refresh digest and token key match one candidate
// (auth/refresh.go:329-348). It returns the matching configured key.
func (s *Service) findParent(ctx context.Context, tx pgx.Tx, candidates []KeyedDigest) (SessionRow, identity.DigestKey, error) {
	digests := make([][]byte, 0, len(candidates))
	for _, candidate := range candidates {
		digests = append(digests, append([]byte(nil), candidate.Sum[:]...))
	}
	rows, err := tx.Query(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE refresh_digest = ANY($1::bytea[])`, digests)
	if err != nil {
		return SessionRow{}, identity.DigestKey{}, fmt.Errorf("authority: find refresh session: %w", errors.Join(err, ErrUnavailable))
	}
	defer rows.Close()
	var matched SessionRow
	var key identity.DigestKey
	matches := 0
	for rows.Next() {
		row, err := scanSession(rows)
		if err != nil {
			return SessionRow{}, identity.DigestKey{}, fmt.Errorf("authority: find refresh session: %w", errors.Join(err, ErrUnavailable))
		}
		for _, candidate := range candidates {
			if candidate.KeyID != row.TokenKeyID || candidate.Sum != row.RefreshDigest {
				continue
			}
			configured, ok := s.tokenKey(candidate.KeyID)
			if !ok {
				continue
			}
			matched, key = row, configured
			matches++
		}
	}
	if err := rows.Err(); err != nil {
		return SessionRow{}, identity.DigestKey{}, fmt.Errorf("authority: find refresh session: %w", errors.Join(err, ErrUnavailable))
	}
	switch matches {
	case 0:
		return SessionRow{}, identity.DigestKey{}, ErrSessionRevoked
	case 1:
		return matched, key, nil
	default:
		return SessionRow{}, identity.DigestKey{}, fmt.Errorf("authority: ambiguous refresh credential: %w", ErrUnavailable)
	}
}

func validCandidates(candidates []KeyedDigest) bool {
	if len(candidates) == 0 || len(candidates) > 8 {
		return false
	}
	for _, candidate := range candidates {
		if !keyIDPattern.MatchString(candidate.KeyID) {
			return false
		}
	}
	return true
}

// Refresh rotates a session (session_rotated), returns the committed child
// to an exact lost-acknowledgement retry, or revokes the lineage on reuse
// (session_revoked; refresh_reused, or refresh_attempt_conflict for a
// recoverable attempt). Locks: the parent's account row, then the parent
// session row (the global order is accounts, families, sessions); a reuse
// also locks the lineage's active sessions. A rotation writes no command
// receipt (the consumed parent row records it); a recoverable rotation
// writes the refresh_receipts row its lost-acknowledgement retry restores.
func (s *Service) Refresh(ctx context.Context, in Refresh) (RefreshResult, error) {
	outputKeyID := in.OutputKeyID
	if outputKeyID == "" {
		outputKeyID = s.tokenKeys.Current.ID
	}
	outputKey, outputOK := s.tokenKey(outputKeyID)
	recoverable := in.AttemptID != ""
	if !validCandidates(in.ParentRefreshDigestCandidates) || in.Derive == nil || in.Material == nil || !outputOK || in.AccessTTL <= 0 || in.RefreshTTL <= 0 ||
		(in.Mode != ModeSelfHosted && in.Mode != ModeHosted) || (!in.At.IsZero() && !validTime(in.At)) ||
		(recoverable && (!validID(in.AttemptID) || !oneProof(in.Continuity, in.ProveContinuity))) ||
		(!recoverable && (in.Continuity != nil || in.ProveContinuity != nil)) {
		return RefreshResult{}, ErrInvalidCommand
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return RefreshResult{}, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	found, key, err := s.findParent(ctx, tx, in.ParentRefreshDigestCandidates)
	if err != nil {
		return RefreshResult{}, err
	}
	if found.RecoveryEpoch != s.epoch {
		return RefreshResult{}, ErrEpochChanged
	}
	accounts, err := lockAccounts(ctx, tx, found.AccountID)
	if err != nil {
		return RefreshResult{}, err
	}
	account, accountOK := accounts[found.AccountID]
	// Re-read the parent under its lock: it may have been consumed or
	// revoked since the unlocked read.
	parent, ok, err := sessionByID(ctx, tx, found.ID, "FOR UPDATE")
	if err != nil {
		return RefreshResult{}, err
	}
	if !ok || parent.RefreshDigest != found.RefreshDigest || parent.TokenKeyID != found.TokenKeyID {
		return RefreshResult{}, ErrSessionRevoked
	}
	now := s.at(in.At)
	authority, err := readAccountAuthority(ctx, tx, parent.AccountID, parent.DeviceID)
	if err != nil {
		return RefreshResult{}, err
	}
	// The device proof is checked first and never leads to a revocation
	// (refresh_recovery.go:88-95).
	if recoverable {
		if !authority.proves(parent.AccountID, parent.DeviceID, proofOf(in.Continuity, in.ProveContinuity)) {
			return RefreshResult{}, ErrDeviceContinuityInvalid
		}
		in.Continuity, in.ProveContinuity = copyBinding(&authority.device), nil
	}
	// ResolveSessionActor: the account is active and the device exists.
	if !accountOK || account.Status != "active" || !authority.deviceFound {
		return RefreshResult{}, ErrSessionRevoked
	}
	op := refreshOp{s: s, tx: tx, in: in, account: account, authority: authority, parent: parent, key: key, now: now}
	switch parent.Status {
	case "consumed":
		if !recoverable {
			return op.revokeLineage(ctx, ErrRefreshReused)
		}
		return op.recoverOrConflict(ctx)
	case "active":
		return op.rotate(ctx, outputKey)
	default:
		return RefreshResult{}, ErrSessionRevoked
	}
}

type refreshOp struct {
	s         *Service
	tx        pgx.Tx
	in        Refresh
	account   accountRow
	authority accountAuthority
	parent    SessionRow
	key       identity.DigestKey
	now       time.Time
}

// rotate consumes the active parent and inserts its child
// (auth/refresh.go:147-239, refresh_recovery.go:283-351).
func (op refreshOp) rotate(ctx context.Context, outputKey identity.DigestKey) (RefreshResult, error) {
	parent, now, recoverable := op.parent, op.now, op.in.AttemptID != ""
	deadline := effectiveDeadline(parent, op.in.Mode)
	if !now.Before(deadline) || !now.Before(parent.RefreshExpiresAt) {
		return RefreshResult{}, ErrReauthenticationRequired
	}
	if op.authority.deviceStatus != "active" {
		return RefreshResult{}, ErrSessionRevoked
	}
	scope, familyID, role, err := op.authority.scope()
	if err != nil {
		return RefreshResult{}, err
	}
	// The parent's own scope must still hold, then the upgraded one, for a
	// legacy rotation as for a recoverable one: a family session whose
	// family_sync entitlement lapsed (hosted), or whose family or membership
	// is no longer active, is not renewed, and an account session is not
	// upgraded into such a family (session_revoked).
	if err := op.authority.validateRecoverableScope(parent.Scope, parent.FamilyID, now, op.in.Mode); err != nil {
		return RefreshResult{}, err
	}
	if err := op.authority.validateRecoverableScope(scope, familyID, now, op.in.Mode); err != nil {
		return RefreshResult{}, err
	}
	view, err := op.authority.view(parent.AccountID)
	if err != nil {
		return RefreshResult{}, err
	}
	refreshExpires := minTime(now.Add(op.in.RefreshTTL), parent.DeadlineAt)
	input := RefreshDerivationInput{Parent: parent, ParentKey: op.key, ParentDigest: parent.RefreshDigest, OutputKeyID: outputKey.ID,
		Scope: scope, FamilyID: familyID, Role: role, AccessExpiresAt: minTime(now.Add(op.in.AccessTTL), refreshExpires), RefreshExpiresAt: refreshExpires,
		AttemptID: op.in.AttemptID, Continuity: copyBinding(op.in.Continuity)}
	derived, err := op.in.Derive(input)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("authority: derive refresh: %w", errors.Join(err, ErrUnavailable))
	}
	if !validID(derived.CommandID) || !validID(derived.SessionID) || derived.SessionID == parent.ID || derived.AccessToken == "" || derived.RefreshToken == "" {
		return RefreshResult{}, fmt.Errorf("authority: invalid refresh derivation: %w", ErrUnavailable)
	}
	if op.account.Chain.Seq >= maxWireInteger {
		return RefreshResult{}, fmt.Errorf("authority: account chain exhausted: %w", ErrUnavailable)
	}
	child := SessionRow{ID: derived.SessionID, AccountID: parent.AccountID, DeviceID: parent.DeviceID, LineageRootID: parent.LineageRootID,
		ParentID: parent.ID, FamilyID: familyID, Scope: scope, Role: role, Status: "active", RecoveryEpoch: parent.RecoveryEpoch,
		IssuedSeq: op.account.Chain.Seq + 1, TokenKeyID: outputKey.ID, AccessDigest: tokenDigest(outputKey, "access", derived.AccessToken),
		RefreshDigest: tokenDigest(outputKey, "refresh", derived.RefreshToken), AccessExpiresAt: input.AccessExpiresAt, RefreshExpiresAt: refreshExpires,
		DeadlineAt: parent.DeadlineAt, LineageStartedAt: parent.LineageStartedAt, CreatedAt: now}
	material, materialDigest, err := op.material(RefreshStage{Input: input, Child: child, EffectiveDeadline: deadline})
	if err != nil {
		return RefreshResult{}, err
	}
	payload := map[string]any{"session_id": child.ID, "parent_session_id": parent.ID, "lineage_root_session_id": parent.LineageRootID,
		"material_digest": hex.EncodeToString(materialDigest[:])}
	if recoverable {
		payload["refresh_attempt_id"] = op.in.AttemptID
	}
	seq, _, err := appendAudit(ctx, op.tx, op.account.Chain, KindSessionRotated, derived.CommandID, parent.AccountID, now, payload)
	if err != nil {
		return RefreshResult{}, err
	}
	if seq != child.IssuedSeq {
		return RefreshResult{}, fmt.Errorf("authority: rotation seq %d, want %d: %w", seq, child.IssuedSeq, ErrUnavailable)
	}
	steps := []batchStep{
		{`UPDATE sessions SET status='consumed', consumed_at=$2, consumed_attempt_id=NULLIF($3,'')::uuid, child_id=$4 WHERE id=$1 AND status='active'`,
			[]any{parent.ID, now, op.in.AttemptID, child.ID}, 1},
		{insertSessionSQL, insertSessionArgs(child), 1},
	}
	// Every rotation, legacy included, proves possession of the
	// predecessor's child (000025:718), retiring that receipt.
	steps = append(steps, batchStep{`DELETE FROM refresh_receipts WHERE child_id=$1`, []any{parent.ID}, -1})
	if recoverable {
		// The receipt freezes the child's scope, family and role: a later
		// rescope of the child row (ForfeitRecovery, RecoverFamily,
		// StartFamilyPurge, a role change) must not change what a
		// lost-acknowledgement retry re-derives.
		steps = append(steps,
			batchStep{`INSERT INTO refresh_receipts(parent_id,child_id,attempt_id,account_id,device_id,material,material_digest,created_at,effective_deadline,refresh_expires_at,
					child_scope,child_family_id,child_role)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::text::app.session_scope,NULLIF($12,'')::uuid,NULLIF($13,'')::text::app.member_role)`,
				[]any{parent.ID, child.ID, op.in.AttemptID, parent.AccountID, parent.DeviceID, material, materialDigest[:], now, deadline, child.RefreshExpiresAt,
					child.Scope, child.FamilyID, child.Role}, 1})
	}
	if err := runBatch(ctx, op.tx, steps); err != nil {
		return RefreshResult{}, err
	}
	if err := refreshFault(ctx, "before_authority_commit"); err != nil {
		return RefreshResult{}, errors.Join(err, ErrUnavailable)
	}
	if err := op.tx.Commit(ctx); err != nil {
		return RefreshResult{}, fmt.Errorf("authority: commit refresh: %w", errors.Join(err, ErrUnavailable))
	}
	for _, point := range []string{"after_authority_commit", "after_projection"} {
		if err := refreshFault(ctx, point); err != nil {
			return RefreshResult{}, errors.Join(err, ErrUnavailable)
		}
	}
	consumed := parent
	consumed.Status, consumed.ConsumedAt, consumed.ConsumedAttemptID, consumed.ChildID = "consumed", &now, op.in.AttemptID, child.ID
	return RefreshResult{Result: Result{CommandID: derived.CommandID, Outcome: "committed", Code: "ok", Revision: seq}, Child: child, Parent: consumed,
		AccessToken: derived.AccessToken, RefreshToken: derived.RefreshToken, EffectiveDeadline: deadline, FamilySlot: copySlot(op.authority.slot), Account: view}, nil
}

// material encodes a stage and checks its digest.
//
// Material never sees a key secret: the stage's parent key carries its ID
// only (the material is persisted in refresh_receipts; legacy froze only
// parent_key_id). Derive alone receives the real key.
func (op refreshOp) material(stage RefreshStage) ([]byte, [32]byte, error) {
	stage.Input.ParentKey.Secret = [32]byte{}
	material, digest, err := op.in.Material(stage)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("authority: encode refresh material: %w", errors.Join(err, ErrUnavailable))
	}
	if sha256.Sum256(material) != digest || (op.in.AttemptID != "" && (len(material) == 0 || len(material) > maxRefreshMaterial)) {
		return nil, [32]byte{}, fmt.Errorf("authority: invalid refresh material: %w", ErrUnavailable)
	}
	return material, digest, nil
}

// recoverOrConflict handles a recoverable refresh of a consumed parent
// (refresh_recovery.go:113-152, 283-308): the exact attempt gets the
// committed child back; another attempt revokes the lineage.
func (op refreshOp) recoverOrConflict(ctx context.Context) (RefreshResult, error) {
	parent := op.parent
	if parent.ConsumedAttemptID == "" || parent.ChildID == "" {
		return RefreshResult{}, ErrRefreshReceiptUnavailable
	}
	child, ok, err := sessionByID(ctx, op.tx, parent.ChildID, "")
	if err != nil {
		return RefreshResult{}, err
	}
	if !ok || child.ParentID != parent.ID || child.AccountID != parent.AccountID || child.DeviceID != parent.DeviceID ||
		child.LineageRootID != parent.LineageRootID || child.RecoveryEpoch != parent.RecoveryEpoch {
		return RefreshResult{}, ErrSessionRevoked
	}
	if child.Status != "active" {
		// A consumed child is a successor rotation: the lineage lives on and
		// this parent is no longer recoverable. A revoked child under another
		// attempt is a replay into a lineage already revoked: nothing is left
		// to revoke.
		if child.Status == "revoked" && op.in.AttemptID != parent.ConsumedAttemptID {
			return RefreshResult{}, ErrSessionRevoked
		}
		return RefreshResult{}, ErrRefreshReceiptUnavailable
	}
	// Another attempt is a reuse whether or not a receipt exists or
	// restores: it never depends on the receipt, so a rescoped child, a
	// receipt written before 000031 or a damaged one cannot spare the
	// lineage. Nor can the child's scope: a child whose scope no longer
	// holds (a hosted family_sync entitlement lapsed) is still live, so the
	// reuse revokes it. Only an expired child or a revoked device, which
	// leave nothing to protect, are refused without a revocation
	// (validateRecoverySession).
	if op.in.AttemptID != parent.ConsumedAttemptID {
		if err := op.validateChildLife(child); err != nil {
			return RefreshResult{}, err
		}
		return op.revokeLineage(ctx, ErrRefreshAttemptConflict)
	}
	var receipt refreshReceipt
	err = op.tx.QueryRow(ctx, `SELECT child_id::text, account_id::text, device_id::text, material, material_digest, effective_deadline,
			child_scope::text, coalesce(child_family_id::text,''), coalesce(child_role::text,'')
		FROM refresh_receipts WHERE parent_id=$1 AND attempt_id=$2`, parent.ID, parent.ConsumedAttemptID).
		Scan(&receipt.childID, &receipt.accountID, &receipt.deviceID, &receipt.material, &receipt.materialDigest, &receipt.deadline,
			&receipt.scope, &receipt.familyID, &receipt.role)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefreshResult{}, ErrRefreshReceiptUnavailable
	}
	if err != nil {
		return RefreshResult{}, fmt.Errorf("authority: read refresh receipt: %w", errors.Join(err, ErrUnavailable))
	}
	receipt.deadline = receipt.deadline.UTC()
	return op.restore(child, receipt) // read-only: the transaction rolls back
}

// refreshReceipt is a recovery receipt row. scope, familyID and role are the
// child's scope frozen at the rotation; scope is nil only on receipts written
// before 000031. familyID and role are "" for an account scope and on those
// receipts (000031 refresh_receipts_child_scope_shape).
type refreshReceipt struct {
	childID, accountID, deviceID string
	material, materialDigest     []byte
	deadline                     time.Time
	scope                        *string
	familyID, role               string
}

// validateChild is validateRecoverySession for the committed child: its
// life, then its current scope.
func (op refreshOp) validateChild(child SessionRow) error {
	if err := op.validateChildLife(child); err != nil {
		return err
	}
	return op.authority.validateRecoverableScope(child.Scope, child.FamilyID, op.now, op.in.Mode)
}

// validateChildLife is validateChild without the scope: the child and its
// device are active and the child is within its deadline and refresh expiry.
func (op refreshOp) validateChildLife(child SessionRow) error {
	if child.Status != "active" || op.authority.deviceStatus != "active" {
		return ErrSessionRevoked
	}
	if !op.now.Before(effectiveDeadline(child, op.in.Mode)) || !op.now.Before(child.RefreshExpiresAt) {
		return ErrReauthenticationRequired
	}
	return nil
}

// restore re-derives the committed child from its frozen inputs and checks
// it against the child row and the receipt (refresh_recovery.go:213-269).
// The scope, family and role come from the receipt, frozen at the rotation
// (legacy frozen.Scope), not from the child row a later command may have
// rescoped; the child is then validated, and returned, with its current
// scope, so the client learns it.
func (op refreshOp) restore(child SessionRow, receipt refreshReceipt) (RefreshResult, error) {
	parent := op.parent
	material, materialDigest, receiptDeadline := receipt.material, receipt.materialDigest, receipt.deadline
	if receipt.childID != child.ID || receipt.accountID != parent.AccountID || receipt.deviceID != parent.DeviceID || len(materialDigest) != 32 ||
		sha256.Sum256(material) != [32]byte(materialDigest) {
		return RefreshResult{}, fmt.Errorf("authority: refresh receipt does not match its child: %w", ErrUnavailable)
	}
	if receipt.scope == nil {
		// Written before 000031: the child's scope at the rotation is lost.
		return RefreshResult{}, ErrRefreshReceiptUnavailable
	}
	outputKey, ok := op.s.tokenKey(child.TokenKeyID)
	if !ok {
		return RefreshResult{}, fmt.Errorf("authority: child token key %s is not configured: %w", child.TokenKeyID, ErrUnavailable)
	}
	frozen := child
	frozen.Scope, frozen.FamilyID, frozen.Role = *receipt.scope, receipt.familyID, receipt.role
	before := parent
	before.Status, before.ConsumedAt, before.ConsumedAttemptID, before.ChildID = "active", nil, "", ""
	input := RefreshDerivationInput{Parent: before, ParentKey: op.key, ParentDigest: parent.RefreshDigest, OutputKeyID: child.TokenKeyID,
		Scope: frozen.Scope, FamilyID: frozen.FamilyID, Role: frozen.Role, AccessExpiresAt: child.AccessExpiresAt, RefreshExpiresAt: child.RefreshExpiresAt,
		AttemptID: parent.ConsumedAttemptID, Continuity: copyBinding(op.in.Continuity)}
	derived, err := op.in.Derive(input)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("authority: derive recovered refresh: %w", errors.Join(err, ErrUnavailable))
	}
	if derived.SessionID != child.ID || tokenDigest(outputKey, "access", derived.AccessToken) != child.AccessDigest ||
		tokenDigest(outputKey, "refresh", derived.RefreshToken) != child.RefreshDigest {
		return RefreshResult{}, fmt.Errorf("authority: recovered refresh does not reproduce the committed child: %w", ErrUnavailable)
	}
	recomputed, recomputedDigest, err := op.material(RefreshStage{Input: input, Child: frozen, EffectiveDeadline: receiptDeadline})
	if err != nil {
		return RefreshResult{}, err
	}
	if recomputedDigest != [32]byte(materialDigest) || !bytes.Equal(recomputed, material) {
		return RefreshResult{}, fmt.Errorf("authority: refresh receipt material mismatch: %w", ErrUnavailable)
	}
	if err := op.validateChild(child); err != nil {
		return RefreshResult{}, err
	}
	if !op.now.Before(receiptDeadline) {
		return RefreshResult{}, ErrReauthenticationRequired
	}
	view, err := op.authority.view(parent.AccountID)
	if err != nil {
		return RefreshResult{}, err
	}
	return RefreshResult{Result: Result{CommandID: derived.CommandID, Outcome: "committed", Code: "ok", Revision: child.IssuedSeq}, Child: child, Parent: parent,
		AccessToken: derived.AccessToken, RefreshToken: derived.RefreshToken, Recovered: true, EffectiveDeadline: receiptDeadline,
		FamilySlot: copySlot(op.authority.slot), Account: view}, nil
}

// revokeLineage revokes every active session of the parent's lineage
// (session_revoked, reason refresh_reused), deletes the lineage's recovery
// receipts, commits and returns code (prepared.go:247-268,
// event_handlers.go:490-516). A lineage with no active session is
// session_revoked and writes nothing.
func (op refreshOp) revokeLineage(ctx context.Context, code error) (RefreshResult, error) {
	parent := op.parent
	rows, err := op.tx.Query(ctx, `SELECT id::text FROM sessions WHERE lineage_root_id=$1 AND status='active' ORDER BY sessions.id FOR UPDATE`, parent.LineageRootID)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("authority: lock lineage: %w", errors.Join(err, ErrUnavailable))
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return RefreshResult{}, fmt.Errorf("authority: lock lineage: %w", errors.Join(err, ErrUnavailable))
	}
	if len(ids) == 0 || op.authority.deviceStatus != "active" {
		return RefreshResult{}, ErrSessionRevoked
	}
	commandID := derivedUUID("baby-tracker-refresh-reuse-v1", parent.ID)
	seq, _, err := appendAudit(ctx, op.tx, op.account.Chain, KindSessionRevoked, commandID, parent.AccountID, op.now,
		map[string]any{"lineage_root_session_id": parent.LineageRootID, "session_id": parent.ID})
	if err != nil {
		return RefreshResult{}, err
	}
	if err := runBatch(ctx, op.tx, []batchStep{
		{`UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='refresh_reused' WHERE lineage_root_id=$1 AND status='active'`,
			[]any{parent.LineageRootID, op.now}, int64(len(ids))},
		{`DELETE FROM refresh_receipts r USING sessions s WHERE r.child_id=s.id AND s.lineage_root_id=$1`, []any{parent.LineageRootID}, -1},
		deadReceiptsStep(parent.AccountID),
	}); err != nil {
		return RefreshResult{}, err
	}
	if err := op.tx.Commit(ctx); err != nil {
		return RefreshResult{}, fmt.Errorf("authority: commit reuse revocation: %w", errors.Join(err, ErrUnavailable))
	}
	revoked := make([]SecurityInvalidation, len(ids))
	for i, id := range ids {
		revoked[i] = SecurityInvalidation{AccountID: parent.AccountID, SessionID: id, Revision: seq}
	}
	op.s.publish(revoked...)
	return RefreshResult{}, code
}

// deadReceiptsStep deletes an account's recovery receipts whose child is no
// longer active (000025 projection after session_revoked,
// device_binding_revoked and identity_unavailable).
func deadReceiptsStep(accountID string) batchStep {
	return batchStep{`DELETE FROM refresh_receipts r USING sessions s WHERE r.child_id=s.id AND r.account_id=$1 AND s.status<>'active'`, []any{accountID}, -1}
}

// derivedUUID is a version-4-shaped UUID from sha256(domain 0x00 value).
func derivedUUID(domain, value string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + value))
	sum[6] = sum[6]&0x0f | 0x40
	sum[8] = sum[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func copyBinding(b *DeviceBinding) *DeviceBinding {
	if b == nil {
		return nil
	}
	out := *b
	return &out
}

func copySlot(slot *FamilySlot) *FamilySlot {
	if slot == nil {
		return nil
	}
	out := *slot
	return &out
}

// batchStep is one statement of a pipelined batch; want is the exact number
// of rows it must affect, or -1 for any.
type batchStep struct {
	sql  string
	args []any
	want int64
}

// runBatch sends steps in one round trip and checks every row count.
func runBatch(ctx context.Context, tx pgx.Tx, steps []batchStep) error {
	batch := &pgx.Batch{}
	for _, step := range steps {
		batch.Queue(step.sql, step.args...)
	}
	results := tx.SendBatch(ctx, batch)
	for i, step := range steps {
		tag, err := results.Exec()
		if err != nil {
			_ = results.Close()
			if isUniqueViolation(err) {
				return fmt.Errorf("authority: batch step %d: %w", i, errors.Join(err, ErrSecurityBusy))
			}
			return fmt.Errorf("authority: batch step %d: %w", i, errors.Join(err, ErrUnavailable))
		}
		if step.want >= 0 && tag.RowsAffected() != step.want {
			_ = results.Close()
			return fmt.Errorf("authority: batch step %d affected %d rows, want %d: %w", i, tag.RowsAffected(), step.want, ErrUnavailable)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("authority: batch: %w", errors.Join(err, ErrUnavailable))
	}
	return nil
}

// ResolveRefreshSubject returns the account and device a refresh token
// belongs to without consuming it, for quota charging (auth/refresh.go:118-145):
// unknown → session_revoked, another epoch → epoch_changed, an inactive
// account or a missing device → session_revoked. It takes no lock and runs
// in one read-only transaction.
func (s *Service) ResolveRefreshSubject(ctx context.Context, candidates []KeyedDigest) (string, string, error) {
	if !validCandidates(candidates) {
		return "", "", ErrSessionRevoked
	}
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return "", "", errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	return s.refreshSubject(ctx, tx, candidates)
}

// RefreshDevice is ResolveRefreshSubject (same refusals) plus the continuity
// binding and status of the subject's device, in one read-only transaction.
// A recoverable refresh recomputes the caller's device proof under the
// binding's key before Refresh, which compares it with the row again under
// its locks.
func (s *Service) RefreshDevice(ctx context.Context, candidates []KeyedDigest) (DeviceBinding, string, error) {
	if !validCandidates(candidates) {
		return DeviceBinding{}, "", ErrSessionRevoked
	}
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return DeviceBinding{}, "", errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	accountID, deviceID, err := s.refreshSubject(ctx, tx, candidates)
	if err != nil {
		return DeviceBinding{}, "", err
	}
	binding, status, err := readDevice(ctx, tx, accountID, deviceID)
	if err != nil {
		return DeviceBinding{}, "", err
	}
	if status == "" {
		return DeviceBinding{}, "", ErrSessionRevoked
	}
	return binding, status, nil
}

// Device reads accountID's device deviceID: its continuity binding and
// status (active or revoked), or status "" when the account has no such
// device. Sign-in and refresh acknowledgement recompute the caller's device
// proof under the binding's key; SignIn and AcknowledgeRefresh compare it
// with the row again in their own transactions.
func (s *Service) Device(ctx context.Context, accountID, deviceID string) (DeviceBinding, string, error) {
	if !validID(accountID) || !validID(deviceID) {
		return DeviceBinding{}, "", nil
	}
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return DeviceBinding{}, "", errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	return readDevice(ctx, tx, accountID, deviceID)
}

// readDevice is Device inside tx.
func readDevice(ctx context.Context, tx pgx.Tx, accountID, deviceID string) (DeviceBinding, string, error) {
	binding := DeviceBinding{DeviceID: deviceID, AccountID: accountID}
	var status string
	var digest []byte
	err := tx.QueryRow(ctx, `SELECT status::text, canonical_origin, continuity_key_id, continuity_digest FROM devices WHERE id=$1 AND account_id=$2`,
		deviceID, accountID).Scan(&status, &binding.CanonicalOrigin, &binding.CredentialKeyID, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceBinding{}, "", nil
	}
	if err != nil {
		return DeviceBinding{}, "", fmt.Errorf("authority: read device: %w", errors.Join(err, ErrUnavailable))
	}
	if len(digest) != 32 {
		return DeviceBinding{}, "", fmt.Errorf("authority: corrupt device digest: %w", ErrUnavailable)
	}
	copy(binding.CredentialDigest[:], digest)
	return binding, status, nil
}

// refreshSubject is ResolveRefreshSubject inside tx.
func (s *Service) refreshSubject(ctx context.Context, tx pgx.Tx, candidates []KeyedDigest) (string, string, error) {
	parent, _, err := s.findParent(ctx, tx, candidates)
	if err != nil {
		return "", "", err
	}
	if parent.RecoveryEpoch != s.epoch {
		return "", "", ErrEpochChanged
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT a.status='active' AND EXISTS (SELECT 1 FROM devices d WHERE d.id=$2 AND d.account_id=a.id)
		FROM accounts a WHERE a.id=$1`, parent.AccountID, parent.DeviceID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
		return "", "", ErrSessionRevoked
	}
	if err != nil {
		return "", "", fmt.Errorf("authority: resolve refresh subject: %w", errors.Join(err, ErrUnavailable))
	}
	return parent.AccountID, parent.DeviceID, nil
}

// principalSession checks p against its session and account
// (prepared.go:130-146 ResolveSessionActor): the account is active and the
// session exists, belongs to p's account and device, and was issued at p's
// epoch and revision. It returns the session and the device's status.
func principalSession(ctx context.Context, tx pgx.Tx, p access.Principal, account accountRow, accountOK bool, lock string) (SessionRow, string, error) {
	if !accountOK || account.Status != "active" {
		return SessionRow{}, "", ErrSessionRevoked
	}
	session, ok, err := sessionByID(ctx, tx, p.SessionID, lock)
	if err != nil {
		return SessionRow{}, "", err
	}
	if !ok || session.AccountID != p.AccountID || session.DeviceID != p.DeviceID || session.RecoveryEpoch != p.RecoveryEpoch ||
		session.IssuedSeq != p.SecurityRevision {
		return SessionRow{}, "", ErrSessionRevoked
	}
	var deviceStatus string
	err = tx.QueryRow(ctx, `SELECT status::text FROM devices WHERE id=$1 AND account_id=$2`, p.DeviceID, p.AccountID).Scan(&deviceStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionRow{}, "", ErrSessionRevoked
	}
	if err != nil {
		return SessionRow{}, "", fmt.Errorf("authority: read device: %w", errors.Join(err, ErrUnavailable))
	}
	return session, deviceStatus, nil
}

func validPrincipal(p access.Principal) bool {
	return validID(p.AccountID) && validID(p.DeviceID) && validID(p.SessionID) && p.SecurityRevision >= 1 && p.SecurityRevision <= maxWireInteger
}

// AcknowledgeRefresh retires the caller's recovery receipt for attemptID
// (auth/refresh_recovery_retention.go:18-84). The caller's session must be
// active and current and the device proof must match the caller's device; a
// receipt must belong to the caller's account, device and lineage. With no
// receipt it returns nil and changes nothing. It takes no row lock (the
// runtime role has no UPDATE on refresh_receipts, which SELECT ... FOR UPDATE
// requires): the DELETE carries the account, device, attempt and lineage
// predicates it validated, so a concurrent rotation that already retired the
// receipt makes it a no-op.
func (s *Service) AcknowledgeRefresh(ctx context.Context, p access.Principal, attemptID string, continuity DeviceBinding) error {
	return s.ProveAndAcknowledgeRefresh(ctx, p, attemptID, proofOf(&continuity, nil))
}

// ProveAndAcknowledgeRefresh is AcknowledgeRefresh with the device proof
// evaluated in the transaction: prove is called with the caller's device row
// binding after the session checks, so the acknowledgement is one
// transaction. A false (or nil) prove is device_continuity_invalid.
func (s *Service) ProveAndAcknowledgeRefresh(ctx context.Context, p access.Principal, attemptID string, prove func(stored DeviceBinding) bool) error {
	if !validID(attemptID) {
		return ErrInvalidRequest
	}
	if p.RecoveryEpoch != s.epoch {
		return ErrEpochChanged
	}
	if !validPrincipal(p) {
		return ErrSessionRevoked
	}
	now := s.at(time.Time{})
	if !now.Before(p.AccessExpiresAt) {
		return ErrAccessExpired
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var accountStatus string
	err = tx.QueryRow(ctx, `SELECT status::text FROM accounts WHERE id=$1`, p.AccountID).Scan(&accountStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("authority: read account: %w", errors.Join(err, ErrUnavailable))
	}
	session, _, err := principalSession(ctx, tx, p, accountRow{ID: p.AccountID, Status: accountStatus}, err == nil, "")
	if err != nil {
		return err
	}
	if session.Status != "active" {
		return ErrSessionRevoked
	}
	// The policy deadline, with the one-year lineage cap applied whatever the
	// mode (AcknowledgeRefresh has no mode). Self-hosted this is exactly
	// Refresh's effective deadline; hosted it equals deadline_at whenever the
	// hosted absolute age is at most a year (default 30 days), and is only
	// stricter otherwise, which merely keeps a receipt until cleanup.
	if !now.Before(effectiveDeadline(session, ModeSelfHosted)) || !now.Before(session.RefreshExpiresAt) {
		return ErrReauthenticationRequired
	}
	if !now.Before(session.AccessExpiresAt) {
		return ErrAccessExpired
	}
	authority, err := readAccountAuthority(ctx, tx, p.AccountID, p.DeviceID)
	if err != nil {
		return err
	}
	if !authority.proves(p.AccountID, p.DeviceID, prove) {
		return ErrDeviceContinuityInvalid
	}
	rows, err := tx.Query(ctx, `SELECT r.child_id::text, coalesce(c.lineage_root_id::text,'') FROM refresh_receipts r LEFT JOIN sessions c ON c.id=r.child_id
		WHERE r.account_id=$1 AND r.device_id=$2 AND r.attempt_id=$3 ORDER BY r.parent_id`, p.AccountID, p.DeviceID, attemptID)
	if err != nil {
		return fmt.Errorf("authority: read refresh receipts: %w", errors.Join(err, ErrUnavailable))
	}
	type receipt struct{ child, lineage string }
	receipts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (receipt, error) {
		var r receipt
		err := row.Scan(&r.child, &r.lineage)
		return r, err
	})
	if err != nil {
		return fmt.Errorf("authority: read refresh receipts: %w", errors.Join(err, ErrUnavailable))
	}
	if len(receipts) == 0 {
		return nil
	}
	for _, r := range receipts {
		if r.lineage != session.LineageRootID {
			return ErrSessionRevoked
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM refresh_receipts r USING sessions c WHERE r.child_id=c.id AND r.account_id=$1 AND r.device_id=$2
			AND r.attempt_id=$3 AND c.lineage_root_id=$4`, p.AccountID, p.DeviceID, attemptID, session.LineageRootID); err != nil {
		return fmt.Errorf("authority: delete refresh receipt: %w", errors.Join(err, ErrUnavailable))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("authority: commit acknowledgement: %w", errors.Join(err, ErrUnavailable))
	}
	return nil
}

// Logout revokes the principal's device binding and every live session on
// that device (device_binding_revoked; auth/refresh.go:260-281,
// prepared.go:270-291, event_handlers.go:430-445). An exact retry returns the
// stored result. Locks: the account, then the device's sessions.
func (s *Service) Logout(ctx context.Context, p access.Principal, commandID string, at time.Time) (Result, error) {
	if !validID(commandID) {
		return Result{}, ErrInvalidRequest
	}
	if p.RecoveryEpoch != s.epoch {
		return Result{}, ErrEpochChanged
	}
	if !validPrincipal(p) || (!at.IsZero() && !validTime(at)) {
		return Result{}, ErrSessionRevoked
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Result{}, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	accounts, err := lockAccounts(ctx, tx, p.AccountID)
	if err != nil {
		return Result{}, err
	}
	hash, err := CanonicalCommandHash(p.AccountID, KindDeviceBindingRevoked, map[string]any{"device_id": p.DeviceID, "session_id": p.SessionID})
	if err != nil {
		return Result{}, err
	}
	if stored, found, err := lookupReceipt(ctx, tx, p.AccountID, commandID, hash); err != nil || found {
		return stored, err
	}
	account, accountOK := accounts[p.AccountID]
	session, deviceStatus, err := principalSession(ctx, tx, p, account, accountOK, "FOR UPDATE")
	if err != nil {
		return Result{}, err
	}
	now := s.at(at)
	if !now.Before(session.DeadlineAt) || deviceStatus != "active" || session.Status != "active" {
		return Result{}, ErrInvalidCommand
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM sessions WHERE device_id=$1 AND status='active' ORDER BY sessions.id FOR UPDATE`, p.DeviceID)
	if err != nil {
		return Result{}, fmt.Errorf("authority: lock device sessions: %w", errors.Join(err, ErrUnavailable))
	}
	revoked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return Result{}, fmt.Errorf("authority: lock device sessions: %w", errors.Join(err, ErrUnavailable))
	}
	seq, _, err := appendAudit(ctx, tx, account.Chain, KindDeviceBindingRevoked, commandID, p.AccountID, now,
		map[string]any{"device_id": p.DeviceID, "session_id": p.SessionID})
	if err != nil {
		return Result{}, err
	}
	result := Result{CommandID: commandID, Outcome: "committed", Code: "ok", Revision: seq}
	if err := runBatch(ctx, tx, []batchStep{
		{`UPDATE devices SET status='revoked', revoked_at=$2, updated_at=$2 WHERE id=$1 AND status='active'`, []any{p.DeviceID, now}, 1},
		{`UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='device_revoked' WHERE device_id=$1 AND status='active'`, []any{p.DeviceID, now}, -1},
		deadReceiptsStep(p.AccountID),
	}); err != nil {
		return Result{}, err
	}
	if err := writeReceipt(ctx, tx, p.AccountID, commandID, hash, KindDeviceBindingRevoked, result, nil, now); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("authority: commit logout: %w", errors.Join(err, ErrUnavailable))
	}
	invalidations := make([]SecurityInvalidation, len(revoked))
	for i, id := range revoked {
		invalidations[i] = SecurityInvalidation{AccountID: p.AccountID, SessionID: id, Revision: seq}
	}
	s.publish(invalidations...)
	return result, nil
}
