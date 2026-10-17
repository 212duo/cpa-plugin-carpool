package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// hostCaller invokes a CPA host callback and returns the raw response envelope and
// the call's return code. main.go wires it to the C ABI; tests replace it.
var hostCaller func(method string, request []byte) ([]byte, int)

var errHostUnavailable = errors.New("host callbacks are unavailable")

// callHost sends payload to a host callback and decodes the result into out.
func callHost(method string, payload, out any) error {
	if hostCaller == nil {
		return errHostUnavailable
	}
	request, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	raw, code := hostCaller(method, request)
	if len(raw) == 0 {
		return fmt.Errorf("%s returned no response (code %d)", method, code)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return fmt.Errorf("%s failed (code %d)", method, code)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// logf writes to the CPA log through host.log, falling back to stderr.
func logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	err := callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": "[carpool] " + msg,
	}, nil)
	if err != nil {
		log.Printf("[carpool] %s: %s", level, msg)
	}
}

// listAuths returns the auths CPA exposes to plugins through host.auth.list.
func listAuths() ([]pluginapi.HostAuthFileEntry, error) {
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := callHost(pluginabi.MethodHostAuthList, map[string]any{}, &list); err != nil {
		return nil, err
	}
	return list.Files, nil
}

// findSeatAuth returns the runtime entry of the seat's first configured Auth ID
// that CPA currently knows about.
func findSeatAuth(s *seat) (pluginapi.HostAuthFileEntry, error) {
	files, err := listAuths()
	if err != nil {
		return pluginapi.HostAuthFileEntry{}, err
	}
	if entry, ok := matchSeatAuth(s, files); ok {
		return entry, nil
	}
	return pluginapi.HostAuthFileEntry{}, fmt.Errorf("none of the auth IDs of seat %s is loaded in CPA", s.user.Name)
}

// matchSeatAuth picks the seat's first configured Auth ID present in files.
func matchSeatAuth(s *seat, files []pluginapi.HostAuthFileEntry) (pluginapi.HostAuthFileEntry, bool) {
	for _, want := range s.matchIDs {
		for _, entry := range files {
			if normalizeAuthID(entry.ID) == want {
				return entry, true
			}
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

// loadCredentials reads the credential JSON of one auth through host.auth.get.
func loadCredentials(authIndex string) (*authFilePayload, error) {
	var resp pluginapi.HostAuthGetResponse
	if err := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex}, &resp); err != nil {
		return nil, err
	}
	var p authFilePayload
	if err := json.Unmarshal(resp.JSON, &p); err != nil {
		return nil, fmt.Errorf("decode credential json: %w", err)
	}
	return &p, nil
}

// isCooling reports whether CPA currently holds the auth back from routing.
func isCooling(authIndex string, now time.Time) bool {
	var resp pluginapi.HostAuthGetRuntimeResponse
	if err := callHost(pluginabi.MethodHostAuthGetRuntime, pluginapi.HostAuthGetRequest{AuthIndex: authIndex}, &resp); err != nil {
		return false
	}
	return resp.Auth.Unavailable || resp.Auth.NextRetryAfter.After(now)
}

// resetCooldown clears CPA's quota and cooldown state for one auth, the same as
// POST /v8/management/routing/cooldown/reset.
func resetCooldown(authIndex string) error {
	return callHost(pluginabi.MethodHostRoutingResetCooldown, pluginapi.HostRoutingResetCooldownRequest{AuthIndex: authIndex}, nil)
}

// supportsUpstreamQuota reports whether the auth is a Codex (ChatGPT) account, the
// only provider whose quota and reset credits the portal knows how to read.
func supportsUpstreamQuota(entry pluginapi.HostAuthFileEntry) bool {
	return strings.EqualFold(entry.Type, "codex") || strings.EqualFold(entry.Provider, "codex")
}

// authProvider names the auth's provider type in lower case, e.g. "codex".
func authProvider(entry pluginapi.HostAuthFileEntry) string {
	if p := strings.TrimSpace(entry.Provider); p != "" {
		return strings.ToLower(p)
	}
	return strings.ToLower(strings.TrimSpace(entry.Type))
}
