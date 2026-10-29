#!/usr/bin/env python3
"""Publish verified files through an owned, resumable GitHub draft.

Never replace uploaded assets or modify an existing published release. Only an
empty starter asset in the exact owned draft may be removed for retry. All remote
bytes are checked against the locally verified signed inventory before publish.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time


ROOT = Path(__file__).resolve().parents[1]
ATTEMPTS = 3
TIMEOUT = 180
RESERVED = ("SHA256SUMS", "SHA256SUMS.sigstore.json")


class PublishError(Exception):
    pass


class GhError(PublishError):
    def __init__(self, status=None, transient=False):
        self.status = status
        self.transient = transient
        # Never include CLI stderr, response bodies, URLs or environment values.
        super().__init__("GitHub request failed" + (f" (HTTP {status})" if status else ""))


def gh(arguments, payload=None, output=None):
    encoded = json.dumps(payload).encode() if payload is not None else None
    if payload is not None:
        arguments = ["-H", "Content-Type: application/json", "-H", f"Content-Length: {len(encoded)}", *arguments]
    try:
        result = subprocess.run(
            ["gh", "api", "--hostname", "github.com", "-H", "X-GitHub-Api-Version: 2022-11-28", *arguments],
            input=encoded,
            stdout=output if output is not None else subprocess.PIPE,
            stderr=subprocess.PIPE, timeout=TIMEOUT,
            env={**os.environ, "GH_HOST": "github.com", "GH_DEBUG": "", "GH_PROMPT_DISABLED": "1"})
    except subprocess.TimeoutExpired:
        raise GhError(transient=True) from None
    if result.returncode:
        message = result.stderr.decode(errors="replace")
        match = re.search(r"\(HTTP (\d{3})\)", message)
        status = int(match[1]) if match else None
        transport = any(reason in message for reason in (
            "i/o timeout", "TLS handshake timeout", "connection reset by peer",
            "unexpected EOF", "context deadline exceeded"))
        raise GhError(status, status in (408, 429) or status is not None and 500 <= status <= 599
                      or status is None and transport)
    if output is not None or not result.stdout:
        return None
    try:
        return json.loads(result.stdout)
    except (ValueError, UnicodeDecodeError):
        raise PublishError("Invalid GitHub JSON response") from None


def pause(attempt):
    time.sleep(min(2 ** (attempt + 1), 30))


def read(arguments, output=None, payload=None):
    for attempt in range(ATTEMPTS):
        try:
            if output is not None:
                output.seek(0)
                output.truncate()
            return gh(arguments, output=output, payload=payload)
        except GhError as error:
            if not error.transient or attempt == ATTEMPTS - 1:
                raise
            pause(attempt)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1048576), b""):
            digest.update(chunk)
    return digest.hexdigest()


def inventory(dist):
    manifest = dist / RESERVED[0]
    if manifest.is_symlink() or not manifest.is_file() or manifest.stat().st_size > 1048576:
        raise PublishError("Invalid local checksum manifest")
    expected = {}
    for line in manifest.read_text().splitlines():
        match = re.fullmatch(r"([0-9a-fA-F]{64})  ([A-Za-z0-9][A-Za-z0-9._-]*)", line)
        if not match or match[2] in expected or match[2] in RESERVED:
            raise PublishError("Invalid local checksum entry")
        expected[match[2]] = match[1].lower()
    if not expected:
        raise PublishError("Empty local checksum manifest")
    for name in (*expected, *RESERVED):
        path = dist / name
        if path.is_symlink() or not path.is_file():
            raise PublishError("Missing or unsafe local release file")
        digest = sha256(path)
        if name in expected and digest != expected[name]:
            raise PublishError("Local release checksum mismatch")
        expected[name] = digest
    return expected


def verify(dist, repository, tag):
    for arguments in (("verify", "--root", str(ROOT)),
                      ("verify-provenance", "--repo", repository, "--tag", tag)):
        try:
            subprocess.run(["go", "run", "./cmd/release-tool", *arguments, "--dist", str(dist)],
                           cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                           check=True, timeout=300)
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
            raise PublishError("Native release or provenance verification failed") from None


class Publisher:
    def __init__(self, repository, tag, commit, dist):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*", repository):
            raise PublishError("Invalid repository")
        if not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", tag):
            raise PublishError("Invalid stable release tag")
        if not re.fullmatch(r"[0-9a-f]{40}", commit):
            raise PublishError("Invalid expected commit")
        self.repository, self.tag, self.commit = repository, tag, commit
        self.dist = dist.resolve()
        self.base = f"repos/{repository}"
        self.marker = f"<!-- mact-release:{repository}@{tag}:{commit} -->"
        self.release_id = None

    def check_tag(self):
        reference = read([f"{self.base}/git/ref/tags/{self.tag}"])
        if not isinstance(reference, dict) or reference.get("ref") != f"refs/tags/{self.tag}":
            raise PublishError("Missing exact release tag")
        target = reference.get("object")
        for _ in range(5):
            if not isinstance(target, dict) or not re.fullmatch(r"[0-9a-f]{40}", str(target.get("sha"))):
                break
            if target.get("type") == "commit":
                if target["sha"] == self.commit:
                    return
                break
            if target.get("type") != "tag":
                break
            annotated = read([f"{self.base}/git/tags/{target['sha']}"])
            if not isinstance(annotated, dict) or annotated.get("sha") != target["sha"]:
                break
            target = annotated.get("object")
        raise PublishError("Release tag differs from expected commit")

    def release(self):
        # REST's by-tag endpoint cannot find pending draft tags. Match the pending
        # tag with GraphQL, then use the immutable numeric ID for all operations.
        owner, name = self.repository.split("/")
        response = read(["graphql", "--input", "-"], payload={
            "query": "query($owner:String!,$name:String!,$tag:String!){repository(owner:$owner,name:$name){nameWithOwner release(tagName:$tag){databaseId tagCommit{oid}}}}",
            "variables": {"owner": owner, "name": name, "tag": self.tag}})
        try:
            repository = response["data"]["repository"]
            if response.get("errors") or repository["nameWithOwner"] != self.repository:
                raise ValueError()
            match = repository["release"]
            if match is None:
                return None
            identity = match["databaseId"]
            tag_commit = match["tagCommit"]
            if type(identity) is not int or identity <= 0:
                raise ValueError()
        except (TypeError, KeyError, ValueError):
            raise PublishError("Invalid pending release identity") from None
        release = read([f"{self.base}/releases/{identity}"])
        if not isinstance(release, dict) or release.get("id") != identity:
            raise PublishError("Release identity changed")
        release["pending_tag"] = self.tag
        release["tag_commit"] = tag_commit
        return release

    def check_release(self, release, published=False):
        identity = release.get("id") if isinstance(release, dict) else None
        if (type(identity) is not int or identity <= 0
                or self.release_id is not None and identity != self.release_id
                or release.get("url") != f"https://api.github.com/{self.base}/releases/{identity}"
                or release.get("pending_tag") != self.tag
                or not (release.get("tag_name") == self.tag or not published
                        and isinstance(release.get("tag_name"), str)
                        and re.fullmatch(r"untagged-[A-Za-z0-9]+", release["tag_name"]))
                or release.get("name") != self.tag or release.get("prerelease") is not False
                or release.get("draft") is not (not published)
                or not isinstance(release.get("body"), str)
                or release["body"].splitlines().count(self.marker) != 1):
            raise PublishError("Refusing an unowned, changed or published release")
        tag_commit = release.get("tag_commit", {})
        if tag_commit is None:
            if published:
                raise PublishError("Published release has no tag commit")
            # GitHub may leave this optional field null on a pending draft. Only
            # after ownership is proven, bind it through the exact existing tag.
            self.check_tag()
        elif not isinstance(tag_commit, dict) or tag_commit.get("oid") != self.commit:
            raise PublishError("Release tag commit differs from expected commit")
        self.release_id = identity
        return release

    def draft(self):
        return self.check_release(self.release())

    def create(self):
        release = self.release()
        if release is not None:
            return self.check_release(release)
        notes = ROOT / "docs" / "releases" / f"{self.tag}.md"
        body = notes.read_text() if notes.is_file() else self.tag
        payload = {"tag_name": self.tag, "target_commitish": self.commit, "name": self.tag,
                   "body": body.rstrip() + "\n\n" + self.marker + "\n", "draft": True, "prerelease": False}
        for attempt in range(ATTEMPTS):
            self.check_tag()
            try:
                gh(["--method", "POST", f"{self.base}/releases", "--input", "-"], payload)
                return self.draft()
            except GhError as error:
                if not error.transient:
                    raise
                # A timed-out create may already have succeeded. Never duplicate it.
                release = self.release()
                if release is not None:
                    return self.check_release(release)
                if attempt == ATTEMPTS - 1:
                    raise
                pause(attempt)

    def assets(self, expected):
        result = {}
        for page in range(1, 11):
            assets = read([f"{self.base}/releases/{self.release_id}/assets?per_page=100&page={page}"])
            if not isinstance(assets, list):
                raise PublishError("Invalid release asset inventory")
            for asset in assets:
                if not isinstance(asset, dict):
                    raise PublishError("Invalid release asset")
                name, identity = asset.get("name"), asset.get("id")
                if (name not in expected or name in result or type(identity) is not int or identity <= 0
                        or asset.get("url") != f"https://api.github.com/{self.base}/releases/assets/{identity}"
                        or type(asset.get("size")) is not int
                        or not (asset.get("state") == "starter" and asset["size"] == 0
                                or asset.get("state") == "uploaded"
                                and asset["size"] == (self.dist / name).stat().st_size)):
                    raise PublishError("Unexpected, incomplete or mismatched release asset")
                result[name] = {key: asset[key] for key in ("id", "name", "url", "state", "size")}
            if len(assets) < 100:
                return result
        raise PublishError("Release asset inventory exceeds limit")

    def remove_starter(self, asset):
        # Only an empty, expected-name starter in this exact owned draft may be
        # removed. Re-fetch its state and release identity immediately first.
        identity = asset["id"]
        current = read([f"{self.base}/releases/assets/{identity}"])
        if (not isinstance(current, dict) or current.get("id") != identity or current.get("name") != asset["name"]
                or current.get("state") != "starter" or type(current.get("size")) is not int
                or current["size"] != 0):
            raise PublishError("Starter asset changed; refusing cleanup")
        self.check_tag()
        self.draft()
        gh(["--method", "DELETE", f"{self.base}/releases/assets/{identity}"])

    def download(self, asset, destination, digest):
        with destination.open("wb") as output:
            read(["-H", "Accept: application/octet-stream",
                  f"{self.base}/releases/assets/{asset['id']}"], output=output)
        if sha256(destination) != digest:
            raise PublishError("Downloaded release asset checksum mismatch")

    def upload(self, name, expected, temporary):
        for attempt in range(ATTEMPTS):
            self.check_tag()
            self.draft()
            existing = self.assets(expected)
            if name in existing:
                if existing[name]["state"] == "starter":
                    self.remove_starter(existing[name])
                else:
                    self.download(existing[name], temporary / name, expected[name])
                    return
            if sha256(self.dist / name) != expected[name]:
                raise PublishError("Local release file changed after verification")
            try:
                gh(["--method", "POST", "-H", "Content-Type: application/octet-stream",
                    "-H", f"Content-Length: {(self.dist / name).stat().st_size}",
                    f"https://uploads.github.com/{self.base}/releases/{self.release_id}/assets?name={name}",
                    "--input", str(self.dist / name)])
                return
            except GhError as error:
                if not error.transient:
                    raise
                # A timeout may leave a complete asset. Verify it before any retry.
                existing = self.assets(expected)
                if name in existing and existing[name]["state"] == "uploaded":
                    self.download(existing[name], temporary / name, expected[name])
                    return
                if attempt == ATTEMPTS - 1:
                    raise
                pause(attempt)

    def publish(self):
        expected = inventory(self.dist)
        verify(self.dist, self.repository, self.tag)
        if read([self.base]).get("full_name") != self.repository:
            raise PublishError("Repository identity mismatch")
        self.check_tag()
        self.create()
        with tempfile.TemporaryDirectory(prefix="mact-release-readback-") as directory:
            temporary = Path(directory)
            # Validate all existing bytes before uploading anything into a resumed draft.
            for name, asset in self.assets(expected).items():
                if asset["state"] == "uploaded":
                    self.download(asset, temporary / name, expected[name])
            for name in sorted(expected):
                self.upload(name, expected, temporary)
            self.draft()
            assets = self.assets(expected)
            if set(assets) != set(expected) or any(asset["state"] != "uploaded" for asset in assets.values()):
                raise PublishError("Incomplete final release asset inventory")
            for name, asset in assets.items():
                self.download(asset, temporary / name, expected[name])
            verify(temporary, self.repository, self.tag)
            for attempt in range(ATTEMPTS):
                self.check_tag()
                if self.assets(expected) != assets:
                    raise PublishError("Release assets changed after readback")
                self.draft()
                try:
                    gh(["--method", "PATCH", f"{self.base}/releases/{self.release_id}", "--input", "-"],
                       {"draft": False, "make_latest": "true"})
                    self.check_release(self.release(), published=True)
                    break
                except GhError as error:
                    if not error.transient:
                        raise
                    release = self.release()
                    if release is not None and release.get("draft") is False:
                        self.check_release(release, published=True)
                        break
                    self.check_release(release)
                    if attempt == ATTEMPTS - 1:
                        raise
                    pause(attempt)
            self.check_release(self.release(), published=True)
            if read([f"{self.base}/releases/latest"]).get("id") != self.release_id:
                raise PublishError("Published release is not latest")
        print("Verified release published as latest.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--dist", type=Path, required=True)
    args = parser.parse_args()
    try:
        Publisher(args.repo, args.tag, args.commit, args.dist).publish()
    except (PublishError, OSError, ValueError):
        # Files and remote responses are untrusted; keep all credentials out of logs.
        print("Release publication refused or failed; existing assets were preserved.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
