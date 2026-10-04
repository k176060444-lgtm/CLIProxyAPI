package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	// MinBalancerInterval is 90 minutes.
	MinBalancerInterval = 90 * time.Minute

	// MaxBalancerInterval is 180 minutes.
	MaxBalancerInterval = 180 * time.Minute

	// InitialBalancerDelay avoids startup burst.
	InitialBalancerDelay = 15 * time.Second

	// QuotaProbeTimeout per credential probe.
	QuotaProbeTimeout = 20 * time.Second

	// AntigravityQuotaEndpoint is the internal API endpoint for retrieving quota summary.
	AntigravityQuotaEndpoint = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"

	// AntigravityOAuthTokenEndpoint is Google's OAuth2 token endpoint.
	AntigravityOAuthTokenEndpoint = "https://oauth2.googleapis.com/token"

	// Known client credentials for antigravity hub OAuth.
	antigravityClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityClientSecret = "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"

	// Night window configuration (local time)
	nightStartHour = 23 // 23:00 starts quiet mode
	nightEndHour   = 8  // 08:00 ends quiet mode
)

type userQuotaSummaryResponse struct {
	Groups []struct {
		DisplayName string `json:"displayName"`
		Buckets     []struct {
			RemainingFraction float64 `json:"remainingFraction"`
			ResetTime         string  `json:"resetTime"`
			Window            string  `json:"window"` // "weekly" or "5h"
		} `json:"buckets"`
	} `json:"groups"`
}

// QuotaWeightBalancer manages periodic background balancing of Antigravity account weights.
type QuotaWeightBalancer struct {
	manager    *Manager
	running    atomic.Bool
	cancelFunc context.CancelFunc
	mu         sync.Mutex
}

// NewQuotaWeightBalancer constructs a new balancer instance.
func NewQuotaWeightBalancer(manager *Manager) *QuotaWeightBalancer {
	return &QuotaWeightBalancer{
		manager: manager,
	}
}

// Start begins the background balancer loop.
func (b *QuotaWeightBalancer) Start(ctx context.Context) {
	if b == nil || b.manager == nil {
		return
	}

	b.mu.Lock()
	if b.running.Load() {
		b.mu.Unlock()
		return
	}

	loopCtx, cancel := context.WithCancel(ctx)
	b.cancelFunc = cancel
	b.running.Store(true)
	b.mu.Unlock()

	go b.run(loopCtx)
}

// Stop gracefully shuts down the background balancer.
func (b *QuotaWeightBalancer) Stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancelFunc != nil {
		b.cancelFunc()
		b.cancelFunc = nil
	}
	b.running.Store(false)
}

// isNightWindow reports whether the given local time falls into the quiet window [23:00, 08:00).
func isNightWindow(t time.Time) bool {
	h := t.Hour()
	return h >= nightStartHour || h < nightEndHour
}

// nextMorningWakeTime returns a random wake-up time between 08:00 and 08:30 for the next morning.
func nextMorningWakeTime(now time.Time) time.Time {
	// Random offset between 0 and 30 minutes (0 to 1800 seconds)
	jitterSeconds := rand.Int64N(30 * 60)
	jitterDuration := time.Duration(jitterSeconds) * time.Second

	year, month, day := now.Date()
	if now.Hour() >= nightStartHour {
		// It's late evening (>=23:00), morning is tomorrow
		tomorrow := now.AddDate(0, 0, 1)
		year, month, day = tomorrow.Date()
	}
	// Target 08:00:00 on the calculated day + jitter
	target := time.Date(year, month, day, nightEndHour, 0, 0, 0, now.Location())
	return target.Add(jitterDuration)
}

// computeNextDelay calculates how long to sleep before the next probe round.
// If currently in or about to cross into the night window, returns the duration until morning (08:00~08:30).
// Otherwise, returns a random duration between 90 and 180 minutes.
func computeNextDelay(now time.Time) (time.Duration, bool) {
	if isNightWindow(now) {
		wakeTime := nextMorningWakeTime(now)
		delay := wakeTime.Sub(now)
		if delay < time.Minute {
			delay = time.Minute
		}
		return delay, true // true = sleeping through night
	}

	// Daytime: pick uniform random interval between 90m and 180m (90m + [0, 90m])
	jitterMinutes := rand.Int64N(90) // 0 to 89 minutes
	jitterSeconds := rand.Int64N(60) // 0 to 59 seconds
	interval := MinBalancerInterval + time.Duration(jitterMinutes)*time.Minute + time.Duration(jitterSeconds)*time.Second

	plannedNext := now.Add(interval)
	// Check if planned next probe falls into the night window (>= 23:00)
	if isNightWindow(plannedNext) {
		wakeTime := nextMorningWakeTime(plannedNext)
		delay := wakeTime.Sub(now)
		if delay < time.Minute {
			delay = time.Minute
		}
		return delay, true
	}

	return interval, false
}

