package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLivenessAndReadiness(t *testing.T) {
	rec := httptest.NewRecorder()
	Liveness(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("liveness %d", rec.Code)
	}
	called := false
	ready := Readiness(func(context.Context) error { called = true; return nil })
	rec = httptest.NewRecorder()
	ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("readiness %d", rec.Code)
	}
	unavailable := Readiness(func(context.Context) error { return errors.New("secret connection detail") })
	rec = httptest.NewRecorder()
	unavailable(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("unavailable readiness: %d %s", rec.Code, rec.Body.String())
	}
}
