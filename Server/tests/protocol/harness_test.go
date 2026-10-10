//go:build identitytest

package protocol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/delivery"
	"babytracker/server/internal/httpapi"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testidentity"
	"babytracker/server/internal/testkit"
)

type ready struct{}

func (ready) Check(context.Context) error { return nil }

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

type protocolFixture struct {
	t                *testing.T
	h                *testkit.Harness
	hub              *realtime.Hub
	server           *httptest.Server
	proxy            *httptest.Server
	issuer           *testidentity.Issuer
	owner, caregiver *ProtocolClient
	dropNextSync     atomic.Bool
	droppedMu        sync.Mutex
	droppedSyncBody  []byte
}

type ProtocolClient struct {
	fixture          *protocolFixture
	principal        access.Principal
	token            string
	refreshToken     string
	deviceCredential string
	cursor           *protocol.Cursor
	subject          string
	frozen           map[string][]byte
}

func newProtocolFixture(t *testing.T) *protocolFixture {
	t.Helper()
	issuer, err := testidentity.New("protocol-e2e", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = issuer.JWKS(); err != nil {
		t.Fatal(err)
	}
	h := testkit.NewHosted(t)
	verifier, err := issuer.Verifier("com.example.babytracker")
	if err != nil {
		t.Fatal(err)
	}
	h.ConfigureSignedIdentity(t, verifier)
	authorizer, err := realtime.NewAuthorityAuthorizer(h.Authority)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Authorizer: authorizer, Invalidations: h.Authority, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &protocolFixture{t: t, h: h, hub: hub, issuer: issuer}
	// As production composes it: each write's post-commit high water goes to
	// the hub; the write transaction notifies nothing.
	h.SetPublisher(t, delivery.NewObserver(hub, nil))
	fixture.startServer()
	fixture.startProxy()
	fixture.owner = &ProtocolClient{fixture: fixture, principal: h.Owner, token: h.AccessToken("owner"), refreshToken: h.RefreshToken("owner"), deviceCredential: h.DeviceCredential("owner"), subject: "owner", frozen: map[string][]byte{}}
	fixture.caregiver = &ProtocolClient{fixture: fixture, principal: h.Caregiver, token: h.AccessToken("caregiver"), refreshToken: h.RefreshToken("caregiver"), deviceCredential: h.DeviceCredential("caregiver"), subject: "caregiver", frozen: map[string][]byte{}}
	t.Cleanup(func() {
		fixture.proxy.Close()
		fixture.server.Close()
		_ = fixture.hub.Shutdown(context.Background())
	})
	return fixture
}

func (fixture *protocolFixture) startProxy() {
	fixture.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		target, err := url.Parse(fixture.server.URL)
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		out := request.Clone(request.Context())
		out.URL.Scheme, out.URL.Host, out.Host, out.RequestURI = target.Scheme, target.Host, target.Host, ""
		response, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if request.URL.Path == "/v1/sync" && fixture.dropNextSync.CompareAndSwap(true, false) {
			raw, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			fixture.droppedMu.Lock()
			fixture.droppedSyncBody = append([]byte(nil), raw...)
			fixture.droppedMu.Unlock()
			connection, _, hijackErr := w.(http.Hijacker).Hijack()
			if hijackErr == nil {
				_ = connection.Close()
			}
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
}

func (fixture *protocolFixture) DropNextSyncResponseAfterCommit() {
	fixture.dropNextSync.Store(true)
}

func (fixture *protocolFixture) RequireReauthByRefreshReuse(client *ProtocolClient) {
	fixture.t.Helper()
	consumed := client.refreshToken
	if err := client.refreshHTTP(consumed); err != nil {
		fixture.t.Fatal(err)
	}
	if err := client.refreshHTTP(consumed); err == nil || !strings.Contains(err.Error(), "refresh_reused") {
		fixture.t.Fatalf("refresh reuse err=%v", err)
	}
	if _, err := client.Sync(nil); err == nil {
		fixture.t.Fatal("old session retained sync access after refresh reuse")
	}
}

func (fixture *protocolFixture) DroppedSyncResponse() protocol.SyncResponse {
	fixture.t.Helper()
	fixture.droppedMu.Lock()
	raw := append([]byte(nil), fixture.droppedSyncBody...)
	fixture.droppedMu.Unlock()
	var response protocol.SyncResponse
	if len(raw) == 0 || json.Unmarshal(raw, &response) != nil {
		fixture.t.Fatal("dropped committed response was not captured")
	}
	return response
}

func (fixture *protocolFixture) startServer() {
	if fixture.server != nil {
		fixture.server.Close()
	}
	policies := limits.DefaultPolicies()
	resolver, err := limits.NewClientIPResolver(nil)
	if err != nil {
		fixture.t.Fatal(err)
	}
	faults := &testkit.RecoveryFaults{}
	fixture.server = httptest.NewServer(httpapi.RecoveryFaultHandler(httpapi.New(httpapi.Options{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Readiness: ready{},
		InstanceID: fixture.h.Info.InstanceID, RecoveryEpoch: fixture.h.Info.RecoveryEpoch,
		CanonicalOrigin: fixture.h.Info.CanonicalOrigin, SyncLimits: &fixture.h.Limits,
		AppleAuthorizationMode: "identity_token_only", ChallengeService: fixture.h.Identity, SessionService: fixture.h.Auth,
		RecoveryService: fixture.h.Auth, RecoveryEnabled: true, RecoveryReadiness: ready{},
		FamilyService: fixture.h.Family, SyncService: fixture.h.Service, ImportService: fixture.h.Importer,
		RequestAuthorizer: fixture.h.Authority, LiveHub: fixture.hub, Limiter: allowLimiter{}, ClientIPResolver: resolver,
		SyncMutationDevicePolicy: policies[limits.ScopeSyncMutationDevice], SyncMutationAccountPolicy: policies[limits.ScopeSyncMutationAccount],
		SyncMutationFamilyPolicy: policies[limits.ScopeSyncMutationFamily], SyncPullDevicePolicy: policies[limits.ScopeSyncPullDevice],
		SyncPullAccountPolicy: policies[limits.ScopeSyncPullAccount], LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP],
		LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice], LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount],
		LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily], ChallengePolicy: policies[limits.ScopeAuthChallengeIP],
		ExchangePolicy: policies[limits.ScopeAuthExchangeIP], RefreshIPPolicy: policies[limits.ScopeAuthRefreshIP],
		RefreshAccountPolicy: policies[limits.ScopeAuthRefreshAccount], RefreshDevicePolicy: policies[limits.ScopeAuthRefreshDevice],
	}), func(point, attempt string) error { return faults.FailOnce(testkit.RecoveryFaultPoint(point), attempt) }))
}

