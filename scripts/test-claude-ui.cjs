#!/usr/bin/env node
// Isolated browser checks: no live worker, credentials or model requests.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = path.resolve(__dirname, '../internal/admin/static');
const secret = 'private-claude-browser-test-token';
const budgetSecret = 'private-enterprise-browser-test-key';
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
  const value = { provider: 'claude', model: 'custom[1m]', base_url: 'http://localhost:8080', auth_mode: 'auth_token', permission_mode: 'default', reasoning_effort: 'high', supported_permission_modes: ['default', 'acceptEdits', 'plan', 'dontAsk', 'auto', 'bypassPermissions'], api_key_configured: true, enterprise_budget: { enabled: false, user_id: '', api_key_configured: false } };
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
      if (fail) return reply({ code: 'runtime_settings_unavailable', message: 'Unsafe worker failure ' + secret + budgetSecret }, 503);
      Object.assign(value, { model: update.model, base_url: update.base_url, auth_mode: update.auth_mode, permission_mode: update.permission_mode, reasoning_effort: update.reasoning_effort });
      if (update.enterprise_budget) Object.assign(value.enterprise_budget, { enabled: update.enterprise_budget.enabled, user_id: update.enterprise_budget.user_id, ...('api_key' in update.enterprise_budget ? { api_key_configured: !!update.enterprise_budget.api_key } : {}) });
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
  assert.equal(await page.locator('#worker-runtime-reasoning').inputValue(), 'high');
  await page.locator('#worker-runtime-reasoning').selectOption('xhigh');
  assert.equal(await page.locator('#worker-runtime-permissions option[value=auto]').isDisabled(), false);
  await page.locator('#worker-runtime-save').click();
  await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.startsWith('Saved.'));
  assert.equal(Object.hasOwn(updates[0], 'api_key'), false, 'Blank retains the credential');
  assert.equal(updates[0].reasoning_effort, 'xhigh');
  assert.equal(Object.hasOwn(updates[0].enterprise_budget, 'api_key'), false, 'Blank retains the separate budget key');
  await page.locator('#worker-runtime-key').fill(secret);
  await page.locator('#worker-runtime-permissions').selectOption('bypassPermissions');
  assert.equal(await page.locator('#worker-runtime-bypass').isVisible(), true);
  await page.locator('#worker-runtime-save').click();
  await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.startsWith('Saved.'));
  assert.equal(updates[1].api_key, secret);
  assert.equal(await page.locator('#worker-runtime-key').inputValue(), '');
  await page.locator('#worker-runtime-clear').check();
  assert.equal(await page.locator('#worker-runtime-key').isDisabled(), true);
  await page.locator('#worker-runtime-reasoning').selectOption('');
  await page.locator('#worker-runtime-auth').selectOption('login');
  await page.locator('#worker-runtime-save').click();
  await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.startsWith('Saved.'));
  assert.equal(updates[2].api_key, '', 'Remove explicitly clears the credential');
  await page.locator('#worker-runtime-url').fill('');
  await page.locator('#worker-runtime-budget-enabled').check();
  await page.locator('#worker-runtime-budget-user').fill('user_Enterprise123');
  await page.locator('#worker-runtime-budget-key').fill(budgetSecret);
  await page.locator('#worker-runtime-save').click();
  await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.startsWith('Saved.'));
  assert.deepEqual(updates[3].enterprise_budget, { enabled: true, user_id: 'user_Enterprise123', api_key: budgetSecret });
  assert.equal(await page.locator('#worker-runtime-budget-key').inputValue(), '');
  assert.match(await page.locator('#worker-runtime-budget-key-help').textContent(), /Admin API key is saved/);
  await page.locator('#worker-runtime-budget-clear').check();
  assert.equal(await page.locator('#worker-runtime-budget-key').isDisabled(), true);
  await page.locator('#worker-runtime-budget-enabled').uncheck();
  await page.locator('#worker-runtime-save').click();
  await page.waitForFunction(() => document.getElementById('worker-runtime-status').textContent.startsWith('Saved.'));
  assert.equal(updates[4].enterprise_budget.api_key, '', 'Budget key removal is explicit');
  assert.equal(updates[4].enterprise_budget.enabled, false);
  assert.equal(await page.locator('#worker-runtime-budget-clear').isVisible(), true, 'A saved budget key can be removed while budget reads are disabled');
  await page.locator('#worker-runtime-budget-enabled').check();
  fail = true;
  await page.locator('#worker-runtime-key').fill(secret);
  await page.locator('#worker-runtime-budget-key').fill(budgetSecret);
  await page.locator('#worker-runtime-save').click();
  await page.locator('#worker-runtime-error').waitFor({ state: 'visible' });
  assert.equal(await page.locator('#worker-runtime-key').inputValue(), '');
  assert.equal(await page.locator('#worker-runtime-budget-key').inputValue(), '');
  assert.ok(!(await page.locator('body').innerText()).includes(budgetSecret));
  assert.ok(!(await page.locator('body').innerText()).includes(secret), 'Worker errors never echo a credential');
  await page.locator('#worker-runtime-key').fill(secret);
  await page.locator('#worker-runtime-budget-key').fill(budgetSecret);
  await page.locator('#worker-runtime-close').click();
  assert.equal(await page.locator('#worker-runtime-budget-key').inputValue(), '');
  assert.equal(await page.locator('#worker-runtime-key').inputValue(), '');
  const storage = await page.evaluate(() => JSON.stringify([Object.entries(localStorage), Object.entries(sessionStorage), history.state, location.href]));
  assert.ok(!storage.includes(secret), 'Credential never enters browser storage or history');
  assert.ok(!logs.join('\n').includes(secret), 'Credential never enters console logs');
  assert.ok(!storage.includes(budgetSecret), 'Budget key never enters browser storage or history');
  assert.ok(!logs.join('\n').includes(budgetSecret), 'Budget key never enters console logs');
  value.supported_permission_modes = ['default', 'acceptEdits', 'plan', 'bypassPermissions'];
  await page.getByRole('button', { name: 'Claude Code settings', exact: true }).click();
  await page.waitForFunction(() => !document.getElementById('worker-runtime-fields').disabled);
  assert.equal(await page.locator('#worker-runtime-permissions option[value=auto]').evaluate(option => option.disabled), true, 'Unadvertised auto mode cannot be selected');
  assert.equal(await page.locator('#worker-runtime-permissions option[value=dontAsk]').evaluate(option => option.disabled), true);
  await page.locator('#worker-runtime-close').click();
  assert.deepEqual(errors, []);
  await page.close();
}

