package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/liveactivity"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
)

const liveActivityTopic = "com.example.babytracker.push-type.liveactivity"

// testInstallation is the installation the route tests register.
const testInstallation = "70000000-0000-4000-8000-000000000001"

// liveActivityServiceStub registers and unregisters nothing (the admission
// tests never reach it).
type liveActivityServiceStub struct{}

func (liveActivityServiceStub) RegisterDevice(context.Context, authority.Request, protocol.LiveActivityDeviceRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	return protocol.LiveActivityRegistrationReceipt{}, errors.New("unexpected registration")
}

func (liveActivityServiceStub) RegisterActivity(context.Context, authority.Request, protocol.LiveActivityTokenRegistration) (protocol.LiveActivityRegistrationReceipt, error) {
	return protocol.LiveActivityRegistrationReceipt{}, errors.New("unexpected registration")
}

func (liveActivityServiceStub) UnregisterDevice(context.Context, authority.Request, string, uint64) error {
	return errors.New("unexpected unregistration")
}

// liveActivityHarness is the harness on the database clock: the session
// binding and app.live_activity_authorized check sessions, and the leases
// and retries their stamps, against clock_timestamp().
func liveActivityHarness(t *testing.T) *testkit.Harness {
	t.Helper()
	h := testkit.NewAtDatabaseClock(t)
	h.Now = h.DatabaseNow()
	return h
}

// deviceRegistration is api/fixtures/live-activity-v1/device-registration.json
// (main's wire) for testInstallation.
func deviceRegistration(t *testing.T, h *testkit.Harness) protocol.LiveActivityDeviceRegistration {
	t.Helper()
	raw, err := os.ReadFile("../../api/fixtures/live-activity-v1/device-registration.json")
	if err != nil {
		t.Fatal(err)
	}
	var body protocol.LiveActivityDeviceRegistration
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body.InstallationID = testInstallation
	if err := body.Validate(); err != nil {
		t.Fatal(err)
	}
	return body
}

// activityRegistration is api/fixtures/live-activity-v1/token-registration.json
// for testInstallation and the harness's realm, family and baby.
func activityRegistration(t *testing.T, h *testkit.Harness, baby string) protocol.LiveActivityTokenRegistration {
	t.Helper()
	raw, err := os.ReadFile("../../api/fixtures/live-activity-v1/token-registration.json")
	if err != nil {
		t.Fatal(err)
	}
	var body protocol.LiveActivityTokenRegistration
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body.InstallationID = testInstallation
	body.Scope = protocol.LiveActivityScope{ServerInstanceID: h.Info.InstanceID, RecoveryEpoch: h.Info.RecoveryEpoch, FamilyID: h.FamilyID, BabyID: baby, KindRaw: "feed"}
	body.KnownGenerationID = nil
	if err := body.Validate(); err != nil {
		t.Fatal(err)
	}
	return body
}

// liveActivityHeaders is a Live Activity request's headers with token.
func liveActivityRequestHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token, "X-Baby-Tracker-Protocol": "v1",
		"X-Baby-Tracker-Capabilities": protocol.CapabilityLiveActivityPushV1}
}

