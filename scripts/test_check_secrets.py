"""Local synthetic regressions for the complete secrets-scanning wrapper."""
from contextlib import redirect_stderr, redirect_stdout
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import secrets
import string
import subprocess
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parent.parent
SPEC = importlib.util.spec_from_file_location('check_secrets', ROOT / 'scripts/check-secrets.py')
SCANNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SCANNER)


class SecretScanTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='mact-secret-fixture-')
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.repository = self.directory / 'repository'
        self.repository.mkdir()
        self.git('init', '-q')
        self.git('config', 'user.name', 'Synthetic fixture')
        self.git('config', 'user.email', 'fixture@example.invalid')
        (self.repository / 'README').write_text('A clean local test repository.\n')
        self.git('add', 'README')
        self.git('commit', '-qm', 'clean initial fixture')
        # Generated only in a temporary directory, never committed to MACT.
        self.canary = 'gh' + 'p_' + ''.join(secrets.choice(string.ascii_letters + string.digits) for _ in range(36))

    def git(self, *arguments):
        result = subprocess.run(['git', '-C', str(self.repository), *arguments],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(result.returncode, 0, 'local Git fixture operation failed')
        return result.stdout

    def scan(self, expected):
        result = subprocess.run([str(ROOT / 'scripts/check-secrets.sh'), str(self.repository)],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=60)
        self.assertFalse(self.canary.encode() in result.stdout + result.stderr,
                         'candidate value escaped the scanner output boundary')
        self.assertEqual(result.returncode, expected, 'unexpected scanner exit status')
        return result

    def test_clean_repository_and_untracked_canary(self):
        self.scan(0)
        (self.repository / 'candidate.txt').write_text('token = "' + self.canary + '"\n')
        self.scan(1)

    def test_staged_canary(self):
        (self.repository / 'candidate.txt').write_text('token = "' + self.canary + '"\n')
        self.git('add', 'candidate.txt')
        self.scan(1)

    def test_deleted_historical_canary_remains_rejected(self):
        (self.repository / 'candidate.txt').write_text('token = "' + self.canary + '"\n')
        self.git('add', 'candidate.txt')
        self.git('commit', '-qm', 'temporary synthetic credential')
        self.git('rm', '-q', 'candidate.txt')
        self.git('commit', '-qm', 'delete synthetic credential')
        result = self.scan(1)
        self.assertIn(b'1 history and 0 current-tree findings', result.stderr)

    def test_ignored_files_and_external_symlinks_are_not_read(self):
        (self.repository / '.gitignore').write_text('private/\ninbound/\ntmp/\nnode_modules/\n.env\n')
        for name in ['private/key', 'inbound/evidence', 'tmp/state', 'node_modules/package/key', '.env']:
            target = self.repository / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(self.canary)
        outside = self.directory / 'outside'
        outside.mkdir()
        (outside / 'key').write_text(self.canary)
        (self.repository / 'external-file').symlink_to(outside / 'key')
        (self.repository / 'external-directory').symlink_to(outside, target_is_directory=True)
        self.scan(0)

    def test_tracked_parent_replaced_with_external_symlink_is_not_read(self):
        parent = self.repository / 'nested'
        parent.mkdir()
        (parent / 'safe.txt').write_text('ordinary source')
        self.git('add', 'nested/safe.txt')
        self.git('commit', '-qm', 'ordinary source directory')
        (parent / 'safe.txt').unlink()
        parent.rmdir()
        outside = self.directory / 'outside'
        outside.mkdir()
        (outside / 'safe.txt').write_text(self.canary)
        parent.symlink_to(outside, target_is_directory=True)
        self.scan(0)

    def test_symlink_target_text_is_scanned_without_dereferencing(self):
        (self.repository / 'link-text').symlink_to(self.canary)
        self.scan(1)

    def test_hardlink_fails_without_reading_external_contents(self):
        outside = self.directory / 'external-key'
        outside.write_text(self.canary)
        os.link(outside, self.repository / 'linked-key')
        self.scan(2)

    def test_inline_allow_and_local_config_cannot_disable_rules(self):
        (self.repository / 'candidate.txt').write_text(self.canary + ' # gitleaks:allow\n')
        (self.repository / '.gitleaks.toml').write_text('title="disabled"\n')
        (self.repository / '.gitleaksignore').write_text('candidate.txt:github-pat:1\n')
        self.scan(1)

    def test_current_exception_is_bound_to_exact_file_contents(self):
        stage = self.directory / 'stage'
        stage.mkdir()
        (stage / 'fixture.txt').write_text('reviewed synthetic fixture')
        finding = {'file': 'fixture.txt', 'rule': 'fixture-rule', 'line': 1,
                   'commit': '', 'fingerprint': 'fixture.txt:fixture-rule:1'}
        policy = {'current': [{'fingerprint': finding['fingerprint'],
                              'sha256': hashlib.sha256((stage / 'fixture.txt').read_bytes()).hexdigest()}]}
        self.assertEqual(SCANNER.untriaged([finding], stage, policy, 'current'), [])
        (stage / 'fixture.txt').write_text(self.canary)
        self.assertEqual(len(SCANNER.untriaged([finding], stage, policy, 'current')), 1)

    def reviewed_commit_policy(self):
        target = self.repository / 'candidate.txt'
        target.write_text(self.canary + '\n')
        self.git('add', 'candidate.txt')
        self.git('commit', '-qm', 'reviewed synthetic fixture')
        policy = self.directory / 'reviewed-policy.json'
        policy.write_text(json.dumps({'history': [], 'current': [{
            'fingerprint': 'files/candidate.txt:github-pat:1',
            'sha256': hashlib.sha256(target.read_bytes()).hexdigest(),
        }]}))
        return target, policy

    def scan_with_policy(self, policy, expected, additional_canary=''):
        output, errors = io.StringIO(), io.StringIO()
        with mock.patch.object(SCANNER, 'POLICY', policy), redirect_stdout(output), redirect_stderr(errors):
            result = SCANNER.main([str(self.repository)])
        text = output.getvalue() + errors.getvalue()
        self.assertFalse(self.canary in text, 'reviewed candidate escaped the output boundary')
        self.assertFalse(bool(additional_canary) and additional_canary in text,
                         'new candidate escaped the output boundary')
        self.assertEqual(result, expected, 'unexpected content-bound history review result')
        return text

    def test_newly_committed_reviewed_file_passes_full_history_scan(self):
        _target, policy = self.reviewed_commit_policy()
        self.scan_with_policy(policy, 0)

    def test_new_commit_with_changed_content_at_reviewed_location_is_rejected(self):
        target, policy = self.reviewed_commit_policy()
        replacement = 'gh' + 'p_' + ''.join(secrets.choice(string.ascii_letters + string.digits) for _ in range(36))
        target.write_text(replacement + '\n')
        self.git('add', 'candidate.txt')
        self.git('commit', '-qm', 'different synthetic fixture at the same location')
        output = self.scan_with_policy(policy, 1, replacement)
        self.assertIn('1 history and 1 current-tree findings', output)

    def test_git_replace_cannot_hide_changed_content_behind_reviewed_commit(self):
        target, policy = self.reviewed_commit_policy()
        reviewed = target.read_bytes()
        reviewed_commit = self.git('rev-parse', 'HEAD').strip().decode()
        replacement = 'gh' + 'p_' + ''.join(secrets.choice(string.ascii_letters + string.digits) for _ in range(36))
        target.write_text(replacement + '\n')
        changed = target.read_bytes()
        self.git('add', 'candidate.txt')
        self.git('commit', '-qm', 'unreviewed synthetic fixture')
        changed_commit = self.git('rev-parse', 'HEAD').strip().decode()
        self.git('replace', changed_commit, reviewed_commit)
        # Prove the negative control is active: ordinary Git sees the reviewed
        # replacement, while the scan must inspect the original immutable blob.
        self.assertTrue(self.git('cat-file', 'blob', changed_commit + ':candidate.txt') == reviewed,
                        'the Git replacement negative control was not active')
        self.assertTrue(SCANNER.git(self.repository, 'cat-file', 'blob',
                                   changed_commit + ':candidate.txt') == changed,
                        'the shared Git helper followed a replacement object')
        output = self.scan_with_policy(policy, 1, replacement)
        self.assertIn('1 history and 1 current-tree findings', output)

    def test_history_content_lookup_requires_confined_path_and_full_commit(self):
        finding = {'file': 'fixture.txt', 'rule': 'fixture-rule', 'line': 1,
                   'commit': 'HEAD', 'fingerprint': 'unreviewed'}
        policy = {'current': [{'fingerprint': 'files/fixture.txt:fixture-rule:1',
                              'sha256': hashlib.sha256(b'fixture').hexdigest()}]}
        with self.assertRaises(SCANNER.ScanFailure):
            SCANNER.untriaged([finding], self.directory, policy, 'history', self.repository)
        for path in ['../outside', '/outside', 'nested/../outside', 'nested//outside']:
            finding['file'] = path
            with self.assertRaises(SCANNER.ScanFailure):
                SCANNER.untriaged([finding], self.directory, policy, 'history', self.repository)


if __name__ == '__main__':
    unittest.main()
