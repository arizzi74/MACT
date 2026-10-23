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

### v0.6.23 bridge

Installed updaters save their release repository and verify both asset locations
and Sigstore provenance. Managers from before v0.6.21 require asset URLs and a
signing identity under the old repository name. A GitHub redirect cannot satisfy
those checks. The original v0.6.21 rollout assumed those managers had already
been upgraded; v0.6.23 provides a path for installations that missed that step.

The v0.6.23 bridge uses `arizzi74/Codex-Telegram-Gateway` as a minimal
compatibility repository. Its release contains the same binaries, archives,
installer asset and byte-for-byte `SHA256SUMS` as the canonical MACT release.
Only `SHA256SUMS.sigstore.json` differs: the compatibility repository's own
`.github/workflows/release.yml` signs the unchanged manifest at
`refs/tags/v0.6.23`. The canonical release retains its MACT attestation. This
lets old managers verify the exact legacy repository, workflow and tag without
weakening verification or rebuilding the binaries.

The v0.6.23 manager translates the exact saved repository name
`arizzi74/Codex-Telegram-Gateway` to `arizzi74/MACT` before requesting release
metadata. It does not depend on the old repository redirect or the compatibility
repository's identity. Before translating the name, it checks that the canonical
GitHub repository has the expected full name and immutable repository ID. Asset
URLs, the release tag and the exact provenance identity remain verified.
Historical releases retain their original signing identity. Custom repository
selections keep their own policy and are not translated by this bridge.

An old manager can install the mirrored v0.6.23 through its existing update
schedule. That installs the current manager and preserves the existing gateway
or worker identity; no enrollment is required. On the next successful normal
scheduled update, the current manager resolves MACT and saves the canonical
repository, including when the service is already at v0.6.23. `--check` verifies
the available release without saving that change. Failed verification or an
update still awaiting successful readiness must not commit the repository
change. Normal worker updates continue to wait for idle.

The compatibility repository freezes at the v0.6.23 bridge. Its release must
remain available for installations that return later, but subsequent releases
need only be published to MACT: current managers follow the canonical repository.
There is no continuing release mirror or source fork at the old name.

Recreating the old repository replaces GitHub's repository-rename redirects.
The compatibility README directs source, issues and history to MACT. Its
`main/scripts/install.sh` forwards to the canonical installer, and a separate
Pages deployment forwards the old
`https://arizzi74.github.io/Codex-Telegram-Gateway/` site to `/MACT/`, preserving
the path, query and fragment. Older historical release URLs at the old name are
not guaranteed to be mirrored; use their canonical MACT URLs. The installer and
Pages paths have separate deployment and verification from release assets.

Fresh official installations still require v0.6.21 or later. The current
bootstrap and native installer reject earlier versions because their bundled
updaters cannot independently follow the repository transition. This does not
prevent an existing older installation from receiving the v0.6.23 bridge through
the compatibility release. Historical releases are not rebuilt or replaced, and
downgrades remain unsupported.

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

These read-only checks do not install the bridge or persist the canonical
repository. The existing normal update schedule performs those steps. An
already-current installation also saves the canonical repository on a successful
normal update run without a service version change.

### Release validation

Validate the final signed bridge with the actual v0.6.19 verifier and package
loader, retaining its embedded trust root. Check that both releases have
identical checksum manifests and assets apart from their provenance bundles,
and that each bundle verifies under its exact repository, workflow and tag.
Use disposable installation layouts for package extraction and manager checks.

State tests must cover the next v0.6.23 update saving MACT without a service
version change, read-only `--check`, pending readiness, busy workers and failure
preserving the saved repository and credentials. Reject an altered asset,
incorrect canonical repository identity or invalid provenance, and preserve
independent custom repository selections and historical signing identities.

A public `--check` with the old manager verifies release discovery and
provenance. Package loading and state transitions require the isolated checks
above; do not use live installations for mutating acceptance tests. Verify the
old installer and Pages paths separately.

The canonical release process requires stable version tags, the complete native
build matrix and CI checks, and exact repository/workflow/tag provenance. It
refuses to replace an existing release and verifies all 29 uploaded draft assets
before publication. The compatibility release verifies the canonical manifest
and assets, signs that unchanged manifest under the legacy identity, and checks
its uploaded draft before publication.

## Recovery when an update cannot run

Download `scripts/install.sh` from the canonical repository and run it with an
explicit `--repo arizzi74/MACT`. The bootstrap supplies a manager from v0.6.21
or later; it rejects earlier versions. Run gateway commands with sudo and worker
commands as the worker's existing account. Preserve the existing configuration
and enrollment.

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
