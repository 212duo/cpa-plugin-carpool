package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// upstreamCacheTTL bounds how often portal refreshes reach ChatGPT for one seat,
// however often buyers reload the page.
const upstreamCacheTTL = 30 * time.Second

// upstreamBaseURL is the ChatGPT quota API. It is a variable so tests can point it
// at a local server.
var upstreamBaseURL = "https://chatgpt.com/backend-api/wham"

type authFilePayload struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

type upstreamInfo struct {
	usage   map[string]any
	credits map[string]any
}

type cachedUpstream struct {
	info upstreamInfo
	at   time.Time
}

type upstreamCache struct {
	mu      sync.Mutex
	entries map[string]cachedUpstream // keyed by auth index
}

var quotaCache = &upstreamCache{entries: make(map[string]cachedUpstream)}

func (c *upstreamCache) get(authIndex string, now time.Time) (upstreamInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[authIndex]
	if !ok || now.Sub(e.at) > upstreamCacheTTL {
		return upstreamInfo{}, false
	}
	return e.info, true
}

func (c *upstreamCache) put(authIndex string, info upstreamInfo, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[authIndex] = cachedUpstream{info: info, at: now}
}

func (c *upstreamCache) drop(authIndex string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, authIndex)
}

// cachedUpstreamInfo returns the seat's quota windows and reset credits, reusing a
// result younger than upstreamCacheTTL.
func cachedUpstreamInfo(authIndex, userAgent string) (upstreamInfo, error) {
	now := time.Now()
	if info, ok := quotaCache.get(authIndex, now); ok {
		return info, nil
	}
	creds, err := loadCredentials(authIndex)
	if err != nil {
		return upstreamInfo{}, err
	}
	info := fetchUpstreamInfo(creds, userAgent)
	quotaCache.put(authIndex, info, now)
	return info, nil
}

func upstreamRequest(method, path string, body []byte, auth *authFilePayload, userAgent string) (*http.Request, error) {
	req, err := http.NewRequest(method, upstreamBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(userAgent) == "" {
		userAgent = DefaultUpstreamUserAgent
	}
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", auth.AccountID)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func fetchUpstreamInfo(auth *authFilePayload, userAgent string) upstreamInfo {
	client := &http.Client{Timeout: 8 * time.Second}
	get := func(path string) map[string]any {
		// Errors are ignored here as before; whatever JSON came back is kept.
		out, _ := getUpstreamJSON(client, path, auth, userAgent)
		if out == nil {
			out = make(map[string]any)
		}
		return out
	}
	return upstreamInfo{
		usage:   get("/usage"),
		credits: get("/rate-limit-reset-credits"),
	}
}

// fetchUpstreamUsage reads the quota windows like fetchUpstreamInfo, but reports
// failures instead of returning an empty map, so a failed query is never taken
// for a recovered quota. The error never carries the response body.
func fetchUpstreamUsage(auth *authFilePayload, userAgent string) (map[string]any, error) {
	return getUpstreamJSON(&http.Client{Timeout: 8 * time.Second}, "/usage", auth, userAgent)
}

func getUpstreamJSON(client *http.Client, path string, auth *authFilePayload, userAgent string) (map[string]any, error) {
	req, err := upstreamRequest(http.MethodGet, path, nil, auth, userAgent)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: request failed", path)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := make(map[string]any)
	if err := json.Unmarshal(body, &out); err != nil {
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
		}
		return nil, fmt.Errorf("GET %s: invalid JSON response", path)
	}
	if resp.StatusCode >= 300 {
		// Keep the decoded body for callers that ignore errors, as before.
		return out, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return out, nil
}

// quotaWindow is one upstream rate-limit window.
type quotaWindow struct {
	UsedPercent   float64
	WindowSeconds int64 // 0 when the upstream does not say
	ResetAt       int64 // unix seconds, 0 when unknown
}

// upstreamQuota is the part of /usage the stale cooldown watcher relies on.
type upstreamQuota struct {
	Short        quotaWindow  // the 5h window
	Weekly       *quotaWindow // nil when the account has no weekly window
	LimitReached bool         // rate_limit.limit_reached == true
	Disallowed   bool         // rate_limit.allowed == false
}

// parseUpstreamQuota extracts the 5h and weekly windows from a /usage response.
// It fails when the 5h used percentage is missing, so an unexpected or empty
// response can never look like a recovered quota.
func parseUpstreamQuota(usage map[string]any) (upstreamQuota, error) {
	rl, ok := usage["rate_limit"].(map[string]any)
	if !ok {
		return upstreamQuota{}, fmt.Errorf("usage response has no rate_limit")
	}
	var q upstreamQuota
	if v, ok := rl["limit_reached"].(bool); ok && v {
		q.LimitReached = true
	}
	if v, ok := rl["allowed"].(bool); ok && !v {
		q.Disallowed = true
	}
	primary, okP := parseQuotaWindow(rl["primary_window"])
	secondary, okS := parseQuotaWindow(rl["secondary_window"])
	// The primary window is the 5h one; swap when the upstream says otherwise.
	if okP && okS && primary.WindowSeconds > 6*3600 && secondary.WindowSeconds > 0 && secondary.WindowSeconds <= 6*3600 {
		primary, secondary = secondary, primary
	}
	if !okP {
		return upstreamQuota{}, fmt.Errorf("usage response has no 5h used_percent")
	}
	q.Short = primary
	if okS {
		w := secondary
		q.Weekly = &w
	}
	return q, nil
}

func parseQuotaWindow(raw any) (quotaWindow, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return quotaWindow{}, false
	}
	used, ok := m["used_percent"].(float64)
	if !ok {
		return quotaWindow{}, false
	}
	w := quotaWindow{UsedPercent: used}
	if v, ok := m["limit_window_seconds"].(float64); ok {
		w.WindowSeconds = int64(v)
	}
	if v, ok := m["reset_at"].(float64); ok {
		w.ResetAt = int64(v)
	}
	return w, true
}

