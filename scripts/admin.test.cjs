const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { webcrypto } = require("node:crypto");

const html = fs.readFileSync(path.join(__dirname, "../admin.html"), "utf8");
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const clone = (v) => JSON.parse(JSON.stringify(v));
const seat = (key = "sk-old") => ({ name: "Alice", "api-keys": [key], primary: ["alice.json"] });
const config = (key = "sk-old") => ({ enabled: true, priority: 100, users: [seat(key)] });

function page(options = {}) {
  const elements = new Map();
  const listeners = new Map();
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, {
      id, style: {}, hidden: false, disabled: false, value: "", innerHTML: "", textContent: "",
      focus() {}, contains() { return false; },
      addEventListener(event, fn) { listeners.set(`${id}:${event}`, fn); },
    });
    return elements.get(id);
  };
  const calls = [];
  const writes = [];
  let stored = clone(options.config || config());
  let keys = clone(options.keys || ["sk-owner", "sk-old"]);
  let runtimeRevision = "old";
  let revisionPolls = 0;
  let revocationPolls = 0;
  let addFailures = options.addFailures || 0;
  const reply = (status, data) => ({ status, ok: status >= 200 && status < 300, json: async () => clone(data) });
  const context = vm.createContext({
    document: { getElementById: element, querySelectorAll: () => [...elements.values()],
      addEventListener() {}, activeElement: null },
    window: { location: { pathname: "/gateway/v0/resource/plugins/cpa-carpool/admin", origin: "https://example.test" }, addEventListener() {} },
    sessionStorage: { getItem: () => "", setItem() {}, removeItem() {} },
    crypto: webcrypto, Uint8Array, AbortSignal, navigator: {}, confirm: () => true,
    setTimeout: (fn, ms) => { if (ms === 250) queueMicrotask(fn); return 0; },
    clearTimeout() {}, setInterval() {},
    fetch: async (url, init = {}) => {
      const method = init.method || "GET";
      const route = url.replace(/^\/gateway/, "");
      const body = init.body === undefined ? undefined : JSON.parse(init.body);
      calls.push({ method, route, body });
      if (options.onFetch) await options.onFetch({ method, route, body, context });
      if (options.unauthorized) return reply(401, { error: "unauthorized" });
      if (route === "/v1/models") {
        revocationPolls++;
        const active = options.neverRevoke || revocationPolls <= (options.revocationDelay || 0);
        return reply(active ? 200 : 401, {});
      }
      if (route.endsWith("/overview")) {
        if (writes.some((w) => w.route.includes("/plugins/configs/"))) {
          revisionPolls++;
          if (!options.neverReload && revisionPolls > (options.reloadDelay || 0)) runtimeRevision = "new";
        }
        return reply(200, { enabled: true, config_revision: runtimeRevision, seats: [] });
      }
      if (route.endsWith("/auths")) return reply(200, { auths: [] });
      if (route.endsWith("/validate")) return reply(200, { valid: true, config_revision: "new" });
      if (route.includes("/plugins/configs/")) {
        if (method === "GET") return reply(200, stored);
        assert.equal(method, "PUT", "replace the whole plugin subtree");
        stored = clone(body);
        writes.push({ route, body });
        return reply(200, { status: "ok" });
      }
      if (route.endsWith("/access/api-keys")) {
        if (method === "GET") return reply(200, keys);
        if (body.includes("sk-new") && addFailures-- > 0) return reply(500, { error: "write_failed" });
        if (body.includes("sk-new")) assert.equal(runtimeRevision, "new", "new key must wait for the routing configuration");
        keys = clone(body);
        writes.push({ route, body });
        return reply(200, { status: "ok" });
      }
      throw new Error(`Unexpected request: ${method} ${route}`);
    },
  });
  vm.runInContext(script, context, { filename: "admin.html" });
  context.fixture = { key: "management-test-key", original: config(), draft: config("sk-new"), apiKeys: clone(keys),
    configExists: true, overview: { seats: [] }, ...options.state };
  vm.runInContext("Object.assign(S, fixture)", context);
  return { context, calls, writes, elements, listeners,
    run: (js) => vm.runInContext(js, context),
    state: () => JSON.parse(vm.runInContext("JSON.stringify(S)", context)),
    keys: () => keys,
  };
}

test("one invalid login consumes one authentication attempt", async () => {
  const p = page({ unauthorized: true });
  await p.run("loadAll()");
  assert.equal(p.calls.length, 1);
  assert.equal(p.state().key, "");
});

