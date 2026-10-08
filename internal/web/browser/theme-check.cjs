"use strict";

/*
 * Browser visual/behaviour checks for the plugin UI theme bridge.
 *
 * Run manually (Playwright is intentionally NOT a repo dependency):
 *
 *   npm i playwright            # outside the repo, e.g. in a temp dir
 *   NODE_PATH="$(npm root)" node internal/web/browser/theme-check.cjs
 *
 * It serves internal/web over a throwaway loopback static server (no production
 * server, no CPA, no network egress) and drives a headless browser to validate:
 *   - embedded light / white / dark themes match the host tokens
 *   - a live host theme mutation is mirrored into the iframe
 *   - whitelisted host tokens are copied into the iframe
 *   - standalone dark/light follows the system preference
 *   - a 390px viewport renders without horizontal breakage
 *
 * Screenshots are written to $SCREENSHOT_DIR (default: OS temp dir), never into
 * the repository.
 */

const http = require("node:http");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");

let chromium;
try {
  ({ chromium } = require("playwright"));
} catch (err) {
  console.error("playwright is required for the browser checks:");
  console.error('  npm i playwright && NODE_PATH="$(npm root)" node internal/web/browser/theme-check.cjs');
  process.exit(2);
}

const WEB_ROOT = path.resolve(__dirname, "..");
const SCREENSHOT_DIR = process.env.SCREENSHOT_DIR || path.join(os.tmpdir(), "opencode-provider-theme-shots");
const CSP =
  "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
  "frame-ancestors 'self'; base-uri 'none'; form-action 'none'";
const CONTENT_TYPES = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8"
};

// Expected computed body colors per theme (from the shared token contract).
const EXPECTED = {
  light: { bg: "rgb(250, 249, 245)", attr: "light" },
  white: { bg: "rgb(255, 255, 255)", attr: "white" },
  dark: { bg: "rgb(21, 20, 18)", attr: "dark" }
};

function startServer() {
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, "http://127.0.0.1");
    let pathname = decodeURIComponent(url.pathname);
    if (pathname === "/") {
      pathname = "/ui.html";
    }
    const file = path.join(WEB_ROOT, pathname);
    if (file !== WEB_ROOT && !file.startsWith(WEB_ROOT + path.sep)) {
      res.writeHead(403);
      res.end("forbidden");
      return;
    }
    fs.readFile(file, (err, body) => {
      if (err) {
        res.writeHead(404);
        res.end("not found");
        return;
      }
      res.writeHead(200, {
        "Content-Type": CONTENT_TYPES[path.extname(file)] || "application/octet-stream",
        "Content-Security-Policy": CSP
      });
      res.end(body);
    });
  });
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => resolve({ server, port: server.address().port }));
  });
}

async function launchBrowser() {
  if (process.env.CHROME_PATH) {
    return chromium.launch({ executablePath: process.env.CHROME_PATH });
  }
  try {
    return await chromium.launch({ channel: "chrome" });
  } catch (err) {
    return chromium.launch();
  }
}

const results = [];
function check(name, condition, detail) {
  results.push({ name, ok: Boolean(condition), detail: detail || "" });
  if (!condition) {
    console.error("FAIL " + name + (detail ? " :: " + detail : ""));
  } else {
    console.log("ok   " + name);
  }
}

async function readFrameTheme(frame) {
  return frame.evaluate(() => ({
    attr: document.documentElement.getAttribute("data-theme"),
    bg: getComputedStyle(document.body).backgroundColor,
    text: getComputedStyle(document.body).color
  }));
}

async function embeddedFrame(page) {
  const handle = await page.$("#ui-frame");
  return handle.contentFrame();
}

async function setHostTheme(page, theme) {
  await page.evaluate((value) => {
    if (value === null) {
      document.documentElement.removeAttribute("data-theme");
    } else {
      document.documentElement.setAttribute("data-theme", value);
    }
  }, theme);
}

async function waitForFrameBg(frame, expected) {
  await frame.waitForFunction(
    (want) => getComputedStyle(document.body).backgroundColor === want,
    expected,
    { timeout: 5000 }
  );
}

