package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Database reads the same settings for API, worker, and the one-off migrator.
// Production requires hostname-verified TLS and an explicitly supplied URL.
func Database() (*pgxpool.Config, error) {
	raw := os.Getenv("DATABASE_URL")
	if raw == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	if os.Getenv("PAYFLOW_ENV") == "production" {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
			return nil, errors.New("production DATABASE_URL must be a PostgreSQL URI")
		}
		sslModes := parsed.Query()["sslmode"]
		rootCerts := parsed.Query()["sslrootcert"]
		if len(sslModes) != 1 || sslModes[0] != "verify-full" || len(rootCerts) != 1 || rootCerts[0] == "" {
			return nil, errors.New("production DATABASE_URL requires sslmode=verify-full and sslrootcert")
		}
		if cfg.ConnConfig.Host == "" || parsed.Hostname() != cfg.ConnConfig.Host || !strings.Contains(cfg.ConnConfig.Host, ".") || net.ParseIP(cfg.ConnConfig.Host) != nil || strings.EqualFold(cfg.ConnConfig.Host, "localhost") {
			return nil, errors.New("production DATABASE_URL requires a DNS hostname")
		}
		if cfg.ConnConfig.TLSConfig == nil || cfg.ConnConfig.TLSConfig.InsecureSkipVerify {
			return nil, errors.New("production database TLS verification is unavailable")
		}
		if _, err := os.Stat(rootCerts[0]); err != nil {
			return nil, errors.New("database CA certificate is unavailable")
		}
	} else if mode := os.Getenv("PAYFLOW_ENV"); mode != "" && mode != "development" {
		return nil, fmt.Errorf("PAYFLOW_ENV must be development or production")
	}
	maxConns := int64(10)
	if v := os.Getenv("PAYFLOW_DB_MAX_CONNS"); v != "" {
		maxConns, err = strconv.ParseInt(v, 10, 32)
		if err != nil || maxConns < 1 || maxConns > 100 {
			return nil, errors.New("PAYFLOW_DB_MAX_CONNS must be 1..100")
		}
	}
	cfg.MaxConns = int32(maxConns)
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	if v := os.Getenv("PAYFLOW_DB_CONNECT_TIMEOUT"); v != "" {
		timeout, err := time.ParseDuration(v)
		if err != nil || timeout <= 0 || timeout > time.Minute {
			return nil, errors.New("PAYFLOW_DB_CONNECT_TIMEOUT must be positive and at most one minute")
		}
		cfg.ConnConfig.ConnectTimeout = timeout
	} else {
		cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	return cfg, nil
}
