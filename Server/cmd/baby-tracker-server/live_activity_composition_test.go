package main

// The real push composition, end to end, in one process: the production
// `configureLiveActivityWorkerWithWait` (role-separated worker pool, schema and
// budget checks, readiness guard, snapshot guard, supervision, capability
// advertisement) behind the production HTTP router, on a disposable database
// with every migration applied. Phones speak authenticated HTTP — sync,
// registration, family and account commands — and the only substitute is
// Apple's transport, which records what it would have sent for synthetic
// tokens. Nothing here reads a token, a key or a family from production.
//
// Synchronisation is by condition, never by elapsed time: a send is awaited on
// the recording transport's channel, and "nothing else was sent" is asserted
// only after the queue is provably idle (no live lease, no due dirty work, no
// unswept family sequence).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/account"
	"babytracker/server/internal/config"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5/pgxpool"
)

const compositionBaby = "10000000-0000-4000-8000-000000000001"

// deliveryWindow bounds one step and is the promptness this suite proves:
// one-second polls, two runners, a send bounded at five seconds. A stale
// lease is released as soon as its holder returns (e4e01a5), so no step waits
// for a 30-second lease to lapse.
const deliveryWindow = 10 * time.Second

// revocationWindow bounds how long a registration whose authority is gone
// stays unrevoked: the worker's authority sweep runs once a minute. Until it
// runs, the dispatch check refuses every send for it; nothing may arrive.
const revocationWindow = 90 * time.Second

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

type compositionLimiter struct{}

func (compositionLimiter) Take(context.Context, string, string, limits.Policy) (limits.Decision, error) {
	return limits.Decision{Allowed: true}, nil
}
func (compositionLimiter) TakeMany(context.Context, []limits.Charge) (limits.Decision, error) {
	return limits.Decision{Allowed: true}, nil
}

// recordingTransport stands in for Apple. Every request is kept and announced;
// `respond` scripts the outcome (accepted unless a test says otherwise). It
// records the worker's PushRequest before APNs encoding and request
// validation: the protocol harness (LiveActivityPushE2ETests) is what runs the
// production APNs transport over these requests.
type recordingTransport struct {
	mu      sync.Mutex
	sent    []liveactivity.PushRequest
	arrived chan liveactivity.PushRequest
	respond func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error)
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{arrived: make(chan liveactivity.PushRequest, 256)}
}

func (r *recordingTransport) Send(ctx context.Context, request liveactivity.PushRequest) (liveactivity.PushResult, error) {
	r.mu.Lock()
	r.sent = append(r.sent, request)
	respond := r.respond
	r.mu.Unlock()
	r.arrived <- request
	if respond != nil {
		return respond(ctx, request)
	}
	return liveactivity.PushResult{HTTPStatus: 200, Classification: liveactivity.Accepted}, nil
}

func (r *recordingTransport) script(respond func(context.Context, liveactivity.PushRequest) (liveactivity.PushResult, error)) {
	r.mu.Lock()
	r.respond = respond
	r.mu.Unlock()
}

func (r *recordingTransport) all() []liveactivity.PushRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]liveactivity.PushRequest(nil), r.sent...)
}

type pushComposition struct {
	t         *testing.T
	h         *testkit.Harness
	transport *recordingTransport
	cfg       config.Config
	base      httpapi.Options
	options   httpapi.Options
	handler   atomic.Pointer[http.Handler]
	server    *httptest.Server
	shutdown  func()
	// inspect is a superuser view of this test's database, for the idle check
	// and for evidence only; nothing the product does goes through it.
	inspect *pgxpool.Pool
	// seen is how many recorded sends earlier steps have accounted for.
	seen int
}

func newPushComposition(t *testing.T, enabled bool) *pushComposition {
	t.Helper()
	h := testkit.New(t)
	// The services' clock becomes the wall clock the worker reads, so feed
	// break deadlines, timer-edit bounds and APNs timestamps agree. Sessions
	// minted at the harness's fixed instant are renewed at the new one.
	h.Now = time.Now().UTC().Truncate(time.Second)
	h.Owner = h.Refresh("owner")
	h.Caregiver = h.Refresh("caregiver")
	head, err := h.Security.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Migrator.Exec(t.Context(), `SELECT app.update_server_state($1,$2,$3,$4,$5)`, h.Info.InstanceID, h.Info.RecoveryEpoch, head.Generation, head.Revision, head.EventHash[:]); err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	resolver, err := limits.NewClientIPResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &pushComposition{t: t, h: h, transport: newRecordingTransport()}
	inspection := h.Admin.Config().Copy()
	inspection.ConnConfig.Database = h.Migrator.Config().ConnConfig.Database
	inspection.MaxConns = 2
	if c.inspect, err = pgxpool.NewWithConfig(t.Context(), inspection); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.inspect.Close)
	c.base = httpapi.Options{
		InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SyncLimits: &h.Limits,
		AppleAuthorizationMode: "identity_token_only", ChallengeService: h.Identity, SessionService: h.Auth,
		FamilyService: h.Family, SyncService: h.Service, ImportService: h.Importer, AccountService: &account.Service{Security: h.Security},
		Logger:    slog.New(slog.NewTextHandler(testLogWriter{t}, nil)),
		Readiness: readinessFunc(func(context.Context) error { return nil }), Limiter: compositionLimiter{}, ClientIPResolver: resolver,
		SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount],
		SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily], SyncPullDevicePolicy: policies[limits.ScopeSyncPullDevice],
		SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount], ChallengePolicy: policies[limits.ScopeAuthChallengeIP],
		ExchangePolicy: policies[limits.ScopeAuthExchangeIP], RefreshIPPolicy: policies[limits.ScopeAuthRefreshIP],
		RefreshAccountPolicy: policies[limits.ScopeAuthRefreshAccount], RefreshDevicePolicy: policies[limits.ScopeAuthRefreshDevice],
	}
	c.cfg = config.Config{Mode: config.ModeSelfHosted}
	if enabled {
		c.cfg.LiveActivity = config.LiveActivityConfig{Enabled: true, BundleID: "com.example.babytracker", WorkerDatabaseURL: h.LiveActivity.Config().ConnString()}
	}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*c.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(c.server.Close)
	c.startWorker()
	t.Cleanup(func() { c.shutdown() })
	return c
}

// startWorker runs the production activation over the same database and the
// same recording transport, then serves a router built from what it returned.
func (c *pushComposition) startWorker() {
	c.t.Helper()
	options := c.base
	shutdown, err := configureLiveActivityWorkerWithWait(c.t.Context(), c.cfg, c.h.Security, c.h.Runtime, &options,
		newDatabasePoolBudget(c.h.Runtime), c.transport, waitLiveActivityRetry)
	if err != nil {
		c.t.Fatalf("activate live activity push: %v", err)
	}
	c.options, c.shutdown = options, shutdown
	handler := httpapi.New(options)
	c.handler.Store(&handler)
}

// restartWorker is a process restart of the push runtime: the old worker is
// cancelled and joined, `between` runs with no worker at all, and a fresh
// activation (new pool, new worker, initial pass) takes over the durable queue.
func (c *pushComposition) restartWorker(between func()) {
	c.t.Helper()
	c.shutdown()
	if state, ok := c.options.LiveActivityReadiness.(*liveActivityRuntime); !ok || state.Check(c.t.Context()) == nil {
		c.t.Fatal("stopped worker still reports ready")
	}
	between()
	c.startWorker()
}

