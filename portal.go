package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed portal.html
var embeddedPortalHTML []byte

//go:embed monitor.html
var embeddedMonitorHTML []byte

// resetMu serializes reset credit redemptions so a double click never redeems twice.
var resetMu sync.Mutex

// handleManagement serves management.handle, which CPA uses both for the plugin's
// Management API routes (under /v0/management/, already authenticated with the
// management key) and for its unauthenticated resources (under
// /v0/resource/plugins/<plugin-id>/).
func handleManagement(raw []byte, table *routingTable, cfg PluginConfig) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := jsonUnmarshal(raw, &req); err != nil {
		return nil, err
	}
	if strings.HasPrefix(req.Path, managementBasePath+"/") {
		return handleAdminAPI(req, table, cfg)
	}
	return handleResource(req, table, cfg)
}

// handleResource routes calls under /v0/resource/plugins/<plugin-id>/*
func handleResource(req pluginapi.ManagementRequest, table *routingTable, cfg PluginConfig) ([]byte, error) {
	switch resourceSubpath(req.Path) {
	case RoutePortal:
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    pageHeaders(),
			Body:       embeddedPortalHTML,
		})

	case RouteMonitor:
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: pageHeaders(), Body: embeddedMonitorHTML})

	case RouteAPIMonitor:
		return handleAPIMonitor(req, table, cfg)

	case RouteAdmin:
		return serveAdminPage()

	case RouteAPIStatus:
		return handleAPIStatus(req, table, cfg)

	case RouteAPIReset:
		return handleAPIReset(req, table, cfg)

	default:
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Body:       []byte(`{"error":"not_found"}`),
		})
	}
}

// pageHeaders are the response headers of the embedded HTML pages. The pages only
// talk to their own origin and never leak their URL (which may carry #key=).
func pageHeaders() http.Header {
	return http.Header{
		"Content-Type":            []string{"text/html; charset=utf-8"},
		"Cache-Control":           []string{"no-store"},
		"Referrer-Policy":         []string{"no-referrer"},
		"X-Content-Type-Options":  []string{"nosniff"},
		"X-Frame-Options":         []string{"SAMEORIGIN"},
		"Content-Security-Policy": []string{"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"},
	}
}

// resourceSubpath strips the /v0/resource/plugins/<plugin-id> prefix that CPA keeps
// on resource requests. The plugin ID comes from the shared library file name, so
// it is not hard-coded.
func resourceSubpath(p string) string {
	rest, ok := strings.CutPrefix(p, "/v0/resource/plugins/")
	if !ok {
		return p
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return "/"
}

// buyerKey reads the buyer key from the X-Carpool-Key header, which keeps it out of
// access logs, falling back to the key query parameter.
func buyerKey(req pluginapi.ManagementRequest) string {
	if k := strings.TrimSpace(req.Headers.Get("X-Carpool-Key")); k != "" {
		return k
	}
	return strings.TrimSpace(req.Query.Get("key"))
}

// lookupSeat resolves the buyer key sent by the portal. When it returns nil, status
// and message describe the error to send back.
func lookupSeat(table *routingTable, key string) (buyer *seat, status int, message string) {
	switch {
	case key == "":
		return nil, http.StatusBadRequest, "key is required"
	case table == nil:
		return nil, http.StatusServiceUnavailable, "plugin not initialized"
	case table.configErr != nil:
		return nil, http.StatusServiceUnavailable, "plugin configuration is invalid, please contact the host"
	}

	buyer = table.byKey[key]
	if buyer == nil {
		return nil, http.StatusNotFound, "Key not found or unauthorized"
	}
	if buyer.user.Paused {
		return nil, http.StatusForbidden, "Seat paused, please contact the host"
	}
	if buyer.expired(time.Now()) {
		return nil, http.StatusForbidden, "Seat expired at " + buyer.user.ExpireAt + ", please contact the host to renew"
	}
	return buyer, http.StatusOK, ""
}

func handleAPIStatus(req pluginapi.ManagementRequest, table *routingTable, cfg PluginConfig) ([]byte, error) {
	key := buyerKey(req)
	buyer, status, message := lookupSeat(table, key)
	if buyer == nil {
		return jsonResponse(status, map[string]string{"error": message})
	}

	entry, err := findSeatAuth(buyer)
	if err != nil {
		logf("warn", "status for seat %s: %v", buyer.user.Name, err)
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "Seat account is not available in the gateway, please contact the host"})
	}

	now := time.Now()
	resp := BuyerStatusResponse{
		SeatName:       buyer.user.Name,
		Provider:       authProvider(entry),
		PublicBaseURL:  table.publicBaseURL,
		Limits:         seatLimits(buyer, now),
		QuotaSupported: supportsUpstreamQuota(entry),
		UpstreamUsage:  map[string]any{},
		IsCooling:      isCooling(entry.AuthIndex, now),
	}
	if !buyer.expireAt.IsZero() {
		resp.ExpiresAt = &buyer.expireAt
	}

	// 1. Upstream quota windows and reset credits (Codex seats only, cached)
	if resp.QuotaSupported {
		info, err := cachedUpstreamInfo(entry.AuthIndex, cfg.UpstreamUserAgent)
		if err != nil {
			logf("warn", "quota for seat %s: %v", buyer.user.Name, err)
		} else {
			resp.UpstreamUsage = filterUsage(info.usage)
			resp.CreditsAvailable, resp.CreditsList = sanitizeCredits(info.credits)
		}
	}

	// 2. Token usage recorded by CPA Manager Plus, when its database is present
	stats, usageErr := readTokenStats(cfg.DBPath, usageKeyHash(key))
	resp.UsageAvailable = usageErr == nil
	if usageErr != nil && !errors.Is(usageErr, errUsageDBUnavailable) {
		logf("warn", "usage query failed: %v", usageErr)
	}
	resp.LocalCalls = stats.Calls
	resp.InputTokens = stats.InputTokens
	resp.OutputTokens = stats.OutputTokens
	resp.CachedTokens = stats.CachedTokens
	resp.TotalTokens = stats.TotalTokens

	return jsonResponse(http.StatusOK, resp)
}

