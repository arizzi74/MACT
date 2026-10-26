# Continuous integration and release gates

GitHub Actions starts Go checks, browser tests, native archive acceptance tests,
and the native audio build matrices independently. Packaging starts as soon as
all native audio artifacts are available, so cross-compilation can overlap the
remaining tests. The final CI `test` gate requires every check and packaging job
to succeed.

The daily scheduled run checks newly published vulnerability advisories and runs
the native session archive acceptance proof. It does not launch the audio build
matrices, browser suite, or complete race suite. Pushes, pull requests, and manual
CI runs retain those complete checks.

## Go checks and race tests

The reusable `go-checks.yml` workflow separates formatting, `go vet`, live
`govulncheck`, Python migration tests, and Go race tests. Checks use the latest
Go 1.26 patch selected by `actions/setup-go`. The formatting job also runs pinned
`actionlint` to validate workflow syntax, job dependencies, and action inputs.

One race job runs every package returned by `go list ./...` except
`internal/registry`. Four registry jobs discover and sort the package's
top-level tests, examples, and fuzz targets, then assign them round-robin to
shards 0 through 3. Exact top-level name matching keeps each test's complete
subtest tree and each fuzz target's seed corpus. New tests enter the partition
automatically; the union of the four partitions is the complete discovered
list.

All race jobs use `-race -count=1 -json -timeout=1800s`. Restored Go compilation
caches do not replace test execution. Actions uploads timing logs and registry
selection manifests, and job summaries expose skipped tests for review. The
native session archive workflow separately exercises the supported pinned Codex
binary against isolated fixtures and a local mock provider.

The same checks remain available locally:

```sh
./scripts/check.sh
./scripts/test-go.sh

# Run an individual check or race partition.
./scripts/check.sh vulnerabilities
./scripts/test-go.sh packages
./scripts/test-go.sh registry 0 4
```

`scripts/check.sh` also accepts `format`, `vet`, and `python`. With no argument,
both scripts run their complete scope. `VERSION=0.6.31 ./scripts/ci.sh` remains
the complete local check, race-test, cross-build, package, and verification
entrypoint. It requires all four platforms' Whisper binaries in `.native-dist`
and Piper/Qwen archives in `.speech-dist`; `WHISPER_DIST_DIR` and
`SPEECH_DIST_DIR` can override those input directories.

## Reusing unchanged native audio builds

Whisper, Piper, and Qwen native outputs can be restored from an exact build-input
cache. Their source digest includes native files, upstream/model pins, licenses,
fixtures, tests, build and verification scripts, the native workflow, and Go
module metadata. The cache key additionally includes the target platform,
runner image, build flags, and resolved toolchain versions. Linux probes the
pinned container's actual package and compiler versions; macOS records its
compiler, CMake, Xcode, SDK, and linker versions. A change to an input selects a
new cache entry. A cache miss, eviction, or unavailable cache causes the normal
native build to run; non-exact restores are cleared before building.

Actions gives the native helpers the version `native-<24 hex digits>`, derived
from their source digest, so an unchanged helper can be reused in later project
releases. The gateway, worker, local CLI, and update manager keep the project
release version. Helper versions are diagnostic identities; release installation
still selects the project release and verifies the assets covered by that
release's signed checksum manifest.

Every native matrix job runs its real model smoke tests on the current runner,
including after a cache hit. Whisper still verifies warm inference, framing,
and EOF. Speech still verifies Italian and English Piper and Qwen replies,
resident models, float32 PCM, EOF, and the optimized Qwen production CPU
selector. Restoring compiled outputs does not bypass these checks. Model files
are downloaded and verified on every run. Each native job also audits the output
inventory, archived checksums, and embedded source version; a mismatch fails the
job.

Compiled caches contain `dist/whisper`, or `dist/piper` and `dist/qwen-tts`, with
an inventory manifest. They are saved only after successful validation on a
default-branch push or manual run. Pull requests, other branches, and release
tags can restore matching entries or build fresh, and do not save compiled
outputs to this cache.

Caches follow GitHub's ref boundaries. Default-branch caches can warm later
release tags; a cache created only under an earlier release tag cannot warm a
different tag. GitHub also isolates any pull-request cache entries under the
pull-request merge ref, outside the default branch and release tag scope. No
broad fallback key is used to accept a different native build's outputs. These
boundaries and cache eviction behavior are defined by the official
[GitHub dependency caching reference](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching).

## Packaging and publication

The reusable `package.yml` workflow downloads all four platforms' native audio
artifacts, cross-builds the Go executables, produces the full release asset set,
and runs the release verifier. Its artifacts belong to the current workflow
run. Packaging does not publish a release.

On a release tag, `test-and-build` waits for packaging, the complete Go and
browser checks, and native archive acceptance. It downloads the packaged files
and verifies them again before creating fresh provenance in `release.yml` for
the exact tag and current checksum manifest. The native updater policy verifies
that provenance before the files can reach the publish job.

Publication retains the stable-tag check, tag-to-commit check, refusal to replace
an existing release, and draft-first upload. The publish job downloads all 29
draft assets again, compares the checksum manifest and provenance bundle,
verifies provenance with the native updater policy, and checks every asset
checksum before making the draft public. Cached native outputs are included in
the current release's verified archives and newly attested manifest.

Workflow edits should preserve the final `test` and `test-and-build` gate names,
the complete race-test partition, real native smoke tests, and the release
workflow's provenance identity. Cache keys and parallel jobs are performance
mechanisms; a successful cache restore is not a substitute for these gates.
