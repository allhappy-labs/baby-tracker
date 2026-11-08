package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// refreshStack is the production auth handlers with the session and
// recovery services' authority on a one-connection counted pool and the
// in-memory limiter.
type refreshStack struct {
	h       *testkit.Harness
	wire    *testkit.Wire
	handler http.Handler
}

func newRefreshStack(t *testing.T, h *testkit.Harness) *refreshStack {
	t.Helper()
	pool, wire := testkit.CountedPool(t, h.Runtime)
	service := h.AuthOn(t, pool)
	limiter, err := limits.New(limits.Options{Pepper: [32]byte{9}, Now: func() time.Time { return h.Now }})
	if err != nil {
		t.Fatal(err)
	}
	options := sessionHandlerOptions(t, service, "identity_token_only")
	options.Limiter = limiter
	options.RecoveryEnabled = true
	options.RecoveryReadiness = accountGateFunc(func(context.Context) error { return nil })
	options.RecoveryService = service
	return &refreshStack{h: h, wire: wire, handler: New(options)}
}

func (s *refreshStack) post(t *testing.T, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	r := httptest.NewRecorder()
	s.handler.ServeHTTP(r, request)
	return r
}

func (s *refreshStack) recoverable(t *testing.T, refreshToken, attempt, credential string) *httptest.ResponseRecorder {
	t.Helper()
	return s.post(t, "/v1/auth/refresh-recoverable", "", map[string]any{"protocol_version": "v1",
		"capabilities": []string{"identity-v1", "refresh-recovery-v1"}, "refresh_token": refreshToken, "refresh_attempt_id": attempt,
		"device_continuity": map[string]string{"device_id": s.h.Owner.DeviceID, "credential": credential}})
}

func (s *refreshStack) ack(t *testing.T, accessToken, attempt, credential string) *httptest.ResponseRecorder {
	t.Helper()
	return s.post(t, "/v1/auth/refresh-ack", accessToken, map[string]any{"protocol_version": "v1",
		"capabilities": []string{"identity-v1", "refresh-recovery-v1"}, "refresh_attempt_id": attempt,
		"device_continuity": map[string]string{"device_id": s.h.Owner.DeviceID, "credential": credential}})
}

// refreshShape is a refresh route's wire shape: round trips, statements,
// transactions begun (BEGIN statements), commits and rollbacks sent, and
// the statements that read a refresh token's session.
type refreshShape struct {
	trips, statements, begins, commits, rollbacks, parentReads, authority int
}

func refreshShapeOf(w *testkit.Wire) refreshShape {
	out := refreshShape{trips: w.Trips()}
	for _, sql := range w.Statements() {
		out.statements++
		lower := strings.ToLower(strings.TrimSpace(sql))
		switch {
		case strings.HasPrefix(lower, "begin"):
			out.begins++
		case lower == "commit":
			out.commits++
		case lower == "rollback":
			out.rollbacks++
		}
		if strings.Contains(lower, "refresh_digest = any(") {
			out.parentReads++
		}
		if strings.Contains(lower, "app.request_authority") {
			out.authority++
		}
	}
	return out
}

// measure resets the wire, runs send (which must answer status) and
// returns its shape.
func (s *refreshStack) measure(t *testing.T, label string, status int, send func() *httptest.ResponseRecorder) (refreshShape, *httptest.ResponseRecorder) {
	t.Helper()
	s.wire.Reset()
	r := send()
	if r.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", label, r.Code, status, r.Body.String())
	}
	got := refreshShapeOf(s.wire)
	t.Logf("%s: %+v: %q", label, got, s.wire.Statements())
	return got, r
}

