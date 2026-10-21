package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDispatchDatabaseErrorRetainsCauseWithoutExposingIt(t *testing.T) {
	const secret = "postgres://private-user:private-password@internal.example/db"
	cause := errors.Join(context.DeadlineExceeded, errors.New(secret))
	err := dispatchDBError("list pending workflow dispatches", cause)
	if !errors.Is(err, ErrDispatchDatabase) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("database marker or underlying timeout lost: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("database wrapper exposed the underlying error text")
	}
}
