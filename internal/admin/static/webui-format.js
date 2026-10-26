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
  // Neither terminal escapes nor control characters are meaningful to a browser transcript.
  const clean = value => String(value ?? '').replace(/\x1b\][^\x07]*(?:\x07|\x1b\\)/g, '').replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '').replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '');
  // Do not turn an invalid raw target into a different, valid worker path when
  // removing terminal controls from the surrounding message.
  function cleanMarkdown(value) {
    const raw = String(value ?? '');
    // Preserve offsets and mark entire terminal escapes before clean() can
    // remove them. Invalid controls around destination padding stay guarded too.
    const guard = value => '\ufffd'.repeat(value.length);
    const guarded = raw.replace(/\x1b\][^\x07]*(?:\x07|\x1b\\)/g, guard).replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, guard).replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '\ufffd');
    // The guard also accepts malformed destination whitespace. Keep padding
    // out of the generic branch to avoid ambiguous sentinel partitions.
    const references = /\[[^\]\n]+\]\((?:[ \t\ufffd]*<[^<>\n]+>[ \t\ufffd]*|(?:\\.|[^\n\\()]|\((?:\\.|[^\n\\()])*\))+)\)/g;
    let result = '', cursor = 0;
    for (const match of guarded.matchAll(references)) {
      result += raw.slice(cursor, match.index) + match[0];
      cursor = match.index + match[0].length;
    }
    return clean(result + raw.slice(cursor));
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
  function inline(parent, value, depth = 0) {
    const text = cleanMarkdown(value);
    if (depth > 8) { parent.append(document.createTextNode(text)); return; }
    const pattern = /(`+)([^`\n]+?)\1|\[([^\]\n]+)\]\([ \t]*(<[^<>\n]+>|(?:\\.|[^\s\\()]|\((?:\\.|[^\s\\()])*\))+)[ \t]*\)|\*\*([^*\n]+)\*\*|__([^_\n]+)__|~~([^~\n]+)~~|\*([^*\n]+)\*|_([^_\n]+)_|https?:\/\/[^\s<>]+/g;
    let cursor = 0;
    for (const match of text.matchAll(pattern)) {
      parent.append(document.createTextNode(text.slice(cursor, match.index)));
      let child;
      if (match[1]) child = node('code', '', match[2]);
      else if (match[3]) {
        const target = match[4].replace(/^<|>$/g, '').replace(/\\([\\ ()])/g, '$1');
        const href = safeLink(target), local = !href && fileLink(target);
        child = node(href || local ? 'a' : 'span', local ? 'file-link' : '', match[3]);
        if (href) { child.href = href; child.target = '_blank'; child.rel = 'noopener noreferrer'; }
        else if (local) {
          child.href = '#file'; child.dataset.filePath = target; child.title = target;
          child.setAttribute('aria-haspopup', 'dialog'); child.setAttribute('aria-controls', 'file-dialog');
          const icon = node('span', 'file-link-icon', '↧'); icon.setAttribute('aria-hidden', 'true'); child.append(icon);
        } else child.title = target;
      } else if (match[5] || match[6]) { child = node('strong'); inline(child, match[5] || match[6], depth + 1); }
      else if (match[7]) { child = node('s'); inline(child, match[7], depth + 1); }
      else if (match[8] || match[9]) { child = node('em'); inline(child, match[8] || match[9], depth + 1); }
      else { child = node('a', '', match[0]); child.href = safeLink(match[0]); child.target = '_blank'; child.rel = 'noopener noreferrer'; }
      parent.append(child);
      cursor = match.index + match[0].length;
    }
    parent.append(document.createTextNode(text.slice(cursor)));
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
  function markdown(value, depth = 0) {
    const root = document.createDocumentFragment();
    if (depth > 12) { root.append(node('p', '', clean(value))); return root; }
    const lines = cleanMarkdown(value).split('\n');
    for (let i = 0; i < lines.length;) {
      const line = lines[i];
      if (!line.trim()) { i++; continue; }
      const fence = line.match(/^\s*(`{3,}|~{3,})([^\s]*)\s*$/);
      if (fence) {
        const body = [];
        i++;
        const closing = new RegExp('^\\s*' + fence[1][0] + '{' + fence[1].length + ',}\\s*$');
        while (i < lines.length && !closing.test(lines[i])) body.push(lines[i++]);
        const complete = i < lines.length;
        if (complete) i++;
        const source = body.join('\n');
        root.append(complete && /^mermaid$/i.test(fence[2]) && window.CodexDiagrams ? window.CodexDiagrams.create(source) : code(source, fence[2]));
        continue;
      }
      const heading = line.match(/^(#{1,6})\s+(.+)$/);
      if (heading) { const h = node('h' + Math.min(6, heading[1].length + 1)); inline(h, heading[1] + ' ' + heading[2]); root.append(h); i++; continue; }
      if (/^\s*(?:---+|\*\*\*+|___+)\s*$/.test(line)) { root.append(node('hr')); i++; continue; }
      if (/^\s*>/.test(line)) {
        const quote = node('blockquote'); const body = [];
        while (i < lines.length && /^\s*>/.test(lines[i])) body.push(lines[i++].replace(/^\s*> ?/, ''));
        quote.append(markdown(body.join('\n'), depth + 1)); root.append(quote); continue;
      }
      if (line.includes('|') && i + 1 < lines.length && /^\s*\|?\s*:?-{3,}:?\s*(?:\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(lines[i + 1])) {
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
  return { node, clean, inline, markdown, code, toolCode };
})();
