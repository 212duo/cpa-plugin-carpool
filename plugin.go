package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var (
	activeConfig atomic.Pointer[PluginConfig]
	activeTable  atomic.Pointer[routingTable]
)

type registrationPayload struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	ManagementAPI             bool `json:"management_api"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		configure(request)
		return okEnvelope(pluginRegistration())

	case pluginabi.MethodSchedulerPick:
		return pickAuth(request, activeTable.Load())

	case pluginabi.MethodManagementRegister:
		// CPA serves plugin resources for GET only, and only on the exact paths
		// registered here, so every portal endpoint must be listed.
		return okEnvelope(pluginapi.ManagementRegistrationResponse{
			Resources: []pluginapi.ResourceRoute{
				{
					Path:        RoutePortal,
					Menu:        "拼车管家",
					Description: "轻量级席位拼车、买家自助用量查询与一键满血重置",
				},
				{
					Path:        RouteAPIStatus,
					Description: "买家席位额度与用量查询接口",
				},
				{
					Path:        RouteAPIReset,
					Description: "买家自助核销重置卡接口",
				},
			},
		})

	case pluginabi.MethodManagementHandle:
		cfg := activeConfig.Load()
		effectiveCfg := PluginConfig{}
		if cfg != nil {
			effectiveCfg = *cfg
		}
		return handleResource(request, activeTable.Load(), effectiveCfg)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// configure loads the configuration sent with plugin.register / plugin.reconfigure.
// It never reports failure to the host: a plugin that fails to register is skipped,
// and buyer keys would then reach every account through CPA's built-in scheduler.
// An invalid configuration instead installs a table that rejects all requests.
func configure(raw []byte) {
	cfg, table, err := loadConfig(raw)
	if err != nil {
		logf("error", "invalid plugin configuration, rejecting all requests until it is fixed: %v", err)
		cfg = PluginConfig{}
		table = &routingTable{configErr: err}
	}
	activeConfig.Store(&cfg)
	activeTable.Store(table)
}

// loadConfig decodes the plugin's own section of config.yaml. CPA passes only the
// plugins.configs.<plugin-id> subtree, not the whole file. Decoding is strict so a
// misspelled key (e.g. api_keys) fails loudly instead of silently dropping a seat.
func loadConfig(raw []byte) (PluginConfig, *routingTable, error) {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return PluginConfig{}, nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}

	var cfg PluginConfig
	if len(bytes.TrimSpace(req.ConfigYAML)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(req.ConfigYAML))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return PluginConfig{}, nil, fmt.Errorf("parse config: %w", err)
		}
	}

	table, err := compileRouting(cfg)
	if err != nil {
		return PluginConfig{}, nil, err
	}
	return cfg, table, nil
}

func pluginRegistration() registrationPayload {
	return registrationPayload{
		SchemaVersion: 1,
		Metadata: pluginapi.Metadata{
			Name:             "cpa-plugin-carpool",
			Version:          PluginVersion,
			Author:           "212duo",
			GitHubRepository: "https://github.com/212duo/cpa-plugin-carpool",
		},
		Capabilities: registrationCapability{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			ManagementAPI:             true,
		},
	}
}

func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	b, _ := json.Marshal(Envelope{
		OK:    false,
		Error: &EnvelopeError{Code: code, Message: message},
	})
	return b
}

func unhandled() ([]byte, error) {
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// main is required by -buildmode=c-shared; the host enters through the exports in main.go.
func main() {}
