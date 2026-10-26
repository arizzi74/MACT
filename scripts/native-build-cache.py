#!/usr/bin/env python3
"""Identify and audit native build caches; real speech smoke tests still run.

The version describes native source content, independent of gateway release tags.
The cache key additionally identifies the actual hosted runner and toolchains.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import stat
import subprocess
import tarfile


ROOT = Path(__file__).resolve().parents[1]
GROUPS = {"whisper": ("whisper",), "speech": ("piper", "qwen-tts")}
SCHEMA = 1


def run(*args):
    return subprocess.check_output(args, cwd=ROOT, text=True).strip()


def digest_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def source_digest(group):
    paths = {
        Path("scripts/native-build-cache.py"),
        Path("scripts/test_ci_native_build_cache.py"),
        Path(f".github/workflows/native-{group}.yml"),
        Path("go.mod"), Path("go.sum"),
    }
    for component in GROUPS[group]:
        # Include fixtures, licenses, upstream/model pins, smoke and C++ tests.
        for path in (ROOT / "native" / component).rglob("*"):
            if "__pycache__" in path.parts:
                continue
            if path.is_symlink():
                raise ValueError(f"native cache input must not be a symlink: {path}")
            if path.is_file():
                paths.add(path.relative_to(ROOT))
        paths.update((Path(f"scripts/build-{component}.sh"), Path(f"scripts/build-{component}-linux.sh")))
    digest = hashlib.sha256()
    for path in sorted(paths):
        full = ROOT / path
        if full.is_symlink() or not full.is_file():
            raise ValueError(f"native cache input must be a regular file: {path}")
        digest.update(f"{path.as_posix()}\0{stat.S_IMODE(full.stat().st_mode):o}\0{digest_file(full)}\n".encode())
    return digest.hexdigest()


def container_toolchain(component, arch):
    """Probe the build wrapper's pinned image and actual apt/apk package versions.

    The wrappers install from mutable package repositories. An image digest alone
    does not identify the compiler. Read their installation command so a wrapper
    change cannot silently leave this probe using a different dependency list.
    """
    script = (ROOT / f"scripts/build-{component}-linux.sh").read_text()
    image_match = re.search(rf"^  {arch}\) image='([^']+)'", script, re.MULTILINE)
    install_match = re.search(r"(?:sh|bash) -ec '(.+?); bash scripts/build-", script)
    if not image_match or not install_match or "@sha256:" not in image_match[1]:
        raise ValueError(f"unrecognized pinned toolchain wrapper: {component}")
    image, install = image_match[1], install_match[1]
    packages = "apk info -vv | LC_ALL=C sort" if image.startswith("alpine@") else "dpkg-query -W"
    probe = f"set -eu; ({install}) >/dev/null; {packages}; cc --version; c++ --version; cmake --version"
    # Keep first-use pull messages outside the toolchain fingerprint.
    run("docker", "pull", "--quiet", "--platform", f"linux/{arch}", image)
    return {"image": image, "packages_and_tools": run(
        "docker", "run", "--rm", "--platform", f"linux/{arch}", "--pull=never", image, "sh", "-ec", probe)}


def runtime_identity(group, target_os, arch):
    if os.environ.get("GITHUB_ACTIONS") == "true" and not os.environ.get("ImageVersion"):
        raise ValueError("hosted runner ImageVersion is required for native caches")
    identity = {
        "os": target_os, "arch": arch,
        "runner_image": {name: os.environ.get(name, "") for name in ("ImageOS", "ImageVersion", "RUNNER_OS", "RUNNER_ARCH")},
        "kernel": run("uname", "-srv", "-m"),
        "python": platform.python_version(),
        "build_env": {name: os.environ.get(name, "") for name in (
            "CC", "CXX", "CFLAGS", "CXXFLAGS", "CPPFLAGS", "LDFLAGS", "BUILD_JOBS",
            "MACOSX_DEPLOYMENT_TARGET", "SDKROOT", "DEVELOPER_DIR")},
    }
    if group == "speech":
        identity["go"] = run("go", "version")
    if target_os == "linux":
        identity["toolchains"] = {component: container_toolchain(component, arch) for component in GROUPS[group]}
    else:
        identity["toolchain"] = {name: run(*command) for name, command in {
            "compiler": ("c++", "--version"), "cmake": ("cmake", "--version"),
            "xcode": ("xcodebuild", "-version"), "sdk": ("xcrun", "--show-sdk-version"),
            "linker": ("xcrun", "ld", "-version_details"), "os": ("sw_vers",),
        }.items()}
    return identity


def cache_identity(group, target_os, arch, runtime):
    source = source_digest(group)
    identity = {"schema": SCHEMA, "group": group, "source": source, "runtime": runtime}
    digest = hashlib.sha256(json.dumps(identity, sort_keys=True).encode()).hexdigest()
    return f"native-{group}-v{SCHEMA}-{target_os}-{arch}-{digest}", f"native-{source[:24]}"


def roots(group):
    return [ROOT / "dist" / component for component in GROUPS[group]]


def manifest_path(group):
    return roots(group)[0] / ".cache-manifest.json"


def inventory(group):
    entries = {}
    for root in roots(group):
        if root.is_symlink() or not root.is_dir():
            raise ValueError(f"missing regular native output directory: {root}")
        for path in sorted(root.rglob("*")):
            if path == manifest_path(group):
                continue
            info = path.lstat()
            if stat.S_ISDIR(info.st_mode):
                continue
            if not stat.S_ISREG(info.st_mode):
                raise ValueError(f"non-regular native output: {path}")
            entries[path.relative_to(ROOT).as_posix()] = {
                "sha256": digest_file(path), "mode": stat.S_IMODE(info.st_mode), "size": info.st_size}
    if not entries:
        raise ValueError("empty native build output")
    return entries


def verify_archive(component, target_os, arch):
    root = ROOT / "dist" / component
    stage = root / f"{target_os}-{arch}"
    binary = f"codex-{component}"
    expected = {path.relative_to(stage).as_posix(): path for path in stage.rglob("*") if path.is_file()}
    required = {binary, "NOTICE.md", "source.env", "SHA256SUMS"}
    if component == "qwen-tts":
        required.add("lib/codex-qwen-tts-optimized")
    if not required <= expected.keys() or not any(name.startswith("licenses/") for name in expected):
        raise ValueError(f"incomplete {component} package")
    if any(name not in required and not name.startswith(("licenses/", "lib/")) for name in expected):
        raise ValueError(f"unexpected {component} package file")
    for name in ("NOTICE.md", "source.env"):
        if expected[name].read_bytes() != (ROOT / "native" / component / name).read_bytes():
            raise ValueError(f"stale {component} {name}")
    checksums = {}
    for line in expected["SHA256SUMS"].read_text().splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  (.+)", line)
        if not match or match[2] in checksums:
            raise ValueError(f"invalid {component} checksum inventory")
        checksums[match[2]] = match[1]
    if set(checksums) != set(expected) - {"SHA256SUMS"}:
        raise ValueError(f"incomplete {component} checksum inventory")
    for name, digest in checksums.items():
        if digest_file(expected[name]) != digest:
            raise ValueError(f"{component} checksum mismatch: {name}")
    seen = set()
    with tarfile.open(root / f"{binary}-{target_os}-{arch}.tar.gz", "r:gz") as archive:
        for entry in archive:
            name = entry.name.rstrip("/")
            path = PurePosixPath(name)
            if (not name or name in seen or "\\" in name or path.is_absolute()
                    or any(part in ("", ".", "..") for part in name.split("/"))
                    or entry.mode & 0o7000 or not (entry.isdir() or entry.isfile())):
                raise ValueError(f"invalid {component} archive member: {name}")
            seen.add(name)
            if entry.isdir():
                continue
            if name not in expected or entry.size != expected[name].stat().st_size:
                raise ValueError(f"unexpected {component} archive file: {name}")
            digest = hashlib.sha256()
            with archive.extractfile(entry) as source:
                for chunk in iter(lambda: source.read(1024 * 1024), b""):
                    digest.update(chunk)
            if digest.hexdigest() != digest_file(expected[name]) or entry.mode != stat.S_IMODE(expected[name].stat().st_mode):
                raise ValueError(f"{component} archive differs from validated build: {name}")
    if not set(expected) <= seen:
        raise ValueError(f"incomplete {component} archive")


def verify(group, target_os, arch, key, version):
    path = manifest_path(group)
    if path.is_symlink() or not path.is_file():
        raise ValueError("native cache manifest is missing or not a regular file")
    manifest = json.loads(path.read_text())
    files = inventory(group)
    if manifest != {"key": key, "version": version, "files": files}:
        raise ValueError("native cache identity or inventory mismatch")
    if group == "whisper" and set(files) != {f"dist/whisper/codex-whisper-{target_os}-{arch}"}:
        raise ValueError("unexpected Whisper build output")
    for component in GROUPS[group]:
        if component == "whisper":
            binary = ROOT / "dist/whisper" / f"codex-whisper-{target_os}-{arch}"
        else:
            verify_archive(component, target_os, arch)
            binary = ROOT / "dist" / component / f"{target_os}-{arch}" / f"codex-{component}"
        if run(str(binary), "version") != version:
            raise ValueError(f"native helper version mismatch: {component}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("identity", "clean", "record", "verify"))
    parser.add_argument("group", choices=GROUPS)
    parser.add_argument("--os", choices=("linux", "darwin"), required=True)
    parser.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    parser.add_argument("--key")
    parser.add_argument("--version")
    args = parser.parse_args()
    if args.command == "identity":
        key, version = cache_identity(args.group, args.os, args.arch, runtime_identity(args.group, args.os, args.arch))
        output = f"key={key}\nversion={version}\n"
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as target:
                target.write(output)
        print(output, end="")
    elif args.command == "clean":
        # A cache action may restore a prefix match even without restore-keys.
        for root in roots(args.group):
            if root.is_symlink():
                root.unlink()
            elif root.exists():
                shutil.rmtree(root)
    else:
        if not args.key or not args.version:
            parser.error("record and verify require --key and --version")
        if args.command == "record":
            manifest_path(args.group).write_text(json.dumps({
                "key": args.key, "version": args.version, "files": inventory(args.group)}, sort_keys=True) + "\n")
        else:
            verify(args.group, args.os, args.arch, args.key, args.version)


if __name__ == "__main__":
    main()