async function webUIChecks(browser, width, usageOnly = false, costOnly = false) {
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
    window.testHoldSettings = false; window.testDeferredSettings = []; window.testHoldCommandAck = false;
    const unknown = { provider: 'claude', model: null, reasoningEffort: null, permissionMode: null, context: { usedTokens: null, maxTokens: null }, cost: { sessionUSD: null, lastTurnUSD: null }, balance: null };
    window.testHoldInitialModels = true;
    window.testModels = [{ id: 'sonnet', model: 'sonnet', resolvedModel: 'native-sonnet', displayName: 'Native Sonnet', supportedReasoningEfforts: [{ reasoningEffort: 'low', description: 'Native low effort' }, { reasoningEffort: 'high', description: 'Native high effort' }, { reasoningEffort: 'xhigh', description: 'Native extra high effort' }] }];
    window.testStatuses = {
      [session.codex_thread_id]: { provider: 'claude', model: 'custom-reported[1m]', reasoningEffort: 'high', permissionMode: 'acceptEdits', context: { usedTokens: 10000, maxTokens: 100000 }, cost: { sessionUSD: 0.00123, lastTurnUSD: 0.00123 }, balance: null },
      'other-thread': unknown,
    };
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.url = String(url); this.readyState = 0; this.activity = this.url.includes('/activity');
        this.session = sessions.find(row => row.session_id === new URL(this.url).searchParams.get('session_id'));
        if (!this.activity) window.testSockets.push(this);
        else { window.testActivitySocket = this; this.settingsSequence = 0; }
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
        let result = {}, settingsMutation = false;
        if (frame.method === 'thread/resume') {
          result = thread === 'codex-thread' ? { thread: { id: thread }, model: 'codex-model', reasoningEffort: 'high', sandbox: { type: 'workspaceWrite' }, approvalPolicy: 'on-request', approvalsReviewer: 'user' }
            : { thread: { id: thread, claudeStatus: structuredClone(window.testStatuses[thread]) } };
          if (window.holdResumeThread === thread) { this.heldResume = { id: frame.id, result }; return; }
        } else if (frame.method === 'thread/items/list') result = { data: [{ turnId: 'active-turn', turnStatus: selected.state === 'running' ? 'inProgress' : 'completed', item: { id: 'reply-' + thread, type: 'agentMessage', text: 'Fixture reply' } }], nextCursor: null };
        else if (frame.method === 'thread/turns/list') result = { data: [{ id: 'active-turn', status: selected.state === 'running' ? 'inProgress' : 'completed', items: [] }], nextCursor: null };
        else if (frame.method === 'gateway/questions') result = { questions: [] };
        else if (frame.method === 'turn/steer') result = { queued: true, turnId: 'active-turn' };
        else if (frame.method === 'model/list') { result = { data: structuredClone(window.testModels) }; if (window.testHoldInitialModels) { window.testHoldInitialModels = false; this.heldInitialModels = { id: frame.id, result }; return; } }
        else if (frame.method === 'gateway/command') {
          const { name, args } = frame.params;
          const native = window.testStatuses[thread];
          let settings;
          if (name === 'compact') result = { text: 'Compaction started. Claude Code will report when it finishes.', state: 'running' };
          if (name === 'model' && !args) result = { model_menu: { options: [{ label: 'Native Sonnet', args: '--menu sonnet' }, { label: 'Cancel', args: '--cancel' }] } };
          else if (name === 'model' && args === '--menu sonnet') result = { model_menu: { options: [{ label: 'Native Sonnet · high', args: 'sonnet high' }, { label: 'Native Sonnet · low', args: 'sonnet low' }, { label: 'Back to models', args: '--menu' }] } };
          else if (name === 'model') { const [model, effort = ''] = args.split(/\s+/); settings = { model: model === 'sonnet' ? 'native-sonnet' : model, effort }; result = { text: 'Model changed.' }; }
          else if (name === 'reasoning' || name === 'effort') { settings = { effort: ['default', 'auto'].includes(args) ? '' : args }; result = { text: 'Reasoning changed.' }; }
          else if (name === 'permissions' && !args) result = { permissions: { options: ['default', 'acceptEdits', 'plan', 'dontAsk', 'auto', 'bypassPermissions'].map(mode => ({ id: mode, label: { default: 'Ask before protected actions', acceptEdits: 'Allow file edits', plan: 'Plan only', dontAsk: 'Do not ask', auto: 'Automatic permissions', bypassPermissions: 'Bypass permission checks' }[mode], current: native.permissionMode === mode })) } };
          else if (name === 'permissions' && args === 'bypassPermissions') result = { permissions: { options: [{ id: 'confirm-bypass-permissions', label: 'Confirm bypass permissions', description: 'Tools and commands can run without permission prompts.' }, { id: 'cancel', label: 'Cancel' }] } };
          else if (name === 'permissions') { settings = { permissionMode: args === 'confirm-bypass-permissions' ? 'bypassPermissions' : args }; result = { text: 'Permissions changed.' }; }
          if (settings) {
            settingsMutation = true;
            Object.assign(native, { ...('model' in settings ? { model: settings.model } : {}), ...('effort' in settings ? { reasoningEffort: settings.effort } : {}), ...('permissionMode' in settings ? { permissionMode: settings.permissionMode } : {}) });
            const update = { method: 'thread/settings/updated', params: { threadId: thread, threadSettings: settings } };
            if (window.testHoldSettings) window.testDeferredSettings.push({ socket: this, message: update });
            else setTimeout(() => this.emit(update), 0);
          }
        }
        else if (frame.method === 'thread/goal/get') result = { goal: null };
        else if (frame.method === 'account/rateLimits/read') result = { rateLimits: { limitId: 'codex', primary: { usedPercent: 25, windowDurationMins: 300 }, secondary: { usedPercent: 50, windowDurationMins: 10080 } } };
        if (settingsMutation && window.testHoldCommandAck) { this.heldCommandAck = { id: frame.id, result }; return; }
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
  const publishSettings = async (model, effort) => {
    await page.evaluate(({ sessionId, threadId, model, effort }) => {
      const socket = window.testActivitySocket, sequence = ++socket.settingsSequence;
      Object.assign(window.testStatuses[threadId], { model, reasoningEffort: effort });
      socket.emit({ type: 'activity_event', version: 3, sequence, revision: sequence + 1, event: 'session_settings_changed', session_id: sessionId,
        session: { session_id: sessionId, state: 'running', pending_questions: 0, settings_revision: '1:' + sequence, model, reasoning_effort: effort } });
    }, { sessionId: session.session_id, threadId: session.codex_thread_id, model, effort });
  };
  const known = { provider: 'claude', model: 'custom-reported[1m]', reasoningEffort: 'high', permissionMode: 'bypassPermissions', context: { usedTokens: 20000, maxTokens: 100000 }, cost: { sessionUSD: 1.23456, lastTurnUSD: 0.0123 }, balance: { remaining: 12.34, unit: 'USD', observedAt: 1793145600 } };
  const assertFits = async () => {
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true, 'Status line stays within the viewport');
    const bounds = await page.locator('#session-status').boundingBox();
    for (const id of ['model', 'effort', 'permissions', 'turn-state', 'usage', 'claude-cost', 'claude-balance', 'claude-budget']) {
      const field = await page.locator('#' + id).boundingBox();
      if (!field) continue;
      assert.ok(field.x >= bounds.x - 1 && field.x + field.width <= bounds.x + bounds.width + 1, id + ' stays within the status line');
    }
  };
  await page.goto('https://webui.test/tgw/webui/');
  await select(session.session_id);
  if (costOnly) {
    const observedAt = Math.floor(Date.now() / 1000);
    const partial = { ...known, cost: { sessionUSD: null, lastTurnUSD: null, knownSessionUSD: 23.85, incomplete: true, reportedSessionUSD: 0.25, observedAt } };
    await emitStatus(session.codex_thread_id, partial);
    assert.equal(await page.locator('#claude-cost').textContent(), 'Session $0.25 (partial)', 'The live native counter is visible below the historical high-water mark');
    assert.match(await page.locator('#claude-cost').getAttribute('title'), /Current native session estimate: \$0\.25.*Historical lower bound: at least \$23\.85.*overlapping figures are not added.*Checked/);
    const growing = { ...partial, cost: { ...partial.cost, reportedSessionUSD: 1.5, observedAt: observedAt + 15 } };
    await emitStatus(session.codex_thread_id, growing);
    assert.equal(await page.locator('#claude-cost').textContent(), 'Session $1.50 (partial)', 'An active turn can update its cost before the result');
    await emitStatus(other.codex_thread_id, { ...known, cost: { reportedSessionUSD: 999 } }, false);
    assert.equal(await page.locator('#claude-cost').textContent(), 'Session $1.50 (partial)', 'Another session cannot replace the selected cost');
    await select(other.session_id); await select(session.session_id);
    assert.equal(await page.locator('#claude-cost').textContent(), 'Session $1.50 (partial)', 'Switching sessions restores the latest observation');
    await emitStatus(session.codex_thread_id, { ...partial, cost: { ...partial.cost, reportedSessionUSD: 0 } });
    assert.equal(await page.locator('#claude-cost').textContent(), 'Session $0.00 (partial)', 'A reported zero stays different from unknown');
    await emitStatus(session.codex_thread_id, { ...partial, cost: { ...partial.cost, reportedSessionUSD: -1 } });
    assert.equal(await page.locator('#claude-cost').textContent(), 'Session ≥$23.85 (partial)', 'Invalid live values cannot replace older evidence');
    await emitStatus(session.codex_thread_id, { ...known, cost: { sessionUSD: 2, lastTurnUSD: .5, reportedSessionUSD: 2.75, observedAt } });
    assert.equal(await page.locator('#claude-cost').textContent(), 'Session $2.75', 'Live local control advances a complete session between results');
    await assertFits();
    assert.deepEqual(errors, []);
    await page.close();
    return;
  }
  if (usageOnly) {
    const observedAt = Math.floor(Date.now() / 1000) - 600, nextAttemptAt = Math.floor(Date.now() / 1000) + 1800;
    const fresh = { ...known, balance: null, usage: { plan: 'enterprise', observedAt, windows: [], apiSpend: { enabled: true, currency: 'USD', period: 'monthly', limit: 500, spent: 125, remaining: 375 } } };
    await emitStatus(session.codex_thread_id, fresh);
    assert.equal(await page.locator('#claude-budget').textContent(), 'API spend $375.00 / $500.00 left');
    const cached = { ...fresh, accountRefresh: { usage: { lastAttemptAt: observedAt + 600, nextAttemptAt, failed: true, rateLimited: true } } };
    await emitStatus(session.codex_thread_id, cached);
    assert.equal(await page.locator('#claude-budget').textContent(), 'API spend $375.00 / $500.00 left (last known)');
    assert.match(await page.locator('#claude-budget').getAttribute('title'), /Checked.*Refresh rate limited.*previous successful report.*Next refresh/);
    await emitStatus(session.codex_thread_id, { ...cached, accountRefresh: { usage: { lastAttemptAt: observedAt + 600, nextAttemptAt, failed: true } } });
    assert.match(await page.locator('#claude-budget').textContent(), /375\.00.*last known/);
    await select(other.session_id); await select(session.session_id);
    assert.match(await page.locator('#claude-budget').textContent(), /375\.00.*last known/, 'Reconnect preserves worker account cache and its age');
    await emitStatus(session.codex_thread_id, { ...fresh, usage: { ...fresh.usage, observedAt: observedAt + 700, apiSpend: { ...fresh.usage.apiSpend, spent: 150, remaining: 350 } }, accountRefresh: { usage: { lastAttemptAt: observedAt + 700, nextAttemptAt, failed: false } } });
    assert.equal(await page.locator('#claude-budget').textContent(), 'API spend $350.00 / $500.00 left');
    await emitStatus(session.codex_thread_id, { ...known, balance: null, usage: null, accountRefresh: { usage: { lastAttemptAt: observedAt + 700, nextAttemptAt, failed: true } } });
    assert.equal(await page.locator('#claude-budget').textContent(), 'API spend unavailable');
    assert.match(await page.locator('#claude-budget').getAttribute('title'), /Next refresh/);
    await assertFits();
    assert.deepEqual(errors, []);
    await page.close();
    return;
  }
  assert.equal(await page.locator('#prompt').getAttribute('placeholder'), 'Ask Claude Code to do anything');
  assert.match(await page.locator('[data-item-id="reply-claude-thread"] .role').textContent(), /Claude Code/);
  assert.equal(await page.locator('.model-controls').isVisible(), true);
  assert.equal(await page.locator('#effort').isVisible(), true);
  assert.equal(await page.locator('#effort').textContent(), 'High');
  assert.equal(await page.locator('#rate-limits').isVisible(), false);
  assert.equal(await page.locator('#show-goal').isVisible(), false);
  assert.equal(await page.locator('#model').textContent(), 'custom-reported[1m]');
  assert.equal(await page.locator('#permissions').textContent(), 'Accept edits');
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
  for (const unsupported of ['thread/goal/get', 'account/rateLimits/read']) assert.ok(!methods.includes(unsupported), unsupported + ' is not requested for Claude');
  assert.ok(methods.includes('model/list'), 'Claude reads native model capabilities');
  await page.waitForFunction(() => !!window.testSockets.at(-1).heldInitialModels);
  await page.evaluate(() => {
    window.testModels.push({ id: 'custom-reported[1m]', model: 'custom-reported[1m]', displayName: 'New account model', supportedReasoningEfforts: [{ reasoningEffort: 'medium', description: 'New account supported effort' }] });
    window.testSockets.at(-1).emit({ method: 'thread/settings/updated', params: { threadId: 'claude-thread', threadSettings: { permissionMode: 'acceptEdits' } } });
  });
  await page.locator('#effort').click();
  await page.getByRole('button', { name: /Medium/ }).waitFor();
  assert.equal(await page.getByRole('button', { name: /Medium/ }).count(), 1, 'Worker settings changes refresh advertised reasoning choices');
  await page.locator('#close-commands').click();
  await page.evaluate(() => { const socket = window.testSockets.at(-1); socket.emit(socket.heldInitialModels); delete socket.heldInitialModels; });
  await page.locator('#effort').click();
  assert.equal(await page.getByRole('button', { name: /Medium/ }).count(), 1, 'An older initial model-list response cannot replace a refreshed account catalog');
  await page.locator('#close-commands').click();
  await emitStatus('other-thread', known, false);
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $0.0012', 'Foreign thread status is ignored');
  await emitStatus(session.codex_thread_id, known);
  assert.equal(await page.locator('#permissions').textContent(), 'Bypass permissions');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $1.23');
  assert.equal(await page.locator('#claude-balance').textContent(), 'Balance $12.34');
  assert.match(await page.locator('#claude-balance').getAttribute('aria-label'), /remaining account credit.*12\.34.*Updated/);
  const nativeUsage = { plan: 'enterprise', observedAt: Math.floor(Date.now() / 1000), windows: [],
    apiSpend: { enabled: true, currency: 'USD', period: 'monthly', limit: 500, spent: 123.45, remaining: 376.55 } };
  await emitStatus(session.codex_thread_id, { ...known, balance: null, usage: nativeUsage,
    enterpriseBudget: { limit: 100, spent: 25, remaining: 75, currency: 'USD', period: 'monthly', unlimited: false, observedAt: nativeUsage.observedAt } });
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend $376.55 / $500.00 left', 'Native /usage supplies the account allocation without an Admin key and takes priority over fallback');
  assert.match(await page.locator('#claude-budget').getAttribute('title'), /Claude Code \/usage.*Monthly API spend.*123\.45/);
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $1.23', 'Account spend never replaces session cost');
  await assertFits();
  await emitStatus(session.codex_thread_id, { ...known, balance: null, usage: { ...nativeUsage, apiSpend: { ...nativeUsage.apiSpend, limit: null, spent: null, remaining: null } } });
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend unavailable', 'Unknown native amounts never become a zero or unlimited allowance');
  await emitStatus(session.codex_thread_id, { ...known, balance: null, usage: { ...nativeUsage, apiSpend: { ...nativeUsage.apiSpend, limit: 0, spent: 0, remaining: 0, enabled: false } } });
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend $0.00 / $0.00 left');
  assert.match(await page.locator('#claude-budget').getAttribute('title'), /Extra usage is disabled/);
  await emitStatus(session.codex_thread_id, { ...known, balance: null, usage: { ...nativeUsage, apiSpend: null, windows: [{ name: 'Current session', usedPercent: 24, resetsAt: Math.floor(Date.now() / 1000) + 3600 }] } });
  assert.equal(await page.locator('#claude-limits').textContent(), 'Current session 24% used');
  assert.match(await page.locator('#claude-limits').getAttribute('title'), /resets/);
  await emitStatus(session.codex_thread_id, known);
  assert.equal(await page.locator('#claude-limits').isVisible(), false, 'Failed or unavailable native observations clear the prior plan windows');
  await emitStatus(session.codex_thread_id, { ...known, enterpriseBudget: { limit: 100, spent: 25, remaining: 75, currency: 'USD', period: 'monthly', unlimited: false, observedAt: 1793145600 } }, false);
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend $75.00 / $100.00 left');
  assert.match(await page.locator('#claude-budget').getAttribute('title'), /monthly member limit.*Spent.*\$25.00.*Checked/);
  assert.ok(!(await page.locator('#claude-budget').getAttribute('title')).includes('reset'), 'No reset date is invented');
  await assertFits();
  await emitStatus(session.codex_thread_id, { ...known, enterpriseBudget: { limit: null, spent: 25, remaining: null, currency: 'USD', period: 'monthly', unlimited: true, observedAt: 1793145600 } }, false);
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend unlimited');
  await emitStatus(session.codex_thread_id, known, false);
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend unavailable', 'An unavailable budget snapshot clears the last known allowance');
  await emitStatus(session.codex_thread_id, { ...known, enterpriseBudget: { limit: 0, spent: 0, remaining: 0, currency: 'USD', period: 'monthly', unlimited: false } }, false);
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend $0.00 / $0.00 left', 'Reported zero remains a known budget');
  await emitStatus(session.codex_thread_id, { ...known, enterpriseBudget: { limit: 100, spent: 25, remaining: '75', currency: 'USD', period: 'monthly', unlimited: false } }, false);
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend unavailable', 'Invalid budget is unknown');
  await emitStatus(session.codex_thread_id, { ...known, cost: { sessionUSD: 0.00000001, lastTurnUSD: null }, balance: { remaining: 0, unit: 'credits', observedAt: 1793145600 } });
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session <$0.0001');
  assert.equal(await page.locator('#claude-balance').textContent(), '0 credits left', 'Reported zero credit is not unknown');
  await emitStatus(session.codex_thread_id, { ...known, cost: { sessionUSD: -1, lastTurnUSD: '7' }, context: { usedTokens: '12', maxTokens: 100000 }, balance: { remaining: '20', unit: 'USD' } }, false);
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session —');
  assert.equal(await page.locator('#usage').textContent(), 'Context —');
  assert.equal(await page.locator('#claude-balance').textContent(), 'Credits —');
  await emitStatus(session.codex_thread_id, { ...known, cost: { sessionUSD: null, lastTurnUSD: null, knownSessionUSD: 23.85, incomplete: true } }, false);
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session ≥$23.85 (partial)', 'Known partial accounting is labelled, not hidden or presented as a full total');
  assert.match(await page.locator('#claude-cost').getAttribute('title'), /reset or lost.*At least.*23.85/);
  await assertFits();
  await emitStatus(session.codex_thread_id, { ...known, cost: { sessionUSD: 999, lastTurnUSD: 1, incomplete: true } }, false);
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session incomplete', 'Incomplete accounting never becomes an exact cost without lower-bound evidence');
  await emitStatus(session.codex_thread_id, { provider: 'claude', model: null, reasoningEffort: null, permissionMode: null, context: { usedTokens: null, maxTokens: null }, cost: { sessionUSD: null, lastTurnUSD: null }, balance: null }, false);
  assert.equal(await page.locator('#model').textContent(), 'Model —');
  assert.equal(await page.locator('#permissions').textContent(), 'Permissions —');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session —', 'Explicit unknown cost is not reported zero');
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend unavailable');
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
  await page.locator('#prompt').fill('/compact');
  await page.waitForFunction(() => document.querySelector('#command-suggestions .command-option')?.title.includes('active turn'));
  await page.locator('#close-commands').click();
  await page.locator('#prompt').fill('');
  await page.locator('#model').click();
  await page.getByRole('button', { name: /Native Sonnet/, exact: false }).click();
  await page.getByRole('button', { name: /Native Sonnet · high/ }).click();
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#model').textContent(), 'Native Sonnet');
  assert.equal(await page.locator('#effort').textContent(), 'High');
  await page.locator('#effort').click();
  assert.equal(await page.getByRole('button', { name: /Extra high/ }).count(), 1, 'Canonical model status resolves alias effort metadata');
  assert.equal(await page.getByRole('button', { name: /Max/ }).count(), 0, 'Unadvertised max is not offered');
  await page.getByRole('button', { name: /Extra high/ }).click();
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#effort').textContent(), 'Extra high');
  await publishSettings('native-sonnet', 'xhigh');
  await page.evaluate(() => { window.testHoldSettings = true; });
  await page.locator('#effort').click();
  await page.getByRole('button', { name: /Auto \(model default\)/ }).click();
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#effort').textContent(), 'Auto', 'Acknowledged reasoning default supersedes unchanged prior activity before a native broadcast');
  assert.equal(await page.evaluate(() => window.testDeferredSettings.length), 1, 'Native reasoning broadcast is still held');
  await page.evaluate(() => { for (const update of window.testDeferredSettings.splice(0)) update.socket.emit(update.message); window.testHoldSettings = false; });
  await page.locator('#effort').click();
  await page.getByRole('button', { name: /Extra high/ }).click();
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#effort').textContent(), 'Extra high');
  await publishSettings('native-sonnet', 'xhigh');
  await page.evaluate(() => { window.testHoldSettings = true; });
  await page.locator('#model').click();
  await page.getByRole('button', { name: /Custom model…/ }).click();
  await page.getByLabel('Model ID or alias').fill('provider/custom[1m]');
  await page.getByRole('button', { name: 'Use model', exact: true }).click();
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#model').textContent(), 'provider/custom[1m]');
  assert.equal(await page.locator('#effort').textContent(), 'Auto', 'Acknowledged model without effort supersedes unchanged prior activity and clears old effort before its native broadcast');
  assert.equal(await page.evaluate(() => window.testDeferredSettings.length), 1, 'Native model broadcast is still held');
  await page.evaluate(() => { for (const update of window.testDeferredSettings.splice(0)) update.socket.emit(update.message); window.testHoldCommandAck = true; });
  await page.locator('#prompt').fill('/model older-ack-model low');
  await page.locator('#send').click();
  await page.waitForFunction(() => !!window.testSockets.at(-1).heldCommandAck);
  await publishSettings('newer-activity-model', 'medium');
  await page.evaluate(() => { const socket = window.testSockets.at(-1); socket.emit(socket.heldCommandAck); delete socket.heldCommandAck; window.testDeferredSettings = []; });
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#model').textContent(), 'newer-activity-model', 'A newer activity revision wins over an older acknowledged model');
  assert.equal(await page.locator('#effort').textContent(), 'Medium');
  await page.locator('#prompt').fill('/model older-status-ack-model low');
  await page.locator('#send').click();
  await page.waitForFunction(() => !!window.testSockets.at(-1).heldCommandAck);
  await emitStatus(session.codex_thread_id, { ...known, model: 'newer-live-model', reasoningEffort: 'high' });
  await page.evaluate(() => { const socket = window.testSockets.at(-1); socket.emit(socket.heldCommandAck); delete socket.heldCommandAck; window.testDeferredSettings = []; window.testHoldSettings = false; window.testHoldCommandAck = false; });
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#model').textContent(), 'newer-live-model', 'An older model acknowledgement cannot replace a newer authoritative Claude status');
  assert.equal(await page.locator('#effort').textContent(), 'High');
  assert.equal(await page.locator('#permissions').isDisabled(), true, 'Permission mutations wait for the active turn');
  await page.evaluate(() => window.testSockets.at(-1).emit({ method: 'turn/completed', params: { threadId: 'claude-thread', turn: { id: 'active-turn', status: 'completed' } } }));
  await page.evaluate(() => window.testSockets.at(-1).emit({ method: 'turn/completed', params: { threadId: 'claude-thread', turn: { id: 'active-turn', status: 'completed' } } }));
  await page.waitForFunction(() => document.getElementById('turn-state').textContent === 'Ready');
  await page.locator('#prompt').fill('/compact');
  await page.locator('#prompt').press('Enter');
  await page.waitForFunction(() => window.testSent.some(frame => frame.method === 'gateway/command' && frame.params.name === 'compact'));
  await page.getByText('Compaction started. Claude Code will report when it finishes.', { exact: true }).waitFor();
  await page.locator('#close-commands').click();
  await page.evaluate(() => {
    const socket = window.testSockets.at(-1), threadId = 'claude-thread', turnId = 'compact-turn';
    socket.emit({ method: 'turn/started', params: { threadId, turn: { id: turnId, status: 'inProgress' } } });
    socket.emit({ method: 'item/started', params: { threadId, turnId, item: { id: 'claude-compact', type: 'contextCompaction', status: 'inProgress' } } });
  });
  await page.locator('[data-item-id="claude-compact"]').getByText('Compacting context…', { exact: true }).waitFor();
  for (const [status, label] of [['skipped', 'Context compaction skipped'], ['failed', 'Context compaction failed'], ['interrupted', 'Context compaction interrupted'], ['completed', 'Context compacted']]) {
    await page.evaluate(status => window.testSockets.at(-1).emit({ method: 'item/completed', params: { threadId: 'claude-thread', turnId: 'compact-turn', item: { id: 'claude-compact', type: 'contextCompaction', status } } }), status);
    await page.locator('[data-item-id="claude-compact"]').getByText(label, { exact: true }).waitFor();
  }
  await page.evaluate(() => window.testSockets.at(-1).emit({ method: 'turn/completed', params: { threadId: 'claude-thread', turn: { id: 'compact-turn', status: 'completed' } } }));
  await page.locator('#permissions').click();
  await page.getByRole('button', { name: /Automatic permissions/ }).click();
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#permissions').textContent(), 'Auto');
  await page.locator('#permissions').click();
  await page.getByRole('button', { name: /Bypass permission checks/ }).click();
  await page.getByRole('button', { name: /Confirm bypass permissions/ }).waitFor();
  assert.equal(await page.locator('#permissions').textContent(), 'Auto', 'Bypass waits for confirmation');
  await page.waitForFunction(() => document.activeElement?.classList.contains('command-choice') && document.activeElement.textContent.includes('Cancel'));
  const bypassCancel = page.getByRole('button', { name: /Cancel/ });
  assert.equal(await bypassCancel.getAttribute('data-active'), 'true', 'Cancel is the initial keyboard selection for Claude bypass confirmation');
  await bypassCancel.press('Escape');
  await page.getByRole('button', { name: /Automatic permissions/ }).waitFor();
  assert.equal(await page.getByRole('button', { name: /Confirm bypass permissions/ }).count(), 0, 'Escape returns to permission choices');
  assert.equal(await page.locator('#permissions').textContent(), 'Auto', 'Returning to choices cannot enable bypass');
  await page.getByRole('button', { name: /Bypass permission checks/ }).click();
  await page.waitForFunction(() => document.activeElement?.classList.contains('command-choice') && document.activeElement.textContent.includes('Cancel'));
  await page.keyboard.press('Enter');
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#permissions').textContent(), 'Auto', 'Enter on the default confirmation choice cancels bypass');
  await page.locator('#permissions').click();
  await page.getByRole('button', { name: /Bypass permission checks/ }).click();
  await page.getByRole('button', { name: /Confirm bypass permissions/ }).click();
  await page.waitForFunction(() => document.getElementById('command-panel').hidden);
  assert.equal(await page.locator('#permissions').textContent(), 'Bypass permissions');
  await emitStatus(session.codex_thread_id, known);
  await page.locator('#prompt').fill('');
  await page.evaluate(() => { window.holdResumeThread = 'other-thread'; });
  if (width <= 650) await page.locator('#show-sessions').click();
  await page.locator('[data-session-id="claude-other"]').click();
  await page.waitForFunction(() => window.testSockets.at(-1)?.heldResume);
  assert.equal(await page.locator('#model').textContent(), 'Model —', 'Switch clears model before resume');
  assert.equal(await page.locator('#effort').textContent(), 'Effort —');
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend —');
  assert.equal(await page.locator('#permissions').textContent(), 'Permissions —');
  assert.equal(await page.locator('#usage').textContent(), 'Context —');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session —');
  assert.equal(await page.locator('#claude-balance').textContent(), 'Credits —');
  await page.evaluate(() => window.testSockets.at(-1).emit({ method: 'thread/settings/updated', params: { threadId: 'other-thread', threadSettings: { model: 'native-loading-model', effort: 'low', permissionMode: 'auto' } } }));
  assert.equal(await page.locator('#model').textContent(), 'native-loading-model');
  assert.equal(await page.locator('#effort').textContent(), 'Low');
  const zero = { provider: 'claude', model: 'other-model', reasoningEffort: 'high', permissionMode: 'plan', context: { usedTokens: 0, maxTokens: 100000 }, cost: { sessionUSD: 0, lastTurnUSD: 0 }, balance: null };
  await emitStatus('other-thread', zero);
  await page.evaluate(() => { const socket = window.testSockets.at(-1); window.holdResumeThread = ''; socket.emit(socket.heldResume); delete socket.heldResume; });
  await page.waitForSelector('#connection[data-state="connected"]');
  assert.equal(await page.locator('#model').textContent(), 'other-model', 'Live update wins over delayed resume snapshot');
  assert.equal(await page.locator('#effort').textContent(), 'High', 'Settings seen during loading cannot replay over a newer live status');
  assert.equal(await page.locator('#permissions').textContent(), 'Plan mode');
  assert.equal(await page.locator('#usage').textContent(), '0% context');
  assert.equal(await page.locator('#claude-cost').textContent(), 'Session $0.00');
  await page.evaluate(() => window.testSockets.at(-1).emit({ method: 'thread/settings/updated', params: { threadId: 'other-thread', threadSettings: { model: 'sonnet', effort: 'low', permissionMode: 'plan' } } }));
  await emitStatus('other-thread', { provider: 'claude', model: null, reasoningEffort: null, permissionMode: null }, false);
  await page.locator('#model').click();
  await page.getByRole('button', { name: /Native Sonnet/ }).waitFor();
  assert.equal(await page.locator('#command-content [aria-current=true]').count(), 0, 'Unknown live settings cannot mark a stale inventory model as current');
  await page.locator('#close-commands').click();
  await emitStatus('other-thread', zero);
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
  assert.equal(await page.locator('#claude-budget').textContent(), 'API spend —');
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
    if (process.argv.includes('--cost-only')) {
      for (const width of [390, 1280]) await webUIChecks(browser, width, false, true);
      process.stdout.write('Claude session cost browser checks passed.\n');
      return;
    }
    if (process.argv.includes('--usage-only')) {
      for (const width of [390, 1280]) await webUIChecks(browser, width, true);
      process.stdout.write('Claude usage refresh browser checks passed.\n');
      return;
    }
    for (const width of [390, 1280]) await adminChecks(browser, width);
    for (const width of [320, 390, 768, 1280]) await webUIChecks(browser, width);
    for (const width of [390, 1280]) await deletionChecks(browser, width);
    process.stdout.write('Claude admin and WebUI checks passed.\n');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
