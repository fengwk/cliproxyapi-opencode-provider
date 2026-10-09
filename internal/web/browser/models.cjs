'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('playwright');
const web = path.resolve(__dirname, '..');
const plugin = 'cliproxyapi-opencode-provider';

// The manual-model draft table derives its design from the step-5-preview-free
// row: text must be vertically centered on a compact row delete button, and the
// add button must share the input/select bottom edge.
const MANUAL_ID = 'step-5-preview-free';
const LONG_ID = 'opencode-go/' + 'wrapped-model-identifier-'.repeat(5) + 'end';

async function readManualGeometry(page) {
  return page.evaluate(() => {
    const round = (value) => Math.round(value * 100) / 100;
    const box = (el) => {
      const rect = el.getBoundingClientRect();
      return {
        top: round(rect.top),
        bottom: round(rect.bottom),
        height: round(rect.height),
        center: round(rect.top + rect.height / 2)
      };
    };
    const textCenter = (el) => {
      const range = document.createRange();
      range.selectNodeContents(el);
      const rect = range.getBoundingClientRect();
      return round(rect.top + rect.height / 2);
    };
    const rows = Array.from(document.querySelectorAll('#manual-model-body tr')).map((row) => {
      const cells = Array.from(row.children);
      const button = row.querySelector('button');
      return {
        id: cells[0] ? cells[0].textContent : '',
        cellHeight: cells[0] ? box(cells[0]).height : 0,
        idTextCenter: cells[0] ? textCenter(cells[0]) : 0,
        protocolTextCenter: cells[1] ? textCenter(cells[1]) : 0,
        button: button ? box(button) : null
      };
    });
    const input = document.querySelector('#manual-model-id');
    const select = document.querySelector('#manual-model-protocol');
    const add = document.querySelector('#manual-model-add');
    return {
      rows,
      input: box(input),
      select: box(select),
      addButton: box(add),
      inputFont: getComputedStyle(input).fontSize,
      selectFont: getComputedStyle(select).fontSize
    };
  });
}

// A row's id/protocol text must be centered on its own delete button, and the
// button must stay compact while keeping a tappable target of at least 24px.
function assertRowCentered(label, row) {
  assert.ok(row.button, label + ': expected a row delete button');
  assert.ok(Math.abs(row.idTextCenter - row.button.center) <= 1,
    label + ': id text center ' + row.idTextCenter + ' vs delete center ' + row.button.center);
  assert.ok(Math.abs(row.protocolTextCenter - row.button.center) <= 1,
    label + ': protocol text center ' + row.protocolTextCenter + ' vs delete center ' + row.button.center);
  assert.ok(row.button.height >= 24 && row.button.height <= 32,
    label + ': delete button height ' + row.button.height + ' outside the compact 24-32px range');
}

// The manual select must inherit the input font, and all three controls must
// share a bottom edge in the desktop row.
function assertControlAlignment(label, geometry) {
  assert.equal(geometry.selectFont, geometry.inputFont,
    label + ': select font-size ' + geometry.selectFont + ' must match input ' + geometry.inputFont);
  assert.ok(Math.abs(geometry.input.bottom - geometry.select.bottom) <= 2,
    label + ': input/select bottom edges differ by ' + Math.abs(geometry.input.bottom - geometry.select.bottom));
  assert.ok(Math.abs(geometry.input.bottom - geometry.addButton.bottom) <= 1,
    label + ': add button bottom ' + geometry.addButton.bottom + ' vs input bottom ' + geometry.input.bottom);
  assert.ok(Math.abs(geometry.select.bottom - geometry.addButton.bottom) <= 1,
    label + ': add button bottom ' + geometry.addButton.bottom + ' vs select bottom ' + geometry.select.bottom);
}

async function addManualRow(page, id, protocol) {
  await page.locator('#manual-model-id').fill(id);
  await page.locator('#manual-model-protocol').selectOption(protocol);
  await page.locator('#manual-model-add').click();
}

