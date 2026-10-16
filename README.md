<div align="center">

# 🚗 cpa-plugin-carpool（CPA 拼车管家）

**为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）打造的轻量级多租户席位插件**

*Key 与账号一对一绑定 · 买家自助查额度 · 额度刷新倒计时 · 撞限自助核销重置卡 · 配置出错即拒绝*

[![CI](https://github.com/212duo/cpa-plugin-carpool/actions/workflows/ci.yml/badge.svg)](https://github.com/212duo/cpa-plugin-carpool/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
![CPA](https://img.shields.io/badge/CLIProxyAPI-v8.0.30%2B-informational)

**简体中文** · [English](./README.en.md)

[快速开始](#-快速开始) · [配置参考](#-配置参考) · [安全设计](#-安全设计) · [已知限制](#-已知限制) · [技术规格 (SPEC.md)](./SPEC.md) · [更新日志](./CHANGELOG.md)

</div>

> [!WARNING]
> **社区项目，与 CLIProxyAPI 官方、OpenAI、Anthropic 均无关联。** 多数 AI 服务的使用条款禁止共享或转让账号，使用本插件可能导致你的账号被限制或封禁；查询额度和核销重置卡依赖 ChatGPT 未公开的接口，随时可能失效。请先阅读文末的[免责声明](#-免责声明)，风险由使用者自行承担。

---

## 💡 它解决什么问题

几个人分摊一个 ChatGPT Team 席位、或者在小团队里分配账号时，常见的做法各有短板：

* **Sub2API / NewAPI 一类方案偏重**：要额外部署数据库、Redis 和独立服务，对只有几个账号的场景来说运维成本太高；
* **纯 CPA 缺少多租户能力**：没有面向使用者的额度查询页面；同一邮箱下有多个账号（如 Team 席位 + 个人 Pro）时，按邮箱匹配的路由插件容易串号；
* **撞限后只能找管理员**：使用者遇到 429 时，只能等管理员登录后台查额度、手动重置。

本插件直接以原生动态库的形式运行在 CPA 内部，不需要数据库和额外进程。管理员在配置里把 key 绑到具体账号，使用者拿着自己的 key 打开 Portal 就能查额度、看倒计时，撞限时自己核销重置卡。

## ✨ 功能

| 功能 | 说明 |
| :--- | :--- |
| **独占调度** | 每个 key 只能用到绑定的账号；账号冷却或不可用时直接拒绝，**绝不回落到其他账号** |
| **精确绑定** | 按 CPA 账号 ID 精确匹配，同邮箱的 Team 席位与个人账号不会串 |
| **租期到期** | 每个席位可设 `expire-at`，到期后调度和 Portal 一律拒绝 |
| **配置出错即拒绝** | 配置写错时插件拒绝所有请求并在 CPA 日志里说明原因，而不是让买家 key 落到默认调度、用到整个号池 |
| **买家 Portal** | 5 小时窗口 / 周窗口已用比例与刷新倒计时、重置卡列表、历史 Token 用量 |
| **自助核销重置卡** | 核销一张官方重置卡，并通过 CPA 宿主接口立即解除该账号的冷却 |
| **最小化暴露** | 返回给买家的数据只保留白名单字段，不包含邮箱、account_id、token |

<p align="center">
  <img src="docs/images/portal.png" alt="买家 Portal：席位状态、额度窗口与刷新倒计时、Token 用量、重置卡列表" width="460">
  <br>
  <sub>买家 Portal（演示数据）</sub>
</p>

## 🔧 工作原理

<p align="center">
  <img src="docs/images/architecture.svg" alt="架构图：客户端请求经 CPA 认证后进入调度，cpa-carpool 按 key 只选绑定账号，否则拒绝；未托管的 key 交回 CPA 内置调度。Portal 通过资源路由提供，经宿主回调读取凭据与冷却状态，并查询 chatgpt.com 额度" width="100%">
</p>

* **调度**：插件注册为 CPA 的调度插件。请求到达时，插件根据调用方的 key（CPA 提供的 `caller_scope`，或 `Authorization` / `x-api-key` 头）找到对应席位，只在该席位绑定的账号里选择。不在配置里的 key（比如你自己的）交回 CPA 默认调度。
* **Portal**：通过 CPA 的插件资源路由提供页面和两个接口。账号凭据、运行时冷却状态都通过 CPA 宿主回调读取，插件不直接读写 auth 目录。

## 📋 兼容性

| 项目 | 要求 |
| :--- | :--- |
| CLIProxyAPI | **v8.0.30 及以上**（插件 ABI v1，按 SDK v8.0.30 编译和测试） |
| 平台 | Linux amd64 / arm64，glibc ≥ 2.36（官方 Docker 镜像 `debian:bookworm` 满足） |
| 调度绑定 | 任意提供商的账号（包括 `config.yaml` 里配置的 API Key 账号） |
| Portal 查询 | 基于认证文件的账号（OAuth 登录生成的凭据文件） |
| 额度查询与重置卡 | 仅 **Codex（ChatGPT）** 账号；其他文件账号的 Portal 只显示席位状态和 Token 用量 |
| 历史 Token 用量 | 可选，需要 [CPA Manager Plus](https://github.com/seakee/CPA-Manager-Plus) 的 `usage.sqlite` |

## 🚀 快速开始

### 1. 获取插件

**下载预编译版本**：在 [Releases](https://github.com/212duo/cpa-plugin-carpool/releases) 下载对应架构的 `cpa-carpool_<版本>_linux_<amd64|arm64>.tar.gz`，解压得到 `cpa-carpool.so`，用 `SHA256SUMS` 校验。

**或者自己编译**（需要 Docker，产物与官方镜像的工具链和 glibc 一致）：

```bash
git clone https://github.com/212duo/cpa-plugin-carpool.git
cd cpa-plugin-carpool
sh build.sh          # 输出 out/cpa-carpool.so；加 test 参数会先跑测试
```

### 2. 放进 CPA 的插件目录

把 `cpa-carpool.so` 放到 CPA 配置里 `plugins.dir` 指向的目录（默认是工作目录下的 `plugins/`，也可以放在 `plugins/linux/amd64/` 这样的平台子目录）。

> 插件 ID 就是文件名去掉扩展名，所以**请保持文件名为 `cpa-carpool.so`**（或 `cpa-carpool-v0.2.0.so` 这种带版本号的形式），下面的配置段名要和它一致。

使用官方 Docker 镜像时，可以把插件目录挂载进去：

```bash
docker run -d -p 8317:8317 \
  -v $PWD/config.yaml:/CLIProxyAPI/config.yaml \
  -v $PWD/auths:/root/.cli-proxy-api \
  -v $PWD/plugins:/CLIProxyAPI/plugins \
  eceasy/cli-proxy-api:latest
```

### 3. 配置

在 CPA 的 `config.yaml` 里加入（完整示例见 [config.example.yaml](./config.example.yaml)）：

```yaml
access:
  api-keys:
    - "sk-owner-xxx"          # 你自己的 key，不写进 carpool，走 CPA 默认调度
    - "sk-buyer-alice"        # 买家的 key 也必须在这里，否则 CPA 在调度前就拒绝了

plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-carpool:
      enabled: true
      priority: 100
      users:
        - name: "TeamA-席位1"
          api-keys: ["sk-buyer-alice"]
          primary: ["codex-xxxxxxxx-a1@example.com-team.json"]
          expire-at: "2026-11-15T23:59:59+08:00"
```

CPA 重载配置时会把新配置推给插件，通常不需要重启。启动后在 CPA 日志里能看到插件加载信息；配置有误时会出现 `[carpool] invalid plugin configuration ...`。

### 4. 把 Portal 链接发给买家

```text
https://你的域名/v0/resource/plugins/cpa-carpool/portal#key=sk-buyer-alice
```

推荐用 `#key=` 的形式：`#` 后面的内容不会发给服务器，不会出现在访问日志里。旧的 `?key=` 形式仍然可用，页面打开后会把它从地址栏移除。

## 🧰 配置参考

配置写在 `plugins.configs.cpa-carpool` 下。插件使用**严格解析**：拼错的字段名会被当作错误，而不是被静默忽略。

| 字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `enabled` | bool | `false` 时插件不接管任何请求（所有 key 都走 CPA 默认调度） |
| `priority` | int | CPA 读取的插件优先级，同时有多个调度插件时只有最高的那个生效 |
| `debug` | bool | 在 CPA 日志里记录每次调度的决定（只记录席位名，不记录 key） |
| `db-path` | string | CPA Manager Plus 的 `usage.sqlite` 路径；留空时自动探测，找不到则用量显示为 0 |
| `upstream-user-agent` | string | 请求 ChatGPT 额度接口时使用的 User-Agent，留空使用内置默认值 |
| `users[].name` | string | 席位名，显示在 Portal 上；留空时为 `seat-N` |
| `users[].api-keys` | []string | 分配给该席位的 key，**至少一个**，同一个 key 不能出现在两个席位里 |
| `users[].primary` | []string | 绑定的 CPA 账号 ID，**至少一个**，精确匹配（忽略大小写和 `.json` 后缀） |
| `users[].expire-at` | string | 可选，RFC3339 格式的到期时间，如 `2026-11-15T23:59:59+08:00` |

**配置校验失败时插件会拒绝所有请求**（包括你自己的 key），并在 CPA 日志里打印原因。这是有意为之：如果让插件"跳过"，买家的 key 会落到 CPA 默认调度，反而能用到整个号池。

### 如何找到账号 ID（`primary`）

账号 ID 是该账号的凭据文件**相对 CPA auth 目录的路径**（默认 auth 目录是 `~/.cli-proxy-api`，由 `oauth.auth-dir` 配置）：

* 直接看 auth 目录里的文件名，例如 `codex-1a2b3c4d-alice@example.com-team.json`；
* 文件在子目录里时要带上子目录，例如 `team/codex-1a2b3c4d-alice@example.com-team.json`；
* 或者调用 CPA 管理接口 `GET /v0/management/auth-files`，使用返回结果里的文件名。

### 绑定方式

* **一个席位发多个 key**：`api-keys` 里写多个；
* **多人拼一个账号**：多个席位的 `primary` 写同一个账号 ID，各自有独立的 key 和到期时间；
* **一个席位绑多个账号**：`primary` 写多个，调度时在其中按 CPA 优先级选择；Portal 显示和重置的是第一个已加载的账号。

## 🌐 Portal 与接口

所有路由都挂在 `/v0/resource/plugins/cpa-carpool/` 下，只接受 GET（CPA 资源路由的限制）。买家 key 通过 `X-Carpool-Key` 请求头传递（也接受 `key` 查询参数）。

| 路由 | 说明 |
| :--- | :--- |
| `GET /portal` | 买家自助页面 |
| `GET /api/status` | 席位状态：额度窗口、重置卡、冷却状态、Token 用量、到期时间 |
| `GET /api/reset?confirm=1` | 核销一张重置卡并解除 CPA 冷却；不带 `confirm=1` 不会执行 |

```bash
curl -H "X-Carpool-Key: sk-buyer-alice" \
  https://你的域名/v0/resource/plugins/cpa-carpool/api/status
```

## 🔒 安全设计

* **出错即拒绝**：配置无法解析或校验失败时拒绝全部请求；席位到期、账号不可用时拒绝该席位的请求，从不回落到其他账号。
* **精确匹配**：账号 ID 精确比较，不做前缀、后缀或邮箱模糊匹配。
* **字段白名单**：Portal 只返回额度窗口、重置卡（标题/状态/时间）、席位名、冷却状态和 Token 用量；邮箱、account_id、token 不会出现在响应里。上游接口的原始错误只写进 CPA 日志，买家只看到通用提示。
* **Key 不进 URL**：Portal 用请求头传 key，分享链接用 `#key=`，避免 key 出现在网关和反代的访问日志里。
* **限制上游请求频率**：每个账号的额度查询结果缓存 30 秒，买家频繁刷新也不会频繁请求 ChatGPT；重置串行执行，连点不会重复核销。
* **不碰凭据文件**：通过 CPA 宿主回调读取凭据和冷却状态，插件不直接读写 auth 目录。
* **页面加固**：Portal 输出内容全部转义，并带有 CSP、`Referrer-Policy: no-referrer` 等响应头。

Portal 本身不需要 CPA 管理密钥，访问控制完全依赖 key 的保密性。请务必通过 **HTTPS** 对外提供服务，并使用足够长的随机 key（例如 `openssl rand -hex 24`）。漏洞报告方式见 [SECURITY.md](./SECURITY.md)。

## 🚧 已知限制

* **不能和 key-router 等其他调度插件同时使用。** CPA 同一时间只调用一个调度插件（优先级最高的那个），它不处理的请求直接交给内置调度，不会轮到下一个插件。如果 key-router 的优先级更高，本插件的绑定会**静默失效**。请只保留一个调度插件。
* 额度查询和重置卡只支持 Codex（ChatGPT）账号，且依赖未公开接口，接口变化时这部分功能会失效（调度绑定不受影响）。
* Portal 只能查询基于认证文件的账号。`config.yaml` 里配置的 API Key 账号可以正常绑定调度，但 CPA 的宿主接口不会把它们列给插件，Portal 会提示账号不可用。
* Portal 只显示 `primary` 中第一个已加载账号的额度。
* 历史 Token 用量来自 CPA Manager Plus 的数据库；没有安装时显示为 0。
* 目前只能通过 `config.yaml` 管理席位，还没有管理界面。

## 🔨 开发

```bash
go test ./...                 # 单元测试，不需要 C 编译器（CGO_ENABLED=0 也能跑）
go vet ./...
sh build.sh test              # 在 Docker 里跑测试并编译 out/cpa-carpool.so
PLATFORM=linux/arm64 sh build.sh   # 交叉编译 arm64（需要 Docker 的 QEMU 支持）
```

```text
.
├── main.go            # C ABI 入口（cgo）：插件初始化、宿主回调桥接
├── plugin.go          # 方法分发、配置加载（严格解析，出错即拒绝）
├── scheduler.go       # 席位编译、key 识别、独占调度
├── portal.go          # Portal 路由与 status / reset 接口
├── host.go            # CPA 宿主回调：账号列表、凭据、冷却状态、日志
├── upstream.go        # ChatGPT 额度与重置卡接口、30 秒缓存、字段白名单
├── stats.go           # 读取 CPA Manager Plus 的用量数据库
├── types.go           # 配置与响应类型
├── portal.html        # 买家 Portal 单页（编译时嵌入）
├── carpool_test.go    # 单元测试（模拟 CPA 宿主与 ChatGPT 上游）
├── config.example.yaml
├── docs/images/       # README 用到的截图与架构图
├── README.en.md       # 英文版 README
└── build.sh           # Docker 编译脚本（golang:1.26-bookworm）
```

参与贡献请先阅读 [CONTRIBUTING.md](./CONTRIBUTING.md)。

## 🧭 路线图

- [x] **v0.1.0**：独立原型（Python + Caddy）验证倒计时算法、一键核销与字段脱敏
- [x] **v0.2.0**：CPA v8 原生插件；精确绑定、租期到期、出错即拒绝；宿主回调解除冷却
- [ ] **v0.3.0**：单 key 并发 / RPM 限制；Portal 客户端接入引导（Codex CLI、Cursor、Claude Code 配置模板）
- [ ] **v0.4.0**：管理界面（席位增删改、key 生成）；提交到 CLIProxyAPI 插件商店

## 📜 免责声明

* 本项目是独立的社区项目，与 CLIProxyAPI 官方、OpenAI、Anthropic 及其他任何服务商**均无关联**，也未获得其认可。
* 多数 AI 服务（包括 ChatGPT 与 Claude）的使用条款限制或禁止共享、转售、转让账号。是否以及如何使用本插件，由你自行判断并承担全部责任，包括账号被限制或封禁的风险。
* 额度查询与重置卡核销调用的是 ChatGPT 未公开的接口，可能随时变化或失效，也可能被服务商视为异常使用。
* 本软件按"原样"提供，不提供任何形式的担保，详见 [LICENSE](./LICENSE)。

## 📄 License

[MIT](./LICENSE) © 2026 212duo

发布包中的 `third_party_licenses/` 目录包含编译进插件的第三方依赖的许可证（CLIProxyAPI SDK、modernc.org/sqlite、gopkg.in/yaml.v3 等，均为 MIT / BSD / Apache-2.0 类宽松许可证）。
