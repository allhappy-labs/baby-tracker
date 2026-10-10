package authority

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

// identityKeyPattern is an identity key: hmac-sha256:<key id>:<digest hex>.
var identityKeyPattern = regexp.MustCompile(`^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$`)

// SignIn issues an account-scope session after a verified Apple proof
// (identity_session_issued, or identity_restored for an identity_unavailable
// account). Apple verification and any code exchange happen before it.
// Candidates are the identity's keys, current key first. ProposedAccountID
// and ProposedGeneration are the actor the caller derived from the
// resolution it read before the transaction: the resolved account and its
// generation, or for an absent/deleted identity a new actor at
// NextActorGeneration. The tokens are the caller's; only their digests are
// stored.
type SignIn struct {
	CommandID          string
	At                 time.Time
	Candidates         []string
	Audience, Subject  string
	ProposedAccountID  string
	ProposedGeneration uint64
	DisplayName        *string
	ProfileConfirmed   bool
	Device             DeviceBinding
	RegisterDevice     bool
	SessionID          string
	TokenKeyID         string
	AccessDigest       [32]byte
	RefreshDigest      [32]byte
	AccessExpiresAt    time.Time
	RefreshExpiresAt   time.Time
	DeadlineAt         time.Time
	CanonicalOrigin    string
	// Mode is the deployment mode; self-hosted, DeadlineAt may be at most
	// one year after At (the lineage cap Refresh and AcknowledgeRefresh
	// apply).
	Mode Mode
}

// SignInResult is a sign-in's outcome; an exact retry returns the stored
// result with the same flags and current presentation.
type SignInResult struct {
	Result
	AccountID            string
	Registered, Restored bool
	FamilySlot           *FamilySlot
	Account              AccountView
}

// IdentityUnavailable marks an account's Apple identity unavailable (an
// Apple server notification). CommandID is the notification's reserved
// command ID; NotificationDigest is "" or 64 lowercase hex.
type IdentityUnavailable struct {
	CommandID          string
	AccountID          string
	Candidates         []string
	NotificationDigest string
	At                 time.Time
}

// IdentityResolution is the authority over an identity
// (security.IdentityAuthorityResolution): Status is absent, active,
// identity_unavailable or deleted. ProfileRequired is the resolved live
// (active or identity_unavailable) account's profile_required; false for an
// absent or deleted identity, whose sign-in registers a new account.
type IdentityResolution struct {
	Status                                   string
	AccountID, IdentityKey                   string
	LifecycleGeneration, NextActorGeneration uint64
	ProfileRequired                          bool
}

// querier is the read side of pgx.Tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type identityCandidate struct {
	keyID  string
	digest []byte
}

func parseIdentityCandidates(candidates []string) ([]identityCandidate, error) {
	if len(candidates) == 0 || len(candidates) > 8 {
		return nil, ErrInvalidCommand
	}
	seen := make(map[string]struct{}, len(candidates))
	out := make([]identityCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !identityKeyPattern.MatchString(candidate) {
			return nil, ErrInvalidCommand
		}
		if _, dup := seen[candidate]; dup {
			return nil, ErrInvalidCommand
		}
		seen[candidate] = struct{}{}
		parts := strings.SplitN(candidate, ":", 3)
		digest, _ := hex.DecodeString(parts[2])
		out = append(out, identityCandidate{keyID: parts[1], digest: digest})
	}
	return out, nil
}

