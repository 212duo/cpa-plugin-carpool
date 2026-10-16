package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// testConfig mirrors what CPA sends on plugin.register: only the plugin's own
// section of config.yaml, with enabled/priority injected by the host.
const testConfig = `
enabled: true
priority: 100
store:
  version: "0.2.0"
upstream-user-agent: test-agent/1.0
users:
  - name: TeamA-1
    api-keys: ["sk-alice"]
    primary: ["Codex-Alice@Example.com-team.json"]
  - name: TeamA-2
    api-keys: ["sk-bob"]
    primary: ["codex-bob@example.com-team"]
    expire-at: "2020-01-01T00:00:00+08:00"
  - name: Claude-1
    api-keys: ["sk-carol"]
    primary: ["claude-carol@example.com.json"]
`

const base = "/v0/resource/plugins/cpa-carpool"

func mustConfigure(t *testing.T, yamlText string) {
	t.Helper()
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte(yamlText), SchemaVersion: PluginSchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	configure(raw)
}

func bearer(key string) map[string][]string {
	return map[string][]string{"Authorization": {"Bearer " + key}}
}

func pick(t *testing.T, opts pluginapi.SchedulerOptions, candidates ...string) pluginapi.SchedulerPickResponse {
	t.Helper()
	req := pluginapi.SchedulerPickRequest{Options: opts}
	for _, id := range candidates {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id})
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pickAuth(raw, activeTable.Load())
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("bad envelope %s: %v", out, err)
	}
	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func resource(t *testing.T, path string, query url.Values, headers http.Header) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: path, Query: query, Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	out, err := handleResource(raw, activeTable.Load(), *activeConfig.Load())
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("bad envelope %s: %v", out, err)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func keyHeader(key string) http.Header {
	return http.Header{"X-Carpool-Key": {key}}
}

// fakeHost stands in for the CPA host callbacks.
type fakeHost struct {
	mu       sync.Mutex
	entries  []pluginapi.HostAuthFileEntry
	creds    map[string]string // auth index -> credential JSON
	cooling  map[string]bool   // auth index -> held back by CPA
	resetErr bool
	resets   []string
	logs     []string
}

func installFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	f := &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{
			// Listed first to prove matching is exact, not by suffix.
			{ID: "codex-malice@example.com-team.json", AuthIndex: "9", Type: "codex"},
			// CPA lowercases IDs on Windows; the config spells it in mixed case.
			{ID: "codex-alice@example.com-team.json", AuthIndex: "1", Type: "codex"},
			{ID: "claude-carol@example.com.json", AuthIndex: "3", Type: "claude"},
		},
		creds: map[string]string{
			"1": `{"access_token":"tok-alice","account_id":"acct-alice","email":"owner@example.com"}`,
			"9": `{"access_token":"tok-malice","account_id":"acct-malice"}`,
		},
		cooling: map[string]bool{},
	}
	old := hostCaller
	hostCaller = f.call
	t.Cleanup(func() { hostCaller = old })
	return f
}

func (f *fakeHost) call(method string, request []byte) ([]byte, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req struct {
		AuthIndex string `json:"auth_index"`
		Message   string `json:"message"`
	}
	_ = json.Unmarshal(request, &req)

	ok := func(v any) ([]byte, int) {
		out, _ := okEnvelope(v)
		return out, 0
	}
	switch method {
	case pluginabi.MethodHostLog:
		f.logs = append(f.logs, req.Message)
		return ok(map[string]any{})
	case pluginabi.MethodHostAuthList:
		return ok(map[string]any{"files": f.entries})
	case pluginabi.MethodHostAuthGet:
		creds, found := f.creds[req.AuthIndex]
		if !found {
			return errorEnvelope("not_found", "auth not found"), 1
		}
		return ok(pluginapi.HostAuthGetResponse{AuthIndex: req.AuthIndex, JSON: json.RawMessage(creds)})
	case pluginabi.MethodHostAuthGetRuntime:
		entry := pluginapi.HostAuthFileEntry{AuthIndex: req.AuthIndex}
		if f.cooling[req.AuthIndex] {
			entry.NextRetryAfter = time.Now().Add(time.Hour)
		}
		return ok(pluginapi.HostAuthGetRuntimeResponse{Auth: entry})
	case pluginabi.MethodHostRoutingResetCooldown:
		if f.resetErr {
			return errorEnvelope("reset_failed", "manager unavailable"), 1
		}
		f.resets = append(f.resets, req.AuthIndex)
		delete(f.cooling, req.AuthIndex)
		return ok(pluginapi.HostRoutingResetCooldownResponse{AuthIndex: req.AuthIndex})
	}
	return errorEnvelope("unknown_method", method), 1
}

