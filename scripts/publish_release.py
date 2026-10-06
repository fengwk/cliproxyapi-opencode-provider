#!/usr/bin/env python3
"""Validate native archives and publish exactly one immutable GitHub Release.

The release workflow builds the archives from the pushed tag and uploads them as
artifacts. This helper runs from the trusted default branch checkout, resolves
the tag's commit independently through the API (it never trusts ``GITHUB_SHA``),
validates the three expected native ZIPs, their checksums, root layout and
metadata, and only then publishes the release.

Safety rules:

* An already **published** release is a no-op: assets are never uploaded,
  replaced or clobbered, and a tag is never moved or deleted.
* A **draft** release may be recovered only when it is owned by
  ``github-actions[bot]`` (the retry path). A human-owned or unknown draft is
  refused.
* A new release is created as a **draft** containing every asset, and is
  published only after the draft upload succeeds.

Python standard library only.
"""

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import zipfile

PLUGIN_ID = "cliproxyapi-opencode-provider"
AUTOMATION_AUTHOR = "github-actions[bot]"
TAG_RE = re.compile(r"^v(\d+)\.(\d+)\.(\d+)$")

# goos, goarch, native library extension: the three shipped native targets.
TARGETS = (
    ("linux", "amd64", "so"),
    ("linux", "arm64", "so"),
    ("darwin", "arm64", "dylib"),
)
STATIC_MEMBERS = frozenset({"LICENSE", "NOTICE", "README.md", "metadata.json"})


class PublishError(Exception):
    """A fail-closed validation or publication problem."""


def sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def run_gh(bin_path, args):
    proc = subprocess.run([bin_path] + list(args), capture_output=True, text=True)
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip().splitlines()
        raise PublishError("gh %s failed: %s" % (args[0], detail[0] if detail else "unknown error"))
    return proc.stdout


def resolve_tag_commit(bin_path, repo, tag):
    out = run_gh(bin_path, ["api", "repos/%s/commits/%s" % (repo, tag)])
    try:
        data = json.loads(out)
    except ValueError:
        raise PublishError("malformed tag resolution response for %s" % tag)
    sha = data.get("sha")
    if not isinstance(sha, str) or not sha:
        raise PublishError("could not resolve the commit for tag %s" % tag)
    return sha


def validate_archive(path, version, commit, goos, goarch, ext):
    name = os.path.basename(path)
    try:
        with zipfile.ZipFile(path) as archive:
            expected = STATIC_MEMBERS | {PLUGIN_ID + "." + ext}
            if set(archive.namelist()) != expected:
                raise PublishError("%s has an unexpected archive layout" % name)
            try:
                metadata = json.loads(archive.read("metadata.json").decode("utf-8"))
            except (KeyError, ValueError, UnicodeDecodeError):
                raise PublishError("%s has malformed metadata.json" % name)
    except zipfile.BadZipFile:
        raise PublishError("%s is not a valid zip archive" % name)
    if metadata.get("id") != PLUGIN_ID:
        raise PublishError("%s metadata id does not match the plugin" % name)
    if metadata.get("version") != version:
        raise PublishError("%s metadata version does not match %s" % (name, version))
    if metadata.get("commit") != commit:
        raise PublishError("%s metadata commit does not match the tagged commit" % name)
    if metadata.get("goos") != goos or metadata.get("goarch") != goarch:
        raise PublishError("%s metadata platform does not match" % name)


def validate_checksums(path, archives):
    if not os.path.isfile(path):
        raise PublishError("checksums.txt is missing")
    recorded = {}
    with open(path, "r", encoding="utf-8") as handle:
        for line in handle:
            parts = line.split()
            if not parts:
                continue
            if len(parts) != 2:
                raise PublishError("checksums.txt is malformed")
            recorded[parts[1]] = parts[0].lower()
    for archive in archives:
        name = os.path.basename(archive)
        digest = recorded.get(name)
        if digest is None:
            raise PublishError("checksums.txt has no entry for %s" % name)
        if digest != sha256(archive):
            raise PublishError("checksum mismatch for %s" % name)


def validate_assets(assets_dir, version, commit):
    if not os.path.isdir(assets_dir):
        raise PublishError("assets directory %r does not exist" % assets_dir)
    archives = []
    for goos, goarch, ext in TARGETS:
        name = "%s_%s_%s_%s.zip" % (PLUGIN_ID, version, goos, goarch)
        path = os.path.join(assets_dir, name)
        if not os.path.isfile(path):
            raise PublishError("expected archive %s is missing" % name)
        validate_archive(path, version, commit, goos, goarch, ext)
        archives.append(path)
    checksums = os.path.join(assets_dir, "checksums.txt")
    validate_checksums(checksums, archives)
    return archives + [checksums]


def get_release(bin_path, repo, tag):
    proc = subprocess.run(
        [
            bin_path,
            "release",
            "view",
            tag,
            "--repo",
            repo,
            "--json",
            "tagName,isDraft,isPrerelease,author",
        ],
        capture_output=True,
        text=True,
    )
    if proc.returncode != 0:
        message = ((proc.stderr or "") + (proc.stdout or "")).lower()
        if "not found" in message or "404" in message:
            return None
        raise PublishError("could not read release %s" % tag)
    try:
        return json.loads(proc.stdout)
    except ValueError:
        raise PublishError("malformed release response for %s" % tag)


def publish(tag, assets_dir, repo):
    if not repo:
        raise PublishError("GH_REPO (or --repo) is required")
    if not TAG_RE.match(tag):
        raise PublishError("tag %r is not a stable vX.Y.Z version" % tag)
    version = tag[1:]
    bin_path = os.environ.get("GH_BIN", "gh").strip() or "gh"

    # The tagged commit is resolved independently of the workflow's environment.
    commit = resolve_tag_commit(bin_path, repo, tag)
    archives = validate_assets(assets_dir, version, commit)

    existing = get_release(bin_path, repo, tag)
    if existing is not None and not existing.get("isDraft"):
        print("publish-release: %s is already published; nothing to do" % tag)
        return
    if existing is not None:
        author = (existing.get("author") or {}).get("login")
        if author != AUTOMATION_AUTHOR:
            raise PublishError(
                "draft release %s is owned by %r; refusing to touch it" % (tag, author)
            )
        # A failed upload may leave a managed partial draft. Recreate the draft
        # (never the tag) so the next attempt starts from a complete upload.
        run_gh(bin_path, ["release", "delete", tag, "--repo", repo, "--yes"])

    run_gh(
        bin_path,
        [
            "release",
            "create",
            tag,
            "--repo",
            repo,
            "--verify-tag",
            "--draft",
            "--title",
            tag,
            "--notes",
            "Native CLIProxyAPI plugin artifacts built from %s. "
            "Verify downloads against checksums.txt." % tag,
        ]
        + archives,
    )
    # Publish only after every asset uploaded successfully.
    run_gh(bin_path, ["release", "edit", tag, "--repo", repo, "--draft=false"])
    print("publish-release: published %s" % tag)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True, help="existing release tag vX.Y.Z")
    parser.add_argument("--assets", required=True, help="directory with the release assets")
    parser.add_argument("--repo", default=os.environ.get("GH_REPO", ""), help="owner/repo")
    args = parser.parse_args(argv)
    try:
        publish(args.tag, args.assets, args.repo)
    except PublishError as error:
        print("publish-release: %s" % error, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
