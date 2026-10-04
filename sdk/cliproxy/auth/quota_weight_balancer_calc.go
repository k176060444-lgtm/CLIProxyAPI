package auth

import (
	"math"
)

// AntigravityQuotaFraction holds parsed quota availability for one account.
type AntigravityQuotaFraction struct {
	WeeklyRemaining float64 // 0.0 to 1.0 (e.g. 0.913)
	FiveHourRemaining float64 // 0.0 to 1.0 (e.g. 0.984)
	HasWeekly       bool
	HasFiveHour     bool
}

// CalculateStrategyThreeWeight implements Strategy 3 (Exponential Penalty / Squared Ratio):
//
// 1. Ratio_i = Weekly_i / max(Weekly)
// 2. BaseWeight = clamp(round(10 * Ratio_i^2), 1, 10)
// 3. Safety Brake: If FiveHour < 0.20, force FinalWeight = 1
func CalculateStrategyThreeWeight(weekly, maxWeekly, fiveHour float64, hasFiveHour bool) int64 {
	if maxWeekly <= 0.001 {
		return 1
	}
	if weekly <= 0 {
		return 1
	}

	ratio := weekly / maxWeekly
	if ratio > 1.0 {
		ratio = 1.0
	}

	// Squared ratio: (weekly / maxWeekly)^2 * 10
	val := math.Round(10.0 * ratio * ratio)
	baseWeight := int64(val)
	if baseWeight < 1 {
		baseWeight = 1
	}
	if baseWeight > 10 {
		baseWeight = 10
	}

	// Safety brake: if 5-hour rolling quota is less than 20%, force to 1
	if hasFiveHour && fiveHour < 0.20 {
		return 1
	}

	return baseWeight
}

// CalculatePoolWeights calculates the new weights for a set of accounts using Strategy 3.
func CalculatePoolWeights(quotas map[string]AntigravityQuotaFraction) map[string]int64 {
	weights := make(map[string]int64, len(quotas))
	if len(quotas) == 0 {
		return weights
	}

	// Find max weekly quota
	var maxWeekly float64
	for _, q := range quotas {
		if q.HasWeekly && q.WeeklyRemaining > maxWeekly {
			maxWeekly = q.WeeklyRemaining
		}
	}

	// If no valid weekly quota found, fallback to 1 for all
	if maxWeekly <= 0.001 {
		for id := range quotas {
			weights[id] = 1
		}
		return weights
	}

	for id, q := range quotas {
		if !q.HasWeekly {
			weights[id] = 1
			continue
		}
		weights[id] = CalculateStrategyThreeWeight(q.WeeklyRemaining, maxWeekly, q.FiveHourRemaining, q.HasFiveHour)
	}

	return weights
}
