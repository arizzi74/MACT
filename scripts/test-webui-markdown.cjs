#!/usr/bin/env node
// Run the production formatter and real assistant/plan/reasoning UI in Chromium.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const csp = "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; object-src 'none'; connect-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; worker-src 'self'; manifest-src 'self'";
const hostile = '<img src=https://attacker.invalid/pixel onerror="window.injected=true"><script>window.injected=true</script>';
const session = { session_id: 'markdown', codex_thread_id: 'thread-markdown', worker_id: 'worker', worker_name: 'Worker', runtime_id: 'runtime', runtime_name: 'Codex', name: 'Markdown checks', cwd: '/projects/demo', state: 'idle' };

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 850 } });
  const errors = [], remote = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('request', request => { if (!request.url().startsWith('https://markdown.test/')) remote.push(request.url()); });
  await page.route('https://markdown.test/**', route => {
    const url = new URL(route.request().url());
    const json = body => route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
    if (url.pathname.endsWith('/admin/session')) return json({ authenticated: true, owner_id: 'owner', session_id: 'login', expires_at: new Date(Date.now() + 28800000).toISOString(), server_time: new Date().toISOString() });
    if (url.pathname.endsWith('/login/options')) return json({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return json({ sessions: [session], workers: [], runtimes: [] });
    if (url.pathname.endsWith('/telegram-bots')) return json({ bots: [], revision: '1' });
    if (url.pathname.endsWith('/push/config')) return json({ supported: false, subscribed: false });
    const filename = url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ headers: { 'content-security-policy': csp }, contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(session => {
    const source = '['.repeat(128 * 1024);
    const items = [{ id: 'assistant', type: 'agentMessage', text: source }, { id: 'reasoning', type: 'reasoning', summary: [source] }, { id: 'plan', type: 'plan', text: source }];
    window.markdownSent = [];
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.url = String(url); this.readyState = 0; this.activity = this.url.includes('/activity');
        if (!this.activity) window.markdownSocket = this;
        setTimeout(() => { this.readyState = 1; this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: [{ ...session, active_turn_id: '', pending_questions: 0 }], telegram_bots: [], telegram_bots_revision: '1' } : { type: 'ready' }); }, 1);
      }
      emit(frame) { this.onmessage?.({ data: JSON.stringify(frame) }); }
      close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
      send(raw) {
        const frame = JSON.parse(raw); if (!frame.method) return;
        window.markdownSent.push(frame);
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: session.codex_thread_id }, model: 'codex' };
        else if (frame.method === 'thread/turns/list') result = { data: [{ id: 'turn', status: 'completed', items }], nextCursor: null };
        else if (frame.method === 'thread/items/list') result = { data: items.map(item => ({ turnId: 'turn', item })), nextCursor: null };
        else if (frame.method === 'gateway/questions') result = { questions: [] };
        else if (frame.method === 'model/list') result = { data: [] };
        else if (frame.method === 'thread/goal/get') result = { goal: null };
        else if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
        else if (frame.method === 'turn/start') result = { turn: { id: 'next', status: 'inProgress', items: [] } };
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    };
  }, session);
  try {
    await page.goto('https://markdown.test/tgw/webui/?session_id=markdown');
    await page.locator('[data-session-id="markdown"]').click();
    await page.waitForSelector('#connection[data-state="connected"]');
    await page.locator('[data-item-id="plan"] .tool-summary').click();
    await page.waitForFunction(() => document.querySelectorAll('#messages .markdown-plain').length === 3);
    for (const id of ['assistant', 'reasoning', 'plan']) {
      assert.equal(await page.locator('[data-item-id="' + id + '"] .markdown-plain').textContent(), '['.repeat(128 * 1024), id + ' uses the complete inert fallback through the real UI');
    }
    // These are message updates from the transport, not a plain-text user echo.
    await page.evaluate(() => {
      for (const [id, type] of [['assistant', 'agentMessage'], ['reasoning', 'reasoning'], ['plan', 'plan']]) {
        const source = 'Text ' + '['.repeat(32 * 1024);
        window.markdownSocket.emit({ method: 'item/completed', params: { threadId: 'thread-markdown', turnId: 'turn', item: { id, type, text: source, ...(type === 'reasoning' ? { summary: [source] } : {}) } } });
      }
    });
    await page.waitForFunction(() => [...document.querySelectorAll('#messages .markdown-plain')].filter(node => node.textContent.startsWith('Text ')).length === 3);
    const interaction = Date.now();
    await page.locator('#prompt').fill('The composer still works.');
    await page.locator('#send').click();
    await page.waitForFunction(() => window.markdownSent.some(frame => frame.method === 'turn/start'));
    assert.ok(Date.now() - interaction < 2000, 'The composer remains usable after hostile assistant, reasoning and plan rendering');

    const timings = await page.evaluate(hostile => {
      const sizes = [8, 16, 32, 64, 128].map(kib => kib * 1024);
      const cases = {
        brackets: size => '['.repeat(size),
        emphasis: size => 'Text ' + '*'.repeat(size),
        underscores: size => 'Text ' + '_'.repeat(size),
        backticks: size => 'Text ' + '`'.repeat(size),
        fence: size => '`'.repeat(size - 2) + ' X',
        tablePadding: size => '| A | B |\n' + ' '.repeat(size) + ':',
        escapes: size => '\\['.repeat(size / 2),
        destination: size => '[link](' + '\\'.repeat(size),
        terminal: size => '\x1b]'.repeat(size / 2),
        links: size => '[safe](https://example.com/docs) '.repeat(Math.ceil(size / 32)),
        html: size => hostile.repeat(Math.ceil(size / hostile.length)),
      };
      const measurements = {};
      for (const [name, make] of Object.entries(cases)) {
        measurements[name] = [];
        for (const size of sizes) {
          const source = make(size), samples = [];
          for (let repeat = 0; repeat < 3; repeat++) {
            const start = performance.now();
            const result = window.CodexFormat.markdown(source);
            samples.push(performance.now() - start);
            if (result.querySelector('script, img, iframe, svg, style, a[href^="javascript:"]')) throw new Error('Active markup in ' + name);
            if (result.querySelectorAll('*').length > 12000) throw new Error('Unbounded DOM in ' + name);
            if (name !== 'links' && result.textContent !== window.CodexFormat.clean(source)) throw new Error('Lost source text in ' + name);
          }
          measurements[name].push(samples.sort((a, b) => a - b)[1]);
        }
      }
      return measurements;
    }, hostile);
    for (const [name, values] of Object.entries(timings)) {
      assert.ok(Math.max(...values) < 1000, name + ' stays below the conservative 1s renderer threshold: ' + JSON.stringify(values));
      assert.ok(values.at(-1) <= values[0] * 24 + 100, name + ' growth stays bounded from 8 to 128 KiB: ' + JSON.stringify(values));
    }
    const normal = await page.evaluate(hostile => {
      const root = document.createElement('article'); root.className = 'message';
      root.append(window.CodexFormat.markdown('# Heading\n\n**bold** _emphasis_ ~~removed~~ `code` [site](https://example.com/path?q=1) [mail](mailto:test@example.com) [file](<./my report (final).md>) [bad](javascript:alert(1))\n\n' + hostile + '\n\n> quote\n\n- item\n\n| A | B |\n| --- | --- |\n| first | second |\n\n```js\nconst answer = 42;\n```'));
      document.querySelector('#messages').replaceChildren(root);
      return { strong: root.querySelector('strong')?.textContent, em: root.querySelector('em')?.textContent, removed: root.querySelector('s')?.textContent, code: root.querySelector('code')?.textContent, heading: !!root.querySelector('h2'), table: !!root.querySelector('table'), quote: !!root.querySelector('blockquote'), list: !!root.querySelector('li'), hrefs: [...root.querySelectorAll('a')].map(a => a.getAttribute('href')), file: root.querySelector('.file-link')?.dataset.filePath, active: root.querySelectorAll('script, img, iframe, svg, style').length, injected: !!window.injected };
    }, hostile);
    assert.deepEqual(normal, { strong: 'bold', em: 'emphasis', removed: 'removed', code: 'code', heading: true, table: true, quote: true, list: true, hrefs: ['https://example.com/path?q=1', 'mailto:test@example.com', '#file', 'https://attacker.invalid/pixel'], file: './my report (final).md', active: 0, injected: false });
    assert.deepEqual(remote, [], 'Markup and links do not fetch remote content during rendering');
    assert.deepEqual(errors, [], 'Adversarial Markdown produces no browser exceptions');
    console.log('Markdown browser checks passed: 8/16/32/64/128 KiB brackets, delimiters, escapes, destinations, OSC, links and HTML; bounded time/DOM, inert full-text fallback, normal Markdown, production assistant/plan/reasoning updates and usable composer. Median milliseconds: ' + JSON.stringify(timings));
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
