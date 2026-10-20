package loadtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPreflightRequiresDedicatedAPIMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Payflow-Loadtest-Database", "payflow_loadtest_smoke")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := preflight(context.Background(), server.Client(), server.URL, "payflow_loadtest_smoke"); err != nil {
		t.Fatal(err)
	}
	if err := preflight(context.Background(), server.Client(), server.URL, "payflow_loadtest_other"); err == nil {
		t.Fatal("accepted an API using a different database")
	}
}
