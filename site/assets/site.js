'use strict';
(() => {
  const timers = new WeakMap();
  for (const button of document.querySelectorAll('[data-copy]')) {
    button.addEventListener('click', async () => {
      const code = document.getElementById(button.dataset.copy);
      const status = document.getElementById('copy-status');
      if (!code) return;
      try {
        await navigator.clipboard.writeText(code.textContent.trim());
        button.textContent = 'Copied ✓';
        button.dataset.copied = 'true';
        const service = { 'gateway-command': 'Gateway', 'worker-command': 'Codex worker', 'claude-worker-command': 'Claude Code worker' }[button.dataset.copy] || 'Worker';
        status.textContent = service + ' install command copied.';
        clearTimeout(timers.get(button));
        timers.set(button, setTimeout(() => { button.textContent = 'Copy ⧉'; delete button.dataset.copied; }, 2500));
      } catch (_) {
        const selection = window.getSelection();
        const range = document.createRange();
        range.selectNodeContents(code);
        selection.removeAllRanges(); selection.addRange(range);
        status.textContent = 'Automatic copying is unavailable. The command is selected; copy it using your browser.';
      }
    });
  }
})();
