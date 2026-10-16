package main

import (
	"encoding/json"
	"time"
)

// PluginVersion is the semantic release version of the carpool plugin.
const PluginVersion = "0.2.0"

// DefaultUpstreamUserAgent is sent to the ChatGPT quota endpoints unless
// upstream-user-agent overrides it.
const DefaultUpstreamUserAgent = "codex_cli_rs/0.160.0 (x86_64-unknown-linux-gnu)"

// Resource route paths under /v0/resource/plugins/<plugin-id>/
const (
	RoutePortal    = "/portal"
	RouteAPIStatus = "/api/status"
	RouteAPIReset  = "/api/reset"
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
//	      users: [...]
type PluginConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Debug   bool   `yaml:"debug" json:"debug"`
	DBPath  string `yaml:"db-path" json:"db-path"`
	// UpstreamUserAgent overrides DefaultUpstreamUserAgent for ChatGPT quota calls.
	UpstreamUserAgent string       `yaml:"upstream-user-agent" json:"upstream-user-agent"`
	Users             []UserConfig `yaml:"users" json:"users"`

	// Priority and Store are read by CPA itself. They are declared only so that
	// strict decoding accepts them.
	Priority int `yaml:"priority" json:"-"`
	Store    any `yaml:"store" json:"-"`
}

// UserConfig binds a buyer's client API key to a specific, isolated upstream account.
type UserConfig struct {
	Name    string   `yaml:"name" json:"name"`
	APIKeys []string `yaml:"api-keys" json:"api-keys"`
	// Primary lists the exact CPA Auth IDs of the seat: the auth file path relative
	// to the CPA auth directory, e.g. "codex-042c0005-alice@example.com-team.json" or
	// "team/codex-....json". Matching is exact (case and the .json suffix are
	// ignored), so seats sharing an email never get mixed up.
	Primary []string `yaml:"primary" json:"primary"`
	// ExpireAt optionally specifies an RFC3339 timestamp (e.g. "2026-11-15T23:59:59+08:00")
	// after which the keys are rejected.
	ExpireAt string `yaml:"expire-at,omitempty" json:"expire-at,omitempty"`
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

// BuyerStatusResponse is the sanitized, zero-trust response returned to the buyer portal.
type BuyerStatusResponse struct {
	SeatName string `json:"seat_name"`
	// QuotaSupported is false when the seat is not a Codex account; quota windows
	// and reset credits are then unavailable.
	QuotaSupported   bool              `json:"quota_supported"`
	UpstreamUsage    map[string]any    `json:"upstream_usage"`
	CreditsAvailable int               `json:"credits_available"`
	CreditsList      []ResetCreditItem `json:"credits_list"`
	LocalCalls       int64             `json:"local_calls"`
	InputTokens      int64             `json:"input_tokens"`
	OutputTokens     int64             `json:"output_tokens"`
	CachedTokens     int64             `json:"cached_tokens"`
	TotalTokens      int64             `json:"total_tokens"`
	IsCooling        bool              `json:"is_cooling"`
	ExpiresAt        *time.Time        `json:"expires_at,omitempty"`
}

// ResetResponse is returned after a reset card redemption attempt.
type ResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}
