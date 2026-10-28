#!/usr/bin/env node
// Isolated browser checks: no live worker, credentials or model requests.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.resolve(__dirname, '../internal/admin/static');
const secret = 'private-claude-browser-test-token';
const worker = { ID: '11111111-2222-4333-8444-555555555555', Name: 'Claude worker', AgentKind: 'claude', Enabled: true, Connectivity: 'online', SupportsWorkerRuntimeSettings: true, SupportsSessionEditing: true, SupportsSessionDeletion: false, SupportsSessionArchives: false };
const session = { session_id: 'claude-session', codex_thread_id: 'claude-thread', worker_id: worker.ID, runtime_id: 'claude-runtime', runtime_name: 'Claude Code', worker_name: worker.Name, name: 'Claude project', cwd: '/projects/claude', state: 'running' };

async function staticReply(route, filename) {
  const file = path.join(assets, filename);
  if (!fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
  return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : filename.endsWith('.svg') ? 'image/svg+xml' : 'text/html',
    headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; object-src 'none'; base-uri 'none'" }, body: fs.readFileSync(file) });
}

async function adminChecks(browser, width) {
  const page = await browser.newPage({ viewport: { width, height: 900 } });
  const errors = [], logs = [], updates = [];
  const value = { provider: 'claude', model: 'custom[1m]', base_url: 'http://localhost:8080', auth_mode: 'auth_token', permission_mode: 'default', api_key_configured: true };
  let fail = false;
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => logs.push(message.text()));
  await page.context().addCookies([{ name: '__Host-telegramgw-csrf', value: 'claude-csrf', url: 'https://admin.test/', secure: true, sameSite: 'Strict' }]);
  await page.route('https://admin.test/**', async route => {
    const request = route.request(), url = new URL(request.url());
    const reply = (body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    if (!url.pathname.startsWith('/tgw/api/')) return staticReply(route, /\.(js|css|svg)$/.test(url.pathname) ? path.basename(url.pathname) : 'index.html');
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, session_id: 'browser-session', expires_at: new Date(Date.now() + 28800000).toISOString(), server_time: new Date().toISOString(), reauthenticated_at: new Date().toISOString() });
    if (url.pathname.endsWith('/dashboard')) return reply({ workers: [worker], runtimes: [], sessions: [], pending_approvals: 0, queued_commands: 0 });
    if (url.pathname.endsWith('/admin/sessions')) return reply({ sessions: [] });
    if (url.pathname.endsWith('/passkeys')) return reply([]);
    if (url.pathname.endsWith('/admin/password')) return reply({ enabled: false, username: '' });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], pending: [] });
    if (url.pathname.endsWith('/admin/audio')) return reply({ settings: { enabled: false, model: 'base', threads: 2, tts_voice: 'auto' } });
    if (url.pathname.endsWith('/runtime-settings')) {
      assert.equal(url.search, '', 'Credentials never appear in a request URL');
      if (request.method() === 'GET') return reply(value);
      assert.equal(request.method(), 'PUT');
      assert.equal(request.headers()['x-csrf-token'], 'claude-csrf');
      const update = JSON.parse(request.postData()); updates.push(update);
      if (fail) return reply({ code: 'runtime_settings_unavailable', message: 'Unsafe worker failure ' + secret }, 503);
      Object.assign(value, { model: update.model, base_url: update.base_url, auth_mode: update.auth_mode, permission_mode: update.permission_mode });
      if ('api_key' in update) value.api_key_configured = !!update.api_key;
      return reply(value);
    }
    return reply({});
  });
  await page.goto('https://admin.test/tgw/admin/');
  await page.locator('#worker-section > summary').click();
  await page.getByRole('button', { name: 'Claude Code settings', exact: true }).click();
  await page.waitForFunction(() => !document.getElementById('worker-runtime-fields').disabled);
  assert.equal(await page.locator('#worker-runtime-key').getAttribute('type'), 'password');
  assert.equal(await page.locator('#worker-runtime-key').inputValue(), '');
  assert.match(await page.locator('#worker-runtime-key-help').textContent(), /credential is saved/);
  assert.equal(await page.locator('#worker-runtime-model').inputValue(), 'custom[1m]');
  await page.locator('#worker-runtime-save').click();
  await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.startsWith('Saved.'));
  assert.equal(Object.hasOwn(updates[0], 'api_key'), false, 'Blank retains the credential');
  await page.locator('#worker-runtime-key').fill(secret);
  await page.locator('#worker-runtime-permissions').selectOption('bypassPermissions');
  assert.equal(await page.locator('#worker-runtime-bypass').isVisible(), true);
  await page.locator('#worker-runtime-save').click();
  await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.startsWith('Saved.'));
  assert.equal(updates[1].api_key, secret);
  assert.equal(await page.locator('#worker-runtime-key').inputValue(), '');
  await page.locator('#worker-runtime-clear').check();
  assert.equal(await page.locator('#worker-runtime-key').isDisabled(), true);
  await page.locator('#worker-runtime-auth').selectOption('login');
  await page.locator('#worker-runtime-save').click();
  await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.startsWith('Saved.'));
  assert.equal(updates[2].api_key, '', 'Remove explicitly clears the credential');
  fail = true;
  await page.locator('#worker-runtime-key').fill(secret);
  await page.locator('#worker-runtime-save').click();
  await page.locator('#worker-runtime-error').waitFor({ state: 'visible' });
  assert.equal(await page.locator('#worker-runtime-key').inputValue(), '');
  assert.ok(!(await page.locator('body').innerText()).includes(secret), 'Worker errors never echo a credential');
  await page.locator('#worker-runtime-key').fill(secret);
  await page.locator('#worker-runtime-close').click();
  assert.equal(await page.locator('#worker-runtime-key').inputValue(), '');
  const storage = await page.evaluate(() => JSON.stringify([Object.entries(localStorage), Object.entries(sessionStorage), history.state, location.href]));
  assert.ok(!storage.includes(secret), 'Credential never enters browser storage or history');
  assert.ok(!logs.join('\n').includes(secret), 'Credential never enters console logs');
  assert.deepEqual(errors, []);
  await page.close();
}

