#!/usr/bin/env python3
"""Test focused release authorization in isolated Git repositories."""

import importlib.util
from pathlib import Path
import re
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("installer_release", ROOT / "scripts/check-installer-release.py")
POLICY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(POLICY)


class InstallerReleaseScopeTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="mact-installer-scope-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.git("init", "-q")
        self.git("config", "user.name", "Installer fixture")
        self.git("config", "user.email", "installer@example.invalid")
        self.write("README.md", "Original installer guidance\n")
        self.write("internal/worker/worker.go", "unchanged runtime\n")
        self.commit("baseline")
        self.baseline, self.tag = "v1.2.3", "v1.2.4"
        self.git("tag", self.baseline)

    def git(self, *arguments):
        return subprocess.check_output(["git", *arguments], cwd=self.root, stderr=subprocess.DEVNULL).decode().strip()

    def write(self, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def commit(self, message):
        self.git("add", ".")
        self.git("commit", "-qm", message)
        return self.git("rev-parse", "HEAD")

    def target(self, changes=None):
        for name in changes or ("README.md",):
            self.write(name, "Updated installer guidance\n")
        self.sha = self.commit("installer update")
        self.git("tag", self.tag)

    def validate(self, **arguments):
        return POLICY.validate(**{"tag": self.tag, "baseline": self.baseline, "commit": self.sha,
                                  "latest": self.baseline, "root": self.root, **arguments})

    def test_all_explicit_installer_files_and_target_release_notes_are_allowed(self):
        changed = POLICY.ALLOWED | {f"docs/releases/{self.tag}.md"}
        self.target(changed)
        self.assertEqual(self.validate(), changed)

    def test_runtime_dependencies_security_publisher_and_unrelated_docs_are_refused(self):
        for name in ("internal/worker/worker.go", "go.mod", "package-lock.json", "scripts/publish-release.py",
                     ".github/workflows/security.yml", "docs/operations.md", "docs/releases/v1.2.3.md"):
            with self.subTest(path=name):
                self.setUp()
                self.target(("README.md", name))
                with self.assertRaises(POLICY.ScopeError):
                    self.validate()

    def test_invalid_or_nonadvancing_stable_tags_are_refused(self):
        self.target()
        for tag in ("main", "v1.2.4-rc1", "v01.2.4", "v1.2.4\n", self.baseline, "v0.9.9"):
            with self.subTest(tag=tag), self.assertRaises(POLICY.ScopeError):
                self.validate(tag=tag)
        for baseline in ("main", "v1.2.3-rc1", "v01.2.3", "v1.2.3\n", ""):
            with self.subTest(baseline=baseline), self.assertRaises(POLICY.ScopeError):
                self.validate(baseline=baseline, latest=baseline)

    def test_baseline_must_exist_and_be_latest_published_release(self):
        self.target()
        with self.assertRaises(POLICY.ScopeError):
            self.validate(latest="v1.2.2")
        with self.assertRaises(subprocess.CalledProcessError):
            self.validate(baseline="v1.2.2", latest="v1.2.2")

    def test_nonancestor_baseline_is_refused(self):
        self.git("checkout", "--orphan", "unrelated")
        self.target()
        with self.assertRaises(subprocess.CalledProcessError):
            self.validate()

    def test_expected_commit_head_and_actual_target_tag_must_match(self):
        self.target()
        with self.assertRaises(POLICY.ScopeError):
            self.validate(commit="a" * 40)
        self.write("README.md", "Another installer change\n")
        new_head = self.commit("after tag")
        with self.assertRaises(POLICY.ScopeError):
            self.validate(commit=new_head)

    def test_annotated_tags_are_accepted_and_missing_target_or_empty_diff_refused(self):
        self.target()
        self.git("tag", "-a", "v1.2.5", "-m", "annotated installer release")
        self.validate(tag="v1.2.5")
        with self.assertRaises(subprocess.CalledProcessError):
            self.validate(tag="v1.2.6")
        self.git("tag", "v1.2.7", f"refs/tags/{self.baseline}")
        with self.assertRaises(POLICY.ScopeError):
            self.validate(tag="v1.2.7", commit=self.git("rev-parse", f"refs/tags/{self.baseline}"))

    def test_allowed_name_cannot_hide_symlink_or_rename_from_runtime_path(self):
        self.write("README.md", "New guidance\n")
        (self.root / "scripts").mkdir()
        (self.root / "scripts/install.sh").symlink_to("../internal/worker/worker.go")
        self.sha = self.commit("unsafe indirection")
        self.git("tag", self.tag)
        with self.assertRaises(POLICY.ScopeError):
            self.validate()
        self.setUp()
        self.git("mv", "internal/worker/worker.go", "README.runtime")
        self.sha = self.commit("runtime rename")
        self.git("tag", self.tag)
        with self.assertRaises(POLICY.ScopeError):
            self.validate()


class InstallerReleaseWorkflowTest(unittest.TestCase):
    def test_manual_scope_proof_gates_both_focused_mode_and_normal_validation(self):
        text = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("github.event_name == 'workflow_dispatch' && inputs.installer_only", text)
        self.assertIn('test "$RELEASE_REF" = "refs/tags/$RELEASE_TAG"', text)
        proof = text.index("python3 scripts/check-installer-release.py --repo")
        self.assertLess(proof, text.index("printf 'installer_only=true"))
        sections = re.split(r"^  ([a-z][a-z-]+):\n", text.split("jobs:\n", 1)[1], flags=re.MULTILINE)
        jobs = dict(zip(sections[1::2], sections[2::2]))
        for name in ("go-checks", "native-session-archive", "browser"):
            self.assertIn("needs: release-mode", jobs[name])
            self.assertIn("if: needs.release-mode.outputs.installer_only != 'true'", jobs[name])
        self.assertIn("if: needs.release-mode.outputs.installer_only == 'true'", jobs["installer-tests"])
        gate = jobs["test-and-build"]
        self.assertIn("always() && !cancelled()", gate)
        for name in ("release-mode", "package", "security", "installer-tests", "go-checks", "browser", "native-session-archive"):
            self.assertIn(f"needs.{name}.result == 'success'", gate)
        for name in ("go-checks", "browser", "native-session-archive", "installer-tests"):
            self.assertIn(f"needs.{name}.result == 'skipped'", gate)
        self.assertIn("needs.release-mode.outputs.installer_only == 'false'", gate)
        self.assertIn("needs.release-mode.outputs.installer_only == 'true'", gate)
        self.assertIn("verify-provenance", gate)
        for name in ("native-whisper", "native-speech"):
            self.assertIn("needs: release-mode", jobs[name])
            self.assertIn("run-smoke-tests: ${{ needs.release-mode.outputs.installer_only != 'true' }}", jobs[name])

    def test_native_smoke_defaults_and_cache_admission_stay_strict(self):
        for name in ("native-whisper", "native-speech"):
            text = (ROOT / f".github/workflows/{name}.yml").read_text()
            self.assertEqual(len(re.findall(r"run-smoke-tests:\n(?:[^\n]*\n){1,3}        default: true", text)), 2)
            self.assertIn("inputs.run-smoke-tests && steps.native-cache.outputs.cache-hit != 'true'", text)
            # Source inventory audits remain unconditional; only inference steps opt out.
            self.assertNotRegex(text, r"Audit[^\n]*\n        if:")


if __name__ == "__main__":
    unittest.main()
