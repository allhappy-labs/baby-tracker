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
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/requestlog"
)

// The sync, import (resolve, resolve-conflict) and push-token (PUT, DELETE)
// routes read their whole body before the credential is checked, so that no
// transaction waits on the network. An unauthenticated client reaches that
// read with any well-formed bearer. These bounds keep it from holding the
// node's admission slots or spending its CPU:
//
//   - the body is read before the route takes its admission slot, under a
//     separate cap on concurrent body reads (at most
//     defaultBodyReadsPerClient per client address, defaultBodyReadTotal in
//     all) and a read deadline of defaultBodyReadTimeout;
//   - a client address whose bearer failed more than its budget
//     (limits.ScopeBearerFailureIP) is refused before its body is read.
//
// Neither changes a refusal a client meets otherwise: the body is replayed to
// the route's own reader, which judges it exactly as before, after the
// credential.
const (
	// defaultBodyReadTotal bounds the bodies read at once before a
	// credential is checked, so their buffers stay within 64 times the
	// largest body.
	defaultBodyReadTotal = 64
	// defaultBodyReadsPerClient bounds one client address's (an IPv6 /64's)
	// share of defaultBodyReadTotal. A phone's body read takes milliseconds,
	// so a household or carrier NAT rarely holds more than one at a time.
	defaultBodyReadsPerClient = 8
	// defaultBodyReadTimeout bounds one body read: 1 MiB in 5 s is about
	// 1.7 Mbit/s, and a phone's sync bodies are a few kilobytes.
	defaultBodyReadTimeout = 5 * time.Second
)

// admittedBody is a body route's request once its body was buffered and it
// holds its admission slot.
type admittedBody struct {
	options Options
	address string
	release func()
}

// admitAfterBody buffers a body route's request body (at most maximum+1
// bytes, and any read error, which the route's reader meets again exactly as
// it would have) and then takes the route's Sync admission slot. In order:
// a client address over its failed-bearer budget is refused (429) and one
// already holding its share of body reads is refused (503 server_busy),
// both before any byte of the body is read; the body is read under the read
// deadline; then admission is taken (503 server_busy when the node is full).
// A body read cut short (the deadline, or a connection closed mid-body)
// spends one unit of the address's failed-bearer budget: such a request
// never reaches its credential check (net/http has cancelled its context, so
// it answers 503 service_unavailable), and uncharged it would let an address
// stall bodies forever. An oversized body is not charged: it is
// read only to its bound and still meets its credential check.
// When it reports false, the response is written.
func admitAfterBody(w http.ResponseWriter, r *http.Request, options Options, maximum int64) (admittedBody, bool) {
	address := bodyClientAddress(r, options)
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
	admitted := admittedBody{options: options, address: address}
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
func (a admittedBody) noteCredential(ctx context.Context, err error) {
	if err == nil || family.Code(err) != "session_revoked" {
		return
	}
	a.charge(ctx)
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
