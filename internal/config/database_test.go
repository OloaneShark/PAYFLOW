package config

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseLocalAndProductionTLS(t *testing.T) {
	t.Setenv("PAYFLOW_ENV", "development")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/payflow?sslmode=disable")
	cfg, err := Database()
	if err != nil || cfg.MaxConns != 10 {
		t.Fatalf("local config: %v, max connections %d", err, cfg.MaxConns)
	}

	t.Setenv("PAYFLOW_ENV", "production")
	if _, err := Database(); err == nil {
		t.Fatal("production accepted plaintext database URL")
	}

	certificate, err := filepath.Abs(filepath.Join("..", "..", "certs", "rds-global-bundle.pem"))
	if err != nil {
		t.Fatal(err)
	}
	escapedCertificate := strings.ReplaceAll(url.QueryEscape(certificate), "+", "%20")
	t.Setenv("DATABASE_URL", "postgres://user:pass@db.example.test:5432/payflow?sslmode=verify-full&sslrootcert="+escapedCertificate)
	cfg, err = Database()
	if err != nil {
		t.Fatalf("production config: %v", err)
	}
	if cfg.ConnConfig.Host != "db.example.test" || cfg.ConnConfig.ConnectTimeout == 0 || cfg.ConnConfig.TLSConfig.ServerName != "db.example.test" {
		t.Fatalf("production config omitted hostname or timeout: %+v", cfg.ConnConfig)
	}

	t.Setenv("DATABASE_URL", "postgres://user:pass@127.0.0.1:5432/payflow?sslmode=verify-full&sslrootcert="+escapedCertificate)
	if _, err := Database(); err == nil {
		t.Fatal("production accepted an IP address instead of a verified DNS name")
	}
	t.Setenv("DATABASE_URL", "postgres://user:pass@db.example.test:5432/payflow?sslmode=verify-full&sslmode=disable&sslrootcert="+escapedCertificate)
	if _, err := Database(); err == nil {
		t.Fatal("production accepted ambiguous TLS settings")
	}
}

func TestDatabaseErrorsDoNotRevealCredentials(t *testing.T) {
	t.Setenv("PAYFLOW_ENV", "production")
	t.Setenv("DATABASE_URL", "postgres://user:supersecret@localhost:5432/payflow?sslmode=disable")
	_, err := Database()
	if err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("expected redacted validation error, got %v", err)
	}
}
