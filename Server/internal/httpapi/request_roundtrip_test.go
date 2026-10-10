package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
)

type stackReady struct{}

func (stackReady) Check(context.Context) error { return nil }

// stackNotReady is a node whose cached readiness refuses.
type stackNotReady struct{}

func (stackNotReady) Check(context.Context) error { return errors.New("readiness: not ready") }

// stack is the production sync and snapshot handlers over a harness, with
// the sync service's authority on a one-connection counted pool and the
// in-memory limiter.
type stack struct {
	h        *testkit.Harness
	wire     *testkit.Wire
	handler  http.Handler
	progress *countingProgress
	// logs is handler's log (Options.Logger); unlogged is the same
	// handler composed with no logger, the baseline a logged response is
	// compared with.
	logs     *syncBuffer
	unlogged http.Handler
}

// newLoggedStackHandlers composes options twice: with a logger writing to
// a fresh buffer (unless options names its own) and with none.
func newLoggedStackHandlers(options Options) (handler, unlogged http.Handler, logs *syncBuffer) {
	logs = &syncBuffer{}
	quiet := options
	quiet.Logger = nil
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	}
	return New(options), New(quiet), logs
}

// countingProgress is the production wake tracker, counting the requests
// that reported progress to it: the round-trip pins hold with it in place.
type countingProgress struct {
	tracker *wake.Tracker
	calls   atomic.Int32
}

func (p *countingProgress) Synced(familyID, deviceID string, acked, delivered uint64, at time.Time) {
	p.calls.Add(1)
	p.tracker.Synced(familyID, deviceID, acked, delivered, at)
}

// reported is the number of requests that reported progress so far.
func (s *stack) reported() int { return int(s.progress.calls.Load()) }

// stackConfig varies a stack: the pull-device policy, the authority's
// options (another clock or epoch), readiness, Live Activity push (the
// registration service, unregistration and a ready worker on the stack's
// counted authority) and any other change to the handler's options, made
// last.
type stackConfig struct {
	pullPolicy   limits.Policy
	authority    func(*authority.Options)
	readiness    Readiness
	liveActivity bool
	options      func(*Options)
}

func newStack(t *testing.T, h *testkit.Harness, pullPolicy limits.Policy) *stack {
	t.Helper()
	return newStackWith(t, h, stackConfig{pullPolicy: pullPolicy})
}

func newStackWith(t *testing.T, h *testkit.Harness, config stackConfig) *stack {
	t.Helper()
	pool, wire := testkit.CountedPool(t, h.Runtime)
	guard := h.AuthorityOn(t, pool)
	if config.authority != nil {
		guard = h.AuthorityOnWith(t, pool, config.authority)
	}
	progress := &countingProgress{tracker: wake.NewTracker(0)}
	service := testkit.SyncServiceWith(t, h, guard, progress)
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{9}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	if config.pullPolicy == (limits.Policy{}) {
		config.pullPolicy = limits.DefaultPolicies()[limits.ScopeSyncPullDevice]
	}
	if config.readiness == nil {
		config.readiness = stackReady{}
	}
	policies := limits.DefaultPolicies()
	options := Options{Readiness: config.readiness, InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch,
		CanonicalOrigin: h.Info.CanonicalOrigin, SyncLimits: &h.Limits, SessionService: h.Auth, SyncService: service, Limiter: limiter,
		SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount],
		SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily], SyncPullDevicePolicy: config.pullPolicy,
		SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount]}
	if config.liveActivity {
		options.LiveActivityService = liveactivity.NewService(guard, liveActivityTopic)
		options.LiveActivityUnregistration = liveactivity.NewUnregistration(guard)
		options.LiveActivityReady = func(context.Context) error { return nil }
	}
	if config.options != nil {
		config.options(&options)
	}
	handler, unlogged, logs := newLoggedStackHandlers(options)
	return &stack{h: h, wire: wire, progress: progress, handler: handler, unlogged: unlogged, logs: logs}
}

