package mcpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/familyread"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testCredential = "btmcp1.60000000-0000-4000-8000-00000000000a.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testGrantID    = "60000000-0000-4000-8000-00000000000a"
	testBabyID     = "10000000-0000-4000-8000-000000000001"
	phoneToken     = "q83vEjRWeJASNFZ4kBI0VniQEjRWeJASNFZ4kBI0VngX"
)

var testNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

type fakeAuth struct {
	calls atomic.Int32
	err   error
	after func(context.Context)
}

func (f *fakeAuth) CheckIntegration(ctx context.Context, credential string) (authority.IntegrationIdentity, error) {
	f.calls.Add(1)
	if f.err != nil {
		return authority.IntegrationIdentity{}, f.err
	}
	if credential != testCredential {
		return authority.IntegrationIdentity{}, authority.ErrIntegrationUnauthorized
	}
	if f.after != nil {
		f.after(ctx)
	}
	return authority.IntegrationIdentity{GrantID: testGrantID, AccountID: "a", FamilyID: "f", ExpiresAt: testNow.Add(time.Hour)}, nil
}

type fakeReader struct {
	calls   atomic.Int32
	babies  func(context.Context) (familyread.BabiesResult, error)
	history func(context.Context, familyread.HistoryQuery, familyread.HistoryBudget) (familyread.HistoryResult, error)
	summary func(context.Context, familyread.SummaryQuery) (familyread.SummaryResult, error)
}

func (f *fakeReader) ListBabies(ctx context.Context, credential string) (familyread.BabiesResult, error) {
	f.calls.Add(1)
	if credential != testCredential {
		return familyread.BabiesResult{}, authority.ErrIntegrationUnauthorized
	}
	if f.babies != nil {
		return f.babies(ctx)
	}
	return familyread.BabiesResult{Revision: 7, Babies: []protocol.CanonicalEntity{babyEntity("Élie")}}, nil
}

func (f *fakeReader) HistoryWithBudget(ctx context.Context, credential string, q familyread.HistoryQuery, fits familyread.HistoryBudget) (familyread.HistoryResult, error) {
	f.calls.Add(1)
	if credential != testCredential {
		return familyread.HistoryResult{}, authority.ErrIntegrationUnauthorized
	}
	if fits == nil {
		return familyread.HistoryResult{}, authority.ErrUnavailable
	}
	if f.history != nil {
		return f.history(ctx, q, fits)
	}
	result := sampleHistory(3, "note")
	if ok, err := fits(result); err != nil || !ok {
		return familyread.HistoryResult{}, familyread.ErrResultTooLarge
	}
	return result, nil
}

func (f *fakeReader) Summary(ctx context.Context, credential string, q familyread.SummaryQuery) (familyread.SummaryResult, error) {
	f.calls.Add(1)
	if credential != testCredential {
		return familyread.SummaryResult{}, authority.ErrIntegrationUnauthorized
	}
	if f.summary != nil {
		return f.summary(ctx, q)
	}
	return sampleSummary(q), nil
}

type fakeQuota struct {
	calls    atomic.Int32
	decision limits.Decision
	err      error
}

func (f *fakeQuota) Charge(ctx context.Context, identity authority.IntegrationIdentity) (limits.Decision, error) {
	f.calls.Add(1)
	if identity.GrantID != testGrantID {
		return limits.Decision{}, fmt.Errorf("quota charged without authenticated identity")
	}
	if f.err != nil {
		return limits.Decision{}, f.err
	}
	if f.decision == (limits.Decision{}) {
		return limits.Decision{Allowed: true, Remaining: 10}, nil
	}
	return f.decision, nil
}

// testGate is the production keyed registry with the global MCP bound.
func testGate() *admission.Keyed {
	gate, err := admission.NewKeyed(4)
	if err != nil {
		panic(err)
	}
	return gate
}

type env struct {
	server     *httptest.Server
	url        string
	origin     string
	controller *admission.Controller
	auth       *fakeAuth
	reader     *fakeReader
	quota      *fakeQuota
	gate       *admission.Keyed
	handler    *handler
}

// newEnv serves the handler over real TCP so deadlines, keep-alive and
// disconnects use a real net/http connection.
func newEnv(t *testing.T, configure func(*Options)) *env {
	t.Helper()
	return newEnvServer(t, configure, nil)
}

