<div align="center">

# 🚗 cpa-plugin-carpool（CPA 拼车管家）

**为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）打造的轻量级多租户席位插件**

*Key 与账号一对一绑定 · 席位并发 / RPM 限制 · 买家自助查额度与接入引导 · 撞限自助核销重置卡 · 车主管理页面 · 配置出错即拒绝*

[![CI](https://github.com/212duo/cpa-plugin-carpool/actions/workflows/ci.yml/badge.svg)](https://github.com/212duo/cpa-plugin-carpool/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
![CPA](https://img.shields.io/badge/CLIProxyAPI-v8.0.30%2B-informational)

**简体中文** · [English](./README.en.md)

本文包含 0.4.0（车主管理与商店发布物）及 0.5.0（请求监控、宽屏页面）的待发布功能。下载旧版安装包时，请以对应 Release 的说明为准。

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

本插件直接以原生动态库的形式运行在 CPA 内部，不需要数据库和额外进程。管理员在管理页面（或 `config.yaml`）里把 key 绑到具体账号、设好限额，使用者拿着自己的 key 打开 Portal 就能查额度、看倒计时、复制客户端配置，撞限时自己核销重置卡。

## ✨ 功能

| 功能 | 说明 |
| :--- | :--- |
| **独占调度** | 每个 key 只能用到绑定的账号；账号冷却或不可用时直接拒绝，**绝不回落到其他账号** |
| **精确绑定** | 按 CPA 账号 ID 精确匹配，同邮箱的 Team 席位与个人账号不会串 |
| **租期到期** | 每个席位可设 `expire-at`，到期后调度和 Portal 一律拒绝 |
| **席位限额** | 每个席位可限制并发请求数和每分钟请求数（RPM），超出时返回 429 和 `Retry-After`，一个人用不满整个号 |
| **配置出错即拒绝** | 配置写错时插件拒绝所有请求并在 CPA 日志里说明原因，而不是让买家 key 落到默认调度、用到整个号池 |
| **买家 Portal** | 5 小时窗口 / 周窗口已用比例与刷新倒计时、并发与 RPM 用量、重置卡列表、历史 Token 用量 |
| **客户端接入引导** | Portal 里按买家的 key 生成 Codex CLI、Claude Code、Cursor 和 OpenAI 兼容客户端的配置，一键复制 |
| **自助核销重置卡** | 核销一张官方重置卡，并通过 CPA 宿主接口立即解除该账号的冷却 |
| **车主管理页面** | 席位增删改、随机 key 生成、账号绑定、限额与租期设置，查看账号状态与用量；保存前按插件规则校验 |
| **最小化暴露** | 返回给买家的数据只保留白名单字段，不包含邮箱、account_id、token |

<table align="center">
  <tr>
    <td align="center"><img src="docs/images/portal.png" alt="买家 Portal：席位状态、并发与 RPM 用量、额度窗口与刷新倒计时、Token 用量、重置卡" width="380"></td>
    <td align="center"><img src="docs/images/portal-guide.png" alt="Portal 客户端接入引导：已填好网关地址、key 和模型的 Codex CLI 配置" width="380"></td>
  </tr>
  <tr>
    <td align="center"><sub>买家 Portal（演示数据）</sub></td>
    <td align="center"><sub>Portal 中的客户端接入引导</sub></td>
  </tr>
</table>

<p align="center">
  <img src="docs/images/admin.png" alt="车主管理页面：全局限额设置与席位列表，包含绑定账号状态、key 数量、到期时间、限额使用和用量" width="800">
  <br>
  <sub>车主管理页面（演示数据）</sub>
</p>

## 🔧 工作原理

<p align="center">
  <img src="docs/images/architecture.svg" alt="架构图：客户端请求经 CPA 认证后进入调度，cpa-carpool 按 key 只选绑定账号，否则拒绝；未托管的 key 交回 CPA 内置调度。Portal 通过资源路由提供，经宿主回调读取凭据与冷却状态，并查询 chatgpt.com 额度" width="100%">
</p>

* **调度**：插件注册为 CPA 的调度插件。请求到达时，插件根据调用方的 key（CPA 提供的 `caller_scope`，或 `Authorization` / `x-api-key` 头）找到对应席位，只在该席位绑定的账号里选择。不在配置里的 key（比如你自己的）交回 CPA 默认调度。
* **限额**：只要有席位设置了限额，插件就同时注册为请求拦截器。每个请求在选账号之前先过一遍席位限额，超出直接返回 429；CPA 在请求结束时通知插件，插件据此释放并发名额。
* **Portal 与管理页面**：通过 CPA 的插件资源路由提供页面，买家接口凭买家 key 访问，管理接口挂在 CPA 的 Management API 下、需要管理密钥。账号凭据、运行时冷却状态都通过 CPA 宿主回调读取，插件不直接读写 auth 目录；管理页面保存时调用 CPA 自己的配置接口写回 `config.yaml`。

## 📋 兼容性

| 项目 | 要求 |
| :--- | :--- |
| CLIProxyAPI | **v8.0.30 及以上**（插件 ABI v1，按 SDK v8.0.30 编译和测试） |
| 平台 | Linux amd64 / arm64，glibc ≥ 2.36（官方 Docker 镜像 `debian:bookworm` 满足） |
| 调度绑定 | 任意提供商的账号（包括 `config.yaml` 里配置的 API Key 账号） |
| Portal 查询 | 基于认证文件的账号（OAuth 登录生成的凭据文件） |
| 额度查询与重置卡 | 仅 **Codex（ChatGPT）** 账号；其他文件账号的 Portal 只显示席位状态和 Token 用量 |
| 历史 Token 用量 | 可选，需要 [CPA Manager Plus](https://github.com/seakee/CPA-Manager-Plus) 的 `usage.sqlite` |
| 管理页面 | 需要配置 CPA 管理密钥（`management.secret-key`）；非本机访问还需开启 `management.allow-remote` |

## 🚀 快速开始

### 1. 获取插件

**从 CPA 插件商店安装**（收录进 [官方插件商店](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store) 后可用）：在 CPA 管理面板的插件商店里搜索 `cpa-carpool` 安装，CPA 会从本仓库的 Release 下载并校验 `checksums.txt`，安装到 `plugins/linux/<架构>/cpa-carpool-v<版本>.so`。装好后直接跳到第 3 步。

**下载预编译版本**：在 [Releases](https://github.com/212duo/cpa-plugin-carpool/releases) 下载对应架构的 `cpa-carpool_<版本>_linux_<amd64|arm64>.tar.gz`（或同名 `.zip`），解压得到 `cpa-carpool.so`，用 `checksums.txt` 校验。

**或者自己编译**（需要 Docker，产物与官方镜像的工具链和 glibc 一致）：

```bash
git clone https://github.com/212duo/cpa-plugin-carpool.git
cd cpa-plugin-carpool
sh build.sh          # 输出 out/cpa-carpool.so；加 test 参数会先跑测试
```

### 2. 放进 CPA 的插件目录

把 `cpa-carpool.so` 放到 CPA 配置里 `plugins.dir` 指向的目录（默认是工作目录下的 `plugins/`，也可以放在 `plugins/linux/amd64/` 这样的平台子目录）。

> 插件 ID 是文件名去掉扩展名及可选的 `-v版本号`。**请保持文件名为 `cpa-carpool.so`**（或 `cpa-carpool-v0.4.0.so`），这两种形式的配置段名和访问路径都使用 `cpa-carpool`。

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
      rpm: 30                 # 可选：席位默认每分钟请求上限
      users:
        - name: "TeamA-席位1"
          api-keys: ["sk-buyer-alice"]
          primary: ["codex-xxxxxxxx-a1@example.com-team.json"]
          expire-at: "2026-11-15T23:59:59+08:00"
          max-concurrency: 2  # 可选：该席位最多同时 2 个请求
```

CPA 重载配置时会把新配置推给插件，通常不需要重启。启动后在 CPA 日志里能看到插件加载信息；配置有误时会出现 `[carpool] invalid plugin configuration ...`。

也可以只写好 `enabled: true`，之后在[管理页面](#-管理页面)里添加席位：页面会生成 key、同步写入 `access.api-keys` 和插件配置。

### 4. 把 Portal 链接发给买家

```text
https://你的域名/v0/resource/plugins/cpa-carpool/portal#key=sk-buyer-alice
```

推荐用 `#key=` 的形式：`#` 后面的内容不会发给服务器，不会出现在访问日志里。旧的 `?key=` 形式仍然可用，页面打开后会把它从地址栏移除。管理页面里每个席位都有「复制 Portal 链接」按钮。

买家打开 Portal 后，「客户端接入」一栏会按他的 key 生成 Codex CLI（`~/.codex/config.toml`）、Claude Code（`~/.claude/settings.json`）、Cursor 和 OpenAI 兼容客户端的配置，模型列表来自网关的 `/v1/models`。网关对外地址和打开 Portal 的地址不同时（例如 Portal 走内网），用 `public-base-url` 指定。

## 🧰 配置参考

配置写在 `plugins.configs.cpa-carpool` 下。插件使用**严格解析**：拼错的字段名会被当作错误，而不是被静默忽略。

| 字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `enabled` | bool | `false` 时插件不接管任何请求（所有 key 都走 CPA 默认调度） |
| `priority` | int | CPA 读取的插件优先级，同时有多个调度插件时只有最高的那个生效 |
| `debug` | bool | 在 CPA 日志里记录每次调度的决定（只记录席位名，不记录 key） |
| `db-path` | string | CPA Manager Plus 的 `usage.sqlite` 路径；填写 CPA 进程或容器内可见的路径。留空时自动探测，找不到或读取失败时显示“统计暂不可用” |
| `upstream-user-agent` | string | 请求 ChatGPT 额度接口时使用的 User-Agent，留空使用内置默认值 |
| `public-base-url` | string | 客户端访问网关的地址（如 `https://api.example.com`），用于 Portal 接入引导；留空使用打开 Portal 的地址 |
| `max-concurrency` | int | 席位默认并发上限，席位没单独设置时使用；`0`（默认）为不限 |
| `rpm` | int | 席位默认每分钟请求上限，席位没单独设置时使用；`0`（默认）为不限 |
| `users[].name` | string | 席位名，显示在 Portal 上，**不能重复**；留空时为 `seat-N` |
| `users[].api-keys` | []string | 分配给该席位的 key，**至少一个**，同一个 key 不能出现在两个席位里 |
| `users[].primary` | []string | 绑定的 CPA 账号 ID，**至少一个**，精确匹配（忽略大小写和 `.json` 后缀） |
| `users[].expire-at` | string | 可选，RFC3339 格式的到期时间，如 `2026-11-15T23:59:59+08:00` |
| `users[].max-concurrency` | int | 可选，覆盖默认并发上限；写 `0` 表示该席位不限 |
| `users[].rpm` | int | 可选，覆盖默认每分钟请求上限；写 `0` 表示该席位不限 |

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

### 请求监控（0.5）

在 Portal 点击“查看请求监控”，或打开 `https://你的域名/v0/resource/plugins/cpa-carpool/monitor`，输入自己的客户端 Key。支持最近 1 小时 / 24 小时 / 7 天、成功 / 失败筛选，以及模型汇总与最近 100 / 500 条请求。每 10 秒刷新，后台标签页自动暂停；手机上支持表格横向滚动。

![请求监控（演示数据）](docs/images/monitor.png)

* **只看当前 Key**：服务端从 `X-Carpool-Key` 校验席位并生成查询范围，全部统计与记录都限制在当前 Key；同席位其他 Key、同上游账号其他人的数据均不包含。不同使用者应分发不同 Key，勿共用或把旧 Key 重新分配给其他人。
* **仅展示必要字段**：请求模型、推理 / 服务枚举、成功 / 失败、HTTP 错误码、Token、首字 / 总耗时与时间。账号邮箱、认证文件、Key / 哈希、原始错误、请求与响应内容、IP 与追踪标识均不返回。未采集请求模型时显示“未记录模型”，不会改用内部路由模型。
* **数据来源**：沿用下面的 Manager Plus 用量库，只展示已采集记录，可能有延迟；不显示进行中的请求或未经采集的调用。费用未接入可靠计价数据，暂不展示。输出 TPS 根据输出 Token /（总耗时 − 首字耗时）估算，缺失或无效时显示 `--`。
* **访问方式**：监控 API 仅接受 `X-Carpool-Key` 请求头，分享页面可用 `#key=...`；不支持 `?key=`。页面移除地址中的凭据，仅在内存保留 Key，不写浏览器存储，切换或退出时清空数据。公网请使用 HTTPS，并保护好分享链接。

#### 开销与数据库索引

近期记录缓存 10 秒，汇总与模型概览缓存 30 秒，页面分别显示更新时间。同一 Key、范围与状态的汇总可以在不同列表长度之间复用；其他 Key 的缓存绝不复用。命中缓存前后仍校验当前席位与有效期，配置更新会清空缓存并取消旧查询。失败不缓存，也不返回过期数据兜底。

相同查询的并发请求会合并，数据库连接复用，整个插件最多同时执行 2 条监控数据查询，排队和执行共用 3 秒期限。两级缓存合计最多 20 MiB 的 JSON 数据（不含 Go / SQLite / 请求处理中额外内存），列表层 256 项、汇总层 512 项。错误刷新按 20 / 40 / 60 秒退避，恢复后回到 10 秒。

**数据较多时，建议由车主在 Manager Plus 数据库的写入端执行一次索引脚本**（插件仍只读，不会自动修改数据库）：

```bash
sqlite3 /实际路径/usage.sqlite < scripts/usage-monitor-index.sql
```

索引为 `(api_key_hash, timestamp_ms DESC, id DESC)`，脚本可重复执行。大表建立索引需要时间与额外磁盘空间，适合在低峰执行；仅挂载在 CPA 容器里的只读目录不能用于建索引。未建索引时仍可查询，但大量数据下首次统计会更慢。

可复现的百万条并发测试、场景与结果见 [监控性能测试](docs/monitor-performance.md)。

### 历史 Token 用量

历史统计来自 CPA Manager Plus 已采集的 `usage.sqlite`，与 ChatGPT 的 5 小时 / 周额度分别查询。只安装插件不会自动生成这份历史数据。

* Portal 显示当前登录 Key 的记录；管理页汇总席位当前绑定的所有 Key。调用次数包括已记录的成功与失败请求。更换或移除 Key 后，不再汇总旧 Key 的记录。
* 兼容新旧数据库字段：新版优先使用标准化输入量与缓存读取量，缓存创建量不会误算为缓存命中。数据库无匹配记录时显示 0；缺失、无权限或结构不兼容时显示“统计暂不可用”。
* CPA 和 Manager Plus 分别运行在 Docker 容器时，需要把 Manager 的完整数据目录只读挂载到 CPA，例如 `/opt/cpa/manager-data`，并将 `db-path` 指向该容器内的 `usage.sqlite`。请同时保留 `usage.sqlite-wal`、`usage.sqlite-shm` 的可见性，不要只复制主数据库文件。自动探测不会跨容器读取文件。

## ⏳ 席位限额

多人拼一个号时，可以给每个席位限制同时进行的请求数（`max-concurrency`）和每分钟发起的请求数（`rpm`），避免一个人把账号的额度或速率用满：

* 限额按**席位**计算，同一席位的多个 key 共享；不同席位互不影响。
* 顶层的 `max-concurrency` / `rpm` 是默认值，席位里写了就以席位为准，写 `0` 表示该席位不限。
* 超出时请求在选账号之前就被拒绝，返回 HTTP 429、`Retry-After` 头和 `rate_limit_error`，Codex CLI、Claude Code 等客户端会自动退避重试。
* RPM 是滑动的 60 秒窗口，按请求发起时间计数；并发数在 CPA 通知请求结束（成功、失败、取消）时释放。如果某个请求的结束通知丢失，它占用的名额最多 30 分钟后自动回收。
* 计数保存在 CPA 进程内存里，CPA 重启后清零；多个 CPA 实例之间不共享。
* 只有至少一个席位设置了限额时，插件才注册请求拦截与完成通知；完全不用限额时不会给任何请求增加开销。当前用量也只在这时统计，显示在买家 Portal 和管理页面上。

## 🔑 管理页面

```text
https://你的域名/v0/resource/plugins/cpa-carpool/admin
```

首次使用时，在 CPA 配置中设置 `management.secret-key`；从其他设备访问还需 `management.allow-remote: true`。旧版配置名 `remote-management` 仍兼容。

打开页面后输入管理密钥的明文，即可：

* 新增、编辑、删除席位，为席位生成随机 key（`sk-carpool-` + 48 位十六进制），复制 key 或买家的 Portal 链接；
* 从 CPA 已加载的账号里选择要绑定的账号（`config.yaml` 中的 API Key 类账号需要手动填写 ID），一眼看出哪些账号已经被哪个席位占用；
* 设置租期、席位限额和全局默认值、`public-base-url`、调试日志等；
* 查看每个席位绑定账号的加载与冷却状态、当前并发与近 1 分钟请求数、历史用量。

管理页面本身不包含任何数据，所有数据都要凭管理密钥读取；密钥只保存在当前浏览器标签页（`sessionStorage`）。点击「保存更改」时，页面先调用插件按严格规则校验新配置，再通过 CPA 的 `/v8/management/config` 接口依次：

1. 从 `access.api-keys` 撤销被删除席位的 key（先撤销，避免它们落到默认调度、用到整个号池）；
2. 确认旧 key 已无法通过 CPA 认证，再写入 `plugins.configs.cpa-carpool`；
3. 确认插件已应用新绑定，再把新 key 加入 `access.api-keys`。

任何一步失败都会停下并提示，已完成的步骤不会让买家多拿到权限。CPA 写回 `config.yaml` 后会自动重载，插件随即生效。

请在 `access.api-keys` 中保留一个不属于席位的车主客户端 key。管理页会阻止撤销最后一个 key，因为空列表会关闭 CPA 的客户端认证。需要停用插件时，先删除席位并保存，再通过 CPA 的插件管理停用。

> [!IMPORTANT]
> 管理密钥能修改整个网关的配置。只在可信设备上打开管理页面，从外网访问时务必使用 HTTPS；CPA 对管理接口有失败次数限制，连续输错 5 次会封禁来源 IP 30 分钟。

## 🌐 Portal 与接口

买家用到的路由都挂在 `/v0/resource/plugins/cpa-carpool/` 下，只接受 GET（CPA 资源路由的限制）。买家 key 通过 `X-Carpool-Key` 请求头传递（也接受 `key` 查询参数）。

| 路由 | 说明 |
| :--- | :--- |
| `GET /portal` | 买家自助页面 |
| `GET /api/status` | 席位状态：额度窗口、重置卡、冷却状态、限额用量、Token 用量、到期时间 |
| `GET /api/reset?confirm=1` | 核销一张重置卡并解除 CPA 冷却；不带 `confirm=1` 不会执行 |
| `GET /admin` | 车主管理页面（页面本身不含数据） |

```bash
curl -H "X-Carpool-Key: sk-buyer-alice" \
  https://你的域名/v0/resource/plugins/cpa-carpool/api/status
```

管理接口挂在 CPA 的 Management API 下，CPA 会先校验管理密钥（`Authorization: Bearer <管理密钥>`）：

| 路由 | 说明 |
| :--- | :--- |
| `GET /v0/management/carpool/overview` | 各席位的运行状态：绑定账号、冷却、限额用量、历史用量（不返回 key） |
| `GET /v0/management/carpool/auths` | CPA 已加载的账号及其已绑定的席位，供绑定时选择 |
| `POST /v0/management/carpool/validate` | 用插件的严格规则校验一份插件配置（JSON），不会修改正在使用的配置 |

## 🔒 安全设计

* **出错即拒绝**：配置无法解析或校验失败时拒绝全部请求；席位到期、账号不可用时拒绝该席位的请求，从不回落到其他账号。
* **精确匹配**：账号 ID 精确比较，不做前缀、后缀或邮箱模糊匹配。
* **字段白名单**：Portal 只返回额度窗口、重置卡（标题/状态/时间）、席位名、席位类型、冷却状态、限额用量和 Token 用量；邮箱、account_id、token 不会出现在响应里。上游接口的原始错误只写进 CPA 日志，买家只看到通用提示。
* **限额拒绝不泄露信息**：429 响应只包含席位名和限额数值，不包含 key 或账号信息。
* **管理接口受管理密钥保护**：管理页面本身不含数据；席位状态、账号列表和校验接口挂在 CPA 的 Management API 下，由 CPA 校验管理密钥。概览接口不返回 key。
* **保存顺序保证安全**：管理页面保存前先校验，先撤销被删除的 key、再写席位配置、最后加入新 key，任何一步失败都不会让 key 在中间状态用到整个号池。
* **Key 不进 URL**：Portal 用请求头传 key，分享链接用 `#key=`，避免 key 出现在网关和反代的访问日志里。
* **限制上游请求频率**：每个账号的额度查询结果缓存 30 秒，买家频繁刷新也不会频繁请求 ChatGPT；重置串行执行，连点不会重复核销。
* **不碰凭据文件**：通过 CPA 宿主回调读取凭据和冷却状态，插件不直接读写 auth 目录。
* **页面加固**：Portal 和管理页面输出内容全部转义，并带有 CSP、`Referrer-Policy: no-referrer` 等响应头；管理密钥只存在当前标签页的 `sessionStorage` 中，不进 URL。

Portal 本身不需要 CPA 管理密钥，访问控制完全依赖 key 的保密性。请务必通过 **HTTPS** 对外提供服务，并使用足够长的随机 key（例如 `openssl rand -hex 24`）。漏洞报告方式见 [SECURITY.md](./SECURITY.md)。

## 🚧 已知限制

* **不能和 key-router 等其他调度插件同时使用。** CPA 同一时间只调用一个调度插件（优先级最高的那个），它不处理的请求直接交给内置调度，不会轮到下一个插件。如果 key-router 的优先级更高，本插件的绑定会**静默失效**。请只保留一个调度插件。
* 额度查询和重置卡只支持 Codex（ChatGPT）账号，且依赖未公开接口，接口变化时这部分功能会失效（调度绑定不受影响）。
* Portal 只能查询基于认证文件的账号。`config.yaml` 里配置的 API Key 账号可以正常绑定调度，但 CPA 的宿主接口不会把它们列给插件，Portal 会提示账号不可用。
* Portal 只显示 `primary` 中第一个已加载账号的额度。
* 历史 Token 用量依赖 CPA Manager Plus 的采集和数据库共享；缺失或不可读时显示“统计暂不可用”，不代表实际用量为 0。
* 限额计数在 CPA 进程内存中，重启后清零，多个 CPA 实例之间不共享。RPM 统计所有经过 CPA 请求拦截的接口调用（包括 Claude Code 的 `count_tokens` 等）。
* 并发限制依赖 CPA 的请求完成通知（插件协议 schema 2 及以上，CPA v8.0.30 满足）；宿主不支持时插件会在日志中提示，只执行 RPM 限制。
* 管理页面使用 CPA v8 的 `/v8/management/config` 接口读写配置，保存时 CPA 会按 v8 布局写回 `config.yaml`。保存前会检测配置是否已被其他页面修改；检测和写入之间仍可能有竞争，请避免同时在多处编辑。
* 预编译版本和插件商店只提供 Linux amd64 / arm64。

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
├── limiter.go         # 席位并发 / RPM 限制（请求拦截与完成通知）
├── portal.go          # 资源路由分发、Portal 与 status / reset 接口
├── admin.go           # 管理接口：席位概览、账号列表、配置校验
├── host.go            # CPA 宿主回调：账号列表、凭据、冷却状态、日志
├── upstream.go        # ChatGPT 额度与重置卡接口、30 秒缓存、字段白名单
├── stats.go           # 读取 CPA Manager Plus 的用量数据库
├── types.go           # 配置与响应类型
├── portal.html        # 买家 Portal 单页（编译时嵌入）
├── admin.html         # 车主管理页面（编译时嵌入）
├── *_test.go          # 单元测试（模拟 CPA 宿主与 ChatGPT 上游）
├── config.example.yaml
├── plugin-store/      # 提交到 CLIProxyAPI 插件商店的 registry 条目
├── docs/images/       # README 用到的截图、架构图与 logo
├── README.en.md       # 英文版 README
└── build.sh           # Docker 编译脚本（golang:1.26-bookworm）
```

参与贡献请先阅读 [CONTRIBUTING.md](./CONTRIBUTING.md)。

## 🧭 路线图

- [x] **v0.1.0**：独立原型（Python + Caddy）验证倒计时算法、一键核销与字段脱敏
- [x] **v0.2.0**：CPA v8 原生插件；精确绑定、租期到期、出错即拒绝；宿主回调解除冷却
- [x] **v0.3.0**：席位级并发 / RPM 限制（同一席位的 key 共享）；Portal 客户端接入引导（Codex CLI、Claude Code、Cursor、OpenAI 兼容配置模板）
- [x] **v0.5.0（待发布）**：当前 Key 请求监控、隐私隔离、Portal / 管理页宽屏布局
- [x] **v0.4.0**：车主管理页面（席位增删改、key 生成、账号绑定、限额与用量）；插件商店发布物与 registry 条目

## 📜 免责声明

* 本项目是独立的社区项目，与 CLIProxyAPI 官方、OpenAI、Anthropic 及其他任何服务商**均无关联**，也未获得其认可。
* 多数 AI 服务（包括 ChatGPT 与 Claude）的使用条款限制或禁止共享、转售、转让账号。是否以及如何使用本插件，由你自行判断并承担全部责任，包括账号被限制或封禁的风险。
* 额度查询与重置卡核销调用的是 ChatGPT 未公开的接口，可能随时变化或失效，也可能被服务商视为异常使用。
* 本软件按"原样"提供，不提供任何形式的担保，详见 [LICENSE](./LICENSE)。

## 📄 License

[MIT](./LICENSE) © 2026 212duo

发布包中的 `third_party_licenses/` 目录包含编译进插件的第三方依赖的许可证（CLIProxyAPI SDK、modernc.org/sqlite、gopkg.in/yaml.v3 等，均为 MIT / BSD / Apache-2.0 类宽松许可证）。
