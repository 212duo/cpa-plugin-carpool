package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type routingTable struct {
	debug bool
	// Keep management settings and routing in one immutable, atomic snapshot.
	config PluginConfig
	// configRevision identifies the configuration actually used for routing.
	configRevision string
	// configErr is set when the configuration could not be loaded. The table then
	// rejects every request: buyers cannot be told apart from other callers, and
	// letting them through would expose the whole account pool.
	configErr error
	// publicBaseURL is the normalized public-base-url, without a trailing slash.
	publicBaseURL string
	// limited is true when any seat has a concurrency or RPM limit. Only then does
	// the plugin register its request hooks, which see every request's body.
	limited bool
	seats   []*seat // in configuration order
	byKey   map[string]*seat
	byScope map[string]*seat
}

// seat is one compiled buyer entry.
type seat struct {
	user *UserConfig
	// authIDs are the configured Auth IDs verbatim, used to locate auth files
	// (file names are case-sensitive on Linux).
	authIDs []string
	// matchIDs are the normalized Auth IDs, used to match scheduler candidates.
	matchIDs []string
	apiKeys  []string  // trimmed, non-empty
	expireAt time.Time // zero when the seat never expires
	// maxConcurrency and rpm are the effective limits; 0 means unlimited.
	maxConcurrency int
	rpm            int
}

func (s *seat) expired(now time.Time) bool {
	return !s.expireAt.IsZero() && now.After(s.expireAt)
}

func compileRouting(cfg PluginConfig) (*routingTable, error) {
	table := &routingTable{
		debug:   cfg.Debug,
		byKey:   make(map[string]*seat),
		byScope: make(map[string]*seat),
	}
	if !cfg.Enabled {
		return table, nil
	}

	baseURL, err := normalizePublicBaseURL(cfg.PublicBaseURL)
	if err != nil {
		return nil, err
	}
	table.publicBaseURL = baseURL
	if cfg.MaxConcurrency < 0 || cfg.RPM < 0 {
		return nil, fmt.Errorf("max-concurrency and rpm must not be negative (0 means unlimited)")
	}

	names := make(map[string]int)
	for i := range cfg.Users {
		user := &cfg.Users[i]
		user.Name = strings.TrimSpace(user.Name)
		if user.Name == "" {
			user.Name = fmt.Sprintf("seat-%d", i+1)
		}
		// Seat names identify seats in the limiter, the logs and the admin page.
		if prev, dup := names[user.Name]; dup {
			return nil, fmt.Errorf("users[%d]: seat name %q is already used by users[%d]", i, user.Name, prev)
		}
		names[user.Name] = i
		s := &seat{user: user}

		s.maxConcurrency, s.rpm = cfg.MaxConcurrency, cfg.RPM
		if user.MaxConcurrency != nil {
			s.maxConcurrency = *user.MaxConcurrency
		}
		if user.RPM != nil {
			s.rpm = *user.RPM
		}
		if s.maxConcurrency < 0 || s.rpm < 0 {
			return nil, fmt.Errorf("users[%d] (%s): max-concurrency and rpm must not be negative (0 means unlimited)", i, user.Name)
		}
		if s.maxConcurrency > 0 || s.rpm > 0 {
			table.limited = true
		}

		for _, a := range user.Primary {
			a = strings.TrimSpace(a)
			if a != "" {
				s.authIDs = append(s.authIDs, a)
				s.matchIDs = append(s.matchIDs, normalizeAuthID(a))
			}
		}
		if len(s.authIDs) == 0 {
			return nil, fmt.Errorf("users[%d] (%s): primary must list at least one auth ID", i, user.Name)
		}

		if exp := strings.TrimSpace(user.ExpireAt); exp != "" {
			t, err := time.Parse(time.RFC3339, exp)
			if err != nil {
				return nil, fmt.Errorf("users[%d] (%s): expire-at must be RFC3339, e.g. 2026-11-15T23:59:59+08:00: %w", i, user.Name, err)
			}
			s.expireAt = t
		}

		for _, k := range user.APIKeys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if prev := table.byKey[k]; prev != nil {
				return nil, fmt.Errorf("users[%d] (%s): an api key is also assigned to %s", i, user.Name, prev.user.Name)
			}
			table.byKey[k] = s
			table.byScope[callerScope(k)] = s
			s.apiKeys = append(s.apiKeys, k)
		}
		if len(s.apiKeys) == 0 {
			return nil, fmt.Errorf("users[%d] (%s): api-keys must list at least one key", i, user.Name)
		}
		table.seats = append(table.seats, s)
	}

	return table, nil
}

