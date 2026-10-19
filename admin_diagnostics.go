package main

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func handleAdminDiagnostics(req pluginapi.ManagementRequest, cfg PluginConfig) ([]byte, error) {
	type check struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	checks := []check{{"页面嵌入", "ok", "允许同源 iframe；CPAMP 与插件页面应使用相同协议、域名和端口"}}
	snapshot, err := readConfigSnapshot(context.Background(), req)
	if err != nil {
		checks = append(checks, check{"配置接口", "error", "无法读取配置接口，请检查 CARPOOL_HOST_URL、CPA 管理密钥、网络或反代"})
	} else {
		checks = append(checks, check{"配置接口", "ok", "已成功直连 CPA 并读取持久化配置，无需向浏览器开放 /v8/"})
		_, revision, err := validateSaveConfig(snapshot.Config)
		table := activeTable.Load()
		if err != nil {
			checks = append(checks, check{"席位配置", "error", "持久化配置未通过校验，请检查席位设置与插件启用状态"})
		} else if table == nil || table.configErr != nil || table.configRevision != revision {
			checks = append(checks, check{"配置生效", "warning", "持久化配置与当前运行配置不同，请等待重载或查看 CPA 日志"})
		} else {
			checks = append(checks, check{"配置生效", "ok", "席位配置已生效"})
		}
		if len(snapshot.Keys) == 0 {
			checks = append(checks, check{"客户端认证", "warning", "客户端 key 列表为空，CPA 未启用客户端 key 认证"})
		} else {
			checks = append(checks, check{"客户端认证", "ok", "已配置客户端 key 认证"})
		}
	}
	if _, err := readTokenStats(cfg.DBPath, usageKeyHash("carpool-diagnostics")); err != nil {
		checks = append(checks, check{"历史用量", "warning", "用量数据库不可读或表结构不兼容，请检查 CPAMP 数据库挂载、权限及采集状态；不影响席位调度"})
	} else {
		checks = append(checks, check{"历史用量", "ok", "用量数据库可读取；数据更新频率由 CPAMP 采集决定"})
	}
	return jsonResponse(http.StatusOK, map[string]any{"checks": checks, "plugin_version": PluginVersion})
}
