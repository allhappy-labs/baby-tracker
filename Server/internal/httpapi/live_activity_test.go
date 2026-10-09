package httpapi

import (
	"babytracker/server/internal/limits"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

func TestLiveActivityRegistrationRoutes(t *testing.T) {
	h := testkit.New(t)
	head, err := h.Security.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Migrator.Exec(t.Context(), `SELECT app.update_server_state($1,$2,$3,$4,$5)`, h.Info.InstanceID, h.Info.RecoveryEpoch, head.Generation, head.Revision, head.EventHash[:]); err != nil {
		t.Fatal(err)
	}
	service := liveactivity.NewService(h.Security, exportFamilyResolver{h.Family}, "com.example.babytracker.push-type.liveactivity")
	options := familyHandlerOptions(t, h.Owner, h.Family, nil)
	options.SessionService = h.Auth
	options.LiveActivityService = service
	options.LiveActivityReadiness = readinessFunc(func(context.Context) error { return nil })
	options.LiveActivityUnregistration = liveactivity.NewUnregistration(h.Security, exportFamilyResolver{h.Family})
	options.Capabilities = append(protocol.ServerCapabilities(), protocol.CapabilityLiveActivityPushV1)
	req := protocol.LiveActivityDeviceRegistration{RequestMeta: protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: []string{protocol.CapabilityLiveActivityPushV1}}, InstallationID: "70000000-0000-4000-8000-000000000001", RegistrationVersion: 1, APNsEnvironment: "sandbox", PayloadVersion: 1, PushToStartToken: "abcd", FeedBreakMinutes: 30}
	body, _ := json.Marshal(req)
	request := func(method, path string, body []byte, token string, opts Options) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("X-Baby-Tracker-Protocol", "v1")
		r.Header.Set("X-Baby-Tracker-Capabilities", protocol.CapabilityLiveActivityPushV1)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		New(opts).ServeHTTP(w, r)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("missing no-store: %d", w.Code)
		}
		return w
	}
	for _, path := range []string{"/v1/live-activities/device", "/v1/live-activities/activity"} {
		if w := request(http.MethodPut, path, body, "", options); w.Code != 401 {
			t.Fatalf("unauthenticated status=%d", w.Code)
		}
	}
	// Push disabled: no registration service and no worker, only the
	// repository-only unregistration path.
	disabled := options
	disabled.LiveActivityService, disabled.LiveActivityReadiness = nil, nil
	if w := request(http.MethodPut, "/v1/live-activities/device", body, h.AccessToken("owner"), disabled); w.Code != 503 {
		t.Fatalf("disabled=%d %s", w.Code, w.Body.String())
	}
	// Nothing at all composed (an older composition): unregistering is refused too.
	absent := disabled
	absent.LiveActivityUnregistration = nil
	if w := request(http.MethodDelete, "/v1/live-activities/device/"+req.InstallationID+"?version=1", nil, h.AccessToken("owner"), absent); w.Code != 503 {
		t.Fatalf("absent unregister=%d %s", w.Code, w.Body.String())
	}
	failed := options
	failed.LiveActivityReadiness = readinessFunc(func(context.Context) error { return errors.New("worker failed") })
	if w := request(http.MethodPut, "/v1/live-activities/device", body, h.AccessToken("owner"), failed); w.Code != 503 {
		t.Fatalf("failed worker registration=%d %s", w.Code, w.Body.String())
	}
	unsupported := options
	unsupported.Capabilities = protocol.ServerCapabilities()
	if w := request(http.MethodPut, "/v1/live-activities/device", body, h.AccessToken("owner"), unsupported); w.Code != 409 {
		t.Fatalf("unsupported=%d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPut, "/v1/live-activities/device", []byte(strings.Repeat("x", 16385)), h.AccessToken("owner"), options); w.Code != 413 {
		t.Fatalf("oversize=%d", w.Code)
	}
	if w := request(http.MethodPut, "/v1/live-activities/device", body, h.AccessToken("owner"), options); w.Code != 200 {
		t.Fatalf("register=%d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPut, "/v1/live-activities/device", body, h.AccessToken("caregiver"), options); w.Code != 403 {
		t.Fatalf("foreign device=%d %s", w.Code, w.Body.String())
	}
	baby := testkit.MustMutation(t, "baby.json")
	if _, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	activity := protocol.LiveActivityTokenRegistration{RequestMeta: req.RequestMeta, InstallationID: req.InstallationID, ActivityID: "activity", Token: "ccdd", TokenVersion: 1, Scope: protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: 1, FamilyID: h.FamilyID, BabyID: baby.EntityID, KindRaw: "feed"}, State: "active"}
	activityRaw, _ := json.Marshal(activity)
	if w := request(http.MethodPut, "/v1/live-activities/activity", activityRaw, h.AccessToken("owner"), options); w.Code != 200 {
		t.Fatalf("activity=%d %s", w.Code, w.Body.String())
	}
	activity.Scope.FamilyID = "30000000-0000-4000-8000-000000000099"
	activityRaw, _ = json.Marshal(activity)
	if w := request(http.MethodPut, "/v1/live-activities/activity", activityRaw, h.AccessToken("owner"), options); w.Code != 403 {
		t.Fatalf("foreign family=%d %s", w.Code, w.Body.String())
	}
	viewer := h.AddMember(t, "viewer", "viewer")
	viewerReq := req
	viewerReq.InstallationID = "70000000-0000-4000-8000-000000000002"
	viewerReq.PushToStartToken = "eeff"
	viewerRaw, _ := json.Marshal(viewerReq)
	if w := request(http.MethodPut, "/v1/live-activities/device", viewerRaw, h.AccessToken("viewer"), options); w.Code != 200 {
		t.Fatalf("viewer register=%d %s", w.Code, w.Body.String())
	}
	if _, err = h.Family.Authorize(t.Context(), viewer, h.FamilyID, "mutate_record"); err == nil {
		t.Fatal("registered viewer gained mutation authority")
	}
	for _, version := range []string{"", "0", "-1", "9007199254740992", "1&version=2", "1&extra=1"} {
		if w := request(http.MethodDelete, "/v1/live-activities/device/"+req.InstallationID+"?version="+version, nil, h.AccessToken("owner"), options); w.Code != 400 {
			t.Fatalf("version %q=%d %s", version, w.Code, w.Body.String())
		}
	}
	// A stopped worker refuses new registrations but never blocks the
	// privacy-protecting unregister direction.
	if w := request(http.MethodPut, "/v1/live-activities/activity", activityRaw, h.AccessToken("owner"), failed); w.Code != 503 {
		t.Fatalf("failed worker activity=%d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodDelete, "/v1/live-activities/device/"+req.InstallationID+"?version=2", nil, h.AccessToken("owner"), failed); w.Code != 204 {
		t.Fatalf("unregister with stopped worker=%d %s", w.Code, w.Body.String())
	}
	// Push disabled: the same removal, with the same authentication,
	// authorization and version idempotency, needs no worker, keys or APNs
	// configuration — while registration stays refused and the capability
	// unadvertised.
	for _, path := range []string{"/v1/live-activities/device", "/v1/live-activities/activity"} {
		if w := request(http.MethodPut, path, activityRaw, h.AccessToken("owner"), disabled); w.Code != 503 {
			t.Fatalf("disabled registration %s=%d %s", path, w.Code, w.Body.String())
		}
	}
	if w := request(http.MethodDelete, "/v1/live-activities/device/"+req.InstallationID+"?version=3", nil, h.AccessToken("owner"), disabled); w.Code != 204 {
		t.Fatalf("disabled unregister=%d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodDelete, "/v1/live-activities/device/"+req.InstallationID+"?version=3", nil, h.AccessToken("owner"), disabled); w.Code != 204 {
		t.Fatalf("disabled unregister repeated=%d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodDelete, "/v1/live-activities/device/"+req.InstallationID+"?version=2", nil, h.AccessToken("owner"), disabled); w.Code != 409 {
		t.Fatalf("disabled unregister older version=%d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodDelete, "/v1/live-activities/device/"+req.InstallationID+"?version=4", nil, h.AccessToken("caregiver"), disabled); w.Code != 403 {
		t.Fatalf("disabled unregister foreign device=%d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodDelete, "/v1/live-activities/device/"+req.InstallationID+"?version=4", nil, "", disabled); w.Code != 401 {
		t.Fatalf("disabled unregister unauthenticated=%d", w.Code)
	}
	var revoked bool
	err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		return tx.QueryRow(t.Context(), `SELECT revoked FROM live_activity_devices WHERE installation_id=$1`, req.InstallationID).Scan(&revoked)
	})
	if err != nil || !revoked {
		t.Fatalf("disabled unregister did not revoke: revoked=%t err=%v", revoked, err)
	}
	// The advertised set (what /v1/server and header validation use) does not
	// name push while only the unregistration path is composed.
	for _, capability := range advertisedCapabilities(disabled) {
		if capability == protocol.CapabilityLiveActivityPushV1 {
			t.Fatalf("disabled server advertises push: %v", advertisedCapabilities(disabled))
		}
	}
}

func TestLiveActivityRegistrationRateLimits(t *testing.T) {
	h := testkit.New(t)
	repository, err := limits.NewRepository(h.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := limits.New(limits.Options{Repository: repository, Pepper: [32]byte{1}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	service := liveactivity.NewService(h.Security, exportFamilyResolver{h.Family}, "com.example.liveactivity")
	for device := 0; device < 5; device++ {
		principal, err := h.NewDevice("owner")
		if err != nil {
			t.Fatal(err)
		}
		options := familyHandlerOptions(t, principal, h.Family, nil)
		options.LiveActivityService = service
		options.LiveActivityReadiness = readinessFunc(func(context.Context) error { return nil })
		options.Limiter = limiter
		options.Capabilities = append(protocol.ServerCapabilities(), protocol.CapabilityLiveActivityPushV1)
		for attempt := 0; attempt < 31; attempt++ {
			r := httptest.NewRequest(http.MethodPut, "/v1/live-activities/device", strings.NewReader("{}"))
			r.Header.Set("Authorization", "Bearer access")
			r.Header.Set("X-Baby-Tracker-Protocol", "v1")
			r.Header.Set("X-Baby-Tracker-Capabilities", protocol.CapabilityLiveActivityPushV1)
			w := httptest.NewRecorder()
			New(options).ServeHTTP(w, r)
			want := 400
			if attempt == 30 || device == 4 {
				want = 429
			}
			if w.Code != want {
				t.Fatalf("device=%d attempt=%d status=%d body=%s", device, attempt, w.Code, w.Body.String())
			}
			if want == 429 && w.Header().Get("Retry-After") == "" {
				t.Fatal("missing retry-after")
			}
			if device == 4 {
				break
			}
		}
	}
}
