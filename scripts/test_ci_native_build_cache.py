#!/usr/bin/env python3
"""Exercise native cache invalidation and audits without native model downloads."""

import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch


REPO = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("native_build_cache", REPO / "scripts/native-build-cache.py")
CACHE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CACHE)


class NativeBuildCacheTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.root_patch = patch.object(CACHE, "ROOT", self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)
        for relative in ("scripts/native-build-cache.py", "scripts/test_ci_native_build_cache.py", "go.mod", "go.sum",
                         ".github/workflows/native-whisper.yml", ".github/workflows/native-speech.yml"):
            self.write(relative, (REPO / relative).read_bytes())
        for component in ("whisper", "piper", "qwen-tts"):
            shutil.copytree(REPO / "native" / component, self.root / "native" / component)
            for suffix in (".sh", "-linux.sh"):
                relative = f"scripts/build-{component}{suffix}"
                self.write(relative, (REPO / relative).read_bytes())
        self.version = "native-0123456789abcdef01234567"
        self.key = "native-test-exact-key"

    def write(self, relative, data):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        return path

    def record(self, group):
        CACHE.manifest_path(group).write_text(json.dumps({
            "key": self.key, "version": self.version, "files": CACHE.inventory(group)}))

    def helper(self, relative):
        path = self.write(relative, f"#!/bin/sh\nprintf '%s\\n' '{self.version}'\n".encode())
        path.chmod(0o755)
        return path

    def whisper(self):
        helper = self.helper("dist/whisper/codex-whisper-linux-amd64")
        self.record("whisper")
        return helper

    def speech(self):
        for component in ("piper", "qwen-tts"):
            shutil.rmtree(self.root / "dist" / component, ignore_errors=True)
            stage = self.root / "dist" / component / "linux-amd64"
            self.helper(f"dist/{component}/linux-amd64/codex-{component}")
            if component == "qwen-tts":
                self.helper(f"dist/{component}/linux-amd64/lib/codex-qwen-tts-optimized")
            else:
                self.write(f"dist/{component}/linux-amd64/lib/runtime.so", b"pinned shared runtime")
            for name in ("NOTICE.md", "source.env"):
                self.write(f"dist/{component}/linux-amd64/{name}", (self.root / "native" / component / name).read_bytes())
            self.write(f"dist/{component}/linux-amd64/licenses/LICENSE", b"test license")
            inventory = "".join(f"{CACHE.digest_file(path)}  {path.relative_to(stage).as_posix()}\n"
                                for path in sorted(stage.rglob("*")) if path.is_file())
            self.write(f"dist/{component}/linux-amd64/SHA256SUMS", inventory.encode())
            self.repack(component)
        self.record("speech")

    def repack(self, component, extra=None, corrupt=None):
        root = self.root / "dist" / component
        stage = root / "linux-amd64"
        with tarfile.open(root / f"codex-{component}-linux-amd64.tar.gz", "w:gz", format=tarfile.USTAR_FORMAT) as archive:
            for path in sorted(stage.rglob("*")):
                name = path.relative_to(stage).as_posix()
                if name == corrupt:
                    info = archive.gettarinfo(str(path), arcname=name)
                    archive.addfile(info, io.BytesIO(b"x" * info.size))
                else:
                    archive.add(path, arcname=name, recursive=False)
            if extra:
                info = tarfile.TarInfo(extra)
                info.size = 4
                archive.addfile(info, io.BytesIO(b"evil"))

    def test_release_version_does_not_invalidate_native_identity(self):
        runtime = {"compiler": "pinned compiler", "runner_image": "20261025"}
        with patch.dict(os.environ, VERSION="1.0.0"):
            first = CACHE.cache_identity("whisper", "linux", "amd64", runtime)
        with patch.dict(os.environ, VERSION="2.0.0"):
            self.assertEqual(first, CACHE.cache_identity("whisper", "linux", "amd64", runtime))
        self.assertRegex(first[0], r"^native-whisper-v1-linux-amd64-[0-9a-f]{64}$")
        self.assertRegex(first[1], r"^native-[0-9a-f]{24}$")

    def test_relevant_sources_pins_scripts_tests_and_toolchains_invalidate(self):
        for group, relative in (
            ("whisper", "native/whisper/main.cpp"), ("whisper", "native/whisper/source.env"),
            ("whisper", "native/whisper/smoke.py"), ("whisper", "native/whisper/protocol_test.cpp"),
            ("whisper", "native/whisper/licenses/whisper.cpp-MIT.txt"),
            ("speech", "native/piper/smoke/main.go"), ("speech", "native/qwen-tts/threads.patch"),
            ("speech", "native/qwen-tts/stock-giovanni.wav"),
            ("speech", "scripts/build-piper-linux.sh"), ("speech", "scripts/build-qwen-tts.sh"),
            ("speech", ".github/workflows/native-speech.yml"), ("whisper", "scripts/native-build-cache.py"),
            ("whisper", "scripts/test_ci_native_build_cache.py"), ("speech", "go.sum"),
        ):
            with self.subTest(path=relative):
                before = CACHE.cache_identity(group, "linux", "amd64", {"compiler": "one"})
                path = self.root / relative
                original = path.read_bytes()
                path.write_bytes(original + b"\nchanged input\n")
                self.assertNotEqual(before, CACHE.cache_identity(group, "linux", "amd64", {"compiler": "one"}))
                path.write_bytes(original)
                self.assertNotEqual(before[0], CACHE.cache_identity(group, "linux", "amd64", {"compiler": "two"})[0])
                self.assertNotEqual(before[0], CACHE.cache_identity(group, "darwin", "amd64", {"compiler": "one"})[0])
                self.assertNotEqual(before[0], CACHE.cache_identity(group, "linux", "arm64", {"compiler": "one"})[0])

    def test_unrelated_gateway_change_preserves_native_identity(self):
        before = CACHE.source_digest("speech")
        self.write("internal/gateway/unrelated.go", b"package gateway\n")
        self.assertEqual(before, CACHE.source_digest("speech"))

    def test_whisper_and_speech_sources_have_independent_cache_scopes(self):
        whisper, speech = CACHE.source_digest("whisper"), CACHE.source_digest("speech")
        self.write("native/qwen-tts/new-source.cpp", b"// new native source\n")
        self.assertEqual(whisper, CACHE.source_digest("whisper"))
        self.assertNotEqual(speech, CACHE.source_digest("speech"))

    def test_symlinked_native_source_directories_fail_closed(self):
        (self.root / "native/whisper/linked").symlink_to(self.root / "scripts", target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "must not be a symlink"):
            CACHE.source_digest("whisper")

    def test_toolchain_probe_uses_pinned_images_and_actual_install_commands(self):
        for component in ("whisper", "piper", "qwen-tts"):
            for arch in ("amd64", "arm64"):
                with self.subTest(component=component, arch=arch), patch.object(CACHE, "run", return_value="compiler and packages") as command:
                    identity = CACHE.container_toolchain(component, arch)
                    self.assertIn("@sha256:", identity["image"])
                    args = command.call_args.args
                    self.assertEqual(args[:5], ("docker", "run", "--rm", "--platform", f"linux/{arch}"))
                    self.assertIn("cmake --version", args[-1])
                    self.assertIn("apt-get install" if component == "piper" else "apk add", args[-1])
                    self.assertIn("dpkg-query -W" if component == "piper" else "apk info -vv", args[-1])

    def test_unknown_toolchain_wrapper_fails_closed(self):
        (self.root / "scripts/build-whisper-linux.sh").write_text("docker run alpine:latest\n")
        with self.assertRaisesRegex(ValueError, "unrecognized pinned toolchain"):
            CACHE.container_toolchain("whisper", "amd64")

    def test_runner_identity_is_required_in_actions(self):
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "ImageVersion": ""}):
            with self.assertRaisesRegex(ValueError, "ImageVersion"):
                CACHE.runtime_identity("whisper", "linux", "amd64")

    def test_whisper_inventory_and_version_are_rechecked(self):
        helper = self.whisper()
        CACHE.verify("whisper", "linux", "amd64", self.key, self.version)
        with self.assertRaisesRegex(ValueError, "identity or inventory mismatch"):
            CACHE.verify("whisper", "linux", "amd64", "different-key", self.version)
        helper.write_text("#!/bin/sh\necho wrong-version\n")
        with self.assertRaisesRegex(ValueError, "identity or inventory mismatch"):
            CACHE.verify("whisper", "linux", "amd64", self.key, self.version)
        self.record("whisper")
        with self.assertRaisesRegex(ValueError, "version mismatch"):
            CACHE.verify("whisper", "linux", "amd64", self.key, self.version)

    def test_missing_extra_and_mode_changed_outputs_are_rejected(self):
        helper = self.whisper()
        helper.chmod(0o644)
        with self.assertRaisesRegex(ValueError, "inventory mismatch"):
            CACHE.verify("whisper", "linux", "amd64", self.key, self.version)
        helper.chmod(0o755)
        extra = self.write("dist/whisper/unexpected", b"extra")
        self.record("whisper")
        with self.assertRaisesRegex(ValueError, "unexpected Whisper"):
            CACHE.verify("whisper", "linux", "amd64", self.key, self.version)
        extra.unlink()
        helper.unlink()
        with self.assertRaisesRegex(ValueError, "empty native"):
            CACHE.verify("whisper", "linux", "amd64", self.key, self.version)

    def test_output_symlinks_are_rejected(self):
        helper = self.whisper()
        helper.unlink()
        helper.symlink_to(self.root / "go.mod")
        with self.assertRaisesRegex(ValueError, "non-regular"):
            CACHE.inventory("whisper")

    def test_speech_archive_checksum_and_inventory_audits_run_on_hits(self):
        self.speech()
        CACHE.verify("speech", "linux", "amd64", self.key, self.version)
        self.write("dist/piper/linux-amd64/lib/runtime.so", b"mutated pinned runtime")
        self.record("speech")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            CACHE.verify("speech", "linux", "amd64", self.key, self.version)

    def test_speech_archives_must_match_staged_validated_files(self):
        self.speech()
        self.repack("piper", corrupt="NOTICE.md")
        self.record("speech")
        with self.assertRaisesRegex(ValueError, "archive differs from validated build"):
            CACHE.verify("speech", "linux", "amd64", self.key, self.version)

    def test_stale_source_notices_are_rejected(self):
        self.speech()
        self.write("dist/piper/linux-amd64/NOTICE.md", b"stale notice")
        self.record("speech")
        with self.assertRaisesRegex(ValueError, "stale piper NOTICE"):
            CACHE.verify("speech", "linux", "amd64", self.key, self.version)

    def test_unsafe_or_duplicate_archive_members_are_rejected(self):
        for name in ("../escape", "/absolute", "codex-piper"):
            with self.subTest(name=name):
                self.speech()
                self.repack("piper", extra=name)
                self.record("speech")
                with self.assertRaisesRegex(ValueError, "invalid piper archive member"):
                    CACHE.verify("speech", "linux", "amd64", self.key, self.version)

    def test_nonexact_cache_cleanup_allows_clean_piper_build(self):
        self.speech()
        with patch.object(sys, "argv", ["native-build-cache.py", "clean", "speech", "--os", "linux", "--arch", "amd64"]):
            CACHE.main()
        self.assertFalse((self.root / "dist/piper").exists())
        self.assertFalse((self.root / "dist/qwen-tts").exists())
        self.assertTrue((self.root / "native/piper").is_dir())


if __name__ == "__main__":
    unittest.main()
