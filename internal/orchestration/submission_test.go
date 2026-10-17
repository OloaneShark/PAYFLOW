package orchestration

import (
	"testing"

	"payflow/internal/banking"
)

func TestOutcomePolicyIsStableAndBounded(t *testing.T) {
	for _, id := range []string{
		"123e4567-e89b-42d3-a456-426614174000",
		"123e4567-e89b-42d3-a456-426614174001",
		"123e4567-e89b-42d3-a456-426614174002",
	} {
		first := OutcomePolicy(id)
		if first != OutcomePolicy(id) || (first != banking.Approve && first != banking.Decline) {
			t.Fatalf("unstable or unsupported outcome for %s: %s", id, first)
		}
	}
}
