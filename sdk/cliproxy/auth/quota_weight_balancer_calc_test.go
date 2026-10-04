package auth

import (
	"testing"
)

func TestCalculateStrategyThreeWeight(t *testing.T) {
	// Case 1: Max weekly account gets 10
	w := CalculateStrategyThreeWeight(0.913, 0.913, 0.984, true)
	if w != 10 {
		t.Fatalf("expected 10, got %d", w)
	}

	// Case 2: 90.1% vs 91.3% -> (0.901/0.913)^2 * 10 = 9.74 -> 10
	w2 := CalculateStrategyThreeWeight(0.901, 0.913, 0.866, true)
	if w2 != 10 {
		t.Fatalf("expected 10, got %d", w2)
	}

	// Case 3: 80.4% vs 91.3% -> (0.804/0.913)^2 * 10 = 7.75 -> 8
	w3 := CalculateStrategyThreeWeight(0.804, 0.913, 0.918, true)
	if w3 != 8 {
		t.Fatalf("expected 8, got %d", w3)
	}

	// Case 4: 65.1% vs 91.3% -> (0.651/0.913)^2 * 10 = 5.08 -> 5
	w4 := CalculateStrategyThreeWeight(0.651, 0.913, 0.895, true)
	if w4 != 5 {
		t.Fatalf("expected 5, got %d", w4)
	}

	// Case 5: 19.5% vs 91.3% -> (0.195/0.913)^2 * 10 = 0.45 -> min 1
	w5 := CalculateStrategyThreeWeight(0.195, 0.913, 0.988, true)
	if w5 != 1 {
		t.Fatalf("expected 1, got %d", w5)
	}

	// Case 6: Safety brake triggered (5-hour < 20%)
	// Weekly is 90% (would be 10), but 5h is 15% -> forced to 1
	wBrake := CalculateStrategyThreeWeight(0.90, 0.913, 0.15, true)
	if wBrake != 1 {
		t.Fatalf("expected 1 due to safety brake, got %d", wBrake)
	}
}

func TestCalculatePoolWeights(t *testing.T) {
	quotas := map[string]AntigravityQuotaFraction{
		"kk": {
			WeeklyRemaining:   0.9128,
			FiveHourRemaining: 0.984,
			HasWeekly:         true,
			HasFiveHour:       true,
		},
		"k": {
			WeeklyRemaining:   0.9013,
			FiveHourRemaining: 0.865,
			HasWeekly:         true,
			HasFiveHour:       true,
		},
		"kkk": {
			WeeklyRemaining:   0.8042,
			FiveHourRemaining: 0.917,
			HasWeekly:         true,
			HasFiveHour:       true,
		},
		"kkkk": {
			WeeklyRemaining:   0.6505,
			FiveHourRemaining: 0.895,
			HasWeekly:         true,
			HasFiveHour:       true,
		},
	}

	weights := CalculatePoolWeights(quotas)
	if weights["kk"] != 10 {
		t.Errorf("kk: want 10, got %d", weights["kk"])
	}
	if weights["k"] != 10 {
		t.Errorf("k: want 10, got %d", weights["k"])
	}
	if weights["kkk"] != 8 {
		t.Errorf("kkk: want 8, got %d", weights["kkk"])
	}
	if weights["kkkk"] != 5 {
		t.Errorf("kkkk: want 5, got %d", weights["kkkk"])
	}
}
