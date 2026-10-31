#!/usr/bin/env python3
"""Authorize focused release checks only for an exact, narrow Git diff."""

import argparse
import json
from pathlib import Path
import re
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[1]
STABLE = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
ALLOWED = frozenset({
    "README.md", "docs/admin.md", "docs/claude-code.md", "docs/installation.md",
    "docs/mact-migration.md", "docs/overview.md", "docs/security-boundaries.md",
    "docs/security-remediation.md", "internal/admin/static/index.html",
    "internal/admin/worker_enrollments.go", "internal/admin/worker_enrollments_test.go",
    "internal/distribution/bootstrap_test.go", "internal/releasemanager/setup_gateway_finish.go",
    "internal/releasemanager/setup_gateway_finish_test.go", "scripts/install.sh",
    "scripts/test-admin-ui.cjs", "scripts/test-site.cjs", "site/index.html",
    ".github/workflows/installer-release-launch.yml",
    ".github/workflows/installer-release-publish.yml",
    ".github/workflows/release.yml", ".github/workflows/native-whisper.yml",
    ".github/workflows/native-speech.yml", "scripts/check-installer-release.py",
    "scripts/test_ci_installer_release.py",
})

COMPACTION_ALLOWED = frozenset({
    "docs/claude-code.md", ".github/workflows/release.yml",
    "internal/claudeadapter/compaction.go", "internal/claudeadapter/compaction_test.go",
    "internal/claudeadapter/runtime.go", "internal/claudeadapter/runtime_test.go",
    "internal/claudeadapter/rpc.go", "internal/claudeadapter/process.go",
    "internal/claudeadapter/lifecycle.go", "internal/worker/slash_commands.go",
    "internal/worker/claude_integration_test.go", "internal/admin/static/webui.js",
    "internal/admin/static/webui-command-ui.js", "scripts/test-claude-ui.cjs",
    "scripts/check-installer-release.py", "scripts/test_ci_installer_release.py",
})

USAGE_ALLOWED = frozenset({
    ".github/workflows/release.yml", "docs/claude-code.md",
    "internal/claudeadapter/native_usage.go", "internal/claudeadapter/native_usage_test.go",
    "internal/claudeadapter/process.go", "internal/claudeadapter/runtime.go",
    "internal/claudeadapter/runtime_test.go", "internal/claudeadapter/status.go",
    "internal/codexadapter/claude_status.go", "internal/admin/static/webui.js",
    "internal/worker/claude_account_poll.go", "internal/worker/claude_account_poll_test.go",
    "internal/worker/claude_budget.go", "internal/worker/claude_budget_poll_test.go",
    "internal/worker/claude_credits.go", "internal/worker/claude_status.go",
    "scripts/test-claude-ui.cjs", "scripts/check-installer-release.py",
    "scripts/test_ci_installer_release.py",
})


class ScopeError(Exception):
    pass


def git(*arguments, root=ROOT):
    return subprocess.check_output(["git", *arguments], cwd=root, stderr=subprocess.DEVNULL)


def validate(tag, baseline, commit, latest, root=ROOT, scope="installer"):
    if scope not in ("installer", "claude-compaction", "claude-usage"):
        raise ScopeError("Unknown focused release scope")
    target = STABLE.fullmatch(tag)
    previous = STABLE.fullmatch(baseline)
    if not target or not previous or tuple(map(int, target.groups())) <= tuple(map(int, previous.groups())):
        raise ScopeError("Expected a stable release newer than its stable baseline")
    if latest != baseline:
        raise ScopeError("Baseline must be the latest published release")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ScopeError("Invalid expected commit")
    head = git("rev-parse", "HEAD", root=root).decode().strip()
    tagged = git("rev-parse", f"refs/tags/{tag}^{{commit}}", root=root).decode().strip()
    base = git("rev-parse", f"refs/tags/{baseline}^{{commit}}", root=root).decode().strip()
    if head != commit or tagged != commit or base == commit:
        raise ScopeError("Target tag, checked-out commit and baseline do not describe a new release")
    subprocess.run(["git", "merge-base", "--is-ancestor", base, commit], cwd=root,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    names = git("diff", "--name-only", "--no-renames", "-z", base, commit, "--", root=root).split(b"\0")
    changed = {name.decode("utf-8", errors="strict") for name in names if name}
    allowed = {"installer": ALLOWED, "claude-compaction": COMPACTION_ALLOWED, "claude-usage": USAGE_ALLOWED}[scope] | {f"docs/releases/{tag}.md"}
    if not changed or changed - allowed:
        raise ScopeError("Focused release contains an empty or out-of-scope change set")
    # Even an allowed path must remain an ordinary file, never executable
    # indirection through a Git symlink or submodule.
    entries = git("ls-tree", "-r", "-z", commit, "--", *sorted(changed), root=root).split(b"\0")
    if any(entry.split(b" ", 1)[0] not in (b"100644", b"100755") for entry in entries if entry):
        raise ScopeError("Focused release contains a symlink or submodule")
    return changed


def latest_release(repository):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*", repository):
        raise ScopeError("Invalid repository")
    result = subprocess.run(["gh", "api", "--hostname", "github.com",
                             f"repos/{repository}/releases/latest"],
                            capture_output=True, timeout=30, check=True)
    release = json.loads(result.stdout)
    if (release.get("draft") is not False or release.get("prerelease") is not False
            or not isinstance(release.get("tag_name"), str)):
        raise ScopeError("Latest release is not published and stable")
    return release["tag_name"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("repo", "tag", "baseline", "commit"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--scope", choices=("installer", "claude-compaction", "claude-usage"), default="installer")
    args = parser.parse_args()
    try:
        changed = validate(args.tag, args.baseline, args.commit, latest_release(args.repo), scope=args.scope)
    except (ScopeError, subprocess.SubprocessError, OSError, ValueError, AttributeError):
        print("Focused release scope was not proven; refusing reduced checks.", file=sys.stderr)
        return 1
    print(f"Verified {args.scope} release scope: {len(changed)} allowed files.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
