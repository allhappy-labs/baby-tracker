package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/requestlog"
)

// The sync, import (resolve, resolve-conflict), push-token (PUT, DELETE)
// and Live Activity registration (PUT) routes read their whole body before
// their request transaction, so that no transaction waits on the network.
// These bounds keep a client without a session from holding the node's
// admission slots or spending its CPU:
//
//   - a client address whose bearer failed more than its budget
//     (limits.ScopeBearerFailureIP) is refused before anything else;
//   - a bearer this node has not seen authenticate is checked before its
//     body is read (Options.BearerAuthenticator: one pooled statement,
//     under the route's admission slot, released before the body is read),
//     and refused with the body unread (Phase 5 Task 19: a 1 MiB body's
//     read and strict decode cost about 24 ms of CPU, which made-up bearers
//     at the failed-bearer budget turned into most of the node's CPU). One
//     that passes is remembered (knownBearers), so a live session's next
//     requests skip that statement;
//   - the body is read before the route takes its admission slot, under a
//     separate cap on concurrent body reads (at most
//     defaultBodyReadsPerClient per client address, defaultBodyReadTotal in
//     all) and a read deadline of Options.BodyReadTimeout (BT_BODY_READ_TIMEOUT,
//     1s to 60s, default 5s).
//
// The body is replayed to the route's own reader, which judges it exactly as
// before, after the credential, which the request transaction checks again.
const (
	// defaultBodyReadTotal bounds the bodies read at once before a
	// credential is checked, so their buffers stay within 64 times the
	// largest body.
	defaultBodyReadTotal = 64
	// defaultBodyReadsPerClient bounds one client address's (an IPv6 /64's)
	// share of defaultBodyReadTotal. A phone's body read takes milliseconds,
	// so a household or carrier NAT rarely holds more than one at a time.
	defaultBodyReadsPerClient = 8
	// defaultBodyReadTimeout is the body read's deadline when
	// Options.BodyReadTimeout is unset (the server always sets it from
	// BT_BODY_READ_TIMEOUT, whose default this is): 1 MiB in 5 s is about
	// 1.7 Mbit/s, and a phone's sync bodies are a few kilobytes.
	defaultBodyReadTimeout = 5 * time.Second
)

// admittedBody is a body route's request once its body was buffered and it
// holds its admission slot.
type admittedBody struct {
	options Options
	address string
	// bearer is the access token's knownBearers key (its first digest);
	// hasBearer is false when it has none.
	bearer    [32]byte
	hasBearer bool
	release   func()
}

// bearerCheckTimeout bounds the credential check made before the body.
const bearerCheckTimeout = 10 * time.Second

// admitAfterBody buffers a body route's request body (at most maximum+1
// bytes, and any read error, which the route's reader meets again exactly as
// it would have) and then takes the route's Sync admission slot. In order:
// a client address over its failed-bearer budget is refused (429); a bearer
// not known to have authenticated is checked (checkBearer: the admission
// slot, then the statement), and refused there with refuse, the route's
// own error writer, as its request transaction would have refused it; one
// already holding its share of body reads is refused (503 server_busy);
// none of these reads any byte of the body. The body is read under the read
// deadline; then admission is taken (503 server_busy when the node is full).
// A body read cut short (the deadline, or a connection closed mid-body)
// spends one unit of the address's failed-bearer budget: such a request
// never reaches its credential check (net/http has cancelled its context, so
// it answers 503 service_unavailable), and uncharged it would let an address
// stall bodies forever. An oversized body is not charged: it is
// read only to its bound and still meets its credential check.
// When it reports false, the response is written.
func admitAfterBody(w http.ResponseWriter, r *http.Request, options Options, digests []authority.KeyedDigest, maximum int64,
	refuse func(http.ResponseWriter, *http.Request, Options, error)) (admittedBody, bool) {
	address := bodyClientAddress(r, options)
	admitted := admittedBody{options: options, address: address}
	if len(digests) != 0 {
		admitted.bearer, admitted.hasBearer = digests[0].Sum, true
	}
	decision, err := options.Limiter.Peek(r.Context(), []limits.Charge{bearerFailureCharge(options, address)})
	if err != nil {
		writeSyncFault(w, r, options, "service_unavailable", err)
		return admittedBody{}, false
	}
	if !decision.Allowed {
		// The body stays unread: close the connection rather than drain it.
		w.Header().Set("Connection", "close")
		requestlog.Aggregate(r.Context())
		writeSyncRateLimited(w, r, options, decision)
		return admittedBody{}, false
	}
	if !admitted.checkBearer(w, r, digests, refuse) {
		return admittedBody{}, false
	}
	releaseRead, ok := options.BodyReads.TryAcquire(address)
	if !ok {
		w.Header().Set("Connection", "close")
		requestlog.Aggregate(r.Context())
		writeServerBusy(w, r)
		return admittedBody{}, false
	}
	buffered, cutShort := bufferBody(w, r, options.BodyReadTimeout, maximum)
	releaseRead()
	r.Body = buffered
	if cutShort {
		admitted.charge(r.Context())
	}
	release, ok := acquireAdmission(w, r, options.Admission, admission.Sync, options.AdmissionMetrics)
	if !ok {
		return admittedBody{}, false
	}
	admitted.release = release
	return admitted, true
}

