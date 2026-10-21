package orchestration

import (
	"context"
	"errors"
	"net"
	"syscall"

	"go.temporal.io/api/serviceerror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"payflow/internal/storage"
)

// dispatchStartError carries only a reviewed category across scanner calls.
// The original Temporal error can contain endpoint or credential details.
type dispatchStartError struct{ category string }

func (e *dispatchStartError) Error() string { return "workflow dispatch failed" }
func (e *dispatchStartError) Unwrap() error { return ErrTemporalUnavailable }

// DispatchErrorCategory uses error types and gRPC status codes, never text.
// A batch with different failure categories is reported as mixed; individual
// payment warnings retain their own category.
func DispatchErrorCategory(err error) string {
	if err == nil {
		return "none"
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		category := "none"
		for _, part := range joined.Unwrap() {
			current := DispatchErrorCategory(part)
			if current == "none" {
				continue
			}
			if category != "none" && category != current {
				return "mixed"
			}
			category = current
		}
		return category
	}
	var start *dispatchStartError
	if errors.As(err, &start) {
		return start.category
	}
	if errors.Is(err, storage.ErrDispatchDatabase) {
		return "database"
	}
	var temporalDeadline *serviceerror.DeadlineExceeded
	if errors.As(err, &temporalDeadline) {
		return "timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var temporalUnavailable *serviceerror.Unavailable
	var namespaceUnavailable *serviceerror.NamespaceUnavailable
	if errors.As(err, &temporalUnavailable) || errors.As(err, &namespaceUnavailable) ||
		status.Code(err) == codes.Unavailable || errors.Is(err, syscall.ECONNREFUSED) {
		return "temporal_unavailable"
	}
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return "canceled"
	}
	return "unexpected"
}
