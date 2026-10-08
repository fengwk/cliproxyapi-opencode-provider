'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('playwright');
const web = path.resolve(__dirname, '..');
const plugin = 'cliproxyapi-opencode-provider';

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

    await page.setViewportSize({ width: 390, height: 900 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
    assert.deepEqual(errors, []);
    await page.screenshot({ path: path.join(out, 'manual-models-390.png'), fullPage: true });
    results.push('mobile layout and no browser exceptions');
  } finally {
    await browser.close();
    await new Promise((resolve) => server.close(resolve));
  }
  fs.writeFileSync(path.join(out, 'models-results.json'), JSON.stringify(results, null, 2));
  console.log(results.length + '/' + results.length + ' passed');
}
main().catch((err) => { console.error(err); process.exitCode = 1; });
