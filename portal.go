package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed portal.html
var embeddedPortalHTML []byte

type authFilePayload struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	Email       string `json:"email"`
	PlanType    string `json:"plan_type"`
}

// handleResource routes calls under /v0/resource/plugins/carpool/*
func handleResource(raw []byte, table *routingTable, cfg PluginConfig) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := jsonUnmarshal(raw, &req); err != nil {
		return nil, err
	}

	cleanPath := strings.TrimPrefix(req.Path, "/v0/resource/plugins/carpool")
	cleanPath = strings.TrimPrefix(cleanPath, RoutePortal)

	switch {
	case cleanPath == "" || cleanPath == "/" || cleanPath == "/index.html":
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"text/html; charset=utf-8"},
				"Cache-Control": []string{"no-cache"},
			},
			Body: embeddedPortalHTML,
		})

	case cleanPath == "/api/status" || strings.HasSuffix(cleanPath, "/api/status"):
		return handleAPIStatus(req, table, cfg)

	case cleanPath == "/api/reset" || strings.HasSuffix(cleanPath, "/api/reset"):
		return handleAPIReset(req, table, cfg)

	default:
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Body:       []byte(`{"error":"not_found"}`),
		})
	}
}

func handleAPIStatus(req pluginapi.ManagementRequest, table *routingTable, cfg PluginConfig) ([]byte, error) {
	key := strings.TrimSpace(req.Query.Get("key"))
	if key == "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "key query parameter is required"})
	}

	if table == nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "plugin not initialized"})
	}

	user := table.byKey[key]
	if user == nil {
		user = table.byScope[callerScope(key)]
	}
	if user == nil {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "Key not found or unauthorized"})
	}

	authIDs := table.authMap[user]
	if len(authIDs) == 0 {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "No primary seat assigned to this key"})
	}

	primaryAuthID := authIDs[0]
	authData, err := loadAuthPayload(cfg.AuthDir, primaryAuthID)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "Auth file missing or corrupted"})
	}

	// 1. Fetch upstream rate limit & credits
	usageInfo, creditsInfo := fetchUpstreamInfo(authData)

	// Filter usage to prevent leaking internal OpenAI account info
	filteredUsage := make(map[string]any)
	if rl, ok := usageInfo["rate_limit"]; ok {
		filteredUsage["rate_limit"] = rl
	}

	// 2. Parse credits list and count
	availableCount := 0
	if ac, ok := creditsInfo["available_count"].(float64); ok {
		availableCount = int(ac)
	}
	var cleanCredits []ResetCreditItem
	if rawList, ok := creditsInfo["credits"].([]any); ok {
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

	// 3. Read SQLite TokenStats (with breakdown of Input / Output / Cached)
	keyHash := callerScopeRaw(key)
	stats := readTokenStats(cfg.DBPath, keyHash)

	// Check local cooldown (.cds file)
	isCooling := checkLocalCooling(cfg.AuthDir, primaryAuthID)

	var expireTime *time.Time
	if user.ExpireAt != "" {
		if t, err := time.Parse(time.RFC3339, user.ExpireAt); err == nil {
			expireTime = &t
		}
	}

	resp := BuyerStatusResponse{
		SeatName:         user.Name,
		UpstreamUsage:    filteredUsage,
		CreditsAvailable: availableCount,
		CreditsList:      cleanCredits,
		LocalCalls:       stats.Calls,
		InputTokens:      stats.InputTokens,
		OutputTokens:     stats.OutputTokens,
		CachedTokens:     stats.CachedTokens,
		TotalTokens:      stats.TotalTokens,
		IsCooling:        isCooling,
		ExpiresAt:        expireTime,
	}

	return jsonResponse(http.StatusOK, resp)
}

