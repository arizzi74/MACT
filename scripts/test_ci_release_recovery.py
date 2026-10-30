#!/usr/bin/env python3
"""Exercise publication recovery admission without network or mutations."""
import copy
from pathlib import Path
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[1]


class ReleaseRecoveryTest(unittest.TestCase):
    def setUp(self):
        workflow = (ROOT / '.github/workflows/installer-release-publish.yml').read_text()
        source = workflow.split("python3 - <<'PY'\n", 1)[1].split('\n          PY', 1)[0]
        self.policy = {'__name__': 'recovery_test'}
        exec(compile(textwrap.dedent(source), 'release-recovery-guard', 'exec'), self.policy)
        self.repo, self.tag, self.run_id = 'arizzi74/MACT', 'v1.2.3', 77
        self.commit, self.head = 'a' * 40, 'b' * 40
        self.branch = 'release/publish-v1.2.3-run-77'
        self.environment = {'GITHUB_REPOSITORY': self.repo, 'GITHUB_EVENT_NAME': 'create',
                            'CREATED_TYPE': 'branch', 'CREATED_REF': self.branch, 'CREATED_SHA': self.head}
        names = ['release-mode', 'browser', 'package / package', 'test-and-build',
                 'security / Git history and working tree secrets',
                 'security / Shipped browser dependency advisories']
        names += ['go-checks / ' + name for name in ('format', 'vet', 'vulnerabilities', 'python', 'Other packages race')]
        names += ['go-checks / Registry race ' + str(index) + ' of 4' for index in range(4)]
        names += [component + ' / build (' + target + ')' for component in ('native-whisper', 'native-speech')
                  for target in ('linux, arm64, ubuntu-24.04-arm', 'linux, amd64, ubuntu-24.04',
                                 'darwin, arm64, macos-26', 'darwin, amd64, macos-15-intel')]
        names += ['native-session-archive / native-session-archive (' + version + ', pinned)' for version in ('0.163.0', '0.165.0')]
        self.jobs = [self.job(name) for name in names]
        self.jobs += [self.job('installer-tests', 'skipped'), self.job('publish', 'failure')]
        self.run = {'id': self.run_id, 'path': '.github/workflows/release.yml', 'name': 'Release',
                    'event': 'push', 'head_branch': self.tag, 'head_sha': self.commit,
                    'repository': {'id': 1, 'full_name': self.repo},
                    'head_repository': {'id': 1, 'full_name': self.repo},
                    'status': 'completed', 'conclusion': 'failure', 'run_attempt': 1}
        self.artifacts = [{'id': 123, 'name': 'signed-release-files', 'expired': False,
                           'size_in_bytes': 1000, 'digest': 'sha256:' + 'c' * 64,
                           'workflow_run': {'id': self.run_id, 'head_sha': self.commit,
                                            'repository_id': 1, 'head_repository_id': 1}}]
        self.policy['api'] = self.api

    def job(self, name, conclusion='success'):
        return {'name': name, 'run_id': self.run_id, 'run_attempt': 1,
                'status': 'completed', 'conclusion': conclusion}

    def api(self, path):
        refs = {'/git/ref/heads/' + self.branch: ('heads/' + self.branch, self.head),
                '/git/ref/heads/main': ('heads/main', self.head),
                '/git/ref/tags/' + self.tag: ('tags/' + self.tag, self.commit)}
        if path in refs:
            ref, sha = refs[path]
            return {'ref': 'refs/' + ref, 'object': {'sha': sha, 'type': 'commit'}}
        if path == '/compare/' + self.commit + '...' + self.head:
            return {'status': 'ahead', 'merge_base_commit': {'sha': self.commit}}
        if path == '/actions/runs/77':
            return copy.deepcopy(self.run)
        if path == '/actions/runs/77/attempts/1/jobs?per_page=100&page=1':
            return {'total_count': len(self.jobs), 'jobs': copy.deepcopy(self.jobs)}
        if path == '/actions/runs/77/artifacts?per_page=100&page=1':
            return {'total_count': len(self.artifacts), 'artifacts': copy.deepcopy(self.artifacts)}
        raise AssertionError('Unexpected API path: ' + path)

    def recover(self):
        return self.policy['recover'](self.environment)

    def test_full_release_with_only_failed_publication_is_admitted(self):
        self.assertEqual(self.recover(), {'tag': self.tag, 'commit': self.commit, 'run_id': '77', 'artifact_id': '123'})

    def test_existing_successful_installer_recovery_remains_supported(self):
        self.run.update(event='workflow_dispatch', conclusion='success')
        self.jobs = [self.job('release-mode'), self.job('installer-tests'), self.job('test-and-build'), self.job('publish', 'skipped')]
        self.recover()

    def test_each_failed_skipped_cancelled_or_running_check_is_refused(self):
        original = copy.deepcopy(self.jobs)
        for index, job in enumerate(original):
            if job['name'] in ('installer-tests', 'publish'):
                continue
            for conclusion in ('failure', 'skipped', 'cancelled', None):
                with self.subTest(job=job['name'], conclusion=conclusion):
                    self.jobs = copy.deepcopy(original)
                    self.jobs[index]['conclusion'] = conclusion
                    if conclusion is None:
                        self.jobs[index]['status'] = 'in_progress'
                    with self.assertRaises(ValueError):
                        self.recover()

    def test_every_missing_or_duplicate_job_is_refused(self):
        original = copy.deepcopy(self.jobs)
        for index, job in enumerate(original):
            for duplicate in (False, True):
                with self.subTest(job=job['name'], duplicate=duplicate):
                    self.jobs = copy.deepcopy(original)
                    if duplicate:
                        self.jobs.append(copy.deepcopy(job))
                    else:
                        self.jobs.pop(index)
                    with self.assertRaises(ValueError):
                        self.recover()

    def test_publication_and_installer_outcomes_must_match_full_mode(self):
        for name, conclusions in (('publish', ('success', 'cancelled', 'skipped')),
                                  ('installer-tests', ('success', 'failure', 'cancelled'))):
            target = next(job for job in self.jobs if job['name'] == name)
            old = target['conclusion']
            for value in conclusions:
                with self.subTest(name=name, value=value), self.assertRaises(ValueError):
                    target['conclusion'] = value
                    self.recover()
            target['conclusion'] = old

    def test_run_identity_foreign_source_and_wrong_outcome_are_refused(self):
        original = copy.deepcopy(self.run)
        for changes in ({'event': 'pull_request'}, {'conclusion': 'success'}, {'status': 'in_progress'},
                        {'head_sha': self.head}, {'head_branch': 'main'}, {'path': '.github/workflows/ci.yml'},
                        {'head_repository': {'id': 2, 'full_name': 'other/MACT'}}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.run = {**copy.deepcopy(original), **changes}
                self.recover()

    def test_signed_artifact_expiry_digest_source_and_missing_payload_are_refused(self):
        original = copy.deepcopy(self.artifacts[0])
        for changes in ({'expired': True}, {'size_in_bytes': 0}, {'digest': 'unknown'},
                        {'workflow_run': {**original['workflow_run'], 'head_sha': self.head}}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.artifacts = [{**copy.deepcopy(original), **changes}]
                self.recover()
        self.artifacts = []
        with self.assertRaises(ValueError):
            self.recover()

    def test_changed_attempt_during_validation_is_refused(self):
        original_api, calls = self.api, []
        def api(path):
            result = original_api(path)
            if path == '/actions/runs/77':
                calls.append(path)
                if len(calls) > 1:
                    result['run_attempt'] += 1
            return result
        self.policy['api'] = api
        with self.assertRaises(ValueError):
            self.recover()


if __name__ == '__main__':
    unittest.main()
