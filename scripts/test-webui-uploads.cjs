#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assets = process.env.WEBUI_ASSETS || path.resolve(__dirname, '../internal/admin/static');
const sessions = ['aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee', '11111111-2222-4333-8444-555555555555'].map((id, index) => ({ session_id: id, codex_thread_id: 'thread-' + index, worker_id: 'worker', runtime_id: 'primary', runtime_generation: 1, worker_name: 'Worker', runtime_name: 'Codex', name: 'Session ' + index, cwd: '/projects/demo', state: 'idle' }));
const maxFileBytes = 20 * 1024 * 1024, maxImageBytes = 10 * 1024 * 1024;

async function setup(browser, mobile) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 }, isMobile: mobile, hasTouch: mobile });
  const errors = [], recoveryRows = new Map(), targetOverrides = {};
  await context.addCookies([{ name: '__Host-telegramgw-csrf', value: 'upload-csrf', url: 'https://uploads.test/', secure: true, sameSite: 'Strict' }]);
  await context.route('https://uploads.test/**', async route => {
    const url = new URL(route.request().url());
    const json = value => route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) });
    if (url.pathname === '/tgw/api/v1/admin/login/options') return json({ passkey: true, password: false });
    if (url.pathname === '/tgw/api/v1/admin/login/begin') return json({ ceremony_id: 'ceremony', publicKey: { challenge: 'AQID', rpId: 'uploads.test', allowCredentials: [] } });
    if (url.pathname === '/tgw/api/v1/admin/login/finish') return json({ ok: true });
    if (url.pathname === '/tgw/api/v1/admin/session') return json({ authenticated: true, owner_id: 'owner', session_id: 'login', server_time: new Date().toISOString(), expires_at: new Date(Date.now() + 28800000).toISOString() });
    if (url.pathname === '/tgw/api/v1/webui/sessions') return json({ sessions: sessions.map(row => ({ ...row, ...targetOverrides[row.session_id] })), runtimes: [{ runtime_id: 'primary', worker_id: 'worker', generation: 1 }] });
    if (url.pathname === '/tgw/api/v1/webui/telegram-bots') return json({ bots: [], revision: 'bots' });
    if (url.pathname === '/tgw/api/v1/webui/push/config') return json({ supported: false, subscribed: false });
    if (url.pathname.includes('/webui/drafts/')) {
      const id = path.basename(url.pathname);
      if (route.request().method() === 'DELETE') { recoveryRows.delete(id); return route.fulfill({ status: 204 }); }
      if (route.request().method() === 'PUT') { const row = route.request().postDataJSON(); recoveryRows.set(id, row); return json({ revision: row.revision }); }
      return json(recoveryRows.get(id) || {});
    }
    const filename = url.pathname.endsWith('/diagram-renderer') ? 'webui-mermaid-frame.html' : url.pathname.includes('/static/') ? path.basename(url.pathname) : 'webui.html';
    if (!fs.existsSync(path.join(assets, filename))) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: filename.endsWith('.js') ? 'text/javascript' : filename.endsWith('.css') ? 'text/css' : 'text/html', body: fs.readFileSync(path.join(assets, filename)) });
  });
  await context.addInitScript(({ sessions }) => {
    Object.defineProperty(navigator, 'credentials', { configurable: true, value: { get: async () => ({ id: 'passkey', rawId: new Uint8Array([1]), type: 'public-key', response: { clientDataJSON: new Uint8Array([2]), authenticatorData: new Uint8Array([3]), signature: new Uint8Array([4]), userHandle: null }, getClientExtensionResults: () => ({}) }) } });
    window.uploadFrames = []; window.uploadSockets = []; window.activeTurns = {}; window.nativeCWD = {}; window.nativeTopLevelCWD = {}; window.readyOverrides = {}; window.uploadBehavior = ''; window.turnBehavior = ''; window.holdRead = false;
    const NativeReader = window.FileReader;
    window.FileReader = class extends NativeReader {
      readAsDataURL(file) {
        if (window.holdRead) { window.releaseRead = () => super.readAsDataURL(file); return; }
        super.readAsDataURL(file);
      }
    };
    class Socket {
      static OPEN = 1;
      constructor(url) {
        const parsed = new URL(url); this.sessionId = parsed.searchParams.get('session_id'); this.activity = parsed.pathname.endsWith('/activity'); this.readyState = 0;
        window.uploadSockets.push(this);
        setTimeout(() => {
          if (this.readyState === 3) return;
          this.readyState = 1;
          this.emit(this.activity ? { type: 'activity_snapshot', version: 3, sequence: 0, revision: 1, telegram_bots: [], telegram_bots_revision: 'bots', sessions: sessions.map(row => ({ ...row, worker_connectivity: 'connected', pending_questions: 0 })) } : { type: 'ready', session: { ...sessions.find(row => row.session_id === this.sessionId), ...window.readyOverrides[this.sessionId] } });
          if (this.activity) this.heartbeat = setInterval(() => this.emit({ type: 'heartbeat', version: 3, sequence: 0 }), 15000);
        }, 1);
      }
      emit(value) { if (this.readyState === 1) this.onmessage?.({ data: JSON.stringify(value) }); }
      close(code = 1000) { this.readyState = 3; clearInterval(this.heartbeat); this.onclose?.({ code }); }
      send(raw) {
        const frame = JSON.parse(raw); window.uploadFrames.push({ sessionId: this.sessionId, ...frame });
        const reply = result => this.emit({ id: frame.id, result });
        const reject = message => this.emit({ id: frame.id, error: { code: -32000, message } });
        let result = {};
        if (frame.method === 'thread/resume') result = { thread: { id: frame.params.threadId, cwd: window.nativeCWD[this.sessionId] || '/projects/demo' }, cwd: window.nativeTopLevelCWD[this.sessionId], model: 'model' };
        if (frame.method === 'thread/items/list') result = { data: [], nextCursor: null };
        if (frame.method === 'thread/turns/list') result = { data: window.activeTurns[this.sessionId] ? [{ id: window.activeTurns[this.sessionId], status: 'inProgress' }] : [], nextCursor: null };
        if (frame.method === 'model/list') result = { data: [] };
        if (frame.method === 'gateway/questions') result = { questions: [] };
        if (frame.method === 'gateway/upload') {
          const behavior = window.uploadBehavior; window.uploadBehavior = '';
          result = { uploadId: frame.params.uploadId, path: '/projects/demo/inbound_files/' + frame.params.uploadId + '/' + frame.params.name };
          result.text = 'File saved in ' + result.path;
          if (behavior === 'reject') { setTimeout(() => reject('Upload rejected by worker'), 1); return; }
          if (behavior === 'mismatch') result.uploadId = 'another-upload';
          if (behavior === 'hold') { window.releaseUpload = () => reply(result); return; }
        }
        if (['turn/start', 'turn/steer'].includes(frame.method)) {
          const behavior = window.turnBehavior; window.turnBehavior = '';
          if (behavior === 'reject') { setTimeout(() => reject('Native prompt rejected'), 1); return; }
          if (behavior === 'hold') { window.releaseTurn = () => reply({}); return; }
        }
        setTimeout(() => reply(result), 1);
      }
    }
    window.WebSocket = Socket;
  }, { sessions });
  const page = await context.newPage();
  page.on('pageerror', error => errors.push(error.message));
  await page.clock.install({ time: Date.now() });
  const ready = () => page.waitForFunction(() => document.querySelector('#connection').dataset.state === 'connected' && !document.querySelector('#composer').hidden && !document.querySelector('#prompt').disabled);
  await page.goto('https://uploads.test/tgw/webui/?session_id=' + sessions[0].session_id); await ready();
  const choose = async index => {
    if (mobile) await page.locator('#toggle-sessions').click();
    await page.locator('[data-session-id="' + sessions[index].session_id + '"]').click(); await ready();
  };
  const count = method => page.evaluate(method => window.uploadFrames.filter(row => method === 'native' ? ['turn/start', 'turn/steer'].includes(row.method) : row.method === method).length, method);
  const last = method => page.evaluate(method => window.uploadFrames.filter(row => method === 'native' ? ['turn/start', 'turn/steer'].includes(row.method) : row.method === method).at(-1), method);
  const attach = async (name, buffer = Buffer.from([0, 255, 128, 13, 10]), mimeType = 'application/octet-stream') => {
    const chooserPromise = page.waitForEvent('filechooser'); await page.locator('#attach-image').click();
    await (await chooserPromise).setFiles({ name, mimeType, buffer });
  };
  const transfer = (eventType, name, bytes = [0, 255, 128]) => page.evaluate(({ eventType, name, bytes }) => {
    const transfer = new DataTransfer(); transfer.items.add(new File([new Uint8Array(bytes)], name, { type: 'application/octet-stream' }));
    const event = eventType === 'paste' ? new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: transfer }) : new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: transfer });
    document.querySelector(eventType === 'paste' ? '#prompt' : '#composer').dispatchEvent(event); return event.defaultPrevented;
  }, { eventType, name, bytes });
  const pasteImage = () => page.evaluate(() => {
    const canvas = document.createElement('canvas'); canvas.width = 2; canvas.height = 2; canvas.getContext('2d').fillRect(0, 0, 2, 2);
    const dataURL = canvas.toDataURL('image/png'), transfer = new DataTransfer();
    transfer.items.add(new File([Uint8Array.from(atob(dataURL.split(',')[1]), char => char.charCodeAt(0))], 'existing-image.png', { type: 'image/png' }));
    document.querySelector('#prompt').dispatchEvent(new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: transfer }));
    return dataURL;
  });
  const sent = () => page.waitForFunction(() => document.querySelector('#file-preview').hidden && !document.querySelector('#send').textContent.includes('Sending'));
  const active = value => page.evaluate(({ value, sessionId }) => {
    const socket = window.uploadSockets.findLast(row => !row.activity && row.readyState === 1);
    window.activeTurns[sessionId] = value;
    socket.emit({ method: value ? 'turn/started' : 'turn/completed', params: { threadId: 'thread-0', turn: { id: value || 'active-turn', status: value ? 'inProgress' : 'completed' } } });
  }, { value, sessionId: sessions[0].session_id });
  return { context, page, errors, recoveryRows, targetOverrides, ready, choose, count, last, attach, transfer, pasteImage, sent, active };
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    for (const mobile of [false, true]) {
      const test = await setup(browser, mobile);
      const { page, attach, transfer, sent, count, last, active, choose, ready, pasteImage } = test;
      try {
        await page.locator('#prompt').fill('A plain text prompt'); await page.locator('#send').click();
        await page.waitForFunction(() => document.querySelector('#prompt').value === '');
        assert.deepEqual((await last('native')).params.input, [{ type: 'text', text: 'A plain text prompt' }]);
        assert.equal(await count('gateway/upload'), 0, 'Text alone never uploads a file');

        const bytes = Buffer.from(Array.from({ length: 256 }, (_, i) => i));
        await attach('binary.dat', bytes); await page.locator('#file-preview').waitFor();
        assert.match(await page.locator('#file-description').textContent(), /binary.dat · 256 bytes/);
        assert.equal(await page.locator('#image-preview').isHidden(), true);
        assert.equal(await page.locator('#attach-image').isDisabled(), true);
        if (process.env.WEBUI_UPLOAD_SCREENSHOTS) {
          fs.mkdirSync(process.env.WEBUI_UPLOAD_SCREENSHOTS, { recursive: true });
          await page.screenshot({ path: path.join(process.env.WEBUI_UPLOAD_SCREENSHOTS, 'webui-upload-' + (mobile ? 'mobile' : 'desktop') + '.png') });
        }
        await pasteImage();
        assert.match(await page.locator('#file-description').textContent(), /binary.dat/);
        assert.equal(await page.locator('#image-preview').isHidden(), true, 'An image cannot replace an existing file draft');
        const nativeBeforeFile = await count('native');
        await page.locator('#send').click(); await sent();
        const upload = await last('gateway/upload');
        assert.deepEqual(Object.keys(upload.params).sort(), ['data', 'name', 'uploadId']);
        assert.match(upload.params.uploadId, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
        assert.equal(upload.params.data, bytes.toString('base64'), 'All byte values survive base64 unchanged');
        assert.equal(await count('native'), nativeBeforeFile, 'File-only upload never starts or steers Codex');
        assert.match(await page.locator('#notice').textContent(), /^File saved in \/projects\/demo\/inbound_files\//);
        const beforeImage = await count('gateway/upload'), imageDataURL = await pasteImage();
        await page.waitForFunction(() => !document.querySelector('#image-preview').hidden && !document.querySelector('#send').disabled);
        await transfer('drop', 'cannot-replace-image.txt');
        assert.equal(await page.locator('#file-preview').isHidden(), true, 'A file cannot replace an existing image draft');
        await page.locator('#send').click();
        await page.waitForFunction(() => document.querySelector('#image-preview').hidden && !document.querySelector('#send').textContent.includes('Sending'));
        assert.deepEqual((await last('native')).params.input, [{ type: 'image', url: imageDataURL }]);
        assert.equal(await count('gateway/upload'), beforeImage, 'PNG images retain the native image attachment behavior');

        assert.equal(await transfer('drop', 'report.pdf'), true); await page.locator('#prompt').fill('Summarize the report.');
        await page.locator('#send').click(); await sent();
        const captionUpload = await last('gateway/upload'), caption = await last('native');
        assert.equal(caption.method, 'turn/start');
        assert.equal(caption.params.threadId, 'thread-0');
        assert.deepEqual(caption.params.input, [{ type: 'text', text: 'Summarize the report.\n\nFile: /projects/demo/inbound_files/' + captionUpload.params.uploadId + '/report.pdf' }]);

        await active('active-turn');
        assert.equal(await transfer('paste', 'sheet.csv'), true);
        await page.locator('#prompt').fill('/tgsteer Use the spreadsheet.'); await page.locator('#send').click(); await sent();
        const steer = await last('native');
        assert.equal(steer.method, 'turn/steer'); assert.equal(steer.params.expectedTurnId, 'active-turn');
        assert.match(steer.params.input[0].text, /^Use the spreadsheet\.\n\nFile: /);
        await attach('explicit-steer.txt'); await page.locator('#prompt').fill('/tgsteer');
        await page.locator('#send').click(); await sent();
        const fileSteer = await last('native');
        assert.equal(fileSteer.method, 'turn/steer'); assert.equal(fileSteer.params.expectedTurnId, 'active-turn');
        assert.match(fileSteer.params.input[0].text, /^File: /, 'Explicit /tgsteer without instructions still sends the file path as guidance');
        await attach('active-file.txt'); const beforeActiveFile = await count('native');
        assert.equal(await page.locator('#send').textContent(), 'Upload ↑');
        await page.locator('#send').click(); await sent();
        assert.equal(await count('native'), beforeActiveFile, 'File-only upload leaves the running turn alone');
        assert.equal(await page.locator('#steer-notice').isHidden(), true, 'File-only confirmation clears older queued-steer feedback');
        await active('');

        await attach('empty.txt', Buffer.alloc(0)); assert.match(await page.locator('#file-description').textContent(), /0 bytes/);
        await page.locator('#send').click(); await sent(); assert.equal((await last('gateway/upload')).params.data, '', 'Empty generic files upload successfully');
        await attach('remove.txt'); await page.locator('#remove-file').click(); assert.equal(await page.locator('#send').isDisabled(), true);

        const geometry = await page.evaluate(() => ({ width: innerWidth, scrollWidth: document.documentElement.scrollWidth, composer: document.querySelector('#composer').getBoundingClientRect().toJSON() }));
        assert.ok(geometry.scrollWidth <= geometry.width && geometry.composer.right <= geometry.width + 1, 'Desktop/mobile composer fits its viewport');

        if (!mobile) {
          await attach('limit.bin', Buffer.alloc(maxFileBytes, 0xa5)); assert.match(await page.locator('#file-description').textContent(), /20 MiB/);
          await page.locator('#send').click(); await sent();
          assert.equal(await page.evaluate(() => atob(window.uploadFrames.filter(row => row.method === 'gateway/upload').at(-1).params.data).length), maxFileBytes, 'Exactly 20 MiB is accepted');
          const beforeOversize = await count('gateway/upload');
          await attach('too-large.bin', Buffer.alloc(maxFileBytes + 1));
          assert.equal(await page.locator('#file-preview').isHidden(), true); assert.match(await page.locator('#notice').textContent(), /20 MiB/);
          assert.equal(await count('gateway/upload'), beforeOversize, 'Oversize selection never dispatches an upload');

          const png = Buffer.from(await page.evaluate(() => {
            const canvas = document.createElement('canvas'); canvas.width = 2; canvas.height = 2;
            return canvas.toDataURL('image/png').split(',')[1];
          }), 'base64');
          const imageAtLimit = Buffer.alloc(maxImageBytes); png.copy(imageAtLimit);
          await attach('image-limit.png', imageAtLimit, 'image/png');
          await page.waitForFunction(() => !document.querySelector('#image-preview').hidden && !document.querySelector('#send').disabled);
          await page.locator('#send').click();
          await page.waitForFunction(() => document.querySelector('#image-preview').hidden && !document.querySelector('#send').textContent.includes('Sending'));
          assert.equal(await page.evaluate(() => atob(window.uploadFrames.filter(row => ['turn/start', 'turn/steer'].includes(row.method)).at(-1).params.input[0].url.split(',')[1]).length), maxImageBytes, 'Exactly 10 MiB remains accepted for images');
          const beforeOversizeImage = await count('native');
          await attach('image-too-large.png', Buffer.alloc(maxImageBytes + 1), 'image/png');
          assert.equal(await page.locator('#image-preview').isHidden(), true); assert.match(await page.locator('#notice').textContent(), /image.*10 MiB/);
          assert.equal(await count('native'), beforeOversizeImage, 'Images above 10 MiB never dispatch native input');
          assert.equal(await count('gateway/upload'), beforeOversize, 'Image boundaries keep the native image input path');

          await attach('command.txt'); await page.locator('#prompt').fill('/status');
          const commandBefore = await count('gateway/command'), uploadBefore = await count('gateway/upload');
          await page.locator('#send').click(); assert.equal(await count('gateway/command'), commandBefore); assert.equal(await count('gateway/upload'), uploadBefore);
          assert.match(await page.locator('#notice').textContent(), /Remove the attachment/);
          await page.locator('#prompt').fill('/tgsteer idle caption'); await page.locator('#send').click();
          assert.match(await page.locator('#notice').textContent(), /no running turn/);
          await page.locator('#remove-file').click(); await page.locator('#prompt').fill('');

          await attach('retry.txt'); await page.locator('#prompt').fill('Retain the acknowledged path.');
          await page.evaluate(() => { window.turnBehavior = 'reject'; }); await page.locator('#send').click();
          await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('Native prompt rejected'));
          assert.match(await page.locator('#file-description').textContent(), /Uploaded/);
          assert.match(await page.locator('#notice').textContent(), /already uploaded.*reuse it/);
          const beforeCaptionRetry = await count('gateway/upload');
          await page.locator('#send').click(); await sent(); assert.equal(await count('gateway/upload'), beforeCaptionRetry, 'Native failure retry reuses the acknowledged path without another upload');

          // An acknowledged path belongs to the original upload destination,
          // including native thread context, across reconnects and retries.
          await attach('bound-target.txt'); await page.locator('#prompt').fill('Keep the original upload destination.');
          await page.evaluate(() => { window.turnBehavior = 'reject'; }); await page.locator('#send').click();
          await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('Native prompt rejected'));
          const boundUploads = await count('gateway/upload'), boundPrompts = await count('native');
          for (const change of [{ cwd: '/projects/changed' }, { runtime_generation: 2 }, { worker_id: 'different-worker' }, { runtime_id: 'different-runtime' }, { codex_thread_id: 'different-thread' }]) {
            test.targetOverrides[sessions[0].session_id] = change;
            await page.locator('#refresh-sessions').click(); await page.waitForFunction(() => !document.querySelector('#refresh-sessions').disabled);
            await page.locator('#send').click(); assert.match(await page.locator('#notice').textContent(), /workspace or runtime changed.*reattach/);
            assert.equal(await count('gateway/upload'), boundUploads); assert.equal(await count('native'), boundPrompts);
            test.targetOverrides[sessions[0].session_id] = {};
            await page.locator('#refresh-sessions').click(); await page.waitForFunction(() => !document.querySelector('#refresh-sessions').disabled);
          }
          await page.evaluate(sessionId => { window.nativeTopLevelCWD[sessionId] = '/native/changed'; window.uploadSockets.findLast(row => !row.activity && row.readyState === 1).close(1006); }, sessions[0].session_id);
          await page.clock.fastForward(5000); await ready(); await page.locator('#send').click();
          assert.match(await page.locator('#notice').textContent(), /workspace or runtime changed.*reattach/);
          assert.equal(await count('native'), boundPrompts, 'Native CWD confirmed by resume fences the saved upload path');
          await page.locator('#remove-file').click(); await page.locator('#prompt').fill('');
          await page.evaluate(sessionId => { delete window.nativeTopLevelCWD[sessionId]; window.readyOverrides[sessionId] = { runtime_generation: 2 }; window.uploadSockets.findLast(row => !row.activity && row.readyState === 1).close(1006); }, sessions[0].session_id);
          await page.clock.fastForward(5000); await ready();
          await attach('fresh-ready.txt'); await page.locator('#prompt').fill('Use the fresh ready target.');
          await page.evaluate(() => { window.turnBehavior = 'reject'; }); await page.locator('#send').click();
          await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('Native prompt rejected'));
          await page.evaluate(sessionId => { window.readyOverrides[sessionId] = { runtime_generation: 3 }; window.uploadSockets.findLast(row => !row.activity && row.readyState === 1).close(1006); }, sessions[0].session_id);
          await page.clock.fastForward(5000); await ready();
          const readyPrompts = await count('native'); await page.locator('#send').click();
          assert.match(await page.locator('#notice').textContent(), /workspace or runtime changed.*reattach/);
          assert.equal(await count('native'), readyPrompts, 'Fresh ready session generation overrides stale inventory before retry');
          await page.locator('#remove-file').click(); await page.locator('#prompt').fill('');
          await page.evaluate(sessionId => { delete window.readyOverrides[sessionId]; window.uploadSockets.findLast(row => !row.activity && row.readyState === 1).close(1006); }, sessions[0].session_id);
          await page.clock.fastForward(5000); await ready();

          await attach('upload-retry.txt'); await page.evaluate(() => { window.uploadBehavior = 'reject'; });
          await page.locator('#send').click(); await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('Upload rejected'));
          const failedUploadID = (await last('gateway/upload')).params.uploadId;
          await page.locator('#send').click(); await sent(); assert.equal((await last('gateway/upload')).params.uploadId, failedUploadID, 'Upload failures retain the attachment ID');

          await attach('mismatch.txt'); await page.evaluate(() => { window.uploadBehavior = 'mismatch'; }); await page.locator('#send').click();
          await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('acknowledgement was incomplete'));
          const mismatchID = (await last('gateway/upload')).params.uploadId;
          await page.locator('#send').click(); await sent(); assert.equal((await last('gateway/upload')).params.uploadId, mismatchID);

          await attach('read-race.txt'); await page.locator('#prompt').fill('Read draft from session zero');
          await page.evaluate(() => { window.holdRead = true; }); const beforeReadRace = await count('gateway/upload');
          await page.locator('#send').click(); await page.waitForFunction(() => !!window.releaseRead);
          await choose(1); await attach('session-one.txt'); await page.locator('#prompt').fill('Unrelated session one draft');
          await choose(0); await page.evaluate(() => { window.holdRead = false; window.releaseRead(); window.releaseRead = null; });
          await page.waitForTimeout(50); assert.equal(await count('gateway/upload'), beforeReadRace, 'Switching away and back fences a delayed FileReader');
          assert.match(await page.locator('#file-description').textContent(), /read-race/); assert.equal(await page.locator('#prompt').inputValue(), 'Read draft from session zero');
          await page.locator('#remove-file').click(); await page.locator('#prompt').fill('');

          await attach('upload-race.txt'); await page.locator('#prompt').fill('Do not steer another session');
          await page.evaluate(() => { window.uploadBehavior = 'hold'; }); const beforeUploadRace = await count('native');
          await page.locator('#send').click(); await page.waitForFunction(() => !!window.releaseUpload);
          const racedID = (await last('gateway/upload')).params.uploadId;
          await choose(1); await page.evaluate(() => { window.releaseUpload(); window.releaseUpload = null; });
          await page.waitForTimeout(50); assert.equal(await count('native'), beforeUploadRace, 'A late upload response never prompts the replacement session');
          assert.equal(await page.locator('#prompt').inputValue(), 'Unrelated session one draft'); assert.match(await page.locator('#file-description').textContent(), /session-one/);
          await choose(0); await page.locator('#send').click(); await sent(); assert.equal((await last('gateway/upload')).params.uploadId, racedID);

          await attach('uncertain.txt'); await page.locator('#prompt').fill('An uncertain upload');
          await page.evaluate(() => { window.uploadBehavior = 'hold'; }); await page.locator('#send').click();
          await page.waitForFunction(() => !!window.releaseUpload);
          const uncertainID = (await last('gateway/upload')).params.uploadId, beforeTimeout = await count('gateway/upload');
          await page.clock.fastForward(120001); await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('timed out'));
          assert.equal(await count('gateway/upload'), beforeTimeout, 'Upload timeout does not replay');
          await page.evaluate(() => window.uploadSockets.findLast(row => !row.activity && row.readyState === 1).close(1006));
          await page.clock.fastForward(5000); await ready(); assert.equal(await count('gateway/upload'), beforeTimeout, 'Reconnect does not replay uploads');
          await page.locator('#send').click(); await sent(); assert.equal((await last('gateway/upload')).params.uploadId, uncertainID, 'Manual retry after lost acknowledgement keeps the original ID');

          // Recovery encrypts only the text map: inspect the decrypted request,
          // so this detects binary metadata entering recovery before encryption.
          await page.evaluate(() => {
            const encrypt = crypto.subtle.encrypt.bind(crypto.subtle); window.recoveryPlaintexts = [];
            crypto.subtle.encrypt = (algorithm, key, data) => { window.recoveryPlaintexts.push(new TextDecoder().decode(data)); return encrypt(algorithm, key, data); };
          });
          await page.locator('#open-settings').click(); await page.locator('#draft-recovery-enabled').check(); await page.locator('#close-settings').click();
          await attach('private-attachment.bin', Buffer.from('NEVER_SAVE_BINARY_PAYLOAD')); await page.locator('#prompt').fill('Recoverable text only');
          await page.clock.fastForward(600);
          await page.waitForFunction(() => window.recoveryPlaintexts.some(text => text.includes('Recoverable text only')));
          const recovered = await page.evaluate(() => window.recoveryPlaintexts.at(-1));
          assert.ok(!recovered.includes('private-attachment') && !recovered.includes('NEVER_SAVE_BINARY_PAYLOAD') && !recovered.includes('uploadId'));
          const storage = await page.evaluate(() => JSON.stringify({ ...sessionStorage, ...localStorage }));
          assert.ok(!storage.includes('private-attachment') && !storage.includes('NEVER_SAVE_BINARY_PAYLOAD'));
          await page.evaluate(() => window.uploadSockets.findLast(row => !row.activity && row.readyState === 1).close(4401));
          await page.waitForSelector('#auth:not([hidden])');
          assert.equal(await page.locator('#file-preview').isHidden(), true); assert.equal(await page.locator('#file-description').textContent(), '');
          assert.equal(await page.locator('#image-file').inputValue(), ''); assert.equal(await page.locator('#prompt').inputValue(), '');
          await page.locator('#auth-login').click(); await ready();
          assert.equal(await page.locator('#file-preview').isHidden(), true, 'Expired attachment is not restored by sign-in');
          await choose(1);
          assert.equal(await page.locator('#file-preview').isHidden(), true, 'Expiry clears attachment drafts from every session');
          assert.equal(await page.locator('#prompt').inputValue(), '', 'Existing encrypted text recovery stays explicitly opt-in to restore');
        }
        assert.deepEqual(test.errors, []);
        console.log('Web UI generic upload checks passed (' + (mobile ? 'mobile' : 'desktop') + ').');
      } finally { await test.context.close(); }
    }
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
