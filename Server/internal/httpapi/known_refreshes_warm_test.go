package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/admission"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// A process started over existing sessions, its known set warmed from the
// database (as the server warms it before serving), admits a live
// session's refresh, legacy or recoverable, in the Sync class with the auth
// lane full, and then its child; a random token, a consumed parent and a
// revoked session's token were not loaded and take the lane.
func TestAProcessWarmedFromTheDatabaseKnowsTheLiveSessionsRefreshes(t *testing.T) {
	h := testkit.New(t)
	ctx := t.Context()
	consumed := h.RefreshToken("caregiver")
	h.Refresh("caregiver")
	revoked, err := h.SignInDevice(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := h.Auth.Authenticate(ctx, revoked.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Auth.Logout(ctx, principal, h.CommandID()); err != nil {
		t.Fatal(err)
	}

	// The process starts knowing no token, and is warmed.
	known := NewKnownRefreshes(nil)
	digests, err := h.Authority.LiveRefreshDigests(ctx, KnownRefreshWarmLimit)
	if err != nil {
		t.Fatal(err)
	}
	if held := known.Warm(digests); held != 3 {
		t.Fatalf("warmed with %d digests, want the owner's, the caregiver's and the outsider's", held)
	}
	for name, token := range map[string]string{"consumed parent": consumed, "revoked session": revoked.RefreshToken} {
		candidates, err := h.Auth.RefreshDigests(token)
		if err != nil {
			t.Fatal(err)
		}
		if known.known(candidates) {
			t.Fatalf("the %s's refresh token was loaded", name)
		}
	}

	c, err := admission.NewLanes(1, 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var metrics diagnostics.CapacityCollector
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{9}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	options := sessionHandlerOptions(t, h.Auth, "identity_token_only")
	options.Limiter, options.Admission, options.AdmissionMetrics, options.KnownRefreshes = limiter, c, &metrics, known
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	options.RecoveryService = h.Auth
	handler := New(options)
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return r
	}
	recoverable := func(token, attempt string) string {
		raw, err := json.Marshal(map[string]any{"protocol_version": "v1", "capabilities": []string{"identity-v1", "refresh-recovery-v1"},
			"refresh_token": token, "refresh_attempt_id": attempt,
			"device_continuity": map[string]string{"device_id": h.Owner.DeviceID, "credential": h.DeviceCredential("owner")}})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	defer func(release func()) { release() }(mustHold(t, c, admission.Auth))
	for name, token := range map[string]string{"random": laneToken("R"), "consumed parent": consumed, "revoked session": revoked.RefreshToken} {
		if r := post("/v1/auth/refresh", laneRefreshBody(token)); !isServerBusy(t, r) {
			t.Fatalf("a %s token took a sync slot: %d %s", name, r.Code, r.Body)
		}
	}
	r := post("/v1/auth/refresh-recoverable", recoverable(h.RefreshToken("owner"), "aaaaaaaa-0000-4000-8000-0000000000a1"))
	if r.Code != http.StatusOK {
		t.Fatalf("the owner's live session's recoverable refresh with the lane full: %d %s", r.Code, r.Body)
	}
	var child protocol.RecoverableRefreshResponse
	if err := json.Unmarshal(r.Body.Bytes(), &child); err != nil {
		t.Fatal(err)
	}
	if r := post("/v1/auth/refresh", laneRefreshBody(h.RefreshToken("caregiver"))); r.Code != http.StatusOK {
		t.Fatalf("the caregiver's live session's refresh with the lane full: %d %s", r.Code, r.Body)
	}
	if r := post("/v1/auth/refresh-recoverable", recoverable(child.RefreshToken, "aaaaaaaa-0000-4000-8000-0000000000a2")); r.Code != http.StatusOK {
		t.Fatalf("a warmed session's child with the lane full: %d %s", r.Code, r.Body)
	}
	counts := metrics.Snapshot().Admission
	if got := counts[diagnostics.SyncAdmission]; got.Accepted != 3 || got.Rejected != 0 {
		t.Fatalf("sync metrics %+v", got)
	}
	if got := counts[diagnostics.AuthAdmission]; got.Accepted != 0 || got.Rejected != 3 {
		t.Fatalf("auth metrics %+v", got)
	}
}

// mustHold takes class's only slot and returns its release.
func mustHold(t *testing.T, c *admission.Controller, class admission.Class) func() {
	t.Helper()
	release, ok := c.TryAcquire(class)
	if !ok {
		t.Fatalf("class %d already full", class)
	}
	return release
}