// resolveIdentity applies security/state.go:126-186 to the identity rows
// matching any candidate: none is absent (next generation 1); otherwise the
// matches, ordered by lifecycle generation, must be generations 1..n, at most
// one of them live (active or identity_unavailable) and that one the newest,
// else authorization_quarantine. The newest match is the resolution.
func resolveIdentity(ctx context.Context, q querier, candidates []string) (IdentityResolution, error) {
	parsed, err := parseIdentityCandidates(candidates)
	if err != nil {
		return IdentityResolution{}, err
	}
	keyIDs := make([]string, len(parsed))
	digests := make([][]byte, len(parsed))
	for i, candidate := range parsed {
		keyIDs[i], digests[i] = candidate.keyID, candidate.digest
	}
	rows, err := q.Query(ctx, `SELECT i.identity_key_id, i.identity_digest, i.account_id::text, i.lifecycle_generation, i.live, a.status::text,
			a.profile_required
		FROM account_identities i JOIN accounts a ON a.id=i.account_id
		WHERE i.provider='apple' AND (i.identity_key_id, i.identity_digest) IN (SELECT * FROM unnest($1::text[], $2::bytea[]))
		ORDER BY i.lifecycle_generation, i.account_id`, keyIDs, digests)
	if err != nil {
		return IdentityResolution{}, fmt.Errorf("authority: resolve identity: %w", errors.Join(err, ErrUnavailable))
	}
	type match struct {
		key, accountID, status string
		generation             uint64
		live, profileRequired  bool
	}
	matches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (match, error) {
		var m match
		var keyID string
		var digest []byte
		err := row.Scan(&keyID, &digest, &m.accountID, &m.generation, &m.live, &m.status, &m.profileRequired)
		m.key = "hmac-sha256:" + keyID + ":" + hex.EncodeToString(digest)
		return m, err
	})
	if err != nil {
		return IdentityResolution{}, fmt.Errorf("authority: resolve identity: %w", errors.Join(err, ErrUnavailable))
	}
	if len(matches) == 0 {
		return IdentityResolution{Status: "absent", NextActorGeneration: 1}, nil
	}
	live := -1
	for i, m := range matches {
		if m.generation != uint64(i+1) {
			return IdentityResolution{}, ErrAuthorizationQuarantine
		}
		switch m.status {
		case "deleted":
			if m.live {
				return IdentityResolution{}, ErrAuthorizationQuarantine
			}
		case "active", "identity_unavailable":
			if live >= 0 || !m.live {
				return IdentityResolution{}, ErrAuthorizationQuarantine
			}
			live = i
		default:
			return IdentityResolution{}, ErrAuthorizationQuarantine
		}
	}
	if live >= 0 && live != len(matches)-1 {
		return IdentityResolution{}, ErrAuthorizationQuarantine
	}
	selected := matches[len(matches)-1]
	generation := uint64(len(matches))
	if generation >= maxWireInteger {
		return IdentityResolution{}, ErrAuthorizationQuarantine
	}
	return IdentityResolution{Status: selected.status, AccountID: selected.accountID, IdentityKey: selected.key, LifecycleGeneration: generation,
		NextActorGeneration: generation + 1, ProfileRequired: live >= 0 && selected.profileRequired}, nil
}

