package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
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
		SessionService: h.Auth, FamilyService: h.Family, LiveFamilyAuthorizer: h.Family, LiveHub: hub,
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
		FamilyService: h.Family, LiveFamilyAuthorizer: h.Family, LiveHub: hub, Limiter: liveAllowLimiter(), ClientIPResolver: resolver,
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
		FamilyService: h.Family, LiveFamilyAuthorizer: h.Family, LiveHub: hub, Limiter: liveAllowLimiter(), ClientIPResolver: resolver,
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
	fixture := testpg.New(t)
	repository, err := limits.NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	pepper := [32]byte{1, 2, 3, 4}
	limiter, err := limits.New(limits.Options{Repository: repository, Pepper: pepper, Now: func() time.Time { return h.Now }})
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
		FamilyService: h.Family, LiveFamilyAuthorizer: h.Family, LiveHub: hub, Limiter: limiter, ClientIPResolver: resolver,
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

func TestLiveRealLimiterDependencyFailureReturns503(t *testing.T) {
	h := testkit.New(t)
	fixture := testpg.New(t)
	repository, err := limits.NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := limits.New(limits.Options{Repository: repository, Pepper: [32]byte{1}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	fixture.Runtime.Close()
	hub, _ := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	resolver, _ := limits.NewClientIPResolver(nil)
	policies := limits.DefaultPolicies()
	server := httptest.NewServer(New(Options{
		Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SessionService: h.Auth,
		FamilyService: h.Family, LiveFamilyAuthorizer: h.Family, LiveHub: hub, Limiter: limiter, ClientIPResolver: resolver,
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
