# 参与贡献

感谢你愿意改进 cpa-plugin-carpool！提交 Issue 或 PR 前请先读一下这份说明。

## 开发环境

- Go 1.26 或更高版本（与 CLIProxyAPI v8 一致）。
- 跑单元测试**不需要** C 编译器：cgo 相关代码只在 `main.go` 里，`CGO_ENABLED=0 go test ./...` 也能通过。
- 编译插件本身需要 cgo。推荐用 `sh build.sh`，它在 `golang:1.26-bookworm` 容器里编译，产物与官方 CPA 镜像的 glibc 一致。

```bash
go test ./...
go vet ./...
gofmt -l .          # 应当没有输出
node --test scripts/admin.test.cjs scripts/portal.test.cjs scripts/monitor.test.cjs  # Node.js 24，页面行为
sh build.sh test    # 在 Docker 里测试并编译 out/cpa-carpool.so
```

## 代码结构

| 文件 | 职责 |
| :--- | :--- |
| `main.go` | C ABI 入口与宿主回调桥接（唯一使用 cgo 的文件） |
| `plugin.go` | 方法分发、配置加载、注册信息 |
| `scheduler.go` | 席位编译、key 识别、独占调度 |
| `limiter.go` | 席位并发 / RPM 限制（`request.intercept_before` 与 `request.complete`） |
| `portal.go` | 资源路由分发、Portal 接口 |
| `admin.go` | 管理接口（概览、账号列表、配置校验）与管理页面 |
| `admin_config.go` / `admin_save.go` / `admin_diagnostics.go` | 持久化配置接口、后台保存任务与部署诊断 |
| `host.go` | CPA 宿主回调封装 |
| `upstream.go` | ChatGPT 额度与重置卡接口 |
| `monitor_cache.go` / `monitor_store.go` | 按 Key 与配置隔离的有界缓存、并发合并与只读连接池 |
| `monitor.go` / `monitor.html` | 当前 Key 的请求监控、字段白名单与宽屏看板 |
| `stats.go` | 读取 CPA Manager Plus 用量数据库 |
| `portal.html` / `admin.html` | 买家 Portal 与车主管理页面（编译时嵌入，不依赖外部资源） |
| `plugin-store/registry-entry.json` | 提交到 CLIProxyAPI 插件商店的 registry 条目 |

## 设计原则

修改代码时请保持这几条不变：

1. **出错即拒绝。** 无法确定调用方是谁、配置有问题、账号不可用时，宁可拒绝请求，也不要交给 CPA 默认调度。
2. **精确匹配。** 账号绑定只做精确比较，不引入前缀、后缀或邮箱模糊匹配。
3. **白名单输出。** 返回给买家的数据只增加明确需要的字段；上游原始响应和错误不能直接透传。
4. **日志里不出现 key。** 日志只记录席位名和账号 ID；概览、诊断和任务状态不返回 key。配置与认证列表仅向持有管理密钥的调用方提供。
5. **页面不含数据。** `portal.html`、`admin.html` 是免鉴权的资源，只能是静态页面，数据一律通过需要 key 或管理密钥的接口读取。

## 测试

- 新功能和 bug 修复都需要配套测试。测试里用 `installFakeHost` 模拟 CPA 宿主回调，用 `installFakeUpstream` 模拟 ChatGPT 接口，不要访问真实服务。
- 修改配置项时同步更新 `config.example.yaml`，`TestConfigExampleIsValid` 会校验它。
- 中英文 README（`README.md`、`README.en.md`）内容要保持一致，改了一份请同步另一份。
- `docs/images/portal.png`、`portal-guide.png`、`admin.png`、`monitor.png` 是用演示数据渲染的截图，界面有明显变化时请重新截图；截图里不能出现真实的 key、邮箱或账号信息。
- 修改 `plugin-store/registry-entry.json` 或 Release 打包方式时，`store_test.go` 会按 CLIProxyAPI 插件商店的规则检查它们。

### 真实宿主管理页联调

`scripts/admin.integration.cjs` 使用 Playwright Chromium，针对隔离的 CPA v8.0.30 测试实例运行。实例必须使用管理密钥 `review-v060-management-demo`、独立车主 key `sk-demo-owner`、名为 `claude-demo.json` 的合成认证文件，并配置一个名为 `synthetic-test` 的本地测试模型。不要使用真实账号或生产配置；脚本会修改并清理测试席位。

设置 `CARPOOL_INTEGRATION_TEST=1`、`CARPOOL_TEST_HOST`（测试 CPA 地址），确保可 `require('playwright')`，然后执行 `node scripts/admin.integration.cjs`。可通过 `CARPOOL_PLAYWRIGHT_PATH` 指定已安装的 Playwright 模块。脚本在本机 28419 端口启动只代理 `/gateway/v0/`、`/gateway/v1/` 的测试反代，验证同源 iframe、后台保存、暂停与恢复、续期、配置读取失败、关闭页面恢复和手机布局，并更新演示截图。

### 大数据量监控压测

默认测试跳过百万条压测。Linux 使用以下命令；数据库与报告可通过 `CARPOOL_MONITOR_STRESS_DB` / `CARPOOL_MONITOR_STRESS_REPORT` 指定，仅允许使用带合成数据标记的测试库，勿指向真实数据。

```bash
CARPOOL_MONITOR_STRESS=1 CARPOOL_MONITOR_STRESS_SUSTAINED=1 go test -run '^TestMonitorLargeDataLoad$' -count=1 -timeout 15m -v .
```

场景与测量边界见 [docs/monitor-performance.md](docs/monitor-performance.md)。`monitor_baseline_test.go` 保留优化前的三查询实现作为对照，不编译进插件。

## 提交 PR

- 提交信息使用 [Conventional Commits](https://www.conventionalcommits.org/zh-hans/) 风格，例如 `fix(portal): ...`、`feat(scheduler): ...`。
- 行为变化写进 `CHANGELOG.md` 的 `[Unreleased]` 部分。
- 不要提交任何真实的 API Key、token、邮箱或 account_id，包括测试数据和截图。

## 发布流程（维护者）

1. 更新 `types.go` 中的 `PluginVersion`，把 `CHANGELOG.md` 的 `[Unreleased]` 改为新版本号和日期。
2. 合并到 `main` 后打 tag：`git tag v0.x.y && git push origin v0.x.y`。
3. Release 工作流会校验 tag 与 `PluginVersion` 一致，编译 amd64 / arm64，打包许可证并创建 GitHub Release。产物包括 `cpa-carpool_<版本>_linux_<架构>.tar.gz`、插件商店使用的同名 `.zip`（`cpa-carpool.so` 在 zip 根目录）和 `checksums.txt`。

### 提交到 CLIProxyAPI 插件商店

[官方插件商店](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)只维护 `registry.json`，CPA 从本仓库最新的 Release 下载安装，所以**只需要首次收录时提交一次**，之后发布新 Release 即可更新。

1. 先按上面的流程发布一个 Release，确认其中有 `cpa-carpool_<版本>_linux_amd64.zip`、`cpa-carpool_<版本>_linux_arm64.zip` 和 `checksums.txt`。
2. Fork 商店仓库，把 `plugin-store/registry-entry.json` 的内容追加到 `registry.json` 的 `plugins` 数组里，只修改这一个文件。
3. 提交 PR，说明里写明：仓库地址、最新 Release 的 tag（如 `v0.4.0`）、Release 中 zip 与 `checksums.txt` 的链接、插件提供的能力（调度插件 + 请求拦截 + 管理页面）。

商店条目中的 `logo` 指向本仓库 `main` 分支的 `docs/images/logo.png`，请勿移动这个文件。