// fakeUpstream stands in for the ChatGPT quota API.
type fakeUpstream struct {
	mu            sync.Mutex
	hits          map[string]int
	userAgents    []string
	creditStatus  string
	consumeStatus int
}

func installFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{hits: map[string]int{}, creditStatus: "available", consumeStatus: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.hits[r.URL.Path]++
		u.userAgents = append(u.userAgents, r.UserAgent())
		if r.Header.Get("Authorization") != "Bearer tok-alice" || r.Header.Get("ChatGPT-Account-Id") != "acct-alice" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/usage":
			_, _ = w.Write([]byte(`{"email":"owner@example.com","account_id":"acct-alice","rate_limit":{"primary_window":{"used_percent":42,"reset_at":1900000000}}}`))
		case "/rate-limit-reset-credits":
			_, _ = w.Write([]byte(`{"available_count":1,"credits":[{"id":"credit-1","status":"` + u.creditStatus + `","title":"Reset"}]}`))
		case "/rate-limit-reset-credits/consume":
			w.WriteHeader(u.consumeStatus)
			if u.consumeStatus != http.StatusOK {
				_, _ = w.Write([]byte(`{"detail":"account owner@example.com is not eligible"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	oldURL, oldCache := upstreamBaseURL, quotaCache
	upstreamBaseURL = srv.URL
	quotaCache = &upstreamCache{entries: make(map[string]cachedUpstream)}
	t.Cleanup(func() {
		srv.Close()
		upstreamBaseURL, quotaCache = oldURL, oldCache
	})
	return u
}

func (u *fakeUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func TestConfigureReadsPluginSection(t *testing.T) {
	mustConfigure(t, testConfig)

	table := activeTable.Load()
	if table.configErr != nil {
		t.Fatalf("unexpected config error: %v", table.configErr)
	}
	if len(table.byKey) != 3 {
		t.Fatalf("got %d keys, want 3", len(table.byKey))
	}
	if got := activeConfig.Load().UpstreamUserAgent; got != "test-agent/1.0" {
		t.Fatalf("upstream-user-agent = %q", got)
	}
}

func TestInvalidConfigRejectsEveryone(t *testing.T) {
	cases := map[string]string{
		"misspelled key":   "enabled: true\nusers:\n  - name: a\n    api_keys: [sk-alice]\n    primary: [a.json]\n",
		"removed auth-dir": "enabled: true\nauth-dir: /root/.cli-proxy-api\nusers:\n  - api-keys: [sk-alice]\n    primary: [a.json]\n",
		"bad expire-at":    "enabled: true\nusers:\n  - name: a\n    api-keys: [sk-alice]\n    primary: [a.json]\n    expire-at: 2026-11-15 23:59\n",
		"duplicate key":    "enabled: true\nusers:\n  - api-keys: [sk-alice]\n    primary: [a.json]\n  - api-keys: [sk-alice]\n    primary: [b.json]\n",
		"no primary":       "enabled: true\nusers:\n  - api-keys: [sk-alice]\n",
		"no api keys":      "enabled: true\nusers:\n  - primary: [a.json]\n",
		"broken yaml":      "enabled: true\nusers: [\n",
	}
	for name, yamlText := range cases {
		t.Run(name, func(t *testing.T) {
			mustConfigure(t, yamlText)
			if activeTable.Load().configErr == nil {
				t.Fatal("expected a config error")
			}
			for _, key := range []string{"sk-alice", "sk-owner"} {
				resp := pick(t, pluginapi.SchedulerOptions{Headers: bearer(key)}, "a.json")
				if !resp.Handled || !resp.Reject || resp.RejectCode != "carpool_config_invalid" {
					t.Fatalf("key %s: got %+v, want carpool_config_invalid reject", key, resp)
				}
			}
			if got := resource(t, base+"/api/status", nil, keyHeader("sk-alice")); got.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status endpoint returned %d, want 503", got.StatusCode)
			}
		})
	}

	t.Run("broken lifecycle request", func(t *testing.T) {
		configure([]byte("{"))
		if activeTable.Load().configErr == nil {
			t.Fatal("expected a config error")
		}
	})
}

func TestInvalidConfigIsLoggedToHost(t *testing.T) {
	host := installFakeHost(t)
	mustConfigure(t, "enabled: true\nusers: [\n")

	if len(host.logs) != 1 || !strings.Contains(host.logs[0], "invalid plugin configuration") {
		t.Fatalf("host logs = %q", host.logs)
	}
}

func TestDisabledPluginPassesThrough(t *testing.T) {
	mustConfigure(t, strings.Replace(testConfig, "enabled: true", "enabled: false", 1))

	if resp := pick(t, pluginapi.SchedulerOptions{Headers: bearer("sk-alice")}, "codex-alice@example.com-team.json"); resp.Handled {
		t.Fatalf("disabled plugin handled the request: %+v", resp)
	}
}

func TestPickAuth(t *testing.T) {
	mustConfigure(t, testConfig)

	const alice = "codex-alice@example.com-team.json"
	const lookalike = "codex-malice@example.com-team.json"

	tests := []struct {
		name       string
		opts       pluginapi.SchedulerOptions
		candidates []string
		wantAuth   string
		wantReject string
		wantPass   bool
	}{
		{name: "exact match ignores case", opts: pluginapi.SchedulerOptions{Headers: bearer("sk-alice")}, candidates: []string{lookalike, alice}, wantAuth: alice},
		{name: "case-preserved candidate", opts: pluginapi.SchedulerOptions{Headers: bearer("sk-alice")}, candidates: []string{"Codex-Alice@Example.com-team.json"}, wantAuth: "Codex-Alice@Example.com-team.json"},
		{name: "lookalike never matches", opts: pluginapi.SchedulerOptions{Headers: bearer("sk-alice")}, candidates: []string{lookalike}, wantReject: "auth_unavailable"},
		{name: "caller scope metadata", opts: pluginapi.SchedulerOptions{Metadata: map[string]any{"caller_scope": callerScope("sk-alice")}}, candidates: []string{alice}, wantAuth: alice},
		{name: "x-api-key header", opts: pluginapi.SchedulerOptions{Headers: map[string][]string{"X-Api-Key": {"sk-alice"}}}, candidates: []string{alice}, wantAuth: alice},
		{name: "expired seat", opts: pluginapi.SchedulerOptions{Headers: bearer("sk-bob")}, candidates: []string{"codex-bob@example.com-team.json"}, wantReject: "seat_expired"},
		{name: "unmanaged key", opts: pluginapi.SchedulerOptions{Headers: bearer("sk-owner")}, candidates: []string{alice}, wantPass: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := pick(t, tt.opts, tt.candidates...)
			switch {
			case tt.wantPass:
				if resp.Handled {
					t.Fatalf("got %+v, want unhandled", resp)
				}
			case tt.wantReject != "":
				if !resp.Handled || !resp.Reject || resp.RejectCode != tt.wantReject {
					t.Fatalf("got %+v, want reject %s", resp, tt.wantReject)
				}
			default:
				if !resp.Handled || resp.Reject || resp.AuthID != tt.wantAuth {
					t.Fatalf("got %+v, want auth %s", resp, tt.wantAuth)
				}
			}
		})
	}
}

func TestResourceSubpath(t *testing.T) {
	tests := map[string]string{
		"/v0/resource/plugins/cpa-carpool/portal":     RoutePortal,
		"/v0/resource/plugins/cpa-carpool/api/status": RouteAPIStatus,
		"/v0/resource/plugins/carpool/api/reset":      RouteAPIReset,
		"/v0/resource/plugins/cpa-carpool":            "/",
		"/portal":                                     RoutePortal,
	}
	for in, want := range tests {
		if got := resourceSubpath(in); got != want {
			t.Errorf("resourceSubpath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestManagementRegisterListsAllPortalRoutes(t *testing.T) {
	out, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	var reg pluginapi.ManagementRegistrationResponse
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range reg.Resources {
		got[r.Path] = true
	}
	for _, p := range []string{RoutePortal, RouteAPIStatus, RouteAPIReset} {
		if !got[p] {
			t.Errorf("resource %s is not registered", p)
		}
	}
}

func TestResourceErrors(t *testing.T) {
	mustConfigure(t, testConfig)

	tests := []struct {
		name    string
		path    string
		query   url.Values
		headers http.Header
		want    int
	}{
		{name: "portal page", path: base + "/portal", want: http.StatusOK},
		{name: "unknown path", path: base + "/api/nope", want: http.StatusNotFound},
		{name: "status without key", path: base + "/api/status", want: http.StatusBadRequest},
		{name: "status with unknown key", path: base + "/api/status", headers: keyHeader("sk-owner"), want: http.StatusNotFound},
		{name: "status for expired seat", path: base + "/api/status", query: url.Values{"key": {"sk-bob"}}, want: http.StatusForbidden},
		{name: "reset without confirm", path: base + "/api/reset", headers: keyHeader("sk-alice"), want: http.StatusBadRequest},
		{name: "reset for expired seat", path: base + "/api/reset", query: url.Values{"confirm": {"1"}}, headers: keyHeader("sk-bob"), want: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resource(t, tt.path, tt.query, tt.headers); got.StatusCode != tt.want {
				t.Fatalf("status %d (%s), want %d", got.StatusCode, got.Body, tt.want)
			}
		})
	}
}

func TestStatusReadsSeatThroughHostAndCachesUpstream(t *testing.T) {
	mustConfigure(t, testConfig)
	host := installFakeHost(t)
	host.cooling["1"] = true
	upstream := installFakeUpstream(t)

	for i := 0; i < 3; i++ {
		got := resource(t, base+"/api/status", nil, keyHeader("sk-alice"))
		if got.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", got.StatusCode, got.Body)
		}
		body := string(got.Body)
		for _, leak := range []string{"owner@example.com", "acct-alice", "tok-alice"} {
			if strings.Contains(body, leak) {
				t.Fatalf("response leaks %q: %s", leak, body)
			}
		}
		var resp BuyerStatusResponse
		if err := json.Unmarshal(got.Body, &resp); err != nil {
			t.Fatal(err)
		}
		if !resp.QuotaSupported || !resp.IsCooling || resp.CreditsAvailable != 1 || resp.SeatName != "TeamA-1" {
			t.Fatalf("unexpected status %+v", resp)
		}
	}

	if n := upstream.count("/usage"); n != 1 {
		t.Fatalf("upstream /usage hit %d times, want 1 (cached)", n)
	}
	if ua := upstream.userAgents[0]; ua != "test-agent/1.0" {
		t.Fatalf("user agent = %q", ua)
	}
}

func TestStatusForNonCodexSeatSkipsUpstream(t *testing.T) {
	mustConfigure(t, testConfig)
	installFakeHost(t)
	upstream := installFakeUpstream(t)

	got := resource(t, base+"/api/status", nil, keyHeader("sk-carol"))
	var resp BuyerStatusResponse
	if err := json.Unmarshal(got.Body, &resp); err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != http.StatusOK || resp.QuotaSupported {
		t.Fatalf("status %d, %+v", got.StatusCode, resp)
	}
	if n := upstream.count("/usage"); n != 0 {
		t.Fatalf("upstream hit %d times for a non-Codex seat", n)
	}
	if got := resource(t, base+"/api/reset", url.Values{"confirm": {"1"}}, keyHeader("sk-carol")); got.StatusCode != http.StatusBadRequest {
		t.Fatalf("reset for non-Codex seat returned %d", got.StatusCode)
	}
}

func resetResult(t *testing.T, got pluginapi.ManagementResponse) ResetResponse {
	t.Helper()
	var resp ResetResponse
	if err := json.Unmarshal(got.Body, &resp); err != nil {
		t.Fatalf("decode %s: %v", got.Body, err)
	}
	return resp
}

func TestResetRedeemsCreditAndClearsCooldown(t *testing.T) {
	mustConfigure(t, testConfig)
	host := installFakeHost(t)
	host.cooling["1"] = true
	upstream := installFakeUpstream(t)

	// Warm the cache so we can check that a reset invalidates it.
	resource(t, base+"/api/status", nil, keyHeader("sk-alice"))

	got := resource(t, base+"/api/reset", url.Values{"confirm": {"1"}}, keyHeader("sk-alice"))
	if resp := resetResult(t, got); got.StatusCode != http.StatusOK || !resp.Success {
		t.Fatalf("reset returned %d: %s", got.StatusCode, got.Body)
	}
	if n := upstream.count("/rate-limit-reset-credits/consume"); n != 1 {
		t.Fatalf("consume hit %d times, want 1", n)
	}
	if len(host.resets) != 1 || host.resets[0] != "1" {
		t.Fatalf("cooldown resets = %v, want [1]", host.resets)
	}

	resource(t, base+"/api/status", nil, keyHeader("sk-alice"))
	if n := upstream.count("/usage"); n != 3 {
		t.Fatalf("/usage hit %d times, want 3 (status, reset, status after cache drop)", n)
	}
}

func TestResetReportsCooldownFailure(t *testing.T) {
	mustConfigure(t, testConfig)
	host := installFakeHost(t)
	host.resetErr = true
	installFakeUpstream(t)

	got := resource(t, base+"/api/reset", url.Values{"confirm": {"1"}}, keyHeader("sk-alice"))
	resp := resetResult(t, got)
	if got.StatusCode != http.StatusOK || !resp.Success || !strings.Contains(resp.Message, "冷却未能自动解除") {
		t.Fatalf("reset returned %d: %s", got.StatusCode, got.Body)
	}
}

func TestResetHidesUpstreamError(t *testing.T) {
	mustConfigure(t, testConfig)
	host := installFakeHost(t)
	upstream := installFakeUpstream(t)
	upstream.consumeStatus = http.StatusBadRequest

	got := resource(t, base+"/api/reset", url.Values{"confirm": {"1"}}, keyHeader("sk-alice"))
	if resp := resetResult(t, got); got.StatusCode != http.StatusBadGateway || resp.Success {
		t.Fatalf("reset returned %d: %s", got.StatusCode, got.Body)
	}
	if strings.Contains(string(got.Body), "owner@example.com") {
		t.Fatalf("upstream error leaked to buyer: %s", got.Body)
	}
	if len(host.resets) != 0 {
		t.Fatal("cooldown was cleared although the credit was not redeemed")
	}
	logged := strings.Join(host.logs, "\n")
	if !strings.Contains(logged, "owner@example.com") {
		t.Fatalf("upstream error missing from host log: %q", logged)
	}
}

func TestResetWithoutCredits(t *testing.T) {
	mustConfigure(t, testConfig)
	installFakeHost(t)
	upstream := installFakeUpstream(t)
	upstream.creditStatus = "redeemed"

	got := resource(t, base+"/api/reset", url.Values{"confirm": {"1"}}, keyHeader("sk-alice"))
	if got.StatusCode != http.StatusBadRequest {
		t.Fatalf("reset returned %d: %s", got.StatusCode, got.Body)
	}
	if n := upstream.count("/rate-limit-reset-credits/consume"); n != 0 {
		t.Fatalf("consume hit %d times without an available credit", n)
	}
}

func TestResetIsSerialized(t *testing.T) {
	mustConfigure(t, testConfig)
	installFakeHost(t)
	upstream := installFakeUpstream(t)

	resetMu.Lock()
	got := resource(t, base+"/api/reset", url.Values{"confirm": {"1"}}, keyHeader("sk-alice"))
	resetMu.Unlock()

	if got.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent reset returned %d, want 409", got.StatusCode)
	}
	if n := upstream.count("/rate-limit-reset-credits/consume"); n != 0 {
		t.Fatalf("consume hit %d times while another reset was running", n)
	}
}

func TestUsageKeyHashMatchesManagerPlus(t *testing.T) {
	// CPA Manager Plus stores hex(sha256(trimmed key)) in usage_events.api_key_hash;
	// the expected value was computed independently with Python's hashlib.
	const want = "099295a3784e1bd368dc348843a7398c1931b6b8ec2504c73e91ed2040bdc46c"
	if got := usageKeyHash("  sk-alice  "); got != want {
		t.Fatalf("usageKeyHash = %q, want %q", got, want)
	}
}

func TestReadTokenStatsFromManagerPlusDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Subset of the usage_events schema created by CPA Manager Plus.
	_, err = db.Exec(`
		create table usage_events (
			api_key_hash text,
			input_tokens integer not null default 0,
			output_tokens integer not null default 0,
			cached_tokens integer not null default 0,
			total_tokens integer not null default 0
		);
		insert into usage_events values (?, 100, 20, 50, 170), (?, 1000, 200, 0, 1200), (?, 7, 7, 7, 21);`,
		usageKeyHash("sk-alice"), usageKeyHash("sk-alice"), usageKeyHash("sk-bob"))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	got := readTokenStats(dbPath, usageKeyHash("sk-alice"))
	want := TokenStats{Calls: 2, InputTokens: 1100, OutputTokens: 220, CachedTokens: 50, TotalTokens: 1370}
	if got != want {
		t.Fatalf("readTokenStats = %+v, want %+v", got, want)
	}
	if got := readTokenStats(filepath.Join(t.TempDir(), "missing.sqlite"), usageKeyHash("sk-alice")); got != (TokenStats{}) {
		t.Fatalf("missing database returned %+v", got)
	}
}

func TestConfigExampleIsValid(t *testing.T) {
	raw, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Plugins struct {
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	section, ok := doc.Plugins.Configs["cpa-carpool"]
	if !ok {
		t.Fatal("config.example.yaml has no plugins.configs.cpa-carpool section")
	}
	// CPA passes exactly this subtree to the plugin.
	sectionYAML, err := yaml.Marshal(&section)
	if err != nil {
		t.Fatal(err)
	}
	cfg, table, err := parseConfig(sectionYAML)
	if err != nil {
		t.Fatalf("config.example.yaml is rejected: %v", err)
	}
	if !cfg.Enabled || len(table.byKey) != 3 {
		t.Fatalf("unexpected example config: enabled=%v keys=%d", cfg.Enabled, len(table.byKey))
	}
}
