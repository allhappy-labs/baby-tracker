package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// shape is a request's statements on the authority's connection: the
// app.request_authority calls, the transactions begun and committed, and
// whether it read the family the way the legacy family read did (its
// members) or locked the family row.
type shape struct {
	trips, authority, begins, commits int
	familyRead, familyLock            bool
}

func shapeOf(w *testkit.Wire) shape {
	out := shape{trips: w.Trips()}
	for _, sql := range w.Statements() {
		lower := strings.ToLower(strings.TrimSpace(sql))
		switch {
		case strings.Contains(lower, "app.request_authority"):
			out.authority++
		case strings.HasPrefix(lower, "begin"):
			out.begins++
		case lower == "commit":
			out.commits++
		}
		if strings.Contains(lower, "from memberships m join family_actor_labels") {
			out.familyRead = true
		}
		if strings.Contains(lower, "for no key update") && strings.Contains(lower, "from families") {
			out.familyLock = true
		}
	}
	return out
}

// measure is one request's shape after warm prepared its statements.
func (s *routeStack) measure(t *testing.T, label string, warm, measured func() *httptest.ResponseRecorder, status int) shape {
	t.Helper()
	if warm != nil {
		if rec := warm(); rec.Code != status {
			t.Fatalf("%s (warm): status %d, want %d: %s", label, rec.Code, status, rec.Body.String())
		}
	}
	s.wire.Reset()
	if rec := measured(); rec.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", label, rec.Code, status, rec.Body.String())
	}
	got := shapeOf(s.wire)
	t.Logf("%s: %d round trips, %d statements: %q", label, got.trips, len(s.wire.Statements()), s.wire.Statements())
	return got
}

// An import request is the one authority statement (credential, the
// route's refusals, family policy, the charge) and then the import
// service's one guarded transaction; neither reads the family as the
// legacy family read did. A status read takes no family lock. A refusal
// before the service costs the one statement.
func TestImportHTTPRoundTrips(t *testing.T) {
	h := testkit.New(t)
	const ownerImport = "8a000000-0000-4000-8000-000000000001"
	h.Import(t, h.Owner, ownerImport, "Mira", "2026-01-01")
	s := newRouteStack(t, h, routeConfig{})
	status := func() *httptest.ResponseRecorder {
		return s.send(t, "owner", http.MethodGet, "/v1/family/import/"+ownerImport, nil)
	}
	got := s.measure(t, "status", status, status, http.StatusOK)
	if want := (shape{trips: 7, authority: 2, begins: 1, commits: 1}); got != want {
		t.Errorf("status: %+v, want %+v", got, want)
	}
	next := 10
	resolve := func() *httptest.ResponseRecorder {
		next++
		return s.send(t, "owner", http.MethodPost, "/v1/family/import/resolve", mustJSON(t, testkit.ImportRequest(next, "Baby", "2026-01-01")))
	}
	got = s.measure(t, "resolve", resolve, resolve, http.StatusOK)
	if got.authority != 2 || got.begins != 1 || got.commits != 1 || got.familyRead {
		t.Errorf("resolve: %+v, want the authority statement, then one guarded transaction", got)
	}
	if want := 29; got.trips != want {
		t.Errorf("resolve: %d round trips, want %d", got.trips, want)
	}
	malformed := func() *httptest.ResponseRecorder {
		return s.send(t, "owner", http.MethodPost, "/v1/family/import/resolve", []byte(`{"protocol_version":`))
	}
	if got = s.measure(t, "held 400", malformed, malformed, http.StatusBadRequest); got != (shape{trips: 1, authority: 1}) {
		t.Errorf("a held refusal: %+v, want the one statement", got)
	}
	unknown := func() *httptest.ResponseRecorder {
		return s.send(t, "caregiver", http.MethodGet, "/v1/family/import/"+ownerImport, nil)
	}
	if got = s.measure(t, "another member's import", unknown, unknown, http.StatusForbidden); got != (shape{trips: 4, authority: 2, begins: 1}) {
		t.Errorf("another member's import: %+v", got)
	}
}

// An export request is the one authority statement (credential, the
// route's refusals, the charge, family policy) and then the export
// service's guarded transaction (a download reads its status, then its
// archive: two).
func TestExportHTTPRoundTrips(t *testing.T) {
	h := testkit.New(t)
	s := newRouteStack(t, h, routeConfig{})
	body := func(id string) []byte {
		return []byte(`{"protocol_version":"v1","capabilities":["export-v1"],"request_id":"` + id + `"}`)
	}
	// The owner's first export prepares the statements; the caregiver's
	// is refused owner_required inside the guarded transaction.
	if rec := s.send(t, "owner", http.MethodPost, "/v1/export", body("30000000-0000-4000-8000-00000000e001")); rec.Code != http.StatusAccepted {
		t.Fatalf("first export: %d %s", rec.Code, rec.Body.String())
	}
	replay := func() *httptest.ResponseRecorder {
		return s.send(t, "owner", http.MethodPost, "/v1/export", body("30000000-0000-4000-8000-00000000e001"))
	}
	got := s.measure(t, "create (replay)", replay, replay, http.StatusOK)
	if want := (shape{trips: 4, authority: 2, begins: 1, commits: 1}); got != want {
		t.Errorf("create: %+v, want %+v", got, want)
	}
	var created protocol.ExportStatus
	if err := json.Unmarshal(replay().Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if err := s.exports.RunOnce(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	status := func() *httptest.ResponseRecorder {
		return s.send(t, "owner", http.MethodGet, "/v1/export/"+created.ExportID, nil)
	}
	got = s.measure(t, "status", status, status, http.StatusOK)
	if want := (shape{trips: 4, authority: 2, begins: 1, commits: 1}); got != want {
		t.Errorf("status: %+v, want %+v", got, want)
	}
	download := func() *httptest.ResponseRecorder {
		return s.send(t, "owner", http.MethodGet, "/v1/export/"+created.ExportID+"/download", nil)
	}
	got = s.measure(t, "download", download, download, http.StatusOK)
	if want := (shape{trips: 9, authority: 3, begins: 2, commits: 2}); got != want {
		t.Errorf("download: %+v, want %+v: the authority statement, then the status and archive transactions", got, want)
	}
	caregiver := func() *httptest.ResponseRecorder {
		return s.send(t, "caregiver", http.MethodPost, "/v1/export", body("30000000-0000-4000-8000-00000000e002"))
	}
	if got = s.measure(t, "caregiver", caregiver, caregiver, http.StatusForbidden); got != (shape{trips: 3, authority: 2, begins: 1}) {
		t.Errorf("caregiver: %+v", got)
	}
	malformed := func() *httptest.ResponseRecorder {
		return s.send(t, "owner", http.MethodPost, "/v1/export", []byte(`{"protocol_version":`))
	}
	if got = s.measure(t, "held 400", malformed, malformed, http.StatusBadRequest); got != (shape{trips: 1, authority: 1}) {
		t.Errorf("a held refusal: %+v, want the one statement", got)
	}
}