func (b *QuotaWeightBalancer) run(ctx context.Context) {
	defer b.running.Store(false)

	now := time.Now()
	log.Infof("antigravity quota weight balancer started (humanized: 90-180m jitter, quiet night 23:00-08:00+random 0-30m)")

	// Initial startup action
	if isNightWindow(now) {
		delay, _ := computeNextDelay(now)
		log.Infof("quota weight balancer: current time %s is in night quiet window; sleeping %s until morning", now.Format("15:04:05"), delay.Round(time.Minute))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			b.BalanceOnce(ctx)
		}
	} else {
		select {
		case <-ctx.Done():
			return
		case <-time.After(InitialBalancerDelay):
			b.BalanceOnce(ctx)
		}
	}

	for {
		nextDelay, isNight := computeNextDelay(time.Now())
		if isNight {
			log.Infof("quota weight balancer: entering night quiet mode, sleeping %s until morning", nextDelay.Round(time.Minute))
		} else {
			log.Infof("quota weight balancer: next probe scheduled in %s", nextDelay.Round(time.Second))
		}

		select {
		case <-ctx.Done():
			log.Info("antigravity quota weight balancer stopped")
			return
		case <-time.After(nextDelay):
			b.BalanceOnce(ctx)
		}
	}
}

// BalanceOnce performs a single round of probing and weight adjustments.
// Completely isolated: does not block request pathways.
func (b *QuotaWeightBalancer) BalanceOnce(ctx context.Context) {
	if b == nil || b.manager == nil {
		return
	}

	// 1. Gather all active antigravity credentials
	b.manager.mu.RLock()
	candidates := make([]*Auth, 0, len(b.manager.auths))
	for _, auth := range b.manager.auths {
		if auth != nil && !auth.Disabled && strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			candidates = append(candidates, auth.Clone())
		}
	}
	b.manager.mu.RUnlock()

	if len(candidates) <= 1 {
		// Nothing to balance if <= 1 account
		return
	}

	log.Debugf("quota weight balancer: probing %d antigravity accounts with stagger", len(candidates))

	// 2. Probe quota for each account with 1~3s stagger between accounts
	quotas := make(map[string]AntigravityQuotaFraction)
	for i, auth := range candidates {
		if i > 0 {
			// Humanized stagger delay between accounts (1000ms to 2500ms)
			stagger := time.Duration(1000+rand.Int64N(1500)) * time.Millisecond
			select {
			case <-ctx.Done():
				return
			case <-time.After(stagger):
			}
		}

		q, err := b.probeAccountQuota(ctx, auth)
		if err != nil {
			log.Warnf("quota weight balancer: probe failed for account %s: %v (preserving existing weight)", auth.ID, err)
			continue
		}
		quotas[auth.ID] = q
	}

	if len(quotas) == 0 {
		return
	}

	// 3. Calculate weights using Strategy 3
	newWeights := CalculatePoolWeights(quotas)

	// 4. Update weights for accounts that changed
	for authID, weight := range newWeights {
		b.applyWeightIfChanged(ctx, authID, weight)
	}
}

func (b *QuotaWeightBalancer) applyWeightIfChanged(ctx context.Context, authID string, newWeight int64) {
	b.manager.mu.RLock()
	currentAuth, ok := b.manager.auths[authID]
	if !ok || currentAuth == nil {
		b.manager.mu.RUnlock()
		return
	}
	oldWeight := authWeight(currentAuth)
	b.manager.mu.RUnlock()

	if oldWeight == newWeight {
		return
	}

	// Prepare updated auth clone
	clone := currentAuth.Clone()
	if clone.Metadata == nil {
		clone.Metadata = make(map[string]any)
	}
	clone.Metadata[AttributeWeight] = newWeight

	// Call manager.Update to apply in-memory atomically and persist safely
	_, err := b.manager.Update(ctx, clone)
	if err != nil {
		log.Errorf("quota weight balancer: failed to update weight for %s to %d: %v", authID, newWeight, err)
	} else {
		log.Infof("quota weight balancer: adjusted weight for %s: %d -> %d", authID, oldWeight, newWeight)
	}
}

func (b *QuotaWeightBalancer) probeAccountQuota(ctx context.Context, auth *Auth) (AntigravityQuotaFraction, error) {
	return b.probeWithEndpoint(ctx, auth, AntigravityQuotaEndpoint)
}

