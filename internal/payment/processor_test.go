package payment

import (
	"testing"
	"time"

	paymentv1 "payflow/gen/go/payflow/v1"
)

func TestValidTransitions(t *testing.T) {
	valid := map[string][]string{
		StatusCreated:                {StatusProcessing},
		StatusProcessing:             {StatusCompleted, StatusFailed, StatusReconciliationRequired},
		StatusReconciliationRequired: {StatusProcessing},
	}
	statuses := []string{StatusCreated, StatusProcessing, StatusCompleted, StatusFailed, StatusReconciliationRequired}
	for _, from := range statuses {
		for _, to := range statuses {
			want := false
			for _, allowed := range valid[from] {
				if to == allowed {
					want = true
				}
			}
			if got := ValidTransition(from, to); got != want {
				t.Errorf("ValidTransition(%q, %q) = %t; want %t", from, to, got, want)
			}
		}
	}
}

func TestProtoStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   paymentv1.PaymentStatus
	}{
		{StatusCreated, paymentv1.PaymentStatus_PAYMENT_STATUS_CREATED},
		{StatusProcessing, paymentv1.PaymentStatus_PAYMENT_STATUS_PROCESSING},
		{StatusCompleted, paymentv1.PaymentStatus_PAYMENT_STATUS_COMPLETED},
		{StatusFailed, paymentv1.PaymentStatus_PAYMENT_STATUS_FAILED},
		{StatusReconciliationRequired, paymentv1.PaymentStatus_PAYMENT_STATUS_RECONCILIATION_REQUIRED},
	} {
		t.Run(tc.status, func(t *testing.T) {
			started := time.Now().UTC()
			got := toProto(Payment{Status: tc.status, ProcessingStartedAt: &started, ProviderOperationID: "operation"})
			if got.GetStatus() != tc.want || got.GetProcessingStartedAt() == nil || got.GetProviderOperationId() != "operation" {
				t.Fatalf("toProto(%q) = %+v", tc.status, got)
			}
		})
	}
}
