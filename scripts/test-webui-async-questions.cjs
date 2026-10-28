#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');

// The native marker expectations are fixed independently of browser formatting.
// The first title reproduces the incident's multiline command question shape.
const multiline = 'Please run this HTTP/2 test and paste the result:\r\n\r\n```sh\ncurl --http2 --head https://example.test\n```\nKeep Smart Stage open.';
const cases = [
  { id: 'command', title: multiline, prefix: '> Please run this HTTP/2 test and paste the result:    ```sh curl --http2 --head https://example.test ``` Keep Smart Stage open.\n\n', answer: 'HTTP/2 200\nSecond output line.' },
  { id: 'unicode', title: 'é'.repeat(255) + '🚀\nQuestion text after the UTF-8 limit?', prefix: '> ' + 'é'.repeat(255) + '\n\n', answer: 'Unicode answer.' },
  { id: 'boundary', title: 'é'.repeat(256), prefix: '> ' + 'é'.repeat(256) + '\n\n', answer: 'Exactly 512 bytes.' },
  { id: 'ascii', title: 'A'.repeat(512) + '\nStill display this complete suffix.', prefix: '> ' + 'A'.repeat(512) + '\n\n', answer: 'ASCII answer.' },
  { id: 'crlf-boundary', title: 'B'.repeat(511) + '\r\nKeep this suffix visible.', prefix: '> ' + 'B'.repeat(511) + ' \n\n', answer: 'CRLF at the byte limit.' },
  { id: 'short', title: 'Which display?', prefix: '> Which display?\n\n', answer: 'External display.' },
  { id: 'bom', title: '\ufeff  Keep the title whitespace.  ', prefix: '> \ufeff  Keep the title whitespace.  \n\n', answer: 'Preserve the original title.' },
];

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 1000 } });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const selected = { session_id: 'stage', codex_thread_id: 'thread-stage', worker_id: 'worker', worker_name: 'Test worker', runtime_id: 'runtime', runtime_name: 'Codex', name: 'Smart Stage', cwd: '/projects/stage', state: 'idle' };
  await page.route('https://questions.test/**', route => {
    const url = new URL(route.request().url());
    const reply = value => route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, owner_id: 'owner', session_id: 'login', server_time: new Date().toISOString(), expires_at: new Date(Date.now() + 28800000).toISOString() });
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return reply({ sessions: [selected], workers: [], runtimes: [] });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], revision: '1' });
    if (url.pathname.endsWith('/push/config')) return reply({ supported: false, subscribed: false });
    const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(({ selected, cases }) => {
    const requests = [
      { approvalId: 'command-request', itemId: 'command-item', fields: cases.slice(0, 1) },
      { approvalId: 'bundle-request', itemId: 'bundle-item', fields: cases.slice(1) },
    ];
    window.questionSockets = []; window.questionFrames = []; window.questionInputs = [];
    window.pendingNativeQuestions = requests.slice(); window.questionSnapshots = 0;
    const savedItems = requests.map(request => ({ turnId: 'question-turn', item: { id: request.itemId, type: 'agentMessage', delivery: 'async', text: 'Please answer.', questions: request.fields.map(field => ({ title: field.title })) } }));
    const pendingSnapshot = () => ({ questions: window.pendingNativeQuestions.map(request => ({ approvalId: request.approvalId, async: true, method: 'item/tool/requestUserInput', params: { threadId: selected.codex_thread_id, turnId: 'question-turn', itemId: request.itemId, questions: request.fields.map(field => ({ id: field.id, question: field.title })) } })) });
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.readyState = 0; this.activity = new URL(url).pathname.endsWith('/activity');
        if (!this.activity) window.questionSockets.push(this);
        setTimeout(() => { this.readyState = 1; this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: [{ session_id: selected.session_id, state: 'idle', pending_questions: 2 }], telegram_bots: [], telegram_bots_revision: '1' } : { type: 'ready' }); }, 1);
      }
      emit(value) { this.onmessage?.({ data: JSON.stringify(value) }); }
      close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
      send(raw) {
        const frame = JSON.parse(raw); window.questionFrames.push(frame);
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: selected.codex_thread_id } };
        else if (frame.method === 'thread/items/list') result = { data: savedItems.slice().reverse(), nextCursor: null };
        else if (frame.method === 'thread/turns/list') result = { data: [{ id: 'question-turn', status: 'completed', items: [] }], nextCursor: null };
        else if (frame.method === 'gateway/questions') { result = pendingSnapshot(); window.questionSnapshots++; }
        else if (frame.method === 'thread/goal/get') result = { goal: null };
        else if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
        else if (frame.method === 'model/list') result = { data: [] };
        else if (frame.method === 'turn/start' || frame.method === 'turn/steer') {
          const text = frame.params.input[0].text;
          window.questionInputs.push({ method: frame.method, text });
          // A native turn accepts any prompt, including a malformed marker.
          // Only canonical markers resolve pending native async questions.
          window.pendingNativeQuestions = window.pendingNativeQuestions.filter(request => !request.fields.every(field => text.includes(field.prefix + field.answer)));
          const echo = { turnId: 'answer-turn', item: { id: 'answer-' + window.questionInputs.length, type: 'userMessage', content: frame.params.input } };
          savedItems.push(echo);
          result = { turn: { id: 'answer-turn', status: 'inProgress', items: [] } };
          setTimeout(() => {
            this.emit({ method: 'turn/started', params: { threadId: selected.codex_thread_id, turn: { id: 'answer-turn', status: 'inProgress', items: [] } } });
            this.emit({ method: 'item/completed', params: { threadId: selected.codex_thread_id, ...echo } });
          }, 1);
        }
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    };
  }, { selected, cases });
  const current = async message => page.evaluate(message => window.questionSockets.at(-1).emit(message), message);
  const refreshPending = async () => {
    const before = await page.evaluate(() => window.questionSnapshots);
    await current({ method: 'serverRequest/resolved', params: { threadId: selected.codex_thread_id, requestId: 'unrelated' } });
    await page.waitForFunction(before => window.questionSnapshots > before, before);
    await page.waitForTimeout(30);
  };
  try {
    await page.goto('https://questions.test/tgw/webui/');
    await page.locator('[data-session-id="stage"]').click();
    await page.waitForSelector('#connection[data-state="connected"]');
    await page.waitForSelector('[data-request-id="worker:command-request"]');
    const command = page.locator('[data-request-id="worker:command-request"]');
    assert.equal(await command.locator('legend').textContent(), multiline.replaceAll('\r', ''), 'The complete multiline question remains displayed');
    await command.locator('textarea').fill(cases[0].answer);
    await command.getByRole('button', { name: 'Send answer', exact: true }).click();
    await page.waitForFunction(() => window.questionInputs.length === 1);
    assert.deepEqual(await page.evaluate(() => window.questionInputs[0]), { method: 'turn/start', text: cases[0].prefix + cases[0].answer }, 'Idle answers use the canonical flattened native marker');
    await refreshPending();
    assert.equal(await command.count(), 0, 'The worker snapshot does not restore the answered multiline question');
    const bundle = page.locator('[data-request-id="worker:bundle-request"]');
    for (let index = 1; index < cases.length; index++) {
      assert.equal(await bundle.locator('legend').nth(index - 1).textContent(), cases[index].title.replaceAll('\r', ''), 'Long Unicode and ASCII question titles remain complete in the form');
      await bundle.locator('textarea').nth(index - 1).fill(cases[index].answer);
    }
    await bundle.getByRole('button', { name: 'Send answer', exact: true }).click();
    await page.waitForFunction(() => window.questionInputs.length === 2);
    assert.deepEqual(await page.evaluate(() => window.questionInputs[1]), { method: 'turn/steer', text: cases.slice(1).map(field => field.prefix + field.answer).join('\n\n') }, 'Active answers preserve native UTF-8 byte boundaries and bundled question markers');
    await refreshPending();
    assert.equal(await page.locator('#questions').isHidden(), true, 'Answered questions stay resolved across authoritative snapshots');
    await page.locator('#disconnect').click();
    await page.locator('#reconnect').click();
    await page.waitForSelector('#connection[data-state="connected"]');
    await page.waitForFunction(() => window.questionSnapshots >= 4);
    assert.equal(await page.locator('#questions').isHidden(), true, 'Reconnect does not resurrect answered historical async items');
    assert.equal(await page.evaluate(() => window.questionInputs.length), 2, 'Reconnect never resubmits an answer');
    for (const [index, answer] of [cases[0].prefix + 'Answered from another client.', '> ' + multiline.replaceAll('\n', '\n> ') + '\n\nAnswered by an older browser.'].entries()) {
      await current({ method: 'item/completed', params: { threadId: selected.codex_thread_id, turnId: 'external-turn', item: { id: 'external-question-' + index, type: 'agentMessage', delivery: 'async', questions: [{ title: multiline }] } } });
      assert.equal(await page.locator('#questions').isVisible(), true);
      await current({ method: 'item/completed', params: { threadId: selected.codex_thread_id, turnId: 'external-answer', item: { id: 'external-answer-' + index, type: 'userMessage', content: [{ type: 'text', text: answer }] } } });
      assert.equal(await page.locator('#questions').isHidden(), true, 'Live canonical and legacy answer echoes resolve multiline questions');
    }
    assert.deepEqual(errors, []);
    console.log('Web UI async question checks passed.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
