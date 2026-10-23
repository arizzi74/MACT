#!/usr/bin/env node
// Scope and shared-session regression using the real application assets.
// Browser engines cannot reproduce macOS installed-app/Safari handoff. Check
// that final OS behavior manually; these checks cover the navigation contract.
// CONSOLE_NAV_WEBKIT=1 also checks mobile WebKit; =only runs just that engine.
// Playwright WebKit omits Cookie from intercepted request headers, even when
// a real HTTPS server receives it. Its fixture reads the URL-filtered cookie
// jar instead; Chromium verifies actual intercepted Cookie headers.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium, webkit, devices } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const origin = 'https://console-navigation.test';
const appPath = '/tgw/webui/';
const consolePath = appPath + 'admin/';
const sessionID = '11111111-2222-4333-8444-555555555555';
const conversationPath = appPath + '?session_id=' + sessionID;
const csrf = 'console-navigation-csrf';
const adminCookie = '__Host-telegramgw-admin';
const manifestPath = appPath + 'manifest.webmanifest';
const manifest = JSON.parse(fs.readFileSync(path.join(assets, 'webui-manifest.webmanifest'), 'utf8'));
const session = { session_id: sessionID, codex_thread_id: 'thread', worker_name: 'Fixture worker', runtime_name: 'Codex', name: 'Navigation conversation', cwd: '/projects/demo', state: 'idle' };
const securityHeaders = {
  'Content-Security-Policy': "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'; object-src 'none'; connect-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; worker-src 'self'; manifest-src 'self'",
  'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff', 'Referrer-Policy': 'no-referrer',
};
const cookieValue = (headers, name) => (headers.cookie || '').split(';').map(value => value.trim()).find(value => value.startsWith(name + '='))?.slice(name.length + 1) || '';