func handleAPIReset(req pluginapi.ManagementRequest, table *routingTable, cfg PluginConfig) ([]byte, error) {
	if req.Method != http.MethodPost {
		return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}

	var resetReq ResetRequest
	if err := json.Unmarshal(req.Body, &resetReq); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid json body"})
	}

	key := strings.TrimSpace(resetReq.Key)
	if key == "" {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "key is required"})
	}

	user := table.byKey[key]
	if user == nil {
		user = table.byScope[callerScope(key)]
	}
	if user == nil {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "Key not found or unauthorized"})
	}

	authIDs := table.authMap[user]
	if len(authIDs) == 0 {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "No primary seat assigned to this key"})
	}

	primaryAuthID := authIDs[0]
	authData, err := loadAuthPayload(cfg.AuthDir, primaryAuthID)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "Auth file missing"})
	}

	// 1. Fetch credits to find valid available card
	_, creditsInfo := fetchUpstreamInfo(authData)
	var targetCreditID string
	if rawList, ok := creditsInfo["credits"].([]any); ok {
		for _, item := range rawList {
			if m, ok := item.(map[string]any); ok {
				if s, _ := m["status"].(string); s == "available" {
					if id, _ := m["id"].(string); id != "" {
						targetCreditID = id
						break
					}
				}
			}
		}
	}

	if targetCreditID == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "No available reset credits left",
		})
	}

	// 2. Consume credit upstream
	errConsume := consumeCreditUpstream(authData, targetCreditID)
	if errConsume != nil {
		errStr := errConsume.Error()
		if strings.Contains(strings.ToLower(errStr), "token") || strings.Contains(strings.ToLower(errStr), "auth") {
			errStr = "核销请求异常，请稍后重试"
		}
		return jsonResponse(http.StatusInternalServerError, map[string]any{
			"success": false,
			"error":   errStr,
		})
	}

	// 3. Clear local CPA cooldown lock
	removeLocalCooling(cfg.AuthDir, primaryAuthID)

	return jsonResponse(http.StatusOK, map[string]any{
		"success": true,
		"message": "重置成功，额度已恢复满血",
	})
}

// Helpers
func loadAuthPayload(authDir, authID string) (*authFilePayload, error) {
	if authDir == "" {
		authDir = "/root/.cli-proxy-api"
	}
	name := strings.TrimSuffix(authID, ".json") + ".json"
	path := filepath.Join(authDir, name)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p authFilePayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func fetchUpstreamInfo(auth *authFilePayload) (map[string]any, map[string]any) {
	client := &http.Client{Timeout: 8 * time.Second}
	usageRes := make(map[string]any)
	creditsRes := make(map[string]any)

	// usage
	reqU, _ := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if reqU != nil {
		reqU.Header.Set("Authorization", "Bearer "+auth.AccessToken)
		reqU.Header.Set("ChatGPT-Account-Id", auth.AccountID)
		reqU.Header.Set("User-Agent", "codex_cli_rs/0.160.0 (x86_64-unknown-linux-gnu)")
		if resp, err := client.Do(reqU); err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			_ = json.Unmarshal(body, &usageRes)
		}
	}

	// credits
	reqC, _ := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", nil)
	if reqC != nil {
		reqC.Header.Set("Authorization", "Bearer "+auth.AccessToken)
		reqC.Header.Set("ChatGPT-Account-Id", auth.AccountID)
		reqC.Header.Set("User-Agent", "codex_cli_rs/0.160.0 (x86_64-unknown-linux-gnu)")
		if resp, err := client.Do(reqC); err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			_ = json.Unmarshal(body, &creditsRes)
		}
	}

	return usageRes, creditsRes
}

func consumeCreditUpstream(auth *authFilePayload, creditID string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	payload := map[string]string{
		"redeem_request_id": fmt.Sprintf("req_%d", time.Now().UnixMilli()),
		"credit_id":        creditID,
	}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", auth.AccountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.160.0 (x86_64-unknown-linux-gnu)")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upstream rejected with HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func checkLocalCooling(authDir, authID string) bool {
	if authDir == "" {
		authDir = "/root/.cli-proxy-api"
	}
	cds := filepath.Join(authDir, strings.TrimSuffix(authID, ".json")+".cds")
	_, err := os.Stat(cds)
	return err == nil
}

func removeLocalCooling(authDir, authID string) {
	if authDir == "" {
		authDir = "/root/.cli-proxy-api"
	}
	cds := filepath.Join(authDir, strings.TrimSuffix(authID, ".json")+".cds")
	_ = os.Remove(cds)
}

func callerScopeRaw(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return fmt.Sprintf("%x", sum[:])
}

func jsonResponse(status int, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: b,
	})
}
