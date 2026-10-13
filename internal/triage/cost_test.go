package triage

import (
	"math"
	"testing"
)

func TestCostUSD(t *testing.T) {
	got, err := CostUSD(Usage{Model: HaikuModel, InputTokens: 1_000_000, OutputTokens: 200_000})
	if err != nil || math.Abs(got-2.0) > 1e-9 {
		t.Fatalf("CostUSD = %v, %v; want 2.00", got, err)
	}
	got, _ = CostUSD(Usage{Model: HaikuModel, InputTokens: 1000, OutputTokens: 100})
	if math.Abs(got-0.0015) > 1e-12 {
		t.Fatalf("small call = %v, want 0.0015", got)
	}
	if _, err := CostUSD(Usage{Model: "claude-unknown"}); err == nil {
		t.Fatal("unknown model priced without error")
	}
}