// server is the stack's GET /v1/server body.
func (s *stack) server(t *testing.T) string {
	t.Helper()
	rec := s.raw(http.MethodGet, "/v1/server", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/server: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// headers is a request's protocol headers for target: the subject's bearer
// token and the route's capabilities.
func (s *stack) headers(subject, target string) map[string]string {
	capabilities := "records-v1"
	if strings.HasPrefix(target, "/v1/sync/snapshot") {
		capabilities = "records-v1,snapshot-v1"
	}
	if strings.HasPrefix(target, "/v1/live-activities/") {
		capabilities = protocol.CapabilityLiveActivityPushV1
	}
	return map[string]string{"Authorization": "Bearer " + s.h.AccessToken(subject), "X-Baby-Tracker-Protocol": "v1",
		"X-Baby-Tracker-Capabilities": capabilities}
}

// raw sends body as given with headers (an empty value omits the header).
func (s *stack) raw(method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	return serveRaw(s.handler, method, target, body, headers)
}

// serveRaw is raw on any handler.
func serveRaw(handler http.Handler, method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for name, value := range headers {
		if value != "" {
			req.Header.Set(name, value)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func (s *stack) send(t *testing.T, subject, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	return s.raw(method, target, raw, s.headers(subject, target))
}

// trips is the round trips of one request, after warm has prepared its statements.
func (s *stack) trips(t *testing.T, label string, warm, measured func() *httptest.ResponseRecorder, status int) int {
	t.Helper()
	if warm != nil {
		warm()
	}
	s.wire.Reset()
	if rec := measured(); rec.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", label, rec.Code, status, rec.Body.String())
	}
	trips := s.wire.Trips()
	t.Logf("%s: %d round trips", label, trips)
	return trips
}

// assertRefusalRollsBack is Task 4's m1 on the wire: a refused request
// ends its transaction with exactly one rollback and never commits.
func (s *stack) assertRefusalRollsBack(t *testing.T, label string) {
	t.Helper()
	statements := s.wire.Statements()
	rollbacks, commits := 0, 0
	for _, statement := range statements {
		switch strings.ToLower(strings.TrimSpace(statement)) {
		case "rollback":
			rollbacks++
		case "commit":
			commits++
		}
	}
	if rollbacks != 1 || commits != 0 {
		t.Errorf("%s: %d rollbacks and %d commits, want 1 and 0: %q", label, rollbacks, commits, statements)
	}
}

func TestSyncHTTPRoundTrips(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	s := newStack(t, h, limits.DefaultPolicies()[limits.ScopeSyncPullDevice])
	pull := func() *httptest.ResponseRecorder {
		return s.send(t, "owner", http.MethodPost, "/v1/sync", h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil))
	}
	if got := s.trips(t, "pull-only", pull, pull, http.StatusOK); got != 3 {
		t.Errorf("pull-only: %d round trips, want 3: %q", got, s.wire.Statements())
	}
	if s.reported() != 2 {
		t.Errorf("pull-only: %d requests reported progress, want the warm-up and the measured pull", s.reported())
	}
	for i, size := range []int{1, 20, 100} {
		batch := func(n int) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				baby := fmt.Sprintf("10000000-0000-4000-8000-%012d", 900+10*i+n)
				caretest.Seed(t, h, caretest.Baby(baby))
				request := h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil)
				request.Mutations = caretest.MixedBatch(baby, size)
				return s.send(t, "owner", http.MethodPost, "/v1/sync", request)
			}
		}
		if got := s.trips(t, fmt.Sprintf("batch of %d", size), batch(0), batch(1), http.StatusOK); got != 4 {
			t.Errorf("batch of %d with a one-page pull: %d round trips, want 4: %q", size, got, s.wire.Statements())
		}
		if want := 4 + 2*i; s.reported() != want {
			t.Errorf("batch of %d: %d requests reported progress, want %d", size, s.reported(), want)
		}
	}
	reportedBeforeRefusals := s.reported()
	bad := func() *httptest.ResponseRecorder {
		request := h.SyncRequest(nil, nil)
		request.Mutations = nil // invalid_request, held until the session is known good
		return s.send(t, "owner", http.MethodPost, "/v1/sync", request)
	}
	if got := s.trips(t, "held 400", bad, bad, http.StatusBadRequest); got != 2 {
		t.Errorf("a held refusal: %d round trips, want 2", got)
	}
	s.assertRefusalRollsBack(t, "held 400")
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	revoked := func() *httptest.ResponseRecorder {
		return s.send(t, "caregiver", http.MethodPost, "/v1/sync", h.SyncRequest(nil, nil))
	}
	if got := s.trips(t, "revoked", revoked, revoked, http.StatusUnauthorized); got != 2 {
		t.Errorf("a revoked session: %d round trips, want 2", got)
	}
	s.assertRefusalRollsBack(t, "revoked")
	if s.reported() != reportedBeforeRefusals {
		t.Errorf("refused requests reported progress: %d, want %d", s.reported(), reportedBeforeRefusals)
	}
	limited := newStack(t, h, limits.Policy{Count: 1, Window: time.Minute})
	pullLimited := func() *httptest.ResponseRecorder {
		return limited.send(t, "owner", http.MethodPost, "/v1/sync", h.SyncRequest(h.Cursor(h.FamilySequence(t)), nil))
	}
	if got := limited.trips(t, "rate limited", pullLimited, pullLimited, http.StatusTooManyRequests); got != 2 {
		t.Errorf("a rate-limited pull: %d round trips, want 2", got)
	}
	limited.assertRefusalRollsBack(t, "rate limited")
}

