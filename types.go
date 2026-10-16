package main

import (
	"encoding/json"
	"time"
)

// PluginVersion is the semantic release version of the carpool plugin.
const PluginVersion = "0.1.0"

// Resource route paths under /v0/resource/plugins/carpool/
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

// PluginConfig is the root configuration loaded from CPA config.yaml:
// plugins:
//   configs:
//     carpool:
//       enabled: true
//       debug: false
//       db-path: "/opt/cpa/manager-data/usage.sqlite" # or auto-detected
//       auth-dir: "/root/.cli-proxy-api"
//       users: [...]
type PluginConfig struct {
	Enabled bool         `yaml:"enabled" json:"enabled"`
	Debug   bool         `yaml:"debug" json:"debug"`
	DBPath  string       `yaml:"db-path" json:"db-path"`
	AuthDir string       `yaml:"auth-dir" json:"auth-dir"`
	Users   []UserConfig `yaml:"users" json:"users"`
}

// UserConfig binds a buyer's client API key to a specific, isolated upstream account.
type UserConfig struct {
	Name     string   `yaml:"name" json:"name"`
	APIKeys  []string `yaml:"api-keys" json:"api-keys"`
	// Primary specifies the exact Auth ID (e.g. "codex-042c0005-alice@example.com-team")
	// or exact file name. This is an EXACT match, avoiding multi-seat same-email confusion.
	Primary  []string `yaml:"primary" json:"primary"`
	// ExpireAt optionally specifies an ISO8601 or RFC3339 timestamp when the key automatically expires.
	ExpireAt string   `yaml:"expire-at,omitempty" json:"expire-at,omitempty"`
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
	SeatName         string           `json:"seat_name"`
	UpstreamUsage    map[string]any   `json:"upstream_usage"`
	CreditsAvailable int              `json:"credits_available"`
	CreditsList      []ResetCreditItem `json:"credits_list"`
	LocalCalls       int64            `json:"local_calls"`
	InputTokens      int64            `json:"input_tokens"`
	OutputTokens     int64            `json:"output_tokens"`
	CachedTokens     int64            `json:"cached_tokens"`
	TotalTokens      int64            `json:"total_tokens"`
	IsCooling        bool             `json:"is_cooling"`
	ExpiresAt        *time.Time       `json:"expires_at,omitempty"`
}

// ResetRequest is the payload received when a buyer requests a reset card consumption.
type ResetRequest struct {
	Key string `json:"key"`
}

// ResetResponse is returned after a reset card redemption attempt.
type ResetResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}
