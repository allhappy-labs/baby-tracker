package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testkit"
	"github.com/coder/websocket"
)

type lockedBuffer struct {
	sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(value []byte) (int, error) {
	buffer.Lock()
	defer buffer.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *lockedBuffer) String() string {
	buffer.Lock()
	defer buffer.Unlock()
	return buffer.buffer.String()
}

func TestRealUpgradeThroughTrackedResponseWriter(t *testing.T) {
	h := testkit.New(t)
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	resolver, err := limits.NewClientIPResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	options := Options{
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Readiness: readinessFunc(func(context.Context) error { return nil }),
		InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin,
		SessionService: h.Auth, RequestAuthorizer: h.Authority, LiveHub: hub,
		Limiter: liveAllowLimiter(), ClientIPResolver: resolver,
		LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP], LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice],
		LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily],
	}
	server := httptest.NewServer(New(options))
	defer server.Close()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+h.AccessToken("owner"))
	header.Set("X-Baby-Tracker-Protocol", "v1")
	header.Set("X-Baby-Tracker-Capabilities", "records-v1,live-hints-v1")
	header.Set("Origin", h.Info.CanonicalOrigin)
	connection, response, err := websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{"baby-tracker.v1"}})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial status=%d err=%v", status, err)
	}
	want := realtime.Hint{FamilyID: h.FamilyID, Cursor: protocol.Cursor{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, FamilyID: h.FamilyID, Sequence: 9}}
	hub.Publish(t.Context(), want)
	readContext, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, raw, err := connection.Read(readContext)
	if err != nil {
		t.Fatal(err)
	}
	var got realtime.Hint
	if err = json.Unmarshal(raw, &got); err != nil || got != want {
		t.Fatalf("hint=%s decoded=%+v err=%v", raw, got, err)
	}
	connection.CloseNow()
	deadline := time.Now().Add(time.Second)
	for strings.Count(logs.String(), `"status":101`) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if strings.Count(logs.String(), `"status":101`) != 1 {
		t.Fatalf("upgrade log=%s", logs.String())
	}
}

func TestUnauthorizedOriginSubprotocolAndTokenPlacementsDeniedWithoutReservationLeak(t *testing.T) {
	h := testkit.New(t)
	limitsValue := h.Limits
	limitsValue.SocketDeviceLimit = 1
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: limitsValue, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	resolver, _ := limits.NewClientIPResolver(nil)
	policies := limits.DefaultPolicies()
	server := httptest.NewServer(New(Options{
		Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SessionService: h.Auth,
		RequestAuthorizer: h.Authority, LiveHub: hub, Limiter: liveAllowLimiter(), ClientIPResolver: resolver,
		LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP], LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice],
		LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily],
	}))
	defer server.Close()
	baseHeader := http.Header{"Authorization": []string{"Bearer " + h.AccessToken("owner")}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	for name, mutate := range map[string]func(*string, http.Header, *[]string){
		"origin":       func(_ *string, header http.Header, _ *[]string) { header.Set("Origin", "https://evil.example") },
		"subprotocol":  func(_ *string, _ http.Header, protocols *[]string) { *protocols = []string{"wrong.v1"} },
		"url token":    func(rawURL *string, _ http.Header, _ *[]string) { *rawURL += "?access_token=secret" },
		"cookie token": func(_ *string, header http.Header, _ *[]string) { header.Set("Cookie", "access_token=secret") },
	} {
		t.Run(name, func(t *testing.T) {
			rawURL := server.URL + "/v1/live"
			header := baseHeader.Clone()
			protocols := []string{"baby-tracker.v1"}
			mutate(&rawURL, header, &protocols)
			connection, response, dialErr := websocket.Dial(t.Context(), rawURL, &websocket.DialOptions{HTTPHeader: header, Subprotocols: protocols})
			if connection != nil {
				connection.CloseNow()
			}
			if dialErr == nil || response == nil || response.StatusCode == http.StatusSwitchingProtocols {
				t.Fatalf("response=%v err=%v", response, dialErr)
			}
		})
	}
	header := baseHeader.Clone()
	connection, response, err := websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{"baby-tracker.v1"}})
	if err != nil {
		t.Fatalf("reservation leaked; response=%v err=%v", response, err)
	}
	connection.CloseNow()
}

