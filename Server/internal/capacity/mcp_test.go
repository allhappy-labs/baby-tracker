package capacity

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/config"
)

func TestMCPDatabaseProfileAddsOnlyTheDedicatedPool(t *testing.T) {
	env, budget, err := databaseProfile("explicit-mcp")
	if err != nil || budget != (config.DatabasePools{Runtime: 16, Maintenance: 2, MCP: 4, ServerMax: 64, Reserved: 8}) {
		t.Fatalf("explicit-mcp=%v %+v %v", env, budget, err)
	}
	syncOnly, _, _ := databaseProfile("explicit")
	for key, value := range syncOnly {
		if key != "BT_DB_MCP_MAX_CONNS" && env[key] != value {
			t.Fatalf("matched cells differ on %s: %s vs %s", key, env[key], value)
		}
	}
}

// Lowered MCP concurrency is a named profile, so the dedicated pool, the
// global MCP admission maximum and result provenance always agree.
func TestLoweredMCPConcurrencyProfilesChangeOnlyTheMCPPoolAndAdmission(t *testing.T) {
	syncOnly, _, _ := databaseProfile("explicit")
	for profile, want := range map[string]int32{"explicit-mcp": 4, "explicit-mcp-3": 3, "explicit-mcp-2": 2, "explicit-mcp-1": 1} {
		env, budget, err := databaseProfile(profile)
		if err != nil || budget != (config.DatabasePools{Runtime: 16, Maintenance: 2, MCP: want, ServerMax: 64, Reserved: 8}) {
			t.Fatalf("%s=%v %+v %v", profile, env, budget, err)
		}
		for key, value := range syncOnly {
			if key != "BT_DB_MCP_MAX_CONNS" && env[key] != value {
				t.Fatalf("%s differs from explicit on %s", profile, key)
			}
		}
		if got := mcpAdmissionMax(budget); got != strconv.Itoa(int(want)) {
			t.Fatalf("%s admission max=%q want %d", profile, got, want)
		}
		w := validMCPWorkload(MCPRead)
		w.DatabaseProfile = profile
		if err := w.Validate(FixtureManifest{Devices: 64}); err != nil {
			t.Fatalf("%s read cell rejected: %v", profile, err)
		}
		r := coherentCell()
		r.DatabaseProfile = profile
		r.DatabasePools = &budget
		if !r.provenanceValid() {
			t.Fatalf("%s provenance rejected", profile)
		}
	}
	for _, profile := range []string{"explicit-mcp-0", "explicit-mcp-5", "explicit-mcp-", "explicit-mcp-04"} {
		if _, _, err := databaseProfile(profile); err == nil {
			t.Fatalf("%s accepted", profile)
		}
	}
	if got := mcpAdmissionMax(config.DatabasePools{Runtime: 16, Maintenance: 2, ServerMax: 64, Reserved: 8}); got != "" {
		t.Fatalf("sync-only profile set MCP admission %q", got)
	}
}

