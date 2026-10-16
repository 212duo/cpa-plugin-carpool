# cpa-plugin-carpool 技术规格说明（SPEC）

> 项目名称：**cpa-plugin-carpool（CPA 拼车管家）**
> 目标宿主：[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8.0.30+（原生插件 ABI v1）
> 适用场景：个人与小团队的多租户账号分配、席位共享
> 当前版本：v0.2.0

---

## 1. 背景与产品定位

### 1.1 痛点

1. **重型方案（Sub2API / NewAPI / OneAPI）过重**：依赖数据库、Redis 和独立服务，对只有 2~10 个账号的场景来说运维成本过高。
2. **纯 CPA 缺少多租户能力**：
   - CPA 是反代网关内核，没有面向使用者的自助页面；
   - 按 `provider:email` 匹配的路由插件（如 `key-router`）在同一邮箱下有多个账号（如 Team 席位 + 个人 Pro）时会串号；
   - 全自动重置类插件由后台决定何时核销，使用者无法自己决定何时使用稀缺的重置卡；
   - 使用者撞限（429）后只能找管理员查额度、手动重置。

### 1.2 定位

**cpa-plugin-carpool** 以原生动态库形式运行在 CLIProxyAPI 内部：

- **对管理员**：零数据库、零额外进程；在 `config.yaml` 里把 key 绑到账号，支持到期自动失效；
- **对使用者**：凭自己的 key 打开 Portal，查看 5 小时窗口与周窗口的额度和刷新倒计时、历史 Token 用量，撞限时自助核销重置卡；
- **对安全**：出错即拒绝；对外只输出白名单字段，不暴露上游账号的邮箱、account_id、token。

---

## 2. 功能需求

### 2.1 买家自助端（Portal）

- [x] **Key 鉴权**：通过 `X-Carpool-Key` 请求头（或 `key` 查询参数）识别席位；分享链接使用 `#key=`。
- [x] **额度窗口**：展示上游 5 小时窗口与周窗口的已用比例（仅 Codex 账号）。
- [x] **刷新倒计时**：5 小时窗口动态倒计时（如 `剩余 1 小时 23 分`），周窗口按本地时间展示（如 `周日 18:00 (剩余 3 天)`）。
- [x] **历史用量**：读取 CPA Manager Plus 的 `usage.sqlite`，以 M（百万 Token）为单位展示调用次数与输入 / 输出 / 缓存命中。
- [x] **重置卡明细与自助核销**：列出重置卡（标题、状态、发放时间、到期时间、使用时间）；核销后通过宿主回调解除 CPA 冷却。
- [x] **冷却状态与租期**：展示 CPA 运行时冷却状态与席位到期时间。
- [ ] **客户端接入引导**：Codex CLI、Cursor、Claude Code 等客户端的一键复制配置模板。

### 2.2 管理端

- [x] **精确绑定**：按 CPA 账号 ID（auth 文件相对路径）精确匹配，同邮箱的多个账号互不干扰。
- [x] **租期到期**：每个席位可配置 `expire-at`，到期后调度与 Portal 一律拒绝。
- [x] **配置校验**：严格解析，配置错误时拒绝全部请求并写日志。
- [ ] **单 key 限频**：限制单 key 并发与 RPM。
- [ ] **管理界面**：席位增删改、key 生成、用量审计。

---

## 3. 架构

### 3.1 插件 ABI

插件用 Go 编写，以 `-buildmode=c-shared` 编译为 `cpa-carpool.so`：

- 入口 `cliproxy_plugin_init(const cliproxy_host_api*, cliproxy_plugin_api*)`：保存宿主回调表，填写插件函数表（ABI 版本 1）。
- 插件 ID 取自文件名（去掉扩展名与 `-v版本号` 后缀），配置位于 `plugins.configs.<插件ID>`。
- 注册时声明能力：`scheduler`、`scheduler_across_priorities`、`management_api`；schema 版本 1。

```text
               客户端 (Codex CLI / Cursor / Claude Code)            买家浏览器
                            │                                          │
               ┌────────────▼──────────────────────────────────────────▼──────┐
               │                    CLIProxyAPI v8                            │
               │  access.api-keys ──► scheduler.pick ──┐   resource (GET) ──┐  │
               │                                       ▼                    ▼  │
               │                 ┌──────────── cpa-carpool.so ─────────────┐   │
               │                 │ plugin.go     配置加载 / 方法分发         │   │
               │                 │ scheduler.go  key → 席位 → 账号 ID        │   │
               │                 │ portal.go     /portal /api/status /reset │   │
               │                 │ host.go       宿主回调                    │   │
               │                 │ upstream.go   ChatGPT 额度接口 + 缓存     │   │
               │                 └──────────────────┬──────────────────────┘   │
               │   host.auth.list / get / get_runtime, host.routing.reset_cooldown, host.log
               └─────────────────────────────────────┼────────────────────────┘
                                                     ▼
                                   chatgpt.com/backend-api/wham/*
```

### 3.2 调度（`scheduler.pick`）

1. 配置无效：返回 `Reject`（`carpool_config_invalid`），**不区分调用方**。
2. 插件未启用或没有席位：返回 `Handled=false`，交给 CPA 默认调度。
3. 识别调用方：优先使用 CPA 提供的 `caller_scope`（`sha256("cli-proxy-api:caller-scope:v1\0" + key)`），其次是 `Authorization: Bearer` 与 `x-api-key` 请求头。未命中任何席位的 key 交给 CPA 默认调度。
4. 席位已到期：返回 `Reject`（`seat_expired`）。
5. 在候选账号中精确匹配席位的账号 ID（忽略大小写、路径分隔符风格和 `.json` 后缀），按 CPA 优先级、再按 ID 排序选第一个。
6. 没有匹配的候选：返回 `Reject`（`auth_unavailable`），**绝不回落到其他账号**。

> CPA 同一时间只调用一个调度插件（优先级最高的那个），返回 `Handled=false` 时直接使用内置调度。因此本插件不能与 key-router 等其他调度插件同时使用。

### 3.3 资源路由（Portal）

CPA 的插件资源路由不需要管理密钥、只接受 GET、按精确路径匹配，因此插件注册三个路由，均位于 `/v0/resource/plugins/<插件ID>/` 下：

| 路由 | 说明 |
| :--- | :--- |
| `GET /portal` | 嵌入式单页（`portal.html`），带 CSP、`Referrer-Policy: no-referrer`、`X-Content-Type-Options: nosniff` |
| `GET /api/status` | 席位状态（见 3.5） |
| `GET /api/reset?confirm=1` | 核销一张重置卡并解除冷却；不带 `confirm=1` 返回 400 |

`/api/status` 与 `/api/reset` 共同的前置检查：key 为空 → 400；配置无效 → 503；key 未配置 → 404；席位到期 → 403。

**核销流程**：全局互斥（已有核销在进行时返回 409）→ `host.auth.list` 找到席位账号 → `host.auth.get` 取凭据 → 实时查询可用重置卡 → 调用上游核销（失败时原始错误只写日志，返回 502 与通用提示）→ 清除额度缓存 → `host.routing.reset_cooldown` 解除 CPA 冷却（失败时返回成功但提示冷却未解除）。

### 3.4 宿主回调

| 回调 | 用途 |
| :--- | :--- |
| `host.auth.list` | 把席位的账号 ID 解析为 CPA 运行时的 `auth_index`（只包含基于认证文件的账号和纯运行时账号，不含 `config.yaml` 中的 API Key 账号） |
| `host.auth.get` | 读取账号凭据 JSON（`access_token`、`account_id`） |
| `host.auth.get_runtime` | 读取运行时状态（`unavailable`、`next_retry_after`）判断是否冷却中 |
| `host.routing.reset_cooldown` | 解除账号的额度与冷却状态，等同于管理接口 `POST /v8/management/routing/cooldown/reset` |
| `host.log` | 写入 CPA 日志，失败时回退到 stderr |

### 3.5 字段白名单

`/api/status` 只返回：`seat_name`、`quota_supported`、`upstream_usage.rate_limit`（primary / secondary 窗口的 `used_percent`、`reset_at`）、`credits_available`、`credits_list`（`title`、`status`、`granted_at`、`expires_at`、`redeemed_at`）、`is_cooling`、`expires_at`、`local_calls`、`input_tokens`、`output_tokens`、`cached_tokens`、`total_tokens`。

明确不返回：`email`、`account_id`、`user_id`、`organization_id`、`access_token`、`refresh_token`，以及上游的原始错误信息。

### 3.6 上游请求控制

- 每个账号的额度与重置卡查询结果缓存 30 秒（按 `auth_index`），核销成功后清除。
- 上游响应体读取上限：查询 1 MiB，核销错误 4 KiB。
- 请求头 `User-Agent` 默认模拟 Codex CLI，可通过 `upstream-user-agent` 覆盖。

### 3.7 配置

```yaml
plugins:
  configs:
    cpa-carpool:
      enabled: true
      priority: 100
      debug: false
      db-path: ""
      upstream-user-agent: ""
      users:
        - name: "TeamA-席位1"
          api-keys: ["sk-..."]
          primary: ["codex-....json"]
          expire-at: "2026-11-15T23:59:59+08:00"
```

校验规则：未知字段报错；每个席位至少一个 `api-keys` 与一个 `primary`；同一 key 不得出现在两个席位；`expire-at` 必须为 RFC3339。`priority` 与 `store` 由 CPA 读取，插件只做声明以通过严格解析。

---

## 4. 实施阶段

| 阶段 | 交付目标 | 核心工作项 | 状态 |
| :--- | :--- | :--- | :--- |
| **Phase 1** | 原型验证 | Python 微服务 + Caddy 反代 + 倒计时 + 一键核销 | 已完成 ✅ |
| **Phase 2** | 开源基础 | 仓库、SPEC、构建流程、协议 | 已完成 ✅ |
| **Phase 3** | Go 原生插件 | CPA v8 插件 ABI、宿主回调、测试与 CI | 已完成 ✅（v0.2.0） |
| **Phase 4** | 限频与接入引导 | 单 key 并发 / RPM 限制、客户端配置模板 | 计划中 |
| **Phase 5** | 管理界面与插件商店 | 席位管理 UI、`registry.json`、提交 CLIProxyAPI 插件商店 | 计划中 |
