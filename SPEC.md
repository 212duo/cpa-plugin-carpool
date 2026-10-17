# cpa-plugin-carpool 技术规格说明（SPEC）

> 项目名称：**cpa-plugin-carpool（CPA 拼车管家）**
> 目标宿主：[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8.0.30+（原生插件 ABI v1）
> 适用场景：个人与小团队的多租户账号分配、席位共享
> 当前开发版本：v0.5.0（待发布）

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

- **对管理员**：零数据库、零额外进程；在管理页面或 `config.yaml` 里把 key 绑到账号，支持到期自动失效与席位限额；
- **对使用者**：凭自己的 key 打开 Portal，查看 5 小时窗口与周窗口的额度和刷新倒计时、限额用量、历史 Token 用量，复制客户端配置，撞限时自助核销重置卡；
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
- [x] **限额用量**：展示席位的并发上限 / 当前并发、RPM 上限 / 近 1 分钟请求数。
- [x] **客户端接入引导**：Codex CLI、Claude Code、Cursor、OpenAI 兼容客户端的一键复制配置模板（见 3.10）。

### 2.2 管理端

- [x] **精确绑定**：按 CPA 账号 ID（auth 文件相对路径）精确匹配，同邮箱的多个账号互不干扰。
- [x] **租期到期**：每个席位可配置 `expire-at`，到期后调度与 Portal 一律拒绝。
- [x] **配置校验**：严格解析，配置错误时拒绝全部请求并写日志。
- [x] **席位限额**：按席位限制并发与 RPM，同一席位的 key 共享（见 3.6）。
- [x] **管理界面**：席位增删改、key 生成、账号绑定、限额与用量查看（见 3.9）。
- [x] **插件商店**：Release 提供商店要求的 zip 与 `checksums.txt`，附 registry 条目（见 3.10）。

---

## 3. 架构

### 3.1 插件 ABI

插件用 Go 编写，以 `-buildmode=c-shared` 编译为 `cpa-carpool.so`：

- 入口 `cliproxy_plugin_init(const cliproxy_host_api*, cliproxy_plugin_api*)`：保存宿主回调表，填写插件函数表（ABI 版本 1）。
- 插件 ID 取自文件名（去掉扩展名与 `-v版本号` 后缀），配置位于 `plugins.configs.<插件ID>`。
- 注册时声明能力：`scheduler`、`scheduler_across_priorities`、`management_api`，有席位设置了限额时再声明 `request_interceptor`、`request_lifecycle_plugin`（CPA 每次 `plugin.reconfigure` 都会重新读取能力）；schema 版本固定为 6（≥ 2 才有 `request.complete`；6 起管理接口的 JSON 不被 HTML 转义）。元数据包含 logo 与 `ConfigFields`。

```text
               客户端 (Codex CLI / Cursor / Claude Code)        浏览器 (买家 / 车主)
                            │                                          │
               ┌────────────▼──────────────────────────────────────────▼──────┐
               │                    CLIProxyAPI v8                            │
               │  access.api-keys ─► intercept_before ─► scheduler.pick       │
               │                     request.complete         │     routes    │
               │                           │                  │       │       │
               │                           ▼                  ▼       ▼       │
               │                 ┌──────────── cpa-carpool.so ─────────────┐   │
               │                 │ plugin.go     配置加载 / 方法分发         │   │
               │                 │ limiter.go    席位并发 / RPM            │   │
               │                 │ scheduler.go  key → 席位 → 账号 ID        │   │
               │                 │ portal.go     /portal /api/status /reset │   │
               │                 │ admin.go      /admin /carpool/*          │   │
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

CPA 的插件资源路由不需要管理密钥、只接受 GET、按精确路径匹配，因此插件注册六个路由，均位于 `/v0/resource/plugins/<插件ID>/` 下：

| 路由 | 说明 |
| :--- | :--- |
| `GET /portal` | 嵌入式单页（`portal.html`），带 CSP、`Referrer-Policy: no-referrer`、`X-Content-Type-Options: nosniff` |
| `GET /admin` | 嵌入式管理页面（`admin.html`），响应头同上，页面本身不含任何数据（见 3.9） |
| `GET /monitor` | 当前 Key 请求监控静态页面 |
| `GET /api/monitor` | 当前 Key 请求监控（见 3.11） |
| `GET /api/status` | 席位状态（见 3.5） |
| `GET /api/reset?confirm=1` | 核销一张重置卡并解除冷却；不带 `confirm=1` 返回 400 |

资源路由与管理路由都通过 `management.handle` 调用插件：路径以 `/v0/management/` 开头的走管理接口，其余走资源路由；资源前缀下的同名路径不会返回管理数据。

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

插件还实现以下由宿主调用的方法：

| 方法 | 用途 |
| :--- | :--- |
| `request.intercept_before` | 选账号之前执行席位限额（见 3.6），超出时 `Terminate` 返回 429 |
| `request.intercept_after` | 原样放行 |
| `request.complete` | 请求结束（成功 / 失败 / 被拒绝 / 取消）时释放并发名额 |

### 3.5 字段白名单

`/api/status` 只返回：`seat_name`、`provider`（席位账号类型，如 `codex`）、`public_base_url`、`limits`（`max_concurrency`、`rpm`、`active`、`recent_requests`）、`quota_supported`、`upstream_usage.rate_limit`（primary / secondary 窗口的 `used_percent`、`reset_at`）、`credits_available`、`credits_list`（`title`、`status`、`granted_at`、`expires_at`、`redeemed_at`）、`is_cooling`、`expires_at`、`usage_available`、`local_calls`、`input_tokens`、`output_tokens`、`cached_tokens`、`total_tokens`。

历史用量按当前 Key 查询，管理概览按席位当前绑定的 Key 汇总；调用次数包含采集到的成功与失败记录。读取采用只读连接和 3 秒查询期限，优先使用 `normalized_total_input_tokens`、`normalized_cache_read_tokens`，旧字段作为兼容回退。`usage_available: false` 表示数据库缺失或读取失败，页面显示不可用；只有成功查询且无匹配记录时才显示真实的 0。管理概览也返回 `usage_available`，独立于仅表示文件存在的 `usage_database`。错误详情只写宿主日志。

明确不返回：`email`、`account_id`、`user_id`、`organization_id`、`access_token`、`refresh_token`，以及上游的原始错误信息。

### 3.6 席位限额

- **配置**：顶层 `max-concurrency` / `rpm` 为默认值；席位的 `max-concurrency` / `rpm` 覆盖默认值（可选整数，显式 `0` 表示不限）；负数为配置错误。
- **作用范围**：按席位名计数，同一席位的所有 key 共享；计数保存在进程内存中，配置重载不清零，CPA 重启清零。没有任何席位设置限额时不注册请求钩子、不统计用量，并在从“有限额”切换到“无限额”时清空计数（此后收不到完成通知）。
- **执行位置**：`request.intercept_before`。按 `caller_scope` 或请求头识别席位（同 3.2）；未托管的 key、配置无效时一律放行（配置无效时由调度拒绝）。
- **并发**：放行时以 `RequestID` 登记，`request.complete` 时释放；同一 `RequestID` 重复拦截不重复计数，重复完成不重复释放。登记超过 30 分钟仍未完成的请求，在该席位达到上限时被回收。宿主 schema 版本低于 2（没有 `request.complete`）时不执行并发限制，只执行 RPM，并写一次警告日志。
- **RPM**：滑动 60 秒窗口，按放行时间计数，被拒绝的请求不计入。拒绝时 `Retry-After` 为最早一个请求移出窗口的剩余秒数。
- **拒绝响应**：HTTP 429、`Retry-After`，正文 `{"type":"error","error":{"type":"rate_limit_error","code":"seat_concurrency_limit|seat_rpm_limit","message":"..."}}`，同时兼容 OpenAI 与 Anthropic 客户端；消息只包含席位名与限额数值。

### 3.7 上游请求控制

- 每个账号的额度与重置卡查询结果缓存 30 秒（按 `auth_index`），核销成功后清除。
- 上游响应体读取上限：查询 1 MiB，核销错误 4 KiB。
- 请求头 `User-Agent` 默认模拟 Codex CLI，可通过 `upstream-user-agent` 覆盖。

### 3.8 配置

```yaml
plugins:
  configs:
    cpa-carpool:
      enabled: true
      priority: 100
      debug: false
      db-path: ""
      upstream-user-agent: ""
      public-base-url: ""
      max-concurrency: 0
      rpm: 0
      users:
        - name: "TeamA-席位1"
          api-keys: ["sk-..."]
          primary: ["codex-....json"]
          expire-at: "2026-11-15T23:59:59+08:00"
          max-concurrency: 2
          rpm: 30
```

校验规则：未知字段报错；每个席位至少一个 `api-keys` 与一个 `primary`；同一 key 不得出现在两个席位；席位名（含自动生成的 `seat-N`）不得重复；`expire-at` 必须为 RFC3339；限额不得为负数；`public-base-url` 必须是不含用户信息、查询参数和片段的 http(s) 地址。`priority` 与 `store` 由 CPA 读取，插件只做声明以通过严格解析。

### 3.9 管理界面

管理页面是资源路由（浏览器导航无法携带管理密钥），本身不含数据。页面要求输入 CPA 管理密钥，只保存在 `sessionStorage`，以 `Authorization: Bearer` 调用：

| 接口 | 提供方 | 用途 |
| :--- | :--- | :--- |
| `GET /v0/management/carpool/overview` | 插件 | 插件版本、已生效配置的 `config_revision` 摘要、启用状态、配置错误、是否旧宿主、是否找到用量库；每个席位的名称、key 数量（不返回 key）、`primary`、到期、限额用量、已加载账号（ID、类型、状态、冷却）、按 key 汇总的历史用量 |
| `GET /v0/management/carpool/auths` | 插件 | `host.auth.list` 的账号（ID、类型、label、email、状态）及已绑定的席位名，不含凭据 |
| `POST /v0/management/carpool/validate` | 插件 | 请求体为插件配置 JSON；转成 YAML 后按 3.8 的规则校验，返回 `{"valid":true,"seats":N,"config_revision":"..."}` 或 `{"valid":false,"error":"..."}`，不影响正在使用的配置 |
| `GET/PUT /v8/management/config/plugins/configs/<插件ID>` | CPA | 读取 / 替换插件配置段 |
| `GET/PUT /v8/management/config/access/api-keys` | CPA | 读取 / 写入客户端 key 列表 |

管理路由不带 `Menu`（CPA 会把带 `Menu` 的 GET 管理路由当作免鉴权资源）。

**保存流程**：检查持久化配置是否仍与编辑时一致 → 校验并取得 `config_revision` → 撤销旧配置中有、新配置中没有的 key → 轮询 `/v1/models`，确认被撤销的 key 返回 401 → `PUT` 插件配置段 → 轮询概览，确认已生效的 `config_revision` 与校验结果一致 → 加入新配置中尚未在 `access.api-keys` 里的 key。轮询最多 40 次、间隔 250ms，每次请求超时为 10 秒。任一步失败即停止并保留重试入口；已保存的配置成为下一次重试的基线。保存期间禁用编辑、刷新与退出。原认证列表为空时，加入 key 前要求确认；撤销 key 会清空列表时拒绝保存，提示先添加独立的车主 key。管理页不提供直接停用插件的操作，应先撤销席位再从 CPA 停用。配置检查不能代替服务端事务，应避免多处同时编辑。

前端校验：席位名非空且不重复、至少一个 key 且不与其他席位重复、至少一个账号、到期时间按浏览器时区转为 RFC3339（未修改时保留原字符串）、限额为非负整数。随机 key 由 `crypto.getRandomValues` 生成：`sk-carpool-` + 24 字节十六进制。

### 3.10 客户端接入引导与插件商店

- **接入引导**：在 Portal 前端生成，不新增服务端数据。网关地址取 `public_base_url`，为空时取页面所在源加上 `/v0/resource/plugins/` 之前的路径前缀；模型列表以买家 key 请求同源 `/v1/models`，与席位类型匹配的模型（按 id / owned_by 关键字）排在前面，失败时改为手动输入。模板：Codex CLI（`config.toml` 自定义 provider、`wire_api = "responses"`、环境变量）、Claude Code（`settings.json` 的 `env`）、Cursor（OpenAI Base URL 覆盖步骤）、OpenAI 兼容 / curl。
- **插件商店**：Release 为每个架构提供 `cpa-carpool_<版本>_linux_<amd64|arm64>.zip`（`cpa-carpool.so` 在 zip 根目录）与 `checksums.txt`（sha256sum 格式，覆盖 zip 与 tar.gz）。CPA 安装到 `plugins/linux/<架构>/cpa-carpool-v<版本>.so`，插件 ID 仍为 `cpa-carpool`。`plugin-store/registry-entry.json` 为提交到 `router-for-me/CLIProxyAPI-Plugins-Store` 的条目，`store_test.go` 按商店规则校验它与 Release 命名。

### 3.11 当前 Key 请求监控

- `/api/monitor` 仅接受 `X-Carpool-Key`，先校验 Key 与席位有效期，拒绝 query key、身份 / 账号 / Key hash 等筛选。时间范围 `range=1h|24h|7d`（默认 24h），`status=all|success|failed`（默认 all），`limit=1..500`（默认 100）。重复或未知参数拒绝。
- 服务端生成 SHA-256 Key hash，三条 SQL（汇总 / 最多 50 个模型 / 最近请求）均绑定 `api_key_hash = ?` 与时间上下界及状态条件。在同一个只读事务中查询，超时 3 秒，不修改数据库。摘要覆盖完整所选范围，列表截取不影响统计。
- 响应采用 `MonitorResponse` 类型白名单：`generated_at_ms/range/status/summary/models/events/truncated`。记录不返回数据库 ID、request ID、认证信息、请求正文、原始错误、来源或网络标识；仅使用 `requested_model`，不读取内部 resolved model。模型名限制格式 / 长度，推理和服务字段限制枚举，耗时与 HTTP 码校验范围。字段缺失返回 null 或未记录；必需表结构缺失返回通用 503，无部分数据。
- 页面 Key 只存在内存，请求头发送，片段在请求前清除。切换 Key / 筛选会中止旧请求，并用代次校验防止旧响应回填；退出或出错时清空数据。每 5 秒刷新，隐藏时暂停，单次网络请求最多 10 秒。
- 所有页面 `no-store`，`Referrer-Policy: no-referrer`、`X-Frame-Options: DENY`，CSP 限制同源连接、禁止嵌入 / base / 表单提交。监控接口不记录数据库原始错误，以免转换失败时把敏感单元格写入日志。
- Token 算法复用历史统计；不提供没有可靠来源的费用、原始错误详情或跨 Key 聚合。数据来自已有 Manager Plus 采集，延迟与保留范围由采集器决定。

---

## 4. 实施阶段

| 阶段 | 交付目标 | 核心工作项 | 状态 |
| :--- | :--- | :--- | :--- |
| **Phase 1** | 原型验证 | Python 微服务 + Caddy 反代 + 倒计时 + 一键核销 | 已完成 ✅ |
| **Phase 2** | 开源基础 | 仓库、SPEC、构建流程、协议 | 已完成 ✅ |
| **Phase 3** | Go 原生插件 | CPA v8 插件 ABI、宿主回调、测试与 CI | 已完成 ✅（v0.2.0） |
| **Phase 4** | 限频与接入引导 | 席位并发 / RPM 限制、客户端配置模板 | 已完成 ✅（v0.3.0） |
| **Phase 5** | 管理界面与插件商店 | 席位管理页面、管理接口、商店发布物与 registry 条目 | 功能已实现，v0.4.0 待发布；商店收录需单独提交 |
| **Phase 6** | 请求监控与宽屏页面 | 当前 Key 隔离、请求记录与模型汇总、响应式布局 | 功能已实现，v0.5.0 待发布 |
