# 更新日志

本项目的所有重要变更都记录在这里。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- 0.5 请求监控页面 `/monitor` 与 `/api/monitor`：当前 Key 的调用、成功率、失败、Token、模型汇总与近期记录；支持 1 小时 / 24 小时 / 7 天、成功 / 失败筛选，每 10 秒刷新，最多显示 500 条记录。
- 请求监控严格按服务端认证后的 Key 查询；仅返回请求模型与白名单统计字段，不返回账号、其他 Key、请求内容或上游错误。过期与无效 Key 拒绝访问，数据库不可用返回明确提示。
- 浏览器切换 Key、退出或读取失败时清空旧数据；防止较早请求覆盖新 Key 的结果，后台页面暂停轮询。

### 变更

- 买家 Portal 与管理页加宽至 1440px；Portal 的席位、额度、历史用量并排展示，客户端配置默认折叠，手机自动单列。
- 监控看板自适应宽屏与手机；页面禁止被嵌入并禁用缓存，所有资源均由插件内置。
- Token 历史与监控共用标准化输入 / 缓存读取算法，监控只读现有 Manager Plus 数据，不新增采集进程。

### 性能与隔离

- 当前 Key 的近期记录缓存 10 秒、汇总缓存 30 秒，页面分别显示更新时间；合并相同并发查询，复用连接，汇总与模型统计合并成一次扫描。
- 缓存绑定 Key hash、筛选条件、配置快照与数据库文件身份；鉴权不缓存，配置更新取消旧查询并清空缓存，返回前再次检查有效期与撤销状态。
- 缓存数据上限 20 MiB，LRU 淘汰；全插件同时执行的监控数据查询最多 2 条，排队与执行共用 3 秒期限，失败刷新退避。
- 增加幂等数据库索引脚本、隔离与缓存并发测试、可复现的百万条 HTTP 压测；插件仍只读数据库。

## [0.4.0] - 待发布

### 新增

- 车主管理页面 `GET /v0/resource/plugins/<插件ID>/admin`：凭 CPA 管理密钥登录，增删改席位、生成随机 key、从 CPA 已加载的账号中选择绑定、设置租期与限额，查看账号加载 / 冷却状态、限额使用与历史用量，复制买家 Portal 链接。
- 管理页面保存时先由插件按严格规则校验，再通过 CPA 的 `/v8/management/config` 接口依次撤销被删除的 key、写入 `plugins.configs.<插件ID>`、加入新 key，写回 `config.yaml` 后由 CPA 自动重载。
- 管理接口（挂在 CPA Management API 下，需要管理密钥）：`GET /v0/management/carpool/overview`、`GET /v0/management/carpool/auths`、`POST /v0/management/carpool/validate`。
- 插件元数据新增 logo 与 `ConfigFields`，CPA 管理面板可以展示各配置项的说明。
- 插件商店发布物：Release 额外提供 `cpa-carpool_<版本>_linux_<架构>.zip`（`cpa-carpool.so` 位于 zip 根目录）和 `checksums.txt`，符合 CLIProxyAPI 插件商店的安装要求；`plugin-store/registry-entry.json` 是提交到商店的 registry 条目。
- 测试覆盖 registry 条目与 Release 产物命名，防止与商店规则不一致。

### 变更

- Release 的校验文件由 `SHA256SUMS` 改名为 `checksums.txt`，同时覆盖 zip 与 tar.gz。
- 未设置任何席位限额时，不注册请求拦截与完成通知，减少无用的插件调用。
- 更新 README 截图与架构图。

### 修复

- 历史统计缺失或读取失败时显示“统计暂不可用”，不再伪装成 0；Portal 明确标注按当前 Key 统计，管理页按席位汇总。
- 修复少量 Token 被显示成 `0.000M` 的问题，并兼容 Manager Plus 的标准化输入量与缓存读取量字段。

- 管理页等待旧 key 的撤销与新席位绑定实际生效后，再继续保存，避免 CPA 异步重载期间出现权限空档。
- 阻止撤销最后一个客户端 key，以免清空 `access.api-keys` 意外关闭 CPA 认证；停用插件前需先撤销买家席位。
- 一次错误登录只发起一次鉴权请求，避免过早触发 CPA 的管理接口封禁。
- 保存期间锁定编辑，支持部分保存后的重试和补齐缺失的认证 key；保存前检查配置是否已被其他页面修改。
- 修复首次配置没有 `users` 字段时无法保存席位的问题；无效的默认限额不再被静默改成不限。
- 用量数据库缺失或路径指向目录时不再显示已找到数据库，支持包含空格和 `#` 的数据库路径。
- 增加管理页行为回归测试，并纳入 CI 和 Release 检查。

## [0.3.0] - 2026-10-17

### 新增

