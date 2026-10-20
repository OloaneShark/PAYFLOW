package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoadtestDatabaseGuard(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://payflow:local@127.0.0.1:15432/payflow_loadtest_smoke?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if name, err := loadtestDatabase("development", "127.0.0.1:18080", cfg); err != nil || name != "payflow_loadtest_smoke" {
		t.Fatalf("dedicated database: %q %v", name, err)
	}
	for _, tc := range []struct{ env, listen, database, host string }{
		{"production", "127.0.0.1:18080", "payflow_loadtest_smoke", "127.0.0.1"},
		{"development", "0.0.0.0:18080", "payflow_loadtest_smoke", "127.0.0.1"},
		{"development", "127.0.0.1:18080", "payflow", "127.0.0.1"},
		{"development", "127.0.0.1:18080", "payflow_loadtest_smoke", "db.example.com"},
	} {
		cfg.ConnConfig.Database, cfg.ConnConfig.Host = tc.database, tc.host
		if _, err := loadtestDatabase(tc.env, tc.listen, cfg); err == nil {
			t.Fatalf("accepted unsafe load test configuration: %+v", tc)
		}
	}
}

func TestLoadtestHealthMarker(t *testing.T) {
	w := httptest.NewRecorder()
	loadtestHealth("payflow_loadtest_smoke", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := w.Header().Get("X-Payflow-Loadtest-Database"); got != "payflow_loadtest_smoke" {
		t.Fatalf("marker = %q", got)
	}
}
