package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// CPA v8.0.30 has no configuration callback in its plugin ABI. Use its direct
// listener instead; never derive a destination from browser headers or query data.
// The environment override belongs to the CPA process, for custom ports/TLS.
var configHTTPClient = &http.Client{
	Timeout: 8 * time.Second,
	Transport: &http.Transport{
		Proxy: nil,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

var managementPluginID atomic.Value
var configMutationMu sync.Mutex

func noteManagementRegistration(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var req pluginapi.ManagementRegistrationRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	if req.ResourceBasePath == "" {
		return nil
	}
	id, ok := strings.CutPrefix(req.ResourceBasePath, "/v0/resource/plugins/")
	if !ok || id == "" || strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return fmt.Errorf("invalid plugin resource base path")
	}
	managementPluginID.Store(id)
	return nil
}

func configPluginID() string {
	if id, ok := managementPluginID.Load().(string); ok {
		return id
	}
	return "cpa-carpool"
}

func hostConfigRequest(req pluginapi.ManagementRequest, method, path string, body []byte) ([]byte, error) {
	return hostConfigRequestContext(context.Background(), req, method, path, body)
}

func hostConfigRequestContext(ctx context.Context, req pluginapi.ManagementRequest, method, path string, body []byte) ([]byte, error) {
	status, data, err := directHostRequest(ctx, req, method, "/v8/management/config"+path, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("CPA configuration request returned HTTP %d", status)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid CPA configuration response")
	}
	return data, nil
}

func directHostRequest(ctx context.Context, req pluginapi.ManagementRequest, method, path string, body []byte) (int, []byte, error) {
	base := strings.TrimSpace(os.Getenv("CARPOOL_HOST_URL"))
	if base == "" {
		base = "http://127.0.0.1:8317"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return 0, nil, fmt.Errorf("invalid CARPOOL_HOST_URL")
	}
	upstream, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	// Only forward credentials required by CPA, never cookies, Host or proxy headers.
	for _, name := range []string{"Authorization", "X-Management-Key"} {
		if value := req.Headers.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
	upstream.Header.Set("Accept", "application/json")
	if body != nil {
		upstream.Header.Set("Content-Type", "application/json")
	}
	resp, err := configHTTPClient.Do(upstream)
	if err != nil {
		return 0, nil, fmt.Errorf("CPA configuration request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return 0, nil, fmt.Errorf("invalid CPA configuration response")
	}
	return resp.StatusCode, data, nil
}

func configObject(raw []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("configuration must be a JSON object")
	}
	return obj, nil
}

func configChild(obj map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	if raw, exists := obj[key]; exists {
		return configObject(raw)
	}
	return map[string]json.RawMessage{}, nil
}

// Read the root rather than interpreting a subtree's 404 as an absent value.
// Only a successful, structurally valid response can authorize first-time setup.
func readHostConfig(req pluginapi.ManagementRequest) (map[string]json.RawMessage, error) {
	raw, err := hostConfigRequest(req, http.MethodGet, "", nil)
	if err != nil {
		return nil, err
	}
	root, err := configObject(raw)
	if err != nil {
		return nil, err
	}
	if _, exists := root["plugins"]; !exists {
		return nil, fmt.Errorf("CPA configuration response is missing plugins")
	}
	return root, nil
}

func handleAdminConfig(req pluginapi.ManagementRequest, route string) ([]byte, error) {
	if strings.EqualFold(req.Method, http.MethodPut) {
		if !configMutationMu.TryLock() {
			return jsonResponse(http.StatusConflict, map[string]string{"error": "后台保存任务正在执行，请稍后再试"})
		}
		defer configMutationMu.Unlock()
	}
	failed := func() ([]byte, error) {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "无法读取配置接口，请检查网络或反代"})
	}
	root, err := readHostConfig(req)
	if err != nil {
		return failed()
	}
	var current any
	path := "/access/api-keys"
	if route == RouteAdminConfig {
		plugins, err := configChild(root, "plugins")
		if err != nil {
			return failed()
		}
		configs, err := configChild(plugins, "configs")
		if err != nil {
			return failed()
		}
		cfg := map[string]json.RawMessage{"enabled": json.RawMessage(`true`), "users": json.RawMessage(`[]`)}
		raw, exists := configs[configPluginID()]
		if exists {
			cfg, err = configObject(raw)
			if err != nil {
				return failed()
			}
		}
		current = map[string]any{"config": cfg, "exists": exists, "revision": persistedRevision(cfg, exists)}
		path = "/plugins/configs/" + url.PathEscape(configPluginID())
	} else {
		access, err := configChild(root, "access")
		if err != nil {
			return failed()
		}
		keys := []string{}
		if raw, exists := access["api-keys"]; exists {
			if err := json.Unmarshal(raw, &keys); err != nil || keys == nil {
				return failed()
			}
		}
		current = keys
	}
	if req.Method == "" || strings.EqualFold(req.Method, http.MethodGet) {
		return jsonResponse(http.StatusOK, current)
	}
	if route == RouteAdminConfig {
		_, err := configObject(req.Body)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		// Match the validation endpoint before allowing a direct PUT as well.
		var doc map[string]any
		if err := json.Unmarshal(req.Body, &doc); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid configuration"})
		}
		configYAML, err := yaml.Marshal(doc)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid configuration"})
		}
		_, table, err := parseConfig(configYAML)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		table.monitor.close()
	} else {
		var keys []string
		if err := json.Unmarshal(req.Body, &keys); err != nil || len(keys) == 0 {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "api-keys must be a non-empty string array"})
		}
		for _, key := range keys {
			if strings.TrimSpace(key) == "" {
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": "api-keys must not contain empty keys"})
			}
		}
	}
	if _, err := hostConfigRequest(req, http.MethodPut, path, req.Body); err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "保存配置失败，请检查 CPA 配置接口后重试"})
	}
	return jsonResponse(http.StatusOK, map[string]string{"status": "ok"})
}