// recovered reports whether the quota is usable again: the 5h window is below
// threshold, the weekly window is not used up and the upstream does not flag
// the limit as reached.
func (q upstreamQuota) recovered(threshold float64) bool {
	if q.LimitReached || q.Disallowed {
		return false
	}
	if q.Short.UsedPercent >= threshold {
		return false
	}
	return q.Weekly == nil || q.Weekly.UsedPercent < 100
}

// consumeCreditUpstream redeems one reset credit. Its error carries the upstream
// response body for the CPA log; never send it to buyers.
func consumeCreditUpstream(auth *authFilePayload, creditID, userAgent string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	payload := map[string]string{
		"redeem_request_id": newRedeemRequestID(),
		"credit_id":         creditID,
	}
	b, _ := json.Marshal(payload)

	req, err := upstreamRequest(http.MethodPost, "/rate-limit-reset-credits/consume", b, auth, userAgent)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("upstream rejected with HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func newRedeemRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req_%d", time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(b[:])
}

// filterUsage keeps only the rate-limit windows, dropping account identifiers.
func filterUsage(usage map[string]any) map[string]any {
	filtered := make(map[string]any)
	if rl, ok := usage["rate_limit"]; ok {
		filtered["rate_limit"] = rl
	}
	return filtered
}

// sanitizeCredits converts the upstream credit list into the whitelisted shape sent
// to buyers.
func sanitizeCredits(credits map[string]any) (int, []ResetCreditItem) {
	availableCount := 0
	if ac, ok := credits["available_count"].(float64); ok {
		availableCount = int(ac)
	}
	var cleanCredits []ResetCreditItem
	if rawList, ok := credits["credits"].([]any); ok {
		for _, item := range rawList {
			if m, ok := item.(map[string]any); ok {
				cItem := ResetCreditItem{
					Title:  "额度重置卡 (周限 + 5小时)",
					Status: "available",
				}
				if t, ok := m["title"].(string); ok && t != "" {
					cItem.Title = t
				}
				if s, ok := m["status"].(string); ok && s != "" {
					cItem.Status = s
				}
				if g, ok := m["granted_at"].(string); ok {
					cItem.GrantedAt = &g
				}
				if exp, ok := m["expires_at"].(string); ok {
					cItem.ExpiresAt = &exp
				}
				if red, ok := m["redeemed_at"].(string); ok {
					cItem.RedeemedAt = &red
				}
				cleanCredits = append(cleanCredits, cItem)
			}
		}
	}
	return availableCount, cleanCredits
}

func firstAvailableCredit(credits map[string]any) string {
	rawList, _ := credits["credits"].([]any)
	for _, item := range rawList {
		if m, ok := item.(map[string]any); ok {
			if s, _ := m["status"].(string); s == "available" {
				if id, _ := m["id"].(string); id != "" {
					return id
				}
			}
		}
	}
	return ""
}
