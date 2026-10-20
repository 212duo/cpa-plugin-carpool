package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Stale cooldown watcher.
//
// CPA never probes a credential while it is cooling down, and the quota headers
// of upstream responses do not clear a cooldown either. When OpenAI resets a
// quota early, CPA keeps the seat account in cooldown until the scheduled
// recovery time, and since carpool is exclusive and fail-closed the buyer gets
// rejected all along. The upstream maintainers declined to change this
// (CLIProxyAPI #6655, #5639, #5611, #5277, #5404) and suggest calling the
// cooldown reset from outside. This watcher does that for seat accounts, only
// when the live upstream quota proves the cooldown is stale.

const (
	defaultUsedPercentThreshold = 90
	defaultCheckInterval        = time.Minute
	minCheckInterval            = time.Minute
	defaultMinClearInterval     = 10 * time.Minute
	defaultMaxClearsPerWindow   = 2
	quotaWindowLength           = 5 * time.Hour
)

// autoClearSettings is the validated stale cooldown configuration.
type autoClearSettings struct {
	Enabled       bool
	Threshold     float64
	CheckInterval time.Duration
	MinInterval   time.Duration
	MaxPerWindow  int
	// Warning is logged on reconfigure, e.g. when check-interval was raised to 1m.
	Warning string
}

// compileAutoClear validates the stale cooldown settings. Values are checked even
// while the feature is off, so turning it on cannot surface a latent typo.
func compileAutoClear(cfg PluginConfig) (autoClearSettings, error) {
	s := autoClearSettings{
		Enabled:       cfg.AutoClearStaleCooldown,
		Threshold:     defaultUsedPercentThreshold,
		CheckInterval: defaultCheckInterval,
		MinInterval:   defaultMinClearInterval,
		MaxPerWindow:  defaultMaxClearsPerWindow,
	}
	if cfg.UsedPercentThreshold != 0 {
		if cfg.UsedPercentThreshold < 0 || cfg.UsedPercentThreshold > 100 {
			return s, fmt.Errorf("used-percent-threshold must be between 1 and 100")
		}
		s.Threshold = cfg.UsedPercentThreshold
	}
	if v := strings.TrimSpace(cfg.CheckInterval); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return s, fmt.Errorf("check-interval must be a positive duration such as 1m or 90s")
		}
		if d < minCheckInterval {
			s.Warning = fmt.Sprintf("check-interval %s is below the 1m minimum, using 1m", d)
			d = minCheckInterval
		}
		s.CheckInterval = d
	}
	if v := strings.TrimSpace(cfg.MinInterval); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return s, fmt.Errorf("min-interval must be a positive duration such as 10m")
		}
		s.MinInterval = d
	}
	if cfg.MaxPerWindow < 0 {
		return s, fmt.Errorf("max-per-window must not be negative")
	}
	if cfg.MaxPerWindow > 0 {
		s.MaxPerWindow = cfg.MaxPerWindow
	}
	return s, nil
}

// Cooldown reasons as far as the plugin can tell from the runtime status message.
// host.auth.list exposes neither CPA's quota reason nor the last error, so the
// status message, which CPA sets from them, is all there is.
const (
	reasonQuota             = "quota"
	reasonCodexModelSupport = "codex_model_support"
	reasonAuth              = "auth"
	reasonOther             = "other"
)

// authFailurePatterns mark a credential problem. A bare "401" is deliberately
// absent: raw quota errors carry reset timestamps that may contain those digits.
var authFailurePatterns = []string{
	"unauthorized", "invalid_grant", "invalid grant", "token expired",
	"token_expired", "token is expired", "revoked", "refresh token",
	"authentication token", "token_invalidated", "invalidated", "deactivated",
}

var quotaPatterns = []string{"usage_limit", "credential_quota", "quota"}

// codexAccountModelPattern is the Codex 400 that CPA files under model support
// and cools the model down for 12h. During a quota storm the upstream sends it
// for models the account normally serves.
const codexAccountModelPattern = "not supported when using codex with a chatgpt account"

// classifyCooldown maps CPA's status message to a cooldown reason. Auth failures
// win over everything else, and anything unrecognized is reasonOther, which is
// never cleared automatically.
func classifyCooldown(statusMessage string) string {
	msg := strings.ToLower(strings.TrimSpace(statusMessage))
	if msg == "" {
		return reasonOther
	}
	for _, p := range authFailurePatterns {
		if strings.Contains(msg, p) {
			return reasonAuth
		}
	}
	if strings.Contains(msg, codexAccountModelPattern) {
		return reasonCodexModelSupport
	}
	for _, p := range quotaPatterns {
		if strings.Contains(msg, p) {
			return reasonQuota
		}
	}
	return reasonOther
}

