# 更新日志

本项目的所有重要变更都记录在这里。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

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

[Unreleased]: https://github.com/212duo/cpa-plugin-carpool/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/212duo/cpa-plugin-carpool/releases/tag/v0.2.0
[0.1.0]: https://github.com/212duo/cpa-plugin-carpool/commits/main
