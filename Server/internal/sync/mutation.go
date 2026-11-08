package sync

import (
	"context"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrMutationIDReused is care's: a (device, mutation id) receipt holds
	// another mutation.
	ErrMutationIDReused       = care.ErrMutationIDReused
	ErrFamilyScopeRequired    = errors.New("family_scope_required")
	ErrSessionRefreshRequired = errors.New("session_refresh_required")
	// ErrReadOnlyCaregiver is returned for a viewer's mutation. The batch's
	// Policy raises it inside the guarded transaction before anything is
	// written, so a refused batch never looks like progress.
	ErrReadOnlyCaregiver  = care.ErrReadOnlyCaregiver
	ErrFeatureUnavailable = care.ErrFeatureUnavailable
)

// Service serves sync and snapshots from care_entities. Each request is one
// guarded transaction (ServeSync, ServeSnapshot); Exchange and SnapshotPage
// are that transaction's body.
type Service struct {
	guard     authority.RequestGuard
	identity  protocol.ServerInfo
	limits    protocol.Limits
	now       func() time.Time
	publisher care.Publisher
	pageKeys  identity.DigestKeyring
	policy    care.Policy
	imports   care.ImportAdmission
	progress  Progress
	// pageLookup fills a first page's random token lookup (nil: crypto/rand;
	// tests force a repeat).
	pageLookup func([]byte) (int, error)
}

// SetImportAdmission admits import-tagged mutations (the importer).
func (s *Service) SetImportAdmission(admission care.ImportAdmission) { s.imports = admission }

// policyOf is the batch Policy over the family policy allowed, refusing as
// authority.familyGrant refuses a disallowed action: a viewer where a
// caregiver would be allowed is read_only_caregiver, another non-owner
// member owner_required, anyone else membership_revoked.
func policyOf(allowed func(role, action string) bool) care.Policy {
	if allowed == nil {
		return nil
	}
	return func(role, action string) error {
		if allowed(role, action) {
			return nil
		}
		if role != "" && role != "owner" {
			if role == "viewer" && allowed("caregiver", action) {
				return care.ErrReadOnlyCaregiver
			}
			return authority.ErrOwnerRequired
		}
		return authority.ErrMembershipRevoked
	}
}

// ready reports whether the service was composed completely.
func (s *Service) ready() bool {
	return s != nil && s.guard != nil && s.now != nil && s.policy != nil && validPageKeys(s.pageKeys) &&
		s.identity.Validate() == nil && s.limits.Validate() == nil
}

// withFamily runs run in one guarded transaction for familyID, the
// principal's session family. The guard takes the family and role from the
// session row (never from the principal), so a grant for any other family or
// account is refused as membership_revoked, the code the legacy guard gave a
// family mismatch. The guard is asked for "read", which every role holds
// (and which the family_sync entitlement covers like every write); the
// batch Policy then decides each mutation's own action.
//
// The whole guarded transaction is recorded as one database phase, after it
// ended, so an opt-in recorder never runs inside it.
func (s *Service) withFamily(ctx context.Context, p access.Principal, familyID string, run func(pgx.Tx, access.Grant) error) error {
	start := time.Now()
	err := s.guard.WithFamilyTx(ctx, p, "read", func(tx pgx.Tx, g access.Grant) error {
		if g.AccountID != p.AccountID || g.FamilyID != familyID {
			return authority.ErrMembershipRevoked
		}
		return run(tx, g)
	})
	diagnostics.Record(ctx, nil, diagnostics.Database, time.Since(start))
	return err
}

// Apply applies one mutation, a batch of one with no pull, in its own
// guarded transaction, and publishes the new high water after the commit.
func (s *Service) Apply(ctx context.Context, p access.Principal, familyID string, m protocol.Mutation) (protocol.Outcome, error) {
	if !s.ready() {
		return protocol.Outcome{}, errUnavailable
	}
	if p.Scope != "family" {
		return protocol.Outcome{}, ErrFamilyScopeRequired
	}
	if p.SessionScope != "family" || p.SessionFamilyID != familyID {
		return protocol.Outcome{}, ErrSessionRefreshRequired
	}
	if !protocol.ValidUUID(familyID) || familyID == "" {
		return protocol.Outcome{}, ErrFamilyScopeRequired
	}
	var result care.BatchResult
	txCtx, cancel := context.WithTimeout(ctx, time.Duration(s.limits.SyncTransactionSeconds)*time.Second)
	err := s.withFamily(txCtx, p, familyID, func(tx pgx.Tx, g access.Grant) error {
		var err error
		result, err = care.ApplyBatch(txCtx, tx, s.batch(p, g, []protocol.Mutation{m}), nil)
		return err
	})
	cancel()
	if err != nil {
		return protocol.Outcome{}, err
	}
	s.publish(ctx, familyID, result)
	if result.Fatal != nil {
		return protocol.Outcome{}, result.Fatal
	}
	return result.Outcomes[0], nil
}

// batch is p's mutations as a care batch under g, with the family policy.
func (s *Service) batch(p access.Principal, g access.Grant, mutations []protocol.Mutation) care.BatchCommand {
	return care.BatchCommand{Grant: g, DeviceID: p.DeviceID, Mutations: mutations, AcceptedAt: s.now().UTC(), Server: s.identity,
		Limits: s.limits, Policy: s.policy, Imports: s.imports}
}

// publish announces the batch's high water after the commit, best effort:
// the committed receipts are already replayable.
func (s *Service) publish(ctx context.Context, familyID string, result care.BatchResult) {
	if cursor := s.publishCursor(familyID, result); cursor != nil && s.publisher != nil {
		_ = s.publisher.Publish(ctx, familyID, *cursor)
	}
}

func (s *Service) publishCursor(familyID string, result care.BatchResult) *protocol.Cursor {
	if result.HighWater <= result.Base {
		return nil
	}
	return &protocol.Cursor{ServerInstanceID: s.identity.InstanceID, RecoveryEpoch: s.identity.RecoveryEpoch, FamilyID: familyID,
		Sequence: result.HighWater}
}
