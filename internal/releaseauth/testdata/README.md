`public.sigstore.json` is the public sigstore-js 2.0.0 provenance fixture from
[sigstore-go v1.3.0](https://github.com/sigstore/sigstore-go/tree/v1.3.0/pkg/testing/data/bundles),
copyright The Sigstore Authors, under the [Apache 2.0 license](https://www.apache.org/licenses/LICENSE-2.0).
It is used unchanged to test real offline cryptographic verification. Tests do
not trust a root or certificate supplied by the fixture.

`official-v0.6.20.SHA256SUMS` and `official-v0.6.20.sigstore.json` are the
unmodified public checksum manifest and provenance bundle from the project's
[v0.6.20 release](https://github.com/arizzi74/Codex-Telegram-Gateway/releases/tag/v0.6.20),
downloaded on 2026-10-22. They exercise exact historical repository identity,
signature and digest verification across the MACT repository migration. Tests
use the compiled trusted root and perform no network requests.
