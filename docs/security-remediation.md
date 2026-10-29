# Security remediation record

Status: validated for the v0.7.8 release. The release operator will record
release publication and deployment verification separately.

This implementation starts at commit
`cce495c3308aeff5fb0f06682c24c6005a27db1c` (v0.7.7). Initial `git status --short`
showed only the operator's untracked `inbound/` upload. Tools were Go 1.26.10
linux/arm64, Node.js 22.22.1 and npm 10.9.4. No applicable `AGENTS.md` was present.
The uploaded instructions consolidate assessments of different older revisions;
their line numbers were not treated as evidence about this revision.

Before these changes, `scripts/install.sh` downloaded a manager and unsigned
checksums, then executed it. The native manager's later update path already
authenticated the checksum manifest using the pinned GitHub Actions Sigstore
policy. Existing CI covered exhaustive Go race-test partitions, vet, reachable
Go vulnerabilities, Python checks, browser tests and native platform smoke tests.
It lacked full-history secret scanning and an audit of prebundled browser code.

## Findings and dispositions

| Finding / assessment identifier | Disposition | Evidence and remaining scope |
| --- | --- | --- |
| First executable trust, MACT-02 / L-7 | **Fixed** | `scripts/install.sh` verifies manifest provenance with a preinstalled trusted `gh` before reading its hashes, downloading/executing the manager. Public commands instead authenticate the native manager directly, avoiding mutable `curl | sh`. Wrong identity, tag, source ref, issuer, predicate, signatures and interrupted downloads fail before execution. |
| Pathological Markdown, MACT-01 | **Fixed** | `webui-format.js` uses bounded scanners/work/DOM budgets and pre-parse size checks, with inert full-text fallback. The permanent production-renderer browser test covers 8–128 KiB hostile inputs and a usable composer. |
| Third-party display credentials, M-1 / L-2 | **Fixed** | Shared `auth` defaults cover GitHub, AWS, Google, Slack and JWT shapes in addition to existing rules. Worker outbox writes and reconnect sanitization retain event identities; live projections and literal credential rotation are tested. Unknown encodings and secrets split across fields remain outside a per-field redactor's guarantee. |
| Raw file credential access, MACT-03 / L-3 | **Fixed** | `workerPrivateFilePaths` excludes default/custom MACT and Claude config/state/credential paths before opening bytes. Symlink, hard-link and traversal restrictions remain; ordinary allowed project files retain exact-byte downloads. Existing `.env` restrictions remain. |
| Claude endpoint plaintext transport, L-1 | **Fixed**, with documented native boundary | Shared validation rejects HTTP on assignment and persisted reads before child credentials are supplied. MACT billing requests reject redirects. Native Claude's subsequent inference requests and redirect behavior are owned by the installed Claude CLI. |
| Shipped browser dependencies, MACT-04 | **Fixed** | Rebuilt Mermaid bundle, notices and 68-version inventory contain patched DOMPurify, KaTeX and js-yaml. Both lockfile and actual shipped-version audits are required. No MACT exploit is asserted. See the package/advisory table in `browser-security.md`. |
| Go OpenPGP advisory, MACT-04 residual | **Already fixed** in v0.7.7 | Runtime signatures use maintained `ProtonMail/go-crypto`; `golang.org/x/crypto/openpgp` is not imported. The scanner checks package/call reachability rather than failing on an unused module package. |
| Live browser revocation, L-4 | **Already fixed**; regressions extended | WebSocket input revalidates authentication before forwarding; file requests authenticate before opening a stream and recheck each chunk/target. Tests cover revocation during activity and before new metadata/download requests. Passive WebSocket output uses the existing 15-second check interval plus bounded I/O; revocation is not an instantaneous recall of bytes already delivered. |
| Abort token in argv, L-5 | **Fixed** | `workerupdate.AbortLease` sends the existing authenticated local control request over the private Unix socket. It spawns no process. Both manual worker CLIs use bounded stdin token input. The socket name and wire protocol remain compatible with older workers. |
| CSRF, L-6 | **Mitigated/documented risk**; controls retained | Exact HTTPS Origin, `__Host-` Secure/SameSiteStrict cookies and double-submit CSRF checks remain. Authenticated settings changes additionally require fresh authentication. No demonstrated bypass justified replacing the scheme. |
| Window-boundary rate bursts, L-8 | **Fixed** | Per-client and global admissions now use a bounded rolling window; boundary, clock rollback, state bound and concurrent-budget tests cover it. |
| Telegram `update_id=0`, L-8 | **Fixed** | Both webhook handlers and registry input validation require a positive update ID; missing/zero/negative IDs are rejected. This follows the [Telegram Update contract](https://core.telegram.org/bots/api#update). |
| Public shell optional database work, L-8 | **Fixed** | Malformed session cookies cause no optional lookup. Plausible cookies use bounded per-client/global rolling lookup budgets. The public shell still serves its independent CSRF cookie; all private-route authentication remains unchanged. |
| Loopback `X-Real-IP`, L-8 | **Mitigated/documented risk** | Existing `httpguard.ClientIP` accepts one valid address only from a loopback peer, ignores forwarded chains and rejects remote spoofing. Generated nginx/Caddy configurations overwrite the header. Other local processes share the trusted proxy boundary; the listener must remain loopback-only. |
| Runtime enum mismatch, L-8 | **Mitigated/documented risk** | Admin configuration deliberately exposes the reviewed subset `default`, `acceptEdits`, `plan`, `bypassPermissions`. A regression compares actual UI choices with shared validation. Native-only observed modes are not implicitly enabled for remote configuration. |
| Remote `bypassPermissions` | **Mitigated/documented intentional capability** | The mode remains explicit and default-off. Tests require authenticated, same-origin, CSRF-protected and fresh administrator access, reject revoked/stale sessions, and preserve authorized use. UI copy explains access as the worker OS account, including already permitted sudo. |
| Continuous verification, item 8 | **Fixed** | Reusable security checks gate both CI and releases. The existing exhaustive Go/race/vet/reachable-vulnerability/browser coverage remains; new jobs scan full Git history/current files with redacted output and audit both lockfile and shipped browser versions. |

## Threat model and compatibility

Initial installation trusts the operator's OS, terminal, independently installed
GitHub CLI and its Sigstore trust roots, plus the exact selected repository,
release workflow and stable tag. A checksum fetched beside an asset is not an
authenticity check until that manifest's attestation is verified. The bootstrap
does not protect a compromised trusted verifier, local root account, or an
authorized publisher/workflow deliberately shipping malicious code. The initial
canonical identity policy is strict; historical renamed signers are not silently
accepted. Subsequent updates retain the native updater's separate explicit
repository migration and Sigstore identity policy.

Workers keep API credentials local; display redaction and raw-file exclusions
reduce accidental disclosure through the gateway. They do not sandbox an agent
or operator with the same host-account privileges. The single trusted operator
can intentionally execute commands and open allowed project files. Raw file
bytes travel through the gateway and bypass conversational redaction, so exact
downloads are a distinct trust boundary. Existing private-path and `.env` guards
are retained. MACT private state should live in a dedicated directory.

The gateway and browser depend on HTTPS, secure cookies, exact Origin and CSRF
checks, current session authorization and trusted loopback proxy configuration.
Permission bypass removes native prompts, not operating-system restrictions; it
can use host-account rights and sudo only where that account/service already
permits them. HTTPS endpoint validation protects the configured transport but
does not make a chosen provider trustworthy or control native Claude networking.

No schema migration is needed. Initial installation now requires a sufficiently
current trusted `gh`; saved HTTP Claude endpoints must be changed to HTTPS before
restart; state-at-project-root installations need a dedicated state directory for
raw project downloads; large/dense Markdown becomes inert text; manual aborts
read stdin. Uploaded evidence and local build/audit data remain ignored rather
than entering source control. See [v0.7.8 behavior changes](releases/v0.7.8.md).

## Validation record

The implementation uses synthetic local credentials and test services; it does
not submit test prompts or credentials to production MACT services. Additional
publisher verification read only public GitHub release manifests and Sigstore
trust data, without downloading or executing a manager.

The complete Go commands ran in `/tmp/mact-security-validation-6p23d64i`, an
isolated worktree containing the current implementation and new files. This
kept preexisting ignored local audit sources and uploaded evidence out of
`./...`; no checks or source packages were excluded. The temporary worktree was
removed after validation. All following commands exited **0**:

```sh
go test ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

The full registry package passed in 149.467 seconds and worker package in
92.327 seconds. Govulncheck found zero reachable vulnerabilities and zero in
imported packages; one unused module-package advisory remains, as described in
the disposition table.

Focused concurrency and security regressions also passed:

```sh
go test -race ./internal/httpguard ./internal/workerupdate ./internal/releasemanager ./internal/admin ./internal/worker -run 'Test(Limiter|PublicShell|WorkerAbort|AbortLease|AbortToken|ClaudeBypass|ClaudeAdminPermission|WebUIStreamRoundTripDisconnectAndRevocation|WebUIFilesCannotCompleteAfterRevocationOrTargetChange|DefaultCredential|DefaultCredentials|WebUIRejectsWorker|WebUIProtectsRelative|ClaudeRejectsInsecure|ClaudeCreditsRejectsRedirects|ClaudeCredentialRotation|RedactorConcurrentCredentialRotation)' -count=1
npm run test:browser
npm run security:audit
./scripts/check.sh format
./scripts/check.sh python
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -color
sh -n scripts/install.sh scripts/check-secrets.sh
git diff --check
```

The entire browser command passed, including the new real-renderer Markdown
regression and existing file, upload, session, authentication, question,
permission, tool, Mermaid sandbox/CSP and website suites. The adversarial
Markdown run's largest median was **15.6 ms** locally; this is a Linux Chromium
measurement, not a guarantee for every device. Both JavaScript scans reported
zero findings: the entire lockfile and all **68 exact shipped package versions**.
Python checks passed **11 SQLite** and **51 CI policy/planner/cache** tests.

With the independently source-built, pinned Gitleaks 8.30.1 binary:

```sh
GITLEAKS_BIN="$PWD/tmp/audit/security-tools/gitleaks" ./scripts/check-secrets.sh
GITLEAKS_BIN="$PWD/tmp/audit/security-tools/gitleaks" python3 scripts/test_check_secrets.py
```

Both passed. The full-history/current-tree scan covered **1178 intended current
files**, with zero excluded paths. Thirteen synthetic tests confirm rejection of
untracked, staged and deleted historical canaries without printing their values;
safe symlink handling, hard-link rejection, ignore/config bypass prevention and
content-bound exception behavior are covered. The reviewed allowlist contains
11 exact immutable-history false positives and three current fingerprints bound
to their complete-file SHA-256. These cover a migration-filename list, an
existing offline UUID fixture and generated Mermaid code, with no blanket
test-directory exception or credential payload in the policy. A future commit
of reviewed content requires the same path/rule/line and exact full-file hash
read from that commit's blob. Changed content at the same location is rejected
in history and the current tree. Real Git-replacement-object controls confirm
that replacement refs cannot conceal the original changed blob.

The complete candidate source was also committed only in the detached validation
worktree as `729dc0723c1044da21cad33080f41ce2e74fcb63`, leaving `main` unchanged.
The real scanner passed against that prospective committed history and current
tree. Its final helper/environment changes and all 13 synthetic regressions
were then checked again. This catches issues that scanning only uncommitted
changes would miss.

The full Go run includes the bootstrap regressions; they were also run explicitly:

```sh
go test ./internal/distribution -run 'Bootstrap|DocumentedInstaller' -count=1 -v
go test ./internal/distribution ./internal/releaseauth -count=1
```

Both passed. Matching substituted manager/checksum pairs, wrong identity/tag,
bad/missing/malformed provenance, interrupted downloads and unavailable/old
verifiers fail before execution. A correctly authenticated independently keyed
synthetic fixture follows the normal flow. Actual privileged gateway rejection
through passwordless sudo passed without installing anything. Platform selection
and the macOS `shasum` fallback are tested through local synthetic tools.

The system `gh` is 2.4.0 and is correctly rejected by the new installer. An
additional integration check built official **gh 2.103.3** from checksum-verified
Go modules with its required **Go 1.27.3** toolchain, in temporary directories.
Under `env -i` with private config/cache and no credentials, this exact policy
command passed against the public v0.7.7 manifest and bundle:

```sh
gh attestation verify "$manifest" \
  --bundle "$bundle" \
  --repo arizzi74/MACT \
  --cert-identity https://github.com/arizzi74/MACT/.github/workflows/release.yml@refs/tags/v0.7.7 \
  --cert-oidc-issuer https://token.actions.githubusercontent.com \
  --source-ref refs/tags/v0.7.7 \
  --predicate-type https://slsa.dev/provenance/v1 \
  --format json
```

Exit **0** confirmed the exact SAN, issuer, repository, tag, predicate and verified
timestamp. Ten negative controls exited **1**: wrong-owner and same-owner wrong
repositories, workflow SAN, tag SAN, issuer, source ref, predicate, substituted
manifest, altered DSSE signature and missing bundle. Temporary verifier files
were removed and the system CLI was unchanged. See the
[official verifier policy](https://github.com/cli/cli/blob/v2.103.3/pkg/cmd/attestation/verify/policy.go).

Both worker commands also cross-compiled successfully for supported macOS
architectures, from the main workspace:

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/codex-worker ./cmd/claude-worker
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./cmd/codex-worker ./cmd/claude-worker
```

An initial cross-build in the temporary worktree failed because Go selected
`/tmp` for its VCS-status query (Git exit 128). The commands above passed from
the normal workspace with VCS stamping enabled; no verification was disabled.
Native audio/model smoke tests and installed-app/Safari manual checks were not
rerun. Existing platform CI remains responsible for native packaging and audio
smoke tests; the security changes add portable script fixtures and Linux Chromium
coverage. MACT does not currently ship a Windows worker package.

## Changed implementation groups

- Installation: `scripts/install.sh`, `internal/distribution/bootstrap_test.go`,
  installer guidance in README/docs/site, admin enrollment and setup completion.
- Rendering/dependencies: `webui-format.js`, `package.json`/lockfile, Mermaid
  build/audit scripts, committed runtime/licenses/inventory and browser tests.
- Worker boundaries: shared `internal/auth` patterns/normalization, outbound
  redaction, `webui_files.go`/uploads, config-source metadata, Claude persisted
  validation and credit-request redirect rejection, plus recovery comparison.
- Local maintenance: shared `internal/workerupdate/control.go`, manager abort
  client, worker CLIs and socket/argv/lease regression tests.
- Gateway/browser hardening: rolling `httpguard` limits, bounded shell lookups,
  positive Telegram IDs, fixed HTTPS error messages and permission/revocation tests.
- Continuous checks: `.github/workflows/security.yml`, required CI/release gate
  wiring, confined secret-scan helpers and reviewed false-positive policy.

The threat model, dependency triage and compatibility notes are documentation;
the enforcement changes and regressions above are executable code. This record
documents implementation validation for v0.7.8. Release publication and production
deployment verification will be recorded separately by the release operator.