test("key replacement waits for revocation and asynchronous routing reload", async () => {
  const p = page({ revocationDelay: 2, reloadDelay: 2 });
  await p.run("save()");
  assert.deepEqual(p.writes.map((w) => w.body), [["sk-owner"], config("sk-new"), ["sk-owner", "sk-new"]]);
  assert.equal(p.calls.filter((c) => c.route === "/v1/models").length, 3);
  assert.equal(p.state().saving, false);
  assert.equal(p.state().saveError, "");
});

test("removing the last authentication key never disables authentication", async () => {
  const p = page({ keys: ["sk-old"], state: { draft: { enabled: true, users: [] } } });
  await p.run("save()");
  assert.deepEqual(p.writes, []);
  assert.match(p.state().saveError, /关闭 CPA 认证/);
});

test("if revocation does not take effect the binding is preserved", async () => {
  const p = page({ neverRevoke: true });
  await p.run("save()");
  assert.equal(p.writes.length, 1);
  assert.match(p.state().saveError, /旧 key 已撤销/);
});

test("if routing reload fails the new key remains unauthorized", async () => {
  const p = page({ neverReload: true });
  await p.run("save()");
  assert.deepEqual(p.keys(), ["sk-owner"]);
  assert.match(p.state().saveError, /绑定生效/);
});

test("partially saved configuration can be retried without restoring revoked keys", async () => {
  const p = page({ addFailures: 1 });
  await p.run("save()");
  assert.match(p.state().saveError, /新 key 未能加入/);
  assert.equal(p.run("needsSave()"), true);
  await p.run("save()");
  assert.deepEqual(p.keys(), ["sk-owner", "sk-new"]);
  assert.equal(p.state().saveError, "");
});

test("unchanged seats with missing authentication keys offer a repair save", async () => {
  const p = page({ keys: ["sk-owner"], state: { draft: config() } });
  assert.equal(p.run("dirty()"), false);
  assert.equal(p.run("needsSave()"), true);
  await p.run("save()");
  assert.deepEqual(p.keys(), ["sk-owner", "sk-old"]);
});

test("a stale editor cannot overwrite another page's configuration", async () => {
  const p = page({ config: { ...config(), rpm: 20 } });
  await p.run("save()");
  assert.equal(p.writes.length, 0);
  assert.match(p.state().saveError, /其他页面修改/);
});

test("save snapshots edits and prevents changes and duplicate submissions while busy", async () => {
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  const p = page({ onFetch: async ({ route }) => { if (route.endsWith("/validate")) await gate; } });
  const saving = p.run("save()");
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(p.state().saving, true);
  assert.equal(p.elements.get("refreshBtn").disabled, true);
  p.run('onSettingInput({target:{dataset:{setting:"rpm"},type:"number",value:"100"}})');
  assert.equal(p.state().draft.rpm, undefined);
  await p.run("save()");
  assert.equal(p.calls.filter((c) => c.route.endsWith("/validate")).length, 1);
  release();
  await saving;
  assert.equal(p.state().saving, false);
});

test("invalid default limits never silently become unlimited", async () => {
  for (const value of ["-1", "1.5"]) {
    const p = page();
    p.context.invalidLimit = value;
    p.run('onSettingInput({target:{dataset:{setting:"rpm"},type:"number",value:invalidLimit}})');
    await p.run("save()");
    assert.equal(p.writes.length, 0);
    assert.match(p.state().saveError, /非负整数/);
  }
});

test("first setup works when the persisted configuration has no users field", async () => {
  const p = page({ config: { enabled: true, priority: 100 } });
  await p.run("loadAll()");
  assert.equal(p.state().original.users, undefined);
  p.context.firstSeat = seat("sk-new");
  p.run("S.draft.users.push(firstSeat)");
  await p.run("save()");
  assert.equal(p.state().saveError, "");
  assert.equal(p.state().original.users.length, 1);
  assert(p.keys().includes("sk-new"));
});

test("unavailable usage is not displayed as a zero or a stale count", () => {
  const p = page({ state: { draft: config(), overview: { usage_database: true, usage_available: false,
    seats: [{ name: "Alice", usage: { local_calls: 5, total_tokens: 88 } }] } } });
  p.run("renderMain()");
  assert.match(p.elements.get("app").innerHTML, /历史用量暂不可用/);
  assert.doesNotMatch(p.elements.get("app").innerHTML, /5 次/);
  assert.equal(p.run("formatTokensM(88)"), "88");
});
