#!/usr/bin/env python3
"""Exercise publication against synthetic local gh responses, without tokens."""

import contextlib
import copy
import hashlib
import importlib.util
import io
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("publish_release", ROOT / "scripts/publish-release.py")
POLICY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(POLICY)


class PublishReleaseTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="mact-publish-tests-")
        self.addCleanup(temporary.cleanup)
        self.dist = Path(temporary.name)
        self.repo, self.tag, self.commit = "arizzi74/MACT", "v1.2.3", "a" * 40
        self.base = f"repos/{self.repo}"
        self.payloads = {"manager-linux-amd64": b"synthetic manager", "install.sh": b"synthetic installer"}
        self.payloads["SHA256SUMS"] = "".join(
            f"{hashlib.sha256(data).hexdigest()}  {name}\n" for name, data in self.payloads.items()).encode()
        self.payloads["SHA256SUMS.sigstore.json"] = b"synthetic bundle"
        for name, data in self.payloads.items():
            (self.dist / name).write_bytes(data)
        self.publisher = POLICY.Publisher(self.repo, self.tag, self.commit, self.dist)
        self.remote = None
        self.remote_files = {}
        self.calls = []
        self.upload_errors = {}
        self.create_error = self.publish_error = None
        self.native_fail = False
        self.native_calls = []
        self.download_mutation = None
        self.before_readback_verify = None
        self.before_starter_fetch = None
        self.api_override = None
        self.tag_sha = self.commit
        self.tag_commit = {"oid": self.commit}

    def draft(self):
        # Draft REST metadata can contain a temporary tag. GraphQL must resolve
        # the exact pending tag rather than treating a REST 404 as absence.
        self.remote = {"id": 42, "url": f"https://api.github.com/{self.base}/releases/42",
                       "tag_name": "untagged-deadbeef", "target_commitish": self.commit,
                       "name": self.tag, "draft": True, "prerelease": False,
                       "body": "Release notes\n\n" + self.publisher.marker + "\n"}

    def asset(self, name, data=None, state="uploaded", identity=None):
        identity = identity or 100 + len(self.remote_files)
        data = self.payloads[name] if data is None else data
        metadata = {"id": identity, "url": f"https://api.github.com/{self.base}/releases/assets/{identity}",
                    "name": name, "size": len(data), "state": state, "download_count": 0}
        self.remote_files[name] = (metadata, data)

    def answer(self, command, returncode=0, data=None, stderr=b"", output=None):
        stdout = json.dumps(data).encode() if data is not None else b""
        if output is not None and not isinstance(output, int):
            output.write(stdout)
            stdout = None
        return subprocess.CompletedProcess(command, returncode, stdout, stderr)

    def gh_failure(self, command, status):
        return self.answer(command, 1, stderr=f"gh: token-that-must-never-appear (HTTP {status})".encode())

    def run_tool(self, command, **kwargs):
        if command[0] == "go":
            self.native_calls.append(command)
            if self.native_fail:
                raise subprocess.CalledProcessError(1, command, stderr=b"private native failure")
            if len(self.native_calls) == 3 and self.before_readback_verify:
                self.before_readback_verify()
            self.assertIn("--dist", command)
            self.assertEqual(command[:3], ["go", "run", "./cmd/release-tool"])
            if "verify-provenance" in command:
                self.assertEqual(command[command.index("--repo") + 1], self.repo)
                self.assertEqual(command[command.index("--tag") + 1], self.tag)
            return self.answer(command)
        self.assertEqual(command[:4], ["gh", "api", "--hostname", "github.com"])
        self.assertIn("X-GitHub-Api-Version: 2022-11-28", command)
        self.assertEqual(kwargs["timeout"], 180)
        self.assertEqual(kwargs["env"]["GH_HOST"], "github.com")
        self.assertEqual(kwargs["env"]["GH_DEBUG"], "")
        arguments = command[4:]
        options = {"-H", "--input", "--method"}
        endpoints = []
        index = 0
        while index < len(arguments):
            if arguments[index] in options:
                index += 2
            else:
                endpoints.append(arguments[index])
                index += 1
        self.assertEqual(len(endpoints), 1, command)
        endpoint = endpoints[0]
        method = arguments[arguments.index("--method") + 1] if "--method" in arguments else "GET"
        payload = json.loads(kwargs["input"]) if kwargs.get("input") else None
        self.calls.append((method, endpoint, copy.deepcopy(payload)))
        if self.api_override:
            override = self.api_override(command, method, endpoint, payload)
            if override is not None:
                return override
        if endpoint == "graphql":
            self.assertEqual(payload["variables"], {"owner": "arizzi74", "name": "MACT", "tag": self.tag})
            self.assertIn("release(tagName:$tag)", payload["query"])
            match = {"databaseId": self.remote["id"], "tagCommit": self.tag_commit} if self.remote else None
            return self.answer(command, data={"data": {"repository": {"nameWithOwner": self.repo, "release": match}}})
        if endpoint == self.base:
            return self.answer(command, data={"full_name": self.repo})
        if endpoint == f"{self.base}/git/ref/tags/{self.tag}":
            return self.answer(command, data={"ref": f"refs/tags/{self.tag}",
                                              "object": {"type": "commit", "sha": self.tag_sha}})
        if endpoint == f"{self.base}/releases" and method == "POST":
            self.assertIsNone(self.remote, "attempted duplicate release creation")
            self.draft()
            self.remote.update(payload)
            self.remote["tag_name"] = "untagged-deadbeef"
            if self.create_error:
                return self.gh_failure(command, self.create_error)
            return self.answer(command, data=self.remote)
        if endpoint == f"{self.base}/releases/42" and method == "GET":
            return self.answer(command, data=self.remote)
        if endpoint.startswith(f"{self.base}/releases/42/assets?per_page=100&page="):
            page = int(endpoint.rsplit("=", 1)[1])
            items = [metadata for metadata, _ in self.remote_files.values()]
            return self.answer(command, data=items[(page - 1) * 100:page * 100])
        upload = f"https://uploads.github.com/{self.base}/releases/42/assets?name="
        if endpoint.startswith(upload):
            self.assertEqual(method, "POST")
            name = endpoint.removeprefix(upload)
            self.assertNotIn(name, self.remote_files, "attempted to replace an asset")
            self.assertIn("Content-Type: application/octet-stream", command)
            self.assertIn(f"Content-Length: {len(self.payloads[name])}", command)
            self.assertEqual(Path(arguments[arguments.index("--input") + 1]).read_bytes(), self.payloads[name])
            errors = self.upload_errors.get(name, [])
            failure = errors.pop(0) if errors else None
            if failure:
                status, state = failure
                if state == "uploaded":
                    self.asset(name)
                elif state == "starter":
                    self.asset(name, b"", "starter")
                return self.gh_failure(command, status)
            self.asset(name)
            return self.answer(command, data=self.remote_files[name][0])
        if endpoint.startswith(f"{self.base}/releases/assets/"):
            identity = int(endpoint.rsplit("/", 1)[1])
            name = next(name for name, (metadata, _) in self.remote_files.items() if metadata["id"] == identity)
            metadata, data = self.remote_files[name]
            if method == "DELETE":
                self.assertTrue(self.remote["draft"])
                self.assertEqual((metadata["state"], metadata["size"]), ("starter", 0))
                self.assertEqual(self.calls[-2][:2], ("GET", f"{self.base}/releases/42"))
                del self.remote_files[name]
                return self.answer(command)
            if "Accept: application/octet-stream" in command:
                metadata["download_count"] += 1
                if self.download_mutation:
                    data = self.download_mutation(name, data)
                kwargs["stdout"].write(data)
                return subprocess.CompletedProcess(command, 0, None, b"")
            if self.before_starter_fetch:
                self.before_starter_fetch()
            return self.answer(command, data=metadata)
        if endpoint == f"{self.base}/releases/42" and method == "PATCH":
            self.assertEqual(payload, {"draft": False, "make_latest": "true"})
            self.assertTrue(self.remote["draft"])
            self.remote["draft"] = False
            self.remote["tag_name"] = self.tag
            if self.publish_error:
                return self.gh_failure(command, self.publish_error)
            return self.answer(command, data=self.remote)
        if endpoint == f"{self.base}/releases/latest":
            return self.answer(command, data=self.remote)
        self.fail("Unexpected tool call: " + repr(command))

    def invoke(self):
        with patch.object(POLICY.subprocess, "run", side_effect=self.run_tool), \
                patch.object(POLICY.time, "sleep") as sleep, contextlib.redirect_stdout(io.StringIO()):
            self.publisher.publish()
            return sleep

    def mutations(self):
        return [call for call in self.calls if call[0] in ("POST", "PATCH", "DELETE")]

    def uploads(self, name=None):
        return [call for call in self.calls if call[1].startswith("https://uploads.github.com/")
                and (name is None or call[1].endswith("?name=" + name))]

    def test_new_release_verifies_locally_uploads_individually_and_reads_back(self):
        self.invoke()
        self.assertFalse(self.remote["draft"])
        self.assertEqual(len(self.uploads()), len(self.payloads))
        self.assertEqual(len(self.native_calls), 4)
        self.assertTrue(all(metadata["download_count"] >= 1 for metadata, _ in self.remote_files.values()))
        self.assertEqual(self.calls[-1][1], f"{self.base}/releases/latest")

    def test_matching_owned_partial_draft_resumes_only_after_existing_bytes_verified(self):
        self.draft()
        self.asset("install.sh")
        self.invoke()
        self.assertEqual(self.uploads("install.sh"), [])
        self.assertEqual(len(self.uploads()), len(self.payloads) - 1)
        self.assertFalse(any(call[1] == f"{self.base}/releases" for call in self.mutations()))

    def test_published_wrong_target_tag_repo_marker_or_changed_draft_are_refused(self):
        for key, value in (("draft", False), ("tag_name", "v9.0.0"),
                           ("body", "another operator's draft"), ("name", "other"), ("prerelease", True),
                           ("url", "https://api.github.com/repos/other/repo/releases/42")):
            with self.subTest(key=key):
                self.setUp()
                self.draft()
                self.remote[key] = value
                with self.assertRaises(POLICY.PublishError):
                    self.invoke()
                self.assertEqual(self.mutations(), [])

    def test_actual_graphql_tag_commit_is_required_regardless_of_rest_target_metadata(self):
        self.draft()
        self.remote["target_commitish"] = "main"
        self.invoke()
        for commit in ({"oid": "b" * 40}, {}, {"oid": "invalid"}, "invalid"):
            with self.subTest(tag_commit=commit):
                self.setUp()
                self.draft()
                self.tag_commit = commit
                with self.assertRaises(POLICY.PublishError):
                    self.invoke()
                self.assertEqual(self.mutations(), [])

    def test_null_tag_commit_is_allowed_only_for_owned_draft_after_fresh_exact_tag_check(self):
        self.draft()
        self.tag_commit = None
        with patch.object(POLICY.subprocess, "run", side_effect=self.run_tool):
            self.publisher.draft()
        self.assertEqual(self.calls[-1][1], f"{self.base}/git/ref/tags/{self.tag}")
        self.assertEqual(self.mutations(), [])
        for problem in ("unowned", "wrong-tag", "missing-tag", "published"):
            with self.subTest(problem=problem):
                self.setUp()
                self.draft()
                self.tag_commit = None
                if problem == "unowned":
                    self.remote["body"] = "another operator's draft"
                elif problem == "wrong-tag":
                    self.tag_sha = "b" * 40
                elif problem == "missing-tag":
                    def missing(command, method, endpoint, payload):
                        if endpoint == f"{self.base}/git/ref/tags/{self.tag}":
                            return self.gh_failure(command, 404)
                    self.api_override = missing
                else:
                    self.remote["draft"] = False
                    self.remote["tag_name"] = self.tag
                with patch.object(POLICY.subprocess, "run", side_effect=self.run_tool), \
                        self.assertRaises(POLICY.PublishError):
                    self.publisher.check_release(self.publisher.release(), published=problem == "published")
                self.assertEqual(self.mutations(), [])
                if problem == "unowned":
                    self.assertFalse(any("/git/ref/" in endpoint for _, endpoint, _ in self.calls))

    def test_local_checksum_provenance_and_tag_failures_never_mutate_remote(self):
        for problem in ("checksum", "provenance", "tag"):
            with self.subTest(problem=problem):
                self.setUp()
                if problem == "checksum":
                    (self.dist / "install.sh").write_bytes(b"changed")
                elif problem == "provenance":
                    self.native_fail = True
                else:
                    self.tag_sha = "b" * 40
                with self.assertRaises(POLICY.PublishError):
                    self.invoke()
                self.assertEqual(self.mutations(), [])

    def test_annotated_tag_is_peeled_to_exact_commit_and_branch_is_not_a_tag(self):
        def annotated(command, method, endpoint, payload):
            if endpoint == f"{self.base}/git/ref/tags/{self.tag}":
                return self.answer(command, data={"ref": f"refs/tags/{self.tag}",
                                                  "object": {"type": "tag", "sha": "b" * 40}})
            if endpoint == f"{self.base}/git/tags/" + "b" * 40:
                return self.answer(command, data={"sha": "b" * 40,
                                                  "object": {"type": "commit", "sha": self.commit}})
        self.api_override = annotated
        self.invoke()
        self.setUp()
        def branch(command, method, endpoint, payload):
            if endpoint == f"{self.base}/git/ref/tags/{self.tag}":
                return self.answer(command, data={"ref": f"refs/heads/{self.tag}",
                                                  "object": {"type": "commit", "sha": self.commit}})
        self.api_override = branch
        with self.assertRaises(POLICY.PublishError):
            self.invoke()
        self.assertEqual(self.mutations(), [])

    def test_transient_upload_retries_are_bounded_and_nontransient_errors_stop(self):
        for status, attempts in ((408, 3), (502, 3), (503, 3), (403, 1), (422, 1)):
            with self.subTest(status=status):
                self.setUp()
                self.upload_errors["SHA256SUMS"] = [(status, None)] * 4
                with patch.object(POLICY.time, "sleep") as sleep, self.assertRaises(POLICY.GhError):
                    self.invoke()
                self.assertEqual(len(self.uploads("SHA256SUMS")), attempts)
                self.assertTrue(self.remote["draft"])
                self.assertFalse(any(call[0] == "PATCH" for call in self.calls))

    def test_ambiguous_upload_success_is_downloaded_and_never_uploaded_twice(self):
        self.upload_errors["SHA256SUMS"] = [(408, "uploaded")]
        self.invoke()
        self.assertEqual(len(self.uploads("SHA256SUMS")), 1)

    def test_empty_owned_starter_after_timeout_is_removed_before_bounded_retry(self):
        self.upload_errors["SHA256SUMS"] = [(408, "starter")]
        sleep = self.invoke()
        self.assertEqual(len(self.uploads("SHA256SUMS")), 2)
        self.assertEqual(len([call for call in self.calls if call[0] == "DELETE"]), 1)
        sleep.assert_called_once_with(2)

    def test_resumed_starter_cleanup_refuses_state_or_draft_changes(self):
        for problem in ("uploaded", "published", "tag"):
            with self.subTest(problem=problem):
                self.setUp()
                self.draft()
                self.asset("SHA256SUMS", b"", "starter")
                def change():
                    if problem == "uploaded":
                        self.remote_files["SHA256SUMS"][0]["state"] = "uploaded"
                    elif problem == "published":
                        self.remote["draft"] = False
                    else:
                        self.tag_sha = "b" * 40
                self.before_starter_fetch = change
                with self.assertRaises(POLICY.PublishError):
                    self.invoke()
                self.assertEqual(self.mutations(), [])

    def test_ambiguous_create_and_publish_success_do_not_repeat_mutations(self):
        self.create_error, self.publish_error = 502, 408
        self.invoke()
        self.assertEqual(len([call for call in self.calls if call[1] == f"{self.base}/releases"]), 1)
        self.assertEqual(len([call for call in self.calls if call[0] == "PATCH"]), 1)

    def test_existing_mismatched_or_unexpected_assets_are_preserved_without_uploads(self):
        for problem in ("bytes", "size", "unexpected", "starter-nonempty"):
            with self.subTest(problem=problem):
                self.setUp()
                self.draft()
                self.asset("install.sh", b"X" * len(self.payloads["install.sh"]) if problem == "bytes"
                           else b"wrong size" if problem == "size" else None)
                if problem == "unexpected":
                    self.remote_files["install.sh"][0]["name"] = "unrelated.txt"
                if problem == "starter-nonempty":
                    self.remote_files["install.sh"][0]["state"] = "starter"
                with self.assertRaises(POLICY.PublishError):
                    self.invoke()
                self.assertEqual(self.mutations(), [])

    def test_corrupt_final_download_and_asset_changes_after_readback_prevent_publish(self):
        for problem in ("bytes", "identity"):
            with self.subTest(problem=problem):
                self.setUp()
                if problem == "bytes":
                    self.download_mutation = lambda name, data: b"X" * len(data)
                else:
                    def change():
                        self.asset("install.sh", identity=999)
                    self.before_readback_verify = change
                with self.assertRaises(POLICY.PublishError):
                    self.invoke()
                self.assertFalse(any(call[0] == "PATCH" for call in self.calls))

    def test_gh_errors_and_timeouts_never_print_credentials_and_retry_only_transients(self):
        for status, transient in ((408, True), (429, True), (500, True), (403, False), (422, False)):
            result = subprocess.CompletedProcess([], 1, b"", f"private-token (HTTP {status})".encode())
            with self.subTest(status=status), patch.object(POLICY.subprocess, "run", return_value=result):
                with self.assertRaises(POLICY.GhError) as error:
                    POLICY.gh([self.base])
                self.assertEqual(error.exception.transient, transient)
                self.assertNotIn("private-token", str(error.exception))
        with patch.object(POLICY.subprocess, "run", side_effect=subprocess.TimeoutExpired([], 180)):
            with self.assertRaises(POLICY.GhError) as error:
                POLICY.gh([self.base])
            self.assertTrue(error.exception.transient)

    def test_workflow_pins_repository_tag_commit_and_dist_as_explicit_inputs(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text().split("  publish:\n", 1)[1]
        self.assertIn("GH_REPO: ${{ github.repository }}", workflow)
        self.assertIn("RELEASE_TAG: ${{ github.ref_name }}", workflow)
        self.assertIn("RELEASE_COMMIT: ${{ github.sha }}", workflow)
        self.assertIn('python3 scripts/publish-release.py --repo "$GH_REPO" --tag "$RELEASE_TAG" --commit "$RELEASE_COMMIT" --dist dist', workflow)
        self.assertNotIn("--clobber", workflow)


class InstalledGhTransportTest(unittest.TestCase):
    @unittest.skipUnless(shutil.which("gh"), "GitHub CLI is unavailable")
    def test_raw_upload_content_length_json_and_empty_delete_use_real_local_http(self):
        requests = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.connection.settimeout(5)
                length = self.headers.get("Content-Length")
                if length is None or self.headers.get("Transfer-Encoding"):
                    self.send_error(400)
                    return
                requests.append((self.path, dict(self.headers), self.rfile.read(int(length))))
                self.send_response(201)
                self.end_headers()
                self.wfile.write(b'{"id":42}')

            def do_DELETE(self):
                requests.append((self.path, dict(self.headers), b""))
                self.send_response(204)
                self.end_headers()

            def log_message(self, *_):
                pass

        with tempfile.TemporaryDirectory(prefix="mact-gh-http-tests-") as directory:
            server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                fixture = Path(directory) / "asset"
                payload = b"synthetic local binary\x00" * 16384
                fixture.write_bytes(payload)
                # Drop all inherited tokens/config, and use only an inert fixture token.
                env = {"PATH": os.environ["PATH"], "HOME": directory, "GH_CONFIG_DIR": directory,
                       "GH_TOKEN": "local-fixture", "GH_PROMPT_DISABLED": "1"}
                base = [shutil.which("gh"), "api", "--hostname", "github.com"]
                endpoint = f"http://127.0.0.1:{server.server_port}/asset"
                result = subprocess.run([*base, "--method", "POST", "-H", "Content-Type: application/octet-stream",
                                         "-H", f"Content-Length: {len(payload)}", endpoint, "--input", str(fixture)],
                                        env=env, capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout), {"id": 42})
                self.assertEqual(requests[-1][2], payload)
                self.assertEqual(requests[-1][1]["Content-Length"], str(len(payload)))
                self.assertNotIn("Transfer-Encoding", requests[-1][1])
                body = b'{"draft":false,"make_latest":"true"}'
                result = subprocess.run([*base, "--method", "POST", "-H", "Content-Type: application/json",
                                         "-H", f"Content-Length: {len(body)}",
                                         "--input", "-", endpoint], input=body,
                                        env=env, capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(requests[-1][2], body)
                self.assertEqual(requests[-1][1]["Content-Type"], "application/json")
                result = subprocess.run([*base, "--method", "DELETE", endpoint],
                                        env=env, capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, b"")
            finally:
                server.shutdown()
                thread.join(timeout=5)
                server.server_close()


if __name__ == "__main__":
    unittest.main()