async function webUIChecks(browser, width) {
  const page = await browser.newPage({ viewport: { width, height: 900 } });
  const errors = [];
  const other = { ...session, session_id: 'claude-other', codex_thread_id: 'other-thread', name: 'Other Claude project', state: 'idle', stats: { model: 'STALE_INVENTORY_MODEL', context_tokens: 777, context_window: 1000 } };
  const codexWorker = { ID: 'codex-worker', Name: 'Codex worker', AgentKind: 'codex', Enabled: true, Connectivity: 'online' };
  const codex = { ...session, session_id: 'codex-session', codex_thread_id: 'codex-thread', worker_id: codexWorker.ID, runtime_id: 'codex-runtime', runtime_name: 'Codex', name: 'Codex project', state: 'idle', stats: { model: 'codex-model', reasoning_effort: 'high', context_tokens: 500, context_window: 1000 } };
  const sessions = [session, other, codex];
  let authenticated = true;
  page.on('pageerror', error => errors.push(error.message));
  await page.route('https://webui.test/**', route => {
    const url = new URL(route.request().url());
    const reply = (body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated, owner_id: 'claude-owner', session_id: 'browser-session', expires_at: new Date(Date.now() + 28800000).toISOString(), server_time: new Date().toISOString() }, authenticated ? 200 : 401);
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return reply({ sessions, workers: [worker, codexWorker], runtimes: [{ runtime_id: session.runtime_id, worker_id: worker.ID, state: 'running' }, { runtime_id: codex.runtime_id, worker_id: codexWorker.ID, state: 'running' }] });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], revision: 'claude-fixture' });
    if (url.pathname.endsWith('/push/config')) return reply({ supported: false, subscribed: false });
    if (url.pathname.startsWith('/tgw/api/')) return reply({});
    return staticReply(route, /\/(?:webui|admin)\/static\//.test(url.pathname) ? path.basename(url.pathname) : 'webui.html');
  });
  await page.addInitScript(({ session, sessions }) => {
    window.testSent = []; window.testSockets = []; window.holdResumeThread = '';
    const unknown = { provider: 'claude', model: null, permissionMode: null, context: { usedTokens: null, maxTokens: null }, cost: { sessionUSD: null, lastTurnUSD: null }, balance: null };
    window.testStatuses = {
      [session.codex_thread_id]: { provider: 'claude', model: 'custom-reported[1m]', permissionMode: 'acceptEdits', context: { usedTokens: 10000, maxTokens: 100000 }, cost: { sessionUSD: 0.00123, lastTurnUSD: 0.00123 }, balance: null },
      'other-thread': unknown,
    };
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.url = String(url); this.readyState = 0; this.activity = this.url.includes('/activity');
        this.session = sessions.find(row => row.session_id === new URL(this.url).searchParams.get('session_id'));
        if (!this.activity) window.testSockets.push(this);
        setTimeout(() => {
          this.readyState = 1; this.onopen?.({});
          this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: sessions.map(row => ({ session_id: row.session_id, state: row.state, pending_questions: 0 })), telegram_bots: [], telegram_bots_revision: 'claude-fixture' } : { type: 'ready' });
        }, 1);
      }
      emit(value) { this.onmessage?.({ data: JSON.stringify(value) }); }
      close(code = 1000) { this.readyState = 3; this.onclose?.({ code }); }
      send(raw) {
        const frame = JSON.parse(raw); window.testSent.push(frame);
        if (!frame.method) return;
        const selected = this.session, thread = selected.codex_thread_id;
        let result = {};
        if (frame.method === 'thread/resume') {
          result = thread === 'codex-thread' ? { thread: { id: thread }, model: 'codex-model', reasoningEffort: 'high', sandbox: { type: 'workspaceWrite' }, approvalPolicy: 'on-request', approvalsReviewer: 'user' }
            : { thread: { id: thread, claudeStatus: structuredClone(window.testStatuses[thread]) } };
          if (window.holdResumeThread === thread) { this.heldResume = { id: frame.id, result }; return; }
        } else if (frame.method === 'thread/items/list') result = { data: [{ turnId: 'active-turn', turnStatus: selected.state === 'running' ? 'inProgress' : 'completed', item: { id: 'reply-' + thread, type: 'agentMessage', text: 'Fixture reply' } }], nextCursor: null };
        else if (frame.method === 'thread/turns/list') result = { data: [{ id: 'active-turn', status: selected.state === 'running' ? 'inProgress' : 'completed', items: [] }], nextCursor: null };
        else if (frame.method === 'gateway/questions') result = { questions: [] };
        else if (frame.method === 'turn/steer') result = { queued: true, turnId: 'active-turn' };
        else if (frame.method === 'model/list') result = { data: [] };
        else if (frame.method === 'thread/goal/get') result = { goal: null };
        else if (frame.method === 'account/rateLimits/read') result = { rateLimits: { limitId: 'codex', primary: { usedPercent: 25, windowDurationMins: 300 }, secondary: { usedPercent: 50, windowDurationMins: 10080 } } };
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    };
  }, { session, sessions });
  const select = async id => {
    if (width <= 650) await page.locator('#show-sessions').click();
    await page.locator('[data-session-id="' + id + '"]').click();
    await page.waitForSelector('#connection[data-state="connected"]');
  };
  const emitStatus = async (threadId, status, persist = true) => {
    await page.evaluate(({ threadId, status, persist }) => {
      if (persist) window.testStatuses[threadId] = status;
      window.testSockets.at(-1).emit({ method: 'thread/claudeStatus/updated', params: { threadId, claudeStatus: status } });
    }, { threadId, status, persist });
  };
  const known = { provider: 'claude', model: 'custom-reported[1m]', permissionMode: 'bypassPermissions', context: { usedTokens: 20000, maxTokens: 100000 }, cost: { sessionUSD: 1.23456, lastTurnUSD: 0.0123 }, balance: { remaining: 12.34, unit: 'USD', observedAt: 1793145600 } };
  const assertFits = async () => {
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true, 'Status line stays within the viewport');
    const bounds = await page.locator('#session-status').boundingBox();
    for (const id of ['model', 'permissions', 'turn-state', 'usage', 'claude-cost', 'claude-balance']) {
      const field = await page.locator('#' + id).boundingBox();
      assert.ok(field.x >= bounds.x - 1 && field.x + field.width <= bounds.x + bounds.width + 1, id + ' stays within the status line');
    }
  };
  await page.goto('https://webui.test/tgw/webui/');
  await select(session.session_id);
  assert.equal(await page.locator('#prompt').getAttribute('placeholder'), 'Ask Claude Code to do anything');
  assert.match(await page.locator('[data-item-id="reply-claude-thread"] .role').textContent(), /Claude Code/);
  assert.equal(await page.locator('.model-controls').isVisible(), true);
  assert.equal(await page.locator('#effort').isVisible(), false);
  assert.equal(await page.locator('#rate-limits').isVisible(), false);
  assert.equal(await page.locator('#show-goal').isVisible(), false);
  assert.equal(await page.locator('#model').textContent(), 'custom-reported[1m]');
  assert.equal(await page.locator('#permissions').textContent(), 'Allow edits');
  assert.match(await page.locator('#permissions').getAttribute('title'), /acceptEdits/);
  assert.equal(await page.locator('#usage').textContent(), '10% context');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $0.0012');
  assert.match(await page.locator('#claude-cost').getAttribute('title'), /reported cumulative session consumption.*0\.00123.*Provider billing can differ/);
  assert.equal(await page.locator('#claude-balance').textContent(), 'Credits —');
  assert.match(await page.locator('#claude-balance').getAttribute('title'), /has not reported/);
  assert.equal(await page.locator('#turn-state').textContent(), 'Working');
  assert.equal(await page.locator('#send').textContent(), 'Queue ↑');
  await assertFits();
  const methods = await page.evaluate(() => window.testSent.map(frame => frame.method));
  for (const unsupported of ['thread/goal/get', 'account/rateLimits/read', 'model/list']) assert.ok(!methods.includes(unsupported), unsupported + ' is not requested for Claude');
  await emitStatus('other-thread', known, false);
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $0.0012', 'Foreign thread status is ignored');
  await emitStatus(session.codex_thread_id, known);
  assert.equal(await page.locator('#permissions').textContent(), 'Bypass');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $1.23');
  assert.equal(await page.locator('#claude-balance').textContent(), 'Balance $12.34');
  assert.match(await page.locator('#claude-balance').getAttribute('aria-label'), /remaining account credit.*12\.34.*Updated/);
  await emitStatus(session.codex_thread_id, { ...known, cost: { sessionUSD: 0.00000001, lastTurnUSD: null }, balance: { remaining: 0, unit: 'credits', observedAt: 1793145600 } });
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session <$0.0001');
  assert.equal(await page.locator('#claude-balance').textContent(), '0 credits left', 'Reported zero credit is not unknown');
  await emitStatus(session.codex_thread_id, { ...known, cost: { sessionUSD: -1, lastTurnUSD: '7' }, context: { usedTokens: '12', maxTokens: 100000 }, balance: { remaining: '20', unit: 'USD' } }, false);
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session —');
  assert.equal(await page.locator('#usage').textContent(), 'Context —');
  assert.equal(await page.locator('#claude-balance').textContent(), 'Credits —');
  await emitStatus(session.codex_thread_id, { provider: 'claude', model: null, permissionMode: null, context: { usedTokens: null, maxTokens: null }, cost: { sessionUSD: null, lastTurnUSD: null }, balance: null }, false);
  assert.equal(await page.locator('#model').textContent(), 'Model —');
  assert.equal(await page.locator('#permissions').textContent(), 'Permissions —');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session —', 'Explicit unknown cost is not reported zero');
  await emitStatus(session.codex_thread_id, known);
  if (width <= 650) await page.locator('#show-sessions').click();
  await page.locator('[data-edit-session-id="claude-session"]').click();
  assert.equal(await page.locator('#edit-session-delete').isDisabled(), true);
  assert.equal(await page.locator('#edit-session-archive').isDisabled(), true);
  await page.locator('#edit-session-close').click();
  if (width <= 650) await page.locator('#close-sessions').click();
  await page.locator('#prompt').fill('Run this after the current turn.');
  await page.locator('#send').click();
  await page.waitForFunction(() => window.testSent.some(frame => frame.method === 'turn/steer'));
  await page.waitForFunction(() => document.getElementById('prompt').value === '');
  assert.equal(await page.locator('#steer-notice').textContent(), '✓ Message queued');
  await page.locator('#prompt').fill('/model');
  await page.locator('#send').click();
  await page.waitForFunction(() => document.getElementById('command-content').textContent.includes('admin console'));
  assert.equal((await page.evaluate(() => window.testSent.filter(frame => frame.method === 'gateway/command'))).length, 0);
  await page.locator('#close-commands').click();
  await page.locator('#prompt').fill('');
  await page.evaluate(() => { window.holdResumeThread = 'other-thread'; });
  if (width <= 650) await page.locator('#show-sessions').click();
  await page.locator('[data-session-id="claude-other"]').click();
  await page.waitForFunction(() => window.testSockets.at(-1)?.heldResume);
  assert.equal(await page.locator('#model').textContent(), 'Model —', 'Switch clears model before resume');
  assert.equal(await page.locator('#permissions').textContent(), 'Permissions —');
  assert.equal(await page.locator('#usage').textContent(), 'Context —');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session —');
  assert.equal(await page.locator('#claude-balance').textContent(), 'Credits —');
  const zero = { provider: 'claude', model: 'other-model', permissionMode: 'plan', context: { usedTokens: 0, maxTokens: 100000 }, cost: { sessionUSD: 0, lastTurnUSD: 0 }, balance: null };
  await emitStatus('other-thread', zero);
  await page.evaluate(() => { const socket = window.testSockets.at(-1); window.holdResumeThread = ''; socket.emit(socket.heldResume); delete socket.heldResume; });
  await page.waitForSelector('#connection[data-state="connected"]');
  assert.equal(await page.locator('#model').textContent(), 'other-model', 'Live update wins over delayed resume snapshot');
  assert.equal(await page.locator('#permissions').textContent(), 'Plan only');
  assert.equal(await page.locator('#usage').textContent(), '0% context');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $0.00');
  await select(session.session_id);
  await page.evaluate(status => window.testSockets.at(-2).emit({ method: 'thread/claudeStatus/updated', params: { threadId: 'claude-thread', claudeStatus: status } }), zero);
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $1.23', 'Old socket cannot replace restored status');
  await emitStatus(session.codex_thread_id, { ...known, model: 'reported-model-' + 'long-'.repeat(20), cost: { sessionUSD: 123456.78, lastTurnUSD: 12.34 }, balance: { remaining: 999999.99, unit: 'USD', observedAt: 1793145600 } });
  await assertFits();
  if (process.env.CLAUDE_STATUS_SCREENSHOTS) {
    fs.mkdirSync(process.env.CLAUDE_STATUS_SCREENSHOTS, { recursive: true });
    await page.locator('#composer').screenshot({ path: path.join(process.env.CLAUDE_STATUS_SCREENSHOTS, 'claude-status-' + width + '.png') });
  }
  await select(codex.session_id);
  assert.equal(await page.locator('#claude-spend').isVisible(), false);
  assert.equal(await page.locator('#effort').isVisible(), true);
  assert.equal(await page.locator('#model').textContent(), 'codex-model');
  assert.equal(await page.locator('#permissions').textContent(), 'On request');
  assert.equal(await page.locator('#usage').textContent(), '50% context');
  await page.waitForFunction(() => document.getElementById('rate-limits').textContent.includes('75% left'));
  assert.equal(await page.locator('#rate-limits').isVisible(), true);
  await select(session.session_id);
  await page.evaluate(() => window.testSockets.at(-1).close(1008));
  await page.waitForSelector('#connection[data-state="error"]');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session —');
  assert.equal(await page.locator('#claude-balance').textContent(), 'Credits —');
  assert.equal(await page.locator('#model').textContent(), 'Model —');
  assert.equal(await page.locator('#permissions').textContent(), 'Permissions —');
  assert.equal(await page.locator('#usage').textContent(), 'Context —');
  await page.locator('#reconnect').click();
  await page.waitForSelector('#connection[data-state="connected"]');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $123,456.78', 'Reconnect restores persisted session status');
  authenticated = false;
  await page.evaluate(() => window.testSockets.at(-1).close(4401));
  await page.waitForSelector('#auth:not([hidden])');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session —', 'Authentication loss clears status');
  assert.equal(await page.locator('#claude-balance').textContent(), 'Credits —');
  assert.equal(await page.locator('#claude-spend').isVisible(), false);
  assert.deepEqual(errors, []);
  await page.close();
}

