package operator

import (
	"testing"
	"time"

	"payflow/internal/payment"
)

func TestRecoveryEligibility(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Second)
	active := now.Add(time.Second)
	for _, tc := range []struct {
		name     string
		payment  payment.Payment
		eligible bool
	}{
		{"uncertain", payment.Payment{Status: payment.StatusReconciliationRequired}, true},
		{"expired claim", payment.Payment{Status: payment.StatusProcessing, ProcessingLeaseUntil: &expired}, true},
		{"active claim", payment.Payment{Status: payment.StatusProcessing, ProcessingLeaseUntil: &active}, false},
		{"missing lease", payment.Payment{Status: payment.StatusProcessing}, false},
		{"completed", payment.Payment{Status: payment.StatusCompleted}, false},
		{"failed", payment.Payment{Status: payment.StatusFailed}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoverable(tc.payment, now); got != tc.eligible {
				t.Fatalf("recoverable=%t, want %t", got, tc.eligible)
			}
		})
	}
}