// normalizePublicBaseURL checks public-base-url and strips its trailing slash.
func normalizePublicBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("public-base-url must be an http(s) URL without credentials, query or fragment, e.g. https://api.example.com")
	}
	return strings.TrimRight(raw, "/"), nil
}

// normalizeAuthID canonicalizes an Auth ID for matching only. CPA uses the auth
// file path relative to auth-dir as the ID and lowercases it on Windows.
func normalizeAuthID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	id = strings.ReplaceAll(id, `\`, "/")
	return strings.TrimSuffix(id, ".json")
}

func callerScope(key string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

// userFor identifies the downstream caller by caller_scope metadata or Authorization header.
func (t *routingTable) userFor(opts pluginapi.SchedulerOptions) *seat {
	if t == nil {
		return nil
	}
	if scope, _ := opts.Metadata["caller_scope"].(string); scope != "" {
		if s := t.byScope[scope]; s != nil {
			return s
		}
	}

	for headerName, values := range opts.Headers {
		if strings.EqualFold(headerName, "authorization") && len(values) > 0 {
			v := strings.TrimSpace(values[0])
			if strings.HasPrefix(strings.ToLower(v), "bearer ") {
				token := strings.TrimSpace(v[7:])
				if s := t.byKey[token]; s != nil {
					return s
				}
				if s := t.byScope[callerScope(token)]; s != nil {
					return s
				}
			}
		}
		if strings.EqualFold(headerName, "x-api-key") && len(values) > 0 {
			token := strings.TrimSpace(values[0])
			if s := t.byKey[token]; s != nil {
				return s
			}
			if s := t.byScope[callerScope(token)]; s != nil {
				return s
			}
		}
	}

	return nil
}

// pickAuth implements the dedicated 1-to-1 fail-closed scheduler logic.
func pickAuth(raw []byte, table *routingTable) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := jsonUnmarshal(raw, &req); err != nil {
		return nil, err
	}

	if table == nil {
		return unhandled()
	}

	// 0. Fail-closed on a broken configuration, whoever the caller is
	if table.configErr != nil {
		return okEnvelope(pluginapi.SchedulerPickResponse{
			Handled:      true,
			Reject:       true,
			RejectCode:   "carpool_config_invalid",
			RejectReason: "carpool: plugin configuration is invalid, please contact the host",
		})
	}

	if len(table.byKey) == 0 {
		return unhandled()
	}

	s := table.userFor(req.Options)
	if s == nil {
		// Key not managed by carpool (e.g. host admin key) -> pass to CPA built-in routing
		return unhandled()
	}

	// 1. Check auto-expiry if configured
	if s.expired(time.Now()) {
		return okEnvelope(pluginapi.SchedulerPickResponse{
			Handled:      true,
			Reject:       true,
			RejectCode:   "seat_expired",
			RejectReason: fmt.Sprintf("carpool: your seat (%s) expired at %s, please contact the host to renew", s.user.Name, s.user.ExpireAt),
		})
	}

	// 2. Exact match candidate against user's primary auth IDs
	var matchedCandidates []pluginapi.SchedulerAuthCandidate

	for _, c := range req.Candidates {
		candidateID := normalizeAuthID(c.ID)
		for _, allowed := range s.matchIDs {
			// Exact match only: a suffix match would let "alice@x.com-team" pick "malice@x.com-team"
			if candidateID == allowed {
				matchedCandidates = append(matchedCandidates, c)
				break
			}
		}
	}

	// 3. If available, pick the best candidate
	if len(matchedCandidates) > 0 {
		sort.SliceStable(matchedCandidates, func(i, j int) bool {
			if matchedCandidates[i].Priority != matchedCandidates[j].Priority {
				return matchedCandidates[i].Priority > matchedCandidates[j].Priority
			}
			return matchedCandidates[i].ID < matchedCandidates[j].ID
		})
		if table.debug {
			logf("debug", "seat %s -> auth %s", s.user.Name, matchedCandidates[0].ID)
		}
		return okEnvelope(pluginapi.SchedulerPickResponse{
			AuthID:  matchedCandidates[0].ID,
			Handled: true,
		})
	}

	// 4. Fail-closed: Never fall back to unassigned accounts (e.g. personal Pro 5x)
	if table.debug {
		logf("debug", "seat %s: none of its auths is among %d candidates, rejecting", s.user.Name, len(req.Candidates))
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		Handled:      true,
		Reject:       true,
		RejectCode:   "auth_unavailable",
		RejectReason: fmt.Sprintf("carpool: your dedicated seat (%s) is currently in cooldown or unavailable", s.user.Name),
	})
}
