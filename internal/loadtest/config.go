package loadtest

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Preset           string
	Workload         string
	Requests         int
	Clients          int
	Duration         time.Duration
	Warmup           time.Duration
	Trials           int
	RequestTimeout   time.Duration
	SettleTimeout    time.Duration
	API              string
	DatabaseURL      string
	Environment      string
	AllowLocalTest   bool
	StressOptIn      bool
	TemporalVersion  string
	TemporalAddress  string
	TemporalNS       string
	RecoveryScenario string
	OutputFormat     string
	OutputDir        string
}

var workloads = map[string]bool{
	"create": true, "get": true, "submit": true, "repeat": true,
	"concurrent-submit": true, "mixed": true, "balance": true,
}

func (c *Config) Defaults() error {
	if c.Preset == "" {
		c.Preset = "smoke"
	}
	var requests, clients int
	switch c.Preset {
	case "smoke":
		requests, clients = 20, 2
	case "baseline":
		requests, clients = 200, 8
	case "stress":
		requests, clients = 1000, 16
		if !c.StressOptIn {
			return errors.New("stress preset requires -stress-opt-in")
		}
	case "sustained":
		requests, clients = 5000, 8
		if c.Duration == 0 {
			c.Duration = 2 * time.Minute
		}
		if c.Warmup == 0 {
			c.Warmup = 10 * time.Second
		}
		if !c.StressOptIn {
			return errors.New("sustained preset requires -stress-opt-in")
		}
	case "recovery":
		requests, clients = 50, 4
	default:
		return errors.New("unknown benchmark preset")
	}
	if c.Requests == 0 {
		c.Requests = requests
	}
	if c.Clients == 0 {
		c.Clients = clients
	}
	if c.Trials == 0 {
		c.Trials = 1
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 5 * time.Second
	}
	if c.SettleTimeout == 0 {
		c.SettleTimeout = 45 * time.Second
	}
	if c.Workload == "" {
		c.Workload = "mixed"
	}
	if c.API == "" {
		c.API = "http://127.0.0.1:18080"
	}
	if c.OutputFormat == "" {
		c.OutputFormat = "all"
	}
	if c.OutputDir == "" {
		c.OutputDir = "results/loadtest"
	}
	if c.TemporalVersion == "" {
		c.TemporalVersion = "unavailable (Compose pins temporalio/temporal:1.9.1)"
	}
	if c.TemporalAddress == "" {
		c.TemporalAddress = "127.0.0.1:17233"
	}
	if c.TemporalNS == "" {
		c.TemporalNS = "default"
	}
	return c.Validate()
}

func (c Config) Validate() error {
	if !c.AllowLocalTest || c.Environment != "development" {
		return errors.New("dedicated local-test opt-in and development environment are required")
	}
	if !workloads[c.Workload] || c.Requests < 1 || c.Requests > 5000 || c.Clients < 1 || c.Clients > 64 ||
		c.Duration < 0 || c.Duration > 5*time.Minute || c.Warmup < 0 || c.Warmup > 30*time.Second ||
		c.Trials < 1 || c.Trials > 5 || c.RequestTimeout <= 0 || c.RequestTimeout > time.Minute ||
		c.SettleTimeout <= 0 || c.SettleTimeout > 5*time.Minute {
		return errors.New("workload, request, client, or timeout limit is invalid")
	}
	if (c.Preset == "stress" || c.Preset == "sustained") && !c.StressOptIn {
		return errors.New("stress and sustained presets require explicit opt-in")
	}
	if (c.Requests > 1000 || c.Clients > 16 || c.Duration > 2*time.Minute || c.Warmup > 0 || c.Trials > 1) && !c.StressOptIn {
		return errors.New("large benchmarks require -stress-opt-in")
	}
	if c.OutputFormat != "json" && c.OutputFormat != "csv" && c.OutputFormat != "markdown" && c.OutputFormat != "all" {
		return errors.New("output format must be json, csv, markdown, or all")
	}
	if c.RecoveryScenario != "" && c.RecoveryScenario != "api-restart" && c.RecoveryScenario != "worker-restart" && c.RecoveryScenario != "postgres-interruption" && c.RecoveryScenario != "temporal-unavailable" {
		return errors.New("unsupported recovery scenario label")
	}
	if host, port, err := net.SplitHostPort(c.TemporalAddress); err != nil || !loopback(host) || port == "" || c.TemporalNS == "" {
		return errors.New("Temporal measurement target must be a loopback host:port and namespace")
	}
	u, err := url.Parse(c.API)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || !loopback(u.Hostname()) || u.Port() == "" {
		return errors.New("API must be an explicit loopback HTTP origin")
	}
	if c.DatabaseURL == "" {
		return errors.New("PAYFLOW_LOADTEST_DATABASE_URL is required")
	}
	databaseURL, err := url.Parse(c.DatabaseURL)
	if err != nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") || !loopback(databaseURL.Hostname()) || databaseURL.Port() == "" {
		return errors.New("database URL must be an explicit loopback PostgreSQL URI")
	}
	poolConfig, err := pgxpool.ParseConfig(c.DatabaseURL)
	if err != nil || !loopback(poolConfig.ConnConfig.Host) || !strings.HasPrefix(poolConfig.ConnConfig.Database, "payflow_loadtest_") {
		return errors.New("a dedicated loopback payflow_loadtest_ database is required")
	}
	if poolConfig.ConnConfig.Port == 5432 {
		return errors.New("ordinary development PostgreSQL port is not permitted")
	}
	return nil
}

func DatabaseName(raw string) (string, error) {
	parsed, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return "", fmt.Errorf("invalid dedicated database URL")
	}
	return parsed.ConnConfig.Database, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