func persistedRevision(cfg any, exists bool) string {
	raw, _ := json.Marshal(cfg)
	var normalized any
	json.Unmarshal(raw, &normalized)
	canonical, _ := json.Marshal([]any{exists, normalized})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

type configSnapshot struct {
	Config   json.RawMessage
	Exists   bool
	Revision string
	Keys     []string
}

func readConfigSnapshot(ctx context.Context, req pluginapi.ManagementRequest) (configSnapshot, error) {
	var out configSnapshot
	raw, err := hostConfigRequestContext(ctx, req, http.MethodGet, "", nil)
	if err != nil {
		return out, err
	}
	root, err := configObject(raw)
	if err != nil {
		return out, err
	}
	if _, ok := root["plugins"]; !ok {
		return out, fmt.Errorf("missing plugins")
	}
	plugins, err := configChild(root, "plugins")
	if err != nil {
		return out, err
	}
	configs, err := configChild(plugins, "configs")
	if err != nil {
		return out, err
	}
	out.Config, out.Exists = configs[configPluginID()]
	if !out.Exists {
		out.Config = json.RawMessage(`{"enabled":true,"users":[]}`)
	}
	if _, err := configObject(out.Config); err != nil {
		return out, err
	}
	out.Revision = persistedRevision(out.Config, out.Exists)
	access, err := configChild(root, "access")
	if err != nil {
		return out, err
	}
	out.Keys = []string{}
	if raw, ok := access["api-keys"]; ok {
		if err := json.Unmarshal(raw, &out.Keys); err != nil || out.Keys == nil {
			return out, fmt.Errorf("invalid client keys")
		}
	}
	for i, key := range out.Keys {
		out.Keys[i] = strings.TrimSpace(key)
		if out.Keys[i] == "" {
			return out, fmt.Errorf("empty client key")
		}
	}
	return out, nil
}