func TestFixtureSpecBoundsMCPGrants(t *testing.T) {
	for _, grants := range []int{-1, 5} {
		if (FixtureSpec{Families: 1, DevicesPerFamily: 2, MCPGrantsPerFamily: grants}).validate() == nil {
			t.Fatalf("grants per family %d accepted", grants)
		}
	}
	spec := FixtureSpec{Families: 3, DevicesPerFamily: 2, MCPGrantsPerFamily: 1}
	if spec.validate() != nil {
		t.Fatal("one grant per family rejected")
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	source := strings.Repeat("a", 40)
	m := FixtureManifest{Version: 1, SourceSHA: source, InputSHA256: fixtureInputDigest(spec, source), Families: 3, Devices: 6, MCPGrants: 3, ActualAuthorityEvents: 9, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	if err := m.Validate(spec, source, now); err != nil {
		t.Fatal(err)
	}
	m.MCPGrants = 2
	if m.Validate(spec, source, now) == nil {
		t.Fatal("manifest grant count mismatch accepted")
	}
	if fixtureInputDigest(spec, source) == fixtureInputDigest(FixtureSpec{Families: 3, DevicesPerFamily: 2}, source) {
		t.Fatal("grant issuance does not change the fixture identity")
	}
}

func validMCPWorkload(mode string) Workload {
	w := Workload{Devices: 32, ActiveWriters: 16, EmptyPullInterval: 300 * time.Second, WritesPerWriterPerMinute: 15, Warmup: 30 * time.Second, Measure: 60 * time.Second, DatabaseProfile: "explicit-mcp", MCPMode: mode}
	switch mode {
	case MCPRead:
		w.MCPClients, w.MCPCallsPerClientPerMinute = 4, 20
	case MCPAbusive:
		w.MCPClients, w.MCPAbuseConcurrency = 4, 8
	}
	return w
}

func TestWorkloadValidatesMatchedMCPCells(t *testing.T) {
	m := FixtureManifest{Devices: 64}
	syncOnly := validMCPWorkload("")
	syncOnly.DatabaseProfile = "explicit"
	for name, w := range map[string]Workload{"sync-only": syncOnly, "idle": validMCPWorkload(MCPIdle), "read": validMCPWorkload(MCPRead), "abusive": validMCPWorkload(MCPAbusive)} {
		if err := w.Validate(m); err != nil {
			t.Fatalf("%s rejected: %v", name, err)
		}
	}
	bad := map[string]func(*Workload){
		"unknown mode":            func(w *Workload) { w.MCPMode = "write" },
		"mcp without mcp pool":    func(w *Workload) { w.DatabaseProfile = "explicit" },
		"idle with clients":       func(w *Workload) { w.MCPMode, w.MCPClients = MCPIdle, 1 },
		"read without clients":    func(w *Workload) { w.MCPClients = 0 },
		"read above quota":        func(w *Workload) { w.MCPCallsPerClientPerMinute = 31 },
		"read with abuse":         func(w *Workload) { w.MCPAbuseConcurrency = 2 },
		"abusive single worker":   func(w *Workload) { w.MCPMode, w.MCPCallsPerClientPerMinute, w.MCPAbuseConcurrency = MCPAbusive, 0, 1 },
		"too many clients":        func(w *Workload) { w.MCPClients = 65 },
		"sync-only with mcp pool": func(w *Workload) { w.MCPMode, w.MCPClients, w.MCPCallsPerClientPerMinute = "", 0, 0 },
	}
	for name, mutate := range bad {
		w := validMCPWorkload(MCPRead)
		mutate(&w)
		if w.Validate(m) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestMCPResponseClassificationKeepsRefusalsVisible(t *testing.T) {
	rpcError := func(code string) string {
		return `{"jsonrpc":"2.0","id":null,"error":{"code":-32029,"message":"x","data":{"code":"` + code + `"}}}`
	}
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}],"structuredContent":{}}}`, mcpAdmitted},
		{200, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}],"isError":true}}`, mcpToolError},
		{429, rpcError("rate_limited"), mcpRateLimited},
		{429, rpcError("concurrency_limited"), mcpConcurrencyLimited},
		{503, rpcError("server_busy"), mcpServerBusy},
		{503, rpcError("timeout"), mcpTimeout},
		{503, rpcError("unavailable"), mcpUnavailable},
		{401, rpcError("unauthorized"), mcpUnauthorized},
		{200, `not json`, mcpOther},
		{500, ``, mcpOther},
	} {
		if got := classifyMCPResponse(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%d %s => %s want %s", tc.status, tc.body, got, tc.want)
		}
	}
}

// mcpTestTarget routes https://sync.example.test to a local test server, as
// the real target routes it to the owned proxy.
func mcpTestTarget(t *testing.T, handler http.Handler) *runTarget {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	address := strings.TrimPrefix(server.URL, "http://")
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	return &runTarget{HTTP: &http.Client{Timeout: 5 * time.Second, Transport: transport}, mcpBase: "http://sync.example.test"}
}

