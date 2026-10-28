#!/usr/bin/env python3
"""Exercise the trusted inline release retry policy without network or tokens."""

import contextlib
import copy
import io
import json
import os
from pathlib import Path
import tempfile
import textwrap
import types
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = (ROOT / ".github/workflows/retry-release-runners.yml").read_text()
SHELL = textwrap.dedent(WORKFLOW.split("        run: |\n", 1)[1])
assert SHELL.startswith("python3 - <<'PY'\n") and SHELL.endswith("PY\n")
POLICY = types.ModuleType("release_runner_retry_tests")
exec(compile(SHELL.split("\n", 1)[1].rsplit("\nPY\n", 1)[0], "retry-release-runners.yml", "exec"), POLICY.__dict__)


class ReleaseRunnerRetryTest(unittest.TestCase):
    def setUp(self):
        self.repository = "arizzi74/MACT"
        self.base = "/repos/" + self.repository
        self.run_path = self.base + "/actions/runs/42"
        self.run = {
            "id": 42, "run_attempt": 1, "workflow_id": 7,
            "name": "Release", "path": ".github/workflows/release.yml",
            "event": "push", "status": "completed", "conclusion": "failure",
            "head_branch": "v0.7.6", "head_sha": "a" * 40,
            "repository": {"full_name": self.repository},
            "head_repository": {"full_name": self.repository},
        }
        self.event = {"action": "completed", "repository": {"full_name": self.repository},
                      "workflow_run": copy.deepcopy(self.run)}
        self.jobs = [self.job(101, "browser", "success"),
                     self.job(102, "native-speech / build (darwin, arm64, macos-26)", "cancelled"),
                     self.job(103, "package", "skipped"),
                     self.job(104, "test-and-build", "skipped"),
                     self.job(105, "publish", "skipped")]
        self.annotations = [
            {"annotation_level": "failure", "message": POLICY.NOT_ACQUIRED},
            {"annotation_level": "notice", "message": POLICY.CAPACITY},
            {"annotation_level": "notice", "message": POLICY.CAPACITY},
        ]
        self.fresh_runs = []
        self.calls = []
        self.responses = {}

    def job(self, identity, name, conclusion):
        return {"id": identity, "name": name, "conclusion": conclusion,
                "run_id": 42, "run_attempt": 1, "status": "completed",
                "steps": [], "runner_id": 0, "runner_name": None,
                # Deliberately distinct IDs: never infer a check ID from job.id.
                "check_run_url": "https://api.github.com" + self.base + "/check-runs/777"}

    def open(self, request, timeout):
        self.assertEqual(timeout, 30)
        self.assertEqual(request.get_header("Authorization"), "Bearer test-token-never-log")
        method = request.get_method()
        self.calls.append((method, request.full_url, request.data))
        self.assertTrue(request.full_url.startswith(POLICY.API + self.base + "/"))
        path = request.full_url.removeprefix(POLICY.API)
        if (method, path) in self.responses:
            result = self.responses[method, path]
        elif method == "GET" and path == self.run_path:
            result = self.fresh_runs.pop(0) if self.fresh_runs else self.run
        elif method == "GET" and path == self.run_path + f"/attempts/{self.run['run_attempt']}/jobs?per_page=100&page=1":
            result = {"total_count": len(self.jobs), "jobs": self.jobs}
        elif method == "GET" and path == self.base + "/check-runs/777/annotations?per_page=100&page=1":
            result = self.annotations
        elif method == "POST" and path == self.run_path + "/rerun-failed-jobs":
            self.assertEqual(request.data, b"{}")
            result = None
        else:
            self.fail(f"Unexpected HTTP request: {method} {path}")
        return io.BytesIO(json.dumps(result).encode() if result is not None else b"")

    def invoke(self):
        opener = types.SimpleNamespace(open=self.open)
        with patch.object(POLICY.urllib.request, "build_opener", return_value=opener), contextlib.redirect_stdout(io.StringIO()):
            return POLICY.retry(self.event, self.repository, "test-token-never-log")

    def assert_refused(self):
        self.assertFalse(self.invoke())
        self.assertFalse(any(method == "POST" for method, _, _ in self.calls))

    def test_confirmed_capacity_retries_original_run_after_fresh_validation(self):
        self.assertTrue(self.invoke())
        self.assertEqual(self.calls[-2][0:2], ("GET", POLICY.API + self.run_path))
        self.assertEqual(self.calls[-1][0:2], ("POST", POLICY.API + self.run_path + "/rerun-failed-jobs"))
        self.assertTrue(any("/check-runs/777/" in url for _, url, _ in self.calls))

    def test_both_known_families_and_runner_generations(self):
        for runner in ("macos-15", "macos-26"):
            with self.subTest(runner=runner):
                self.setUp()
                self.jobs[1]["name"] = f"native-speech / build (darwin, arm64, {runner})"
                self.jobs.append(self.job(106, f"native-whisper / build (darwin, arm64, {runner})", "cancelled"))
                self.run["conclusion"] = self.event["workflow_run"]["conclusion"] = "cancelled"
                self.assertTrue(self.invoke())

    def test_second_attempt_can_retry_but_third_cannot(self):
        self.run["run_attempt"] = self.event["workflow_run"]["run_attempt"] = 2
        for job in self.jobs:
            job["run_attempt"] = 2
        self.assertTrue(self.invoke())
        self.run["run_attempt"] = self.event["workflow_run"]["run_attempt"] = 3
        self.calls.clear()
        self.assert_refused()
        self.assertEqual(self.calls, [])

    def test_wrong_event_repository_workflow_tag_or_attempt_is_refused(self):
        invalid = {
            "event": ("pull_request", "workflow_dispatch"), "name": ("CI",),
            "path": (".github/workflows/ci.yml", ".github/workflows/release.yml@main"),
            "head_branch": ("main", "v0.7.6-rc1", "v00.7.6", "v0.7.6\n", "refs/tags/v0.7.6"),
            "head_sha": ("short", "a" * 40 + "\n"),
            "run_attempt": (0, 3, -1, "1", True),
            "status": ("queued", "in_progress"), "conclusion": ("success", "timed_out", None),
            "head_repository": ({"full_name": "attacker/fork"}, None),
            "repository": ({"full_name": "attacker/fork"},), "id": (0, "42", True),
        }
        for key, values in invalid.items():
            for value in values:
                with self.subTest(key=key, value=value):
                    self.setUp()
                    self.event["workflow_run"][key] = value
                    self.assert_refused()
                    self.assertEqual(self.calls, [])

    def test_wrong_outer_repository_or_event_is_refused(self):
        for key, value in (("repository", {"full_name": "attacker/fork"}), ("action", "requested")):
            with self.subTest(key=key):
                self.setUp()
                self.event[key] = value
                self.assert_refused()
                self.assertEqual(self.calls, [])

    def test_genuine_failure_cancelled_other_job_or_inflight_job_prevents_retry(self):
        for key, value in (("conclusion", "failure"), ("conclusion", "timed_out"),
                           ("conclusion", "cancelled"), ("conclusion", "skipped"), ("status", "in_progress")):
            with self.subTest(key=key, value=value):
                self.setUp()
                self.jobs[0][key] = value
                self.assert_refused()

    def test_capacity_job_requires_exact_name_no_runner_and_no_steps(self):
        invalid = {"name": ("native-speech / build (darwin, amd64, macos-26)",
                            "native-speech / build (darwin, arm64, macos-14)", "other / build (darwin, arm64, macos-26)"),
                   "steps": ([{"name": "test", "conclusion": "failure"}], None),
                   "runner_id": (123,), "runner_name": ("GitHub Actions 1",),
                   "run_id": (43,), "run_attempt": (2,), "status": ("queued",)}
        for key, values in invalid.items():
            for value in values:
                with self.subTest(key=key, value=value):
                    self.setUp()
                    self.jobs[1][key] = value
                    self.assert_refused()

    def test_both_exact_annotations_required_and_other_failures_refused(self):
        cases = ([], self.annotations[:1], self.annotations[1:],
                 self.annotations + [{"annotation_level": "failure", "message": "Native smoke test failed"}],
                 [{"annotation_level": "failure", "message": POLICY.NOT_ACQUIRED + "."}, self.annotations[1]],
                 [{"annotation_level": "notice", "message": POLICY.NOT_ACQUIRED}, self.annotations[1]])
        for annotations in cases:
            with self.subTest(annotations=annotations):
                self.setUp()
                self.annotations = annotations
                self.assert_refused()

    def test_check_url_cannot_send_token_to_other_origin_or_repository(self):
        for url in ("https://evil.test/check-runs/777", "http://api.github.com" + self.base + "/check-runs/777",
                    POLICY.API + "/repos/attacker/fork/check-runs/777",
                    POLICY.API + self.base + "/check-runs/777?redirect=1",
                    POLICY.API + self.base + "/check-runs/777/../888"):
            with self.subTest(url=url):
                self.setUp()
                self.jobs[1]["check_run_url"] = url
                self.assert_refused()
                self.assertFalse(any("/check-runs/" in target for _, target, _ in self.calls))

    def test_concurrent_rerun_or_changed_identity_before_post_is_refused(self):
        for key, value in (("run_attempt", 2), ("status", "queued"), ("head_sha", "b" * 40),
                           ("head_branch", "v0.7.7"), ("head_repository", {"full_name": "attacker/fork"})):
            with self.subTest(key=key):
                self.setUp()
                changed = copy.deepcopy(self.run)
                changed[key] = value
                self.fresh_runs = [copy.deepcopy(self.run), changed]
                self.assert_refused()
                self.assertEqual(self.calls[-1][0:2], ("GET", POLICY.API + self.run_path))

    def test_stale_payload_refused_before_fetching_jobs(self):
        self.run["run_attempt"] = 2
        self.assert_refused()
        self.assertEqual(len(self.calls), 1)

    def test_no_capacity_jobs_duplicate_family_or_more_than_two_refused(self):
        for jobs in ([self.jobs[0]], self.jobs + [self.job(106, self.jobs[1]["name"], "cancelled")],
                     self.jobs + [self.job(106, self.jobs[1]["name"], "cancelled"),
                                  self.job(107, "native-whisper / build (darwin, arm64, macos-26)", "cancelled")]):
            with self.subTest(jobs=jobs):
                self.setUp()
                self.jobs = jobs
                self.assert_refused()

    def test_jobs_are_paginated_and_inventory_must_be_complete(self):
        self.jobs += [self.job(1000 + index, f"successful-{index}", "success") for index in range(100)]
        prefix = self.run_path + "/attempts/1/jobs?per_page=100&page="
        self.responses["GET", prefix + "1"] = {"total_count": len(self.jobs), "jobs": self.jobs[:100]}
        self.responses["GET", prefix + "2"] = {"total_count": len(self.jobs), "jobs": self.jobs[100:]}
        self.assertTrue(self.invoke())
        self.calls.clear()
        self.responses["GET", prefix + "2"]["total_count"] += 1
        with self.assertRaises(ValueError):
            self.invoke()
        self.assertFalse(any(method == "POST" for method, _, _ in self.calls))

    def test_main_uses_event_file_and_logs_no_token_on_api_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            event_file = Path(directory) / "event.json"
            event_file.write_text(json.dumps(self.event))
            env = {"GITHUB_EVENT_NAME": "workflow_run", "GITHUB_API_URL": POLICY.API,
                   "GITHUB_EVENT_PATH": str(event_file), "GITHUB_REPOSITORY": self.repository,
                   "GH_TOKEN": "test-token-never-log"}
            output = io.StringIO()
            with patch.dict(os.environ, env, clear=True), contextlib.redirect_stdout(output), \
                    patch.object(POLICY.urllib.request, "build_opener", return_value=types.SimpleNamespace(open=self.open)):
                POLICY.main()
            self.assertIn("attempt 2/3", output.getvalue())
            self.assertNotIn(env["GH_TOKEN"], output.getvalue())
            with patch.dict(os.environ, env, clear=True), patch.object(POLICY, "api", side_effect=ValueError(env["GH_TOKEN"])):
                with self.assertRaises(SystemExit) as error:
                    POLICY.main()
            self.assertNotIn(env["GH_TOKEN"], str(error.exception))

    def test_privileged_listener_never_checks_out_or_executes_triggering_code(self):
        self.assertIn("workflows: [Release]", WORKFLOW)
        self.assertIn("types: [completed]", WORKFLOW)
        self.assertIn("actions: write", WORKFLOW)
        self.assertIn("checks: read", WORKFLOW)
        self.assertIn("group: retry-release-runners-${{ github.event.workflow_run.id }}", WORKFLOW)
        self.assertIn("cancel-in-progress: false", WORKFLOW)
        self.assertNotIn("uses:", WORKFLOW)
        self.assertNotIn("${{", SHELL)
        self.assertNotIn("secrets.", WORKFLOW)
        self.assertIsNone(POLICY.NoRedirect().redirect_request(None, None, 302, "redirect", {}, "https://evil.test"))


if __name__ == "__main__":
    unittest.main()
