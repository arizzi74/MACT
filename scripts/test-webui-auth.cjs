#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const sessionID = '11111111-2222-4333-8444-555555555555';
const selected = { session_id: sessionID, codex_thread_id: 'thread', worker_name: 'Test worker', runtime_name: 'Codex', name: 'Restored conversation', cwd: '/projects/demo', state: 'idle', stats: { model: 'model', reasoning_effort: 'high' } };
(async () => {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1280, height: 850 } });
  let now = Date.now(), expiry = now + 600000, auth = true, loginID = 1, finishCount = 0, sessionChecks = 0;
  let trusted = false, deviceName = '', pendingTrust = {}, holdSessionCheck = false, releaseSessionCheck = null;
  let csrfValue = 'csrf-1', csrfBootstraps = 0;
  let logoutFails = false, holdLogout = false, releaseLogout = null;
  const logoutRequests = [], apiMutations = [];
  const errors = [], posts = [], drafts = new Map(), rotationSockets = [];
  let finishFails = false, activePage = null;
  await context.addCookies([{ name: '__Host-telegramgw-csrf', value: 'csrf-1', url: 'https://auth.test', secure: true, sameSite: 'Strict' }]);
  await context.route('https://auth.test/**', async route => {
    const url = new URL(route.request().url());
    const json = (value, status = 200, headers = {}) => route.fulfill({ status, headers, contentType: 'application/json', body: JSON.stringify(value) });
    if (url.pathname.startsWith('/tgw/api/') && !['GET', 'HEAD'].includes(route.request().method())) apiMutations.push({ path: url.pathname, method: route.request().method() });
    if (url.pathname === '/tgw/api/v1/admin/logout') {
      logoutRequests.push({ method: route.request().method(), csrf: route.request().headers()['x-csrf-token'], body: route.request().postDataJSON() });
      if (route.request().method() !== 'POST') return json({}, 405);
      if (logoutRequests.at(-1).csrf !== csrfValue) return json({ code: 'invalid_csrf' }, 403);
      if (holdLogout) await new Promise(resolve => { releaseLogout = resolve; });
      if (logoutFails) { logoutFails = false; return json({ error: 'Gateway temporarily unavailable' }, 503); }
      auth = false;
      return route.fulfill({ status: 204 });
    }
    if (url.pathname === '/tgw/api/v1/admin/login/options') {
      if (!route.request().headers().cookie?.includes('__Host-telegramgw-csrf=')) {
        csrfValue = 'csrf-expired-login-' + ++csrfBootstraps;
        return json({ passkey: true, password: false }, 200, { 'Set-Cookie': '__Host-telegramgw-csrf=' + csrfValue + '; Path=/; Secure; SameSite=Strict' });
      }
      return json({ passkey: true, password: false });
    }
    if (url.pathname === '/tgw/api/v1/admin/session') {
      sessionChecks++;
      if (holdSessionCheck) await new Promise(resolve => { releaseSessionCheck = resolve; });
      return json({ authenticated: true, owner_id: 'owner', session_id: 'login-' + loginID, device_id: 'device-' + loginID, trusted_device: trusted, device_name: deviceName, browser_label: 'Chrome on Linux', authentication_method: 'passkey', server_time: new Date(now).toISOString(), expires_at: new Date(expiry).toISOString(), reauthenticated_at: new Date(now).toISOString() }, auth && now < expiry ? 200 : 401);
    }
    if (url.pathname === '/tgw/api/v1/admin/login/begin') {
      const csrf = route.request().headers()['x-csrf-token'];
      if (!csrf || csrf !== csrfValue) return json({ code: 'invalid_csrf' }, 403);
      pendingTrust = route.request().postDataJSON(); posts.push({ route: 'begin', csrf, body: pendingTrust });
      return json({ ceremony_id: 'ceremony', publicKey: { challenge: 'AQID', rpId: 'auth.test', allowCredentials: [] } });
    }
    if (url.pathname === '/tgw/api/v1/admin/login/finish') {
      if (activePage) {
        rotationSockets.push(await activePage.evaluate(() => window.authSockets.filter(socket => socket.readyState === 1).length));
        // Simulate revocation frames already queued by the old connection.
        await activePage.evaluate(() => { for (const socket of window.authSockets) socket.originalClose?.({ code: 4401 }); });
      }
      if (finishFails) { finishFails = false; return json({}, 503); }
      posts.push({ route: 'finish', csrf: route.request().headers()['x-csrf-token'], body: route.request().postDataJSON() }); finishCount++; loginID++; auth = true;
      if (typeof pendingTrust.trust_device === 'boolean') { trusted = pendingTrust.trust_device; deviceName = trusted ? pendingTrust.device_name || '' : ''; expiry = now + (trusted ? 30 * 86400000 : 28800000); }
      else if (!trusted) expiry = now + 28800000;
      csrfValue = 'csrf-' + loginID;
      return json({ ok: true }, 200, { 'Set-Cookie': '__Host-telegramgw-csrf=' + csrfValue + '; Path=/; Secure; SameSite=Strict' });
    }
    if (url.pathname.startsWith('/tgw/api/v1/webui/drafts/')) {
      if (!auth) return json({}, 401);
      const id = path.basename(url.pathname), method = route.request().method();
      if (method === 'DELETE') { drafts.delete(id); return route.fulfill({ status: 204 }); }
      if (method === 'PUT') { const value = { ...route.request().postDataJSON(), expires_at: new Date(now + 1800000).toISOString() }; drafts.set(id, value); return json(value); }
      return json(drafts.get(id) || {}, drafts.has(id) ? 200 : 404);
    }
    if (url.pathname === '/tgw/api/v1/webui/sessions') return json({ sessions: [selected] }, auth ? 200 : 401);
    if (url.pathname === '/tgw/api/v1/webui/telegram-bots') return json({ revision: 'fixture-bots', bots: [] });
    if (url.pathname === '/tgw/api/v1/webui/push/config') return json({ supported: false, subscribed: false });
    const filename = url.pathname.endsWith('/diagram-renderer') ? 'webui-mermaid-frame.html' : url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    const headers = filename === 'webui-mermaid-frame.html' ? { 'Content-Security-Policy': "default-src 'none'; base-uri 'none'; frame-ancestors 'self'; form-action 'none'; object-src 'none'; connect-src 'none'; script-src https://auth.test/tgw/webui/static/webui-mermaid-runtime.js; style-src 'unsafe-inline'; img-src 'none'; font-src 'none'; frame-src 'none'; worker-src 'none'; sandbox allow-scripts" } : {};
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', headers, body: fs.readFileSync(path.join(assets, filename)) });
  });
  await context.addInitScript(({ sessionID }) => {
    window.authSockets = []; window.authFrames = []; window.passkeyMode = 'success'; window.holdSend = false;
    Object.defineProperty(navigator, 'credentials', { configurable: true, value: { get: async () => {
      if (window.passkeyMode === 'cancel') throw new DOMException('Cancelled', 'NotAllowedError');
      if (window.passkeyMode === 'hold') await new Promise(resolve => { window.releasePasskey = resolve; });
      return { id: 'passkey', rawId: new Uint8Array([1, 2]), type: 'public-key', response: { clientDataJSON: new Uint8Array([3]), authenticatorData: new Uint8Array([4]), signature: new Uint8Array([5]), userHandle: null }, getClientExtensionResults: () => ({}) };
    } } });
    window.testHidden = false;
    Object.defineProperty(document, 'hidden', { get: () => window.testHidden });
    window.visibility = value => { window.testHidden = value; document.dispatchEvent(new Event('visibilitychange')); };
    const items = Array.from({ length: 60 }, (_, i) => ({ turnId: 'turn', item: { id: 'message-' + i, type: 'agentMessage', text: 'Saved message ' + i + '\n\n' + 'Conversation detail for scroll restoration. '.repeat(22), createdAt: 1780000000 + i } })).reverse();
    class Socket {
      static OPEN = 1;
      constructor(url) {
        this.activity = new URL(url).pathname.endsWith('/activity'); this.readyState = 0; this.sequence = 0;
        window.authSockets.push(this);
        setTimeout(() => { if (this.readyState === 3) return; this.readyState = 1; this.originalClose = this.onclose; this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: [{ session_id: sessionID, state: 'idle', worker_connectivity: 'connected', pending_questions: 0 }] } : { type: 'ready' }); if (this.activity) this.heartbeat = setInterval(() => this.emit({ type: 'heartbeat', version: 3, sequence: 0 }), 15000); }, 5);
      }
      emit(value) { if (value.type === 'activity_snapshot') value = { telegram_bots: [], telegram_bots_revision: 'fixture-bots', ...value }; if (this.readyState === 1) this.onmessage?.({ data: JSON.stringify(value) }); }
      close(code = 1000) { this.readyState = 3; clearInterval(this.heartbeat); this.onclose?.({ code }); }
      send(raw) {
        const frame = JSON.parse(raw); window.authFrames.push(frame);
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: 'thread' }, model: 'model', reasoningEffort: 'high' };
        if (frame.method === 'thread/turns/list') result = { data: [{ id: 'turn', status: 'completed', startedAt: 1780000000 }], nextCursor: null };
        if (frame.method === 'thread/items/list') { const offset = Number(frame.params.cursor || 0); result = { data: items.slice(offset, offset + 20), nextCursor: offset + 20 < items.length ? String(offset + 20) : null }; }
        if (frame.method === 'gateway/questions') result = { questions: [] };
        if (frame.method === 'model/list') result = { data: [] };
        if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
        if (frame.method === 'turn/start' && window.holdSend) return;
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    }
    window.WebSocket = Socket;
  }, { sessionID });
  const page = await context.newPage(); activePage = page; page.on('pageerror', error => errors.push(error.message));
  await page.clock.install({ time: now });
  const ready = async target => {
    try { await target.waitForFunction(() => document.querySelector('#connection').dataset.state === 'connected' && !document.querySelector('#composer').hidden); }
    catch (error) {
      console.error('UI state:', await target.evaluate(() => ({ url: location.href, connection: document.querySelector('#connection').textContent, authHidden: document.querySelector('#auth').hidden, authStatus: document.querySelector('#auth-status').textContent, notice: document.querySelector('#notice').textContent, sessions: document.querySelectorAll('.session-button').length, sockets: window.authSockets.map(socket => ({ activity: socket.activity, readyState: socket.readyState })), frames: window.authFrames.map(frame => frame.method) })));
      throw error;
    }
  };
  const viewerCount = () => page.evaluate(() => window.authSockets.filter(value => !value.activity).length);
  const anchor = target => target.evaluate(() => { const view = document.querySelector('#transcript'); const item = [...document.querySelector('#messages').children].find(row => row.getBoundingClientRect().bottom > view.getBoundingClientRect().top); return { id: item?.dataset.itemId, offset: Math.round(item.getBoundingClientRect().top - view.getBoundingClientRect().top) }; });
  try {
    await page.goto('https://auth.test/tgw/webui/?session_id=' + sessionID); await ready(page);
    assert.equal(await page.locator('.session-auth-banner').isHidden(), true);
    await page.locator('#prompt').fill('An unsent private draft');
    now += 300050; await page.clock.fastForward(300050);
    await page.waitForSelector('.session-auth-banner:not([hidden])');
    await page.evaluate(() => { window.passkeyMode = 'cancel'; });
    await page.locator('.session-auth-banner button:not(.session-auth-password)').click();
    await page.waitForFunction(() => document.querySelector('.session-auth-status').textContent.includes('cancelled'));
    assert.equal(finishCount, 0, 'Cancelling a passkey does not renew authentication');
    assert.equal(await page.locator('#prompt').inputValue(), 'An unsent private draft');
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 20 && !document.querySelector('#older').hidden && !document.querySelector('#older').disabled);
    // Scrolling an offscreen history button into view already loads a page.
    // Use the reader's upward wheel action so click retries cannot page twice.
    await page.locator('#transcript').hover();
    await page.mouse.wheel(0, -await page.locator('#transcript').evaluate(element => element.scrollHeight));
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 40);
    assert.deepEqual(await page.evaluate(() => window.authFrames.filter(frame => frame.method === 'thread/items/list' && frame.params.cursor).map(frame => frame.params.cursor)), ['20'], 'Upward history navigation loads exactly one earlier page');
    await page.evaluate(() => { const view = document.querySelector('#transcript'); view.dispatchEvent(new WheelEvent('wheel', { deltaY: -1 })); view.scrollTop = 1600; });
    const before = await anchor(page);
    finishFails = true;
    const socketsBeforeFailure = await viewerCount();
    await page.evaluate(() => { window.passkeyMode = 'success'; });
    await page.locator('.session-auth-banner button:not(.session-auth-password)').click();
    await page.waitForFunction(count => window.authSockets.filter(socket => !socket.activity).length > count, socketsBeforeFailure); await ready(page);
    assert.equal(finishCount, 0, 'A failed finish does not create a login');
    assert.equal(await page.locator('#prompt').inputValue(), 'An unsent private draft', 'Finish failure reconnects the still-authorized view without losing its draft');
    assert.equal(await page.locator('.session-auth-banner').isVisible(), true);
    const socketsBefore = await viewerCount();
    await page.evaluate(() => { window.passkeyMode = 'hold'; });
    await page.locator('.session-auth-banner button:not(.session-auth-password)').click();
    await page.waitForFunction(() => typeof window.releasePasskey === 'function');
    assert.equal(await page.locator('#open-logout').isDisabled(), true, 'Logout is disabled while passkey renewal is pending');
    await page.locator('#open-logout').dispatchEvent('click');
    assert.equal(await page.locator('#logout-dialog').isHidden(), true, 'A queued sidebar click cannot start logout during renewal');
    await page.evaluate(() => { window.passkeyMode = 'success'; window.releasePasskey(); delete window.releasePasskey; });
    await page.waitForFunction(count => window.authSockets.filter(socket => !socket.activity).length > count, socketsBefore); await ready(page);
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 40);
    assert.equal(await viewerCount(), socketsBefore + 1, 'Renewal reconnects the selected session using the rotated cookie');
    assert.deepEqual(await anchor(page), before, 'Renewal restores the earlier loaded history anchor');
    assert.equal(await page.locator('#prompt').inputValue(), 'An unsent private draft', 'Early renewal preserves an already authorized in-memory draft');
    assert.equal(await page.locator('.session-auth-banner').isHidden(), true);
    assert.equal(finishCount, 1);
    assert.ok(rotationSockets.every(count => count === 0), 'Both old sockets detach before server cookie rotation; queued revocation callbacks cannot cancel login');
    assert.equal(posts.at(-1).body.credential.response.signature, 'BQ');
    assert.equal(await page.evaluate(() => document.cookie.includes('csrf-2')), true);

    await page.locator('#open-settings').click();
    auth = false;
    await page.evaluate(() => window.visibility(false));
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.locator('#settings-dialog').isHidden(), true, 'Expired authentication closes Settings so sign-in remains reachable');
    assert.equal(await page.locator('#messages article').count(), 0);
    assert.equal(await page.locator('#prompt').inputValue(), '');
    assert.equal(await page.locator('.session-button').count(), 0);
    assert.equal(await page.evaluate(() => window.authSockets.every(socket => socket.readyState === 3)), true);
    assert.equal(await page.evaluate(() => Object.keys(sessionStorage).length), 0, 'Default expiry retains no drafts or conversation in browser storage');
    assert.equal(await page.locator('#auth-trust-device').isChecked(), false, 'Trust is an unchecked choice on the sign-in screen');
    assert.equal(await page.locator('#auth-device-name-field').isHidden(), true);
    await page.locator('#auth-login').click(); await ready(page);
    assert.equal(posts.findLast(row => row.route === 'begin').body.trust_device, false, 'Ordinary sign-in does not opt into persistent trust');
    assert.equal(expiry - now, 8 * 3600000, 'Ordinary sign-in uses an eight-hour session');
    await page.waitForFunction(() => document.querySelectorAll('#messages article').length === 40);
    assert.deepEqual(await anchor(page), before, 'Expired login resumes the selected session and previous scroll anchor');
    assert.equal(await page.locator('#prompt').inputValue(), '');
    assert.equal(posts.at(-1).csrf, 'csrf-2', 'Renewal reads the current CSRF cookie for each ceremony');

    await page.evaluate(() => { window.holdSend = true; });
    await page.locator('#prompt').fill('A prompt with an uncertain result'); await page.locator('#send').click();
    await page.waitForFunction(() => window.authFrames.some(frame => frame.method === 'turn/start'));
    const sends = await page.evaluate(() => window.authFrames.filter(frame => frame.method === 'turn/start').length);
    auth = false; await page.evaluate(() => window.authSockets.findLast(socket => !socket.activity).close(4401));
    await page.waitForSelector('#auth:not([hidden])'); await page.locator('#auth-login').click(); await ready(page);
    assert.equal(await page.evaluate(() => window.authFrames.filter(frame => frame.method === 'turn/start').length), sends, 'Unlock never replays an uncertain prompt');
    await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('Nothing was resent'));
    assert.equal(await page.locator('#prompt').inputValue(), '');

    await page.locator('#prompt').fill('A draft explicitly saved for recovery');
    await page.locator('#open-settings').click();
    await page.locator('#draft-recovery-enabled').check();
    await page.waitForFunction(() => document.querySelector('#draft-recovery-status').textContent.includes('is on'));
    await page.locator('#close-settings').click();
    await page.locator('#open-settings').click();
    assert.equal(await page.locator('#draft-recovery-enabled').isChecked(), true, 'Closing and reopening Settings preserves the recovery preference');
    while (!drafts.size) await new Promise(resolve => setTimeout(resolve, 10));
    const encrypted = [...drafts.values()][0].ciphertext;
    assert.ok(!encrypted.includes('explicitly saved'));
    assert.ok(!(await page.evaluate(() => JSON.stringify({ ...sessionStorage }))).includes('explicitly saved'), 'No plaintext draft enters browser storage');
    auth = false; await page.evaluate(() => window.visibility(false)); await page.waitForSelector('#auth:not([hidden])');
    await page.locator('#auth-login').click(); await ready(page);
    await page.waitForSelector('#draft-recovery-offer:not([hidden])');
    await page.locator('#open-settings').click();
    assert.equal(await page.locator('#draft-recovery-enabled').isChecked(), true, 'The Settings recovery preference survives renewed authentication');
    await page.locator('#close-settings').click();
    assert.equal(await page.locator('#prompt').inputValue(), '', 'Recovery always waits for an explicit restore action');
    await page.locator('#restore-drafts').click();
    assert.equal(await page.locator('#prompt').inputValue(), 'A draft explicitly saved for recovery');
    assert.equal(await page.evaluate(() => window.authFrames.filter(frame => frame.method === 'turn/start').length), sends, 'Restoring a draft never submits it');
    await page.locator('#prompt').fill('An uncertain encrypted prompt');
    await page.locator('#send').click();
    await page.waitForFunction(count => window.authFrames.filter(frame => frame.method === 'turn/start').length > count, sends);
    auth = false; await page.evaluate(() => window.authSockets.findLast(socket => !socket.activity).close(4401)); await page.waitForSelector('#auth:not([hidden])');
    await page.locator('#auth-login').click(); await ready(page);
    assert.equal(await page.locator('#prompt').inputValue(), '');
    assert.equal(await page.locator('#draft-recovery-offer').isHidden(), true, 'A send with unknown outcome cannot reappear as a saved unsent draft');
    await page.locator('#open-settings').click();
    await page.locator('#draft-recovery-enabled').uncheck();
    await page.locator('#close-settings').click();

    const other = await context.newPage(); other.on('pageerror', error => errors.push(error.message));
    await other.goto('https://auth.test/tgw/webui/?session_id=' + sessionID); await ready(other);
    const otherBefore = await other.evaluate(() => window.authSockets.filter(socket => !socket.activity).length);
    // Simulate a login in an operations-console tab. Its non-secret hint must
    // cause both web views to fetch the authoritative new session themselves.
    loginID++; expiry = now + 28800000;
    const primaryBeforeBroadcast = await viewerCount();
    await other.evaluate(() => { const channel = new BroadcastChannel('codex-gateway-auth'); channel.postMessage({ kind: 'renewed', nonce: 'test' }); channel.close(); });
    await page.waitForFunction(count => window.authSockets.filter(socket => !socket.activity).length > count, primaryBeforeBroadcast);
    await ready(page);
    await other.waitForFunction(count => window.authSockets.filter(socket => !socket.activity).length > count, otherBefore);
    assert.ok(sessionChecks > 2, 'Every tab verifies renewal hints against the server');
    auth = false;
    await other.evaluate(() => { const channel = new BroadcastChannel('codex-gateway-auth'); channel.postMessage({ kind: 'logout' }); channel.close(); });
    await page.waitForSelector('#auth:not([hidden])');
    assert.equal(await page.locator('#messages article').count(), 0, 'Logout in another tab clears the transcript');
    await other.close();

    await page.setViewportSize({ width: 390, height: 844 });
    await page.locator('#auth-login').click();
    await page.locator('.session-button').waitFor({ state: 'attached' });
    assert.equal(await page.locator('.session-button[aria-current="true"]').count(), 0, 'Cross-tab logout also forgets the previously selected session');
    await page.locator('#show-sessions').click();
    await page.locator('.session-button[data-session-id="' + sessionID + '"]').click(); await ready(page);
    await page.evaluate(() => window.visibility(true));
    const checksBefore = sessionChecks; auth = false;
    await page.evaluate(() => window.visibility(false)); await page.waitForSelector('#auth:not([hidden])');
    assert.ok(sessionChecks > checksBefore, 'Mobile foreground always rechecks server authentication');
    await page.locator('#auth-login').click(); await ready(page);
    await page.evaluate(() => window.visibility(true));
    // OS sleep can advance wall time while performance.now is paused. The
    // foreground handler must clear content before waiting for a network check.
    // The fake browser clock keeps running while the mocked server time is
    // fixed. Advance both by the same duration to cross the local deadline.
    const suspendedMS = expiry - now + 1000;
    const resumedWall = await page.evaluate(() => Date.now()) + suspendedMS;
    now += suspendedMS; auth = false; await page.clock.setSystemTime(resumedWall);
    const messagesAfterForeground = await page.evaluate(() => { window.visibility(false); return document.querySelectorAll('#messages article').length; });
    assert.equal(messagesAfterForeground, 0, 'Suspended mobile expiry fails closed using wall time before the network check');
    await page.waitForSelector('#auth:not([hidden])');

    // Thirty days exceeds the signed 32-bit browser timer limit. Capture the
    // requested delays, capping only invalid values so a regression reports an
    // assertion instead of creating an immediate-timeout loop in Chromium.
    await page.evaluate(() => {
      window.trustTimeoutDelays = [];
      const nativeSetTimeout = window.setTimeout;
      window.setTimeout = (callback, delay, ...args) => {
        window.trustTimeoutDelays.push(Number(delay) || 0);
        return nativeSetTimeout(callback, Math.min(Number(delay) || 0, 0x7fffffff), ...args);
      };
    });
    assert.equal(await page.locator('#auth-trust-device').isChecked(), false, 'Trust is not remembered after the previous session expires');
    await page.locator('#auth-trust-device').check();
    await page.locator('#auth-device-name').fill('My personal phone');
    await page.locator('#auth-login').click(); await ready(page);
    const trustedDeadline = expiry;
    assert.equal(trusted, true);
    assert.equal(expiry - now, 30 * 86400000, 'Explicit passkey trust receives a thirty-day absolute deadline');
    assert.deepEqual(posts.findLast(row => row.route === 'begin').body, { trust_device: true, device_name: 'My personal phone' }, 'The explicit trust choice and optional device name are bound at ceremony creation');
    assert.ok((await page.evaluate(() => window.trustTimeoutDelays)).every(delay => delay <= 0x7fffffff), 'Thirty-day trust never overflows the browser timer limit');
    assert.equal(await page.locator('.session-auth-banner').isHidden(), true);
    // Playwright's own clock accepts a signed 32-bit step; cross the browser
    // checkpoint in two valid steps without overflowing the test harness.
    for (const days of [20, 5]) { now += days * 86400000; await page.clock.fastForward(days * 86400000); }
    await page.evaluate(() => window.visibility(false)); await ready(page);
    assert.equal(expiry, trustedDeadline, 'Foreground verification does not silently extend the trusted-device deadline');
    assert.equal(await page.locator('.session-auth-banner').isHidden(), true, 'A long-lived trusted session stays usable after the first timer checkpoint');
    await page.locator('#prompt').fill('Private trusted-device draft');
    if (await page.locator('#show-sessions').getAttribute('aria-expanded') !== 'true') await page.locator('#show-sessions').click();
    await page.locator('#open-settings').click();
    await page.evaluate(() => window.visibility(true));
    const trustedSuspendedMS = expiry - now + 1000;
    const trustedResumedWall = await page.evaluate(() => Date.now()) + trustedSuspendedMS;
    now += trustedSuspendedMS; auth = false; holdSessionCheck = true;
    await page.clock.setSystemTime(trustedResumedWall);
    const trustedAfterForeground = await page.evaluate(() => {
      window.visibility(false);
      return { messages: document.querySelectorAll('#messages article').length, draft: document.querySelector('#prompt').value, sessions: document.querySelectorAll('.session-button').length, settingsOpen: document.querySelector('#settings-dialog').open, socketsClosed: window.authSockets.every(socket => socket.readyState === 3) };
    });
    assert.deepEqual(trustedAfterForeground, { messages: 0, draft: '', sessions: 0, settingsOpen: false, socketsClosed: true }, 'Trusted-device expiry clears private state synchronously before the foreground network reply');
    while (!releaseSessionCheck) await new Promise(resolve => setTimeout(resolve, 10));
    holdSessionCheck = false; releaseSessionCheck(); releaseSessionCheck = null;
    await page.waitForSelector('#auth:not([hidden])');
    // The browser may delete both thirty-day cookies while an installed web
    // app remains open. Its next login must bootstrap CSRF without a reload.
    await context.clearCookies();
    await page.evaluate(() => { window.expiryPageSentinel = 'same loaded page'; });
    const beforeCookieBootstrap = csrfBootstraps;
    await page.locator('#auth-login').click(); await ready(page);
    assert.ok(csrfBootstraps > beforeCookieBootstrap, 'An expired CSRF cookie is reacquired through login options before sign-in');
    assert.match(posts.findLast(row => row.route === 'begin').csrf, /^csrf-expired-login-/);
    assert.equal(await page.evaluate(() => window.expiryPageSentinel), 'same loaded page', 'Cookie-expired sign-in succeeds without reloading the installed web app');
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);

    // Explicit logout has its own confirmation and must leave active worker
    // work alone. The only intentional mutations are auth revocation and
    // deleting this browser's optional encrypted draft recovery copy.
    await page.setViewportSize({ width: 1280, height: 850 });
    const openSidebar = async target => {
      if (await target.locator('#show-sessions').isVisible() && await target.locator('#show-sessions').getAttribute('aria-expanded') !== 'true') await target.locator('#show-sessions').click();
    };
    const attachPrivateFile = async (target, file) => {
      const chooser = target.waitForEvent('filechooser'); await target.locator('#attach-image').click();
      await (await chooser).setFiles(file);
    };
    const privateState = target => target.evaluate(() => ({
      draft: document.querySelector('#prompt').value,
      selected: document.querySelector('.session-button[aria-current="true"]')?.dataset.sessionId,
      messages: document.querySelectorAll('#messages article').length,
      sockets: window.authSockets.filter(socket => socket.readyState === 1).length,
      turn: document.querySelector('#turn-state').textContent,
      attachment: document.querySelector('#file-description').textContent,
      recovery: JSON.parse(sessionStorage.getItem('codex-webui-encrypted-text-draft-v1') || 'null')?.key,
    }));
    const erased = async (target, label) => {
      await target.waitForSelector('#auth:not([hidden])');
      assert.deepEqual(await target.evaluate(() => ({
        draft: document.querySelector('#prompt').value,
        messages: document.querySelectorAll('#messages article').length,
        sessions: document.querySelectorAll('.session-button').length,
        questions: document.querySelectorAll('#questions > *').length,
        socketsClosed: window.authSockets.every(socket => socket.readyState === 3),
        imageHidden: document.querySelector('#image-preview').hidden,
        fileHidden: document.querySelector('#file-preview').hidden,
        imageInput: document.querySelector('#image-file').value,
        recovery: sessionStorage.getItem('codex-webui-encrypted-text-draft-v1'),
        settingsOpen: document.querySelector('#settings-dialog').open,
        logoutOpen: document.querySelector('#logout-dialog').open,
      })), { draft: '', messages: 0, sessions: 0, questions: 0, socketsClosed: true, imageHidden: true, fileHidden: true, imageInput: '', recovery: null, settingsOpen: false, logoutOpen: false }, label + ' erases private browser content and closes transports and dialogs');
    };
    const logoutScreenshot = async name => {
      if (!process.env.WEBUI_AUTH_SCREENSHOTS) return;
      fs.mkdirSync(process.env.WEBUI_AUTH_SCREENSHOTS, { recursive: true });
      await page.screenshot({ path: path.join(process.env.WEBUI_AUTH_SCREENSHOTS, name + '.png') });
    };
    await openSidebar(page);
    assert.deepEqual(await page.locator('.sidebar-footer > button').allTextContents(), ['Settings', 'Log out'], 'Log out follows Settings in the sidebar footer');
    await page.locator('#prompt').fill('Private draft kept when logout is cancelled');
    await attachPrivateFile(page, { name: 'private-notes.txt', mimeType: 'text/plain', buffer: Buffer.from('Unsent private attachment') });
    await page.locator('#file-preview').waitFor();
    await page.locator('#open-settings').click();
    await page.locator('#draft-recovery-enabled').check();
    await page.locator('#close-settings').click();
    await page.waitForFunction(() => !!sessionStorage.getItem('codex-webui-encrypted-text-draft-v1'));
    await page.evaluate(() => {
      window.logoutHints = [];
      window.logoutObserver = new BroadcastChannel('codex-gateway-auth');
      window.logoutObserver.onmessage = event => window.logoutHints.push(event.data);
      window.authSockets.findLast(socket => !socket.activity && socket.readyState === 1).emit({ method: 'turn/started', params: { threadId: 'thread', turn: { id: 'running-during-logout', status: 'inProgress' } } });
    });
    await page.waitForFunction(() => document.querySelector('#turn-state').textContent === 'Working');
    await logoutScreenshot('desktop-sidebar');
    const logoutBefore = await privateState(page), framesBeforeLogout = await page.evaluate(() => window.authFrames.length), mutationsBeforeLogout = apiMutations.length;
    for (const cancel of ['button', 'Escape']) {
      await page.locator('#open-logout').click();
      assert.equal(await page.getByRole('dialog', { name: 'Log out?', exact: true }).isVisible(), true);
      assert.equal(await page.locator('#logout-cancel').evaluate(element => document.activeElement === element), true, 'Confirmation focuses Cancel');
      if (cancel === 'button') await logoutScreenshot('desktop-confirmation');
      if (cancel === 'button') await page.locator('#logout-cancel').click(); else await page.keyboard.press('Escape');
      assert.equal(await page.locator('#logout-dialog').isHidden(), true);
      assert.equal(await page.locator('#open-logout').evaluate(element => document.activeElement === element), true, 'Cancelling restores focus to the sidebar action');
      assert.deepEqual(await privateState(page), logoutBefore, cancel + ' cancellation preserves auth, the selected session, running turn, attachment, text draft, and recovery key');
      assert.equal(logoutRequests.length, 0, 'Opening or cancelling confirmation never calls logout');
    }
    assert.equal(await page.evaluate(() => window.authFrames.length), framesBeforeLogout, 'Opening or cancelling logout sends no worker commands');
    assert.equal(apiMutations.length, mutationsBeforeLogout, 'Opening or cancelling logout makes no API mutations');
    logoutFails = true;
    await page.locator('#open-logout').click(); await page.locator('#logout-confirm').click();
    await page.waitForFunction(() => !document.querySelector('#logout-error').hidden && !!document.querySelector('#logout-error').textContent && !document.querySelector('#logout-confirm').disabled);
    assert.equal(await page.locator('#logout-dialog').isVisible(), true, 'Server failure keeps confirmation open for retry');
    assert.deepEqual(await privateState(page), logoutBefore, 'Server failure preserves the authorized conversation, running work, and unsent drafts');
    assert.equal(auth, true);
    assert.deepEqual(await page.evaluate(() => window.logoutHints), [], 'A failed logout never broadcasts revocation');
    assert.deepEqual(logoutRequests[0], { method: 'POST', csrf: csrfValue, body: {} }, 'Logout uses the existing authenticated endpoint with CSRF');

    const sibling = await context.newPage(); sibling.on('pageerror', error => errors.push(error.message));
    await sibling.goto('https://auth.test/tgw/webui/?session_id=' + sessionID);
    // Clock installation applies to the entire context. Resume its inherited
    // clock after the long expiry jumps so new-page socket timers can run.
    await page.clock.resume();
    await sibling.locator('.session-button').waitFor({ state: 'attached' });
    await page.clock.runFor(1000); await ready(sibling);
    await sibling.locator('#prompt').fill('Private sibling draft');
    await attachPrivateFile(sibling, { name: 'private-image.png', mimeType: 'image/png', buffer: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jZzoAAAAASUVORK5CYII=', 'base64') });
    await sibling.locator('#image-preview').waitFor();
    await sibling.waitForFunction(() => !!sessionStorage.getItem('codex-webui-encrypted-text-draft-v1'));
    await sibling.locator('#open-settings').click();
    csrfValue = 'csrf-logout-retry';
    await context.addCookies([{ name: '__Host-telegramgw-csrf', value: csrfValue, url: 'https://auth.test', secure: true, sameSite: 'Strict' }]);
    holdLogout = true;
    await page.locator('#logout-confirm').click();
    await page.waitForFunction(() => document.querySelector('#logout-confirm').disabled);
    assert.equal(await page.locator('#logout-cancel').isDisabled(), true, 'Pending confirmation cannot be cancelled midway through revocation');
    // Dispatch directly as well as pressing Enter: a disabled button alone is
    // insufficient protection against duplicate handlers or queued actions.
    await page.locator('#logout-confirm').dispatchEvent('click'); await page.keyboard.press('Enter'); await page.keyboard.press('Escape');
    assert.equal(await page.locator('#logout-dialog').isVisible(), true, 'Escape cannot hide a pending logout request');
    assert.equal(logoutRequests.length, 2, 'Repeated confirmation while pending sends only one retry');
    assert.deepEqual(logoutRequests[1], { method: 'POST', csrf: csrfValue, body: {} }, 'Retry reads the current CSRF cookie');
    assert.deepEqual(await privateState(page), logoutBefore, 'A pending request leaves the current session intact until the server responds');
    assert.ok(releaseLogout, 'The server received the held request');
    const checksBeforeConfirmedLogout = sessionChecks;
    holdLogout = false; releaseLogout(); releaseLogout = null;
    await erased(page, 'Confirmed logout'); await erased(sibling, 'Sibling logout');
    assert.ok(sessionChecks > checksBeforeConfirmedLogout, 'The sibling tab verifies the logout hint against the server');
    assert.equal(await page.evaluate(() => window.logoutHints.filter(hint => hint.kind === 'logout').length), 1, 'Successful logout broadcasts one sibling hint');
    assert.equal(await page.evaluate(() => window.authFrames.length), framesBeforeLogout, 'Logging out a running turn sends no interrupt, disconnect, or other worker RPC');
    assert.ok(apiMutations.slice(mutationsBeforeLogout).every(request => request.path === '/tgw/api/v1/admin/logout' || request.path.startsWith('/tgw/api/v1/webui/drafts/')), 'Logout never mutates a worker, runtime, or session');
    await sibling.close();
    assert.equal(new URL(page.url()).searchParams.has('session_id'), false, 'Explicit logout removes the previous selected session from the URL');
    await page.locator('#auth-login').click();
    await page.waitForSelector('.session-button');
    await page.waitForSelector('#auth', { state: 'hidden' });
    assert.equal(await page.locator('.session-button[aria-current="true"]').count(), 0, 'Signing in after logout requires choosing a session again');
    assert.equal(await page.locator('#prompt').inputValue(), '', 'Signing in after logout does not restore an unsent private draft');
    assert.equal(await page.locator('#draft-recovery-offer').isHidden(), true, 'Logout discards the key needed to recover previous drafts');
    await page.locator('.session-button[data-session-id="' + sessionID + '"]').click(); await ready(page);

    await page.setViewportSize({ width: 390, height: 844 }); await openSidebar(page);
    const mobileLogout = await page.evaluate(() => {
      const settings = document.querySelector('#open-settings').getBoundingClientRect(), logout = document.querySelector('#open-logout').getBoundingClientRect();
      return { below: logout.top >= settings.bottom, fits: logout.left >= 0 && logout.right <= innerWidth && logout.bottom <= innerHeight, height: logout.height, noOverflow: document.documentElement.scrollWidth <= innerWidth };
    });
    assert.equal(mobileLogout.below && mobileLogout.fits && mobileLogout.noOverflow, true, 'Mobile logout stays visible below Settings without horizontal overflow');
    assert.ok(mobileLogout.height >= 44, 'Mobile logout has an accessible touch target');
    await logoutScreenshot('mobile-sidebar');
    await page.locator('#open-logout').click();
    assert.equal(await page.locator('#logout-dialog').evaluate(element => { const rect = element.getBoundingClientRect(); return rect.left >= 0 && rect.right <= innerWidth && rect.top >= 0 && rect.bottom <= innerHeight; }), true, 'Mobile confirmation fits the viewport');
    await logoutScreenshot('mobile-confirmation');
    await page.locator('#logout-cancel').click();
    assert.equal(await page.locator('#auth').isHidden(), true, 'Mobile Cancel keeps authentication');
    await page.waitForFunction(() => document.activeElement === document.querySelector('#show-sessions'));
    assert.equal(await page.locator('#show-sessions').evaluate(element => document.activeElement === element), true, 'Mobile Cancel returns focus to the closed drawer trigger');
    await openSidebar(page);
    await page.locator('#open-logout').click(); await page.locator('#logout-confirm').click();
    await erased(page, 'Mobile logout');
    assert.equal(logoutRequests.length, 3);
    assert.equal(await page.locator('#open-logout').isDisabled(), true, 'The logout action is disabled after signing out');
    assert.deepEqual(errors, []);
    console.log('Web UI auth checks passed: warning, passkey cancel/finish-failure/rotation-race/renewal, CSRF rotation/bootstrap after cookie expiry, history anchor restore, expiry erasure/Settings dismissal, encrypted opt-in/Settings preference persistence/explicit recovery/no uncertain-send replay, cross-tab renewal/logout, optional eight-hour/thirty-day trust, safe long timers, fixed trust deadlines, foreground expiry erasure before network replies, and desktop/mobile confirmed logout with cancellation, failure/retry, duplicate protection, CSRF, private-state erasure, sibling propagation, and uninterrupted worker work.');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
