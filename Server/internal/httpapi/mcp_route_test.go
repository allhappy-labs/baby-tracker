package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"babytracker/server/internal/admission"
)

func TestMCPRouteIsAbsentUnlessConfigured(t *testing.T) {
	handler := New(Options{})
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/mcp", strings.NewReader("{}")))
		if response.Code != http.StatusNotFound {
			t.Fatalf("disabled %s /mcp=%d", method, response.Code)
		}
	}
}

func TestMCPRouteSeesEveryMethodWithoutSyncAdmission(t *testing.T) {
	controller, err := admission.New(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	var syncFree []bool
	mcp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		// If the router had charged a Sync slot for /mcp, only one of two
		// slots would remain for Sync here.
		first, ok1 := controller.TryAcquire(admission.Sync)
		second, ok2 := controller.TryAcquire(admission.Sync)
		syncFree = append(syncFree, ok1 && ok2)
		if ok1 {
			first()
		}
		if ok2 {
			second()
		}
		_, hasDeadline := w.(interface{ Unwrap() http.ResponseWriter })
		if !hasDeadline {
			t.Error("router writer lost Unwrap")
		}
		w.WriteHeader(http.StatusTeapot)
	})
	handler := New(Options{Admission: controller, MCPHandler: mcp})
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodOptions} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/mcp", nil))
		if response.Code != http.StatusTeapot {
			t.Fatalf("%s /mcp=%d", method, response.Code)
		}
	}
	for _, path := range []string{"/mcp/", "/mcp/x", "/MCP"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s=%d, want exact /mcp only", path, response.Code)
		}
	}
	if strings.Join(methods, ",") != "POST,GET,DELETE,OPTIONS" {
		t.Fatalf("methods=%v", methods)
	}
	for i, free := range syncFree {
		if !free {
			t.Fatalf("request %d charged a Sync admission slot", i)
		}
	}
}
