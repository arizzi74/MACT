#!/usr/bin/env python3
"""Scan full Git history and a confined snapshot of intended working-tree files.

Only finding locations are collected in memory. Scanner output and source snippets
are never forwarded, and no findings report is written to disk.
"""
import errno
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import sys
import tempfile

VERSION = '8.30.1'
POLICY = Path(__file__).with_name('secrets-allowlist.json')
TEMPLATE = '{{range .}}{{dict "file" .File "rule" .RuleID "line" .StartLine "commit" .Commit "fingerprint" .Fingerprint | toJson}}{{"\\n"}}{{end}}'


class ScanFailure(Exception):
    """A safe, deliberately content-free diagnostic."""


def run(arguments, *, cwd, timeout=330, env=None):
    try:
        return subprocess.run(arguments, cwd=cwd, env=env, stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise ScanFailure('required tool failed or exceeded its time limit') from None


def git_environment():
    environment = {key: value for key, value in os.environ.items()
                   if not key.startswith(('GITLEAKS_CONFIG', 'GIT_CONFIG_KEY_', 'GIT_CONFIG_VALUE_'))
                   and key not in ('GIT_CONFIG_PARAMETERS', 'GIT_CONFIG_COUNT')}
    environment.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull,
                       GIT_PAGER='cat', GIT_TERMINAL_PROMPT='0', GIT_NO_REPLACE_OBJECTS='1')
    return environment


def git(repository, *arguments):
    result = run(['git', '--no-replace-objects', '-C', str(repository), *arguments],
                 cwd=repository, env=git_environment())
    if result.returncode:
        raise ScanFailure('could not inspect the Git repository')
    return result.stdout


def snapshot(repository, destination):
    """Copy Git-enumerated regular files through retained, no-follow descriptors."""
    paths = git(repository, 'ls-files', '-z', '--cached', '--others', '--exclude-standard')
    copied, skipped = 0, 0
    root_fd = os.open(repository, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for raw in sorted(set(paths.split(b'\0')) - {b''}):
            parts = os.fsdecode(raw).split('/')
            if any(part in ('', '.', '..') for part in parts) or parts[0] == '.git':
                raise ScanFailure('Git returned an unsafe source path')
            parent_fd = os.dup(root_fd)
            file_fd = None
            try:
                for part in parts[:-1]:
                    next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                                      dir_fd=parent_fd)
                    os.close(parent_fd)
                    parent_fd = next_fd
                metadata = os.stat(parts[-1], dir_fd=parent_fd, follow_symlinks=False)
                if stat.S_ISLNK(metadata.st_mode):
                    # Git records the link text, never the target's contents.
                    # Scan that text as a regular file without dereferencing it.
                    target = destination.joinpath(*parts)
                    target.parent.mkdir(parents=True, exist_ok=True)
                    with target.open('xb') as output:
                        output.write(os.fsencode(os.readlink(parts[-1], dir_fd=parent_fd)))
                    copied += 1
                    continue
                file_fd = os.open(parts[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                                  dir_fd=parent_fd)
                before = os.fstat(file_fd)
                if not stat.S_ISREG(before.st_mode):
                    skipped += 1
                    continue
                if before.st_nlink != 1:
                    raise ScanFailure('a source file has hard links; replace it with a regular private copy')
                target = destination.joinpath(*parts)
                target.parent.mkdir(parents=True, exist_ok=True)
                with os.fdopen(os.dup(file_fd), 'rb') as source, target.open('xb') as output:
                    shutil.copyfileobj(source, output, 1024 * 1024)
                after = os.fstat(file_fd)
                if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns,
                    before.st_ctime_ns) != (after.st_dev, after.st_ino, after.st_size,
                                          after.st_mtime_ns, after.st_ctime_ns):
                    raise ScanFailure('a source file changed during the scan; retry with a stable tree')
                copied += 1
            except OSError as error:
                if error.errno in (errno.ENOENT, errno.ELOOP, errno.ENOTDIR):
                    # Deleted paths and symlinks (including symlinked parent
                    # directories) are never opened through another pathname.
                    skipped += 1
                else:
                    raise ScanFailure('could not safely copy a source file') from None
            finally:
                if file_fd is not None:
                    os.close(file_fd)
                os.close(parent_fd)
    finally:
        os.close(root_fd)
    return copied, skipped


def scan(binary, mode, source, temporary):
    config = temporary / 'policy.toml'
    config.write_text('[extend]\nuseDefault = true\n', encoding='utf-8')
    ignore = temporary / 'empty.ignore'
    ignore.write_text('', encoding='utf-8')
    template = temporary / 'locations.template'
    template.write_text(TEMPLATE, encoding='utf-8')
    flags = ['--redact=100', '--no-banner', '--log-level=error', '--no-color',
             '--ignore-gitleaks-allow', '--config', str(config),
             '--gitleaks-ignore-path', str(ignore), '--timeout=300', '--exit-code=2',
             '--report-format=template', '--report-template', str(template),
             '--report-path=-']
    if mode == 'git':
        flags += ['--log-opts=--all --full-history --no-ext-diff --no-textconv']
    result = run([binary, mode, *flags, '.'], cwd=source, env=git_environment())
    if result.returncode not in (0, 2):
        raise ScanFailure('Gitleaks could not complete the ' + mode + ' scan')
    try:
        findings = [json.loads(line) for line in result.stdout.splitlines() if line]
        if any(set(item) != {'file', 'rule', 'line', 'commit', 'fingerprint'}
               or not isinstance(item['line'], int)
               or any(not isinstance(item[key], str) for key in ('file', 'rule', 'commit', 'fingerprint'))
               for item in findings):
            raise ValueError
    except (ValueError, TypeError):
        raise ScanFailure('Gitleaks returned an invalid location-only report') from None
    if bool(findings) != (result.returncode == 2):
        raise ScanFailure('Gitleaks returned an inconsistent scan result')
    return findings


