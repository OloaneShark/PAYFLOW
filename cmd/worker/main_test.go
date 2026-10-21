package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDispatchScannerWarningReportsSafeCategoryAndCount(t *testing.T) {
	const secret = "TEMPORAL_API_KEY=private-value"
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	logDispatchScanFailure(logger, errors.Join(status.Error(codes.Unavailable, secret), errors.New(secret)), 17)
	got := output.String()
	if strings.Contains(got, secret) || !strings.Contains(got, `"error_category":"mixed"`) ||
		!strings.Contains(got, `"dispatch_stage":"scan"`) || !strings.Contains(got, `"scanned_requests":17`) {
		t.Fatalf("unsafe or incomplete scanner log: %s", got)
	}
}
