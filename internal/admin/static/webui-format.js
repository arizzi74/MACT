/* Independent DOM renderer informed by OpenAI Codex's Apache-2.0 TUI.
 * Upstream revision and source mapping: docs/webui.md. No HTML from messages is executed. */
'use strict';
window.CodexFormat = (() => {
  const node = (tag, cls, text) => {
    const value = document.createElement(tag);
    if (cls) value.className = cls;
    if (text !== undefined) value.textContent = String(text);
    return value;
  };
  // Consume terminal escapes once. An unterminated OSC must not cause another
  // scan of the remaining string for every later ESC ] in streamed output.
  function terminalText(value, guard = false) {
    const text = String(value ?? ''), parts = [];
    let cursor = 0, noOSCEnd = false;
    for (let i = 0; i < text.length; i++) {
      const char = text.charCodeAt(i);
      if (!(char < 32 && char !== 9 && char !== 10 || char === 127)) continue;
      let end = i + 1;
      if (char === 27 && text[i + 1] === ']' && !noOSCEnd) {
        let close = i + 2;
        while (close < text.length && text[close] !== '\x07' && !(text[close] === '\x1b' && text[close + 1] === '\\')) close++;
        if (close < text.length) end = close + (text[close] === '\x07' ? 1 : 2);
        else noOSCEnd = true;
      } else if (char === 27 && text[i + 1] === '[') {
        let close = i + 2;
        while (close < text.length && /[0-?]/.test(text[close])) close++;
        while (close < text.length && /[ -/]/.test(text[close])) close++;
        if (close < text.length && /[@-~]/.test(text[close])) end = close + 1;
      }
      parts.push(text.slice(cursor, i), guard ? '\ufffd'.repeat(end - i) : '');
      cursor = end; i = end - 1;
    }
    parts.push(text.slice(cursor));
    return parts.join('');
  }
  // Neither terminal escapes nor control characters are meaningful to a browser transcript.
  const clean = value => terminalText(value);
  const markdownLimit = 64 * 1024, markdownNodeLimit = 4000;
  const exhausted = Symbol('Markdown work limit');
  const budgetFor = text => ({ work: 4096 + text.length * 32, nodes: markdownNodeLimit });
  function spend(budget, work = 1, nodes = 0) {
    budget.work -= work; budget.nodes -= nodes;
    if (budget.work < 0 || budget.nodes < 0) throw exhausted;
  }
  // All speculative scans share the message budget. A failed opening delimiter
  // cannot repeatedly scan an arbitrarily large suffix on the main thread.
  function linkAt(text, start, budget, guarding = false) {
    let i = start + 1;
    while (i < text.length && text[i] !== ']' && text[i] !== '\n') { spend(budget); i++; }
    if (i === start + 1 || text[i] !== ']' || text[i + 1] !== '(') return null;
    const labelEnd = i; i += 2;
    const padding = char => char === ' ' || char === '\t' || guarding && char === '\ufffd';
    while (i < text.length && padding(text[i])) { spend(budget); i++; }
    const targetStart = i;
    if (text[i] === '<') {
      i++;
      while (i < text.length && !['<', '>', '\n'].includes(text[i])) { spend(budget); i++; }
      if (i === targetStart + 1 || text[i] !== '>') return null;
      const targetEnd = ++i;
      while (i < text.length && padding(text[i])) { spend(budget); i++; }
      return text[i] === ')' ? { end: i + 1, label: text.slice(start + 1, labelEnd), target: text.slice(targetStart, targetEnd) } : null;
    }
    let nesting = 0, targetEnd = i, trailing = false;
    for (; i < text.length && text[i] !== '\n'; i++) {
      spend(budget);
      const char = text[i];
      if (char === ')' && !nesting) return i > targetStart ? { end: i + 1, label: text.slice(start + 1, labelEnd), target: text.slice(targetStart, targetEnd) } : null;
      if (!guarding && /\s/.test(char)) {
        if (char !== ' ' && char !== '\t') return null;
        trailing = true; continue;
      }
      if (trailing) return null;
      if (char === '\\') {
        if (i + 1 >= text.length || text[i + 1] === '\n') return null;
        spend(budget); i++;
      } else if (char === '(') { if (++nesting > 1) return null; }
      else if (char === ')') nesting--;
      targetEnd = i + 1;
    }
    return null;
  }
  // Do not turn an invalid raw target into a different, valid worker path when
  // removing terminal controls from the surrounding message.
  function cleanMarkdown(value, budget) {
    const raw = String(value ?? '');
    // Preserve offsets and mark entire terminal escapes before clean() can
    // remove them. Invalid controls around destination padding stay guarded too.
    const guarded = terminalText(raw, true), parts = [];
    let cursor = 0;
    for (let i = 0; i < guarded.length; i++) {
      spend(budget);
      if (guarded[i] === '\\') { i++; continue; }
      if (guarded[i] !== '[') continue;
      const match = linkAt(guarded, i, budget, true);
      if (!match) continue;
      parts.push(raw.slice(cursor, i), guarded.slice(i, match.end));
      cursor = match.end; i = match.end - 1;
    }
    parts.push(raw.slice(cursor));
    return clean(parts.join(''));
  }
  function safeLink(value) {
    if (value.includes('\ufffd') || /[\x00-\x1f\x7f]/.test(value)) return null;
    try {
      const parsed = new URL(value);
      if (['https:', 'http:', 'mailto:'].includes(parsed.protocol)) return parsed.href;
    } catch (_) { /* Worker files are handled separately from browser URLs. */ }
    return null;
  }
  function fileLink(value) {
    if (!value || /^[#?]/.test(value) || value.startsWith('//') || /[\x00-\x1f\x7f\\\ufffd]/.test(value)) return false;
    if (/^file:/i.test(value)) {
      if (!/^file:\/\/(?:localhost)?\//i.test(value)) return false;
      try { const parsed = new URL(value); return parsed.protocol === 'file:' && (!parsed.hostname || parsed.hostname === 'localhost') && !!parsed.pathname; }
      catch (_) { return false; }
    }
    // Bare filenames with an extension may also carry an editor line/column suffix.
    if (/^[a-z][a-z0-9+.-]*:/i.test(value)) return /^[^/:]+\.[^/:]+(?::\d+){1,2}$/.test(value);
    return true;
  }
  function renderInline(parent, value, depth, budget) {
    spend(budget, 0, 1);
    const text = cleanMarkdown(value, budget);
    if (depth > 8) { parent.append(document.createTextNode(text)); return; }
    let cursor = 0;
    for (let i = 0; i < text.length; i++) {
      spend(budget);
      if (text[i] === '\\') { i++; continue; }
      let child, end = i, content, tag;
      const link = text[i] === '[' ? linkAt(text, i, budget) : null;
      if (link) {
        const target = link.target.replace(/^<|>$/g, '').replace(/\\([\\ ()])/g, '$1');
        const href = safeLink(target), local = !href && fileLink(target);
        child = node(href || local ? 'a' : 'span', local ? 'file-link' : '', link.label);
        if (href) { child.href = href; child.target = '_blank'; child.rel = 'noopener noreferrer'; }
        else if (local) {
          child.href = '#file'; child.dataset.filePath = target; child.title = target;
          child.setAttribute('aria-haspopup', 'dialog'); child.setAttribute('aria-controls', 'file-dialog');
          const icon = node('span', 'file-link-icon', '↧'); icon.setAttribute('aria-hidden', 'true'); child.append(icon);
        } else child.title = target;
        end = link.end;
      } else if ('`*_~'.includes(text[i])) {
        const char = text[i]; let width = char !== '`' && text[i + 1] === char ? 2 : 1;
        if (char === '`') { while (text[i + width] === char) { spend(budget); width++; } }
        if (char === '~' && width === 1) continue;
        let close = i + width;
        while (close < text.length && text[close] !== char && text[close] !== '\n') { spend(budget); close++; }
        if (close > i + width && text.slice(close, close + width) === char.repeat(width)) {
          content = text.slice(i + width, close); end = close + width;
          tag = char === '`' ? 'code' : char === '~' ? 's' : width === 2 ? 'strong' : 'em';
          child = node(tag);
          if (tag === 'code') child.textContent = content;
          else renderInline(child, content, depth + 1, budget);
        }
      } else if (text.startsWith('https://', i) || text.startsWith('http://', i)) {
        end = i;
        while (end < text.length && !/[\s<>]/.test(text[end])) { spend(budget); end++; }
        content = text.slice(i, end);
        const href = safeLink(content);
        if (href) { child = node('a', '', content); child.href = href; child.target = '_blank'; child.rel = 'noopener noreferrer'; }
      }
      if (!child) continue;
      spend(budget, 0, 3);
      parent.append(document.createTextNode(text.slice(cursor, i)), child);
      cursor = end; i = end - 1;
    }
    parent.append(document.createTextNode(text.slice(cursor)));
  }
  function inline(parent, value) {
    const text = String(value ?? ''), fragment = document.createDocumentFragment();
    try {
      if (text.length > markdownLimit) throw exhausted;
      renderInline(fragment, text, 0, budgetFor(text));
      parent.append(fragment);
    } catch (error) {
      if (error !== exhausted) throw error;
      parent.append(document.createTextNode(clean(text)));
    }
  }
  const highlightLimit = 100000, tokenLimit = 4000;
  const aliases = { js: 'javascript', node: 'javascript', nodejs: 'javascript', ts: 'typescript', py: 'python', sh: 'shell', bash: 'shell', zsh: 'shell', command: 'shell', golang: 'go', rs: 'rust', yml: 'yaml', patch: 'diff' };
  const languages = new Set(['javascript', 'typescript', 'json', 'go', 'rust', 'python', 'shell', 'sql', 'c', 'cpp', 'java', 'css', 'yaml']);
  const keywords = new Set(('const let var function return if else elif for while class import from export async await true false null None True False def fn pub use struct func package type nil SELECT FROM WHERE select from where as in is not and or pass raise try except finally with yield lambda break continue switch case default interface map range chan defer go select fallthrough extends new throw catch this void static public private protected int string bool typeof instanceof do done then fi esac until function local export readonly unset echo printf cd pwd exit exec eval source set test command').split(' '));
  const shellCommand = /^(?:(?:\$|>)\s+)?(?:(?:[A-Za-z_][\w]*=(?:[^\s"']+|"[^"\n]*"|'[^'\n]*')|sudo|env|command|exec)\s+)*(?:\.?\.?\/[^\s]+|\/(?:usr|bin|opt|home|tmp)\/[^\s]+|bash|sh|zsh|fish|python(?:\d+(?:\.\d+)*)?|node(?:js)?|npm|npx|pnpm|yarn|git|go|cargo|rustc|make|cmake|docker|kubectl|curl|wget|jq|rg|grep|sed|awk|find|ls|cat|head|tail|wc|sort|uniq|printf|echo|pwd|cd|mkdir|cp|mv|rm|chmod|touch|test|true|false)(?:\s|$)/;
  const normalizeLanguage = value => {
    const name = String(value || '').trim().toLowerCase();
    return Object.hasOwn(aliases, name) ? aliases[name] : name;
  };
  function inferLanguage(text, output = false) {
    const source = text.trim();
    if (!source || text.length > highlightLimit) return '';
    if (/^[{[]/.test(source)) {
      try { const value = JSON.parse(source); if (value && typeof value === 'object') return 'json'; }
      catch (_) { /* Incomplete tool output remains readable plain text. */ }
    }
    const shebang = source.match(/^#![^\n]*\b(python[\d.]*|node(?:js)?|bash|sh|zsh)\b/);
    if (shebang) return /^python/.test(shebang[1]) ? 'python' : /^node/.test(shebang[1]) ? 'javascript' : 'shell';
    if (!output && shellCommand.test(source)) return 'shell';
    if (/^\s*(?:package\s+[A-Za-z_]\w*\s*$|func\s+(?:\([^\n)]*\)\s*)?[A-Za-z_]\w*\s*\()/m.test(source)) return 'go';
    if (/^\s*(?:(?:const|let|var)\s+[\w$[{]|(?:async\s+)?function\s*[\w$]*\s*\(|(?:import\s+.+\s+from\s+|export\s+(?:default|const|function|class)\b)|console\.(?:log|error)\s*\()/m.test(source)) return 'javascript';
    if (/^\s*(?:(?:async\s+)?def\s+\w+\s*\(|class\s+\w+[(:]|from\s+[\w.]+\s+import\s|import\s+[\w.]+(?:\s|$)|if\s+__name__\s*==|print\s*\()/m.test(source)) return 'python';
    return '';
  }
  // This deliberately small lexer only creates text nodes and spans. Scanning
  // strings ourselves avoids repeated regex backtracking on unclosed quotes.
  function syntaxTokens(text, language, base, tokens) {
    let i = 0, expression = true;
    const add = (start, end, kind) => {
      tokens.push({ start: base + start, end: base + end, kind, language });
      return tokens.length <= tokenLimit;
    };
    while (i < text.length) {
      const start = i, char = text[i], next = text[i + 1];
      const hashComment = char === '#' && (['python', 'yaml'].includes(language) || language === 'shell' && (!i || /[\s;|&()]/.test(text[i - 1])));
      const slashComment = char === '/' && next === '/' && ['javascript', 'typescript', 'go', 'rust', 'c', 'cpp', 'java'].includes(language);
      const sqlComment = language === 'sql' && char === '-' && next === '-';
      if (hashComment || slashComment || sqlComment) {
        const end = text.indexOf('\n', i); i = end < 0 ? text.length : end;
        if (!add(start, i, 'comment')) return false;
      } else if (char === '/' && next === '*' && ['javascript', 'typescript', 'go', 'rust', 'c', 'cpp', 'java', 'css'].includes(language)) {
        const end = text.indexOf('*/', i + 2); i = end < 0 ? text.length : end + 2;
        if (!add(start, i, 'comment')) return false;
      } else if (char === '"' || char === "'" && language !== 'json' || char === '`' && ['javascript', 'typescript', 'go', 'shell'].includes(language)) {
        const triple = language === 'python' && text.slice(i, i + 3) === char.repeat(3);
        const width = triple ? 3 : 1; i += width;
        while (i < text.length) {
          if (text[i] === '\\' && !(language === 'shell' && char === "'") && !(language === 'go' && char === '`')) { i = Math.min(text.length, i + 2); continue; }
          if (text.slice(i, i + width) === char.repeat(width)) { i += width; break; }
          if (text[i] === '\n' && !triple && char !== '`' && language !== 'shell') break;
          i++;
        }
        if (!add(start, i, 'string')) return false;
        expression = false;
      } else if (char === '/' && expression && ['javascript', 'typescript'].includes(language)) {
        // Regex contents may contain quotes or comment markers. Consume them
        // once, including incomplete streamed literals, rather than re-lexing.
        let characterClass = false; i++;
        while (i < text.length && text[i] !== '\n') {
          if (text[i] === '\\') { i = Math.min(text.length, i + 2); continue; }
          if (text[i] === '[') characterClass = true;
          else if (text[i] === ']') characterClass = false;
          else if (text[i] === '/' && !characterClass) { i++; while (i < text.length && /[A-Za-z]/.test(text[i])) i++; break; }
          i++;
        }
        if (!add(start, i, 'string')) return false;
        expression = false;
      } else if (/[A-Za-z_$]/.test(char)) {
        i++;
        while (i < text.length && /[\w$]/.test(text[i])) i++;
        const word = text.slice(start, i);
        if ((keywords.has(word) || language === 'shell' && /^(?:bash|sh|zsh|python[\d.]*|node(?:js)?|git|go|rg|grep|sed|awk|cat|jq)$/.test(word)) && !add(start, i, 'keyword')) return false;
        expression = /^(?:return|throw|case|delete|void|typeof|instanceof|in|of|await|yield|new)$/.test(word);
      } else if (/\d/.test(char) && (!i || !/[\w$]/.test(text[i - 1]))) {
        i++;
        while (i < text.length && /[\dA-Fa-f_xXob.]/.test(text[i])) i++;
        if (/[eE]/.test(text[i] || '') && /[+\-\d]/.test(text[i + 1] || '')) {
          i++; if (/[+-]/.test(text[i])) i++;
          while (i < text.length && /\d/.test(text[i])) i++;
        }
        if (!add(start, i, 'number')) return false;
        expression = false;
      } else {
        if (!/\s/.test(char)) expression = /[([{=,:;!?&|+*%<>-]/.test(char);
        i++;
      }
    }
    return true;
  }
  function heredocLanguage(header, delimiter, source) {
    // The command nearest the redirect also works inside bash -lc "…".
    const commands = Array.from(header.matchAll(/\b(python(?:\d+(?:\.\d+)*)?|node(?:js)?|bash|sh|zsh)\b/g));
    const command = commands.length ? commands[commands.length - 1][1] : '';
    if (/^python/.test(command)) return 'python';
    if (/^node/.test(command)) return 'javascript';
    const extension = header.match(/\.(py|js|ts|go|json)\b/);
    if (extension) return normalizeLanguage(extension[1]);
    const marker = { PY: 'python', PYTHON: 'python', JS: 'javascript', JAVASCRIPT: 'javascript', NODE: 'javascript', TS: 'typescript', GO: 'go', JSON: 'json', SH: 'shell', BASH: 'shell' }[delimiter.toUpperCase()];
    if (marker) return marker;
    if (command) return 'shell';
    return inferLanguage(source);
  }
  function shellTokens(text, base, tokens, depth = 0) {
    if (depth > 3) return true;
    const lines = text.split('\n'), offsets = []; let offset = 0, cursor = 0;
    for (const line of lines) { offsets.push(offset); offset += line.length + 1; }
    for (let lineIndex = 0; lineIndex < lines.length; lineIndex++) {
      const header = lines[lineIndex], redirects = [];
      const pattern = /<<(-?)[ \t]*(?:'([^'\n]+)'|"([^"\n]+)"|([A-Za-z_]\w*))/g;
      for (const match of header.matchAll(pattern)) {
        if (header[match.index - 1] !== '<' && header[match.index + 2] !== '<') redirects.push({ stripTabs: !!match[1], delimiter: match[2] || match[3] || match[4], header: header.slice(0, match.index) });
      }
      if (!redirects.length || lineIndex + 1 >= lines.length) continue;
      const bodyStart = offsets[lineIndex + 1];
      if (!syntaxTokens(text.slice(cursor, bodyStart), 'shell', base + cursor, tokens)) return false;
      cursor = bodyStart;
      for (const redirect of redirects) {
        const start = cursor, wrapper = header.match(/\b(?:bash|sh|zsh)\s+(?:-[A-Za-z]*c[A-Za-z]*|--command)\s+(["'])/);
        let closing = lineIndex + 1;
        while (closing < lines.length) {
          const line = redirect.stripTabs ? lines[closing].replace(/^\t+/, '') : lines[closing];
          if (line === redirect.delimiter || wrapper && line === redirect.delimiter + wrapper[1]) break;
          closing++;
        }
        const end = closing < lines.length ? offsets[closing] : text.length;
        const body = text.slice(start, end), language = heredocLanguage(redirect.header, redirect.delimiter, body);
        if (language === 'shell' ? !shellTokens(body, base + start, tokens, depth + 1) : languages.has(language) && !syntaxTokens(body, language, base + start, tokens)) return false;
        // Delimiters and any wrapper quotes remain part of the shell source.
        cursor = end; lineIndex = closing;
        if (closing < lines.length) {
          const next = Math.min(text.length, offsets[closing] + lines[closing].length + 1);
          if (!syntaxTokens(text.slice(cursor, next), 'shell', base + cursor, tokens)) return false;
          cursor = next;
        }
      }
    }
    return syntaxTokens(text.slice(cursor), 'shell', base + cursor, tokens);
  }
  function highlight(content, text, language) {
    if (language === 'diff' && text.length <= highlightLimit) {
      const lines = text.match(/[^\n]*\n|[^\n]+$/g) || [];
      if (lines.length <= tokenLimit) {
        for (const line of lines) content.append(node('span', line.startsWith('+') ? 'diff-add' : line.startsWith('-') ? 'diff-delete' : /^(?:@@|diff |index )/.test(line) ? 'diff-meta' : '', line));
        return;
      }
    }
    if (!languages.has(language) || text.length > highlightLimit) { content.textContent = text; return; }
    const tokens = [];
    if (!(language === 'shell' ? shellTokens(text, 0, tokens) : syntaxTokens(text, language, 0, tokens))) { content.textContent = text; return; }
    let cursor = 0;
    for (const token of tokens) {
      const span = node('span', 'syntax-' + token.kind, text.slice(token.start, token.end)); span.dataset.language = token.language;
      content.append(document.createTextNode(text.slice(cursor, token.start)), span); cursor = token.end;
    }
    content.append(document.createTextNode(text.slice(cursor)));
  }
  function code(value, language = '') {
    const pre = node('pre');
    if (language) pre.append(node('span', 'code-label', clean(language).slice(0, 60)));
    const content = node('code');
    const text = clean(value);
    if (/^(diff|patch)$/i.test(language)) {
      for (const line of text.split('\n')) content.append(node('span', 'diff-line ' + (line.startsWith('+') ? 'diff-add' : line.startsWith('-') ? 'diff-delete' : /^(?:@@|diff |index )/.test(line) ? 'diff-meta' : ''), line));
    } else highlight(content, text, normalizeLanguage(language));
    pre.append(content);
    return pre;
  }
  function toolCode(value, languageHint = '') {
    const text = clean(value), hint = normalizeLanguage(languageHint);
    const language = hint === 'text' || hint === 'plain' ? '' : hint && hint !== 'output' ? hint : inferLanguage(text, hint === 'output');
    const pre = node('pre'), content = node('code');
    if (language) { pre.dataset.language = language; pre.append(node('span', 'code-label', language.slice(0, 60))); }
    highlight(content, text, language); pre.append(content);
    return pre;
  }
  const cells = text => text.trim().replace(/^\||\|$/g, '').split('|').map(value => value.trim());
  function fenceAt(line) {
    const text = line.trim(), char = text[0];
    if (char !== '`' && char !== '~') return null;
    let width = 0;
    while (text[width] === char) width++;
    if (width < 3 || /\s/.test(text.slice(width))) return null;
    return [text, text.slice(0, width), text.slice(width)];
  }
  function tableSeparator(line) {
    const columns = cells(line);
    return columns.length > 1 && columns.every(column => /^:?-{3,}:?$/.test(column));
  }
  function renderMarkdown(value, depth, budget) {
    const root = document.createDocumentFragment();
    if (depth > 12) { root.append(node('p', '', clean(value))); return root; }
    const lines = cleanMarkdown(value, budget).split('\n');
    const inline = (parent, text) => renderInline(parent, text, 0, budget);
    for (let i = 0; i < lines.length;) {
      const line = lines[i];
      spend(budget, line.length + 1, 1);
      if (!line.trim()) { i++; continue; }
      const fence = fenceAt(line);
      if (fence) {
        const body = [];
        i++;
        const closing = new RegExp('^\\s*' + fence[1][0] + '{' + fence[1].length + ',}\\s*$');
        while (i < lines.length && !closing.test(lines[i])) body.push(lines[i++]);
        const complete = i < lines.length;
        if (complete) i++;
        const source = body.join('\n');
        spend(budget, source.length, body.length);
        root.append(complete && /^mermaid$/i.test(fence[2]) && window.CodexDiagrams ? window.CodexDiagrams.create(source) : code(source, fence[2]));
        continue;
      }
      const heading = line.match(/^(#{1,6})\s+(.+)$/);
      if (heading) { const h = node('h' + Math.min(6, heading[1].length + 1)); inline(h, heading[1] + ' ' + heading[2]); root.append(h); i++; continue; }
      if (/^\s*(?:---+|\*\*\*+|___+)\s*$/.test(line)) { root.append(node('hr')); i++; continue; }
      if (/^\s*>/.test(line)) {
        const quote = node('blockquote'); const body = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) body.push(lines[i++].replace(/^\s*> ?/, ''));
        quote.append(renderMarkdown(body.join('\n'), depth + 1, budget)); root.append(quote); continue;
      }
      if (line.includes('|') && i + 1 < lines.length && tableSeparator(lines[i + 1])) {
        const labels = cells(line); const wrap = node('div', 'table-wrap'); const table = node('table'); const head = node('thead'); const row = node('tr');
        for (const label of labels) { const th = node('th'); th.scope = 'col'; inline(th, label); row.append(th); }
        head.append(row); table.append(head); const body = node('tbody'); i += 2;
        while (i < lines.length && lines[i].includes('|') && lines[i].trim()) {
          const tr = node('tr'); const values = cells(lines[i++]);
          labels.forEach((label, index) => { const td = node('td'); td.dataset.label = label; inline(td, values[index] || ''); tr.append(td); });
          body.append(tr);
        }
        table.append(body); wrap.append(table); root.append(wrap); continue;
      }
      const list = line.match(/^\s*([-+*]|\d+[.)])\s+(.+)$/);
      if (list) {
        const ordered = /^\d/.test(list[1]); const ul = node(ordered ? 'ol' : 'ul');
        if (ordered) ul.start = parseInt(list[1], 10);
        while (i < lines.length) {
          const item = lines[i].match(/^\s*([-+*]|\d+[.)])\s+(.+)$/);
          if (!item || /^\d/.test(item[1]) !== ordered) break;
          const li = node('li'); inline(li, item[2]); ul.append(li); i++;
        }
        root.append(ul); continue;
      }
      const para = [line]; i++;
      while (i < lines.length && lines[i].trim() && !/^(?:\s*[`~]{3,}|#{1,6}\s|\s*>|\s*[-+*]\s|\s*\d+[.)]\s)/.test(lines[i]) && !(lines[i].includes('|') && i + 1 < lines.length && /^[\s|:-]+$/.test(lines[i + 1]))) para.push(lines[i++]);
      const p = node('p'); inline(p, para.join('\n')); root.append(p);
    }
    return root;
  }
  function markdown(value) {
    const text = String(value ?? '');
    try {
      // Check before cleaning, splitting, matching delimiters or constructing
      // DOM nodes. Plain text retains the complete response without parser work.
      if (text.length > markdownLimit) throw exhausted;
      return renderMarkdown(text, 0, budgetFor(text));
    } catch (error) {
      if (error !== exhausted) throw error;
      const root = document.createDocumentFragment(), plain = node('p', 'markdown-plain', clean(text));
      root.append(plain); return root;
    }
  }
  return { node, clean, inline, markdown, code, toolCode };
})();
