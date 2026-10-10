// Package authority is the only security authority: every security write is
// one PostgreSQL transaction (lock accounts then families in ID order, look
// up the command receipt, validate, write state + per-scope audit rows +
// receipt, commit), and request authentication reads the session row.
package authority

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"regexp"
	"time"

	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxWireInteger is the largest integer clients can represent exactly.
const maxWireInteger = 9007199254740991

// Options configures a Service.
type Options struct {
	// Pool is the runtime pool; every authority transaction runs on it.
	Pool *pgxpool.Pool
	Now  func() time.Time
	// InstanceID and RecoveryEpoch are the node identity server_state holds.
	InstanceID    string
	RecoveryEpoch uint64
	// TokenKeys digest access and refresh tokens (current first).
	TokenKeys identity.DigestKeyring
	// MaxCaregivers and MaxInvitations default to 10 when zero.
	MaxCaregivers, MaxInvitations int
	RequireFamilySyncEntitlement  bool
	// Allowed reports whether a membership role may perform an action.
	Allowed func(role, action string) bool
	// EntitlementExempt reports whether an action skips the family_sync
	// entitlement check (legacy family entitlementExempt: read_receipt,
	// export, export_family, delete_family, recover_family,
	// forfeit_recovery). Nil means LegacyEntitlementExempt.
	EntitlementExempt func(action string) bool
}

// LegacyEntitlementExempt is the legacy list of actions that skip the
// family_sync entitlement check (family/membership.go entitlementExempt):
// read_receipt, export, export_family, delete_family, recover_family and
// forfeit_recovery. Server composition passes it as
// Options.EntitlementExempt.
func LegacyEntitlementExempt(action string) bool {
	switch action {
	case "read_receipt", "export", "export_family", "delete_family", "recover_family", "forfeit_recovery":
		return true
	default:
		return false
	}
}

// Service owns authority transactions.
type Service struct {
	pool                         *pgxpool.Pool
	now                          func() time.Time
	instanceID                   string
	epoch                        uint64
	tokenKeys                    identity.DigestKeyring
	maxCaregivers                int
	maxInvitations               int
	requireFamilySyncEntitlement bool
	allowed                      func(role, action string) bool
	entitlementExempt            func(action string) bool
	// invalidations are the post-commit security invalidation subscribers.
	invalidations invalidations
	// afterPruneBatch, when set (tests only, export_test.go), runs after a
	// prune's statements and before its COMMIT.
	afterPruneBatch func()
}

// New validates o and returns a Service. It does not touch the database.
func New(o Options) (*Service, error) {
	if o.Pool == nil || o.Now == nil || o.Allowed == nil {
		return nil, errors.New("authority: pool, clock and role policy are required")
	}
	if !validID(o.InstanceID) {
		return nil, errors.New("authority: instance ID must be a canonical non-nil UUID")
	}
	if o.RecoveryEpoch < 1 || o.RecoveryEpoch > maxWireInteger {
		return nil, errors.New("authority: recovery epoch out of range")
	}
	if !validKeyring(o.TokenKeys) {
		return nil, errors.New("authority: invalid token keyring")
	}
	maxCaregivers, maxInvitations := o.MaxCaregivers, o.MaxInvitations
	if maxCaregivers == 0 {
		maxCaregivers = 10
	}
	if maxInvitations == 0 {
		maxInvitations = 10
	}
	if maxCaregivers < 1 || maxInvitations < 1 {
		return nil, errors.New("authority: family limits must be positive")
	}
	exempt := o.EntitlementExempt
	if exempt == nil {
		exempt = LegacyEntitlementExempt
	}
	return &Service{pool: o.Pool, now: o.Now, instanceID: o.InstanceID, epoch: o.RecoveryEpoch, tokenKeys: o.TokenKeys,
		maxCaregivers: maxCaregivers, maxInvitations: maxInvitations, requireFamilySyncEntitlement: o.RequireFamilySyncEntitlement,
		allowed: o.Allowed, entitlementExempt: exempt}, nil
}

// TokenKeyCommitment commits to a token keyring without revealing it: per
// key, current first, the key ID and HMAC-SHA256(secret,
// "baby-tracker-token-keyring-commitment-v1"). Callers that digest tokens
// themselves (auth) compare theirs with Service.TokenKeyCommitment.
func TokenKeyCommitment(keys identity.DigestKeyring) []KeyedDigest {
	out := make([]KeyedDigest, 0, 1+len(keys.Previous))
	for _, key := range append([]identity.DigestKey{keys.Current}, keys.Previous...) {
		mac := hmac.New(sha256.New, key.Secret[:])
		mac.Write([]byte("baby-tracker-token-keyring-commitment-v1"))
		digest := KeyedDigest{KeyID: key.ID}
		copy(digest.Sum[:], mac.Sum(nil))
		out = append(out, digest)
	}
	return out
}

// TokenKeyCommitment is the commitment to the service's token keyring.
func (s *Service) TokenKeyCommitment() []KeyedDigest { return TokenKeyCommitment(s.tokenKeys) }

// InstanceID is the node's instance ID (server_state.instance_id).
func (s *Service) InstanceID() string { return s.instanceID }

// RecoveryEpoch is the node's recovery epoch: every session it issues
// carries it, and a session of another epoch is refused (epoch_changed).
func (s *Service) RecoveryEpoch() uint64 { return s.epoch }

