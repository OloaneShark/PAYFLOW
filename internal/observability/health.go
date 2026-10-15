package observability

import (
	"context"
	"net/http"
)

func Liveness(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

// Readiness reports only availability, without exposing dependency errors or
// connection details. Callers provide their database and Temporal probes.
func Readiness(checks ...func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, check := range checks {
			if err := check(r.Context()); err != nil {
				http.Error(w, "dependency unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}
}
