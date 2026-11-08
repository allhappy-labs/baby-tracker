package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"babytracker/server/internal/protocol"
)

// Outcome classes a request can end in besides OutcomeOK. The set is fixed
// so the result's outcome maps stay small whatever the server answers.
const (
	OutcomeServerBusy    = "server_busy"    // 503 server_busy: admission shed the request (httpapi or MCP)
	OutcomeServerTimeout = "server_timeout" // 503 timeout: MCP's own deadline refusal
	OutcomeNotReady      = "not_ready"      // 503 not_ready
	OutcomeRateLimited   = "rate_limited"   // 429
	OutcomeHTTP5xx       = "http_5xx"       // any other 5xx
	OutcomeTransport     = "transport"      // the connection failed or the answer was lost
	OutcomeTimeout       = "timeout"        // the 10 s request deadline passed
	OutcomeProtocol      = "protocol"       // an answer that does not decode as the protocol says
	OutcomeRejected      = "rejected_mutation"
	OutcomeCrossFamily   = "cross_family"   // an entity or hint of another family reached a device
	OutcomeAccessExpired = "access_expired" // 401 access_expired: the schedule should have refreshed first
	OutcomeRestartWindow = "restart_window" // any failure inside a declared restart window
	// Lost races (Task 5): retryable 503s with Retry-After 1. The request is
	// retried per Retry-After; the lost attempt is recorded under its code
	// and is expected when a retry follows it (expectedOutcome).
	OutcomeSecurityBusy           = "security_busy"
	OutcomeCredentialRotationBusy = "credential_rotation_busy"
	// OutcomeLostRaceExhausted is a lost race no retry followed: the
	// operation gave up, so it is unexpected.
	OutcomeLostRaceExhausted = "lost_race_exhausted"
	// OutcomeImportPaused is a 429 the import cell waited out per
	// Retry-After before resuming (expected).
	OutcomeImportPaused = "import_paused"
	// OutcomeImportUnmapped is an import whose final chunk left its baby
	// mapping incomplete: it writes no record (unexpected; the cell's
	// fresh baby UUIDs never conflict).
	OutcomeImportUnmapped = "import_unmapped"
	// OutcomeImportLeftover is a new import's chunk 0 refused
	// import_unfinished_limit: the owner's account holds an unfinished
	// import this fleet holds no checkpoint of (a run before it, another
	// client). The cell passes the owner over for the staging TTL and
	// imports with the next (expected).
	OutcomeImportLeftover = "import_leftover"
)

// settled is res's outcome as recorded: a lost race no retry follows is
// lost_race_exhausted.
func settled(res result, retrying bool) string {
	if res.lostRace() && !retrying {
		return OutcomeLostRaceExhausted
	}
	return res.Outcome
}

// http4xxCodes bounds the http_4xx_<code> outcomes to the protocol's error
// codes; any other 4xx code is http_4xx_other.
var http4xxCodes = map[string]bool{
	"invalid_request": true, "unsupported_protocol": true, "unsupported_capability": true, "unsupported_resource": true,
	"invalid_timestamp": true, "snapshot_required": true, "snapshot_limit": true, "invalid_page_token": true, "page_expired": true,
	"body_too_large": true, "batch_limit": true, "session_revoked": true, "refresh_reused": true, "reauthentication_required": true,
	"epoch_changed": true, "refresh_receipt_unavailable": true, "device_continuity_invalid": true, "refresh_attempt_conflict": true,
	"credential_conflict": true, "family_scope_required": true, "read_only_caregiver": true, "owner_required": true,
	"entitlement_required": true, "recovery_only": true, "membership_revoked": true, "family_unavailable": true,
	"mutation_id_reused": true, "quota_exceeded": true, "invalid_push_token": true, "live_activity_conflict": true,
	"import_limit": true, "export_limit": true, "unauthorized": true, "not_found": true,
	// The import resolver's refusals (httpapi writeImportError).
	"import_history_limit": true, "import_unfinished_limit": true, "import_chunk_reused": true, "import_chunk_out_of_order": true,
	"import_manifest_changed": true, "import_manifest_incomplete": true, "import_resolution_stale": true,
	"import_target_already_assigned": true, "baby_uuid_collision": true, "command_id_reused": true, "import_page_limit": true,
	"family_storage_limit": true, "import_expired": true, "import_access_denied": true, "import_not_found": true,
}