async function checkNavigation(engine, options, label) {
  const browser = await engine.launch({ headless: true });
  try {
    const context = await browser.newContext({ ...options, serviceWorkers: 'block' });
    const errors = [], cspViolations = [], unexpectedRequests = [], authenticatedReads = [], authChecks = [];
    let activeSession = '', signIns = 0, passkeyPrompts = 0, signOuts = 0, createdPages = 0;
    context.on('page', page => { createdPages++; page.on('pageerror', error => errors.push(error.message)); });
    await context.exposeBinding('recordPasskeyPrompt', () => { passkeyPrompts++; });
    await context.exposeBinding('recordCSPViolation', (_source, directive) => { cspViolations.push(directive); });
    await context.addInitScript(({ sessionID }) => {
      document.addEventListener('securitypolicyviolation', event => window.recordCSPViolation(event.violatedDirective));
      Object.defineProperty(navigator, 'credentials', { configurable: true, value: { get: async () => {
        await window.recordPasskeyPrompt();
        return { id: 'passkey', rawId: new Uint8Array([1]), type: 'public-key', response: { clientDataJSON: new Uint8Array([2]), authenticatorData: new Uint8Array([3]), signature: new Uint8Array([4]), userHandle: null }, getClientExtensionResults: () => ({}) };
      } } });
      class Socket {
        static OPEN = 1;
        constructor(url) {
          this.activity = new URL(url).pathname.endsWith('/activity'); this.readyState = 0;
          setTimeout(() => {
            if (this.readyState === 3) return;
            this.readyState = 1;
            this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: [{ session_id: sessionID, state: 'idle', worker_connectivity: 'connected', pending_questions: 0 }], telegram_bots: [], telegram_bots_revision: 'fixture-bots' } : { type: 'ready' });
          }, 5);
        }
        emit(value) { if (this.readyState === 1) this.onmessage?.({ data: JSON.stringify(value) }); }
        close(code = 1000) { this.readyState = 3; this.onclose?.({ code }); }
        send(raw) {
          const frame = JSON.parse(raw);
          let result = {};
          if (frame.method === 'thread/resume') result = { thread: { id: 'thread' }, model: 'model', reasoningEffort: 'high' };
          if (frame.method === 'thread/turns/list') result = { data: [{ id: 'turn', status: 'completed', startedAt: 1780000000 }], nextCursor: null };
          if (frame.method === 'thread/items/list') result = { data: [{ turnId: 'turn', item: { id: 'message', type: 'agentMessage', text: 'Private fixture conversation.' } }], nextCursor: null };
          if (frame.method === 'gateway/questions') result = { questions: [] };
          if (frame.method === 'model/list') result = { data: [] };
          if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
          setTimeout(() => this.emit({ id: frame.id, result }), 1);
        }
      }
      window.WebSocket = Socket;
    }, { sessionID });
    await context.route(origin + '/**', async route => {
      const request = route.request(), url = new URL(request.url()), headers = await request.allHeaders();
      const cookieSource = engine === webkit && !headers.cookie ? 'cookie-jar' : 'request-header';
      const cookie = cookieSource === 'cookie-jar'
        ? (await context.cookies(request.url())).find(value => value.name === adminCookie)?.value || ''
        : cookieValue(headers, adminCookie);
      const authenticated = !!activeSession && cookie === activeSession;
      if (url.pathname.startsWith('/tgw/api/')) authChecks.push({ path: url.pathname, authenticated, hasCookie: !!cookie, cookieSource });
      const json = (value, status = 200, extraHeaders = {}) => route.fulfill({ status, contentType: 'application/json', headers: { ...securityHeaders, ...extraHeaders }, body: status === 204 ? '' : JSON.stringify(value) });
      if (url.pathname === '/tgw/api/v1/admin/login/options') return json({ passkey: true, password: false });
      if (url.pathname === '/tgw/api/v1/admin/login/begin' || url.pathname === '/tgw/api/v1/admin/login/finish' || url.pathname === '/tgw/api/v1/admin/logout') {
        assert.equal(request.method(), 'POST');
        assert.equal(headers['x-csrf-token'], csrf, 'Sign-in and sign-out use the shared CSRF cookie');
        if (url.pathname.endsWith('/begin')) return json({ ceremony_id: 'fixture-ceremony', publicKey: { challenge: 'AQID', rpId: 'console-navigation.test' } });
        if (url.pathname.endsWith('/finish')) {
          activeSession = 'fixture-login-' + ++signIns;
          return json({ ok: true }, 200, { 'Set-Cookie': adminCookie + '=' + activeSession + '; Path=/; Secure; HttpOnly; SameSite=Strict' });
        }
        assert.equal(authenticated, true, 'Console sign-out carries the original authenticated cookie');
        signOuts++; activeSession = '';
        return json(null, 204, { 'Set-Cookie': adminCookie + '=; Path=/; Secure; HttpOnly; SameSite=Strict; Max-Age=0' });
      }
      if (url.pathname.startsWith('/tgw/api/')) {
        if (!authenticated) return json({}, 401);
        authenticatedReads.push({ path: url.pathname, cookie, cookieSource });
        const now = Date.now();
        const responses = {
          '/tgw/api/v1/admin/session': { authenticated: true, owner_id: 'fixture-owner', session_id: activeSession, authentication_method: 'passkey', server_time: new Date(now).toISOString(), expires_at: new Date(now + 28800000).toISOString(), reauthenticated_at: new Date(now).toISOString() },
          '/tgw/api/v1/admin/dashboard': { workers: [], runtimes: [], sessions: [], pending_approvals: 0, queued_commands: 0 },
          '/tgw/api/v1/admin/passkeys': [],
          '/tgw/api/v1/admin/sessions': { sessions: [] },
          '/tgw/api/v1/admin/password': { enabled: false, username: '' },
          '/tgw/api/v1/admin/audio': { settings: { enabled: false, model: 'base', threads: 1, reply_with_audio: false, tts_voice: 'auto', tts_engine: '' }, models: [], resources: {}, tts: {} },
          '/tgw/api/v1/admin/telegram-bots': { bots: [], pending: [] },
          '/tgw/api/v1/webui/sessions': { workers: [], runtimes: [], sessions: [session] },
          '/tgw/api/v1/webui/telegram-bots': { revision: 'fixture-bots', bots: [] },
          '/tgw/api/v1/webui/push/config': { supported: false, subscribed: false },
        };
        if (Object.hasOwn(responses, url.pathname)) return json(responses[url.pathname]);
      }
      let filename;
      if (url.pathname === appPath) filename = 'webui.html';
      else if (url.pathname === consolePath || url.pathname === '/tgw/admin/') filename = 'index.html';
      else if (url.pathname === manifestPath) filename = 'webui-manifest.webmanifest';
      else if (url.pathname.startsWith('/tgw/webui/static/') || url.pathname.startsWith('/tgw/admin/static/')) filename = path.basename(url.pathname);
      const file = filename && path.join(assets, filename);
      if (!file || !fs.existsSync(file)) {
        unexpectedRequests.push(url.pathname);
        return route.fulfill({ status: 404, body: '' });
      }
      const contentTypes = { '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png', '.webmanifest': 'application/manifest+json', '.html': 'text/html' };
      return route.fulfill({ contentType: contentTypes[path.extname(filename)], headers: { ...securityHeaders, ...(!cookieValue(headers, '__Host-telegramgw-csrf') ? { 'Set-Cookie': '__Host-telegramgw-csrf=' + csrf + '; Path=/; Secure; SameSite=Strict' } : {}) }, body: fs.readFileSync(file) });
    });
    const page = await context.newPage();
    const ready = async target => {
      try {
        await target.waitForFunction(() => document.querySelector('#connection').dataset.state === 'connected' && !document.querySelector('#composer').hidden);
      } catch (error) {
        console.error(label + ' readiness:', { signIns, passkeyPrompts, errors, cspViolations, unexpectedRequests, authChecks, cookies: await context.cookies(origin), state: await target.evaluate(() => ({ connection: document.querySelector('#connection')?.textContent, auth: document.querySelector('#auth-status')?.textContent })) });
        throw error;
      }
    };
    const openConsole = async () => {
      if (!await page.locator('#open-settings').isVisible()) await page.locator('#show-sessions').click();
      await page.locator('#open-settings').click();
      const link = page.getByRole('link', { name: 'Open operations console', exact: true });
      assert.equal(await link.getAttribute('href'), consolePath, 'The console stays inside the installed app scope');
      assert.ok(['', '_self', null].includes(await link.getAttribute('target')), 'The console reuses the current window');
      await link.click();
      await page.waitForURL(origin + consolePath);
      await page.waitForSelector('#console:not([hidden])');
      assert.equal(await page.locator('#auth').isHidden(), true, 'The existing chat session also authorizes the console');
      assert.equal(await page.locator('link[rel=manifest]').getAttribute('href'), manifestPath, 'The console keeps the installed MACT identity');
    };
    const assertManageDestination = async (target, returnPath = conversationPath) => {
      const destination = new URL(await target.locator('#auth-manage').getAttribute('href'), origin);
      assert.equal(destination.origin, origin);
      assert.equal(destination.pathname, consolePath, 'Sign-in management stays inside the installed app scope');
      assert.equal(destination.searchParams.get('next'), returnPath, 'Sign-in management uses the current return path');
    };

    await page.goto(origin + conversationPath);
    await page.waitForSelector('#auth:not([hidden])');
    await assertManageDestination(page);
    await page.locator('#auth-login').click();
    await ready(page);
    const initialCookie = (await context.cookies(origin)).find(cookie => cookie.name === adminCookie);
    assert.ok(initialCookie, 'Chat sign-in establishes an authenticated cookie');
    assert.deepEqual({ path: initialCookie.path, secure: initialCookie.secure, httpOnly: initialCookie.httpOnly, sameSite: initialCookie.sameSite }, { path: '/', secure: true, httpOnly: true, sameSite: 'Strict' });
    assert.equal(await page.evaluate(name => document.cookie.includes(name + '='), adminCookie), false, 'Authentication stays in the HttpOnly cookie');
    assert.equal(await page.locator('link[rel=manifest]').getAttribute('href'), manifestPath);
    const installedManifest = await page.evaluate(async url => (await fetch(url)).json(), manifestPath);
    for (const key of ['id', 'scope', 'start_url']) assert.equal(installedManifest[key], appPath, 'The existing manifest ' + key + ' stays stable');
    assert.equal(new URL(consolePath, origin).pathname.startsWith(manifest.scope), true);

    await openConsole();
    assert.equal(createdPages, 1, 'Opening the console creates no browser page');
    assert.equal(passkeyPrompts, 1, 'Opening the console does not request another passkey');
    assert.equal(signIns, 1);
    assert.ok(authenticatedReads.some(read => read.path === '/tgw/api/v1/admin/dashboard' && read.cookie === initialCookie.value && read.cookieSource === (engine === webkit ? 'cookie-jar' : 'request-header')), 'The console shares the chat session cookie with the unchanged admin API');
    await page.getByRole('link', { name: 'Open MACT', exact: true }).click();
    await page.waitForURL(origin + appPath);
    await page.waitForSelector('.session-button', { state: 'attached' });
    assert.equal(await page.locator('#auth').isHidden(), true);
    assert.equal(createdPages, 1, 'Returning to MACT reuses the current window');
    assert.equal(passkeyPrompts, 1);
    assert.equal((await context.cookies(origin)).find(cookie => cookie.name === adminCookie).value, initialCookie.value);

    // Keep one additional conversation open to verify the real cross-tab
    // session-auth notification, in addition to the same-window round trip.
    const conversation = await context.newPage();
    await conversation.goto(origin + conversationPath);
    await ready(conversation);
    assert.equal(await conversation.locator('#messages article').count(), 1, 'The open conversation contains private history before sign-out');
    await conversation.locator('#prompt').fill('Private unsent text');
    await openConsole();
    await page.locator('#logout').click();
    await page.waitForSelector('#auth:not([hidden])');
    await conversation.waitForSelector('#auth:not([hidden])');
    assert.equal(signOuts, 1);
    assert.equal((await context.cookies(origin)).some(cookie => cookie.name === adminCookie), false, 'Console sign-out removes the shared session cookie');
    assert.equal(await conversation.locator('#messages article').count(), 0, 'Console sign-out clears the conversation in another MACT window');
    assert.equal(await conversation.locator('#prompt').inputValue(), '', 'Console sign-out clears private composer text');
    assert.equal(await conversation.locator('.session-button').count(), 0);
    assert.equal(passkeyPrompts, 1, 'Sign-out never starts a sign-in ceremony');
    await page.getByRole('link', { name: 'Open MACT', exact: true }).click();
    await page.waitForURL(origin + appPath);
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(createdPages, 2, 'Sign-out and return create no additional page');

    await assertManageDestination(conversation, appPath);
    await conversation.locator('#auth-manage').click();
    await conversation.waitForURL(url => url.pathname === consolePath && url.searchParams.get('next') === appPath);
    await conversation.waitForSelector('#auth:not([hidden])');
    assert.equal(passkeyPrompts, 1, 'Opening sign-in management waits for an explicit sign-in');
    await conversation.locator('#login').click();
    await conversation.waitForURL(origin + appPath);
    await conversation.waitForSelector('.session-button', { state: 'attached' });
    assert.equal(await conversation.locator('#auth').isHidden(), true);
    assert.equal(await conversation.locator('#messages article').count(), 0, 'Explicit logout clears the previous conversation selection');
    assert.equal(signIns, 2, 'The explicit console sign-in reuses the shared authentication API');
    assert.equal(passkeyPrompts, 2);
    assert.equal(createdPages, 2, 'Sign-in returns to MACT in the same window');
    assert.deepEqual(errors, []);
    assert.deepEqual(cspViolations, [], 'Navigation works with strict production-style CSP');
    assert.deepEqual(unexpectedRequests, []);
    await context.close();
    console.log(label + ': console navigation, shared cookie, return path, and sign-out passed');
  } finally {
    await browser.close();
  }
}

(async () => {
  if (process.env.CONSOLE_NAV_WEBKIT !== 'only') {
    await checkNavigation(chromium, { viewport: { width: 1280, height: 850 } }, 'Desktop Chromium');
    await checkNavigation(chromium, { ...devices['Pixel 7'] }, 'Mobile Chromium');
  }
  if (['1', 'only'].includes(process.env.CONSOLE_NAV_WEBKIT)) {
    console.log('WebKit fixture checks its cookie jar; Chromium checks intercepted Cookie headers.');
    await checkNavigation(webkit, { ...devices['iPhone 13'] }, 'Mobile WebKit');
  }
  console.log('macOS installed-app/Safari handoff still requires an installed-app smoke check.');
})().catch(error => { console.error(error); process.exitCode = 1; });
