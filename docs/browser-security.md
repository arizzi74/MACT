# Browser renderer security verification

## Markdown (MACT-01)

`internal/admin/static/webui-format.js` renders message content with DOM nodes and
`textContent`. A shared work budget covers speculative link and inline delimiter
scans, recursive formatting and DOM construction. Inputs over 65,536 UTF-16 code
units are rejected before Markdown cleaning, line splitting or parsing. Budget
exhaustion discards the partial fragment and returns the complete cleaned source
in one inert paragraph. Public inline formatting has the same fallback. Terminal
escape cleaning, fenced-code recognition and table-separator scanning also avoid
repeated scans of the remaining input. No event-loop timeout is used as a parser
limit.

This intentionally changes presentation for very large or dense messages: their
Markdown source remains readable without formatting or actionable links. Normal
Markdown, restricted URL schemes, guarded worker paths, HTML safety, diagram
configuration restrictions and CSP are preserved.

`node scripts/test-webui-markdown.cjs` runs Chromium against the production
formatter and application. It checks 8/16/32/64/128 KiB runs of unmatched brackets,
emphasis delimiters, backticks, invalid fence prefixes, table padding, escapes,
unfinished destinations, OSC controls, valid links and HTML/XSS strings. It
asserts complete safe text, bounded DOM size, median rendering under a conservative
one-second ceiling and bounded growth. Real history and transport updates exercise
assistant messages, public reasoning summaries and expanded plans; the composer
must still submit a new input. Existing file-link tests separately cover padded
and escaped destinations, embedded controls and unsupported schemes.

## Shipped dependencies (MACT-04)

Verification on 29 October 2026 used the current npm registry audit and the
maintainers' advisories. The original npm audit reported DOMPurify 3.4.16 and
KaTeX 0.16.47. Upgrading to Mermaid 12.2.0 brings compatible KaTeX 0.18.10;
DOMPurify is pinned in the lockfile at 3.4.17.

| Advisory | Maintainer's affected versions / patch | Disposition |
| --- | --- | --- |
| [DOMPurify template/shadow-root bypass, GHSA-cg2g-xh64-v4q7](https://github.com/cure53/DOMPurify/security/advisories/GHSA-cg2g-xh64-v4q7) | 3.4.4–3.4.16 / 3.4.17 | Patched in shipped code. |
| [DOMPurify detached subtree hooks, GHSA-mrv2-3pqr-j5jr](https://github.com/cure53/DOMPurify/security/advisories/GHSA-mrv2-3pqr-j5jr) | ≤3.4.16 / 3.4.17 | Patched in shipped code. |
| [DOMPurify XHTML/SMIL guard, GHSA-c6gr-qvv4-5v3f](https://github.com/cure53/DOMPurify/security/advisories/GHSA-c6gr-qvv4-5v3f) | ≤3.4.16 / 3.4.17 | Patched in shipped code. |
| [KaTeX inherited trust options, GHSA-238p-pmpm-9mq7](https://github.com/KaTeX/KaTeX/security/advisories/GHSA-238p-pmpm-9mq7) | ≥0.11.0, <0.18.2 / 0.18.2 | Shipped 0.18.10 is patched. |
| [js-yaml ordered-map CPU use, GHSA-5p4m-2wfm-xmqj](https://github.com/nodeca/js-yaml/security/advisories/GHSA-5p4m-2wfm-xmqj) | 4.x <4.3.1 / 4.3.1 | Mermaid's prebundled 4.3.0 replaced with 4.3.2. |
| [js-yaml empty merge CPU use, GHSA-2883-xcg3-v3hh](https://github.com/nodeca/js-yaml/security/advisories/GHSA-2883-xcg3-v3hh) | 4.x <4.3.2 / 4.3.2 | Mermaid's prebundled 4.3.0 replaced with 4.3.2. |

These advisories do not establish an exploit in MACT. Several require options,
DOM inputs, prototype pollution or schemas MACT does not expose. MACT retains
Mermaid's strict mode, rejects directives/frontmatter, uses an opaque sandbox
without network or parent-DOM access, sanitizes the serialized SVG again, and
displays it only as an image. The patches remove the affected code regardless of
those additional boundaries.

Mermaid's published distribution already contains js-yaml 4.3.0, which an npm
override cannot replace. The build verifies the exact dedicated chunk SHA-256
`2cb4e5f8d5fd99daccb0135fda6a5583c91f39efb0bb845af9d968ed427efcb2`, then replaces
its `JSON_SCHEMA` and `load` exports with the pinned compatible 4.3.2 package.
It fails if the chunk changes or the replacement is not applied exactly once.
A future Mermaid upgrade requires reviewing this replacement. The browser suite
renders real flowchart YAML node metadata through that path, as well as testing
the production sandbox, CSP and directive rejection.

## Reproduce and enforce

Use Node.js 22.12 or later (Mermaid 12.2.0's minimum):

```sh
npm ci --ignore-scripts
npm run build:mermaid -- --check
npm run security:audit
npx playwright install chromium
npm run test:browser
```

`build-mermaid.mjs` generates the runtime, licenses and exact package inventory
from esbuild's contributing inputs and Mermaid's published source maps. The
inventory records packages precompiled into Mermaid even when they do not appear
in `package-lock.json`. The replaced YAML chunk is excluded because none of its
old implementation survives; the installed patched implementation is inventoried
instead. Prebundled notices are required for every remaining such dependency.

`security:audit` verifies those committed artifacts, audits all lockfile
dependencies including development packages, and separately audits all 68 exact
shipped versions through a temporary npm lockfile. npm aliases preserve duplicate
versions; their real package names are used for advisory lookup. It prints package
counts and advisory identities, never source content or credentials. Service
failure or malformed output fails the command. Any advisory finding, including
low severity, fails; there are no allowlists. A new finding needs remediation or
an explicit, reviewed, package-level reachability disposition before changing
that policy.

The completed scans reported **0 findings in the full lockfile** and **0 findings
in the 68-version shipped inventory**. A live negative-control scan using a
synthetic lockfile for DOMPurify 3.4.16 correctly returned nonzero and the three
DOMPurify advisories. Before replacing prebundled js-yaml, the shipped-inventory
scan also correctly failed for the two js-yaml advisories while the ordinary
lockfile audit reported zero. Advisory results are time-sensitive; CI repeats
the checks against the current registry.