func encoded(t *testing.T, body any) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLiveActivityRoutesFollowTheSwitch(t *testing.T) {
	h := liveActivityHarness(t)
	off := newStackWith(t, h, stackConfig{options: func(o *Options) {
		o.LiveActivityService = nil
		o.LiveActivityUnregistration = liveactivity.NewUnregistration(h.Authority)
	}})
	if rec := off.send(t, "owner", http.MethodPut, "/v1/live-activities/device", deviceRegistration(t, h)); rec.Code != http.StatusNotFound {
		t.Fatalf("PUT with push off: %d", rec.Code)
	}
	if rec := off.send(t, "owner", http.MethodPut, "/v1/live-activities/activity", activityRegistration(t, h, testkit.TimerA)); rec.Code != http.StatusNotFound {
		t.Fatalf("activity PUT with push off: %d", rec.Code)
	}
	if rec := off.send(t, "owner", http.MethodDelete, "/v1/live-activities/device/"+testInstallation+"?version=1", nil); rec.Code == http.StatusNotFound {
		t.Fatal("DELETE must work with push off")
	} else if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE with push off: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(off.server(t), "live-activity-push-v1") {
		t.Fatal("advertised with push off")
	}
	on := newStackWith(t, h, stackConfig{options: func(o *Options) {
		o.LiveActivityService = liveactivity.NewService(h.Authority, liveActivityTopic)
		o.LiveActivityReady = func(context.Context) error { return nil }
	}})
	body := deviceRegistration(t, h)
	body.RegistrationVersion = 2 // version 1 was unregistered above
	for _, subject := range []string{"owner", "viewer"} {
		if subject == "viewer" {
			h.AddMember(t, "viewer", "viewer")
			body.InstallationID = "70000000-0000-4000-8000-000000000002"
			body.PushToStartToken = "ccdd"
		}
		if rec := on.send(t, subject, http.MethodPut, "/v1/live-activities/device", body); rec.Code != http.StatusOK {
			t.Fatalf("PUT with push on (%s): %d %s", subject, rec.Code, rec.Body.String())
		}
	}
	if !strings.Contains(on.server(t), "live-activity-push-v1") {
		t.Fatal("not advertised with push on and the worker ready")
	}
	// Neither a worker that is not ready nor a node that is not ready
	// advertises push.
	notReady := newStackWith(t, h, stackConfig{liveActivity: true, options: func(o *Options) {
		o.LiveActivityReady = func(context.Context) error { return errors.New("worker stopped") }
	}})
	if strings.Contains(notReady.server(t), "live-activity-push-v1") {
		t.Fatal("advertised with the worker not ready")
	}
	nodeNotReady := newStackWith(t, h, stackConfig{liveActivity: true, readiness: stackNotReady{}})
	if strings.Contains(nodeNotReady.server(t), "live-activity-push-v1") {
		t.Fatal("advertised with the node not ready")
	}
	// A service without the worker's readiness is not composed push.
	unready := newStackWith(t, h, stackConfig{liveActivity: true, options: func(o *Options) { o.LiveActivityReady = nil }})
	if strings.Contains(unready.server(t), "live-activity-push-v1") {
		t.Fatal("advertised with no worker readiness")
	}
	if rec := unready.send(t, "owner", http.MethodPut, "/v1/live-activities/device", body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT with no worker readiness: %d %s", rec.Code, rec.Body.String())
	}
	// Nothing composed: neither route is served.
	absent := newStackWith(t, h, stackConfig{})
	if rec := absent.send(t, "owner", http.MethodDelete, "/v1/live-activities/device/"+testInstallation+"?version=9", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE with nothing composed: %d", rec.Code)
	}
}