// 13 rows (a baby and 12 feeds) at page size 3 page as 3, 3, 3, 3, 1: a
// first page and three continuations with more, then a last continuation.
func TestPagedHTTPRoundTrips(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 3
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	for n := range 12 {
		caretest.Seed(t, h, caretest.Feed(caretest.BabyA, n))
	}
	s := newStack(t, h, limits.DefaultPolicies()[limits.ScopeSyncPullDevice])
	var token *string
	first := func() *httptest.ResponseRecorder {
		rec := s.send(t, "owner", http.MethodPost, "/v1/sync", h.SyncRequest(h.Cursor(0), nil))
		token = decodedToken(t, rec)
		return rec
	}
	next := func() *httptest.ResponseRecorder {
		rec := s.send(t, "owner", http.MethodPost, "/v1/sync", h.SyncRequest(nil, token))
		token = decodedToken(t, rec)
		return rec
	}
	first() // prepares the first-page statements
	if got := s.trips(t, "first page with more", nil, first, http.StatusOK); got != 4 || token == nil {
		t.Errorf("first pull page with a token: %d (token %v), want 4", got, token) // page 1
	}
	next() // page 2: prepares the continuation statements
	if got := s.trips(t, "continuation with more", nil, next, http.StatusOK); got != 4 || token == nil {
		t.Errorf("continuation with more: %d, want 4", got) // page 3
	}
	next() // page 4
	if got := s.trips(t, "last continuation", nil, next, http.StatusOK); got != 3 || token != nil {
		t.Errorf("last continuation: %d round trips (token %v), want 3", got, token) // page 5
	}
	// The first pages presented cursor 0 and the continuations none, so
	// only the final page reports: its high water.
	if s.reported() != 1 {
		t.Errorf("paged pull: %d requests reported progress, want the final page", s.reported())
	}
	var snapshot *string
	snap := func() *httptest.ResponseRecorder {
		target := "/v1/sync/snapshot"
		if snapshot != nil {
			target += "?page_token=" + url.QueryEscape(*snapshot)
		}
		rec := s.send(t, "owner", http.MethodGet, target, nil)
		snapshot = decodedToken(t, rec)
		return rec
	}
	start := func() *httptest.ResponseRecorder { snapshot = nil; return snap() }
	start() // snapshot 1 (of the account's 2): prepares the capture statements
	if got := s.trips(t, "snapshot first page", nil, start, http.StatusOK); got != 4 || snapshot == nil {
		t.Errorf("first snapshot page: %d, want 4", got) // snapshot 2, page 1
	}
	snap() // page 2: prepares the continuation statements
	if got := s.trips(t, "snapshot continuation with more", nil, snap, http.StatusOK); got != 4 || snapshot == nil {
		t.Errorf("snapshot continuation with more: %d, want 4", got) // page 3
	}
	snap() // page 4
	if got := s.trips(t, "last snapshot continuation", nil, snap, http.StatusOK); got != 3 || snapshot != nil {
		t.Errorf("last snapshot continuation: %d (token %v), want 3", got, snapshot) // page 5
	}
	if s.reported() != 2 {
		t.Errorf("paged snapshots: %d requests reported progress, want the pull's and the snapshot's final pages", s.reported())
	}
}

