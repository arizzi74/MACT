#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const screenshots = process.env.SCREENSHOT_DIR || '/tmp/mact-worker-activity-screenshots';

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 850 }, reducedMotion: 'reduce' });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  let workers = [
    { ID: 'alpha', Name: 'Alpha worker', Connectivity: 'connected' },
    { ID: 'beta', Name: 'Beta worker', Connectivity: 'connected' },
    { ID: 'empty', Name: 'Empty worker', Connectivity: 'connected' },
  ];
  const session = (id, name, extra = {}) => ({ session_id: id, codex_thread_id: 'thread-' + id,
    worker_id: 'alpha', worker_name: 'Alpha worker', runtime_name: 'Codex', name, cwd: '/projects/' + id, state: 'idle', ...extra });
  let sessions = [
    session('working', 'Working sibling', { state: 'running', active_turn_id: 'turn-working' }),
    session('idle', 'Quiet project'),
    session('both', 'Working with questions', { state: 'running', active_turn_id: 'turn-both', pending_questions: 2 }),
    session('question', 'Waiting sibling', { pending_questions: 1 }),
    session('archived', 'Archived work', { archived: true, state: 'running', active_turn_id: 'turn-archived' }),
    session('deleted', 'Deleted work', { deleted: true, state: 'running', active_turn_id: 'turn-deleted' }),
    session('beta-idle', 'Beta quiet', { worker_id: 'beta', worker_name: 'Beta worker' }),
  ];
  await page.route('https://workers.test/**', route => {
    const url = new URL(route.request().url());
    const json = body => route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
    if (url.pathname.endsWith('/admin/session')) return json({ authenticated: true, owner_id: 'owner', session_id: 'login', server_time: new Date().toISOString(), expires_at: new Date(Date.now() + 28800000).toISOString() });
    if (url.pathname.endsWith('/login/options')) return json({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return json({ sessions, workers, runtimes: [] });
    if (url.pathname.endsWith('/telegram-bots')) return json({ bots: [], revision: '1' });
    if (url.pathname.endsWith('/push/config')) return json({ supported: false, subscribed: false });
    const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(sessions => {
    window.workerRows = sessions.map(session => ({ session_id: session.session_id, state: session.state, active_turn_id: session.active_turn_id || '', pending_questions: session.pending_questions || 0 }));
    window.workerEvent = (id, changes, event = 'turn_started') => {
      const row = window.workerRows.find(row => row.session_id === id); Object.assign(row, changes);
      const socket = window.workerActivity;
      socket.emit({ type: 'activity_event', version: 3, sequence: ++socket.sequence, revision: socket.sequence + 1, event, session_id: id, session: row });
    };
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.readyState = 0; this.sequence = 0;
        if (!String(url).includes('/activity')) throw new Error('Sidebar activity must not open a conversation');
        window.workerActivity = this;
        setTimeout(() => {
          this.readyState = 1;
          this.emit({ type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: window.workerRows, telegram_bots: [], telegram_bots_revision: '1' });
        }, 1);
      }
      emit(message) { this.onmessage?.({ data: JSON.stringify(message) }); }
      close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
      send() {}
    };
  }, sessions);
  const group = id => page.locator('.session-worker[data-worker-id="' + id + '"]');
  const alpha = group('alpha'), toggle = alpha.locator('.session-group-toggle');
  const snapshot = () => alpha.evaluate(element => {
    const heading = element.querySelector('.session-group'), toggle = element.querySelector('.session-group-toggle');
    const active = element.querySelector('.session-group-active-count'), total = element.querySelector('.session-group-count');
    return { kind: heading.dataset.activity, collapsed: heading.dataset.collapsed, expanded: toggle.getAttribute('aria-expanded'), active: active.textContent,
      activeHidden: active.hidden, total: total.textContent, label: toggle.getAttribute('aria-label'), color: getComputedStyle(element.querySelector('.worker-name')).color,
      border: getComputedStyle(heading).borderLeftColor, activeBeforeTotal: active.nextElementSibling === total };
  });
  const event = (id, changes, type) => page.evaluate(({ id, changes, type }) => window.workerEvent(id, changes, type), { id, changes, type });
  const waitKind = kind => page.waitForFunction(kind => document.querySelector('.session-worker[data-worker-id="alpha"] .session-group')?.dataset.activity === kind, kind);
  try {
    await page.goto('https://workers.test/tgw/webui/');
    await waitKind('question');
    assert.equal(await alpha.locator('.session-button').count(), 4, 'Archived and deleted sessions do not contribute rows or counts');
    let current = await snapshot();
    assert.equal(current.total, '4'); assert.equal(current.active, '3', 'A working session with questions counts once');
    assert.equal(current.expanded, 'true'); assert.equal(current.activeHidden, false); assert.equal(current.activeBeforeTotal, true);
    assert.match(current.label, /4 sessions · 3 active sessions/);
    const normalColor = current.color, normalBorder = current.border;
    const questionColor = await alpha.locator('[data-session-id="question"] .name').evaluate(element => getComputedStyle(element).color);
    assert.notEqual(normalColor, questionColor, 'Expanded worker headings keep their normal color');
    await toggle.click();
    current = await snapshot();
    assert.equal(current.collapsed, 'true'); assert.equal(current.color, questionColor); assert.equal(current.border, questionColor);
    assert.equal(await alpha.locator('.session-group-sessions').isHidden(), true);
    assert.equal(await page.evaluate(() => JSON.parse(localStorage.getItem('mact-webui-worker-collapsed')).alpha), true);
    assert.equal(await group('beta').locator('.session-group-active-count').isHidden(), true, 'Idle siblings have no active badge');
    assert.equal(await group('empty').locator('.session-group-count').textContent(), '0');
    assert.equal(await group('empty').locator('.session-group-active-count').isHidden(), true, 'Empty workers have no active badge');
    fs.mkdirSync(screenshots, { recursive: true });
    await page.screenshot({ path: path.join(screenshots, 'collapsed-question-desktop.png') });

    await event('both', { pending_questions: 0 }, 'question_resolved');
    assert.equal((await snapshot()).kind, 'question', 'One waiting sibling takes priority over all working siblings');
    await event('question', { pending_questions: 0 }, 'question_resolved'); await waitKind('running');
    current = await snapshot();
    const runningColor = await alpha.locator('[data-session-id="working"] .name').evaluate(element => getComputedStyle(element).color);
    assert.equal(current.active, '2'); assert.equal(current.color, runningColor); assert.equal(current.border, runningColor);
    await toggle.click();
    assert.equal((await snapshot()).color, normalColor, 'Expanding an active worker clears the aggregate tint');
    assert.equal((await snapshot()).border, normalBorder);
    await toggle.click();
    await event('working', { state: 'idle', active_turn_id: '' }, 'turn_ended');
    assert.equal((await snapshot()).active, '1');
    await event('both', { state: 'idle', active_turn_id: '' }, 'turn_ended'); await waitKind('idle');
    current = await snapshot();
    assert.equal(current.active, '0'); assert.equal(current.activeHidden, true); assert.equal(current.color, normalColor); assert.equal(current.border, normalBorder);
    assert.doesNotMatch(current.label, /active sessions/);
    await event('idle', { pending_questions: 1 }, 'question_requested'); await waitKind('question');
    assert.equal((await snapshot()).active, '1', 'A waiting session is active even with no running turn');
    assert.match((await snapshot()).label, /1 active session$/);
    await page.locator('#refresh-sessions').click();
    await page.waitForFunction(() => !document.querySelector('#refresh-sessions').disabled);
    assert.equal((await snapshot()).collapsed, 'true', 'Inventory refresh preserves collapse and live activity');
    assert.equal((await snapshot()).active, '1');
    await page.reload(); await waitKind('question');
    assert.equal((await snapshot()).collapsed, 'true', 'Collapsed worker preference survives reload');
    assert.equal((await snapshot()).active, '3');

    await page.locator('#session-search').fill('Quiet project');
    current = await snapshot();
    assert.equal(current.expanded, 'true', 'Search reveals matching sessions');
    assert.equal(current.total, '1'); assert.equal(current.activeHidden, true, 'Active count follows the same search results as the total');
    assert.match(current.label, /1 matching session$/);
    await toggle.click(); assert.equal((await snapshot()).collapsed, 'true');
    await page.locator('#session-search').fill('Waiting sibling');
    assert.equal((await snapshot()).active, '1'); assert.equal((await snapshot()).expanded, 'true', 'Changing search resets only the temporary search collapse');
    await toggle.click(); assert.equal((await snapshot()).color, questionColor);
    await page.locator('#session-search').fill('Nothing matches');
    assert.equal((await snapshot()).total, '0'); assert.equal((await snapshot()).activeHidden, true);
    await page.locator('#session-search').fill('');
    assert.equal((await snapshot()).collapsed, 'true', 'Search does not overwrite the saved collapse preference');
    assert.equal((await snapshot()).active, '3');

    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 844 });
      await page.locator('#show-sessions').click();
      assert.equal(await toggle.isVisible(), true);
      const layout = await alpha.evaluate(element => {
        const badge = element.querySelector('.session-group-active-count').getBoundingClientRect(), count = element.querySelector('.session-group-count').getBoundingClientRect();
        const row = element.getBoundingClientRect();
        return { before: badge.right <= count.left && count.left - badge.right <= 10, fits: row.left >= 0 && row.right <= innerWidth && element.scrollWidth <= element.clientWidth };
      });
      assert.equal(layout.before, true, 'Active number sits immediately left of total at ' + width + 'px');
      assert.equal(layout.fits, true, 'Worker heading and badges fit at ' + width + 'px');
      await page.screenshot({ path: path.join(screenshots, 'collapsed-question-mobile-' + width + '.png') });
      await page.locator('#close-sessions').click();
    }
    await page.setViewportSize({ width: 1280, height: 850 });
    sessions = sessions.filter(session => session.worker_id !== 'beta'); workers = workers.filter(worker => worker.ID !== 'beta');
    await page.evaluate(() => {
      const socket = window.workerActivity;
      socket.emit({ type: 'activity_event', version: 3, sequence: ++socket.sequence, revision: socket.sequence + 1, event: 'session_removed', session_id: 'beta-idle', session: null });
    });
    await group('beta').waitFor({ state: 'detached' });
    assert.equal(await alpha.locator('.session-group-count').textContent(), '4', 'Removing a revoked worker leaves other worker totals intact');
    assert.deepEqual(errors, []);
    console.log('WebUI worker activity checks passed: collapsed running/question colors, unique active counts, live transitions, persistence, filtered totals, mobile layout, and removed workers. Screenshots: ' + screenshots);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
