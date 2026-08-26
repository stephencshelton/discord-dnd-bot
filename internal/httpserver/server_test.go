package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func doRequest(t *testing.T, s *Server, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestHealthz(t *testing.T) {
	s := New(":0", nil, nil)
	code, body := doRequest(t, s, "/healthz")
	if code != http.StatusOK || body != "ok" {
		t.Errorf("healthz = %d %q, want 200 ok", code, body)
	}
}

func TestReadyzNilAlwaysReady(t *testing.T) {
	s := New(":0", nil, nil)
	code, body := doRequest(t, s, "/readyz")
	if code != http.StatusOK || body != "ready" {
		t.Errorf("readyz = %d %q, want 200 ready", code, body)
	}
}

func TestReadyzReady(t *testing.T) {
	s := New(":0", nil, func(ctx context.Context) error { return nil })
	code, _ := doRequest(t, s, "/readyz")
	if code != http.StatusOK {
		t.Errorf("readyz = %d, want 200", code)
	}
}

func TestReadyzNotReady(t *testing.T) {
	s := New(":0", nil, func(ctx context.Context) error { return errors.New("db unreachable") })
	code, body := doRequest(t, s, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d, want 503", code)
	}
	if body != "db unreachable" {
		t.Errorf("readyz body = %q, want error message", body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s := New(":0", nil, nil)
	code, _ := doRequest(t, s, "/metrics")
	if code != http.StatusOK {
		t.Errorf("metrics = %d, want 200", code)
	}
}

func TestPanicRecovery(t *testing.T) {
	// A readiness func that panics must be caught by the recovery middleware
	// and surfaced as a 500 rather than crashing the process.
	s := New(":0", nil, func(context.Context) error { panic("boom") })
	code, _ := doRequest(t, s, "/readyz")
	if code != http.StatusInternalServerError {
		t.Errorf("panicking handler = %d, want 500", code)
	}
}
