// Package mcpapi exposes the private read-only family tools over stateless
// MCP Streamable HTTP.
//
// Private bearer profile: every request (discovery, initialize, list, call,
// notifications and GET) carries exactly one "Authorization: Bearer btmcp1..."
// header issued by the operator CLI. There is no OAuth: no protected-resource
// metadata, no authorization server, no token exchange or refresh. A missing,
// malformed, duplicate, phone-session, revoked or expired credential is 401
// with "WWW-Authenticate: Bearer realm=..." and nothing else.
package mcpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/familyread"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/requestlog"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// MaxRequestBytes bounds one HTTP request body, including chunked input.
	MaxRequestBytes = 32 << 10
	// MaxResponseBytes bounds one complete encoded HTTP response body.
	MaxResponseBytes = 256 << 10
	// RequestTimeout is the end-to-end budget from handler entry.
	RequestTimeout = 5 * time.Second

	ProtocolCurrent = "2026-07-28"
	ProtocolLegacy  = "2025-11-25"

	maxAuthorizationBytes = 512
	maxResponseReserve    = 250 * time.Millisecond
)

// FamilyReader is the guarded family-read service (familyread.Service).
type FamilyReader interface {
	ListBabies(ctx context.Context, credential string) (familyread.BabiesResult, error)
	HistoryWithBudget(ctx context.Context, credential string, q familyread.HistoryQuery, fits familyread.HistoryBudget) (familyread.HistoryResult, error)
	Summary(ctx context.Context, credential string, q familyread.SummaryQuery) (familyread.SummaryResult, error)
}

// Authenticator is the live HTTP-level credential check
// (authority.IntegrationReader). Its identity only keys quotas and the
// credential gate; every data read re-authorizes through FamilyReader.
type Authenticator interface {
	CheckIntegration(ctx context.Context, credential string) (authority.IntegrationIdentity, error)
}

// Admission is the shared finite-work controller (*admission.Controller).
// The handler acquires exactly one admission.MCP slot per HTTP request and
// holds it until the SDK and every tool goroutine have returned.
type Admission interface {
	TryAcquire(class admission.Class) (release func(), ok bool)
}

// CredentialGate admits one in-flight authenticated request per integration
// grant (*admission.Keyed). It is keyed only by authenticated grant IDs.
type CredentialGate interface {
	TryAcquire(grantID string) (release func(), ok bool)
}

// Quota charges every authenticated HTTP request once, notifications and
// metadata requests included, after the credential gate admitted it and
// before its body is read. A refused decision maps to 429 with Retry-After;
// an error maps to 503.
type Quota interface {
	Charge(ctx context.Context, identity authority.IntegrationIdentity) (limits.Decision, error)
}

// Readiness is the node readiness gate (the server's database readiness).
// A node that is not ready serves no MCP read.
type Readiness interface {
	Check(ctx context.Context) error
}

type Options struct {
	Reader        FamilyReader
	Authenticator Authenticator
	Admission     Admission
	Gate          CredentialGate
	Quota         Quota
	// Readiness, when set, is checked on every request before admission and
	// credential work; a refusal is the safe no-store JSON-RPC 503, so MCP
	// fails closed exactly when phones do (a rotated epoch, a server_state or
	// actor-ID key mismatch, role or schema drift, a missing credential key).
	Readiness Readiness
	// CanonicalOrigin is the single private origin, for example
	// https://baby-tracker.example.ts.net. Host must match it; a supplied
	// Origin header must equal it. Forwarding headers are never consulted.
	CanonicalOrigin string
	// Timeout shortens the five-second budget (tests). Zero means RequestTimeout.
	Timeout time.Duration
	// Now is the clock for repeat-refusal expiry (tests). Nil means time.Now.
	Now func() time.Time
}

// heldGate is implemented by credential gates that can confirm a current
// holder (*admission.Keyed); only then are concurrency refusals repeated
// without the full check.
type heldGate interface {
	Held(grantID string) bool
}

type handler struct {
	options      Options
	sdk          http.Handler
	origin, host string
	timeout      time.Duration
	now          func() time.Time
	refusals     *refusalCache
}