func (fixture *protocolFixture) Owner() *ProtocolClient     { return fixture.owner }
func (fixture *protocolFixture) Caregiver() *ProtocolClient { return fixture.caregiver }

// JoinAsNewAccount signs a brand-new account in over the real Apple exchange
// and redeems `token` as them, returning a client for the session that
// results.
//
// Not `testkit.AddMember`: the protocol fixture replaces the harness's
// identity services with ones pinned to a signing issuer, so a sign-in that
// works for the in-process suites is refused here as `invalid_apple_proof`.
// The whole point of this fixture is that it speaks the routes a second
// device speaks, and that includes how a second device arrives.
func (fixture *protocolFixture) JoinAsNewAccount(t *testing.T, subject, token string) *ProtocolClient {
	t.Helper()
	session, err := fixture.exchangeSignedWithProfile(subject, "installation-"+subject, subject)
	if err != nil {
		t.Fatalf("sign %s in: %v", subject, err)
	}
	principal, err := fixture.h.Auth.Authenticate(t.Context(), session.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.h.Family.Redeem(t.Context(), principal, fixture.h.CommandID(), token, true); err != nil {
		t.Fatalf("%s could not redeem: %v", subject, err)
	}
	// Re-read the session: the family slot only exists after redemption, and a
	// token minted before it carries no family scope.
	refreshed, err := fixture.h.Auth.Refresh(t.Context(), session.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := fixture.h.Auth.Authenticate(t.Context(), refreshed.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	return &ProtocolClient{
		fixture: fixture, principal: authenticated, token: refreshed.AccessToken,
		refreshToken: refreshed.RefreshToken, deviceCredential: refreshed.DeviceCredential,
		subject: subject, frozen: map[string][]byte{},
	}
}

func (client *ProtocolClient) Sync(mutations []protocol.Mutation) (protocol.SyncResponse, error) {
	raw, err := client.syncRequestBytes(mutations)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	return client.SyncExact(raw)
}

func (client *ProtocolClient) syncRequestBytes(mutations []protocol.Mutation) ([]byte, error) {
	if mutations == nil {
		mutations = []protocol.Mutation{}
	}
	capabilities := []string{protocol.CapabilityRecordsV1}
	for _, mutation := range mutations {
		if mutation.EntityKind == "timer" {
			capabilities = append(capabilities, protocol.CapabilityTimerCommandsV1)
			break
		}
	}
	request := protocol.SyncRequest{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: capabilities}, Cursor: client.cursor, Mutations: mutations}
	return json.Marshal(request)
}

func (client *ProtocolClient) SyncExact(raw []byte) (protocol.SyncResponse, error) {
	var envelope protocol.SyncRequest
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return protocol.SyncResponse{}, err
	}
	capabilities := envelope.Capabilities
	httpRequest, _ := http.NewRequestWithContext(client.fixture.t.Context(), http.MethodPost, client.fixture.proxy.URL+"/v1/sync", bytes.NewReader(raw))
	httpRequest.Header.Set("Authorization", "Bearer "+client.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("X-Baby-Tracker-Protocol", "v1")
	httpRequest.Header.Set("X-Baby-Tracker-Capabilities", strings.Join(capabilities, ","))
	// Matches every header the real client always sends
	// (FamilySyncHTTPClient.swift): the server still only ignores them, but a
	// family holding a current-version feed or timer refuses a sync that
	// claims not to support it.
	httpRequest.Header.Set("X-Baby-Tracker-Growth", "v1")
	httpRequest.Header.Set("X-Baby-Tracker-Supplements", "v1")
	httpRequest.Header.Set("X-Baby-Tracker-Timed-Bottle", "v1")
	httpRequest.Header.Set("X-Baby-Tracker-Feeding-Session", "v1")
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	defer response.Body.Close()
	var result protocol.SyncResponse
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return result, &statusError{status: response.StatusCode, body: string(body)}
	}
	err = json.NewDecoder(response.Body).Decode(&result)
	if err == nil {
		cursor := result.HighWater
		client.cursor = &cursor
	}
	return result, err
}

// FamilyPost drives one of the /v1/family routes as this client, returning the
// status and the decoded error envelope.
//
// The family API is not part of the sync protocol, so it has no place on the
// sync client — except that authorization is the thing under test, and the
// only honest way to ask "may this member do that" is through the same request
// a second device would send, with the same bearer token.
func (client *ProtocolClient) FamilyPost(t *testing.T, path, body string) (int, protocol.ErrorDetail) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.fixture.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Baby-Tracker-Protocol", "v1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	var envelope protocol.ErrorResponse
	_ = json.Unmarshal(raw, &envelope)
	return response.StatusCode, envelope.Error
}

