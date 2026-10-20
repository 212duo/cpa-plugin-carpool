package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const autoClearConfig = `
enabled: true
auto-clear-stale-cooldown: true
users:
  - name: Seat-A
    api-keys: ["sk-a"]
    primary: ["codex-a1b2c3d4-alice@example.com-team.json"]
  - name: Seat-B
    api-keys: ["sk-b"]
    primary: ["codex-a1b2c3d4-alice@example.com-team"]
  - name: Seat-C
    api-keys: ["sk-c"]
    primary: ["codex-e5f6-carol@example.com-plus.json"]
    paused: true
  - name: Seat-D
    api-keys: ["sk-d"]
    primary: ["claude-dave@example.com.json"]
`

// cooldownEnv is a scripted host and upstream for the watcher.
type cooldownEnv struct {
	mu         sync.Mutex
	now        time.Time
	entries    []pluginapi.HostAuthFileEntry
	quota      map[string]upstreamQuota
	quotaErr   error
	quotaCalls int
	listCalls  int
	resets     []string
	logs       []string
	// recool puts the account back into cooldown right after a reset, to
	// exercise the debounce and the per-window cap.
	recool bool
}

func newCooldownEnv(t *testing.T) (*cooldownEnv, *cooldownWatcher) {
	t.Helper()
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	env := &cooldownEnv{
		now: now,
		entries: []pluginapi.HostAuthFileEntry{
			{ID: "codex-a1b2c3d4-alice@example.com-team.json", AuthIndex: "1", Type: "codex", Unavailable: true,
				StatusMessage: "quota exhausted", NextRetryAfter: now.Add(3 * time.Hour)},
			{ID: "codex-e5f6-carol@example.com-plus.json", AuthIndex: "2", Type: "codex", Unavailable: true,
				StatusMessage: "quota exhausted", NextRetryAfter: now.Add(3 * time.Hour)},
			{ID: "claude-dave@example.com.json", AuthIndex: "3", Type: "claude", Unavailable: true,
				StatusMessage: "quota exhausted", NextRetryAfter: now.Add(3 * time.Hour)},
			{ID: "codex-unbound@example.com.json", AuthIndex: "4", Type: "codex", Unavailable: true,
				StatusMessage: "quota exhausted", NextRetryAfter: now.Add(3 * time.Hour)},
		},
		quota: map[string]upstreamQuota{},
	}
	for _, idx := range []string{"1", "2", "3", "4"} {
		env.quota[idx] = upstreamQuota{Short: quotaWindow{UsedPercent: 0, ResetAt: now.Add(4 * time.Hour).Unix()}, Weekly: &quotaWindow{UsedPercent: 30}}
	}
	w := newCooldownWatcher(cooldownDeps{
		listAuths: func() ([]pluginapi.HostAuthFileEntry, error) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.listCalls++
			return append([]pluginapi.HostAuthFileEntry(nil), env.entries...), nil
		},
		quota: func(authIndex, _ string) (upstreamQuota, error) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.quotaCalls++
			if env.quotaErr != nil {
				return upstreamQuota{}, env.quotaErr
			}
			return env.quota[authIndex], nil
		},
		reset: func(authIndex string) error {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.resets = append(env.resets, authIndex)
			if !env.recool {
				for i := range env.entries {
					if env.entries[i].AuthIndex == authIndex {
						env.entries[i].Unavailable = false
						env.entries[i].NextRetryAfter = time.Time{}
						env.entries[i].StatusMessage = ""
					}
				}
			}
			return nil
		},
		now: func() time.Time {
			env.mu.Lock()
			defer env.mu.Unlock()
			return env.now
		},
		logf: func(level, format string, args ...any) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.logs = append(env.logs, level+": "+fmt.Sprintf(format, args...))
		},
	})
	return env, w
}

func (e *cooldownEnv) set(f func(e *cooldownEnv)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f(e)
}

func (e *cooldownEnv) resetCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.resets)
}

func compileTable(t *testing.T, yamlText string) *routingTable {
	t.Helper()
	_, table, err := parseConfig([]byte(yamlText))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(table.monitor.close)
	return table
}

