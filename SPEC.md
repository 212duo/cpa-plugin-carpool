# cpa-plugin-carpool 技术方案与需求规格说明书 (SPEC)

> 项目名称：**cpa-plugin-carpool (CPA 拼车管家)**  
> 目标宿主：[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA v8+ ABI)  
> 适用场景：个人车主、小团队、闲置席位合租转售、轻量级多租户管理

---

## 1. 背景与产品定位

### 1.1 痛点诊断
1. **重型方案（Sub2API / NewAPI / OneAPI）过重**：
   - 依赖 MySQL、Redis、独立容器集群，常驻内存 500MB~1GB+；
   - 运维和配置极其繁琐，对于只有 2~10 个账号想做轻量拼车分摊成本的小车主而言属于“杀鸡用牛刀”。
2. **纯 CPA / 现有插件过于简陋**：
   - CPA 本身为反代网关内核，完全没有面向买家/租户的自助服务前端；
   - 现有 `key-router` 插件仅做简陋路由，无用量透视与额度感知，且在“同一邮箱下存在多个不同类型账号（如 Team 席位 + 个人 Pro 5x）”时存在严重的前端模糊匹配与覆盖 Bug；
   - 现有 `codex-auto-reset` 仅支持后台全自动盲目核销，买家无法主观决定何时使用稀缺的重置卡；
   - **车主沦为 24 小时人工客服**：买家一旦撞限（429 Too Many Requests），只能找车主看后台、人工查询额度并手动重置，沟通成本极高。

### 1.2 产品定位
**CPA 拼车管家 (cpa-plugin-carpool)** 是一款嵌入在 CLIProxyAPI 内部的高性能轻量级微服务插件。  
- **对车主**：零数据库、零外部中间件，配置文件或 Web 控制台一键生成买家独占 Key，支持到期自动熔断与防刷频控，实现“收钱后甩手”；
- **对买家**：凭专属 API Key 进入极简自助 Portal，实时查看 5 小时短窗与周额度刷新倒计时、历史 Token 消耗，并在撞限时 2 秒内自助核销重置卡瞬间满血复活；
- **对安全**：坚持零信任原则，彻底剥离上游账号的真实邮箱、UUID、Token，物理阻断跨租户越权。

---

## 2. 核心需求功能矩阵

### 2.1 买家自助端 (Buyer Portal)
- [x] **API Key 快速鉴权**：通过 URL 参数 `?key=sk-...` 或输入框快速鉴权，支持免密码直接访问自身席位。
- [x] **上游双窗口额度透视**：
  - 实时采集上游 5 小时短窗（18000s）已用百分比；
  - 实时采集上游 7 天周限额度（604800s）已用百分比。
- [x] **刷新时间与倒计时（核心创新点）**：
  - `⏱️ 5小时额度刷新倒计时`：实时动态递减展示（如 `剩余 1 小时 23 分`）；
  - `📅 周限额度刷新时间`：格式化本地时间与星期展示（如 `周日 18:00 (剩余 3 天)`）。
- [x] **历史用量统计**：
  - 读取网关本地 SQLite 统计数据，展示该 Key 历史总请求次数与 Token 吞吐（输入/输出/缓存）。
- [x] **重置卡时间线明细与自主核销**：
  - 列表展示当前账号拥有的全部重置卡（Title、状态、发放时间、到期失效时间）；
  - 若已使用，展示具体的**使用/核销时间**；
  - 撞限时提供防误触的【⚡ 一键核销重置卡】按钮，2 秒内调用 OpenAI 上游并同步解除 CPA 本地冷却，立刻满血。
- [ ] **客户端极速接入卡片 (接入引导)**：
  - 提供 Cursor、Claude Code、Windsurf、Cline 的 1 键复制配置模板。