// NewHandler returns the /mcp handler. Every dependency is required.
func NewHandler(options Options) (http.Handler, error) {
	if options.Reader == nil || options.Authenticator == nil || options.Admission == nil || options.Gate == nil || options.Quota == nil {
		return nil, errors.New("mcp handler requires reader, authenticator, admission, credential gate and quota")
	}
	if options.Timeout < 0 || options.Timeout > RequestTimeout {
		return nil, errors.New("mcp timeout must not exceed five seconds")
	}
	origin, host, err := canonicalOrigin(options.CanonicalOrigin)
	if err != nil {
		return nil, err
	}
	h := &handler{options: options, origin: origin, host: host, timeout: options.Timeout, now: options.Now}
	if h.timeout == 0 {
		h.timeout = RequestTimeout
	}
	if h.now == nil {
		h.now = time.Now
	}
	var held func(string) bool
	if gate, ok := options.Gate.(heldGate); ok {
		held = gate.Held
	}
	h.refusals = newRefusalCache(maxRefusalEntries, h.now, held)
	server := mcp.NewServer(implementation, &mcp.ServerOptions{
		Instructions:              serverInstructions,
		Capabilities:              &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		SupportedProtocolVersions: []string{ProtocolCurrent, ProtocolLegacy},
		// Discovery and lists are per-credential: never a shared-cache hint.
		// Pure: the SDK calls this under its list lock.
		SetCacheable: func(_ context.Context, _ mcp.Request, cacheable *mcp.Cacheable) {
			cacheable.TTLMs, cacheable.CacheScope = 0, "private"
		},
	})
	specs, err := h.catalog()
	if err != nil {
		return nil, err
	}
	for _, spec := range specs {
		server.AddTool(spec.tool, h.toolHandler(spec))
	}
	server.AddReceivingMiddleware(h.receive)
	// The lookup is side-effect-free: the SDK consults it more than once.
	h.sdk = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: MaxRequestBytes, PropagateRequestCancellation: true,
	})
	return h, nil
}

func canonicalOrigin(raw string) (origin, host string, err error) {
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", "", errors.New("mcp requires an http(s) canonical origin without path, query or credentials")
	}
	host = normalizeHost(parsed.Scheme, parsed.Host)
	return parsed.Scheme + "://" + host, host, nil
}

func normalizeHost(scheme, host string) string {
	host = strings.ToLower(host)
	if (scheme == "https" && strings.HasSuffix(host, ":443")) || (scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndexByte(host, ':')]
	}
	return host
}

// requestState is request-local: it carries the original bounded HTTP
// context, the authenticated credential and the per-request outcome that
// the buffered final commit turns into an HTTP status.
type requestState struct {
	work       context.Context
	credential string
	identity   authority.IntegrationIdentity
	id         jsonrpc.ID
	current    bool
	mu         sync.Mutex
	failure    *failure
}

type stateKey struct{}

func stateFrom(ctx context.Context) *requestState {
	state, _ := ctx.Value(stateKey{}).(*requestState)
	return state
}

func (s *requestState) fail(f failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil {
		s.failure = &f
	}
}

func (s *requestState) failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure != nil
}

func (s *requestState) recorded() *failure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

// link derives the effective handler context from the SDK's context, keeping
// its private values and explicit MCP cancellation, while adding the original
// absolute deadline and HTTP cancellation. The legacy SDK path strips both
// from its connection context, so this bridge is what bounds 2025-11-25 work.
func (s *requestState) link(ctx context.Context) (context.Context, func()) {
	deadline, _ := s.work.Deadline()
	ctx, cancelDeadline := context.WithDeadline(ctx, deadline)
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(s.work, func() { cancel(context.Cause(s.work)) })
	if s.work.Err() != nil {
		cancel(context.Cause(s.work))
	}
	return ctx, func() {
		stop()
		cancel(nil)
		cancelDeadline()
	}
}

var allowedMethods = map[string]bool{
	"initialize": true, "notifications/initialized": true, "ping": true, "server/discover": true,
	"tools/list": true, "tools/call": true, "notifications/cancelled": true,
}

