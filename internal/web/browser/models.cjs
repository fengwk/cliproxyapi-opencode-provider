'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('playwright');
const web = path.resolve(__dirname, '..');
const plugin = 'cliproxyapi-opencode-provider';

// Draft rows used to exercise the manual-model geometry and wrapping.
const MANUAL_ID = 'step-5-preview-free';
const LONG_ID = 'opencode-go/' + 'wrapped-model-identifier-'.repeat(5) + 'end';

// Shared token contract: computed body background per theme.
const THEME_BG = { light: 'rgb(250, 249, 245)', white: 'rgb(255, 255, 255)', dark: 'rgb(21, 20, 18)' };
// Shared button contract (BUTTONS.md): normal controls are 46px, .btn-sm is 39px.
const BUTTON_HEIGHT = { normal: 46, small: 39 };
const SEMANTICS = ['btn-primary', 'btn-secondary', 'btn-danger', 'btn-ghost'];

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
    const lineCount = (el) => {
      const range = document.createRange();
      range.selectNodeContents(el);
      return range.getClientRects().length;
    };
    const rows = Array.from(document.querySelectorAll('#manual-model-body tr')).map((row) => {
      const cells = Array.from(row.children);
      const button = row.querySelector('button');
      return {
        id: cells[0] ? cells[0].textContent : '',
        idLines: cells[0] ? lineCount(cells[0]) : 0,
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

// A row's id/protocol text must be centered on its own (shared-small) delete
// button, whose single-line label keeps the target compact.
function assertRowCentered(label, row) {
  assert.ok(row.button, label + ': expected a row delete button');
  assert.ok(Math.abs(row.idTextCenter - row.button.center) <= 1,
    label + ': id text center ' + row.idTextCenter + ' vs delete center ' + row.button.center);
  assert.ok(Math.abs(row.protocolTextCenter - row.button.center) <= 1,
    label + ': protocol text center ' + row.protocolTextCenter + ' vs delete center ' + row.button.center);
  assert.ok(Math.abs(row.button.height - BUTTON_HEIGHT.small) <= 1,
    label + ': delete button height ' + row.button.height + ' != shared small ' + BUTTON_HEIGHT.small);
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

async function readButtons(page) {
  return page.evaluate(() => {
    const round = (value) => Math.round(value * 100) / 100;
    return Array.from(document.querySelectorAll('button')).map((button) => {
      const style = getComputedStyle(button);
      return {
        id: button.id || '',
        label: button.textContent,
        action: button.getAttribute('data-action') || '',
        inDraft: Boolean(button.closest('#manual-model-body')),
        classes: Array.from(button.classList),
        height: round(button.getBoundingClientRect().height),
        fontSize: style.fontSize,
        fontWeight: style.fontWeight,
        borderRadius: style.borderRadius,
        whiteSpace: style.whiteSpace,
        color: style.color,
        background: style.backgroundColor,
        disabled: button.disabled,
        opacity: style.opacity,
        cursor: style.cursor
      };
    });
  });
}

// Every live button carries .btn with exactly one semantic variant; the shared
// size classes dictate the rendered dimensions.
function assertButtonContract(label, buttons) {
  assert.ok(buttons.length >= 9, label + ': expected every live button, got ' + buttons.length);
  for (const button of buttons) {
    const where = label + ': ' + (button.id || button.action || button.label);
    assert.ok(button.classes.includes('btn'), where + ' must carry .btn');
    assert.equal(SEMANTICS.filter((variant) => button.classes.includes(variant)).length, 1,
      where + ' must have exactly one semantic variant');
    const small = button.classes.includes('btn-sm');
    const want = small ? BUTTON_HEIGHT.small : BUTTON_HEIGHT.normal;
    assert.ok(Math.abs(button.height - want) <= 1, where + ' height ' + button.height + ' != ' + want);
    assert.equal(button.fontSize, small ? '14px' : '16px', where + ' font-size');
    assert.equal(button.fontWeight, '600', where + ' font-weight');
    assert.equal(button.borderRadius, '8px', where + ' radius');
    assert.equal(button.whiteSpace, 'nowrap', where + ' white-space');
    if (button.classes.includes('btn-primary')) {
      assert.equal(button.background, 'rgb(139, 134, 128)', where + ' primary background');
      assert.equal(button.color, 'rgb(255, 255, 255)', where + ' primary text');
    }
    if (button.classes.includes('btn-danger')) {
      assert.equal(button.background, 'rgb(198, 87, 70)', where + ' danger background');
      assert.equal(button.color, 'rgb(255, 255, 255)', where + ' danger text');
    }
    if (button.classes.includes('btn-secondary')) {
      assert.equal(button.background, 'rgb(246, 246, 246)', where + ' secondary background');
    }
  }
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
  // Wait out the body color transition so screenshots never capture a blend.
  await page.waitForFunction(
    (want) => getComputedStyle(document.body).backgroundColor === want,
    THEME_BG[theme], { timeout: 5000 }
  );
}

async function screenshotStable(target, file) {
  // animations: 'disabled' fast-forwards finite transitions to completion.
  await target.screenshot({ path: file, animations: 'disabled' });
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
  const evidence = {};
  const config = { enabled: true, 'base-url': 'https://example.invalid/v1', models: [{ id: 'old', protocol: 'openai' }], 'manual-models': [] };
  // One managed credential renders the quota (secondary small) and delete
  // (danger small) actions alongside the manual draft rows.
  const files = [{ name: 'opencode-go-1.json', label: 'batch', status: 'active', disabled: false, success: 4, failed: 1, auth_index: '0' }];
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
      if (pathname.endsWith('/keys')) payload = { files };
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

    // The save button starts disabled and must show the shared disabled state.
    const disabledSave = await page.evaluate(() => {
      const button = document.getElementById('manual-model-save');
      const style = getComputedStyle(button);
      return { disabled: button.disabled, opacity: style.opacity, cursor: style.cursor, classes: Array.from(button.classList) };
    });
    assert.equal(disabledSave.disabled, true);
    assert.equal(disabledSave.opacity, '0.6');
    assert.equal(disabledSave.cursor, 'not-allowed');
    assert.ok(disabledSave.classes.includes('btn') && disabledSave.classes.includes('btn-primary'));
    results.push('disabled buttons keep the shared .btn disabled treatment');

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

    // Geometry: an explicit-protocol row plus a long wrapping id verify the
    // control row and every draft row align.
    await addManualRow(page, MANUAL_ID, 'openai');
    await addManualRow(page, LONG_ID, 'claude');
    const geometry = await readManualGeometry(page);
    evidence.geometry = geometry;
    assert.equal(geometry.rows.length, 2);
    assertControlAlignment('desktop control row', geometry);
    const shortRow = geometry.rows.find((row) => row.id === MANUAL_ID);
    const longRow = geometry.rows.find((row) => row.id === LONG_ID.replace(/^opencode-go\//, ''));
    assert.ok(shortRow && longRow, 'expected the explicit and long draft rows');
    assertRowCentered('explicit openai row', shortRow);
    assertRowCentered('long wrapping row', longRow);
    // The long identifier must genuinely wrap (multiple line boxes); its row
    // height may match the short row under the shared small control size.
    assert.equal(shortRow.idLines, 1, 'short id should stay on one line');
    assert.ok(longRow.idLines >= 2, 'long id should wrap into multiple line boxes, got ' + longRow.idLines);
    results.push('manual row text centers on the shared small delete control');

    // Every live button opts into the shared contract with the right variant.
    // The white theme keeps the computed semantic colors deterministic.
    await setPageTheme(page, 'white');
    const buttons = await readButtons(page);
    evidence.buttons = buttons;
    assertButtonContract('live buttons', buttons);
    const byId = (id) => buttons.find((button) => button.id === id);
    const has = (button, ...classes) => button && classes.every((name) => button.classes.includes(name));
    for (const id of ['refresh-btn', 'import-btn', 'manual-model-save']) {
      assert.ok(has(byId(id), 'btn', 'btn-primary'), id + ' must be a primary .btn');
    }
    for (const id of ['reload-btn', 'manual-model-add', 'manual-model-reload']) {
      assert.ok(has(byId(id), 'btn', 'btn-secondary'), id + ' must be a secondary .btn');
    }
    const quotaButtons = buttons.filter((button) => button.action === 'quota');
    assert.ok(quotaButtons.length >= 1 && quotaButtons.every((button) => has(button, 'btn', 'btn-secondary', 'btn-sm')));
    const credentialDeletes = buttons.filter((button) => button.action === 'delete');
    assert.ok(credentialDeletes.length >= 1 && credentialDeletes.every((button) => has(button, 'btn', 'btn-danger', 'btn-sm')));
    const draftDeletes = buttons.filter((button) => button.inDraft);
    assert.ok(draftDeletes.length >= 1 && draftDeletes.every((button) => has(button, 'btn', 'btn-danger', 'btn-sm')));
    results.push('all live buttons carry .btn with the contracted semantics and sizes');

    // Keyboard focus: a tab-focused button shows the shared focus outline.
    await page.mouse.move(0, 0);
    await page.evaluate(() => { if (document.activeElement) document.activeElement.blur(); });
    let focus = null;
    for (let i = 0; i < 40 && !focus; i++) {
      await page.keyboard.press('Tab');
      focus = await page.evaluate(() => {
        const el = document.activeElement;
        if (!el || el.tagName !== 'BUTTON') return null;
        const style = getComputedStyle(el);
        return {
          label: el.textContent,
          focusVisible: el.matches(':focus-visible'),
          outlineStyle: style.outlineStyle,
          outlineWidth: style.outlineWidth,
          outlineOffset: style.outlineOffset,
          outlineColor: style.outlineColor
        };
      });
    }
    assert.ok(focus, 'a button should receive keyboard focus');
    assert.equal(focus.focusVisible, true, 'focused button should match :focus-visible');
    assert.equal(focus.outlineStyle, 'solid');
    assert.equal(focus.outlineWidth, '2px');
    assert.equal(focus.outlineOffset, '3px');
    assert.equal(focus.outlineColor, 'rgb(45, 42, 38)');
    results.push('keyboard focus shows the shared .btn focus outline');

    // Hover: a secondary button moves to the contracted hover border.
    await page.locator('#manual-model-add').hover();
    await page.waitForFunction(
      () => getComputedStyle(document.getElementById('manual-model-add')).borderColor === 'rgb(204, 204, 204)',
      { timeout: 2000 }
    );
    results.push('secondary hover applies the shared hover border');
    // Leave the pointer off the buttons so screenshots never show hover state.
    await page.mouse.move(0, 0);

    // Light / white / dark close-ups of the manual editor at desktop width.
    for (const theme of ['light', 'white', 'dark']) {
      await setPageTheme(page, theme);
      await screenshotStable(
        page.locator('section[aria-labelledby="models-heading"]'),
        path.join(out, 'manual-models-' + theme + '-1280.png')
      );
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
    await screenshotStable(page, path.join(out, 'manual-models-390.png'));
    results.push('mobile keeps table-internal scroll without page overflow');
    assert.deepEqual(errors, []);
    results.push('no browser exceptions');
  } finally {
    await browser.close();
    await new Promise((resolve) => server.close(resolve));
  }
  fs.writeFileSync(path.join(out, 'models-geometry.json'), JSON.stringify(evidence, null, 2));
  fs.writeFileSync(path.join(out, 'models-results.json'), JSON.stringify(results, null, 2));
  console.log(results.length + '/' + results.length + ' passed');
}
main().catch((err) => { console.error(err); process.exitCode = 1; });