// noteCredential counts a request whose bearer matched no live session
// (session_revoked) against its client address's failed-bearer budget.
// (A body read cut short was charged already; such a request never meets its
// credential check, since net/http has ended its context.)
// A refusal of the session's own state (sessionStateRefusal) also forgets
// the bearer, so its next request is checked before its body again; a
// route's refusal of a live session (a held or Charge refusal, family
// policy, the service's own) keeps it known, so a client that retries one
// does not pay the check each time.
func (a admittedBody) noteCredential(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if a.hasBearer && sessionStateRefusal(err) {
		a.options.knownBearers.forget(a.bearer)
	}
	if family.Code(err) == "session_revoked" {
		a.charge(ctx)
	}
}

// sessionStateRefusal reports whether err is the authority's refusal of the
// session's own state: session_revoked, access_expired, epoch_changed or
// reauthentication_required as an authority.Code, which is how
// Authenticate and the request transaction's authentication
// (checkAuthenticated) return them, also when a service wraps them (a
// paged pull's second transaction). The routes' own 401s of the same codes
// are syncRefusal values (Admit's unrefreshed account session, import's
// family check in Charge) or a service's sentinel (sync's
// ErrSessionRefreshRequired), never an authority.Code, so they do not
// match. (familyGrant's session_revoked for a session scope the schema
// refuses cannot reach it: checkAuthenticated refuses that row first.)
func sessionStateRefusal(err error) bool {
	var code authority.Code
	if !errors.As(err, &code) {
		return false
	}
	switch code {
	case authority.ErrSessionRevoked, authority.ErrAccessExpired, authority.ErrEpochChanged, authority.ErrReauthenticationRequired:
		return true
	}
	return false
}

// checkBearer is admitAfterBody's credential check before the body, made
// only with Options.BearerAuthenticator and for a bearer not known to have
// authenticated: the route's Sync admission slot (so these statements stay
// within the node's admission like every other; 503 server_busy when it is
// full), Authenticate (one pooled statement), the slot released. A refusal
// is written with refuse as the request transaction's would have been (a
// session_revoked one is charged to the address's failed-bearer budget, as
// noteCredential charges it), and with the body unread the connection is
// closed rather than drained. A bearer that passes is remembered. It reports
// whether the request goes on.
func (a admittedBody) checkBearer(w http.ResponseWriter, r *http.Request, digests []authority.KeyedDigest,
	refuse func(http.ResponseWriter, *http.Request, Options, error)) bool {
	options := a.options
	if options.BearerAuthenticator == nil || a.hasBearer && options.knownBearers.known(a.bearer) {
		return true
	}
	w.Header().Set("Connection", "close")
	release, ok := acquireAdmission(w, r, options.Admission, admission.Sync, options.AdmissionMetrics)
	if !ok {
		return false
	}
	options.knownBearers.checked()
	err := func() error {
		// Deferred, so a panic in Authenticate (which the middleware
		// recovers) cannot keep the slot.
		defer release()
		ctx, cancel := context.WithTimeout(r.Context(), bearerCheckTimeout)
		defer cancel()
		_, err := options.BearerAuthenticator.Authenticate(ctx, digests)
		return err
	}()
	if err != nil {
		a.noteCredential(r.Context(), err)
		refuse(w, r, options, err)
		return false
	}
	w.Header().Del("Connection")
	if a.hasBearer {
		options.knownBearers.remember(a.bearer)
	}
	return true
}

