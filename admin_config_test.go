package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const persistedConfig = `{"plugins":{"configs":{"cpa-carpool":{"enabled":true,"priority":100,"store":{"version":"0.5.0"},"users":[{"name":"Alice","api-keys":["sk-old"],"primary":["alice.json"]}]}}},"access":{"api-keys":["sk-owner","sk-old"]}}`

func configCall(t *testing.T, method, route, body string) pluginapi.ManagementResponse {
	t.Helper()
	return adminRequest(t, pluginapi.ManagementRequest{
		Method: method, Path: managementBasePath + route, Body: []byte(body),
		Headers: http.Header{
			"Authorization": {"Bearer test-management"},
			"Cookie":        {"must-not-forward"}, "X-Forwarded-Host": {"untrusted.test"},
		},
	})
}

func TestAdminConfigReadsAndWritesDirectHost(t *testing.T) {
	var writes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-management" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-Host") != "" {
			t.Errorf("unexpected forwarded headers: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v8/management/config" {
			io.WriteString(w, persistedConfig)
			return
		}
		body, _ := io.ReadAll(r.Body)
		writes = append(writes, r.Method+" "+r.URL.Path+" "+string(body))
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	t.Setenv("CARPOOL_HOST_URL", server.URL)

	got := configCall(t, http.MethodGet, RouteAdminConfig, "")
	var cfg struct {
		Exists bool           `json:"exists"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(got.Body, &cfg); err != nil || got.StatusCode != 200 || !cfg.Exists || cfg.Config["store"] == nil {
		t.Fatalf("config %d %s: %v", got.StatusCode, got.Body, err)
	}
	if got.Headers.Get("Cache-Control") != "no-store" || strings.Contains(string(got.Body), "sk-owner") {
		t.Fatalf("config leaks unrelated configuration or caches secrets: %v %s", got.Headers, got.Body)
	}
	got = configCall(t, http.MethodGet, RouteAdminAPIKeys, "")
	if got.StatusCode != 200 || string(got.Body) != `["sk-owner","sk-old"]` {
		t.Fatalf("keys %d %s", got.StatusCode, got.Body)
	}
	for _, tt := range []struct{ route, body, path string }{
		{RouteAdminConfig, `{"enabled":true,"users":[]}`, "/plugins/configs/cpa-carpool"},
		{RouteAdminAPIKeys, `["sk-owner"]`, "/access/api-keys"},
	} {
		got = configCall(t, http.MethodPut, tt.route, tt.body)
		if got.StatusCode != 200 || writes[len(writes)-1] != "PUT /v8/management/config"+tt.path+" "+tt.body {
			t.Fatalf("write %d %s, requests %v", got.StatusCode, got.Body, writes)
		}
	}
}

func TestAdminConfigAbsentRequiresSuccessfulRootRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"plugins":{"enabled":true}}`)
	}))
	defer server.Close()
	t.Setenv("CARPOOL_HOST_URL", server.URL)
	got := configCall(t, http.MethodGet, RouteAdminConfig, "")
	if got.StatusCode != 200 || !strings.Contains(string(got.Body), `"exists":false`) || !strings.Contains(string(got.Body), `"users":[]`) {
		t.Fatalf("first setup %d %s", got.StatusCode, got.Body)
	}
	got = configCall(t, http.MethodGet, RouteAdminAPIKeys, "")
	if got.StatusCode != 200 || string(got.Body) != `[]` {
		t.Fatalf("unconfigured keys %d %s", got.StatusCode, got.Body)
	}
}

func TestAdminConfigFailureNeverWritesOrReturnsDefaults(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing route", 404, `{"error":"not_found"}`},
		{"host error", 500, `{"error":"read_failed"}`},
		{"authorization failure", 401, `{"error":"unauthorized"}`},
		{"HTML from proxy", 200, `<html>login</html>`},
		{"null", 200, `null`},
		{"array", 200, `[]`},
		{"unexpected JSON", 200, `{"error":"unavailable"}`},
		{"invalid plugin container", 200, `{"plugins":[]}`},
		{"invalid config container", 200, `{"plugins":{"configs":null}}`},
		{"invalid plugin config", 200, `{"plugins":{"configs":{"cpa-carpool":null}},"access":{"api-keys":null}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			t.Setenv("CARPOOL_HOST_URL", server.URL)
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				got := configCall(t, method, RouteAdminConfig, `{"enabled":true,"users":[]}`)
				if got.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(got.Body), "无法读取配置接口，请检查网络或反代") || strings.Contains(string(got.Body), `"users"`) {
					t.Fatalf("%s returned %d %s", method, got.StatusCode, got.Body)
				}
			}
			if writes != 0 {
				t.Fatalf("failed reads caused %d writes", writes)
			}
		})
	}
}

func TestAdminConfigTimeoutAndConnectionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Setenv("CARPOOL_HOST_URL", server.URL)
	previous := configHTTPClient
	client := *previous
	client.Timeout = 20 * time.Millisecond
	configHTTPClient = &client
	t.Cleanup(func() { configHTTPClient = previous })
	if got := configCall(t, http.MethodGet, RouteAdminConfig, ""); got.StatusCode != 503 {
		t.Fatalf("timeout returned %d %s", got.StatusCode, got.Body)
	}
	server.Close()
	if got := configCall(t, http.MethodPut, RouteAdminConfig, `{"enabled":true,"users":[]}`); got.StatusCode != 503 {
		t.Fatalf("unreachable host returned %d %s", got.StatusCode, got.Body)
	}
}

func TestAdminConfigRejectsInvalidWrites(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes++
		}
		io.WriteString(w, persistedConfig)
	}))
	defer server.Close()
	t.Setenv("CARPOOL_HOST_URL", server.URL)
	for _, tt := range []struct{ route, body string }{
		{RouteAdminConfig, `null`}, {RouteAdminConfig, `[]`},
		{RouteAdminConfig, `{"users":[{"api_keys":["sk-bad"]}]}`},
		{RouteAdminConfig, `{"enabled":true,"rpm":-1}`},
		{RouteAdminAPIKeys, `[]`}, {RouteAdminAPIKeys, `null`},
		{RouteAdminAPIKeys, `[" "]`}, {RouteAdminAPIKeys, `[42]`},
	} {
		if got := configCall(t, http.MethodPut, tt.route, tt.body); got.StatusCode != 400 {
			t.Errorf("%s %s returned %d %s", tt.route, tt.body, got.StatusCode, got.Body)
		}
	}
	if writes != 0 {
		t.Fatalf("invalid requests caused %d writes", writes)
	}
}

func TestAdminAPIKeysReadFailureNeverReturnsEmptyKeys(t *testing.T) {
	for _, body := range []string{
		`{"plugins":{},"access":null}`,
		`{"plugins":{},"access":{"api-keys":null}}`,
		`{"plugins":{},"access":{"api-keys":[42]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					writes++
				}
				io.WriteString(w, body)
			}))
			defer server.Close()
			t.Setenv("CARPOOL_HOST_URL", server.URL)
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				got := configCall(t, method, RouteAdminAPIKeys, `["sk-owner"]`)
				if got.StatusCode != 503 || string(got.Body) == `[]` {
					t.Fatalf("%s returned %d %s", method, got.StatusCode, got.Body)
				}
			}
			if writes != 0 {
				t.Fatalf("failed key reads caused %d writes", writes)
			}
		})
	}
}

func TestAdminConfigReportsFailedHostWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"write_failed"}`)
			return
		}
		io.WriteString(w, persistedConfig)
	}))
	defer server.Close()
	t.Setenv("CARPOOL_HOST_URL", server.URL)
	for _, route := range []string{RouteAdminConfig, RouteAdminAPIKeys} {
		body := `{"enabled":true,"users":[]}`
		if route == RouteAdminAPIKeys {
			body = `["sk-owner"]`
		}
		got := configCall(t, http.MethodPut, route, body)
		if got.StatusCode != 502 || !strings.Contains(string(got.Body), "保存配置失败") {
			t.Fatalf("failed write %s returned %d %s", route, got.StatusCode, got.Body)
		}
	}
}

func TestAdminConfigDoesNotFollowRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	t.Setenv("CARPOOL_HOST_URL", server.URL)
	got := configCall(t, http.MethodPut, RouteAdminConfig, `{"enabled":true,"users":[]}`)
	if got.StatusCode != 503 || redirected {
		t.Fatalf("redirect followed: %t; response %d", redirected, got.StatusCode)
	}
}

func TestAdminConfigUsesRegisteredPluginID(t *testing.T) {
	t.Cleanup(func() { managementPluginID.Store("cpa-carpool") })
	raw, _ := json.Marshal(pluginapi.ManagementRegistrationRequest{ResourceBasePath: "/v0/resource/plugins/custom-carpool"})
	if _, err := handleMethod(pluginabi.MethodManagementRegister, raw); err != nil {
		t.Fatal(err)
	}
	path := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			path = r.URL.Path
		}
		io.WriteString(w, `{"plugins":{"configs":{"custom-carpool":{"enabled":true,"rpm":10}}}}`)
	}))
	defer server.Close()
	t.Setenv("CARPOOL_HOST_URL", server.URL)
	got := configCall(t, http.MethodGet, RouteAdminConfig, "")
	if got.StatusCode != 200 || !strings.Contains(string(got.Body), `"rpm":10`) || !strings.Contains(string(got.Body), `"exists":true`) {
		t.Fatalf("custom config %d %s", got.StatusCode, got.Body)
	}
	got = configCall(t, http.MethodPut, RouteAdminConfig, `{"enabled":true,"users":[]}`)
	if got.StatusCode != 200 || path != "/v8/management/config/plugins/configs/custom-carpool" {
		t.Fatalf("custom save %d %s, path %s", got.StatusCode, got.Body, path)
	}
}

func TestAdminConfigRoutesAreAuthenticatedOnly(t *testing.T) {
	mustConfigure(t, testConfig)
	for _, route := range []string{RouteAdminConfig, RouteAdminAPIKeys} {
		if got := resource(t, base+route, nil, nil); got.StatusCode != 404 {
			t.Fatalf("resource route %s exposes config: %d", route, got.StatusCode)
		}
		if got := adminCall(t, http.MethodPost, managementBasePath+route, nil); got.StatusCode != 405 {
			t.Fatalf("POST %s returned %d", route, got.StatusCode)
		}
	}
}

func TestAllPagesAllowSameOriginEmbedding(t *testing.T) {
	mustConfigure(t, testConfig)
	for _, route := range []string{RouteAdmin, RoutePortal, RouteMonitor} {
		got := resource(t, base+route, nil, nil)
		if got.StatusCode != 200 || got.Headers.Get("X-Frame-Options") != "SAMEORIGIN" ||
			!strings.Contains(got.Headers.Get("Content-Security-Policy"), "frame-ancestors 'self'") {
			t.Errorf("%s embedding headers: %d %v", route, got.StatusCode, got.Headers)
		}
	}
}