func rotated(t *testing.T, r *httptest.ResponseRecorder) protocol.RecoverableRefreshResponse {
	t.Helper()
	var out protocol.RecoverableRefreshResponse
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A recoverable refresh is four round trips (it was twelve in two
// transactions): the quota subject is one read-only transaction of one
// round trip (BEGIN, the authority flag, the session with its account and
// device, the rollback), then the rotation reuses the session it found:
// BEGIN, the flag, the account lock, the parent re-read FOR UPDATE and the
// account's authority in one round trip, then the write batch, then the
// commit. The session is read by its refresh digest once. A lost
// acknowledgement's exact retry is the same subject read and lock round
// trip, then the child and the receipt, then the rollback; a wrong device
// proof costs the subject read alone.
func TestRecoverableRefreshRoundTrips(t *testing.T) {
	h := testkit.New(t)
	s := newRefreshStack(t, h)
	credential := h.DeviceCredential("owner")
	// The first rotation and its retry prepare the statements on the
	// connection.
	for range 2 {
		if r := s.recoverable(t, h.RefreshToken("owner"), "aaaaaaaa-0000-4000-8000-000000000001", credential); r.Code != http.StatusOK {
			t.Fatalf("first rotation: %d %s", r.Code, r.Body.String())
		}
	}
	first := s.recoverable(t, h.RefreshToken("owner"), "aaaaaaaa-0000-4000-8000-000000000001", credential)
	parent := rotated(t, first)

	const attempt = "aaaaaaaa-0000-4000-8000-000000000002"
	got, r := s.measure(t, "rotation", http.StatusOK, func() *httptest.ResponseRecorder {
		return s.recoverable(t, parent.RefreshToken, attempt, credential)
	})
	if want := (refreshShape{trips: 4, statements: 14, begins: 2, commits: 1, rollbacks: 1, parentReads: 1}); got != want {
		t.Errorf("rotation: %+v, want %+v", got, want)
	}
	child := rotated(t, r)

	got, r = s.measure(t, "lost-acknowledgement retry", http.StatusOK, func() *httptest.ResponseRecorder {
		return s.recoverable(t, parent.RefreshToken, attempt, credential)
	})
	if retry := rotated(t, r); retry.AccessToken != child.AccessToken || retry.RefreshToken != child.RefreshToken {
		t.Fatal("the retry did not return the committed child")
	}
	if want := (refreshShape{trips: 5, statements: 12, begins: 2, rollbacks: 2, parentReads: 1}); got != want {
		t.Errorf("retry: %+v, want %+v", got, want)
	}

	got, _ = s.measure(t, "wrong device proof", http.StatusBadRequest, func() *httptest.ResponseRecorder {
		return s.recoverable(t, child.RefreshToken, "aaaaaaaa-0000-4000-8000-000000000003", strings.Repeat("D", 43))
	})
	if want := (refreshShape{trips: 1, statements: 4, begins: 1, rollbacks: 1, parentReads: 1}); got != want {
		t.Errorf("wrong proof: %+v, want %+v", got, want)
	}
}

// A legacy refresh reads its quota subject in one round trip and, without a
// found session to reuse, looks the session up again inside the rotation,
// pipelined with BEGIN: five round trips (it was twelve).
func TestLegacyRefreshRoundTrips(t *testing.T) {
	h := testkit.New(t)
	s := newRefreshStack(t, h)
	legacy := func(token string) *httptest.ResponseRecorder {
		return s.post(t, "/v1/auth/refresh", "", map[string]any{"protocol_version": "v1", "capabilities": protocol.IdentityCapabilities(), "refresh_token": token})
	}
	first := legacy(h.RefreshToken("owner"))
	if first.Code != http.StatusOK {
		t.Fatalf("first rotation: %d %s", first.Code, first.Body.String())
	}
	var parent protocol.SessionResponse
	if err := json.Unmarshal(first.Body.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	got, _ := s.measure(t, "legacy rotation", http.StatusOK, func() *httptest.ResponseRecorder { return legacy(parent.RefreshToken) })
	if want := (refreshShape{trips: 5, statements: 14, begins: 2, commits: 1, rollbacks: 1, parentReads: 2}); got != want {
		t.Errorf("legacy rotation: %+v, want %+v", got, want)
	}
}

// A refresh acknowledgement is two round trips in one transaction (it was
// nine: the bearer's authentication, then the acknowledgement's six reads,
// DELETE and commit): BEGIN, app.request_authority by the bearer's digests,
// the authority flag, the session with its account and device and the
// attempt's receipts, pipelined; then the DELETE with the commit. With no
// receipt left, or a refusal, the second round trip is the rollback.
func TestRefreshAckRoundTrips(t *testing.T) {
	h := testkit.New(t)
	s := newRefreshStack(t, h)
	credential := h.DeviceCredential("owner")
	rotate := func(refreshToken, attempt string) protocol.RecoverableRefreshResponse {
		r := s.recoverable(t, refreshToken, attempt, credential)
		if r.Code != http.StatusOK {
			t.Fatalf("rotation: %d %s", r.Code, r.Body.String())
		}
		return rotated(t, r)
	}
	// The first acknowledgement prepares the statements on the connection.
	const firstAttempt, attempt = "bbbbbbbb-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002"
	first := rotate(h.RefreshToken("owner"), firstAttempt)
	if r := s.ack(t, first.AccessToken, firstAttempt, credential); r.Code != http.StatusNoContent {
		t.Fatalf("first ack: %d %s", r.Code, r.Body.String())
	}
	child := rotate(first.RefreshToken, attempt)

	got, _ := s.measure(t, "ack", http.StatusNoContent, func() *httptest.ResponseRecorder { return s.ack(t, child.AccessToken, attempt, credential) })
	if want := (refreshShape{trips: 2, statements: 7, begins: 1, commits: 1, authority: 1}); got != want {
		t.Errorf("ack: %+v, want %+v", got, want)
	}
	got, _ = s.measure(t, "ack with no receipt", http.StatusNoContent, func() *httptest.ResponseRecorder { return s.ack(t, child.AccessToken, attempt, credential) })
	if want := (refreshShape{trips: 2, statements: 6, begins: 1, rollbacks: 1, authority: 1}); got != want {
		t.Errorf("ack with no receipt: %+v, want %+v", got, want)
	}
	got, _ = s.measure(t, "wrong device proof", http.StatusBadRequest, func() *httptest.ResponseRecorder {
		return s.ack(t, child.AccessToken, attempt, strings.Repeat("D", 43))
	})
	if want := (refreshShape{trips: 2, statements: 6, begins: 1, rollbacks: 1, authority: 1}); got != want {
		t.Errorf("wrong proof: %+v, want %+v", got, want)
	}
	if _, err := h.Auth.RefreshRecoverable(t.Context(), auth.RecoverableRefreshInput{RefreshToken: first.RefreshToken, AttemptID: attempt,
		DeviceContinuity: identity.DeviceContinuityClaim{DeviceID: h.Owner.DeviceID, Credential: credential}}); auth.Code(err) != "refresh_receipt_unavailable" {
		t.Fatalf("the acknowledged receipt is still there: %v", err)
	}
}