### 2.2 车主管理端 (Operator Controls)
- [ ] **真·物理凭据精确绑定 (修复 key-router Bug)**：
  - 放弃脆弱的 `provider:email` 模糊匹配，采用 `provider:auth_id` / 文件名精确绑定，支持同一邮箱下 Team 席位与个人 Pro 5x 彻底隔离。
- [ ] **租期到期自动熔断 (Auto-Expiry)**：
  - 支持给每个 Key 设置 `expire_at`（如周租第 7 天 23:59:59），到期自动失效，无须人工下线。
- [ ] **单 Key 限频与防刷保护 (Rate Limiting)**：
  - 限制单 Key 最大并发（如并发 1~2）与 RPM（每分钟请求数），防止自动化脚本死循环把账号打崩。

---

## 3. 架构设计与技术规范

### 3.1 CPA 插件 ABI 契约
插件采用 Go 语言开发，编译为 C-shared 动态库（`.so` / `.dll` / `.dylib`）：

```text
               客户端 (Cursor / Claude Code)
                            │
               ┌────────────▼────────────┐
               │    CLIProxyAPI (CPA)    │
               │  ┌───────────────────┐  │
               │  │  Plugin ABI Host  │  │
               │  └─────────┬─────────┘  │
               └────────────┼────────────┘
                            │
              ┌─────────────▼─────────────┐
              │    cpa-plugin-carpool     │
              ├───────────────────────────┤
              │ 1. Scheduler (独占调度)    │
              │ 2. Resource (买家 Portal) │
              │ 3. Reset Engine (一键重置) │
              │ 4. Rate Limiter (防刷频控) │
              └───────────────────────────┘
```

1. **Resource API (静态 Portal 与买家接口)**：
   - `GET /v0/resource/plugins/carpool/portal`：提供嵌入式的自适应响应式 Portal 页面；
   - `GET /v0/resource/plugins/carpool/api/status?key=...`：脱敏后的实时配额与倒计时接口；
   - `POST /v0/resource/plugins/carpool/api/reset`：买家自主核销重置卡并同步清除网关冷却。
2. **Scheduler (调度核心)**：
   - 实现 `pluginapi.Scheduler` 接口；
   - 在 `Pick(ctx, req)` 中提取客户端 `caller_scope` 或 Header 中的 API Key；
   - 命中买家规则直接指定唯一的 `AuthID`，未授权账号对该买家完全不可见；
   - 买家账号冷却或故障时直接返回 typed reject，严禁流量溢出回退。
3. **Management API (车主管理)**：
   - 挂载在 `/v0/management/plugins/carpool/` 下，受 CPA 管理秘钥保护；
   - 提供可视化配置、买家 CRUD、Key 生成与用量审计。

### 3.2 零信任脱敏原则 (Zero-Trust PII Masking)
在向买家前端吐出数据时，执行严格的**最小字段白名单过滤**：
- 严格屏蔽：`account_id`、`user_id`、`email`、`access_token`、`refresh_token`、`organization_id`；
- 仅保留：`seat_name`、`primary_window` (used/reset_at)、`secondary_window` (used/reset_at)、`credits` (title/status/granted_at/expires_at/redeemed_at)、`local_stats`。

---

## 4. 实施阶段演进路线

| 阶段 | 交付目标 | 核心工作项 | 状态 |
| :--- | :--- | :--- | :--- |
| **Phase 1** | 原型验证与独立 Portal | Python 微服务 + Caddy 反代 + 倒计时 + 一键核销 | **已完成** ✅ |
| **Phase 2** | 项目开源基础搭建 | GitHub 仓库初始化、SPEC 规格书、编译流与协议 | **进行中** 🚀 |
| **Phase 3** | Go 原生动态库插件化 | 接入 CPA Plugin ABI，移除 Python 依赖与 Caddy 反代 | 待启动 |
| **Phase 4** | 官方插件商店发布 | 编写 `registry.json`，投放到 NodeSeek / LINUX DO 社区 | 待启动 |
