'use strict';
(() => {
  const { node, clean, markdown, code } = window.CodexFormat;
  const $ = id => document.getElementById(id);
  const api = '/tgw/api/v1/webui';
  const state = { sessions: [], workers: [], runtimes: [], selected: null, socket: null, generation: 0, authGeneration: 0, sequence: 0, pending: new Map(), drafts: new Map(), positions: new Map(), items: new Map(), questions: new Map(), models: [], turn: null, connected: false, stopped: false, attempts: 0, timer: null, loading: false, cursor: null, queuedEvents: [], submitting: false, renderTimer: null, authenticated: false, refreshing: false, refreshTimer: null };
  let commandUI = null, notificationUI = null, sessionAuth = null, rawView = false, gatewayCommandAbort = null;
  let authRestore = null, restoringPosition = false, uncertainSend = false;
  let draftRecovery = null, recoveredDrafts = null, draftSaveTimer = null, draftDirty = false;
  const uncertainDrafts = new Set();
  let sessionInventoryAbort = null, sessionInventoryEpoch = 0, authRotationPending = false;
  let authRenewing = false, logoutPending = null;
  const sessionUUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  const workerGroupPreferenceKey = 'mact-webui-worker-collapsed';
  const workerGroupCollapsed = new Map(), workerGroupReveals = new Set(), workerGroupSearchCollapsed = new Map();
  let workerGroupSearchQuery = '';
  try {
    const raw = localStorage.getItem(workerGroupPreferenceKey);
    const saved = raw && raw.length <= 131072 ? JSON.parse(raw) : null;
    if (saved && typeof saved === 'object' && !Array.isArray(saved)) {
      for (const [id, collapsed] of Object.entries(saved).slice(0, 1000)) {
        if (/^[a-zA-Z0-9_-]{1,128}$/.test(id) && typeof collapsed === 'boolean') workerGroupCollapsed.set(id, collapsed);
      }
    }
  } catch (_) { /* Worker grouping is an optional local preference. */ }
  const requestedSession = new URL(location.href).searchParams.get('session_id') || '';
  let notificationSessionID = sessionUUID.test(requestedSession) ? requestedSession.toLowerCase() : '';
  const activity = { socket: null, timer: null, generation: 0, revision: -1, sequence: -1, inventoryRevision: '', sessions: new Map(), attempts: 0, watchdog: null, ready: false };
  const telegramBots = { rows: [], revision: '', activityRevision: '', httpRevision: '', epoch: 0, ready: false, read: null, readAgain: false, pending: null, errorTimer: null };
  const sessionSettings = new Map(), activitySettingsRevisions = new Map();
  const sessionErrors = new Map(), sessionErrorDismissals = new Map(), sessionErrorTurns = new Map();
  let settingsSerial = 0, modelReadEpoch = 0;
  let permissionSettings = null, permissionRevision = 0;
  let claudeStatus = null, claudeStatusRevision = 0;
  let loadingOlder = false, suppressHistoryScroll = false;
  let questionRevision = 0, questionSnapshotSequence = 0;
  let questionRefresh = null;
  let selectedGoal = null, goalRevision = 0, goalSnapshotSequence = 0, goalDialogSessionID = '';
  let turnTimes = new Map(), turnCursor = null, turnMetadataWork = null, historyRead = rpc, resumedThread = null;
  const attachmentDrafts = new Map();
  const sessionDeletions = new Map();
  let deleteSessionTarget = null;
  const sessionCreations = new Map();
  const sessionOperations = new Map();
  let editSessionTarget = null, sessionOperationOwner = '', sessionOperationSerial = 0, newSessionMode = 'create', selectedArchiveID = '';
  let newSessionWorkerID = '', workspaceBrowse = null, workspacePage = null, newSessionPathEdited = false;
  const maxFileBytes = 20 * 1024 * 1024, maxImageBytes = 10 * 1024 * 1024, maxDraftAttachmentBytes = 40 * 1024 * 1024, maxDraftAttachments = 16;
  let attachmentPickerSession = null;
  let followLatest = true, bottomFrame = null;
  const fileUI = window.CodexFiles.create({
    getContext: () => ({ sessionId: state.selected?.session_id || '', authGeneration: state.authGeneration, authenticated: state.authenticated, connected: state.connected }),
    expired: expire, focusFallback: () => state.authenticated ? $('transcript') : $('auth-login')
  });
  let lastHistoryScrollTop = 0, lastHistoryHeight = 0, lastHistoryViewport = 0;
  let fontSize = 14;
  try { const saved = Number(localStorage.getItem('codex-webui-font-size')); if (Number.isInteger(saved) && saved >= 12 && saved <= 22) fontSize = saved; } catch (_) { /* Storage can be disabled. */ }
  function setFontSize(value, persist = true) {
    const bottom = followLatest && atBottom();
    const scroller = $('transcript');
    const anchor = [...$('messages').children].find(item => item.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
    const offset = anchor?.getBoundingClientRect().top;
    fontSize = Math.max(12, Math.min(22, value));
    document.documentElement.style.setProperty('--desktop-font-scale', fontSize / 14);
    $('font-size').textContent = fontSize + 'px';
    $('font-decrease').disabled = fontSize === 12;
    $('font-increase').disabled = fontSize === 22;
    if (persist) { try { localStorage.setItem('codex-webui-font-size', String(fontSize)); } catch (_) { /* Optional preference only. */ } }
    resizePrompt(bottom);
    if (!bottom && anchor) scroller.scrollTop += anchor.getBoundingClientRect().top - offset;
  }
  $('font-decrease').addEventListener('click', () => setFontSize(fontSize - 1));
  $('font-increase').addEventListener('click', () => setFontSize(fontSize + 1));
  setFontSize(fontSize, false);
  let rateLimitSnapshot = null;
  let rateLimitRevision = 0;
  let rateLimitEpoch = 0;
  function clearRateLimits(text = '') {
    rateLimitSnapshot = null; rateLimitRevision++; rateLimitEpoch++;
    $('rate-limits').replaceChildren(document.createTextNode(text));
    $('rate-limits').removeAttribute('title');
  }
  function isClaude(session = state.selected) {
    return state.workers.find(worker => worker.ID === session?.worker_id)?.AgentKind === 'claude' || state.runtimes.find(runtime => runtime.runtime_id === session?.runtime_id)?.agent_kind === 'claude';
  }
  function agentLabel(session = state.selected) { return isClaude(session) ? 'Claude Code' : 'Codex'; }
  function sessionDeletionUnavailable(session = state.selected) {
    if (!session || !isClaude(session)) return '';
    const worker = state.workers.find(value => value.ID === session.worker_id);
    return worker?.SupportsSessionDeletion === true ? '' : 'Update this Claude Code worker to delete conversations.';
  }
  function sessionDeletionDescription(session) {
    return isClaude(session)
      ? 'This permanently removes this Claude conversation’s files and its MACT session data. The project directory and all project files will be kept.'
      : 'This permanently removes the conversation and any child sessions it created. The working directory and all files on disk will be kept.';
  }
  function statusNumber(value) { return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= Number.MAX_SAFE_INTEGER ? value : null; }
  function statusText(value) { return typeof value === 'string' && value.length <= 512 ? clean(value).trim() : ''; }
  function usd(value) {
    if (value === 0) return '$0.00';
    if (value < 0.0001) return '<$0.0001';
    return '$' + value.toLocaleString('en-US', { minimumFractionDigits: value < 0.01 ? 4 : 2, maximumFractionDigits: value < 0.01 ? 4 : 2 });
  }
  function exactUSD(value) { return value > 0 && value < 1e-8 ? '$' + value.toExponential(4) : value.toLocaleString('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 8 }); }
  function statusField(id, value, description) {
    const field = $(id);
    field.textContent = value; field.title = description; field.setAttribute('aria-label', description);
  }
  function claudeEffortLabel(value) { return ({ '': 'Auto', auto: 'Auto', default: 'Auto', low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Extra high', max: 'Max' })[value] || statusText(value); }
  function nativeClaudeUsage(value) {
    if (!value || statusNumber(value.observedAt) === null) return null;
    const spend = value.apiSpend;
    const money = spend?.currency === 'USD' && spend?.period === 'monthly';
    const limit = money ? statusNumber(spend.limit) : null, spent = money ? statusNumber(spend.spent) : null;
    const remaining = limit !== null && spent !== null ? Math.max(0, limit - spent) : null;
    return { plan: ['pro', 'max', 'team', 'enterprise'].includes(value.plan) ? value.plan : '', observedAt: value.observedAt,
      windows: Array.isArray(value.windows) ? value.windows.slice(0, 32).filter(row => statusText(row?.name) && statusNumber(row.usedPercent) !== null).map(row => ({ name: statusText(row.name), usedPercent: row.usedPercent, resetsAt: statusNumber(row.resetsAt) })) : [],
      apiSpend: money && typeof spend.enabled === 'boolean' ? { enabled: spend.enabled, limit, spent, remaining, currency: 'USD', period: 'monthly' } : null };
  }
  function clearClaudeStatus() {
    claudeStatus = null; claudeStatusRevision++;
    statusField('claude-cost', 'Session —', 'Claude Code has not reported this session’s consumption in USD.');
    statusField('claude-balance', 'Credits —', 'The API provider has not reported a remaining account credit balance.');
    statusField('claude-budget', 'API spend —', 'Claude Code has not reported the account’s spending allowance.');
    $('claude-limits').textContent = ''; $('claude-limits').hidden = true;
    if (isClaude()) renderClaudeStatus();
  }
  function applyClaudeStatus(raw, threadId) {
    if (!isClaude() || !state.authenticated || !state.connected || threadId !== state.selected?.codex_thread_id) return;
    const value = raw && typeof raw === 'object' && (!raw.provider || raw.provider === 'claude') ? raw : {};
    const balance = value.balance, budget = value.enterpriseBudget;
    claudeStatus = {
      model: statusText(value.model), modelDisplayName: statusText(value.modelDisplayName), usage: nativeClaudeUsage(value.usage), reasoningEffort: typeof value.reasoningEffort === 'string' ? statusText(value.reasoningEffort) : null, permissionMode: statusText(value.permissionMode),
      context: { usedTokens: statusNumber(value.context?.usedTokens), maxTokens: statusNumber(value.context?.maxTokens) },
      cost: { sessionUSD: statusNumber(value.cost?.sessionUSD), lastTurnUSD: statusNumber(value.cost?.lastTurnUSD) },
      balance: balance && ['USD', 'credits'].includes(balance.unit) && statusNumber(balance.remaining) !== null
        ? { remaining: balance.remaining, unit: balance.unit, observedAt: statusNumber(balance.observedAt) } : null,
      enterpriseBudget: budget && budget.currency === 'USD' && budget.period === 'monthly' && statusNumber(budget.spent) !== null &&
        (budget.unlimited === true || statusNumber(budget.limit) !== null && statusNumber(budget.remaining) !== null)
        ? { limit: statusNumber(budget.limit), spent: budget.spent, remaining: statusNumber(budget.remaining), currency: budget.currency, period: budget.period, unlimited: budget.unlimited === true, observedAt: statusNumber(budget.observedAt) } : null,
    };
    claudeStatusRevision++;
    renderClaudeStatus();
  }
  function applyClaudeSettings(settings) {
    if (!isClaude() || !settings || !state.connected) return;
    const next = { ...(claudeStatus || {}), provider: 'claude' };
    if (Object.hasOwn(settings, 'model')) { next.model = settings.model; if (settings.model !== claudeStatus?.model) next.modelDisplayName = ''; }
    if (Object.hasOwn(settings, 'effort')) next.reasoningEffort = settings.effort;
    if (Object.hasOwn(settings, 'reasoningEffort')) next.reasoningEffort = settings.reasoningEffort;
    if (Object.hasOwn(settings, 'permissionMode')) next.permissionMode = settings.permissionMode;
    applyClaudeStatus(next, state.selected?.codex_thread_id);
  }
  function handleClaudeStatusEvent(message) {
    if (message.method !== 'thread/claudeStatus/updated') return false;
    if (message.id === undefined) applyClaudeStatus(message.params?.claudeStatus, message.params?.threadId);
    return true;
  }
  function renderClaudeStatus() {
    if (!isClaude()) return;
    const status = claudeStatus;
    statusField('model', status?.modelDisplayName || currentModelInfo()?.displayName || status?.model || 'Model —', status?.model ? 'Claude Code reported model: ' + status.model + '. Choose model.' : 'The current Claude Code model has not been reported. Choose model.');
    statusField('effort', status?.reasoningEffort != null ? claudeEffortLabel(status.reasoningEffort) : 'Effort —', status?.reasoningEffort != null ? 'Claude Code effort: ' + claudeEffortLabel(status.reasoningEffort) + '. Choose effort level.' : 'Claude Code has not reported the effort level.');
    const permissions = { default: 'Default', manual: 'Manual', acceptEdits: 'Accept edits', plan: 'Plan mode', bypassPermissions: 'Bypass permissions', dontAsk: 'Don’t ask', auto: 'Auto' };
    const permission = permissions[status?.permissionMode];
    statusField('permissions', permission || 'Permissions —', permission ? 'Claude Code reported permission mode: ' + status.permissionMode + '. Choose permissions.' : 'The current Claude Code permission mode has not been reported. Choose permissions.');
    $('permissions').dataset.mode = permission ? status.permissionMode : 'unknown';
    const used = status?.context.usedTokens ?? null, maximum = status?.context.maxTokens ?? null;
    const context = used !== null && maximum > 0 ? Math.round(used / maximum * 100) + '% context'
      : used !== null ? 'Context ' + used.toLocaleString('en-US', { notation: 'compact', maximumFractionDigits: 1 }) : 'Context —';
    const contextDescription = used !== null ? 'Reported context: ' + used.toLocaleString('en-US') + ' tokens' + (maximum > 0 ? ' of ' + maximum.toLocaleString('en-US') + '.' : '; context window size unavailable.') : 'Claude Code has not reported the current context usage.';
    statusField('usage', context, contextDescription);
    const cost = status?.cost.sessionUSD ?? null, last = status?.cost.lastTurnUSD ?? null;
    statusField('claude-cost', cost !== null ? 'Session ' + usd(cost) : 'Session —', cost !== null
      ? 'Claude Code reported cumulative session consumption: ' + exactUSD(cost) + ' USD. Provider billing can differ.' + (last !== null ? ' Last reported turn: ' + exactUSD(last) + ' USD.' : '')
      : 'Claude Code has not reported this session’s consumption in USD.');
    const balance = status?.balance;
    const observed = balance?.observedAt > 0 ? timestamp(balance.observedAt) : null;
    statusField('claude-balance', balance ? balance.unit === 'USD' ? 'Balance ' + usd(balance.remaining) : balance.remaining.toLocaleString('en-US', { maximumFractionDigits: 4 }) + ' credits left' : 'Credits —', balance
      ? 'API provider remaining account credit: ' + (balance.unit === 'USD' ? exactUSD(balance.remaining) + ' USD' : balance.remaining.toLocaleString('en-US', { maximumFractionDigits: 8 }) + ' credits') + '. Based on the latest provider totals; finalized usage can lag activity.' + (observed ? ' Updated ' + observed.toLocaleString() + '.' : '')
      : 'The API provider has not reported a remaining account credit balance. Session consumption does not establish an account balance.');
    const nativeUsage = status?.usage, nativeSpend = nativeUsage?.apiSpend;
    const budget = nativeSpend || status?.enterpriseBudget;
    const budgetObserved = (nativeSpend ? nativeUsage.observedAt : budget?.observedAt) > 0 ? timestamp(nativeSpend ? nativeUsage.observedAt : budget.observedAt) : null;
    let budgetText = 'API spend —', budgetDescription = 'Claude Code has not reported the account’s spending allowance. Session cost does not establish a remaining allowance.';
    if (nativeSpend) {
      budgetText = nativeSpend.remaining !== null ? 'API spend ' + usd(nativeSpend.remaining) + ' / ' + usd(nativeSpend.limit) + ' left'
        : nativeSpend.spent !== null ? 'API spend ' + usd(nativeSpend.spent) + ' spent' : 'API spend —';
      budgetDescription = 'Claude Code /usage · Monthly API spend. ' + (nativeSpend.limit !== null ? 'Limit: ' + exactUSD(nativeSpend.limit) + '. ' : 'Limit not reported. ')
        + (nativeSpend.spent !== null ? 'Spent: ' + exactUSD(nativeSpend.spent) + '. ' : '') + (nativeSpend.remaining !== null ? 'Remaining: ' + exactUSD(nativeSpend.remaining) + '. ' : '')
        + (!nativeSpend.enabled ? 'Extra usage is disabled. ' : '');
    } else if (budget) {
      budgetText = budget.unlimited ? 'API spend unlimited' : 'API spend ' + usd(budget.remaining) + ' / ' + usd(budget.limit) + ' left';
      budgetDescription = 'Enterprise Admin API monthly member limit: ' + (budget.unlimited ? 'unlimited' : exactUSD(budget.limit)) + '. Spent: ' + exactUSD(budget.spent) + '. Includes applicable member, group or seat policies.';
    }
    if (budgetObserved) budgetDescription += ' Checked ' + budgetObserved.toLocaleString() + '.';
    statusField('claude-budget', budgetText, budgetDescription);
    $('claude-balance').hidden = !balance;
    $('claude-budget').hidden = !!balance && !budget;
    const windows = nativeUsage?.windows || [];
    $('claude-limits').hidden = !windows.length;
    statusField('claude-limits', windows.map(row => row.name + ' ' + Math.round(row.usedPercent) + '% used').join(' · '), windows.map(row => {
      const reset = row.resetsAt > 0 ? timestamp(row.resetsAt) : null;
      return row.name + ': ' + row.usedPercent + '% used' + (reset ? '; resets ' + reset.toLocaleString() : '');
    }).join('. '));
    settleBottom();
  }
  function title(session) { return session.name || session.title || session.codex_thread_id || 'Untitled session'; }
  function notificationLocation(id = '') {
    const destination = '/tgw/webui/' + (id && sessionUUID.test(id) ? '?session_id=' + id.toLowerCase() : '');
    history.replaceState(null, '', destination);
    $('auth').querySelector('a').href = '/tgw/webui/admin/?next=' + encodeURIComponent(destination);
  }
  function openNotification(url) {
    let destination;
    try { destination = new URL(url, location.origin); } catch (_) { return false; }
    if (destination.origin !== location.origin || destination.pathname !== '/tgw/webui/') return false;
    const id = destination.searchParams.get('session_id') || '';
    if (id && !sessionUUID.test(id)) return false;
    if (id) {
      notificationSessionID = id.toLowerCase(); notificationLocation(notificationSessionID);
      if (state.authenticated) refreshSessions();
    }
    return true;
  }
  let noticeRecovery = '', noticeText = '';
  let providerRetry = null, providerEventOrder = 0;
  let steerNoticeTimer = null, steerNoticeTurn = null;
  let capacityRetryTask = null, capacityRetryTimer = null;
  function renderNotice() {
    const error = state.authenticated && state.selected ? sessionErrors.get(state.selected.session_id) : null;
    const text = [error?.message, noticeText].filter((value, index, values) => value && values.indexOf(value) === index).join('\n\n');
    $('notice').textContent = text; $('notice').hidden = !text;
    renderCapacityRetry(error);
  }
  function notice(text = '', recovery = '') {
    noticeText = text;
    noticeRecovery = text ? recovery : '';
    providerRetry = null;
    renderNotice();
  }
  function errorWasDismissed(id, value) {
    const dismissed = sessionErrorDismissals.get(id);
    return dismissed && (dismissed.cancelledMessage && !value.revision && !value.turnID && value.message === dismissed.cancelledMessage || value.revision && value.revision === dismissed.revision || value.turnID && value.turnID === dismissed.turnID);
  }
  function rememberSessionError(id, message, turnID = '', revision = '', source = 'native', capacity = false) {
    if (!id || typeof message !== 'string') return;
    message = clean(message).trim().slice(0, 8192);
    if (!message) return;
    const value = { message, turnID, revision, source, capacity };
    if (errorWasDismissed(id, value)) return;
    if (turnID && source === 'activity' && state.selected?.session_id === id && state.turn && state.turn !== turnID) return;
    const previous = sessionErrors.get(id);
    // A live result may carry only a generic fallback after the activity feed
    // already delivered the detailed, durable error for this same turn.
    if (source === 'native' && previous?.source === 'activity' && previous.turnID === turnID) return;
    sessionErrors.set(id, value);
    if (source === 'activity' && turnID) sessionErrorTurns.set(id, turnID);
    if (state.selected?.session_id === id) renderNotice();
  }
  function dismissSessionError(id = state.selected?.session_id) {
    if (!id) return;
    const previous = sessionErrors.get(id);
    if (previous) sessionErrorDismissals.set(id, previous);
    else if (sessionErrorTurns.has(id)) sessionErrorDismissals.set(id, { turnID: sessionErrorTurns.get(id) });
    sessionErrors.delete(id);
    if (state.selected?.session_id === id) renderNotice();
  }
  function startSessionTurn(id, turnID) {
    if (!id || !turnID) return;
    const previous = sessionErrors.get(id);
    const dismissal = sessionErrorDismissals.get(id); if (dismissal) { delete dismissal.historySuppressed; delete dismissal.cancelledMessage; }
    if (previous && previous.turnID !== turnID) dismissSessionError(id);
    sessionErrorTurns.set(id, turnID);
  }
  function applyActivityError(row) {
    const id = row.session_id;
    if (row.active_turn_id) startSessionTurn(id, row.active_turn_id);
    if (applyCapacityRetryState(row)) return;
    if (typeof row.error_message === 'string' && row.error_message.trim()) {
      rememberSessionError(id, row.error_message, typeof row.error_turn_id === 'string' ? row.error_turn_id : '',
        typeof row.error_revision === 'string' ? row.error_revision : '', 'activity', row.capacity_error === true);
    } else {
      // An empty activity row can race ahead of a just-received native error.
      // Only clear errors this feed has acknowledged; a new turn explicitly
      // clears native-only errors even when an older gateway lacks this field.
      if (sessionErrors.get(id)?.source === 'activity') sessionErrors.delete(id);
      if (state.selected?.session_id === id) renderNotice();
    }
  }
  function rememberLatestTurnError(turn) {
    const id = state.selected?.session_id;
    if (!id || !turn?.id) return;
    // A prompt can fail before a new native turn exists. Its durable error is
    // newer than the latest saved turn, even though it has no turn ID yet.
    if (sessionErrors.has(id) || sessionErrorDismissals.get(id)?.historySuppressed) return;
    if (!sessionErrorTurns.has(id)) sessionErrorTurns.set(id, turn.id);
    if (turn.error) rememberSessionError(id, turn.error.message || 'The turn failed.', turn.id);
  }
  function clearCapacityRetryView() {
    clearTimeout(capacityRetryTimer); capacityRetryTimer = null;
    capacityRetryTask?.controller?.abort(); capacityRetryTask = null;
    $('capacity-retry').hidden = true; $('notice').removeAttribute('data-capacity-retry');
  }
  function capacityRetryCurrent(task) {
    return capacityRetryTask === task && state.authenticated && task.authGeneration === state.authGeneration &&
      state.selected?.session_id === task.sessionID && sessionErrors.get(task.sessionID)?.revision === task.revision;
  }
  function capacityRetryHandled(id, revision, turnID, message = '', retryState = '') {
    const previous = sessionErrors.get(id);
    if (previous && previous.revision && previous.revision !== revision) return;
    if (retryState === 'cancelled' && capacityRetryTask?.sessionID === id && capacityRetryTask?.revision === revision) capacityRetryTask.cancelled = true;
    sessionErrorDismissals.set(id, { revision, turnID, historySuppressed: true, cancelledMessage: message || previous?.message || '' });
    sessionErrors.delete(id);
    if (state.selected?.session_id === id) renderNotice();
  }
  function applyCapacityRetryState(row) {
    const id = row.session_id, revision = row.capacity_retry_revision;
    const matches = typeof revision === 'string' && revision && (!row.error_revision || row.error_revision === revision);
    if (matches && !row.active_turn_id && ['cancelled', 'submitted'].includes(row.capacity_retry_state)) {
      capacityRetryHandled(id, revision, row.capacity_retry_turn_id || '', row.error_message || '', row.capacity_retry_state);
      return true;
    }
    if (row.error_revision && row.error_revision !== revision) {
      const dismissal = sessionErrorDismissals.get(id);
      if (dismissal) sessionErrorDismissals.delete(id);
    }
    return false;
  }
  async function capacityRetryRequest(task, action) {
    task.controller?.abort(); const controller = new AbortController(); task.controller = controller;
    const timeout = setTimeout(() => controller.abort(), 10000);
    try {
      const response = await fetch(api + '/sessions/capacity-retry', { method: 'POST', credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': window.CodexSessionAuth.cookie('__Host-telegramgw-csrf') },
        body: JSON.stringify({ session_id: task.sessionID, error_revision: task.revision, action }) });
      if (response.status === 401) { expire(); throw new Error('Sign in to manage this retry.'); }
      if (!response.ok) { const error = new Error('Capacity retry is no longer available.'); error.response = true; throw error; }
      const value = await response.json(), retry = value.retry;
      if (!retry || retry.error_revision !== task.revision || !['waiting', 'cancelled', 'submitted', 'superseded'].includes(retry.state) ||
          !Number.isSafeInteger(retry.due_at_ms) || !Number.isSafeInteger(value.server_time_ms)) throw new Error('Invalid retry status.');
      return value;
    } finally { clearTimeout(timeout); }
  }
  function paintCapacityRetry(task) {
    clearTimeout(capacityRetryTimer);
    if (!capacityRetryCurrent(task)) return;
    const seconds = Math.max(0, Math.ceil((task.deadline - Date.now()) / 1000));
    const label = task.phase === 'cancelling' ? 'Cancelling…'
      : task.phase === 'unknown' ? task.message || 'Retry status unavailable. You can still cancel.'
      : seconds ? 'Retrying in ' + seconds + (seconds === 1 ? ' second' : ' seconds') : 'Retrying…';
    if ($('capacity-retry-countdown').textContent !== label) $('capacity-retry-countdown').textContent = label;
    $('capacity-retry-cancel').disabled = task.phase === 'cancelling' || task.phase === 'waiting' && seconds === 0;
    if (!['unknown', 'cancelling'].includes(task.phase)) capacityRetryTimer = setTimeout(() => paintCapacityRetry(task), 250);
  }
  function acceptCapacityRetry(task, value) {
    if (!capacityRetryCurrent(task)) return;
    const retry = value.retry;
    if (['cancelled', 'submitted'].includes(retry.state)) { capacityRetryHandled(task.sessionID, task.revision, retry.turn_id || '', task.messageText, retry.state); return; }
    if (retry.state === 'superseded') { task.phase = 'stopped'; clearTimeout(capacityRetryTimer); $('capacity-retry').hidden = true; $('notice').removeAttribute('data-capacity-retry'); return; }
    task.phase = 'waiting'; task.deadline = Date.now() + Math.max(0, retry.due_at_ms - value.server_time_ms);
    paintCapacityRetry(task);
  }
  async function scheduleCapacityRetry(task) {
    const operation = task.operation;
    for (let attempt = 0; attempt < 2; attempt++) {
      try {
        const value = await capacityRetryRequest(task, 'schedule');
        if (capacityRetryCurrent(task) && task.operation === operation) acceptCapacityRetry(task, value);
        return;
      } catch (error) {
        if (!capacityRetryCurrent(task) || task.operation !== operation) return;
        if (attempt === 0 && !error.response) continue; // Same failure/deadline: idempotent, never a prompt replay.
        task.phase = 'unknown'; task.message = 'Retry status unavailable. You can still cancel.'; paintCapacityRetry(task); return;
      }
    }
  }
  async function cancelCapacityRetry(task = capacityRetryTask) {
    if (!task || !capacityRetryCurrent(task) || task.phase === 'cancelling') return false;
    task.operation++; task.phase = 'cancelling'; paintCapacityRetry(task);
    for (let attempt = 0; attempt < 2; attempt++) {
      try {
        const value = await capacityRetryRequest(task, 'cancel');
        if (capacityRetryCurrent(task)) acceptCapacityRetry(task, value);
        return value.retry.state === 'cancelled';
      } catch (error) {
        if (!capacityRetryCurrent(task)) return task.cancelled === true;
        if (attempt === 0 && !error.response) continue;
        task.phase = 'unknown'; task.message = 'Cancellation was not confirmed. Try Cancel again.'; paintCapacityRetry(task); return false;
      }
    }
  }
  function renderCapacityRetry(error) {
    const visible = state.authenticated && state.selected && error?.capacity && error.revision && !state.turn;
    if (!visible) { clearCapacityRetryView(); return; }
    const id = state.selected.session_id;
    if (capacityRetryTask?.sessionID !== id || capacityRetryTask?.revision !== error.revision) {
      clearCapacityRetryView();
      if (!state.connected || state.loading) return;
      capacityRetryTask = { sessionID: id, revision: error.revision, authGeneration: state.authGeneration, operation: 0,
        phase: 'scheduling', deadline: Date.now() + 30000, messageText: error.message };
      scheduleCapacityRetry(capacityRetryTask);
    }
    if (capacityRetryTask.phase === 'stopped') return;
    $('capacity-retry').hidden = false; $('notice').dataset.capacityRetry = 'true'; paintCapacityRetry(capacityRetryTask);
  }
  $('capacity-retry-cancel').addEventListener('click', () => cancelCapacityRetry());
  function recoverNotice(...scopes) {
    // A recovered transport does not confirm an interrupted send or resolve
    // an unrelated Codex error. Clear only the operation that recovered.
    if (scopes.includes(noticeRecovery)) notice();
  }
  function recoverProviderRetry(message) {
    const retry = providerRetry, p = message.params || {};
    if (!retry || noticeRecovery !== 'retry:' + retry.turnID || !p.turnId || p.turnId !== retry.turnID) return;
    if (!Number.isSafeInteger(message._providerEventOrder) || !Number.isSafeInteger(retry.order) || message._providerEventOrder <= retry.order || itemMilliseconds(retry.observedAt) === undefined) return;
    const item = p.item, previous = item?.id ? state.items.get(item.id) : null;
    const completed = message.method === 'item/completed';
    const modelOutput = completed && ['agentMessage', 'reasoning'].includes(item?.type) && clean(itemText(item)).trim();
    const modelDelta = ['item/agentMessage/delta', 'item/reasoning/summaryTextDelta'].includes(message.method) && typeof p.delta === 'string' && clean(p.delta).trim();
    // A completed history item replayed over the live connection is not new
    // provider output. Lifecycle dates also reject delayed pre-retry frames.
    const completedAt = itemMilliseconds(p.completedAtMs) ?? itemMilliseconds(item?.completedAtMs);
    if (modelDelta || modelOutput && !previous?._complete && !retry.completedItems.has(item.id) && (completedAt === undefined || completedAt >= retry.observedAt)) {
      recoverNotice('retry:' + retry.turnID); return;
    }
    // An old command or background agent can finish during an upstream retry.
    // A fresh native tool invocation demonstrates that the model resumed issuing
    // work, even when the worker withholds raw model/reasoning stream deltas.
    const toolCall = item && (
      item.type === 'commandExecution' && typeof item.command === 'string' && item.command.trim() ||
      ['dynamicToolCall', 'mcpToolCall'].includes(item.type) && typeof item.tool === 'string' && item.tool.trim() ||
      item.type === 'webSearch' && (item.query || item.action?.query) ||
      item.type === 'fileChange' && Array.isArray(item.changes) && item.changes.length
    );
    if (!toolCall || typeof item.id !== 'string' || !item.id || !['item/started', 'item/completed'].includes(message.method) || retry.knownItems.has(item.id) || previous?._complete) return;
    const startedAt = itemMilliseconds(p.startedAtMs) ?? itemMilliseconds(item.startedAtMs) ?? legacyItemMilliseconds(item.createdAt);
    if (startedAt !== undefined && startedAt < retry.observedAt) return;
    // Completion alone needs an authoritative post-retry start date; otherwise
    // it could belong to work that began before this viewer attached.
    if (completed && startedAt === undefined) return;
    recoverNotice('retry:' + retry.turnID);
  }
  function clearSteerNotice() {
    clearTimeout(steerNoticeTimer); steerNoticeTimer = null; steerNoticeTurn = null;
    $('steer-notice').hidden = true; $('steer-notice').textContent = '';
  }
  function queuedSteer(turnId) {
    if (!state.connected || state.loading || !turnId || state.turn !== turnId) return;
    clearSteerNotice(); steerNoticeTurn = turnId;
    $('steer-notice').textContent = '✓ Message queued'; $('steer-notice').hidden = false;
    steerNoticeTimer = setTimeout(clearSteerNotice, 4000);
    settleBottom();
  }
  function connection(kind, text) { $('connection').dataset.state = kind; $('connection-text').textContent = text; $('reconnect').textContent = 'Reconnect'; $('reconnect').hidden = !state.selected || !['disconnected', 'error'].includes(kind); renderTelegramBots(); }
  function telegramBotContextAvailable() {
    const session = state.selected;
    return state.authenticated && !authRotationPending && !!session && !session.archived && !session.deleted &&
      state.sessions.some(row => row.session_id === session.session_id && !row.archived && !row.deleted);
  }
  function telegramBotStatus(text = '') {
    clearTimeout(telegramBots.errorTimer); telegramBots.errorTimer = null;
    $('telegram-bot-status').textContent = text; $('telegram-bot-status').hidden = !text;
    if (text) telegramBots.errorTimer = setTimeout(() => telegramBotStatus(), 6000);
  }
  function renderTelegramBots() {
    const select = $('telegram-bot');
    const rows = state.authenticated ? telegramBots.rows : [];
    const sessionNames = new Map(state.sessions.map(session => [session.session_id, title(session)]));
    const labels = rows.map(row => row.display_name + ' · ' + (row.session_id ? sessionNames.get(row.session_id) || 'Session unavailable' : '<no session>'));
    if (select.options.length !== rows.length + 1 || rows.some((row, index) => select.options[index + 1]?.value !== row.id || select.options[index + 1]?.textContent !== labels[index])) {
      const none = document.createElement('option'); none.value = ''; none.textContent = '<no bot>';
      select.replaceChildren(none, ...rows.map((row, index) => { const option = document.createElement('option'); option.value = row.id; option.textContent = labels[index]; return option; }));
    }
    const sessionID = state.selected?.session_id;
    // Legacy duplicate bindings are displayed deterministically. Rendering
    // never changes them; an explicit choice performs the exclusive update.
    const assigned = rows.filter(row => row.session_id === sessionID).sort((a, b) => a.id.localeCompare(b.id))[0]?.id || '';
    const pending = telegramBots.pending;
    select.value = pending && pending.sessionID === sessionID && pending.epoch === telegramBots.epoch ? pending.botID : assigned;
    select.title = select.selectedOptions[0]?.textContent || '<no bot>';
    select.disabled = !telegramBotContextAvailable() || !telegramBots.ready || !!pending;
    select.setAttribute('aria-busy', String(!!pending));
  }
  function validTelegramBots(rows, revision) {
    return typeof revision === 'string' && Array.isArray(rows) && rows.every(row => row && typeof row.id === 'string' && row.id.length > 0 &&
      typeof row.display_name === 'string' && typeof row.session_id === 'string') && new Set(rows.map(row => row.id)).size === rows.length;
  }
  function applyTelegramBots(rows, revision, source = 'http') {
    if (!validTelegramBots(rows, revision)) return false;
    if (source === 'activity') {
      telegramBots.activityRevision = revision;
      if (telegramBots.httpRevision && telegramBots.httpRevision !== revision) {
        // Opaque revisions cannot order HTTP against the activity stream. Keep
        // a confirmed HTTP result until the stream catches up or a fresh read
        // confirms this different revision as the current registry state.
        telegramBots.epoch++; refreshTelegramBots(); return true;
      }
      telegramBots.httpRevision = '';
    } else telegramBots.httpRevision = telegramBots.activityRevision === revision ? '' : revision;
    telegramBots.rows = rows.map(row => ({ id: row.id, display_name: row.display_name, session_id: row.session_id }));
    telegramBots.revision = revision; telegramBots.ready = true; telegramBots.epoch++;
    renderTelegramBots(); return true;
  }
  function resetTelegramBots() {
    telegramBots.read?.controller.abort(); telegramBots.pending?.controller.abort();
    telegramBots.read = null; telegramBots.readAgain = false; telegramBots.pending = null; telegramBots.rows = []; telegramBots.revision = ''; telegramBots.activityRevision = ''; telegramBots.httpRevision = ''; telegramBots.ready = false; telegramBots.epoch++;
    telegramBotStatus(); renderTelegramBots();
  }
  async function refreshTelegramBots() {
    if (!state.authenticated || authRotationPending) return;
    if (telegramBots.read) { telegramBots.readAgain = true; return; }
    telegramBots.readAgain = false;
    const operation = { controller: new AbortController(), authGeneration: state.authGeneration, epoch: telegramBots.epoch };
    telegramBots.read = operation;
    const timeout = setTimeout(() => operation.controller.abort(), 15000);
    try {
      const data = await fetchJSON(api + '/telegram-bots', operation.controller.signal);
      if (telegramBots.read !== operation || operation.authGeneration !== state.authGeneration || !state.authenticated || operation.epoch !== telegramBots.epoch) return;
      if (!applyTelegramBots(data.bots, data.revision)) throw new Error('Invalid bot inventory');
    } catch (_) {
      if (telegramBots.read === operation && operation.authGeneration === state.authGeneration && state.authenticated && !telegramBots.ready) telegramBotStatus('Telegram bots unavailable.');
    } finally {
      clearTimeout(timeout);
      if (telegramBots.read === operation) {
        telegramBots.read = null;
        if (telegramBots.readAgain) queueMicrotask(refreshTelegramBots);
      }
    }
  }
  async function changeTelegramBot() {
    const botID = $('telegram-bot').value;
    if (!telegramBotContextAvailable() || !telegramBots.ready || telegramBots.pending || (botID && !telegramBots.rows.some(row => row.id === botID))) { renderTelegramBots(); return; }
    const operation = { sessionID: state.selected.session_id, botID, authGeneration: state.authGeneration, epoch: telegramBots.epoch, controller: new AbortController() };
    telegramBots.pending = operation; telegramBotStatus(); renderTelegramBots();
    const timeout = setTimeout(() => operation.controller.abort(), 15000);
    const current = () => telegramBots.pending === operation && operation.authGeneration === state.authGeneration && state.authenticated;
    let failed = false, reconcile = false;
    try {
      const response = await fetch(api + '/telegram-bots', { method: 'POST', credentials: 'same-origin', cache: 'no-store', signal: operation.controller.signal,
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': window.CodexSessionAuth.cookie('__Host-telegramgw-csrf') }, body: JSON.stringify({ session_id: operation.sessionID, bot_id: botID }) });
      if (!current()) return;
      if (response.status === 401) { expire(); return; }
      if (!response.ok) throw new Error('Assignment failed');
      const data = await response.json();
      if (!current()) return;
      // A sequenced live update received during this request is newer evidence
      // than an HTTP response that may have been delayed in transit.
      if (operation.epoch === telegramBots.epoch) {
        if (!applyTelegramBots(data.bots, data.revision)) throw new Error('Invalid bot inventory');
      } else reconcile = true;
    } catch (_) {
      if (!current()) return;
      failed = true; reconcile = true;
    } finally {
      clearTimeout(timeout);
      if (current()) {
        telegramBots.pending = null; renderTelegramBots();
        if (failed && state.selected?.session_id === operation.sessionID) telegramBotStatus('Could not update the Telegram bot. Try again.');
        if (reconcile) refreshTelegramBots();
      }
    }
  }
  $('telegram-bot').addEventListener('change', changeTelegramBot);
  function showSessions(open) {
    const mobile = matchMedia('(max-width:650px)').matches;
    if (mobile) blurEditable();
    document.body.classList.toggle('sessions-open', open);
    $('sidebar-backdrop').hidden = !open;
    $('show-sessions').setAttribute('aria-expanded', String(open));
    if (open) {
      (mobile ? $('close-sessions') : $('session-search')).focus({ preventScroll: true });
      if (state.authenticated) refreshSessions();
    }
  }
  function blurEditable() {
    const active = document.activeElement;
    if (active?.matches('input, textarea, select, [contenteditable="true"]')) active.blur();
  }
  function openSettings() {
    blurEditable();
    showSessions(false);
    const dialog = $('settings-dialog');
    if (!dialog.open) dialog.showModal();
    $('open-settings').setAttribute('aria-expanded', 'true');
    $('close-settings').focus({ preventScroll: true });
  }
  function renderLogout() {
    const disabled = !state.authenticated || authRenewing || authRotationPending || !!logoutPending;
    $('open-logout').hidden = !state.authenticated;
    $('open-logout').disabled = disabled;
    $('logout-confirm').disabled = disabled;
    $('logout-cancel').disabled = !!logoutPending;
    $('logout-confirm').textContent = logoutPending ? 'Logging out…' : 'Log out';
    $('logout-dialog').setAttribute('aria-busy', String(!!logoutPending));
  }
  function logoutError(message = '') {
    $('logout-error').textContent = message;
    $('logout-error').hidden = !message;
  }
  function openLogout() {
    if (!state.authenticated || authRenewing || authRotationPending || logoutPending) return;
    blurEditable(); showSessions(false); logoutError(); renderLogout();
    if (!$('logout-dialog').open) $('logout-dialog').showModal();
    $('open-logout').setAttribute('aria-expanded', 'true');
    $('logout-cancel').focus({ preventScroll: true });
  }
  async function clearLogoutPush() {
    try { localStorage.removeItem('codex-webui-push-subscription'); } catch (_) { /* Storage can be disabled. */ }
    const generation = state.authGeneration;
    let timer, active = true;
    const current = () => active && generation === state.authGeneration && !state.authenticated;
    try {
      if (navigator.serviceWorker) await Promise.race([
        navigator.serviceWorker.getRegistration('/tgw/webui/').then(registration => current() ? registration?.pushManager?.getSubscription() : null).then(subscription => current() ? subscription?.unsubscribe() : null),
        new Promise(resolve => { timer = setTimeout(resolve, 3000); }),
      ]);
    } catch (_) { /* Server logout already revokes push devices bound to this login. */ }
    finally { active = false; clearTimeout(timer); }
  }
  async function confirmLogout() {
    if (!$('logout-dialog').open || !state.authenticated || authRenewing || authRotationPending || logoutPending) return;
    const operation = { sessionID: sessionAuth.snapshot()?.session_id };
    logoutPending = operation; logoutError(); renderLogout();
    try {
      // Logging out needs the current cookie and CSRF token, without a new
      // passkey ceremony. Renewal is fenced until this request settles.
      const response = await fetch('/tgw/api/v1/admin/logout', { method: 'POST', credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(15000),
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': window.CodexSessionAuth.cookie('__Host-telegramgw-csrf') }, body: '{}' });
      if (logoutPending !== operation) return;
      if (!response.ok && response.status !== 401) throw new Error('Log out could not be confirmed (' + response.status + '). Try again.');
      logoutPending = null;
      // A concurrent expiry check may already have locked the view. Explicit
      // logout must still discard the recovery key and remembered selection.
      const alreadyLocked = !state.authenticated && !sessionAuth.snapshot();
      sessionAuth.expire('logout');
      if (alreadyLocked) lockAuthentication('logout');
      sessionAuth.broadcast('logout');
      void clearLogoutPush();
    } catch (error) {
      if (logoutPending !== operation) return;
      const message = error.name === 'AbortError' || error.name === 'TimeoutError' || error instanceof TypeError
        ? 'Log out could not be confirmed. Check your connection and try again. You may still be signed in.' : error.message;
      if ($('logout-dialog').open) logoutError(message);
      else $('auth-status').textContent = message;
    } finally {
      if (logoutPending === operation) logoutPending = null;
      renderLogout();
    }
  }
  function goalContextAvailable() {
    const session = state.selected;
    return !isClaude(session) && state.authenticated && state.connected && !!session && !session.archived && !session.deleted &&
      state.sessions.some(value => value.session_id === session.session_id && !value.archived && !value.deleted);
  }
  function renderGoal() {
    const visible = goalContextAvailable() && selectedGoal?.sessionID === state.selected.session_id && selectedGoal.threadId === state.selected.codex_thread_id;
    $('show-goal').hidden = !visible;
    // The objective is untrusted session text, including when updated live.
    $('goal-objective').textContent = visible ? selectedGoal.objective : '';
    if (!visible && $('goal-dialog').open) $('goal-dialog').close();
    if (!visible) $('show-goal').setAttribute('aria-expanded', 'false');
  }
  function applyGoal(goal, threadId) {
    selectedGoal = goal?.status === 'active' && typeof goal.objective === 'string' && goal.objective.trim() ?
      { sessionID: state.selected.session_id, threadId, objective: goal.objective } : null;
    renderGoal();
  }
  function clearGoal() {
    goalRevision++; goalSnapshotSequence++; selectedGoal = null;
    renderGoal();
  }
  function refreshGoal(read = rpc) {
    if (!goalContextAvailable()) return;
    const generation = state.generation, threadId = state.selected.codex_thread_id, sessionID = state.selected.session_id;
    const revision = goalRevision, sequence = ++goalSnapshotSequence;
    const current = () => generation === state.generation && threadId === state.selected?.codex_thread_id && sessionID === state.selected?.session_id &&
      goalContextAvailable() && sequence === goalSnapshotSequence && revision === goalRevision;
    read('thread/goal/get', { threadId }).then(result => {
      if (current()) applyGoal(result?.goal, threadId);
    }).catch(() => {
      // This optional native read is unavailable on older runtimes.
      if (current()) applyGoal(null, threadId);
    });
  }
  function handleGoalEvent(message) {
    if (!['thread/goal/updated', 'thread/goal/cleared'].includes(message.method)) return false;
    const params = message.params;
    if (goalContextAvailable() && params?.threadId === state.selected.codex_thread_id) {
      goalRevision++;
      applyGoal(message.method === 'thread/goal/cleared' ? null : params.goal, params.threadId);
    }
    return true;
  }
  function openGoal() {
    renderGoal();
    if ($('show-goal').hidden) return;
    blurEditable();
    goalDialogSessionID = state.selected.session_id;
    if (!$('goal-dialog').open) $('goal-dialog').showModal();
    $('show-goal').setAttribute('aria-expanded', 'true');
    $('close-goal').focus({ preventScroll: true });
  }
  function sessionActivity(session) {
    const current = { ...session, ...activity.sessions.get(session.session_id) };
    const selected = state.selected?.session_id === session.session_id && state.connected && !state.loading;
    const localQuestions = selected ? [...state.questions.values()].filter(question => !question.answered).length : 0;
    const authoritative = activity.sessions.has(session.session_id);
    const pending = authoritative ? Number(current.pending_questions || 0) : Math.max(Number(current.pending_approvals || 0), localQuestions);
    const running = authoritative ? !!current.active_turn_id || isRunning(current.state) : selected ? !!state.turn : !!current.active_turn_id || isRunning(current.state);
    return { ...current, pending, running, kind: pending ? 'question' : running ? 'running' : 'idle' };
  }
  function updateSessionIndicators() {
    let pending = 0, running = 0;
    const buttons = new Map([...$('session-list').querySelectorAll('.session-button')].map(button => [button.dataset.sessionId, button]));
    const groups = new Map([...$('session-list').querySelectorAll('.session-worker')].map(element => [element.dataset.workerId, { element, active: 0, kind: 'idle' }]));
    for (const session of state.sessions) {
      if (session.archived || session.deleted) continue;
      const current = sessionActivity(session);
      pending += current.pending;
      if (current.running) running++;
      const button = buttons.get(session.session_id);
      if (!button) continue;
      const group = groups.get(session.worker_id);
      if (group && current.kind !== 'idle') {
        group.active++;
        if (current.kind === 'question' || group.kind === 'idle') group.kind = current.kind;
      }
      button.dataset.activity = current.kind;
      const offline = current.worker_connectivity && !['connected', 'online'].includes(current.worker_connectivity);
      const status = current.pending ? '? Answer requested' : offline ? '○ Worker ' + current.worker_connectivity : current.running ? '● Working' : '○ ' + (isRunning(current.state) ? 'idle' : current.state || 'Saved');
      const detail = button.querySelector('.detail');
      const workspace = session.cwd || 'Workspace unavailable';
      if (detail.querySelector('.session-status-label')?.textContent !== status || detail.querySelector('.session-workspace')?.textContent !== workspace) {
        const label = node('span', 'session-status-label');
        if (status === '● Working') label.append(node('span', 'session-working-dot activity-dot', '●'), document.createTextNode(' Working'));
        else label.textContent = status;
        detail.replaceChildren(label, document.createTextNode(' · '), node('span', 'session-workspace', workspace));
      }
      detail.classList.toggle('running', current.running && !current.pending && !offline);
      button.setAttribute('aria-label', title(session) + ' · ' + status);
    }
    for (const { element, active, kind } of groups.values()) {
      const heading = element.querySelector('.session-group'), toggle = element.querySelector('.session-group-toggle');
      const badge = element.querySelector('.session-group-active-count'), count = element.querySelector('.session-group-count');
      const description = active + (active === 1 ? ' active session' : ' active sessions');
      heading.dataset.activity = kind;
      badge.textContent = String(active); badge.hidden = !active; badge.title = description;
      const action = toggle.getAttribute('aria-expanded') === 'true' ? 'Collapse' : 'Expand';
      toggle.setAttribute('aria-label', action + ' sessions for ' + element.querySelector('.worker-name').textContent + ' · ' + count.title + (active ? ' · ' + description : ''));
    }
    const menu = $('show-sessions');
    menu.dataset.activity = pending ? 'question' : running ? 'running' : 'idle';
    menu.textContent = pending ? '?' : '☰';
    menu.setAttribute('aria-label', pending ? 'Choose a session · ' + pending + ' pending questions or approvals' : running ? 'Choose a session · ' + running + ' sessions working' : 'Choose a session');
    menu.title = pending ? 'A session needs your answer' : running ? 'A session is working' : 'Choose a session';
  }
  function activityStatus(kind, text) {
    $('activity-status').dataset.state = kind;
    $('activity-status').textContent = text;
  }
  function settingsCheckpoint() { const serial = sessionSettings.get(state.selected?.session_id)?.serial || 0; return isClaude() ? serial + ':' + claudeStatusRevision : serial; }
  function applySessionSettings(id, model, effort, source, revision = '') {
    if (!id || typeof model !== 'string' || typeof effort !== 'string') return;
    sessionSettings.set(id, { model, effort, source, revision, serial: ++settingsSerial });
    const session = state.sessions.find(value => value.session_id === id);
    if (session) session.stats = { ...session.stats, model, reasoning_effort: effort };
    if (state.selected?.session_id === id) {
      state.selected.stats = { ...state.selected.stats, model, reasoning_effort: effort };
      if (isClaude()) applyClaudeSettings({ model, effort: source === 'unconfirmed' ? null : effort });
      renderModels(); updateControls();
    }
  }
  function applyActivitySettings(row) {
    const revision = row.settings_revision;
    if (typeof revision !== 'string' || activitySettingsRevisions.get(row.session_id) === revision) return;
    if (revision && (!/^[1-9]\d*:[1-9]\d*$/.test(revision) || typeof row.model !== 'string' || typeof row.reasoning_effort !== 'string')) return;
    activitySettingsRevisions.set(row.session_id, revision);
    if (revision) applySessionSettings(row.session_id, row.model, row.reasoning_effort, 'activity', revision);
    else if (sessionSettings.get(row.session_id)?.source === 'activity') {
      // A changed runtime generation has no confirmed settings yet. Do not
      // display a previous generation's model as the current configuration.
      applySessionSettings(row.session_id, '', '', 'unconfirmed');
    }
  }
  function closeActivity() {
    activity.generation++; clearTimeout(activity.timer); clearTimeout(activity.watchdog);
    activity.timer = null; activity.watchdog = null; activity.ready = false;
    if (activity.socket) { activity.socket.onclose = null; activity.socket.onmessage = null; activity.socket.close(); activity.socket = null; }
  }
  function connectActivity() {
    if (!state.authenticated || document.hidden || activity.socket) return;
    clearTimeout(activity.timer); activity.timer = null;
    const generation = ++activity.generation;
    const url = new URL(api + '/activity', location.href); url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'; url.searchParams.set('v', '3');
    const socket = new WebSocket(url); activity.socket = socket; activity.sequence = -1; activity.revision = -1; activity.ready = false;
    activityStatus(activity.attempts ? 'reconnecting' : 'connecting', activity.attempts ? 'Reconnecting live updates…' : 'Connecting live updates…');
    const recover = async code => {
      if (activity.generation !== generation || activity.socket !== socket) return;
      closeActivity();
      if (code === 4001 || code === 4401) { expire(); return; }
      if (!state.authenticated || document.hidden) return;
      const ticket = activity.generation;
      activityStatus('reconnecting', 'Reconnecting live updates…');
      // Failed upgrades hide HTTP401 from browser WebSockets. Recheck auth
      // before reconnecting, without interrupting the selected conversation.
      try {
        const response = await fetch('/tgw/api/v1/admin/session', { credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(10000) });
        if (ticket !== activity.generation) return;
        if (response.status === 401) { expire(); return; }
      } catch (_) { /* Keep last-known indicators during a network outage. */ }
      if (ticket !== activity.generation || !state.authenticated || document.hidden) return;
      activity.timer = setTimeout(connectActivity, Math.min(30000, 1000 * (2 ** Math.min(5, activity.attempts++))));
    };
    const watch = delay => {
      clearTimeout(activity.watchdog);
      activity.watchdog = setTimeout(() => recover(0), delay);
    };
    watch(20000);
    socket.onmessage = event => {
      if (activity.generation !== generation || !state.authenticated || activity.socket !== socket) return;
      let message; try { message = JSON.parse(event.data); } catch (_) { recover(0); return; }
      if (message.type === 'error') return;
      if (message.version !== 3 || !Number.isSafeInteger(message.sequence) || message.sequence < 0) { recover(0); return; }
      if (message.type === 'heartbeat') {
        if (!activity.ready || message.sequence !== activity.sequence) { recover(0); return; }
        watch(45000); return;
      }
      const previousPending = activity.sessions.get(state.selected?.session_id);
      const validRow = row => row && typeof row.session_id === 'string' && row.session_id.length > 0;
      let inventoryChanged = false;
      if (message.type === 'activity_snapshot') {
        if (!Array.isArray(message.sessions) || !message.sessions.every(validRow) || new Set(message.sessions.map(row => row.session_id)).size !== message.sessions.length) { recover(0); return; }
        if (activity.ready && message.sequence <= activity.sequence) return;
        if (!validTelegramBots(message.telegram_bots, message.telegram_bots_revision)) { recover(0); return; }
        applyTelegramBots(message.telegram_bots, message.telegram_bots_revision, 'activity');
        activity.sessions = new Map(message.sessions.map(row => [row.session_id, row]));
        for (const row of message.sessions) { applyActivitySettings(row); applyActivityError(row); }
        activity.ready = true;
        const known = new Set(state.sessions.map(row => row.session_id));
        inventoryChanged = known.size !== activity.sessions.size || [...activity.sessions.keys()].some(id => !known.has(id));
        if (typeof message.inventory_revision === 'string' && message.inventory_revision !== activity.inventoryRevision) { activity.inventoryRevision = message.inventory_revision; inventoryChanged = true; }
      } else if (message.type === 'activity_event') {
        if (!activity.ready) { recover(0); return; }
        if (message.sequence <= activity.sequence) return;
        if (message.sequence !== activity.sequence + 1 || !['turn_started', 'turn_ended', 'question_requested', 'question_resolved', 'session_changed', 'session_removed', 'session_settings_changed', 'telegram_bots_changed'].includes(message.event)) { recover(0); return; }
        if (message.event === 'telegram_bots_changed') {
          if (!validTelegramBots(message.telegram_bots, message.telegram_bots_revision)) { recover(0); return; }
          applyTelegramBots(message.telegram_bots, message.telegram_bots_revision, 'activity');
        } else if (message.event === 'session_removed') {
          if (typeof message.session_id !== 'string' || message.session !== null) { recover(0); return; }
          activity.sessions.delete(message.session_id);
          sessionSettings.delete(message.session_id); activitySettingsRevisions.delete(message.session_id);
          sessionErrors.delete(message.session_id); sessionErrorDismissals.delete(message.session_id); sessionErrorTurns.delete(message.session_id);
          state.sessions = state.sessions.filter(row => row.session_id !== message.session_id);
          if (state.selected?.session_id === message.session_id) clearGoal();
          inventoryChanged = true;
        } else {
          if (!validRow(message.session) || message.session.session_id !== message.session_id) { recover(0); return; }
          inventoryChanged = !activity.sessions.has(message.session_id) || message.event === 'session_changed';
          activity.sessions.set(message.session_id, message.session);
          applyActivitySettings(message.session);
          applyActivityError(message.session);
        }
      } else { recover(0); return; }
      activity.sequence = message.sequence; activity.revision = message.revision;
      activity.attempts = 0; watch(45000);
      activityStatus('live', '● Live updates');
      updateSessionIndicators();
      renderTelegramBots();
      const nextPending = activity.sessions.get(state.selected?.session_id);
      if (state.connected && !state.loading && (previousPending?.pending_questions !== nextPending?.pending_questions || previousPending?.pending_revision !== nextPending?.pending_revision)) refreshQuestions();
      if (message.event === 'session_removed') renderSessions();
      if (inventoryChanged) refreshSessions();
    };
    socket.onclose = event => recover(event.code);
    socket.onerror = () => {};
  }
  function timestamp(value) {
    if (value === undefined || value === null) return null;
    const date = new Date(typeof value === 'number' && value < 1e12 ? value * 1000 : value);
    return Number.isNaN(date.getTime()) ? null : date;
  }
  function itemMilliseconds(value) {
    return Number.isSafeInteger(value) && value > 0 && value <= 253402300799999 ? value : undefined;
  }
  function legacyItemMilliseconds(value) {
    if (!['number', 'string'].includes(typeof value)) return undefined;
    if (typeof value === 'string') {
      const parts = value.match(/^(\d{4})-(\d{2})-(\d{2})T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})$/i);
      if (!parts) return undefined;
      const [year, month, day] = parts.slice(1).map(Number);
      if (month < 1 || month > 12 || day < 1 || day > new Date(Date.UTC(year, month, 0)).getUTCDate()) return undefined;
    }
    return itemMilliseconds(timestamp(value)?.getTime());
  }
  function itemTiming(item, turn = {}, timing = {}, previous) {
    // Native lifecycle fields are milliseconds; turn dates and older createdAt
    // fields can be seconds. Keep one unit and preserve how each date is known.
    const candidates = [
      [itemMilliseconds(timing.startedAtMs), 'item-started'],
      [legacyItemMilliseconds(item.createdAt), 'item-created'],
      [itemMilliseconds(timing.recordedAtMs), 'item-recorded'],
      [itemMilliseconds(timing.observedStartedAtMs), 'observed-started'],
      [itemMilliseconds(timing.completedAtMs), 'item-completed'],
      [itemMilliseconds(timing.observedCompletedAtMs), 'observed-completed'],
      [legacyItemMilliseconds(turn.startedAt), 'turn-started'],
    ];
    const previousRank = candidates.findIndex(([, source]) => source === previous?._timeSource);
    const previousTime = itemMilliseconds(previous?._time);
    for (let rank = 0; rank < candidates.length; rank++) {
      const [value, source] = candidates[rank];
      if (value === undefined) continue;
      if (previousTime !== undefined && previousRank >= 0 && previousRank <= rank) break;
      return { _time: value, _timeSource: source };
    }
    return previousTime !== undefined && previousRank >= 0 ? { _time: previousTime, _timeSource: previous._timeSource } : { _time: undefined, _timeSource: undefined };
  }
  function itemDate(item) {
    const value = itemMilliseconds(item._time);
    return value === undefined ? null : new Date(value);
  }
  function itemTimeLabel(item) {
    return { 'turn-started': 'Turn started', 'item-recorded': 'Recorded', 'item-completed': 'Completed', 'observed-started': 'Observed start', 'observed-completed': 'Observed completion' }[item._timeSource] || '';
  }
  function isRunning(value) { return ['inProgress', 'in_progress', 'running', 'active'].includes(typeof value === 'object' ? value?.type : value); }
  function nativeStatus(value) { return String(typeof value === 'object' ? value?.type || '' : value || '').replace(/[_\s-]/g, '').toLowerCase(); }
  function terminalStatus(value) { return ['completed', 'done', 'succeeded', 'success', 'failed', 'error', 'errored', 'cancelled', 'canceled', 'interrupted', 'aborted', 'declined', 'denied'].includes(nativeStatus(value)); }
  function toolStatus(item) {
    const status = nativeStatus(item.status), result = item.result;
    const resultStatus = nativeStatus(result?.status);
    const exit = typeof item.exitCode === 'number' && Number.isFinite(item.exitCode) ? item.exitCode : null;
    let outcome;
    if (['cancelled', 'canceled', 'declined', 'denied'].includes(status)) outcome = 'cancelled';
    else if (['interrupted', 'aborted'].includes(status)) outcome = 'interrupted';
    else if (['failed', 'error', 'errored'].includes(status) || ['failed', 'error', 'errored'].includes(resultStatus) || exit !== null && exit !== 0 || item.isError === true || result?.isError === true || item.success === false || result?.success === false || item.error || result?.error) outcome = 'failed';
    else if (exit === 0 || ['success', 'succeeded'].includes(status) || ['success', 'succeeded'].includes(resultStatus) || item.success === true || result?.success === true || result?.isError === false || status === 'completed' && ['mcpToolCall', 'fileChange', 'webSearch'].includes(item.type)) outcome = 'succeeded';
    // Ending a turn settles unfinished tools, but does not prove that they
    // succeeded. Preserve an interruption only when the item has no outcome.
    if (!outcome && !terminalStatus(item.status)) {
      const turnStatus = nativeStatus(item._terminalTurnStatus);
      if (['cancelled', 'canceled'].includes(turnStatus)) outcome = 'cancelled';
      else if (['interrupted', 'aborted'].includes(turnStatus)) outcome = 'interrupted';
    }
    const complete = !!outcome || terminalStatus(item.status) || item._complete === true;
    const state = outcome || (complete ? 'completed' : 'running');
    return { complete, state, label: { succeeded: 'Succeeded', failed: 'Failed', cancelled: 'Cancelled', interrupted: 'Interrupted', completed: 'Completed', running: 'Running' }[state] };
  }
  function atBottom() { const scroller = $('transcript'); return scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 100; }
  function jump() {
    followLatest = true;
    const scroller = $('transcript');
    scroller.scrollTop = scroller.scrollHeight;
    lastHistoryScrollTop = scroller.scrollTop;
    lastHistoryHeight = scroller.scrollHeight; lastHistoryViewport = scroller.clientHeight;
    $('jump-latest').hidden = true;
  }
  function settleBottom() {
    if (!followLatest || bottomFrame !== null) return;
    const generation = state.generation;
    bottomFrame = requestAnimationFrame(() => {
      bottomFrame = null;
      if (generation === state.generation && followLatest && !loadingOlder && !state.loading && !$('transcript').hidden) jump();
    });
  }
  function resizePrompt(keepBottom = followLatest && atBottom()) {
    const prompt = $('prompt');
    if (!$('composer').hidden) {
      prompt.style.height = 'auto';
      // A wrapping placeholder must not make an empty one-line input taller.
      if (prompt.value) prompt.style.height = Math.min(prompt.scrollHeight, 140) + 'px';
      if (keepBottom) jump();
    }
  }
  function updateControls() {
    if (steerNoticeTurn && (!state.connected || state.turn !== steerNoticeTurn)) clearSteerNotice();
    const available = state.connected && !state.loading && !state.submitting && !commandUI?.pending;
    const attachment = attachmentDrafts.get(state.selected?.session_id);
    const gatewayAllowed = !attachment && commandUI?.gatewayAllowed($('prompt').value);
    const fileOnly = attachment?.kind === 'file' && !$('prompt').value.trim();
    const sendAvailable = available || (gatewayAllowed && !state.submitting && !commandUI.pending);
    $('send').disabled = !sendAvailable || (!attachment && !$('prompt').value.trim()) || !!attachment?.loading;
    $('send').textContent = state.submitting ? 'Sending…' : fileOnly ? 'Upload ↑' : state.turn && !gatewayAllowed ? (isClaude() ? 'Queue ↑' : 'Steer ↑') : 'Send ↑';
    $('prompt').placeholder = 'Ask ' + agentLabel() + ' to do anything';
    document.querySelector('label[for="prompt"]').textContent = 'Message ' + agentLabel();
    const claude = isClaude();
    document.querySelector('.model-controls').hidden = false;
    for (const id of ['model', 'effort', 'permissions']) $(id).disabled = !available || (id === 'permissions' && !!state.turn);
    $('session-status').classList.toggle('claude-status', claude);
    $('claude-spend').hidden = !claude;
    renderNotice();
    $('rate-limits').hidden = claude;
    if (claude) renderClaudeStatus();
    $('stop').hidden = !state.turn;
    $('stop').disabled = !available;
    $('attach-image').disabled = !state.selected || state.submitting || !!attachment;
    $('remove-file').disabled = state.submitting;
    $('remove-image').disabled = state.submitting;
    $('activity').hidden = !state.turn;
    $('turn-state').textContent = !state.connected ? 'Disconnected' : state.loading ? 'Loading' : state.turn ? 'Working' : 'Ready';
    $('turn-state').classList.toggle('running', !!state.turn);
    updateSessionIndicators();
    renderTelegramBots();
    commandUI?.update();
  }
  function renderPermissions() {
    if (isClaude()) { renderClaudeStatus(); return; }
    const target = $('permissions'), settings = permissionSettings;
    target.setAttribute('aria-label', 'Session permissions');
    if (!settings) {
      target.textContent = 'Permissions ?';
      target.title = 'Permissions unavailable until confirmed by the connected session. Use /permissions to view choices.';
      target.dataset.mode = 'unknown';
      return;
    }
    const { sandbox, approval, reviewer, profile } = settings;
    const automatic = reviewer === 'auto_review' || reviewer === 'guardian_subagent';
    let label = 'Custom';
    if (profile && !profile.startsWith(':')) label = profile;
    else if (sandbox === 'dangerFullAccess' && approval === 'never') label = 'Full access';
    else if (sandbox === 'readOnly' && approval === 'on-request' && reviewer === 'user') label = 'Read only';
    else if (sandbox === 'workspaceWrite' && approval === 'on-request') label = automatic ? 'Auto review' : reviewer === 'user' ? 'On request' : 'Custom';
    else if (sandbox === 'externalSandbox') label = 'External';
    target.textContent = label;
    target.dataset.mode = sandbox;
    const sandboxNames = { dangerFullAccess: 'Full system access', readOnly: 'Read only', workspaceWrite: 'Workspace write', externalSandbox: 'External sandbox' };
    target.title = ['Permissions: ' + label, 'Sandbox: ' + (sandboxNames[sandbox] || sandbox), 'Approvals: ' + (typeof approval === 'string' ? approval : JSON.stringify(approval)), 'Reviewer: ' + (reviewer || 'unavailable'), ...(profile ? ['Profile: ' + profile] : []), 'Use /permissions to change the setting.'].join('\n');
    settleBottom();
  }
  function clearPermissions() { permissionSettings = null; permissionRevision++; renderPermissions(); }
  function applyPermissions(settings, sandboxKey) {
    if (!settings || ![sandboxKey, 'approvalPolicy', 'approvalsReviewer', 'activePermissionProfile'].some(key => Object.hasOwn(settings, key))) return;
    const sandbox = settings[sandboxKey]?.type, approval = settings.approvalPolicy;
    permissionSettings = typeof sandbox === 'string' && sandbox && (typeof approval === 'string' && approval || approval?.granular) ? {
      sandbox: clean(sandbox), approval,
      reviewer: typeof settings.approvalsReviewer === 'string' ? clean(settings.approvalsReviewer) : '',
      profile: typeof settings.activePermissionProfile?.id === 'string' ? clean(settings.activePermissionProfile.id) : '',
    } : null;
    permissionRevision++;
    renderPermissions();
  }
  function saveDraft() {
    if (state.selected) state.drafts.set(state.selected.session_id, $('prompt').value);
    syncRecoveryDrafts();
    resizePrompt();
    updateControls();
  }
  function renderAttachmentDraft() {
    const attachment = attachmentDrafts.get(state.selected?.session_id);
    const image = attachment?.kind === 'image' ? attachment : null;
    const file = attachment?.kind === 'file' ? attachment : null;
    $('image-preview').hidden = !image;
    if (image) {
      $('image-thumbnail').src = image.url;
      $('image-description').textContent = image.file.name + ' · ' + Math.ceil(image.file.size / 1024).toLocaleString() + ' KB' + (image.loading ? ' · Loading…' : '');
    } else {
      $('image-thumbnail').removeAttribute('src'); $('image-description').textContent = '';
    }
    $('file-preview').hidden = !file;
    $('file-description').textContent = file ? file.name + ' · ' + attachmentSize(file.file.size) + (file.uploadedPath ? ' · Uploaded' : '') : '';
    updateControls(); settleBottom();
  }
  function attachmentSize(bytes) {
    if (bytes < 1024) return bytes.toLocaleString() + (bytes === 1 ? ' byte' : ' bytes');
    if (bytes < 1024 * 1024) return (bytes / 1024).toLocaleString(undefined, { maximumFractionDigits: 1 }) + ' KiB';
    return (bytes / (1024 * 1024)).toLocaleString(undefined, { maximumFractionDigits: 1 }) + ' MiB';
  }
  function removeAttachment(sessionId, expected) {
    const attachment = attachmentDrafts.get(sessionId);
    if (!attachment || (expected && attachment !== expected)) return;
    attachmentDrafts.delete(sessionId);
    attachment.reader?.abort();
    if (attachment.url) URL.revokeObjectURL(attachment.url);
    if (state.selected?.session_id === sessionId) renderAttachmentDraft();
  }
  function attachFile(file, sessionId = state.selected?.session_id) {
    if (!sessionId || !state.authenticated || !file || state.submitting) return;
    if (attachmentDrafts.has(sessionId)) { notice('One attachment per message. Remove the attached file or image before choosing another.'); return; }
    const isImage = ['image/png', 'image/jpeg', 'image/gif'].includes(file.type);
    if (isImage && (!file.size || file.size > maxImageBytes)) { notice('Choose a non-empty image no larger than 10 MiB.'); return; }
    if (!isImage && file.size > maxFileBytes) { notice('Files must be no larger than 20 MiB.'); return; }
    if (attachmentDrafts.size >= maxDraftAttachments || [...attachmentDrafts.values()].reduce((sum, attachment) => sum + attachment.file.size, 0) + file.size > maxDraftAttachmentBytes) { notice('Send or remove an attachment draft from another session before attaching more files.'); return; }
    const attachment = isImage ? { kind: 'image', file, url: URL.createObjectURL(file), loading: true } :
      { kind: 'file', file, name: file.name.split(/[\\/]/).pop() || 'attachment', uploadId: crypto.randomUUID(), uploadedPath: '' };
    attachmentDrafts.set(sessionId, attachment); notice();
    if (isImage) {
      const probe = new Image();
      const timer = setTimeout(() => finish('The image could not be opened. Try another PNG, JPEG or GIF.'), 15000);
      function finish(error = '') {
        clearTimeout(timer); probe.onload = null; probe.onerror = null;
        if (attachmentDrafts.get(sessionId) !== attachment) return;
        if (error) { removeAttachment(sessionId, attachment); if (state.selected?.session_id === sessionId) notice(error); }
        else { attachment.loading = false; if (state.selected?.session_id === sessionId) renderAttachmentDraft(); }
      }
      probe.onload = () => finish(probe.naturalWidth > 8192 || probe.naturalHeight > 8192 || probe.naturalWidth * probe.naturalHeight > 16000000 ? 'Choose an image up to 16 megapixels and 8,192 pixels per side.' : '');
      probe.onerror = () => finish('The image could not be opened. Try another PNG, JPEG or GIF.');
      probe.src = attachment.url;
    }
    if (state.selected?.session_id === sessionId) renderAttachmentDraft();
  }
  function attachmentDataURL(attachment) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader(); attachment.reader = reader;
      const finish = () => { if (attachment.reader === reader) delete attachment.reader; reader.onload = reader.onerror = reader.onabort = null; };
      reader.onload = () => { const result = reader.result; finish(); resolve(result); };
      reader.onerror = reader.onabort = () => { finish(); reject(new Error('The attachment could not be read. Your draft has been kept.')); };
      reader.readAsDataURL(attachment.file);
    });
  }
  function rejectPending(message) {
    for (const request of state.pending.values()) { clearTimeout(request.timer); request.reject(new Error(message)); }
    state.pending.clear();
  }
  function closeSocket() {
    fileUI.clear();
    resumedThread = null;
    clearSteerNotice();
    clearGoal();
    loadingOlder = false; suppressHistoryScroll = false;
    turnMetadataWork = null; historyRead = rpc;
    if (bottomFrame !== null) { cancelAnimationFrame(bottomFrame); bottomFrame = null; }
    gatewayCommandAbort?.abort();
    clearTimeout(state.timer);
    state.timer = null;
    if (state.socket) { state.socket.onclose = null; state.socket.onmessage = null; state.socket.close(1000, 'Viewer detached'); state.socket = null; }
    rejectPending('Connection closed. No messages have been resent.');
    state.connected = false;
    state.loading = false;
    state.submitting = false;
    clearPermissions();
    clearRateLimits();
    clearClaudeStatus();
  }
  function rpc(method, params = {}, timeout = 30000) {
    if (!state.socket || state.socket.readyState !== WebSocket.OPEN) return Promise.reject(new Error('Reconnect before sending. Your draft has been kept.'));
    const id = 'web-' + (++state.sequence);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { state.pending.delete(id); reject(new Error('The request timed out. Check the conversation before retrying; it may have reached ' + agentLabel() + '.')); }, timeout);
      state.pending.set(id, { resolve, reject, timer });
      try { state.socket.send(JSON.stringify({ id, method, params })); }
      catch (error) { clearTimeout(timer); state.pending.delete(id); reject(error); }
    });
  }
  function backgroundReader(generation, socket) {
    let active = 0;
    const queue = [];
    const pump = () => {
      while (active < 3 && queue.length) {
        const request = queue.shift();
        if (generation !== state.generation || state.socket !== socket || !state.connected) { request.reject(new Error('Connection closed.')); continue; }
        active++;
        rpc(request.method, request.params).then(request.resolve, request.reject).finally(() => { active--; pump(); });
      }
    };
    // Leave room in the worker's bounded input lane for an interactive send.
    return (method, params = {}) => new Promise((resolve, reject) => { queue.push({ method, params, resolve, reject }); pump(); });
  }
  async function fetchJSON(path, signal = AbortSignal.timeout(15000)) {
    const authGeneration = state.authGeneration;
    const response = await fetch(path, { credentials: 'same-origin', cache: 'no-store', signal });
    if (response.status === 401) { if (authGeneration === state.authGeneration) expire(); throw new Error('Sign in to continue.'); }
    if (!response.ok) throw new Error('Gateway request failed (' + response.status + '). Try again shortly.');
    return response.json();
  }
  function captureAuthPosition() {
    if (!state.selected) return;
    const scroller = $('transcript');
    const anchor = [...$('messages').children].find(item => item.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
    authRestore = { id: state.selected.session_id, top: scroller.scrollTop, bottom: followLatest, anchor: anchor?.dataset.itemId, offset: anchor ? anchor.getBoundingClientRect().top - scroller.getBoundingClientRect().top : 0, count: state.items.size };
    uncertainSend ||= state.submitting || uncertainDrafts.has(state.selected.session_id);
  }
  function expire() { if (sessionAuth) sessionAuth.expire('expired'); else lockAuthentication('expired'); }
  function lockAuthentication(reason) {
    captureAuthPosition();
    if (['logout', 'revoked'].includes(reason)) {
      authRestore = null; uncertainSend = false; uncertainDrafts.clear();
      notificationSessionID = ''; notificationLocation();
    }
    window.CodexDiagrams?.clear();
    clearTimeout(draftSaveTimer); draftSaveTimer = null; draftDirty = false;
    if (['logout', 'revoked'].includes(reason)) draftRecovery?.clear();
    draftRecovery?.expireAuth(); recoveredDrafts = null; $('draft-recovery-offer').hidden = true;
    window.dispatchEvent(new CustomEvent('codex-auth-expiring'));
    commandUI?.sessionChanged();
    state.generation++;
    state.authGeneration++;
    resetTelegramBots();
    resetInventory();
    pauseSessionOperations(['logout', 'revoked'].includes(reason)); editSessionTarget = null;
    if ($('edit-session-dialog').open) $('edit-session-dialog').close();
    for (const deletion of sessionDeletions.values()) { clearTimeout(deletion.timer); deletion.controller?.abort(); }
    sessionDeletions.clear(); deleteSessionTarget = null;
    if ($('delete-session-dialog').open) $('delete-session-dialog').close();
    for (const creation of sessionCreations.values()) { clearTimeout(creation.timer); creation.controller?.abort(); }
    sessionCreations.clear(); newSessionWorkerID = ''; cancelWorkspaceBrowse();
    if ($('new-session-dialog').open) $('new-session-dialog').close();
    state.stopped = true;
    closeSocket();
    state.authenticated = false;
    renderLogout();
    if ($('logout-dialog').open) $('logout-dialog').close();
    if ($('settings-dialog').open) $('settings-dialog').close();
    notificationUI?.setAuthenticated(false);
    closeActivity(); activity.sessions.clear(); activity.inventoryRevision = ''; activity.attempts = 0; activityStatus('disconnected', 'Sign in for live updates');
    sessionSettings.clear(); activitySettingsRevisions.clear();
    clearCapacityRetryView();
    sessionErrors.clear(); sessionErrorDismissals.clear(); sessionErrorTurns.clear();
    state.selected = null;
    state.sessions = [];
    state.workers = []; state.runtimes = [];
    state.drafts.clear();
    for (const id of attachmentDrafts.keys()) removeAttachment(id);
    renderAttachmentDraft(); attachmentPickerSession = null;
    state.positions.clear();
    state.items.clear();
    state.questions.clear();
    state.models = [];
    state.turn = null;
    state.cursor = null;
    state.queuedEvents = [];
    $('prompt').value = '';
    $('session-search').value = '';
    $('workspace-path').textContent = ''; $('workspace-path').removeAttribute('title');
    $('usage').textContent = '';
    $('model').textContent = 'Session model'; $('effort').textContent = 'Session effort';
    for (const id of ['delete-session-name', 'delete-session-cwd', 'delete-session-message']) $(id).textContent = '';
    $('image-file').value = '';
    $('messages').replaceChildren();
    $('questions').replaceChildren();
    $('questions').hidden = true;
    $('session-list').replaceChildren();
    $('session-title').textContent = 'MACT';
    $('session-location').textContent = 'Sign in to continue';
    $('sessions-status').textContent = 'Sign in to see your sessions.';
    for (const id of ['transcript', 'composer', 'empty', 'disconnect', 'jump-latest']) $(id).hidden = true;
    $('auth').hidden = false;
    $('auth-title').textContent = authRestore ? 'Unlock your sessions' : 'Sign in to MACT';
    updateAuthDescription();
    if (reason === 'logout') $('auth-status').textContent = 'Signed out.';
    showSessions(false);
    notice();
    connection('disconnected', 'Sign in required');
    updateControls();
  }
  function updateAuthDescription() {
    const password = sessionAuth?.hasPassword();
    $('auth-description').textContent = authRestore
      ? password ? 'Your running work continues. Sign in to return to the same conversation.' : 'Your running work continues. Use your passkey to return to the same conversation.'
      : password ? 'Use your MACT Gateway passkey or password to open your sessions.' : 'Use your MACT Gateway passkey to open your sessions.';
  }
  function sessionMatchesSearch(session, query) {
    return !query || [title(session), session.cwd, session.worker_name, session.runtime_name].some(value => String(value || '').toLocaleLowerCase().includes(query));
  }
  function revealWorkerSessions(session) {
    const query = $('session-search').value.toLocaleLowerCase().trim();
    const clearSearch = query && !sessionMatchesSearch(session, query);
    if (clearSearch) $('session-search').value = '';
    const changed = clearSearch || workerGroupSearchCollapsed.get(session.worker_id) === true || (workerGroupCollapsed.get(session.worker_id) === true && !workerGroupReveals.has(session.worker_id));
    workerGroupReveals.add(session.worker_id);
    workerGroupSearchCollapsed.delete(session.worker_id);
    return changed;
  }
  function renderSessions() {
    const query = $('session-search').value.toLocaleLowerCase().trim();
    if (query !== workerGroupSearchQuery) { workerGroupSearchQuery = query; workerGroupSearchCollapsed.clear(); }
    const list = $('session-list');
    const focusedWorker = list.contains(document.activeElement) ? document.activeElement.dataset.toggleWorkerId : '';
    list.replaceChildren();
    const sessions = state.sessions.filter(session => !session.archived && !session.deleted && sessionMatchesSearch(session, query));
    // Workers are independent of session inventory: a fresh worker remains
    // visible and can create its first conversation.
    const workers = new Map(state.workers.map(worker => [worker.ID, worker]));
    for (const session of state.sessions) if (!workers.has(session.worker_id)) workers.set(session.worker_id, { ID: session.worker_id, Name: session.worker_name || session.worker_id, Connectivity: session.worker_connectivity || 'unknown' });
    for (const worker of workers.values()) {
      const values = sessions.filter(session => session.worker_id === worker.ID);
      const group = node('section', 'session-worker'); group.dataset.workerId = worker.ID;
      const heading = node('h2', 'session-group');
      heading.dataset.workerId = worker.ID;
      const body = node('div', 'session-group-sessions'); body.id = 'worker-sessions-' + encodeURIComponent(worker.ID);
      const toggle = node('button', 'session-group-toggle'); toggle.type = 'button'; toggle.dataset.toggleWorkerId = worker.ID;
      toggle.setAttribute('aria-controls', body.id);
      const arrow = node('span', 'session-group-arrow', '›'); arrow.setAttribute('aria-hidden', 'true');
      const label = node('span', 'session-group-label');
      label.append(node('span', 'worker-name', worker.Name || worker.ID));
      const runtimes = state.runtimes.filter(runtime => runtime.worker_id === worker.ID);
      if (runtimes.length === 1 || (!runtimes.length && values[0]?.runtime_name)) label.append(node('span', 'runtime-name', runtimes[0]?.name || values[0]?.runtime_name || (worker.AgentKind === 'claude' ? 'Claude Code' : 'Codex') + ' runtime'));
      else if (runtimes.length > 1) label.append(node('span', 'runtime-name', (worker.AgentKind === 'claude' ? 'Claude Code' : 'Codex') + ' · ' + runtimes.length + ' runtimes'));
      const unavailable = workerCreationUnavailable(worker);
      if (unavailable) label.append(node('span', 'session-group-status', unavailable));
      const count = node('span', 'session-group-count', String(values.length)); count.setAttribute('aria-hidden', 'true');
      const activeCount = node('span', 'session-group-active-count'); activeCount.hidden = true; activeCount.setAttribute('aria-hidden', 'true');
      toggle.append(arrow, label, activeCount, count);
      const searching = !!query && values.length > 0;
      let expanded;
      const updateGroup = () => {
        expanded = query && workerGroupSearchCollapsed.has(worker.ID) ? workerGroupSearchCollapsed.get(worker.ID) !== true : searching || workerGroupCollapsed.get(worker.ID) !== true || workerGroupReveals.has(worker.ID);
        body.hidden = !expanded;
        heading.dataset.collapsed = String(!expanded);
        toggle.setAttribute('aria-expanded', String(expanded));
        const action = expanded ? 'Collapse' : 'Expand';
        const description = values.length + (searching ? ' matching' : '') + (values.length === 1 ? ' session' : ' sessions');
        toggle.setAttribute('aria-label', action + ' sessions for ' + (worker.Name || worker.ID) + ' · ' + description);
        toggle.title = action + ' sessions' + (searching ? ' in these search results' : '');
        count.title = description;
      };
      updateGroup();
      toggle.addEventListener('click', () => {
        if (query) workerGroupSearchCollapsed.set(worker.ID, expanded);
        else {
          workerGroupCollapsed.set(worker.ID, expanded); workerGroupReveals.delete(worker.ID);
          try { localStorage.setItem(workerGroupPreferenceKey, JSON.stringify(Object.fromEntries(workerGroupCollapsed))); } catch (_) { /* Optional preference only. */ }
        }
        updateGroup();
        updateSessionIndicators();
      });
      const create = node('button', 'session-create'); create.type = 'button'; create.dataset.createWorkerId = worker.ID;
      create.setAttribute('aria-label', 'New session on ' + (worker.Name || worker.ID)); create.setAttribute('aria-haspopup', 'dialog'); create.setAttribute('aria-controls', 'new-session-dialog');
      create.title = unavailable || 'New session'; create.disabled = !!unavailable && !!sessionFeatureUnavailable(worker, 'SupportsSessionArchives', 'restore archived projects') && !sessionCreations.has(worker.ID) && !operationLocked(sessionOperationFor('restore', worker.ID));
      const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
      icon.setAttribute('viewBox', '0 0 24 24'); icon.setAttribute('fill', 'none'); icon.setAttribute('stroke', 'currentColor'); icon.setAttribute('stroke-width', '1.6'); icon.setAttribute('aria-hidden', 'true');
      const path = document.createElementNS('http://www.w3.org/2000/svg', 'path'); path.setAttribute('d', 'M12 4v16M4 12h16'); icon.append(path); create.append(icon);
      create.addEventListener('click', () => openSessionCreation(worker.ID));
      heading.append(toggle, create);
      group.append(heading, body); list.append(group);
      if (!values.length) body.append(node('p', 'session-group-empty', query ? 'No matching sessions.' : 'No sessions yet.'));
      let previousRuntime = '';
      values.sort((a, b) => String(a.runtime_id || a.runtime_name || '').localeCompare(String(b.runtime_id || b.runtime_name || '')));
      for (const session of values) {
        const runtimeKey = session.runtime_id || session.runtime_name || '';
        if (runtimes.length > 1 && runtimeKey !== previousRuntime) { body.append(node('h3', 'session-runtime-heading', session.runtime_name || runtimes.find(runtime => runtime.runtime_id === session.runtime_id)?.name || agentLabel(session) + ' runtime')); previousRuntime = runtimeKey; }
        const row = node('div', 'session-row');
        const button = node('button', 'session-button'); button.type = 'button'; button.dataset.sessionId = session.session_id;
        button.setAttribute('aria-current', String(state.selected?.session_id === session.session_id));
        button.append(node('span', 'name', title(session)));
        const running = isRunning(session.state);
        const offline = session.worker_connectivity && !['connected', 'online'].includes(session.worker_connectivity);
        const status = offline ? '○ Worker ' + session.worker_connectivity : running ? '● Working' : '○ ' + (session.state || 'Saved');
        button.append(node('span', 'detail' + (running && !offline ? ' running' : ''), status + ' · ' + (session.cwd || 'Workspace unavailable')));
        button.addEventListener('click', () => selectSession(session));
        const remove = node('button', 'session-edit'); remove.type = 'button'; remove.dataset.editSessionId = session.session_id;
        remove.setAttribute('aria-label', 'Edit session ' + title(session)); remove.title = 'Edit session';
        remove.setAttribute('aria-haspopup', 'dialog'); remove.setAttribute('aria-controls', 'edit-session-dialog');
        remove.dataset.pending = String(!!sessionDeletions.get(session.session_id)?.pending || !!sessionOperationFor('edit', session.session_id)?.pending || !!sessionOperationFor('archive', session.session_id)?.pending);
        const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
        icon.setAttribute('viewBox', '0 0 24 24'); icon.setAttribute('fill', 'none'); icon.setAttribute('stroke', 'currentColor'); icon.setAttribute('stroke-width', '1.6'); icon.setAttribute('aria-hidden', 'true');
        const path = document.createElementNS('http://www.w3.org/2000/svg', 'path'); path.setAttribute('d', 'm15 5 4 4M4 20l4-1L20 7a2.8 2.8 0 0 0-4-4L4 15l-1 6z'); icon.append(path); remove.append(icon);
        remove.addEventListener('click', () => openSessionEditor(session));
        row.append(button, remove); body.append(row);
      }
    }
    if (focusedWorker) [...list.querySelectorAll('[data-toggle-worker-id]')].find(button => button.dataset.toggleWorkerId === focusedWorker)?.focus({ preventScroll: true });
    $('sessions-status').textContent = sessions.length ? sessions.length + (sessions.length === 1 ? ' session' : ' sessions') : query ? 'No matching sessions.' : workers.size ? 'Use + beside a worker to create a session.' : 'No workers enrolled.';
    updateSessionIndicators();
    renderTelegramBots();
    if ($('new-session-dialog').open) renderSessionCreation();
    if ($('edit-session-dialog').open) renderSessionEditor();
    if ($('delete-session-dialog').open) renderSessionDeletion();
  }
  function workerCreationUnavailable(worker) {
    if (!worker || !state.authenticated) return 'Sign in to create a session.';
    if (authRotationPending) return 'Sign-in renewal in progress';
    if (worker.Enabled === false) return 'Worker disabled';
    if (!['connected', 'online'].includes(worker.Connectivity)) return 'Worker ' + (worker.Connectivity || 'unavailable');
    if (!worker.SupportsWebUISessionCreation) return 'Update this worker to create sessions.';
    if (!state.runtimes.some(runtime => runtime.worker_id === worker.ID && runtime.state === 'running')) return 'No running ' + (worker.AgentKind === 'claude' ? 'Claude Code' : 'Codex') + ' runtime';
    return '';
  }
  function creationRuntime() {
    return state.runtimes.find(runtime => runtime.worker_id === newSessionWorkerID && runtime.runtime_id === $('new-session-runtime').value);
  }
  function sessionNameError(value) {
    const name = value.trim();
    if (!name || name === '.' || name === '..' || new TextEncoder().encode(name).length > 120) return 'Enter a session name of 1–120 UTF-8 bytes.';
    if (/[\/\\\p{Cc}]/u.test(name)) return 'Session name cannot contain slashes or control characters.';
    return '';
  }
  function newSessionProjectRoot() {
    return isClaude({ worker_id: newSessionWorkerID, runtime_id: $('new-session-runtime').value }) ? '~/CLAUDE_CODE/' : '~/CODEX/';
  }
  function updateNewSessionPath() {
    if (!newSessionPathEdited) $('new-session-cwd').value = newSessionProjectRoot() + $('new-session-name').value.trim().replace(/\p{White_Space}/gu, '_');
    $('new-session-preview').textContent = $('new-session-cwd').value.trim() ? 'Session folder: ' + $('new-session-cwd').value.trim() : '';
    $('new-session-name').setCustomValidity(sessionNameError($('new-session-name').value));
  }
  function creationMessage(message = '', error = false) {
    $('new-session-message').textContent = message; $('new-session-message').hidden = !message; $('new-session-message').dataset.error = String(error);
  }
  function openSessionCreation(workerID) {
    const worker = state.workers.find(value => value.ID === workerID);
    if (workerCreationUnavailable(worker) && sessionFeatureUnavailable(worker, 'SupportsSessionArchives', 'restore archived projects') && !sessionCreations.has(workerID) && !operationLocked(sessionOperationFor('restore', workerID))) return;
    blurEditable(); commandUI?.sessionChanged();
    newSessionWorkerID = workerID; cancelWorkspaceBrowse(); workspacePage = null;
    const runtimes = state.runtimes.filter(runtime => runtime.worker_id === workerID);
    $('new-session-runtime').replaceChildren(...runtimes.map(runtime => {
      const option = node('option', '', runtime.name || runtime.runtime_id); option.value = runtime.runtime_id; option.disabled = runtime.state !== 'running'; return option;
    }));
    const creation = sessionCreations.get(workerID);
    const restoration = sessionOperationFor('restore', workerID);
    $('new-session-runtime').value = creation?.runtimeID || (operationLocked(restoration) ? restoration.runtimeID : '') || runtimes.find(runtime => runtime.state === 'running')?.runtime_id || '';
    $('new-session-name').value = creation?.name || ''; $('new-session-cwd').value = creation?.cwd || newSessionProjectRoot();
    newSessionPathEdited = !!creation;
    $('new-session-runtime-field').hidden = runtimes.length < 2;
    document.querySelector('label[for="new-session-runtime"]').textContent = (worker?.AgentKind === 'claude' ? 'Claude Code' : 'Codex') + ' runtime';
    $('new-session-browser').hidden = true; $('new-session-directories').replaceChildren();
    newSessionMode = operationLocked(restoration) || (workerCreationUnavailable(worker) && !sessionFeatureUnavailable(worker, 'SupportsSessionArchives', 'restore archived projects')) ? 'restore' : 'create'; selectedArchiveID = restoration?.archiveID || '';
    creationMessage(); updateNewSessionPath(); renderSessionCreation();
    $('new-session-dialog').showModal();
    // Keep phone keyboards closed until the user selects a text field.
    $('new-session-close').focus({ preventScroll: true });
    if (newSessionMode === 'restore') loadSessionArchives();
  }
  function renderSessionCreation() {
    if (!newSessionWorkerID) return;
    const worker = state.workers.find(value => value.ID === newSessionWorkerID), runtime = creationRuntime();
    const creation = sessionCreations.get(newSessionWorkerID), restoration = sessionOperationFor('restore', newSessionWorkerID);
    const restoring = newSessionMode === 'restore';
    $('new-session-create-fields').hidden = restoring; $('new-session-project-fields').hidden = restoring; $('new-session-archives').hidden = !restoring;
    $('new-session-name').required = !restoring; $('new-session-cwd').required = !restoring;
    $('new-session-mode-create').setAttribute('aria-pressed', String(!restoring)); $('new-session-mode-restore').setAttribute('aria-pressed', String(restoring));
    $('new-session-mode-create').disabled = operationLocked(restoration); $('new-session-mode-restore').disabled = !!creation && creation.phase !== 'failed';
    if (restoring) { renderSessionRestore(worker, runtime, restoration); return; }
    $('new-session-name').setCustomValidity(sessionNameError($('new-session-name').value));
    const unavailable = workerCreationUnavailable(worker) || (!runtime || runtime.state !== 'running' ? 'This runtime is unavailable. Refresh the workers and try again.' : '');
    const locked = creation && creation.phase !== 'failed';
    $('new-session-worker').textContent = (worker?.Name || newSessionWorkerID) + (runtime ? ' · ' + (runtime.name || runtime.runtime_id) : '');
    for (const id of ['new-session-name', 'new-session-cwd', 'new-session-runtime']) $(id).disabled = !!locked;
    $('new-session-browse').disabled = !!locked || !!workspaceBrowse || !!unavailable;
    for (const id of ['new-session-parent', 'new-session-default', 'new-session-more', 'new-session-use-folder']) $(id).disabled = !!locked || !!workspaceBrowse || !!unavailable;
    for (const button of $('new-session-directories').querySelectorAll('button')) button.disabled = !!locked || !!workspaceBrowse || !!unavailable;
    const checking = creation?.phase === 'submitting' || !!creation?.checking;
    $('new-session-submit').disabled = !!checking || !!authRotationPending || (!!unavailable && !locked) || !!workspaceBrowse;
    $('new-session-submit').textContent = creation?.phase === 'submitting' ? 'Creating…' : creation?.checking ? 'Checking…' : locked ? 'Check status' : 'Create session';
    $('new-session-cancel').textContent = locked || creation?.phase === 'complete' ? 'Close' : 'Cancel';
    if (creation) creationMessage(creation.message, ['failed', 'unknown'].includes(creation.phase));
    else if (unavailable) creationMessage(unavailable, true);
  }
  function cancelWorkspaceBrowse() {
    const previous = workspaceBrowse; workspaceBrowse = null;
    previous?.controller?.abort(); clearTimeout(previous?.timer);
  }
  async function workerSessionRequest(operation, endpoint, method, extra = {}) {
    const authGeneration = state.authGeneration;
    const controller = new AbortController(); operation.controller = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);
    const parameters = { worker_id: operation.workerID, runtime_id: operation.runtimeID, runtime_generation: operation.runtimeGeneration, request_id: operation.requestID, ...extra };
    const csrf = document.cookie.split('; ').find(value => value.startsWith('__Host-telegramgw-csrf='))?.split('=').slice(1).join('=') || '';
    try {
      const response = await fetch(api + endpoint + (method === 'GET' ? '?' + new URLSearchParams(parameters) : ''), { method, credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
        ...(method === 'POST' ? { headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify(parameters) } : {}) });
      if (authGeneration !== state.authGeneration || operation.authGeneration !== state.authGeneration || !state.authenticated) throw new Error('Sign in to continue.');
      if (response.status === 401) { expire(); throw new Error('Sign in to continue.'); }
      let result; try { result = await response.json(); } catch (_) { /* Gateway errors can be plain text. */ }
      if (!response.ok) {
        const fallback = 'Worker request failed (' + response.status + '). Refresh workers and try again.';
        const error = new Error(operation.kind ? sessionOperationMessage(result?.error_code ? result : null, fallback, operation.kind) : result?.message || fallback); error.status = response.status; error.result = result; throw error;
      }
      if (!result || result.command_id !== operation.requestID || result.worker_id !== operation.workerID || result.runtime_id !== operation.runtimeID || result.runtime_generation !== operation.runtimeGeneration || typeof result.pending !== 'boolean') throw new Error('The worker returned an unexpected result.');
      return result;
    } finally { clearTimeout(timeout); if (operation.controller === controller) operation.controller = null; }
  }
  function workerOperation(runtime) {
    return { workerID: newSessionWorkerID, runtimeID: runtime.runtime_id, runtimeGeneration: runtime.generation, requestID: crypto.randomUUID(), authGeneration: state.authGeneration };
  }
  async function browseSessionWorkspace(path = '', offset = 0) {
    const runtime = creationRuntime();
    if (!runtime || runtime.state !== 'running' || workerCreationUnavailable(state.workers.find(worker => worker.ID === newSessionWorkerID)) || workspaceBrowse) return;
    const operation = { ...workerOperation(runtime), path, offset, deadline: Date.now() + 30000 };
    workspaceBrowse = operation; creationMessage('Loading folders…'); renderSessionCreation();
    const current = () => workspaceBrowse === operation && operation.authGeneration === state.authGeneration && $('new-session-dialog').open;
    async function read(method) {
      try {
        const result = await workerSessionRequest(operation, '/workspaces', method, method === 'POST' ? { path, offset } : {});
        if (!current()) return;
        if (result.pending) {
          if (Date.now() >= operation.deadline) throw new Error('Folder listing is taking longer than expected. Use Browse to try again.');
          operation.timer = setTimeout(() => read('GET'), 1000); return;
        }
        if (!result.workspace || typeof result.workspace.path !== 'string' || !Array.isArray(result.workspace.directories)) throw new Error(result.message || 'These folders are unavailable. Edit the path or choose another folder.');
        workspacePage = result.workspace; workspaceBrowse = null;
        $('new-session-browser').hidden = false; $('new-session-browser-path').textContent = workspacePage.path;
        $('new-session-parent').hidden = !workspacePage.parent; $('new-session-more').hidden = !workspacePage.has_more;
        $('new-session-folders-empty').hidden = workspacePage.directories.length > 0;
        $('new-session-directories').replaceChildren(...workspacePage.directories.filter(entry => typeof entry.name === 'string' && typeof entry.path === 'string').map(entry => {
          const button = node('button', 'new-session-directory', '📁 ' + entry.name); button.type = 'button'; button.setAttribute('aria-label', 'Open folder ' + entry.name); button.addEventListener('click', () => browseSessionWorkspace(entry.path)); return button;
        }));
        creationMessage(); renderSessionCreation();
      } catch (error) {
        if (!current()) return;
        workspaceBrowse = null; creationMessage(error.message, true); renderSessionCreation();
      }
    }
    await read('POST');
  }
  function creationIsCurrent(creation) {
    return state.authenticated && creation.authGeneration === state.authGeneration && sessionCreations.get(creation.workerID) === creation;
  }
  function updateCreationView(creation) {
    if (newSessionWorkerID === creation.workerID && $('new-session-dialog').open) renderSessionCreation();
  }
  function pauseSessionCreationRequests() {
    cancelWorkspaceBrowse();
    for (const creation of sessionCreations.values()) {
      clearTimeout(creation.timer); creation.requestEpoch = (creation.requestEpoch || 0) + 1; creation.controller?.abort(); creation.checking = false;
      if (creation.phase !== 'failed') { creation.phase = 'unknown'; creation.message = 'Sign-in changed while checking creation. Use Check status after sign-in to confirm the same request; it will not be resent.'; }
    }
    if ($('new-session-dialog').open) renderSessionCreation();
  }
  function resumeSessionCreationRequests() {
    for (const creation of sessionCreations.values()) creation.authGeneration = state.authGeneration;
    if ($('new-session-dialog').open) renderSessionCreation();
  }
  async function finishSessionCreation(creation, result) {
    creation.phase = 'complete'; creation.message = result.message || 'Session created. Refresh workers to open it.'; clearTimeout(creation.timer); updateCreationView(creation);
    // The durable inventory notification can arrive just after the command
    // result. Keep the current conversation attached until the new ID exists.
    for (let attempt = 0; attempt < 20 && creationIsCurrent(creation); attempt++) {
      await refreshSessions();
      if (!creationIsCurrent(creation)) return;
      const created = state.sessions.find(session => session.session_id === result.session.session_id && session.worker_id === creation.workerID && session.runtime_id === creation.runtimeID);
      if (created) {
        sessionCreations.delete(creation.workerID);
        if (newSessionWorkerID === creation.workerID && $('new-session-dialog').open && (state.selected?.session_id || '') === creation.selectedID) { $('new-session-dialog').close(); selectSession(created); }
        else notice('Created “' + title(created) + '” on ' + (state.workers.find(worker => worker.ID === creation.workerID)?.Name || creation.workerID) + '. Select it in the sidebar.');
        return;
      }
      await new Promise(resolve => setTimeout(resolve, 250));
    }
    updateCreationView(creation);
  }
  async function requestSessionCreation(creation, method) {
    if (!creationIsCurrent(creation) || creation.checking) return;
    const epoch = creation.requestEpoch = (creation.requestEpoch || 0) + 1;
    const current = () => creationIsCurrent(creation) && creation.requestEpoch === epoch;
    creation.checking = true; updateCreationView(creation);
    try {
      const result = await workerSessionRequest(creation, '/sessions/new', method, method === 'POST' ? { name: creation.name, cwd: creation.cwd } : {});
      if (!current()) return;
      creation.message = result.message || (result.pending ? 'Creating session on the worker…' : 'The worker did not confirm creation.');
      if (!result.pending && result.session?.session_id) {
        if (result.session.worker_id !== creation.workerID || result.session.runtime_id !== creation.runtimeID) throw new Error('The worker returned a session on a different runtime.');
        await finishSessionCreation(creation, result); return;
      }
      creation.phase = result.pending ? 'pending' : result.status === 'outcome_unknown' || result.error_code === 'outcome_unknown' ? 'unknown' : result.error_code || ['failed', 'rejected'].includes(result.status) ? 'failed' : 'unknown';
      if (creation.phase === 'unknown') creation.message += ' Check the session list and use Check status before trying again. This request will not be resent.';
      if (creation.phase === 'pending' && Date.now() < creation.deadline) creation.timer = setTimeout(() => requestSessionCreation(creation, 'GET'), 1000);
    } catch (error) {
      if (!current()) return;
      // Only an explicit HTTP rejection before dispatch permits another POST.
      // A lost reply or timeout is resolved through this request's GET status.
      creation.phase = method === 'POST' && error.status >= 400 && error.status < 500 ? 'failed' : 'unknown';
      creation.message = creation.phase === 'failed' ? error.message : error.message + ' Creation may have been accepted. Check the session list and use Check status; this request will not be resent.';
    } finally { if (current()) { creation.checking = false; updateCreationView(creation); } }
  }
  function submitSessionCreation(event) {
    event.preventDefault();
    if (!state.authenticated || !newSessionWorkerID) return;
    if (newSessionMode === 'restore') { submitSessionRestore(); return; }
    const previous = sessionCreations.get(newSessionWorkerID);
    if (previous && previous.phase !== 'failed') { requestSessionCreation(previous, 'GET'); return; }
    const runtime = creationRuntime(), worker = state.workers.find(value => value.ID === newSessionWorkerID);
    if (workerCreationUnavailable(worker) || !runtime || runtime.state !== 'running' || workspaceBrowse) { renderSessionCreation(); return; }
    const name = $('new-session-name').value.trim(), cwd = $('new-session-cwd').value.trim();
    const invalid = sessionNameError(name) || (cwd !== '~' && !cwd.startsWith('/') && !cwd.startsWith('~/') ? 'Enter an absolute path or a path starting with ~/ on this worker.' : '');
    if (invalid) { creationMessage(invalid, true); return; }
    const creation = { ...workerOperation(runtime), name, cwd, selectedID: state.selected?.session_id || '', phase: 'submitting', message: 'Creating session on the worker…', deadline: Date.now() + 30000 };
    sessionCreations.set(newSessionWorkerID, creation); requestSessionCreation(creation, 'POST');
  }
  function sessionOperationKey(kind, id, runtimeID = '') { return kind + ':' + id + (kind === 'list' ? ':' + runtimeID : ''); }
  function sessionOperationFor(kind, id, runtimeID = '') { return sessionOperations.get(sessionOperationKey(kind, id, runtimeID)); }
  function operationLocked(operation) { return !!operation && !['failed', 'complete'].includes(operation.phase); }
  function sessionFeatureUnavailable(worker, capability, action) {
    if (!state.authenticated) return 'Sign in to ' + action + '.';
    if (authRotationPending) return 'Sign-in renewal in progress.';
    if (!worker || worker.Enabled === false) return 'This worker is unavailable.';
    if (!['connected', 'online'].includes(worker.Connectivity)) return 'Reconnect this worker to ' + action + '.';
    if (!worker[capability]) return worker.AgentKind === 'claude' ? 'Claude Code workers do not support this action.' : 'Update this worker to ' + action + '.';
    return '';
  }
  function sessionOperationMessage(result, fallback, kind = '') {
    const phases = { preparing: 'Preparing the operation on the worker…', exporting: 'Exporting the conversation…', compressing: 'Compressing the project and conversation…', recovering: 'Recovering the operation on the worker…', verifying: 'Verifying the archive…', archiving: 'Archiving the native conversation…', cleaning: 'Removing the original project directory…', extracting: 'Extracting the project files…', restoring: 'Restoring the conversation…' };
    if (result?.pending && phases[result.phase]) return phases[result.phase];
    const aliases = { path_exists: 'archive_conflict', archive_unsupported: 'archive_incompatible', codex_method_unsupported: 'archive_incompatible', worker_unavailable: 'worker_offline', archive_busy: 'session_busy', unsupported_operation: 'unsupported', stale_runtime: 'runtime_unavailable', codex_unavailable: 'runtime_unavailable', command_outcome_unknown: 'outcome_unknown' };
    const messages = {
      shared_directory: 'Other sessions share this project directory. Keep the original directory, or move those sessions before removing it.',
      shared_workspace: 'Other sessions share this project directory. Keep the original directory, or move those sessions before removing it.',
      archive_conflict: 'The original project path already exists. Move that directory on the worker, then explicitly retry the restore. Nothing was overwritten; the archive is kept.',
      path_conflict: 'The original project path already exists. Move that directory on the worker, then explicitly retry the restore. Nothing was overwritten; the archive is kept.',
      archive_incompatible: kind === 'restore' ? 'Restoring this archive requires its original Codex version and a supported session layout. The project and archive have been kept.' : 'The Codex runtime version or session layout is not supported for archiving or restoring. The project and any existing archive have been kept.',
      archive_not_found: 'This archive is no longer available. Refresh the archive list and choose another archive.',
      archive_invalid: 'This archive failed verification. The original project and archive have been kept. Choose another archive or resolve the worker issue before retrying.',
      archive_corrupt: 'This archive failed verification. The original project and archive have been kept. Choose another archive or resolve the worker issue before retrying.',
      session_name_invalid: 'Enter a session name of 1–120 characters without control characters.',
      session_busy: 'The project or Codex runtime has pending activity, or its idle state could not be verified. Check running work, questions and background terminals, then refresh sessions and try again.',
      runtime_unavailable: 'This Codex runtime is unavailable. Refresh the workers and choose a running runtime.',
      invalid_workspace: 'The project directory changed, is protected, or is unavailable. Refresh sessions and reopen this editor to review its current path. Keep the project directory when archiving a protected startup folder or workspace root.',
      busy: 'The project or Codex runtime has pending activity, or its idle state could not be verified. Check running work, questions and background terminals, then refresh sessions and try again.',
      worker_offline: 'Reconnect this worker, then check this request’s status.',
      unsupported: 'Update this worker to use this feature.',
      outcome_unknown: 'The worker has not confirmed the outcome. Check status before starting another request.'
    };
    if (result?.error_code) return messages[aliases[result.error_code] || result.error_code] || 'The worker could not complete this request. Resolve the worker issue, then explicitly retry.';
    return typeof result?.message === 'string' && result.message.trim() ? result.message : fallback;
  }
  function persistSessionOperations() {
    // Persist identities only. Recovery reads the durable gateway result and
    // never reconstructs or repeats a mutation from browser storage.
    const operations = [...sessionOperations.values()].filter(operationLocked).map(operation => ({
      kind: operation.kind, key: operation.key, requestID: operation.requestID, workerID: operation.workerID,
      runtimeID: operation.runtimeID, runtimeGeneration: operation.runtimeGeneration, sessionID: operation.sessionID || '', archiveID: operation.archiveID || ''
    }));
    try {
      if (operations.length) sessionStorage.setItem('codex-webui-session-operations', JSON.stringify({ owner: sessionOperationOwner, operations }));
      else sessionStorage.removeItem('codex-webui-session-operations');
    } catch (_) { /* In-memory status recovery remains available. */ }
  }
  function recoverSessionOperations(owner) {
    if (sessionOperationOwner && sessionOperationOwner !== (owner || '')) pauseSessionOperations(true);
    sessionOperationOwner = owner || '';
    try {
      const saved = JSON.parse(sessionStorage.getItem('codex-webui-session-operations') || 'null');
      if (!saved || saved.owner !== sessionOperationOwner || !Array.isArray(saved.operations)) return;
      for (const entry of saved.operations.slice(0, 50)) {
        if (!entry || !['edit', 'archive', 'restore', 'list'].includes(entry.kind) || !sessionUUID.test(entry.requestID) ||
          typeof entry.workerID !== 'string' || typeof entry.runtimeID !== 'string' || !Number.isSafeInteger(entry.runtimeGeneration) ||
          typeof entry.sessionID !== 'string' || typeof entry.archiveID !== 'string') continue;
        const id = ['edit', 'archive'].includes(entry.kind) ? entry.sessionID : entry.workerID;
        const key = sessionOperationKey(entry.kind, id, entry.runtimeID);
        if (!sessionOperations.has(key)) sessionOperations.set(key, { ...entry, key, phase: 'unknown', pending: true, authGeneration: state.authGeneration,
          deadline: Date.now() + 120000, message: 'Recovering this request’s status. It will not be submitted again.' });
      }
    } catch (_) { /* Invalid browser storage is ignored. */ }
  }
  function pauseSessionOperations(clear = false) {
    for (const operation of sessionOperations.values()) {
      clearTimeout(operation.timer); operation.timer = null; operation.epoch = (operation.epoch || 0) + 1; operation.controller?.abort(); operation.checking = false;
      if (operationLocked(operation)) { operation.phase = 'unknown'; operation.message = 'Sign in, then check this request’s status. It will not be submitted again.'; }
    }
    if (clear) { sessionOperations.clear(); persistSessionOperations(); }
  }
  function resumeSessionOperations() {
    if (!state.authenticated || authRotationPending) return;
    for (const operation of sessionOperations.values()) {
      operation.authGeneration = state.authGeneration;
      if (operationLocked(operation) && !operation.checking && !operation.timer) requestSessionOperation(operation, 'GET');
    }
  }
  function operationCurrent(operation) {
    return state.authenticated && !authRotationPending && operation.authGeneration === state.authGeneration && sessionOperations.get(operation.key) === operation;
  }
  function updateSessionOperationView(operation) {
    if (editSessionTarget?.session_id === operation.sessionID && $('edit-session-dialog').open) renderSessionEditor();
    if (newSessionWorkerID === operation.workerID && $('new-session-dialog').open) renderSessionCreation();
    const button = [...$('session-list').querySelectorAll('.session-edit')].find(value => value.dataset.editSessionId === operation.sessionID);
    if (button) button.dataset.pending = String(operationLocked(operation));
  }
  async function sessionTargetRequest(operation, method) {
    const controller = new AbortController(); operation.controller = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);
    const parameters = { session_id: operation.sessionID, request_id: operation.requestID };
    const extra = operation.kind === 'edit' ? { name: operation.name } : { delete_project: operation.deleteProject, cwd: operation.cwd };
    try {
      const response = await fetch(api + '/sessions/' + operation.kind + (method === 'GET' ? '?' + new URLSearchParams(parameters) : ''), {
        method, credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
        ...(method === 'POST' ? { headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': window.CodexSessionAuth.cookie('__Host-telegramgw-csrf') }, body: JSON.stringify({ ...parameters, ...extra, confirmed: true }) } : {})
      });
      if (!operationCurrent(operation)) throw new Error('This request’s view expired.');
      if (response.status === 401) { expire(); throw new Error('Sign in to continue.'); }
      let result; try { result = await response.json(); } catch (_) { /* HTTP errors can be plain text. */ }
      if (!response.ok) { const error = new Error(sessionOperationMessage(result?.error_code ? result : null, 'Request failed (' + response.status + '). Check status before retrying.', operation.kind)); error.status = response.status; error.result = result; throw error; }
      if (!result || result.command_id !== operation.requestID || result.session_id !== operation.sessionID || result.worker_id !== operation.workerID ||
        result.runtime_id !== operation.runtimeID || result.runtime_generation !== operation.runtimeGeneration || typeof result.pending !== 'boolean') throw new Error('The worker returned an unexpected result.');
      return result;
    } finally { clearTimeout(timeout); if (operation.controller === controller) operation.controller = null; }
  }
  function applyConfirmedSession(session) {
    sessionInventoryEpoch++;
    const index = state.sessions.findIndex(row => row.session_id === session.session_id);
    if (index >= 0) state.sessions[index] = { ...state.sessions[index], ...session };
    else state.sessions.push(session);
    if (state.selected?.session_id === session.session_id) {
      state.selected = { ...state.selected, ...session };
      $('session-title').textContent = title(state.selected);
      $('session-location').textContent = [state.selected.worker_name, state.selected.runtime_name].filter(Boolean).join(' / ');
      sessionConnectionReady();
    }
    renderSessions();
  }
  async function finishSessionOperation(operation, result) {
    if (operation.kind === 'edit') {
      if (!result.session || result.session.session_id !== operation.sessionID || result.session.worker_id !== operation.workerID || result.session.runtime_id !== operation.runtimeID || typeof result.session.name !== 'string') throw new Error('The worker did not confirm the chosen session’s name.');
      applyConfirmedSession(result.session);
      if (editSessionTarget?.session_id === operation.sessionID) { editSessionTarget = { ...editSessionTarget, ...result.session }; $('edit-session-name').value = result.session.name; }
    } else if (operation.kind === 'archive') {
      if (result.session && (result.session.session_id !== operation.sessionID || result.session.worker_id !== operation.workerID || result.session.runtime_id !== operation.runtimeID || !result.session.archived)) throw new Error('The worker did not confirm the chosen session’s archive.');
      forgetDeletedSession(operation.sessionID, 'Session archived · choose another session');
    } else if (operation.kind === 'list') {
      if (result.archives !== undefined && !Array.isArray(result.archives)) throw new Error('The worker did not return an archive list.');
      const entries = (result.archives || []).filter(entry => entry && typeof entry.id === 'string' && typeof entry.name === 'string' && typeof entry.original_path === 'string');
      operation.archives = [...new Map([...(operation.archives || []), ...entries].map(entry => [entry.id, entry])).values()];
      operation.nextOffset = Number.isSafeInteger(result.next_offset) && result.next_offset > (operation.offset || 0) ? result.next_offset : null;
    } else if (operation.kind === 'restore') {
      if (!result.session?.session_id || result.session.worker_id !== operation.workerID || result.session.runtime_id !== operation.runtimeID || result.session.archived) throw new Error('The worker did not confirm restoration on the chosen runtime.');
      applyConfirmedSession(result.session);
    }
    operation.phase = 'complete'; operation.pending = false; operation.message = sessionOperationMessage(result, operation.kind === 'edit' ? 'Session name saved.' : operation.kind === 'archive' ? 'Archive saved on the worker. The session was archived.' : operation.kind === 'restore' ? 'Project restored. The original archive was kept.' : 'Choose an archive to restore.', operation.kind);
    operation.warningCode = result.warning_code || '';
    if (operation.warningCode === 'project_cleanup_failed') operation.message = sessionOperationMessage(result, 'Session archived. Project folder cleanup could not finish; the verified archive was kept.', operation.kind) + ' Review the retained project files on the worker.';
    if (result.archive_directory) operation.archiveDirectory = result.archive_directory;
    if (result.archive) operation.archive = result.archive;
    persistSessionOperations(); updateSessionOperationView(operation);
    if (operation.kind !== 'list') refreshSessions();
    if (operation.kind === 'restore') {
      if (newSessionWorkerID === operation.workerID && newSessionMode === 'restore' && $('new-session-dialog').open && (state.selected?.session_id || '') === operation.selectedID) {
        $('new-session-dialog').close(); selectSession(state.sessions.find(row => row.session_id === result.session.session_id) || result.session);
      } else notice('Restored “' + title(result.session) + '”. Select it in the sidebar. The original archive was kept.');
    } else if (operation.kind === 'archive' && (!$('edit-session-dialog').open || editSessionTarget?.session_id !== operation.sessionID)) notice(operation.message);
  }
  async function requestSessionOperation(operation, method) {
    if (!operationCurrent(operation) || operation.checking) return;
    clearTimeout(operation.timer); operation.timer = null;
    const epoch = operation.epoch = (operation.epoch || 0) + 1;
    const current = () => operationCurrent(operation) && operation.epoch === epoch;
    operation.checking = true; updateSessionOperationView(operation);
    try {
      const result = ['edit', 'archive'].includes(operation.kind) ? await sessionTargetRequest(operation, method) :
        await workerSessionRequest(operation, operation.kind === 'list' ? '/session-archives' : '/sessions/restore', method, method === 'POST' ? operation.kind === 'restore' ? { archive_id: operation.archiveID, confirmed: true } : { offset: operation.offset || 0 } : {});
      if (!current()) return;
      operation.pending = result.pending; operation.archiveDirectory = result.archive_directory || operation.archiveDirectory;
      if (result.pending) operation.deadline = Date.now() + 120000;
      operation.progress = result.phase || '';
      if (!result.pending && result.status === 'completed') { await finishSessionOperation(operation, result); return; }
      operation.phase = result.pending ? 'pending' : result.status === 'outcome_unknown' || result.error_code === 'outcome_unknown' ? 'unknown' : 'failed';
      operation.message = sessionOperationMessage(result, result.pending ? 'Waiting for the worker…' : 'The worker could not complete this request.', operation.kind);
    } catch (error) {
      if (!current()) return;
      operation.phase = method === 'POST' && error.status >= 400 && error.status < 500 ? 'failed' : 'unknown';
      operation.pending = operation.phase !== 'failed';
      operation.message = operation.phase === 'failed' ? error.result ? sessionOperationMessage(error.result?.error_code ? error.result : null, 'The worker rejected this request. Refresh workers and try again.', operation.kind) : error.message : 'The result has not been confirmed. This request may still be running. Check status; it will not be submitted again.';
    } finally {
      if (current()) {
        operation.checking = false; persistSessionOperations(); updateSessionOperationView(operation);
        if (operationLocked(operation) && Date.now() < operation.deadline) operation.timer = setTimeout(() => { operation.timer = null; requestSessionOperation(operation, 'GET'); }, 1500);
      }
    }
  }
  function startSessionOperation(kind, target, extra = {}) {
    const session = ['edit', 'archive'].includes(kind) ? target : null;
    const runtime = session ? state.runtimes.find(row => row.runtime_id === session.runtime_id && row.worker_id === session.worker_id) : target;
    if (!runtime) return;
    const workerID = session?.worker_id || runtime.worker_id;
    const key = sessionOperationKey(kind, session?.session_id || workerID, runtime.runtime_id);
    const operation = { kind, key, workerID, runtimeID: runtime.runtime_id, runtimeGeneration: runtime.generation,
      sessionID: session?.session_id || '', requestID: crypto.randomUUID(), authGeneration: state.authGeneration,
      selectedID: state.selected?.session_id || '', serial: ++sessionOperationSerial, phase: 'submitting', pending: true, message: kind === 'archive' ? 'Archiving project and conversation on the worker…' : kind === 'edit' ? 'Saving session name…' : kind === 'list' ? 'Loading archives from the worker…' : 'Restoring project and conversation on the worker…', deadline: Date.now() + 120000, ...extra };
    sessionOperations.set(key, operation); persistSessionOperations(); requestSessionOperation(operation, 'POST'); return operation;
  }
  function openSessionEditor(session) {
    const current = state.sessions.find(row => row.session_id === session.session_id && !row.archived && !row.deleted);
    if (!current || !state.authenticated) return;
    blurEditable(); editSessionTarget = { ...current };
    $('edit-session-name').value = current.name || title(current); $('edit-session-name').setCustomValidity('');
    $('edit-session-delete-project').checked = false;
    renderSessionEditor(); $('edit-session-dialog').showModal(); $('edit-session-close').focus({ preventScroll: true });
  }
  function renderSessionEditor() {
    if (!editSessionTarget) return;
    const session = editSessionTarget, worker = state.workers.find(row => row.ID === session.worker_id);
    $('edit-session-description').textContent = isClaude(session)
      ? 'Manage this Claude session. Deleting its conversation preserves the project directory and all project files.'
      : 'Change the session name, delete its conversation, or archive the project and conversation on its worker.';
    const editing = sessionOperationFor('edit', session.session_id), archiving = sessionOperationFor('archive', session.session_id);
    const deletion = sessionDeletions.get(session.session_id), deleting = !!deletion && !['failed', 'complete', 'retry'].includes(deletion.phase);
    const locked = operationLocked(editing) || operationLocked(archiving) || deleting;
    const removed = archiving?.phase === 'complete' || deletion?.deleted || !state.sessions.some(row => row.session_id === session.session_id && !row.archived && !row.deleted);
    const runtime = state.runtimes.find(row => row.worker_id === session.worker_id && row.runtime_id === session.runtime_id);
    const runtimeUnavailable = !runtime || runtime.state !== 'running' ? 'This runtime is unavailable. Refresh workers and try again.' : '';
    const editUnavailable = sessionFeatureUnavailable(worker, 'SupportsSessionEditing', 'edit session names') || runtimeUnavailable;
    const archiveUnavailable = sessionFeatureUnavailable(worker, 'SupportsSessionArchives', 'archive and restore projects') || runtimeUnavailable;
    $('edit-session-worker').textContent = worker?.Name || session.worker_name || session.worker_id;
    $('edit-session-path').textContent = session.cwd || 'Project path unavailable';
    $('edit-session-name').disabled = locked || removed || !!editUnavailable;
    $('edit-session-editing-help').textContent = editUnavailable; $('edit-session-editing-help').hidden = !editUnavailable;
    $('edit-session-archive-help').textContent = archiveUnavailable; $('edit-session-archive-help').hidden = !archiveUnavailable;
    $('edit-session-delete-project').disabled = locked || removed || !!archiveUnavailable;
    const archiveDirectory = archiving?.archiveDirectory || worker?.ArchiveDirectory || worker?.archive_directory || '';
    const destination = archiving?.archive?.path || (archiveDirectory && archiving?.archive?.name ? archiveDirectory.replace(/\/$/, '') + '/' + archiving.archive.name : archiveDirectory);
    $('edit-session-archive-destination').textContent = destination ? 'Archive destination on this worker: ' + destination : 'Archive folder on this worker: ~/CODEX_Archived';
    for (const [kind, operation, unavailable] of [['edit', editing, editUnavailable], ['archive', archiving, archiveUnavailable]]) {
      const button = $(kind === 'edit' ? 'edit-session-save' : 'edit-session-archive');
      button.disabled = (removed && !operationLocked(operation)) || deleting || !!operation?.checking || (locked && !operationLocked(operation)) || (!!unavailable && !operationLocked(operation));
      button.textContent = operation?.checking ? operation.phase === 'submitting' ? kind === 'edit' ? 'Saving…' : 'Archiving…' : 'Checking…' : operationLocked(operation) ? 'Check status' : kind === 'edit' ? 'Save' : 'Archive';
    }
    const deletionUnavailable = sessionDeletionUnavailable(session);
    $('edit-session-delete').disabled = locked || removed || !!authRotationPending || !!deletionUnavailable;
    $('edit-session-delete').title = deletionUnavailable;
    const latest = [editing, archiving].filter(Boolean).sort((a, b) => (b.serial || 0) - (a.serial || 0))[0];
    $('edit-session-message').textContent = latest?.message || ''; $('edit-session-message').hidden = !latest?.message;
    $('edit-session-message').dataset.error = String(['failed', 'unknown'].includes(latest?.phase) || !!latest?.warningCode);
    $('edit-session-cancel').textContent = locked || removed ? 'Close' : 'Cancel';
  }
  function submitSessionEdit(event) {
    event.preventDefault();
    if (!editSessionTarget || !state.authenticated) return;
    const existing = sessionOperationFor('edit', editSessionTarget.session_id);
    if (operationLocked(existing)) { existing.deadline = Date.now() + 120000; requestSessionOperation(existing, 'GET'); return; }
    const worker = state.workers.find(row => row.ID === editSessionTarget.worker_id);
    if ($('edit-session-save').disabled || sessionFeatureUnavailable(worker, 'SupportsSessionEditing', 'edit session names')) return;
    const name = $('edit-session-name').value.trim();
    const invalid = !name || [...name].length > 120 || /\p{Cc}/u.test(name) ? 'Enter a session name of 1–120 characters without control characters.' : ''; $('edit-session-name').setCustomValidity(invalid);
    if (invalid) { $('edit-session-name').reportValidity(); return; }
    startSessionOperation('edit', editSessionTarget, { name });
  }
  function archiveEditorSession() {
    if (!editSessionTarget || !state.authenticated || $('edit-session-archive').disabled) return;
    const existing = sessionOperationFor('archive', editSessionTarget.session_id);
    if (operationLocked(existing)) { existing.deadline = Date.now() + 120000; requestSessionOperation(existing, 'GET'); return; }
    startSessionOperation('archive', editSessionTarget, { deleteProject: $('edit-session-delete-project').checked, cwd: editSessionTarget.cwd });
  }
  function currentArchiveList() {
    const runtime = creationRuntime(), operation = runtime ? sessionOperationFor('list', newSessionWorkerID, runtime.runtime_id) : null;
    return operation?.runtimeGeneration === runtime?.generation ? operation : null;
  }
  function archiveBytes(bytes) {
    if (!Number.isFinite(bytes) || bytes < 0) return 'Size unavailable';
    if (bytes < 1024) return bytes + ' B';
    const power = Math.min(3, Math.floor(Math.log(bytes) / Math.log(1024)));
    return (bytes / 1024 ** power).toFixed(1) + ' ' + ['B', 'KiB', 'MiB', 'GiB'][power];
  }
  function renderSessionRestore(worker, runtime, restoration) {
    const unavailable = sessionFeatureUnavailable(worker, 'SupportsSessionArchives', 'archive and restore projects') || (!runtime || runtime.state !== 'running' ? 'Choose a running Codex runtime.' : '');
    const locked = operationLocked(restoration), listing = currentArchiveList();
    $('new-session-worker').textContent = (worker?.Name || newSessionWorkerID) + (runtime ? ' · ' + (runtime.name || runtime.runtime_id) : '');
    $('new-session-runtime').disabled = locked; $('new-session-name').setCustomValidity('');
    $('new-session-archives-refresh').disabled = locked || !!listing?.checking || !!unavailable;
    $('new-session-archives-more').hidden = !Number.isSafeInteger(listing?.nextOffset);
    $('new-session-archives-more').disabled = locked || operationLocked(listing) || !!unavailable;
    $('new-session-archive-directory').textContent = 'Archive folder on this worker: ' + (listing?.archiveDirectory || '~/CODEX_Archived');
    const archives = listing?.archives || [];
    // Preserve the focused radio while progress updates arrive.
    const list = $('new-session-archive-list'), signature = JSON.stringify([archives, selectedArchiveID, locked]);
    if (list.dataset.signature !== signature) {
      list.dataset.signature = signature;
      list.replaceChildren(...archives.map(entry => {
        const label = node('label', 'session-archive-choice'), radio = document.createElement('input'), body = node('span');
        radio.type = 'radio'; radio.name = 'session-archive'; radio.value = entry.id; radio.checked = entry.id === selectedArchiveID; radio.disabled = locked;
        const date = new Date(entry.created_at); const created = Number.isFinite(date.getTime()) ? date.toLocaleString() : 'Date unavailable';
        const version = typeof entry.codex_version === 'string' ? entry.codex_version.trim().replace(/^codex-cli\s+/i, '') : '';
        body.append(node('strong', '', entry.session_name || entry.name), node('small', '', entry.name), node('small', '', created + ' · ' + archiveBytes(entry.bytes) + ' · ' + (version ? 'Codex ' + version : 'Codex version unavailable')), node('small', '', 'Original path: ' + entry.original_path));
        radio.addEventListener('change', () => {
          selectedArchiveID = entry.id;
          if (restoration?.phase === 'failed') sessionOperations.delete(restoration.key);
          creationMessage(); renderSessionCreation();
        });
        label.append(radio, body); return label;
      }));
    }
    $('new-session-archives-message').textContent = unavailable || (listing && listing.phase !== 'complete' ? listing.message : archives.length ? '' : listing ? 'No .tar.xz archives found on this worker.' : 'Load the archive list to choose a project.');
    $('new-session-submit').disabled = !!authRotationPending || !!restoration?.checking || (!locked && (!!unavailable || !archives.some(entry => entry.id === selectedArchiveID)));
    $('new-session-submit').textContent = restoration?.checking ? restoration.phase === 'submitting' ? 'Restoring…' : 'Checking…' : locked ? 'Check status' : 'Restore archive';
    $('new-session-cancel').textContent = locked ? 'Close' : 'Cancel';
    if (restoration) creationMessage(restoration.message, ['failed', 'unknown'].includes(restoration.phase));
    else if (unavailable) creationMessage(unavailable, true);
  }
  function loadSessionArchives(refresh = false, more = false) {
    if (newSessionMode !== 'restore' || !newSessionWorkerID || !$('new-session-dialog').open) return;
    const runtime = creationRuntime(), worker = state.workers.find(row => row.ID === newSessionWorkerID);
    if (!runtime || runtime.state !== 'running' || sessionFeatureUnavailable(worker, 'SupportsSessionArchives', 'archive and restore projects')) { renderSessionCreation(); return; }
    const existing = currentArchiveList();
    if (operationLocked(existing)) { existing.deadline = Date.now() + 120000; requestSessionOperation(existing, 'GET'); return; }
    if (more && Number.isSafeInteger(existing?.nextOffset)) startSessionOperation('list', runtime, { offset: existing.nextOffset, archives: existing.archives });
    else if (!existing || refresh) { selectedArchiveID = ''; startSessionOperation('list', runtime); }
    else renderSessionCreation();
  }
  function submitSessionRestore() {
    if ($('new-session-submit').disabled) return;
    const existing = sessionOperationFor('restore', newSessionWorkerID);
    if (operationLocked(existing)) { existing.deadline = Date.now() + 120000; requestSessionOperation(existing, 'GET'); return; }
    const runtime = creationRuntime(), entry = currentArchiveList()?.archives?.find(row => row.id === selectedArchiveID);
    if (!runtime || !entry) return;
    startSessionOperation('restore', runtime, { archiveID: entry.id });
  }
  function openSessionDeletion(session) {
    const current = state.sessions.find(value => value.session_id === session.session_id);
    if (!current || !state.authenticated) return;
    if (sessionDeletionUnavailable(current)) return;
    blurEditable();
    deleteSessionTarget = { ...current };
    $('delete-session-name').textContent = title(current);
    $('delete-session-description').textContent = sessionDeletionDescription(current);
    $('delete-session-cwd').textContent = current.cwd || 'Working directory unavailable';
    renderSessionDeletion();
    $('delete-session-dialog').showModal();
    $('delete-session-cancel').focus({ preventScroll: true });
  }
  function renderSessionDeletion() {
    if (!deleteSessionTarget) return;
    const deletion = sessionDeletions.get(deleteSessionTarget.session_id);
    const unavailable = sessionDeletionUnavailable(deleteSessionTarget);
    const checkingPrevious = deletion?.pending || deletion?.phase === 'unknown';
    const message = $('delete-session-message'), confirm = $('delete-session-confirm');
    message.textContent = deletion?.message || unavailable;
    message.hidden = !message.textContent;
    message.dataset.error = String(['failed', 'unknown', 'retry'].includes(deletion?.phase));
    confirm.hidden = !!deletion?.deleted;
    confirm.disabled = deletion?.phase === 'submitting' || !!deletion?.checking || !!unavailable && !checkingPrevious;
    confirm.textContent = deletion?.phase === 'submitting' ? 'Deleting…' : deletion?.checking ? 'Checking…' : deletion?.pending || deletion?.phase === 'unknown' ? 'Check status' : deletion ? 'Retry deletion' : 'Delete session';
    $('delete-session-cancel').textContent = deletion ? 'Close' : 'Cancel';
    if (confirm.hidden && document.activeElement === confirm) $('delete-session-cancel').focus({ preventScroll: true });
  }
  function deletionIsCurrent(deletion) {
    return state.authenticated && deletion.authGeneration === state.authGeneration && sessionDeletions.get(deletion.session.session_id) === deletion;
  }
  function updateDeletionView(deletion) {
    if (deleteSessionTarget?.session_id === deletion.session.session_id) renderSessionDeletion();
    const button = [...$('session-list').querySelectorAll('.session-edit')].find(value => value.dataset.editSessionId === deletion.session.session_id);
    if (button) button.dataset.pending = String(!!deletion.pending);
  }
  async function deletionRequest(deletion, method) {
    const controller = new AbortController(); deletion.controller = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);
    const csrf = document.cookie.split('; ').find(value => value.startsWith('__Host-telegramgw-csrf='))?.split('=').slice(1).join('=') || '';
    const path = api + '/sessions/delete' + (method === 'GET' ? '?' + new URLSearchParams({ session_id: deletion.session.session_id, request_id: deletion.requestId }) : '');
    try {
      const response = await fetch(path, { method, credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
        ...(method === 'POST' ? { headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ session_id: deletion.session.session_id, request_id: deletion.requestId, confirmed: true }) } : {}) });
      if (!deletionIsCurrent(deletion)) throw new Error('Deletion view expired.');
      if (response.status === 401) { expire(); throw new Error('Sign in to continue.'); }
      let result; try { result = await response.json(); } catch (_) { /* HTTP errors can have a plain response. */ }
      if (!deletionIsCurrent(deletion)) throw new Error('Deletion view expired.');
      if (!response.ok) {
        const error = new Error(typeof result?.message === 'string' ? result.message : response.status === 409 ? 'This session cannot be deleted now. Wait for its running work and pending questions to finish.' : 'Deletion request failed (' + response.status + ').');
        error.status = response.status; throw error;
      }
      if (result?.session_id !== deletion.session.session_id || result.command_id !== deletion.requestId || typeof result.deleted !== 'boolean' || typeof result.pending !== 'boolean' || (result.deleted && result.pending)) throw new Error('Unexpected deletion status.');
      return result;
    } finally {
      clearTimeout(timeout);
      if (deletion.controller === controller) deletion.controller = null;
    }
  }
  function forgetDeletedSession(id, statusMessage = 'Session deleted · choose another session', refresh = true) {
    sessionInventoryEpoch++;
    state.drafts.delete(id); state.positions.delete(id);
    removeAttachment(id);
    state.sessions = state.sessions.filter(session => session.session_id !== id);
    activity.sessions.delete(id);
    sessionSettings.delete(id); activitySettingsRevisions.delete(id);
    if (state.selected?.session_id === id) {
      notificationLocation();
      if (authRestore?.id === id) authRestore = null;
      window.CodexDiagrams?.clear();
      commandUI?.sessionChanged(); state.generation++; state.stopped = true; closeSocket(); state.selected = null;
      state.items.clear(); state.questions.clear(); state.models = []; state.turn = null; state.cursor = null; state.queuedEvents = [];
      $('messages').replaceChildren(); $('questions').replaceChildren(); $('questions').hidden = true; $('prompt').value = ''; renderAttachmentDraft();
      for (const name of ['transcript', 'composer', 'disconnect', 'jump-latest']) $(name).hidden = true;
      $('empty').hidden = false; $('session-title').textContent = 'MACT'; $('session-location').textContent = 'Choose a session to continue';
      $('workspace-path').textContent = ''; $('usage').textContent = '';
      connection('connected', statusMessage); updateControls();
    }
    renderSessions(); if (refresh) refreshSessions();
  }
  function applyDeletionResult(deletion, result) {
    deletion.pending = result.pending; deletion.deleted = result.deleted;
    deletion.phase = result.deleted ? 'complete' : result.pending ? 'pending' : result.status === 'outcome_unknown' ? 'unknown' : 'failed';
    deletion.message = result.message || (result.deleted ? 'Session deleted. The working directory and all files were kept.' : result.pending ? 'Deletion requested. Waiting for the worker to confirm.' : 'The session could not be deleted.');
    if (result.deleted) forgetDeletedSession(deletion.session.session_id);
  }
  function scheduleDeletionCheck(deletion) {
    clearTimeout(deletion.timer);
    if (!deletionIsCurrent(deletion) || (!deletion.pending && deletion.phase !== 'unknown')) return;
    if (Date.now() >= deletion.deadline) {
      deletion.message = 'The deletion has not been confirmed yet. You can close this dialog and check its status again. It will not be submitted again automatically.';
      updateDeletionView(deletion); return;
    }
    deletion.timer = setTimeout(() => checkSessionDeletion(deletion), 1500);
  }
  async function checkSessionDeletion(deletion) {
    if (!deletionIsCurrent(deletion) || deletion.checking || deletion.phase === 'submitting') return;
    clearTimeout(deletion.timer); deletion.checking = true; updateDeletionView(deletion);
    try {
      const result = await deletionRequest(deletion, 'GET');
      if (!deletionIsCurrent(deletion)) return;
      applyDeletionResult(deletion, result);
    } catch (error) {
      if (!deletionIsCurrent(deletion)) return;
      if (error.status === 404) {
        deletion.pending = false; deletion.phase = 'retry';
        deletion.message = 'No deletion request is recorded. You can retry the same request, or close this dialog.';
      } else {
        deletion.phase = 'unknown'; deletion.message = 'Could not confirm deletion. The request may still be running. Check its status before retrying; it will not be submitted again automatically.';
      }
    } finally {
      deletion.checking = false;
      if (deletionIsCurrent(deletion)) { updateDeletionView(deletion); scheduleDeletionCheck(deletion); }
    }
  }
  async function confirmSessionDeletion() {
    if (!deleteSessionTarget || !state.authenticated) return;
    let deletion = sessionDeletions.get(deleteSessionTarget.session_id);
    if (deletion?.checking || deletion?.phase === 'submitting' || deletion?.deleted) return;
    if (deletion?.pending || deletion?.phase === 'unknown') { deletion.deadline = Date.now() + 120000; await checkSessionDeletion(deletion); return; }
    if (sessionDeletionUnavailable(deleteSessionTarget)) { renderSessionDeletion(); return; }
    // Only this explicit confirmation submits a destructive request. If its
    // response was lost, subsequent requests read status; they never replay it.
    deletion = { session: { ...deleteSessionTarget }, requestId: deletion?.phase === 'retry' ? deletion.requestId : crypto.randomUUID(), authGeneration: state.authGeneration,
      phase: 'submitting', pending: true, deleted: false, deadline: Date.now() + 120000, message: 'Requesting deletion. The working directory and all files will be kept.' };
    sessionDeletions.set(deletion.session.session_id, deletion); updateDeletionView(deletion);
    try {
      const result = await deletionRequest(deletion, 'POST');
      if (!deletionIsCurrent(deletion)) return;
      applyDeletionResult(deletion, result);
    } catch (error) {
      if (!deletionIsCurrent(deletion)) return;
      const rejected = [400, 403, 404, 409].includes(error.status);
      deletion.pending = !rejected; deletion.phase = rejected ? 'failed' : 'unknown';
      deletion.message = rejected ? error.message : 'The connection was interrupted. The deletion may have been accepted. Checking its status; it will not be submitted again automatically.';
    } finally {
      if (deletionIsCurrent(deletion)) { updateDeletionView(deletion); scheduleDeletionCheck(deletion); }
    }
  }
  function resetInventory() {
    sessionInventoryAbort?.abort(); sessionInventoryAbort = null;
    state.refreshing = false; state.refreshAgain = false;
  }
  async function refreshSessions() {
    if (!state.authenticated) return;
    if (state.refreshing) { state.refreshAgain = true; return; }
    state.refreshAgain = false; state.refreshing = true;
    const authGeneration = state.authGeneration, inventoryEpoch = sessionInventoryEpoch;
    const controller = new AbortController(); sessionInventoryAbort = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);
    $('refresh-sessions').disabled = true;
    try {
      const data = await fetchJSON(api + '/sessions', controller.signal);
      if (authGeneration !== state.authGeneration || !state.authenticated) return;
      // A confirmed edit/archive/restore can finish while an older inventory
      // response is in flight. Re-read before replacing that confirmed state.
      if (inventoryEpoch !== sessionInventoryEpoch) { state.refreshAgain = true; return; }
      state.sessions = (data.sessions || []).filter(session => !sessionDeletions.get(session.session_id)?.deleted);
      if (state.selected && !state.sessions.some(session => session.session_id === state.selected.session_id && !session.archived && !session.deleted)) {
        forgetDeletedSession(state.selected.session_id, 'Session is no longer active · choose another session', false);
        notice('This session is no longer active. Choose another session.');
      }
      const selectedSnapshot = state.sessions.find(session => session.session_id === state.selected?.session_id && !session.archived && !session.deleted);
      if (selectedSnapshot) {
        state.selected = { ...state.selected, ...selectedSnapshot };
        $('session-title').textContent = title(state.selected);
        sessionConnectionReady();
      }

      state.workers = Array.isArray(data.workers) ? data.workers : [];
      state.runtimes = Array.isArray(data.runtimes) ? data.runtimes : [];
      for (const session of state.sessions) {
        const settings = sessionSettings.get(session.session_id);
        if (settings) session.stats = { ...session.stats, model: settings.model, reasoning_effort: settings.effort };
      }
      state.authenticated = true;
      notificationUI?.setAuthenticated(true);
      $('auth').hidden = true;
      renderSessions();
      refreshTelegramBots();
      connectActivity();
      commandUI?.update();
      if (!state.selected) { $('empty').hidden = false; connection('connected', 'Gateway connected · choose a session'); }
      if (authRestore && !state.selected) {
        const target = state.sessions.find(session => session.session_id === authRestore.id && !session.archived && !session.deleted);
        if (target) { notificationSessionID = ''; selectSession(target); }
        else { authRestore = null; notice('Your previous session is no longer available. Choose another session.'); }
      }
      if (notificationSessionID) {
        const requested = notificationSessionID; notificationSessionID = '';
        const target = state.sessions.find(session => session.session_id === requested && !session.archived && !session.deleted);
        if (target) selectSession(target);
        else { notificationLocation(); notice('This notification’s session is no longer available. Choose another session.'); }
      }
    } catch (error) { if (authGeneration !== state.authGeneration) return; if (state.authenticated) $('sessions-status').textContent = error.message; else if ($('auth').hidden) connection('error', error.message); }
    finally {
      clearTimeout(timeout);
      if (sessionInventoryAbort === controller) sessionInventoryAbort = null;
      if (authGeneration !== state.authGeneration) return;
      state.refreshing = false; $('refresh-sessions').disabled = false;
      if (state.refreshAgain && state.authenticated) { state.refreshAgain = false; queueMicrotask(refreshSessions); }
    }
  }
  function selectSession(session) {
    showSessions(false);
    telegramBotStatus();
    if (sessionUUID.test(session.session_id)) notificationLocation(session.session_id);
    const revealed = revealWorkerSessions(session);
    if (state.selected?.session_id === session.session_id && state.connected) { if (revealed) renderSessions(); return; }
    window.CodexDiagrams?.clear();
    commandUI?.sessionChanged();
    if (state.selected) { saveDraft(); state.positions.set(state.selected.session_id, { top: $('transcript').scrollTop, bottom: atBottom() }); }
    state.generation++;
    closeSocket();
    state.selected = session;
    state.stopped = false;
    state.attempts = 0;
    state.items.clear();
    state.questions.clear();
    state.turn = null;
    state.cursor = null;
    state.models = [];
    state.queuedEvents = [];
    state.submitting = false;
    followLatest = true;
    loadingOlder = false;
    $('messages').replaceChildren();
    $('questions').replaceChildren(); $('questions').hidden = true;
    $('older').hidden = true;
    $('older').disabled = false;
    $('jump-latest').hidden = true;
    $('prompt').value = state.drafts.get(session.session_id) || '';
    renderAttachmentDraft();
    $('session-title').textContent = title(session);
    $('session-location').textContent = [session.worker_name, session.runtime_name].filter(Boolean).join(' / ');
    $('workspace-path').textContent = session.cwd || '';
    $('workspace-path').title = session.cwd || '';
    $('usage').textContent = '';
    $('empty').hidden = true;
    $('transcript').hidden = false;
    $('composer').hidden = false;
    resizePrompt();
    $('disconnect').hidden = false;
    renderModels();
    renderSessions();
    notice();
    connect(state.generation);
  }
  function connect(generation) {
    if (generation !== state.generation || state.stopped || !state.selected) return;
    // Each socket attempt owns its callbacks, including automatic retries. A late
    // response from the previous transport cannot change the new session view.
    generation = ++state.generation;
    state.queuedEvents = [];
    state.loading = true;
    disableQuestions();
    connection(state.attempts ? 'reconnecting' : 'connecting', state.attempts ? 'Reconnecting… Your work continues on the worker.' : 'Connecting to ' + title(state.selected) + '…');
    updateControls();
    const url = new URL(api + '/connect', location.href); url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'; url.searchParams.set('session_id', state.selected.session_id);
    const socket = new WebSocket(url); state.socket = socket;
    const handshakeTimer = setTimeout(() => { if (state.socket === socket && !state.connected) socket.close(); }, 20000);
    socket.onmessage = event => {
      if (generation !== state.generation || state.socket !== socket) return;
      let message;
      try { message = JSON.parse(event.data); } catch (_) { notice('Received an unreadable update. Reconnect to refresh this conversation.', 'connection'); return; }
      if (message.method) { message._observedAtMs = Date.now(); message._providerEventOrder = ++providerEventOrder; }
      if (message.type === 'ready') {
        clearTimeout(handshakeTimer);
        if (message.session?.session_id === state.selected?.session_id) state.selected = { ...state.selected, ...message.session };
        state.connected = true;
        state.attempts = 0;
        connection('loading', 'Connected · Restoring session…');
        updateControls();
        bootstrap(generation, socket).catch(error => { if (generation === state.generation && state.socket === socket) { closeSocket(); disableQuestions(); notice(error.message, 'connection'); connection('error', 'Could not load this session. Reconnect to try again.'); updateControls(); } });
        return;
      }
      if (message.type === 'error') { notice(message.message || 'Worker connection failed.', 'connection'); return; }
      if (message.id !== undefined && !message.method) {
        const request = state.pending.get(String(message.id));
        if (request) {
          clearTimeout(request.timer); state.pending.delete(String(message.id));
          if (message.error) {
            const error = new Error(message.error.message || agentLabel() + ' rejected this request.');
            error.rpcResponse = true; request.reject(error);
          } else request.resolve(message.result);
        }
        else if (message.error && state.questions.has(String(message.id))) {
          state.questions.get(String(message.id)).answered = false;
          renderQuestions(); notice(message.error.message || 'The answer was rejected. Check whether this question is still pending.');
        }
        return;
      }
      // Goals are independent of transcript loading and turn activity. Apply
      // them immediately so a delayed snapshot cannot replace a live change.
      if (handleGoalEvent(message)) return;
      if (handleClaudeStatusEvent(message)) return;
      if (isClaude() && message.method === 'thread/settings/updated') { handleEvent(message, true); return; }
      if (state.loading) {
        // Permissions are authoritative even while history is still loading.
        // A newer notification must win over a delayed resume snapshot.
        if (message.method === 'thread/settings/updated' && message.params?.threadId === state.selected?.codex_thread_id) applyPermissions(message.params.threadSettings, 'sandboxPolicy');
        state.queuedEvents.push(message);
      }
      else handleEvent(message);
    };
    socket.onclose = async event => {
      clearTimeout(handshakeTimer);
      if (generation !== state.generation || state.socket !== socket) return;
      fileUI.clear();
      state.connected = false; state.loading = false;
      clearGoal();
      clearPermissions();
      clearRateLimits();
      clearClaudeStatus();
      rejectPending('Connection interrupted. No input was resent. Check the conversation before retrying.');
      updateControls();
      disableQuestions();
      if (event.code === 4001 || event.code === 4401) { expire(); return; }
      if (state.stopped) return;
      connection('reconnecting', 'Connection lost. Your draft is kept; running work continues.');
      try { await fetchJSON('/tgw/api/v1/admin/session'); } catch (_) { if (!state.authenticated) return; }
      if (generation !== state.generation || state.stopped) return;
      if (event.code === 1008) { connection('error', 'Connection refused. Refresh sessions or sign in again.'); return; }
      const delay = Math.min(30000, 1000 * (2 ** Math.min(5, state.attempts++))) + Math.floor(Math.random() * 300);
      state.timer = setTimeout(() => connect(generation), delay);
    };
    socket.onerror = () => { /* close is authoritative; never retry a prompt from this handler. */ };
  }
  async function restoreAuthPosition(generation) {
    if (!authRestore || authRestore.id !== state.selected?.session_id) return;
    const position = authRestore; authRestore = null;
    restoringPosition = true;
    try {
      if (position.bottom) { jump(); settleBottom(); }
      else {
        followLatest = false;
        const findAnchor = () => [...$('messages').children].find(item => item.dataset.itemId === position.anchor);
        // Keep only an item ID and geometry while locked. Load the same pages
        // again after authentication; no transcript is retained in storage.
        let pages = Math.ceil(position.count / 20);
        while (!findAnchor() && state.cursor && pages-- > 0 && generation === state.generation) {
          const cursor = state.cursor; await loadOlder(); if (cursor === state.cursor) break;
        }
        if (generation !== state.generation) return;
        const anchor = findAnchor(), scroller = $('transcript');
        scroller.scrollTop = anchor ? scroller.scrollTop + anchor.getBoundingClientRect().top - scroller.getBoundingClientRect().top - position.offset : position.top;
        $('jump-latest').hidden = atBottom();
      }
      if (uncertainSend) { notice('A previous send may have reached ' + agentLabel() + '. Check the conversation before sending it again. Nothing was resent.'); uncertainSend = false; }
      window.dispatchEvent(new CustomEvent('codex-auth-restored', { detail: { sessionId: state.selected.session_id } }));
    } finally { restoringPosition = false; }
  }
  function sessionConnectionReady() {
    if (!state.connected || state.loading || !state.selected) return;
    const questions = state.questionsStatus === 'loading' ? ' · Checking questions…' : state.questionsStatus === 'error' ? ' · Questions unavailable; reconnect to retry' : '';
    connection('connected', 'Connected · ' + title(state.selected) + questions);
  }
  async function bootstrap(generation, socket) {
    const threadId = state.selected.codex_thread_id;
    const current = () => generation === state.generation && state.socket === socket && state.connected;
    const settingsBeforeResume = settingsCheckpoint();
    const permissionsBeforeResume = permissionRevision;
    const statusBeforeResume = claudeStatusRevision;
    const resume = await rpc('thread/resume', { threadId });
    if (!current()) return;
    const thread = resume?.thread || {};
    if (isClaude() && claudeStatusRevision === statusBeforeResume) applyClaudeStatus(thread.claudeStatus, threadId);
    resumedThread = { id: thread.id || threadId, cwd: typeof resume.cwd === 'string' ? resume.cwd : typeof thread.cwd === 'string' ? thread.cwd : state.selected.cwd };
    if (permissionRevision === permissionsBeforeResume) applyPermissions(resume, 'sandbox');
    if (!isClaude() && resume?.model && settingsCheckpoint() === settingsBeforeResume && sessionSettings.get(state.selected.session_id)?.source !== 'activity') {
      applySessionSettings(state.selected.session_id, resume.model, resume.reasoningEffort || '', 'resume');
    }
    connection('loading', 'Connected · Loading recent messages…');
    state.questions.clear();
    state.turn = null;
    turnTimes = new Map(); turnCursor = null;
    state.questionsStatus = 'loading';
    let initialItems = [];
    const [, metadata] = await Promise.all([
      rpc('thread/items/list', { threadId, limit: 20, sortDirection: 'desc' }).then(page => {
        if (!current()) return;
        const preserve = state.items.size > 0;
        const previous = { top: $('transcript').scrollTop, bottom: followLatest };
        const previousItems = state.items; state.items = new Map();
        state.cursor = page.nextCursor || null;
        initialItems = (page.data || []).filter(entry => entry.item?.id && entry.turnId).map(entry => entry.item.id);
        // The authoritative pending-question response owns restored questions;
        // historical async questions may already have been dismissed.
        for (const entry of [...(page.data || [])].reverse()) {
          const previous = previousItems.get(entry.item?.id);
          if (previous && entry.turnId && previous._turn === entry.turnId) state.items.set(entry.item.id, { _turn: previous._turn, _time: previous._time, _timeSource: previous._timeSource });
          ingestHistoryEntry(entry, false);
        }
        renderMessages(false);
        $('older').hidden = !state.cursor; $('older').disabled = true;
        connection('loading', 'Connected · Checking active turn…');
        if (authRestore?.id === state.selected?.session_id && !authRestore.bottom) { followLatest = false; }
        else if (!preserve || previous.bottom) { jump(); settleBottom(); }
        else { followLatest = false; $('transcript').scrollTop = previous.top; }
      }),
      rpc('thread/turns/list', { threadId, limit: 1, sortDirection: 'desc', itemsView: 'notLoaded' }),
    ]);
    if (!current()) return;
    turnCursor = metadata.nextCursor || null;
    rememberTurns(metadata.data || []);
    rememberLatestTurnError(metadata.data?.[0]);
    for (const item of state.items.values()) {
      const turn = turnTimes.get(item._turn);
      if (turn) {
        Object.assign(item, itemTiming(item, turn, {}, item));
        if (terminalStatus(turn.status)) { item._complete = true; item._terminalTurnStatus = turn.status; }
      }
    }
    for (const turn of metadata.data || []) if (isRunning(turn.status)) state.turn = turn.id;
    // Resume can know a new turn before the lightweight state page catches up.
    for (const turn of thread.turns || []) if (isRunning(turn.status)) state.turn = turn.id;
    state.loading = false;
    recoverNotice('connection', 'history');
    for (const message of state.queuedEvents.splice(0)) handleEvent(message, true);

    renderMessages(false);
    renderQuestions();
    $('older').hidden = !state.cursor;
    $('older').disabled = false;
    sessionConnectionReady();
    updateUsage(state.selected.stats);
    updateControls();
    await restoreAuthPosition(generation);
    if (!current()) return;
    settleBottom();
    const read = backgroundReader(generation, socket);
    historyRead = read;
    // A legacy source may yield only a few turns per response. The first
    // bounded page is usable immediately; fill the remaining viewport later.
    topUpHistory(generation, initialItems, read).catch(() => { /* Older remains available for a reader-initiated retry. */ });
    fillTurnMetadata(generation, read).catch(() => { /* Missing historical dates do not disable the conversation. */ });
    // Start this optional worker lookup only after the essential native reads
    // finish: older workers share its input lane with history fallback reads.
    // New live requests received after dispatch take precedence over a snapshot.
    const revision = questionRevision;
    read('gateway/questions', {}).then(snapshot => {
      if (!current()) return;
      state.questionsStatus = 'ready';
      if (questionRevision === revision) { applyQuestionSnapshot(snapshot); renderQuestions(); }
      else refreshQuestions();
      sessionConnectionReady();
    }).catch(() => {
      if (!current()) return;
      state.questionsStatus = 'error';
      sessionConnectionReady();
    });
    if (!isClaude()) { refreshRateLimits(read); refreshGoal(read); }
    refreshModels(read);
  }
  function refreshModels(read = rpc) {
    if (!state.authenticated || !state.connected || !state.selected) return;
    const generation = state.generation, authGeneration = state.authGeneration, socket = state.socket, sessionID = state.selected.session_id;
    const epoch = ++modelReadEpoch;
    const current = () => epoch === modelReadEpoch && generation === state.generation && authGeneration === state.authGeneration &&
      socket === state.socket && sessionID === state.selected?.session_id && state.authenticated && state.connected;
    if (isClaude()) { state.models = []; commandUI?.modelsChanged(); }
    read('model/list', {}).then(result => {
      if (!current()) return;
      const models = result?.data || result?.models;
      state.models = Array.isArray(models) ? models : [];
      renderModels(); updateControls();
      commandUI?.modelsChanged();
    }).catch(() => {
      if (!current()) return;
      commandUI?.modelsChanged();
    });
  }
  function refreshRateLimits(read = rpc) {
    if (isClaude()) return;
    // A newer explicit refresh also supersedes an older bootstrap read. Live
    // quota events still take precedence over either in-flight snapshot.
    const generation = state.generation, limitRevision = rateLimitRevision, limitEpoch = ++rateLimitEpoch;
    const current = () => generation === state.generation && state.connected && state.authenticated && limitEpoch === rateLimitEpoch;
    if (!rateLimitSnapshot) $('rate-limits').textContent = 'Loading limits…';
    read('account/rateLimits/read', {}).then(result => {
      if (!current()) return;
      const snapshot = result?.rateLimitsByLimitId?.codex || result?.rateLimits;
      updateRateLimits(!snapshot?.limitId || snapshot.limitId === 'codex' ? snapshot : null, limitRevision !== rateLimitRevision);
    }).catch(() => {
      if (current() && limitRevision === rateLimitRevision) clearRateLimits('Limits unavailable');
    });
  }
  function rememberTurns(turns) {
    for (const turn of turns) {
      const previous = turnTimes.get(turn.id);
      turnTimes.set(turn.id, { id: turn.id, startedAt: turn.startedAt ?? previous?.startedAt, status: turn.status ?? previous?.status });
    }
  }
  function updateHistoryTimes(items) {
    if (!items.length) return;
    const scroller = $('transcript'), bottom = followLatest, top = scroller.scrollTop;
    const anchor = [...$('messages').children].find(article => article.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
    const offset = anchor?.getBoundingClientRect().top;
    const articles = new Map([...$('messages').children].map(article => [article.dataset.itemId, article]));
    for (const item of items) {
      const header = articles.get(item.id)?.querySelector('.message-header');
      if (header) renderItemTime(header, item);
    }
    if (bottom) jump();
    else scroller.scrollTop = anchor && offset !== undefined ? top + anchor.getBoundingClientRect().top - offset : top;
  }
  function applyHistoryTimestamps(params) {
    if (params.threadId !== state.selected?.codex_thread_id || !Array.isArray(params.items)) return;
    const changed = [];
    for (const entry of params.items.slice(0, 20)) {
      const item = state.items.get(entry?.itemId);
      if (!item || item._turn !== entry.turnId || itemMilliseconds(entry.recordedAtMs) === undefined) continue;
      const timing = itemTiming(item, turnTimes.get(item._turn), { recordedAtMs: entry.recordedAtMs }, item);
      if (timing._time === item._time && timing._timeSource === item._timeSource) continue;
      Object.assign(item, timing); changed.push(item);
    }
    updateHistoryTimes(changed);
  }
  async function fillTurnMetadata(generation, read = historyRead) {
    if (generation !== state.generation || !state.connected) return;
    if (turnMetadataWork?.generation === generation) {
      await turnMetadataWork.promise;
      return fillTurnMetadata(generation, read);
    }
    const work = { generation }; turnMetadataWork = work;
    work.promise = (async () => {
      const needsMetadata = () => [...state.items.values()].some(item => item._turn && item._time === undefined && !turnTimes.has(item._turn));
      // Item pages from native runtimes need not contain turn dates. Recover
      // those dates only after readiness and only for the visible transcript.
      for (let pages = 0; pages < 20 && generation === state.generation && state.connected && turnCursor && needsMetadata(); pages++) {
        const cursor = turnCursor;
        const metadata = await read('thread/turns/list', { threadId: state.selected.codex_thread_id, cursor, limit: 20, sortDirection: 'desc', itemsView: 'notLoaded' });
        if (generation !== state.generation || !state.connected || turnCursor !== cursor) return;
        rememberTurns(metadata.data || []);
        turnCursor = metadata.nextCursor && metadata.nextCursor !== cursor ? metadata.nextCursor : null;
        const changed = [];
        let completed = false;
        for (const item of state.items.values()) {
          const turn = turnTimes.get(item._turn);
          if (!turn) continue;
          if (!item._liveObserved && terminalStatus(turn.status) && !item._complete) {
            item._complete = true; item._terminalTurnStatus = turn.status; completed = true;
          }
          const timing = itemTiming(item, turn, {}, item);
          if (timing._time === item._time && timing._timeSource === item._timeSource) continue;
          Object.assign(item, timing); changed.push(item);
        }
        if (completed) renderMessages(false); else updateHistoryTimes(changed);
      }
    })();
    try { await work.promise; }
    finally { if (turnMetadataWork === work) turnMetadataWork = null; }
  }
  function ingestHistoryEntry(entry, trackQuestions = true) {
    if (!entry.item || !entry.turnId) return;
    const turn = { ...turnTimes.get(entry.turnId), id: entry.turnId };
    if (entry.turnStartedAt !== undefined) turn.startedAt = entry.turnStartedAt;
    if (entry.turnStatus) turn.status = entry.turnStatus;
    ingestItem(entry.item, turn, false, trackQuestions, entry, 'history');
  }
  function ingestTurn(turn, prepend = false) {
    if (isRunning(turn.status)) state.turn = turn.id;
    for (const item of turn.items || []) ingestItem(item, turn, prepend, true, {}, 'live');
  }
  function ingestItem(item, turn = {}, prepend = false, trackQuestions = true, timing = {}, source = 'live') {
    const key = item.id || 'item-' + (++state.sequence);
    const previous = state.items.get(key);
    // History pages can finish after a newer live event. A delayed snapshot or
    // repeated start must not replace that command, output, or final outcome.
    const stale = previous && (previous._liveObserved && source === 'history' || toolStatus(previous).complete && item._liveStarted === true);
    const incoming = stale ? {} : item;
    const turnComplete = !stale && terminalStatus(turn.status);
    const value = {
      ...previous, ...incoming, _turn: turn.id || previous?._turn,
      ...itemTiming(item, turn, timing, turn.id && previous?._turn === turn.id ? previous : undefined),
      _complete: previous?._complete === true || incoming._complete === true || terminalStatus(incoming.status) || turnComplete,
      _liveObserved: previous?._liveObserved === true || source === 'live',
      _terminalTurnStatus: turnComplete ? turn.status : previous?._terminalTurnStatus,
    };
    // A complete native output replaces the bounded stream preview. Keep a
    // truncation notice only when the newly supplied output is itself partial.
    if (!stale && value._complete && ['aggregatedOutput', 'output'].some(field => typeof incoming[field] === 'string')) value._truncated = incoming._truncated === true;
    if (prepend && !previous) state.items = new Map([[key, value], ...state.items]); else state.items.set(key, value);
    if (trackQuestions) trackAsyncQuestions(value);
  }
  function promptText(text) {
    let value = text.trimStart();
    // Codex history can prefix actual user input with recognized context wrappers.
    // Context-only items must not dismiss unanswered questions.
    for (let i = 0; i < 32; i++) {
      const wrapper = value.match(/^<(user_instructions|environment_context|skill|user_shell_command|turn_aborted|subagent_notification|recommended_plugins|goal_context|external_[A-Za-z0-9_]+)(?:\s[^>]*)?>/i);
      const end = wrapper ? '</' + wrapper[1] + '>' : value.toLowerCase().startsWith('# agents.md instructions') ? '</instructions>' : null;
      if (!end) break;
      const index = value.toLowerCase().indexOf(end.toLowerCase());
      if (index < 0) return '';
      value = value.slice(index + end.length).trimStart();
    }
    return value;
  }
  function trackAsyncQuestions(item) {
    if (item.type === 'agentMessage' && item.delivery === 'async' && item.questions?.length) {
      const id = 'async:' + item.id;
      if (!state.questions.has(id) && ![...state.questions.values()].some(request => request.params.itemId === item.id)) {
        questionRevision++;
        state.questions.set(id, { id, synthetic: true, answered: false, method: 'item/tool/requestUserInput', params: { turnId: item._turn, itemId: item.id, questions: item.questions.map((question, index) => ({ id: String(index), question: question.title, options: (question.options || []).map(option => typeof option === 'string' ? { label: option } : option) })) } });
      }
    } else if (item.type === 'userMessage') {
      const input = promptText(itemText(item));
      if (!input) return;
      const explicit = input.startsWith('> ') && input.includes('\n\n');
      questionRevision++;
      for (const [id, request] of state.questions) {
        if (!request.synthetic) continue;
        if (!explicit) { state.questions.delete(id); continue; }
        request.params.questions = request.params.questions.filter(question => {
          // Accept older browser answers while sending the native format below.
          const prefixes = [asyncQuestionAnswerPrefix(question.question), '> ' + question.question.replaceAll('\n', '\n> ') + '\n\n'];
          return !prefixes.some(prefix => {
            const index = input.indexOf(prefix);
            return index >= 0 && (index === 0 || input.slice(0, index).endsWith('\n\n')) && input.slice(index + prefix.length).trim();
          });
        });
        if (!request.params.questions.length) state.questions.delete(id);
      }
    }
  }
  function asyncQuestionAnswerPrefix(title) {
    // Codex's native marker uses at most 512 UTF-8 bytes of the title, cut at
    // a code point boundary, with each CR/LF replaced by one space. Keep the
    // complete title in the form; this normalization applies only on the wire.
    const bytes = new TextEncoder().encode(title);
    let end = Math.min(bytes.length, 512);
    while (end < bytes.length && (bytes[end] & 0xc0) === 0x80) end--;
    const quoted = new TextDecoder('utf-8', { ignoreBOM: true }).decode(bytes.subarray(0, end)).replace(/[\r\n]/g, ' ');
    return '> ' + quoted + '\n\n';
  }
  function itemText(item) {
    if (item.type === 'reasoning') return Array.isArray(item.summary) ? item.summary.map(part => typeof part === 'string' ? part : typeof part?.text === 'string' ? part.text : '').filter(Boolean).join('\n') : '';
    if (typeof item.text === 'string') return item.text;
    if (Array.isArray(item.content)) return item.content.map(part => part.text || (part.type?.toLowerCase().includes('image') ? '[Image attachment]' : '')).filter(Boolean).join('\n');
    if (Array.isArray(item.summary)) return item.summary.map(part => typeof part === 'string' ? part : part.text || '').join('\n');
    return '';
  }
  function appendToolOutput(parent, output, truncated = false) {
    const text = clean(typeof output === 'string' ? output : JSON.stringify(output, null, 2));
    if (!text.trim()) return;
    if (truncated) parent.append(node('p', 'muted small', 'Tool output was truncated by the runtime or live preview.'));
    parent.append((window.CodexFormat.toolCode || code)(text, typeof output === 'string' ? 'output' : 'json'));
  }
  function webSearchPresentation(item) {
    const text = value => typeof value === 'string' ? clean(value).trim() : '';
    const action = item.action || {};
    const queries = [...new Set((Array.isArray(action.queries) ? action.queries : []).map(text).filter(Boolean))];
    if (!queries.length && text(action.query || item.query)) queries.push(text(action.query || item.query));
    const url = text(action.url), pattern = text(action.pattern);
    let label = 'Search · ' + (text(item.query) || queries[0] || 'Web');
    const fields = [];
    const field = (name, value) => { if (value) fields.push([name, value]); };
    if (action.type === 'openPage' || action.type === 'findInPage') {
      label = action.type === 'openPage' ? 'Open page · ' + (url || text(item.query) || 'Web') : 'Find in page · ' + (pattern || text(item.query) || url || 'Web');
      field('URL', url);
      if (action.type === 'findInPage') field('Find', pattern);
      if (!fields.length) field('Query', queries.join('\n'));
    } else field(queries.length > 1 ? 'Queries' : 'Query', queries.join('\n'));
    // Native search results are opaque JSON. Keep their public contents intact
    // as literal text rather than assuming a result schema or inserting HTML.
    const hasResults = Array.isArray(item.results) && item.results.length > 0;
    return { label, hasBody: fields.length > 0 || hasResults, body: () => {
      const body = document.createDocumentFragment();
      for (const [name, value] of fields) body.append(node('p', 'muted small', name), code(value));
      if (hasResults) { body.append(node('p', 'muted small', 'Results')); appendToolOutput(body, item.results); }
      return body;
    } };
  }
  function toolBody(item, search) {
    const body = document.createDocumentFragment(), type = item.type;
    const field = (name, value, language = '') => {
      if (value === undefined || value === null) return;
      const text = clean(typeof value === 'string' ? value : JSON.stringify(value, null, 2));
      if (!text.trim()) return;
      body.append(node('p', 'muted small tool-field-label', name), (window.CodexFormat.toolCode || code)(text, language || (typeof value === 'string' ? '' : 'json')));
    };
    if (type === 'commandExecution') field('Command', item.command || 'command', 'shell');
    else if (type === 'mcpToolCall' || type === 'dynamicToolCall') field('Tool', [item.server || item.namespace, item.tool || item.toolName || item.name].filter(Boolean).join(' / ') || type);
    if (item.arguments !== undefined) field('Arguments', item.arguments);
    else if (item.input !== undefined) field('Input', item.input);
    else if (item.params !== undefined) field('Input', item.params);
    if (item.displayNotice) body.append(node('p', 'muted small', clean(item.displayNotice)));
    if (search) body.append(search.body());
    else if (type === 'fileChange') {
      for (const change of item.changes || []) { body.append(node('p', '', clean(change.path || '')), code(change.diff || '', 'diff')); }
    } else if (type === 'plan') body.append(markdown(itemText(item)));
    else {
      const output = item.aggregatedOutput ?? item.output ?? item.result ?? item.contentItems ?? itemText(item);
      if (output !== undefined && output !== null && (typeof output !== 'string' || clean(output).trim())) {
        body.append(node('p', 'muted small tool-field-label', 'Result'));
        appendToolOutput(body, output, item._truncated);
      } else body.append(node('p', 'muted small', toolStatus(item).complete ? 'No result was reported.' : 'Waiting for result…'));
      if (item.error) field('Error', item.error);
      if (item.exitCode !== undefined && item.exitCode !== null) body.append(node('p', 'muted small', 'Exit code: ' + item.exitCode));
    }
    return body;
  }
  function renderItemTime(header, item) {
    const date = itemDate(item), label = itemTimeLabel(item);
    let time = header.querySelector('time');
    if (!date) { time?.remove(); return; }
    if (!time) { time = node('time'); header.append(time); }
    time.textContent = (label ? label + ' · ' : '') + date.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
    time.dateTime = date.toISOString(); time.dataset.source = item._timeSource;
    time.title = (label || (item._timeSource === 'item-started' ? 'Item started' : 'Item created')) + ': ' + date.toLocaleString();
  }
  function renderItem(item) {
    const type = item.type;
    const isUser = type === 'userMessage';
    const isAssistant = type === 'agentMessage';
    const isReasoning = type === 'reasoning';
    const reasoningLabel = item._turn && item._turn === state.turn && !item._complete ? (isClaude() ? 'Thinking' : 'Reasoning') : (isClaude() ? 'Thinking complete' : 'Reasoning complete');
    const isTool = !isUser && !isAssistant;
    const article = node('article', 'message ' + (isUser ? 'user' : isTool ? 'tool' : item.phase === 'commentary' ? 'commentary' : 'assistant'));
    if (isReasoning) article.classList.add('reasoning');
    article.dataset.itemId = item.id || '';
    article.dataset.fileSessionId = state.selected?.session_id || '';
    article.dataset.workerName = state.selected?.worker_name || state.selected?.worker_id || 'Worker';
    const header = node('div', 'message-header'); header.append(node('span', 'role', isUser ? '› You' : isTool ? '• ' + (isReasoning ? reasoningLabel : 'Tool') : '• ' + agentLabel()));
    renderItemTime(header, item);
    article.append(header);
    if (isUser) article.append(node('div', 'plain', clean(itemText(item))));
    else if (isAssistant) article.append(rawView ? node('div', 'command-raw', clean(itemText(item))) : markdown(itemText(item)));
    else if (isReasoning) {
      // The native public summary is optional. Never substitute private
      // reasoning content, or create an expander with no summary to display.
      const summaryText = clean(itemText(item)).trim();
      if (summaryText) {
        const details = node('details'); const summary = node('summary');
        summary.append(node('span', 'tool-title', reasoningLabel));
        details.append(summary, markdown(summaryText)); article.append(details);
      }
      if (item.displayNotice) article.append(node('p', 'muted small', clean(item.displayNotice)));
    } else if (type === 'contextCompaction') {
      // This native item has no summary or output. Only live item events can
      // distinguish an active compaction within an otherwise running turn.
      const label = item._complete ? 'Context compacted' : item._liveStarted && item._turn === state.turn ? 'Compacting context…' : 'Context compaction';
      article.append(node('p', 'tool-title', label));
      if (item.displayNotice) article.append(node('p', 'muted small', clean(item.displayNotice)));
    } else if (type === 'subAgentActivity') {
      // Native SubAgentActivity carries a lifecycle kind and target identity,
      // not tool output. Codex's TUI presents the same action as a plain line.
      const actions = { started: 'Started', interacted: 'Interacted with', interrupted: 'Interrupted', completed: 'Completed' };
      const action = Object.hasOwn(actions, item.kind) ? actions[item.kind] : 'Agent activity';
      const target = [item.agentPath, item.agentThreadId].find(value => typeof value === 'string' && clean(value).trim());
      const line = node('p', 'tool-title subagent-activity');
      line.append(document.createTextNode(action));
      if (target) line.append(document.createTextNode(' '), node('code', '', clean(target)));
      article.append(line);
      if (item.displayNotice) article.append(node('p', 'muted small', clean(item.displayNotice)));
    }
    else {
      const search = type === 'webSearch' ? webSearchPresentation(item) : null;
      const status = toolStatus(item);
      let label = type || 'Activity';
      if (type === 'commandExecution') label = (status.complete ? '' : 'Running ') + (item.command || 'command');
      else if (type === 'fileChange') label = 'File changes · ' + ((item.changes || []).map(change => change.path).join(', ') || item.status || 'pending');
      else if (type === 'mcpToolCall' || type === 'dynamicToolCall') label = [item.server || item.namespace, item.tool || item.toolName || item.name].filter(Boolean).join(' / ') || type;
      else if (search) label = search.label;
      else if (type === 'plan') label = 'Plan';
      article.dataset.failed = String(status.state === 'failed');
      article.dataset.toolState = status.state;
      article.dataset.toolComplete = String(status.complete);
      if (search && !status.complete && !search.hasBody && !item.displayNotice) article.append(node('p', 'tool-title', clean(label)));
      else {
        const details = node('details', 'tool-details'), summary = node('summary', 'tool-summary');
        const preview = clean(label).slice(0, 600);
        const previewTitle = node('span', 'tool-title');
        if (!status.complete && type === 'commandExecution' && window.CodexFormat.toolCode) {
          // Reuse shell and heredoc tokens inline, keeping the running
          // preview's existing text limit, spacing, and wrapping.
          const highlighted = window.CodexFormat.toolCode(preview, 'shell').querySelector('code');
          previewTitle.append(...highlighted.childNodes);
        } else previewTitle.textContent = preview;
        summary.append(previewTitle);
        if (status.complete) summary.append(document.createTextNode(' '), node('span', 'tool-outcome', status.label));
        summary.title = clean(label);
        details.append(summary);
        // Most completed tools remain collapsed. Build their full command and
        // result only when opened, so large native outputs do not slow history.
        let rendered = false;
        details._renderToolBody = () => {
          if (rendered) return;
          rendered = true; details.append(toolBody(item, search));
        };
        summary.addEventListener('click', () => { if (!details.open) details._renderToolBody(); });
        details.addEventListener('toggle', () => { if (details.open) details._renderToolBody(); });
        article.append(details);
      }
    }
    return article;
  }
  function renderMessages(follow = true) {
    const bottom = followLatest;
    const scroller = $('transcript'), top = scroller.scrollTop, viewportTop = scroller.getBoundingClientRect().top;
    const articles = [...$('messages').children];
    const expanded = new Map(articles.map(article => [article.dataset.itemId, { open: !!article.querySelector('details[open]:not(.mermaid-source)'), complete: article.dataset.toolComplete }]));
    // A tool finishing above the viewport can remove many lines. Anchor the
    // reader to the next stable message instead of keeping a numeric scrollTop.
    // History prepending restores its own anchor after this render; applying
    // both corrections would move the reader by the inserted height twice.
    const anchor = bottom || suppressHistoryScroll ? null : articles.find(article => article.getBoundingClientRect().bottom > viewportTop && !(article.dataset.toolComplete === 'false' && toolStatus(state.items.get(article.dataset.itemId) || {}).complete));
    const anchorTop = anchor?.getBoundingClientRect().top;
    const diagrams = new Map(articles.map(article => [article.dataset.itemId, [...article.querySelectorAll('figure.mermaid-diagram')]]));
    const fragment = document.createDocumentFragment();
    for (const item of state.items.values()) {
      const article = renderItem(item);
      const previousDetails = expanded.get(article.dataset.itemId);
      if (previousDetails?.open && !(previousDetails.complete === 'false' && article.dataset.toolComplete === 'true')) {
        const details = article.querySelector('details:not(.mermaid-source)');
        if (details) { details.open = true; details._renderToolBody?.(); }
      }
      // Reuse diagrams within the same message so live activity doesn't reset
      // their zoom/source controls or replace a decoded image with a spinner.
      const previous = diagrams.get(article.dataset.itemId) || [];
      [...article.querySelectorAll('figure.mermaid-diagram')].forEach((figure, index) => {
        const old = previous[index];
        if (old && old.querySelector('.mermaid-source code')?.textContent === figure.querySelector('.mermaid-source code')?.textContent) figure.replaceWith(old);
      });
      fragment.append(article);
    }
    $('messages').replaceChildren(fragment);
    if (follow && bottom) jump();
    else {
      const restoredAnchor = anchor && [...$('messages').children].find(article => article.dataset.itemId === anchor.dataset.itemId);
      scroller.scrollTop = restoredAnchor && anchorTop !== undefined ? top + restoredAnchor.getBoundingClientRect().top - anchorTop : top;
      if (follow) $('jump-latest').hidden = false;
    }
  }
  function scheduleRender() {
    if (state.renderTimer) return;
    state.renderTimer = setTimeout(() => { state.renderTimer = null; if (state.selected) renderMessages(); }, 80);
  }
  function updateUsage(stats) {
    if (isClaude()) { renderClaudeStatus(); return; }
    if (!stats) return;
    const context = stats.context_tokens ?? stats.last?.totalTokens;
    const maximum = stats.context_window ?? stats.modelContextWindow;
    $('usage').textContent = typeof context === 'number' && maximum > 0 ? Math.round(context / maximum * 100) + '% context' : '';
  }
  function updateRateLimits(snapshot, fillMissing = false) {
    // Codex's statusline uses the default bucket. Other model-specific buckets
    // cannot be reliably mapped to the selected model from this protocol.
    if (snapshot?.limitId && snapshot.limitId !== 'codex') {
      return;
    }
    if (snapshot) {
      rateLimitSnapshot = { ...rateLimitSnapshot };
      for (const [key, value] of Object.entries(snapshot)) {
        if (value !== null && value !== undefined && (!fillMissing || rateLimitSnapshot[key] === undefined)) rateLimitSnapshot[key] = value;
      }
    }
    rateLimitRevision++;
    const limits = $('rate-limits');
    limits.replaceChildren();
    const descriptions = [];
    for (const [key, fallback] of [['primary', 'Usage'], ['secondary', 'Secondary usage']]) {
      const window = rateLimitSnapshot?.[key];
      if (!window || !Number.isFinite(window.usedPercent)) continue;
      const duration = window.windowDurationMins;
      const label = [[300, '5h'], [1440, 'Daily'], [10080, 'Weekly'], [43200, 'Monthly'], [525600, 'Annual']].find(([minutes]) => Number.isFinite(duration) && Math.abs(duration - minutes) <= minutes * .05)?.[1] || fallback;
      const left = Math.round(Math.max(0, Math.min(100, 100 - window.usedPercent)));
      const span = node('span', 'rate-limit' + (left <= 10 ? ' exhausted' : left <= 25 ? ' low' : ''), label + ' ' + left + '% left');
      const reset = Number.isFinite(window.resetsAt) ? timestamp(window.resetsAt) : null;
      const description = label + ': ' + left + '% remaining' + (reset ? ' · resets ' + reset.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' }) : '');
      span.title = description;
      span.setAttribute('aria-label', description);
      descriptions.push(description);
      limits.append(span);
    }
    if (!limits.childNodes.length) limits.textContent = 'Limits unavailable';
    limits.title = descriptions.join('\n') || 'This runtime has not reported account usage limits.';
  }
  function handleEvent(message, deferRender = false) {
    const { method, params: p = {} } = message;
    if (!method) return; // Transport heartbeats must not repaint an idle transcript.
    if (p.threadId && p.threadId !== state.selected?.codex_thread_id) return;
    if (handleClaudeStatusEvent(message)) return;
    if (method === 'gateway/history/timestamps') { applyHistoryTimestamps(p); return; }
    if (handleGoalEvent(message)) return;
    recoverProviderRetry(message);
    if (method === 'thread/settings/updated') {
      // Older workers can already relay this native notification to viewers.
      // Once the versioned activity feed has confirmed settings, its ordered
      // revisions are authoritative across all connections and sessions.
      const settings = p.threadSettings;
      if (p.threadId === state.selected?.codex_thread_id) { applyPermissions(settings, 'sandboxPolicy'); applyClaudeSettings(settings); }
      if (p.threadId === state.selected?.codex_thread_id && settings && typeof settings.model === 'string' && (typeof settings.effort === 'string' || settings.effort === null) && sessionSettings.get(state.selected.session_id)?.source !== 'activity') {
        applySessionSettings(state.selected.session_id, settings.model, settings.effort || '', 'native');
      }
      if (isClaude() && p.threadId === state.selected?.codex_thread_id) refreshModels();
      return;
    }
    if (method === 'account/rateLimits/updated') { updateRateLimits(p.rateLimits); return; }
    if (message.id !== undefined && method) {
      questionRevision++;
      for (const [key, request] of state.questions) if (request.source === 'worker' && p.itemId && p.itemId === request.params.itemId && !request.synthetic) state.questions.delete(key);
      state.questions.set(String(message.id), { ...message, answered: false }); if (!deferRender) renderQuestions(); return;
    }
    if (method === 'turn/started') { state.turn = p.turn?.id || p.turnId; startSessionTurn(state.selected?.session_id, state.turn); ingestTurn(p.turn || {}); }
    else if (method === 'turn/completed') {
      questionRevision++;
      const turn = { ...p.turn, id: p.turn?.id || p.turnId, status: p.turn?.status || 'completed' };
      if (!state.turn || state.turn === turn.id) state.turn = null;
      ingestTurn(turn); rememberTurns([turn]);
      for (const item of state.items.values()) if (item._turn === turn.id) { item._complete = true; item._terminalTurnStatus = turn.status; }

      if (p.turn?.error) rememberSessionError(state.selected?.session_id, p.turn.error.message || 'The turn failed.', turn.id);
      else if (['completed', 'interrupted'].includes(p.turn?.status)) recoverNotice('retry:' + (p.turn?.id || p.turnId));
      for (const [key, question] of state.questions) if (!question.synthetic && question.params?.turnId === (p.turn?.id || p.turnId)) { state.questions.delete(key); }
      renderQuestions();
    } else if (method === 'thread/status/changed') {
      if (p.status?.type === 'idle') { state.turn = null; }
    } else if (method === 'item/started' || method === 'item/completed') {
      if (p.item) {
        const started = method === 'item/started';
        const observed = itemMilliseconds(message._observedAtMs) ?? Date.now();
        // Item completion does not complete its parent turn. In particular, a
        // late sparse item must retain the turn's real interrupted state.
        ingestItem({ ...p.item, _liveStarted: started, _complete: !started }, { id: p.turnId }, false, true, { startedAtMs: p.startedAtMs, completedAtMs: p.completedAtMs, ...(started ? { observedStartedAtMs: observed } : { observedCompletedAtMs: observed }) });
        renderQuestions();
      }
    } else if (method === 'item/agentMessage/delta') {
      const item = state.items.get(p.itemId) || { id: p.itemId, type: 'agentMessage', text: '', _turn: p.turnId };
      item.text = (item.text || '') + (p.delta || ''); state.items.set(p.itemId, item);
    } else if (method === 'item/commandExecution/outputDelta') {
      const item = state.items.get(p.itemId) || { id: p.itemId, type: 'commandExecution', _turn: p.turnId };
      if (!toolStatus(item).complete) {
        const output = (item.aggregatedOutput || '') + (p.delta || '');
        item._truncated = item._truncated || output.length > 64000;
        item.aggregatedOutput = output.slice(-64000); item._liveObserved = true; state.items.set(p.itemId, item);
      }
    } else if (method === 'item/reasoning/summaryTextDelta') {
      const item = state.items.get(p.itemId) || { id: p.itemId, type: 'reasoning', summary: [], _turn: p.turnId };
      const index = Number.isInteger(p.summaryIndex) && p.summaryIndex >= 0 && p.summaryIndex < 100 ? p.summaryIndex : 0;
      const summary = [...(item.summary || [])]; summary[index] = (summary[index] || '') + (p.delta || '');
      item.summary = summary; state.items.set(p.itemId, item);
    } else if (method === 'turn/diff/updated') {
      ingestItem({ id: 'diff-' + p.turnId, type: 'fileChange', changes: [{ path: 'Turn diff', diff: p.diff || '' }] }, { ...turnTimes.get(p.turnId), id: p.turnId });
    } else if (method === 'turn/plan/updated') {
      ingestItem({ id: 'plan-' + p.turnId, type: 'plan', text: [p.explanation, ...(p.plan || []).map(step => (step.status === 'completed' ? '✓ ' : step.status === 'inProgress' ? '› ' : '○ ') + step.step)].filter(Boolean).join('\n') }, { ...turnTimes.get(p.turnId), id: p.turnId });
    } else if (method === 'thread/tokenUsage/updated') { updateUsage(p.tokenUsage || p); return; }
    else if (method === 'serverRequest/resolved') { questionRevision++; state.questions.delete(String(p.requestId)); renderQuestions(); refreshQuestions(); return; }
    else if (method === 'error') {
      const retrying = p.willRetry === true;
      const text = p.error?.message || p.message || 'Codex reported an error.';
      // The native event owns its turn ID even if the active-turn snapshot is
      // still catching up. Do not turn that transient retry into a sticky error.
      if (retrying) {
        const turnID = p.turnId || state.turn || '';
        notice(agentLabel() + ' is retrying: ' + text, 'retry:' + turnID);
        providerRetry = { turnID, order: message._providerEventOrder, observedAt: message._observedAtMs,
          knownItems: new Set(state.items.keys()), completedItems: new Set([...state.items.values()].filter(item => item._complete).map(item => item.id)) };
      }
      else {
        const turnID = p.turnId || state.turn || providerRetry?.turnID || '';
        recoverNotice('retry:' + turnID);
        rememberSessionError(state.selected?.session_id, text, turnID);
      }
      return;
    }
    else return;
    updateControls();
    if (!deferRender) scheduleRender();
  }
  function disableQuestions() { for (const control of $('questions').querySelectorAll('button, input, textarea')) control.disabled = true; }
  function applyQuestionSnapshot(snapshot) {
    const pending = Array.isArray(snapshot?.questions) ? snapshot.questions : [];
    const retained = new Set(pending.map(request => 'worker:' + request.approvalId));
    const pendingItems = new Set(pending.filter(request => request.async && request.params?.itemId).map(request => request.params.itemId));
    for (const [id, request] of state.questions) {
      if (request.source === 'worker' && !retained.has(id)) state.questions.delete(id);
      else if (request.synthetic && request.source !== 'worker' && !pendingItems.has(request.params.itemId)) state.questions.delete(id);
    }
    for (const request of pending) {
      if (!request.approvalId || !request.params || !request.method) continue;
      const id = 'worker:' + request.approvalId;
      const duplicate = [...state.questions.entries()].find(([key, current]) => key !== id && request.params.itemId && current.params.itemId === request.params.itemId);
      // Live native requests are answered on this browser's connection. A
      // restored request is answered through its owning worker connection.
      if (duplicate && !duplicate[1].synthetic) continue;
      if (duplicate) state.questions.delete(duplicate[0]);
      const previous = state.questions.get(id);
      // Keep an in-flight answer attached to this exact request object. A
      // refresh must not strand a failed answer in a permanently disabled card.
      const current = previous || {};
      Object.assign(current, { id, source: 'worker', approvalId: request.approvalId, synthetic: !!request.async, method: request.method, params: request.params, answered: previous?.answered || false });
      state.questions.set(id, current);
    }
    questionRevision++;
  }
  async function refreshQuestions() {
    if (!state.connected || state.loading) return;
    const generation = state.generation, revision = questionRevision, sequence = ++questionSnapshotSequence;
    if (questionRefresh?.generation === generation) { questionRefresh.dirty = true; return; }
    const flight = { generation, dirty: false }; questionRefresh = flight;
    try {
      const snapshot = await rpc('gateway/questions', {});
      if (generation !== state.generation || sequence !== questionSnapshotSequence || revision !== questionRevision) return;
      applyQuestionSnapshot(snapshot); renderQuestions();
      state.questionsStatus = 'ready'; sessionConnectionReady();
    } catch (_) { /* Existing live requests remain usable after a missed refresh. */ }
    finally {
      if (questionRefresh === flight) {
        questionRefresh = null;
        if (flight.dirty && generation === state.generation) refreshQuestions();
      }
    }
  }
  async function sendAnswer(request, result) {
    if (!state.connected || state.loading || state.socket?.readyState !== WebSocket.OPEN || request.answered || state.questions.get(String(request.id)) !== request) return;
    const generation = state.generation;
    request.formDraft = new Map([...$('questions').querySelectorAll('textarea')].map(field => [field.name, field.value]));
    request.formChecked = new Set([...$('questions').querySelectorAll('input:checked')].map(field => field.name + '\0' + field.value));
    try {
      questionRevision++;
      request.answered = true;
      renderQuestions();
      if (request.synthetic) {
        const text = request.params.questions.map(question => asyncQuestionAnswerPrefix(question.question) + (result.answers[question.id]?.answers || []).join('\n')).join('\n\n');
        const params = { threadId: state.selected.codex_thread_id, input: [{ type: 'text', text }] };
        const reply = state.turn ? await rpc('turn/steer', { ...params, expectedTurnId: state.turn }) : await rpc('turn/start', params);
        if (generation !== state.generation) return;
        if (reply?.turn?.id) state.turn = reply.turn.id;
        state.questions.delete(String(request.id)); renderQuestions(); updateControls();
      } else if (request.source === 'worker') {
        await rpc('gateway/answer', { approvalId: request.approvalId, result });
        if (generation !== state.generation) return;
        state.questions.delete(String(request.id)); renderQuestions(); updateControls();
      } else state.socket.send(JSON.stringify({ id: request.id, result }));
    } catch (error) {
      if (generation !== state.generation) return;
      request.answered = false; renderQuestions(); notice(error.message || 'The answer could not be sent. Reconnect and check whether the question is still pending.');
    }
  }
  function renderQuestions() {
    // Preserve typed answers across unrelated events; fields are rebuilt only when request membership changes.
    const active = $('questions').contains(document.activeElement) ? document.activeElement : null;
    const focused = active?.name ? { name: active.name, value: active.value, start: active.selectionStart, end: active.selectionEnd } : null;
    const drafts = new Map([...$('questions').querySelectorAll('textarea')].map(field => [field.name, field.value]));
    const checked = new Set([...$('questions').querySelectorAll('input:checked')].map(field => field.name + '\0' + field.value));
    $('questions').replaceChildren();
    for (const request of state.questions.values()) {
      const p = request.params || {};
      const card = node('form', 'question-card' + (request.answered ? ' answered' : ''));
      card.dataset.requestId = String(request.id);
      card.append(node('h2', '', request.method.includes('requestUserInput') ? 'Input requested · ' + title(state.selected) : 'Approval requested · ' + title(state.selected)));
      if (request.answered) { card.append(node('p', '', 'Response sent. Waiting for ' + agentLabel() + ' to resolve this request.')); $('questions').append(card); continue; }
      if (request.method.includes('requestUserInput')) {
        const fields = [];
        for (const question of p.questions || []) {
          const group = node('fieldset'); const name = String(request.id) + ':' + question.id;
          group.append(node('legend', '', clean(question.question || question.header || 'Your answer')));
          for (const option of question.options || []) {
            const label = node('label'); const input = node('input'); input.type = 'radio'; input.name = name; input.value = option.label;
            const hasCurrentChoice = [...checked].some(value => value.startsWith(name + '\0'));
            input.checked = hasCurrentChoice ? checked.has(name + '\0' + option.label) : !!request.formChecked?.has(name + '\0' + option.label);
            const words = node('span', '', option.label + (option.description ? ' — ' + option.description : ''));
            label.append(input, words); group.append(label);
          }
          const text = node('textarea'); text.name = name; text.rows = 2; text.placeholder = question.options?.length ? 'Or type your answer…' : 'Type your answer…'; text.setAttribute('aria-label', question.question || question.header || 'Your answer'); text.value = drafts.get(name) ?? request.formDraft?.get(name) ?? '';
          group.append(text); card.append(group); fields.push({ question, group, text });
        }
        const actions = node('div', 'actions'); const send = node('button', 'primary', 'Send answer'); send.type = 'submit'; actions.append(send); card.append(actions);
        card.addEventListener('submit', event => {
          event.preventDefault(); const answers = {};
          for (const { question, group, text } of fields) {
            const answer = text.value.trim() || group.querySelector('input:checked')?.value;
            if (!answer) { text.focus(); notice('Answer each question before sending.'); return; }
            answers[question.id] = { answers: [answer] };
          }
          notice(); sendAnswer(request, { answers });
        });
      } else if (['item/commandExecution/requestApproval', 'item/fileChange/requestApproval'].includes(request.method)) {
        card.append(node('p', '', clean(p.reason || agentLabel() + ' needs your permission to continue.')));
        if (p.command) card.append(code(p.command));
        if (p.cwd) card.append(node('p', 'muted small', p.cwd));
        const actions = node('div', 'actions');
        const decisions = Array.isArray(p.availableDecisions) ? p.availableDecisions.filter(value => typeof value === 'string') : ['accept', 'decline'];
        const labels = { accept: 'Allow once', acceptForSession: 'Allow for session', decline: 'Decline', cancel: 'Cancel turn' };
        for (const decision of decisions) {
          if (!labels[decision]) continue;
          const button = node('button', decision === 'accept' ? 'primary' : 'quiet', labels[decision]); button.type = 'button'; button.addEventListener('click', () => sendAnswer(request, { decision })); actions.append(button);
        }
        card.append(actions);
      } else if (request.method === 'item/permissions/requestApproval') {
        card.append(node('p', '', clean(p.reason || agentLabel() + ' requests additional permissions for this turn.')));
        card.append(code(JSON.stringify(p.permissions || {}, null, 2), 'json'));
        const actions = node('div', 'actions');
        for (const [label, permissions] of [['Allow for this turn', p.permissions || {}], ['Decline', {}]]) {
          const button = node('button', label.startsWith('Allow') ? 'primary' : 'quiet', label); button.type = 'button';
          button.addEventListener('click', () => sendAnswer(request, { permissions, scope: 'turn' })); actions.append(button);
        }
        card.append(actions);
      } else {
        card.append(node('p', '', 'This request needs a control this web interface does not yet support. Open the session in ' + agentLabel() + ' or Telegram to respond.'));
      }
      $('questions').append(card);
    }
    $('questions').hidden = !state.questions.size;
    if (!state.connected || state.loading) disableQuestions();
    if (focused) {
      const replacement = [...$('questions').querySelectorAll('input,textarea')].find(field => field.name === focused.name && (field.tagName === 'TEXTAREA' || field.value === focused.value));
      if (replacement && !replacement.disabled) { replacement.focus({ preventScroll: true }); if (focused.start !== null && replacement.setSelectionRange) replacement.setSelectionRange(focused.start, focused.end); }
    }
    updateSessionIndicators();
  }
  function currentModel() { if (isClaude()) return claudeStatus?.model || ''; return sessionSettings.get(state.selected?.session_id)?.model ?? state.selected?.stats?.model ?? ''; }
  function currentModelInfo() { const current = currentModel(); return state.models.find(model => model.model === current || model.id === current) || state.models.find(model => model.model !== 'default' && model.resolvedModel === current) || state.models.find(model => model.resolvedModel === current); }
  function currentEffort() { if (isClaude()) return claudeStatus?.reasoningEffort || ''; const settings = sessionSettings.get(state.selected?.session_id); return settings ? settings.effort : state.selected?.stats?.reasoning_effort || currentModelInfo()?.defaultReasoningEffort || ''; }
  function reasoningOptions() {
    return (currentModelInfo()?.supportedReasoningEfforts || []).map(option => {
      const value = typeof option === 'string' ? option : option.reasoningEffort;
      return { value, label: isClaude() ? claudeEffortLabel(value) : value, description: typeof option === 'object' ? option.description : '' };
    }).filter(option => option.value);
  }
  function renderModels() {
    if (isClaude()) { renderClaudeStatus(); return; }
    $('model').textContent = currentModel() || 'Session model'; $('model').title = 'Use /model to change the model'; $('model').setAttribute('aria-label', 'Model');
    $('usage').removeAttribute('title'); $('usage').removeAttribute('aria-label');
    renderPermissions(); renderEfforts();
  }
  function renderEfforts() { const settings = sessionSettings.get(state.selected?.session_id); $('effort').textContent = currentEffort() || (settings && settings.source !== 'unconfirmed' ? 'Default' : 'Session effort'); settleBottom(); }
  function attachmentTarget() {
    const session = state.selected || {};
    const runtime = state.runtimes.find(row => row.runtime_id === session.runtime_id && row.worker_id === session.worker_id);
    return JSON.stringify([session.session_id, session.worker_id, session.runtime_id, session.runtime_generation ?? runtime?.generation,
      session.codex_thread_id, session.cwd, resumedThread?.id, resumedThread?.cwd]);
  }
  async function send(event) {
    event.preventDefault();
    let text = $('prompt').value.trim();
    const attachment = attachmentDrafts.get(state.selected?.session_id);
    const image = attachment?.kind === 'image' ? attachment : null;
    const file = attachment?.kind === 'file' ? attachment : null;
    const attachmentSteer = attachment && /^\/tgsteer(?:\s|$)/i.test(text);
    if (attachmentSteer) {
      if (!state.turn) { notice('There is no running turn to steer. Your attachment and message are kept.'); return; }
      text = text.replace(/^\/tgsteer(?:\s+|$)/i, '').trim();
    } else if (attachment && text.startsWith('/')) { notice('Send the attachment with a message, or use /tgsteer during a running turn. Remove the attachment before using other / commands.'); return; }
    if (!attachmentSteer && text && commandUI?.handle(text)) return;
    if ((!text && !attachment) || attachment?.loading || !state.connected || state.loading || state.submitting) return;
    if (new TextEncoder().encode(text).length > 256 * 1024) { notice('This prompt is too long. Shorten it before sending.'); return; }
    const target = attachmentTarget();
    const changedTargetMessage = 'The session workspace or runtime changed. Remove and reattach this file before sending.';
    if (file?.target && file.target !== target) { notice(changedTargetMessage); return; }
    if (file) file.target = target;
    const generation = state.generation, authGeneration = state.authGeneration, socket = state.socket;
    const sessionId = state.selected.session_id, threadId = state.selected.codex_thread_id;
    const original = $('prompt').value, steerTurn = state.turn;
    const sameDraft = () => generation === state.generation && authGeneration === state.authGeneration && state.authenticated &&
      state.socket === socket && state.selected?.session_id === sessionId && attachmentDrafts.get(sessionId) === attachment;
    const current = () => {
      if (!sameDraft() || !state.connected) return false;
      if (file && target !== attachmentTarget()) { notice(changedTargetMessage); return false; }
      return true;
    };
    const clearSentText = () => {
      if ($('prompt').value !== original) return;
      $('prompt').value = ''; state.drafts.delete(sessionId); uncertainDrafts.delete(sessionId); syncRecoveryDrafts(); resizePrompt();
      if (attachmentSteer) commandUI?.close();
    };
    let nativePromptDispatched = false;
    state.submitting = true; updateControls(); notice();
    try {
      if (capacityRetryTask?.sessionID === sessionId) {
        if (!await cancelCapacityRetry()) throw new Error('Cancel the pending capacity retry before sending this prompt. Your draft has been kept.');
        if (!current()) return;
      }
      if (file && !file.uploadedPath) {
        const dataURL = await attachmentDataURL(file);
        if (!current()) return;
        const result = await rpc('gateway/upload', { uploadId: file.uploadId, name: file.name, data: dataURL.slice(dataURL.indexOf(',') + 1) }, 120000);
        if (result?.uploadId !== file.uploadId || typeof result?.path !== 'string' || !result.path) throw new Error('The upload acknowledgement was incomplete. Your attachment has been kept; retry to check the same upload.');
        // Remember an acknowledged upload only on its original draft, even if
        // navigation interrupts the following native prompt.
        if (authGeneration === state.authGeneration && attachmentDrafts.get(sessionId) === file) file.uploadedPath = result.path;
        if (!current()) return;
        renderAttachmentDraft();
      }
      if (file && !text && !attachmentSteer) {
        if (!current()) return;
        clearSteerNotice();
        clearSentText(); removeAttachment(sessionId, file); notice('File saved in ' + file.uploadedPath);
        return;
      }
      const input = file ? [{ type: 'text', text: (text ? text + '\n\n' : '') + 'File: ' + file.uploadedPath }] : text ? [{ type: 'text', text }] : [];
      if (image) input.push({ type: 'image', url: await attachmentDataURL(image) });
      if (!current()) return;
      // Forget the old recovery key synchronously before dispatch. Even if
      // this send loses its acknowledgement, its text cannot be recovered
      // later as an apparently unsent draft. Other drafts get a fresh copy.
      uncertainDrafts.add(sessionId);
      if (draftRecovery?.enabled()) { draftRecovery.clear(); syncRecoveryDrafts(true); }
      const params = { threadId, input };
      dismissSessionError(sessionId);
      let result;
      if (steerTurn) result = await rpc('turn/steer', { ...params, expectedTurnId: steerTurn });
      else { nativePromptDispatched = true; result = await rpc('turn/start', params); }
      if (!current()) return;
      clearSentText();
      if (attachment) removeAttachment(sessionId, attachment);
      if (result?.turn?.id) { state.turn = result.turn.id; ingestTurn(result.turn); }
      if (steerTurn) queuedSteer(steerTurn);
      jump();
    } catch (error) {
      if (sameDraft()) {
        const message = error.message + (file?.uploadedPath ? ' The file is already uploaded. Retrying this draft will reuse it.' : '');
        if (error.rpcResponse && nativePromptDispatched) { rememberSessionError(sessionId, message); notice(); }
        else notice(message);
      }
    }
    finally { if (generation === state.generation && state.socket === socket) { state.submitting = false; updateControls(); } }
  }
  function prependHistoryPage(result, generation) {
    const scroller = $('transcript'), bottom = followLatest, top = scroller.scrollTop, previousHeight = scroller.scrollHeight;
    const anchor = [...$('messages').children].find(item => item.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top);
    const anchorID = anchor?.dataset.itemId, anchorOffset = anchor?.getBoundingClientRect().top;
    // Native events received during this request own existing items. A saved
    // page may add older entries, but cannot replace a newer live item.
    const previous = state.items; state.items = new Map();
    for (const entry of [...(result.data || [])].reverse()) if (!previous.has(entry.item?.id)) ingestHistoryEntry(entry, false);
    for (const [key, item] of previous) state.items.set(key, item);
    state.cursor = result.nextCursor || null;
    suppressHistoryScroll = true;
    renderMessages(false); $('older').hidden = !state.cursor;
    if (bottom) { jump(); settleBottom(); }
    else {
      const restored = [...$('messages').children].find(item => item.dataset.itemId === anchorID);
      scroller.scrollTop = restored && anchorOffset !== undefined ? top + restored.getBoundingClientRect().top - anchorOffset : top + scroller.scrollHeight - previousHeight;
    }
    requestAnimationFrame(() => requestAnimationFrame(() => { if (generation === state.generation) suppressHistoryScroll = false; }));
  }
  async function topUpHistory(generation, initialItems, read) {
    if (loadingOlder || !state.cursor || initialItems.length >= 20 || state.items.size >= 20) return;
    loadingOlder = true; $('older').disabled = true;
    const seen = new Set(initialItems);
    try {
      for (let pages = 0; pages < 20 && generation === state.generation && state.connected && state.cursor && seen.size < 20 && state.items.size < 20; pages++) {
        const cursor = state.cursor;
        const result = await read('thread/items/list', { threadId: state.selected.codex_thread_id, cursor, limit: Math.min(20 - seen.size, 20 - state.items.size), sortDirection: 'desc' });
        if (generation !== state.generation || !state.connected) return;
        for (const entry of result.data || []) if (entry.item?.id && entry.turnId) seen.add(entry.item.id);
        prependHistoryPage(result, generation);
        if (state.cursor === cursor) state.cursor = null;
        fillTurnMetadata(generation, read).catch(() => { /* Dates can be recovered on a later history read. */ });
      }
    } finally {
      if (generation === state.generation) { loadingOlder = false; $('older').hidden = !state.cursor; $('older').disabled = false; }
    }
  }
  async function loadOlder() {
    if (!state.cursor || !state.connected || state.loading || loadingOlder) return;
    const generation = state.generation;
    followLatest = false;
    loadingOlder = true;
    $('older').disabled = true;
    try {
      const threadId = state.selected.codex_thread_id;
      const result = await rpc('thread/items/list', { threadId, cursor: state.cursor, limit: 20, sortDirection: 'desc' });
      if (generation !== state.generation) return;
      // The user can keep reading while the page is in flight. Anchor at the
      // current position, not where the request originally started.
      prependHistoryPage(result, generation);
      fillTurnMetadata(generation).catch(() => { /* Historical dates do not gate reading the page. */ });
      recoverNotice('history');
      if ($('reconnect').textContent === 'Reload recent messages') $('reconnect').hidden = true;
    } catch (error) {
      if (generation === state.generation) {
        notice(error.message, 'history');
        $('reconnect').textContent = 'Reload recent messages'; $('reconnect').hidden = false;
      }
    } finally { if (generation === state.generation) { loadingOlder = false; $('older').disabled = false; } }
  }
  function disconnect() {
    saveDraft(); commandUI?.sessionChanged(); state.stopped = true; state.generation++;
    closeSocket(); disableQuestions(); connection('disconnected', 'Disconnected. Running work continues; your draft is kept.'); updateControls();
  }
  async function gatewayCommand(command, options = {}) {
    const readonly = command === 'tgstatus' || command === 'tginstances';
    const generation = state.generation;
    const csrf = document.cookie.split('; ').find(value => value.startsWith('__Host-telegramgw-csrf='))?.split('=').slice(1).join('=') || '';
    const controller = new AbortController(); gatewayCommandAbort = controller;
    const timeout = setTimeout(() => controller.abort(), 30000);
    let response;
    try {
      const parameters = { command, ...(options.includeSession !== false && state.selected ? { session_id: state.selected.session_id } : {}) };
      const url = api + '/commands' + (readonly ? '?' + new URLSearchParams(parameters) : '');
      response = await fetch(url, { method: readonly ? 'GET' : 'POST', credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
        ...(readonly ? {} : { headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify(parameters) }) });
      if (response.status === 401) { if (generation === state.generation) expire(); throw new Error('Sign in to continue.'); }
      if (!response.ok) throw new Error('Gateway command failed (' + response.status + '). Refresh the page and retry if necessary.');
      return await response.json();
    } catch (error) {
      if (response && !response.ok) throw error;
      if (readonly) throw new Error('Could not read the latest gateway status. Check your connection and try again.');
      throw new Error('The connection was interrupted or timed out. The command may have been accepted; check /tgstatus before retrying. It will not be resent automatically.');
    } finally { clearTimeout(timeout); if (gatewayCommandAbort === controller) gatewayCommandAbort = null; }
  }
  async function applyCommandResult(name, args, result, settingsBeforeCommand) {
    const commandSettingsCurrent = () => (sessionSettings.get(state.selected?.session_id)?.source !== 'activity' || isClaude() && settingsBeforeCommand !== undefined) &&
      (settingsBeforeCommand === undefined || settingsBeforeCommand === settingsCheckpoint());
    // A confirmed redemption can change this account's allowances even when
    // the runtime does not publish a quota event. Refresh once, without ever
    // retrying the account mutation or delaying its visible result.
    if (name === 'usage' && /^confirm\s/.test(args) && !result.error) refreshRateLimits();
    if (name === 'setbotname' && !result.error) refreshTelegramBots();
    if (name === 'goal' && !result.error) refreshGoal();
    if (result.turn_id) { state.turn = result.turn_id; updateControls(); }
    if (result.session && ['new', 'fork'].includes(name)) {
      const generation = state.generation;
      // The worker's durable inventory event can arrive just after its command
      // reply. Wait for the gateway to authorize the new ID before attaching.
      for (let attempt = 0; attempt < 20; attempt++) {
        await refreshSessions();
        if (generation !== state.generation) return;
        const created = state.sessions.find(session => session.session_id === result.session.session_id);
        if (created) { selectSession(created); return; }
        await new Promise(resolve => setTimeout(resolve, 250));
      }
      notice('Session created. Refresh the session list to connect when its worker inventory arrives.');
    } else if (['archive', 'delete'].includes(name)) {
      window.CodexDiagrams?.clear();
      disconnect(); state.selected = null; state.items.clear(); state.questions.clear();
      $('messages').replaceChildren(); $('questions').replaceChildren(); $('questions').hidden = true;
      for (const id of ['transcript', 'composer', 'disconnect', 'jump-latest']) $(id).hidden = true;
      $('empty').hidden = false; $('session-title').textContent = 'MACT'; $('session-location').textContent = 'Choose a session to continue';
      notice(result.text || 'Conversation removed from the session list.'); await refreshSessions();
    } else if (state.selected && name === 'rename') {
      state.selected.name = args;
      state.sessions = state.sessions.map(session => session.session_id === state.selected.session_id ? { ...session, name: args } : session);
      $('session-title').textContent = args; renderSessions();
      connection('connected', 'Connected · ' + args);
    } else if (state.selected && name === 'model' && args && !args.startsWith('--') && !result.model_menu) {
      const [model, effort] = args.split(/\s+/);
      if (commandSettingsCurrent()) {
        const canonical = state.models.find(value => value.model === model || value.id === model)?.model || model;
        applySessionSettings(state.selected.session_id, canonical, isClaude() ? effort || '' : effort || currentEffort(), 'command');
      }
    } else if (state.selected && ['reasoning', 'effort'].includes(name) && args) {
      if (isClaude() && args === 'default') args = '';
      if (commandSettingsCurrent()) applySessionSettings(state.selected.session_id, currentModel(), args, 'command');
    }
  }
  async function commandHistory(count) {
    const generation = state.generation, threadId = state.selected.codex_thread_id;
    let cursor = null; const found = [], seen = new Set();
    for (let page = 0; page < 20 && found.length < count; page++) {
      const result = await rpc('thread/turns/list', { threadId, limit: 20, sortDirection: 'desc', itemsView: 'full', ...(cursor ? { cursor } : {}) });
      if (generation !== state.generation) throw new Error('The selected session changed.');
      for (const turn of result.data || []) {
        for (const item of [...(turn.items || [])].reverse()) {
          if (!['userMessage', 'agentMessage'].includes(item.type) || item.phase === 'commentary') continue;
          const key = turn.id + ':' + item.id;
          if (seen.has(key)) continue; seen.add(key);
          const value = item.type === 'userMessage' ? promptText(itemText(item)) : itemText(item);
          if (!value.trim()) continue;
          const previous = state.items.get(item.id);
          const timing = itemTiming(item, turn, {}, previous?._turn === turn.id ? previous : undefined);
          const date = itemDate(timing), label = itemTimeLabel(timing);
          found.push({ role: item.type === 'userMessage' ? 'You' : agentLabel(), text: value, time: date ? (label ? label + ': ' : '') + date.toLocaleString() : 'Date/time unavailable' });
          if (found.length === count) break;
        }
        if (found.length === count) break;
      }
      if (!result.nextCursor || result.nextCursor === cursor) break;
      cursor = result.nextCursor;
    }
    return found.reverse();
  }
  commandUI = window.CodexCommandUI.create({
    isClaude, agentLabel, deletionUnavailable: sessionDeletionUnavailable, deletionDescription: sessionDeletionDescription,
    state, rpc, title, notice, queuedSteer, saveDraft, updateControls, showSessions, renderSessions, renderQuestions, selectSession, refreshSessions,
    currentModel, currentEffort, reasoningOptions, settingsCheckpoint,
    cancelGatewayCommand: () => gatewayCommandAbort?.abort(),
    composerHidden: () => $('composer').hidden, gatewayCommand, applyResult: applyCommandResult, disconnect,
    readHistory: commandHistory, setRaw: value => { rawView = value; renderMessages(false); },
    lastResponse: () => itemText([...state.items.values()].reverse().find(item => item.type === 'agentMessage' && item.phase !== 'commentary') || {}),
    exportText: () => '# ' + title(state.selected) + '\n\n' + [...state.items.values()].filter(item => ['userMessage', 'agentMessage'].includes(item.type)).map(item => '## ' + (item.type === 'userMessage' ? 'You' : agentLabel()) + (itemDate(item) ? ' · ' + (itemTimeLabel(item) ? itemTimeLabel(item) + ': ' : '') + itemDate(item).toISOString() : '') + '\n\n' + clean(item.type === 'userMessage' ? promptText(itemText(item)) : itemText(item))).join('\n\n'),
  });
  for (const [id, name] of [['model', 'model'], ['effort', 'reasoning'], ['permissions', 'permissions']]) $(id).addEventListener('click', () => commandUI.handle('/' + (id === 'effort' && isClaude() ? 'effort' : name)));
  $('show-sessions').addEventListener('click', () => showSessions(true));
  $('empty-sessions').addEventListener('click', () => showSessions(true));
  $('close-sessions').addEventListener('click', () => showSessions(false));
  $('sidebar-backdrop').addEventListener('click', () => showSessions(false));
  $('open-settings').addEventListener('click', openSettings);
  $('open-logout').addEventListener('click', openLogout);
  $('logout-confirm').addEventListener('click', confirmLogout);
  $('logout-cancel').addEventListener('click', () => { if (!logoutPending) $('logout-dialog').close(); });
  $('logout-dialog').addEventListener('cancel', event => { if (logoutPending) event.preventDefault(); });
  $('logout-dialog').addEventListener('close', () => {
    $('open-logout').setAttribute('aria-expanded', 'false'); logoutError();
    const target = !state.authenticated ? $('auth-login') : matchMedia('(max-width:650px)').matches ? $('show-sessions') : $('open-logout');
    if (target.getClientRects().length) target.focus({ preventScroll: true });
  });
  $('show-goal').addEventListener('click', openGoal);
  $('close-goal').addEventListener('click', () => $('goal-dialog').close());
  $('goal-dialog').addEventListener('close', () => {
    const dialog = $('goal-dialog');
    if (dialog.open) return;
    const sessionID = goalDialogSessionID; goalDialogSessionID = '';
    $('show-goal').setAttribute('aria-expanded', 'false');
    const target = !state.authenticated ? $('auth-login') : sessionID === state.selected?.session_id && !$('show-goal').hidden ? $('show-goal') :
      matchMedia('(max-width:650px)').matches ? $('show-sessions') : $('transcript');
    if (target.getClientRects().length) target.focus({ preventScroll: true });
  });
  $('goal-dialog').addEventListener('click', event => {
    const dialog = $('goal-dialog');
    if (event.target !== dialog) return;
    const bounds = dialog.getBoundingClientRect();
    if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) dialog.close();
  });
  $('close-settings').addEventListener('click', () => $('settings-dialog').close());
  $('settings-dialog').addEventListener('close', () => {
    $('open-settings').setAttribute('aria-expanded', 'false');
    const target = !state.authenticated ? $('auth-login') : matchMedia('(max-width:650px)').matches ? $('show-sessions') : $('open-settings');
    if (target.getClientRects().length) target.focus({ preventScroll: true });
  });
  $('settings-dialog').addEventListener('click', event => {
    const dialog = $('settings-dialog');
    if (event.target !== dialog) return;
    const bounds = dialog.getBoundingClientRect();
    if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) dialog.close();
  });
  $('session-search').addEventListener('input', renderSessions);
  $('refresh-sessions').addEventListener('click', refreshSessions);
  $('new-session-form').addEventListener('submit', submitSessionCreation);
  $('new-session-name').addEventListener('input', () => {
    if (sessionCreations.get(newSessionWorkerID)?.phase === 'failed') { sessionCreations.delete(newSessionWorkerID); creationMessage(); }
    updateNewSessionPath(); renderSessionCreation();
  });
  $('new-session-cwd').addEventListener('input', () => {
    newSessionPathEdited = true;
    if (sessionCreations.get(newSessionWorkerID)?.phase === 'failed') { sessionCreations.delete(newSessionWorkerID); creationMessage(); }
    updateNewSessionPath(); renderSessionCreation();
  });
  $('new-session-runtime').addEventListener('change', () => {
    cancelWorkspaceBrowse(); workspacePage = null; $('new-session-browser').hidden = true;
    if (sessionCreations.get(newSessionWorkerID)?.phase === 'failed') sessionCreations.delete(newSessionWorkerID);
    selectedArchiveID = ''; creationMessage(); updateNewSessionPath(); renderSessionCreation();
    if (newSessionMode === 'restore') loadSessionArchives();
  });
  for (const mode of ['create', 'restore']) $('new-session-mode-' + mode).addEventListener('click', () => {
    if ($('new-session-mode-' + mode).disabled) return;
    cancelWorkspaceBrowse(); newSessionMode = mode; creationMessage(); renderSessionCreation();
    if (mode === 'restore') loadSessionArchives();
  });
  $('new-session-archives-refresh').addEventListener('click', () => loadSessionArchives(true));
  $('new-session-archives-more').addEventListener('click', () => loadSessionArchives(false, true));
  $('new-session-browse').addEventListener('click', () => browseSessionWorkspace(newSessionPathEdited ? $('new-session-cwd').value.trim() : ''));
  $('new-session-parent').addEventListener('click', () => { if (workspacePage?.parent) browseSessionWorkspace(workspacePage.parent); });
  $('new-session-default').addEventListener('click', () => browseSessionWorkspace());
  $('new-session-more').addEventListener('click', () => { if (workspacePage?.has_more) browseSessionWorkspace(workspacePage.path, workspacePage.offset + workspacePage.directories.length); });
  $('new-session-use-folder').addEventListener('click', () => { if (workspacePage) { newSessionPathEdited = true; $('new-session-cwd').value = workspacePage.path; updateNewSessionPath(); } });
  for (const id of ['new-session-close', 'new-session-cancel']) $(id).addEventListener('click', () => $('new-session-dialog').close());
  $('new-session-dialog').addEventListener('close', () => {
    const workerID = newSessionWorkerID; newSessionWorkerID = ''; cancelWorkspaceBrowse(); workspacePage = null;
    $('new-session-name').value = ''; $('new-session-cwd').value = ''; creationMessage();
    const button = [...$('session-list').querySelectorAll('.session-create')].find(value => value.dataset.createWorkerId === workerID);
    if (button && !button.disabled && button.getClientRects().length) button.focus({ preventScroll: true });
    else if (matchMedia('(max-width:650px)').matches && $('show-sessions').getClientRects().length) $('show-sessions').focus({ preventScroll: true });
  });
  $('edit-session-form').addEventListener('submit', submitSessionEdit);
  $('edit-session-name').addEventListener('input', () => $('edit-session-name').setCustomValidity(''));
  $('edit-session-archive').addEventListener('click', archiveEditorSession);
  $('edit-session-delete').addEventListener('click', () => {
    if (!editSessionTarget || $('edit-session-delete').disabled) return;
    const target = { ...editSessionTarget }; $('edit-session-dialog').close(); openSessionDeletion(target);
  });
  for (const id of ['edit-session-close', 'edit-session-cancel']) $(id).addEventListener('click', () => $('edit-session-dialog').close());
  $('edit-session-dialog').addEventListener('close', () => {
    const id = editSessionTarget?.session_id; editSessionTarget = null;
    $('edit-session-name').value = ''; $('edit-session-path').textContent = ''; $('edit-session-worker').textContent = '';
    const button = [...$('session-list').querySelectorAll('.session-edit')].find(value => value.dataset.editSessionId === id);
    if (button && button.getClientRects().length) button.focus({ preventScroll: true });
    else if (matchMedia('(max-width:650px)').matches && $('show-sessions').getClientRects().length) $('show-sessions').focus({ preventScroll: true });
  });
  $('delete-session-confirm').addEventListener('click', confirmSessionDeletion);
  $('delete-session-cancel').addEventListener('click', () => $('delete-session-dialog').close());
  $('delete-session-dialog').addEventListener('close', () => {
    const id = deleteSessionTarget?.session_id; deleteSessionTarget = null;
    const button = [...$('session-list').querySelectorAll('.session-edit')].find(value => value.dataset.editSessionId === id);
    if (button && button.getClientRects().length) button.focus({ preventScroll: true });
    else if (matchMedia('(max-width:650px)').matches && $('show-sessions').getClientRects().length) $('show-sessions').focus({ preventScroll: true });
  });
  $('prompt').setAttribute('aria-controls', 'command-suggestions');
  $('prompt').setAttribute('aria-autocomplete', 'list');
  $('prompt').addEventListener('input', () => { uncertainDrafts.delete(state.selected?.session_id); saveDraft(); commandUI.changed(); });
  $('prompt').addEventListener('keydown', event => { if (commandUI.keydown(event)) return; if (event.key === 'Enter' && !event.shiftKey && !event.isComposing && !matchMedia('(pointer: coarse)').matches) { event.preventDefault(); if (!$('send').disabled) $('composer').requestSubmit(); } });
  $('composer').addEventListener('submit', send);
  $('attach-image').addEventListener('click', () => { attachmentPickerSession = { id: state.selected?.session_id, generation: state.generation }; $('image-file').click(); });
  $('image-file').addEventListener('change', () => {
    const file = $('image-file').files[0]; $('image-file').value = '';
    if (attachmentPickerSession?.id === state.selected?.session_id && attachmentPickerSession.generation === state.generation) attachFile(file, attachmentPickerSession.id);
    attachmentPickerSession = null;
  });
  $('remove-image').addEventListener('click', () => { if (!state.submitting) removeAttachment(state.selected?.session_id); });
  $('remove-file').addEventListener('click', () => { if (!state.submitting) removeAttachment(state.selected?.session_id); });
  $('prompt').addEventListener('paste', event => {
    const files = [...(event.clipboardData?.items || [])].filter(item => item.kind === 'file').map(item => item.getAsFile()).filter(Boolean);
    if (!files.length) return;
    event.preventDefault();
    if (files.length !== 1) { notice('Paste one attachment per message.'); return; }
    attachFile(files[0]);
  });
  $('composer').addEventListener('dragover', event => { if ([...(event.dataTransfer?.types || [])].includes('Files')) event.preventDefault(); });
  $('composer').addEventListener('drop', event => {
    if (!event.dataTransfer?.files.length) return;
    event.preventDefault();
    if (event.dataTransfer.files.length !== 1) { notice('Attach one file or image per message.'); return; }
    attachFile(event.dataTransfer.files[0]);
  });
  $('stop').addEventListener('click', () => { const generation = state.generation; if (state.turn) rpc('turn/interrupt', { threadId: state.selected.codex_thread_id, turnId: state.turn }).catch(error => { if (generation === state.generation) notice(error.message); }); });
  $('older').addEventListener('click', loadOlder);
  $('jump-latest').addEventListener('click', jump);
  $('transcript').addEventListener('scroll', () => {
    const scroller = $('transcript'), top = scroller.scrollTop;
    const movingUp = top < lastHistoryScrollTop;
    const sameLayout = scroller.scrollHeight === lastHistoryHeight && scroller.clientHeight === lastHistoryViewport;
    lastHistoryScrollTop = top;
    lastHistoryHeight = scroller.scrollHeight; lastHistoryViewport = scroller.clientHeight;
    if (!suppressHistoryScroll) {
      if (scroller.scrollHeight - top - scroller.clientHeight <= 3) followLatest = true;
      // Focusing/removing a command choice can scroll the conversation while
      // its panel changes size. Explicit wheel/touch/key input still opts out
      // immediately; do not mistake menu focus scrolls for reading history.
      else if (movingUp && sameLayout && $('command-panel').hidden) followLatest = false;
    }
    if (atBottom()) $('jump-latest').hidden = true;
    if (movingUp && top <= 64 && !suppressHistoryScroll && !restoringPosition) loadOlder();
  }, { passive: true });
  $('transcript').addEventListener('wheel', event => { if (event.deltaY < 0) followLatest = false; }, { passive: true });
  $('transcript').addEventListener('keydown', event => { if (['ArrowUp', 'PageUp', 'Home'].includes(event.key)) followLatest = false; });
  let historyTouchY = null;
  $('transcript').addEventListener('touchstart', event => { historyTouchY = event.touches[0]?.clientY ?? null; }, { passive: true });
  $('transcript').addEventListener('touchmove', event => { if (historyTouchY !== null && event.touches[0]?.clientY > historyTouchY + 3) followLatest = false; }, { passive: true });
  $('disconnect').addEventListener('click', disconnect);
  function reconnect() { if (!state.selected) { refreshSessions(); return; } state.generation++; closeSocket(); state.stopped = false; state.attempts = 0; connect(state.generation); }
  $('reconnect').addEventListener('click', reconnect);
  document.addEventListener('visibilitychange', () => { if (document.hidden) { flushRecoveryDrafts(); closeActivity(); } });
  window.addEventListener('pagehide', closeActivity);
  navigator.serviceWorker?.addEventListener('message', event => {
    if (event.data?.type !== 'codex-notification-open') return;
    let source;
    try { source = new URL(event.source?.scriptURL); } catch (_) { return; }
    if (source.origin !== location.origin || source.pathname !== '/tgw/webui/sw.js') return;
    if (openNotification(event.data.url)) event.ports?.[0]?.postMessage({ type: 'notification-handled' });
  });
  document.addEventListener('keydown', event => { if (event.key === 'Escape' && !event.defaultPrevented && !document.querySelector('dialog[open]') && document.body.classList.contains('sessions-open')) { showSessions(false); $('show-sessions').focus({ preventScroll: true }); } });
  // Safari permits manual pinch despite viewport limits; keep one-finger
  // scrolling and text editing native, and cancel only zoom gestures.
  for (const type of ['gesturestart', 'gesturechange']) {
    document.addEventListener(type, event => event.preventDefault(), { passive: false });
  }
  // Use document coordinates for the absolute shell. Safari may move its
  // layout viewport as well as its visual viewport when focusing an input;
  // offsetTop alone misses that movement and fixed elements can be clipped.
  let viewportFrame = null;
  let viewportSettleUntil = 0;
  let viewportGeometry = null;
  // Font scaling and questions change footer height. Keep this control above
  // the actual footer rather than positioning it over the Send button.
  const footerObserver = new ResizeObserver(() => {
    if ($('transcript').hidden) return;
    const height = $('workspace').getBoundingClientRect().bottom - $('transcript').getBoundingClientRect().bottom;
    $('workspace').style.setProperty('--conversation-footer-height', Math.max(0, height) + 'px');
    settleBottom();
  });
  for (const id of ['transcript', 'messages', 'composer', 'questions', 'notice', 'command-panel']) footerObserver.observe($(id));
  function syncViewport() {
    viewportFrame = null;
    const viewport = window.visualViewport;
    const geometry = {
      top: Math.max(0, viewport?.pageTop ?? (window.scrollY + (viewport?.offsetTop || 0))),
      left: Math.max(0, viewport?.pageLeft ?? (window.scrollX + (viewport?.offsetLeft || 0))),
      width: viewport?.width || window.innerWidth,
      height: viewport?.height || window.innerHeight,
    };
    if (!viewportGeometry || Object.keys(geometry).some(key => geometry[key] !== viewportGeometry[key])) {
      const bottom = followLatest;
      const widthChanged = !viewportGeometry || geometry.width !== viewportGeometry.width;
      const heightChanged = !viewportGeometry || geometry.height !== viewportGeometry.height;
      const style = document.documentElement.style;
      for (const [key, value] of Object.entries(geometry)) {
        if (!viewportGeometry || value !== viewportGeometry[key]) style.setProperty('--viewport-' + key, value + 'px');
      }
      viewportGeometry = geometry;
      // Changing the textarea height during every keyboard-pan frame can
      // trigger another native caret scroll. Only remeasure when it can wrap.
      if (widthChanged) resizePrompt(bottom);
      else if (heightChanged && bottom) jump();
    }
    // WebKit can report old viewport metrics in the focus/resize event and
    // publish the final values later, without another event. Recheck briefly,
    // then stop completely; idle sessions must not run a polling loop.
    if (!document.hidden && performance.now() < viewportSettleUntil) scheduleViewport();
  }
  function scheduleViewport() {
    if (viewportFrame === null) viewportFrame = requestAnimationFrame(syncViewport);
  }
  function settleViewport(duration = 300) {
    viewportSettleUntil = Math.max(viewportSettleUntil, performance.now() + duration);
    scheduleViewport();
  }
  for (const type of ['resize', 'scroll']) {
    window.addEventListener(type, () => settleViewport(), { passive: true });
    window.visualViewport?.addEventListener(type, () => settleViewport(), { passive: true });
  }
  window.addEventListener('orientationchange', () => settleViewport(1000));
  document.addEventListener('focusin', () => settleViewport(1000));
  document.addEventListener('focusout', () => settleViewport(1000));
  document.addEventListener('visibilitychange', () => { if (!document.hidden) settleViewport(1000); });
  syncViewport();
  notificationUI = window.CodexNotifications.create({ expired: expire });
  if (notificationSessionID) notificationLocation(notificationSessionID);
  // Activity is pushed independently of the selected session. No history or
  // session-list polling runs while idle, and no transcript is persisted here.
  const loginTrust = window.CodexSessionAuth.bindTrustControls($('auth-trust-device'), $('auth-device-name'), $('auth-device-name-field'));
  sessionAuth = window.CodexSessionAuth.create({
    mount: $('workspace'), isLocked: () => !state.authenticated && !$('auth').hidden,
    onExpired: reason => { loginTrust.reset(); lockAuthentication(reason); },
    onRenewing: (busy, method) => {
      authRenewing = busy; renderLogout();
      $('auth-login').disabled = busy; $('auth-login').textContent = busy && method !== 'password' ? 'Waiting for passkey…' : 'Continue with passkey';
      $('auth-password').disabled = busy;
      loginTrust.setBusy(busy);
      if (!busy && authRotationPending) {
        authRotationPending = false;
        renderLogout();
        // A failed finish can leave either the old or new cookie in place.
        // Let the server confirm it before restoring either transport.
        sessionAuth.verify('renewal-recovery').then(value => {
          if (!value || !state.authenticated) return;
          resumeSessionCreationRequests(); resumeSessionOperations();
          connectActivity();
          if (state.selected && !state.connected && !state.loading && !state.stopped) reconnect();
        }).catch(error => notice(error.message, 'connection'));
      }
    },
    onOptions: options => {
      $('auth-password').hidden = !options.password;
      $('auth-manage').textContent = options.password ? 'Manage sign-in' : 'Manage passkeys';
      updateAuthDescription();
    },
    onBeforeRotate: () => {
      if (logoutPending) throw new Error('Log out is in progress. Wait before signing in again.');
      if (!state.authenticated) return;
      captureAuthPosition(); saveDraft();
      authRotationPending = true;
      renderLogout();
      state.authGeneration++; state.generation++; resetInventory();
      resetTelegramBots();
      pauseSessionCreationRequests(); pauseSessionOperations();
      closeSocket(); closeActivity(); disableQuestions();
      connection('reconnecting', 'Renewing sign-in… Your running work continues.'); updateControls();
    },
    onStatus: text => { $('auth-status').textContent = text; },
    onWarning: warning => { if (warning) flushRecoveryDrafts(); },
    onAuthenticated: async (authenticatedSession) => {
      loginTrust.reset();
      authRotationPending = false;
      if (logoutPending && logoutPending.sessionID !== authenticatedSession?.session_id) {
        logoutPending = null;
        logoutError('Your sign-in changed while logging out. Log out again to end the current sign-in.');
      }
      const previouslyAuthenticated = state.authenticated;
      const authEpoch = state.authGeneration + 1;
      draftRecovery?.identityChanged();
      captureAuthPosition();
      state.authGeneration++; state.generation++;
      resetTelegramBots();
      pauseSessionCreationRequests(); pauseSessionOperations();
      resetInventory();
      closeSocket(); closeActivity();
      state.authenticated = true; state.stopped = false;
      renderLogout();
      recoverSessionOperations(authenticatedSession?.owner_id || '');
      resumeSessionCreationRequests();
      $('auth').hidden = true;
      // selectSession performs a fresh native bootstrap with the new login.
      // Preserve a still-authorized draft during early renewal only.
      if (state.selected) saveDraft();
      state.selected = null;
      await refreshSessions();
      if (authEpoch !== state.authGeneration || !state.authenticated) return;
      resumeSessionOperations();
      if (previouslyAuthenticated) syncRecoveryDrafts();
      else if (draftRecovery?.enabled()) {
        const recovered = await draftRecovery.recover();
        if (authEpoch !== state.authGeneration || !state.authenticated) return;
        recoveredDrafts = recovered; $('draft-recovery-offer').hidden = !recoveredDrafts;
      }
    },
    onVerified: (_session, { reason }) => {
      if (reason !== 'foreground' || !state.authenticated) return;
      refreshSessions(); connectActivity(); resumeSessionOperations();
      if (state.selected && !state.connected && !state.loading && !state.stopped && (!state.socket || state.socket.readyState === 3)) reconnect();
    },
  });
  async function draftRequest(path, options = {}) {
    const authGeneration = state.authGeneration;
    const response = await fetch(path, { ...options, credentials: 'same-origin', cache: 'no-store', headers: { ...(options.headers || {}), 'X-CSRF-Token': window.CodexSessionAuth.cookie('__Host-telegramgw-csrf') } });
    if (!response.ok) {
      if (response.status === 401 && authGeneration === state.authGeneration) expire();
      const error = new Error('Encrypted draft request failed (' + response.status + ').'); error.status = response.status; throw error;
    }
    return response.status === 204 ? null : response.json();
  }
  function flushRecoveryDrafts() {
    clearTimeout(draftSaveTimer); draftSaveTimer = null;
    if (!draftDirty || !draftRecovery?.enabled() || !state.authenticated || recoveredDrafts) return;
    draftDirty = false;
    draftRecovery.update(Object.fromEntries([...state.drafts].filter(([id]) => !uncertainDrafts.has(id))));
  }
  function syncRecoveryDrafts(immediate = false) {
    if (!draftRecovery?.enabled() || !state.authenticated || recoveredDrafts) return;
    draftDirty = true; clearTimeout(draftSaveTimer);
    if (immediate) flushRecoveryDrafts();
    else draftSaveTimer = setTimeout(flushRecoveryDrafts, 500);
  }
  draftRecovery = window.CodexDraftRecovery.create({ request: draftRequest, getIdentity: () => sessionAuth.snapshot()?.owner_id, onStatus: text => { $('draft-recovery-status').textContent = text; } });
  $('draft-recovery-enabled').checked = draftRecovery.enabled();
  $('draft-recovery-enabled').addEventListener('change', async event => {
    await draftRecovery.setEnabled(event.target.checked);
    if (!event.target.checked) { clearTimeout(draftSaveTimer); draftSaveTimer = null; draftDirty = false; recoveredDrafts = null; $('draft-recovery-offer').hidden = true; }
    else syncRecoveryDrafts(true);
  });
  $('restore-drafts').addEventListener('click', () => {
    if (!state.authenticated || !recoveredDrafts) return;
    const saved = recoveredDrafts; recoveredDrafts = null; $('draft-recovery-offer').hidden = true;
    const allowed = new Set(state.sessions.map(session => session.session_id));
    for (const [id, text] of Object.entries(saved)) if (allowed.has(id) && !uncertainDrafts.has(id) && !state.drafts.get(id)) state.drafts.set(id, text);
    if (state.selected && !$('prompt').value) { $('prompt').value = state.drafts.get(state.selected.session_id) || ''; resizePrompt(); updateControls(); }
    draftRecovery.consume();
  });
  $('discard-drafts').addEventListener('click', () => { recoveredDrafts = null; $('draft-recovery-offer').hidden = true; draftRecovery.clear(); });
  $('auth-login').addEventListener('click', () => {
    const preferences = loginTrust.preferences();
    if (preferences) sessionAuth.login('passkey', preferences).catch(() => {});
  });
  $('auth-password').addEventListener('click', () => sessionAuth.login('password').catch(() => {}));
  sessionAuth.verify('initial').catch(error => { lockAuthentication('unavailable'); $('auth-status').textContent = error.message; });
})();