def reviewed_history_blob(finding, policy, history_source, digests):
    """Carry a reviewed whole-file exception into a future immutable commit."""
    path = finding['file']
    parts = path.split('/')
    if path.startswith('/') or any(part in ('', '.', '..') for part in parts) or '\0' in path:
        raise ScanFailure('scanner returned an unsafe historical finding location')
    fingerprint = f"files/{path}:{finding['rule']}:{finding['line']}"
    expected = {item['sha256'] for item in policy.get('current', [])
                if item['fingerprint'] == fingerprint}
    if not expected:
        return False
    commit = finding['commit']
    if re.fullmatch(r'(?:[0-9a-f]{40}|[0-9a-f]{64})', commit) is None:
        raise ScanFailure('scanner returned an invalid historical commit identity')
    if history_source is None:
        raise ScanFailure('a private history view is required for content-bound review')
    key = (commit, path)
    if key not in digests:
        # cat-file reads a Git object, never a worktree path or symlink target.
        # All bytes remain captured in memory; no blob or candidate is logged.
        data = git(history_source, 'cat-file', 'blob', commit + ':' + path)
        digests[key] = hashlib.sha256(data).hexdigest()
    return digests[key] in expected


def untriaged(findings, stage, policy, mode, history_source=None):
    allowed = policy.get(mode, [])
    remaining = []
    digests = {}
    for finding in findings:
        if mode == 'history':
            reviewed = finding['fingerprint'] in allowed
            if not reviewed:
                reviewed = reviewed_history_blob(finding, policy, history_source, digests)
        else:
            # A location alone cannot suppress a different credential added to
            # the same test line. Current-tree exceptions bind the entire file.
            path = Path(finding['file'])
            if path.is_absolute() or '..' in path.parts:
                raise ScanFailure('scanner returned an unsafe finding location')
            digest = hashlib.sha256((stage / path).read_bytes()).hexdigest()
            reviewed = {'fingerprint': finding['fingerprint'], 'sha256': digest} in allowed
        if not reviewed:
            remaining.append(finding)
    return remaining


def main(arguments):
    if len(arguments) > 1:
        raise ScanFailure('usage: scripts/check-secrets.sh [repository]')
    repository = Path(arguments[0] if arguments else '.').resolve()
    repository = Path(os.fsdecode(git(repository, 'rev-parse', '--show-toplevel')).strip())
    if git(repository, 'rev-parse', '--is-shallow-repository').strip() != b'false':
        raise ScanFailure('full Git history is required; fetch with depth 0')
    binary = shutil.which(os.environ.get('GITLEAKS_BIN', 'gitleaks'))
    if binary is None:
        raise ScanFailure('install the pinned Gitleaks binary and set GITLEAKS_BIN')
    version = run([binary, 'version'], cwd=repository, timeout=10)
    if version.returncode or version.stdout.strip() != VERSION.encode():
        raise ScanFailure('Gitleaks ' + VERSION + ' is required')
    try:
        policy = json.loads(POLICY.read_text(encoding='utf-8'))
    except (OSError, ValueError):
        raise ScanFailure('could not read the reviewed synthetic-fixture policy') from None
    with tempfile.TemporaryDirectory(prefix='mact-secrets-') as directory:
        temporary = Path(directory)
        stage = temporary / 'worktree'
        stage.mkdir(mode=0o700)
        files = stage / 'files'
        files.mkdir(mode=0o700)
        count, skipped = snapshot(repository, files)
        # Gitleaks always loads source/.gitleaksignore, even with an explicit
        # --gitleaks-ignore-path. A clean mirror and a wrapper directory keep
        # repository-controlled ignore files out of that privileged position.
        history_source = temporary / 'history.git'
        clone = run(['git', 'clone', '--quiet', '--mirror', '--shared', '--no-hardlinks',
                     str(repository), str(history_source)], cwd=temporary, env=git_environment())
        if clone.returncode:
            raise ScanFailure('could not create a private full-history view')
        history = untriaged(scan(binary, 'git', history_source, temporary), stage, policy,
                            'history', history_source)
        current = untriaged(scan(binary, 'dir', stage, temporary), stage, policy, 'current')
        if history or current:
            print(f'Secret scan rejected: {len(history)} history and {len(current)} current-tree findings. '
                  'Candidate values and locations are withheld.', file=sys.stderr)
            return 1
        print(f'Secret scan passed: full Git history and {count} intended current files '
              f'({skipped} deleted, symlink or nonregular paths excluded).')
    return 0


def interrupted(_signum, _frame):
    raise ScanFailure('scan interrupted')


if __name__ == '__main__':
    for signum in (signal.SIGTERM, signal.SIGHUP):
        signal.signal(signum, interrupted)
    try:
        sys.exit(main(sys.argv[1:]))
    except (ScanFailure, KeyboardInterrupt, OSError, ValueError, TypeError) as error:
        print('Secret scan failed: ' + (str(error) if isinstance(error, ScanFailure) else
               'scan interrupted' if isinstance(error, KeyboardInterrupt) else 'could not safely complete the scan'),
              file=sys.stderr)
        sys.exit(2)
