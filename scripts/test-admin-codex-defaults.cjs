#!/usr/bin/env node
// Isolated browser checks: no live workers or model requests.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.resolve(__dirname, '../internal/admin/static');
const worker = { ID: '11111111-2222-4333-8444-555555555555', Name: 'Codex worker <img src=x onerror="window.injected=true">', AgentKind: 'codex', Enabled: true, Connectivity: 'connected', SupportsWorkerRuntimeSettings: true };

async function checks(browser, width) {
  const page = await browser.newPage({ viewport: { width, height: 900 } });
  const errors = [], updates = [], verifications = [];
  let defaults = { model: 'worker-model-v1', reasoning_effort: 'high', permission_level: 'ask-for-approval' };
  let staleAuth = false, loginFinishes = 0, abortWrite = false, heldRead = null, holdRead = false;
  let codexProvider = 'codex', claudeProvider = 'claude';
  const writeStatuses = [];
  const workers = [worker,
    { ...worker, ID: 'legacy-kind', Name: 'Codex without explicit kind', AgentKind: '' },
    { ...worker, ID: 'older', Name: 'Older Codex worker', SupportsWorkerRuntimeSettings: false },
    { ...worker, ID: 'revoked', Name: 'Revoked Codex worker', Enabled: false },
    { ...worker, ID: 'claude', Name: 'Claude worker', AgentKind: 'claude' },
  ];
  page.on('pageerror', error => errors.push(error.message));
  await page.context().addCookies([{ name: '__Host-telegramgw-csrf', value: 'codex-defaults-csrf', url: 'https://admin.test/', secure: true, sameSite: 'Strict' }]);
  await page.addInitScript(() => {
    Object.defineProperty(navigator.credentials, 'get', { configurable: true, value: async () => {
      if (window.cancelPasskey) throw new DOMException('Cancelled', 'NotAllowedError');
      if (window.holdPasskey) await new Promise(resolve => { window.releasePasskey = resolve; });
      return { id: 'passkey', rawId: new Uint8Array([1, 2, 3]).buffer, type: 'public-key',
        response: { clientDataJSON: new Uint8Array([1]).buffer, authenticatorData: new Uint8Array([2]).buffer, signature: new Uint8Array([3]).buffer } };
    } });
  });
  await page.route('https://admin.test/**', async route => {
    const request = route.request(), url = new URL(request.url());
    const reply = (body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    if (!url.pathname.startsWith('/tgw/api/')) {
      const filename = /\.(js|css|svg)$/.test(url.pathname) ? path.basename(url.pathname) : 'index.html';
      return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : filename.endsWith('.svg') ? 'image/svg+xml' : 'text/html',
        headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'" }, body: fs.readFileSync(path.join(assets, filename)) });
    }
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, session_id: 'browser-' + loginFinishes, authentication_method: 'passkey',
      expires_at: new Date(Date.now() + 28800000).toISOString(), server_time: new Date().toISOString(), reauthenticated_at: new Date(Date.now() - (staleAuth ? 360000 : 0)).toISOString() });
    if (url.pathname.endsWith('/login/begin')) {
      verifications.push(request.postDataJSON());
      return reply({ ceremony_id: 'ceremony', publicKey: { challenge: 'dGVzdC1jaGFsbGVuZ2U', rpId: 'admin.test' } });
    }
    if (url.pathname.endsWith('/login/finish')) { staleAuth = false; loginFinishes++; return reply({ ok: true }); }
    if (url.pathname.endsWith('/dashboard')) return reply({ workers, runtimes: [], sessions: [], pending_approvals: 0, queued_commands: 0 });
    if (url.pathname.endsWith('/admin/sessions')) return reply({ sessions: [] });
    if (url.pathname.endsWith('/passkeys')) return reply([]);
    if (url.pathname.endsWith('/admin/password')) return reply({ enabled: false, username: '' });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], pending: [] });
    if (url.pathname.endsWith('/admin/audio')) return reply({ settings: { enabled: false, model: 'base', threads: 2, tts_voice: 'auto' } });
    if (url.pathname.endsWith('/runtime-settings')) {
      assert.equal(url.search, '');
      if (url.pathname.includes('/workers/claude/')) return reply({ provider: claudeProvider, model: 'claude-model', base_url: '', auth_mode: 'login', permission_mode: 'default', api_key_configured: false });
      if (request.method() === 'GET') {
        const value = { provider: codexProvider, codex_defaults: { ...defaults } };
        if (holdRead) { holdRead = false; await new Promise(resolve => { heldRead = resolve; }); }
        return reply(value);
      }
      assert.equal(request.method(), 'PUT');
      assert.equal(request.headers()['x-csrf-token'], 'codex-defaults-csrf');
      const update = request.postDataJSON();
      assert.deepEqual(Object.keys(update), ['codex_defaults'], 'Codex writes contain no Claude fields');
      assert.deepEqual(Object.keys(update.codex_defaults).sort(), ['model', 'permission_level', 'reasoning_effort']);
      updates.push(update);
      if (abortWrite) { abortWrite = false; return route.abort('failed'); }
      const status = writeStatuses.shift() || 200;
      if (status !== 200) return reply({ code: status === 403 ? 'reauthentication_required' : 'invalid_runtime_settings', message: 'unsafe upstream error' }, status);
      defaults = { ...update.codex_defaults };
      return reply({ provider: 'codex', codex_defaults: defaults });
    }
    return reply({});
  });
  const open = async () => {
    await page.getByRole('button', { name: 'Codex defaults', exact: true }).first().click();
    await page.waitForFunction(() => !document.getElementById('worker-runtime-fields').disabled);
  };
  const save = async () => {
    await page.locator('#worker-runtime-save').click();
    await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.startsWith('Saved.'));
  };
  try {
    await page.goto('https://admin.test/tgw/admin/');
    await page.locator('#worker-section > summary').click();
    assert.equal(await page.getByRole('button', { name: 'Codex defaults', exact: true }).count(), 2, 'Only supported enabled Codex workers expose defaults');
    assert.equal(await page.getByRole('button', { name: 'Claude Code settings', exact: true }).count(), 1);
    await open();
    assert.equal(await page.locator('#worker-runtime-heading').textContent(), 'Codex defaults');
    assert.equal(await page.locator('#worker-runtime-target').textContent(), worker.Name);
    assert.equal(await page.locator('#worker-runtime-dialog img').count(), 0);
    assert.equal(await page.locator('#worker-runtime-claude').isVisible(), false);
    assert.equal(await page.locator('#worker-runtime-codex').isVisible(), true);
    assert.equal(await page.locator('#worker-runtime-codex-model').inputValue(), defaults.model);
    assert.equal(await page.locator('#worker-runtime-codex-reasoning').inputValue(), defaults.reasoning_effort);
    assert.equal(await page.locator('#worker-runtime-codex-permissions').inputValue(), defaults.permission_level);
    assert.match(await page.locator('#worker-runtime-description').textContent(), /every new session.*\/clear.*Existing sessions keep their saved settings until cleared/);
    assert.deepEqual(await page.locator('#worker-runtime-codex-reasoning option').evaluateAll(nodes => nodes.map(node => node.value)), ['', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'ultra']);
    assert.deepEqual(await page.locator('#worker-runtime-codex-permissions option').evaluateAll(nodes => nodes.map(node => node.value)), ['', 'ask-for-approval', 'read-only', 'full-access']);
    assert.equal(await page.locator('#worker-runtime-codex-model').getAttribute('list'), null, 'Model IDs remain free input');
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true, 'Worker controls fit the viewport');
    const bounds = await page.locator('#worker-runtime-dialog').boundingBox();
    assert.ok(bounds.x >= 0 && bounds.x + bounds.width <= width + 1);
    for (const id of ['model', 'reasoning', 'permissions']) {
      const field = await page.locator('#worker-runtime-codex-' + id).boundingBox();
      assert.ok(field.x >= bounds.x && field.x + field.width <= bounds.x + bounds.width, id + ' fits the mobile dialog');
    }
    await page.locator('#worker-runtime-codex-model').fill('  future-model-v2  ');
    await page.locator('#worker-runtime-codex-reasoning').selectOption('xhigh');
    await page.locator('#worker-runtime-codex-permissions').selectOption('full-access');
    assert.equal(await page.locator('#worker-runtime-codex-full-access').isVisible(), true);
    staleAuth = true;
    await save();
    assert.equal(loginFinishes, 1, 'Saving stale authentication performs fresh verification');
    assert.deepEqual(verifications[0], {}, 'Sensitive verification preserves existing device trust');
    assert.deepEqual(updates.at(-1), { codex_defaults: { model: 'future-model-v2', reasoning_effort: 'xhigh', permission_level: 'full-access' } });
    assert.match(await page.locator('#worker-runtime-status').textContent(), /Existing sessions keep their saved settings until cleared/);
    await page.locator('#worker-runtime-close').click();
    await open();
    assert.equal(await page.locator('#worker-runtime-codex-model').inputValue(), 'future-model-v2', 'Reopening loads saved worker defaults');
    const beforeReset = updates.length;
    await page.locator('#worker-runtime-codex-reset').click();
    for (const id of ['model', 'reasoning', 'permissions']) assert.equal(await page.locator('#worker-runtime-codex-' + id).inputValue(), '');
    assert.equal(await page.locator('#worker-runtime-codex-full-access').isVisible(), false);
    assert.equal(updates.length, beforeReset, 'Resetting the form requires an explicit save');
    await save();
    assert.deepEqual(updates.at(-1), { codex_defaults: { model: '', reasoning_effort: '', permission_level: '' } }, 'Empty strings remove all overrides');
    await page.locator('#worker-runtime-codex-permissions').selectOption('read-only');
    writeStatuses.push(403, 200);
    const beforeRetry = updates.length, beforeLogin = loginFinishes;
    await save();
    assert.equal(updates.length, beforeRetry + 2, 'Only an explicit reauthentication rejection retries a write');
    assert.equal(loginFinishes, beforeLogin + 1);
    assert.deepEqual(updates.at(-2), updates.at(-1));
    assert.equal(defaults.permission_level, 'read-only');
    abortWrite = true;
    const beforeFailure = updates.length;
    await page.locator('#worker-runtime-save').click();
    await page.locator('#worker-runtime-error').waitFor({ state: 'visible' });
    assert.equal(updates.length, beforeFailure + 1, 'Ambiguous network failures do not replay the mutation');
    assert.match(await page.locator('#worker-runtime-error').textContent(), /reload the worker settings/);
    await page.locator('#worker-runtime-close').click();
    await open();
    staleAuth = true;
    await page.evaluate(() => { window.cancelPasskey = true; });
    const beforeCancel = updates.length;
    await page.locator('#worker-runtime-save').click();
    await page.locator('#worker-runtime-error').waitFor({ state: 'visible' });
    assert.equal(updates.length, beforeCancel, 'Cancelled fresh verification never writes defaults');
    await page.evaluate(() => { window.cancelPasskey = false; window.holdPasskey = true; });
    await page.locator('#worker-runtime-save').click();
    await page.waitForFunction(() => typeof window.releasePasskey === 'function');
    await page.locator('#worker-runtime-close').click();
    await page.evaluate(() => { window.holdPasskey = false; window.releasePasskey(); });
    await page.waitForResponse(response => response.url().endsWith('/login/finish'));
    await page.waitForFunction(() => !document.getElementById('console').inert);
    assert.equal(updates.length, beforeCancel, 'Closing during fresh verification fences the later write');
    holdRead = true;
    await page.getByRole('button', { name: 'Codex defaults', exact: true }).first().click();
    assert.equal(await page.locator('#worker-runtime-save').isDisabled(), true, 'Loading cannot submit stale values');
    await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.includes('Loading'));
    await page.locator('#worker-runtime-close').click();
    await page.getByRole('button', { name: 'Claude Code settings', exact: true }).click();
    await page.waitForFunction(() => !document.getElementById('worker-runtime-fields').disabled);
    assert.equal(typeof heldRead, 'function');
    const staleResponse = page.waitForResponse(response => response.url().includes('/workers/' + worker.ID + '/runtime-settings'));
    heldRead();
    await staleResponse;
    assert.equal(await page.locator('#worker-runtime-heading').textContent(), 'Claude Code settings');
    assert.equal(await page.locator('#worker-runtime-claude').isVisible(), true);
    assert.equal(await page.locator('#worker-runtime-codex').isVisible(), false);
    assert.equal(await page.locator('#worker-runtime-codex-reset').isVisible(), false);
    assert.equal(await page.locator('#worker-runtime-model').inputValue(), 'claude-model', 'A delayed Codex read cannot overwrite Claude settings');
    assert.equal(await page.locator('#worker-runtime-save').textContent(), 'Save settings');
    await page.locator('#worker-runtime-close').click();
    assert.equal(await page.locator('#worker-runtime-codex-model').inputValue(), '');
    codexProvider = 'claude';
    await page.getByRole('button', { name: 'Codex defaults', exact: true }).first().click();
    await page.locator('#worker-runtime-error').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#worker-runtime-save').isDisabled(), true, 'A Codex dialog rejects a mismatched provider response');
    await page.locator('#worker-runtime-close').click();
    claudeProvider = 'codex';
    await page.getByRole('button', { name: 'Claude Code settings', exact: true }).click();
    await page.locator('#worker-runtime-error').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#worker-runtime-save').isDisabled(), true, 'A Claude dialog rejects a mismatched provider response');
    await page.locator('#worker-runtime-close').click();
    assert.equal(await page.evaluate(() => window.injected), undefined);
    assert.deepEqual(errors, []);
  } finally { await page.close(); }
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    for (const width of [320, 390, 1280]) await checks(browser, width);
    process.stdout.write('Codex worker defaults browser checks passed.\n');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
