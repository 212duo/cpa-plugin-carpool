package main

import (
	"encoding/json"
	"time"
)

// PluginVersion is the semantic release version of the carpool plugin.
const PluginVersion = "0.6.0"

// PluginSchemaVersion is the RPC contract version declared at plugin.register.
// Version 2 adds request.complete, which the concurrency limit needs; version 6
// keeps management JSON responses unescaped. Pinned rather than taken from the SDK
// so an SDK upgrade cannot silently opt into a newer contract.
const PluginSchemaVersion = 6

// PluginLogoURL is the logo shown by CPA's management clients and the plugin store.
const PluginLogoURL = "https://raw.githubusercontent.com/212duo/cpa-plugin-carpool/main/docs/images/logo.png"

// DefaultUpstreamUserAgent is sent to the ChatGPT quota endpoints unless
// upstream-user-agent overrides it.
const DefaultUpstreamUserAgent = "codex_cli_rs/0.160.0 (x86_64-unknown-linux-gnu)"

// Resource route paths under /v0/resource/plugins/<plugin-id>/
const (
	RoutePortal     = "/portal"
	RouteAPIStatus  = "/api/status"
	RouteAPIReset   = "/api/reset"
	RouteAdmin      = "/admin"
	RouteMonitor    = "/monitor"
	RouteAPIMonitor = "/api/monitor"
)

// Management API route paths under /v0/management/. CPA serves them only to
// callers holding the management key. Unlike resources they are not scoped by
// plugin ID, hence the fixed carpool prefix.
const (
	managementBasePath = "/v0/management"

	RouteAdminOverview    = "/carpool/overview"
	RouteAdminAuths       = "/carpool/auths"
	RouteAdminValidate    = "/carpool/validate"
	RouteAdminConfig      = "/carpool/config"
	RouteAdminAPIKeys     = "/carpool/api-keys"
	RouteAdminSave        = "/carpool/save"
	RouteAdminRetry       = "/carpool/save/retry"
	RouteAdminDiagnostics = "/carpool/diagnostics"
)

// Envelope is the standard JSON envelope exchanged with the CPA plugin ABI.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError represents an error payload in the standard envelope.
type EnvelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PluginConfig is the plugin's own section of CPA config.yaml. CPA passes only
// the plugins.configs.<plugin-id> subtree, and the plugin ID is the shared
// library file name without extension (cpa-carpool.so -> cpa-carpool):
//
//	plugins:
//	  configs:
//	    cpa-carpool:
//	      enabled: true
//	      priority: 100
//	      debug: false
//	      db-path: "/opt/cpa/manager-data/usage.sqlite" # optional, auto-detected
//	      upstream-user-agent: ""                       # optional
//	      public-base-url: "https://api.example.com"    # optional
//	      max-concurrency: 0                            # optional default per seat
//	      rpm: 0                                        # optional default per seat
//	      users: [...]
type PluginConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Debug   bool   `yaml:"debug" json:"debug"`
	DBPath  string `yaml:"db-path" json:"db-path"`
	// UpstreamUserAgent overrides DefaultUpstreamUserAgent for ChatGPT quota calls.
	UpstreamUserAgent string `yaml:"upstream-user-agent" json:"upstream-user-agent"`
	// PublicBaseURL is the address clients use to reach the gateway, shown in the
	// portal's client setup guide. Empty means the origin the portal was opened on.
	PublicBaseURL string `yaml:"public-base-url" json:"public-base-url"`
	// MaxConcurrency and RPM are the limits of seats that do not set their own;
	// 0 means unlimited.
	MaxConcurrency int          `yaml:"max-concurrency" json:"max-concurrency"`
	RPM            int          `yaml:"rpm" json:"rpm"`
	Users          []UserConfig `yaml:"users" json:"users"`

	// Priority and Store are read by CPA itself. They are declared only so that
	// strict decoding accepts them.
	Priority int `yaml:"priority" json:"-"`
	Store    any `yaml:"store" json:"-"`
}

// UserConfig binds a buyer's client API key to a specific, isolated upstream account.
type UserConfig struct {
	Name    string   `yaml:"name" json:"name"`
	APIKeys []string `yaml:"api-keys" json:"api-keys"`
	// Paused seats retain their keys and bindings and explicitly reject requests.
	Paused bool `yaml:"paused,omitempty" json:"paused,omitempty"`
	// Primary lists the exact CPA Auth IDs of the seat: the auth file path relative
	// to the CPA auth directory, e.g. "codex-042c0005-alice@example.com-team.json" or
	// "team/codex-....json". Matching is exact (case and the .json suffix are
	// ignored), so seats sharing an email never get mixed up.
	Primary []string `yaml:"primary" json:"primary"`
	// ExpireAt optionally specifies an RFC3339 timestamp (e.g. "2026-11-15T23:59:59+08:00")
	// after which the keys are rejected.
	ExpireAt string `yaml:"expire-at,omitempty" json:"expire-at,omitempty"`
	// MaxConcurrency and RPM limit the seat as a whole: all of its keys share them.
	// Unset inherits the top-level value; 0 means unlimited.
	MaxConcurrency *int `yaml:"max-concurrency,omitempty" json:"max-concurrency,omitempty"`
	RPM            *int `yaml:"rpm,omitempty" json:"rpm,omitempty"`
}