// Result is a command's outcome, stored in its receipt and returned verbatim
// on an exact retry. Revision is the audit seq of the primary scope row the
// command wrote.
type Result struct {
	CommandID string `json:"command_id"`
	Outcome   string `json:"outcome"`
	Code      string `json:"code"`
	Revision  uint64 `json:"revision"`
}

// Scope is an audit chain's owner kind (app.audit_scope).
type Scope string

const (
	ScopeAccount Scope = "account"
	ScopeFamily  Scope = "family"
)

// scopeRef names one audit chain: the account or family row holding its head.
type scopeRef struct {
	kind Scope
	id   string
}

func accountScope(id string) scopeRef { return scopeRef{kind: ScopeAccount, id: id} }
func familyScope(id string) scopeRef  { return scopeRef{kind: ScopeFamily, id: id} }

// Kind is a security change kind (app.audit_kind); it names audit rows and
// receipts.
type Kind string

const (
	KindAccountRegistered            Kind = "account_registered"
	KindIdentityUnavailable          Kind = "identity_unavailable"
	KindIdentityRestored             Kind = "identity_restored"
	KindDeviceRegistered             Kind = "device_registered"
	KindDeviceBindingRevoked         Kind = "device_binding_revoked"
	KindSessionGranted               Kind = "session_granted"
	KindSessionRotated               Kind = "session_rotated"
	KindSessionRevoked               Kind = "session_revoked"
	KindIdentitySessionIssued        Kind = "identity_session_issued"
	KindFamilyCreated                Kind = "family_created"
	KindFamilyBootstrapped           Kind = "family_bootstrapped"
	KindFamilyProfileRepaired        Kind = "family_profile_repaired"
	KindInvitationCreated            Kind = "invitation_created"
	KindInvitationRevoked            Kind = "invitation_revoked"
	KindInvitationRedeemed           Kind = "invitation_redeemed"
	KindMemberLeft                   Kind = "member_left"
	KindMemberRemoved                Kind = "member_removed"
	KindMemberRoleChanged            Kind = "member_role_changed"
	KindOwnerTransferred             Kind = "owner_transferred"
	KindOwnerRecovered               Kind = "owner_recovered"
	KindEntitlementGranted           Kind = "entitlement_granted"
	KindEntitlementSuspended         Kind = "entitlement_suspended"
	KindOperatorEntitlementGranted   Kind = "operator_entitlement_granted"
	KindOperatorEntitlementSuspended Kind = "operator_entitlement_suspended"
	KindFamilyDeletionScheduled      Kind = "family_deletion_scheduled"
	KindFamilyRecovered              Kind = "family_recovered"
	KindRecoveryForfeited            Kind = "recovery_forfeited"
	KindAccountDeleted               Kind = "account_deleted"
	KindFamilyPurgeStarted           Kind = "family_purge_started"
	KindFamilyPurgeCompleted         Kind = "family_purge_completed"
	KindIntegrationGranted           Kind = "integration_granted"
	KindIntegrationRevoked           Kind = "integration_revoked"
	KindLegacyImported               Kind = "legacy_imported"
)

var kinds = map[Kind]struct{}{
	KindAccountRegistered: {}, KindIdentityUnavailable: {}, KindIdentityRestored: {}, KindDeviceRegistered: {}, KindDeviceBindingRevoked: {},
	KindSessionGranted: {}, KindSessionRotated: {}, KindSessionRevoked: {}, KindIdentitySessionIssued: {}, KindFamilyCreated: {},
	KindFamilyBootstrapped: {}, KindFamilyProfileRepaired: {}, KindInvitationCreated: {}, KindInvitationRevoked: {}, KindInvitationRedeemed: {},
	KindMemberLeft: {}, KindMemberRemoved: {}, KindMemberRoleChanged: {}, KindOwnerTransferred: {}, KindOwnerRecovered: {},
	KindEntitlementGranted: {}, KindEntitlementSuspended: {}, KindOperatorEntitlementGranted: {}, KindOperatorEntitlementSuspended: {},
	KindFamilyDeletionScheduled: {}, KindFamilyRecovered: {}, KindRecoveryForfeited: {}, KindAccountDeleted: {}, KindFamilyPurgeStarted: {},
	KindFamilyPurgeCompleted: {}, KindIntegrationGranted: {}, KindIntegrationRevoked: {}, KindLegacyImported: {},
}

func (k Kind) valid() bool { _, ok := kinds[k]; return ok }

var (
	canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	keyIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

const nilUUID = "00000000-0000-0000-0000-000000000000"

// validID accepts the lowercase canonical text PostgreSQL renders for a
// uuid, so IDs hash and compare identically before and after a round trip.
func validID(id string) bool { return canonicalUUID.MatchString(id) && id != nilUUID }

func validKeyring(keyring identity.DigestKeyring) bool {
	if len(keyring.Previous) > 7 {
		return false
	}
	ids := map[string]struct{}{}
	secrets := map[[32]byte]struct{}{}
	for _, key := range append([]identity.DigestKey{keyring.Current}, keyring.Previous...) {
		if !keyIDPattern.MatchString(key.ID) || key.Secret == ([32]byte{}) {
			return false
		}
		if _, dup := ids[key.ID]; dup {
			return false
		}
		if _, dup := secrets[key.Secret]; dup {
			return false
		}
		ids[key.ID], secrets[key.Secret] = struct{}{}, struct{}{}
	}
	return true
}
