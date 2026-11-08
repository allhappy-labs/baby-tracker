package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"babytracker/server/internal/limits"
)

// TestLiveActivityRegistrationRoundTrips pins the Live Activity routes on a
// counted pool, after a warm request has prepared their statements. A device
// registration is one guarded transaction: BEGIN+authority (one pipelined
// round trip), the session binding and the installation fence (one
// statement), the version-CAS upsert (which also answers an identical
// retry), the registration state, COMMIT. A card token is BEGIN+authority,
// the binding, the registration row, the baby, the registration state, the
// token upsert, COMMIT. An unregistration is BEGIN+authority, the binding and
// fence, the revoke, COMMIT. A refusal after authentication (a held
// refusal, the rate limit) is BEGIN+authority and ROLLBACK.
func TestLiveActivityRegistrationRoundTrips(t *testing.T) {
	h := liveActivityHarness(t)
	baby := h.SeedBabies(t, 1)[0].CanonicalEntity.Meta.ID
	s := newStackWith(t, h, stackConfig{liveActivity: true})
	installations := 0
	fresh := func() *httptest.ResponseRecorder {
		installations++
		body := deviceRegistration(t, h)
		body.InstallationID = fmt.Sprintf("72000000-0000-4000-8000-%012d", installations)
		body.PushToStartToken = fmt.Sprintf("%04x", 0xb000+installations)
		return s.send(t, "owner", http.MethodPut, "/v1/live-activities/device", body)
	}
	if got := s.trips(t, "device registration", fresh, fresh, http.StatusOK); got != 5 {
		t.Errorf("a device registration: %d round trips, want 5: %q", got, s.wire.Statements())
	}
	retry := func() *httptest.ResponseRecorder {
		body := deviceRegistration(t, h)
		body.InstallationID = fmt.Sprintf("72000000-0000-4000-8000-%012d", installations)
		body.PushToStartToken = fmt.Sprintf("%04x", 0xb000+installations)
		return s.send(t, "owner", http.MethodPut, "/v1/live-activities/device", body)
	}
	if got := s.trips(t, "identical retry", retry, retry, http.StatusOK); got != 5 {
		t.Errorf("an identical retry: %d round trips, want 5: %q", got, s.wire.Statements())
	}
	activityVersion := uint64(0)
	activity := func() *httptest.ResponseRecorder {
		activityVersion++
		body := activityRegistration(t, h, baby)
		body.InstallationID = fmt.Sprintf("72000000-0000-4000-8000-%012d", installations)
		body.TokenVersion = activityVersion
		return s.send(t, "owner", http.MethodPut, "/v1/live-activities/activity", body)
	}
	if got := s.trips(t, "card token", activity, activity, http.StatusOK); got != 7 {
		t.Errorf("a card token: %d round trips, want 7: %q", got, s.wire.Statements())
	}
	unregistrationVersion := uint64(1)
	unregister := func() *httptest.ResponseRecorder {
		unregistrationVersion++
		return s.send(t, "owner", http.MethodDelete, fmt.Sprintf("/v1/live-activities/device/%s?version=%d", testInstallation, unregistrationVersion), nil)
	}
	if got := s.trips(t, "unregistration", unregister, unregister, http.StatusNoContent); got != 4 {
		t.Errorf("an unregistration: %d round trips, want 4: %q", got, s.wire.Statements())
	}
	held := func() *httptest.ResponseRecorder {
		return s.raw(http.MethodPut, "/v1/live-activities/device", []byte(`{}`), liveActivityRequestHeaders(h.AccessToken("owner")))
	}
	if got := s.trips(t, "held 400", held, held, http.StatusBadRequest); got != 2 {
		t.Errorf("a held refusal: %d round trips, want 2: %q", got, s.wire.Statements())
	}
	s.assertRefusalRollsBack(t, "held 400")
	// Phase 5 Task 19: a bearer the stack has not seen authenticate is
	// checked before its body, one statement more than a known bearer's
	// request; one the check refuses costs that statement alone, with no
	// transaction.
	firstSeen := deviceRegistration(t, h)
	firstSeen.InstallationID = "72000000-0000-4000-8000-000000000999"
	firstSeen.PushToStartToken = "c999"
	s.wire.Reset()
	if rec := s.send(t, "caregiver", http.MethodPut, "/v1/live-activities/device", firstSeen); rec.Code != http.StatusOK || s.wire.Trips() != 6 {
		t.Errorf("a first-seen bearer's device registration: %d, %d round trips, want 200 and 6: %q", rec.Code, s.wire.Trips(), s.wire.Statements())
	}
	for _, route := range []string{"/v1/live-activities/device", "/v1/live-activities/activity"} {
		s.wire.Reset()
		rec := s.raw(http.MethodPut, route, []byte(`{}`), liveActivityRequestHeaders(fakeBearer(t)))
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "session_revoked" || s.wire.Trips() != 1 {
			t.Errorf("%s with a made-up bearer: %d %s, %d round trips, want 401 session_revoked and 1", route, rec.Code, rec.Body.String(), s.wire.Trips())
		}
		s.assertRefusedBeforeBody(t, route+" made-up bearer")
	}
	limited := newStackWith(t, h, stackConfig{liveActivity: true, options: func(o *Options) {
		o.LiveActivityDevicePolicy = limits.Policy{Count: 1, Window: time.Hour}
	}})
	again := func() *httptest.ResponseRecorder {
		body := deviceRegistration(t, h)
		body.InstallationID = fmt.Sprintf("72000000-0000-4000-8000-%012d", installations)
		body.PushToStartToken = fmt.Sprintf("%04x", 0xb000+installations)
		return limited.send(t, "owner", http.MethodPut, "/v1/live-activities/device", body)
	}
	if got := limited.trips(t, "rate limited", again, again, http.StatusTooManyRequests); got != 2 {
		t.Errorf("a rate-limited registration: %d round trips, want 2: %q", got, limited.wire.Statements())
	}
	limited.assertRefusalRollsBack(t, "rate limited")
}