func TestAutoClearClearsRecoveredQuotaOnce(t *testing.T) {
	env, w := newCooldownEnv(t)
	table := compileTable(t, autoClearConfig)

	w.tick(table)
	// Seat-A and Seat-B share auth 1: one reset. Paused Seat-C, the Claude seat
	// and the unbound account are never touched.
	if len(env.resets) != 1 || env.resets[0] != "1" {
		t.Fatalf("resets = %v, want [1]", env.resets)
	}
	var info string
	for _, l := range env.logs {
		if strings.HasPrefix(l, "info: ") {
			info = l
		}
	}
	for _, want := range []string{"Seat-A,Seat-B", "codex-***-team", "auth_index 1", "5h used 0%", "previous next_recover_at 2026-10-20T18:00:00Z"} {
		if !strings.Contains(info, want) {
			t.Fatalf("info log %q lacks %q", info, want)
		}
	}
	if strings.Contains(info, "alice@") || strings.Contains(info, "example.com") {
		t.Fatalf("info log leaks the account email: %q", info)
	}
	if got := w.lastClearOf("1"); got == nil || !got.Equal(env.now) {
		t.Fatalf("lastClearOf = %v", got)
	}
	ov := w.overview(table)
	if !ov.Enabled || ov.Last == nil || ov.Last.AuthIndex != "1" || ov.Last.UsedPercent != 0 || ov.Last.Account != "codex-***-team" {
		t.Fatalf("overview = %+v", ov)
	}

	// The cooldown is gone, so later ticks leave the account alone.
	env.set(func(e *cooldownEnv) { e.now = e.now.Add(time.Hour) })
	w.tick(table)
	if env.resetCount() != 1 {
		t.Fatalf("resets = %v after the cooldown was cleared", env.resets)
	}
}

func TestAutoClearKeepsGenuineCooldowns(t *testing.T) {
	cases := map[string]func(e *cooldownEnv){
		// A real fresh 429 shows a full window upstream.
		"5h used 100%": func(e *cooldownEnv) { q := e.quota["1"]; q.Short.UsedPercent = 100; e.quota["1"] = q },
		"5h at threshold": func(e *cooldownEnv) {
			q := e.quota["1"]
			q.Short.UsedPercent = 90
			e.quota["1"] = q
		},
		"weekly used up": func(e *cooldownEnv) { q := e.quota["1"]; q.Weekly = &quotaWindow{UsedPercent: 100}; e.quota["1"] = q },
		"limit reached":  func(e *cooldownEnv) { q := e.quota["1"]; q.LimitReached = true; e.quota["1"] = q },
		"query failed":   func(e *cooldownEnv) { e.quotaErr = errors.New("GET /usage: HTTP 502") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env, w := newCooldownEnv(t)
			env.set(mutate)
			w.tick(compileTable(t, autoClearConfig))
			if env.resetCount() != 0 {
				t.Fatalf("resets = %v, want none", env.resets)
			}
			if env.quotaCalls == 0 {
				t.Fatal("the upstream quota was never queried")
			}
		})
	}
}

func TestAutoClearQueryFailureIsLogged(t *testing.T) {
	env, w := newCooldownEnv(t)
	env.quotaErr = errors.New("GET /usage: HTTP 401")
	w.tick(compileTable(t, autoClearConfig))
	if env.resetCount() != 0 || len(env.logs) != 1 || !strings.Contains(env.logs[0], "upstream quota query failed, cooldown kept") {
		t.Fatalf("resets = %v, logs = %q", env.resets, env.logs)
	}
}

func TestAutoClearNeverClearsAuthFailures(t *testing.T) {
	messages := []string{
		"unauthorized",
		"unauthorized (refresh token invalid)",
		"invalid_grant",
		"token expired",
		`{"error":{"code":"token_invalidated","message":"Your authentication token has been invalidated. Please try signing in again."}}`,
		"payment_required",
		"transient upstream error",
		"",
	}
	for _, msg := range messages {
		t.Run(msg, func(t *testing.T) {
			env, w := newCooldownEnv(t)
			env.set(func(e *cooldownEnv) { e.entries[0].StatusMessage = msg })
			w.tick(compileTable(t, autoClearConfig))
			if env.resetCount() != 0 || env.quotaCalls != 0 {
				t.Fatalf("resets = %v, quota calls = %d, want none", env.resets, env.quotaCalls)
			}
		})
	}

	t.Run("unavailable without a recovery time", func(t *testing.T) {
		env, w := newCooldownEnv(t)
		env.set(func(e *cooldownEnv) { e.entries[0].NextRetryAfter = time.Time{} })
		w.tick(compileTable(t, autoClearConfig))
		if env.resetCount() != 0 {
			t.Fatalf("resets = %v", env.resets)
		}
	})
	t.Run("disabled account", func(t *testing.T) {
		env, w := newCooldownEnv(t)
		env.set(func(e *cooldownEnv) { e.entries[0].Disabled = true })
		w.tick(compileTable(t, autoClearConfig))
		if env.resetCount() != 0 {
			t.Fatalf("resets = %v", env.resets)
		}
	})
}

