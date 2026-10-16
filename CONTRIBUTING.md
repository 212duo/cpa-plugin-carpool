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
sh build.sh test    # 在 Docker 里测试并编译 out/cpa-carpool.so
```

## 代码结构

| 文件 | 职责 |
| :--- | :--- |
| `main.go` | C ABI 入口与宿主回调桥接（唯一使用 cgo 的文件） |
| `plugin.go` | 方法分发、配置加载 |
| `scheduler.go` | 席位编译、key 识别、独占调度 |
| `portal.go` | Portal 路由与接口 |
| `host.go` | CPA 宿主回调封装 |
| `upstream.go` | ChatGPT 额度与重置卡接口 |
| `stats.go` | 读取 CPA Manager Plus 用量数据库 |

## 设计原则

修改代码时请保持这几条不变：

1. **出错即拒绝。** 无法确定调用方是谁、配置有问题、账号不可用时，宁可拒绝请求，也不要交给 CPA 默认调度。
2. **精确匹配。** 账号绑定只做精确比较，不引入前缀、后缀或邮箱模糊匹配。
3. **白名单输出。** 返回给买家的数据只增加明确需要的字段；上游原始响应和错误不能直接透传。
4. **日志里不出现 key。** 日志只记录席位名和账号 ID。

## 测试

- 新功能和 bug 修复都需要配套测试。测试里用 `installFakeHost` 模拟 CPA 宿主回调，用 `installFakeUpstream` 模拟 ChatGPT 接口，不要访问真实服务。
- 修改配置项时同步更新 `config.example.yaml`，`TestConfigExampleIsValid` 会校验它。
- 中英文 README（`README.md`、`README.en.md`）内容要保持一致，改了一份请同步另一份。
- `docs/images/portal.png` 是用演示数据渲染的 Portal 截图，Portal 界面有明显变化时请重新截图；截图里不能出现真实的 key、邮箱或账号信息。

## 提交 PR

- 提交信息使用 [Conventional Commits](https://www.conventionalcommits.org/zh-hans/) 风格，例如 `fix(portal): ...`、`feat(scheduler): ...`。
- 行为变化写进 `CHANGELOG.md` 的 `[Unreleased]` 部分。
- 不要提交任何真实的 API Key、token、邮箱或 account_id，包括测试数据和截图。

## 发布流程（维护者）

1. 更新 `types.go` 中的 `PluginVersion`，把 `CHANGELOG.md` 的 `[Unreleased]` 改为新版本号和日期。
2. 合并到 `main` 后打 tag：`git tag v0.x.y && git push origin v0.x.y`。
3. Release 工作流会校验 tag 与 `PluginVersion` 一致，编译 amd64 / arm64，打包许可证并创建 GitHub Release。
