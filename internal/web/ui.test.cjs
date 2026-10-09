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
  const ok = ui.formatImportResult(200, { imported: 2, skipped: 1, failed: 0 }, 3);
  assert.equal(ok.ok, true);
  assert.equal(ok.level, "ok");
  assert.match(ok.summary, /成功 2/);

  const partial = ui.formatImportResult(207, { imported: 1, skipped: 0, failed: 1 }, 2);
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

test("formatImportResult accepts only trusted counts summing to the submitted total", () => {
  const check = (body, expected) => ui.formatImportResult(200, body, expected);

  // Missing or non-integer counts are never treated as a confirmed success.
  assert.equal(check({}, 0).ok, false);
  assert.equal(check(null, 0).ok, false);
  assert.equal(check({ imported: 1, skipped: 0 }, 1).ok, false); // missing failed
  assert.equal(check({ imported: "1", skipped: 0, failed: 0 }, 1).ok, false); // string
  assert.equal(check({ imported: -1, skipped: 2, failed: 0 }, 1).ok, false); // negative
  assert.equal(check({ imported: 1.5, skipped: 0, failed: 0 }, 1).ok, false); // fraction
  assert.equal(check({ imported: NaN, skipped: 0, failed: 0 }, 1).ok, false);
  assert.equal(
    check({ imported: Number.MAX_SAFE_INTEGER + 1, skipped: 0, failed: 0 }, 1).ok,
    false // unsafe integer
  );
  // The three counts must account for exactly the submitted deduped keys.
  assert.equal(check({ imported: 1, skipped: 0, failed: 0 }, 3).ok, false);
  assert.equal(check({ imported: 1, skipped: 0, failed: 0 }, undefined).ok, false);
  assert.equal(check({ imported: 1, skipped: 0, failed: 0 }, "1").ok, false);
  // Every unconfirmed body uses the same fixed local, non-reflective message.
  assert.match(check({}, 0).summary, /无法确认导入结果/);

  // A contract-compliant success is accepted and exposes failed so the caller
  // can decide whether the input may be cleared.
  const confirmed = check({ imported: 2, skipped: 1, failed: 0 }, 3);
  assert.equal(confirmed.ok, true);
  assert.equal(confirmed.level, "ok");
  assert.equal(confirmed.failed, 0);
  assert.match(confirmed.summary, /成功 2/);
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

// Unnamed secret controls cannot enter native form URLs or CSP diagnostics.
test("ui.html excludes sensitive controls from native form serialization", () => {
  const html = readAsset("ui.html");
  for (const id of ["mgmt-key", "keys"]) {
    const controls = html.match(new RegExp(`<(?:input|textarea)\\b[^>]*\\bid=["']${id}["'][^>]*>`, "gi"));
    assert.equal(controls && controls.length, 1, `expected one sensitive control: ${id}`);
    assert.doesNotMatch(controls[0], /\sname(?:\s|=|\/?>)/i, `${id} must not have a name attribute`);
  }
});

test("ui.html shows the plugin display name and keeps the technical ID", () => {
  const html = readAsset("ui.html");
  assert.ok(html.includes("<title>OpenCode Provider · 密钥与配额管理</title>"));
  assert.ok(html.includes("<h1>OpenCode Provider</h1>"));
  // The technical ID stays visible in the subtitle.
  assert.ok(html.includes("<code>cliproxyapi-opencode-provider</code>"));
  // Upstream-facing import labels still name the OpenCode Go upstream.
  assert.ok(html.includes("导入 OpenCode Go 密钥"));
});

// The manual-model alignment fix is CSS-only and stays scoped: the credential
// and quota tables keep the global top alignment, the draft table centers each
// row on a compact delete button, and the control row shares one bottom edge.
test("manual-model alignment rules stay scoped to the draft table and control row", () => {
  const css = readAsset("ui.css");
  const html = readAsset("ui.html");
  // Read a rule body anchored at a line start so a lookalike selector (e.g.
  // ".model-fields input, .model-fields select") never satisfies another rule.
  const rule = (selector) => {
    const pattern = new RegExp(
      "(?:^|\\n)[ \\t]*" + selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "\\s*\\{([^}]*)\\}"
    );
    const match = css.match(pattern);
    assert.ok(match, "missing CSS rule: " + selector);
    return match[1];
  };
  // Only the manual draft table opts into the scoped treatment.
  assert.match(html, /<table class="manual-model-table">/);
  // Other tables (credentials, quota) keep the global top alignment.
  assert.match(rule("th,\ntd"), /vertical-align:\s*top/);
  assert.match(rule(".manual-model-table td"), /vertical-align:\s*middle/);
  const button = rule(".manual-model-table td button");
  assert.match(button, /white-space:\s*nowrap/);
  assert.match(button, /font-size:\s*0\.8125rem/);
  assert.match(button, /line-height:\s*1\.2/);
  // The flex control row owns the outer spacing; its fields add none, and the
  // select inherits the input font so all three controls share a bottom edge.
  assert.match(rule(".model-fields .field"), /margin:\s*0/);
  assert.match(rule(".model-fields"), /margin:\s*0\.75rem 0/);
  assert.match(rule(".model-fields select"), /font:\s*inherit/);
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

// Same fake DOM as submitImport, but the fetch promise stays pending until the
// test resolves it, so the locked inputs can be observed mid-request.
async function startImportDeferred() {
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
  let resolveFetch;
  const pending = new Promise((resolve) => { resolveFetch = resolve; });
  vm.runInNewContext(readAsset("ui.js"), {
    document: { readyState: "complete", getElementById: getElement },
    window: {
      location: {
        protocol: "http:", hostname: "127.0.0.1",
        pathname: "/v0/resource/plugins/cliproxyapi-opencode-provider/ui"
      }
    },
    TextEncoder,
    fetch: (url, init) => {
      requests.push({ url, init });
      return pending;
    }
  });
  getElement("mgmt-key").value = "fake-management-key";
  getElement("keys").value = "fake-upstream-key";
  getElement("label").value = "batch-1";
  getElement("import-form").handlers.submit({ preventDefault() {} });
  // Let the handler reach fetch before the test inspects the in-flight state.
  await new Promise((resolve) => setImmediate(resolve));
  return {
    get: getElement,
    requests,
    respond: (status, body) => resolveFetch({
      status,
      ok: status >= 200 && status < 300,
      text: async () => body
    }),
    settle: async () => {
      for (let i = 0; i < 10 && getElement("import-btn").disabled; i++) {
        await new Promise((resolve) => setImmediate(resolve));
      }
    }
  };
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

test("import locks the key and label inputs while the request is pending", async () => {
  const flow = await startImportDeferred();
  // In flight: both secret-bearing inputs must be locked so a late response can
  // never clear text the user typed after submitting.
  assert.equal(flow.get("keys").disabled, true);
  assert.equal(flow.get("label").disabled, true);
  assert.equal(flow.get("import-btn").disabled, true);
  assert.equal(flow.requests.length, 1);
  // The wire body matches the parsed, deduped, labelled input.
  assert.deepEqual(JSON.parse(flow.requests[0].init.body), {
    keys: ["fake-upstream-key"], label: "batch-1"
  });

  flow.respond(200, JSON.stringify({ imported: 1, skipped: 0, failed: 0 }));
  await flow.settle();
  // Controls are restored and a confirmed success clears only the key textarea.
  assert.equal(flow.get("keys").disabled, false);
  assert.equal(flow.get("label").disabled, false);
  assert.equal(flow.get("keys").value, "");
  assert.equal(flow.get("label").value, "batch-1");
  assert.match(flow.get("status").textContent, /导入成功/);
});

test("unconfirmed 2xx import bodies retain the keys and report an unknown result", async () => {
  const bodies = [
    "<html><body>ok</body></html>", // HTML page instead of JSON
    "",                             // empty body -> readJson fallback {}
    "{}",                           // missing imported/skipped/failed
    JSON.stringify({ imported: 2, skipped: 0, failed: 0 }) // count mismatch
  ];
  for (const body of bodies) {
    const flow = await startImportDeferred();
    flow.respond(200, body);
    await flow.settle();
    assert.equal(flow.get("keys").value, "fake-upstream-key", "keys retained for " + JSON.stringify(body));
    assert.match(flow.get("status").textContent, /无法确认导入结果/, "unknown reported for " + JSON.stringify(body));
    assert.equal(flow.requests.length, 1, "no automatic retry");
    assert.equal(flow.get("keys").disabled, false, "controls restored");
  }
});

test("a valid skipped-only 200 report clears the submitted keys", async () => {
  const flow = await startImportDeferred();
  // Every submitted key was skipped but the counts still confirm the response.
  flow.respond(200, JSON.stringify({ imported: 0, skipped: 1, failed: 0 }));
  await flow.settle();
  assert.equal(flow.get("keys").value, "");
  assert.match(flow.get("status").textContent, /导入成功/);
  assert.equal(flow.get("keys").disabled, false);
});

test("a 200 report carrying failures retains the keys", async () => {
  const flow = await startImportDeferred();
  flow.respond(200, JSON.stringify({ imported: 0, skipped: 0, failed: 1 }));
  await flow.settle();
  assert.equal(flow.get("keys").value, "fake-upstream-key");
  assert.match(flow.get("status").textContent, /部分导入完成/);
});

/* ---------------------------------------------------------------- quota helpers */

test("deriveEndpoints exposes the native plugin quota endpoint with a proxy prefix", () => {
  const proxied = ui.deriveEndpoints({
    pathname: "/gateway/cliproxy/v0/resource/plugins/cliproxyapi-opencode-provider/ui"
  });
  assert.equal(
    proxied.quotaUrl,
    "/gateway/cliproxy/v0/management/plugins/cliproxyapi-opencode-provider/quota"
  );
  const root = ui.deriveEndpoints({ pathname: "/some/stub/ui.html" });
  assert.equal(root.quotaUrl, "/v0/management/plugins/cliproxyapi-opencode-provider/quota");
});

test("isSafeAuthIndex and buildQuotaPayload bound the credential index", () => {
  assert.equal(ui.isSafeAuthIndex("idx-1"), true);
  assert.equal(ui.isSafeAuthIndex("  idx-1  "), true);
  assert.equal(ui.isSafeAuthIndex(""), false);
  assert.equal(ui.isSafeAuthIndex("   "), false);
  assert.equal(ui.isSafeAuthIndex("a b"), false);
  assert.equal(ui.isSafeAuthIndex("bad\u0000index"), false);
  assert.equal(ui.isSafeAuthIndex("x".repeat(ui.MAX_AUTH_INDEX_LENGTH + 1)), false);
  assert.equal(ui.isSafeAuthIndex(42), false);

  const body = ui.buildQuotaPayload("idx-1");
  assert.equal(body.ok, true);
  assert.deepEqual(JSON.parse(body.body), { auth_index: "idx-1" });
  // No unvalidated index ever reaches the wire.
  const empty = ui.buildQuotaPayload("a b");
  assert.equal(empty.ok, false);
  assert.equal(empty.body, "");
});

test("isValidIsoTimestamp and formatQuotaReset accept only strict RFC3339", () => {
  assert.equal(ui.isValidIsoTimestamp("2026-10-07T00:00:00Z"), true);
  assert.equal(ui.isValidIsoTimestamp("2026-10-07T00:00:00.500+08:00"), true);
  assert.equal(ui.isValidIsoTimestamp("2026-10-07 00:00:00"), false);
  assert.equal(ui.isValidIsoTimestamp("not-a-date"), false);
  assert.equal(ui.isValidIsoTimestamp("2026-13-99T00:00:00Z"), false);
  assert.equal(ui.isValidIsoTimestamp(1234), false);

  assert.equal(ui.formatQuotaReset("2026-10-07T00:00:00Z"), "2026-10-07 00:00 UTC");
  assert.equal(ui.formatQuotaReset("2026-10-07T09:05:00Z"), "2026-10-07 09:05 UTC");
  // An unparseable value never yields a fabricated display.
  assert.equal(ui.formatQuotaReset("not-a-date"), "");
});

test("parseQuotaResponse maps the three windows and derives percentages", () => {
  const parsed = ui.parseQuotaResponse({
    subscription: { plan: "OpenCode Go" },
    groups: [
      {
        displayName: "OpenCode Go",
        buckets: [
          { window: "monthly", remainingFraction: 1, resetTime: "2026-11-01T00:00:00Z" },
          { window: "rolling", remainingFraction: 0.75, resetTime: "2026-10-07T00:00:00Z" },
          { window: "weekly", remainingFraction: 0, resetTime: "2026-10-08T00:00:00+00:00" }
        ]
      },
      // Unknown windows are ignored, not guessed at.
      { displayName: "extra", buckets: [{ window: "bonus", remainingFraction: 0.5, resetTime: "2026-10-09T00:00:00Z" }] }
    ]
  });
  assert.equal(parsed.ok, true);
  assert.equal(parsed.plan, "OpenCode Go");
  assert.deepEqual(
    parsed.buckets.map((b) => [b.window, b.label, b.remainingPercent, b.usedPercent]),
    [
      ["rolling", "滚动窗口", 75, 25],
      ["weekly", "每周窗口", 0, 100],
      ["monthly", "每月窗口", 100, 0]
    ]
  );
  assert.equal(parsed.buckets[0].resetTime, "2026-10-07 00:00 UTC");
  assert.equal(parsed.buckets[0].remainingFraction, 0.75);
});

test("parseQuotaResponse rejects malformed data instead of inferring values", () => {
  const valid = () => ({
    subscription: { plan: "OpenCode Go" },
    groups: [
      {
        buckets: [
          { window: "rolling", remainingFraction: 0.5, resetTime: "2026-10-07T00:00:00Z" },
          { window: "weekly", remainingFraction: 0.5, resetTime: "2026-10-08T00:00:00Z" },
          { window: "monthly", remainingFraction: 0.5, resetTime: "2026-11-01T00:00:00Z" }
        ]
      }
    ]
  });
  const mutate = (fn) => {
    const payload = valid();
    fn(payload);
    return ui.parseQuotaResponse(payload);
  };

  assert.equal(ui.parseQuotaResponse(null).ok, false);
  assert.equal(ui.parseQuotaResponse({}).ok, false);
  // Missing subscription plan is not assumed.
  assert.equal(mutate((p) => delete p.subscription).ok, false);
  assert.equal(mutate((p) => delete p.subscription.plan).ok, false);
  // A missing official window must never be shown as full remaining.
  assert.equal(mutate((p) => { p.groups[0].buckets.pop(); }).ok, false);
  // A duplicated window is ambiguous.
  assert.equal(mutate((p) => { p.groups[0].buckets[2].window = "rolling"; }).ok, false);
  // Fractions must be finite numbers inside 0..1, not strings or out-of-range.
  assert.equal(mutate((p) => { p.groups[0].buckets[0].remainingFraction = "0.5"; }).ok, false);
  assert.equal(mutate((p) => { p.groups[0].buckets[0].remainingFraction = 1.5; }).ok, false);
  assert.equal(mutate((p) => { p.groups[0].buckets[0].remainingFraction = -0.1; }).ok, false);
  assert.equal(mutate((p) => { p.groups[0].buckets[0].remainingFraction = NaN; }).ok, false);
  // Reset times must be valid ISO timestamps.
  assert.equal(mutate((p) => { p.groups[0].buckets[0].resetTime = "tomorrow"; }).ok, false);
  assert.equal(mutate((p) => { p.groups[0].buckets[0].resetTime = undefined; }).ok, false);
});

test("describeQuotaFailure returns fixed messages without server text", () => {
  assert.match(ui.describeQuotaFailure(400), /400/);
  assert.match(ui.describeQuotaFailure(404), /版本/);
  assert.match(ui.describeQuotaFailure(501), /不支持/);
  assert.match(ui.describeQuotaFailure(502), /服务端错误/);
  assert.ok(!ui.describeQuotaFailure(418).includes("<"));
});

test("normalizeFileEntries flags which rows can query quota", () => {
  const rows = ui.normalizeFileEntries([
    { name: "opencode-go-1.json", auth_index: "idx-1" },
    { name: "opencode-go-2.json" },
    { name: "opencode-go-3.json", auth_index: "bad index" }
  ]);
  assert.equal(rows[0].quotaAvailable, true);
  assert.equal(rows[0].authIndex, "idx-1");
  assert.equal(rows[1].quotaAvailable, false);
  assert.equal(rows[2].quotaAvailable, false);
});

/* ---------------------------------------------------------------- quota DOM flow */

// Minimal DOM stand-in: enough structure for the real ui.js handlers to run
// without pulling in a browser or any dependency.
function matchesSelector(element, selector) {
  const spec = /^([a-zA-Z]+)?(?:\[([^\]=]+)(?:=['"]?([^'"\]]*)['"]?)?\])?$/.exec(selector.trim());
  if (!spec) {
    return false;
  }
  const tag = spec[1];
  const attr = spec[2];
  const value = spec[3];
  if (tag && element.tagName !== tag.toUpperCase()) {
    return false;
  }
  if (attr) {
    if (!Object.prototype.hasOwnProperty.call(element.attributes, attr)) {
      return false;
    }
    if (value !== undefined && element.attributes[attr] !== value) {
      return false;
    }
  }
  return true;
}

function createQuotaDom() {
  const registry = new Map();

  function makeElement(tag) {
    const el = {
      tagName: String(tag).toUpperCase(),
      children: [],
      attributes: {},
      handlers: {},
      textContent: "",
      className: "",
      value: "",
      disabled: false,
      hidden: false,
      max: 0,
      type: "",
      focus() {},
      appendChild(child) {
        this.children.push(child);
        child.parentNode = this;
        return child;
      },
      removeChild(child) {
        const index = this.children.indexOf(child);
        if (index >= 0) {
          this.children.splice(index, 1);
        }
        child.parentNode = null;
        return child;
      },
      setAttribute(name, value) {
        this.attributes[name] = String(value);
      },
      getAttribute(name) {
        return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null;
      },
      addEventListener(name, handler) {
        this.handlers[name] = handler;
      },
      querySelectorAll(selector) {
        const found = [];
        const walk = (node) => {
          for (const child of node.children) {
            if (matchesSelector(child, selector)) {
              found.push(child);
            }
            walk(child);
          }
        };
        walk(this);
        return found;
      },
      get firstChild() {
        return this.children.length ? this.children[0] : null;
      }
    };
    el.classList = {
      toggle(name, force) {
        const parts = el.className ? el.className.split(/\s+/).filter(Boolean) : [];
        const has = parts.indexOf(name) >= 0;
        const on = force === undefined ? !has : Boolean(force);
        if (on && !has) {
          parts.push(name);
        }
        if (!on && has) {
          parts.splice(parts.indexOf(name), 1);
        }
        el.className = parts.join(" ");
        return on;
      }
    };
    return el;
  }

  return {
    document: {
      readyState: "complete",
      addEventListener() {},
      createElement: makeElement,
      getElementById(id) {
        if (!registry.has(id)) {
          registry.set(id, makeElement("div"));
        }
        return registry.get(id);
      }
    }
  };
}

const QUOTA_OK_BODY = {
  subscription: { plan: "OpenCode Go" },
  groups: [
    {
      displayName: "OpenCode Go",
      buckets: [
        { window: "rolling", remainingFraction: 0.75, resetTime: "2026-10-07T00:00:00Z" },
        { window: "weekly", remainingFraction: 0.25, resetTime: "2026-10-08T00:00:00+00:00" },
        { window: "monthly", remainingFraction: 0, resetTime: "2026-11-01T00:00:00Z" }
      ]
    }
  ]
};

// Boot the real ui.js against a fake DOM and a scripted fetch. The list route
// returns `files`; the quota route returns `quotaResponse`.
async function mountQuotaUi(files, quotaResponse, locationOverride) {
  const dom = createQuotaDom();
  const requests = [];
  const fetchImpl = async (url, init) => {
    requests.push({ url, init });
    const scripted = url.indexOf("/quota") >= 0 ? quotaResponse :
      { status: 200, body: url.endsWith("/settings") ? { "manual-models": [] } : { files } };
    if (!scripted) {
      throw new Error("unexpected fetch");
    }
    return {
      status: scripted.status,
      ok: scripted.status >= 200 && scripted.status < 300,
      text: async () => JSON.stringify(scripted.body),
      json: async () => scripted.body
    };
  };
  vm.runInNewContext(readAsset("ui.js"), {
    document: dom.document,
    window: {
      location: locationOverride || {
        protocol: "http:", hostname: "127.0.0.1",
        pathname: "/v0/resource/plugins/cliproxyapi-opencode-provider/ui"
      },
      confirm: () => true,
      setTimeout,
      clearTimeout
    },
    AbortController,
    TextEncoder,
    fetch: fetchImpl
  });
  const get = (id) => dom.document.getElementById(id);
  const settle = async () => {
    // Wait out any in-flight request (disabled controls mean "busy").
    for (let i = 0; i < 30 && get("reload-btn").disabled; i++) {
      await new Promise((resolve) => setImmediate(resolve));
    }
  };
  get("mgmt-key").value = "fake-management-key";
  get("auth-form").handlers.submit({ preventDefault() {} });
  await settle();
  return { get, requests, settle };
}

test("quota button posts the selected auth index to the native read-only endpoint", async () => {
  const { get, requests, settle } = await mountQuotaUi(
    [
      { name: "opencode-go-1.json", auth_index: "idx-1" },
      { name: "opencode-go-2.json", auth_index: "idx-2" }
    ],
    { status: 200, body: QUOTA_OK_BODY }
  );

  const buttons = get("files-body").querySelectorAll("button[data-action='quota']");
  assert.equal(buttons.length, 2);
  assert.equal(buttons[0].disabled, false);

  buttons[0].handlers.click({ currentTarget: buttons[0] });
  await settle();

  // List/settings GETs plus one quota POST: no polling or automatic retry.
  assert.equal(requests.length, 3);
  const quotaRequest = requests[2];
  assert.equal(quotaRequest.url, "/v0/management/plugins/cliproxyapi-opencode-provider/quota");
  assert.equal(quotaRequest.init.method, "POST");
  assert.deepEqual(JSON.parse(quotaRequest.init.body), { auth_index: "idx-1" });
  assert.equal(quotaRequest.init.headers.Authorization, "Bearer fake-management-key");
  assert.equal(quotaRequest.init.credentials, "omit");

  // Three windows rendered with derived percentages and formatted reset times.
  assert.equal(get("quota-panel").hidden, false);
  const rows = get("quota-body").children;
  assert.equal(rows.length, 3);
  assert.equal(rows[0].children[0].textContent, "滚动窗口");
  assert.equal(rows[0].children[1].textContent, "75%");
  assert.equal(rows[0].children[2].textContent, "25%");
  assert.equal(rows[0].children[4].textContent, "2026-10-07 00:00 UTC");
  assert.equal(rows[2].children[1].textContent, "0%");
  assert.equal(rows[2].children[2].textContent, "100%");
  const progress = rows[0].children[3].children[0];
  assert.equal(progress.tagName, "PROGRESS");
  assert.equal(progress.max, 1);
  assert.equal(progress.value, 0.75);
  assert.match(get("quota-selection").textContent, /opencode-go-1\.json/);
  assert.match(get("quota-selection").textContent, /OpenCode Go/);
  assert.match(get("quota-status").textContent, /已加载/);

  // A later list refresh clears the stale selection panel.
  get("reload-btn").handlers.click({});
  await settle();
  assert.equal(get("quota-panel").hidden, true);
  assert.match(get("quota-status").textContent, /尚未选择/);
  assert.equal(requests.length, 5);
});

test("malformed quota response shows a fixed error and never a guessed value", async () => {
  const broken = JSON.parse(JSON.stringify(QUOTA_OK_BODY));
  broken.groups[0].buckets[0].remainingFraction = "<img src=x onerror=alert(1)>";
  const { get, requests, settle } = await mountQuotaUi(
    [{ name: "opencode-go-1.json", auth_index: "idx-1" }],
    { status: 200, body: broken }
  );
  const button = get("files-body").querySelectorAll("button[data-action='quota']")[0];
  button.handlers.click({ currentTarget: button });
  await settle();

  assert.equal(get("quota-panel").hidden, true);
  assert.match(get("quota-status").textContent, /配额数据不可用/);
  assert.equal(get("quota-status").textContent.includes("img"), false);
  assert.equal(get("quota-status").textContent.includes("alert"), false);
  assert.equal(requests.length, 3);
});

test("quota auth failure disconnects and clears the key and old quota", async () => {
  for (const status of [401, 403]) {
    const { get, requests, settle } = await mountQuotaUi(
      [{ name: "opencode-go-1.json", auth_index: "idx-1" }],
      { status, body: { error: "leaked-secret" } }
    );
    const button = get("files-body").querySelectorAll("button[data-action='quota']")[0];
    button.handlers.click({ currentTarget: button });
    await settle();

    assert.equal(get("mgmt-key").value, "");
    assert.equal(get("quota-panel").hidden, true);
    assert.equal(get("quota-status").textContent.includes(String(status)), true);
    assert.equal(get("quota-status").textContent.includes("leaked-secret"), false);
    assert.equal(requests.length, 3);
  }
});

test("a non-auth quota error uses a fixed message and does not echo the body", async () => {
  const { get, settle } = await mountQuotaUi(
    [{ name: "opencode-go-1.json", auth_index: "idx-1" }],
    { status: 502, body: { error: "<b>upstream secret</b>" } }
  );
  const button = get("files-body").querySelectorAll("button[data-action='quota']")[0];
  button.handlers.click({ currentTarget: button });
  await settle();

  assert.equal(get("quota-panel").hidden, true);
  assert.match(get("quota-status").textContent, /服务端错误/);
  assert.equal(get("quota-status").textContent.includes("upstream"), false);
});

test("a row without a safe auth index cannot query quota", async () => {
  const { get, requests } = await mountQuotaUi(
    [
      { name: "opencode-go-1.json" },
      { name: "opencode-go-2.json", auth_index: "bad index" }
    ],
    { status: 200, body: QUOTA_OK_BODY }
  );
  const buttons = get("files-body").querySelectorAll("button[data-action='quota']");
  assert.equal(buttons.length, 2);
  assert.equal(buttons[0].disabled, true);
  assert.equal(buttons[1].disabled, true);
  assert.equal(requests.length, 2, "only list/settings; no quota request without a safe index");
});

test("plaintext HTTP off loopback keeps quota disabled and sends nothing", async () => {
  const { get, requests } = await mountQuotaUi(
    [{ name: "opencode-go-1.json", auth_index: "idx-1" }],
    { status: 200, body: QUOTA_OK_BODY },
    { protocol: "http:", hostname: "cpa.example.com", pathname: "/v0/resource/plugins/cliproxyapi-opencode-provider/ui" }
  );
  // The gate blocks the initial list refresh, so no rows and no requests exist.
  assert.equal(requests.length, 0);
  assert.equal(get("reload-btn").disabled, true);
  assert.equal(get("insecure-warning").hidden, false);
});

test("off-loopback plaintext keeps the key and label inputs disabled", async () => {
  // Busy-state locking must never re-enable the secret inputs under the
  // insecure transport gate.
  const { get } = await mountQuotaUi(
    [],
    { status: 200, body: { files: [] } },
    { protocol: "http:", hostname: "cpa.example.com", pathname: "/v0/resource/plugins/cliproxyapi-opencode-provider/ui" }
  );
  assert.equal(get("keys").disabled, true);
  assert.equal(get("label").disabled, true);
  assert.equal(get("mgmt-key").disabled, true);
  assert.equal(get("import-btn").disabled, true);
});

/* --------------------------------------------------------------- theme bridge */

test("resolveAppliedTheme mirrors the host theme and falls back to the system scheme", () => {
  assert.equal(ui.resolveAppliedTheme("dark", false), "dark");
  assert.equal(ui.resolveAppliedTheme("white", true), "white");
  assert.equal(ui.resolveAppliedTheme("light", true), "light");
  // No host signal: follow the system, where the host's auto theme maps light
  // to the pure-white theme.
  assert.equal(ui.resolveAppliedTheme(null, true), "dark");
  assert.equal(ui.resolveAppliedTheme(null, false), "white");
  assert.equal(ui.resolveAppliedTheme(undefined, false), "white");
  // An unexpected host value is never trusted as a theme name.
  assert.equal(ui.resolveAppliedTheme("solarized", false), "white");
});

test("readParentTheme trusts only a same-origin parent and defaults to light", () => {
  const withAttr = (attr) => ({ document: { documentElement: { getAttribute: () => attr } } });
  assert.equal(ui.readParentTheme(null), null);
  assert.equal(ui.readParentTheme(undefined), null);
  // No data-theme attribute is the host's warm-grey light default.
  assert.equal(ui.readParentTheme(withAttr(null)), "light");
  assert.equal(ui.readParentTheme(withAttr("light")), "light");
  assert.equal(ui.readParentTheme(withAttr("white")), "white");
  assert.equal(ui.readParentTheme(withAttr("dark")), "dark");
  // A cross-origin parent throws on access and must fall back, never propagate.
  const crossOrigin = { get document() { throw new Error("blocked by the same-origin policy"); } };
  assert.equal(ui.readParentTheme(crossOrigin), null);
  // A detached/empty window is not mistaken for a light host.
  assert.equal(ui.readParentTheme({}), null);
});

test("collectThemeTokens copies only whitelisted, non-empty tokens", () => {
  const provided = {};
  for (const name of ui.THEME_TOKENS) {
    provided[name] = "  " + name + "-value  ";
  }
  provided["--bg-primary"] = "   "; // blank value is dropped
  provided["--leaked-secret"] = "do-not-copy"; // not whitelisted
  const tokens = ui.collectThemeTokens((name) => provided[name]);
  assert.equal(tokens["--bg-secondary"], "--bg-secondary-value");
  assert.equal(Object.prototype.hasOwnProperty.call(tokens, "--bg-primary"), false);
  assert.equal(Object.prototype.hasOwnProperty.call(tokens, "--leaked-secret"), false);
  assert.deepEqual(ui.collectThemeTokens(null), {});
});

test("THEME_TOKENS is a bounded, non-secret custom-property whitelist", () => {
  assert.ok(ui.THEME_TOKENS.length > 0);
  assert.equal(new Set(ui.THEME_TOKENS).size, ui.THEME_TOKENS.length);
  for (const name of ui.THEME_TOKENS) {
    assert.match(name, /^--[a-z-]+$/, name + " must be a plain custom property");
    assert.equal(/secret|password|credential|authorization|bearer/i.test(name), false, name + " must not cover secret state");
  }
});

// Boot the real ui.js with a scripted host window so the theme bridge runs.
function bootThemeUi(options) {
  const opts = options || {};
  const root = {
    attributes: {},
    styleProps: {},
    getAttribute(name) {
      return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null;
    },
    setAttribute(name, value) {
      this.attributes[name] = String(value);
    },
    style: {
      _props: {},
      setProperty(name, value) {
        this._props[name] = value;
      },
      removeProperty(name) {
        delete this._props[name];
      }
    }
  };
  const parentWindow = opts.crossOrigin
    ? { get document() { throw new Error("cross-origin"); } }
    : { document: { documentElement: { getAttribute: () => (opts.parentTheme === undefined ? null : opts.parentTheme) } } };
  const elements = new Map();
  const getElement = (id) => {
    if (!elements.has(id)) {
      elements.set(id, {
        value: "", disabled: false, hidden: false, textContent: "", className: "",
        handlers: {},
        addEventListener(name, handler) { this.handlers[name] = handler; },
        querySelectorAll() { return []; }
      });
    }
    return elements.get(id);
  };
  const hostTokens = opts.hostTokens || {};
  vm.runInNewContext(readAsset("ui.js"), {
    document: {
      readyState: "complete",
      documentElement: root,
      getElementById: getElement,
      addEventListener() {}
    },
    window: {
      location: {
        protocol: "http:", hostname: "127.0.0.1",
        pathname: "/v0/resource/plugins/cliproxyapi-opencode-provider/ui"
      },
      parent: opts.hostPresent === false ? undefined : parentWindow,
      matchMedia: () => ({ matches: Boolean(opts.systemDark), addEventListener() {} }),
      getComputedStyle: () => ({
        getPropertyValue: (name) => (Object.prototype.hasOwnProperty.call(hostTokens, name) ? hostTokens[name] : "")
      })
    },
    TextEncoder
  });
  return root;
}

test("initTheme mirrors the host data-theme and copies whitelisted computed tokens", () => {
  const root = bootThemeUi({
    parentTheme: "dark",
    hostTokens: { "--bg-secondary": "#151412", "--text-primary": "#f6f4f1", "--leaked-secret": "#000" }
  });
  assert.equal(root.getAttribute("data-theme"), "dark");
  assert.equal(root.style._props["--bg-secondary"], "#151412");
  assert.equal(root.style._props["--text-primary"], "#f6f4f1");
  // Only whitelisted tokens are copied from the host.
  assert.equal(Object.prototype.hasOwnProperty.call(root.style._props, "--leaked-secret"), false);
});

test("initTheme keeps the host light default even when the system prefers dark", () => {
  const root = bootThemeUi({ parentTheme: null, systemDark: true });
  assert.equal(root.getAttribute("data-theme"), "light");
});

test("initTheme falls back to the system scheme for a cross-origin or absent parent", () => {
  assert.equal(bootThemeUi({ crossOrigin: true, systemDark: true }).getAttribute("data-theme"), "dark");
  assert.equal(bootThemeUi({ crossOrigin: true, systemDark: false }).getAttribute("data-theme"), "white");
  assert.equal(bootThemeUi({ hostPresent: false, systemDark: true }).getAttribute("data-theme"), "dark");
  assert.equal(bootThemeUi({ hostPresent: false, systemDark: false }).getAttribute("data-theme"), "white");
});
// Native and namespaced IDs share validation and preserve explicit protocol choices.
test("manual models normalize namespace, auto protocol and explicit overrides", () => {
  assert.deepEqual(ui.normalizeManualModels([
    { id: " opencode-go/glm-5.2 " }, { id: "novel", protocol: "claude" }
  ]), [{ id: "glm-5.2" }, { id: "novel", protocol: "claude" }]);
  assert.deepEqual(ui.normalizeManualModels([]), []);
});

test("manual models reject duplicate ids, unknown auto protocols and invalid input", () => {
  for (const models of [
    null, [{ id: "glm x" }], [{ id: "novel" }], [{ id: "glm\u0085x" }],
    [{ id: "glm-x" }, { id: "opencode-go/glm-x" }],
    [{ id: "opencode-go/opencode-go/glm-x" }],
    [{ id: "novel", protocol: "bad" }], [{ id: "g".repeat(201), protocol: "openai" }],
    Array.from({ length: 101 }, (_, i) => ({ id: "glm-" + i }))
  ]) assert.throws(() => ui.normalizeManualModels(models));
});

test("manual model management endpoints preserve the proxy prefix", () => {
  const urls = ui.deriveEndpoints({ pathname: "/proxy/v0/resource/plugins/" + ui.PLUGIN_ID + "/ui" });
  for (const field of ["settings", "validate", "config"]) {
    assert.equal(urls[field + "Url"], "/proxy/v0/management/plugins/" + ui.PLUGIN_ID + "/" + field);
  }
});