// quiesce returns once nothing is in flight or due: no live lease, no dirty
// job whose time has come, and no committed family sequence the worker's
// sweep has not yet turned into jobs. Future deadlines (a break's expiry, a
// retry) are not "due".
func (c *pushComposition) quiesce() {
	c.t.Helper()
	deadline := time.Now().Add(deliveryWindow)
	for {
		var busy bool
		err := c.inspect.QueryRow(c.t.Context(), `SELECT
  EXISTS(SELECT FROM live_activity_delivery d WHERE d.lease_id IS NOT NULL AND d.lease_deadline>clock_timestamp())
  OR EXISTS(SELECT FROM live_activity_delivery d JOIN live_activity_registration_index r USING(registration_id)
            WHERE NOT r.revoked AND r.authority_expires_at>clock_timestamp() AND d.dirty_version>d.completed_version AND d.not_before<=clock_timestamp())
  OR EXISTS(SELECT FROM live_activity_registration_index r JOIN families f ON f.id=r.family_id
            JOIN server_state s ON s.singleton AND s.instance_id=r.instance_id AND s.recovery_epoch=r.recovery_epoch
            LEFT JOIN live_activity_family_dirty fd ON fd.family_id=r.family_id
            WHERE NOT r.revoked AND f.next_sequence>coalesce(fd.journal_sequence,-1))`).Scan(&busy)
		if err != nil {
			c.t.Fatal(err)
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			rows, _ := c.inspect.Query(c.t.Context(), `SELECT d.kind||' dirty='||d.dirty_version||' done='||d.completed_version||' due='||(d.not_before<=clock_timestamp())||' lease='||(d.lease_id IS NOT NULL)||' revoked='||r.revoked||' reason='||coalesce(d.last_reason,'-') FROM live_activity_delivery d JOIN live_activity_registration_index r USING(registration_id)
  UNION ALL SELECT 'family next='||f.next_sequence||' swept='||coalesce(fd.journal_sequence,-1) FROM families f LEFT JOIN live_activity_family_dirty fd ON fd.family_id=f.id`)
			var state []string
			for rows != nil && rows.Next() {
				var line string
				_ = rows.Scan(&line)
				state = append(state, line)
			}
			running := false
			if r, ok := c.options.LiveActivityReadiness.(*liveActivityRuntime); ok {
				running = r.running.Load()
			}
			var dump strings.Builder
			_ = pprof.Lookup("goroutine").WriteTo(&dump, 1)
			c.t.Fatalf("live activity queue did not become idle (worker running=%t): %v\n%s", running, state, dump.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// expect waits for exactly `want` new sends, then for the queue to go idle,
// and fails if anything more was sent. Sends are returned sorted by token so
// steps can assert per phone regardless of the worker's two-runner order.
func (c *pushComposition) expect(want int, why string) []liveactivity.PushRequest {
	c.t.Helper()
	return c.expectWithin(want, deliveryWindow, why)
}

func (c *pushComposition) expectWithin(want int, window time.Duration, why string) []liveactivity.PushRequest {
	c.t.Helper()
	began := time.Now()
	timeout := time.NewTimer(window)
	defer timeout.Stop()
	for received := 0; received < want; received++ {
		select {
		case <-c.transport.arrived:
		case <-timeout.C:
			c.t.Fatalf("%s: %d of %d sends arrived; all sends=%s", why, received, want, describeSends(c.transport.all()[c.seen:]))
		}
	}
	c.quiesce()
	// Drain announcements of anything sent while quiescing, then count.
	c.drainArrived()
	all := c.transport.all()
	fresh := append([]liveactivity.PushRequest(nil), all[c.seen:]...)
	if len(fresh) != want {
		c.t.Fatalf("%s: want %d sends, got %d: %s", why, want, len(fresh), describeSends(fresh))
	}
	c.seen = len(all)
	c.t.Logf("%s: delivered and idle after %v", why, time.Since(began).Round(100*time.Millisecond))
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Token < fresh[j].Token })
	return fresh
}

// drainArrived discards the announcements of sends already recorded, so the
// next wait counts only sends made after this point.
func (c *pushComposition) drainArrived() {
	for {
		select {
		case <-c.transport.arrived:
		default:
			return
		}
	}
}

// accountForAll treats every send so far as accounted for: once the queue is
// idle, their announcements are drained and `seen` moves past them, so the
// next expect counts only what happens after this point.
func (c *pushComposition) accountForAll() {
	c.t.Helper()
	c.quiesce()
	c.drainArrived()
	c.seen = len(c.transport.all())
}

func describeSends(sends []liveactivity.PushRequest) string {
	parts := make([]string, 0, len(sends))
	for _, s := range sends {
		phase, kind, generation := "", "", ""
		if s.Content.Remote != nil {
			generation = s.Content.Remote.GenerationID
		}
		phase, kind = s.Content.Phase, s.Content.ActivityKind
		parts = append(parts, fmt.Sprintf("{%s %s token=%s kind=%s phase=%s gen=%s}", s.Event, s.Attributes.KindRaw, s.Token, kind, phase, generation))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// pushPhone is one device's session, speaking the routes a phone speaks.
type pushPhone struct {
	c            *pushComposition
	subject      string
	token        string
	refresh      string
	installation string
	version      uint64
	cursor       *protocol.Cursor
}

func (c *pushComposition) phone(subject, installation string) *pushPhone {
	return &pushPhone{c: c, subject: subject, token: c.h.AccessToken(subject), refresh: c.h.RefreshToken(subject), installation: installation}
}

func (p *pushPhone) do(method, path string, body any, capabilities string) (int, []byte) {
	p.c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			p.c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(p.c.t.Context(), method, p.c.server.URL+path, reader)
	if err != nil {
		p.c.t.Fatal(err)
	}
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Baby-Tracker-Protocol", "v1")
	if capabilities != "" {
		request.Header.Set("X-Baby-Tracker-Capabilities", capabilities)
	}
	if path == "/v1/sync" {
		for _, header := range []string{"Growth", "Supplements", "Timed-Bottle", "Feeding-Session"} {
			request.Header.Set("X-Baby-Tracker-"+header, "v1")
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		p.c.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, raw
}

func (p *pushPhone) registerDevice(pushToken string, inventory *protocol.LiveActivityForegroundInventory) (int, protocol.LiveActivityRegistrationReceipt) {
	p.c.t.Helper()
	p.version++
	request := protocol.LiveActivityDeviceRegistration{
		RequestMeta:    protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}},
		InstallationID: p.installation, RegistrationVersion: p.version, APNsEnvironment: "sandbox", PayloadVersion: 1,
		PushToStartToken: pushToken, FeedBreakMinutes: 5, ForegroundInventory: inventory,
	}
	status, raw := p.do(http.MethodPut, "/v1/live-activities/device", request, protocol.CapabilityLiveActivityPushV1)
	var receipt protocol.LiveActivityRegistrationReceipt
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &receipt); err != nil {
			p.c.t.Fatal(err)
		}
	}
	return status, receipt
}

func (p *pushPhone) mustRegisterDevice(pushToken string, inventory *protocol.LiveActivityForegroundInventory) protocol.LiveActivityRegistrationReceipt {
	p.c.t.Helper()
	status, receipt := p.registerDevice(pushToken, inventory)
	if status != http.StatusOK {
		p.c.t.Fatalf("%s device registration status=%d", p.subject, status)
	}
	return receipt
}

// activity is what a phone registers for one card it holds.
type activity struct {
	id, token, kind string
	version         uint64
	generation      *string
	timer, session  *string
	state           string
}

func (p *pushPhone) registerActivity(a activity) (int, protocol.LiveActivityRegistrationReceipt) {
	p.c.t.Helper()
	state := a.state
	if state == "" {
		state = "active"
	}
	request := protocol.LiveActivityTokenRegistration{
		RequestMeta:    protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}},
		InstallationID: p.installation, ActivityID: a.id, Token: a.token, TokenVersion: a.version,
		Scope:             protocol.LiveActivityScope{ServerInstanceID: p.c.h.Info.InstanceID, RecoveryEpoch: p.c.h.Info.RecoveryEpoch, FamilyID: p.c.h.FamilyID, BabyID: compositionBaby, KindRaw: a.kind},
		KnownGenerationID: a.generation, TimerID: a.timer, SessionID: a.session, State: state,
	}
	status, raw := p.do(http.MethodPut, "/v1/live-activities/activity", request, protocol.CapabilityLiveActivityPushV1)
	var receipt protocol.LiveActivityRegistrationReceipt
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &receipt); err != nil {
			p.c.t.Fatal(err)
		}
	}
	return status, receipt
}

func (p *pushPhone) mustRegisterActivity(a activity) protocol.LiveActivityRegistrationReceipt {
	p.c.t.Helper()
	status, receipt := p.registerActivity(a)
	if status != http.StatusOK {
		p.c.t.Fatalf("%s activity registration status=%d", p.subject, status)
	}
	return receipt
}

func (p *pushPhone) sync(mutations ...protocol.Mutation) (int, protocol.SyncResponse) {
	p.c.t.Helper()
	status, raw := p.syncRaw(mutations...)
	var response protocol.SyncResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &response); err != nil {
			p.c.t.Fatal(err)
		}
		cursor := response.HighWater
		p.cursor = &cursor
	}
	return status, response
}

// syncRaw sends one sync request and returns the status and the body as the
// server wrote it, refusal envelopes included.
func (p *pushPhone) syncRaw(mutations ...protocol.Mutation) (int, []byte) {
	p.c.t.Helper()
	if mutations == nil {
		mutations = []protocol.Mutation{}
	}
	capabilities := []string{protocol.CapabilityRecordsV1}
	for _, m := range mutations {
		if m.EntityKind == "timer" {
			capabilities = append(capabilities, protocol.CapabilityTimerCommandsV1)
			break
		}
	}
	request := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: capabilities}, Cursor: p.cursor, Mutations: mutations}
	return p.do(http.MethodPost, "/v1/sync", request, strings.Join(capabilities, ","))
}

// write commits mutations and requires every one to be applied.
func (p *pushPhone) write(mutations ...protocol.Mutation) {
	p.c.t.Helper()
	status, response := p.sync(mutations...)
	if status != http.StatusOK || len(response.Outcomes) != len(mutations) {
		p.c.t.Fatalf("%s sync status=%d outcomes=%+v", p.subject, status, response.Outcomes)
	}
	for _, outcome := range response.Outcomes {
		if outcome.Status != "applied" {
			code := ""
			if outcome.Code != nil {
				code = *outcome.Code
			}
			p.c.t.Fatalf("%s mutation %s status=%s code=%s", p.subject, outcome.MutationID, outcome.Status, code)
		}
	}
}