// newEnvServer lets a test adjust the server (handler wrapper, connection
// hooks) before it starts accepting connections.
func newEnvServer(t *testing.T, configure func(*Options), adjust func(*http.Server)) *env {
	t.Helper()
	controller, err := admission.New(8, 4)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{controller: controller, auth: &fakeAuth{}, reader: &fakeReader{}, quota: &fakeQuota{}, gate: testGate()}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.origin = "http://" + listener.Addr().String()
	options := Options{Reader: e.reader, Authenticator: e.auth, Admission: controller, Gate: e.gate, Quota: e.quota, CanonicalOrigin: e.origin}
	if configure != nil {
		configure(&options)
	}
	served, err := NewHandler(options)
	if err != nil {
		t.Fatal(err)
	}
	e.handler = served.(*handler)
	// Another route shares keep-alive connections with /mcp.
	mux := http.NewServeMux()
	mux.Handle("/mcp", served)
	mux.HandleFunc("/other", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "other") })
	e.server = httptest.NewUnstartedServer(mux)
	e.server.Listener.Close()
	e.server.Listener = listener
	if adjust != nil {
		adjust(e.server.Config)
	}
	e.server.Start()
	t.Cleanup(e.server.Close)
	e.url = e.server.URL + "/mcp"
	return e
}

// assertIdle proves every admission slot was returned.
func (e *env) assertIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var releases []func()
		ok := true
		for range 4 {
			release, acquired := e.controller.TryAcquire(admission.MCP)
			if !acquired {
				ok = false
				break
			}
			releases = append(releases, release)
		}
		for _, release := range releases {
			release()
		}
		if ok {
			release, acquired := e.gate.TryAcquire(testGrantID)
			if acquired {
				release()
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("admission or credential gate slot leaked")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
	mu    sync.Mutex
	seen  []*http.Response
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	response, err := b.base.RoundTrip(clone)
	if err == nil {
		b.mu.Lock()
		b.seen = append(b.seen, response)
		b.mu.Unlock()
	}
	return response, err
}

func (b *bearerTransport) responses() []*http.Response {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*http.Response(nil), b.seen...)
}

// connect drives the official SDK client. A current version uses discover; a
// forced 2025-11-25 version uses initialize/initialized.
func (e *env) connect(t *testing.T, version, token string) (*mcp.ClientSession, *bearerTransport) {
	t.Helper()
	transport := &bearerTransport{token: token, base: &http.Transport{}}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcpapi-test", Version: "1"}, nil)
	httpClient := &http.Client{Timeout: 10 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: e.url, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1},
		&mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("connect %s: %v", version, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, transport
}

type rawResponse struct {
	status int
	header http.Header
	body   []byte
}

func (r rawResponse) String() string { return fmt.Sprintf("%d %v %s", r.status, r.header, r.body) }

func legacyHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
		"Authorization": "Bearer " + testCredential, "Mcp-Protocol-Version": ProtocolLegacy}
}

func currentHeaders(method, name string) map[string]string {
	headers := legacyHeaders()
	headers["Mcp-Protocol-Version"] = ProtocolCurrent
	headers["Mcp-Method"] = method
	if name != "" {
		headers["Mcp-Name"] = name
	}
	return headers
}

const currentMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func (e *env) do(t *testing.T, method, body string, headers map[string]string) rawResponse {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, e.url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		if value != "" {
			request.Header.Set(name, value)
		}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rawResponse{status: response.StatusCode, header: response.Header, body: data}
}

func (e *env) post(t *testing.T, body string) rawResponse {
	t.Helper()
	return e.do(t, http.MethodPost, body, legacyHeaders())
}

func toolCall(id any, name, arguments string) string {
	rawID, _ := json.Marshal(id)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, rawID, name, arguments)
}

