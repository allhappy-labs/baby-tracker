package authority

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"babytracker/server/internal/access"
	"github.com/jackc/pgx/v5"
)

// Request is one HTTP request's credential, with the caller's checks at
// their legacy positions around family policy. It names no family and no
// role: both come only from the authenticated session row (the grant).
//
// Admit and Charge run while a request pool connection is held: in
// WithRequestTx inside the open transaction, in
// AuthorizeRequest after its one statement returned the connection. They
// must stay in memory and never block (no I/O, no lock waits): a hook that
// waits holds a transaction open and a pooled connection busy, which the
// one-transaction request path and the pool's sizing do not allow.
//
// Digests are session lookup keys, so Request never prints them (String,
// LogValue).
type Request struct {
	// Digests are the bearer access token's digests under every token key,
	// current first (auth.Service.AccessDigests).
	Digests []KeyedDigest
	// Admit runs after authentication and before family policy: the caller's
	// refusals that rank after a bad credential and before a family refusal
	// (scope, then a held header or body refusal).
	Admit func(access.Principal) error
	// Charge runs after family policy and before run: the in-memory rate
	// limit. A refusal rolls back with nothing else done.
	Charge func(access.Principal, access.Grant) error
}

// String is req with its digests redacted to their key IDs (also %v, %+v
// and %#v).
func (req Request) String() string {
	return "authority.Request{Digests:" + redactedDigests(req.Digests) + " Admit:" + strconv.FormatBool(req.Admit != nil) +
		" Charge:" + strconv.FormatBool(req.Charge != nil) + "}"
}

// GoString is String (%#v).
func (req Request) GoString() string { return req.String() }

// LogValue is req's digest key IDs and which hooks it carries.
func (req Request) LogValue() slog.Value {
	return slog.GroupValue(slog.String("digests", redactedDigests(req.Digests)), slog.Bool("admit", req.Admit != nil),
		slog.Bool("charge", req.Charge != nil))
}

// redactedDigests is digests as their key IDs, in order.
func redactedDigests(digests []KeyedDigest) string {
	ids := make([]string, len(digests))
	for i, digest := range digests {
		ids[i] = digest.KeyID + ":[redacted]"
	}
	return "[" + strings.Join(ids, " ") + "]"
}

// RequestGuard is the request seam: FamilyGuard plus the one-round-trip
// request transaction. *Service implements it; tests wrap it to inject
// faults or pauses.
type RequestGuard interface {
	FamilyGuard
	WithRequestTx(ctx context.Context, req Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error
}

var _ RequestGuard = (*Service)(nil)

// WithRequestTx authenticates req's access token, sets the tenant context
// and reads family policy's inputs in one round trip (BEGIN and
// app.request_authority by digest, pipelined), then applies admitRequest's
// order in Go and runs run in that read-committed transaction, which it
// commits. It locks nothing: a mutating caller locks the family row itself.
// Any refusal rolls back (one more round trip); so does run's error or a
// panic in a hook or in run. Revocation after commit wins: the session row
// is read in this transaction, so a revocation committed before it began
// refuses it, and one committed after it read the row lets it complete.
func (s *Service) WithRequestTx(ctx context.Context, req Request, action string, run func(pgx.Tx, access.Principal, access.Grant) error) error {
	if run == nil {
		return ErrUnavailable
	}
	args, ok := digestArgs(req.Digests)
	if !ok {
		return ErrSessionRevoked
	}
	var row requestRow
	tx, err := beginRequest(ctx, s.pool, requestBegin, func(r pgx.Row) (err error) { row, err = scanRequest(r); return err },
		requestAuthoritySQL, args, nil, true, int64(s.epoch))
	if err != nil {
		return err
	}
	// The hooks run inside finish, so its deferred rollback also covers a
	// refusal (the same one round trip refuse would cost) and a panic.
	return finish(ctx, tx, func() error {
		principal, grant, err := s.admitRequest(row, req, action, s.at(time.Time{}))
		if err != nil {
			return err
		}
		return run(tx, principal, grant)
	})
}

// AuthorizeRequest is the same order as one pooled statement with no
// transaction (the live handshake, which holds nothing open). The tenant
// context app.request_authority installs is local to that statement's
// implicit transaction, so the pooled connection keeps none of it.
func (s *Service) AuthorizeRequest(ctx context.Context, req Request, action string) (access.Principal, access.Grant, error) {
	args, ok := digestArgs(req.Digests)
	if !ok {
		return access.Principal{}, access.Grant{}, ErrSessionRevoked
	}
	row, err := scanRequest(s.pool.QueryRow(ctx, requestAuthoritySQL, args, nil, true, int64(s.epoch)))
	if err != nil {
		return access.Principal{}, access.Grant{}, unavailable("authorize request", err)
	}
	return s.admitRequest(row, req, action, s.at(time.Time{}))
}

// admitRequest is every request's refusal order over one request row:
// authentication (checkAuthenticated: session_revoked, scope_failed,
// epoch_changed, reauthentication_required, access_expired), the caller's
// Admit, family policy (familyGrant), the caller's Charge. The principal and
// grant are the row's; nothing the caller supplies besides the digests
// reaches them.
func (s *Service) admitRequest(row requestRow, req Request, action string, now time.Time) (access.Principal, access.Grant, error) {
	if err := s.checkAuthenticated(row, req.Digests, now); err != nil {
		return access.Principal{}, access.Grant{}, err
	}
	principal := row.principal()
	if req.Admit != nil {
		if err := req.Admit(principal); err != nil {
			return access.Principal{}, access.Grant{}, err
		}
	}
	grant, err := s.familyGrant(row, action, now)
	if err != nil {
		return access.Principal{}, access.Grant{}, err
	}
	if req.Charge != nil {
		if err := req.Charge(principal, grant); err != nil {
			return access.Principal{}, access.Grant{}, err
		}
	}
	return principal, grant, nil
}
