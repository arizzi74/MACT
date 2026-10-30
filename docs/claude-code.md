# Claude Code workers

MACT supports Claude Code through a dedicated `claude-worker` service. One
Claude worker manages one local Claude installation and account configuration;
it can run multiple independent sessions. Codex continues to use `codex-worker`.
Both workers can coexist on a host with separate enrollment identities, services,
configuration, state and update queues.

```mermaid
flowchart LR
  UI[Web UI and Telegram bots] <--> Gateway[MACT gateway]
  Gateway <-->|Authenticated outbound WSS| CW[Codex worker]
  Gateway <-->|Authenticated outbound WSS| AW[Claude Code worker]
  CW <--> Codex[Codex app-server]
  AW <--> Bridge[Claude streaming adapter]
  Bridge <--> A[Claude session A]
  Bridge <--> B[Claude session B]
  Admin[Admin runtime settings] --> Gateway
  AW --> Settings[Private local account configuration]
```

The Go adapter launches the installed Claude CLI directly, using its streaming
JSON input, output and permission control protocol. It does not evaluate a shell
wrapper. Claude owns native conversation persistence; the worker maintains a
private session inventory and display history. This release lists sessions
created through the Claude worker; it does not import unrelated existing CLI
sessions automatically.

## Install

Install and sign in to Claude Code using its
[official installation instructions](https://code.claude.com/docs/en/setup).
Alternatively, configure an API key or authentication token in MACT after
worker enrollment. Claude must already be installed for the worker's OS account.
MACT uses an existing Claude CLI installation. After setup, it checks the configured official
release channel daily and updates supported native installations when the worker is idle.

In the gateway admin console choose **Enroll worker**, then on the worker host:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/MACT/main/scripts/install.sh | sh -s -- setup claude-worker
```

Setup installs the latest stable MACT release. See the
[installation guide](installation.md) for requirements and the
[optional authenticated-first procedure](installation.md#download-and-authenticate-the-installer).

On an existing MACT installation:

```sh
mact setup claude-worker
```

Enter the worker name and the one-use enrollment URL. Linux setup asks whether
to restrict the service. Service restrictions and Claude's tool permission mode
are separate: selecting a Claude permission mode never grants operating-system
privileges. Linux uses `claude-worker.service`; macOS uses
`com.iaia.claude-worker` as the logged-in user's LaunchAgent.

Paths are relative to the worker service account's home:

| Purpose | Path |
| --- | --- |
| Worker configuration | `~/.config/claude-worker/config.json` |
| Worker state | `~/.local/state/claude-worker/` |
| Runtime account settings | `~/.local/state/claude-worker/runtime-settings.json` |
| Worker executable | `~/.local/bin/claude-worker` |

The account settings file is optional until first saved through the admin
console. Its permissions must be `0600` or stricter. Native Claude login files
remain under the installed CLI's normal configuration directory.

## Configure the worker account

Expand **Workers** in the admin console, select the Claude worker's runtime
configuration button, and set:

- **Model:** the default Claude model alias or custom endpoint's model identifier.
  For custom endpoints, the chosen model also populates the Opus, Sonnet and
  Haiku model aliases. Official Claude accounts retain the distinct native aliases.
- **Effort level:** inherit the Claude default or choose an effort supported
  by the selected model. Available session choices come from the installed CLI;
  support for higher effort levels depends on the model and runtime version.
  When changing the endpoint or authentication account, first save with
  **Auto (model default)** effort, then choose an effort after the worker has
  refreshed that account's model capabilities. Unadvertised combinations are
  rejected rather than silently downgraded.
- **API endpoint:** leave empty for Claude's normal endpoint, or provide the
  compatible endpoint used by your account.
- **Authentication:** existing Claude login, API key (`ANTHROPIC_API_KEY`), or
  authentication token (`ANTHROPIC_AUTH_TOKEN`).
- **Permissions:** normal approval requests, accept edits, plan mode, don't ask,
  automatic review, or an explicit bypass-permissions choice. MACT offers modes
  supported by the installed CLI. Account or managed policies can further
  restrict them. Permission modes do not grant operating-system privileges.

The credential is write-only in the UI. An omitted credential preserves the
existing value; **Remove saved credential** clears it. Switching to existing
login stops passing the saved key/token to Claude, even if it is retained for
later use. An active API-key or token mode requires a nonempty credential.

Configuration requests use the authenticated live worker connection. The gateway
does not persist credentials in its database, command queue or event history.
The worker writes them atomically to the private settings file. Offline workers
cannot be configured, and interrupted saves are never automatically replayed.
Refresh the settings to check their state before retrying.

Changes require all worker sessions and queues to be idle, including outstanding
approval requests. Idle Claude processes are retired after a successful save;
the next message resumes the native session using the new settings. Existing
conversation context is retained when you change account or model.

### Select settings for one session

Click the model, effort level, or permission mode in the Web UI status bar,
or use `/model`, `/effort`, and `/permissions` in the Web UI or Telegram.
`/reasoning` remains a compatible alias for `/effort`. `/effort auto` restores
the native model default. The controls use **Low**, **Medium**, **High**,
**Extra high**, and **Max**, offering only the levels the installed CLI advertises
for that model. Model display names also come from the native CLI.
The model picker lists the native CLI's advertised models and effort levels;
custom model identifiers can also be entered. Model and effort changes apply
after the final selection. Cancelling a picker keeps the existing settings.

Session overrides are saved on the worker and survive reconnects and worker
restarts. They take precedence over worker defaults without changing other
sessions. A session must have no active turn, queued input, pending approvals,
questions, or background work before its settings can change. Its next prompt
resumes the same native conversation with the selected settings. A new session
uses the worker defaults.

The permissions menu describes Claude's native modes. Bypass permissions
requires a separate confirmation. Plan mode limits Claude's workflow; it is
not a filesystem sandbox. The worker service and OS account continue to define
which files, commands, and sudo privileges are actually available.

## Session behavior

Use **+** beside the worker to create a session with a project path, then use
Telegram or the Web UI to send messages. Live assistant text, tool calls/results,
approvals and clarification questions use MACT's existing session routing.
File uploads and downloads retain the worker workspace restrictions.

New Claude sessions in the Web UI default to `~/CLAUDE_CODE/<session_name>`
on the worker, with spaces in the session name replaced by underscores. You can
edit the path before creating the session.

Interactive enrollment permits the worker account's home and its subfolders,
including `~/CLAUDE_CODE`. For a manual installation using
`examples/claude-worker.json`, create `~/CLAUDE_CODE` first and replace the example
account path. Its default workspace root and runtime directory both use that
folder.

Existing configurations retain their explicit `allowed_workspace_roots` and
runtime directories during setup and updates. If a Claude worker is restricted
to `~/CODEX` or another custom folder, either choose a permitted project path
when creating a session or explicitly add `~/CLAUDE_CODE` to that worker's
allowed roots. Changing the suggested path does not grant additional filesystem
access or move existing projects.

A follow-up sent during a Claude turn is **queued** for a separate successor
turn. It does not change the current turn's instructions. The browser labels the
button **Queue**; Telegram's active-turn messages follow the same queue behavior.
A worker restart never silently replays uncertain prompts. Pending bridge queue
items are cancelled with a saved history notice so they can be resent explicitly.

Claude background agents and commands remain part of the request that launched
them, including agents that Claude starts in the background implicitly. MACT
keeps the turn active while that work runs and while the parent assembles its
final response. Interim replies and task updates stay in the same conversation;
queued input starts after the request finishes. Account changes and maintenance
remain blocked during this work. **Stop** cancels the turn and retires its Claude
process group before another message can start. Ambient housekeeping does not
keep a request active. A deliberately persistent background command keeps its
request active until it ends or you stop it.

This first integration does not provide Claude project archive/restore,
Codex goals, Codex quotas/reset redemption, native Codex terminal
attachment, or Codex-specific settings and plugins. These capabilities are not
advertised for Claude workers. Session naming changes only the name displayed in
MACT and Telegram.

### Screenshots and generated files

The worker tells Claude to save deliverables inside the session project and
return Markdown file links. These open the Web UI's download dialog. Screenshot
paths written in inline code also become clickable, including in saved history.
Local image references in a final reply are attached to that reply's Telegram
bot automatically, using the protected worker file-transfer channel. Claude
does not need the bot token or a separate Telegram tool. See
[worker file downloads and image attachments](webui.md#downloading-project-files)
for supported formats and limits.

### Delete a session

Choose **Edit session → Delete** and confirm the selected session. Deletion
requires an idle session with no queued messages, pending approvals or questions.
The worker removes that session's native transcript and session-specific files
under `~/.claude/projects/` (or the configured `CLAUDE_CONFIG_DIR/projects/`),
including its subagent transcripts and saved tool results. Other sessions and
shared project memory are preserved.

MACT also removes the worker's cached conversation, question and upload records,
and clears stored command content for the session. Minimal deletion markers and
command receipts remain so a restart or repeated request cannot restore the
deleted session. Already queued delivery events retain their original contents
until the gateway acknowledges them, then the worker removes them normally.

The project directory and every file in it are kept, including directories under
`~/CLAUDE_CODE/` and any custom project path. This action does not archive or
delete the project.

### Recover missed native output

For a turn completed prematurely by an older worker, an administrator can call
`thread/history/recover` on the worker's private local runtime socket after
initialization, supplying the existing `threadId` and `turnId`:

```json
{"id":2,"method":"thread/history/recover","params":{"threadId":"SESSION_UUID","turnId":"TURN_UUID"}}
```

Recovery requires a completed saved turn, its recorded native location, and no
open native process, active or queued input, pending approvals, or deletion.
After confirming sessions are idle, restarting the worker closes idle native
processes. Recovery reads the matching main native transcript without starting
Claude or submitting a prompt. It preserves the saved turn and existing item
identities, imports missed parent replies, tool calls and parent-visible task
completion summaries, and retains native message timestamps. Child transcripts
and private reasoning are excluded. Repeating a successful recovery adds no
duplicates. Ambiguous or changing sources leave saved history untouched.

The repaired turn ends at the final native reply. When the additional activity
has no trustworthy cumulative cost result, consumption is shown as unknown.
Refresh an open conversation to load the repaired history.

## Status line, usage and API spend

Claude sessions show the model, actual Claude permission mode, turn activity,
current context, and cumulative session consumption in USD. Context and cost
appear when the CLI reports them; an unknown value is shown as a dash rather
than zero. The last turn's reported cost is available in the session cost
indicator's tooltip. `/status` and `/usage` report the same observations in text.

The CLI's USD figure is a reported estimate, not a provider invoice. Custom
endpoints can bill at different rates. Session cost is retained across worker
restarts, and cumulative CLI totals are not added repeatedly. Historical costs
that were never reported to MACT remain unavailable until the native CLI
reports them; a crashed process can leave unreported spending unknown.
If a native counter resets below its saved total, MACT marks session cost as
unavailable instead of presenting the old subtotal as current spending.

For an existing Claude Code account on the default endpoint, the worker reads
Claude Code’s structured `/usage` control automatically on startup and about
every minute. **API spend** shows the reported monthly allowance, spending and
remaining amount. Available subscription windows use Claude’s **Current session**
and **Current week** labels, with reset times in their tooltip and text report.
This read uses the native login; no Enterprise Admin API key is required.

The query runs in an isolated, non-persistent CLI process and sends no model
prompt. It requests `skip_behaviors` to avoid scanning local transcripts. A
failed or unsupported usage query clears the last observation without stopping
an active conversation. Native API spend amounts are currency minor units; MACT
converts explicit USD to dollars. Missing amounts or currencies stay unknown,
and a missing limit is not presented as unlimited. Dollar allowances are kept
separate from estimated session cost and provider credit balances. The native
control is experimental, so older or incompatible runtimes can omit this data.
See the [Claude Agent SDK usage report](https://code.claude.com/docs/en/agent-sdk/typescript#sdkusagereport).

For `https://api.abliteration.ai`, the worker also reads the documented
[`GET /v1/credits`](https://docs.abliteration.ai/api-reference/credits/get-the-credit-balance-for-the-api-keys-organization)
endpoint using its configured API credential. It displays remaining USD credit
as `max(0, total_credits - total_usage)`. This is an account-wide balance, including
usage outside this session. It refreshes on worker startup and approximately
every 60 seconds; the tooltip shows the check time. Finalized usage can lag an
active turn.

The provider request runs on the worker. Credentials and raw billing responses
are never sent to the browser or gateway. Account changes clear the old balance,
and an in-flight response from the old account is discarded. Failed checks show
an unavailable value. Other providers show no invented account balance.

### Optional Enterprise Admin API fallback

The native `/usage` budget takes precedence when it is available. For accounts
that need the separate Admin API, in **Workers → Claude Code settings**, enable
**Use Enterprise Admin API budget fallback** for an
official Claude login, enter the Enterprise member's `user_...` identifier,
and save an Enterprise Admin API key with the **`read:spend_limits`** scope.
The organization's Primary Owner creates this separate key in Claude's
**Organization settings → API**. A normal Claude login or inference API key
cannot authenticate this documented Admin endpoint. See the official
[Enterprise Admin API guide](https://support.claude.com/en/articles/15330651-claude-enterprise-admin-api-reference-guide).

The worker reads only that member's effective spending limit and accrued usage
using `GET /v1/organizations/spend_limits/effective`, at startup and approximately
every minute. The status bar and `/status` show the maximum monthly budget and
remaining amount, or an explicit unlimited limit. Reported amounts are converted
from USD minor units; remaining budget is the limit minus period-to-date spend,
clamped at zero. Zero is a disabled spending allowance, not unlimited. Other
currencies and unsupported billing periods are shown as unavailable.

This is the configured member's effective limit, which may be inherited from
a group or seat policy. It is separate from session cost, an organization's
shared consumption pool, and any additional organization-wide spending ceiling.
The API does not report the next reset timestamp, so MACT does not invent one.
API access requires an eligible Enterprise organization with usage credits
enabled; unavailable or unauthorized responses clear the displayed observation.

The Admin key is write-only, saved only in the worker's private runtime settings,
and never passed to the Claude subprocess. It is independent of the inference
credential. Requests use Anthropic's fixed HTTPS endpoint and reject redirects.
Changing account settings clears the previous budget and discards older
in-flight responses. Billing identities and the Admin key are excluded from
conversation history and status notifications.

## Maintenance

```sh
claude-worker status
claude-worker doctor
mact update claude-worker
mact update claude --check
mact update claude
```

Worker updates use the normal signed MACT release and idle checks. They do not
invoke the Codex runtime updater or overwrite another worker's service or state.
Claude worker maintenance also checks the official Claude Code release channel
once per UTC day. The default channel is `latest`; an installation that explicitly
selects `stable` retains that choice. Pending runtime updates and restarts are retried at every maintenance
tick until all turns, queues, approvals and background work are idle. The worker's
private update lease blocks new work throughout installation and readiness checks.
The Web UI worker update action and `/tgupdateworkers` perform a fresh runtime
check even when the worker binary is current, and report worker and Claude runtime
outcomes separately. Their discovery result is retained across idle retries and
updater restarts without changing the automatic daily schedule.

`mact update claude` runs only runtime maintenance under the Claude worker's own
installation lock; `--check` inspects versions without installing or changing
update state. Supported native installations use Claude Code's official configured
release channel. npm, package-manager, wrapper, custom or otherwise unrecognized
installations remain externally managed and are reported without modification.
JSON managed settings do not by themselves prevent updates. MACT reads the
native user settings and cached remote/machine policies to evaluate release
channel, version bounds and update-disable environment settings, respecting
managed-source precedence. Unrelated model, tool and permission policies remain
in place. The CLI still fetches and enforces its own fresh startup policy;
`forceRemoteSettingsRefresh` is not treated as an update-disable switch.
Unreadable or ambiguous policy, computed helpers, unsupported MDM sources and
unverifiable provider routing still require administrator-controlled updates.
Failed validation restores the previous launcher and withholds the rejected
release. Runtime replacement preserves configuration, credentials, workspaces
and native conversation data. Claude and Codex maintenance keep separate state,
recovery journals, services and runtime reports. Interrupted Claude runtime
recovery finishes before another worker release can replace the worker binary;
normal updates resume on the following maintenance tick.

MACT keeps three private copies of previous Claude executables for recovery.
Retention removes only old native version files that MACT created and whose
contents still match its recorded hash. Independently installed or changed files
are preserved, along with global configuration, credentials, projects and
conversations.