// ResolveIdentity resolves an identity's candidates (read-only, no lock).
func (s *Service) ResolveIdentity(ctx context.Context, candidates []string) (IdentityResolution, error) {
	if _, err := parseIdentityCandidates(candidates); err != nil {
		return IdentityResolution{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return IdentityResolution{}, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	return resolveIdentity(ctx, tx, candidates)
}

func validDisplayName(name *string) bool {
	return name == nil || (utf8.ValidString(*name) && utf8.RuneCountInString(*name) >= 1 && utf8.RuneCountInString(*name) <= 80 && len(*name) <= 320)
}

func validText(value string) bool {
	return len(value) >= 1 && len(value) <= 255 && utf8.ValidString(value)
}

func (s *Service) validSignIn(in SignIn) bool {
	_, keyOK := s.tokenKey(in.TokenKeyID)
	at := in.At.UTC().Truncate(time.Microsecond)
	if in.Mode != ModeSelfHosted && in.Mode != ModeHosted ||
		(in.Mode == ModeSelfHosted && in.DeadlineAt.UTC().Truncate(time.Microsecond).After(at.Add(selfHostedLineageAge))) {
		return false
	}
	return validID(in.CommandID) && validID(in.ProposedAccountID) && validID(in.SessionID) && in.ProposedGeneration >= 1 && in.ProposedGeneration < maxWireInteger &&
		validText(in.Audience) && validText(in.Subject) && validDisplayName(in.DisplayName) && (in.DisplayName != nil) == in.ProfileConfirmed &&
		validBinding(&in.Device) && in.Device.AccountID == in.ProposedAccountID && in.Device.CanonicalOrigin == in.CanonicalOrigin && keyOK &&
		validTime(in.At) && validTime(in.DeadlineAt) && in.AccessDigest != ([32]byte{}) && in.RefreshDigest != ([32]byte{}) && in.AccessDigest != in.RefreshDigest &&
		!in.AccessExpiresAt.After(in.RefreshExpiresAt) && !in.RefreshExpiresAt.After(in.DeadlineAt) && at.Before(in.AccessExpiresAt.UTC().Truncate(time.Microsecond))
}

func timeText(at time.Time) string {
	return at.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

// signInHash is the command hash of a sign-in: every input except At.
func signInHash(in SignIn) ([32]byte, error) {
	return CanonicalCommandHash(in.ProposedAccountID, KindIdentitySessionIssued, map[string]any{
		"candidates": in.Candidates, "audience": in.Audience, "subject": in.Subject, "generation": in.ProposedGeneration,
		"display_name": in.DisplayName, "profile_confirmed": in.ProfileConfirmed, "device_id": in.Device.DeviceID,
		"device_key_id": in.Device.CredentialKeyID, "device_digest": hex.EncodeToString(in.Device.CredentialDigest[:]), "register_device": in.RegisterDevice,
		"session_id": in.SessionID, "token_key_id": in.TokenKeyID, "access_digest": hex.EncodeToString(in.AccessDigest[:]),
		"refresh_digest": hex.EncodeToString(in.RefreshDigest[:]), "access_expires_at": timeText(in.AccessExpiresAt),
		"refresh_expires_at": timeText(in.RefreshExpiresAt), "deadline_at": timeText(in.DeadlineAt), "origin": in.CanonicalOrigin,
	})
}

// SignIn registers or restores the account and device as needed and inserts
// the account-scope lineage root session (security/prepared.go:109-218).
// Preconditions: the resolution matches the proposed actor; an absent or
// deleted identity registers ProposedAccountID at the next generation under
// Candidates[0]; an active identity keeps its key; an identity_unavailable
// one is restored; a registered device must be new; an existing device must
// be active and bound to the account, origin and continuity digest
// (device_continuity_invalid); the session ID must be new. Any other failure
// is invalid_command. Locks: the account
// row (a registration's concurrent twin loses on the primary key, as
// security_busy).
func (s *Service) SignIn(ctx context.Context, in SignIn) (SignInResult, error) {
	if !s.validSignIn(in) {
		return SignInResult{}, ErrInvalidCommand
	}
	if _, err := parseIdentityCandidates(in.Candidates); err != nil {
		return SignInResult{}, err
	}
	hash, err := signInHash(in)
	if err != nil {
		return SignInResult{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return SignInResult{}, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	accounts, err := lockAccounts(ctx, tx, in.ProposedAccountID)
	if err != nil {
		return SignInResult{}, err
	}
	if stored, found, err := lookupReceipt(ctx, tx, in.ProposedAccountID, in.CommandID, hash); err != nil {
		return SignInResult{}, err
	} else if found {
		return signInReplay(ctx, tx, in.ProposedAccountID, stored)
	}
	resolution, err := resolveIdentity(ctx, tx, in.Candidates)
	if err != nil {
		return SignInResult{}, err
	}
	account, exists := accounts[in.ProposedAccountID]
	register, restore := false, false
	identityKey := resolution.IdentityKey
	switch resolution.Status {
	case "absent", "deleted":
		if in.ProposedGeneration != resolution.NextActorGeneration || exists {
			return SignInResult{}, ErrInvalidCommand
		}
		register, identityKey = true, in.Candidates[0]
	case "active", "identity_unavailable":
		if in.ProposedAccountID != resolution.AccountID || in.ProposedGeneration != resolution.LifecycleGeneration {
			return SignInResult{}, ErrInvalidCommand
		}
		if !exists {
			// Registered by a transaction that committed after our lock
			// attempt found no row: lock it now, and if that was this very
			// command, return its result.
			if accounts, err = lockAccounts(ctx, tx, in.ProposedAccountID); err != nil {
				return SignInResult{}, err
			}
			if stored, found, err := lookupReceipt(ctx, tx, in.ProposedAccountID, in.CommandID, hash); err != nil {
				return SignInResult{}, err
			} else if found {
				return signInReplay(ctx, tx, in.ProposedAccountID, stored)
			}
			if resolution, err = resolveIdentity(ctx, tx, in.Candidates); err != nil {
				return SignInResult{}, err
			}
			account, exists = accounts[in.ProposedAccountID]
		}
		if !exists || account.Status != resolution.Status || resolution.AccountID != in.ProposedAccountID {
			return SignInResult{}, fmt.Errorf("authority: resolved account %s is not locked as %s: %w", resolution.AccountID, resolution.Status, ErrUnavailable)
		}
		restore = resolution.Status == "identity_unavailable"
	default:
		return SignInResult{}, ErrInvalidCommand
	}
	now := in.At.UTC().Truncate(time.Microsecond)
	parts := strings.SplitN(identityKey, ":", 3)
	identityDigest, _ := hex.DecodeString(parts[2])
	head := account.Chain
	if register {
		// Claim the new actor first: its primary key serializes a concurrent
		// twin registration, which waits here and then fails as
		// security_busy (its retry returns this command's result), so every
		// check below runs after the twin is decided.
		name, profileRequired := "", true
		if in.DisplayName != nil {
			name, profileRequired = *in.DisplayName, false
		}
		if err := runBatch(ctx, tx, []batchStep{
			{`INSERT INTO accounts(id,display_name,profile_required,status,lifecycle_generation,created_at,updated_at) VALUES ($1,$2,$3,'active',$4,$5,$5)`,
				[]any{in.ProposedAccountID, name, profileRequired, int64(in.ProposedGeneration), now}, 1},
			{`INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,lifecycle_generation,audience,subject,live,created_at)
				VALUES ('apple',$1,$2,$3,$4,$5,$6,true,$7)`,
				[]any{parts[1], identityDigest, in.ProposedAccountID, int64(in.ProposedGeneration), in.Audience, in.Subject, now}, 1},
		}); err != nil {
			return SignInResult{}, err
		}
		head = &chainHead{Scope: accountScope(in.ProposedAccountID)}
	}
	if err := checkSignInDevice(ctx, tx, in); err != nil {
		return SignInResult{}, err
	}
	if _, taken, err := sessionByID(ctx, tx, in.SessionID, ""); err != nil {
		return SignInResult{}, err
	} else if taken {
		return SignInResult{}, ErrInvalidCommand
	}
	var steps []batchStep
	if !register {
		if restore {
			steps = append(steps, batchStep{`UPDATE accounts SET status='active', updated_at=$2 WHERE id=$1 AND status='identity_unavailable'`,
				[]any{in.ProposedAccountID, now}, 1})
		}
		if in.DisplayName != nil {
			steps = append(steps, batchStep{`UPDATE accounts SET display_name=$2, profile_required=false, updated_at=$3 WHERE id=$1 AND profile_required`,
				[]any{in.ProposedAccountID, *in.DisplayName, now}, -1})
		}
	}
	if in.RegisterDevice {
		steps = append(steps, batchStep{`INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$6)`, []any{in.Device.DeviceID, in.ProposedAccountID, in.CanonicalOrigin, in.Device.CredentialKeyID,
			in.Device.CredentialDigest[:], now}, 1})
	} else {
		// A fresh proof on an existing device replaces that device's
		// unresolved recovery installs (000025 identity_session_issued).
		steps = append(steps, batchStep{`DELETE FROM refresh_receipts WHERE account_id=$1 AND device_id=$2`, []any{in.ProposedAccountID, in.Device.DeviceID}, -1})
	}
	if err := runBatch(ctx, tx, steps); err != nil {
		return SignInResult{}, err
	}
	kind := KindIdentitySessionIssued
	if restore {
		kind = KindIdentityRestored
	}
	seq, _, err := appendAudit(ctx, tx, head, kind, in.CommandID, in.ProposedAccountID, now, map[string]any{
		"register_account": register, "register_device": in.RegisterDevice, "device_id": in.Device.DeviceID, "session_id": in.SessionID,
		"identity_key_id": parts[1], "identity_digest": parts[2], "origin": in.CanonicalOrigin})
	if err != nil {
		return SignInResult{}, err
	}
	session := SessionRow{ID: in.SessionID, AccountID: in.ProposedAccountID, DeviceID: in.Device.DeviceID, LineageRootID: in.SessionID, Scope: "account",
		RecoveryEpoch: s.epoch, IssuedSeq: seq, TokenKeyID: in.TokenKeyID, AccessDigest: in.AccessDigest, RefreshDigest: in.RefreshDigest,
		AccessExpiresAt: in.AccessExpiresAt.UTC().Truncate(time.Microsecond), RefreshExpiresAt: in.RefreshExpiresAt.UTC().Truncate(time.Microsecond),
		DeadlineAt: in.DeadlineAt.UTC().Truncate(time.Microsecond), LineageStartedAt: now, CreatedAt: now}
	result := Result{CommandID: in.CommandID, Outcome: "committed", Code: "ok", Revision: seq}
	if err := runBatch(ctx, tx, []batchStep{{insertSessionSQL, insertSessionArgs(session), 1}}); err != nil {
		return SignInResult{}, err
	}
	if err := writeReceipt(ctx, tx, in.ProposedAccountID, in.CommandID, hash, kind, result, nil, now); err != nil {
		return SignInResult{}, err
	}
	out, err := signInPresentation(ctx, tx, in.ProposedAccountID, result, register, restore)
	if err != nil {
		return SignInResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return SignInResult{}, errors.Join(err, ErrSecurityBusy)
		}
		return SignInResult{}, fmt.Errorf("authority: commit sign-in: %w", errors.Join(err, ErrUnavailable))
	}
	return out, nil
}

// checkSignInDevice is prepared.go:205-218.
func checkSignInDevice(ctx context.Context, tx pgx.Tx, in SignIn) error {
	var accountID, status, origin, keyID string
	var digest []byte
	err := tx.QueryRow(ctx, `SELECT account_id::text, status::text, canonical_origin, continuity_key_id, continuity_digest FROM devices WHERE id=$1`,
		in.Device.DeviceID).Scan(&accountID, &status, &origin, &keyID, &digest)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("authority: read device: %w", errors.Join(err, ErrUnavailable))
	}
	if in.RegisterDevice {
		if exists {
			return ErrInvalidCommand
		}
		return nil
	}
	// An existing device that is missing, revoked or rebound since the
	// caller proved it is device_continuity_invalid (legacy sign-in code).
	if !exists || status != "active" || accountID != in.ProposedAccountID || origin != in.CanonicalOrigin || keyID != in.Device.CredentialKeyID ||
		len(digest) != 32 || [32]byte(digest) != in.Device.CredentialDigest {
		return ErrDeviceContinuityInvalid
	}
	return nil
}

// signInPresentation reads the account view and family slot.
func signInPresentation(ctx context.Context, tx pgx.Tx, accountID string, result Result, registered, restored bool) (SignInResult, error) {
	out := SignInResult{Result: result, AccountID: accountID, Registered: registered, Restored: restored, Account: AccountView{ID: accountID}}
	var slotFamily, slotState *string
	err := tx.QueryRow(ctx, `SELECT a.display_name, a.profile_required, s.family_id::text, s.state::text
		FROM accounts a LEFT JOIN family_slots s ON s.account_id=a.id WHERE a.id=$1`, accountID).
		Scan(&out.Account.DisplayName, &out.Account.ProfileRequired, &slotFamily, &slotState)
	if err != nil {
		return SignInResult{}, fmt.Errorf("authority: read sign-in presentation: %w", errors.Join(err, ErrUnavailable))
	}
	if slotFamily != nil && slotState != nil {
		out.FamilySlot = &FamilySlot{FamilyID: *slotFamily, State: *slotState}
	}
	return out, nil
}

// signInReplay rebuilds an exact retry's result: the stored result, the
// flags from the audit row it committed, and current presentation.
func signInReplay(ctx context.Context, tx pgx.Tx, accountID string, stored Result) (SignInResult, error) {
	var kind string
	var registered bool
	err := tx.QueryRow(ctx, `SELECT kind::text, coalesce((payload->>'register_account')::boolean, false) FROM security_audit_events
		WHERE scope='account' AND scope_id=$1 AND seq=$2 AND command_id=$3`, accountID, int64(stored.Revision), stored.CommandID).Scan(&kind, &registered)
	if err != nil {
		return SignInResult{}, fmt.Errorf("authority: read sign-in audit row: %w", errors.Join(err, ErrUnavailable))
	}
	return signInPresentation(ctx, tx, accountID, stored, registered, kind == string(KindIdentityRestored))
}

// MarkIdentityUnavailable marks the account identity_unavailable and revokes
// every live session of the account (prepared.go:293-313,
// apple_notifications.go:401-418, event_handlers.go:396-412). An exact retry
// returns the stored result before any other check. The resolution must be
// the account's (else service_unavailable, as the legacy notification path)
// and active (else invalid_command). Locks: the account, then its sessions.
func (s *Service) MarkIdentityUnavailable(ctx context.Context, in IdentityUnavailable) (Result, error) {
	if !validID(in.CommandID) || !validID(in.AccountID) || (in.NotificationDigest != "" && !hexDigest.MatchString(in.NotificationDigest)) ||
		(!in.At.IsZero() && !validTime(in.At)) {
		return Result{}, ErrInvalidCommand
	}
	if _, err := parseIdentityCandidates(in.Candidates); err != nil {
		return Result{}, err
	}
	hash, err := identityUnavailableHash(in.AccountID, in.NotificationDigest)
	if err != nil {
		return Result{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Result{}, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	accounts, err := lockAccounts(ctx, tx, in.AccountID)
	if err != nil {
		return Result{}, err
	}
	if stored, found, err := lookupReceipt(ctx, tx, in.AccountID, in.CommandID, hash); err != nil || found {
		return stored, err
	}
	resolution, err := resolveIdentity(ctx, tx, in.Candidates)
	if err != nil || resolution.AccountID != in.AccountID || resolution.Status == "absent" || resolution.Status == "deleted" {
		// Any resolution failure, quarantine included, is service_unavailable
		// (apple_notifications.go:404-405): err is formatted, not wrapped.
		return Result{}, fmt.Errorf("authority: identity does not resolve to account %s (%v): %w", in.AccountID, err, ErrUnavailable)
	}
	account, ok := accounts[in.AccountID]
	if resolution.Status != "active" || !ok || account.Status != "active" {
		return Result{}, ErrInvalidCommand
	}
	now := s.at(in.At)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE account_id=$1 AND status='active' ORDER BY id FOR UPDATE`, in.AccountID); err != nil {
		return Result{}, fmt.Errorf("authority: lock account sessions: %w", errors.Join(err, ErrUnavailable))
	}
	seq, _, err := appendAudit(ctx, tx, account.Chain, KindIdentityUnavailable, in.CommandID, in.AccountID, now,
		map[string]any{"notification_digest": in.NotificationDigest})
	if err != nil {
		return Result{}, err
	}
	result := Result{CommandID: in.CommandID, Outcome: "committed", Code: "ok", Revision: seq}
	if err := runBatch(ctx, tx, []batchStep{
		{`UPDATE accounts SET status='identity_unavailable', updated_at=$2 WHERE id=$1 AND status='active'`, []any{in.AccountID, now}, 1},
		{`UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='identity_unavailable' WHERE account_id=$1 AND status='active'`, []any{in.AccountID, now}, -1},
		deadReceiptsStep(in.AccountID),
	}); err != nil {
		return Result{}, err
	}
	if err := writeReceipt(ctx, tx, in.AccountID, in.CommandID, hash, KindIdentityUnavailable, result, nil, now); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("authority: commit identity unavailable: %w", errors.Join(err, ErrUnavailable))
	}
	s.publish(SecurityInvalidation{AccountID: in.AccountID, Revision: seq})
	return result, nil
}

// identityUnavailableHash is MarkIdentityUnavailable's command hash.
func identityUnavailableHash(accountID, notificationDigest string) ([32]byte, error) {
	return CanonicalCommandHash(accountID, KindIdentityUnavailable, map[string]any{"account_id": accountID, "notification_digest": notificationDigest})
}

// NotificationReceipt reports whether one of commandIDs already marked
// accountID's identity unavailable for the notification whose semantic
// digest is notificationDigest: MarkIdentityUnavailable's exact retry,
// checked without writing or locking (apple_notifications.go
// checkNotificationReceipts). A receipt under one of the IDs for other
// content is command_id_reused. Apple notification handling checks it
// before reserving the notification, so a redelivery after a notification
// key rotation finds the receipt its first delivery committed.
func (s *Service) NotificationReceipt(ctx context.Context, accountID string, commandIDs []string, notificationDigest string) (bool, error) {
	if !validID(accountID) || len(commandIDs) == 0 || len(commandIDs) > 8 || (notificationDigest != "" && !hexDigest.MatchString(notificationDigest)) {
		return false, ErrInvalidCommand
	}
	for _, commandID := range commandIDs {
		if !validID(commandID) {
			return false, ErrInvalidCommand
		}
	}
	hash, err := identityUnavailableHash(accountID, notificationDigest)
	if err != nil {
		return false, err
	}
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return false, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, commandID := range commandIDs {
		_, found, err := lookupReceipt(ctx, tx, accountID, commandID, hash)
		if err != nil {
			if errors.Is(err, ErrCommandIDReused) {
				return false, err
			}
			return false, fmt.Errorf("authority: notification receipt: %w", errors.Join(err, ErrUnavailable))
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// ReserveAppleNotification reserves a notification's command ID
// (app.reserve_apple_notification, 000005:677-716): a notification already
// reserved under any candidate key returns its stored command ID; otherwise
// the current-key digest (candidates[0]) is inserted with commandID. A
// concurrent reservation of the same notification waits on the primary key
// and returns the winner's command ID. Every failure is service_unavailable.
func (s *Service) ReserveAppleNotification(ctx context.Context, candidates []identity.KeyedDigest, commandID string, at time.Time) (string, bool, error) {
	if len(candidates) == 0 || len(candidates) > 8 || !validID(commandID) || !validTime(at) {
		return "", false, ErrUnavailable
	}
	keyIDs := make([]string, 0, len(candidates))
	digests := make([][]byte, 0, len(candidates))
	seenIDs := map[string]struct{}{}
	seen := map[identity.KeyedDigest]struct{}{}
	for _, candidate := range candidates {
		if !keyIDPattern.MatchString(candidate.KeyID) {
			return "", false, ErrUnavailable
		}
		if _, dup := seenIDs[candidate.KeyID]; dup {
			return "", false, ErrUnavailable
		}
		if _, dup := seen[candidate]; dup {
			return "", false, ErrUnavailable
		}
		seenIDs[candidate.KeyID], seen[candidate] = struct{}{}, struct{}{}
		keyIDs = append(keyIDs, candidate.KeyID)
		digests = append(digests, append([]byte(nil), candidate.Sum[:]...))
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return "", false, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	lookup := func() ([]string, error) {
		rows, err := tx.Query(ctx, `SELECT DISTINCT command_id::text FROM apple_notifications
			WHERE (notification_key_id, notification_digest) IN (SELECT * FROM unnest($1::text[], $2::bytea[]))`, keyIDs, digests)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[string])
	}
	stored, err := lookup()
	if err != nil {
		return "", false, fmt.Errorf("authority: lookup notification: %w", errors.Join(err, ErrUnavailable))
	}
	if len(stored) > 1 {
		return "", false, fmt.Errorf("authority: ambiguous notification reservation: %w", ErrUnavailable)
	}
	if len(stored) == 1 {
		return stored[0], false, nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO apple_notifications(notification_key_id,notification_digest,command_id,received_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT (notification_key_id, notification_digest) DO NOTHING`, keyIDs[0], digests[0], commandID, at.UTC().Truncate(time.Microsecond))
	if err != nil {
		return "", false, fmt.Errorf("authority: reserve notification: %w", errors.Join(err, ErrUnavailable))
	}
	newly := tag.RowsAffected() == 1
	if !newly {
		// The conflicting reservation committed first; read-committed sees it.
		if stored, err = lookup(); err != nil || len(stored) != 1 {
			return "", false, fmt.Errorf("authority: reread notification: %w", errors.Join(err, ErrUnavailable))
		}
		commandID = stored[0]
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("authority: commit notification: %w", errors.Join(err, ErrUnavailable))
	}
	return commandID, newly, nil
}