func (p *pushPhone) post(path string, body any, capabilities string) int {
	p.c.t.Helper()
	status, _ := p.do(http.MethodPost, path, body, capabilities)
	return status
}

func (p *pushPhone) refreshSession() {
	p.c.t.Helper()
	status, raw := p.do(http.MethodPost, "/v1/auth/refresh", protocol.RefreshRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities()}, RefreshToken: p.refresh}, "")
	if status != http.StatusOK {
		p.c.t.Fatalf("%s refresh status=%d", p.subject, status)
	}
	var session protocol.SessionResponse
	if err := json.Unmarshal(raw, &session); err != nil {
		p.c.t.Fatal(err)
	}
	p.token, p.refresh = session.AccessToken, session.RefreshToken
}

func familyMeta() protocol.RequestMeta {
	return protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.FamilyCapabilities()}
}

// MARK: care mutations, as a phone's outbox sends them

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }

func strptr(s string) *string { return &s }

func (c *pushComposition) id(prefix string) string {
	return prefix + c.h.CommandID()[len(prefix):]
}

func (c *pushComposition) startFeed(timer string, side string, at time.Time, session *string) protocol.Mutation {
	m := testkit.MustMutation(c.t, "timer-feed.json")
	p := m.Payload.(protocol.TimerStartPayload)
	m.MutationID, m.EntityID, m.ClientMutationTime = c.h.CommandID(), timer, stamp(at)
	p.StartedAt, p.CurrentSide, p.CurrentSideStartedAt, p.SessionID = stamp(at), strptr(side), strptr(stamp(at)), session
	m.Payload = p
	return m
}

func (c *pushComposition) stopTimer(timer, record string, at time.Time, kind string) protocol.Mutation {
	fixture := "timer-feed.json"
	if kind == "sleep" {
		fixture = "timer-sleep.json"
	}
	m := testkit.MustMutation(c.t, fixture)
	m.MutationID, m.EntityID, m.Operation, m.ClientMutationTime = c.h.CommandID(), timer, "timer_stop", stamp(at)
	stop := protocol.TimerStopPayload{BabyID: compositionBaby, CompletedRecordID: record, StoppedAt: stamp(at)}
	if kind == "feed" {
		stop.ComponentID = "4" + record[1:]
	}
	m.Payload = stop
	return m
}

func (c *pushComposition) editTimerStart(timer string, at time.Time, version uint64) protocol.Mutation {
	m := testkit.MustMutation(c.t, "timer-feed.json")
	m.MutationID, m.EntityID, m.Operation, m.ObservedEntityVersion, m.ClientMutationTime = c.h.CommandID(), timer, "update", version, stamp(at)
	m.Payload = protocol.TimerEditPayload{BabyID: compositionBaby, StartedAt: strptr(stamp(at))}
	return m
}

func (c *pushComposition) startSleep(timer string, at time.Time) protocol.Mutation {
	m := testkit.MustMutation(c.t, "timer-sleep.json")
	p := m.Payload.(protocol.TimerStartPayload)
	m.MutationID, m.EntityID, m.ClientMutationTime = c.h.CommandID(), timer, stamp(at)
	p.StartedAt = stamp(at)
	m.Payload = p
	return m
}

func (c *pushComposition) baby() protocol.Mutation {
	m := testkit.MustMutation(c.t, "baby.json")
	m.MutationID = c.h.CommandID()
	return m
}

// MARK: assertions on what Apple would have received

func requireSend(t *testing.T, s liveactivity.PushRequest, event string, token string, kind liveactivity.TokenKind, activityKind, phase string) {
	t.Helper()
	if s.Event != event || s.Token != token || s.TokenKind != kind || s.Content.ActivityKind != activityKind || s.Content.Phase != phase || s.Content.Remote == nil {
		t.Fatalf("send=%s want event=%s token=%s kind=%s activityKind=%s phase=%s", describeSends([]liveactivity.PushRequest{s}), event, token, kind, activityKind, phase)
	}
}

// requireEnd: an end carries the last revision but no projected content.
func requireEnd(t *testing.T, s liveactivity.PushRequest, token string, generation string) {
	t.Helper()
	if s.Event != "end" || s.Token != token || s.TokenKind != liveactivity.UpdateToken || s.Content.Remote == nil || (generation != "" && s.Content.Remote.GenerationID != generation) {
		t.Fatalf("send=%s want end to %s generation=%s", describeSends([]liveactivity.PushRequest{s}), token, generation)
	}
}

func sendsTo(sends []liveactivity.PushRequest, token string) []liveactivity.PushRequest {
	var out []liveactivity.PushRequest
	for _, s := range sends {
		if s.Token == token {
			out = append(out, s)
		}
	}
	return out
}

