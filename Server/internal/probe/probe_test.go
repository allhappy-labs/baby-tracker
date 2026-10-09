package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckAcceptsHTTP200WithBoundedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	if err := Check(context.Background(), server.URL); err != nil {
		t.Fatalf("Check returned %v, want nil", err)
	}
}

func TestCheckRejectsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	started := time.Now()
	err := Check(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Check returned nil, want timeout error")
	}
	if elapsed := time.Since(started); elapsed >= 2500*time.Millisecond {
		t.Fatalf("Check timed out after %s, want dedicated two-second client timeout", elapsed)
	}
}

func TestCheckRejectsRedirect(t *testing.T) {
	redirectTargetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetCalled = true
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	err := Check(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Check returned nil, want redirect rejection")
	}
	if redirectTargetCalled {
		t.Fatal("probe followed redirect")
	}
}

func TestCheckRejectsChunkedOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Commit headers before writing so net/http sends an unknown-length,
		// chunked response instead of calculating Content-Length.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", 4097)))
	}))
	defer server.Close()

	err := Check(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Check returned nil, want oversized-response error")
	}
}

func TestCheckRejectsNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := Check(context.Background(), server.URL)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Check returned %v, want non-200 error", err)
	}
}
