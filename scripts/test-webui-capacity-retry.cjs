#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');

(async () => {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1280, height: 850 } });
  context.setDefaultTimeout(10000);
  const pages = [], errors = [], requests = [], commands = [];
  let now = Date.now(), revision = 1, retry = null, cancelUnavailable = false;
  const message = 'The model is at capacity. <img src=x onerror="window.injected=true">';
  const sessions = ['first', 'second'].map(id => ({ session_id: id, codex_thread_id: 'thread-' + id,
    worker_id: id, worker_name: id, runtime_id: id, runtime_name: 'Codex', name: id, cwd: '/projects/' + id, state: 'idle' }));
  const rows = sessions.map(s => ({ session_id: s.session_id, state: 'idle', active_turn_id: '', pending_questions: 0 }));
  const row = rows[0];
  const broadcast = () => Promise.all(pages.filter(p => !p.isClosed()).map(p => p.evaluate(rows => {
    if (!window.testActivity) return;
    for (const row of rows) window.testActivity.emit({ type: 'activity_event', version: 3,
      sequence: ++window.testActivity.sequence, revision: window.testActivity.sequence + 1, event: 'session_changed', session_id: row.session_id, session: row });
  }, rows)));
  const fail = async (capacity = true) => {
    retry = null;
    Object.assign(row, { state: 'idle', active_turn_id: '', error_message: capacity ? message : 'The usage quota is exhausted.',
      error_revision: 'failure-' + revision++, error_turn_id: '', capacity_error: capacity,
      capacity_retry_state: '', capacity_retry_revision: '', capacity_retry_turn_id: '', capacity_retry_at_ms: 0 });
    await broadcast();
  };
  await fail();
  await context.route('https://webui.test/**', async route => {
    const url = new URL(route.request().url());
    const reply = (body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, owner_id: 'owner', session_id: 'login',
      expires_at: new Date(now + 28800000).toISOString(), server_time: new Date(now).toISOString() });
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return reply({ sessions, workers: [], runtimes: [] });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], revision: '1' });
    if (url.pathname.endsWith('/push/config')) return reply({ supported: false, subscribed: false });
    if (url.pathname === '/fixture/state') return reply({ rows });
    if (url.pathname.endsWith('/sessions/capacity-retry')) {
      const input = route.request().postDataJSON(); requests.push(input);
      assert.equal(route.request().method(), 'POST');
      assert.equal(input.session_id, 'first', 'Countdown keeps its original session');
      assert.equal(typeof route.request().headers()['x-csrf-token'], 'string');
      if (input.error_revision !== row.error_revision && input.error_revision !== retry?.error_revision) return reply({}, 409);
      if (input.action === 'cancel' && cancelUnavailable) return reply({}, 409);
      if (!retry) retry = { error_revision: input.error_revision, turn_id: '', state: 'waiting', due_at_ms: now + 30000 };
      if (input.action === 'cancel') { retry.state = 'cancelled'; row.error_message = ''; row.error_revision = ''; }
      Object.assign(row, { capacity_retry_state: retry.state, capacity_retry_revision: retry.error_revision,
        capacity_retry_turn_id: '', capacity_retry_at_ms: retry.due_at_ms });
      await reply({ retry, server_time_ms: now });
      await broadcast(); return;
    }
    const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html',
      body: fs.readFileSync(path.join(assets, filename)) });
  });
  const makePage = async () => {
    const page = await context.newPage(); pages.push(page);
    page.on('pageerror', error => errors.push(error.message));
    await page.clock.install({ time: new Date(now) });
    await page.addInitScript(() => {
      window.testSent = []; window.testActivity = null;
      window.WebSocket = class {
        static OPEN = 1;
        constructor(url) {
          this.url = String(url); this.session = new URL(url).searchParams.get('session_id'); this.sequence = 0;
          this.activity = this.url.includes('/activity'); this.readyState = 0;
          if (this.activity) window.testActivity = this;
          setTimeout(async () => {
            const state = await (await fetch('/fixture/state')).json(); this.readyState = 1;
            this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1,
              sessions: state.rows, telegram_bots: [], telegram_bots_revision: '1' } : { type: 'ready' });
          }, 1);
        }
        emit(frame) { this.onmessage?.({ data: JSON.stringify(frame) }); }
        close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
        send(raw) {
          const frame = JSON.parse(raw); if (!frame.method) return;
          window.testSent.push({ session: this.session, ...frame }); let result = {};
          if (frame.method === 'thread/resume') result = { thread: { id: frame.params.threadId }, model: 'codex' };
          else if (frame.method === 'thread/turns/list') result = { data: [{ id: 'old-failure', status: 'failed', error: { message: 'An old saved capacity failure.' }, items: [] }], nextCursor: null };
          else if (frame.method === 'thread/items/list') result = { data: [], nextCursor: null };
          else if (frame.method === 'gateway/questions') result = { questions: [] };
          else if (frame.method === 'model/list') result = { data: [] };
          else if (frame.method === 'thread/goal/get') result = { goal: null };
          else if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
          else if (frame.method === 'turn/start') result = { turn: { id: 'manual-turn', status: 'inProgress', items: [] } };
          setTimeout(() => this.emit({ id: frame.id, result }), 1);
        }
      };
    });
    await page.goto('https://webui.test/tgw/webui/'); return page;
  };
  const choose = async (page, id) => {
    await page.locator('[data-session-id="' + id + '"]').click();
    await page.waitForSelector('#connection[data-state="connected"]');
  };
  const countdown = page => page.locator('#capacity-retry-countdown');
  const advance = async ms => {
    now += ms;
    for (const page of pages.filter(p => !p.isClosed())) await page.clock.fastForward(ms);
    // The real registry dispatcher is tested separately. This fixture delivers
    // its shared state transition, never a browser-generated turn/start call.
    if (retry?.state === 'waiting' && retry.due_at_ms <= now) {
      retry.state = row.capacity_retry_state = 'submitted';
      commands.push({ text: 'Continue', session: 'first', revision: retry.error_revision });
      await broadcast();
    }
  };
  try {
    const first = await makePage(); await choose(first, 'first');
    await first.waitForFunction(() => document.querySelector('#capacity-retry-countdown').textContent === 'Retrying in 30 seconds');
    assert.equal(await first.locator('#notice').textContent(), message);
    assert.equal(await first.locator('#notice img').count(), 0);
    await advance(1000);
    assert.equal(await countdown(first).textContent(), 'Retrying in 29 seconds');
    await first.locator('#capacity-retry-cancel').click();
    await first.waitForFunction(() => document.querySelector('#notice').hidden);
    assert.equal(await first.locator('#capacity-retry').isHidden(), true);
    await advance(40000); assert.equal(commands.length, 0, 'Cancel never submits Continue');
    await first.reload(); await choose(first, 'first');
    assert.equal(await first.locator('#notice').isHidden(), true, 'Cancelled failures stay dismissed after reloading saved history');

    await fail(); await first.waitForFunction(() => !document.querySelector('#capacity-retry').hidden);
    await advance(10000);
    const deadline = retry.due_at_ms;
    const second = await makePage(); await choose(second, 'first');
    await second.waitForFunction(() => document.querySelector('#capacity-retry-countdown').textContent.startsWith('Retrying in 20'));
    assert.equal(retry.due_at_ms, deadline, 'A second browser cannot reset the deadline');
    await broadcast(); assert.equal(retry.due_at_ms, deadline, 'Repeated error snapshots do not reset the deadline');
    await first.locator('#prompt').fill('My unsent draft');
    await choose(first, 'second');
    assert.equal(await first.locator('#capacity-retry').isHidden(), true, 'Countdown belongs only to its session');
    await advance(20000);
    assert.deepEqual(commands.map(c => [c.text, c.session]), [['Continue', 'first']]);
    assert.equal(await second.locator('#notice').isHidden(), true, 'Submitted retry dismisses the shared failure');
    await choose(first, 'first');
    assert.equal(await first.locator('#prompt').inputValue(), 'My unsent draft', 'Automatic retry does not consume a draft');
    const sent = await first.evaluate(() => window.testSent.filter(f => f.method === 'turn/start'));
    assert.equal(sent.length, 0, 'Browsers never submit a second automatic turn');
    await advance(31000); assert.equal(commands.length, 1);
    await second.close();

    await fail(); await first.waitForFunction(() => !document.querySelector('#capacity-retry').hidden);
    await first.locator('#prompt').fill('A different request');
    cancelUnavailable = true;
    await first.locator('#send').click();
    await first.waitForFunction(() => document.querySelector('#notice').textContent.includes('Your draft has been kept'));
    assert.equal(await first.locator('#prompt').inputValue(), 'A different request');
    assert.equal((await first.evaluate(() => window.testSent.filter(f => f.method === 'turn/start'))).length, 0);
    cancelUnavailable = false;
    await first.locator('#send').click();
    await first.waitForFunction(() => document.querySelector('#prompt').value === '');
    assert.equal(retry.state, 'cancelled', 'Manual input cancels the old countdown before dispatch');
    assert.equal((await first.evaluate(() => window.testSent.filter(f => f.method === 'turn/start'))).length, 1);
    await advance(31000); assert.equal(commands.length, 1);
    // End the fixture's manual turn before presenting another terminal failure.
    await first.evaluate(() => window.testActivity.emit({ type: 'activity_event', version: 3,
      sequence: ++window.testActivity.sequence, revision: window.testActivity.sequence + 1, event: 'turn_ended', session_id: 'first',
      session: { session_id: 'first', state: 'idle', active_turn_id: '', pending_questions: 0 } }));
    const schedules = requests.filter(r => r.action === 'schedule').length;
    await fail(false);
    assert.equal(await first.locator('#capacity-retry').isHidden(), true, 'Quota and other terminal errors are never auto-retried');
    assert.equal(requests.filter(r => r.action === 'schedule').length, schedules);

    await first.reload(); await choose(first, 'first');
    await fail(); await first.waitForFunction(() => !document.querySelector('#capacity-retry').hidden);
    for (const width of [320, 390, 1280]) {
      await first.setViewportSize({ width, height: 850 });
      await first.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      assert.equal(await first.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'Countdown fits at ' + width + 'px');
      assert.equal(await first.locator('#capacity-retry-cancel').isVisible(), true);
    }
    await first.locator('#capacity-retry-cancel').focus(); await first.keyboard.press('Enter');
    await first.waitForFunction(() => document.querySelector('#notice').hidden);
    assert.deepEqual(errors, []);
    console.log('Web UI model-capacity countdown, cancellation and shared-session checks passed.');
  } catch (error) {
    console.error(JSON.stringify({ requests, retry, errors, ui: await pages[0].evaluate(() => ({ notice: document.querySelector('#notice').textContent, sent: window.testSent.slice(-5), countdown: document.querySelector('#capacity-retry-countdown').textContent, overflow: [...document.querySelectorAll('body *')].filter(e => e.getBoundingClientRect().right > innerWidth + 1).slice(0, 12).map(e => [e.id, e.tagName, e.getBoundingClientRect().right]) })) }, null, 2));
    throw error;
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