// receive is the only receiving middleware. It denies every SDK built-in
// outside the read-only surface, links deadlines and turns panics into a
// safe 503. The credential gate and quota were already applied per HTTP
// request in serve, before the body was read.
func (h *handler) receive(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (result mcp.Result, err error) {
		state := stateFrom(ctx)
		if state == nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
		}
		if !allowedMethods[method] {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not supported by this read-only server"}
		}
		ctx, stop := state.link(ctx)
		defer stop()
		defer func() {
			if recover() != nil {
				state.fail(failUnavailable)
				result, err = nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
			}
		}()
		return next(ctx, method, request)
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	end := time.Now().Add(h.timeout)
	if deadline, ok := r.Context().Deadline(); ok && deadline.Before(end) {
		end = deadline
	}
	// Work and body reads stop early enough to leave time for a safe reply.
	workEnd := end.Add(-min(maxResponseReserve, h.timeout/10))
	controller := http.NewResponseController(w)
	if controller.SetWriteDeadline(end) != nil || controller.SetReadDeadline(workEnd) != nil {
		writeFailure(w, failUnavailable, jsonrpc.ID{})
		return
	}
	work, cancel := context.WithDeadline(r.Context(), workEnd)
	defer cancel()
	// On success, send the committed bytes under the write deadline, then
	// clear the route deadlines so they cannot poison the next request on a
	// keep-alive connection. (Go's HTTP/1 server also clears both after each
	// request; this keeps the guarantee where it does not.)
	if h.serve(w, r.WithContext(work), work) && controller.Flush() == nil {
		_ = controller.SetReadDeadline(time.Time{})
		_ = controller.SetWriteDeadline(time.Time{})
	}
}

// serve reports whether the request completed without a deadline failure.
func (h *handler) serve(w http.ResponseWriter, r *http.Request, work context.Context) (completed bool) {
	// A panic before the final commit (authentication, credential gate,
	// quota, body read or an SDK panic escaping its middleware) still gets the
	// safe JSON-RPC 503 with no-store. This deferred recover runs last, after
	// the admission and credential releases deferred below have run, and only
	// writes when no client byte has been committed yet.
	committing := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if committing {
				panic(recovered)
			}
			writeFailure(w, failUnavailable, jsonrpc.ID{})
			completed = true
		}
	}()
	if !h.allowedOrigin(r) {
		writeFailure(w, failForbidden, jsonrpc.ID{})
		return true
	}
	credential, ok := bearer(r.Header)
	if !ok {
		writeFailure(w, failUnauthorized, jsonrpc.ID{})
		return true
	}
	// A credential the full check recently authenticated and refused for
	// being over its limit is refused again here, before it can take a
	// global MCP slot or pay for CheckIntegration. A hit only ever refuses.
	digest := credentialDigest(credential)
	if f, hit := h.refusals.lookup(digest); hit {
		requestlog.Aggregate(r.Context())
		writeFailure(w, f, jsonrpc.ID{})
		return true
	}
	if h.options.Readiness != nil && h.options.Readiness.Check(work) != nil {
		writeFailure(w, failUnavailable, jsonrpc.ID{})
		return true
	}
	release, ok := h.options.Admission.TryAcquire(admission.MCP)
	if !ok {
		requestlog.Aggregate(r.Context())
		writeFailure(w, failBusy, jsonrpc.ID{})
		return true
	}
	defer release()
	identity, err := h.options.Authenticator.CheckIntegration(work, credential)
	if err != nil {
		switch {
		case errors.Is(err, authority.ErrIntegrationUnauthorized):
			writeFailure(w, failUnauthorized, jsonrpc.ID{})
			return true
		case work.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
			writeFailure(w, failTimeout, jsonrpc.ID{})
			return false
		}
		writeFailure(w, failUnavailable, jsonrpc.ID{})
		return true
	}
	// One in-flight request per credential, for every authenticated request,
	// before any body byte is read: one credential cannot hold every global
	// MCP slot with slow bodies.
	releaseGrant, ok := h.options.Gate.TryAcquire(identity.GrantID)
	if !ok {
		h.refusals.refuseConcurrency(digest, identity.GrantID)
		writeFailure(w, failConcurrency, jsonrpc.ID{})
		return true
	}
	defer releaseGrant()
	// Quota is charged after the gate, so a concurrency refusal costs none.
	decision, err := h.options.Quota.Charge(work, identity)
	switch {
	case err != nil && work.Err() != nil:
		writeFailure(w, failTimeout, jsonrpc.ID{})
		return false
	case err != nil:
		writeFailure(w, failUnavailable, jsonrpc.ID{})
		return true
	case !decision.Allowed:
		h.refusals.refuseQuota(digest, decision.RetryAfter)
		writeFailure(w, rateLimited(int(max(1, math.Ceil(decision.RetryAfter.Seconds())))), jsonrpc.ID{})
		return true
	}
	body, f := readBody(w, r, work)
	if f != nil {
		writeFailure(w, *f, jsonrpc.ID{})
		return f.status != http.StatusRequestTimeout
	}
	state := &requestState{work: work, credential: credential, identity: identity}
	versions := r.Header.Values("Mcp-Protocol-Version")
	if len(versions) > 1 {
		writeFailure(w, failVersionHeader, jsonrpc.ID{})
		return true
	}
	state.current = len(versions) == 1 && versions[0] >= ProtocolCurrent
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		writeFailure(w, failBatch, jsonrpc.ID{})
		return true
	}
	if message, err := jsonrpc.DecodeMessage(body); err == nil {
		if request, ok := message.(*jsonrpc.Request); ok {
			state.id = request.ID
			// subscriptions/listen would force an SSE stream; it is not part
			// of this read-only surface.
			if request.Method == "subscriptions/listen" {
				f := failMethodNotFound
				if state.current {
					f.status = http.StatusNotFound
				}
				writeFailure(w, f, request.ID)
				return true
			}
			// Only a legacy initialize may precede version negotiation.
			if len(versions) == 0 && request.Method != "initialize" {
				writeFailure(w, failVersionHeader, request.ID)
				return true
			}
		}
	}
	forward := r.WithContext(context.WithValue(work, stateKey{}, state))
	forward.Body = io.NopCloser(bytes.NewReader(body))
	forward.ContentLength = int64(len(body))
	buffer := &bufferedWriter{real: w, header: http.Header{}}
	h.sdk.ServeHTTP(buffer, forward)
	committing = true
	return h.commit(w, buffer, state, work)
}