// TokenStats holds the detailed M-level token usage for an API key.
type TokenStats struct {
	Calls        int64 `json:"local_calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// ResetCreditItem represents one rate limit reset card.
type ResetCreditItem struct {
	ID         string  `json:"id,omitempty"`
	Title      string  `json:"title"`
	Status     string  `json:"status"` // "available" or "redeemed"
	GrantedAt  *string `json:"granted_at,omitempty"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
	RedeemedAt *string `json:"redeemed_at,omitempty"`
}

// SeatLimits reports a seat's request limits and its current use of them. Use is
// only counted while a seat has a limit or is paused, since then the request hooks
// registered.
type SeatLimits struct {
	MaxConcurrency int `json:"max_concurrency"` // 0 = unlimited
	RPM            int `json:"rpm"`             // 0 = unlimited
	Active         int `json:"active"`          // requests in flight
	RecentRequests int `json:"recent_requests"` // requests started in the last minute
}

// BuyerStatusResponse is the sanitized, zero-trust response returned to the buyer portal.
type BuyerStatusResponse struct {
	SeatName string `json:"seat_name"`
	// Provider is the seat account's type (codex, claude, ...), used by the portal
	// to suggest models in the client setup guide.
	Provider string `json:"provider,omitempty"`
	// PublicBaseURL is the configured gateway address for client setup; empty means
	// the portal's own origin.
	PublicBaseURL string     `json:"public_base_url,omitempty"`
	Limits        SeatLimits `json:"limits"`
	// QuotaSupported is false when the seat is not a Codex account; quota windows
	// and reset credits are then unavailable.
	QuotaSupported   bool              `json:"quota_supported"`
	UpstreamUsage    map[string]any    `json:"upstream_usage"`
	CreditsAvailable int               `json:"credits_available"`
	CreditsList      []ResetCreditItem `json:"credits_list"`
	UsageAvailable   bool              `json:"usage_available"`
	LocalCalls       int64             `json:"local_calls"`
	InputTokens      int64             `json:"input_tokens"`
	OutputTokens     int64             `json:"output_tokens"`
	CachedTokens     int64             `json:"cached_tokens"`
	TotalTokens      int64             `json:"total_tokens"`
	IsCooling        bool              `json:"is_cooling"`
	ExpiresAt        *time.Time        `json:"expires_at,omitempty"`
}

// AdminOverview is the runtime state shown on the admin page. The seats'
// configuration itself, keys included, is read from CPA's config API.
type AdminOverview struct {
	PluginVersion  string `json:"plugin_version"`
	ConfigRevision string `json:"config_revision"`
	Enabled        bool   `json:"enabled"`
	ConfigError    string `json:"config_error,omitempty"`
	// LegacyHost is true when the host cannot report request completion, so
	// max-concurrency is not enforced.
	LegacyHost bool `json:"legacy_host,omitempty"`
	// UsageDatabase reports whether the CPA Manager Plus usage database was found.
	UsageDatabase  bool        `json:"usage_database"`
	UsageAvailable bool        `json:"usage_available"`
	Seats          []AdminSeat `json:"seats"`
}

// AdminSeat is the runtime state of one compiled seat.
type AdminSeat struct {
	Name      string     `json:"name"`
	Paused    bool       `json:"paused"`
	Keys      int        `json:"keys"`
	Primary   []string   `json:"primary"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Expired   bool       `json:"expired"`
	Limits    SeatLimits `json:"limits"`
	// Auth is the seat account CPA currently has loaded, nil when none is.
	Auth  *AdminSeatAuth `json:"auth,omitempty"`
	Usage TokenStats     `json:"usage"`
}

// AdminSeatAuth describes the loaded account of a seat.
type AdminSeatAuth struct {
	ID          string `json:"id"`
	Provider    string `json:"provider,omitempty"`
	Status      string `json:"status,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
	Cooling     bool   `json:"cooling"`
}

// AdminAuth is one CPA account offered when binding a seat.
type AdminAuth struct {
	ID          string `json:"id"`
	Provider    string `json:"provider,omitempty"`
	Label       string `json:"label,omitempty"`
	Email       string `json:"email,omitempty"`
	Status      string `json:"status,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
	RuntimeOnly bool   `json:"runtime_only,omitempty"`
	// Seats lists the seats already bound to this account.
	Seats []string `json:"seats,omitempty"`
}

// ResetResponse is returned after a reset card redemption attempt.
type ResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}
