package sync_test

import (
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// The Phase 3 seams (Exchange, SnapshotPage) run inside a caller's guarded
// transaction, so they check what ApplyAndPull and FullSnapshot check
// before it: a service composed without the family policy (a nil batch
// Policy would apply a viewer's batch) refuses before any statement, and so
// does a grant of another account than the principal's.
func TestSeamsRefuseAnUnreadyServiceAndAForeignGrant(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	viewer := h.AddMember(t, "viewer", "viewer")
	noPolicy := familysync.New(familysync.Options{Guard: h.Authority, Server: h.Info, Limits: h.Limits, Now: func() time.Time { return h.Now },
		PageKeys: testkit.PageKeyring("page.v1", 0x77)})
	full := serviceGuarded(h, h.Authority)
	batch := h.SyncRequest(h.Cursor(1), nil)
	batch.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 1)}
	run := func(p access.Principal, grant func(access.Grant) access.Grant, use func(pgx.Tx, access.Grant) error) error {
		return h.Authority.WithFamilyTx(t.Context(), p, "read", func(tx pgx.Tx, g access.Grant) error { return use(tx, grant(g)) })
	}
	same := func(g access.Grant) access.Grant { return g }
	foreign := func(g access.Grant) access.Grant { g.AccountID = h.Caregiver.AccountID; return g }
	for _, c := range []struct {
		name    string
		service *familysync.Service
		p       access.Principal
		grant   func(access.Grant) access.Grant
		want    error
	}{
		{"unready service, viewer batch", noPolicy, viewer, same, nil},
		{"foreign grant", full, h.Owner, foreign, authority.ErrMembershipRevoked},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := run(c.p, c.grant, func(tx pgx.Tx, g access.Grant) error {
				_, err := c.service.Exchange(t.Context(), tx, c.p, g, batch)
				return err
			})
			if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
				t.Fatalf("Exchange: %v, want %v", err, c.want)
			}
			err = run(c.p, c.grant, func(tx pgx.Tx, g access.Grant) error {
				_, err := c.service.SnapshotPage(t.Context(), tx, c.p, g, protocol.SnapshotRequest{})
				return err
			})
			if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
				t.Fatalf("SnapshotPage: %v, want %v", err, c.want)
			}
		})
	}
	if h.FamilySequence(t) != 1 || h.Count(t, "sync_snapshot_sessions", h.FamilyID) != 0 || h.CareCount(t, "feed", h.FamilyID) != 0 {
		t.Fatal("a refused seam call wrote something")
	}
}
