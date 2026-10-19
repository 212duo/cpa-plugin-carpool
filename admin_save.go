package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type saveInput struct {
	RequestID        string          `json:"request_id"`
	ExpectedRevision string          `json:"expected_revision"`
	Config           json.RawMessage `json:"config"`
	AllowEnableAuth  bool            `json:"allow_enable_auth"`
}

// Only this public status is serialized. Credentials and draft keys stay in the
// bounded in-memory job store, which survives page closure and reconfiguration.
type saveStatus struct {
	ID          string    `json:"id"`
	State       string    `json:"state"`
	Stage       string    `json:"stage"`
	Message     string    `json:"message"`
	ErrorCode   string    `json:"error_code,omitempty"`
	Retryable   bool      `json:"retryable"`
	ConfigSaved bool      `json:"config_saved"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type saveJob struct {
	status          saveStatus
	input           saveInput
	fingerprint     string
	initialized     bool
	oldKeys         []string
	newKeys         []string
	writeAttempted  bool
	routingRevision string
	targetRevision  string
}

type saveManager struct {
	mu     sync.Mutex
	jobs   map[string]*saveJob
	order  []string
	latest string
}

var saveJobs = &saveManager{jobs: make(map[string]*saveJob)}
var savePollInterval = 250 * time.Millisecond
var savePollAttempts = 40

type saveFailure struct {
	code, message string
	retryable     bool
}

func (e *saveFailure) Error() string { return e.message }
func saveError(code, message string, retryable bool) error {
	return &saveFailure{code, message, retryable}
}

func handleAdminSave(req pluginapi.ManagementRequest, route string) ([]byte, error) {
	m := saveJobs
	if req.Method == http.MethodGet {
		m.mu.Lock()
		id := req.Query.Get("id")
		if id == "" {
			id = m.latest
		}
		var status *saveStatus
		if job := m.jobs[id]; job != nil {
			copy := job.status
			status = &copy
		}
		m.mu.Unlock()
		return jsonResponse(http.StatusOK, map[string]any{"job": status})
	}
	var input saveInput
	if err := json.Unmarshal(req.Body, &input); err != nil || input.RequestID == "" || len(input.RequestID) > 128 {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "request_id is required (maximum 128 characters)"})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[input.RequestID]
	if route == RouteAdminRetry {
		if job == nil {
			return jsonResponse(http.StatusNotFound, map[string]string{"error": "任务不存在或已过期，请刷新后重新提交"})
		}
		if job.status.State != "failed" || !job.status.Retryable {
			return jsonResponse(http.StatusConflict, map[string]string{"error": "当前任务无法重试，请刷新查看状态"})
		}
	} else {
		if input.ExpectedRevision == "" {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "expected_revision is required"})
		}
		fingerprint := persistedRevision(input, true)
		if job != nil {
			if job.fingerprint != fingerprint {
				return jsonResponse(http.StatusConflict, map[string]string{"error": "同一 request_id 不能提交不同配置"})
			}
			return jsonResponse(http.StatusOK, map[string]any{"job": job.status})
		}
		if len(input.Config) == 0 || len(input.Config) > 2<<20 {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid configuration size"})
		}
		cfg, revision, err := validateSaveConfig(input.Config)
		if err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		job = &saveJob{input: input, fingerprint: fingerprint, newKeys: configKeys(cfg), routingRevision: revision, targetRevision: persistedRevision(input.Config, true)}
		job.status.ID = input.RequestID
	}
	if !configMutationMu.TryLock() {
		return jsonResponse(http.StatusConflict, map[string]string{"error": "另一笔保存正在执行，请等待完成后刷新"})
	}
	job.status.State, job.status.Stage, job.status.Message = "running", "checking", "正在检查配置版本与客户端 key…"
	job.status.ErrorCode, job.status.Retryable = "", false
	job.status.UpdatedAt = time.Now().UTC()
	if m.jobs[input.RequestID] == nil {
		// Keep the newest 32 jobs, including failed jobs that can be retried.
		if len(m.order) >= 32 {
			delete(m.jobs, m.order[0])
			m.order = m.order[1:]
		}
		m.order = append(m.order, input.RequestID)
		m.jobs[input.RequestID] = job
	}
	m.latest = input.RequestID
	status := job.status
	request := pluginapi.ManagementRequest{Headers: req.Headers.Clone()}
	go m.run(job, request)
	return jsonResponse(http.StatusAccepted, map[string]any{"job": status})
}

func validateSaveConfig(raw []byte) (PluginConfig, string, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return PluginConfig{}, "", fmt.Errorf("配置必须是 JSON 对象")
	}
	configYAML, err := yaml.Marshal(doc)
	if err != nil {
		return PluginConfig{}, "", fmt.Errorf("无法解析配置")
	}
	cfg, table, err := parseConfig(configYAML)
	if err != nil {
		return PluginConfig{}, "", err
	}
	defer table.monitor.close()
	if !cfg.Enabled {
		return cfg, "", fmt.Errorf("请先删除席位并保存，再通过 CPA 插件管理停用插件")
	}
	return cfg, table.configRevision, nil
}

func configKeys(cfg PluginConfig) []string {
	keys := []string{}
	for _, user := range cfg.Users {
		for _, key := range user.APIKeys {
			key = strings.TrimSpace(key)
			if key != "" && !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func (m *saveManager) stage(job *saveJob, stage, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job.status.Stage, job.status.Message, job.status.UpdatedAt = stage, message, time.Now().UTC()
}

func (m *saveManager) run(job *saveJob, req pluginapi.ManagementRequest) {
	defer configMutationMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err := m.apply(ctx, job, req)
	m.mu.Lock()
	defer m.mu.Unlock()
	job.status.UpdatedAt = time.Now().UTC()
	if err != nil {
		job.status.State, job.status.Message = "failed", err.Error()
		if failure, ok := err.(*saveFailure); ok {
			job.status.ErrorCode, job.status.Retryable = failure.code, failure.retryable
		}
		return
	}
	job.status.State, job.status.Stage, job.status.Message = "succeeded", "complete", "已保存，席位绑定与客户端 key 已生效"
}

func saveRead(ctx context.Context, req pluginapi.ManagementRequest) (configSnapshot, error) {
	snapshot, err := readConfigSnapshot(ctx, req)
	if err != nil {
		return snapshot, saveError("read_failed", "无法读取配置接口，请检查网络或反代", true)
	}
	return snapshot, nil
}

func (m *saveManager) apply(ctx context.Context, job *saveJob, req pluginapi.ManagementRequest) error {
	snapshot, err := saveRead(ctx, req)
	if err != nil {
		return err
	}
	if !job.initialized {
		if snapshot.Revision != job.input.ExpectedRevision {
			return saveError("conflict", "插件配置已被其他页面修改，请刷新后重新编辑", false)
		}
		var old PluginConfig
		if err := json.Unmarshal(snapshot.Config, &old); err != nil {
			return saveError("read_failed", "无法读取现有席位配置，未执行保存", false)
		}
		job.oldKeys = configKeys(old)
		job.initialized = true
	}
	if snapshot.Revision != job.input.ExpectedRevision && !(job.writeAttempted && snapshot.Revision == job.targetRevision) {
		return saveError("conflict", "插件配置已被其他页面修改，已停止后续写入，请刷新核对", false)
	}
	removed := []string{}
	for _, key := range job.oldKeys {
		if !slices.Contains(job.newKeys, key) {
			removed = append(removed, key)
		}
	}
	retained := []string{}
	for _, key := range snapshot.Keys {
		if !slices.Contains(removed, key) {
			retained = append(retained, key)
		}
	}
	if len(removed) > 0 && len(retained) == 0 {
		return saveError("last_key", "撤销这些 key 会关闭 CPA 认证，请先添加独立的车主客户端 key", false)
	}
	if len(job.newKeys) > 0 && len(snapshot.Keys) == 0 && !job.input.AllowEnableAuth {
		return saveError("auth_confirmation", "客户端 key 列表为空；加入席位 key 会启用 CPA 认证，请确认后重新提交", false)
	}
	m.stage(job, "revoking", "正在撤销不再使用的旧 key…")
	if len(retained) != len(snapshot.Keys) {
		body, _ := json.Marshal(retained)
		if _, err := hostConfigRequestContext(ctx, req, http.MethodPut, "/access/api-keys", body); err != nil {
			return saveError("revoke_failed", "撤销旧 key 失败，未修改席位绑定，可重试", true)
		}
	}
	for _, key := range removed {
		if err := waitSaveCondition(ctx, func() bool { return keyHasStatus(ctx, key, http.StatusUnauthorized) }); err != nil {
			return saveError("revocation_pending", "尚未确认旧 key 已撤销，保留原席位绑定；请稍后重试", true)
		}
	}
	// Re-check after asynchronous key reloads; do not overwrite another editor.
	snapshot, err = saveRead(ctx, req)
	if err != nil {
		return err
	}
	if snapshot.Revision != job.input.ExpectedRevision && !(job.writeAttempted && snapshot.Revision == job.targetRevision) {
		return saveError("conflict", "配置在保存期间被其他页面修改，已停止，请刷新核对", false)
	}
	for _, key := range removed {
		if slices.Contains(snapshot.Keys, key) {
			return saveError("conflict", "旧 key 被其他入口重新授权，已停止修改绑定", false)
		}
	}
	m.stage(job, "saving_config", "正在保存席位配置…")
	if snapshot.Revision != job.targetRevision {
		job.writeAttempted = true
		if _, err := hostConfigRequestContext(ctx, req, http.MethodPut, "/plugins/configs/"+url.PathEscape(configPluginID()), job.input.Config); err != nil {
			return saveError("write_failed", "席位配置写入未确认，已撤销的 key 不会恢复；请重试任务核对", true)
		}
	}
	m.mu.Lock()
	job.status.ConfigSaved = true
	m.mu.Unlock()
	m.stage(job, "waiting_routing", "正在等待席位绑定生效…")
	if err := waitSaveCondition(ctx, func() bool {
		table := activeTable.Load()
		return table != nil && table.configErr == nil && table.configRevision == job.routingRevision
	}); err != nil {
		return saveError("routing_pending", "尚未确认席位绑定生效，未授权新 key；请稍后重试", true)
	}
	snapshot, err = saveRead(ctx, req)
	if err != nil {
		return err
	}
	if snapshot.Revision != job.targetRevision {
		return saveError("conflict", "席位配置又被修改，未授权新 key，请刷新核对", false)
	}
	for _, key := range removed {
		if slices.Contains(snapshot.Keys, key) {
			return saveError("conflict", "旧 key 被其他入口重新授权，请检查 CPA 配置", false)
		}
	}
	m.stage(job, "authorizing", "正在授权新 key 并确认生效…")
	keys := slices.Clone(snapshot.Keys)
	for _, key := range job.newKeys {
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	if len(snapshot.Keys) == 0 && len(keys) > 0 && !job.input.AllowEnableAuth {
		return saveError("auth_confirmation", "认证列表发生变化，请刷新核对并确认启用 CPA 认证", false)
	}
	if len(keys) != len(snapshot.Keys) {
		body, _ := json.Marshal(keys)
		if _, err := hostConfigRequestContext(ctx, req, http.MethodPut, "/access/api-keys", body); err != nil {
			return saveError("authorize_failed", "席位已保存，但新 key 未能加入认证列表；可重试任务", true)
		}
	}
	for _, key := range job.newKeys {
		if err := waitSaveCondition(ctx, func() bool { return keyHasStatus(ctx, key, http.StatusOK) }); err != nil {
			return saveError("authorization_pending", "席位已保存，客户端 key 生效尚未确认；请稍后重试", true)
		}
	}
	return nil
}

func keyHasStatus(ctx context.Context, key string, expected int) bool {
	req := pluginapi.ManagementRequest{Headers: http.Header{"Authorization": {"Bearer " + key}}}
	status, _, err := directHostRequest(ctx, req, http.MethodGet, "/v1/models", nil)
	return err == nil && status == expected
}

func waitSaveCondition(ctx context.Context, check func() bool) error {
	for attempt := 0; attempt < savePollAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if check() {
			return nil
		}
		timer := time.NewTimer(savePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("reload not confirmed")
}
