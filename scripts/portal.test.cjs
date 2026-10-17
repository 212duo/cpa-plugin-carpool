const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "../portal.html"), "utf8").match(/<script>([\s\S]*?)<\/script>/)[1];

async function query(status) {
  const nodes = new Map();
  const node = (id) => {
    if (!nodes.has(id)) nodes.set(id, { value: "sk-demo", innerHTML: "", style: {} });
    return nodes.get(id);
  };
  const context = vm.createContext({
    document: { getElementById: node, addEventListener() {} },
    window: { location: { pathname: "/v0/resource/plugins/cpa-carpool/portal", origin: "https://example.test" } },
    fetch: async () => ({ ok: true, status: 200, json: async () => ({ quota_supported: false, seat_name: "Demo", ...status }) }),
    setInterval: () => 0, clearInterval() {},
  });
  vm.runInContext(source, context, { filename: "portal.html" });
  vm.runInContext("setupGuide = () => {}", context);
  await vm.runInContext("doQuery()", context);
  return node("resultArea").innerHTML;
}

test("unavailable history is never displayed as zero usage", async () => {
  const html = await query({ usage_available: false, local_calls: 0, total_tokens: 0 });
  assert.match(html, /统计暂不可用/);
  assert.match(html, /不表示用量为 0/);
  assert.doesNotMatch(html, /0 次 · 总计/);
  assert.equal((html.match(/>--<\/div>/g) || []).length, 3);
});

test("a valid empty history displays a real zero", async () => {
  const html = await query({ usage_available: true, local_calls: 0, total_tokens: 0 });
  assert.match(html, /0 次 · 总计 0 tokens/);
  assert.doesNotMatch(html, /统计暂不可用/);
});

test("small positive token counts are not rounded to zero", async () => {
  const html = await query({ usage_available: true, local_calls: 1, input_tokens: 100, output_tokens: 26, cached_tokens: 1, total_tokens: 127 });
  assert.match(html, /1 次 · 总计 127 tokens/);
  assert.match(html, /当前 Key 历史调用/);
  assert.doesNotMatch(html, /0\.000M/);
});