func TestApplicationFrameReceivesPolicyClose(t *testing.T) {
	h := testkit.New(t)
	hub, _ := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	resolver, _ := limits.NewClientIPResolver(nil)
	policies := limits.DefaultPolicies()
	server := httptest.NewServer(New(Options{
		Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SessionService: h.Auth,
		RequestAuthorizer: h.Authority, LiveHub: hub, Limiter: liveAllowLimiter(), ClientIPResolver: resolver,
		LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP], LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice],
		LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily],
	}))
	defer server.Close()
	header := http.Header{"Authorization": []string{"Bearer " + h.AccessToken("owner")}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	connection, _, err := websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	if err = connection.Write(t.Context(), websocket.MessageText, []byte(`{"unexpected":true}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err = connection.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close status=%d err=%v", websocket.CloseStatus(err), err)
	}
}

func TestLiveRealLimiterUpgradesThenReturns429AndResets(t *testing.T) {
	h := testkit.New(t)
	pepper := [32]byte{1, 2, 3, 4}
	limiter, err := limits.New(limits.Options{Pepper: pepper, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	resolver, _ := limits.NewClientIPResolver(nil)
	server := httptest.NewServer(New(Options{
		Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SessionService: h.Auth,
		RequestAuthorizer: h.Authority, LiveHub: hub, Limiter: limiter, ClientIPResolver: resolver,
		LiveAttemptIPPolicy: limits.Policy{Count: 2, Window: time.Minute}, LiveAttemptDevicePolicy: limits.Policy{Count: 1, Window: time.Minute},
		LiveAttemptAccountPolicy: limits.Policy{Count: 1, Window: time.Minute}, LiveAttemptFamilyPolicy: limits.Policy{Count: 1, Window: time.Minute},
	}))
	defer server.Close()
	header := http.Header{"Authorization": []string{"Bearer " + h.AccessToken("owner")}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	dial := func() (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	}
	connection, response, err := dial()
	if err != nil {
		t.Fatalf("first dial response=%v err=%v", response, err)
	}
	connection.CloseNow()
	connection, response, err = dial()
	if connection != nil {
		connection.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "60" {
		t.Fatalf("second dial response=%v retry-after=%q err=%v", response, responseHeader(response, "Retry-After"), err)
	}
	// The authenticated denial must not roll back the already-admitted pre-auth
	// IP attempt. A third request is therefore rejected by the IP dimension too.
	connection, response, err = dial()
	if connection != nil {
		connection.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third dial response=%v err=%v", response, err)
	}
	h.Now = h.Now.Add(time.Minute)
	connection, response, err = dial()
	if err != nil {
		t.Fatalf("next-window dial response=%v err=%v", response, err)
	}
	connection.CloseNow()
}

// The in-memory limiter fails only when its verified-key pool is full: it
// refuses a new bucket as the retryable rate_limit_unavailable, which the live
// route answers with 503, as it answered the retired database limiter's
// failures. An IP-keyed flood cannot cause this (TestLiveIPFloodStillUpgrades).
func TestLiveRealLimiterAtCapacityReturns503(t *testing.T) {
	h := testkit.New(t)
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{1}, Now: func() time.Time { return h.Now }, MaxVerifiedEntries: 64}) // one verified bucket per shard
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for i := range 2000 {
		key := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
		if _, takeErr := limiter.Take(t.Context(), limits.ScopeSyncPullDevice, key, limits.Policy{Count: 120, Window: time.Minute}); takeErr == nil {
			stored++
		} else if limits.ErrorCodeOf(takeErr) != limits.CodeLimitUnavailable || !limits.IsRetryable(takeErr) {
			t.Fatalf("fill refused with %v", takeErr)
		}
	}
	if stored != 64 {
		t.Fatalf("filled %d of 64 verified buckets", stored)
	}
	hub, _ := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	resolver, _ := limits.NewClientIPResolver(nil)
	policies := limits.DefaultPolicies()
	server := httptest.NewServer(New(Options{
		Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SessionService: h.Auth,
		RequestAuthorizer: h.Authority, LiveHub: hub, Limiter: limiter, ClientIPResolver: resolver,
		LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP], LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice],
		LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily],
	}))
	defer server.Close()
	header := http.Header{"Authorization": []string{"Bearer " + h.AccessToken("owner")}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	connection, response, err := websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if connection != nil {
		connection.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response=%v err=%v", response, err)
	}
}

// A flood of client addresses on the live IP scope evicts within that scope
// and denies no one else: a signed-in device still upgrades.
func TestLiveIPFloodStillUpgrades(t *testing.T) {
	h := testkit.New(t)
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{1}, Now: func() time.Time { return h.Now }, MaxIPEntriesPerScope: 64}) // one bucket per shard per IP scope
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	for i := range 2000 {
		address := fmt.Sprintf("2001:db8:%x::1", i)
		if decision, takeErr := limiter.Take(t.Context(), limits.ScopeLiveAttemptIP, address, policies[limits.ScopeLiveAttemptIP]); takeErr != nil || !decision.Allowed {
			t.Fatalf("flood address %d refused: %+v %v", i, decision, takeErr)
		}
	}
	hub, _ := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	resolver, _ := limits.NewClientIPResolver(nil)
	server := httptest.NewServer(New(Options{
		Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SessionService: h.Auth,
		RequestAuthorizer: h.Authority, LiveHub: hub, Limiter: limiter, ClientIPResolver: resolver,
		LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP], LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice],
		LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily],
	}))
	defer server.Close()
	header := http.Header{"Authorization": []string{"Bearer " + h.AccessToken("owner")}, "X-Baby-Tracker-Protocol": []string{"v1"}, "X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	connection, response, err := websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
	if err != nil {
		t.Fatalf("dial during an IP flood: response=%v err=%v", response, err)
	}
	connection.CloseNow()
	if evicted := limiter.(*limits.Service).Stats().IPEvictions[limits.ScopeLiveAttemptIP]; evicted == 0 {
		t.Fatal("the flood evicted nothing")
	}
}

func responseHeader(response *http.Response, name string) string {
	if response == nil {
		return ""
	}
	return response.Header.Get(name)
}

func liveAllowLimiter() limiterStub {
	return limiterStub{
		take: func(context.Context, string, string, limits.Policy) (limits.Decision, error) {
			return limits.Decision{Allowed: true}, nil
		},
		takeMany: func(context.Context, []limits.Charge) (limits.Decision, error) {
			return limits.Decision{Allowed: true}, nil
		},
	}
}

// The live route authorizes through the RequestAuthorizer alone (the
// family service is never asked) and charges the device, account and
// family attempt limits, the family from the grant, after family policy; a
// grant for another family than the session's is refused access_expired
// before any charge, as the legacy family read refused it.
func TestLiveAuthorizesThroughTheRequestAuthorizerAlone(t *testing.T) {
	p := access.Principal{AccountID: "20000000-0000-4000-8000-000000000001", DeviceID: "21000000-0000-4000-8000-000000000001",
		SessionID: "22000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, SecurityRevision: 1, AccessExpiresAt: time.Now().Add(time.Hour),
		Scope: "family", SessionScope: "family", SessionFamilyID: "30000000-0000-4000-8000-000000000001"}
	for _, c := range []struct {
		name    string
		family  string
		status  int
		charges int
	}{
		{"the session's family", p.SessionFamilyID, http.StatusBadRequest, 1}, // charged, then no socket: invalid_request
		{"another family", "30000000-0000-4000-8000-000000000002", http.StatusUnauthorized, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var charged [][]limits.Charge
			hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: protocol.DefaultLimits()})
			if err != nil {
				t.Fatal(err)
			}
			options := familyHandlerOptions(t, p, familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
				t.Error("the live route read the family")
				return family.FamilyView{}, nil
			}}, func(charges []limits.Charge) { charged = append(charged, charges) })
			options.RequestAuthorizer = requestAuthorizerStub{principal: p, grant: &access.Grant{AccountID: p.AccountID, FamilyID: c.family, Role: "viewer", Revision: 1}}
			options.LiveHub = hub
			options.InstanceID, options.RecoveryEpoch = "50000000-0000-4000-8000-000000000001", 1
			request := httptest.NewRequest(http.MethodGet, "/v1/live", nil)
			request.Header.Set("Authorization", "Bearer access")
			request.Header.Set("X-Baby-Tracker-Protocol", "v1")
			request.Header.Set("X-Baby-Tracker-Capabilities", "records-v1,live-hints-v1")
			recorder := httptest.NewRecorder()
			New(options).ServeHTTP(recorder, request)
			if recorder.Code != c.status || len(charged) != c.charges {
				t.Fatalf("status %d, %d charges, want %d and %d: %s", recorder.Code, len(charged), c.status, c.charges, recorder.Body.String())
			}
			if c.charges == 1 && (len(charged[0]) != 3 || charged[0][0].Scope != limits.ScopeLiveAttemptDevice || charged[0][0].Key != p.DeviceID ||
				charged[0][1].Scope != limits.ScopeLiveAttemptAccount || charged[0][1].Key != p.AccountID ||
				charged[0][2].Scope != limits.ScopeLiveAttemptFamily || charged[0][2].Key != c.family) {
				t.Fatalf("charges %+v", charged[0])
			}
		})
	}
}
