#!/usr/bin/env python3
"""Validate Hermes releases and prepare local release changes without network access."""

import argparse
import json
import os
import re
import subprocess
import tomllib
from pathlib import Path

PACKAGE = Path("plugins/hermes")
SCRIPTS = Path(".github/scripts")
NAME = "agento11y-hermes"
PREFIX = "plugins/hermes"
VERSION = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"


def run(*args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def require(condition, message):
    if not condition:
        raise ValueError(message)


def version_tuple(version):
    require(re.fullmatch(VERSION, version), f"Invalid release version: {version}")
    return tuple(map(int, version.split(".")))


def current_version():
    project = tomllib.loads((PACKAGE / "pyproject.toml").read_text())["project"]
    require(project["name"] == NAME, f"Expected distribution {NAME}")
    version = project["version"]
    require(version_tuple(version) >= (0, 11, 0), "Imported 0.10.0 is not a publishable release")
    lock = tomllib.loads((PACKAGE / "uv.lock").read_text())
    packages = [p for p in lock["package"] if p["name"] == NAME]
    require(len(packages) == 1 and packages[0]["version"] == version, "uv.lock version disagrees with pyproject.toml")
    top = run("bash", str(SCRIPTS / "changelog-top-version.sh"), str(PACKAGE / "CHANGELOG.md"))
    require(top == version, "Changelog top disagrees with package version")
    return version


def reachable(ref, main):
    subprocess.run(["git", "merge-base", "--is-ancestor", ref, main], check=True)


def guard(ref, main):
    match = re.fullmatch(r"refs/tags/plugins/hermes/v(" + VERSION + r")", ref)
    require(match is not None, "Expected a stable refs/tags/plugins/hermes/vX.Y.Z release ref")
    version = current_version()
    require(match[1] == version, "Tag disagrees with package version")
    commit = run("git", "rev-parse", "--verify", ref + "^{commit}")
    require(commit == run("git", "rev-parse", "HEAD"), "Checkout does not match tag commit")
    reachable(commit, main)
    return version


def update_version(old, new):
    version_tuple(new)
    project_path = PACKAGE / "pyproject.toml"
    lock_path = PACKAGE / "uv.lock"
    project = project_path.read_text()
    lock = lock_path.read_text()
    project, count = re.subn(
        r'(?ms)(^\[project\]\n(?:(?!^\[).)*?^version = ")' + re.escape(old) + r'("$)',
        lambda m: m[1] + new + m[2],
        project,
    )
    require(count == 1, "Expected one static project version")
    blocks = lock.split("[[package]]")
    count = 0
    for index, block in enumerate(blocks[1:], 1):
        if tomllib.loads(block).get("name") == NAME:
            blocks[index], changed = re.subn(
                r'^version = "' + re.escape(old) + r'"$', f'version = "{new}"', block, flags=re.M
            )
            count += changed
    require(count == 1, "Expected one locked Hermes version")
    project_path.write_text(project)
    lock_path.write_text("[[package]]".join(blocks))


def prepare(bump, main):
    current = current_version()
    reachable("HEAD", main)
    tag = f"refs/tags/{PREFIX}/v{current}"
    # The first release comes from the reviewed 0.11.0 changelog, not a synthetic historical tag.
    require(
        subprocess.run(["git", "show-ref", "--verify", "--quiet", tag]).returncode == 0,
        f"Current release lacks {tag}; finish that release before preparing another",
    )
    reachable(tag, "HEAD")
    new = run("bash", str(SCRIPTS / "bump-version.sh"), bump, current)
    require(
        subprocess.run(["git", "show-ref", "--verify", "--quiet", f"refs/tags/{PREFIX}/v{new}"]).returncode != 0,
        f"Release {new} already has a tag",
    )
    rows = json.loads(Path(".github/sdk-releases.json").read_text())
    (row,) = [row for row in rows if row["id"] == "hermes"]
    section = run(
        "bash",
        str(SCRIPTS / "changelog-for-release.sh"),
        new,
        row["tag_prefix"],
        *row["paths"],
        env={**os.environ, "CHANGELOG_FROM": tag},
    )
    update_version(current, new)
    subprocess.run(
        ["bash", str(SCRIPTS / "prepend-changelog.sh"), row["changelog"]], input=section + "\n", text=True, check=True
    )
    return new


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["guard", "prepare"])
    parser.add_argument("value", help="Full tag ref, or patch/minor/major")
    parser.add_argument("--main", default="refs/remotes/origin/main")
    args = parser.parse_args()
    try:
        print(guard(args.value, args.main) if args.mode == "guard" else prepare(args.value, args.main))
    except (ValueError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"Hermes release refused: {error}\n")


if __name__ == "__main__":
    main()