func (b *QuotaWeightBalancer) probeWithEndpoint(ctx context.Context, auth *Auth, endpoint string) (AntigravityQuotaFraction, error) {
	var zero AntigravityQuotaFraction
	if auth == nil {
		return zero, fmt.Errorf("nil auth")
	}

	projectID := stringValue(auth.Metadata, "project_id")
	if projectID == "" {
		projectID = auth.Attributes["project_id"]
	}
	if projectID == "" {
		return zero, fmt.Errorf("missing project_id in auth metadata/attributes")
	}

	probeCtx, cancel := context.WithTimeout(ctx, QuotaProbeTimeout)
	defer cancel()

	// Ensure token is valid or refreshed
	accessToken, errToken := b.ensureAccessToken(probeCtx, auth)
	if errToken != nil {
		return zero, fmt.Errorf("resolve token: %w", errToken)
	}

	reqBody, _ := json.Marshal(map[string]string{"project": projectID})
	req, errReq := http.NewRequestWithContext(probeCtx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if errReq != nil {
		return zero, errReq
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "antigravity/hub/1.0.13")

	client := &http.Client{
		Timeout: QuotaProbeTimeout,
	}
	if auth.ProxyURL != "" {
		if transport := buildProxyTransport(auth.ProxyURL); transport != nil {
			client.Transport = transport
		}
	}

	resp, errDo := client.Do(req)
	if errDo != nil {
		return zero, errDo
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return zero, fmt.Errorf("upstream status %d: %s", resp.StatusCode, string(respBody))
	}

	var summary userQuotaSummaryResponse
	if errDecode := json.NewDecoder(resp.Body).Decode(&summary); errDecode != nil {
		return zero, fmt.Errorf("decode quota response: %w", errDecode)
	}

	res := AntigravityQuotaFraction{}
	for _, g := range summary.Groups {
		// Only inspect Gemini Models group for Gemini quotas
		if !strings.Contains(strings.ToLower(g.DisplayName), "gemini") {
			continue
		}
		for _, b := range g.Buckets {
			w := strings.ToLower(strings.TrimSpace(b.Window))
			if w == "weekly" {
				res.WeeklyRemaining = b.RemainingFraction
				res.HasWeekly = true
			} else if w == "5h" || strings.Contains(w, "5") {
				res.FiveHourRemaining = b.RemainingFraction
				res.HasFiveHour = true
			}
		}
	}

	return res, nil
}

func (b *QuotaWeightBalancer) ensureAccessToken(ctx context.Context, auth *Auth) (string, error) {
	if auth == nil || auth.Metadata == nil {
		return "", fmt.Errorf("empty auth metadata")
	}

	current := strings.TrimSpace(tokenValueFromMetadata(auth.Metadata))
	if current != "" && !antigravityTokenNeedsRefresh(auth.Metadata) {
		return current, nil
	}

	refreshToken := stringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		if current != "" {
			return current, nil
		}
		return "", fmt.Errorf("missing refresh token")
	}

	form := url.Values{}
	form.Set("client_id", antigravityClientID)
	form.Set("client_secret", antigravityClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, AntigravityOAuthTokenEndpoint, strings.NewReader(form.Encode()))
	if errReq != nil {
		return "", errReq
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	if auth.ProxyURL != "" {
		if transport := buildProxyTransport(auth.ProxyURL); transport != nil {
			client.Transport = transport
		}
	}

	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("refresh status %d: %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if errDecode := json.NewDecoder(resp.Body).Decode(&tr); errDecode != nil {
		return "", errDecode
	}

	// Update in-memory metadata
	auth.Metadata["access_token"] = tr.AccessToken
	if tr.ExpiresIn > 0 {
		auth.Metadata["expired"] = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Format(time.RFC3339)
	}

	return tr.AccessToken, nil
}

func antigravityTokenNeedsRefresh(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return true
	}
	expiredStr := stringValue(metadata, "expired")
	if expiredStr == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339, expiredStr)
	if err != nil {
		return false
	}
	// Refresh if within 5 minutes of expiration
	return time.Now().Add(5 * time.Minute).After(parsed)
}

func tokenValueFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if v, ok := metadata["access_token"].(string); ok {
		return v
	}
	if v, ok := metadata["token"].(string); ok {
		return v
	}
	return ""
}

func stringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func buildProxyTransport(proxyStr string) http.RoundTripper {
	transport, _, err := proxyutil.BuildHTTPTransport(proxyStr)
	if err != nil {
		return nil
	}
	return transport
}
