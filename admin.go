package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// The admin page is a resource, because a browser cannot send the management key
// when it navigates, so it carries no data at all. Everything it shows comes from
// the management routes below, which CPA serves only to callers holding the
// management key, and from CPA's own /v8/management/config API, which is also
// where the page saves seats and access.api-keys.

//go:embed admin.html
var embeddedAdminHTML []byte

// adminRoutes are the management routes registered by the plugin.
var adminRoutes = []pluginapi.ManagementRoute{
	{Method: http.MethodGet, Path: RouteAdminOverview, Description: "拼车管家：席位运行状态（账号、限额使用、用量）"},
	{Method: http.MethodGet, Path: RouteAdminAuths, Description: "拼车管家：可绑定的 CPA 账号"},
	{Method: http.MethodPost, Path: RouteAdminValidate, Description: "拼车管家：保存前校验插件配置"},
}

// handleAdminAPI serves the management routes. CPA has already checked the
// management key before the request reaches the plugin.
func handleAdminAPI(req pluginapi.ManagementRequest, table *routingTable, cfg PluginConfig) ([]byte, error) {
	route := strings.TrimPrefix(req.Path, managementBasePath)
	for _, r := range adminRoutes {
		if r.Path != route {
			continue
		}
		if req.Method != "" && !strings.EqualFold(req.Method, r.Method) {
			return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		}
		switch route {
		case RouteAdminOverview:
			return jsonResponse(http.StatusOK, adminOverview(table, cfg, time.Now()))
		case RouteAdminAuths:
			return handleAdminAuths(table)
		case RouteAdminValidate:
			return handleAdminValidate(req.Body)
		}
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
}

func adminOverview(table *routingTable, cfg PluginConfig, now time.Time) AdminOverview {
	out := AdminOverview{
		PluginVersion: PluginVersion,
		Enabled:       cfg.Enabled,
		LegacyHost:    legacyHost.Load(),
		UsageDatabase: resolveUsageDB(cfg.DBPath) != "",
		Seats:         []AdminSeat{},
	}
	if table == nil {
		return out
	}
	out.ConfigRevision = table.configRevision
	if table.configErr != nil {
		out.ConfigError = table.configErr.Error()
		return out
	}

	files, err := listAuths()
	if err != nil {
		logf("warn", "admin overview: %v", err)
	}

	var hashes []string
	for _, s := range table.seats {
		for _, k := range s.apiKeys {
			hashes = append(hashes, usageKeyHash(k))
		}
	}
	usage, usageErr := readTokenStatsByHash(cfg.DBPath, hashes)
	out.UsageAvailable = usageErr == nil
	if usageErr != nil && !errors.Is(usageErr, errUsageDBUnavailable) {
		logf("warn", "admin usage query failed: %v", usageErr)
	}

	for _, s := range table.seats {
		as := AdminSeat{
			Name:    s.user.Name,
			Keys:    len(s.apiKeys),
			Primary: append([]string{}, s.authIDs...),
			Expired: s.expired(now),
			Limits:  seatLimits(s, now),
		}
		if !s.expireAt.IsZero() {
			exp := s.expireAt
			as.ExpiresAt = &exp
		}
		for _, k := range s.apiKeys {
			as.Usage = as.Usage.add(usage[usageKeyHash(k)])
		}
		if entry, ok := matchSeatAuth(s, files); ok {
			as.Auth = &AdminSeatAuth{
				ID:          entry.ID,
				Provider:    authProvider(entry),
				Status:      entry.Status,
				Disabled:    entry.Disabled,
				Unavailable: entry.Unavailable,
				Cooling:     isCooling(entry.AuthIndex, now),
			}
		}
		out.Seats = append(out.Seats, as)
	}
	return out
}

// handleAdminAuths lists the accounts a seat can be bound to, with the seats
// already bound to each. Accounts from config.yaml (API keys) are not listed by
// CPA's host.auth.list and have to be typed in by hand.
func handleAdminAuths(table *routingTable) ([]byte, error) {
	files, err := listAuths()
	if err != nil {
		logf("warn", "admin auths: %v", err)
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "CPA did not return its accounts"})
	}
	boundTo := make(map[string][]string)
	if table != nil {
		for _, s := range table.seats {
			for _, id := range s.matchIDs {
				boundTo[id] = append(boundTo[id], s.user.Name)
			}
		}
	}
	auths := make([]AdminAuth, 0, len(files))
	for _, f := range files {
		auths = append(auths, AdminAuth{
			ID:          f.ID,
			Provider:    authProvider(f),
			Label:       f.Label,
			Email:       f.Email,
			Status:      f.Status,
			Disabled:    f.Disabled,
			Unavailable: f.Unavailable,
			RuntimeOnly: f.RuntimeOnly,
			Seats:       boundTo[normalizeAuthID(f.ID)],
		})
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	return jsonResponse(http.StatusOK, map[string]any{"auths": auths})
}

// handleAdminValidate checks a plugin configuration (the JSON object the admin
// page is about to save) with the same strict rules as plugin.reconfigure, so a
// typo cannot put the plugin into its reject-everything state.
func handleAdminValidate(body []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "body must be a JSON object"})
	}
	// CPA stores the JSON as YAML and hands that to the plugin, so validate the
	// same way rather than feeding JSON to the YAML decoder directly.
	configYAML, err := yaml.Marshal(doc)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	_, table, err := parseConfig(configYAML)
	if err != nil {
		return jsonResponse(http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"valid": true, "seats": len(table.seats), "config_revision": table.configRevision})
}

// serveAdminPage returns the admin single-page app. It holds no data.
func serveAdminPage() ([]byte, error) {
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    pageHeaders(),
		Body:       embeddedAdminHTML,
	})
}
