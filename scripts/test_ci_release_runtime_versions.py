#!/usr/bin/env python3
"""Exercise the packaged gateway version guard with executable archive fixtures."""

import io
import os
from pathlib import Path
import platform
import shlex
import subprocess
import tarfile
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[1]
SCRIPT = REPO / "scripts/verify-release-runtime-versions.sh"
PROGRAMS = ("codex-gateway", "codex-telegramgw", "scripts/release-manager.py", "codex-whisper")
ARCHITECTURES = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}


@unittest.skipUnless(platform.system() == "Linux" and platform.machine() in ARCHITECTURES,
                     "the package runtime guard executes native Linux programs")
class ReleaseRuntimeVersionsTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="mact-release-version-tests-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.dist = self.root / "release files"
        self.dist.mkdir()
        self.probes = self.root / "version probes"
        self.env = {**os.environ, "MACT_RELEASE_VERSION_PROBES": str(self.probes)}

    def package(self, overrides=None, missing=None, nonexecutable=None, symlink=None):
        versions = dict.fromkeys(PROGRAMS, "1.2.3")
        versions.update(overrides or {})
        path = self.dist / f"codex-gateway-linux-{ARCHITECTURES[platform.machine()]}.tar.gz"
        with tarfile.open(path, "w:gz", format=tarfile.USTAR_FORMAT) as archive:
            for name, version in versions.items():
                if name == missing:
                    continue
                header = tarfile.TarInfo(name)
                header.mode = 0o600 if name == nonexecutable else 0o700
                if name == symlink:
                    header.type = tarfile.SYMTYPE
                    header.linkname = "codex-gateway"
                    archive.addfile(header)
                    continue
                payload = ("#!/bin/sh\n"
                           '[ "$#" -eq 1 ] && [ "$1" = version ] || exit 2\n'
                           f"printf '%s\\n' {shlex.quote(name)} >> \"$MACT_RELEASE_VERSION_PROBES\"\n"
                           f"printf '%s\\n' {shlex.quote(version)}\n").encode()
                header.size = len(payload)
                archive.addfile(header, io.BytesIO(payload))

    def run_guard(self, expected="v1.2.3", dist=None, script=SCRIPT):
        return subprocess.run(["bash", str(script), expected, str(dist or self.dist)],
                              cwd=self.root, env=self.env, capture_output=True, text=True, timeout=20)

    def test_matching_release_executes_every_packaged_program(self):
        self.package()
        result = self.run_guard()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertCountEqual(self.probes.read_text().splitlines(), PROGRAMS)

    def test_rejects_native_whisper_and_wrong_gateway_or_manager_aliases(self):
        for name in PROGRAMS:
            with self.subTest(program=name):
                self.probes.unlink(missing_ok=True)
                self.package({name: "native-95746b28cb7752922528c112" if name == "codex-whisper" else "1.2.2"})
                result = self.run_guard()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(name, result.stdout + result.stderr)

    def test_nonrelease_ci_versions_compare_as_exact_strings(self):
        for expected in ("dev", "d00d59ab02d36270c19975e0154ae846f656aa91"):
            with self.subTest(expected=expected):
                self.package(dict.fromkeys(PROGRAMS, expected))
                result = self.run_guard(expected)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.package({**dict.fromkeys(PROGRAMS, expected), "codex-whisper": expected + "-other"})
                self.assertNotEqual(self.run_guard(expected).returncode, 0)

    def test_rejects_missing_nonexecutable_or_symlinked_required_programs(self):
        for problem in ("missing", "nonexecutable", "symlink"):
            with self.subTest(problem=problem):
                self.package(**{problem: "codex-whisper"})
                result = self.run_guard()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_expected_version_cannot_execute_shell_syntax(self):
        marker = self.root / "injected-command-ran"
        for injected in (f"$(touch {shlex.quote(str(marker))})",
                         f"1.2.3'; touch {shlex.quote(str(marker))}; #"):
            with self.subTest(expected=injected):
                self.package(dict.fromkeys(PROGRAMS, injected))
                result = self.run_guard(injected)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(marker.exists(), result.stdout + result.stderr)

    def test_explicit_relative_dist_directory_uses_caller_working_directory(self):
        self.package()
        result = self.run_guard(dist=Path("release files"))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