- 席位限额：`max-concurrency`（并发请求数）与 `rpm`（每分钟请求数），可在顶层设默认值、在席位上覆盖，`0` 表示不限。同一席位的多个 key 共享限额。
- 超出限额的请求在选账号之前被拒绝，返回 HTTP 429、`Retry-After` 与 `rate_limit_error`（`seat_concurrency_limit` / `seat_rpm_limit`），客户端会自动退避重试。
- 插件通过请求拦截器执行席位限额，并通过 `request.complete` 释放并发名额；结束通知丢失的请求最多 30 分钟后自动回收名额。宿主不支持请求完成通知时只执行 RPM 限制并在日志中提示。
- Portal 显示当前并发与近 1 分钟请求数。
- Portal 客户端接入引导：按买家的 key 生成 Codex CLI、Claude Code、Cursor、OpenAI 兼容客户端的配置，模型从网关的 `/v1/models` 读取并优先列出与席位类型匹配的模型，一键复制。
- `public-base-url` 配置项：指定接入引导中展示的网关地址。
- `/api/status` 新增 `provider`、`public_base_url`、`limits` 字段。

### 变更

- 席位名不能重复（包括留空时自动生成的 `seat-N`），重复视为配置错误。席位名用于限额计数、日志和管理页面。

## [0.2.0] - 2026-10-16

首个可用于 CLIProxyAPI v8 的原生插件版本。

### 新增

- CPA v8 原生动态库插件（`cpa-carpool.so`），实现调度插件与资源路由。
- 独占调度：key 只能用到绑定的账号；账号不可用、席位到期时直接拒绝，不回落到其他账号。
- 租期到期：席位支持 `expire-at`（RFC3339），调度和 Portal 同时生效。
- 买家 Portal：额度窗口与刷新倒计时、重置卡列表、冷却状态、历史 Token 用量、租期到期时间。
- 自助核销重置卡，并通过宿主回调 `host.routing.reset_cooldown` 立即解除 CPA 中该账号的冷却。
- 通过宿主回调读取账号凭据（`host.auth.get`）和运行时冷却状态（`host.auth.get_runtime`），插件不再直接读写 auth 目录。
- 插件日志通过 `host.log` 写入 CPA 日志；`debug: true` 时记录每次调度的决定。
- `upstream-user-agent` 配置项。
- 非 Codex 账号的席位也能使用调度绑定，Portal 会提示该类型不支持额度查询。
- 单元测试（模拟 CPA 宿主与 ChatGPT 上游）、GitHub Actions CI 与 Release 流程、linux/amd64 与 linux/arm64 预编译包。
- 英文版 README（`README.en.md`），README 中加入 Portal 截图与架构图。

### 安全

- 配置严格解析：拼错的字段名、缺少 key 或账号、重复的 key、格式错误的 `expire-at` 都视为配置错误。
- 配置错误时拒绝全部请求，而不是让买家 key 落到 CPA 默认调度。
- 账号 ID 改为精确匹配（忽略大小写和 `.json` 后缀），不再做后缀匹配。
- Portal 的 status / reset 接口都会检查席位是否到期。
- 上游接口的原始错误只写进 CPA 日志，买家只看到通用提示。
- 买家 key 改用 `X-Carpool-Key` 请求头传递，分享链接改用 `#key=`。
- 每个账号的额度查询结果缓存 30 秒；重置串行执行，避免重复核销。
- Portal 输出全部转义，并增加 CSP、`Referrer-Policy` 等响应头。

### 修复

- 插件入口 `cliproxy_plugin_init` 改为 CPA v8 的双参数签名，修复插件无法加载的问题。
- 修复配置解析层级错误（CPA 只传入 `plugins.configs.<插件ID>` 这一段）导致绑定全部失效的问题。
- 修复资源路由前缀写死为 `carpool` 导致 Portal 404 的问题；插件 ID 现在取自 `.so` 文件名。
- 注册 `api/status` 与 `api/reset` 两个资源路由；`api/reset` 改为 `GET ?confirm=1`（CPA 资源路由只支持 GET）。
- 修复 Linux 上账号文件名含大写字母时找不到账号的问题。
- 修复编译错误（缺少 `crypto/sha256` 引用、多余的 `net/url` 引用）。

### 移除

- `auth-dir` 配置项：凭据改由 CPA 宿主回调提供。
- 依赖本地 `../CLIProxyAPI` 源码的 `replace`：直接依赖 `github.com/router-for-me/CLIProxyAPI/v8 v8.0.30`。

## [0.1.0] - 2026-10-16

- 独立原型（Python + Caddy），验证倒计时算法、一键核销与字段脱敏。该原型未包含在本仓库中。

[Unreleased]: https://github.com/212duo/cpa-plugin-carpool/compare/7e45ad9...codex/private-request-monitor
[0.4.0]: https://github.com/212duo/cpa-plugin-carpool/commit/7e45ad9
[0.3.0]: https://github.com/212duo/cpa-plugin-carpool/commit/f05b126
[0.2.0]: https://github.com/212duo/cpa-plugin-carpool/releases/tag/v0.2.0
[0.1.0]: https://github.com/212duo/cpa-plugin-carpool/commits/main
