package sync

import (
	"context"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// ServeSync is one sync request: one guarded transaction around Exchange,
// then the post-commit publish, then Fatal or the response. A completed
// request reports the device's progress to Progress (in memory): the
// presented cursor, and on a final page the high water served.
//
// The transaction is the request seam's (authority.RequestGuard
// WithRequestTx): BEGIN and the session lookup by req's digests in one round
// trip, then, in Go, authentication, req.Admit, family policy for "read"
// and req.Charge, in that order; any refusal rolls back with nothing else
// done. Exchange then validates the request against the grant's family
// before its first statement. Family and role come only from the session
// row: nothing reads the family first.
func (s *Service) ServeSync(ctx context.Context, req authority.Request, request protocol.SyncRequest) (protocol.SyncResponse, error) {
	if !s.ready() {
		return protocol.SyncResponse{}, errUnavailable
	}
	txCtx, cancel := context.WithTimeout(ctx, time.Duration(s.limits.SyncTransactionSeconds)*time.Second)
	defer cancel()
	var exchanged Exchanged
	var familyID, deviceID string
	start := time.Now()
	err := s.guard.WithRequestTx(txCtx, req, "read", func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		familyID, deviceID = g.FamilyID, p.DeviceID
		var err error
		exchanged, err = s.Exchange(txCtx, tx, p, g, request) // seamReady and validateSync run first, before any statement
		return err
	})
	// The whole transaction is one database phase, recorded after it ended,
	// so an opt-in recorder never runs inside it.
	diagnostics.Record(ctx, nil, diagnostics.Database, time.Since(start))
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	response, err := s.committed(ctx, familyID, exchanged)
	if err == nil {
		acked, delivered := uint64(0), uint64(0)
		if request.Cursor != nil {
			acked = request.Cursor.Sequence
		}
		if response.NextPageToken == nil {
			delivered = response.HighWater.Sequence
		}
		s.reportProgress(familyID, deviceID, acked, delivered)
	}
	return response, err
}

// reportProgress hands a completed request's progress to Progress, in
// memory and after the commit. A request that advanced nothing (no cursor
// presented, no final page) has nothing to report.
func (s *Service) reportProgress(familyID, deviceID string, acked, delivered uint64) {
	if s.progress == nil || acked == 0 && delivered == 0 {
		return
	}
	s.progress.Synced(familyID, deviceID, acked, delivered, s.now())
}

// ServeSnapshot is one snapshot page in one guarded transaction: the
// request seam (as ServeSync) around SnapshotPage. The final page reports
// its high water to Progress as delivered. The transaction is
// read-committed and writable, because a first page stores its snapshot
// session and a page with more its continuation binding.
func (s *Service) ServeSnapshot(ctx context.Context, req authority.Request, request protocol.SnapshotRequest) (protocol.SnapshotResponse, error) {
	if !s.ready() {
		return protocol.SnapshotResponse{}, errUnavailable
	}
	txCtx, cancel := context.WithTimeout(ctx, time.Duration(s.limits.SyncTransactionSeconds)*time.Second)
	defer cancel()
	var response protocol.SnapshotResponse
	var familyID, deviceID string
	start := time.Now()
	err := s.guard.WithRequestTx(txCtx, req, "read", func(tx pgx.Tx, p access.Principal, g access.Grant) error {
		familyID, deviceID = g.FamilyID, p.DeviceID
		var err error
		response, err = s.SnapshotPage(txCtx, tx, p, g, request)
		return err
	})
	diagnostics.Record(ctx, nil, diagnostics.Database, time.Since(start))
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	if response.NextPageToken == nil {
		s.reportProgress(familyID, deviceID, 0, response.HighWater.Sequence)
	}
	return response, nil
}