func currentToolCall(id any, name, arguments string) string {
	rawID, _ := json.Marshal(id)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{%s,"name":%q,"arguments":%s}}`, rawID, currentMeta, name, arguments)
}

func requireNoStore(t *testing.T, r rawResponse) {
	t.Helper()
	if r.header.Get("Cache-Control") != "no-store" || len(r.header.Values("Cache-Control")) != 1 {
		t.Fatalf("Cache-Control=%q in %s", r.header.Values("Cache-Control"), r)
	}
	if r.header.Get("Mcp-Session-Id") != "" || r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("session or CORS header leaked: %s", r)
	}
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func decodeEnvelope(t *testing.T, r rawResponse) rpcEnvelope {
	t.Helper()
	var envelope rpcEnvelope
	decoder := json.NewDecoder(bytes.NewReader(r.body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("decode %s: %v", r, err)
	}
	return envelope
}

// errorCode returns the safe code carried in a JSON-RPC error's data.
func errorCode(t *testing.T, r rawResponse) string {
	t.Helper()
	envelope := decodeEnvelope(t, r)
	if envelope.Error == nil {
		t.Fatalf("not an error: %s", r)
	}
	var data struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(envelope.Error.Data, &data); err != nil {
		t.Fatalf("error data %s: %v", r, err)
	}
	return data.Code
}

type toolEnvelope struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
	ResultType        string          `json:"resultType"`
	Meta              map[string]any  `json:"_meta"`
}

func decodeTool(t *testing.T, r rawResponse) toolEnvelope {
	t.Helper()
	envelope := decodeEnvelope(t, r)
	if envelope.Error != nil || r.status != http.StatusOK {
		t.Fatalf("tool call failed: %s", r)
	}
	var result toolEnvelope
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func toolErrorCode(t *testing.T, result toolEnvelope) string {
	t.Helper()
	if !result.IsError || len(result.Content) != 1 || result.StructuredContent != nil {
		t.Fatalf("not a tool error: %+v", result)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &body); err != nil || body.Error.Message == "" {
		t.Fatalf("tool error text %q: %v", result.Content[0].Text, err)
	}
	return body.Error.Code
}

func babyEntity(name string) protocol.CanonicalEntity {
	return protocol.CanonicalEntity{EntityKind: "baby", Meta: entityMeta(testBabyID),
		Payload: protocol.BabyPayload{Name: name, BirthDate: "2026-01-02", SortOrder: 0}}
}

func entityMeta(id string) protocol.EntityMeta {
	return protocol.EntityMeta{ID: id, FamilyID: "20000000-0000-4000-8000-000000000001", EntityVersion: 1,
		CreatorAccountID: "30000000-0000-4000-8000-000000000001", LastEditorAccountID: "30000000-0000-4000-8000-000000000001",
		ClientMutationTime: "2026-09-10T08:00:00.000000Z", AcceptedAt: "2026-09-10T08:00:01.000000Z"}
}

func diaperEntry(i int, note string) familyread.HistoryEntry {
	id := fmt.Sprintf("40000000-0000-4000-8000-%012x", i)
	start := testNow.Add(-time.Duration(100-i) * time.Minute)
	payload := protocol.DiaperPayload{BabyID: testBabyID, StartedAt: start.Format(time.RFC3339Nano), TimeZoneID: "Europe/Zurich", Note: &note, Contents: "wet"}
	return familyread.HistoryEntry{Kind: "diaper", ID: id, Start: start,
		Entity: protocol.CanonicalEntity{EntityKind: "diaper", Meta: entityMeta(id), Payload: payload}}
}

func sampleHistory(entries int, note string) familyread.HistoryResult {
	result := familyread.HistoryResult{Revision: 42, AsOf: testNow, Entries: []familyread.HistoryEntry{}, ActiveTimers: []familyread.ActiveTimer{}}
	for i := range entries {
		result.Entries = append(result.Entries, diaperEntry(i, note))
	}
	end := testNow.Add(-time.Hour)
	result.Entries = append(result.Entries, familyread.HistoryEntry{Kind: "sleep", ID: "50000000-0000-4000-8000-000000000001", Start: testNow.Add(-2 * time.Hour), End: &end,
		Session: &familyread.Session{ID: "50000000-0000-4000-8000-000000000001", EarliestMemberStart: testNow.Add(-2 * time.Hour)},
		Entity:  protocol.CanonicalEntity{EntityKind: "sleep", Meta: entityMeta("50000000-0000-4000-8000-000000000001"), Payload: protocol.SleepPayload{BabyID: testBabyID, StartedAt: "2026-09-10T10:00:00Z"}}})
	result.ActiveTimers = append(result.ActiveTimers, familyread.ActiveTimer{ID: "70000000-0000-4000-8000-000000000001", TimerKind: "sleep", StartedAt: testNow.Add(-5 * time.Minute),
		Entity: protocol.CanonicalEntity{EntityKind: "timer", Meta: entityMeta("70000000-0000-4000-8000-000000000001"), Payload: protocol.TimerStartPayload{BabyID: testBabyID, StartedAt: "2026-09-10T11:55:00Z"}}})
	return result
}

func sampleSummary(q familyread.SummaryQuery) familyread.SummaryResult {
	volume := familyread.Decimal6(1_500_000)
	day := familyread.SummaryDay{Date: "2026-09-10", Start: q.Start, End: q.End, ElapsedSeconds: familyread.Decimal6(86400_000000),
		SummaryCounts: familyread.SummaryCounts{Feed: familyread.FeedTotals{Sessions: 1, LoggedFeeds: 2, BottleVolumeObservations: 1, KnownBottleVolumeML: &volume},
			Sleep: familyread.SleepTotals{Sessions: 1, CompletedSeconds: familyread.Decimal6(3600_500000)}, Diapers: 3}}
	return familyread.SummaryResult{BabyID: q.BabyID, Start: q.Start, End: q.End, Timezone: q.Timezone, ElapsedSeconds: day.ElapsedSeconds, Revision: 42, AsOf: testNow,
		Totals: day.SummaryCounts, Days: []familyread.SummaryDay{day}, SupplementsByName: []familyread.SupplementDoses{{Name: "Vitamin D", Doses: 1}},
		Measurements: []familyread.MeasurementObservation{}, ActiveTimers: []familyread.ActiveTimer{}}
}
