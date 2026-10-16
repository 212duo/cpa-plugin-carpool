<div align="center">

# 🚗 cpa-plugin-carpool

**A lightweight multi-tenant seat plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA)**

*One key, one account · Self-service quota portal · Reset countdowns · Self-service reset credits · Fail-closed configuration*

[![CI](https://github.com/212duo/cpa-plugin-carpool/actions/workflows/ci.yml/badge.svg)](https://github.com/212duo/cpa-plugin-carpool/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
![CPA](https://img.shields.io/badge/CLIProxyAPI-v8.0.30%2B-informational)

[简体中文](./README.md) · **English**

[Quick start](#-quick-start) · [Configuration](#-configuration-reference) · [Security](#-security-design) · [Known limitations](#-known-limitations) · [Spec (SPEC.md, Chinese)](./SPEC.md) · [Changelog](./CHANGELOG.md)

</div>

> [!WARNING]
> **This is a community project with no affiliation to CLIProxyAPI, OpenAI or Anthropic.** The terms of most AI services forbid sharing or transferring accounts, and using this plugin may get your accounts restricted or banned. Quota lookup and reset-credit redemption rely on undocumented ChatGPT endpoints that can break at any time. Read the [disclaimer](#-disclaimer) first; you use this plugin at your own risk.

---

## 💡 What problem does it solve

When a few people share a ChatGPT Team seat, or a small team hands out accounts, the usual options all fall short:

* **Sub2API / NewAPI style gateways are heavy**: they need a database, Redis and separate services, which is too much to run for a handful of accounts;
* **Plain CPA has no multi-tenancy**: there is no quota page for the people using it, and routing plugins that match by email mix up accounts when one email has several (for example a Team seat and a personal Pro plan);
* **Hitting a limit means waiting for the admin**: on a 429 the user has to ask the admin to check the quota and reset it by hand.

This plugin runs inside CPA as a native shared library, with no database and no extra process. The admin binds each key to specific accounts in the config; users open the portal with their own key to see their quota and countdowns, and redeem a reset credit themselves when they hit the limit.

## ✨ Features

| Feature | Description |
| :--- | :--- |
| **Dedicated scheduling** | A key can only use the accounts bound to it; when they are cooling down or unavailable the request is rejected and **never falls back to other accounts** |
| **Exact binding** | Accounts are matched by their exact CPA auth ID, so a Team seat and a personal account under the same email never mix |
| **Lease expiry** | Each seat can have an `expire-at`; after it, scheduling and the portal reject the seat |
| **Fail-closed config** | A broken config makes the plugin reject every request and log the reason, instead of letting buyer keys fall through to the default scheduler and the whole account pool |
| **Buyer portal** | Usage and reset countdowns for the 5-hour and weekly windows, reset credits, historical token usage |
| **Self-service reset** | Redeems an official reset credit and immediately clears the account's cooldown through a CPA host callback |
| **Minimal exposure** | Responses to buyers only contain whitelisted fields; no email, account_id or tokens |

<p align="center">
  <img src="docs/images/portal.png" alt="Buyer portal: seat status, quota windows with reset countdowns, token usage and reset credits" width="460">
  <br>
  <sub>Buyer portal (demo data; the UI is in Chinese)</sub>
</p>

## 🔧 How it works

<p align="center">
  <img src="docs/images/architecture.svg" alt="Architecture: client requests pass CPA key authentication and reach the scheduler, where cpa-carpool routes each managed key only to its bound account or rejects it; unmanaged keys go to CPA's built-in scheduler. The portal is served through resource routes, reads credentials and cooldown state through host callbacks, and queries quota from chatgpt.com" width="100%">
</p>

* **Scheduling**: the plugin registers as CPA's scheduler plugin. For each request it identifies the caller's key (CPA's `caller_scope`, or the `Authorization` / `x-api-key` header), finds the seat and picks only among the accounts bound to it. Keys that are not configured (such as your own) are handed back to CPA's default scheduler.
* **Portal**: a page and two endpoints served through CPA's plugin resource routes. Credentials and runtime cooldown state are read through CPA host callbacks; the plugin never reads or writes the auth directory itself.

## 📋 Compatibility

| Item | Requirement |
| :--- | :--- |
| CLIProxyAPI | **v8.0.30 or later** (plugin ABI v1; built and tested against SDK v8.0.30) |
| Platform | Linux amd64 / arm64, glibc ≥ 2.36 (the official `debian:bookworm` Docker image qualifies) |
| Scheduling | Accounts from any provider, including API-key accounts configured in `config.yaml` |
| Portal lookup | File-backed accounts (credential files created by OAuth login) |
| Quota and reset credits | **Codex (ChatGPT)** accounts only; for other file-backed accounts the portal shows seat status and token usage |
| Token history | Optional; needs the `usage.sqlite` of [CPA Manager Plus](https://github.com/seakee/CPA-Manager-Plus) |

## 🚀 Quick start

### 1. Get the plugin

**Prebuilt**: download `cpa-carpool_<version>_linux_<amd64|arm64>.tar.gz` for your architecture from [Releases](https://github.com/212duo/cpa-plugin-carpool/releases), extract `cpa-carpool.so` and verify it against `SHA256SUMS`.

**Or build it yourself** (needs Docker; the output matches the toolchain and glibc of the official image):

```bash
git clone https://github.com/212duo/cpa-plugin-carpool.git
cd cpa-plugin-carpool
sh build.sh          # writes out/cpa-carpool.so; pass "test" to run the tests first
```

### 2. Put it in CPA's plugin directory

Copy `cpa-carpool.so` into the directory configured as `plugins.dir` (by default `plugins/` under the working directory; a platform subdirectory such as `plugins/linux/amd64/` also works).

> The plugin ID is the file name without its extension, so **keep the file name `cpa-carpool.so`** (or a versioned name such as `cpa-carpool-v0.2.0.so`). The config section below must use the same ID.

With the official Docker image, mount the plugin directory:

```bash
docker run -d -p 8317:8317 \
  -v $PWD/config.yaml:/CLIProxyAPI/config.yaml \
  -v $PWD/auths:/root/.cli-proxy-api \
  -v $PWD/plugins:/CLIProxyAPI/plugins \
  eceasy/cli-proxy-api:latest
```

### 3. Configure

Add the following to CPA's `config.yaml` (full example: [config.example.yaml](./config.example.yaml), comments in Chinese):

```yaml
access:
  api-keys:
    - "sk-owner-xxx"          # your own key: not in carpool, uses CPA's default scheduler
    - "sk-buyer-alice"        # buyer keys must be listed here too, or CPA rejects them before scheduling

plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-carpool:
      enabled: true
      priority: 100
      users:
        - name: "TeamA-seat1"
          api-keys: ["sk-buyer-alice"]
          primary: ["codex-xxxxxxxx-a1@example.com-team.json"]
          expire-at: "2026-11-15T23:59:59+08:00"
```

CPA pushes the new config to the plugin when it reloads `config.yaml`, so a restart is usually not needed. The CPA log shows the plugin being loaded; a bad config shows up as `[carpool] invalid plugin configuration ...`.

### 4. Send the portal link to the buyer

```text
https://your-domain/v0/resource/plugins/cpa-carpool/portal#key=sk-buyer-alice
```

Use the `#key=` form: everything after `#` stays in the browser and never reaches the server's access logs. The older `?key=` form still works, and the page removes it from the address bar after loading.

## 🧰 Configuration reference

The config lives under `plugins.configs.cpa-carpool`. Parsing is **strict**: a misspelled field name is an error, not something silently ignored.

| Field | Type | Description |
| :--- | :--- | :--- |
| `enabled` | bool | When `false` the plugin handles nothing (every key uses CPA's default scheduler) |
| `priority` | int | Plugin priority read by CPA; with several scheduler plugins only the highest one runs |
| `debug` | bool | Log each scheduling decision to the CPA log (seat names only, never keys) |
| `db-path` | string | Path to CPA Manager Plus's `usage.sqlite`; auto-detected when empty, usage shows 0 if not found |
| `upstream-user-agent` | string | User-Agent for the ChatGPT quota endpoints; empty uses the built-in default |
| `users[].name` | string | Seat name shown in the portal; defaults to `seat-N` |
| `users[].api-keys` | []string | Keys assigned to the seat, **at least one**; a key may not appear in two seats |
| `users[].primary` | []string | Bound CPA auth IDs, **at least one**; exact match (case and `.json` suffix ignored) |
| `users[].expire-at` | string | Optional RFC3339 expiry, e.g. `2026-11-15T23:59:59+08:00` |

**If validation fails, the plugin rejects every request** (your own key included) and logs the reason. This is deliberate: if the plugin stepped aside instead, buyer keys would reach CPA's default scheduler and the whole account pool.

### Finding an auth ID (`primary`)

An auth ID is the account's credential file path **relative to CPA's auth directory** (`~/.cli-proxy-api` by default, set by `oauth.auth-dir`):

* use the file name in the auth directory, e.g. `codex-1a2b3c4d-alice@example.com-team.json`;
* include the subdirectory when the file is in one, e.g. `team/codex-1a2b3c4d-alice@example.com-team.json`;
* or call CPA's management API `GET /v0/management/auth-files` and use the file names it returns.

### Binding patterns

* **Several keys for one seat**: list them all under `api-keys`;
* **Several people sharing one account**: give several seats the same `primary`, each with its own keys and expiry;
* **One seat over several accounts**: list several `primary` IDs; scheduling picks among them by CPA priority, while the portal shows and resets the first one that is loaded.

## 🌐 Portal and API

All routes live under `/v0/resource/plugins/cpa-carpool/` and accept GET only (a limit of CPA's resource routes). The buyer key goes in the `X-Carpool-Key` header (the `key` query parameter is also accepted).

| Route | Description |
| :--- | :--- |
| `GET /portal` | Buyer self-service page |
| `GET /api/status` | Seat status: quota windows, reset credits, cooldown, token usage, expiry |
| `GET /api/reset?confirm=1` | Redeems one reset credit and clears CPA's cooldown; does nothing without `confirm=1` |

```bash
curl -H "X-Carpool-Key: sk-buyer-alice" \
  https://your-domain/v0/resource/plugins/cpa-carpool/api/status
```

## 🔒 Security design

* **Fail closed**: an unparsable or invalid config rejects all requests; an expired seat or unavailable account rejects that seat's requests, which never fall back to other accounts.
* **Exact matching**: auth IDs are compared exactly, with no prefix, suffix or email matching.
* **Field whitelist**: the portal only returns quota windows, reset credits (title, status, times), seat name, cooldown state and token usage. Email, account_id and tokens never appear in responses; raw upstream errors only go to the CPA log, and buyers see a generic message.
* **Keys stay out of URLs**: the portal sends the key in a header and share links use `#key=`, so keys don't end up in gateway or reverse-proxy access logs.
* **Rate-limited upstream calls**: quota results are cached per account for 30 seconds, so buyers refreshing the page don't hammer ChatGPT; resets run one at a time, so a double click never redeems twice.
* **No direct access to credential files**: credentials and cooldown state come from CPA host callbacks.
* **Hardened page**: everything rendered is escaped, and responses carry CSP, `Referrer-Policy: no-referrer` and similar headers.

The portal does not need CPA's management key; access control rests entirely on keeping buyer keys secret. Always serve it over **HTTPS** and use long random keys (for example `openssl rand -hex 24`). See [SECURITY.md](./SECURITY.md) (Chinese) for how to report vulnerabilities.

## 🚧 Known limitations

* **Do not combine it with key-router or any other scheduler plugin.** CPA calls only one scheduler plugin at a time (the highest priority one) and sends anything it doesn't handle straight to the built-in scheduler, never to the next plugin. If key-router has a higher priority, this plugin's bindings **silently stop working**. Keep a single scheduler plugin.
* Quota lookup and reset credits only support Codex (ChatGPT) accounts and rely on undocumented endpoints; if those change, this part stops working (scheduling is unaffected).
* The portal can only look up file-backed accounts. API-key accounts configured in `config.yaml` are bound and scheduled normally, but CPA's host callbacks don't list them to plugins, so the portal reports the account as unavailable.
* The portal only shows the quota of the first loaded account in `primary`.
* Token history comes from CPA Manager Plus's database and shows 0 without it.
* Seats are managed through `config.yaml` only; there is no admin UI yet.

## 🔨 Development

```bash
go test ./...                 # unit tests, no C compiler needed (CGO_ENABLED=0 works)
go vet ./...
sh build.sh test              # run the tests and build out/cpa-carpool.so in Docker
PLATFORM=linux/arm64 sh build.sh   # cross-build for arm64 (needs QEMU support in Docker)
```

```text
.
├── main.go            # C ABI entry points (cgo): plugin init, host callback bridge
├── plugin.go          # method dispatch, config loading (strict, fail-closed)
├── scheduler.go       # seat compilation, key identification, dedicated scheduling
├── portal.go          # portal routes and the status / reset endpoints
├── host.go            # CPA host callbacks: accounts, credentials, cooldown, logging
├── upstream.go        # ChatGPT quota and reset-credit API, 30 s cache, field whitelist
├── stats.go           # reads CPA Manager Plus's usage database
├── types.go           # config and response types
├── portal.html        # single-page buyer portal (embedded at build time)
├── carpool_test.go    # unit tests (fake CPA host and fake ChatGPT upstream)
├── config.example.yaml
├── docs/images/       # screenshots and diagrams used by the READMEs
├── README.md          # Chinese README
└── build.sh           # Docker build script (golang:1.26-bookworm)
```

Please read [CONTRIBUTING.md](./CONTRIBUTING.md) (Chinese) before contributing. Issues and pull requests in English are welcome.

## 🧭 Roadmap

- [x] **v0.1.0**: standalone prototype (Python + Caddy) validating the countdowns, one-click reset and field masking
- [x] **v0.2.0**: native CPA v8 plugin; exact binding, lease expiry, fail-closed config; cooldown cleared through host callbacks
- [ ] **v0.3.0**: per-key concurrency / RPM limits; client setup guides in the portal (Codex CLI, Cursor, Claude Code)
- [ ] **v0.4.0**: admin UI (seat management, key generation); publish to the CLIProxyAPI plugin store

## 📜 Disclaimer

* This is an independent community project. It is **not affiliated with or endorsed by** CLIProxyAPI, OpenAI, Anthropic or any other provider.
* The terms of most AI services, including ChatGPT and Claude, restrict or forbid sharing, reselling or transferring accounts. Whether and how you use this plugin is your own decision and responsibility, including the risk of accounts being restricted or banned.
* Quota lookup and reset-credit redemption call undocumented ChatGPT endpoints that may change or break at any time, and the provider may treat such calls as abnormal use.
* The software is provided "as is", without warranty of any kind; see [LICENSE](./LICENSE).

## 📄 License

[MIT](./LICENSE) © 2026 212duo

Release archives include a `third_party_licenses/` directory with the licenses of the third-party modules compiled into the plugin (CLIProxyAPI SDK, modernc.org/sqlite, gopkg.in/yaml.v3 and others, all permissive MIT / BSD / Apache-2.0 style licenses).
