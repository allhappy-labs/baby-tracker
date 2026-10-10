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