// TestLiveActivityCompositionTwoCaregiverLifecycle: caregiver A runs a feed
// and a sleep through HTTP sync; caregiver B never syncs. Everything B's
// cards show comes from the worker's projection of A's committed writes.
func TestLiveActivityCompositionTwoCaregiverLifecycle(t *testing.T) {
	c := newPushComposition(t, true)
	a := c.phone("owner", "70000000-0000-4000-8000-00000000000a")
	b := c.phone("caregiver", "70000000-0000-4000-8000-00000000000b")
	const (
		aStart = "aaaa0001"
		bStart = "bbbb0001"
		aFeed  = "aaaa1001"
		bFeed  = "bbbb1001"
		aSleep = "aaaa2001"
		bSleep = "bbbb2001"
	)
	assertLiveActivityCapability(t, c.options, true)
	a.write(c.baby())

	// A registers first, then starts a feed on this phone: its own card is
	// local, so the server never pushes A a start for A's own timer.
	a.mustRegisterDevice(aStart, nil)
	c.expect(0, "A's registration with no running care")
	now := time.Now().UTC().Truncate(time.Second)
	feedLeft := c.id("51000000-0000-4000-8000-")
	a.write(c.startFeed(feedLeft, "left", now.Add(-10*time.Minute), nil))
	c.expect(0, "A's own start is suppressed for A and B has no registration")
	// A's local card registers its update token as LiveActivityPushCoordinator
	// sends it: bound to the timer the card shows, with no generation.
	a.mustRegisterActivity(activity{id: "a-feed", token: aFeed, kind: "feed", version: 1, timer: &feedLeft})
	sends := c.expect(1, "A's local card is adopted by the server")
	requireSend(t, sends[0], "update", aFeed, liveactivity.UpdateToken, "breastLeft", "running")

	// B registers late — the private network was unreachable until now. The
	// already-running feed is pushed to start on B's phone.
	receipt := b.mustRegisterDevice(bStart, nil)
	if receipt.AcceptedRegistrationVersion != 1 {
		t.Fatalf("B receipt=%+v", receipt)
	}
	sends = c.expect(1, "delayed registration starts the running feed on B")
	start := sends[0]
	requireSend(t, start, "start", bStart, liveactivity.PushToStartToken, "breastLeft", "running")
	if start.Attributes.KindRaw != "feed" || start.Attributes.BabyID != compositionBaby || start.Attributes.BabyName == "" ||
		start.Content.Remote.TimerID == nil || *start.Content.Remote.TimerID != feedLeft || !start.Content.Remote.CanWrite ||
		start.Content.Remote.Scope.FamilyID != c.h.FamilyID || start.Environment != "sandbox" {
		t.Fatalf("start metadata=%+v remote=%+v", start.Attributes, *start.Content.Remote)
	}
	feedGeneration := start.Content.Remote.GenerationID
	// ActivityKit created B's card from that push and handed B its update
	// token; B registers it against the pushed generation.
	b.mustRegisterActivity(activity{id: "b-feed", token: bFeed, kind: "feed", version: 1, generation: &feedGeneration, timer: &feedLeft})
	// Registering the pushed card confirms the start: the server answers
	// with one update of the same content on the same generation — never a
	// second start, never a second alert.
	sends = c.expect(1, "B's pushed card is confirmed in place")
	requireSend(t, sends[0], "update", bFeed, liveactivity.UpdateToken, "breastLeft", "running")
	if sends[0].Content.Remote.GenerationID != feedGeneration {
		t.Fatalf("confirmation moved B to generation %s", sends[0].Content.Remote.GenerationID)
	}

	// A corrects the running feed's start: both cards update in place.
	a.write(c.editTimerStart(feedLeft, now.Add(-11*time.Minute), 1))
	sends = c.expect(2, "edited start reaches both cards")
	requireSend(t, sends[0], "update", aFeed, liveactivity.UpdateToken, "breastLeft", "running")
	requireSend(t, sends[1], "update", bFeed, liveactivity.UpdateToken, "breastLeft", "running")
	if sends[1].Content.Remote.GenerationID != feedGeneration || sends[1].Content.StartedAt != liveactivity.FoundationDate(float64(now.Add(-11*time.Minute).Unix())) {
		t.Fatalf("B update=%+v", sends[1].Content)
	}

	// A switches sides from this first part. The Left stop and the Right
	// start commit in one request, as BabyStore.startPartStoppingRunning
	// sends them: one card continues — an update, never an end or a start.
	switchAt := time.Now().UTC().Truncate(time.Second).Add(-6 * time.Minute)
	leftRecord := c.id("21000000-0000-4000-8000-")
	feedRight := c.id("51000000-0000-4000-8000-")
	a.write(c.stopTimer(feedLeft, leftRecord, switchAt, "feed"), c.startFeed(feedRight, "right", switchAt, &leftRecord))
	sends = c.expect(2, "a first-part side switch updates both cards in place")
	for i, token := range []string{aFeed, bFeed} {
		requireSend(t, sends[i], "update", token, liveactivity.UpdateToken, "breastRight", "running")
		remote := sends[i].Content.Remote
		if remote.TimerID == nil || *remote.TimerID != feedRight || remote.SessionID == nil || *remote.SessionID != leftRecord {
			t.Fatalf("side switch revision=%+v", *remote)
		}
	}
	if sends[1].Content.Remote.GenerationID != feedGeneration {
		t.Fatalf("side switch replaced B's card: %s", describeSends(sends))
	}

	// The push runtime restarts. A pauses the feed while it is down; the
	// queued change is delivered by the next process without another write.
	// Five minutes is the shortest break a phone can register; the pause is
	// dated so the break has 25 seconds left when it is committed.
	pauseAt := time.Now().UTC().Add(-5*time.Minute + 25*time.Second)
	feedRecord := c.id("21000000-0000-4000-8000-")
	c.restartWorker(func() {
		a.write(c.stopTimer(feedRight, feedRecord, pauseAt, "feed"))
	})
	sends = c.expect(2, "worker restart delivers the queued pause to both cards")
	for i, token := range []string{aFeed, bFeed} {
		requireSend(t, sends[i], "update", token, liveactivity.UpdateToken, "breastRight", "feedBreak")
		if sends[i].Content.BreakEndsAt == nil || len(sends[i].Content.Parts) != 2 || sends[i].Content.Remote.GenerationID == "" {
			t.Fatalf("break content=%+v", sends[i].Content)
		}
	}
	if sends[1].Content.Remote.GenerationID != feedGeneration {
		t.Fatalf("pause moved B to another generation: %s", sends[1].Content.Remote.GenerationID)
	}

	// The five-minute break runs out with no further write: the worker's own
	// deadline ends both cards.
	breakEnds := pauseAt.Add(5 * time.Minute)
	sends = c.expectWithin(2, time.Until(breakEnds)+deliveryWindow, "expired break ends both cards without a write")
	if late := time.Since(breakEnds); late < 0 || late > deliveryWindow {
		t.Fatalf("break end delivered %v after the break ended", late)
	}
	requireEnd(t, sends[0], aFeed, "")
	requireEnd(t, sends[1], bFeed, feedGeneration)

	// Sleep: A starts it on this phone; B gets a start, A does not.
	sleepTimer := c.id("52000000-0000-4000-8000-")
	a.write(c.startSleep(sleepTimer, now.Add(-time.Minute)))
	sends = c.expect(1, "sleep start reaches only B")
	requireSend(t, sends[0], "start", bStart, liveactivity.PushToStartToken, "sleep", "running")
	if sends[0].Attributes.KindRaw != "sleep" {
		t.Fatalf("sleep attributes=%+v", sends[0].Attributes)
	}
	sleepGeneration := sends[0].Content.Remote.GenerationID
	b.mustRegisterActivity(activity{id: "b-sleep", token: bSleep, kind: "sleep", version: 1, generation: &sleepGeneration, timer: &sleepTimer})
	a.mustRegisterActivity(activity{id: "a-sleep", token: aSleep, kind: "sleep", version: 1, timer: &sleepTimer})
	sends = c.expect(2, "A's local sleep card is adopted and B's pushed one confirmed")
	requireSend(t, sends[0], "update", aSleep, liveactivity.UpdateToken, "sleep", "running")
	requireSend(t, sends[1], "update", bSleep, liveactivity.UpdateToken, "sleep", "running")
	if sends[1].Content.Remote.GenerationID != sleepGeneration {
		t.Fatalf("sleep confirmation generation=%s", sends[1].Content.Remote.GenerationID)
	}
	a.write(c.stopTimer(sleepTimer, c.id("22000000-0000-4000-8000-"), now, "sleep"))
	sends = c.expect(2, "sleep stop ends both cards")
	requireEnd(t, sends[0], aSleep, "")
	requireEnd(t, sends[1], bSleep, sleepGeneration)

	// A second feed changes part inside its session: a pause, then the other
	// side within the break. The card stays one card — same generation,
	// updates only — on both phones.
	secondFeed := c.id("51000000-0000-4000-8000-")
	second := time.Now().UTC().Truncate(time.Second)
	a.write(c.startFeed(secondFeed, "left", second.Add(-3*time.Minute), nil))
	sends = c.expect(1, "a second feed starts on B")
	requireSend(t, sends[0], "start", bStart, liveactivity.PushToStartToken, "breastLeft", "running")
	secondGeneration := sends[0].Content.Remote.GenerationID
	b.mustRegisterActivity(activity{id: "b-feed-2", token: "bbbb1002", kind: "feed", version: 1, generation: &secondGeneration, timer: &secondFeed})
	a.mustRegisterActivity(activity{id: "a-feed-2", token: "aaaa1002", kind: "feed", version: 1, timer: &secondFeed})
	c.expect(2, "both second-feed cards are registered")
	secondLeftRecord := c.id("21000000-0000-4000-8000-")
	a.write(c.stopTimer(secondFeed, secondLeftRecord, second.Add(-150*time.Second), "feed"))
	sends = c.expect(2, "the pause shows a break on both cards")
	requireSend(t, sends[0], "update", "aaaa1002", liveactivity.UpdateToken, "breastLeft", "feedBreak")
	requireSend(t, sends[1], "update", "bbbb1002", liveactivity.UpdateToken, "breastLeft", "feedBreak")
	rightPart := c.id("51000000-0000-4000-8000-")
	a.write(c.startFeed(rightPart, "right", second.Add(-2*time.Minute), &secondLeftRecord))
	sends = c.expect(2, "the other side resumes the same card")
	requireSend(t, sends[0], "update", "aaaa1002", liveactivity.UpdateToken, "breastRight", "running")
	requireSend(t, sends[1], "update", "bbbb1002", liveactivity.UpdateToken, "breastRight", "running")
	if sends[1].Content.Remote.GenerationID != secondGeneration {
		t.Fatalf("resuming in the session replaced B's card: %s", describeSends(sends))
	}
	a.write(c.stopTimer(rightPart, c.id("21000000-0000-4000-8000-"), second.Add(-time.Minute), "feed"))
	sends = c.expect(2, "the second pause lists both parts")
	for i, token := range []string{"aaaa1002", "bbbb1002"} {
		requireSend(t, sends[i], "update", token, liveactivity.UpdateToken, "breastRight", "feedBreak")
		if len(sends[i].Content.Parts) != 2 || sends[i].Content.SuggestedSide == nil {
			t.Fatalf("session break content=%+v", sends[i].Content)
		}
	}
	if sends[1].Content.Remote.GenerationID != secondGeneration {
		t.Fatalf("second pause replaced B's card: %s", describeSends(sends))
	}

	for _, s := range c.transport.all() {
		if s.Token == aStart {
			t.Fatalf("A was pushed a start for its own timer: %s", describeSends([]liveactivity.PushRequest{s}))
		}
	}
	if starts := sendsTo(c.transport.all(), bStart); len(starts) != 3 {
		t.Fatalf("B starts=%s", describeSends(starts))
	}
}

// runningFeedOnB is the shared opening of the authority profiles: A runs a
// feed, B registers, receives the push-to-start and registers the card.
type runningFeedOnB struct {
	a, b       *pushPhone
	timer      string
	generation string
	started    time.Time
	version    uint64
}

const (
	profileStart = "cccc0001"
	profileFeed  = "cccc1001"
)

func (c *pushComposition) runningFeedOnB() *runningFeedOnB {
	c.t.Helper()
	r := &runningFeedOnB{a: c.phone("owner", "70000000-0000-4000-8000-0000000000aa"), b: c.phone("caregiver", "70000000-0000-4000-8000-0000000000bb")}
	r.a.write(c.baby())
	r.started = time.Now().UTC().Truncate(time.Second).Add(-20 * time.Minute)
	r.timer = c.id("53000000-0000-4000-8000-")
	r.a.write(c.startFeed(r.timer, "left", r.started, nil))
	r.version = 1
	r.b.mustRegisterDevice(profileStart, nil)
	start := c.expect(1, "B is pushed the running feed")[0]
	requireSend(c.t, start, "start", profileStart, liveactivity.PushToStartToken, "breastLeft", "running")
	r.generation = start.Content.Remote.GenerationID
	r.b.mustRegisterActivity(activity{id: "b-feed", token: profileFeed, kind: "feed", version: 1, generation: &r.generation, timer: &r.timer})
	requireSend(c.t, c.expect(1, "B's card is confirmed")[0], "update", profileFeed, liveactivity.UpdateToken, "breastLeft", "running")
	return r
}

