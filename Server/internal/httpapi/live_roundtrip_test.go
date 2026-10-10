package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/family"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/realtime"
	"babytracker/server/internal/testkit"
	"github.com/coder/websocket"
)

// authorityStatements counts app.request_authority calls among statements.
func authorityStatements(statements []string) int {
	n := 0
	for _, sql := range statements {
		if strings.Contains(sql, "app.request_authority") {
			n++
		}
	}
	return n
}

// TestLiveHandshakeIsOneStatement pins the live handshake's database cost:
// one pooled app.request_authority statement, with no transaction, which
// authenticates, applies the route's own refusals and family policy, and
// charges the in-memory limits. The family service is never asked.
func TestLiveHandshakeIsOneStatement(t *testing.T) {
	h := testkit.New(t)
	removed := h.AddMember(t, "removed", "caregiver")
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), removed.AccountID); err != nil {
		t.Fatal(err)
	}
	pool, wire := testkit.CountedPool(t, h.Runtime)
	hub, err := realtime.NewHub(t.Context(), realtime.HubOptions{Limits: h.Limits, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{3}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := limits.NewClientIPResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	policies := limits.DefaultPolicies()
	server := httptest.NewServer(New(Options{
		Readiness: readinessFunc(func(context.Context) error { return nil }), InstanceID: h.Info.InstanceID,
		RecoveryEpoch: h.Info.RecoveryEpoch, CanonicalOrigin: h.Info.CanonicalOrigin, SessionService: h.Auth,
		FamilyService: familyServiceStub{current: func(context.Context, access.Principal) (family.FamilyView, error) {
			t.Error("the live handshake read the family")
			return family.FamilyView{}, nil
		}},
		RequestAuthorizer: h.AuthorityOn(t, pool), LiveHub: hub, Limiter: limiter, ClientIPResolver: resolver,
		LiveAttemptIPPolicy: policies[limits.ScopeLiveAttemptIP], LiveAttemptDevicePolicy: policies[limits.ScopeLiveAttemptDevice],
		LiveAttemptAccountPolicy: policies[limits.ScopeLiveAttemptAccount], LiveAttemptFamilyPolicy: policies[limits.ScopeLiveAttemptFamily],
	}))
	defer server.Close()
	headers := func(subject string) http.Header {
		return http.Header{"Authorization": []string{"Bearer " + h.AccessToken(subject)}, "X-Baby-Tracker-Protocol": []string{"v1"},
			"X-Baby-Tracker-Capabilities": []string{"records-v1,live-hints-v1"}}
	}
	dial := func(header http.Header) (*websocket.Conn, int, error) {
		connection, response, err := websocket.Dial(t.Context(), server.URL+"/v1/live", &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{realtime.Subprotocol}})
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return connection, status, err
	}
	for i, label := range []string{"warm", "measured", "again"} {
		wire.Reset()
		connection, status, err := dial(headers("owner"))
		if err != nil {
			t.Fatalf("%s dial: status %d: %v", label, status, err)
		}
		connection.CloseNow()
		if i == 0 {
			continue
		}
		statements := wire.Statements()
		if trips := wire.Trips(); trips != 1 || len(statements) != 1 || authorityStatements(statements) != 1 {
			t.Fatalf("%s handshake: %d round trips, statements %q; want 1 round trip and the one app.request_authority statement", label, trips, statements)
		}
	}
	// A refused handshake costs the same statement (a removed member's
	// revoked session), and a malformed credential none.
	wire.Reset()
	if _, status, err := dial(headers("removed")); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("removed member: status %d, err %v", status, err)
	}
	if trips, statements := wire.Trips(), wire.Statements(); trips != 1 || len(statements) != 1 {
		t.Fatalf("a refused handshake: %d round trips, statements %q", trips, statements)
	}
	wire.Reset()
	bad := headers("owner")
	bad.Set("Authorization", "Bearer not-a-token")
	if _, status, err := dial(bad); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("malformed bearer: status %d, err %v", status, err)
	}
	if trips := wire.Trips(); trips != 0 {
		t.Fatalf("a malformed credential reached the database: %d round trips", trips)
	}
}
