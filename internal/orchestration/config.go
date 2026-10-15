package orchestration

import (
	"fmt"
	"os"
	"time"
)

const DefaultTaskQueue = "payflow-payments"

type Config struct {
	Address              string
	Namespace            string
	TaskQueue            string
	ReconcileInterval    time.Duration
	ReconcileMaxInterval time.Duration
	ReconcileWindow      time.Duration
}

func ConfigFromEnv() (Config, error) {
	c := Config{
		Address:   valueOr("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		Namespace: valueOr("TEMPORAL_NAMESPACE", "default"),
		TaskQueue: valueOr("TEMPORAL_TASK_QUEUE", DefaultTaskQueue),
	}
	var err error
	if c.ReconcileInterval, err = durationOr("PAYFLOW_RECONCILE_INTERVAL", 2*time.Second); err != nil {
		return Config{}, err
	}
	if c.ReconcileMaxInterval, err = durationOr("PAYFLOW_RECONCILE_MAX_INTERVAL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if c.ReconcileWindow, err = durationOr("PAYFLOW_RECONCILE_WINDOW", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if c.ReconcileMaxInterval < c.ReconcileInterval {
		return Config{}, fmt.Errorf("PAYFLOW_RECONCILE_MAX_INTERVAL must be >= PAYFLOW_RECONCILE_INTERVAL")
	}
	if c.ReconcileWindow < c.ReconcileInterval {
		return Config{}, fmt.Errorf("PAYFLOW_RECONCILE_WINDOW must be >= PAYFLOW_RECONCILE_INTERVAL")
	}
	return c, nil
}

func valueOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", key)
	}
	return d, nil
}
