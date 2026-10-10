package identity

import (
	"context"
	"time"
)

// Authority is the PostgreSQL security authority as identity uses it: sign-in
// challenges, identity resolution and Apple notification effects.
// authority.(*Service).IdentityAuthority() implements it; identity cannot
// import the authority package, which imports identity's key types.
//
// Errors: ConsumeChallenge reports a missing, consumed or expired challenge
// as ErrInvalidOrExpiredChallenge and CreateChallenge wraps ErrUnavailable.
// The other methods return the authority's coded errors unchanged (for
// example authorization_quarantine or command_id_reused), which auth.Code
// reports.
type Authority interface {
	// CreateChallenge stores a challenge digest; expiresAt is now plus five
	// minutes.
	CreateChallenge(ctx context.Context, digest [32]byte, expiresAt, now time.Time) error
	// ConsumeChallenge consumes an unconsumed, unexpired challenge once.
	ConsumeChallenge(ctx context.Context, digest [32]byte, now time.Time) error
	// ResolveIdentity resolves an identity's authority keys (current first).
	ResolveIdentity(ctx context.Context, candidates []string) (IdentityAuthority, error)
	// ReserveAppleNotification reserves a notification's command ID under
	// its keyed digests (current first) and returns the stored command ID.
	ReserveAppleNotification(ctx context.Context, candidates []KeyedDigest, commandID string, at time.Time) (string, bool, error)
	// NotificationReceipt reports whether one of commandIDs already marked
	// the account's identity unavailable for this notification digest.
	NotificationReceipt(ctx context.Context, accountID string, commandIDs []string, notificationDigest string) (bool, error)
	// MarkIdentityUnavailable marks an active account's identity unavailable
	// and revokes its sessions; an exact retry returns nil.
	MarkIdentityUnavailable(ctx context.Context, command IdentityUnavailableCommand) error
}

// IdentityAuthority is the authority over an identity. Status is absent,
// active, identity_unavailable or deleted; AccountID, IdentityKey and
// LifecycleGeneration name the newest account bound to the identity (empty
// when absent); NextActorGeneration is the generation a new account for it
// takes. ProfileRequired is the live account's profile_required.
type IdentityAuthority struct {
	Status                                   string
	AccountID, IdentityKey                   string
	LifecycleGeneration, NextActorGeneration uint64
	ProfileRequired                          bool
}

// Live reports whether the identity resolves to a live account (active or
// identity_unavailable) that a sign-in signs into or restores.
func (resolution IdentityAuthority) Live() bool {
	return resolution.Status == "active" || resolution.Status == "identity_unavailable"
}

// IdentityUnavailableCommand marks AccountID's Apple identity unavailable
// after an Apple server notification. CommandID is the notification's
// reserved command ID and NotificationDigest its semantic digest.
type IdentityUnavailableCommand struct {
	CommandID, AccountID string
	Candidates           []string
	NotificationDigest   string
	At                   time.Time
}