func (client *ProtocolClient) FrozenTimerStart(timerID string) []byte {
	if frozen := client.frozen[timerID]; frozen != nil {
		return append([]byte(nil), frozen...)
	}
	start := testkit.MustMutation(client.fixture.t, "timer-feed.json")
	start.MutationID, start.EntityID = client.fixture.h.CommandID(), timerID
	raw, err := client.syncRequestBytes([]protocol.Mutation{start})
	if err != nil {
		client.fixture.t.Fatal(err)
	}
	client.frozen[timerID] = append([]byte(nil), raw...)
	return append([]byte(nil), raw...)
}

func (fixture *protocolFixture) RestartClient(client *ProtocolClient) *ProtocolClient {
	fixture.t.Helper()
	type state struct {
		Principal        access.Principal  `json:"principal"`
		Token            string            `json:"token"`
		RefreshToken     string            `json:"refresh_token"`
		DeviceCredential string            `json:"device_credential"`
		Cursor           *protocol.Cursor  `json:"cursor"`
		Subject          string            `json:"subject"`
		Frozen           map[string][]byte `json:"frozen"`
	}
	raw, err := json.Marshal(state{client.principal, client.token, client.refreshToken, client.deviceCredential, client.cursor, client.subject, client.frozen})
	if err != nil {
		fixture.t.Fatal(err)
	}
	path := filepath.Join(fixture.t.TempDir(), "protocol-client-state.json")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		fixture.t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		fixture.t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		fixture.t.Fatalf("client state permissions=%v", info.Mode().Perm())
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		fixture.t.Fatal(err)
	}
	var restored state
	if err = json.Unmarshal(raw, &restored); err != nil {
		fixture.t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		fixture.t.Fatal(err)
	}
	return &ProtocolClient{fixture: fixture, principal: restored.Principal, token: restored.Token,
		refreshToken: restored.RefreshToken, deviceCredential: restored.DeviceCredential, cursor: restored.Cursor, subject: restored.Subject, frozen: restored.Frozen}
}