// charge spends one unit of the address's failed-bearer budget. The budget
// is advisory: a limiter error leaves the request's own answer as it was. The
// limiter is in memory, so a cancelled request still charges.
func (a admittedBody) charge(ctx context.Context) {
	_, _ = a.options.Limiter.TakeMany(context.WithoutCancel(ctx), []limits.Charge{bearerFailureCharge(a.options, a.address)})
}

func bearerFailureCharge(options Options, address string) limits.Charge {
	return limits.Charge{Scope: limits.ScopeBearerFailureIP, Key: address, Cost: 1, Policy: options.BearerFailureIPPolicy}
}

// bodyClientAddress is the client address a body route's request is
// counted by: the resolved client address, an IPv6 one as its /64. A
// forwarded chain that does not resolve is counted by the peer (the proxy):
// it is never refused here, since a bad credential still answers first.
func bodyClientAddress(r *http.Request, options Options) string {
	address, err := options.ClientIPResolver.Resolve(r.RemoteAddr, r.Header)
	if err != nil {
		var direct *limits.ClientIPResolver // trusts no proxy: the peer
		if address, err = direct.Resolve(r.RemoteAddr, nil); err != nil {
			address = netip.IPv4Unspecified()
		}
	}
	address = address.Unmap()
	if address.Is6() {
		address = netip.PrefixFrom(address, 64).Masked().Addr()
	}
	return address.String()
}

// bufferBody reads at most maximum+1 bytes of r's body under a read deadline
// of timeout, and returns a body that replays them, then the read's error
// (nil at the end of the body), and whether the read was cut short: it
// failed other than by exceeding the body bound (the deadline, a closed
// connection; either also ends the request's context). A body read in full
// is never cut short. The deadline is cleared once the whole body is read,
// and kept when
// it was not.
func bufferBody(w http.ResponseWriter, r *http.Request, timeout time.Duration, maximum int64) (io.ReadCloser, bool) {
	controller := http.NewResponseController(w)
	// Unsupported (a test recorder): the server's ReadTimeout still applies.
	deadlineSet := controller.SetReadDeadline(time.Now().Add(timeout)) == nil
	data, err := io.ReadAll(io.LimitReader(r.Body, maximum+1))
	if deadlineSet && err == nil && int64(len(data)) <= maximum {
		// The whole body is read, and the connection's idle read has
		// begun: without the deadline it cannot cancel the request.
		// (net/http clears it too when that read starts, at the body's
		// end; this does not rely on it.)
		_ = controller.SetReadDeadline(time.Time{})
	}
	// Otherwise the body was cut short (an error, or too large) and the
	// deadline stays, so the server's drain of what is left after the
	// response ends with it and the connection closes.
	var tooLarge *http.MaxBytesError
	cutShort := err != nil && !errors.As(err, &tooLarge)
	return &replayedBody{data: bytes.NewReader(data), err: err}, cutShort
}

// replayedBody is a buffered request body: its bytes, then the error the
// original read ended with, or io.EOF.
type replayedBody struct {
	data *bytes.Reader
	err  error
}

func (b *replayedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if n, _ := b.data.Read(p); n > 0 {
		return n, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	return 0, io.EOF
}

func (*replayedBody) Close() error { return nil }
