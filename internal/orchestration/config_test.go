package orchestration

import "testing"

func TestTemporalProductionRequiresExternalAuthenticatedTLS(t *testing.T) {
	t.Setenv("PAYFLOW_ENV", "production")
	t.Setenv("TEMPORAL_ADDRESS", "127.0.0.1:7233")
	t.Setenv("TEMPORAL_NAMESPACE", "default")
	t.Setenv("TEMPORAL_API_KEY", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("production accepted local unauthenticated Temporal")
	}
	t.Setenv("TEMPORAL_ADDRESS", "example.tmprl.cloud:7233")
	t.Setenv("TEMPORAL_NAMESPACE", "payflow.example")
	t.Setenv("TEMPORAL_API_KEY", "test-key")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	options := cfg.ClientOptions()
	if options.Credentials == nil || options.ConnectionOptions.TLS == nil || options.ConnectionOptions.TLS.InsecureSkipVerify {
		t.Fatal("production Temporal client lacks verified TLS and API-key credentials")
	}
}