func (client *ProtocolClient) StopTimer(timerID, recordID string) protocol.Outcome {
	stop := testkit.MustMutation(client.fixture.t, "timer-feed.json")
	payload := stop.Payload.(protocol.TimerStartPayload)
	stop.MutationID, stop.EntityID, stop.Operation = client.fixture.h.CommandID(), timerID, "timer_stop"
	// The timer's running breast side only finalizes into a component at
	// component_version>=2 if the stop names a component id.
	stop.Payload = protocol.TimerStopPayload{BabyID: payload.BabyID, CompletedRecordID: recordID, StoppedAt: "2026-09-20T15:10:00.000000Z", ComponentID: "4" + recordID[1:]}
	response, err := client.Sync([]protocol.Mutation{stop})
	if err != nil || len(response.Outcomes) != 1 {
		client.fixture.t.Fatalf("stop outcomes=%d err=%v", len(response.Outcomes), err)
	}
	return response.Outcomes[0]
}

func (fixture *protocolFixture) CountCompletedRecords(string) int {
	return fixture.h.CareCount(fixture.t, "feed", fixture.h.FamilyID)
}

func (client *ProtocolClient) Snapshot() (protocol.SnapshotResponse, error) {
	httpRequest, _ := http.NewRequestWithContext(client.fixture.t.Context(), http.MethodGet, client.fixture.proxy.URL+"/v1/sync/snapshot", nil)
	httpRequest.Header.Set("Authorization", "Bearer "+client.token)
	httpRequest.Header.Set("X-Baby-Tracker-Protocol", "v1")
	httpRequest.Header.Set("X-Baby-Tracker-Capabilities", "records-v1,snapshot-v1")
	httpRequest.Header.Set("X-Baby-Tracker-Growth", "v1")
	httpRequest.Header.Set("X-Baby-Tracker-Supplements", "v1")
	httpRequest.Header.Set("X-Baby-Tracker-Timed-Bottle", "v1")
	httpRequest.Header.Set("X-Baby-Tracker-Feeding-Session", "v1")
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return protocol.SnapshotResponse{}, err
	}
	defer response.Body.Close()
	var result protocol.SnapshotResponse
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return result, &statusError{status: response.StatusCode, body: string(body)}
	}
	err = json.NewDecoder(response.Body).Decode(&result)
	return result, err
}

func (client *ProtocolClient) Import(request protocol.ImportResolveRequest) (protocol.ImportResolveResponse, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return protocol.ImportResolveResponse{}, err
	}
	httpRequest, _ := http.NewRequestWithContext(client.fixture.t.Context(), http.MethodPost, client.fixture.proxy.URL+"/v1/family/import/resolve", bytes.NewReader(raw))
	httpRequest.Header.Set("Authorization", "Bearer "+client.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("X-Baby-Tracker-Protocol", "v1")
	httpRequest.Header.Set("X-Baby-Tracker-Capabilities", "records-v1,import-resolver-v1")
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return protocol.ImportResolveResponse{}, err
	}
	defer response.Body.Close()
	var result protocol.ImportResolveResponse
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return result, &statusError{status: response.StatusCode, body: string(body)}
	}
	err = json.NewDecoder(response.Body).Decode(&result)
	return result, err
}

type statusError struct {
	status int
	body   string
}

func (err *statusError) Error() string {
	return fmt.Sprintf("%s: %s", http.StatusText(err.status), err.body)
}

func mutation(t *testing.T, fixture string, commandID string) protocol.Mutation {
	t.Helper()
	value := testkit.MustMutation(t, fixture)
	value.MutationID = commandID
	return value
}
