package pricing

import (
	"testing"

	"github.com/personal/broxy/internal/domain"
)

func TestEstimateCost(t *testing.T) {
	entry := &domain.PricingEntry{
		InputPerMTokens:  3.0,
		OutputPerMTokens: 15.0,
	}
	usage := domain.TokenUsage{Input: 1000, Output: 2000}
	got := EstimateCost(entry, usage)
	want := 0.033
	if got != want {
		t.Fatalf("EstimateCost() = %f, want %f", got, want)
	}
}
