package authority

import (
	"context"
	"fmt"
	"time"

	"babytracker/server/internal/access"
	"github.com/jackc/pgx/v5"
)

// maxAccessDigests bounds the candidate digests of one access token: one per
// token key (current plus at most seven previous, validKeyring).
const maxAccessDigests = 8

// requestAuthoritySQL is the request path's one authority statement
// (app.request_authority in 000029): the session found by access digests or
// by session ID, its account, the account's slot and membership in the
// slot's family and, for a family request, its family and entitlement. It also installs the
// transaction-local tenant context for a live session.
const requestAuthoritySQL = `SELECT matches, access_digest, token_key_id, session_id::text, account_id::text, device_id::text, scope,
	coalesce(session_family_id::text,''), coalesce(session_role,''), session_status, recovery_epoch, issued_seq, access_expires_at, deadline_at,
	lineage_started_at, account_status, account_chain_failed, coalesce(slot_family_id::text,''), coalesce(slot_state,''), coalesce(member_role,''),
	coalesce(member_status,''), coalesce(family_id::text,''), coalesce(family_status,''), coalesce(family_audit_seq,0), coalesce(family_chain_failed,false),
	coalesce(family_entitlement_required,false), coalesce(entitlement_status,''), entitlement_expires_at
	FROM app.request_authority($1,$2,$3,$4)`

// requestRow is one app.request_authority row. The family fields are zero
// for an account request and when the session has no family (an account
// scope session without a slot).
type requestRow struct {
	matches                                     int
	accessDigest                                []byte
	tokenKeyID                                  string
	sessionID, accountID, deviceID              string
	scope, sessionFamilyID, sessionRole, status string
	recoveryEpoch, issuedSeq                    uint64
	accessExpiresAt, deadlineAt, lineageStarted time.Time
	accountStatus                               string
	accountChainFailed                          bool
	slotFamilyID, slotState                     string
	memberRole, memberStatus                    string
	familyID, familyStatus                      string
	familyAuditSeq                              uint64
	familyChainFailed, entitlementRequired      bool
	entitlementStatus                           string
	entitlementExpiresAt                        *time.Time
}

func scanRequest(row pgx.Row) (requestRow, error) {
	var r requestRow
	var tokenKeyID, sessionID, accountID, deviceID, scope, status, accountStatus *string
	var recoveryEpoch, issuedSeq *int64
	var accessExpires, deadline, lineage *time.Time
	var accountFailed *bool
	if err := row.Scan(&r.matches, &r.accessDigest, &tokenKeyID, &sessionID, &accountID, &deviceID, &scope, &r.sessionFamilyID, &r.sessionRole,
		&status, &recoveryEpoch, &issuedSeq, &accessExpires, &deadline, &lineage, &accountStatus, &accountFailed, &r.slotFamilyID, &r.slotState,
		&r.memberRole, &r.memberStatus, &r.familyID, &r.familyStatus, &r.familyAuditSeq, &r.familyChainFailed, &r.entitlementRequired,
		&r.entitlementStatus, &r.entitlementExpiresAt); err != nil {
		return requestRow{}, err
	}
	if r.matches != 1 {
		return r, nil
	}
	if tokenKeyID == nil || sessionID == nil || accountID == nil || deviceID == nil || scope == nil || status == nil || accountStatus == nil ||
		recoveryEpoch == nil || issuedSeq == nil || accessExpires == nil || deadline == nil || lineage == nil || accountFailed == nil ||
		len(r.accessDigest) != 32 {
		return requestRow{}, fmt.Errorf("authority: incomplete request authority row: %w", ErrUnavailable)
	}
	r.tokenKeyID, r.sessionID, r.accountID, r.deviceID, r.scope, r.status, r.accountStatus = *tokenKeyID, *sessionID, *accountID, *deviceID, *scope,
		*status, *accountStatus
	r.recoveryEpoch, r.issuedSeq = uint64(*recoveryEpoch), uint64(*issuedSeq)
	r.accessExpiresAt, r.deadlineAt, r.lineageStarted = accessExpires.UTC(), deadline.UTC(), lineage.UTC()
	r.accountChainFailed = *accountFailed
	if r.entitlementExpiresAt != nil {
		at := r.entitlementExpiresAt.UTC()
		r.entitlementExpiresAt = &at
	}
	return r, nil
}

// digestArgs validates an access token's candidate digests (one per token
// key, 1..8 of them, no key twice) and returns the digests for the lookup.
func digestArgs(digests []KeyedDigest) ([][]byte, bool) {
	if len(digests) == 0 || len(digests) > maxAccessDigests {
		return nil, false
	}
	seen := make(map[string]struct{}, len(digests))
	out := make([][]byte, len(digests))
	for i, d := range digests {
		if !keyIDPattern.MatchString(d.KeyID) || d.Sum == ([32]byte{}) {
			return nil, false
		}
		if _, dup := seen[d.KeyID]; dup {
			return nil, false
		}
		seen[d.KeyID] = struct{}{}
		out[i] = append([]byte(nil), d.Sum[:]...)
	}
	return out, true
}

// matchedDigest reports whether the row's (token key, access digest) pair is
// one of the candidates: a digest found under a different key's name does
// not authenticate.
func (r requestRow) matchedDigest(digests []KeyedDigest) bool {
	for _, d := range digests {
		if d.KeyID == r.tokenKeyID && string(d.Sum[:]) == string(r.accessDigest) {
			return true
		}
	}
	return false
}