func TestAutoClearQuotaReasons(t *testing.T) {
	messages := []string{
		"quota exhausted",
		"credential_quota", // restored from save-cooldown-status
		`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":1761401234}}`,
		// Codex 400 that CPA cools down for 12h as model support during a quota storm.
		`{"detail":"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account."}`,
	}
	for _, msg := range messages {
		t.Run(msg, func(t *testing.T) {
			env, w := newCooldownEnv(t)
			env.set(func(e *cooldownEnv) { e.entries[0].StatusMessage = msg })
			w.tick(compileTable(t, autoClearConfig))
			if env.resetCount() != 1 {
				t.Fatalf("resets = %v, want one", env.resets)
			}
		})
	}

	t.Run("model support with quota still used up", func(t *testing.T) {
		env, w := newCooldownEnv(t)
		env.set(func(e *cooldownEnv) {
			e.entries[0].StatusMessage = messages[3]
			q := e.quota["1"]
			q.Short.UsedPercent = 100
			e.quota["1"] = q
		})
		w.tick(compileTable(t, autoClearConfig))
		if env.resetCount() != 0 {
			t.Fatalf("resets = %v", env.resets)
		}
	})
}

func TestAutoClearDebounceAndWindowCap(t *testing.T) {
	env, w := newCooldownEnv(t)
	env.recool = true
	table := compileTable(t, autoClearConfig)
	start := env.now
	step := func(offset time.Duration, want int) {
		t.Helper()
		env.set(func(e *cooldownEnv) {
			e.now = start.Add(offset)
			e.entries[0].NextRetryAfter = e.now.Add(3 * time.Hour)
		})
		w.tick(table)
		if got := env.resetCount(); got != want {
			t.Fatalf("at +%s: %d resets, want %d", offset, got, want)
		}
	}
	step(0, 1)
	step(5*time.Minute, 1)  // min-interval 10m
	step(10*time.Minute, 2) // second clear of this window
	step(30*time.Minute, 2) // max-per-window 2
	step(3*time.Hour, 2)
	// The window ends at the upstream reset time (start+4h); a new one starts.
	step(4*time.Hour, 3)
	step(4*time.Hour+5*time.Minute, 3)
	if env.quotaCalls != 3 {
		t.Fatalf("quota calls = %d: blocked ticks must not reach the upstream", env.quotaCalls)
	}
}

func TestAutoClearCustomLimits(t *testing.T) {
	env, w := newCooldownEnv(t)
	env.recool = true
	table := compileTable(t, autoClearConfig+"min-interval: 1m\nmax-per-window: 1\nused-percent-threshold: 50\n")
	env.set(func(e *cooldownEnv) { q := e.quota["1"]; q.Short.UsedPercent = 60; e.quota["1"] = q })
	w.tick(table)
	if env.resetCount() != 0 {
		t.Fatalf("resets = %v above the custom threshold", env.resets)
	}
	env.set(func(e *cooldownEnv) { q := e.quota["1"]; q.Short.UsedPercent = 40; e.quota["1"] = q })
	w.tick(table)
	env.set(func(e *cooldownEnv) { e.now = e.now.Add(2 * time.Minute) })
	w.tick(table)
	if env.resetCount() != 1 {
		t.Fatalf("resets = %v, want 1 with max-per-window 1", env.resets)
	}
}

func TestAutoClearOffDoesNothing(t *testing.T) {
	off := strings.Replace(autoClearConfig, "auto-clear-stale-cooldown: true", "auto-clear-stale-cooldown: false", 1)
	for name, yamlText := range map[string]string{
		"switch off":       off,
		"switch omitted":   strings.Replace(autoClearConfig, "auto-clear-stale-cooldown: true\n", "", 1),
		"plugin disabled":  strings.Replace(autoClearConfig, "enabled: true", "enabled: false", 1),
		"no configuration": "",
	} {
		t.Run(name, func(t *testing.T) {
			env, w := newCooldownEnv(t)
			table := compileTable(t, yamlText)
			w.apply(table)
			if w.running() {
				t.Fatal("the watcher loop started")
			}
			w.tick(table)
			if env.listCalls != 0 || env.quotaCalls != 0 || env.resetCount() != 0 || len(env.logs) != 0 {
				t.Fatalf("watcher ran: list=%d quota=%d resets=%v logs=%q", env.listCalls, env.quotaCalls, env.resets, env.logs)
			}
		})
	}

	t.Run("config error", func(t *testing.T) {
		env, w := newCooldownEnv(t)
		w.tick(&routingTable{configErr: errors.New("broken")})
		w.tick(nil)
		if env.listCalls != 0 {
			t.Fatal("watcher ran on a broken configuration")
		}
	})
}

