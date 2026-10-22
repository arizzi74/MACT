# Migration to MACT

MACT stands for **Multi Agent Control Tower**. It is the new product name for
Codex Telegram Gateway: a control hub for coding agents across sessions and
machines. Codex is the currently supported runtime. Other runtime integrations
will be separate changes; the name change does not add them.

## Compatibility release

Release v0.6.21 introduced MACT in the browser, admin console, website,
notifications and installer, and added the `mact` management command. The existing
`codex-telegramgw` command invokes the same manager and remains supported.

The installer adds `/usr/local/bin/mact` for a gateway and `~/.local/bin/mact`
for a worker. These are command aliases, not separate installations. An unrelated
program at either path is preserved, with a warning to continue using
`codex-telegramgw` instead.

Existing service names, configuration paths, state directories, installation
locks and release asset filenames stay the same. Gateway and worker updates
continue to use one installation and one update lock. Linux systemd units and
macOS LaunchAgent identifiers retain their existing names.

In particular, this migration preserves:

- Sessions, their identifiers, history, names and Telegram bot bindings.
- Worker identifiers, enrollment credentials and runtime settings.
- Project paths, `inbound` folders and existing `~/CODEX_Archived` archives.
- Codex's own executable, account credentials and `~/.codex` data.
- The gateway's hostname, `/tgw/` routes and passkey relying-party identity.
- Browser storage keys, installed web app identity, notification subscriptions
  and the service worker scope.

The Go module path and internal protocol identifiers also retain their existing
names. A runtime-specific label such as Codex still identifies the Codex runtime;
MACT identifies the application that manages it.

## Repository transition

The repository has moved from `arizzi74/Codex-Telegram-Gateway` to
[`arizzi74/MACT`](https://github.com/arizzi74/MACT). Release v0.6.22 uses MACT as
the default release repository. Current installation commands, documentation and
website links use the new repository and `https://arizzi74.github.io/MACT/`.
The old repository name remains an explicitly supported update source through
GitHub's redirect; custom repository selections remain independent.

The move requires the v0.6.21 compatibility manager on every enabled gateway and
worker before GitHub is renamed. An installation that missed that prerequisite
must recover its manager using the bootstrap below before updating its service.

Installed updaters save their release repository and verify both asset locations
and Sigstore provenance. A repository redirect alone is insufficient: the
expected release signer contains the repository, workflow and release tag.

The compatibility updater recognizes only the explicit official repository
transition. It validates the destination and continues to verify the exact
release workflow, source repository and tag. Historical releases keep their
original signing identity. Custom repositories remain independently configured;
they are not redirected to MACT by this migration.

Successful updates save the resolved repository in the installation's update
settings. Failed installation or verification must not change the repository
selection. Release versions continue increasing, and historical releases are
not rebuilt or replaced.

Installation from the official repository requires v0.6.21 or
later. Earlier release archives remain available and their original provenance
can still be verified, but their bundled updaters do not support the renamed
repository. Both the current bootstrap and native installer reject those
pre-migration versions after the move rather than installing an updater that
cannot receive future releases. Existing installations can upgrade directly to
the compatibility release; downgrades remain unsupported.

Check that every enabled installation has a v0.6.21 or newer manager. A worker
can keep serving its existing runtime while a verified manager
is installed through the supported `adopt worker` path. Normal worker binary
updates still wait for idle. Do not restart an active worker solely to change
the product name.

## Operator verification

Check the existing and new management commands:

```sh
codex-telegramgw version
mact version
```

Check gateway and worker updates using the appropriate account:

```sh
sudo mact update gateway --check
mact update worker --check
```

To validate the completed transition, verify a fresh installation, an upgrade
through the old saved repository name, the v0.6.21 release with its historical signer, a
rejected pre-migration installation, and a rejected release with the wrong
signer. Also verify gateway readiness, worker
reconnection, passkey sign-in, installed mobile web apps and archived sessions.
Use isolated fixtures for mutating session and authentication tests.

GitHub Pages uses the new `/MACT/` project URL and a separate website deployment
from gateway and worker releases. Site links, metadata and installation examples
use the new URL. Do not recreate a repository at the old name: it would replace
GitHub's repository redirects. See
[GitHub's repository rename documentation](https://docs.github.com/en/repositories/creating-and-managing-repositories/renaming-a-repository).

## Recovery for an installation that missed the compatibility release

Download `scripts/install.sh` from the new repository and run it with an explicit
`--repo arizzi74/MACT`. The bootstrap supplies a manager from v0.6.21 or later;
it rejects earlier versions. Run gateway commands with sudo and worker commands
as the worker's existing account. Preserve the existing configuration and
enrollment.

For a worker, install the verified manager without restarting its runtime:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/MACT/main/scripts/install.sh | sh -s -- adopt worker --repo arizzi74/MACT
```

For a gateway, use the corresponding supported adoption command:

```sh
curl -fsSL https://raw.githubusercontent.com/arizzi74/MACT/main/scripts/install.sh | sudo sh -s -- adopt gateway --repo arizzi74/MACT
```

For an active worker, install the verified manager with `adopt worker` first;
apply the worker update when its work is idle. The normal updater's busy check
continues to protect running sessions. Existing `codex-telegramgw` commands are
available when the `mact` alias cannot be installed because of a name collision.

Renaming on-disk directories, service identifiers or the public gateway hostname
is outside this migration. Such changes need their own backup, migration and
recovery procedure.
