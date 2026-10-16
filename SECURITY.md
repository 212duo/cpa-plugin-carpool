# 安全策略

## 支持的版本

| 版本 | 是否接收安全修复 |
| :--- | :--- |
| 0.2.x | ✅ |
| < 0.2 | ❌ |

## 报告漏洞

**请不要通过公开 Issue 报告安全漏洞。**

请使用 GitHub 的私密漏洞报告：打开仓库的 **Security → Report a vulnerability**，或直接访问
<https://github.com/212duo/cpa-plugin-carpool/security/advisories/new>。

报告中请尽量包含：

- 受影响的插件版本与 CLIProxyAPI 版本；
- 复现步骤或概念验证（请使用测试 key 和测试账号）；
- 你判断的影响范围。

提交报告前请删除其中真实的 API Key、token、邮箱和 account_id。我会在 7 天内确认收到，并在修复发布后致谢（如你希望署名）。

## 适用范围

以下问题属于本项目的安全范围：

- 买家能用到未绑定给自己的账号，或在配置错误、席位到期后仍能使用账号；
- Portal 或接口泄露邮箱、account_id、token 等账号信息，或泄露其他买家的数据；
- 未持有有效 key 却能查询状态或核销重置卡；
- Portal 中的 XSS、注入等问题。

以下问题不在范围内：

- CLIProxyAPI 本身的漏洞，请报告给 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)；
- 需要已经拿到 CPA 服务器权限或 `config.yaml` 才能实施的攻击；
- 上游服务商的接口变化导致的功能失效。

## 部署建议

- Portal 不需要 CPA 管理密钥，访问控制完全依赖买家 key 的保密性。请通过 **HTTPS** 对外提供服务，并使用足够长的随机 key，例如 `openssl rand -hex 24`。
- 给买家分享 Portal 链接时使用 `#key=` 形式，避免 key 出现在访问日志里。
- 席位到期或买家退出后，及时从 `access.api-keys` 和插件配置里删除对应的 key。
- 不要同时启用其他调度插件（如 key-router），否则本插件的绑定可能静默失效。