async function deletionChecks(browser, width) {
  const page = await browser.newPage({ viewport: { width, height: 900 } });
  const currentWorker = { ...worker, SupportsSessionDeletion: false };
  const first = { ...session, session_id: 'delete-claude', codex_thread_id: 'delete-thread', name: 'Claude <img src=x onerror="window.injected=true">', cwd: '/projects/keep-files', state: 'idle' };
  const second = { ...first, session_id: 'delete-second', codex_thread_id: 'second-thread', name: 'Second conversation', cwd: '/projects/second-project' };
  const retained = { ...first, session_id: 'keep-claude', codex_thread_id: 'keep-thread', name: 'Keep conversation', cwd: '/projects/untouched' };
  let inventory = [first, second, retained];
  const errors = [], posts = [], commands = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.context().addCookies([{ name: '__Host-telegramgw-csrf', value: 'delete-csrf', url: 'https://delete.test/', secure: true, sameSite: 'Strict' }]);
  await page.route('https://delete.test/**', route => {
    const request = route.request(), url = new URL(request.url());
    const reply = body => route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, owner_id: 'delete-owner', session_id: 'delete-browser', expires_at: new Date(Date.now() + 28800000).toISOString(), server_time: new Date().toISOString() });
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return reply({ sessions: inventory, workers: [currentWorker], runtimes: [{ runtime_id: session.runtime_id, worker_id: worker.ID, state: 'running' }] });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], revision: 'delete-fixture' });
    if (url.pathname.endsWith('/push/config')) return reply({ supported: false, subscribed: false });
    if (url.pathname.endsWith('/sessions/delete') && request.method() === 'POST') {
      const payload = JSON.parse(request.postData());
      assert.equal(currentWorker.SupportsSessionDeletion, true, 'Unsupported workers cannot receive a browser deletion');
      assert.equal(request.headers()['x-csrf-token'], 'delete-csrf');
      assert.deepEqual(Object.keys(payload).sort(), ['confirmed', 'request_id', 'session_id'], 'Deletion cannot include project-removal flags or arbitrary paths');
      assert.equal(payload.confirmed, true);
      posts.push(payload);
      inventory = inventory.filter(row => row.session_id !== payload.session_id);
      return reply({ session_id: payload.session_id, command_id: payload.request_id, pending: false, deleted: true, status: 'completed', message: 'Claude conversation deleted. The project directory and all project files were kept.' });
    }
    if (url.pathname.startsWith('/tgw/api/')) return reply({});
    return staticReply(route, /\/(?:webui|admin)\/static\//.test(url.pathname) ? path.basename(url.pathname) : 'webui.html');
  });
  await page.exposeFunction('recordClaudeDelete', (selectedID, name, args) => {
    const target = inventory.find(row => row.session_id === selectedID);
    assert.equal(currentWorker.SupportsSessionDeletion, true);
    assert.equal(name, 'delete');
    assert.equal(args, 'confirm ' + target.codex_thread_id, 'Command confirmation is bound to the selected conversation');
    commands.push({ session_id: selectedID, name, args });
    inventory = inventory.filter(row => row.session_id !== selectedID);
    return { text: 'Claude conversation deleted. The project directory and all project files were kept.' };
  });
  await page.addInitScript(sessions => {
    window.deleteCommands = [];
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.readyState = 0; this.activity = String(url).includes('/activity');
        this.session = sessions.find(row => row.session_id === new URL(String(url)).searchParams.get('session_id'));
        setTimeout(() => { this.readyState = 1; this.onopen?.({}); this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: sessions.map(row => ({ session_id: row.session_id, state: 'idle', pending_questions: 0 })), telegram_bots: [], telegram_bots_revision: 'delete-fixture' } : { type: 'ready' }); }, 1);
      }
      emit(value) { this.onmessage?.({ data: JSON.stringify(value) }); }
      close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
      async send(raw) {
        const frame = JSON.parse(raw); if (!frame.method) return;
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: this.session.codex_thread_id } };
        else if (['thread/items/list', 'thread/turns/list'].includes(frame.method)) result = { data: [], nextCursor: null };
        else if (frame.method === 'gateway/questions') result = { questions: [] };
        else if (frame.method === 'gateway/command') {
          window.deleteCommands.push(frame.params);
          result = await window.recordClaudeDelete(this.session.session_id, frame.params.name, frame.params.args);
        }
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    };
  }, inventory);
  const sidebar = async () => { if (width <= 650) await page.locator('#show-sessions').click(); };
  const hideSidebar = async () => { if (width <= 650) await page.locator('#close-sessions').click(); };
  const select = async target => { await sidebar(); await page.locator('[data-session-id="' + target.session_id + '"]').click(); await page.waitForSelector('#connection[data-state="connected"]'); };
  const editor = async target => { await sidebar(); await page.locator('[data-edit-session-id="' + target.session_id + '"]').click(); };
  const refresh = async () => {
    const response = page.waitForResponse(response => response.url().endsWith('/webui/sessions'));
    await page.evaluate(() => document.getElementById('refresh-sessions').click());
    await response;
    await page.waitForFunction(() => !document.getElementById('refresh-sessions').disabled);
  };
  const slash = async command => { await page.locator('#prompt').fill(command); await page.locator('#send').click(); };
  const assertDescription = text => {
    assert.match(text, /permanently removes this Claude conversation’s files and its MACT session data/);
    assert.match(text, /project directory and all project files will be kept/);
    assert.doesNotMatch(text, /child sessions/);
  };
  await page.goto('https://delete.test/tgw/webui/');
  await select(first);
  await editor(first);
  assert.equal(await page.locator('#edit-session-delete').isDisabled(), true, 'Older Claude workers keep deletion disabled');
  assert.match(await page.locator('#edit-session-delete').getAttribute('title'), /Update this Claude Code worker/);
  assert.equal(await page.locator('#edit-session-archive').isDisabled(), true);
  assert.equal(await page.locator('#edit-session-delete-project').isDisabled(), true);
  await page.locator('#edit-session-close').click(); await hideSidebar();
  await slash('/delete');
  await page.waitForFunction(() => document.getElementById('command-content').textContent.includes('Update this Claude Code worker'));
  assert.equal(commands.length, 0); assert.equal(posts.length, 0);
  await page.locator('#close-commands').click();
  currentWorker.SupportsSessionDeletion = true;
  await refresh(); await editor(first);
  assert.equal(await page.locator('#edit-session-delete').isDisabled(), false);
  assert.equal(await page.locator('#edit-session-archive').isDisabled(), true, 'Enabling conversation deletion does not enable project archives');
  await page.locator('#edit-session-delete').click();
  assertDescription(await page.locator('#delete-session-description').textContent());
  assert.equal(await page.locator('#delete-session-name').textContent(), first.name);
  assert.equal(await page.locator('#delete-session-dialog img').count(), 0);
  assert.equal(await page.locator('#delete-session-cwd').textContent(), first.cwd);
  assert.equal(await page.evaluate(() => document.activeElement.id), 'delete-session-cancel', 'Destructive confirmation focuses Cancel');
  assert.equal(posts.length, 0, 'Opening confirmation does not delete');
  await page.locator('#delete-session-cancel').click();
  assert.equal(posts.length, 0, 'Cancel preserves the conversation');
  await page.locator('[data-edit-session-id="delete-claude"]').click(); await page.locator('#edit-session-delete').click();
  currentWorker.SupportsSessionDeletion = false; await refresh();
  assert.equal(await page.locator('#delete-session-confirm').isDisabled(), true, 'A capability change disables an existing confirmation');
  assert.equal(posts.length, 0);
  currentWorker.SupportsSessionDeletion = true; await refresh();
  assert.equal(await page.locator('#delete-session-confirm').isDisabled(), false);
  await page.locator('#delete-session-cancel').click(); await hideSidebar();
  await slash('/delete');
  await page.getByRole('button', { name: 'Delete conversation', exact: true }).waitFor();
  assertDescription(await page.locator('#command-content').textContent());
  assert.equal(commands.length, 0, 'Command deletion also requires explicit confirmation');
  await page.locator('#command-content').getByRole('button', { name: 'Cancel', exact: true }).click();
  assert.equal(commands.length, 0);
  await slash('/tgdeletesession');
  await page.getByRole('button', { name: 'Delete conversation', exact: true }).waitFor();
  currentWorker.SupportsSessionDeletion = false; await refresh();
  await page.getByRole('button', { name: 'Delete conversation', exact: true }).click();
  await page.waitForFunction(() => document.getElementById('command-content').textContent.includes('Update this Claude Code worker'));
  assert.equal(commands.length, 0, 'A stale command confirmation cannot bypass the capability check');
  await page.locator('#close-commands').click();
  currentWorker.SupportsSessionDeletion = true; await refresh();
  await slash('/delete');
  await page.getByRole('button', { name: 'Delete conversation', exact: true }).click();
  await page.waitForFunction(() => !document.querySelector('[data-session-id="delete-claude"]'));
  assert.equal(commands.length, 1); assert.equal(posts.length, 0);
  assert.equal(inventory.some(row => row.session_id === retained.session_id), true);
  await select(second); await editor(second); await page.locator('#edit-session-delete').click();
  assertDescription(await page.locator('#delete-session-description').textContent());
  await page.locator('#delete-session-confirm').click();
  await page.waitForFunction(() => !document.querySelector('[data-session-id="delete-second"]'));
  assert.equal(posts.length, 1); assert.equal(posts[0].session_id, second.session_id);
  assert.deepEqual(inventory.map(row => row.session_id), [retained.session_id], 'Deletion affects only the selected conversation');
  assert.equal(inventory[0].cwd, '/projects/untouched');
  assert.equal(await page.evaluate(() => window.injected), undefined);
  assert.deepEqual(errors, []);
  await page.close();
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    for (const width of [390, 1280]) await adminChecks(browser, width);
    for (const width of [320, 390, 768, 1280]) await webUIChecks(browser, width);
    for (const width of [390, 1280]) await deletionChecks(browser, width);
    process.stdout.write('Claude admin and WebUI checks passed.\n');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
