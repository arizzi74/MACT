#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 850 } });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const sessions = ['first', 'second'].map(id => ({ session_id: id, codex_thread_id: 'thread-' + id, worker_id: id,
    worker_name: id, runtime_id: id, runtime_name: 'Codex', name: id, cwd: '/projects/' + id, state: 'idle' }));
  const capacity = 'The model is at capacity. Please try again later. <img src=x onerror="window.injected=true">';
  await page.route('https://webui.test/**', route => {
    const url = new URL(route.request().url());
    const reply = body => route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, owner_id: 'owner', session_id: 'login', expires_at: new Date(Date.now() + 28800000).toISOString(), server_time: new Date().toISOString() });
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return reply({ sessions, workers: [], runtimes: [] });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], revision: '1' });
    if (url.pathname.endsWith('/push/config')) return reply({ supported: false, subscribed: false });
    const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(({ sessions, capacity }) => {
    window.testSockets = []; window.testActivity = null; window.testSent = []; window.testHistory = {};
    window.testRows = sessions.map(s => ({ session_id: s.session_id, state: 'idle', active_turn_id: '', pending_questions: 0,
      ...(s.session_id === 'second' ? { error_turn_id: '', error_message: capacity, error_revision: '1:10', error_code: 'model_capacity' } : {}) }));
    window.testTurns = { first: { id: 'old-first', status: 'completed', items: [] }, second: { id: 'failed-second', status: 'failed', error: { message: 'An older failed turn must not hide the newer prompt rejection.' }, items: [] } };
    window.testActivityEvent = (id, changes, event = 'session_changed') => {
      const row = window.testRows.find(r => r.session_id === id); Object.assign(row, changes);
      const socket = window.testActivity;
      socket.emit({ type: 'activity_event', version: 3, sequence: ++socket.sequence, revision: socket.sequence + 1, event, session_id: id, session: row });
    };
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.readyState = 0; this.url = String(url); this.session = new URL(url).searchParams.get('session_id');
        this.activity = this.url.includes('/activity'); this.sequence = 0;
        if (this.activity) window.testActivity = this; else window.testSockets.push(this);
        setTimeout(() => {
          this.readyState = 1;
          this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: window.testRows, telegram_bots: [], telegram_bots_revision: '1' } : { type: 'ready' });
        }, 1);
      }
      emit(message) { this.onmessage?.({ data: JSON.stringify(message) }); }
      close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
      send(raw) {
        const frame = JSON.parse(raw); if (!frame.method) return;
        window.testSent.push({ session: this.session, ...frame });
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: frame.params.threadId }, model: 'codex' };
        else if (frame.method === 'thread/turns/list') result = { data: [window.testTurns[this.session]], nextCursor: null };
        else if (frame.method === 'thread/items/list') result = { data: window.testHistory[this.session] || [], nextCursor: null };
        else if (frame.method === 'gateway/questions') result = { questions: [] };
        else if (frame.method === 'model/list') result = { data: [] };
        else if (frame.method === 'thread/goal/get') result = { goal: null };
        else if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
        else if (frame.method === 'turn/start') {
          if (window.testRejectStart) {
            window.testRejectStart = false;
            setTimeout(() => this.emit({ id: frame.id, error: { code: -32000, message: 'Model capacity rejected the prompt before starting.' } }), 1);
            return;
          }
          const turn = { id: 'new-' + this.session, status: 'inProgress', items: [] };
          window.testTurns[this.session] = turn;
          this.emit({ method: 'turn/started', params: { threadId: frame.params.threadId, turn } });
          result = { turn };
        }
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    };
  }, { sessions, capacity });
  const choose = async id => {
    await page.locator('[data-session-id="' + id + '"]').click();
    await page.waitForSelector('#connection[data-state="connected"]');
  };
  const current = message => page.evaluate(value => window.testSockets.at(-1).emit(value), message);
  const notice = () => page.locator('#notice').textContent();
  try {
    await page.goto('https://webui.test/tgw/webui/');
    await choose('first');
    assert.equal(await page.locator('#notice').isHidden(), true, 'Another session’s error stays isolated');
    await choose('second');
    assert.equal(await notice(), capacity, 'Previously unseen terminal error comes from the durable snapshot');
    assert.equal(await page.locator('#notice img').count(), 0, 'Error text is never interpreted as HTML');
    assert.equal(await page.evaluate(() => window.injected), undefined);
    await current({ type: 'error', message: 'Temporary connection warning.' });
    assert.equal(await notice(), capacity + '\n\nTemporary connection warning.', 'A transport warning cannot hide the retained terminal error');
    await choose('first'); await choose('second');
    assert.equal(await notice(), capacity, 'Switching sessions retains the failure');
    await page.locator('#disconnect').click();
    await page.locator('#reconnect').click();
    await page.waitForSelector('#connection[data-state="connected"]');
    assert.equal(await notice(), capacity, 'Reconnect/bootstrap does not dismiss a model failure');
    await page.reload(); await choose('second');
    assert.equal(await notice(), capacity, 'A fresh page restores the gateway error');
    assert.equal(await page.evaluate(() => JSON.stringify(localStorage).includes('at capacity')), false, 'Error text is not saved in browser storage');

    await choose('first');
    await page.evaluate(() => window.testActivityEvent('second', { error_turn_id: 'later-second', error_message: 'A second attempt reached capacity.', error_revision: '1:15' }, 'turn_ended'));
    await choose('second');
    assert.equal(await notice(), 'A second attempt reached capacity.', 'New failure while viewing another session replaces the previous failure');
    await page.locator('#prompt').fill('Try this request again.'); await page.locator('#send').click();
    await page.waitForFunction(() => document.querySelector('#prompt').value === '');
    assert.equal(await page.locator('#notice').isHidden(), true, 'Sending a new prompt dismisses the old failure');
    await page.evaluate(() => window.testActivityEvent('second', {}, 'session_changed'));
    assert.equal(await page.locator('#notice').isHidden(), true, 'A delayed snapshot cannot resurrect an acknowledged error');
    await page.evaluate(() => window.testActivityEvent('second', { state: 'running', active_turn_id: 'new-second', error_turn_id: '', error_message: '', error_revision: '' }, 'turn_started'));

    const retry = () => current({ method: 'error', params: { threadId: 'thread-second', turnId: 'new-second', willRetry: true, error: { message: 'Reconnecting upstream…' } } });
    const itemEvent = (method, item, timing = {}) => current({ method, params: { threadId: 'thread-second', turnId: 'new-second', item, ...timing } });
    await itemEvent('item/started', { id: 'old-running-tool', type: 'commandExecution', command: 'sleep 5', status: 'inProgress' });
    await retry();
    assert.match(await notice(), /retrying/);
    await itemEvent('item/completed', { id: 'old-running-tool', type: 'commandExecution', command: 'sleep 5', status: 'completed', aggregatedOutput: 'An earlier tool finished.' });
    assert.match(await notice(), /retrying/, 'Completion of a pre-retry tool cannot prove provider recovery');
    await itemEvent('item/started', { id: 'old-running-tool', type: 'commandExecution', command: 'sleep 5' });
    assert.match(await notice(), /retrying/, 'A repeated lifecycle start cannot dismiss the retry');
    await itemEvent('item/started', { id: 'old-replayed-tool', type: 'dynamicToolCall', tool: 'exec_command' }, { startedAtMs: Date.now() - 60000 });
    assert.match(await notice(), /retrying/, 'A delayed old tool start cannot dismiss the retry');
    await current({ method: 'thread/status/changed', params: { threadId: 'thread-second', status: { type: 'active' } } });
    assert.match(await notice(), /retrying/, 'Generic active status is not proof of resumed provider output');
    await itemEvent('item/started', { id: 'fresh-tool-call', type: 'dynamicToolCall', tool: 'exec_command', arguments: { cmd: 'go test ./...' } });
    assert.equal(await page.locator('#notice').isHidden(), true, 'A new live tool call clears a stale provider retry before final model output');
    await retry();
    await itemEvent('item/completed', { id: 'old-replayed-output', type: 'reasoning', summary: ['Older reasoning.'] }, { completedAtMs: Date.now() - 60000 });
    assert.match(await notice(), /retrying/, 'Old completed reasoning cannot dismiss a later retry');
    await current({ type: 'error', message: 'A later worker connection error remains active.' });
    await itemEvent('item/started', { id: 'fresh-tool-after-connection-error', type: 'dynamicToolCall', tool: 'exec_command' });
    assert.equal(await notice(), 'A later worker connection error remains active.', 'Fresh provider work cannot dismiss a later unrelated connection error');
    await retry();
    await page.evaluate(() => { window.testHistory.second = [{ turnId: 'new-second', item: { id: 'restored-old-reasoning', type: 'reasoning', summary: ['Restored older reasoning.'], status: 'completed' }, completedAtMs: Date.now() - 60000 }]; });
    await page.locator('#disconnect').click(); await page.locator('#reconnect').click();
    await page.waitForSelector('#connection[data-state="connected"]');
    assert.match(await notice(), /retrying/, 'Restoring history does not itself resolve an upstream retry');
    await itemEvent('item/completed', { id: 'restored-old-reasoning', type: 'reasoning', summary: ['Restored older reasoning.'] });
    assert.match(await notice(), /retrying/, 'Replay of an already restored complete item cannot prove recovery');
    await itemEvent('item/started', { id: 'fresh-command-after-restore', type: 'commandExecution', command: 'git status', status: 'inProgress' });
    assert.equal(await page.locator('#notice').isHidden(), true, 'Fresh live work after history restore clears only the stale retry');
    await retry();
    await current({ method: 'item/completed', params: { threadId: 'thread-second', turnId: 'new-second', item: { id: 'recovered', type: 'agentMessage', text: 'Recovered.' } } });
    assert.equal(await page.locator('#notice').isHidden(), true, 'Successful output still clears transient retries');
    await current({ method: 'turn/completed', params: { threadId: 'thread-second', turn: { id: 'new-second', status: 'failed', error: { message: 'The provider rejected this request.' } } } });
    assert.equal(await notice(), 'The provider rejected this request.');
    await itemEvent('item/started', { id: 'late-tool-after-terminal-error', type: 'dynamicToolCall', tool: 'exec_command' });
    assert.equal(await notice(), 'The provider rejected this request.', 'Fresh tool activity cannot dismiss a terminal provider failure');
    await page.evaluate(() => window.testActivityEvent('second', { state: 'idle', active_turn_id: '' }, 'turn_ended'));
    assert.equal(await notice(), 'The provider rejected this request.', 'An empty activity frame racing a live error cannot erase it');
    await choose('first'); await choose('second');
    assert.equal(await notice(), 'The provider rejected this request.', 'Native-only errors survive navigation before gateway persistence');
    await page.evaluate(() => window.testActivityEvent('second', { state: 'running', active_turn_id: 'telegram-turn' }, 'turn_started'));
    assert.equal(await page.locator('#notice').isHidden(), true, 'A new prompt from Telegram also clears the old error');
    await choose('first');
    for (let attempt = 0; attempt < 2; attempt++) {
      await page.evaluate(() => { window.testRejectStart = true; });
      await page.locator('#prompt').fill('Try a new prompt ' + attempt); await page.locator('#send').click();
      await page.waitForFunction(() => document.querySelector('#notice').textContent === 'Model capacity rejected the prompt before starting.');
      await choose('second'); await choose('first');
      assert.equal(await notice(), 'Model capacity rejected the prompt before starting.', 'A definitive RPC rejection persists, including repeated equal errors');
    }
    assert.deepEqual(errors, []);
    console.log('Web UI persistent session error checks passed.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
