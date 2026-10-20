package main

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const loadtestDatabasePrefix = "payflow_loadtest_"

// loadtestDatabase validates the API's dedicated local database before it
// advertises itself to the load client. The header is an identity check, not
// an authentication mechanism; merchant bearer credentials remain required.
func loadtestDatabase(environment, listenAddress string, cfg *pgxpool.Config) (string, error) {
	if environment != "development" || cfg == nil || !strings.HasPrefix(cfg.ConnConfig.Database, loadtestDatabasePrefix) {
		return "", errors.New("load test mode requires a dedicated development database")
	}
	listenHost, _, listenErr := net.SplitHostPort(listenAddress)
	if listenErr != nil || !loopbackHost(listenHost) || !loopbackHost(cfg.ConnConfig.Host) {
		return "", errors.New("load test mode requires loopback API and PostgreSQL")
	}
	return cfg.ConnConfig.Database, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loadtestHealth(database string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if database != "" {
			w.Header().Set("X-Payflow-Loadtest-Database", database)
		}
		next(w, r)
	}
}
