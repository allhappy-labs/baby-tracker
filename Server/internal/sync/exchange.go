package sync

import (
	"context"
	"errors"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// Options composes a Service. Guard authenticates and authorizes each
// request (the security authority); Allowed is the family policy
// (family.Allowed); every batch is decided under it. Progress (optional)
// receives each completed sync and snapshot request's progress after the
// commit (the wake worker's tracker).
type Options struct {
	Guard     authority.RequestGuard
	Server    protocol.ServerInfo
	Limits    protocol.Limits
	Now       func() time.Time
	Publisher care.Publisher
	PageKeys  identity.DigestKeyring
	Allowed   func(role, action string) bool
	Progress  Progress
}

// Progress records a device's progress in memory (wake.Tracker): the cursor
// it presented (acked, 0 for none) and, for a final page, the high water
// served (delivered, 0 otherwise). It must not touch the database: it runs
// on the request path, after the commit.
type Progress interface {
	Synced(familyID, deviceID string, acked, delivered uint64, at time.Time)
}

// New is the sync service over o. A service missing its guard, clock,
// policy, page keys or a valid server identity and limits refuses every
// request as unavailable.
func New(o Options) *Service {
	return &Service{guard: o.Guard, identity: o.Server, limits: o.Limits, now: o.Now, publisher: o.Publisher,
		pageKeys: clonePageKeys(o.PageKeys), policy: policyOf(o.Allowed), progress: o.Progress}
}

// Exchanged is what one sync request did inside its transaction: the
// response, the high water to publish after the commit (nil when nothing
// was written), and Fatal, the refusal to return after the commit (the
// mutations before it are written and must be committed).
type Exchanged struct {
	Response protocol.SyncResponse
	Publish  *protocol.Cursor
	Fatal    error
}

// Exchange is everything one sync request does, inside tx, a transaction
// the caller's guard opened with the tenant context of g (ServeSync calls
// it from WithRequestTx). Family and role come from g only.
//
//   - A page token continues a pull: one statement reads the binding and
//     the current rows of its range.
//   - No mutations: one statement reads the high water and the first page,
//     with no lock.
//   - Mutations: care.ApplyBatch locks the family row, applies the batch and
//     reads the first page in its write round trip.
//
// A returned error means roll back. The caller commits, then publishes
// Publish and returns Fatal or Response. An incompletely composed service
// (no family policy, page keys, clock or valid limits) and a grant of
// another account than p refuse before any statement.
func (s *Service) Exchange(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, request protocol.SyncRequest) (Exchanged, error) {
	if err := s.seamReady(p, g); err != nil {
		return Exchanged{}, err
	}
	if err := s.validateSync(request, g.FamilyID); err != nil {
		return Exchanged{}, err
	}
	return s.exchange(ctx, tx, p, g, request)
}

// errUnavailable refuses a request to an incompletely composed service.
var errUnavailable = errors.New("sync service unavailable")

// seamReady is what the in-transaction seams check before any statement:
// a completely composed service and a grant of p's own account.
func (s *Service) seamReady(p access.Principal, g access.Grant) error {
	if !s.ready() {
		return errUnavailable
	}
	if g.AccountID != p.AccountID {
		return authority.ErrMembershipRevoked
	}
	return nil
}

// exchange is Exchange of a request validateSync accepted.
func (s *Service) exchange(ctx context.Context, tx pgx.Tx, p access.Principal, g access.Grant, request protocol.SyncRequest) (Exchanged, error) {
	if request.PageToken != nil {
		response, err := s.syncContinuation(ctx, tx, p, g, *request.PageToken)
		return Exchanged{Response: response}, err
	}
	after := uint64(0)
	if request.Cursor != nil {
		after = request.Cursor.Sequence
	}
	limit := s.limits.SyncPageSize + 1
	if len(request.Mutations) == 0 {
		highWater, changes, err := care.ReadPull(ctx, tx, g.FamilyID, after, limit)
		if err != nil {
			return Exchanged{}, err
		}
		if after > highWater {
			return Exchanged{}, ErrSnapshotRequired
		}
		response, err := s.syncPage(ctx, tx, p, g, s.firstRange(after, highWater), changes, []protocol.Outcome{}, nil)
		return Exchanged{Response: response}, err
	}
	if err := s.firstRefused(g, request.Mutations[0]); err != nil {
		return Exchanged{}, err
	}
	result, err := care.ApplyBatch(ctx, tx, s.batch(p, g, request.Mutations), &care.PullRequest{After: after, Limit: limit})
	if err != nil {
		return Exchanged{}, err
	}
	out := Exchanged{Publish: s.publishCursor(g.FamilyID, result), Fatal: result.Fatal}
	if out.Fatal != nil {
		return out, nil
	}
	if after > result.HighWater {
		// The mutations are written; the stale cursor is refused after the
		// commit, as the legacy pull after the per-mutation commits was.
		out.Fatal = ErrSnapshotRequired
		return out, nil
	}
	out.Response, err = s.syncPage(ctx, tx, p, g, s.firstRange(after, result.HighWater), result.Changes, result.Outcomes, nil)
	return out, err
}

// firstRefused is the family policy's refusal of a batch's first mutation
// (a viewer's batch: read_only_caregiver), decided before any statement.
// care.ApplyBatch would refuse the same mutation with the same error
// (BatchResult.Fatal) after locking the family row, with nothing written
// before it, so refusing first changes nothing but the cost: the request
// rolls back after its authority round trip. A first mutation of an
// unknown action is left to ApplyBatch, which refuses it in its own order.
func (s *Service) firstRefused(g access.Grant, first protocol.Mutation) error {
	action, err := care.ActionFor(first)
	if err != nil || s.policy == nil {
		return nil
	}
	return s.policy(g.Role, action)
}

// validateSync refuses a malformed request before any database work, as
// the legacy service did.
func (s *Service) validateSync(request protocol.SyncRequest, familyID string) error {
	surface := protocol.SurfaceSync
	for _, mutation := range request.Mutations {
		if mutation.EntityKind == "timer" {
			surface = protocol.SurfaceSyncTimer
			break
		}
	}
	if request.ProtocolVersion != "v1" || protocol.ValidateRouteCapabilities(surface, request.Capabilities, request.Capabilities, s.identity.Capabilities) != nil || request.Mutations == nil {
		return ErrInvalidSync
	}
	if len(request.Mutations) > s.limits.SyncBatchSize {
		return ErrBatchLimit
	}
	if request.PageToken != nil && (request.Cursor != nil || len(request.Mutations) != 0) {
		return ErrInvalidSync
	}
	if request.PageToken == nil && request.Cursor != nil {
		if request.Cursor.Validate() != nil || request.Cursor.ServerInstanceID != s.identity.InstanceID || request.Cursor.RecoveryEpoch != s.identity.RecoveryEpoch || request.Cursor.FamilyID != familyID {
			return ErrSnapshotRequired
		}
	}
	return nil
}
