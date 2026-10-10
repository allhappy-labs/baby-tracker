package httpapi

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testkit"
	"github.com/coder/websocket"
)

// afterAuthorization runs hook once, after the live handshake's
// authorization read answered and before the handler reserves: the
// Authorize→Reserve half of the window.
type afterAuthorization struct {
	inner RequestAuthorizer
	once  sync.Once
	hook  func()
}

func (a *afterAuthorization) AuthorizeRequest(ctx context.Context, request authority.Request, action string) (access.Principal, access.Grant, error) {
	p, g, err := a.inner.AuthorizeRequest(ctx, request, action)
	if err == nil {
		a.once.Do(a.hook)
	}
	return p, g, err
}

// beforeHijack runs hook once when the upgrade hijacks the connection:
// after the reservation and before Bind, the Reserve→Bind half.
type beforeHijack struct {
	http.ResponseWriter
	once *sync.Once
	hook func()
}

func (w beforeHijack) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.once.Do(w.hook)
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w beforeHijack) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// A session revoked, or a member removed, after the handshake's
// authorization read and before Bind never gets a bound connection, at
// either point of the window. Each revocation commits through the real
// authority, and the hub processes its invalidation before the handler
// moves on, so the interleaving is fixed.
func TestLiveRevocationBetweenAuthorizeAndBindNeverBinds(t *testing.T) {
	for _, window := range []string{"authorize_to_reserve", "reserve_to_bind"} {
		for _, revocation := range []string{"member_removed", "logout"} {
			t.Run(window+"/"+revocation, func(t *testing.T) {
				h := testkit.New(t)
				hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Invalidations: h.Authority, Now: func() time.Time { return h.Now }})
				if err != nil {
					t.Fatal(err)
				}
				revoke := func() {
					before := hub.InvalidationMark()
					var err error
					if revocation == "logout" {
						_, err = h.Auth.Logout(context.Background(), h.Caregiver, h.CommandID())
					} else {
						_, err = h.Family.Remove(context.Background(), h.Owner, h.CommandID(), h.Caregiver.AccountID)
					}
					if err != nil {
						t.Error(err)
						return
					}
					deadline := time.Now().Add(2 * time.Second)
					for hub.InvalidationMark() == before && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					if hub.InvalidationMark() == before {
						t.Error("the hub never processed the revocation")
					}
				}
				resolver, err := limits.NewClientIPResolver(nil)
				if err != nil {
					t.Fatal(err)
				}
				policies := limits.DefaultPolicies()
				options := Options{
					Readiness:  readinessFunc(func(context.Context) error { return nil }),
					InstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin,
					SessionService: h.Auth, RequestAuthorizer: h.Authority, LiveHub: hub,
					Limiter: liveAllowLimiter(), ClientIPResolver: resolver,
					LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP], LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice],
					LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily],
				}
				var hijacked sync.Once
				if window == "authorize_to_reserve" {
					options.RequestAuthorizer = &afterAuthorization{inner: h.Authority, hook: revoke}
				}
				handler := New(options)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if window == "reserve_to_bind" {
						w = beforeHijack{ResponseWriter: w, once: &hijacked, hook: revoke}
					}
					handler.ServeHTTP(w, request)
				}))
				defer server.Close()
				dial := func(subject string) (*websocket.Conn, *http.Response, error) {
					header := http.Header{}
					header.Set("Authorization", "Bearer "+h.AccessToken(subject))
					header.Set("X-Baby-Tracker-Protocol", "v1")
					header.Set("X-Baby-Tracker-Capabilities", "records-v1,live-hints-v1")
					return websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
				}
				connection, response, err := dial("caregiver")
				switch window {
				case "authorize_to_reserve":
					// Refused before the upgrade, retryably; the client's
					// next attempt meets the revocation itself.
					if err == nil {
						connection.CloseNow()
						t.Fatal("a connection authorized before its revocation was upgraded")
					}
					if response == nil || response.StatusCode != http.StatusServiceUnavailable {
						t.Fatalf("response=%v err=%v, want 503", response, err)
					}
				case "reserve_to_bind":
					// Upgraded, then closed as a revoked bound connection is.
					if err != nil {
						t.Fatalf("dial: %v", err)
					}
					readContext, cancel := context.WithTimeout(t.Context(), time.Second)
					_, _, err = connection.Read(readContext)
					cancel()
					connection.CloseNow()
					if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
						t.Fatalf("close=%d err=%v, want 1008", websocket.CloseStatus(err), err)
					}
				}
				if hub.HasRegisteredFamily(h.FamilyID) {
					t.Fatal("the revoked connection was bound")
				}
				// The revocation refused only the connection it names.
				owner, _, err := dial("owner")
				if err != nil {
					t.Fatalf("an unaffected member was refused: %v", err)
				}
				defer owner.CloseNow()
				deadline := time.Now().Add(time.Second)
				for !hub.HasRegisteredFamily(h.FamilyID) && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if !hub.HasRegisteredFamily(h.FamilyID) {
					t.Fatal("the unaffected member's connection was not bound")
				}
			})
		}
	}
}
