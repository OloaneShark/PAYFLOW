package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"payflow/internal/auth"
	"payflow/internal/observability"
)

type authenticator interface {
	Authenticate(context.Context, string) (string, error)
}

func requestID(ctx context.Context) string { return observability.RequestID(ctx) }

func apiMiddleware(next http.Handler, credentials authenticator, metrics *observability.Registry, logger *slog.Logger, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		var raw [12]byte
		_, _ = rand.Read(raw[:])
		id := hex.EncodeToString(raw[:])
		ctx := observability.WithRequestID(r.Context(), id)
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		w.Header().Set("X-Request-ID", id)
		recorder := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		merchant := ""
		defer func() {
			metrics.Observe("payflow_api_response_seconds", time.Since(started))
			logger.Info("api request", "request_id", id, "merchant_id", merchant, "method", r.Method, "route", safeRoute(r.URL.Path), "status", recorder.status, "duration_ms", time.Since(started).Milliseconds())
		}()
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(recorder, r.WithContext(ctx))
			return
		}
		header := r.Header.Get("Authorization")
		key, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || key == "" || strings.TrimSpace(key) != key {
			http.Error(recorder, "API credential required", http.StatusUnauthorized)
			return
		}
		var err error
		merchant, err = credentials.Authenticate(ctx, key)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				http.Error(recorder, "invalid API credential", http.StatusUnauthorized)
			} else {
				metrics.Inc("payflow_database_errors_total")
				http.Error(recorder, "credential lookup unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		ctx = auth.WithMerchant(ctx, merchant)
		next.ServeHTTP(recorder, r.WithContext(ctx))
	})
}

func safeRoute(path string) string {
	switch path {
	case "/healthz", "/readyz",
		"/payflow.v1.PaymentService/CreatePayment",
		"/payflow.v1.PaymentService/GetPayment",
		"/payflow.v1.PaymentService/SimulatePayment",
		"/payflow.v1.PaymentService/ReconcilePayment",
		"/payflow.v1.PaymentService/GetMerchantBalance":
		return path
	default:
		return "other"
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
