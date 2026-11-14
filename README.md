# Twarm compatibility for existing MACT installations

The original project is now [Twarm](https://github.com/arizzi74/Twarm).
Source, issues, documentation, history and future releases live there.

This minimal compatibility repository exists so managers that require exact
`arizzi74/MACT` release URLs and signing identities can install v0.7.32. Every
bridge asset, including SHA256SUMS and its original MACT provenance bundle, is
copied byte-for-byte from the official release published before the rename.
The original canonical repository retains its immutable GitHub ID and its full
signed release history in Twarm. This repository has a different GitHub ID and
contains no source fork.

The v0.7.32 manager recognizes only the explicitly authorized official names,
resolves the canonical repository by its original immutable ID, verifies exact
release URLs and provenance, and persists the new name only after successful
readiness. Existing configuration, credentials, storage, services, sessions and
Telegram `/tg` commands remain unchanged.

A separate non-latest v0.7.10 release preserves four standalone native manager
files pinned by historical installers, plus their unchanged official manifest
and original MACT provenance bundle. It intentionally contains no component
archives. Use the current Twarm installer for fresh installation and recovery.

The release workflow accepts only `refs/tags/v0.7.10` or `refs/tags/v0.7.32`.
It verifies the original Twarm repository ID and exact pinned bridge source,
checks original MACT signatures and all selected bytes, then verifies uploaded
draft files before publication. No assets are rebuilt, re-signed or overwritten.
Publish the v0.7.10 compatibility files before the v0.7.32 bridge; only v0.7.32
is marked latest. Existing releases must remain unchanged.

The historical installer forwards arguments unchanged to the current Twarm
bootstrap. GitHub Pages from `main` root forwards `/MACT/` paths, query strings
and fragments to `/Twarm/`. Old historical release URLs other than these two
compatibility tags are not mirrored; use their canonical Twarm URLs.
