package orchestration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"

	"go.temporal.io/api/serviceerror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"payflow/internal/banking"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

func TestDispatchErrorCategory(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "none"},
		{"Temporal unavailable", fmt.Errorf("dial: %w", status.Error(codes.Unavailable, "private endpoint")), "temporal_unavailable"},
		{"Temporal service unavailable", fmt.Errorf("dial: %w", serviceerror.NewUnavailable("private endpoint")), "temporal_unavailable"},
		{"Temporal namespace unavailable", serviceerror.NewNamespaceUnavailable("private-namespace"), "temporal_unavailable"},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), "temporal_unavailable"},
		{"gRPC deadline", fmt.Errorf("start: %w", status.Error(codes.DeadlineExceeded, "private endpoint")), "timeout"},
		{"Temporal service deadline", fmt.Errorf("start: %w", serviceerror.NewDeadlineExceeded("private endpoint")), "timeout"},
		{"context deadline", fmt.Errorf("start: %w", context.DeadlineExceeded), "timeout"},
		{"network timeout", &net.DNSError{IsTimeout: true}, "timeout"},
		{"database failure", fmt.Errorf("query: %w", storage.ErrDispatchDatabase), "database"},
		{"canceled", context.Canceled, "canceled"},
		{"unknown text is not classified by wording", errors.New("Temporal unavailable postgres://user:secret@localhost/db"), "unexpected"},
		{"joined same category", errors.Join(status.Error(codes.Unavailable, "a"), status.Error(codes.Unavailable, "b")), "temporal_unavailable"},
		{"joined mixed categories", errors.Join(status.Error(codes.Unavailable, "a"), storage.ErrDispatchDatabase), "mixed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DispatchErrorCategory(tc.err); got != tc.want {
				t.Fatalf("category = %q, want %q", got, tc.want)
			}
		})
	}
}

type diagnosticStore struct {
	request   storage.DispatchRequest
	markErr   error
	recordErr error
	recorded  string
}

func (*diagnosticStore) Enqueue(context.Context, string, banking.Scenario, string) (storage.DispatchRequest, error) {
	return storage.DispatchRequest{}, errors.New("unused")
}
func (s *diagnosticStore) PendingDispatches(context.Context, int) ([]storage.DispatchRequest, error) {
	return []storage.DispatchRequest{s.request}, nil
}
func (s *diagnosticStore) MarkDispatched(context.Context, string) error { return s.markErr }
func (s *diagnosticStore) RecordDispatchError(_ context.Context, _ string, category string) error {
	s.recorded = category
	return s.recordErr
}
func (*diagnosticStore) Get(context.Context, string) (payment.Payment, error) {
	return payment.Payment{}, errors.New("unused")
}

type diagnosticStarter struct{ err error }

func (s diagnosticStarter) Start(context.Context, storage.DispatchRequest) error { return s.err }

func TestDispatchWarningAndStoredCategoryOmitUnderlyingError(t *testing.T) {
	const secret = "postgres://private-user:private-password@internal.example/db?sslmode=disable"
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	store := &diagnosticStore{request: storage.DispatchRequest{PaymentID: "payment-id", WorkflowID: "workflow-id"}}
	coordinator := &Coordinator{Store: store, Starter: diagnosticStarter{err: status.Error(codes.Unavailable, secret)}}
	if _, err := coordinator.DispatchPending(context.Background()); !errors.Is(err, ErrTemporalUnavailable) || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe or unrecognizable dispatch error: %v", err)
	}
	if store.recorded != "temporal_unavailable" {
		t.Fatalf("stored category = %q", store.recorded)
	}
	if got := log.String(); strings.Contains(got, secret) || !strings.Contains(got, `"error_category":"temporal_unavailable"`) ||
		!strings.Contains(got, `"dispatch_stage":"start_workflow"`) {
		t.Fatalf("unsafe or incomplete dispatch log: %s", got)
	}
}

func TestDispatchConfirmationWarningIdentifiesDatabaseStageWithoutRawError(t *testing.T) {
	const secret = "DATABASE_URL=postgres://user:password@internal.example/db"
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	store := &diagnosticStore{request: storage.DispatchRequest{PaymentID: "payment-id", WorkflowID: "workflow-id"},
		markErr: fmt.Errorf("%w: %s", storage.ErrDispatchDatabase, secret)}
	coordinator := &Coordinator{Store: store, Starter: diagnosticStarter{}}
	if _, err := coordinator.DispatchPending(context.Background()); DispatchErrorCategory(err) != "database" {
		t.Fatalf("confirmation error category = %q", DispatchErrorCategory(err))
	}
	if got := log.String(); strings.Contains(got, secret) || !strings.Contains(got, `"dispatch_stage":"mark_dispatched"`) ||
		!strings.Contains(got, `"error_category":"database"`) {
		t.Fatalf("unsafe or incomplete confirmation log: %s", got)
	}
}
