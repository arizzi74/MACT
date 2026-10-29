# Worker credential and file boundaries

MACT assumes a single trusted operator. An authorized operator can run commands
on an enrolled worker through its coding agent, subject to the selected native
permission mode. The protections below reduce accidental credential disclosure;
they do not isolate the worker account from that operator or from code running
with the same filesystem privileges.

## Installation trust

The default one-line setup trusts the shell bootstrap delivered from the
canonical repository over HTTPS. The bootstrap checks a pinned v0.7.10 native
manager against embedded SHA-256 hashes. That manager verifies signed release
provenance and all target packages before installation. This does not
independently authenticate the bootstrap script itself.

Operators who want to authenticate the first executable separately can use the
[optional GitHub CLI procedure](installation.md#download-and-authenticate-the-installer)
with an independently trusted verifier. Subsequent native updates verify signed
release provenance without requiring GitHub CLI.

For maintainers: the pinned manager can install newer authenticated target
payloads, while setup wizard and CLI logic run from that pinned manager. When
wizard, CLI or security compatibility changes require a newer bootstrap
manager, independently verify its signed release manifest, then refresh
`bootstrap_version` and all four reviewed platform hashes together in
`scripts/install.sh` and update the bootstrap tests. The pin can remain valid
across multiple target releases.

## Outbound display text

Worker display text is redacted before it enters the durable gateway event
outbox. Startup/reconnect also sanitizes older queued events while retaining
their event IDs, sequence numbers and timestamps. Telegram and native Web UI
projections use the same default credential rules. Default rules cover MACT
worker tokens, `sk-` API keys, Telegram bot tokens, GitHub classic and fine-grained
PAT formats, AWS access-key IDs (including temporary IDs), Google API keys,
common Slack token formats and signed JWT-shaped strings. JWT matching is based
on the token's shape and does not require a `Bearer` label. These are format
checks; MACT does not contact providers to determine whether a match is live.

`redact_patterns` adds rules to these defaults. Worker-stored Claude API keys and
auth tokens are additionally matched literally, including previous values after
rotation. Common zero-width separators inside a matching string are ignored for
matching; original Unicode outside the redacted span is preserved.

Redaction is per display-text field. Secrets split across separate fields or
events, arbitrary encodings, altered formats and credentials without a known
shape or configured rule cannot be guaranteed to match. Identifiers, workspace
paths and other native routing inputs retain their exact values. Keep secrets
out of those values. This policy does not promise to erase private local native
history, command input or filesystem bytes.

## Intentionally opened files

Browser file open, preview and download use the same worker `file_open` path.
Before opening a file, that path excludes the worker's configuration source,
token, database and its state directory, default MACT state directories,
configured runtime-settings files (including custom locations), atomic settings
temporary files, `.claude` directories, `.claude.json` files/backups and the
native directory selected by `CLAUDE_CONFIG_DIR`. Relative native config paths
are resolved against the thread workspace. Configured aliases are checked
against canonical paths. Existing restrictions on other credential directories,
workspace containment, symlinks, nonregular files and multiple hard links remain.
Browser uploads cannot write into protected directories.

Ordinary project files remain available to the trusted operator. The existing
restriction on `.env` and `.env.*` files is retained; changing that product
boundary would require a separate decision. Intentionally opening or downloading
an allowed file sends its exact
bytes through the gateway to the browser. Raw file bytes bypass conversational
redaction, because changing them would corrupt the file. Review the file before
opening it and choose workspace roots appropriate to the operator's access.

Compatibility change: the entire configured worker state directory is now
private to raw file browsing and browser uploads. Keep worker state in a
dedicated directory, as the installer does. If a custom worker places its state
file directly in a project root, move state to a dedicated directory to retain
raw file access to that project. Existing filename and protected-directory
restrictions continue to apply.

## Claude endpoint transport

Custom Claude API endpoints must use HTTPS, with no URL credentials, query or
fragment. Assignment and every persisted-settings read enforce this policy;
previously saved HTTP values are rejected before a native Claude process can
receive the saved credential. Loopback HTTP proxies also require migration to
HTTPS; there is no implicit local exemption. An empty endpoint inherits the
normal native endpoint. For an existing HTTP configuration, change `base_url`
in the worker's private runtime-settings file to a trusted HTTPS endpoint (or
empty) before restarting the upgraded worker.

The MACT-owned credit-balance request accepts only its fixed HTTPS provider
endpoint and rejects every redirect before making a second request. Native
Claude inference networking, including response-redirect handling, is performed
by the installed Claude CLI. MACT validates the initial configured endpoint but
does not control that CLI's subsequent network behavior. Only configure trusted
HTTPS endpoints; HTTPS protects transport and does not establish that a provider
is trustworthy. Validation errors never include the endpoint or credential.