async function setPageTheme(page, theme) {
  await page.evaluate((value) => {
    // The warm-grey light theme is the no-attribute default.
    if (value === 'light') document.documentElement.removeAttribute('data-theme');
    else document.documentElement.setAttribute('data-theme', value);
  }, theme);
}

async function main() {
  const out = process.argv[2];
  if (!out) throw new Error('output directory required');
  fs.mkdirSync(out, { recursive: true });
  const policy = fs.readFileSync(path.resolve(web, '../provider/management.go'), 'utf8').match(/headers.Set\("Content-Security-Policy", "([^"]+)"\)/)[1];
  const server = http.createServer((req, res) => {
    const name = req.url.endsWith('/ui') ? 'ui.html' : path.basename(req.url);
    if (!['ui.html', 'ui.js', 'ui.css'].includes(name)) { res.writeHead(404).end(); return; }
    res.writeHead(200, { 'Content-Type': name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'text/html', 'Content-Security-Policy': policy });
    res.end(fs.readFileSync(path.join(web, name)));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const origin = 'http://127.0.0.1:' + server.address().port;
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true });
  const results = [];
  const errors = [];
  const config = { enabled: true, 'base-url': 'https://example.invalid/v1', models: [{ id: 'old', protocol: 'openai' }], 'manual-models': [] };
  let failValidation = false;
  let temporary503 = 0;
  let writes = 0;
  const secret = 'fake-model-management-key';
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
    page.on('pageerror', (err) => errors.push(err.message));
    await page.route('**/management/**', async (route) => {
      const req = route.request();
      assert.equal(req.headers().authorization, 'Bearer ' + secret);
      assert.equal(req.url().includes(secret), false);
      const pathname = new URL(req.url()).pathname;
      assert.ok(pathname.startsWith('/proxy/v0/management/plugins/' + plugin + '/'));
      let payload = {};
      let status = 200;
      if (pathname.endsWith('/keys')) payload = { files: [] };
      if (pathname.endsWith('/settings')) {
        payload = { 'manual-models': config['manual-models'] };
        if (temporary503 > 0) { temporary503--; status = 503; }
      }
      if (pathname.endsWith('/validate')) { status = failValidation ? 400 : 200; payload = { valid: !failValidation }; }
      if (pathname.endsWith('/config')) {
        if (req.method() === 'PATCH') {
          const patch = req.postDataJSON();
          assert.deepEqual(Object.keys(patch), ['manual-models']);
          config['manual-models'] = patch['manual-models'];
          writes++;
          temporary503 = 1;
        }
        payload = config;
      }
      await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(payload) });
    });
    await page.goto(origin + '/proxy/v0/resource/plugins/' + plugin + '/ui');
    await page.locator('#mgmt-key').fill(secret);
    await page.locator('#refresh-btn').click();
    await page.waitForFunction(() => document.getElementById('manual-model-status').textContent.includes('已加载'));
    await page.locator('#manual-model-id').fill('novel');
    await page.locator('#manual-model-add').click();
    assert.match(await page.locator('#manual-model-status').textContent(), /未知模型族/);
    await page.locator('#manual-model-protocol').selectOption('claude');
    await page.locator('#manual-model-add').click();
    assert.match(await page.locator('#manual-model-body').textContent(), /novel/);
    await page.locator('#manual-model-id').fill('opencode-go/glm-5.2');
    await page.locator('#manual-model-protocol').selectOption('');
    await page.locator('#manual-model-add').click();
    results.push('add native/namespaced IDs and validate unknown protocols');

    failValidation = true;
    await page.locator('#manual-model-save').click();
    await page.waitForFunction(() => document.getElementById('manual-model-status').textContent.includes('未发出写入请求'));
    assert.equal(writes, 0);
    assert.equal(await page.locator('#manual-model-body tr').count(), 2);
    failValidation = false;
    await page.locator('#manual-model-save').click();
    await page.waitForFunction(() => document.getElementById('manual-model-status').textContent.includes('已保存并确认'));
    assert.equal(writes, 1);
    assert.equal(config.enabled, true);
    assert.equal(config.models[0].id, 'old');
    assert.equal(config['base-url'], 'https://example.invalid/v1');
    results.push('failed validation preserves draft; bounded 503 confirmation and shallow save');

    await page.locator('#manual-model-body button').first().click();
    await page.locator('#manual-model-body button').first().click();
    await page.locator('#manual-model-save').click();
    await page.waitForFunction(() => document.getElementById('manual-model-status').textContent.includes('已保存并确认'));
    assert.deepEqual(config['manual-models'], []);
    assert.equal(await page.locator('#manual-model-save').isDisabled(), true);
    results.push('remove and persist an empty manual catalog');

    // Empty draft: one spanning placeholder row, still geometry-safe.
    assert.equal(await page.locator('#manual-model-body tr').count(), 1);
    assert.match(await page.locator('#manual-model-body').textContent(), /未手动注册模型/);
    assert.equal(await page.locator('#manual-model-body td').getAttribute('colspan'), '3');
    results.push('empty draft renders a single spanning placeholder row');

    // Geometry: reproduce the reported step-5-preview-free/openai row, then a
    // long wrapping id, and verify the control row and every draft row align.
    await addManualRow(page, MANUAL_ID, 'openai');
    await addManualRow(page, LONG_ID, 'claude');
    const geometry = await readManualGeometry(page);
    assert.equal(geometry.rows.length, 2);
    assertControlAlignment('desktop control row', geometry);
    const shortRow = geometry.rows.find((row) => row.id === MANUAL_ID);
    const longRow = geometry.rows.find((row) => row.id === LONG_ID.replace(/^opencode-go\//, ''));
    assert.ok(shortRow && longRow, 'expected the explicit and long draft rows');
    assertRowCentered('explicit openai row', shortRow);
    assertRowCentered('long wrapping row', longRow);
    // The long identifier must actually wrap so centering is exercised on a
    // multi-line cell rather than a single line.
    assert.ok(longRow.cellHeight > shortRow.cellHeight,
      'long id should wrap: long cell ' + longRow.cellHeight + ' vs short ' + shortRow.cellHeight);
    results.push('manual row text centers on a compact (>=24px) delete button');

    // Light / white / dark close-ups of the manual editor at desktop width.
    for (const theme of ['light', 'white', 'dark']) {
      await setPageTheme(page, theme);
      await page.locator('section[aria-labelledby="models-heading"]')
        .screenshot({ path: path.join(out, 'manual-models-' + theme + '-1280.png') });
    }
    results.push('light/white/dark manual editor close-ups at 1280');

    // Mobile: the wide table scrolls inside its wrapper, never the page.
    await setPageTheme(page, 'light');
    await page.setViewportSize({ width: 390, height: 900 });
    const mobile = await page.evaluate(() => {
      const wrap = document.querySelector('section[aria-labelledby="models-heading"] .table-wrap');
      return {
        pageScrollWidth: document.documentElement.scrollWidth,
        innerWidth: window.innerWidth,
        wrapClientWidth: wrap.clientWidth,
        wrapScrollWidth: wrap.scrollWidth
      };
    });
    assert.ok(mobile.pageScrollWidth <= mobile.innerWidth,
      'page overflows: ' + mobile.pageScrollWidth + ' > ' + mobile.innerWidth);
    assert.ok(mobile.wrapScrollWidth > mobile.wrapClientWidth,
      'wide draft table should scroll inside its wrapper');
    await page.screenshot({ path: path.join(out, 'manual-models-390.png'), fullPage: true });
    results.push('mobile keeps table-internal scroll without page overflow');
    assert.deepEqual(errors, []);
    results.push('no browser exceptions');
  } finally {
    await browser.close();
    await new Promise((resolve) => server.close(resolve));
  }
  fs.writeFileSync(path.join(out, 'models-results.json'), JSON.stringify(results, null, 2));
  console.log(results.length + '/' + results.length + ' passed');
}
main().catch((err) => { console.error(err); process.exitCode = 1; });
