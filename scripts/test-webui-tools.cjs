#!/usr/bin/env node
// Real WebUI assets with an isolated fake gateway: no prompts or live runtime.
// WEBUI_SCREENSHOTS=/path/to/output node scripts/test-webui-tools.cjs
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const screenshots = process.env.WEBUI_SCREENSHOTS || path.resolve(__dirname, '../tmp/tool-ui-screenshots');
const hostile = '<img src="https://attacker.invalid/tool" onerror="window.injected=true"><script>window.injected=true</script>';
const python = "python3 - <<'PY'\nimport json\n# Keep source literal: " + hostile + '\nfor value in range(3):\n    print(json.dumps({"value": value, "ready": True}))\nPY';
const javascript = "node - <<'JS'\n// A public JavaScript tool fixture\nconst ready = true;\nconst count = 42;\nconsole.log(JSON.stringify({ready, count}));\nJS";
const longCommand = "python3 - <<'PY'\nimport json\n# " + 'A long command must remain available in the expanded body. '.repeat(24) + '\nprint(json.dumps({"command_tail": "FULL_COMMAND_END", "ready": True}))\nPY';
const longOutput = 'BEGIN_OUTPUT\n' + 'Complete result retained. '.repeat(42) + '\n' + hostile + '\nFULL_OUTPUT_END';
const jsonArguments = { language: 'json', enabled: true, attempts: 3, markup: hostile };
const dynamicArguments = 'const count = 42;\n// Native functions.exec JavaScript arguments\nconst markup = ' + JSON.stringify(hostile) + ';\nconst value = await tools.exec_command({cmd: "printf fixture"});\ntext({value, count, markup});';
const dynamicContentItems = [{ type: 'inputText', text: 'NATIVE_DYNAMIC_CONTENT_START\n' + longOutput }, { type: 'inputText', text: 'NATIVE_DYNAMIC_CONTENT_END' }];
const runningPython = "python3 - <<'PY'\nimport json\nfor index in range(3):\n    print(json.dumps({\"index\": index, \"ready\": True}))\n# Literal markup: " + hostile + '\n' + '# Retain the full running Python source.\n'.repeat(18) + "print('RUNNING_PYTHON_SOURCE_END')\nPY";
const runningJavascript = "node - <<'JS'\nconst ready = true;\nconst count = 42;\nconst markup = " + JSON.stringify(hostile) + ';\nconsole.log({ready, count, markup});\n' + '// Retain the full running Node source.\n'.repeat(18) + 'console.log("RUNNING_JS_SOURCE_END");\nJS';
const runningSources = [['running-python', runningPython, 'python'], ['running-javascript', runningJavascript, 'javascript']];
const session = { session_id: 'tools', codex_thread_id: 'thread-tools', worker_id: 'fixture', worker_name: 'Browser fixture', runtime_id: 'codex', runtime_name: 'Codex', name: 'Tool activity regression', cwd: '/projects/tool-ui', state: 'running' };
const saved = [
  { id: 'long-success', type: 'commandExecution', status: 'completed', command: longCommand, exitCode: 0, aggregatedOutput: longOutput },
  { id: 'exit-failure', type: 'commandExecution', status: 'completed', command: 'go test ./... ' + 'long_package_name_'.repeat(50), exitCode: 7, aggregatedOutput: 'FAIL: fixture assertion\n' + hostile },
  { id: 'quiet-success', type: 'commandExecution', status: 'completed', command: 'mkdir -p /tmp/fixture', exitCode: 0, aggregatedOutput: '' },
  { id: 'mcp-failure', type: 'mcpToolCall', status: 'completed', server: 'fixture-service', tool: 'validate_' + 'long_tool_name_'.repeat(35), arguments: jsonArguments, result: { isError: true, content: [{ type: 'text', text: 'Validation failed: ' + hostile }] } },
  { id: 'interrupted', type: 'commandExecution', status: 'interrupted', command: 'sleep 30', aggregatedOutput: 'Interrupted before exit status was reported.' },
  { id: 'cancelled', type: 'commandExecution', status: 'cancelled', command: 'cancelled-check', aggregatedOutput: '' },
  { id: 'unknown-outcome', type: 'commandExecution', status: 'completed', command: 'check-without-exit-code', aggregatedOutput: 'Exit status was not recorded.' },
  { id: 'python-source', type: 'commandExecution', status: 'completed', command: python, exitCode: 0, aggregatedOutput: '{"value": 2, "ready": true}' },
  { id: 'javascript-source', type: 'commandExecution', status: 'completed', command: javascript, exitCode: 0, aggregatedOutput: '{"ready": true, "count": 42}' },
  { id: 'json-source', type: 'mcpToolCall', status: 'completed', server: 'fixture-service', tool: 'read_config', arguments: jsonArguments, result: { isError: false, content: [{ type: 'text', text: JSON.stringify({ accepted: true, count: 42, markup: hostile }) }] } },
  { id: 'dynamic-success', type: 'dynamicToolCall', namespace: 'functions', tool: 'exec', arguments: dynamicArguments, contentItems: dynamicContentItems, success: true },
  { id: 'dynamic-failure', type: 'dynamicToolCall', namespace: 'functions', tool: 'validate', arguments: 'const valid = false;\ntext(valid);', contentItems: [{ type: 'inputText', text: 'NATIVE_DYNAMIC_FAILURE: ' + hostile }], success: false },
  { id: 'running-preview', type: 'commandExecution', status: 'inProgress', command: 'printf "RUNNING_PREVIEW_START"\n' + 'echo still-running\n'.repeat(55), aggregatedOutput: 'RUNNING_OUTPUT_VISIBLE\nWork is continuing.' },
  ...runningSources.map(([id, command]) => ({ id, type: 'commandExecution', status: 'inProgress', command, aggregatedOutput: 'This heredoc is still running.' })),
  { id: 'history-race', type: 'commandExecution', status: 'inProgress', command: 'saved-running-command', aggregatedOutput: 'Saved running output.' },
];

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 }, colorScheme: 'dark' });
  const errors = [], externalRequests = [], apiWrites = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/*', route => {
    const request = route.request(), url = new URL(request.url());
    if (url.origin !== 'https://webui.test') { externalRequests.push(request.url()); return route.abort(); }
    const reply = body => route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
    if (url.pathname.startsWith('/tgw/api/') && request.method() !== 'GET') apiWrites.push({ method: request.method(), path: url.pathname });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, owner_id: 'fixture-owner', session_id: 'fixture-login', expires_at: new Date(Date.now() + 28800000).toISOString(), server_time: new Date().toISOString() });
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return reply({ sessions: [session], workers: [], runtimes: [] });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], revision: 'tools-fixture' });
    if (url.pathname.endsWith('/push/config')) return reply({ supported: false, subscribed: false });
    if (url.pathname.startsWith('/tgw/api/')) return reply({});
    const filename = /\/(?:webui|admin)\/static\//.test(url.pathname) ? path.basename(url.pathname) : 'webui.html';
    const file = path.join(assets, filename);
    if (!fs.existsSync(file)) return route.fulfill({ status: 404, body: '' });
    const contentType = filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : filename.endsWith('.svg') ? 'image/svg+xml' : filename.endsWith('.png') ? 'image/png' : 'text/html';
    return route.fulfill({ contentType, headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'" }, body: fs.readFileSync(file) });
  });
  await page.addInitScript(({ session, saved }) => {
    window.testSockets = []; window.testSent = [];
    const entry = item => ({ turnId: item.status === 'inProgress' ? 'active-turn' : 'saved-turn', turnStatus: item.status === 'inProgress' ? 'inProgress' : 'completed', item });
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.url = String(url); this.readyState = 0; this.activity = this.url.includes('/activity');
        if (!this.activity) window.testSockets.push(this);
        setTimeout(() => {
          if (this.readyState === 3) return;
          this.readyState = 1; this.onopen?.({});
          this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: [{ session_id: session.session_id, state: 'running', pending_questions: 0 }], telegram_bots: [], telegram_bots_revision: 'tools-fixture' } : { type: 'ready' });
        }, 1);
      }
      emit(value) { this.onmessage?.({ data: JSON.stringify(value) }); }
      close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
      send(raw) {
        const frame = JSON.parse(raw); window.testSent.push(frame);
        if (!frame.method) return;
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: 'thread-tools' }, model: 'codex' };
        else if (frame.method === 'thread/turns/list') result = { data: [{ id: 'active-turn', status: 'inProgress', items: [] }], nextCursor: null };
        else if (frame.method === 'thread/items/list') {
          if (!frame.params.cursor) result = { data: [...saved].reverse().map(entry), nextCursor: 'older-tools' };
          else {
            this.heldHistory = { frame, result: { data: [
              entry({ id: 'history-race', type: 'commandExecution', status: 'inProgress', command: 'STALE_SAVED_COMMAND', aggregatedOutput: 'STALE_SAVED_OUTPUT' }),
              entry({ id: 'history-running', type: 'commandExecution', status: 'completed', command: 'STALE_COMPLETED_COMMAND', aggregatedOutput: 'STALE_COMPLETED_OUTPUT', exitCode: 9 }),
              entry({ id: 'old-note', type: 'agentMessage', text: 'Older saved fixture.' }),
            ], nextCursor: null } };
            return;
          }
        } else if (frame.method === 'gateway/questions') result = { questions: [] };
        else if (frame.method === 'model/list') result = { data: [] };
        else if (frame.method === 'thread/goal/get') result = { goal: null };
        else if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    };
  }, { session, saved });
  const article = id => page.locator('[data-item-id="' + id + '"]');
  const details = id => article(id).locator('details.tool-details');
  const isOpen = id => details(id).evaluate(element => element.open);
  let marker = 0;
  const events = async messages => {
    const text = 'Rendered update ' + (++marker);
    await page.evaluate(({ messages, text }) => {
      const socket = window.testSockets.at(-1);
      for (const message of messages) socket.emit(message);
      socket.emit({ method: 'item/completed', params: { threadId: 'thread-tools', turnId: 'active-turn', item: { id: 'update-marker', type: 'agentMessage', text } } });
    }, { messages, text });
    await page.waitForFunction(text => document.querySelector('[data-item-id="update-marker"]')?.textContent.includes(text), text);
  };
  const itemEvent = (method, item, turnId = 'active-turn') => ({ method, params: { threadId: 'thread-tools', turnId, item } });
  const assertState = async (id, expected, complete = true) => {
    assert.equal(await article(id).getAttribute('data-tool-state'), expected, id + ' has a truthful outcome');
    assert.equal(await article(id).getAttribute('data-tool-complete'), String(complete), id + ' has the expected lifecycle');
    if (complete) assert.ok((await article(id).locator('.tool-outcome').textContent()).trim(), id + ' has a visible text outcome');
  };
  const assertSafe = async () => {
    assert.equal(await page.evaluate(() => Boolean(window.injected)), false, 'Tool source and output cannot execute markup');
    assert.equal(await page.locator('#messages img, #messages script, #messages iframe, #messages a[href^="javascript:"], #messages [onerror]').count(), 0, 'Hostile markup remains literal tool text');
  };
  try {
    await page.goto('https://webui.test/tgw/webui/');
    await page.locator('[data-session-id="tools"]').click();
    await page.waitForSelector('#connection[data-state="connected"]');
    await page.waitForFunction(() => Boolean(window.testSockets.at(-1)?.heldHistory));
    await article('long-success').waitFor();
    for (const [id, state] of [['long-success', 'succeeded'], ['exit-failure', 'failed'], ['mcp-failure', 'failed'], ['dynamic-success', 'succeeded'], ['dynamic-failure', 'failed'], ['interrupted', 'interrupted'], ['cancelled', 'cancelled'], ['unknown-outcome', 'completed']]) {
      await assertState(id, state);
      assert.equal(await isOpen(id), false, id + ' starts collapsed in saved history');
    }
    const successColor = await article('long-success').locator('.tool-outcome').evaluate(element => getComputedStyle(element).color);
    for (const id of ['exit-failure', 'mcp-failure', 'dynamic-failure', 'interrupted', 'cancelled', 'unknown-outcome']) {
      assert.notEqual(await article(id).locator('.tool-outcome').evaluate(element => getComputedStyle(element).color), successColor, id + ' cannot use the success badge color');
    }
    await assertState('running-preview', 'running', false);
    assert.equal(await isOpen('running-preview'), false, 'Running tools retain their existing collapsed detail default');
    const runningPreview = await article('running-preview').locator('summary .tool-title').textContent();
    assert.ok(runningPreview.includes('RUNNING_PREVIEW_START'), 'Running command retains a useful preview');
    assert.equal(runningPreview.length, 600, 'Running command retains the existing bounded 600-character preview');
    assert.ok(runningPreview.split('\n').length > 10, 'Running preview retains multiline source');
    await article('running-preview').locator('summary').click();
    await article('running-preview').getByText('RUNNING_OUTPUT_VISIBLE\nWork is continuing.', { exact: true }).waitFor({ state: 'visible' });
    await article('running-preview').locator('summary').click();
    for (const [id, source, language] of runningSources) {
      await assertState(id, 'running', false);
      assert.equal(await isOpen(id), false, id + ' syntax is visible while full details remain closed');
      const preview = article(id).locator('summary .tool-title');
      assert.equal(await preview.textContent(), ('Running ' + source).slice(0, 600), id + ' preserves the exact existing 600-character source prefix');
      assert.equal(await details(id).locator('pre, .code-label').count(), 0, id + ' preview does not add a code block, language label or eager detail body');
      for (const token of ['keyword', 'string', 'number', 'comment']) {
        const syntax = preview.locator('.syntax-' + token + '[data-language="' + language + '"]');
        assert.ok(await syntax.count(), id + ' highlights ' + language + ' ' + token + ' tokens directly in the closed preview');
        assert.equal(await syntax.first().isVisible(), true, id + ' ' + token + ' highlighting is visible without opening details');
      }
      const colors = await preview.evaluate(element => ({ plain: getComputedStyle(element).color, keyword: getComputedStyle(element.querySelector('.syntax-keyword')).color, string: getComputedStyle(element.querySelector('.syntax-string')).color }));
      assert.notEqual(colors.keyword, colors.plain, id + ' keyword spans have a visible syntax color');
      assert.notEqual(colors.string, colors.plain, id + ' string spans have a visible syntax color');
      await article(id).locator('summary').click();
      assert.equal(await details(id).locator('pre code').first().textContent(), source, id + ' retains complete source beyond the highlighted preview');
      await article(id).locator('summary').click();
    }
    assert.equal(await article('long-success').locator('summary [class*="syntax-"]').count(), 0, 'Completed single-line summaries remain plain text');
    await assertSafe();

    await article('long-success').locator('summary').click();
    assert.equal(await details('long-success').locator('pre code').first().textContent(), longCommand, 'Expanded command retains every character beyond the summary preview');
    assert.ok((await details('long-success').textContent()).includes(longOutput), 'Expanded command retains complete output');
    assert.ok((await details('long-success').textContent()).includes('FULL_COMMAND_END'));
    assert.ok((await details('long-success').textContent()).includes('FULL_OUTPUT_END'));
    await article('long-success').locator('summary').click();
    await article('quiet-success').locator('summary').click();
    assert.equal(await details('quiet-success').locator('pre code').first().textContent(), 'mkdir -p /tmp/fixture', 'A quiet command still exposes its complete invocation');
    assert.equal(await article('quiet-success').locator('.message-header').isVisible(), true, 'Expanding a completed tool restores its retained header');
    assert.match(await details('quiet-success').textContent(), /Command/, 'A quiet command has a labeled detail body');
    await article('quiet-success').locator('summary').click();

    for (const [id, source] of [['python-source', python], ['javascript-source', javascript]]) {
      await article(id).locator('summary').click();
      const sourceCode = details(id).locator('pre code').first();
      assert.equal(await sourceCode.textContent(), source, id + ' preserves literal heredoc source');
      assert.ok(await sourceCode.locator('.syntax-keyword').count(), id + ' has highlighted language keywords');
      assert.ok(await sourceCode.locator('.syntax-string').count(), id + ' has highlighted strings');
      assert.ok(await sourceCode.locator('.syntax-number').count(), id + ' has highlighted numbers');
      assert.ok(await sourceCode.locator('.syntax-comment').count(), id + ' has highlighted comments');
      await article(id).locator('summary').click();
    }
    await article('json-source').locator('summary').click();
    await details('json-source').locator('pre code').first().waitFor();
    assert.ok((await details('json-source').textContent()).includes(JSON.stringify(jsonArguments, null, 2)), 'MCP input retains complete structured JSON including hostile strings');
    assert.ok(await details('json-source').locator('.syntax-string').count(), 'JSON tool data has highlighted strings');
    assert.ok(await details('json-source').locator('.syntax-keyword').count(), 'JSON tool data has highlighted booleans');
    assert.ok(await details('json-source').locator('.syntax-number').count(), 'JSON tool data has highlighted numbers');
    await article('json-source').locator('summary').click();
    for (const [id, tool] of [['dynamic-success', 'exec'], ['dynamic-failure', 'validate']]) {
      assert.equal(await article(id).locator('summary .tool-title').textContent(), 'functions / ' + tool, 'Native dynamic tool summary retains namespace and tool identity');
      await article(id).locator('summary').click();
      assert.equal(await details(id).locator('pre code').first().textContent(), 'functions / ' + tool, 'Native dynamic tool detail retains namespace and tool identity');
    }
    const dynamicSource = details('dynamic-success').locator('pre code').nth(1);
    assert.equal(await dynamicSource.textContent(), dynamicArguments, 'Native dynamic tool retains its complete JavaScript argument string');
    assert.equal(await dynamicSource.evaluate(element => element.parentElement.dataset.language), 'javascript', 'Native dynamic arguments are recognized as JavaScript source');
    for (const token of ['keyword', 'string', 'number', 'comment']) assert.ok(await dynamicSource.locator('.syntax-' + token).count(), 'Native dynamic JavaScript arguments highlight ' + token + ' tokens');
    assert.deepEqual(JSON.parse(await details('dynamic-success').locator('pre code').nth(2).textContent()), dynamicContentItems, 'Native dynamic tool retains every contentItems result and the full long output');
    assert.deepEqual(JSON.parse(await details('dynamic-failure').locator('pre code').nth(2).textContent()), [{ type: 'inputText', text: 'NATIVE_DYNAMIC_FAILURE: ' + hostile }], 'Failed native dynamic tool retains its contentItems result');
    await assertSafe();
    await article('dynamic-success').locator('summary').click();
    await article('dynamic-failure').locator('summary').click();

    // Native summary provides the keyboard disclosure control, including its arrow.
    const keyboardSummary = article('quiet-success').locator('summary');
    await keyboardSummary.focus();
    await page.keyboard.press('Enter');
    assert.equal(await isOpen('quiet-success'), true, 'Enter expands a focused tool summary');
    await page.keyboard.press('Space');
    assert.equal(await isOpen('quiet-success'), false, 'Space collapses a focused tool summary');
    assert.equal(await keyboardSummary.evaluate(element => document.activeElement === element), true, 'Disclosure keyboard actions retain summary focus');
    assert.equal(await keyboardSummary.evaluate(element => getComputedStyle(element, '::marker').content !== 'none' || getComputedStyle(element, '::before').content !== 'none'), true, 'The summary retains a disclosure arrow');

    await events([
      itemEvent('item/completed', { id: 'history-race', type: 'commandExecution', status: 'completed', command: 'fresh-live-command', exitCode: 0, aggregatedOutput: 'FRESH_LIVE_OUTPUT' }),
      itemEvent('item/started', { id: 'history-running', type: 'commandExecution', status: 'inProgress', command: 'fresh-running-command', aggregatedOutput: 'FRESH_RUNNING_OUTPUT' }),
    ]);
    await article('history-race').locator('summary').click();
    await page.evaluate(() => { const socket = window.testSockets.at(-1), held = socket.heldHistory; socket.emit({ id: held.frame.id, result: held.result }); });
    await article('old-note').waitFor();
    await assertState('history-race', 'succeeded');
    assert.equal(await isOpen('history-race'), true, 'Saved history cannot reset a manual completed expansion');
    assert.match(await article('history-race').textContent(), /fresh-live-command/);
    assert.doesNotMatch(await article('history-race').textContent(), /STALE_SAVED/);
    await assertState('history-running', 'running', false);
    await article('history-running').locator('summary').click();
    await details('history-running').locator('pre code').first().waitFor();
    assert.match(await article('history-running').textContent(), /FRESH_RUNNING_OUTPUT/);
    assert.doesNotMatch(await article('history-running').textContent(), /STALE_COMPLETED/);

    const lifecycleCommand = 'printf "LIFECYCLE_COMMAND"\n' + 'echo complete-source\n'.repeat(45);
    await events([itemEvent('item/started', { id: 'lifecycle', type: 'commandExecution', status: 'inProgress', command: lifecycleCommand, aggregatedOutput: 'LIFECYCLE_OUTPUT_START\n' })]);
    await article('lifecycle').locator('summary').click();
    assert.equal(await isOpen('lifecycle'), true, 'A running tool can be expanded before completion');
    await events([{ method: 'item/commandExecution/outputDelta', params: { threadId: 'thread-tools', turnId: 'active-turn', itemId: 'lifecycle', delta: 'LIFECYCLE_OUTPUT_END' } }]);
    assert.equal(await isOpen('lifecycle'), true, 'Streaming output does not collapse a running preview');
    await events([itemEvent('item/completed', { id: 'lifecycle', type: 'commandExecution', status: 'completed', exitCode: 0 })]);
    await assertState('lifecycle', 'succeeded');
    assert.equal(await isOpen('lifecycle'), false, 'Completion automatically collapses the running tool once');
    await article('lifecycle').locator('summary').click();
    assert.equal(await details('lifecycle').locator('pre code').first().textContent(), lifecycleCommand, 'Sparse completion retains the full command');
    assert.match(await details('lifecycle').textContent(), /LIFECYCLE_OUTPUT_START\nLIFECYCLE_OUTPUT_END/, 'Sparse completion retains accumulated output');
    await events([itemEvent('item/completed', { id: 'unrelated', type: 'commandExecution', status: 'completed', command: 'echo unrelated', exitCode: 0, aggregatedOutput: 'unrelated output' })]);
    assert.equal(await isOpen('lifecycle'), true, 'Unrelated tool activity preserves manual completed expansion');
    await events([itemEvent('item/completed', { id: 'lifecycle', type: 'commandExecution', status: 'completed', exitCode: 0 })]);
    assert.equal(await isOpen('lifecycle'), true, 'Duplicate completion does not collapse a manually expanded completed tool');
    await article('lifecycle').locator('summary').click();
    await events([itemEvent('item/completed', { id: 'unrelated', type: 'commandExecution', status: 'completed', exitCode: 0, aggregatedOutput: 'updated unrelated output' })]);
    assert.equal(await isOpen('lifecycle'), false, 'Unrelated activity preserves manual completed closure');
    await events([itemEvent('item/started', { id: 'lifecycle', type: 'commandExecution', status: 'inProgress', command: 'STALE_LIVE_COMMAND', aggregatedOutput: 'STALE_LIVE_OUTPUT' })]);
    await assertState('lifecycle', 'succeeded');
    assert.equal(await isOpen('lifecycle'), false, 'A late start cannot reopen a completed tool');
    await article('lifecycle').locator('summary').click();
    assert.equal(await details('lifecycle').locator('pre code').first().textContent(), lifecycleCommand, 'A late start cannot overwrite the command');
    assert.doesNotMatch(await article('lifecycle').textContent(), /STALE_LIVE/);
    await article('lifecycle').locator('summary').click();

    await events([itemEvent('item/started', { id: 'turn-interrupted', type: 'commandExecution', status: 'inProgress', command: 'wait-for-interrupt', aggregatedOutput: 'Waiting.' }, 'interrupt-turn')]);
    await article('turn-interrupted').locator('summary').click();
    await events([{ method: 'turn/completed', params: { threadId: 'thread-tools', turn: { id: 'interrupt-turn', status: 'interrupted' } } }]);
    await assertState('turn-interrupted', 'interrupted');
    assert.equal(await isOpen('turn-interrupted'), false, 'Turn interruption terminates and collapses a still-running tool');
    await events([itemEvent('item/completed', { id: 'turn-interrupted', type: 'commandExecution' }, 'interrupt-turn')]);
    await assertState('turn-interrupted', 'interrupted');
    assert.equal(await isOpen('turn-interrupted'), false, 'A later sparse item completion preserves the interrupted outcome and closed state');
    await events([itemEvent('item/completed', { id: 'turn-interrupted', type: 'commandExecution', status: 'completed', exitCode: 0 }, 'interrupt-turn')]);
    await assertState('turn-interrupted', 'succeeded');
    assert.equal(await isOpen('turn-interrupted'), false, 'A later explicit successful exit can replace the inherited turn interruption without reopening details');
    await events([itemEvent('item/started', { id: 'turn-unknown', type: 'commandExecution', status: 'inProgress', command: 'wait-for-outcome', aggregatedOutput: 'Waiting.' }, 'unknown-turn')]);
    await events([{ method: 'turn/completed', params: { threadId: 'thread-tools', turn: { id: 'unknown-turn', status: 'completed' } } }]);
    await assertState('turn-unknown', 'completed');
    await events([
      { method: 'turn/started', params: { threadId: 'thread-tools', turn: { id: 'synthetic-turn', status: 'inProgress' } } },
      { method: 'turn/diff/updated', params: { threadId: 'thread-tools', turnId: 'synthetic-turn', diff: '+SYNTHETIC_DIFF_RUNNING' } },
      { method: 'turn/plan/updated', params: { threadId: 'thread-tools', turnId: 'synthetic-turn', plan: [{ step: 'Synthetic plan running', status: 'inProgress' }] } },
    ]);
    const syntheticIDs = ['diff-synthetic-turn', 'plan-synthetic-turn'];
    for (const id of syntheticIDs) await assertState(id, 'running', false);
    await events([{ method: 'turn/completed', params: { threadId: 'thread-tools', turn: { id: 'synthetic-turn', status: 'completed' } } }]);
    for (const id of syntheticIDs) await assertState(id, 'completed');
    await events([
      { method: 'turn/diff/updated', params: { threadId: 'thread-tools', turnId: 'synthetic-turn', diff: '+SYNTHETIC_DIFF_LATE' } },
      { method: 'turn/plan/updated', params: { threadId: 'thread-tools', turnId: 'synthetic-turn', plan: [{ step: 'Synthetic plan late', status: 'completed' }] } },
    ]);
    for (const id of syntheticIDs) {
      await assertState(id, 'completed');
      assert.equal(await isOpen(id), false, id + ' stays collapsed after a late update to the completed turn');
      assert.notEqual(await article(id).locator('.tool-outcome').evaluate(element => getComputedStyle(element).color), successColor, id + ' turn completion does not imply a successful tool outcome');
      await article(id).locator('summary').click();
    }
    assert.match(await details('diff-synthetic-turn').textContent(), /SYNTHETIC_DIFF_LATE/, 'Completed synthetic diff retains its late update');
    assert.match(await details('plan-synthetic-turn').textContent(), /Synthetic plan late/, 'Completed synthetic plan retains its late update');

    // The real font controls cover 129%, 143% and 157% conversation scale.
    // Root font scaling additionally exercises text enlargement at 125%.
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: width === 1440 ? 1000 : 844 });
      for (const fontSize of [18, 20, 22]) {
        await page.evaluate(fontSize => {
          let current = Number(document.querySelector('#font-size').textContent.replace('px', ''));
          while (current !== fontSize) { document.querySelector(current < fontSize ? '#font-increase' : '#font-decrease').click(); current += current < fontSize ? 1 : -1; }
        }, fontSize);
        assert.equal(await page.locator('#font-size').textContent(), fontSize + 'px');
        for (const enlarged of [false, true]) {
          await page.evaluate(enlarged => { document.documentElement.style.fontSize = enlarged ? '125%' : ''; }, enlarged);
          for (const id of ['long-success', 'exit-failure', 'mcp-failure']) {
            await article(id).locator('summary').scrollIntoViewIfNeeded();
            const geometry = await article(id).evaluate(element => {
              const summary = element.querySelector('summary'), title = summary.querySelector('.tool-title'), badge = summary.querySelector('.tool-outcome');
              const line = title.getBoundingClientRect(), bounds = summary.getBoundingClientRect(), outcome = badge.getBoundingClientRect(), style = getComputedStyle(title);
              return { whiteSpace: style.whiteSpace, overflow: style.overflow, ellipsis: style.textOverflow, lineHeight: parseFloat(style.lineHeight), height: line.height, truncates: title.scrollWidth > title.clientWidth,
                oneRow: element.getBoundingClientRect().height <= bounds.height + 2, hiddenHeader: element.querySelector('.message-header').getClientRects().length === 0,
                badgeFits: outcome.left >= bounds.left && outcome.right <= bounds.right + 1 && outcome.width > 0, badgeUnclipped: badge.scrollWidth <= badge.clientWidth + 1,
                noOverlap: line.right <= outcome.left + 1, summaryFits: bounds.left >= -1 && bounds.right <= innerWidth + 1, pageFits: document.documentElement.scrollWidth <= innerWidth + 1 };
            });
            const context = id + ' at ' + width + 'px / ' + fontSize + 'px font / ' + (enlarged ? '125%' : '100%') + ' root text';
            assert.equal(geometry.whiteSpace, 'nowrap', context + ' summary stays on one line');
            assert.equal(geometry.overflow, 'hidden', context + ' summary clips overflow');
            assert.equal(geometry.ellipsis, 'ellipsis', context + ' summary uses ellipsis');
            assert.ok(geometry.height <= geometry.lineHeight + 1, context + ' title occupies one line');
            assert.equal(geometry.oneRow && geometry.hiddenHeader, true, context + ' completed entry occupies one visible row without an extra header');
            assert.equal(geometry.truncates, true, context + ' long title is actually truncated');
            assert.equal(geometry.badgeFits && geometry.badgeUnclipped && geometry.noOverlap, true, context + ' outcome badge stays visible without overlap');
            assert.equal(geometry.summaryFits && geometry.pageFits, true, context + ' does not overflow the page');
          }
          const runningGeometry = await article('running-preview').evaluate(element => {
            const summary = element.querySelector('summary'), title = summary.querySelector('.tool-title');
            return { height: summary.getBoundingClientRect().height, lineHeight: parseFloat(getComputedStyle(summary).lineHeight), ellipsis: getComputedStyle(title).textOverflow };
          });
          assert.ok(runningGeometry.height > runningGeometry.lineHeight * 2, 'Running preview keeps multiple visible lines at ' + width + 'px / ' + fontSize + 'px');
          assert.notEqual(runningGeometry.ellipsis, 'ellipsis', 'Completed single-line truncation does not affect the running preview');
        }
      }
    }
    await page.evaluate(() => { document.documentElement.style.fontSize = ''; while (Number(document.querySelector('#font-size').textContent.replace('px', '')) > 18) document.querySelector('#font-decrease').click(); });
    await article('python-source').locator('summary').click();
    fs.mkdirSync(screenshots, { recursive: true });
    for (const [label, width] of [['desktop', 1440], ['mobile', 390]]) {
      await page.setViewportSize({ width, height: width === 1440 ? 1000 : 844 });
      await article('python-source').evaluate(element => {
        const scroller = document.querySelector('#transcript');
        scroller.scrollTop += element.getBoundingClientRect().top - scroller.getBoundingClientRect().top - 12;
      });
      await page.screenshot({ path: path.join(screenshots, 'webui-tools-' + label + '.png') });
      await article('python-source').locator('summary').click();
      await page.locator('#transcript').evaluate(scroller => { scroller.scrollTop = 0; });
      await page.screenshot({ path: path.join(screenshots, 'webui-tools-' + label + '-compact.png') });
      await article('python-source').locator('summary').click();
      for (const [id, source] of runningSources) {
        assert.equal(await isOpen(id), false, id + ' screenshot shows the closed running preview');
        assert.equal(await article(id).locator('summary .tool-title').textContent(), ('Running ' + source).slice(0, 600), id + ' preserves its highlighted prefix across later updates');
        await article(id).evaluate(element => {
          const scroller = document.querySelector('#transcript');
          scroller.scrollTop += element.getBoundingClientRect().top - scroller.getBoundingClientRect().top - 12;
        });
        await page.screenshot({ path: path.join(screenshots, 'webui-tools-' + id + '-' + label + '.png') });
      }
    }
    await assertSafe();
    assert.deepEqual(errors, [], 'Tool events and highlighting produce no unhandled browser errors');
    assert.deepEqual(externalRequests, [], 'Hostile source cannot request remote resources');
    assert.deepEqual(apiWrites, [], 'The isolated suite never mutates a gateway API');
    assert.equal(await page.evaluate(() => window.testSent.filter(frame => ['turn/start', 'turn/steer'].includes(frame.method)).length), 0, 'The suite never submits a prompt');
    console.log('WebUI tool browser checks passed: compact truthful outcomes, desktop/mobile ellipsis and enlarged fonts, full source/results and native dynamic contentItems, safe Python/JavaScript/JSON highlighting, keyboard disclosure, highlighted closed running previews, one-time collapse, manual choices and stale event/history protection. Screenshots: ' + screenshots);
  } catch (error) {
    console.error('Tool browser diagnostics:', JSON.stringify({ errors, ...await page.evaluate(() => ({ connection: document.querySelector('#connection')?.dataset.state, notice: document.querySelector('#notice')?.textContent, sent: window.testSent?.map(frame => frame.method), articles: document.querySelectorAll('#messages article').length })) }));
    throw error;
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