// Route-specific capability headers (internal/capacity/workload.go sets the
// same; protocol.RequiredCapabilities is the rule).
var (
	capabilitiesSync         = []string{protocol.CapabilityRecordsV1}
	capabilitiesSyncTimer    = []string{protocol.CapabilityRecordsV1, protocol.CapabilityTimerCommandsV1}
	capabilitiesSnapshot     = []string{protocol.CapabilityRecordsV1, protocol.CapabilitySnapshotV1}
	capabilitiesLive         = []string{protocol.CapabilityRecordsV1, protocol.CapabilityLiveHintsV1}
	capabilitiesPushToken    = []string{protocol.CapabilityPushTokenV1}
	capabilitiesLiveActivity = []string{protocol.CapabilityLiveActivityPushV1}
	capabilitiesImport       = []string{protocol.CapabilityRecordsV1, protocol.CapabilityImportResolverV1}
	capabilitiesExport       = []string{protocol.CapabilityExportV1}
)

const (
	requestTimeout  = 10 * time.Second
	maxResponseBody = 4 << 20
	mcpProtocol     = "2025-11-25"
)

// client sends the generator's requests. One transport serves every device:
// HTTP/1.1 (the server sits behind Tailscale Serve in production), a large
// idle pool, and a 10 s deadline per request. Each request names the
// device's forwarded address and the configured Host, so the server sees
// one client per device (the generator's address is its trusted proxy).
type client struct {
	base, host string
	http       *http.Client
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          4096,
		MaxIdleConnsPerHost:   4096,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func newClient(base, host string) *client {
	return &client{base: strings.TrimRight(base, "/"), host: host,
		http: &http.Client{Transport: newTransport(), Timeout: requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// result is one request's classified answer.
type result struct {
	Outcome    string
	Status     int
	Code       string        // the error envelope's code, when there is one
	RetryAfter time.Duration // Retry-After, when the server sent one
	// SessionWait is how long a request sent with its device's session
	// (request.session) waited for the device's refresh: the client's
	// serialisation, which the caller moves its intended time by.
	SessionWait time.Duration
}

func (r result) ok() bool { return r.Outcome == OutcomeOK }

// lostRace reports a retryable lost race (Task 5): retried per Retry-After.
func (r result) lostRace() bool {
	return r.Outcome == OutcomeSecurityBusy || r.Outcome == OutcomeCredentialRotationBusy
}

// request is one HTTP request the client sends.
type request struct {
	method, path string
	device       *device // the forwarded address; nil sends none
	bearer       string
	// session, when set, sends the request with that device's current
	// access token, holding its session (shared) for the exchange, so a
	// refresh of the device waits for it and it waits for one in flight.
	// It must not be set by a caller that holds the session already.
	session      *device
	capabilities []string // X-Baby-Tracker-Capabilities; nil sends none
	body         any      // JSON-encoded; []byte is sent as is
	header       map[string]string
}

// do sends req and decodes a 200 answer into out (nil: discarded). A 204 is
// ok. Every other answer is classified; the body's error code is read
// within a bounded prefix and nothing else of it is kept.
func (c *client) do(ctx context.Context, req request, out any) result {
	if d := req.session; d != nil {
		release, waited := d.holdSession()
		defer release()
		req.bearer, req.session = d.access(), nil
		res := c.do(ctx, req, out)
		res.SessionWait = waited
		return res
	}
	var body io.Reader
	switch b := req.body.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return result{Outcome: OutcomeProtocol}
		}
		body = bytes.NewReader(raw)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, req.method, c.base+req.path, body)
	if err != nil {
		return result{Outcome: OutcomeProtocol}
	}
	c.decorate(httpRequest, req)
	response, err := c.http.Do(httpRequest)
	if err != nil {
		return result{Outcome: transportOutcome(ctx, err)}
	}
	defer response.Body.Close()
	return readResponse(ctx, response, out)
}

// decorate sets the headers every v1 request carries.
func (c *client) decorate(r *http.Request, req request) {
	r.Host = c.host
	r.Header.Set("X-Baby-Tracker-Protocol", "v1")
	if req.capabilities != nil {
		r.Header.Set("X-Baby-Tracker-Capabilities", strings.Join(req.capabilities, ","))
	}
	if req.body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if req.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+req.bearer)
	}
	if req.device != nil && req.device.ClientIP != "" {
		r.Header.Set("X-Forwarded-For", req.device.ClientIP)
	}
	for k, v := range req.header {
		r.Header.Set(k, v)
	}
}

func readResponse(ctx context.Context, response *http.Response, out any) result {
	res := result{Status: response.StatusCode}
	if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
		res.RetryAfter = time.Duration(seconds) * time.Second
	}
	limited := io.LimitReader(response.Body, maxResponseBody+1)
	switch {
	case response.StatusCode == http.StatusNoContent:
		_, _ = io.Copy(io.Discard, limited)
		res.Outcome = OutcomeOK
		return res
	case response.StatusCode >= 200 && response.StatusCode < 300:
		buf := getBody(response.ContentLength)
		defer putBody(buf)
		if _, err := buf.ReadFrom(limited); err != nil {
			res.Outcome = transportOutcome(ctx, err)
			return res
		}
		raw := buf.Bytes()
		if len(raw) > maxResponseBody {
			res.Outcome = OutcomeProtocol
			return res
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				res.Outcome = OutcomeProtocol
				return res
			}
			if v, ok := out.(validated); ok && !v.valid() {
				res.Outcome = OutcomeProtocol
				return res
			}
		}
		res.Outcome = OutcomeOK
		return res
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil && len(raw) == 0 {
		res.Outcome = transportOutcome(ctx, err)
		return res
	}
	res.Code = errorCode(raw)
	res.Outcome = classify(response.StatusCode, res.Code)
	return res
}