// handleAPIReset consumes one reset credit and clears CPA's cooldown for the seat.
// CPA routes plugin resources for GET only, so the reset is a GET that requires an
// explicit confirm=1; merely opening the URL without it changes nothing.
func handleAPIReset(req pluginapi.ManagementRequest, table *routingTable, cfg PluginConfig) ([]byte, error) {
	if req.Method != "" && req.Method != http.MethodGet {
		return jsonResponse(http.StatusMethodNotAllowed, ResetResponse{Error: "method not allowed"})
	}
	if req.Query.Get("confirm") != "1" {
		return jsonResponse(http.StatusBadRequest, ResetResponse{Error: "confirm=1 query parameter is required"})
	}

	buyer, status, message := lookupSeat(table, buyerKey(req))
	if buyer == nil {
		return jsonResponse(status, ResetResponse{Error: message})
	}

	if !resetMu.TryLock() {
		return jsonResponse(http.StatusConflict, ResetResponse{Error: "另一笔重置正在处理中，请几秒后再试"})
	}
	defer resetMu.Unlock()

	entry, err := findSeatAuth(buyer)
	if err != nil {
		logf("warn", "reset for seat %s: %v", buyer.user.Name, err)
		return jsonResponse(http.StatusServiceUnavailable, ResetResponse{Error: "席位账号当前不可用，请联系车主"})
	}
	if !supportsUpstreamQuota(entry) {
		return jsonResponse(http.StatusBadRequest, ResetResponse{Error: "该席位类型不支持重置卡"})
	}
	creds, err := loadCredentials(entry.AuthIndex)
	if err != nil {
		logf("warn", "reset for seat %s: %v", buyer.user.Name, err)
		return jsonResponse(http.StatusServiceUnavailable, ResetResponse{Error: "席位账号当前不可用，请联系车主"})
	}

	// 1. Fetch fresh credits to find an available card
	info := fetchUpstreamInfo(creds, cfg.UpstreamUserAgent)
	creditID := firstAvailableCredit(info.credits)
	if creditID == "" {
		return jsonResponse(http.StatusBadRequest, ResetResponse{Error: "No available reset credits left"})
	}

	// 2. Consume the credit upstream. The raw error may contain account details, so
	// it only goes to the CPA log.
	if err := consumeCreditUpstream(creds, creditID, cfg.UpstreamUserAgent); err != nil {
		logf("error", "reset for seat %s: %v", buyer.user.Name, err)
		return jsonResponse(http.StatusBadGateway, ResetResponse{Error: "核销失败，上游未接受请求，请稍后重试或联系车主"})
	}
	quotaCache.drop(entry.AuthIndex)

	// 3. Clear CPA's in-memory quota cooldown so the seat is routable again
	if err := resetCooldown(entry.AuthIndex); err != nil {
		logf("warn", "reset for seat %s: credit redeemed but cooldown not cleared: %v", buyer.user.Name, err)
		return jsonResponse(http.StatusOK, ResetResponse{Success: true, Message: "重置卡已核销，但网关冷却未能自动解除，请稍后再试或联系车主"})
	}

	logf("info", "seat %s redeemed a reset credit", buyer.user.Name)
	return jsonResponse(http.StatusOK, ResetResponse{Success: true, Message: "重置成功，额度已恢复满血，网关冷却已解除"})
}

// usageKeyHash matches the api_key_hash column written by CPA Manager Plus.
func usageKeyHash(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

func jsonResponse(status int, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  []string{"application/json"},
			"Cache-Control": []string{"no-store"},
		},
		Body: b,
	})
}
