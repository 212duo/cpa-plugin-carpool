package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var activeTable atomic.Pointer[routingTable]

type registrationPayload struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	RequestInterceptor        bool `json:"request_interceptor"`
	RequestLifecyclePlugin    bool `json:"request_lifecycle_plugin"`
	ManagementAPI             bool `json:"management_api"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		configure(request)
		// CPA takes the capabilities from every reconfigure, so the request hooks
		// come and go with the limits.
		return okEnvelope(pluginRegistration(activeTable.Load()))

	case pluginabi.MethodSchedulerPick:
		return pickAuth(request, activeTable.Load())

	case pluginabi.MethodRequestInterceptBefore:
		return interceptBeforeAuth(request, activeTable.Load())

	case pluginabi.MethodRequestInterceptAfter:
		return passThrough()

	case pluginabi.MethodRequestComplete:
		return completeRequest(request)

	case pluginabi.MethodManagementRegister:
		// CPA serves plugin resources for GET only, and only on the exact paths
		// registered here, so every portal endpoint must be listed. Management
		// routes must not carry a Menu: CPA would turn a GET one into a resource.
		return okEnvelope(pluginapi.ManagementRegistrationResponse{
			Routes: adminRoutes,
			Resources: []pluginapi.ResourceRoute{
				{
					Path:        RoutePortal,
					Menu:        "拼车管家",
					Description: "买家自助页面：额度与刷新倒计时、重置卡核销、客户端接入配置",
				},
				{Path: RouteMonitor, Menu: "拼车管家 · 请求监控", Description: "仅查看当前 Key 的请求状态、Token 与耗时"},
				{Path: RouteAPIMonitor, Description: "当前 Key 的请求监控接口"},
				{
					Path:        RouteAdmin,
					Menu:        "拼车管家 · 管理",
					Description: "车主管理页面：席位增删改、key 生成、限额与用量（需要管理密钥）",
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
		table := activeTable.Load()
		effectiveCfg := PluginConfig{}
		if table != nil {
			effectiveCfg = table.config
		}
		return handleManagement(request, table, effectiveCfg)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// configure loads the configuration sent with plugin.register / plugin.reconfigure.
// It never reports failure to the host: a plugin that fails to register is skipped,
// and buyer keys would then reach every account through CPA's built-in scheduler.
// An invalid configuration instead installs a table that rejects all requests.
func configure(raw []byte) {
	_, table, err := loadConfig(raw)
	if err != nil {
		logf("error", "invalid plugin configuration, rejecting all requests until it is fixed: %v", err)
		table = &routingTable{configErr: err}
	}
	// Without limits the request hooks are unregistered and completions stop
	// arriving, so drop the counters rather than keep slots that are never freed.
	if prev := activeTable.Load(); prev != nil && prev.limited && !table.limited {
		limiter.reset()
	}
	prev := activeTable.Swap(table)
	if prev != nil && prev.monitor != nil {
		prev.monitor.close()
	}
}

// loadConfig decodes the lifecycle request of plugin.register / plugin.reconfigure
// and the configuration it carries.
func loadConfig(raw []byte) (PluginConfig, *routingTable, error) {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return PluginConfig{}, nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	noteHostSchema(req.SchemaVersion)
	return parseConfig(req.ConfigYAML)
}

// parseConfig decodes the plugin's own section of config.yaml. CPA passes only the
// plugins.configs.<plugin-id> subtree, not the whole file. Decoding is strict so a
// misspelled key (e.g. api_keys) fails loudly instead of silently dropping a seat.
func parseConfig(configYAML []byte) (PluginConfig, *routingTable, error) {
	var cfg PluginConfig
	if len(bytes.TrimSpace(configYAML)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(configYAML))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return PluginConfig{}, nil, fmt.Errorf("parse config: %w", err)
		}
	}

	table, err := compileRouting(cfg)
	if err != nil {
		return PluginConfig{}, nil, err
	}
	// Use parsed values, since CPA may reformat YAML and inject host-owned fields.
	// The digest is only returned on authenticated management routes.
	canonical, err := json.Marshal(cfg)
	if err != nil {
		return PluginConfig{}, nil, fmt.Errorf("encode config revision: %w", err)
	}
	digest := sha256.Sum256(canonical)
	table.configRevision = hex.EncodeToString(digest[:])
	table.config = cfg
	table.monitor = newMonitorService(cfg.DBPath)
	return cfg, table, nil
}

// noteHostSchema records whether the host sends request.complete, without which
// concurrency slots could never be freed.
func noteHostSchema(version uint32) {
	legacy := version < 2
	if legacy && !legacyHost.Load() {
		logf("warn", "host plugin schema version %d does not report request completion; max-concurrency is not enforced (rpm still is)", version)
	}
	legacyHost.Store(legacy)
}

// pluginRegistration describes the plugin for the configuration in table. The
// request hooks see every request, the owner's too, so they are only registered
// while some seat has a limit.
func pluginRegistration(table *routingTable) registrationPayload {
	limited := table != nil && table.limited
	return registrationPayload{
		SchemaVersion: PluginSchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "cpa-plugin-carpool",
			Version:          PluginVersion,
			Author:           "212duo",
			GitHubRepository: "https://github.com/212duo/cpa-plugin-carpool",
			Logo:             PluginLogoURL,
			ConfigFields:     configFields,
		},
		Capabilities: registrationCapability{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			RequestInterceptor:        limited,
			RequestLifecyclePlugin:    limited,
			ManagementAPI:             true,
		},
	}
}

// configFields describes plugins.configs.<plugin-id> for CPA's management clients.
var configFields = []pluginapi.ConfigField{
	{Name: "users", Type: pluginapi.ConfigFieldTypeArray, Description: "Seats: name, api-keys, primary, expire-at, max-concurrency, rpm. Easiest to edit on the plugin's admin page. / 席位列表，建议在插件的管理页面编辑"},
	{Name: "max-concurrency", Type: pluginapi.ConfigFieldTypeInteger, Default: 0, Description: "Default concurrent requests per seat, 0 = unlimited / 席位默认并发上限，0 为不限"},
	{Name: "rpm", Type: pluginapi.ConfigFieldTypeInteger, Default: 0, Description: "Default requests per minute per seat, 0 = unlimited / 席位默认每分钟请求上限，0 为不限"},
	{Name: "public-base-url", Type: pluginapi.ConfigFieldTypeString, Default: "", Description: "Gateway address shown in the portal's client setup guide / Portal 接入引导中显示的网关地址"},
	{Name: "debug", Type: pluginapi.ConfigFieldTypeBoolean, Default: false, Description: "Log every scheduling decision (seat names only) / 记录每次调度决定（只记录席位名）"},
	{Name: "db-path", Type: pluginapi.ConfigFieldTypeString, Default: "", Description: "CPA Manager Plus usage.sqlite, auto-detected when empty / CPA Manager Plus 用量数据库，留空自动探测"},
	{Name: "upstream-user-agent", Type: pluginapi.ConfigFieldTypeString, Default: "", Description: "User-Agent for ChatGPT quota requests / 查询 ChatGPT 额度时使用的 User-Agent"},
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