async function main() {
  fs.mkdirSync(SCREENSHOT_DIR, { recursive: true });
  const { server, port } = await startServer();
  const base = "http://127.0.0.1:" + port;
  const browser = await launchBrowser();
  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const page = await context.newPage();
    await page.goto(base + "/browser/fixture.html");
    const frame = await embeddedFrame(page);

    // 1. Embedded warm-grey light (host has no data-theme attribute).
    await setHostTheme(page, null);
    await waitForFrameBg(frame, EXPECTED.light.bg);
    let state = await readFrameTheme(frame);
    check("embedded light theme mirrors the host default", state.attr === "light" && state.bg === EXPECTED.light.bg,
      JSON.stringify(state));
    await (await page.$("#ui-frame")).screenshot({ path: path.join(SCREENSHOT_DIR, "embedded-light.png") });

    // 2. Embedded white.
    await setHostTheme(page, "white");
    await waitForFrameBg(frame, EXPECTED.white.bg);
    state = await readFrameTheme(frame);
    check("embedded white theme mirrors the host", state.attr === "white" && state.bg === EXPECTED.white.bg,
      JSON.stringify(state));
    await (await page.$("#ui-frame")).screenshot({ path: path.join(SCREENSHOT_DIR, "embedded-white.png") });

    // 3. Embedded dark.
    await setHostTheme(page, "dark");
    await waitForFrameBg(frame, EXPECTED.dark.bg);
    state = await readFrameTheme(frame);
    check("embedded dark theme mirrors the host", state.attr === "dark" && state.bg === EXPECTED.dark.bg,
      JSON.stringify(state));
    await (await page.$("#ui-frame")).screenshot({ path: path.join(SCREENSHOT_DIR, "embedded-dark.png") });

    // 4. Live mutation light -> dark is mirrored without reloading the iframe.
    await setHostTheme(page, null);
    await waitForFrameBg(frame, EXPECTED.light.bg);
    await setHostTheme(page, "dark");
    await waitForFrameBg(frame, EXPECTED.dark.bg);
    check("a live host theme mutation updates the iframe", true);

    // 5. Whitelisted host tokens are copied into the iframe (sentinel value that
    //    is not part of the built-in theme proves the copy, not our own CSS).
    await page.evaluate(() => {
      document.documentElement.style.setProperty("--bg-secondary", "#123456");
      document.documentElement.setAttribute("data-theme", "white");
    });
    await waitForFrameBg(frame, "rgb(18, 52, 86)");
    state = await readFrameTheme(frame);
    check("whitelisted host tokens are copied into the iframe", state.bg === "rgb(18, 52, 86)", JSON.stringify(state));

    // 6. 390px viewport renders the embedded UI without horizontal page overflow.
    await setHostTheme(page, null);
    const mobileContext = await browser.newContext({ viewport: { width: 390, height: 844 } });
    const mobilePage = await mobileContext.newPage();
    await mobilePage.goto(base + "/browser/fixture.html");
    const mobileFrame = await embeddedFrame(mobilePage);
    await waitForFrameBg(mobileFrame, EXPECTED.light.bg);
    await setHostTheme(mobilePage, "dark");
    await waitForFrameBg(mobileFrame, EXPECTED.dark.bg);
    const overflow = await mobileFrame.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth
    }));
    check("390px viewport has no page-level horizontal overflow", overflow.scrollWidth <= overflow.clientWidth + 1,
      JSON.stringify(overflow));
    await (await mobilePage.$("#ui-frame")).screenshot({ path: path.join(SCREENSHOT_DIR, "embedded-390-dark.png") });
    await mobileContext.close();

    // 7. Standalone (no embedding parent) follows the system preference.
    const standaloneDark = await browser.newContext({ colorScheme: "dark", viewport: { width: 1280, height: 900 } });
    const standaloneDarkPage = await standaloneDark.newPage();
    await standaloneDarkPage.goto(base + "/ui.html");
    await standaloneDarkPage.waitForFunction(
      (want) => getComputedStyle(document.body).backgroundColor === want,
      EXPECTED.dark.bg,
      { timeout: 5000 }
    );
    const darkState = await standaloneDarkPage.evaluate(() => ({
      attr: document.documentElement.getAttribute("data-theme"),
      bg: getComputedStyle(document.body).backgroundColor
    }));
    check("standalone dark follows the system preference",
      darkState.attr === "dark" && darkState.bg === EXPECTED.dark.bg, JSON.stringify(darkState));
    await standaloneDarkPage.screenshot({ path: path.join(SCREENSHOT_DIR, "standalone-system-dark.png") });
    await standaloneDark.close();

    const standaloneLight = await browser.newContext({ colorScheme: "light", viewport: { width: 1280, height: 900 } });
    const standaloneLightPage = await standaloneLight.newPage();
    await standaloneLightPage.goto(base + "/ui.html");
    await standaloneLightPage.waitForFunction(
      (want) => getComputedStyle(document.body).backgroundColor === want,
      EXPECTED.white.bg,
      { timeout: 5000 }
    );
    const lightState = await standaloneLightPage.evaluate(() => ({
      attr: document.documentElement.getAttribute("data-theme"),
      bg: getComputedStyle(document.body).backgroundColor
    }));
    check("standalone light follows the system preference (white theme)",
      lightState.attr === "white" && lightState.bg === EXPECTED.white.bg, JSON.stringify(lightState));
    await standaloneLightPage.screenshot({ path: path.join(SCREENSHOT_DIR, "standalone-system-light.png") });
    await standaloneLight.close();

    await context.close();
  } finally {
    await browser.close();
    server.close();
  }

  const failed = results.filter((r) => !r.ok);
  console.log("\nscreenshots: " + SCREENSHOT_DIR);
  console.log(results.length - failed.length + "/" + results.length + " browser checks passed");
  if (failed.length) {
    process.exitCode = 1;
  }
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
