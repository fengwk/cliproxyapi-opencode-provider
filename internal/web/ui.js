"use strict";

/*
 * OpenCode Go key management UI for the cliproxyapi-opencode-provider CPA plugin.
 *
 * Security invariants:
 * - The CPA management key lives only in the password input and in request headers.
 * - Imported OpenCode Go keys live only in the textarea and in the in-flight request body.
 * - No secret is written to browser storage, Cookies, URLs, logs, tooltips, titles or toasts.
 * - All endpoints are derived relative to the current location so an upstream proxy prefix works.
 */

var PLUGIN_ID = "cliproxyapi-opencode-provider";
var MAX_KEYS = 100;
var MAX_KEY_LENGTH = 4096;
var MAX_BODY_BYTES = 64 * 1024;
var MAX_LABEL_LENGTH = 128;
var MAX_CELL_LENGTH = 200;
var MAX_FILE_NAME_LENGTH = 255;

var CONTROL_CHARS_RE = /[\u0000-\u001f\u007f]/;
var CONTROL_CHARS_RE_GLOBAL = /[\u0000-\u001f\u007f]/g;
var WHITESPACE_RE = /\s/;

/* ---------------------------------------------------------------- pure helpers */

function isPlainObject(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

// UTF-8 byte length, used to enforce the 64 KiB request-body budget.
function utf8ByteLength(text) {
  var value = String(text);
  if (typeof TextEncoder === "function") {
    return new TextEncoder().encode(value).length;
  }
  var bytes = 0;
  for (var i = 0; i < value.length; i++) {
    var code = value.codePointAt(i);
    if (code > 0xffff) {
      i++;
    }
    if (code <= 0x7f) {
      bytes += 1;
    } else if (code <= 0x7ff) {
      bytes += 2;
    } else if (code <= 0xffff) {
      bytes += 3;
    } else {
      bytes += 4;
    }
  }
  return bytes;
}

// Derive management endpoints from the page location, keeping any proxy prefix.
function deriveEndpoints(locationLike) {
  var pathname = locationLike && typeof locationLike.pathname === "string" ? locationLike.pathname : "";
  var marker = "/v0/resource/plugins/" + PLUGIN_ID + "/";
  var index = pathname.indexOf(marker);
  var prefix = index >= 0 ? pathname.slice(0, index) : "";
  return {
    prefix: prefix,
    keysUrl: prefix + "/v0/management/plugins/" + PLUGIN_ID + "/keys",
    credentialsUrl: prefix + "/v8/management/credentials"
  };
}

// Plaintext HTTP is only tolerated on loopback; everything else needs HTTPS.
function isSecureUiContext(locationLike) {
  var protocol = locationLike && typeof locationLike.protocol === "string" ? locationLike.protocol : "";
  var hostname = String((locationLike && locationLike.hostname) || "").toLowerCase();
  if (protocol === "https:") {
    return true;
  }
  if (protocol !== "http:") {
    return false;
  }
  return hostname === "localhost" || hostname === "127.0.0.1" || hostname === "[::1]" || hostname === "::1";
}

// Parse the textarea: one key per line, trimmed, deduplicated, validated.
function parseKeys(raw) {
  var text = typeof raw === "string" ? raw : "";
  var lines = text.split(/\r\n|\r|\n/);
  var seen = new Set();
  var keys = [];
  var invalidLines = [];
  var empty = 0;
  var duplicates = 0;

  for (var i = 0; i < lines.length; i++) {
    var value = lines[i].trim();
    if (!value) {
      empty++;
      continue;
    }
    if (CONTROL_CHARS_RE.test(value)) {
      invalidLines.push({ line: i + 1, reason: "control" });
      continue;
    }
    if (WHITESPACE_RE.test(value)) {
      invalidLines.push({ line: i + 1, reason: "whitespace" });
      continue;
    }
    if (value.length > MAX_KEY_LENGTH) {
      invalidLines.push({ line: i + 1, reason: "too-long" });
      continue;
    }
    if (seen.has(value)) {
      duplicates++;
      continue;
    }
    seen.add(value);
    keys.push(value);
  }

  var counts = {
    lines: lines.length,
    empty: empty,
    duplicates: duplicates,
    invalid: invalidLines.length,
    valid: keys.length
  };

  if (keys.length > MAX_KEYS) {
    return {
      keys: [],
      counts: counts,
      invalidLines: invalidLines,
      error: { code: "too-many", message: "有效密钥超过 " + MAX_KEYS + " 条上限，请分批导入。" }
    };
  }

  return { keys: keys, counts: counts, invalidLines: invalidLines, error: null };
}

function validateLabel(label) {
  var value = typeof label === "string" ? label.trim() : "";
  if (!value) {
    return { ok: true, value: "", reason: "" };
  }
  if (CONTROL_CHARS_RE.test(value)) {
    return { ok: false, value: "", reason: "control" };
  }
  if (value.length > MAX_LABEL_LENGTH) {
    return { ok: false, value: "", reason: "too-long" };
  }
  return { ok: true, value: value, reason: "" };
}

// Build the exact JSON body that will be POSTed, and check the byte budget.
function buildImportPayload(keys, label) {
  var payload = { keys: Array.isArray(keys) ? keys.slice() : [] };
  if (typeof label === "string" && label) {
    payload.label = label;
  }
  var body = JSON.stringify(payload);
  var bytes = utf8ByteLength(body);
  return { body: body, bytes: bytes, ok: bytes <= MAX_BODY_BYTES };
}

function toCellText(value) {
  var text = "";
  if (typeof value === "string") {
    text = value;
  } else if (typeof value === "number" && isFinite(value)) {
    text = String(value);
  } else if (typeof value === "boolean") {
    text = value ? "true" : "false";
  } else {
    return "";
  }
  text = text.replace(CONTROL_CHARS_RE_GLOBAL, "").trim();
  if (text.length > MAX_CELL_LENGTH) {
    text = text.slice(0, MAX_CELL_LENGTH) + "…";
  }
  return text;
}

function toCount(value) {
  var number = typeof value === "number" ? value : Number(value);
  if (!isFinite(number) || number <= 0) {
    return 0;
  }
  return Math.trunc(number);
}

// Only a plain file name (no path separators, no control characters) may be deleted.
function isSafeFileName(name) {
  if (typeof name !== "string") {
    return false;
  }
  var value = name.trim();
  if (!value || value.length > MAX_FILE_NAME_LENGTH || value === "." || value === "..") {
    return false;
  }
  if (CONTROL_CHARS_RE.test(value)) {
    return false;
  }
  return value.indexOf("/") === -1 && value.indexOf("\\") === -1;
}

// Convert non-secret list entries into plain display rows; never carries secrets.
function normalizeFileEntries(files) {
  if (!Array.isArray(files)) {
    return [];
  }
  var rows = [];
  for (var i = 0; i < files.length; i++) {
    var entry = files[i];
    if (!isPlainObject(entry)) {
      continue;
    }
    var rawName = typeof entry.name === "string" ? entry.name : "";
    rows.push({
      rawName: rawName,
      name: toCellText(rawName),
      label: toCellText(entry.label),
      status: toCellText(entry.status),
      disabled: entry.disabled === true,
      unavailable: entry.unavailable === true,
      success: toCount(entry.success),
      failed: toCount(entry.failed),
      deletable: isSafeFileName(rawName)
    });
  }
  return rows;
}

// Local, fixed messages only: untrusted server strings are never surfaced.
function describeHttpFailure(status) {
  if (status === 400) {
    return "请求无效（400），请检查输入内容后重试。";
  }
  if (status === 401) {
    return "管理密钥无效或缺失（401）。已断开连接，请重新输入密钥。";
  }
  if (status === 403) {
    return "未授权（403）。可能未启用 CPA 远程管理、密钥错误，或当前 IP 因多次失败被临时封禁。";
  }
  if (status === 404) {
    return "管理接口不存在（404），请确认 CPA 与插件版本匹配。";
  }
  if (status === 405) {
    return "请求方法不被支持（405），请确认 CPA 与插件版本匹配。";
  }
  if (status === 413) {
    return "请求体过大（413），请减少密钥数量后重试。";
  }
  if (status >= 500) {
    return "CPA 服务端错误（" + status + "），请稍后重试。";
  }
  return "请求失败（" + status + "）。";
}

function countNotice(counts) {
  var parts = [];
  if (counts.duplicates > 0) {
    parts.push("忽略重复 " + counts.duplicates + " 条");
  }
  if (counts.invalid > 0) {
    parts.push("跳过无效 " + counts.invalid + " 行");
  }
  if (counts.empty > 0) {
    parts.push("忽略空行 " + counts.empty + " 行");
  }
  return parts.length ? "（" + parts.join("，") + "）" : "";
}

function formatImportResult(status, payload) {
  var data = isPlainObject(payload) ? payload : {};
  var imported = toCount(data.imported);
  var skipped = toCount(data.skipped);
  var failed = toCount(data.failed);
  var ok = status >= 200 && status < 300;
  var summary;
  var level;
  if (!ok) {
    summary = describeHttpFailure(status);
    level = "error";
  } else if (status === 207 || failed > 0) {
    summary = "部分导入完成：成功 " + imported + "，跳过 " + skipped + "，失败 " + failed + "。";
    level = imported > 0 ? "warn" : "error";
  } else {
    summary = "导入成功：成功 " + imported + "，跳过 " + skipped + "，失败 " + failed + "。";
    level = "ok";
  }
  return { ok: ok, level: level, summary: summary, imported: imported, skipped: skipped, failed: failed };
}

// Compose the management request. The key is only ever placed in the Authorization header.
function buildManagementRequest(key, options) {
  var opts = isPlainObject(options) ? options : {};
  var headers = { Accept: "application/json" };
  if (typeof key === "string" && key.length > 0) {
    headers.Authorization = "Bearer " + key;
  }
  var init = {
    method: typeof opts.method === "string" ? opts.method : "GET",
    credentials: "omit",
    cache: "no-store",
    headers: headers
  };
  if (typeof opts.body === "string") {
    init.body = opts.body;
    headers["Content-Type"] = "application/json";
  }
  return init;
}

var api = {
  PLUGIN_ID: PLUGIN_ID,
  MAX_KEYS: MAX_KEYS,
  MAX_KEY_LENGTH: MAX_KEY_LENGTH,
  MAX_BODY_BYTES: MAX_BODY_BYTES,
  MAX_LABEL_LENGTH: MAX_LABEL_LENGTH,
  utf8ByteLength: utf8ByteLength,
  deriveEndpoints: deriveEndpoints,
  isSecureUiContext: isSecureUiContext,
  parseKeys: parseKeys,
  validateLabel: validateLabel,
  buildImportPayload: buildImportPayload,
  toCellText: toCellText,
  toCount: toCount,
  isSafeFileName: isSafeFileName,
  normalizeFileEntries: normalizeFileEntries,
  describeHttpFailure: describeHttpFailure,
  countNotice: countNotice,
  formatImportResult: formatImportResult,
  buildManagementRequest: buildManagementRequest
};

if (typeof module !== "undefined" && module.exports) {
  module.exports = api;
}

/* ---------------------------------------------------------------- DOM wiring */

function initUi() {
  var doc = document;
  var el = {
    mgmtKey: doc.getElementById("mgmt-key"),
    authForm: doc.getElementById("auth-form"),
    refreshBtn: doc.getElementById("refresh-btn"),
    importForm: doc.getElementById("import-form"),
    keys: doc.getElementById("keys"),
    label: doc.getElementById("label"),
    importBtn: doc.getElementById("import-btn"),
    importProgress: doc.getElementById("import-progress"),
    reloadBtn: doc.getElementById("reload-btn"),
    summary: doc.getElementById("summary"),
    status: doc.getElementById("status"),
    tableWrap: doc.getElementById("table-wrap"),
    filesBody: doc.getElementById("files-body"),
    insecureWarning: doc.getElementById("insecure-warning")
  };

  if (!el.mgmtKey || !el.keys || !el.filesBody || !el.status) {
    return;
  }

  var endpoints = deriveEndpoints(window.location);
  var secureContext = false;
  var busy = false;

  function setStatus(message, level) {
    el.status.textContent = message;
    el.status.className = "status" + (level ? " " + level : "");
  }

  function setBusy(flag) {
    busy = flag;
    var disabled = flag || !secureContext;
    el.refreshBtn.disabled = disabled;
    el.importBtn.disabled = disabled;
    el.reloadBtn.disabled = disabled;
    el.importProgress.hidden = !flag;
    var deleteButtons = el.filesBody.querySelectorAll("button[data-action='delete']");
    for (var i = 0; i < deleteButtons.length; i++) {
      deleteButtons[i].disabled = disabled;
    }
  }

  function applySecurityGate() {
    secureContext = isSecureUiContext(window.location);
    el.insecureWarning.hidden = secureContext;
    el.mgmtKey.disabled = !secureContext;
    el.keys.disabled = !secureContext;
    el.label.disabled = !secureContext;
    setBusy(false);
    if (secureContext) {
      setStatus("尚未加载。请输入 CPA 管理密钥并点击“连接并刷新”。", "info");
    } else {
      setStatus("当前环境不安全，已禁用密钥操作。请改用 HTTPS 或本机地址访问。", "error");
    }
  }

  function disconnect() {
    el.mgmtKey.value = "";
  }

  // Read the management key only when a request is issued; it is never cached.
  function currentKey() {
    return typeof el.mgmtKey.value === "string" ? el.mgmtKey.value : "";
  }

  function appendCell(row, text, className) {
    var cell = doc.createElement("td");
    if (className) {
      cell.className = className;
    }
    cell.textContent = text;
    row.appendChild(cell);
    return cell;
  }

  function buildRow(row) {
    var tr = doc.createElement("tr");
    appendCell(tr, row.name, "cell-name");
    appendCell(tr, row.label || "—");
    appendCell(tr, row.status || "—");
    appendCell(tr, row.disabled ? "是" : "否");
    appendCell(tr, row.unavailable ? "不可用" : "可用", row.unavailable ? "cell-warn" : "cell-ok");
    appendCell(tr, String(row.success));
    appendCell(tr, String(row.failed));
    var actions = doc.createElement("td");
    if (row.deletable) {
      var button = doc.createElement("button");
      button.type = "button";
      button.className = "danger";
      button.setAttribute("data-action", "delete");
      button.setAttribute("data-file-name", row.rawName);
      button.textContent = "删除";
      button.setAttribute("aria-label", "删除密钥文件 " + toCellText(row.rawName));
      button.disabled = busy || !secureContext;
      button.addEventListener("click", onDeleteClick);
      actions.appendChild(button);
    } else {
      actions.textContent = "—";
    }
    tr.appendChild(actions);
    return tr;
  }

  function updateSummary(rows) {
    var total = rows.length;
    var unavailable = 0;
    var disabled = 0;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].unavailable) {
        unavailable++;
      }
      if (rows[i].disabled) {
        disabled++;
      }
    }
    el.summary.textContent =
      "共 " + total + " 条：可用 " + (total - unavailable) + "，不可用 " + unavailable + "，已禁用 " + disabled + "。";
  }

  function renderFiles(rows) {
    el.tableWrap.classList.toggle("is-empty", rows.length === 0);
    while (el.filesBody.firstChild) {
      el.filesBody.removeChild(el.filesBody.firstChild);
    }
    if (!rows.length) {
      var emptyRow = doc.createElement("tr");
      var emptyCell = doc.createElement("td");
      emptyCell.colSpan = 8;
      emptyCell.className = "empty-cell";
      emptyCell.textContent = "暂无数据。";
      emptyRow.appendChild(emptyCell);
      el.filesBody.appendChild(emptyRow);
    } else {
      for (var i = 0; i < rows.length; i++) {
        el.filesBody.appendChild(buildRow(rows[i]));
      }
    }
    updateSummary(rows);
  }

  function readJson(response) {
    return response.text().then(function (text) {
      if (!text) {
        return {};
      }
      try {
        return JSON.parse(text);
      } catch (err) {
        return {};
      }
    });
  }

  function refreshList() {
    if (busy || !secureContext) {
      return;
    }
    var managementKey = currentKey();
    if (!managementKey) {
      setStatus("请先输入 CPA 管理密钥。", "error");
      el.mgmtKey.focus();
      return;
    }

    setBusy(true);
    setStatus("正在加载密钥列表…", "info");

    fetch(endpoints.keysUrl, buildManagementRequest(managementKey, { method: "GET" }))
      .then(function (response) {
        if (response.status === 401 || response.status === 403) {
          disconnect();
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        if (!response.ok) {
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        return readJson(response);
      })
      .then(function (payload) {
        if (!payload) {
          return;
        }
        var rows = normalizeFileEntries(isPlainObject(payload) ? payload.files : []);
        renderFiles(rows);
        setStatus(rows.length ? "已加载 " + rows.length + " 条密钥。" : "列表为空，暂无已托管的密钥。", "ok");
      })
      .catch(function () {
        setStatus("无法连接 CPA 管理接口，请确认地址与网络。", "error");
      })
      .then(function () {
        // Drop the key reference held by this scope once the request has settled.
        managementKey = "";
        setBusy(false);
      });
  }

  function importKeys() {
    if (busy || !secureContext) {
      return;
    }
    var managementKey = currentKey();
    if (!managementKey) {
      setStatus("请先输入 CPA 管理密钥（管理认证）。", "error");
      el.mgmtKey.focus();
      return;
    }

    var labelCheck = validateLabel(el.label.value);
    if (!labelCheck.ok) {
      setStatus(
        labelCheck.reason === "too-long"
          ? "标签过长（上限 " + MAX_LABEL_LENGTH + " 个字符）。"
          : "标签包含非法控制字符。",
        "error"
      );
      return;
    }

    var parsed = parseKeys(el.keys.value);
    var notice = countNotice(parsed.counts);
    if (parsed.error) {
      setStatus(parsed.error.message, "error");
      return;
    }
    if (parsed.counts.valid === 0) {
      setStatus("没有可导入的有效密钥" + notice + "。", "error");
      return;
    }

    var payload = buildImportPayload(parsed.keys, labelCheck.value);
    // The submitted keys are no longer needed once the body is built.
    parsed.keys.length = 0;
    if (!payload.ok) {
      payload.body = "";
      setStatus("请求体过大，请减少密钥数量后重试。", "error");
      return;
    }

    setBusy(true);
    setStatus("正在导入 " + parsed.counts.valid + " 条密钥…" + notice, "info");

    fetch(endpoints.keysUrl, buildManagementRequest(managementKey, { method: "POST", body: payload.body }))
      .then(function (response) {
        if (response.status === 401 || response.status === 403) {
          disconnect();
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        if (!response.ok) {
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        return readJson(response).then(function (resultPayload) {
          var result = formatImportResult(response.status, resultPayload);
          // The import was processed, so the textarea content is cleared.
          el.keys.value = "";
          setStatus(result.summary + notice + " 请点击“刷新列表”查看最新状态。", result.level);
        });
      })
      .catch(function () {
        setStatus("无法连接 CPA 管理接口，请确认地址与网络；密钥未提交。", "error");
      })
      .then(function () {
        // Drop every reference this scope held to the management key and request body.
        managementKey = "";
        payload.body = "";
        setBusy(false);
      });
  }

  function onDeleteClick(event) {
    if (busy || !secureContext) {
      return;
    }
    var button = event.currentTarget;
    var fileName = button.getAttribute("data-file-name");
    if (!isSafeFileName(fileName)) {
      setStatus("文件名无效，无法删除。", "error");
      return;
    }
    var managementKey = currentKey();
    if (!managementKey) {
      setStatus("请先输入 CPA 管理密钥。", "error");
      el.mgmtKey.focus();
      return;
    }
    if (!window.confirm('确定删除密钥文件 "' + toCellText(fileName) + '" 吗？此操作不可撤销。')) {
      return;
    }

    setBusy(true);
    setStatus("正在删除…", "info");
    var url = endpoints.credentialsUrl + "?name=" + encodeURIComponent(fileName);

    fetch(url, buildManagementRequest(managementKey, { method: "DELETE" }))
      .then(function (response) {
        if (response.status === 401 || response.status === 403) {
          disconnect();
          setStatus(describeHttpFailure(response.status), "error");
          return;
        }
        if (!response.ok) {
          setStatus(describeHttpFailure(response.status), "error");
          return;
        }
        setStatus("已删除。列表可能已过期，请点击“刷新列表”。", "ok");
      })
      .catch(function () {
        setStatus("无法连接 CPA 管理接口，请确认地址与网络。", "error");
      })
      .then(function () {
        managementKey = "";
        setBusy(false);
      });
  }

  el.authForm.addEventListener("submit", function (event) {
    event.preventDefault();
    refreshList();
  });
  el.importForm.addEventListener("submit", function (event) {
    event.preventDefault();
    importKeys();
  });
  el.reloadBtn.addEventListener("click", function () {
    refreshList();
  });

  applySecurityGate();
  if (secureContext) {
    setStatus("尚未加载。请输入 CPA 管理密钥并点击“连接并刷新”。", "info");
  }
}

if (typeof document !== "undefined" && typeof window !== "undefined" && document.getElementById) {
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", initUi);
  } else {
    initUi();
  }
}
