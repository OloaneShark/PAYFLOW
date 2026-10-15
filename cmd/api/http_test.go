package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"payflow/internal/auth"
	"payflow/internal/observability"
)

type testAuthenticator struct{}

func (testAuthenticator) Authenticate(_ context.Context, key string) (string, error) {
	if key == "pfk_valid" {
		return "merchant", nil
	}
	return "", auth.ErrUnauthenticated
}

func TestAPIMiddlewareAuthenticationAndNoKeyInLogs(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	metrics := observability.New()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		merchant, ok := auth.Merchant(r.Context())
		if !ok || merchant != "merchant" || requestID(r.Context()) == "" {
			t.Error("missing request context")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := apiMiddleware(next, testAuthenticator{}, metrics, logger, time.Second)
	for _, tc := range []struct {
		key    string
		status int
	}{{"", 401}, {"Bearer wrong", 401}, {"Bearer pfk_valid", 204}} {
		req := httptest.NewRequest(http.MethodPost, "/payflow.v1.PaymentService/GetPayment", nil)
		if tc.key != "" {
			req.Header.Set("Authorization", tc.key)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%q: %d", tc.key, rec.Code)
		}
	}
	if strings.Contains(logs.String(), "pfk_valid") || strings.Contains(logs.String(), "wrong") {
		t.Fatal("credential leaked to logs")
	}
	rec := httptest.NewRecorder()
	metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "payflow_api_response_seconds_count 3") || strings.Contains(rec.Body.String(), "merchant") {
		t.Fatalf("invalid metrics: %s", rec.Body.String())
	}
}
