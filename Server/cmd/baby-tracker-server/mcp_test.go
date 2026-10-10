package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/database"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/mcpapi"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const mcpLegacyVersion = "2025-11-25"

// ---------------------------------------------------------------- disabled

func TestDisabledMCPOpensNoPoolAndRegistersNoRoute(t *testing.T) {
	cfg := mcpUnitConfig(t)
	cfg.MCP = config.MCPConfig{DatabaseURL: "postgres://would-fail-if-opened:1/x"}
	pool, err := openMCPPool(t.Context(), cfg)
	if err != nil || pool != nil {
		t.Fatalf("disabled MCP opened a pool: %v %v", pool, err)
	}
	runtime, _ := database.OpenWithMax(t.Context(), "postgres://user@127.0.0.1:1/test", 16)
	defer runtime.Close()
	if budgets := serverPoolBudgets(runtime, nil, nil, nil); len(budgets) != 1 {
		t.Fatalf("disabled MCP counted in the budget: %+v", budgets)
	}
	if _, err := composeMCP(cfg, mcpDependencies{}); err == nil {
		t.Fatal("disabled MCP composed a handler")
	}
	authorityService, familyService, readiness := testComposition(t, cfg, runtime)
	options, err := applicationOptions(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), readiness, authorityService, familyService, runtime, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachMCP(cfg, &options, mcpDependencies{}); err != nil || options.MCPHandler != nil {
		t.Fatalf("disabled attach: handler=%v err=%v", options.MCPHandler, err)
	}
	server := httptest.NewServer(httpapi.New(options))
	defer server.Close()
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		request, _ := http.NewRequest(method, server.URL+"/mcp", strings.NewReader(`{}`))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s /mcp=%d with MCP disabled", method, response.StatusCode)
		}
	}
}

