#!/usr/bin/env node
// Exercise the production formatter in a browser without adding runtime dependencies.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const csp = "default-src 'self'; base-uri 'none'; object-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'";
const fixture = '<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/webui.css"><script defer src="/webui-format.js"></script></head><body><article class="message tool" id="code"></article></body></html>';

(async () => {
  const server = http.createServer((request, response) => {
    if (request.url === '/favicon.ico') { response.writeHead(204); response.end(); return; }
    const filename = request.url === '/webui-format.js' ? 'webui-format.js' : request.url === '/webui.css' ? 'webui.css' : '';
    if (!filename && request.url !== '/') { response.writeHead(404); response.end(); return; }
    response.writeHead(200, { 'Content-Type': filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', 'Content-Security-Policy': csp });
    response.end(filename ? fs.readFileSync(path.join(assets, filename)) : fixture);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = 'http://127.0.0.1:' + server.address().port;
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();
  const errors = [], remoteRequests = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('request', request => { if (!request.url().startsWith(origin)) remoteRequests.push(request.url()); });
  const render = (source, hint = '', markdown = false) => page.evaluate(({ source, hint, markdown }) => {
    const pre = markdown ? window.CodexFormat.code(source, hint) : window.CodexFormat.toolCode(source, hint);
    document.querySelector('#code').replaceChildren(pre);
    const code = pre.querySelector('code');
    return {
      text: code.textContent, language: pre.dataset.language || '', label: pre.querySelector('.code-label')?.textContent || '',
      tokens: Array.from(code.querySelectorAll('span'), span => ({ text: span.textContent, language: span.dataset.language, kind: span.className })),
      unsafeElements: code.querySelectorAll('script, img, iframe, svg, a, style').length,
    };
  }, { source, hint, markdown });
  const assertSource = (result, source) => assert.equal(result.text, source, 'Highlighting retains every character of the cleaned source');
  const token = (result, text, language, kind = 'syntax-keyword') => assert.ok(result.tokens.some(value => value.text === text && value.language === language && value.kind === kind), language + ' highlights ' + JSON.stringify(text));
  try {
    await page.goto(origin);
    await page.waitForFunction(() => !!window.CodexFormat?.toolCode);
    const wrapped = 'bash -lc "python3 - <<\'PY\'\nimport json\nfor item in [1, 2]:\n    print(json.dumps({\"value\": item}))\nPY"';
    let result = await render(wrapped);
    assertSource(result, wrapped); assert.equal(result.language, 'shell');
    token(result, 'import', 'python'); token(result, 'for', 'python'); token(result, '1', 'python', 'syntax-number');
    assert.ok(result.tokens.filter(value => value.kind === 'syntax-string').every(value => !value.text.includes('import json')), 'The wrapper quote cannot turn the embedded Python program into one shell string');

    const node = "node <<'JS'\nconst pattern = /[<'\"]+/g;\nconsole.log(`value ${1}`); // keep the source\nJS\n";
    result = await render(node, 'shell'); assertSource(result, node);
    token(result, 'const', 'javascript'); token(result, '/[<\'\"]+/g', 'javascript', 'syntax-string'); token(result, '`value ${1}`', 'javascript', 'syntax-string');
    const tabbed = "python3 - <<-'PY'\n\t# a tabbed script\n\tprint(42)\n\tPY\nprintf 'done\\n'\n";
    result = await render(tabbed); assertSource(result, tabbed);
    token(result, '# a tabbed script', 'python', 'syntax-comment'); token(result, '42', 'python', 'syntax-number'); token(result, 'printf', 'shell');
    const nested = "bash <<'SH'\npython3 <<'PY'\nimport sys\nprint(sys.version)\nPY\nnode <<'JS'\nconst ready = true;\nJS\nSH\n";
    result = await render(nested); assertSource(result, nested);
    token(result, 'import', 'python'); token(result, 'const', 'javascript');
    const multiple = "cat <<'PY' <<'JS'\nimport os\nPY\nconst count = 2;\nJS\n";
    result = await render(multiple); assertSource(result, multiple);
    token(result, 'import', 'python'); token(result, 'const', 'javascript');

    for (const [source, hint, language, keyword] of [
      ['import json\nprint(json.dumps({"ok": True}))', '', 'python', 'import'],
      ['const result = await Promise.resolve(42);', '', 'javascript', 'const'],
      ['package main\nfunc main() { println("ready") }', '', 'go', 'func'],
      ['{"cmd":"python3 - <<\'PY\'\\nprint(1)\\nPY", "ok":true}', '', 'json', 'true'],
      ['[1, {"ok": false, "message": "<script>alert(1)</script>"}]', 'output', 'json', 'false'],
    ]) {
      result = await render(source, hint); assertSource(result, source); assert.equal(result.language, language); token(result, keyword, language);
    }
    for (const source of ['ok\n2 files changed', 'go: downloading example.invalid/module v1.2.3', 'git status completed successfully', '{"partial":']) {
      result = await render(source, 'output'); assertSource(result, source); assert.equal(result.tokens.length, 0, 'Ambiguous terminal output remains plain');
    }
    result = await render('echo "plain even with a command"', 'plain'); assert.equal(result.tokens.length, 0);
    result = await render('@@ -1 +1 @@\n-old\n+new\n', 'patch');
    assertSource(result, '@@ -1 +1 @@\n-old\n+new\n');
    assert.ok(result.tokens.some(value => value.kind === 'diff-add' && value.text === '+new\n'), 'Tool diffs keep their line endings');

    const hostile = '<img src="https://attacker.invalid/pixel" onerror="window.injected=true">\n<script>window.injected=true</script>\nconst text = "</code><svg onload=window.injected=true>";';
    result = await render(hostile, 'javascript'); assertSource(result, hostile); assert.equal(result.unsafeElements, 0);
    assert.equal(await page.evaluate(() => !!window.injected), false, 'Code markup cannot execute');
    const incomplete = [
      "bash -lc \"python3 - <<'PY'\nimport os\nprint('still streaming",
      'const regex = /unterminated[\\' + 'x'.repeat(20000),
      'const text = "' + '\\'.repeat(20001),
      '"'.repeat(22001),
      "python3 <<'PY'\n" + "'''" + 'x'.repeat(20000),
    ];
    for (const source of incomplete) { result = await render(source, source.startsWith('const') || source.startsWith('"') ? 'javascript' : 'shell'); assertSource(result, source); }
    result = await render(incomplete[0], 'shell'); token(result, 'import', 'python');

    for (const source of ['const answer = 42;\n'.repeat(30000), 'true '.repeat(5000)]) {
      result = await render(source, 'javascript'); assertSource(result, source); assert.equal(result.tokens.length, 0, 'Large or dense code falls back to complete plain text');
    }
    result = await render('\x1b[32mconst value = 42;\x1b[0m\n\x1b]0;title\x07done\x00', 'javascript');
    assertSource(result, 'const value = 42;\ndone'); token(result, 'const', 'javascript');
    result = await render('def ready():\n    return True\n', 'python', true);
    assertSource(result, 'def ready():\n    return True\n'); token(result, 'def', 'python');
    assert.deepEqual(remoteRequests, [], 'Formatting never loads code, markup, or a remote highlighter');
    assert.deepEqual(errors, [], 'Malformed and streamed source produces no browser exceptions');
    console.log('Tool code browser checks passed: shell commands, quoted and nested Python/Node heredocs, JSON, Go, exact safe source, plain ambiguous output, incomplete quotes/regex, bounded large fallback and shared Markdown highlighting.');
  } finally { await browser.close(); await new Promise(resolve => server.close(resolve)); }
})().catch(error => { console.error(error); process.exitCode = 1; });
