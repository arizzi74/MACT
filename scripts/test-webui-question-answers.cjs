#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
(async () => {
 const browser = await chromium.launch({ headless: true });
 try {
  for (const agent of ['claude', 'codex']) {
   const page = await browser.newPage({ viewport: { width: 1280, height: 1000 } });
   const errors = []; page.on('pageerror', error => errors.push(error.message));
   const selected = { session_id: 'session-' + agent, codex_thread_id: 'thread-' + agent, worker_id: 'worker', worker_name: 'Test worker', runtime_id: 'runtime', runtime_name: agent, agent_kind: agent, name: 'Two questions', cwd: '/projects/test', state: 'running' };
   await page.route('https://answers.test/**', async route => {
    const url = new URL(route.request().url());
    const reply = value => route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, owner_id: 'owner', session_id: 'login', server_time: new Date().toISOString(), expires_at: new Date(Date.now() + 28800000).toISOString() });
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return reply({ sessions: [selected], workers: [], runtimes: [] });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], revision: '1' });
    if (url.pathname.endsWith('/push/config')) return reply({ supported: false, subscribed: false });
    if (url.pathname.endsWith('/question-answers')) {
     assert.equal(url.searchParams.get('session_id'), selected.session_id);
     const saved = await page.evaluate(() => JSON.parse(localStorage.getItem('answers') || '[]'));
     return reply({ answers: saved });
    }
    const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(assets, filename)) });
   });
   await page.addInitScript(({ selected }) => {
    window.framesSent = []; window.sockets = []; window.failAnswer = true;
    window.nativeParams = { threadId: selected.codex_thread_id, turnId: 'turn', itemId: 'question-item', questions: [
     { id: 'first', question: 'Which opportunities?', options: [{ label: 'Renewal (Recommended)' }] },
     { id: 'second', question: 'What should the review produce?' },
    ] };
    const pending = () => localStorage.getItem('answers') ? [] : [{ approvalId: 'approval', method: 'item/tool/requestUserInput', params: window.nativeParams }];
    window.WebSocket = class {
     static OPEN = 1;
     constructor(url) {
      this.readyState = 0; this.activity = new URL(url).pathname.endsWith('/activity');
      if (!this.activity) window.sockets.push(this);
      setTimeout(() => { this.readyState = 1; this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: [{ session_id: selected.session_id, state: 'running', pending_questions: pending().length }], telegram_bots: [], telegram_bots_revision: '1' } : { type: 'ready' }); }, 1);
     }
     emit(value) { this.onmessage?.({ data: JSON.stringify(value) }); }
     close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
     send(raw) {
      const frame = JSON.parse(raw); window.framesSent.push(frame);
      // A raw native response is the original regression; never acknowledge it.
      if (!frame.method) return;
      let result = {};
      if (frame.method === 'thread/resume') result = { thread: { id: selected.codex_thread_id } };
      else if (frame.method === 'thread/items/list') result = { data: [
       { turnId: 'turn', recordedAtMs: Date.now() - 60000, item: { id: 'question-item', type: 'mcpToolCall', server: 'claude', tool: 'AskUserQuestion', status: 'completed', arguments: {} } },
      ], nextCursor: null };
      else if (frame.method === 'thread/turns/list') result = { data: [{ id: 'turn', status: 'inProgress', items: [] }], nextCursor: null };
      else if (frame.method === 'gateway/questions') result = { questions: pending() };
      else if (frame.method === 'thread/goal/get') result = { goal: null };
      else if (frame.method === 'model/list') result = { data: [] };
      else if (frame.method === 'gateway/answer') {
       if (window.failAnswer) { setTimeout(() => this.emit({ id: frame.id, error: { code: -32000, message: 'Temporary failure; answer not accepted.' } }), 1); return; }
       const fields = window.nativeParams.questions.map(q => ({ question: q.question, answer: q.isSecret ? '[REDACTED]' : frame.params.result.answers[q.id].answers.join('\n') }));
       localStorage.setItem('answers', JSON.stringify([{ approval_id: frame.params.approvalId, turn_id: 'turn', item_id: 'question-item', answered_at: new Date().toISOString(), fields }]));
       this.emit({ method: 'serverRequest/resolved', params: { threadId: selected.codex_thread_id, requestId: 'live' } });
      }
      setTimeout(() => this.emit({ id: frame.id, result }), 1);
     }
    };
   }, { selected });
   const open = async () => {
    await page.goto('https://answers.test/tgw/webui/');
    if (!(await page.locator('#connection').getAttribute('data-state') === 'connected')) await page.locator('[data-session-id="' + selected.session_id + '"]').click();
    await page.waitForSelector('#connection[data-state="connected"]');
   };
   await open();
   await page.waitForSelector('[data-request-id="worker:approval"]');
   await page.waitForTimeout(300);
   await page.evaluate(() => {
    window.answerHistoryArticle = document.querySelector('#messages article');
    window.sockets.at(-1).emit({ method: 'serverRequest/resolved', params: { threadId: window.nativeParams.threadId, requestId: 'unrelated-empty' } });
   });
   await page.waitForTimeout(300);
   assert.equal(await page.evaluate(() => window.answerHistoryArticle === document.querySelector('#messages article')), true, 'Empty answer-history refresh does not redraw the conversation');
   await page.evaluate(() => window.sockets.at(-1).emit({ id: 'live', method: 'item/tool/requestUserInput', params: window.nativeParams }));
   const card = page.locator('[data-request-id="live"]');
   await card.locator('input[type=radio]').check();
   await card.locator('textarea').nth(1).fill('Claude Doc review');
   await card.getByRole('button', { name: 'Send answer' }).click();
   await page.getByText('Temporary failure; answer not accepted.', { exact: true }).waitFor();
   assert.equal(await page.locator('[data-item-id="question-answer-approval"]').count(), 0, 'Failed answers must not be echoed');
   assert.equal(await card.locator('textarea').nth(1).inputValue(), 'Claude Doc review', 'Failure preserves typed answers');
   await page.evaluate(() => { window.failAnswer = false; });
   await card.getByRole('button', { name: 'Send answer' }).click();
   const echo = page.locator('[data-item-id="question-answer-approval"]');
   await echo.waitFor();
   assert.match(await echo.textContent(), /Which opportunities\?[\s\S]*Renewal \(Recommended\)[\s\S]*What should the review produce\?[\s\S]*Claude Doc review/);
   assert.equal(await page.locator('#questions').isHidden(), true);
   const frames = await page.evaluate(() => window.framesSent);
   assert.equal(frames.filter(f => !f.method).length, 0, 'Native reply must not bypass worker event recording');
   assert.equal(frames.filter(f => f.method === 'gateway/answer').length, 2, 'Only explicit clicks submit an answer');
   assert.equal(frames.filter(f => f.method === 'turn/start' || f.method === 'turn/steer').length, 0, 'Blocking answers must not start another turn');
   assert.deepEqual(frames.filter(f => f.method === 'gateway/answer').at(-1).params, { approvalId: 'approval', result: { answers: { first: { answers: ['Renewal (Recommended)'] }, second: { answers: ['Claude Doc review'] } } } });
   await page.reload();
   await page.waitForSelector('#connection[data-state="connected"]');
   await page.locator('[data-session-id="' + selected.session_id + '"]').click();
   await page.waitForSelector('#connection[data-state="connected"]');
   await page.locator('[data-item-id="question-answer-approval"]').waitFor();
   assert.equal(await page.locator('[data-item-id="question-answer-approval"]').count(), 1, 'Reconnect restores exactly one durable answer');
   assert.equal(await page.locator('#questions').isHidden(), true);
   await page.waitForTimeout(300);
   await page.evaluate(() => {
    window.answerHistoryArticle = document.querySelector('[data-item-id="question-answer-approval"]');
    window.sockets.at(-1).emit({ method: 'serverRequest/resolved', params: { threadId: window.nativeParams.threadId, requestId: 'unrelated-unchanged' } });
   });
   await page.waitForTimeout(300);
   assert.equal(await page.evaluate(() => window.answerHistoryArticle === document.querySelector('[data-item-id="question-answer-approval"]')), true, 'Unchanged answered history does not redraw the conversation');
   assert.deepEqual(errors, []);
   await page.close();
   console.log(agent + ': live two-question reply, failure, worker recording, durable echo and reconnect passed');
  }
 } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