// validated is an answer type with rules beyond its JSON shape (pullPage):
// an answer that breaks them is a protocol outcome.
type validated interface{ valid() bool }

// bodies are the read buffers of 2xx answers, reused: a pull page is up to
// about 1 MiB, and reading each into a fresh, doubling slice was most of
// the generator's garbage. A decoded answer keeps no byte of its buffer
// (encoding/json copies strings and raw values).
var bodies = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// maxPooledBody bounds the buffers kept for reuse.
const maxPooledBody = 2 << 20

func getBody(contentLength int64) *bytes.Buffer {
	buf := bodies.Get().(*bytes.Buffer)
	buf.Reset()
	if contentLength > 0 && contentLength <= maxResponseBody {
		// ReadFrom wants bytes.MinRead free at the end before it sees EOF.
		buf.Grow(int(contentLength) + bytes.MinRead)
	}
	return buf
}

func putBody(buf *bytes.Buffer) {
	if buf.Cap() <= maxPooledBody {
		bodies.Put(buf)
	}
}

// errorCode is the v1 error envelope's code (or MCP's error.data.code).
func errorCode(raw []byte) string {
	var envelope struct {
		Error *struct {
			Code json.RawMessage `json:"code"`
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Error == nil {
		return ""
	}
	var code string
	if json.Unmarshal(envelope.Error.Code, &code) == nil && code != "" {
		return code
	}
	return envelope.Error.Data.Code
}

// classify maps a refused answer to its outcome class.
func classify(status int, code string) string {
	switch {
	case status == http.StatusServiceUnavailable && code == "server_busy":
		return OutcomeServerBusy
	case status == http.StatusServiceUnavailable && code == "not_ready":
		return OutcomeNotReady
	case status == http.StatusServiceUnavailable && code == "security_busy":
		return OutcomeSecurityBusy
	case status == http.StatusServiceUnavailable && code == "credential_rotation_busy":
		return OutcomeCredentialRotationBusy
	case status == http.StatusServiceUnavailable && code == "timeout":
		return OutcomeServerTimeout
	case status == http.StatusTooManyRequests:
		return OutcomeRateLimited
	case status == http.StatusUnauthorized && code == "access_expired":
		return OutcomeAccessExpired
	case status >= 400 && status < 500:
		if http4xxCodes[code] {
			return "http_4xx_" + code
		}
		return "http_4xx_other"
	case status >= 500:
		return OutcomeHTTP5xx
	}
	return OutcomeProtocol
}

// transportOutcome classes a failed exchange: the request's deadline (or the
// run's) is a timeout, anything else a transport failure.
func transportOutcome(ctx context.Context, err error) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || errors.As(err, &netErr) && netErr.Timeout() {
		return OutcomeTimeout
	}
	return OutcomeTransport
}

// syncBody is a POST /v1/sync body.
func syncBody(capabilities []string, cursor *protocol.Cursor, mutations []protocol.Mutation, pageToken *string) protocol.SyncRequest {
	if mutations == nil {
		mutations = []protocol.Mutation{}
	}
	return protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: capabilities},
		Cursor: cursor, Mutations: mutations, PageToken: pageToken}
}

// syncAs sends one POST /v1/sync as d with its session (request.session).
func (c *client) syncAs(ctx context.Context, d *device, body protocol.SyncRequest) (protocol.SyncResponse, result) {
	var response protocol.SyncResponse
	res := c.do(ctx, request{method: http.MethodPost, path: "/v1/sync", device: d, session: d,
		capabilities: body.Capabilities, body: body}, &response)
	return response, res
}

// sync sends one POST /v1/sync.
func (c *client) sync(ctx context.Context, d *device, bearer string, body protocol.SyncRequest) (protocol.SyncResponse, result) {
	var response protocol.SyncResponse
	res := c.do(ctx, request{method: http.MethodPost, path: "/v1/sync", device: d, bearer: bearer,
		capabilities: body.Capabilities, body: body}, &response)
	return response, res
}

// pull sends one pull page from cursor (nil: from the start) as d.
func (c *client) pull(ctx context.Context, d *device, cursor *protocol.Cursor) (protocol.SyncResponse, result) {
	return c.sync(ctx, d, d.access(), syncBody(capabilitiesSync, cursor, nil, nil))
}
