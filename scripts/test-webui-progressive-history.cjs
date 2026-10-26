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
  const sessions = ['native', 'other', 'legacy'].map(id => ({ session_id: id, codex_thread_id: 'thread-' + id, worker_id: id,
    worker_name: id, runtime_id: id, runtime_name: 'Codex', name: id, cwd: '/projects/' + id, state: id === 'native' ? 'running' : 'idle' }));
  await page.route('https://webui.test/**', route => {
    const url = new URL(route.request().url());
    const reply = body => route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, owner_id: 'owner', session_id: 'login', expires_at: new Date(Date.now() + 28800000).toISOString(), server_time: new Date().toISOString() });
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return reply({ sessions, workers: [], runtimes: [] });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], revision: '1' });
    if (url.pathname.endsWith('/push/config')) return reply({ supported: false, subscribed: false });
    const filename = url.pathname.startsWith('/tgw/webui/static/') || url.pathname.startsWith('/tgw/admin/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(sessions => {
    window.testSockets = []; window.testSent = [];
    window.testDate = 1790965260000;
    window.testHoldTopUp = true;
    const nativeEntry = index => ({ turnId: 'turn-' + index, startedAtMs: index === 23 ? window.testDate + 23000 : null, completedAtMs: null,
      item: { id: 'native-' + index, type: 'agentMessage', text: 'Native message ' + index + '\n\n' + 'Saved detail. '.repeat(40) } });
    const legacyEntry = index => ({ turnId: 'legacy-turn-' + index, turnStartedAt: window.testDate / 1000 + index, turnStatus: 'completed',
      item: { id: 'legacy-' + index, type: 'agentMessage', text: 'Legacy message ' + index + '\n\n' + 'Saved detail. '.repeat(40) } });
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.readyState = 0; this.url = String(url); this.session = new URL(url).searchParams.get('session_id');
        this.activity = this.url.includes('/activity'); this.pending = new Set(); this.maximumPending = 0;
        if (!this.activity) window.testSockets.push(this);
        setTimeout(() => {
          this.readyState = 1;
          this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1,
            sessions: sessions.map(session => ({ session_id: session.session_id, state: session.state, pending_questions: 0 })), telegram_bots: [], telegram_bots_revision: '1' } : { type: 'ready' });
        }, 1);
      }
      emit(message) { if (message.id !== undefined) this.pending.delete(message.id); this.onmessage?.({ data: JSON.stringify(message) }); }
      close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
      send(raw) {
        const frame = JSON.parse(raw);
        if (!frame.method) return;
        window.testSent.push({ session: this.session, ...frame });
        this.pending.add(frame.id); this.maximumPending = Math.max(this.maximumPending, this.pending.size);
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: frame.params.threadId }, model: 'codex' };
        else if (frame.method === 'thread/turns/list') {
          if (this.session === 'native') {
            const offset = Number(frame.params.cursor || 0), turns = Array.from({ length: 24 }, (_, i) => ({ id: 'turn-' + (23 - i), status: i === 0 ? 'inProgress' : 'completed', startedAt: window.testDate / 1000 + 23 - i, items: [] }));
            result = { data: turns.slice(offset, offset + frame.params.limit), nextCursor: offset + frame.params.limit < turns.length ? String(offset + frame.params.limit) : null };
            if (frame.params.cursor) { this.heldMetadata = { frame, result }; return; }
          } else result = { data: [{ id: this.session + '-turn-23', status: 'completed', startedAt: window.testDate / 1000 + 23, items: [] }], nextCursor: null };
        } else if (frame.method === 'thread/items/list') {
          if (this.session === 'native') {
            const offset = Number(frame.params.cursor || 0), items = Array.from({ length: 24 }, (_, i) => nativeEntry(23 - i));
            result = { data: items.slice(offset, offset + frame.params.limit), nextCursor: offset + frame.params.limit < items.length ? String(offset + frame.params.limit) : null };
          } else if (this.session === 'legacy') {
            if (!frame.params.cursor) result = { data: [legacyEntry(23), legacyEntry(22), legacyEntry(21)], nextCursor: 'empty' };
            else if (frame.params.cursor === 'empty') result = { data: [], nextCursor: '21' };
            else {
              const offset = Number(frame.params.cursor), size = Math.min(3, frame.params.limit, offset);
              result = { data: Array.from({ length: size }, (_, i) => legacyEntry(offset - 1 - i)), nextCursor: offset - size > 0 ? String(offset - size) : null };
              if (offset === 21) result.data.unshift(legacyEntry(23));
            }
            if (frame.params.cursor && window.testHoldTopUp) { this.heldTopUp = { frame, result }; return; }
          } else result = { data: [{ turnId: 'other-turn', item: { id: 'native-15', type: 'agentMessage', text: 'Other worker message.' } }], nextCursor: null };
        } else if (frame.method === 'gateway/questions') result = { questions: [] };
        else if (frame.method === 'model/list') result = { data: [] };
        else if (frame.method === 'thread/goal/get') result = { goal: null };
        else if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
        else if (frame.method === 'turn/steer') { this.heldSteer = { frame, result }; return; }
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    };
  }, sessions);
  const choose = async id => {
    await page.locator('[data-session-id="' + id + '"]').click();
    await page.waitForSelector('#connection[data-state="connected"]');
  };
  const time = id => page.locator('[data-item-id="' + id + '"] time');
  const current = message => page.evaluate(value => window.testSockets.at(-1).emit(value), message);
  const timestamps = (thread, items) => current({ method: 'gateway/history/timestamps', params: { threadId: thread, items } });
  try {
    await page.goto('https://webui.test/tgw/webui/');
    await page.locator('[data-session-id="native"]').waitFor();
    await choose('native');
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldMetadata));
    assert.equal(await page.locator('#messages article').count(), 20, 'Native first payload is readable before optional turn dates');
    await page.locator('#prompt').fill('Continue the active turn.');
    assert.equal(await page.locator('#send').isDisabled(), false, 'Delayed historical metadata cannot disable controls');
    assert.equal(await page.locator('#stop').isVisible(), true, 'One latest-turn checkpoint establishes the active turn');
    assert.equal(await time('native-15').count(), 0, 'Native items with null own dates do not acquire invented dates');
    assert.equal(await time('native-23').getAttribute('data-source'), 'item-started');
    const initial = await page.evaluate(() => window.testSent.filter(frame => frame.session === 'native' && frame.method === 'thread/turns/list'));
    assert.deepEqual(initial.map(frame => ({ limit: frame.params.limit, cursor: frame.params.cursor || null, view: frame.params.itemsView })),
      [{ limit: 1, cursor: null, view: 'notLoaded' }, { limit: 20, cursor: '1', view: 'notLoaded' }]);
    await page.locator('#send').click();
    assert.ok(await page.evaluate(() => window.testSent.some(frame => frame.method === 'turn/steer' && frame.params.expectedTurnId === 'turn-23')), 'Active-turn state remains authoritative before optional metadata');
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldSteer));
    assert.equal(await page.locator('#prompt').inputValue(), 'Continue the active turn.', 'The delayed send remains pending until its acknowledgement');
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1), held = socket.heldSteer;
      socket.emit({ id: held.frame.id, result: held.result });
    });
    // Accepted sends clear/resize the composer and intentionally jump to the
    // latest output. Finish that interaction before testing a reader's anchor.
    await page.waitForFunction(() => document.querySelector('#prompt').value === '' && !document.querySelector('#stop').disabled);
    await page.evaluate(async () => { await document.fonts.ready; await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))); });
    await page.locator('#transcript').evaluate(scroller => {
      window.readingScrollEnded = false;
      scroller.addEventListener('scrollend', () => { window.readingScrollEnded = true; }, { once: true });
    });
    await page.locator('#transcript').hover();
    const readingDistance = await page.locator('#transcript').evaluate(scroller => Math.floor((scroller.scrollHeight - scroller.clientHeight) / 2));
    await page.mouse.wheel(0, -readingDistance);
    await page.waitForFunction(() => {
      const scroller = document.querySelector('#transcript');
      return window.readingScrollEnded && scroller.scrollTop > 0 && scroller.scrollTop < scroller.scrollHeight - scroller.clientHeight - 100;
    });
    const saved = 1790965275555;
    await timestamps('thread-native', [{ itemId: 'native-15', turnId: 'wrong-turn', recordedAtMs: saved }, { itemId: 'native-23', turnId: 'turn-23', recordedAtMs: saved }]);
    assert.equal(await time('native-15').count(), 0, 'Timestamp evidence is fenced by item turn identity');
    assert.equal(await time('native-23').getAttribute('data-source'), 'item-started', 'Saved dates cannot replace producer start times');
    await timestamps('thread-other', [{ itemId: 'native-15', turnId: 'turn-15', recordedAtMs: saved }]);
    assert.equal(await time('native-15').count(), 0, 'Timestamp evidence is fenced by selected thread');
    // Capture and enrich in the same browser task so unrelated pending work
    // cannot become part of the timestamp update's measured displacement.
    await page.evaluate(recordedAtMs => {
      const scroller = document.querySelector('#transcript');
      window.keptArticle = document.querySelector('[data-item-id="native-15"]');
      const anchor = [...document.querySelectorAll('#messages article')].find(article => article.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
      window.keptAnchor = { id: anchor.dataset.itemId, top: anchor.getBoundingClientRect().top };
      window.testSockets.at(-1).emit({ method: 'gateway/history/timestamps', params: { threadId: 'thread-native', items: [{ itemId: 'native-15', turnId: 'turn-15', recordedAtMs }] } });
    }, saved);
    assert.equal(await time('native-15').getAttribute('datetime'), new Date(saved).toISOString());
    assert.equal(await time('native-15').getAttribute('data-source'), 'item-recorded');
    assert.equal(await page.evaluate(() => window.keptArticle === document.querySelector('[data-item-id="native-15"]')), true, 'Enrichment keeps the existing article DOM');
    const anchorDrift = await page.evaluate(() => document.querySelector('[data-item-id="' + window.keptAnchor.id + '"]').getBoundingClientRect().top - window.keptAnchor.top);
    assert.ok(Math.abs(anchorDrift) < 2, 'Enrichment preserves the reading anchor: ' + anchorDrift + 'px');
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1), held = socket.heldMetadata;
      socket.emit({ id: held.frame.id, result: held.result });
    });
    await page.waitForSelector('[data-item-id="native-14"] time');
    assert.equal(await time('native-14').getAttribute('data-source'), 'turn-started', 'Optional metadata backfills earlier visible native turns');
    assert.equal(await time('native-15').getAttribute('data-source'), 'item-recorded', 'Turn fallback cannot replace verified saved item dates');
    await choose('other');
    await choose('native');
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldMetadata));
    await page.evaluate(() => { const socket = window.testSockets.at(-1); window.oldNative = { callback: socket.onmessage, metadata: socket.heldMetadata }; });
    await choose('other');
    await page.evaluate(() => {
      const { callback, metadata } = window.oldNative;
      callback({ data: JSON.stringify({ id: metadata.frame.id, result: metadata.result }) });
      callback({ data: JSON.stringify({ method: 'gateway/history/timestamps', params: { threadId: 'thread-native', items: [{ itemId: 'native-15', turnId: 'turn-15', recordedAtMs: window.testDate }] } }) });
    });
    assert.equal(await page.locator('#messages article').count(), 1);
    assert.equal(await time('native-15').count(), 0, 'Late metadata and timestamp frames cannot cross socket generations');

    await choose('legacy');
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1).heldTopUp));
    assert.equal(await page.locator('#messages article').count(), 3, 'Partial legacy payload appears before continuation work');
    await page.locator('#prompt').fill('Usable during the background continuation.');
    assert.equal(await page.locator('#send').isDisabled(), false);
    assert.equal(await page.locator('#older').isDisabled(), true, 'One continuation owns the item cursor');
    const reads = await page.evaluate(() => window.testSent.filter(frame => frame.session === 'legacy' && frame.method === 'thread/items/list').length);
    await page.locator('#older').evaluate(button => button.click());
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.session === 'legacy' && frame.method === 'thread/items/list').length), reads, 'Manual paging cannot race the initial continuation');
    await current({ method: 'item/completed', params: { threadId: 'thread-legacy', turnId: 'legacy-turn-23', item: { id: 'legacy-23', type: 'agentMessage', text: 'Newer live text wins over the delayed saved page.' } } });
    await current({ method: 'item/completed', params: { threadId: 'thread-legacy', turnId: 'live-turn', item: { id: 'legacy-live', type: 'agentMessage', text: 'Live output while the snapshot continues.' } } });
    await page.waitForSelector('[data-item-id="legacy-live"]');
    await page.evaluate(() => {
      const socket = window.testSockets.at(-1), held = socket.heldTopUp;
      window.testHoldTopUp = false; socket.emit({ id: held.frame.id, result: held.result });
    });
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 20 && !document.querySelector('#older').disabled);
    assert.deepEqual(await page.locator('#messages article').evaluateAll(articles => articles.map(article => article.dataset.itemId)),
      [...Array.from({ length: 19 }, (_, i) => 'legacy-' + (i + 5)), 'legacy-live'], 'Continuations preserve chronological snapshot order and newer live output');
    assert.match(await page.locator('[data-item-id="legacy-23"]').textContent(), /Newer live text wins/, 'A delayed saved duplicate cannot overwrite a live item');
    assert.equal(await page.locator('#prompt').inputValue(), 'Usable during the background continuation.');
    const toppedUpReads = await page.evaluate(() => window.testSent.filter(frame => frame.session === 'legacy' && frame.method === 'thread/items/list').length);
    await page.waitForTimeout(100);
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.session === 'legacy' && frame.method === 'thread/items/list').length), toppedUpReads, 'Automatic fill stops at the recent 20 entries');
    await page.locator('#older').evaluate(button => button.click());
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 23);
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => frame.session === 'legacy' && frame.method === 'thread/items/list').length), toppedUpReads + 1, 'Reader-initiated paging retains the continuation after automatic fill');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).length), 1, 'Background work never replays a prompt');
    assert.ok(await page.evaluate(() => window.testSockets.every(socket => socket.maximumPending <= 4)), 'Bootstrap background requests leave capacity for an interactive send');
    assert.deepEqual(errors, []);
    console.log('Progressive Web UI history checks passed: native dates, latest-turn checkpoint, bounded partial/empty continuations, live output, cursor ownership, asynchronous timestamp enrichment, scroll anchors and session fences.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
