"use strict";

/*
 * Node built-in tests for the dependency-free UI helpers.
 * Run with: node --test internal/web/*.test.cjs
 *
 * ui.js is loaded as a CommonJS module (no DOM in Node), so only its pure helpers run.
 */

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const ui = require("./ui.js");

const DIR = __dirname;
const readAsset = (name) => fs.readFileSync(path.join(DIR, name), "utf8");

test("deriveEndpoints keeps a reverse-proxy prefix before /v0", () => {
  const proxied = ui.deriveEndpoints({
    pathname: "/gateway/cliproxy/v0/resource/plugins/cliproxyapi-opencode-provider/ui"
  });
  assert.equal(proxied.prefix, "/gateway/cliproxy");
  assert.equal(
    proxied.keysUrl,
    "/gateway/cliproxy/v0/management/plugins/cliproxyapi-opencode-provider/keys"
  );
  assert.equal(proxied.credentialsUrl, "/gateway/cliproxy/v8/management/credentials");
});

test("deriveEndpoints works at the origin root", () => {
  const root = ui.deriveEndpoints({
    pathname: "/v0/resource/plugins/cliproxyapi-opencode-provider/ui"
  });
  assert.equal(root.prefix, "");
  assert.equal(root.keysUrl, "/v0/management/plugins/cliproxyapi-opencode-provider/keys");
  assert.equal(root.credentialsUrl, "/v8/management/credentials");
});

test("deriveEndpoints falls back to origin-relative paths for unknown pages", () => {
  const fallback = ui.deriveEndpoints({ pathname: "/some/stub/ui.html" });
  assert.equal(fallback.keysUrl, "/v0/management/plugins/cliproxyapi-opencode-provider/keys");
  assert.equal(fallback.credentialsUrl, "/v8/management/credentials");
});

test("plaintext HTTP is only allowed on loopback", () => {
  assert.equal(ui.isSecureUiContext({ protocol: "https:", hostname: "cpa.example.com" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "localhost" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "127.0.0.1" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "[::1]" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "::1" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "cpa.example.com" }), false);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "192.168.1.10" }), false);
  assert.equal(ui.isSecureUiContext({ protocol: "file:", hostname: "" }), false);
});

test("parseKeys trims, drops blanks, dedupes and reports counts", () => {
  const parsed = ui.parseKeys("  key-one  \n\nkey-two\r\nkey-one\n   \nkey-two\r");
  assert.deepEqual(parsed.keys, ["key-one", "key-two"]);
  assert.equal(parsed.error, null);
  assert.deepEqual(parsed.counts, {
    lines: 7,
    empty: 3,
    duplicates: 2,
    invalid: 0,
    valid: 2
  });
});

test("parseKeys rejects interior whitespace, control characters and over-long keys", () => {
  const parsed = ui.parseKeys("good-key\nbad key\ntab\tkey\n" + "x".repeat(4097) + "\nok-key");
  assert.deepEqual(parsed.keys, ["good-key", "ok-key"]);
  assert.deepEqual(parsed.invalidLines, [
    { line: 2, reason: "whitespace" },
    { line: 3, reason: "control" },
    { line: 4, reason: "too-long" }
  ]);
  assert.equal(parsed.counts.invalid, 3);
});

test("parseKeys enforces the 100-key ceiling without returning partial input", () => {
  const lines = [];
  for (let i = 0; i < 101; i++) {
    lines.push("key-" + i);
  }
  const parsed = ui.parseKeys(lines.join("\n"));
  assert.equal(parsed.error.code, "too-many");
  assert.deepEqual(parsed.keys, []);
  assert.equal(parsed.counts.valid, 101);
});

test("validateLabel keeps text but rejects control characters and over-long labels", () => {
  assert.deepEqual(ui.validateLabel("  batch 1  "), { ok: true, value: "batch 1", reason: "" });
  assert.equal(ui.validateLabel("line\nbreak").ok, false);
  assert.equal(ui.validateLabel("x".repeat(129)).reason, "too-long");
  assert.deepEqual(ui.validateLabel(undefined), { ok: true, value: "", reason: "" });
});

test("buildImportPayload measures the UTF-8 body against the 64 KiB budget", () => {
  const small = ui.buildImportPayload(["key-one"], "label");
  assert.equal(small.ok, true);
  assert.deepEqual(JSON.parse(small.body), { keys: ["key-one"], label: "label" });

  // Multi-byte characters must count as multiple bytes, not characters.
  const multibyte = ui.utf8ByteLength("密钥");
  assert.equal(multibyte, 6);
  assert.equal(ui.utf8ByteLength("abc"), 3);

  const huge = ui.buildImportPayload(["k".repeat(4096)].concat(
    Array.from({ length: 17 }, (_, i) => "j" + i + "k".repeat(4096))
  ));
  assert.equal(huge.ok, false);
  assert.ok(huge.bytes > ui.MAX_BODY_BYTES);
});