// TestPushTokenRouteRoundTrips pins the push-token route on a counted pool:
// a PUT or a DELETE is one guarded transaction, BEGIN+authority (one
// pipelined round trip), the upsert or delete, COMMIT; a refusal after
// authentication is BEGIN+authority and ROLLBACK.
func TestPushTokenRouteRoundTrips(t *testing.T) {
	h := testkit.New(t)
	s := newPushStack(t, h, pushConfig{})
	token := bytes.Repeat([]byte{0x3c}, 32)
	put := func() *httptest.ResponseRecorder { return s.putToken("owner", token, "sandbox") }
	if got := s.trips(t, "PUT", put, put, http.StatusNoContent); got != 3 {
		t.Errorf("PUT: %d round trips, want 3: %q", got, s.wire.Statements())
	}
	del := func() *httptest.ResponseRecorder { return s.deleteToken("owner") }
	if got := s.trips(t, "DELETE", del, del, http.StatusNoContent); got != 3 {
		t.Errorf("DELETE: %d round trips, want 3: %q", got, s.wire.Statements())
	}
	refused := func() *httptest.ResponseRecorder { return s.putToken("owner", token[:15], "sandbox") }
	if got := s.trips(t, "refused PUT", put, refused, http.StatusBadRequest); got != 2 {
		t.Errorf("a refused PUT: %d round trips, want 2: %q", got, s.wire.Statements())
	}
	s.assertRefusalRollsBack(t, "refused PUT")
}