// main's TestLiveActivityRegistrationRoutes on the request seam.
func TestLiveActivityRegistrationRoutes(t *testing.T) {
	h := liveActivityHarness(t)
	on := newStackWith(t, h, stackConfig{liveActivity: true})
	failed := newStackWith(t, h, stackConfig{liveActivity: true, options: func(o *Options) {
		o.LiveActivityReady = func(context.Context) error { return errors.New("worker failed") }
	}})
	disabled := newStackWith(t, h, stackConfig{liveActivity: true, options: func(o *Options) { o.LiveActivityService, o.LiveActivityReady = nil, nil }})
	req := deviceRegistration(t, h)
	body := encoded(t, req)
	put := func(s *stack, path string, body []byte, token string) *httptest.ResponseRecorder {
		t.Helper()
		headers := liveActivityRequestHeaders(token)
		if token == "" {
			headers["Authorization"] = ""
		}
		rec := s.raw(http.MethodPut, path, body, headers)
		if rec.Code != http.StatusNotFound && rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("missing no-store: %d", rec.Code)
		}
		return rec
	}
	del := func(s *stack, query, token string) *httptest.ResponseRecorder {
		t.Helper()
		headers := liveActivityRequestHeaders(token)
		if token == "" {
			headers["Authorization"] = ""
		}
		rec := s.raw(http.MethodDelete, "/v1/live-activities/device/"+testInstallation+query, nil, headers)
		if rec.Code != http.StatusNotFound && rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("missing no-store: %d", rec.Code)
		}
		return rec
	}
	expect := func(label string, rec *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("%s: %d %s, want %d", label, rec.Code, rec.Body.String(), status)
		}
		if code != "" && errorCode(t, rec) != code {
			t.Fatalf("%s: code %s, want %s", label, errorCode(t, rec), code)
		}
	}
	owner, caregiver := h.AccessToken("owner"), h.AccessToken("caregiver")
	for _, path := range []string{"/v1/live-activities/device", "/v1/live-activities/activity"} {
		expect("unauthenticated "+path, put(on, path, body, ""), http.StatusUnauthorized, "access_expired")
		// A bad credential answers before the stopped worker.
		expect("unauthenticated, worker failed "+path, put(failed, path, body, ""), http.StatusUnauthorized, "access_expired")
	}
	expect("disabled", put(disabled, "/v1/live-activities/device", body, owner), http.StatusNotFound, "")
	expect("failed worker registration", put(failed, "/v1/live-activities/device", body, owner), http.StatusServiceUnavailable, "service_unavailable")
	// The stopped worker's 503 outranks the request's own refusals (main's
	// order: availability before headers and body).
	badProtocol := liveActivityRequestHeaders(owner)
	badProtocol["X-Baby-Tracker-Protocol"] = "v2"
	expect("failed worker, bad header", failed.raw(http.MethodPut, "/v1/live-activities/device", body, badProtocol), http.StatusServiceUnavailable, "service_unavailable")
	expect("failed worker, bad body", put(failed, "/v1/live-activities/device", []byte(`{}`), owner), http.StatusServiceUnavailable, "service_unavailable")
	expect("failed worker, oversize", put(failed, "/v1/live-activities/device", []byte(strings.Repeat("x", 16385)), owner), http.StatusServiceUnavailable, "service_unavailable")
	wrongCapability := liveActivityRequestHeaders(owner)
	wrongCapability["X-Baby-Tracker-Capabilities"] = "records-v1"
	expect("unsupported", on.raw(http.MethodPut, "/v1/live-activities/device", body, wrongCapability), http.StatusConflict, "unsupported_capability")
	wrongProtocol := liveActivityRequestHeaders(owner)
	wrongProtocol["X-Baby-Tracker-Protocol"] = "v2"
	expect("unsupported protocol", on.raw(http.MethodPut, "/v1/live-activities/device", body, wrongProtocol), http.StatusBadRequest, "unsupported_protocol")
	expect("oversize", put(on, "/v1/live-activities/device", []byte(strings.Repeat("x", 16385)), owner), http.StatusRequestEntityTooLarge, "body_too_large")
	expect("query", put(on, "/v1/live-activities/device?x=1", body, owner), http.StatusBadRequest, "invalid_request")
	expect("malformed", put(on, "/v1/live-activities/device", []byte(`{}`), owner), http.StatusBadRequest, "")
	expect("register", put(on, "/v1/live-activities/device", body, owner), http.StatusOK, "")
	expect("foreign device", put(on, "/v1/live-activities/device", body, caregiver), http.StatusForbidden, "live_activity_registration_denied")
	changed := req
	changed.FeedBreakMinutes = 60
	expect("changed retry at the same version", put(on, "/v1/live-activities/device", encoded(t, changed), owner), http.StatusConflict, "live_activity_version_conflict")

	baby := h.SeedBabies(t, 1)[0].CanonicalEntity.Meta.ID
	activity := activityRegistration(t, h, baby)
	activityRaw := encoded(t, activity)
	rec := put(on, "/v1/live-activities/activity", activityRaw, owner)
	expect("activity", rec, http.StatusOK, "")
	var receipt protocol.LiveActivityRegistrationReceipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil || receipt.Validate() != nil || len(receipt.Generations) != 1 || receipt.AcceptedTokenVersion == nil || *receipt.AcceptedTokenVersion != activity.TokenVersion {
		t.Fatalf("activity receipt %s (%v)", rec.Body.String(), err)
	}
	if strings.Contains(rec.Body.String(), activity.Token) {
		t.Fatal("the receipt echoed the token")
	}
	foreign := activity
	foreign.Scope.FamilyID = "30000000-0000-4000-8000-000000000099"
	expect("foreign family", put(on, "/v1/live-activities/activity", encoded(t, foreign), owner), http.StatusForbidden, "live_activity_registration_denied")
	// A card for a baby deleted since (on another phone) registers, as on
	// main; a card naming another kind's entity does not.
	deleted := "10000000-0000-4000-8000-0000000000d1"
	caretest.Seed(t, h, caretest.Baby(deleted))
	caretest.Seed(t, h, caretest.DeleteBaby(deleted))
	deletedCard := activity
	deletedCard.ActivityID, deletedCard.Token, deletedCard.Scope.BabyID = "activity-deleted-baby", "abab", deleted
	expect("card for a deleted baby", put(on, "/v1/live-activities/activity", encoded(t, deletedCard), owner), http.StatusOK, "")
	feed := caretest.Feed(baby, 1)
	caretest.Seed(t, h, feed)
	feedCard := activity
	feedCard.ActivityID, feedCard.Token, feedCard.Scope.BabyID = "activity-feed", "adad", feed.EntityID
	expect("card for a feed", put(on, "/v1/live-activities/activity", encoded(t, feedCard), owner), http.StatusForbidden, "live_activity_registration_denied")

	viewer := h.AddMember(t, "viewer", "viewer")
	viewerReq := req
	viewerReq.InstallationID = "70000000-0000-4000-8000-000000000002"
	viewerReq.PushToStartToken = "eeff"
	expect("viewer register", put(on, "/v1/live-activities/device", encoded(t, viewerReq), h.AccessToken("viewer")), http.StatusOK, "")
	digests, err := h.Auth.AccessDigests(h.AccessToken("viewer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Authority.WithRequestTx(t.Context(), authority.Request{Digests: digests}, "mutate_record", func(pgx.Tx, access.Principal, access.Grant) error { return nil }); err == nil {
		t.Fatalf("registered viewer %s gained mutation authority", viewer.AccountID)
	}
	for _, version := range []string{"", "0", "-1", "01", "9007199254740992", "1&version=2", "1&extra=1"} {
		expect("version "+version, del(on, "?version="+version, owner), http.StatusBadRequest, "invalid_request")
	}
	expect("no version", del(on, "", owner), http.StatusBadRequest, "invalid_request")
	badInstallation := on.raw(http.MethodDelete, "/v1/live-activities/device/not-a-uuid?version=1", nil, liveActivityRequestHeaders(owner))
	expect("installation", badInstallation, http.StatusBadRequest, "invalid_request")
	// A stopped worker refuses new registrations but never blocks the
	// privacy-protecting unregister direction.
	expect("failed worker activity", put(failed, "/v1/live-activities/activity", activityRaw, owner), http.StatusServiceUnavailable, "service_unavailable")
	expect("unregister with stopped worker", del(failed, "?version=2", owner), http.StatusNoContent, "")
	// Push disabled: the same removal, with the same authentication,
	// authorization and version idempotency, needs no worker, keys or APNs
	// configuration — while registration is not served and the capability
	// unadvertised.
	for _, path := range []string{"/v1/live-activities/device", "/v1/live-activities/activity"} {
		expect("disabled registration "+path, put(disabled, path, activityRaw, owner), http.StatusNotFound, "")
	}
	expect("disabled unregister", del(disabled, "?version=3", owner), http.StatusNoContent, "")
	expect("disabled unregister repeated", del(disabled, "?version=3", owner), http.StatusNoContent, "")
	expect("disabled unregister older version", del(disabled, "?version=2", owner), http.StatusConflict, "live_activity_version_conflict")
	expect("disabled unregister foreign device", del(disabled, "?version=4", caregiver), http.StatusForbidden, "live_activity_registration_denied")
	expect("disabled unregister unauthenticated", del(disabled, "?version=4", ""), http.StatusUnauthorized, "access_expired")
	revoked := false
	err = pgx.BeginFunc(t.Context(), h.Migrator, func(tx pgx.Tx) error {
		// live_activity_devices is forced-RLS on the tenant coordinates.
		if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
			return err
		}
		return tx.QueryRow(t.Context(), `SELECT revoked FROM live_activity_devices WHERE installation_id=$1`, testInstallation).Scan(&revoked)
	})
	if err != nil || !revoked {
		t.Fatalf("disabled unregister did not revoke: revoked=%t err=%v", revoked, err)
	}
	if slices.Contains(serverCapabilities(t, disabled.handler), protocol.CapabilityLiveActivityPushV1) {
		t.Fatal("disabled server advertises push")
	}
	if !slices.Contains(serverCapabilities(t, on.handler), protocol.CapabilityLiveActivityPushV1) {
		t.Fatal("enabled server does not advertise push")
	}
}