test("normalizeFileEntries renders hostile content as inert text and flags unsafe names", () => {
  const rows = ui.normalizeFileEntries([
    {
      name: 'evil\">\u0007<img src=x onerror=alert(1)>.json',
      label: "<script>alert(1)</script>",
      status: "active",
      disabled: false,
      unavailable: true,
      success: "3",
      failed: -2
    },
    { name: "../escape.json", label: "traversal" },
    { name: "sub/dir.json" },
    { name: "opencode-go-1.json", label: "batch", success: 4, failed: 1 },
    "not-an-object",
    null
  ]);

  assert.equal(rows.length, 4);
  // Control characters are stripped; markup stays literal text for textContent.
  assert.ok(!rows[0].name.includes("\u0007"));
  assert.ok(rows[0].name.includes("<img src=x onerror=alert(1)>"));
  assert.ok(rows[0].label.includes("<script>"));
  assert.equal(rows[0].unavailable, true);
  assert.equal(rows[0].success, 3);
  assert.equal(rows[0].failed, 0);
  // The raw name still contains a control character, so it must not be deletable.
  assert.equal(rows[0].deletable, false);

  assert.equal(rows[1].deletable, false);
  assert.equal(rows[2].deletable, false);
  assert.equal(rows[3].deletable, true);
  assert.equal(rows[3].success, 4);
  assert.equal(rows[3].failed, 1);
});

test("isSafeFileName and toCellText guard delete input", () => {
  assert.equal(ui.isSafeFileName("opencode-go-1.json"), true);
  assert.equal(ui.isSafeFileName("  spaced name.json  "), true);
  assert.equal(ui.isSafeFileName("../escape.json"), false);
  assert.equal(ui.isSafeFileName("a/b.json"), false);
  assert.equal(ui.isSafeFileName("a\\b.json"), false);
  assert.equal(ui.isSafeFileName("bad\u0000name"), false);
  assert.equal(ui.isSafeFileName(""), false);
  assert.equal(ui.isSafeFileName(".."), false);
  assert.equal(ui.isSafeFileName(42), false);

  assert.equal(ui.toCellText("a\u0000b\nc"), "abc");
  assert.equal(ui.toCellText({ html: "<b>x</b>" }), "");
  assert.equal(ui.toCellText("x".repeat(500)).length, 201);
});

test("formatImportResult reports success and partial imports without server strings", () => {
  const ok = ui.formatImportResult(200, { imported: 2, skipped: 1, failed: 0 });
  assert.equal(ok.ok, true);
  assert.equal(ok.level, "ok");
  assert.match(ok.summary, /成功 2/);

  const partial = ui.formatImportResult(207, { imported: 1, skipped: 0, failed: 1 });
  assert.equal(partial.ok, true);
  assert.equal(partial.level, "warn");
  assert.match(partial.summary, /部分导入完成/);

  // Untrusted server error text must never be surfaced.
  const failed = ui.formatImportResult(500, { error: "<img src=x onerror=alert(1)>" });
  assert.equal(failed.ok, false);
  assert.equal(failed.level, "error");
  assert.ok(!failed.summary.includes("img"));
  assert.ok(!failed.summary.includes("alert"));
});

test("describeHttpFailure returns fixed safe messages", () => {
  assert.match(ui.describeHttpFailure(401), /401/);
  assert.match(ui.describeHttpFailure(403), /远程管理/);
  assert.match(ui.describeHttpFailure(404), /版本/);
  assert.match(ui.describeHttpFailure(500), /服务端错误/);
  assert.ok(!ui.describeHttpFailure(418).includes("<"));
});

test("buildManagementRequest keeps the key in the Authorization header only", () => {
  const get = ui.buildManagementRequest("mgmt-secret", { method: "GET" });
  assert.deepEqual(get.headers, { Accept: "application/json", Authorization: "Bearer mgmt-secret" });
  assert.equal(get.credentials, "omit");
  assert.equal(get.cache, "no-store");
  assert.equal(get.body, undefined);

  const post = ui.buildManagementRequest("mgmt-secret", { method: "POST", body: '{"keys":["upstream"]}' });
  assert.equal(post.headers["Content-Type"], "application/json");
  assert.ok(!post.body.includes("mgmt-secret"));

  // Without a key no Authorization header is produced (no implicit/default auth).
  const anon = ui.buildManagementRequest("", { method: "GET" });
  assert.equal(Object.prototype.hasOwnProperty.call(anon.headers, "Authorization"), false);
});

