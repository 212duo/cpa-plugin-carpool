package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"sync/atomic"
	"unsafe"

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

//export cliproxy_plugin_init
func cliproxy_plugin_init(api *C.cliproxy_plugin_api) C.int {
	if api == nil {
		return 1
	}
	api.abi_version = 1
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}

	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())

	case pluginabi.MethodSchedulerPick:
		return pickAuth(request, activeTable.Load())

	case pluginabi.MethodManagementRegister:
		return okEnvelope(pluginapi.ManagementRegistrationResponse{
			Resources: []pluginapi.ResourceRoute{
				{
					Path:        RoutePortal,
					Menu:        "拼车管家",
					Description: "轻量级席位拼车、买家自助用量查询与一键满血重置",
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

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}

	cfg := PluginConfig{
		Enabled: true,
	}

	if len(req.ConfigYAML) > 0 {
		var wrapper struct {
			Plugins struct {
				Configs struct {
					Carpool PluginConfig `yaml:"carpool"`
				} `yaml:"configs"`
			} `yaml:"plugins"`
		}
		if err := yaml.Unmarshal(req.ConfigYAML, &wrapper); err == nil {
			cfg = wrapper.Plugins.Configs.Carpool
		}
	}

	table := compileRouting(cfg)
	activeConfig.Store(&cfg)
	activeTable.Store(table)

	return nil
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

func writeResponse(buf *C.cliproxy_buffer, payload []byte) {
	if buf == nil || len(payload) == 0 {
		return
	}
	buf.ptr = C.CBytes(payload)
	buf.len = C.size_t(len(payload))
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

func main() {}