// A session that is not a refreshed family session is refused access_expired
// before any held refusal, as main refused it before its headers.
func TestLiveActivityRequiresAFamilySession(t *testing.T) {
	h := liveActivityHarness(t)
	s := newStackWith(t, h, stackConfig{liveActivity: true})
	if _, err := h.Family.Leave(t.Context(), h.Caregiver, h.CommandID()); err != nil {
		t.Fatal(err)
	}
	account, err := h.Reauthenticate("caregiver")
	if err != nil || account.SessionScope == "family" {
		t.Fatalf("caregiver session %q (%v)", account.SessionScope, err)
	}
	headers := liveActivityRequestHeaders(h.AccessToken("caregiver"))
	headers["X-Baby-Tracker-Protocol"] = "v2" // a held refusal ranks after the session
	for _, rec := range []*httptest.ResponseRecorder{
		s.raw(http.MethodPut, "/v1/live-activities/device", []byte(`{}`), headers),
		s.raw(http.MethodDelete, "/v1/live-activities/device/"+testInstallation+"?version=1", nil, headers),
	} {
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "access_expired" {
			t.Fatalf("an account session: %d %s", rec.Code, rec.Body.String())
		}
	}
}

// main's rate-limit test with valid bodies: the limits are charged after
// family policy (Charge), so a refused body is never charged. Each device
// gets 30 a minute; the account 120.
func TestLiveActivityRegistrationRateLimits(t *testing.T) {
	h := liveActivityHarness(t)
	s := newStackWith(t, h, stackConfig{liveActivity: true})
	for device := 0; device < 5; device++ {
		_, token, err := h.NewDeviceSession("owner")
		if err != nil {
			t.Fatal(err)
		}
		body := deviceRegistration(t, h)
		body.InstallationID = fmt.Sprintf("71000000-0000-4000-8000-%012d", device)
		body.PushToStartToken = fmt.Sprintf("%04x", 0xa000+device)
		raw := encoded(t, body)
		for attempt := 0; attempt < 31; attempt++ {
			rec := s.raw(http.MethodPut, "/v1/live-activities/device", raw, liveActivityRequestHeaders(token))
			want := http.StatusOK
			if attempt == 30 || device == 4 {
				want = http.StatusTooManyRequests
			}
			if rec.Code != want {
				t.Fatalf("device=%d attempt=%d status=%d body=%s", device, attempt, rec.Code, rec.Body.String())
			}
			if want == http.StatusTooManyRequests && (rec.Header().Get("Retry-After") == "" || errorCode(t, rec) != "rate_limited") {
				t.Fatalf("device=%d attempt=%d: %v %s", device, attempt, rec.Header(), rec.Body.String())
			}
			if device == 4 {
				break
			}
		}
	}
	if got := limits.DefaultPolicies()[limits.ScopeLiveActivityDevice]; got != (limits.Policy{Count: 30, Window: time.Minute}) {
		t.Fatalf("device policy %+v", got)
	}
}

// Options' zero Live Activity policies are the defaults, and configured
// ones are charged instead.
func TestLiveActivityPoliciesAreConfigurable(t *testing.T) {
	h := liveActivityHarness(t)
	s := newStackWith(t, h, stackConfig{liveActivity: true, options: func(o *Options) {
		o.LiveActivityDevicePolicy = limits.Policy{Count: 1, Window: time.Minute}
	}})
	body := deviceRegistration(t, h)
	if rec := s.send(t, "owner", http.MethodPut, "/v1/live-activities/device", body); rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body.String())
	}
	if rec := s.send(t, "owner", http.MethodPut, "/v1/live-activities/device", body); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second over a policy of 1: %d %s", rec.Code, rec.Body.String())
	}
}