func TestAutoClearLoopLifecycle(t *testing.T) {
	_, w := newCooldownEnv(t)
	on := compileTable(t, autoClearConfig)
	w.apply(on)
	if !w.running() {
		t.Fatal("loop not started")
	}
	w.apply(compileTable(t, autoClearConfig+"check-interval: 2m\n"))
	if !w.running() || w.interval != 2*time.Minute {
		t.Fatalf("loop not restarted with the new interval: %s", w.interval)
	}
	w.apply(compileTable(t, strings.Replace(autoClearConfig, "auto-clear-stale-cooldown: true", "auto-clear-stale-cooldown: false", 1)))
	if w.running() {
		t.Fatal("loop still running after the switch was turned off")
	}
	w.apply(on)
	w.shutdown()
	if w.running() {
		t.Fatal("loop still running after shutdown")
	}
}

func TestAutoClearConfigValidation(t *testing.T) {
	table := compileTable(t, autoClearConfig)
	s := table.autoClear
	if !s.Enabled || s.Threshold != 90 || s.CheckInterval != time.Minute || s.MinInterval != 10*time.Minute || s.MaxPerWindow != 2 || s.Warning != "" {
		t.Fatalf("defaults = %+v", s)
	}
	table = compileTable(t, autoClearConfig+"check-interval: 10s\n")
	if table.autoClear.CheckInterval != time.Minute || table.autoClear.Warning == "" {
		t.Fatalf("check-interval below 1m not raised: %+v", table.autoClear)
	}
	for name, extra := range map[string]string{
		"bad check-interval": "check-interval: soon\n",
		"bad min-interval":   "min-interval: -1m\n",
		"threshold above":    "used-percent-threshold: 150\n",
		"threshold negative": "used-percent-threshold: -1\n",
		"negative cap":       "max-per-window: -1\n",
		"misspelled key":     "auto_clear_stale_cooldown: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseConfig([]byte(autoClearConfig + extra)); err == nil {
				t.Fatal("expected a config error")
			}
		})
	}
}

func TestAutoClearKeepsConfigRevision(t *testing.T) {
	// Configurations that do not use the feature keep the revision of 0.6.0, so
	// an upgrade does not look like a concurrent edit to an open admin page.
	cfg := PluginConfig{Enabled: true}
	raw, _ := json.Marshal(cfg)
	if strings.Contains(string(raw), "auto-clear") || strings.Contains(string(raw), "interval") {
		t.Fatalf("unused settings are serialized: %s", raw)
	}
}