// edit moves the running feed's start a minute earlier: a committed care
// change every registered card should hear about.
func (r *runningFeedOnB) edit(c *pushComposition) {
	c.t.Helper()
	r.started = r.started.Add(-time.Minute)
	r.a.write(c.editTimerStart(r.timer, r.started, r.version))
	r.version++
}

// awaitRevoked waits for the worker's authority sweep to revoke the
// registration of `installation` (or for its rows to be gone with the
// account) and for its delivery jobs to be deleted, failing on any payload to
// `tokens` meanwhile. Only then is the queue idle again.
func (c *pushComposition) awaitRevoked(installation string, mark int, why string, tokens ...string) {
	c.t.Helper()
	began := time.Now()
	deadline := began.Add(revocationWindow)
	for {
		for _, s := range c.transport.all()[mark:] {
			for _, token := range tokens {
				if s.Token == token {
					c.t.Fatalf("%s: a payload reached %s before revocation completed: %s", why, token, describeSends([]liveactivity.PushRequest{s}))
				}
			}
		}
		var live bool
		err := c.inspect.QueryRow(c.t.Context(), `SELECT EXISTS(SELECT FROM live_activity_registration_index r WHERE r.installation_id=$1 AND NOT r.revoked)
  OR EXISTS(SELECT FROM live_activity_delivery d JOIN live_activity_registration_index r USING(registration_id) WHERE r.installation_id=$1)`, installation).Scan(&live)
		if err != nil {
			c.t.Fatal(err)
		}
		if !live {
			break
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: registration %s still live after %v", why, installation, revocationWindow)
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Logf("%s: registration revoked and its jobs deleted after %v", why, time.Since(began).Round(100*time.Millisecond))
	c.requireNothingSince(mark, why, tokens...)
}

// mark is the number of sends so far; requireNothingSince checks the rest.
func (c *pushComposition) mark() int { return len(c.transport.all()) }

func (c *pushComposition) requireNothingSince(mark int, why string, tokens ...string) {
	c.t.Helper()
	c.quiesce()
	for _, s := range c.transport.all()[mark:] {
		for _, token := range tokens {
			if s.Token == token {
				c.t.Fatalf("%s: a payload reached %s: %s", why, token, describeSends([]liveactivity.PushRequest{s}))
			}
		}
	}
}

// TestLiveActivityCompositionAuthorityProfiles: each change of authority
// through the real routes either keeps a valid lineage delivering or stops
// every later payload. Nothing is sent after the dispatch check refuses.
func TestLiveActivityCompositionAuthorityProfiles(t *testing.T) {
	t.Run("refresh_rotation_then_logout", func(t *testing.T) {
		c := newPushComposition(t, true)
		r := c.runningFeedOnB()
		// The app renews its session while the phone sleeps: the rotated
		// lineage is still the registration's authority.
		r.b.refreshSession()
		r.edit(c)
		requireSend(t, c.expect(1, "rotated lineage still delivers")[0], "update", profileFeed, liveactivity.UpdateToken, "breastLeft", "running")
		if status := r.b.post("/v1/auth/logout", protocol.LogoutRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.IdentityCapabilities()}, CommandID: c.h.CommandID()}, ""); status != http.StatusOK && status != http.StatusNoContent {
			t.Fatalf("logout status=%d", status)
		}
		mark := c.mark()
		r.edit(c)
		c.awaitRevoked(r.b.installation, mark, "logout stops delivery", profileStart, profileFeed)
		c.expect(0, "logout stops delivery")
		if status, _ := r.b.registerDevice(profileStart, nil); status != http.StatusUnauthorized {
			t.Fatalf("logged-out registration status=%d", status)
		}
		c.requireNothingSince(mark, "after logout", profileStart, profileFeed)
	})

	t.Run("remove_then_rejoin", func(t *testing.T) {
		c := newPushComposition(t, true)
		r := c.runningFeedOnB()
		removed := c.h.Caregiver.AccountID
		mark := c.mark()
		if status := r.a.post("/v1/family/members/"+removed+"/remove", protocol.CommandRequest{RequestMeta: familyMeta(), CommandID: c.h.CommandID()}, ""); status != http.StatusOK {
			t.Fatalf("remove status=%d", status)
		}
		r.edit(c)
		c.awaitRevoked(r.b.installation, mark, "a removed caregiver's registration receives nothing", profileStart, profileFeed)
		c.expect(0, "a removed caregiver's registration receives nothing")
		// B comes back through a new invitation with a new session.
		principal, err := c.h.Reauthenticate("caregiver")
		if err != nil {
			t.Fatal(err)
		}
		invite, err := c.h.Family.Invite(t.Context(), c.h.Owner, c.h.CommandID(), "caregiver")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.h.Family.Redeem(t.Context(), principal, c.h.CommandID(), invite.Token, true); err != nil {
			t.Fatal(err)
		}
		c.h.Caregiver = c.h.Refresh("caregiver")
		r.b.token, r.b.refresh = c.h.AccessToken("caregiver"), c.h.RefreshToken("caregiver")
		r.edit(c)
		c.expect(0, "rejoining alone does not revive the registration")
		c.requireNothingSince(mark, "between removal and re-registration", profileStart, profileFeed)
		// The rejoined phone registers again under its new membership, and its
		// coordinator re-registers the card it still holds (a newer token
		// version). Delivery resumes with the current content — the edits made
		// while B was out — and never as a second start.
		r.b.mustRegisterDevice(profileStart, nil)
		r.b.mustRegisterActivity(activity{id: "b-feed", token: profileFeed, kind: "feed", version: 2, generation: &r.generation, timer: &r.timer})
		resumed := c.expect(1, "the rejoined, re-registered card resumes")
		requireSend(t, resumed[0], "update", profileFeed, liveactivity.UpdateToken, "breastLeft", "running")
		if resumed[0].Content.StartedAt != liveactivity.FoundationDate(float64(r.started.Unix())) || !resumed[0].Content.Remote.CanWrite {
			t.Fatalf("resumed content=%+v remote=%+v", resumed[0].Content, *resumed[0].Content.Remote)
		}
		r.edit(c)
		requireSend(t, c.expect(1, "and keeps updating")[0], "update", profileFeed, liveactivity.UpdateToken, "breastLeft", "running")

	})

	t.Run("owner_demotes_to_viewer", func(t *testing.T) {
		c := newPushComposition(t, true)
		r := c.runningFeedOnB()
		if status := r.a.post("/v1/family/members/"+c.h.Caregiver.AccountID+"/role", protocol.MemberRoleRequest{CommandRequest: protocol.CommandRequest{RequestMeta: familyMeta(), CommandID: c.h.CommandID()}, Role: "viewer"}, ""); status != http.StatusOK {
			t.Fatalf("role status=%d", status)
		}
		c.h.Caregiver = c.h.Refresh("caregiver")
		r.b.token, r.b.refresh = c.h.AccessToken("caregiver"), c.h.RefreshToken("caregiver")
		c.accountForAll()
		r.edit(c)
		// A viewer still sees the feed, read-only.
		for _, s := range c.expect(1, "a viewer's card keeps updating") {
			requireSend(t, s, "update", profileFeed, liveactivity.UpdateToken, "breastLeft", "running")
			if s.Content.Remote.CanWrite {
				t.Fatalf("viewer card offered controls: %+v", *s.Content.Remote)
			}
		}
		// Its card's Stop reaches the server as the viewer's own command: refused,
		// with nothing written and nothing pushed.
		var journalBefore int
		if err := c.inspect.QueryRow(t.Context(), `SELECT count(*) FROM mutation_journal WHERE family_id=$1`, c.h.FamilyID).Scan(&journalBefore); err != nil {
			t.Fatal(err)
		}
		status, raw := r.b.syncRaw(c.stopTimer(r.timer, c.id("23000000-0000-4000-8000-"), time.Now().UTC(), "feed"))
		var refusal protocol.ErrorResponse
		if status != http.StatusForbidden || json.Unmarshal(raw, &refusal) != nil || refusal.Error.Code != "read_only_caregiver" || refusal.Error.Retryable {
			t.Fatalf("viewer stop: want 403 read_only_caregiver (not retryable), got status=%d body=%s", status, raw)
		}
		var journalAfter int
		if err := c.inspect.QueryRow(t.Context(), `SELECT count(*) FROM mutation_journal WHERE family_id=$1`, c.h.FamilyID).Scan(&journalAfter); err != nil {
			t.Fatal(err)
		}
		if journalAfter != journalBefore || !c.h.TimerIsLive(t, r.timer) {
			t.Fatalf("read-only control wrote: journal %d→%d live=%t", journalBefore, journalAfter, c.h.TimerIsLive(t, r.timer))
		}
		c.expect(0, "a refused viewer command pushes nothing")
	})

	t.Run("account_deleted", func(t *testing.T) {
		c := newPushComposition(t, true)
		r := c.runningFeedOnB()
		mark := c.mark()
		if status := r.b.post("/v1/account/delete", protocol.ConfirmedCommandRequest{RequestMeta: familyMeta(), CommandID: c.h.CommandID(), Confirmed: true}, ""); status != http.StatusOK {
			t.Fatalf("account delete status=%d", status)
		}
		r.edit(c)
		c.awaitRevoked(r.b.installation, mark, "a deleted account receives nothing", profileStart, profileFeed)
		c.expect(0, "a deleted account receives nothing")
		c.requireNothingSince(mark, "after account deletion", profileStart, profileFeed)
	})

	t.Run("recovery_epoch_bump", func(t *testing.T) {
		c := newPushComposition(t, true)
		r := c.runningFeedOnB()
		mark := c.mark()
		// The change is committed while no worker runs, so the queued work is
		// only ever examined under the new epoch.
		c.restartWorker(func() {
			r.edit(c)
			if _, err := c.h.Migrator.Exec(t.Context(), `UPDATE server_state SET recovery_epoch=recovery_epoch+1`); err != nil {
				t.Fatal(err)
			}
		})
		// The worker retires every prior-epoch registration on its next claim;
		// until then and after, nothing may reach B's tokens.
		c.awaitRevoked(r.b.installation, mark, "a prior-epoch registration receives nothing", profileStart, profileFeed)
		c.expect(0, "prior-epoch registrations receive nothing")
		c.requireNothingSince(mark, "after an epoch bump", profileStart, profileFeed)
	})

	t.Run("queued_retry_after_token_replacement", func(t *testing.T) {
		c := newPushComposition(t, true)
		r := c.runningFeedOnB()
		var refused atomic.Int32
		c.transport.script(func(_ context.Context, s liveactivity.PushRequest) (liveactivity.PushResult, error) {
			if s.Token == profileFeed {
				refused.Add(1)
				return liveactivity.PushResult{HTTPStatus: 503, Classification: liveactivity.Retryable}, fmt.Errorf("APNs unavailable")
			}
			return liveactivity.PushResult{HTTPStatus: 200, Classification: liveactivity.Accepted}, nil
		})
		mark := c.mark()
		r.edit(c)
		requireSend(t, c.expect(1, "the update meets a temporary APNs refusal")[0], "update", profileFeed, liveactivity.UpdateToken, "breastLeft", "running")
		// ActivityKit rotates the card's token before the retry is due.
		const replaced = "cccc1002"
		r.b.mustRegisterActivity(activity{id: "b-feed", token: replaced, kind: "feed", version: 2, generation: &r.generation, timer: &r.timer})
		retried := c.expect(1, "the queued retry goes to the replacement token")
		requireSend(t, retried[0], "update", replaced, liveactivity.UpdateToken, "breastLeft", "running")
		if retried[0].Content.Remote.GenerationID != r.generation || retried[0].Content.StartedAt != liveactivity.FoundationDate(float64(r.started.Unix())) {
			t.Fatalf("retry content=%+v", retried[0].Content)
		}
		if refused.Load() != 1 {
			t.Fatalf("old token was retried %d times", refused.Load())
		}
		c.requireNothingSince(mark, "after replacement", profileStart)
	})
}