// Authenticate resolves an access token, given as its digests under every
// configured token key, to its principal with one pool query outside any
// transaction (app.request_authority by digest). Refusals, in order: no
// session (session_revoked); more than one (service_unavailable); a digest
// matched under another key's name (session_revoked); the account's audit
// chain failed (scope_failed, 503); another recovery epoch (epoch_changed);
// the session or its account not active (session_revoked); the
// reauthentication deadline passed (reauthentication_required); the access
// token expired (access_expired). The deadline is deadline_at capped at one
// year from the lineage's sign-in whatever the mode, as AcknowledgeRefresh
// applies it (sign-in already caps self-hosted deadlines; hosted deadlines
// are at most the hosted absolute age). Principal.Scope is derived from the
// account's slot as legacy derived it (requestRow.principal); SessionScope is
// the session row's. The slot and membership come from the same query.
func (s *Service) Authenticate(ctx context.Context, accessDigests []KeyedDigest) (access.Principal, error) {
	args, ok := digestArgs(accessDigests)
	if !ok {
		return access.Principal{}, ErrSessionRevoked
	}
	row, err := scanRequest(s.pool.QueryRow(ctx, requestAuthoritySQL, args, nil, false, int64(s.epoch)))
	if err != nil {
		return access.Principal{}, unavailable("authenticate", err)
	}
	if err := s.checkAuthenticated(row, accessDigests, s.at(time.Time{})); err != nil {
		return access.Principal{}, err
	}
	return row.principal(), nil
}

// checkAuthenticated is Authenticate's refusal order over a digest lookup.
func (s *Service) checkAuthenticated(row requestRow, digests []KeyedDigest, now time.Time) error {
	switch {
	case row.matches == 0:
		return ErrSessionRevoked
	case row.matches != 1:
		return fmt.Errorf("authority: access digests match %d sessions: %w", row.matches, ErrUnavailable)
	case !row.matchedDigest(digests):
		return ErrSessionRevoked
	}
	if err := s.checkLiveSession(row, now); err != nil {
		return err
	}
	if !now.Before(row.accessExpiresAt) {
		return ErrAccessExpired
	}
	return nil
}

// checkLiveSession is the session check every request transaction repeats:
// scope_failed, epoch_changed, session_revoked (session or account not
// active, or a malformed scope), reauthentication_required.
func (s *Service) checkLiveSession(row requestRow, now time.Time) error {
	if row.accountChainFailed {
		return ErrScopeFailed
	}
	if row.recoveryEpoch != s.epoch {
		return ErrEpochChanged
	}
	if row.status != "active" || row.accountStatus != "active" || !validSessionShape(row.scope, row.sessionFamilyID, row.sessionRole) {
		return ErrSessionRevoked
	}
	if !now.Before(minTime(row.deadlineAt, row.lineageStarted.Add(selfHostedLineageAge))) {
		return ErrReauthenticationRequired
	}
	return nil
}

// validSessionShape is the sessions scope CHECK, restated so a row the
// schema would refuse never authenticates.
func validSessionShape(scope, familyID, role string) bool {
	switch scope {
	case "account":
		return familyID == "" && role == ""
	case "family", "family_recovery":
		return validID(familyID) && (role == "owner" || role == "caregiver" || role == "viewer")
	default:
		return false
	}
}

// principal is the authenticated principal of a live row. SessionScope and
// SessionFamilyID are the session row's; Scope is derived from the account's
// slot as legacy Authenticate derived it (auth/refresh.go
// authorityFamilySlot and the fresh-proof rule, at 071dc4d): an active slot
// with an active membership in its family is "family" (a session signed in
// before the account created or joined its family is family scoped at once,
// as clients expect); a deletion-recovery slot is "family_recovery" only for
// a row refresh already narrowed to family_recovery, "account" for any other
// row; no slot (or an active slot without an active membership) is
// "account". Policy never reads Scope: familyGrant evaluates the row.
func (r requestRow) principal() access.Principal {
	return access.Principal{AccountID: r.accountID, DeviceID: r.deviceID, SessionID: r.sessionID, RecoveryEpoch: r.recoveryEpoch,
		SecurityRevision: r.issuedSeq, Scope: r.derivedScope(), SessionScope: r.scope, SessionFamilyID: r.sessionFamilyID, AccessExpiresAt: r.accessExpiresAt}
}

// derivedScope is principal's Scope rule.
func (r requestRow) derivedScope() string {
	switch {
	case r.slotState == "active" && r.memberStatus == "active":
		return "family"
	case r.slotState == "family_recovery" && r.scope == "family_recovery":
		return "family_recovery"
	default:
		return "account"
	}
}

// matchesPrincipal reports whether a session-ID lookup found p's session as
// it was authenticated (security/transaction.go validateSessionPrincipal):
// the same session, account, device, epoch and issuance revision. The scope,
// family and role are the row's current ones, not the principal's: family
// recovery and forfeiture move a live session's scope in place, as legacy
// derived it from the live slot.
func (r requestRow) matchesPrincipal(p access.Principal) bool {
	return r.matches == 1 && r.sessionID == p.SessionID && r.accountID == p.AccountID && r.deviceID == p.DeviceID &&
		r.recoveryEpoch == p.RecoveryEpoch && r.issuedSeq == p.SecurityRevision
}