test("ui.js contains no storage, HTML-injection, eval or external-request primitives", () => {
  const source = readAsset("ui.js");
  for (const forbidden of [
    "localStorage",
    "sessionStorage",
    "document.cookie",
    "indexedDB",
    "innerHTML",
    "outerHTML",
    "insertAdjacentHTML",
    "document.write",
    "eval(",
    "new Function",
    "console."
  ]) {
    assert.equal(source.includes(forbidden), false, "ui.js must not reference " + forbidden);
  }
  assert.equal(/https?:\/\//.test(source), false, "ui.js must not contain absolute URLs");
  assert.equal(source.includes("credentials: \"omit\""), true);
  assert.equal(source.includes("cache: \"no-store\""), true);
});

test("ui.html is CSP-safe and wires the embedded assets", () => {
  const html = readAsset("ui.html");
  assert.equal(/<style[\s>]/i.test(html), false, "no inline <style> block");
  assert.equal(/\sstyle\s*=/i.test(html), false, "no inline style attribute");
  assert.equal(/\son[a-z]+\s*=/i.test(html), false, "no inline event handler");
  assert.equal(html.includes('href="./ui.css"'), true);
  assert.equal(html.includes('src="./ui.js" defer'), true);
  assert.equal(/<script(?![^>]*\ssrc=)/i.test(html), false, "scripts must have a src (no inline JS)");
  assert.match(html, /type="password"[^>]*autocomplete="off"/);
  assert.match(html, /rel="noopener noreferrer"/);
  assert.ok(html.includes("https://github.com/fengwk/cliproxyapi-opencode-provider"));
  assert.ok(html.includes("https://opencode.ai/docs/go"));
  assert.ok(html.includes('aria-live="polite"'));
  assert.ok(html.includes("routing.session-affinity: true"));
});

// Run the actual form handler, not only the exported pure helpers.
async function submitImport(response, networkFailure = false) {
  const elements = new Map();
  const getElement = (id) => {
    if (!elements.has(id)) {
      elements.set(id, {
        value: "", disabled: false, hidden: false, textContent: "",
        handlers: {},
        addEventListener(name, handler) { this.handlers[name] = handler; },
        querySelectorAll() { return []; }
      });
    }
    return elements.get(id);
  };
  const requests = [];
  vm.runInNewContext(readAsset("ui.js"), {
    document: { readyState: "complete", getElementById: getElement },
    window: {
      location: {
        protocol: "http:", hostname: "127.0.0.1",
        pathname: "/v0/resource/plugins/cliproxyapi-opencode-provider/ui"
      }
    },
    TextEncoder,
    fetch: async (url, init) => {
      requests.push({ url, init });
      if (networkFailure) {
        throw new Error("uncertain network result");
      }
      return {
        status: response.status, ok: response.status >= 200 && response.status < 300,
        text: async () => JSON.stringify(response.body)
      };
    }
  });
  getElement("mgmt-key").value = "fake-management-key";
  getElement("keys").value = "fake-upstream-key";
  getElement("import-form").handlers.submit({ preventDefault() {} });
  for (let i = 0; i < 10 && getElement("import-btn").disabled; i++) {
    await new Promise((resolve) => setImmediate(resolve));
  }
  assert.equal(getElement("import-btn").disabled, false, "request must settle");
  assert.equal(requests.length, 1, "no automatic retry");
  assert.equal(requests[0].init.headers.Authorization, "Bearer fake-management-key");
  return getElement;
}

test("successful form import clears the submitted keys", async () => {
  const get = await submitImport({ status: 200, body: { imported: 1, skipped: 0, failed: 0 } });
  assert.equal(get("keys").value, "");
  assert.match(get("status").textContent, /导入成功/);
});

test("partial form import retains keys so failed entries can be retried", async () => {
  const get = await submitImport({ status: 207, body: { imported: 0, skipped: 0, failed: 1 } });
  assert.equal(get("keys").value, "fake-upstream-key");
  assert.match(get("status").textContent, /部分导入完成/);
});

test("network failure retains keys and does not claim the server rejected them", async () => {
  const get = await submitImport({}, true);
  assert.equal(get("keys").value, "fake-upstream-key");
  assert.match(get("status").textContent, /无法确认导入结果/);
  assert.equal(get("status").textContent.includes("未提交"), false);
});