// TestLiveActivityCompositionInFlightRevocationBoundary records the one
// boundary no server can close: a payload already handed to Apple when the
// revocation commits. The dispatch check before it was valid; nothing after
// the revocation is sent.
func TestLiveActivityCompositionInFlightRevocationBoundary(t *testing.T) {
	c := newPushComposition(t, true)
	r := c.runningFeedOnB()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c.transport.script(func(ctx context.Context, s liveactivity.PushRequest) (liveactivity.PushResult, error) {
		if s.Token == profileFeed {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return liveactivity.PushResult{}, ctx.Err()
			}
		}
		return liveactivity.PushResult{HTTPStatus: 200, Classification: liveactivity.Accepted}, nil
	})
	r.edit(c)
	select {
	case <-entered:
	case <-time.After(deliveryWindow):
		t.Fatal("the update never reached transport")
	}
	// Removal commits while the request is with Apple: the worker holds no
	// authority fence or snapshot across transport, so it is not delayed.
	removed := make(chan int, 1)
	go func() {
		removed <- r.a.post("/v1/family/members/"+c.h.Caregiver.AccountID+"/remove", protocol.CommandRequest{RequestMeta: familyMeta(), CommandID: c.h.CommandID()}, "")
	}()
	select {
	case status := <-removed:
		if status != http.StatusOK {
			t.Fatalf("remove status=%d", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("revocation waited on an in-flight send")
	}
	close(release)
	var boundary liveactivity.PushRequest
	select {
	case boundary = <-c.transport.arrived:
	case <-time.After(deliveryWindow):
		t.Fatal("the in-flight payload was never recorded")
	}
	requireSend(t, boundary, "update", profileFeed, liveactivity.UpdateToken, "breastLeft", "running")
	afterBoundary := c.mark()
	t.Logf("in-flight revocation boundary: 1 payload authorized before removal committed was delivered after it")
	mark := afterBoundary
	r.edit(c)
	c.awaitRevoked(r.b.installation, mark, "nothing is sent after the revocation", profileStart, profileFeed)
	c.accountForAll()
	c.expect(0, "nothing is sent after the revocation")
	c.requireNothingSince(mark, "after the boundary", profileStart, profileFeed)
}

// TestLiveActivityCompositionLostAcknowledgment: Apple accepted a start but
// the answer never arrived. The server never repeats it blindly; the phone's
// foreground inventory proves absence for exactly one retry, and a phone that
// does hold the card simply registers it.
func TestLiveActivityCompositionLostAcknowledgment(t *testing.T) {
	c := newPushComposition(t, true)
	a := c.phone("owner", "70000000-0000-4000-8000-0000000000a1")
	b := c.phone("caregiver", "70000000-0000-4000-8000-0000000000b1")
	const bStart, bFeed, bSleep = "eeee0001", "eeee1001", "eeee2001"
	lose := atomic.Bool{}
	c.transport.script(func(_ context.Context, s liveactivity.PushRequest) (liveactivity.PushResult, error) {
		if s.Event == "start" && lose.CompareAndSwap(true, false) {
			return liveactivity.PushResult{}, fmt.Errorf("connection reset after the request was written")
		}
		return liveactivity.PushResult{HTTPStatus: 200, Classification: liveactivity.Accepted}, nil
	})
	a.write(c.baby())
	b.mustRegisterDevice(bStart, nil)
	c.expect(0, "nothing is running")

	// Absent: the lost start never produced a card on B.
	lose.Store(true)
	feed := c.id("54000000-0000-4000-8000-")
	started := time.Now().UTC().Truncate(time.Second).Add(-15 * time.Minute)
	a.write(c.startFeed(feed, "left", started, nil))
	lost := c.expect(1, "the start goes out and its acknowledgment is lost")[0]
	requireSend(t, lost, "start", bStart, liveactivity.PushToStartToken, "breastLeft", "running")
	a.write(c.editTimerStart(feed, started.Add(-time.Minute), 1))
	c.expect(0, "an unconfirmed start is never repeated blindly")
	receipt := b.mustRegisterDevice(bStart, nil)
	if len(receipt.UnconfirmedStarts) != 1 || receipt.UnconfirmedStarts[0].GenerationID != lost.Content.Remote.GenerationID || receipt.UnconfirmedStarts[0].AttemptID != lost.APNSID {
		t.Fatalf("receipt unconfirmed=%+v lost=%s/%s", receipt.UnconfirmedStarts, lost.Content.Remote.GenerationID, lost.APNSID)
	}
	c.expect(0, "replaying the registration is not proof of absence")
	inventory := &protocol.LiveActivityForegroundInventory{InventoryVersion: 1, AbsentGenerations: []protocol.LiveActivityAbsentGeneration{{GenerationID: receipt.UnconfirmedStarts[0].GenerationID, ObservedAttemptID: receipt.UnconfirmedStarts[0].AttemptID}}}
	b.mustRegisterDevice(bStart, inventory)
	retry := c.expect(1, "a foreground absence proof authorizes one retry")[0]
	requireSend(t, retry, "start", bStart, liveactivity.PushToStartToken, "breastLeft", "running")
	if retry.APNSID == lost.APNSID {
		t.Fatal("retry reused the lost attempt")
	}
	retryGeneration := retry.Content.Remote.GenerationID
	b.mustRegisterActivity(activity{id: "b-feed", token: bFeed, kind: "feed", version: 1, generation: &retryGeneration, timer: &feed})
	requireSend(t, c.expect(1, "the retried card is confirmed")[0], "update", bFeed, liveactivity.UpdateToken, "breastLeft", "running")
	inventory.InventoryVersion = 2
	b.mustRegisterDevice(bStart, inventory)
	c.expect(0, "a second absence proof for the same generation buys nothing")

	// Present: Apple did deliver; the card exists and registers itself.
	lose.Store(true)
	sleepTimer := c.id("55000000-0000-4000-8000-")
	a.write(c.startSleep(sleepTimer, time.Now().UTC().Add(-time.Minute)))
	lostSleep := c.expect(1, "the sleep start's acknowledgment is lost")[0]
	requireSend(t, lostSleep, "start", bStart, liveactivity.PushToStartToken, "sleep", "running")
	sleepGeneration := lostSleep.Content.Remote.GenerationID
	b.mustRegisterActivity(activity{id: "b-sleep", token: bSleep, kind: "sleep", version: 1, generation: &sleepGeneration, timer: &sleepTimer})
	requireSend(t, c.expect(1, "the delivered card confirms the start")[0], "update", bSleep, liveactivity.UpdateToken, "sleep", "running")
	if receipt = b.mustRegisterDevice(bStart, nil); len(receipt.UnconfirmedStarts) != 0 {
		t.Fatalf("confirmed starts still unconfirmed: %+v", receipt.UnconfirmedStarts)
	}
	c.expect(0, "nothing further")
	if starts := sendsTo(c.transport.all(), bStart); len(starts) != 3 {
		t.Fatalf("starts=%s", describeSends(starts))
	}
}

// TestLiveActivityCompositionSelfHostedWithoutCapability: push disabled (the
// default for every self-hosted server) advertises nothing, refuses
// registration and constructs no worker or transport; sync is unaffected.
func TestLiveActivityCompositionSelfHostedWithoutCapability(t *testing.T) {
	c := newPushComposition(t, false)
	assertLiveActivityCapability(t, c.options, false)
	if c.options.LiveActivityService != nil || c.options.LiveActivityReadiness != nil {
		t.Fatal("disabled server constructed push services")
	}
	a := c.phone("owner", "70000000-0000-4000-8000-0000000000a2")
	b := c.phone("caregiver", "70000000-0000-4000-8000-0000000000b2")
	a.write(c.baby())
	if status, _ := b.registerDevice("ffff0001", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("registration on a server without the capability=%d", status)
	}
	a.write(c.startFeed(c.id("56000000-0000-4000-8000-"), "left", time.Now().UTC().Add(-time.Minute), nil))
	if status, response := b.sync(); status != http.StatusOK || len(response.Changes) == 0 {
		t.Fatalf("sync without push status=%d changes=%d", status, len(response.Changes))
	}
	var registrations int
	if err := c.inspect.QueryRow(t.Context(), `SELECT count(*) FROM live_activity_devices`).Scan(&registrations); err != nil {
		t.Fatal(err)
	}
	if registrations != 0 || len(c.transport.all()) != 0 {
		t.Fatalf("disabled server registered=%d sent=%d", registrations, len(c.transport.all()))
	}
}

// TestLiveActivityCompositionRetiredStartAndAdoptedOwnCard probes, through the
// real composition:
//   - the originating phone's own card, registered as the phone sends it (bound
//     to its timer, no generation), is adopted by the server at once;
//   - a server-started card the phone retires before its token registers is
//     retried at most once per generation on a foreground absence proof (Task 9);
//   - the own card registered as ended (the parent's swipe) suppresses only its
//     lifecycle: the next lifecycle's push-to-start still reaches that phone.
func TestLiveActivityCompositionRetiredStartAndAdoptedOwnCard(t *testing.T) {
	c := newPushComposition(t, true)
	a := c.phone("owner", "70000000-0000-4000-8000-0000000000a3")
	b := c.phone("caregiver", "70000000-0000-4000-8000-0000000000b3")
	const aStart, aLocal, bStart = "9a9a0001", "9a9a1001", "9b9b0001"
	a.write(c.baby())
	a.mustRegisterDevice(aStart, nil)
	b.mustRegisterDevice(bStart, nil)
	c.expect(0, "nothing is running")

	first := c.id("58000000-0000-4000-8000-")
	started := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	a.write(c.startFeed(first, "left", started, nil))
	pushed := c.expect(1, "B is pushed A's feed; A is not")[0]
	requireSend(t, pushed, "start", bStart, liveactivity.PushToStartToken, "breastLeft", "running")
	// A's own card, local and never pushed: registered bound to its timer,
	// exactly as the phone does, and adopted with an update — never a start.
	a.mustRegisterActivity(activity{id: "a-local", token: aLocal, kind: "feed", version: 1, timer: &first})
	requireSend(t, c.expect(1, "A's own card is adopted")[0], "update", aLocal, liveactivity.UpdateToken, "breastLeft", "running")

	// B's phone retires the pushed card before its token registers (a
	// duplicate it reconciled away). Its foreground inventory proves absence.
	receipt := b.mustRegisterDevice(bStart, nil)
	if len(receipt.UnconfirmedStarts) != 1 {
		t.Fatalf("unconfirmed=%+v", receipt.UnconfirmedStarts)
	}
	proof := func(version uint64, start protocol.LiveActivityUnconfirmedStart) *protocol.LiveActivityForegroundInventory {
		return &protocol.LiveActivityForegroundInventory{InventoryVersion: version, AbsentGenerations: []protocol.LiveActivityAbsentGeneration{{GenerationID: start.GenerationID, ObservedAttemptID: start.AttemptID}}}
	}
	b.mustRegisterDevice(bStart, proof(1, receipt.UnconfirmedStarts[0]))
	retry := c.expect(1, "one retry for the retired card")[0]
	requireSend(t, retry, "start", bStart, liveactivity.PushToStartToken, "breastLeft", "running")
	// Retired again: a second proof, for the retry's own attempt.
	receipt = b.mustRegisterDevice(bStart, nil)
	if len(receipt.UnconfirmedStarts) != 1 || receipt.UnconfirmedStarts[0].AttemptID != retry.APNSID {
		t.Fatalf("after retry unconfirmed=%+v", receipt.UnconfirmedStarts)
	}
	if status, _ := b.registerDevice(bStart, proof(2, receipt.UnconfirmedStarts[0])); status != http.StatusOK {
		t.Fatalf("second absence proof status=%d", status)
	}
	c.expect(0, "no second retry for the same generation")
	t.Logf("ledger (Task 9): a server-started card retired before registration was retried once; a second absence proof for the same generation sent nothing")

	// B stops A's feed: A's adopted card shows the break. The parent then
	// swipes it away; the phone registers that as ended.
	b.write(c.stopTimer(first, c.id("25000000-0000-4000-8000-"), time.Now().UTC().Add(-time.Minute), "feed"))
	requireSend(t, sendsTo(c.expect(1, "B's stop reaches A's own card"), aLocal)[0], "update", aLocal, liveactivity.UpdateToken, "breastLeft", "feedBreak")
	a.mustRegisterActivity(activity{id: "a-local", token: aLocal, kind: "feed", version: 2, timer: &first, state: "ended"})
	c.accountForAll()
	// B starts the next feed: A did not originate it, so A is pushed it.
	second := c.id("58000000-0000-4000-8000-")
	b.write(c.startFeed(second, "right", time.Now().UTC().Truncate(time.Second).Add(-30*time.Second), nil))
	next := sendsTo(c.expect(1, "the next lifecycle starts on A despite the ended own card"), aStart)
	if len(next) != 1 {
		t.Fatal("A was not pushed B's feed")
	}
	requireSend(t, next[0], "start", aStart, liveactivity.PushToStartToken, "breastRight", "running")
	t.Logf("an adopted own card registered ended suppressed only its lifecycle; the next lifecycle's start reached A")
}

// TestLiveActivityCompositionOwnCardReceivesPartnerChanges: the phone that
// starts a feed shows it on its own card at once and registers that card's
// token as LiveActivityPushCoordinator sends it — bound to the timer, with no
// generation, back to back with the start, before the worker has seen either.
// The server adopts it, and from then on the partner's changes reach the
// originating phone's card: a side switch, a pause and, once the partner
// starts a sleep, the feed card's end.
func TestLiveActivityCompositionOwnCardReceivesPartnerChanges(t *testing.T) {
	c := newPushComposition(t, true)
	a := c.phone("owner", "70000000-0000-4000-8000-0000000000a6")
	b := c.phone("caregiver", "70000000-0000-4000-8000-0000000000b6")
	const aStart, aFeed = "9f9f0001", "9f9f1001"
	a.write(c.baby())
	a.mustRegisterDevice(aStart, nil)
	c.expect(0, "nothing is running")

	left := c.id("5a000000-0000-4000-8000-")
	a.write(c.startFeed(left, "left", time.Now().UTC().Truncate(time.Second).Add(-10*time.Minute), nil))
	a.mustRegisterActivity(activity{id: "a-feed", token: aFeed, kind: "feed", version: 1, timer: &left})
	adopted := c.expect(1, "A's own card is adopted")
	requireSend(t, adopted[0], "update", aFeed, liveactivity.UpdateToken, "breastLeft", "running")
	generation := adopted[0].Content.Remote.GenerationID
	if adopted[0].Content.Remote.TimerID == nil || *adopted[0].Content.Remote.TimerID != left {
		t.Fatalf("adopted revision=%+v", *adopted[0].Content.Remote)
	}

	// B switches sides from this first part: stop and next part in one sync.
	switchAt := time.Now().UTC().Truncate(time.Second).Add(-6 * time.Minute)
	leftRecord := c.id("2a000000-0000-4000-8000-")
	right := c.id("5a000000-0000-4000-8000-")
	b.write(c.stopTimer(left, leftRecord, switchAt, "feed"), c.startFeed(right, "right", switchAt, &leftRecord))
	switched := c.expect(1, "B's side switch reaches A's own card")[0]
	requireSend(t, switched, "update", aFeed, liveactivity.UpdateToken, "breastRight", "running")
	if switched.Content.Remote.GenerationID != generation || switched.Content.Remote.TimerID == nil || *switched.Content.Remote.TimerID != right {
		t.Fatalf("side switch revision=%+v", *switched.Content.Remote)
	}

	// B pauses: A's own card shows the break.
	b.write(c.stopTimer(right, c.id("2a000000-0000-4000-8000-"), time.Now().UTC().Add(-2*time.Minute), "feed"))
	paused := c.expect(1, "B's pause reaches A's own card")[0]
	requireSend(t, paused, "update", aFeed, liveactivity.UpdateToken, "breastRight", "feedBreak")
	if paused.Content.Remote.GenerationID != generation || paused.Content.BreakEndsAt == nil || len(paused.Content.Parts) != 2 {
		t.Fatalf("pause content=%+v remote=%+v", paused.Content, *paused.Content.Remote)
	}

	// B starts a sleep: the feed break ends on A's own card, and B's sleep is
	// pushed to start on A.
	b.write(c.startSleep(c.id("5b000000-0000-4000-8000-"), time.Now().UTC().Add(-time.Minute)))
	sends := c.expect(2, "B's sleep ends A's own feed card and starts on A")
	ends, starts := sendsTo(sends, aFeed), sendsTo(sends, aStart)
	if len(ends) != 1 || len(starts) != 1 {
		t.Fatalf("after B's sleep: %s", describeSends(sends))
	}
	requireEnd(t, ends[0], aFeed, generation)
	requireSend(t, starts[0], "start", aStart, liveactivity.PushToStartToken, "sleep", "running")
	for _, s := range c.transport.all() {
		if s.Token == aStart && s.Attributes.KindRaw == "feed" {
			t.Fatalf("A was pushed a start for its own feed: %s", describeSends([]liveactivity.PushRequest{s}))
		}
	}
}

// TestLiveActivityCompositionAppEndsAndDismissals: what the server makes of a
// phone's card that is gone while its lifecycle goes on.
//
//   - An end the app makes is a retirement (9f27098): the phone records it in
//     its ledger and never registers it, so A's adopted, timer-bound card keeps
//     its registration and the feed keeps reaching A. Registered as `ended`
//     instead — what the phone did before 9f27098 when a non-own pass ended a
//     card — the server reads it as the parent dismissing the feed and A
//     silently stops receiving it.
//   - The parent's own swipe is registered (`dismissed`, or ActivityKit's
//     `ended` on a card the app did not retire). It suppresses that generation
//     on that phone only: the other phone keeps the feed, and A's next feed
//     reaches A.
func TestLiveActivityCompositionAppEndsAndDismissals(t *testing.T) {
	// adoptedFeedOnA: A starts a feed here and registers its local card, bound
	// to the running timer; the server adopts it with an update.
	adoptedFeedOnA := func(c *pushComposition, a *pushPhone, aStart, aFeed string) (timer string, started time.Time) {
		c.t.Helper()
		started = time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
		timer = c.id("59000000-0000-4000-8000-")
		a.write(c.startFeed(timer, "left", started, nil))
		c.expect(0, "A's own start is suppressed for A")
		a.mustRegisterActivity(activity{id: "a-feed", token: aFeed, kind: "feed", version: 1, timer: &timer})
		requireSend(c.t, c.expect(1, "A's timer-bound card is adopted")[0], "update", aFeed, liveactivity.UpdateToken, "breastLeft", "running")
		return timer, started
	}

	t.Run("app_retirement_keeps_the_lifecycle", func(t *testing.T) {
		c := newPushComposition(t, true)
		a := c.phone("owner", "70000000-0000-4000-8000-0000000000a4")
		const aStart, aFeed = "9c9c0001", "9c9c1001"
		a.write(c.baby())
		a.mustRegisterDevice(aStart, nil)
		c.expect(0, "nothing is running")
		timer, started := adoptedFeedOnA(c, a, aStart, aFeed)

		// A non-own pass on A's phone ends the card while the feed runs. The
		// app retires it: its ledger forgets the card and nothing reaches the
		// server — A's card registration stays as it was.
		c.expect(0, "a retired end registers nothing")

		// A edits the feed, then pauses it: both still reach A's card.
		a.write(c.editTimerStart(timer, started.Add(-time.Minute), 1))
		requireSend(t, c.expect(1, "the edit still reaches A after the retirement")[0], "update", aFeed, liveactivity.UpdateToken, "breastLeft", "running")
		a.write(c.stopTimer(timer, c.id("26000000-0000-4000-8000-"), time.Now().UTC().Add(-time.Minute), "feed"))
		requireSend(t, c.expect(1, "the pause still reaches A after the retirement")[0], "update", aFeed, liveactivity.UpdateToken, "breastLeft", "feedBreak")
		t.Logf("ledger (Task 9 (b), fixed in 9f27098): an app-retired end of an adopted card left the feed reaching that phone (edit and pause delivered)")
	})

	for _, state := range []string{"ended", "dismissed"} {
		t.Run("parent_"+state+"_suppresses_that_generation_only", func(t *testing.T) {
			c := newPushComposition(t, true)
			a := c.phone("owner", "70000000-0000-4000-8000-0000000000a5")
			b := c.phone("caregiver", "70000000-0000-4000-8000-0000000000b5")
			const aStart, aFeed, bStart, bFeed = "9d9d0001", "9d9d1001", "9e9e0001", "9e9e1001"
			a.write(c.baby())
			a.mustRegisterDevice(aStart, nil)
			b.mustRegisterDevice(bStart, nil)
			c.expect(0, "nothing is running")
			started := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
			timer := c.id("59000000-0000-4000-8000-")
			a.write(c.startFeed(timer, "left", started, nil))
			start := sendsTo(c.expect(1, "B is pushed A's feed"), bStart)
			if len(start) != 1 {
				t.Fatal("B was not pushed A's feed")
			}
			generation := start[0].Content.Remote.GenerationID
			b.mustRegisterActivity(activity{id: "b-feed", token: bFeed, kind: "feed", version: 1, generation: &generation, timer: &timer})
			a.mustRegisterActivity(activity{id: "a-feed", token: aFeed, kind: "feed", version: 1, timer: &timer})
			c.expect(2, "B's card is confirmed and A's adopted")

			// The parent swipes A's card away while the feed runs.
			a.mustRegisterActivity(activity{id: "a-feed", token: aFeed, kind: "feed", version: 2, timer: &timer, state: state})
			c.expect(0, "a dismissal is not answered with a send")
			a.write(c.editTimerStart(timer, started.Add(-time.Minute), 1))
			edited := c.expect(1, "the edit reaches B only")
			requireSend(t, edited[0], "update", bFeed, liveactivity.UpdateToken, "breastLeft", "running")
			a.write(c.stopTimer(timer, c.id("26000000-0000-4000-8000-"), time.Now().UTC().Add(-2*time.Minute), "feed"))
			paused := c.expect(1, "the pause reaches B only")
			requireSend(t, paused[0], "update", bFeed, liveactivity.UpdateToken, "breastLeft", "feedBreak")

			// A's next feed is a new lifecycle: A's new card for it is adopted.
			next := c.id("59000000-0000-4000-8000-")
			a.write(c.startFeed(next, "right", time.Now().UTC().Truncate(time.Second).Add(-30*time.Second), nil))
			replaced := c.expect(2, "the next feed ends B's break card and is pushed to B")
			if ends, starts := sendsTo(replaced, bFeed), sendsTo(replaced, bStart); len(ends) != 1 || len(starts) != 1 {
				t.Fatalf("next feed on B: %s", describeSends(replaced))
			} else {
				requireEnd(t, ends[0], bFeed, generation)
				requireSend(t, starts[0], "start", bStart, liveactivity.PushToStartToken, "breastRight", "running")
			}
			a.mustRegisterActivity(activity{id: "a-feed-2", token: "9d9d1002", kind: "feed", version: 1, timer: &next})
			requireSend(t, sendsTo(c.expect(1, "A's next feed reaches A"), "9d9d1002")[0], "update", "9d9d1002", liveactivity.UpdateToken, "breastRight", "running")
			t.Logf("a parent's %s registration suppressed that generation on that phone only; the next lifecycle reached it", state)
		})
	}
}
