#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { EventEmitter } = require('node:events');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const hostile = '<img src=x onerror="window.injected=true">';

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  await page.clock.install({ time: Date.now() });
  await page.context().addCookies([{ name: '__Host-telegramgw-csrf', value: 'session-test-csrf', url: 'https://webui.test/', secure: true }]);
  const errors = [], requests = [], statuses = new Map();
  const routedPosts = new EventEmitter();
  page.on('pageerror', error => errors.push(error.message));
  const workers = [{ ID: 'w', Name: 'Worker ' + hostile, Enabled: true, Connectivity: 'connected', SupportsWebUISessionCreation: true, SupportsSessionEditing: true, SupportsSessionArchives: true }, { ID: 'old', Name: 'Older worker', Enabled: true, Connectivity: 'connected', SupportsWebUISessionCreation: true }];
  const runtimes = [{ worker_id: 'w', runtime_id: 'r', generation: 4, name: 'Codex', state: 'running' }, { worker_id: 'w', runtime_id: 'other', generation: 9, name: 'Other Codex', state: 'running' }, { worker_id: 'old', runtime_id: 'old', generation: 1, state: 'running' }];
  const makeSession = id => ({ session_id: id, codex_thread_id: 'thread-' + id, worker_id: 'w', worker_name: workers[0].Name, runtime_id: 'r', runtime_name: 'Codex', name: 'Session ' + id, cwd: '/home/demo/CODEX/' + id, state: 'idle' });
  let sessions = ['a', 'b', 'c', 'd'].map(makeSession);
  sessions.push({ ...makeSession('old'), worker_id: 'old', runtime_id: 'old' });
  const archive = { id: 'project-20261018.tar.xz', name: 'project-20261018.tar.xz', session_name: 'Archived ' + hostile, original_path: '/home/demo/CODEX/' + 'long-project-'.repeat(18), created_at: '2026-10-18T10:00:00Z', bytes: 2048, codex_version: 'codex-cli 0.163.0' };
  const behaviors = {}, heldReplies = new Map(); let emptyArchiveList = false;
  await page.route('https://webui.test/**', async route => {
    const url = new URL(route.request().url()), method = route.request().method();
    const reply = (body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
    if (url.pathname.endsWith('/admin/session')) return reply({ authenticated: true, owner_id: 'owner', session_id: 'login', expires_at: new Date(Date.now() + 28800000).toISOString(), server_time: new Date().toISOString() });
    if (url.pathname.endsWith('/login/options')) return reply({ passkey: true, password: false });
    if (url.pathname.endsWith('/webui/sessions')) return reply({ workers, runtimes, sessions });
    if (url.pathname.endsWith('/telegram-bots')) return reply({ bots: [], revision: '1' });
    if (url.pathname.endsWith('/push/config')) return reply({ supported: false, subscribed: false });
    const kind = url.pathname.endsWith('/session-archives') ? 'list' : url.pathname.match(/\/sessions\/(edit|archive|restore)$/)?.[1];
    if (kind) {
      const body = method === 'POST' ? route.request().postDataJSON() : Object.fromEntries(url.searchParams);
      const recorded = { kind, method, csrf: route.request().headers()['x-csrf-token'], ...body };
      requests.push(recorded);
      if (method === 'POST') {
        const target = sessions.find(row => row.session_id === body.session_id);
        const status = { command_id: body.request_id, worker_id: target?.worker_id || body.worker_id, runtime_id: target?.runtime_id || body.runtime_id, runtime_generation: target ? 4 : body.runtime_generation,
          ...(target ? { session_id: target.session_id } : {}), pending: kind !== 'list', status: kind === 'list' ? 'completed' : 'pending', message: kind === 'list' ? 'Archives loaded.' : 'Worker operation queued.', archive_directory: '/home/demo/CODEX_Archived' };
        if (kind === 'list' && !emptyArchiveList) {
          status.archives = Number(body.offset || 0) > 0 ? [{ ...archive, id: 'second.tar.xz', name: 'second.tar.xz', session_name: 'Second archive' }] : [archive];
          if (!body.offset) status.next_offset = 1;
        }
        statuses.set(body.request_id, status);
        routedPosts.emit('post', recorded);
        if (behaviors[kind] === 'incompatible-http') { delete behaviors[kind]; return reply({ error_code: 'codex_method_unsupported', message: 'SECRET arbitrary native backend error' }, 400); }
        if (behaviors[kind] === 'hold') { delete behaviors[kind]; heldReplies.set(body.request_id, { route, status }); return; }
        if (behaviors[kind] === 'drop') { delete behaviors[kind]; return route.abort('failed'); }
        return reply(status, kind === 'list' ? 200 : 202);
      }
      return reply(statuses.get(body.request_id) || { message: 'Request not found.' }, statuses.has(body.request_id) ? 200 : 404);
    }
    const filename = url.pathname.startsWith('/tgw/webui/static/') || url.pathname.startsWith('/tgw/admin/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(assets, filename)) });
  });
  await page.addInitScript(() => {
    window.testSockets = [];
    window.WebSocket = class {
      static OPEN = 1;
      constructor(url) {
        this.readyState = 0; this.url = String(url); this.activity = this.url.includes('/activity');
        if (!this.activity) window.testSockets.push(this); else window.testActivitySocket = this;
        setTimeout(() => { this.readyState = 1; this.onopen?.({}); this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, sessions: [], telegram_bots: [], telegram_bots_revision: '1' } : { type: 'ready' }); }, 1);
      }
      emit(message) { this.onmessage?.({ data: JSON.stringify(message) }); }
      close() { this.readyState = 3; this.onclose?.({ code: 1000 }); }
      send(value) {
        const frame = JSON.parse(value);
        if (frame.type === 'ping') return;
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: frame.params.threadId, turns: [] }, model: 'codex', reasoningEffort: 'high' };
        else if (frame.method === 'thread/items/list' || frame.method === 'thread/turns/list' || frame.method === 'model/list') result = { data: [], nextCursor: null };
        else if (frame.method === 'gateway/questions') result = { questions: [] };
        else if (frame.method === 'thread/goal/get') result = { goal: null };
        else if (frame.method === 'account/rateLimits/read') result = { rateLimits: null };
        setTimeout(() => this.emit({ id: frame.id, result }), 1);
      }
    };
  });
  const pencil = id => page.locator('[data-edit-session-id="' + id + '"]');
  const selected = id => page.locator('[data-session-id="' + id + '"]');
  const choose = async id => {
    if (await page.locator('#show-sessions').isVisible() && await page.locator('#show-sessions').getAttribute('aria-expanded') !== 'true') await page.locator('#show-sessions').click();
    await selected(id).click(); await page.waitForSelector('#connection[data-state="connected"]');
  };
  const openEditor = async id => { await pencil(id).click(); await page.locator('#edit-session-dialog').waitFor(); };
  const posts = kind => requests.filter(row => row.kind === kind && row.method === 'POST');
  const waitPost = async (kind, action) => {
    let listener, timeout;
    const routed = new Promise((resolve, reject) => {
      listener = request => { if (request.kind === kind && request.method === 'POST') resolve(request); };
      routedPosts.on('post', listener);
      timeout = setTimeout(() => reject(new Error('Timed out waiting for the fixture to handle a ' + kind + ' POST')), 30000);
    });
    try {
      // Browser request events precede route handling. Wait for this fixture's
      // exact recorded POST, including its status, even for held/lost replies.
      const [request] = await Promise.all([routed, Promise.resolve().then(action)]);
      return request;
    } finally {
      clearTimeout(timeout); routedPosts.off('post', listener);
    }
  };
  const complete = (request, extra = {}) => {
    const status = statuses.get(request.request_id);
    if (request.kind === 'edit') { const row = sessions.find(row => row.session_id === request.session_id); row.name = request.name; status.session = { ...row }; }
    else if (request.kind === 'archive') { const row = sessions.find(row => row.session_id === request.session_id); status.session = { ...row, archived: true }; status.archive = archive; sessions = sessions.filter(row => row.session_id !== request.session_id); }
    else if (request.kind === 'restore') { const row = { ...makeSession('restored'), runtime_id: request.runtime_id }; sessions.push(row); status.session = row; }
    Object.assign(status, { status: 'completed', pending: false, message: 'Operation completed.' }, extra);
  };
  try {
    await page.goto('https://webui.test/tgw/webui/'); await pencil('b').waitFor(); await choose('a');
    const sockets = await page.evaluate(() => window.testSockets.length);
    await openEditor('b');
    assert.equal(await page.locator('#edit-session-name').inputValue(), 'Session b');
    assert.equal(await page.locator('#edit-session-path').textContent(), '/home/demo/CODEX/b');
    assert.equal(await page.locator('#edit-session-close').evaluate(element => element === document.activeElement), true);
    assert.equal(await page.locator('#edit-session-worker img').count(), 0);
    assert.equal(await page.evaluate(() => window.testSockets.length), sockets, 'Editing another session never attaches its transcript');
    await page.locator('#edit-session-name').fill('Renamed / project 😀'); behaviors.edit = 'drop';
    const edit = await waitPost('edit', () => page.locator('#edit-session-save').click());
    assert.deepEqual({ session_id: edit.session_id, name: edit.name, confirmed: edit.confirmed, csrf: edit.csrf }, { session_id: 'b', name: 'Renamed / project 😀', confirmed: true, csrf: 'session-test-csrf' });
    assert.equal(await selected('b').locator('.name').textContent(), 'Session b', 'A dispatched edit does not optimistically rename inventory');
    await page.waitForFunction(() => document.querySelector('#edit-session-message').textContent.includes('not been confirmed'));
    await page.locator('#edit-session-cancel').click(); complete(edit); await page.clock.fastForward(1800);
    await page.waitForFunction(() => document.querySelector('[data-session-id="b"] .name').textContent === 'Renamed / project 😀');
    assert.equal(await selected('a').getAttribute('aria-current'), 'true');
    assert.equal(posts('edit').length, 1, 'Dropped rename acknowledgement is recovered with GET only');
    assert.equal(sessions.find(row => row.session_id === 'b').cwd, '/home/demo/CODEX/b', 'Renaming never changes the project path');

    await openEditor('a'); await page.locator('#edit-session-name').fill('Current title');
    const currentEdit = await waitPost('edit', () => page.locator('#edit-session-save').click()); complete(currentEdit);
    const confirmed = { ...statuses.get(currentEdit.request_id).session };
    statuses.get(currentEdit.request_id).session.session_id = 'b'; await page.clock.fastForward(1800);
    assert.equal(await page.locator('#session-title').textContent(), 'Session a', 'A mismatched confirmed result cannot rename the selected session');
    assert.equal(await page.locator('#connection-text').textContent(), 'Connected · Session a', 'An unconfirmed rename keeps the current connection label');
    statuses.get(currentEdit.request_id).session = confirmed; await page.clock.fastForward(1800);
    await page.waitForFunction(() => document.querySelector('#session-title').textContent === 'Current title');
    assert.equal(await page.locator('#connection-text').textContent(), 'Connected · Current title', 'A confirmed rename updates the connected session label');
    await page.locator('#edit-session-cancel').click();

    sessions.find(row => row.session_id === 'a').name = 'Renamed from another client';
    await page.locator('#refresh-sessions').click();
    await page.waitForFunction(() => document.querySelector('#session-title').textContent === 'Renamed from another client');
    assert.equal(await page.locator('#connection-text').textContent(), 'Connected · Renamed from another client', 'An inventory rename updates the connected session label');
    assert.equal(await page.evaluate(() => window.testSockets.length), sockets, 'Renaming the selected session preserves its existing connection');

    await openEditor('b');
    const unsupportedArchive = await waitPost('archive', () => page.locator('#edit-session-archive').click());
    Object.assign(statuses.get(unsupportedArchive.request_id), { pending: false, status: 'failed', error_code: 'codex_method_unsupported', message: 'SECRET arbitrary native backend error' });
    await page.clock.fastForward(1800);
    await page.waitForFunction(() => document.querySelector('#edit-session-message').textContent.includes('runtime version or session layout'));
    const unsupportedMessage = await page.locator('#edit-session-message').textContent();
    assert.match(unsupportedMessage, /runtime version or session layout.*archiving or restoring/);
    assert.match(unsupportedMessage, /project and any existing archive have been kept/);
    assert.doesNotMatch(unsupportedMessage, /This archive is incompatible|SECRET|arbitrary native/);
    assert.equal(statuses.get(unsupportedArchive.request_id).archive, undefined, 'An unsupported export can fail before an archive exists');
    assert.equal(await selected('b').count(), 1, 'Unsupported archiving leaves the original session available');
    assert.equal(await page.locator('#edit-session-archive').isDisabled(), false, 'A confirmed unsupported result allows an explicit retry');
    assert.equal(posts('archive').filter(row => row.request_id === unsupportedArchive.request_id).length, 1, 'Unsupported archiving is never retried automatically');
    behaviors.archive = 'incompatible-http';
    const rejectedArchive = await waitPost('archive', () => page.locator('#edit-session-archive').click());
    await page.waitForFunction(() => !document.querySelector('#edit-session-archive').disabled);
    assert.equal(await page.locator('#edit-session-message').textContent(), unsupportedMessage, 'HTTP archive failures use the same safe fixed message');
    assert.equal(posts('archive').filter(row => row.request_id === rejectedArchive.request_id).length, 1);
    await page.locator('#edit-session-cancel').click();

    await openEditor('b'); assert.equal(await page.locator('#edit-session-delete-project').isChecked(), false);
    const archived = await waitPost('archive', () => page.locator('#edit-session-archive').click());
    assert.equal(archived.delete_project, false); assert.equal(archived.cwd, '/home/demo/CODEX/b');
    assert.equal(await selected('b').count(), 1, 'Archive request keeps the session until success is confirmed');
    complete(archived); await page.clock.fastForward(1800); await selected('b').waitFor({ state: 'detached' });
    assert.match(await page.locator('#edit-session-archive-destination').textContent(), /\/home\/demo\/CODEX_Archived\/project-20261018.tar.xz/);
    assert.equal(await selected('a').getAttribute('aria-current'), 'true'); await page.locator('#edit-session-cancel').click();

    await openEditor('c'); await page.locator('#edit-session-delete-project').check(); behaviors.archive = 'drop';
    const reloadArchive = await waitPost('archive', () => page.locator('#edit-session-archive').click());
    await page.waitForFunction(() => document.querySelector('#edit-session-message').textContent.includes('not been confirmed'));
    assert.equal(reloadArchive.delete_project, true);
    assert.ok(await page.evaluate(() => sessionStorage.getItem('codex-webui-session-operations')).then(value => value.includes(reloadArchive.request_id)));
    complete(reloadArchive); await page.reload();
    await page.waitForFunction(() => !document.querySelector('[data-session-id="c"]'));
    await page.waitForFunction(() => !sessionStorage.getItem('codex-webui-session-operations'));
    assert.equal(posts('archive').filter(row => row.request_id === reloadArchive.request_id).length, 1, 'Reload resumes the same durable archive with GET, never POST');
    assert.ok(requests.some(row => row.kind === 'archive' && row.method === 'GET' && row.request_id === reloadArchive.request_id));

    await choose('a'); await page.locator('[data-create-worker-id="w"]').click();
    await page.locator('#new-session-mode-restore').click(); await page.locator('.session-archive-choice').waitFor();
    assert.match(await page.locator('#new-session-archive-directory').textContent(), /\/home\/demo\/CODEX_Archived/);
    assert.match(await page.locator('.session-archive-choice').textContent(), /2.0 KiB/);
    assert.match(await page.locator('.session-archive-choice').textContent(), /Codex 0\.163\.0/, 'Archive choices show the normalized native Codex version');
    assert.doesNotMatch(await page.locator('.session-archive-choice').textContent(), /codex-cli/);
    assert.match(await page.locator('.session-archive-choice').textContent(), /Original path:/);
    assert.equal(await page.locator('.session-archive-choice img').count(), 0, 'Archive names are rendered as text');
    await page.locator('#new-session-archives-more').click();
    await page.waitForFunction(() => document.querySelectorAll('.session-archive-choice').length === 2);
    assert.equal(requests.filter(row => row.kind === 'list' && row.method === 'POST').at(-1).offset, 1, 'More archives uses the worker-provided pagination offset');
    assert.equal(await page.locator('#new-session-archives-more').isHidden(), true);
    await page.locator('#new-session-runtime').selectOption('other');
    await page.waitForFunction(() => document.querySelectorAll('.session-archive-choice').length === 1);
    await page.locator('#new-session-archives-refresh').click();
    await page.waitForFunction(() => !document.querySelector('#new-session-archives-refresh').disabled);
    await page.locator('input[name="session-archive"]').check();
    const unsupportedRestore = await waitPost('restore', () => page.locator('#new-session-submit').click());
    Object.assign(statuses.get(unsupportedRestore.request_id), { pending: false, status: 'failed', error_code: 'archive_incompatible', message: 'SECRET arbitrary native backend error' });
    await page.clock.fastForward(1800);
    await page.waitForFunction(() => document.querySelector('#new-session-message').textContent.includes('original Codex version'));
    const restoreMessage = await page.locator('#new-session-message').textContent();
    assert.match(restoreMessage, /original Codex version and a supported session layout/);
    assert.match(restoreMessage, /project and archive have been kept/);
    assert.doesNotMatch(restoreMessage, /SECRET|arbitrary native/);
    assert.equal(await page.locator('input[name="session-archive"]:checked').inputValue(), archive.id, 'An incompatible restore retains its selected archive');
    assert.equal(await selected('a').getAttribute('aria-current'), 'true', 'An incompatible restore preserves the selected session');
    assert.equal(posts('restore').filter(row => row.request_id === unsupportedRestore.request_id).length, 1, 'An incompatible restore is never retried automatically');
    behaviors.restore = 'incompatible-http';
    const rejectedRestore = await waitPost('restore', () => page.locator('#new-session-submit').click());
    await page.waitForFunction(() => !document.querySelector('#new-session-submit').disabled);
    assert.equal(await page.locator('#new-session-message').textContent(), restoreMessage, 'HTTP restore failures preserve the specific version guidance and hide upstream messages');
    assert.equal(posts('restore').filter(row => row.request_id === rejectedRestore.request_id).length, 1);
    const restoresBeforeCollision = posts('restore').length;
    const restore = await waitPost('restore', () => page.locator('#new-session-submit').click());
    assert.deepEqual({ worker_id: restore.worker_id, runtime_id: restore.runtime_id, runtime_generation: restore.runtime_generation, archive_id: restore.archive_id, confirmed: restore.confirmed }, { worker_id: 'w', runtime_id: 'other', runtime_generation: 9, archive_id: archive.id, confirmed: true });
    Object.assign(statuses.get(restore.request_id), { pending: false, status: 'failed', error_code: 'archive_conflict', message: 'internal filesystem error must not appear' });
    await page.clock.fastForward(1800); await page.waitForFunction(() => document.querySelector('#new-session-message').textContent.includes('Nothing was overwritten'));
    assert.match(await page.locator('#new-session-message').textContent(), /Move that directory.*explicitly retry/);
    assert.doesNotMatch(await page.locator('#new-session-message').textContent(), /internal filesystem/);
    assert.equal(await page.locator('#new-session-submit').isDisabled(), false, 'A path collision allows an explicit retry after resolving it');
    const retry = await waitPost('restore', () => page.locator('#new-session-submit').click());
    assert.notEqual(retry.request_id, restore.request_id); complete(retry); await page.clock.fastForward(1800);
    await page.waitForFunction(() => document.querySelector('#session-title').textContent === 'Session restored');
    assert.equal(posts('restore').length, restoresBeforeCollision + 2, 'Restore only repeats after an explicit click');
    assert.equal(statuses.get(requests.find(row => row.kind === 'list').request_id).archives[0].id, archive.id, 'Restore preserves the original archive');

    await page.locator('[data-create-worker-id="w"]').click(); await page.locator('#new-session-mode-restore').click();
    emptyArchiveList = true; await page.locator('#new-session-archives-refresh').click();
    await page.waitForFunction(() => document.querySelector('#new-session-archives-message').textContent.includes('No .tar.xz archives'));
    assert.equal(await page.locator('.session-archive-choice').count(), 0, 'An omitted empty archive array is a completed empty list');
    emptyArchiveList = false; await page.locator('#new-session-archives-refresh').click(); await page.locator('.session-archive-choice').waitFor();
    await page.locator('#new-session-cancel').click();

    await openEditor('old');
    assert.equal(await page.locator('#edit-session-save').isDisabled(), true); assert.equal(await page.locator('#edit-session-archive').isDisabled(), true);
    assert.match(await page.locator('#edit-session-editing-help').textContent(), /Update this worker/);
    assert.equal(await page.locator('#edit-session-delete').isDisabled(), false, 'Older workers retain existing durable deletion');
    await page.locator('#edit-session-delete').click(); await page.locator('#delete-session-dialog').waitFor();
    assert.match(await page.locator('#delete-session-description').textContent(), /working directory.*all files.*kept/is);
    await page.locator('#delete-session-cancel').click();

    const mobileEditorActionsFit = async context => {
      const geometry = await page.locator('.session-editor-actions').evaluate(element => {
        const bounds = element.getBoundingClientRect();
        return [...element.querySelectorAll('button')].map(button => {
          const box = button.getBoundingClientRect(), text = document.createRange(); text.selectNodeContents(button);
          return { label: button.textContent, top: box.top, bottom: box.bottom, left: box.left, right: box.right, width: box.width, height: box.height,
            lines: text.getClientRects().length, containerLeft: bounds.left, containerRight: bounds.right };
        });
      });
      for (const button of geometry) {
        assert.equal(button.lines, 1, context + ': ' + button.label + ' stays on one line');
        assert.ok(button.width >= 44 && button.height >= 44, context + ': ' + button.label + ' keeps its tap target');
        assert.ok(button.left >= button.containerLeft - 1 && button.right <= button.containerRight + 1, context + ': ' + button.label + ' fits the dialog');
      }
      assert.ok(Math.abs(geometry[0].top - geometry[1].top) < 1 && Math.abs(geometry[2].top - geometry[3].top) < 1, context + ': actions form two complete rows');
      assert.ok(geometry[0].right < geometry[1].left && geometry[2].right < geometry[3].left && geometry[2].top >= geometry[0].bottom, context + ': action targets do not overlap');
    };
    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 844 }); await choose('a'); await page.locator('#show-sessions').click();
      await openEditor('d');
      assert.equal(await page.evaluate(() => document.activeElement.matches('input,textarea')), false, 'Mobile editor keeps the keyboard closed');
      assert.equal(await page.locator('#edit-session-name').evaluate(element => getComputedStyle(element).fontSize), '16px');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      await mobileEditorActionsFit(width + 'px idle');
      behaviors.archive = 'hold';
      const busyArchive = await waitPost('archive', () => page.locator('#edit-session-archive').click());
      await page.waitForFunction(() => document.querySelector('#edit-session-archive').textContent === 'Archiving…');
      await mobileEditorActionsFit(width + 'px submitting');
      const held = heldReplies.get(busyArchive.request_id);
      await held.route.fulfill({ status: 202, contentType: 'application/json', body: JSON.stringify(held.status) }); heldReplies.delete(busyArchive.request_id);
      await page.waitForFunction(() => document.querySelector('#edit-session-archive').textContent === 'Check status');
      await mobileEditorActionsFit(width + 'px pending');
      Object.assign(statuses.get(busyArchive.request_id), { pending: false, status: 'failed', error_code: width === 390 ? 'session_busy' : 'busy', message: 'SECRET arbitrary native backend error' });
      await page.clock.fastForward(1800);
      await page.waitForFunction(() => document.querySelector('#edit-session-message').textContent.includes('idle state could not be verified'));
      const busyMessage = await page.locator('#edit-session-message').textContent();
      assert.match(busyMessage, /project or Codex runtime.*pending activity/);
      assert.doesNotMatch(busyMessage, /this session’s running|SECRET|arbitrary native/);
      assert.equal(await selected('d').count(), 1, 'An unverified idle state leaves the original session available');
      assert.equal(posts('archive').filter(row => row.request_id === busyArchive.request_id).length, 1, 'A rejected archive is not replayed automatically');
      await mobileEditorActionsFit(width + 'px rejected');
      await page.locator('#edit-session-cancel').click();
      await page.locator('[data-create-worker-id="w"]').click(); await page.locator('#new-session-mode-restore').click();
      assert.equal(await page.evaluate(() => document.activeElement.matches('input,textarea')), false, 'Mobile archive picker keeps the keyboard closed');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'Long archive paths fit the mobile dialog');
      const bounds = await page.locator('#new-session-dialog').boundingBox(); assert.ok(bounds.width <= width - 16);
      await page.locator('#new-session-cancel').click();
    }
    await page.setViewportSize({ width: 1440, height: 1000 }); await choose('d'); await openEditor('d');
    const beforeRemoteRemoval = { ...sessions.find(row => row.session_id === 'd') };
    const pendingAcrossRemoval = await waitPost('archive', () => page.locator('#edit-session-archive').click());
    await page.waitForFunction(() => document.querySelector('#edit-session-archive').textContent === 'Check status' && !document.querySelector('#edit-session-archive').disabled);
    Object.assign(statuses.get(pendingAcrossRemoval.request_id), { phase: 'compressing' });
    await page.clock.setSystemTime(await page.evaluate(() => Date.now() + 121000));
    const archiveStatusReply = () => page.waitForResponse(response => response.request().method() === 'GET' && response.url().includes('/sessions/archive?') && response.url().includes(pendingAcrossRemoval.request_id), { timeout: 5000 });
    let progressReply = archiveStatusReply(); await page.clock.fastForward(1800); await progressReply;
    await page.waitForFunction(() => document.querySelector('#edit-session-message').textContent.includes('Compressing'));
    progressReply = archiveStatusReply(); await page.clock.fastForward(1800); await progressReply;
    assert.ok(requests.filter(row => row.kind === 'archive' && row.method === 'GET' && row.request_id === pendingAcrossRemoval.request_id).length >= 2, 'Confirmed pending progress continues beyond the initial 120-second observation window');
    assert.equal(posts('archive').filter(row => row.request_id === pendingAcrossRemoval.request_id).length, 1, 'Long-running compression never replays the archive POST');
    sessions = sessions.filter(row => row.session_id !== 'd');
    await page.evaluate(() => window.testActivitySocket.emit({ type: 'activity_snapshot', version: 3, sequence: 1, revision: 2, inventory_revision: 'cross-client-archive', sessions: [], telegram_bots: [], telegram_bots_revision: '1' }));
    await page.locator('#composer').waitFor({ state: 'hidden' });
    assert.equal(await page.locator('#transcript').isHidden(), true, 'A server inventory removal detaches the selected transcript');
    assert.equal(await page.evaluate(() => window.testSockets.at(-1).readyState), 3, 'A server inventory removal closes the selected conversation socket');
    assert.equal(await page.locator('#send').isDisabled(), true);
    assert.match(await page.locator('#notice').textContent(), /no longer active/);
    assert.equal(await page.locator('#edit-session-dialog').isVisible(), true, 'The operation dialog survives a server inventory removal');
    assert.ok(await page.evaluate(() => sessionStorage.getItem('codex-webui-session-operations')).then(value => value.includes(pendingAcrossRemoval.request_id)), 'Server inventory updates preserve pending operation identities');
    Object.assign(statuses.get(pendingAcrossRemoval.request_id), { pending: false, status: 'completed', session: { ...beforeRemoteRemoval, archived: true }, archive, warning_code: 'project_cleanup_failed', project_deleted: false, message: 'Session archived. Project folder cleanup could not finish; the verified archive was kept. Retained project files: /home/demo/CODEX/.codex-archived-retained' });
    await page.clock.fastForward(1800);
    await page.waitForFunction(() => document.querySelector('#edit-session-message').textContent.includes('cleanup could not finish'));
    assert.match(await page.locator('#edit-session-message').textContent(), /Retained project files: \/home\/demo\/CODEX\/\.codex-archived-retained/, 'A cleanup warning preserves the safe server-provided retained path');
    assert.equal(await page.locator('#edit-session-message').getAttribute('data-error'), 'true', 'A verified archive with incomplete cleanup retains a visible warning');
    assert.equal(posts('archive').filter(row => row.request_id === pendingAcrossRemoval.request_id).length, 1, 'Inventory removal never replays a pending archive');
    await page.locator('#edit-session-cancel').click();
    assert.deepEqual(errors, []); console.log('WebUI session management browser checks passed (desktop, mobile, lost replies, reload recovery, target isolation, restore collisions).');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