// clearable reports whether a cooldown with this reason may be cleared once the
// upstream quota has recovered.
func clearable(reason string) bool {
	return reason == reasonQuota || reason == reasonCodexModelSupport
}

// maskAccount shortens an auth ID for logs so it identifies the file to the owner
// without spelling out the email.
func maskAccount(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.LastIndexAny(id, `/\`); i >= 0 {
		id = id[i+1:]
	}
	id = strings.TrimSuffix(strings.TrimSuffix(id, ".json"), ".JSON")
	r := []rune(id)
	var out string
	switch {
	case len(r) == 0:
		return "***"
	case len(r) <= 12:
		out = string(r[:min(2, len(r))]) + "***"
	default:
		out = string(r[:6]) + "***" + string(r[len(r)-5:])
	}
	return strings.ReplaceAll(out, "@", "*")
}

// cooldownDeps are the host and upstream calls of the watcher; tests replace them.
type cooldownDeps struct {
	listAuths func() ([]pluginapi.HostAuthFileEntry, error)
	// quota reads the live upstream quota of one auth.
	quota func(authIndex, userAgent string) (upstreamQuota, error)
	reset func(authIndex string) error
	now   func() time.Time
	logf  func(level, format string, args ...any)
}

func hostCooldownDeps() cooldownDeps {
	return cooldownDeps{
		listAuths: listAuths,
		quota: func(authIndex, userAgent string) (upstreamQuota, error) {
			creds, err := loadCredentials(authIndex)
			if err != nil {
				return upstreamQuota{}, err
			}
			usage, err := fetchUpstreamUsage(creds, userAgent)
			if err != nil {
				return upstreamQuota{}, err
			}
			return parseUpstreamQuota(usage)
		},
		reset: resetCooldown,
		now:   time.Now,
		logf:  logf,
	}
}

// clearHistory tracks the automatic clears of one auth.
type clearHistory struct {
	last      time.Time
	windowEnd time.Time // end of the 5h window the count belongs to
	count     int
}

type cooldownWatcher struct {
	deps cooldownDeps

	mu      sync.Mutex
	history map[string]*clearHistory // by auth index
	last    *AutoClearEvent

	runMu sync.Mutex // one tick at a time

	loopMu   sync.Mutex
	stop     chan struct{}
	interval time.Duration
}

func newCooldownWatcher(deps cooldownDeps) *cooldownWatcher {
	return &cooldownWatcher{deps: deps, history: make(map[string]*clearHistory)}
}

// autoClear is the plugin's stale cooldown watcher.
var autoClear = newCooldownWatcher(hostCooldownDeps())

// autoClearFor returns the effective settings of a routing table; the watcher
// only runs for a valid, enabled configuration that turned it on.
func autoClearFor(table *routingTable) (autoClearSettings, bool) {
	if table == nil || table.configErr != nil || !table.config.Enabled || !table.autoClear.Enabled {
		return autoClearSettings{}, false
	}
	return table.autoClear, true
}

// apply starts, restarts or stops the background loop for a new routing table.
// It does not wait for a running tick to finish, so a reconfigure never blocks on
// upstream calls; runMu keeps an old and a new loop from ticking at once, and
// every tick reads the current table.
func (w *cooldownWatcher) apply(table *routingTable) {
	settings, on := autoClearFor(table)
	w.loopMu.Lock()
	defer w.loopMu.Unlock()
	if w.stop != nil && (!on || w.interval != settings.CheckInterval) {
		close(w.stop)
		w.stop = nil
	}
	if !on || w.stop != nil {
		return
	}
	if settings.Warning != "" {
		w.deps.logf("warn", "auto-clear-stale-cooldown: %s", settings.Warning)
	}
	w.stop, w.interval = make(chan struct{}), settings.CheckInterval
	go w.loop(w.stop, settings.CheckInterval)
}

// shutdown stops the background loop.
func (w *cooldownWatcher) shutdown() {
	w.apply(nil)
}

func (w *cooldownWatcher) running() bool {
	w.loopMu.Lock()
	defer w.loopMu.Unlock()
	return w.stop != nil
}

func (w *cooldownWatcher) loop(stop chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			select {
			case <-stop: // stopped while waiting for the tick
				return
			default:
			}
			w.tick(activeTable.Load())
		}
	}
}

// watchedAuth is one seat account to check, deduplicated by auth index.
type watchedAuth struct {
	entry pluginapi.HostAuthFileEntry
	seats []string
}

// tick checks every primary account of the active seats once.
func (w *cooldownWatcher) tick(table *routingTable) {
	settings, on := autoClearFor(table)
	if !on {
		return
	}
	if !w.runMu.TryLock() {
		return
	}
	defer w.runMu.Unlock()

	now := w.deps.now()
	files, err := w.deps.listAuths()
	if err != nil {
		w.deps.logf("warn", "auto-clear-stale-cooldown: cannot list auths: %v", err)
		return
	}
	byIndex := make(map[string]*watchedAuth)
	for _, s := range table.seats {
		if s.user.Paused || s.expired(now) {
			continue
		}
		for _, want := range s.matchIDs {
			for _, entry := range files {
				if normalizeAuthID(entry.ID) != want || entry.AuthIndex == "" {
					continue
				}
				wa := byIndex[entry.AuthIndex]
				if wa == nil {
					wa = &watchedAuth{entry: entry}
					byIndex[entry.AuthIndex] = wa
				}
				if !containsString(wa.seats, s.user.Name) {
					wa.seats = append(wa.seats, s.user.Name)
				}
			}
		}
	}
	indexes := make([]string, 0, len(byIndex))
	for idx := range byIndex {
		indexes = append(indexes, idx)
	}
	sort.Strings(indexes)
	for _, idx := range indexes {
		w.check(byIndex[idx], settings, table.config.UpstreamUserAgent, now)
	}
}

// check clears one account's cooldown when every condition holds.
func (w *cooldownWatcher) check(wa *watchedAuth, settings autoClearSettings, userAgent string, now time.Time) {
	entry := wa.entry
	if !supportsUpstreamQuota(entry) || entry.Disabled || strings.EqualFold(entry.Status, "disabled") {
		return
	}
	// A cooldown has a recovery time; unavailable without one is a terminal state
	// such as a revoked token, which a reset must not paper over.
	if !entry.NextRetryAfter.After(now) {
		return
	}
	reason := classifyCooldown(entry.StatusMessage)
	if !clearable(reason) {
		return
	}
	if !w.allowed(entry.AuthIndex, settings, now) {
		return
	}
	quota, err := w.deps.quota(entry.AuthIndex, userAgent)
	if err != nil {
		w.deps.logf("warn", "auto-clear-stale-cooldown: seat %s auth_index %s: upstream quota query failed, cooldown kept: %v",
			strings.Join(wa.seats, ","), entry.AuthIndex, err)
		return
	}
	if !quota.recovered(settings.Threshold) {
		return
	}
	if err := w.deps.reset(entry.AuthIndex); err != nil {
		w.deps.logf("warn", "auto-clear-stale-cooldown: seat %s auth_index %s: reset failed: %v",
			strings.Join(wa.seats, ","), entry.AuthIndex, err)
		return
	}
	quotaCache.drop(entry.AuthIndex)
	rejectNotices.drop()
	event := AutoClearEvent{
		At:                now,
		Seats:             append([]string(nil), wa.seats...),
		AuthIndex:         entry.AuthIndex,
		Account:           maskAccount(entry.ID),
		UsedPercent:       quota.Short.UsedPercent,
		Reason:            reason,
		PreviousRecoverAt: entry.NextRetryAfter,
	}
	w.record(event, quota, now)
	weekly := "n/a"
	if quota.Weekly != nil {
		weekly = fmt.Sprintf("%g%%", quota.Weekly.UsedPercent)
	}
	w.deps.logf("info", "auto-clear-stale-cooldown: cleared cooldown of seat %s account %s auth_index %s (reason %s, upstream 5h used %g%%, weekly used %s, previous next_recover_at %s)",
		strings.Join(wa.seats, ","), event.Account, entry.AuthIndex, reason, quota.Short.UsedPercent, weekly, entry.NextRetryAfter.Format(time.RFC3339))
}

// allowed applies the debounce and the per-window cap before any upstream call.
func (w *cooldownWatcher) allowed(authIndex string, settings autoClearSettings, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	h := w.history[authIndex]
	if h == nil {
		return true
	}
	if now.Sub(h.last) < settings.MinInterval {
		return false
	}
	if now.Before(h.windowEnd) && h.count >= settings.MaxPerWindow {
		return false
	}
	return true
}

func (w *cooldownWatcher) record(event AutoClearEvent, quota upstreamQuota, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	h := w.history[event.AuthIndex]
	if h == nil {
		h = &clearHistory{}
		w.history[event.AuthIndex] = h
	}
	if !now.Before(h.windowEnd) {
		// A new 5h window: it ends at the upstream reset time when known.
		h.count = 0
		h.windowEnd = now.Add(quotaWindowLength)
		if quota.Short.ResetAt > 0 {
			end := time.Unix(quota.Short.ResetAt, 0)
			if end.After(now) && !end.After(now.Add(quotaWindowLength)) {
				h.windowEnd = end
			}
		}
	}
	h.count++
	h.last = now
	e := event
	w.last = &e
}

// lastClearOf returns the latest automatic clear of one auth.
func (w *cooldownWatcher) lastClearOf(authIndex string) *time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if h := w.history[authIndex]; h != nil && !h.last.IsZero() {
		t := h.last
		return &t
	}
	return nil
}

func (w *cooldownWatcher) overview(table *routingTable) AutoClearOverview {
	_, on := autoClearFor(table)
	w.mu.Lock()
	defer w.mu.Unlock()
	out := AutoClearOverview{Enabled: on}
	if w.last != nil {
		e := *w.last
		e.Seats = append([]string(nil), w.last.Seats...)
		out.Last = &e
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Buyer-facing rejection details.

// rejectNoticeTTL bounds host.auth.list calls while a cooling seat keeps getting
// requests.
const rejectNoticeTTL = 5 * time.Second

type noticeCache struct {
	mu      sync.Mutex
	entries map[string]cachedNotice
}

type cachedNotice struct {
	text string
	at   time.Time
}

var rejectNotices = &noticeCache{entries: make(map[string]cachedNotice)}

func (c *noticeCache) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cachedNotice)
}

// seatRejectReason explains to a buyer why their seat cannot take the request:
// a sanitized cooldown category and the expected recovery time. It never
// includes the account ID, email, token or CPA's raw status message.
func seatRejectReason(s *seat, now time.Time) string {
	rejectNotices.mu.Lock()
	if e, ok := rejectNotices.entries[s.user.Name]; ok && now.Sub(e.at) < rejectNoticeTTL && now.Sub(e.at) >= 0 {
		rejectNotices.mu.Unlock()
		return e.text
	}
	rejectNotices.mu.Unlock()

	text := buildSeatRejectReason(s, now)
	rejectNotices.mu.Lock()
	rejectNotices.entries[s.user.Name] = cachedNotice{text: text, at: now}
	rejectNotices.mu.Unlock()
	return text
}

func buildSeatRejectReason(s *seat, now time.Time) string {
	generic := fmt.Sprintf("carpool: your dedicated seat (%s) is currently in cooldown or unavailable / 你的专属席位（%s）暂时不可用", s.user.Name, s.user.Name)
	files, err := listAuths()
	if err != nil {
		return generic
	}
	var found bool
	var best pluginapi.HostAuthFileEntry
	for _, want := range s.matchIDs {
		for _, entry := range files {
			if normalizeAuthID(entry.ID) != want {
				continue
			}
			if !found {
				best, found = entry, true
				continue
			}
			// Prefer the cooling account that recovers first.
			if entry.NextRetryAfter.After(now) && (!best.NextRetryAfter.After(now) || entry.NextRetryAfter.Before(best.NextRetryAfter)) {
				best = entry
			}
		}
	}
	if !found {
		return fmt.Sprintf("carpool: the account of your dedicated seat (%s) is not loaded in the gateway, please contact the host / 你的专属席位（%s）的账号未在网关中加载，请联系车主", s.user.Name, s.user.Name)
	}
	if best.Disabled {
		return fmt.Sprintf("carpool: the account of your dedicated seat (%s) is disabled, please contact the host / 你的专属席位（%s）的账号已停用，请联系车主", s.user.Name, s.user.Name)
	}
	if !best.NextRetryAfter.After(now) {
		if best.Unavailable && classifyCooldown(best.StatusMessage) == reasonAuth {
			return fmt.Sprintf("carpool: the account of your dedicated seat (%s) needs the host to re-authorize it / 你的专属席位（%s）的账号凭证失效，需要车主处理", s.user.Name, s.user.Name)
		}
		return generic + "; only this seat is affected, not the whole gateway / 仅影响你的席位，并非网关整体故障"
	}
	en, zh := reasonLabels(classifyCooldown(best.StatusMessage))
	at := best.NextRetryAfter.In(time.Local)
	remaining := formatRemaining(best.NextRetryAfter.Sub(now))
	return fmt.Sprintf("carpool: your dedicated seat (%s) is cooling down: %s; expected to recover at %s (in %s). Only this seat is affected, not the whole gateway. / 你的专属席位（%s）正在冷却：%s，预计 %s 恢复（约 %s 后）；仅影响你的席位，并非网关整体故障",
		s.user.Name, en, at.Format(time.RFC3339), remaining,
		s.user.Name, zh, at.Format("2006-01-02 15:04 (UTC-07:00)"), remaining)
}

func reasonLabels(reason string) (string, string) {
	switch reason {
	case reasonQuota:
		return "upstream quota exhausted", "上游额度用尽"
	case reasonCodexModelSupport:
		return "upstream temporarily rejected the model for this account", "上游暂时拒绝该账号使用此模型"
	case reasonAuth:
		return "account credentials need the host's attention", "账号凭证失效，需要车主处理"
	default:
		return "temporary upstream error", "上游临时错误"
	}
}

func formatRemaining(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}
