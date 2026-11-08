package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

// afterFirstRequestTx runs after once, right after the first request
// transaction ended: between a pull page's commit and its binding
// transaction.
type afterFirstRequestTx struct {
	authority.RequestGuard
	calls int
	after func()
}

func (g *afterFirstRequestTx) WithRequestTx(ctx context.Context, req authority.Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	err := g.RequestGuard.WithRequestTx(ctx, req, action, run)
	if g.calls++; g.calls == 1 && g.after != nil {
		g.after()
	}
	return err
}

// A paged pull whose state changes between its page's commit and its
// binding transaction is refused with exactly the status and code a new
// request gets in that state, in the documented order (the binding
// transaction repeats the authority read, Admit and family policy), never
// family policy's recovery_only where a new pull answers Admit's
// family-scope refusal. Two changes: the account's slot moved into
// family recovery with its session left live (the scope moves in place,
// as deletion recovery moves it), and the family scheduled for deletion
// (which revokes the session).
func TestPagedPullRefusedMidResponseAnswersAsANewRequest(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*testing.T, *testkit.Harness)
		want   protocol.ErrorCode
	}{
		{"slot in family recovery", func(t *testing.T, h *testkit.Harness) {
			h.AuthorityExec(t, `UPDATE family_slots SET state='family_recovery' WHERE account_id=$1`, h.Owner.AccountID)
		}, "family_scope_required"},
		{"family scheduled for deletion", func(t *testing.T, h *testkit.Harness) {
			if _, err := h.Family.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(), true); err != nil {
				t.Error(err)
			}
		}, "session_revoked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := testkit.New(t)
			h.Limits.SyncPageSize = 1
			h.RebuildSyncService(t, h.Limits)
			h.SeedBabies(t, 3)
			guard := &afterFirstRequestTx{RequestGuard: h.AuthorityOn(t, h.Runtime)}
			guard.after = func() { c.change(t, h) }
			s := newStackWith(t, h, stackConfig{options: func(o *Options) { o.SyncService = testkit.SyncServiceOn(t, h, guard) }})
			code := func(body []byte) protocol.ErrorCode {
				var response protocol.ErrorResponse
				_ = json.Unmarshal(body, &response)
				return response.Error.Code
			}
			pull := h.SyncRequest(h.Cursor(0), nil)
			raced := s.send(t, "owner", http.MethodPost, "/v1/sync", pull)
			if guard.calls != 2 {
				t.Fatalf("the paged pull ran %d request transactions, want 2 (its page, then its binding)", guard.calls)
			}
			fresh := s.send(t, "owner", http.MethodPost, "/v1/sync", pull)
			if raced.Code != fresh.Code || code(raced.Body.Bytes()) != c.want || code(fresh.Body.Bytes()) != c.want {
				t.Fatalf("refused mid-response: %d %s; a new request: %d %s; want both %s", raced.Code, raced.Body.String(),
					fresh.Code, fresh.Body.String(), c.want)
			}
		})
	}
}