// decodedToken is the response's next_page_token, nil on a final page.
func decodedToken(t *testing.T, rec *httptest.ResponseRecorder) *string {
	t.Helper()
	var page struct {
		NextPageToken *string `json:"next_page_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("%s: %v", rec.Body.String(), err)
	}
	return page.NextPageToken
}

// syncPrecedenceRow is one row of the sync and snapshot refusal table.
type syncPrecedenceRow struct {
	name    string
	on      *stack
	method  string
	target  string
	body    []byte
	headers map[string]string
	status  int
	code    string
	trips   int // after the collapse: 0 before any database work, else at most 2
}

// syncPrecedenceRows is the sync and snapshot refusal table over its
// stacks, with every budget a row depends on already spent.
func syncPrecedenceRows(t *testing.T) []syncPrecedenceRow {
	t.Helper()
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	h.AddMember(t, "viewer", "viewer")
	removed := h.AddMember(t, "removed", "caregiver")
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), removed.AccountID); err != nil {
		t.Fatal(err)
	}
	s := newStack(t, h, limits.DefaultPolicies()[limits.ScopeSyncPullDevice])
	unrefreshed := testkit.NewWithUnrefreshedCaregiver(t)
	u := newStack(t, unrefreshed, limits.DefaultPolicies()[limits.ScopeSyncPullDevice])
	// The session refused by its own state, each on a stack whose authority
	// sees it so: an expired access token, another recovery epoch, a
	// lineage past its reauthentication deadline.
	expired := newStackWith(t, h, stackConfig{authority: func(o *authority.Options) { o.Now = func() time.Time { return h.Now.Add(time.Hour) } }})
	epoch := newStackWith(t, h, stackConfig{authority: func(o *authority.Options) { o.RecoveryEpoch++ }})
	deadline := newStackWith(t, h, stackConfig{authority: func(o *authority.Options) { o.Now = func() time.Time { return h.Now.Add(400 * 24 * time.Hour) } }})
	notReady := newStackWith(t, h, stackConfig{readiness: stackNotReady{}})
	// Family policy refuses a live family session: a family whose status
	// left active (membership_revoked, 403) and one whose audit chain
	// failed (scope_failed, a retryable 503). Each also has a stack whose
	// pull limit the owner already spent: policy is decided before Charge.
	policyStacks := func(state string) (*stack, *stack) {
		ph := testkit.New(t)
		ps := newStack(t, ph, limits.Policy{})
		limited := newStack(t, ph, limits.Policy{Count: 1, Window: time.Minute})
		for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
			if rec := limited.send(t, "owner", http.MethodPost, "/v1/sync", ph.SyncRequest(nil, nil)); rec.Code != want {
				t.Fatalf("%s: limited pull %d: %d %s", state, i, rec.Code, rec.Body.String())
			}
		}
		if rec := ps.send(t, "owner", http.MethodPost, "/v1/sync", ph.SyncRequest(nil, nil)); rec.Code != http.StatusOK {
			t.Fatalf("%s: warm pull: %d %s", state, rec.Code, rec.Body.String())
		}
		ph.AuthorityExec(t, `UPDATE families SET `+state+` WHERE id=$1`, ph.FamilyID)
		return ps, limited
	}
	pending, pendingLimited := policyStacks("status='deletion_pending'")
	failed, failedLimited := policyStacks("chain_failed=true")
	for _, warm := range []*stack{s, u, expired, epoch, deadline} { // prepares the authority and pull statements
		warm.send(t, "owner", http.MethodPost, "/v1/sync", warm.h.SyncRequest(nil, nil))
	}
	encode := func(request protocol.SyncRequest) []byte {
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	pull := encode(h.SyncRequest(nil, nil))
	malformed := []byte(`{"protocol_version":`)
	tooMany := h.SyncRequest(nil, nil)
	for n := range h.Limits.SyncBatchSize + 1 {
		tooMany.Mutations = append(tooMany.Mutations, caretest.Feed(caretest.BabyA, n))
	}
	foreign := h.SyncRequest(h.Cursor(0), nil)
	foreign.Cursor.ServerInstanceID = "50000000-0000-4000-8000-0000000000ff"
	viewerBatch := h.SyncRequest(nil, nil)
	viewerBatch.Mutations = []protocol.Mutation{caretest.Feed(caretest.BabyA, 1)}
	with := func(headers map[string]string, name, value string) map[string]string {
		out := map[string]string{}
		for k, v := range headers {
			out[k] = v
		}
		out[name] = value
		return out
	}
	const syncRoute, snapshotRoute = "/v1/sync", "/v1/sync/snapshot"
	return []syncPrecedenceRow{
		{"no credential, malformed body", s, http.MethodPost, syncRoute, malformed, with(s.headers("owner", syncRoute), "Authorization", ""), 401, "access_expired", 0},
		{"malformed bearer", s, http.MethodPost, syncRoute, pull, with(s.headers("owner", syncRoute), "Authorization", "Bearer not-a-token"), 401, "session_revoked", 0},
		{"removed caregiver, malformed body", s, http.MethodPost, syncRoute, malformed, s.headers("removed", syncRoute), 401, "session_revoked", 2},
		{"removed caregiver, bad snapshot query", s, http.MethodGet, snapshotRoute + "?page_token=", nil, s.headers("removed", snapshotRoute), 401, "session_revoked", 2},
		{"unsupported protocol header", s, http.MethodPost, syncRoute, pull, with(s.headers("owner", syncRoute), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol", 2},
		{"body over the limit", s, http.MethodPost, syncRoute, append(pull, bytes.Repeat([]byte(" "), h.Limits.SyncBodyBytes)...), s.headers("owner", syncRoute), 413, "body_too_large", 2},
		{"batch over the limit", s, http.MethodPost, syncRoute, encode(tooMany), s.headers("owner", syncRoute), 400, "batch_limit", 2},
		{"bad snapshot query", s, http.MethodGet, snapshotRoute + "?page_token=", nil, s.headers("owner", snapshotRoute), 400, "invalid_request", 2},
		{"cursor of another instance", s, http.MethodPost, syncRoute, encode(foreign), s.headers("owner", syncRoute), 409, "snapshot_required", 2},
		{"viewer's mutation", s, http.MethodPost, syncRoute, encode(viewerBatch), s.headers("viewer", syncRoute), 403, "read_only_caregiver", 2},
		{"unrefreshed account session, malformed body", u, http.MethodPost, syncRoute, malformed, u.headers("caregiver", syncRoute), 401, "access_expired", 2},
		{"unrefreshed account session, snapshot", u, http.MethodGet, snapshotRoute, nil, u.headers("caregiver", snapshotRoute), 401, "access_expired", 2},
		{"expired access token, malformed body", expired, http.MethodPost, syncRoute, malformed, expired.headers("owner", syncRoute), 401, "access_expired", 2},
		{"another recovery epoch, malformed body", epoch, http.MethodPost, syncRoute, malformed, epoch.headers("owner", syncRoute), 401, "epoch_changed", 2},
		{"past the reauthentication deadline, malformed body", deadline, http.MethodPost, syncRoute, malformed, deadline.headers("owner", syncRoute), 401, "reauthentication_required", 2},
		{"node not ready, malformed body", notReady, http.MethodPost, syncRoute, malformed, notReady.headers("owner", syncRoute), 503, "service_unavailable", 0},
		{"snapshot with a bad header and a bad query", s, http.MethodGet, snapshotRoute + "?page_token=", nil, with(s.headers("owner", snapshotRoute), "X-Baby-Tracker-Capabilities", "records-v1"), 409, "unsupported_capability", 2},
		// A held header or body refusal ranks before family policy (the
		// legacy handler validated the body before its family read), and
		// family policy before the rate limit.
		{"family not active, malformed body", pending, http.MethodPost, syncRoute, malformed, pending.headers("owner", syncRoute), 400, "invalid_request", 2},
		{"family not active, unsupported protocol header", pending, http.MethodPost, syncRoute, pull, with(pending.headers("owner", syncRoute), "X-Baby-Tracker-Protocol", "v2"), 400, "unsupported_protocol", 2},
		{"family not active, bad snapshot query", pending, http.MethodGet, snapshotRoute + "?page_token=", nil, pending.headers("owner", snapshotRoute), 400, "invalid_request", 2},
		{"family not active", pending, http.MethodPost, syncRoute, pull, pending.headers("owner", syncRoute), 403, "membership_revoked", 2},
		{"family not active, snapshot", pending, http.MethodGet, snapshotRoute, nil, pending.headers("owner", snapshotRoute), 403, "membership_revoked", 2},
		{"family not active, pull limit spent", pendingLimited, http.MethodPost, syncRoute, pull, pendingLimited.headers("owner", syncRoute), 403, "membership_revoked", 2},
		{"family chain failed, malformed body", failed, http.MethodPost, syncRoute, malformed, failed.headers("owner", syncRoute), 400, "invalid_request", 2},
		{"family chain failed", failed, http.MethodPost, syncRoute, pull, failed.headers("owner", syncRoute), 503, "service_unavailable", 2},
		{"family chain failed, pull limit spent", failedLimited, http.MethodPost, syncRoute, pull, failedLimited.headers("owner", syncRoute), 503, "service_unavailable", 2},
	}
}

// TestSyncErrorPrecedenceIsUnchanged pins the legacy refusal order on the
// wire (written green against the handler that authenticated, read the
// family and charged in separate steps): a bad credential answers before
// any header or body refusal, those before family policy, and family
// policy before the in-transaction refusals. Every refusal costs at most
// the one transaction's two round trips (none before any database work)
// and ends in one rollback.
func TestSyncErrorPrecedenceIsUnchanged(t *testing.T) {
	for _, c := range syncPrecedenceRows(t) {
		t.Run(c.name, func(t *testing.T) {
			c.on.wire.Reset()
			rec := c.on.raw(c.method, c.target, c.body, c.headers)
			var response protocol.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != c.status || string(response.Error.Code) != c.code {
				t.Fatalf("status %d code %q, want %d %q: %s", rec.Code, response.Error.Code, c.status, c.code, rec.Body.String())
			}
			trips := c.on.wire.Trips()
			t.Logf("%s: %d round trips", c.name, trips)
			if trips > c.trips {
				t.Errorf("%d round trips, want at most %d: %q", trips, c.trips, c.on.wire.Statements())
			}
			if c.trips != 0 {
				c.on.assertRefusalRollsBack(t, c.name)
			}
		})
	}
}
