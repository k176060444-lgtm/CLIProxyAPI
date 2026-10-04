package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQuotaWeightBalancer_ApplyWeightIfChanged(t *testing.T) {
	manager := NewManager(nil, nil, nil)

	auth1 := &Auth{
		ID:       "antigravity-1",
		Provider: "antigravity",
		Metadata: map[string]any{
			AttributeWeight: int64(1),
		},
	}
	auth2 := &Auth{
		ID:       "antigravity-2",
		Provider: "antigravity",
		Metadata: map[string]any{
			AttributeWeight: int64(10),
		},
	}

	_, _ = manager.Register(context.Background(), auth1)
	_, _ = manager.Register(context.Background(), auth2)

	balancer := NewQuotaWeightBalancer(manager)

	// Change auth1 to 8
	balancer.applyWeightIfChanged(context.Background(), "antigravity-1", 8)

	// Verify auth1 updated
	gotAuth1, ok1 := manager.GetByID("antigravity-1")
	if !ok1 || gotAuth1 == nil {
		t.Fatal("expected auth1 to exist")
	}
	if w := authWeight(gotAuth1); w != 8 {
		t.Fatalf("expected auth1 weight to be 8, got %d", w)
	}

	// Verify auth2 untouched
	gotAuth2, ok2 := manager.GetByID("antigravity-2")
	if !ok2 || gotAuth2 == nil {
		t.Fatal("expected auth2 to exist")
	}
	if w := authWeight(gotAuth2); w != 10 {
		t.Fatalf("expected auth2 weight to be 10, got %d", w)
	}

	// Re-applying same weight is a no-op
	balancer.applyWeightIfChanged(context.Background(), "antigravity-1", 8)
	if w := authWeight(gotAuth1); w != 8 {
		t.Fatalf("expected auth1 weight to still be 8, got %d", w)
	}
}

func TestQuotaWeightBalancer_ProbeAccountQuotaMock(t *testing.T) {
	// Mock upstream Google API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mock-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		resp := userQuotaSummaryResponse{
			Groups: []struct {
				DisplayName string `json:"displayName"`
				Buckets     []struct {
					RemainingFraction float64 `json:"remainingFraction"`
					ResetTime         string  `json:"resetTime"`
					Window            string  `json:"window"`
				} `json:"buckets"`
			}{
				{
					DisplayName: "Gemini Models",
					Buckets: []struct {
						RemainingFraction float64 `json:"remainingFraction"`
						ResetTime         string  `json:"resetTime"`
						Window            string  `json:"window"`
					}{
						{RemainingFraction: 0.85, Window: "weekly"},
						{RemainingFraction: 0.95, Window: "5h"},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	manager := NewManager(nil, nil, nil)
	balancer := NewQuotaWeightBalancer(manager)

	auth := &Auth{
		ID:       "mock-account",
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "mock-token",
			"project_id":   "mock-proj",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	q, err := balancer.probeWithEndpoint(context.Background(), auth, server.URL)
	if err != nil {
		t.Fatalf("unexpected probe error: %v", err)
	}
	if !q.HasWeekly || q.WeeklyRemaining != 0.85 {
		t.Fatalf("expected weekly 0.85, got %v", q.WeeklyRemaining)
	}
	if !q.HasFiveHour || q.FiveHourRemaining != 0.95 {
		t.Fatalf("expected 5h 0.95, got %v", q.FiveHourRemaining)
	}
}

func TestQuotaWeightBalancer_NightWindowAndJitter(t *testing.T) {
	loc := time.Local

	// 1. Test isNightWindow
	t23 := time.Date(2026, 10, 4, 23, 0, 0, 0, loc)
	t03 := time.Date(2026, 10, 4, 3, 30, 0, 0, loc)
	t0759 := time.Date(2026, 10, 4, 7, 59, 59, 0, loc)
	t0800 := time.Date(2026, 10, 4, 8, 0, 0, 0, loc)
	t1400 := time.Date(2026, 10, 4, 14, 0, 0, 0, loc)

	if !isNightWindow(t23) {
		t.Errorf("expected 23:00 to be night window")
	}
	if !isNightWindow(t03) {
		t.Errorf("expected 03:30 to be night window")
	}
	if !isNightWindow(t0759) {
		t.Errorf("expected 07:59:59 to be night window")
	}
	if isNightWindow(t0800) {
		t.Errorf("expected 08:00:00 to NOT be night window")
	}
	if isNightWindow(t1400) {
		t.Errorf("expected 14:00 to NOT be night window")
	}

	// 2. Test nextMorningWakeTime range [08:00, 08:30]
	for i := 0; i < 50; i++ {
		wake := nextMorningWakeTime(t23)
		if wake.Hour() != 8 || wake.Minute() > 30 {
			t.Fatalf("invalid wake time from 23:00: %v", wake)
		}

		wake2 := nextMorningWakeTime(t03)
		if wake2.Hour() != 8 || wake2.Minute() > 30 {
			t.Fatalf("invalid wake time from 03:00: %v", wake2)
		}
	}

	// 3. Test daytime computeNextDelay range [90m, 180m]
	for i := 0; i < 50; i++ {
		t10 := time.Date(2026, 10, 4, 10, 0, 0, 0, loc)
		delay, isNight := computeNextDelay(t10)
		if isNight {
			t.Fatalf("did not expect night at 10:00, got %v", delay)
		}
		if delay < 90*time.Minute || delay > 180*time.Minute {
			t.Fatalf("delay out of [90m, 180m] bounds: %v", delay)
		}
	}
}
