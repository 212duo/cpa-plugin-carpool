package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const limitsConfig = `
enabled: true
max-concurrency: 2
rpm: 3
users:
  - name: Limited
    api-keys: ["sk-alice", "sk-alice-2"]
    primary: ["codex-alice@example.com-team.json"]
  - name: Custom
    api-keys: ["sk-bob"]
    primary: ["codex-bob@example.com-team.json"]
    max-concurrency: 1
    rpm: 0
`

func resetLimiter(t *testing.T) {
	t.Helper()
	limiter.reset()
	t.Cleanup(limiter.reset)
}

func intercept(t *testing.T, requestID string, headers map[string][]string) pluginapi.RequestInterceptResponse {
	t.Helper()
	raw, err := json.Marshal(pluginapi.RequestInterceptRequest{
		RequestID: requestID,
		Headers:   headers,
		Body:      []byte(`{"model":"gpt-test","input":"hi"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := handleMethod(pluginabi.MethodRequestInterceptBefore, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("bad envelope %s: %v", out, err)
	}
	var resp pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func completeReq(t *testing.T, requestID string) {
	t.Helper()
	raw, _ := json.Marshal(pluginapi.RequestCompletion{RequestID: requestID, Outcome: pluginapi.RequestCompletionSucceeded})
	out, err := handleMethod(pluginabi.MethodRequestComplete, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("bad envelope %s: %v", out, err)
	}
}

func seatNamed(t *testing.T, name string) *seat {
	t.Helper()
	for _, s := range activeTable.Load().seats {
		if s.user.Name == name {
			return s
		}
	}
	t.Fatalf("no seat %s", name)
	return nil
}

func TestSeatLimitsInheritAndOverride(t *testing.T) {
	mustConfigure(t, limitsConfig)
	if err := activeTable.Load().configErr; err != nil {
		t.Fatal(err)
	}
	if s := seatNamed(t, "Limited"); s.maxConcurrency != 2 || s.rpm != 3 {
		t.Fatalf("Limited: concurrency %d rpm %d, want the defaults 2 and 3", s.maxConcurrency, s.rpm)
	}
	// An explicit 0 overrides the default with "unlimited".
	if s := seatNamed(t, "Custom"); s.maxConcurrency != 1 || s.rpm != 0 {
		t.Fatalf("Custom: concurrency %d rpm %d, want 1 and 0", s.maxConcurrency, s.rpm)
	}
}

func TestLimitConfigValidation(t *testing.T) {
	cases := map[string]string{
		"negative default":    "enabled: true\nrpm: -1\nusers:\n  - api-keys: [sk-a]\n    primary: [a.json]\n",
		"negative seat limit": "enabled: true\nusers:\n  - api-keys: [sk-a]\n    primary: [a.json]\n    max-concurrency: -2\n",
		"duplicate names":     "enabled: true\nusers:\n  - name: A\n    api-keys: [sk-a]\n    primary: [a.json]\n  - name: A\n    api-keys: [sk-b]\n    primary: [b.json]\n",
		"default name clash":  "enabled: true\nusers:\n  - api-keys: [sk-a]\n    primary: [a.json]\n  - name: seat-1\n    api-keys: [sk-b]\n    primary: [b.json]\n",
		"bad base url":        "enabled: true\npublic-base-url: api.example.com\nusers:\n  - api-keys: [sk-a]\n    primary: [a.json]\n",
		"base url with query": "enabled: true\npublic-base-url: https://api.example.com/?k=1\nusers:\n  - api-keys: [sk-a]\n    primary: [a.json]\n",
	}
	for name, yamlText := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseConfig([]byte(yamlText)); err == nil {
				t.Fatal("expected a config error")
			}
		})
	}

	_, table, err := parseConfig([]byte("enabled: true\npublic-base-url: https://api.example.com/cpa/\nusers:\n  - api-keys: [sk-a]\n    primary: [a.json]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if table.publicBaseURL != "https://api.example.com/cpa" {
		t.Fatalf("publicBaseURL = %q", table.publicBaseURL)
	}
}

func TestConcurrencyLimitIsSharedBySeatKeys(t *testing.T) {
	mustConfigure(t, limitsConfig)
	resetLimiter(t)

	if resp := intercept(t, "r1", bearer("sk-alice")); resp.Terminate {
		t.Fatalf("first request rejected: %s", resp.ResponseBody)
	}
	if resp := intercept(t, "r2", map[string][]string{"X-Api-Key": {"sk-alice-2"}}); resp.Terminate {
		t.Fatalf("second request rejected: %s", resp.ResponseBody)
	}
	// The same request intercepted again does not take another slot.
	if resp := intercept(t, "r2", bearer("sk-alice-2")); resp.Terminate {
		t.Fatal("re-intercepted request was rejected")
	}

	resp := intercept(t, "r3", bearer("sk-alice"))
	if !resp.Terminate || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third concurrent request: %+v, want 429", resp)
	}
	if resp.ResponseHeaders.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.ResponseBody, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "seat_concurrency_limit" || body.Error.Type != "rate_limit_error" {
		t.Fatalf("unexpected error body %s", resp.ResponseBody)
	}
	if strings.Contains(string(resp.ResponseBody), "sk-alice") {
		t.Fatalf("rejection leaks the key: %s", resp.ResponseBody)
	}

	// Another seat is not affected.
	if resp := intercept(t, "b1", bearer("sk-bob")); resp.Terminate {
		t.Fatal("other seat was limited")
	}

	completeReq(t, "r1")
	completeReq(t, "r1") // a duplicate completion must not free a second slot
	if resp := intercept(t, "r4", bearer("sk-alice")); resp.Terminate {
		t.Fatalf("request after a completion rejected: %s", resp.ResponseBody)
	}
	if resp := intercept(t, "r5", bearer("sk-alice")); !resp.Terminate {
		t.Fatal("duplicate completion freed an extra slot")
	}
}

func TestRPMLimitSlidingWindow(t *testing.T) {
	mustConfigure(t, "enabled: true\nusers:\n  - name: R\n    api-keys: [sk-r]\n    primary: [a.json]\n    rpm: 2\n")
	resetLimiter(t)
	s := seatNamed(t, "R")

	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if d := limiter.acquire("a", s, true, t0); !d.allowed {
		t.Fatal("first request rejected")
	}
	if d := limiter.acquire("b", s, true, t0.Add(20*time.Second)); !d.allowed {
		t.Fatal("second request rejected")
	}
	d := limiter.acquire("c", s, true, t0.Add(30*time.Second))
	if d.allowed || d.code != "seat_rpm_limit" {
		t.Fatalf("third request in the window: %+v", d)
	}
	if d.retryAfter != 30*time.Second {
		t.Fatalf("retryAfter = %v, want 30s (until the first request leaves the window)", d.retryAfter)
	}
	// Completing requests does not reset the RPM window.
	limiter.release("a")
	limiter.release("b")
	if d := limiter.acquire("d", s, true, t0.Add(59*time.Second)); d.allowed {
		t.Fatal("request admitted before the window moved")
	}
	if d := limiter.acquire("e", s, true, t0.Add(60*time.Second)); !d.allowed {
		t.Fatal("request rejected after the first one left the window")
	}

	limiter.reset()
	for i := 0; i < 2; i++ {
		intercept(t, fmt.Sprint("x", i), bearer("sk-r"))
	}
	resp := intercept(t, "x9", bearer("sk-r"))
	if !resp.Terminate || resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(resp.ResponseBody), "seat_rpm_limit") {
		t.Fatalf("rpm rejection through the interceptor: %+v %s", resp, resp.ResponseBody)
	}
}

func TestStaleRequestsFreeTheirSlot(t *testing.T) {
	mustConfigure(t, "enabled: true\nusers:\n  - name: S\n    api-keys: [sk-s]\n    primary: [a.json]\n    max-concurrency: 1\n")
	resetLimiter(t)
	s := seatNamed(t, "S")

	t0 := time.Now()
	if d := limiter.acquire("lost", s, true, t0); !d.allowed {
		t.Fatal("first request rejected")
	}
	if d := limiter.acquire("next", s, true, t0.Add(time.Minute)); d.allowed {
		t.Fatal("second concurrent request admitted")
	}
	// request.complete for "lost" never arrives.
	if d := limiter.acquire("later", s, true, t0.Add(inflightTTL+time.Minute)); !d.allowed {
		t.Fatal("slot of a request without completion was never freed")
	}
}

func TestInterceptPassesThroughUnmanagedRequests(t *testing.T) {
	mustConfigure(t, "enabled: true\nmax-concurrency: 1\nusers:\n  - api-keys: [sk-a]\n    primary: [a.json]\n")
	resetLimiter(t)

	for i := 0; i < 3; i++ {
		if resp := intercept(t, fmt.Sprint("owner", i), bearer("sk-owner")); resp.Terminate || len(resp.Body) != 0 || len(resp.Headers) != 0 {
			t.Fatalf("unmanaged key was touched: %+v", resp)
		}
	}

	mustConfigure(t, "enabled: true\nusers: [\n")
	if resp := intercept(t, "x", bearer("sk-a")); resp.Terminate {
		t.Fatal("interceptor rejected while the config is invalid; the scheduler does that")
	}

	out, err := handleMethod(pluginabi.MethodRequestInterceptAfter, []byte(`{"RequestID":"x"}`))
	if err != nil || !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("intercept_after = %s, %v", out, err)
	}
}

func TestLegacyHostLimitsRPMOnly(t *testing.T) {
	raw, _ := json.Marshal(lifecycleRequest{
		ConfigYAML:    []byte("enabled: true\nusers:\n  - api-keys: [sk-a]\n    primary: [a.json]\n    max-concurrency: 1\n    rpm: 3\n"),
		SchemaVersion: 1,
	})
	configure(raw)
	t.Cleanup(func() { legacyHost.Store(false) })
	resetLimiter(t)

	for i := 0; i < 3; i++ {
		if resp := intercept(t, fmt.Sprint("r", i), bearer("sk-a")); resp.Terminate {
			t.Fatalf("request %d rejected although the host cannot report completion", i)
		}
	}
	if resp := intercept(t, "r9", bearer("sk-a")); !resp.Terminate || !strings.Contains(string(resp.ResponseBody), "seat_rpm_limit") {
		t.Fatalf("rpm not enforced on a legacy host: %+v", resp)
	}
}

func TestStatusReportsLimitsAndGuideFields(t *testing.T) {
	mustConfigure(t, strings.Replace(limitsConfig, "enabled: true", "enabled: true\npublic-base-url: https://api.example.com", 1))
	installFakeHost(t)
	installFakeUpstream(t)
	resetLimiter(t)

	intercept(t, "r1", bearer("sk-alice"))
	intercept(t, "r2", bearer("sk-alice"))
	completeReq(t, "r2")

	got := resource(t, base+"/api/status", nil, keyHeader("sk-alice"))
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", got.StatusCode, got.Body)
	}
	var resp BuyerStatusResponse
	if err := json.Unmarshal(got.Body, &resp); err != nil {
		t.Fatal(err)
	}
	want := SeatLimits{MaxConcurrency: 2, RPM: 3, Active: 1, RecentRequests: 2}
	if resp.Limits != want {
		t.Fatalf("limits = %+v, want %+v", resp.Limits, want)
	}
	if resp.Provider != "codex" || resp.PublicBaseURL != "https://api.example.com" {
		t.Fatalf("provider %q, public base url %q", resp.Provider, resp.PublicBaseURL)
	}
}

func register(t *testing.T, yamlText string) registrationPayload {
	t.Helper()
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(yamlText), SchemaVersion: PluginSchemaVersion})
	out, err := handleMethod(pluginabi.MethodPluginReconfigure, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("bad envelope %s: %v", out, err)
	}
	var reg registrationPayload
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRequestHooksFollowLimits(t *testing.T) {
	resetLimiter(t)

	reg := register(t, limitsConfig)
	if reg.SchemaVersion < 2 || reg.SchemaVersion > pluginabi.SchemaVersion {
		t.Fatalf("schema version %d: request.complete needs >= 2, the SDK supports <= %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	caps := reg.Capabilities
	if !caps.Scheduler || !caps.RequestInterceptor || !caps.RequestLifecyclePlugin || !caps.ManagementAPI {
		t.Fatalf("limited config: capabilities %+v", caps)
	}
	intercept(t, "r1", bearer("sk-alice"))

	// Without limits the hooks would only add a plugin call to every request.
	caps = register(t, testConfig).Capabilities
	if caps.RequestInterceptor || caps.RequestLifecyclePlugin || !caps.Scheduler || !caps.ManagementAPI {
		t.Fatalf("unlimited config: capabilities %+v", caps)
	}
	// request.complete for r1 will never arrive now, so its slot must not linger.
	if active, _ := limiter.usage("TeamA-1", time.Now()); active != 0 {
		t.Fatal("counters survived unregistering the hooks")
	}
	if active, recent := limiter.usage("Limited", time.Now()); active != 0 || recent != 0 {
		t.Fatalf("counters survived unregistering the hooks: %d active, %d recent", active, recent)
	}

	if caps := register(t, "enabled: true\nusers: [\n").Capabilities; caps.RequestInterceptor || !caps.Scheduler {
		t.Fatalf("invalid config: capabilities %+v", caps)
	}
}