func TestMCPPoolCountsOnceInTheAggregateBudget(t *testing.T) {
	open := func(max int32, readOnly bool) *pgxpool.Pool {
		var p *pgxpool.Pool
		var err error
		if readOnly {
			p, err = database.OpenReadOnlyWithMax(t.Context(), "postgres://user@127.0.0.1:1/test", max)
		} else {
			p, err = database.OpenWithMax(t.Context(), "postgres://user@127.0.0.1:1/test", max)
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	runtime, readinessPool, background, maintenance, mcp := open(16, false), open(1, false), open(1, false), open(2, false), open(4, true)
	budgets := serverPoolBudgets(runtime, mcp, readinessPool, background, maintenance, maintenance)
	if len(budgets) != 5 || budgets[4] != (database.NamedPoolBudget{Identity: mcpPoolIdentity, Max: 4}) {
		t.Fatalf("budgets=%+v", budgets)
	}
	if database.ValidateBudget(budgets, 32, 8) != nil || database.ValidateBudget(budgets, 31, 8) == nil {
		t.Fatal("MCP pool not counted exactly once in the aggregate budget")
	}
	// The same pointer can never be counted as a second distinct pool.
	if got := serverPoolBudgets(runtime, runtime, nil, nil); len(got) != 1 {
		t.Fatalf("runtime pool reused as MCP was counted: %+v", got)
	}
	roles := poolRoles(runtime, nil, maintenance, maintenance, mcp)
	if roles[diagnostics.MCPPool] != mcp || len(roles) != 3 {
		t.Fatalf("sampler roles=%v", roles)
	}
}

func TestCompositionRejectsUnsafeMCPPools(t *testing.T) {
	cfg := mcpUnitConfig(t)
	cfg.MCP = config.MCPConfig{Enabled: true, DatabaseURL: "postgres://user@127.0.0.1:1/test", CursorKey: [32]byte{1}}
	cfg.DatabasePools.MCP, cfg.Admission = 4, config.AdmissionConfig{Total: 8, MCPMax: 4}
	writable, _ := database.OpenWithMax(t.Context(), cfg.MCP.DatabaseURL, 4)
	defer writable.Close()
	readOnly, _ := database.OpenReadOnlyWithMax(t.Context(), cfg.MCP.DatabaseURL, 4)
	defer readOnly.Close()
	tooLarge, _ := database.OpenReadOnlyWithMax(t.Context(), cfg.MCP.DatabaseURL, 5)
	defer tooLarge.Close()
	controller, _ := admission.New(8, 4)
	limiter := allowLimiter{}
	incomplete := &authority.Service{}
	for name, tc := range map[string]struct {
		deps mcpDependencies
		want string
	}{
		"writable pool":     {mcpDependencies{Authority: incomplete, Pool: writable, Admission: controller, Limiter: limiter}, "read-only"},
		"runtime pool":      {mcpDependencies{Authority: incomplete, Pool: readOnly, Others: []*pgxpool.Pool{readOnly}, Admission: controller, Limiter: limiter}, "dedicated"},
		"pool above four":   {mcpDependencies{Authority: incomplete, Pool: tooLarge, Admission: controller, Limiter: limiter}, "at most"},
		"no pool":           {mcpDependencies{Authority: incomplete, Admission: controller, Limiter: limiter}, "pool"},
		"no admission":      {mcpDependencies{Authority: incomplete, Pool: readOnly, Limiter: limiter}, "admission"},
		"no limiter":        {mcpDependencies{Authority: incomplete, Pool: readOnly, Admission: controller}, "limiter"},
		"incomplete reader": {mcpDependencies{Authority: incomplete, Pool: readOnly, Admission: controller, Limiter: limiter}, "authority"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := composeMCP(cfg, tc.deps); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe MCP composition err=%v, want %q", err, tc.want)
			}
		})
	}
	opened, err := openMCPPool(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if !database.ReadOnlyByDefault(opened) || opened.Config().MaxConns != 4 {
		t.Fatalf("opened MCP pool read-only=%v max=%d", database.ReadOnlyByDefault(opened), opened.Config().MaxConns)
	}
}

// ---------------------------------------------------------------- DB-backed

type allowLimiter struct{}

func (allowLimiter) Take(context.Context, string, string, limits.Policy) (limits.Decision, error) {
	return limits.Decision{Allowed: true}, nil
}
func (allowLimiter) TakeMany(context.Context, []limits.Charge) (limits.Decision, error) {
	return limits.Decision{Allowed: true}, nil
}
func (allowLimiter) Peek(context.Context, []limits.Charge) (limits.Decision, error) {
	return limits.Decision{Allowed: true}, nil
}

type panicLimiter struct{ allowLimiter }

func (panicLimiter) TakeMany(context.Context, []limits.Charge) (limits.Decision, error) {
	panic("limiter exploded: password=hunter2")
}

func mcpUnitConfig(t *testing.T) config.Config {
	t.Helper()
	origin, _ := url.Parse("https://sync.example.test")
	cfg := config.Config{Admission: config.AdmissionConfig{Total: 8}, Mode: config.ModeSelfHosted, PublicOrigin: origin,
		ServerInstanceID: testkit.InstanceID, RecoveryEpoch: 1, AppleAudience: "com.example.babytracker"}
	for i, ring := range []*config.DigestKeyringConfig{&cfg.Auth.IdentityKeys, &cfg.Auth.SessionTokenKeys, &cfg.Auth.DeviceContinuityKeys,
		&cfg.Auth.AppleNotificationKeys, &cfg.Auth.InvitationKeys, &cfg.Auth.PageTokenKeys} {
		*ring = config.DigestKeyringConfig{Current: config.DigestKeyConfig{ID: "mcp.v1", Secret: [32]byte{byte(i + 1)}}}
	}
	cfg.Auth.ActorIDPepper[0], cfg.Auth.RateLimitPepper[0] = 7, 8
	cfg.Limits = config.LimitsConfig{Policies: limits.DefaultPolicies(), MaxCaregivers: 10, MaxInvitations: 10, InvitationTTL: time.Hour}
	cfg.Sync.Limits = protocol.DefaultLimits()
	return cfg
}

type mcpFixture struct {
	h          *testkit.Harness
	cfg        config.Config
	pool       *pgxpool.Pool
	controller *admission.Controller
	comp       *mcpComposition
	url        string
	metrics    *diagnostics.CapacityCollector
	phases     *diagnostics.CapacityCollector
	limiter    limits.Limiter
	grants     int
}

// newMCPFixture composes the production MCP path (dedicated read-only pool,
// real guard, real familyread, keyed registry, real limiter quotas, shared
// admission) against a bound authority, served over real TCP.
func newMCPFixture(t *testing.T, configure func(*config.Config, *mcpDependencies)) *mcpFixture {
	return newServedMCPFixture(t, configure, nil)
}

func newServedMCPFixture(t *testing.T, configure func(*config.Config, *mcpDependencies), serve func(config.Config, mcpDependencies) http.Handler) *mcpFixture {
	t.Helper()
	h := testkit.NewBound(t)
	f := &mcpFixture{h: h, cfg: mcpUnitConfig(t)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.PublicOrigin, _ = url.Parse("http://" + listener.Addr().String())
	f.cfg.MCP = config.MCPConfig{Enabled: true, DatabaseURL: h.Runtime.Config().ConnString(), CursorKey: [32]byte{9, 9, 9}}
	f.cfg.DatabasePools = config.DatabasePools{Runtime: 16, MCP: 4, ServerMax: 64, Reserved: 8}
	f.cfg.Admission = config.AdmissionConfig{Total: 8, MCPMax: 4}
	f.pool, err = openMCPPool(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.pool.Close)
	f.controller, _ = admission.New(f.cfg.Admission.Total, f.cfg.Admission.MCPMax)
	// A fixed clock keeps every charge inside one quota window.
	f.limiter, err = limits.New(limits.Options{Pepper: [32]byte{3}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	f.metrics, f.phases = &diagnostics.CapacityCollector{}, &diagnostics.CapacityCollector{}
	deps := mcpDependencies{Authority: h.Authority, Pool: f.pool, Others: []*pgxpool.Pool{h.Runtime, h.Maintenance}, Admission: f.controller,
		Limiter: f.limiter, Metrics: f.metrics, Recorder: f.phases.Recorder(), Now: func() time.Time { return h.Now }}
	if configure != nil {
		configure(&f.cfg, &deps)
	}
	f.comp, err = composeMCP(f.cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	handler := f.comp.handler
	if serve != nil {
		handler = serve(f.cfg, deps)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	f.url = server.URL + "/mcp"
	return f
}

func (f *mcpFixture) operator(t *testing.T, operation, grantID, verifier string) {
	t.Helper()
	if err := f.operatorErr(t.Context(), operation, grantID, verifier); err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
}

func (f *mcpFixture) operatorErr(ctx context.Context, operation, grantID, verifier string) error {
	revision, err := f.h.Authority.FamilyRevision(ctx, f.h.FamilyID)
	if err != nil {
		return err
	}
	input := authority.OperatorIntegration{CommandID: f.h.CommandID(), OperatorID: "90000000-0000-4000-8000-000000000001", Operation: operation,
		GrantID: grantID, FamilyID: f.h.FamilyID, AccountID: f.h.Owner.AccountID, ServerInstanceID: testkit.InstanceID, RecoveryEpoch: 1,
		PolicyID: "Integration.Policy_test-v1", EvidenceDigest: strings.Repeat("e", 64), ReasonCode: "integration_" + operation + "d",
		ExpectedRevision: revision, ConfirmationExpiresAt: f.h.Now.Add(10 * time.Minute).Truncate(time.Second)}
	if operation == "grant" {
		input.Name, input.Scope, input.Verifier, input.ValiditySeconds = "Claude Desktop", authority.IntegrationScopeFamilyRead, verifier, authority.IntegrationValiditySeconds
		input.ReasonCode = "integration_issued"
	}
	_, err = f.h.Authority.ExecuteOperatorIntegration(ctx, input)
	return err
}

// grant issues one more credential for the harness family's owner.
func (f *mcpFixture) grant(t *testing.T) (grantID, credential string) {
	t.Helper()
	f.grants++
	grantID = fmt.Sprintf("60000000-0000-4000-8000-%012x", f.grants)
	secret, err := authority.NewIntegrationSecret()
	if err != nil {
		t.Fatal(err)
	}
	credential, err = authority.FormatIntegrationCredential(grantID, secret)
	if err != nil {
		t.Fatal(err)
	}
	_, verifier, ok := authority.IntegrationCredentialVerifier(credential)
	if !ok {
		t.Fatal("generated credential is not canonical")
	}
	f.operator(t, "grant", grantID, verifier)
	return grantID, credential
}

type mcpReply struct {
	status int
	header http.Header
	body   string
}

func (f *mcpFixture) post(ctx context.Context, credential, body string) (mcpReply, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, strings.NewReader(body))
	if err != nil {
		return mcpReply{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", mcpLegacyVersion)
	request.Header.Set("Authorization", "Bearer "+credential)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return mcpReply{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	return mcpReply{status: response.StatusCode, header: response.Header, body: string(raw)}, err
}

const mcpListTools = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
const mcpListBabies = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_babies","arguments":{}}}`

// assertIdle proves every global MCP slot and every credential entry returned.
func (f *mcpFixture) assertIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var releases []func()
		for range f.cfg.Admission.MCPMax {
			if release, ok := f.controller.TryAcquire(admission.MCP); ok {
				releases = append(releases, release)
			}
		}
		idle := len(releases) == f.cfg.Admission.MCPMax && f.comp.gate.Len() == 0
		for _, release := range releases {
			release()
		}
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("leaked: mcp slots free=%d registry=%d", len(releases), f.comp.gate.Len())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEnabledCompositionServesGuardedReadsFromItsOwnReadOnlyPool(t *testing.T) {
	f := newMCPFixture(t, nil)
	_, credential := f.grant(t)
	before := f.h.Runtime.Stat().AcquireCount()
	mcpBefore := f.pool.Stat().AcquireCount()
	reply, err := f.post(t.Context(), credential, mcpListBabies)
	if err != nil || reply.status != 200 || !strings.Contains(reply.body, `"revision"`) || reply.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("list_babies: %+v err=%v", reply, err)
	}
	// The credential check (one query) and the guarded read (one read-only
	// transaction) each use the dedicated MCP pool once.
	if f.pool.Stat().AcquireCount() != mcpBefore+2 {
		t.Fatalf("one MCP request used the dedicated MCP pool %d times", f.pool.Stat().AcquireCount()-mcpBefore)
	}
	// Quota reservations are operational metadata written through the
	// existing limiter on the runtime pool; the care read never uses it.
	t.Logf("runtime pool acquisitions for one MCP request (quota and authority): %d", f.h.Runtime.Stat().AcquireCount()-before)
	// A raw COMMIT inside a guarded callback cannot escape read-only mode:
	// the session default keeps the next transaction read-only too.
	err = f.comp.reader.WithIntegrationRead(t.Context(), credential, func(ctx context.Context, tx pgx.Tx, _ access.Grant) error {
		if _, err := tx.Exec(ctx, "COMMIT"); err != nil {
			return err
		}
		_, err := tx.Conn().Exec(ctx, "INSERT INTO families(id) VALUES (gen_random_uuid())")
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write after raw COMMIT err=%v, want read-only 25006", err)
	}
	var readOnly string
	if err := f.pool.QueryRow(t.Context(), "SHOW default_transaction_read_only").Scan(&readOnly); err != nil || readOnly != "on" {
		t.Fatalf("MCP session default_transaction_read_only=%q err=%v", readOnly, err)
	}
	// Telemetry: admission counts land on the shared collector under the
	// fixed mcp class; guard phases land only on the MCP phase collector.
	counts := f.metrics.Snapshot().Admission[diagnostics.MCPAdmission]
	if counts.Accepted != 1 || counts.Released != 1 || counts.Active != 0 || len(f.metrics.Snapshot().Phases) != 0 {
		t.Fatalf("shared collector=%+v", f.metrics.Snapshot())
	}
	phases := f.phases.Snapshot().Phases
	// One authority timing per guard use (HTTP check + the tool's read) and
	// one database timing for the read; integrity/fence captures flush once.
	if phases[diagnostics.Authority].Count != 2 || phases[diagnostics.Database].Count != 1 || phases[diagnostics.TransactionAcquire].Count != 1 {
		t.Fatalf("mcp phases=%+v", phases)
	}
	f.assertIdle(t)
}

func TestSaturatedMCPReadsLeaveSyncItsOwnRuntimeConnections(t *testing.T) {
	f := newMCPFixture(t, nil)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var wg sync.WaitGroup
	// Grants are security writes: issue them before reads hold the fence.
	var credentials []string
	for range 5 {
		_, credential := f.grant(t)
		credentials = append(credentials, credential)
	}
	fifth := credentials[4]
	for _, credential := range credentials[:4] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = f.comp.reader.WithIntegrationRead(ctx, credential, func(ctx context.Context, tx pgx.Tx, _ access.Grant) error {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
				return nil
			})
		}()
	}
	defer func() { close(release); wg.Wait() }()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("MCP reads did not start")
		}
	}
	if acquired := f.pool.Stat().AcquiredConns(); acquired != 4 {
		t.Fatalf("MCP pool acquired=%d, want saturated 4", acquired)
	}
	runtimeAcquired := f.h.Runtime.Stat().AcquiredConns()
	// Phone sync keeps its own runtime connections while MCP is saturated.
	started := time.Now()
	outcome, err := f.h.Service.Apply(t.Context(), f.h.Owner, f.h.FamilyID, mutationFor(t, f.h))
	if err != nil || outcome.Status != "applied" || time.Since(started) > 2*time.Second {
		t.Fatalf("sync under saturated MCP: %+v err=%v after %v", outcome, err, time.Since(started))
	}
	// A fifth MCP read waits on the MCP pool only and never spills over.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err = f.comp.reader.WithIntegrationRead(ctx, fifth, func(context.Context, pgx.Tx, access.Grant) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || f.h.Runtime.Stat().AcquiredConns() > runtimeAcquired {
		t.Fatalf("fifth MCP read err=%v runtime acquired %d -> %d", err, runtimeAcquired, f.h.Runtime.Stat().AcquiredConns())
	}
}

func mutationFor(t *testing.T, h *testkit.Harness) protocol.Mutation {
	t.Helper()
	m := testkit.MustMutation(t, "baby.json")
	m.MutationID = h.CommandID()
	m.EntityID = h.CommandID()
	return m
}

func TestManyCredentialsForOneFamilyStillHitTheFamilyQuota(t *testing.T) {
	f := newMCPFixture(t, func(cfg *config.Config, _ *mcpDependencies) {
		// The family dimension is lowered (as BT_RATE_LIMIT_MCP_FAMILY_COUNT
		// may) below the per-credential and account defaults.
		cfg.Limits.Policies[limits.ScopeMCPFamily] = limits.Policy{Count: 10, Window: time.Minute}
	})
	var credentials []string
	for range 6 {
		_, credential := f.grant(t)
		credentials = append(credentials, credential)
	}
	admitted, limited := 0, 0
	for i := range 18 {
		reply, err := f.post(t.Context(), credentials[i%len(credentials)], mcpListTools)
		switch {
		case err != nil:
			t.Fatal(err)
		case reply.status == 200:
			admitted++
		case reply.status == 429 && strings.Contains(reply.body, "rate_limited") && reply.header.Get("Retry-After") != "":
			limited++
		default:
			t.Fatalf("request %d: %+v", i, reply)
		}
	}
	if admitted != 10 || limited != 8 {
		t.Fatalf("family quota admitted=%d limited=%d, want 10/8", admitted, limited)
	}
	f.assertIdle(t)
}

func TestDefaultMCPQuotasCapOneOwnerAcrossCredentials(t *testing.T) {
	f := newMCPFixture(t, nil)
	var credentials []string
	for range 3 {
		_, credential := f.grant(t)
		credentials = append(credentials, credential)
	}
	perCredential := map[int]int{}
	admitted := 0
	for i := range 75 {
		reply, err := f.post(t.Context(), credentials[i%3], mcpListTools)
		if err != nil {
			t.Fatal(err)
		}
		if reply.status == 200 {
			admitted++
			perCredential[i%3]++
		} else if reply.status != 429 {
			t.Fatalf("request %d: %+v", i, reply)
		}
	}
	// 30 per integration would admit 75; the owner account's 60 binds first.
	if admitted != 60 {
		t.Fatalf("admitted=%d per credential=%v, want account quota 60", admitted, perCredential)
	}
	// One credential alone stops at its own 30.
	_, solo := f.grant(t)
	soloAdmitted := 0
	for range 32 {
		reply, err := f.post(t.Context(), solo, mcpListTools)
		if err != nil {
			t.Fatal(err)
		}
		if reply.status == 200 {
			soloAdmitted++
		}
	}
	if soloAdmitted != 0 {
		t.Fatalf("account quota exhausted, yet a new credential was admitted %d times", soloAdmitted)
	}
}

func TestEveryRequestOutcomeReleasesGlobalAndCredentialAdmission(t *testing.T) {
	f := newMCPFixture(t, nil)
	grantID, credential := f.grant(t)
	cases := []struct {
		name, credential, body string
		status                 int
	}{
		{"success", credential, mcpListBabies, 200},
		{"notification", credential, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202},
		{"oversized body", credential, strings.Repeat(" ", 33<<10), 413},
		{"unknown tool", credential, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"delete_family","arguments":{}}}`, 0},
		{"invalid credential", "btmcp1.60000000-0000-4000-8000-0000000000ff.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", mcpListTools, 401},
	}
	for _, tc := range cases {
		reply, err := f.post(t.Context(), tc.credential, tc.body)
		if err != nil || (tc.status != 0 && reply.status != tc.status) {
			t.Fatalf("%s: %+v err=%v", tc.name, reply, err)
		}
		f.assertIdle(t)
	}
	// Canceled mid-request: the client disconnects while the request runs.
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(time.Millisecond); cancel() }()
	_, _ = f.post(ctx, credential, mcpListBabies)
	f.assertIdle(t)
	// Revoked after authentication: 401, then slots are free.
	f.operator(t, "revoke", grantID, "")
	if reply, err := f.post(t.Context(), credential, mcpListBabies); err != nil || reply.status != 401 {
		t.Fatalf("revoked: %+v err=%v", reply, err)
	}
	f.assertIdle(t)
}

func TestPanickingQuotaReleasesGlobalAndCredentialAdmission(t *testing.T) {
	f := newMCPFixture(t, func(_ *config.Config, deps *mcpDependencies) { deps.Limiter = panicLimiter{} })
	_, credential := f.grant(t)
	reply, err := f.post(t.Context(), credential, mcpListTools)
	if err != nil || reply.status != http.StatusServiceUnavailable || reply.header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(reply.body, `"code":"unavailable"`) || !strings.Contains(reply.body, `"jsonrpc":"2.0"`) || strings.Contains(reply.body, "hunter2") {
		t.Fatalf("panicking quota: %+v err=%v, want safe JSON-RPC 503 with no-store", reply, err)
	}
	f.assertIdle(t)
}

// The production router mounts the composed handler without a Sync slot:
// with one Sync slot held and total 2, one MCP request still fits.
func TestRouterChargesExactlyOneMCPAdmissionPerRequest(t *testing.T) {
	var shared *admission.Controller
	f := newServedMCPFixture(t, func(cfg *config.Config, _ *mcpDependencies) {
		cfg.Admission = config.AdmissionConfig{Total: 2, MCPMax: 1}
	}, func(cfg config.Config, deps mcpDependencies) http.Handler {
		// Phone auth services require an HTTPS origin; MCP keeps the test's
		// plain-HTTP canonical origin so Host checks match the listener.
		appCfg := cfg
		appCfg.PublicOrigin, _ = url.Parse("https://sync.example.test")
		appAuthority, appFamilies, appReadiness := testComposition(t, appCfg, deps.Others[0])
		options, err := applicationOptions(appCfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), appReadiness, appAuthority, appFamilies, deps.Others[0], nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		deps.Admission = nil // attachMCP must use the router's shared controller
		// Admission accounting only: node readiness is proved by
		// TestMCPRefusedWhileNodeNotReady.
		options.Readiness = readinessFunc(func(context.Context) error { return nil })
		if err := attachMCP(cfg, &options, deps); err != nil || options.MCPHandler == nil {
			t.Fatalf("attach: %v", err)
		}
		shared = options.Admission
		return httpapi.New(options)
	})
	_, credential := f.grant(t)
	held, ok := shared.TryAcquire(admission.Sync)
	if !ok {
		t.Fatal("setup")
	}
	reply, err := f.post(t.Context(), credential, mcpListTools)
	if err != nil || reply.status != 200 {
		held()
		t.Fatalf("router charged a Sync slot for /mcp (or used another controller): %+v err=%v", reply, err)
	}
	// With the one MCP slot taken, /mcp is refused by the shared controller.
	mcpHeld, ok := shared.TryAcquire(admission.MCP)
	if !ok {
		t.Fatal("MCP slot not released after the request")
	}
	if reply, err := f.post(t.Context(), credential, mcpListTools); err != nil || reply.status != 503 || !strings.Contains(reply.body, "server_busy") {
		t.Fatalf("saturated shared MCP class: %+v err=%v", reply, err)
	}
	mcpHeld()
	held()
}

// Revocation (a security write) must complete while four max-duration MCP
// reads are continuously in flight: no read may starve the writer.
func TestRevocationCompletesUnderContinuousMCPReadLoad(t *testing.T) {
	f := newMCPFixture(t, nil)
	var readers []string
	for range 4 {
		_, credential := f.grant(t)
		readers = append(readers, credential)
	}
	victim, _ := f.grant(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads atomic.Int64
	const hold = time.Second
	for i, credential := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Staggered so that some read is always in flight.
			time.Sleep(time.Duration(i) * hold / 4)
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = f.comp.reader.WithIntegrationRead(ctx, credential, func(ctx context.Context, tx pgx.Tx, _ access.Grant) error {
					reads.Add(1)
					_, err := tx.Exec(ctx, "SELECT pg_sleep($1)", hold.Seconds())
					return err
				})
				cancel()
			}
		}()
	}
	time.Sleep(1500 * time.Millisecond)
	started := time.Now()
	done := make(chan struct{})
	var revokeErr error
	go func() { defer close(done); revokeErr = f.operatorErr(context.Background(), "revoke", victim, "") }()
	var latency time.Duration
	select {
	case <-done:
		latency = time.Since(started)
	case <-time.After(15 * time.Second):
		latency = time.Since(started)
	}
	close(stop)
	wg.Wait()
	<-done
	if revokeErr != nil {
		t.Fatal(revokeErr)
	}
	t.Logf("revocation latency under 4 continuous 1s MCP reads: %v (reads completed: %d)", latency, reads.Load())
	// Bounded by one in-flight read (1 s), never by the stream of new reads.
	if latency > 2*hold+500*time.Millisecond {
		t.Fatalf("revocation starved for %v behind continuous MCP reads", latency)
	}
}

// Four overlapping reads that run to the request deadline delay a
// revocation by at most their remaining time (under five seconds).
func TestRevocationWaitsAtMostForMaxDurationMCPReads(t *testing.T) {
	f := newMCPFixture(t, nil)
	var readers []string
	for range 4 {
		_, credential := f.grant(t)
		readers = append(readers, credential)
	}
	victim, _ := f.grant(t)
	entered := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, credential := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The MCP handler's work deadline is 4.75 s after request entry.
			ctx, cancel := context.WithTimeout(context.Background(), 4750*time.Millisecond)
			defer cancel()
			_ = f.comp.reader.WithIntegrationRead(ctx, credential, func(ctx context.Context, tx pgx.Tx, _ access.Grant) error {
				entered <- struct{}{}
				_, err := tx.Exec(ctx, "SELECT pg_sleep(10)")
				return err
			})
		}()
	}
	for range 4 {
		<-entered
	}
	started := time.Now()
	err := f.operatorErr(t.Context(), "revoke", victim, "")
	latency := time.Since(started)
	wg.Wait()
	t.Logf("revocation latency behind 4 overlapping max-duration MCP reads: %v", latency)
	// Bounded by the reads' own 4.75 s deadline (plus scheduling slack).
	if err != nil || latency > 4750*time.Millisecond+500*time.Millisecond {
		t.Fatalf("revocation err=%v after %v", err, latency)
	}
}

// switchableReadiness lets a test install the node readiness after the
// composed MCP fixture (and its database) exists.
type switchableReadiness struct {
	mu     sync.Mutex
	target mcpapi.Readiness
}

func (r *switchableReadiness) set(target mcpapi.Readiness) {
	r.mu.Lock()
	r.target = target
	r.mu.Unlock()
}

func (r *switchableReadiness) Check(ctx context.Context) error {
	r.mu.Lock()
	target := r.target
	r.mu.Unlock()
	if target == nil {
		return errors.New("readiness not installed")
	}
	return target.Check(ctx)
}

// MCP fails closed exactly when phones do: with the server's real database
// readiness gating /mcp, a server_state actor-key commitment mismatch and a
// recovery-epoch rotation on a running node (before any restart) both refuse
// every MCP request with the safe no-store 503, before admission or
// credential work. Legacy failed MCP closed with the node.
func TestMCPRefusedWhileNodeNotReady(t *testing.T) {
	gate := &switchableReadiness{}
	f := newMCPFixture(t, func(_ *config.Config, deps *mcpDependencies) { deps.Readiness = gate })
	cfg := mcpUnitConfig(t)
	cfg.Auth.IdentityKeys.Current.ID, cfg.Auth.SessionTokenKeys.Current.ID = "identity.v1", "session.v1"
	cfg.Auth.DeviceContinuityKeys.Current.ID, cfg.Auth.InvitationKeys.Current.ID = "device.v1", "invitation.v1"
	if result, err := authority.InitServerState(t.Context(), f.h.Migrator, testkit.InstanceID, cfg.Auth.ActorIDPepper); err != nil || result != authority.InitBound {
		t.Fatalf("bind server_state: %s %v", result, err)
	}
	readiness := readinessOver(t, cfg, f.h.Runtime, f.h.Maintenance)
	if err := readiness.inventory.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	gate.set(readiness)
	_, credential := f.grant(t)
	assertRead := func(name string, wantStatus int) {
		t.Helper()
		// The monitor's next tick: requests read its cached outcome.
		_ = readiness.Refresh(t.Context())
		reply, err := f.post(t.Context(), credential, mcpListBabies)
		if err != nil || reply.status != wantStatus || reply.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %+v err=%v, want %d with no-store", name, reply, err, wantStatus)
		}
		if wantStatus == http.StatusServiceUnavailable && (!strings.Contains(reply.body, `"code":"unavailable"`) || !strings.Contains(reply.body, `"jsonrpc":"2.0"`)) {
			t.Fatalf("%s: unsafe refusal body %s", name, reply.body)
		}
		f.assertIdle(t)
	}
	assertRead("ready node", http.StatusOK)
	var commitment []byte
	if err := f.h.Migrator.QueryRow(t.Context(), "SELECT actor_key_commitment FROM server_state WHERE singleton").Scan(&commitment); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Migrator.Exec(t.Context(), "UPDATE server_state SET actor_key_commitment=$1 WHERE singleton", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	assertRead("actor-ID key mismatch", http.StatusServiceUnavailable)
	if _, err := f.h.Migrator.Exec(t.Context(), "UPDATE server_state SET actor_key_commitment=$1 WHERE singleton", commitment); err != nil {
		t.Fatal(err)
	}
	assertRead("restored commitment", http.StatusOK)
	// `authority rotate-epoch` on the running node: the grant is bound to
	// epoch 1 and the node is still configured for it, but server_state now
	// says epoch 2, so the node is not ready and MCP refuses before restart.
	if _, err := authority.RotateRecoveryEpoch(t.Context(), f.h.Migrator, testkit.InstanceID, 1); err != nil {
		t.Fatal(err)
	}
	assertRead("rotated epoch before restart", http.StatusServiceUnavailable)
}