func TestParseUpstreamQuota(t *testing.T) {
	var usage map[string]any
	_ = json.Unmarshal([]byte(`{"email":"x@example.com","rate_limit":{"allowed":true,"limit_reached":false,
		"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_at":1900000000},
		"secondary_window":{"used_percent":55,"limit_window_seconds":604800}}}`), &usage)
	q, err := parseUpstreamQuota(usage)
	if err != nil || q.Short.UsedPercent != 12 || q.Short.ResetAt != 1900000000 || q.Weekly == nil || q.Weekly.UsedPercent != 55 || !q.recovered(90) {
		t.Fatalf("q = %+v, err = %v", q, err)
	}

	_ = json.Unmarshal([]byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800},
		"secondary_window":{"used_percent":3,"limit_window_seconds":18000}}}`), &usage)
	if q, err = parseUpstreamQuota(usage); err != nil || q.Short.UsedPercent != 3 || q.Weekly.UsedPercent != 100 || q.recovered(90) {
		t.Fatalf("swapped windows: q = %+v, err = %v", q, err)
	}

	for _, body := range []string{`{}`, `{"rate_limit":{}}`, `{"rate_limit":{"primary_window":{"reset_at":1}}}`, `{"detail":"Unauthorized"}`} {
		usage = nil
		_ = json.Unmarshal([]byte(body), &usage)
		if _, err := parseUpstreamQuota(usage); err == nil {
			t.Fatalf("%s: expected an error", body)
		}
	}
}

func TestHostQuotaDependency(t *testing.T) {
	installFakeHost(t)
	installFakeUpstream(t)
	deps := hostCooldownDeps()

	q, err := deps.quota("1", "")
	if err != nil || q.Short.UsedPercent != 42 {
		t.Fatalf("alice: q = %+v, err = %v", q, err)
	}
	// The fake upstream answers 401 to any other token: a failure, not a recovery.
	if _, err := deps.quota("9", ""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("malice: err = %v, want HTTP 401", err)
	}
	if _, err := deps.quota("404", ""); err == nil {
		t.Fatal("missing credentials: expected an error")
	}
}

func TestRejectReasonExplainsCooldownWithoutSecrets(t *testing.T) {
	host := installFakeHost(t)
	rejectNotices.drop()
	t.Cleanup(rejectNotices.drop)
	mustConfigure(t, testConfig)

	recoverAt := time.Now().Add(2*time.Hour + 30*time.Minute)
	host.mu.Lock()
	host.entries[1].Unavailable = true
	host.entries[1].NextRetryAfter = recoverAt
	host.entries[1].StatusMessage = `{"error":{"type":"usage_limit_reached","message":"limit for owner@example.com, token tok-alice"}}`
	host.mu.Unlock()

	resp := pick(t, pluginapi.SchedulerOptions{Headers: bearer("sk-alice")}, "codex-other@example.com.json")
	if !resp.Reject || resp.RejectCode != "auth_unavailable" {
		t.Fatalf("got %+v, want auth_unavailable reject", resp)
	}
	reason := resp.RejectReason
	for _, want := range []string{"TeamA-1", "cooling down", "upstream quota exhausted", "上游额度用尽", recoverAt.In(time.Local).Format(time.RFC3339), "2h30m", "not the whole gateway"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}
	for _, leak := range []string{"owner@example.com", "tok-alice", "alice@", "usage_limit_reached", "example.com", "Codex-Alice"} {
		if strings.Contains(strings.ToLower(reason), strings.ToLower(leak)) {
			t.Fatalf("reason %q leaks %q", reason, leak)
		}
	}

	// Cached for a few seconds: no host.auth.list per rejected request.
	host.mu.Lock()
	host.entries[1].StatusMessage = "unauthorized"
	host.mu.Unlock()
	if again := pick(t, pluginapi.SchedulerOptions{Headers: bearer("sk-alice")}).RejectReason; again != reason {
		t.Fatalf("reason changed within the cache window: %q", again)
	}

	rejectNotices.drop()
	host.mu.Lock()
	host.entries[1].NextRetryAfter = time.Time{}
	host.mu.Unlock()
	reason = pick(t, pluginapi.SchedulerOptions{Headers: bearer("sk-alice")}).RejectReason
	if !strings.Contains(reason, "re-authorize") || strings.Contains(reason, "alice") {
		t.Fatalf("auth failure reason = %q", reason)
	}
}

func TestRejectReasonWithoutHost(t *testing.T) {
	old := hostCaller
	hostCaller = nil
	t.Cleanup(func() { hostCaller = old })
	rejectNotices.drop()
	t.Cleanup(rejectNotices.drop)
	mustConfigure(t, testConfig)
	resp := pick(t, pluginapi.SchedulerOptions{Headers: bearer("sk-alice")})
	if !resp.Reject || !strings.Contains(resp.RejectReason, "TeamA-1") || !strings.Contains(resp.RejectReason, "cooldown or unavailable") {
		t.Fatalf("got %+v", resp)
	}
}

func TestStatusShowsLastAutoClear(t *testing.T) {
	installFakeHost(t)
	installFakeUpstream(t)
	mustConfigure(t, testConfig)
	old := autoClear
	autoClear = newCooldownWatcher(hostCooldownDeps())
	t.Cleanup(func() { autoClear = old })

	got := resource(t, base+"/api/status", nil, keyHeader("sk-alice"))
	if got.StatusCode != http.StatusOK || strings.Contains(string(got.Body), "last_auto_clear_at") {
		t.Fatalf("status before any clear: %d %s", got.StatusCode, got.Body)
	}
	at := time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)
	autoClear.record(AutoClearEvent{AuthIndex: "1", Seats: []string{"TeamA-1"}}, upstreamQuota{}, at)
	got = resource(t, base+"/api/status", nil, keyHeader("sk-alice"))
	var resp BuyerStatusResponse
	if err := json.Unmarshal(got.Body, &resp); err != nil || resp.LastAutoClearAt == nil || !resp.LastAutoClearAt.Equal(at) {
		t.Fatalf("status = %s, err = %v", got.Body, err)
	}
}

func TestMaskAccount(t *testing.T) {
	cases := map[string]string{
		"codex-042c0005-alice@example.com-team.json": "codex-***-team",
		"team/codex-bob@example.com.json":            "codex-***e.com",
		"a@b.json":                                   "a****",
		"":                                           "***",
	}
	for in, want := range cases {
		if got := maskAccount(in); got != want || strings.Contains(got, "@") {
			t.Fatalf("maskAccount(%q) = %q, want %q", in, got, want)
		}
	}
}