func TestMCPCallSendsOneStatelessAuthenticatedToolCall(t *testing.T) {
	var seen atomic.Int32
	target := mcpTestTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		var message struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer btmcp1.secret" || r.Header.Get("Mcp-Protocol-Version") != "2025-11-25" ||
			json.Unmarshal(raw, &message) != nil || message.Method != "tools/call" || r.Header.Get("Mcp-Session-Id") != "" {
			http.Error(w, "bad", 400)
			return
		}
		if message.Params.Name != "list_babies" && message.Params.Arguments["baby_id"] != "baby" {
			http.Error(w, "bad", 400)
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{}}}`)
	}))
	for call := range 3 {
		outcome, latency := mcpCall(t.Context(), target, mcpCredential{Credential: "btmcp1.secret", BabyID: "baby"}, call, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
		if outcome != mcpAdmitted || latency <= 0 {
			t.Fatalf("call %d outcome=%s", call, outcome)
		}
	}
	if seen.Load() != 3 {
		t.Fatalf("server saw %d requests", seen.Load())
	}
}

func TestMCPEvidenceAccountingAndPercentiles(t *testing.T) {
	e := &MCPEvidence{Mode: MCPRead, Clients: 1}
	for i, outcome := range []string{mcpAdmitted, mcpAdmitted, mcpRateLimited, mcpServerBusy, mcpTimeout, mcpConcurrencyLimited, mcpToolError} {
		e.record(outcome, time.Duration(i+1)*100*time.Millisecond)
	}
	e.MeasurementSeconds = 60
	e.finalize()
	if !e.coherent() || e.Attempts != 7 || e.Admitted != 2 || e.Refused != 3 || e.Errors != 2 || len(e.AdmittedLatencySeconds) != 2 {
		t.Fatalf("evidence=%+v", e)
	}
	if e.AdmittedP95Seconds != 0.2 || e.AdmittedPerMinute != 2 || e.RefusedPerMinute != 3 {
		t.Fatalf("rates=%+v", e)
	}
	e.Admitted++
	if e.coherent() {
		t.Fatal("incoherent MCP evidence accepted")
	}
}

func TestMCPClientsOfferPacedAndAbusiveLoadAndRecordEveryOutcome(t *testing.T) {
	var inFlight, served atomic.Int32
	target := mcpTestTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		// One in-flight request per credential, like the real gate.
		if inFlight.Add(1) > 1 {
			inFlight.Add(-1)
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32029,"message":"x","data":{"code":"concurrency_limited"}}}`)
			return
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{}}}`)
	}))
	target.MCPCredentials = []mcpCredential{{Credential: "btmcp1.a", BabyID: "baby"}}
	run := func(w Workload, measured bool) *MCPEvidence {
		evidence := &MCPEvidence{Mode: w.MCPMode, Clients: w.MCPClients}
		window := newMeasurementWindow(t.Context(), 600*time.Millisecond, time.Second)
		defer window.Close()
		var mu sync.Mutex
		var wg sync.WaitGroup
		runMCPClients(w, target, window, measured, evidence, &mu, &wg)
		wg.Wait()
		return evidence
	}
	read := validMCPWorkload(MCPRead)
	read.MCPClients, read.MCPCallsPerClientPerMinute = 1, 30
	if e := run(read, true); e.Attempts != 1 || e.Admitted != 1 || !e.coherent() {
		t.Fatalf("paced read=%+v", e)
	}
	abusive := validMCPWorkload(MCPAbusive)
	abusive.MCPClients, abusive.MCPAbuseConcurrency = 1, 4
	e := run(abusive, true)
	if e.Attempts < 20 || e.Admitted == 0 || e.Outcomes[mcpConcurrencyLimited] == 0 || e.Refused != e.Outcomes[mcpConcurrencyLimited] || !e.coherent() {
		t.Fatalf("abusive=%+v", e)
	}
	before := served.Load()
	if e := run(abusive, false); e.Attempts != 0 || served.Load() == before {
		t.Fatalf("warmup recorded %d attempts or offered no load", e.Attempts)
	}
	if e := run(validMCPWorkload(MCPIdle), true); e.Attempts != 0 {
		t.Fatal("idle cell offered MCP load")
	}
}

func TestMCPSetupProvesTheRouteMatchesTheCell(t *testing.T) {
	routes := map[bool]*runTarget{}
	for _, mounted := range []bool{true, false} {
		routes[mounted] = mcpTestTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case !mounted:
				http.NotFound(w, r)
			case r.Header.Get("Authorization") == "":
				w.WriteHeader(401)
			default:
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{}}}`)
			}
		}))
		routes[mounted].MCPCredentials = []mcpCredential{{Credential: "btmcp1.a", BabyID: "baby"}}
	}
	syncOnly := validMCPWorkload("")
	read := validMCPWorkload(MCPRead)
	read.MCPClients = 1
	evidence := &MCPEvidence{}
	if mcpSetup(t.Context(), syncOnly, routes[false], evidence) != nil || mcpSetup(t.Context(), read, routes[true], evidence) != nil || evidence.SetupProofs != 1 {
		t.Fatalf("matching cells rejected: %+v", evidence)
	}
	if mcpSetup(t.Context(), syncOnly, routes[true], &MCPEvidence{}) == nil || mcpSetup(t.Context(), read, routes[false], &MCPEvidence{}) == nil {
		t.Fatal("cell/route mismatch accepted")
	}
	read.MCPClients = 2
	if mcpSetup(t.Context(), read, routes[true], &MCPEvidence{}) == nil {
		t.Fatal("missing credentials accepted")
	}
}
