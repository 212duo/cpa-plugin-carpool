package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type routingTable struct {
	debug   bool
	byKey   map[string]*UserConfig
	byScope map[string]*UserConfig
	authMap map[*UserConfig][]string
}

func compileRouting(cfg PluginConfig) *routingTable {
	table := &routingTable{
		debug:   cfg.Debug,
		byKey:   make(map[string]*UserConfig),
		byScope: make(map[string]*UserConfig),
		authMap: make(map[*UserConfig][]string),
	}

	for i := range cfg.Users {
		user := &cfg.Users[i]
		user.Name = strings.TrimSpace(user.Name)
		if user.Name == "" {
			user.Name = fmt.Sprintf("seat-%d", i+1)
		}

		for _, k := range user.APIKeys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			table.byKey[k] = user
			table.byScope[callerScope(k)] = user
		}

		// Normalize primary auth IDs to lowercase
		var normalized []string
		for _, a := range user.Primary {
			a = strings.TrimSpace(strings.ToLower(a))
			if a != "" {
				// Strip .json suffix if provided by user
				a = strings.TrimSuffix(a, ".json")
				normalized = append(normalized, a)
			}
		}
		table.authMap[user] = normalized
	}

	return table
}

func callerScope(key string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

// userFor identifies the downstream caller by caller_scope metadata or Authorization header.
func (t *routingTable) userFor(opts pluginapi.SchedulerOptions) *UserConfig {
	if t == nil {
		return nil
	}
	if scope, _ := opts.Metadata["caller_scope"].(string); scope != "" {
		if u := t.byScope[scope]; u != nil {
			return u
		}
	}

	for headerName, values := range opts.Headers {
		if strings.EqualFold(headerName, "authorization") && len(values) > 0 {
			v := strings.TrimSpace(values[0])
			if strings.HasPrefix(strings.ToLower(v), "bearer ") {
				token := strings.TrimSpace(v[7:])
				if u := t.byKey[token]; u != nil {
					return u
				}
				if u := t.byScope[callerScope(token)]; u != nil {
					return u
				}
			}
		}
		if strings.EqualFold(headerName, "x-api-key") && len(values) > 0 {
			token := strings.TrimSpace(values[0])
			if u := t.byKey[token]; u != nil {
				return u
			}
			if u := t.byScope[callerScope(token)]; u != nil {
				return u
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

	if table == nil || len(table.byKey) == 0 {
		return unhandled()
	}

	user := table.userFor(req.Options)
	if user == nil {
		// Key not managed by carpool (e.g. host admin key) -> pass to CPA built-in routing
		return unhandled()
	}

	// 1. Check auto-expiry if configured
	if user.ExpireAt != "" {
		if expireTime, err := time.Parse(time.RFC3339, user.ExpireAt); err == nil {
			if time.Now().After(expireTime) {
				return okEnvelope(pluginapi.SchedulerPickResponse{
					Handled:      true,
					Reject:       true,
					RejectCode:   "seat_expired",
					RejectReason: fmt.Sprintf("carpool: your seat (%s) expired at %s, please contact the host to renew", user.Name, user.ExpireAt),
				})
			}
		}
	}

	// 2. Exact match candidate against user's primary auth IDs
	allowedIDs := table.authMap[user]
	var matchedCandidates []pluginapi.SchedulerAuthCandidate

	for _, c := range req.Candidates {
		candidateID := strings.TrimSuffix(strings.ToLower(c.ID), ".json")
		for _, allowed := range allowedIDs {
			// Exact match or token boundary match to prevent multi-seat same-email confusion
			if candidateID == allowed || strings.HasSuffix(candidateID, allowed) {
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
		return okEnvelope(pluginapi.SchedulerPickResponse{
			AuthID:  matchedCandidates[0].ID,
			Handled: true,
		})
	}

	// 4. Fail-closed: Never fall back to unassigned accounts (e.g. personal Pro 5x)
	return okEnvelope(pluginapi.SchedulerPickResponse{
		Handled:      true,
		Reject:       true,
		RejectCode:   "auth_unavailable",
		RejectReason: fmt.Sprintf("carpool: your dedicated seat (%s) is currently in cooldown or unavailable", user.Name),
	})
}