func (h *handler) allowedOrigin(r *http.Request) bool {
	scheme := h.origin[:strings.Index(h.origin, "://")]
	if normalizeHost(scheme, r.Host) != h.host {
		return false
	}
	origins := r.Header.Values("Origin")
	switch len(origins) {
	case 0:
		return true // native clients send no Origin
	case 1:
		return origins[0] == h.origin
	}
	return false
}

func bearer(header http.Header) (string, bool) {
	values := header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > maxAuthorizationBytes {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	for _, c := range []byte(token) {
		// RFC 6750 b64token characters only.
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~+/=", c) >= 0) {
			return "", false
		}
	}
	return token, true
}

// readBody reads at most MaxRequestBytes under the real read deadline.
func readBody(w http.ResponseWriter, r *http.Request, work context.Context) ([]byte, *failure) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err == nil {
		return body, nil
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, &failTooLarge
	case work.Err() != nil || isTimeout(err):
		return nil, &failRequestTimeout
	}
	return nil, &failBadRequest
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// commit is the single place response bytes reach the client: a recorded
// failure, a safe overflow error, a deadline error or the SDK's buffered
// response, always with Cache-Control: no-store.
func (h *handler) commit(w http.ResponseWriter, buffer *bufferedWriter, state *requestState, work context.Context) bool {
	status, header, body, overflow := buffer.close()
	if f := state.recorded(); f != nil {
		writeFailure(w, *f, state.id)
		return f.code != failTimeout.code
	}
	if overflow {
		writeFailure(w, failResultTooLarge, state.id)
		return true
	}
	if status == 0 {
		if work.Err() != nil {
			writeFailure(w, failTimeout, state.id)
			return false
		}
		writeFailure(w, failUnavailable, state.id)
		return true
	}
	target := w.Header()
	for name, values := range header {
		if name == "Content-Length" || name == "Mcp-Session-Id" {
			continue
		}
		target[name] = values
	}
	target.Set("Cache-Control", "no-store")
	target.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return true
}

// bufferedWriter retains the SDK's status, headers and at most
// MaxResponseBytes of body before any client byte is written. Overflow
// discards the whole candidate. Unwrap keeps deadline access; Flush is a
// deliberate no-op so nothing escapes before the final commit.
type bufferedWriter struct {
	real     http.ResponseWriter
	mu       sync.Mutex
	header   http.Header
	status   int
	body     []byte
	overflow bool
	closed   bool
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.status == 0 && !b.closed {
		b.status = status
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, http.ErrHandlerTimeout
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if b.overflow || len(b.body)+len(p) > MaxResponseBytes {
		b.overflow, b.body = true, nil
		return len(p), nil
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

func (b *bufferedWriter) Flush() {}

func (b *bufferedWriter) Unwrap() http.ResponseWriter { return b.real }

func (b *bufferedWriter) close() (int, http.Header, []byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return b.status, b.header.Clone(), b.body, b.overflow
}
