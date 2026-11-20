"""Framework identity and opt-in automatic client tags.

Automatic tags become metric labels. Resolve them once per process, only when
requested, and leave explicit environment tags to the SDK. Read Git files
without subprocesses so exporter credentials never reach a child process.
"""

from __future__ import annotations

import configparser
import os
import re
from urllib.parse import urlsplit

_ENTRYPOINT = "hermes"
_FRAMEWORK_TAGS = {
    "agento11y.framework.name": "hermes",
    "agento11y.framework.source": "plugin",
    "agento11y.framework.language": "python",
}
_TAG_KEYS = {"user": "user", "repo": "repo", "branch": "git.branch"}
_MAX_VALUE_LENGTH = 128
# Depth of the walk from cwd towards the filesystem root, matching the
# first-party resolver.
_MAX_PARENTS = 6
_GITDIR_LINE = re.compile(r"^gitdir:\s*(.+)$", re.MULTILINE)
_HEAD_REF = re.compile(r"^ref:\s*refs/heads/(.+)$")
_SHA = re.compile(r"^[0-9a-fA-F]{7,}$")
# Hosts that resolve a repository path regardless of case. gitlab.com is not
# listed: GitLab does not document case-insensitive path lookup.
# ssh.github.com and altssh.bitbucket.org are the vendors' documented SSH hosts
# on port 443 for the same repositories.
_CASE_INSENSITIVE_HOSTS = frozenset({"github.com", "ssh.github.com", "bitbucket.org", "altssh.bitbucket.org"})

_CACHED: dict[str, str] | None = None


def builtin_tags() -> dict[str, str]:
    """The built-in tags, resolved on the first call and cached after.

    Keys that cannot be resolved are omitted rather than sent empty.
    """
    global _CACHED
    if _CACHED is None:
        _CACHED = _resolve()
    return dict(_CACHED)


def client_tags() -> dict[str, str]:
    """Client labels, with explicit environment tags left for the SDK to merge."""
    explicit = _explicit_tags()
    return {k: v for k, v in {**_FRAMEWORK_TAGS, **builtin_tags()}.items() if k not in explicit}


def seed_tags() -> dict[str, str]:
    """No unconditional working directory or branch on generations."""
    return {}


def _resolve() -> dict[str, str]:
    tags = {"entrypoint": _ENTRYPOINT}
    explicit = _explicit_tags()
    for name in _selected_tags():
        key = _TAG_KEYS[name]
        if key in explicit:
            continue
        try:
            if name == "user":
                value = _env("USER_ID")
            else:
                cwd = os.getcwd()
                value = _git_repo(cwd) if name == "repo" else _git_branch(cwd)
            value = value.strip()[:_MAX_VALUE_LENGTH]
            if value:
                tags[key] = value
        except Exception:
            # Telemetry must not interrupt the agent if Git lookup fails.
            continue
    return tags


def _env(suffix: str) -> str:
    for prefix in ("AGENTO11Y_", "SIGIL_"):
        value = os.environ.get(prefix + suffix, "").strip()
        if value:
            return value
    return ""


def _selected_tags() -> list[str]:
    if _env("AUTO_CODING_AGENT_TAGS").lower() not in {"1", "true", "yes", "on"}:
        return []
    raw = _env("AUTO_CODING_AGENT_TAGS_NAMES")
    names = {name.strip().lower() for name in raw.split(",")}
    return [name for name in _TAG_KEYS if not raw or "all" in names or name in names]


def _explicit_tags() -> dict[str, str]:
    tags = {}
    for pair in _env("TAGS").split(","):
        key, sep, value = pair.partition("=")
        if sep and key.strip() and value.strip():
            tags[key.strip()] = value.strip()
    return tags


def _git_repo(start: str) -> str:
    git_dir = _find_git_dir(start)
    if not git_dir:
        return ""
    common = _read_git_file(os.path.join(git_dir, "commondir"))
    if common:
        git_dir = os.path.normpath(os.path.join(git_dir, common))
    config = configparser.RawConfigParser(strict=False)
    try:
        config.read_string(_read_git_file(os.path.join(git_dir, "config")))
        remote = config.get('remote "origin"', "url", fallback="")
    except configparser.Error:
        remote = ""
    repo = _repo_from_remote(remote)
    if repo:
        return repo
    base = (
        os.path.basename(os.path.dirname(git_dir)) if os.path.basename(git_dir) == ".git" else os.path.basename(git_dir)
    )
    return base.removesuffix(".git")


def _repo_from_remote(raw: str) -> str:
    """The repo tag for a remote URL, as ``repoFromRemoteURL`` in the Go plugin
    gives it. Both must give the ``want`` column of
    ``conformance/git-remotes/remotes.tsv``."""
    url = raw.strip()
    if not url:
        return ""
    host, over_http = "", False
    if "://" in url:
        try:
            parsed = urlsplit(url)
        except ValueError:
            return ""
        path = parsed.path.rstrip("/")
        if parsed.scheme == "file":
            path = os.path.basename(path)
        host = parsed.hostname or ""
        over_http = parsed.scheme in ("http", "https")
    elif re.match(r"^[^/:]+:", url):
        authority, path = url.split(":", 1)
        path = path.rstrip("/")
        host = authority.rpartition("@")[2].lower()
    else:
        path = os.path.basename(url.rstrip("/"))
    return _canonical_repo(host, over_http, path.lstrip("/"))


def _canonical_repo(host: str, over_http: bool, path: str) -> str:
    """Give each remote form of one repository the same tag on known hosts,
    and drop a ``.git`` suffix.

    Known hosts ignore case, so there the suffix is matched in any case too.
    An unknown host keeps the path as the remote spells it, apart from
    Bitbucket Server's ``scm/``: it may be case-sensitive, and lowercasing
    there could merge two repositories.
    """
    if host in _CASE_INSENSITIVE_HOSTS:
        return path.lower().removesuffix(".git")
    if host in ("dev.azure.com", "ssh.dev.azure.com") or host.endswith(".visualstudio.com"):
        return _azure_repo(host, path)
    path = path.removesuffix(".git")
    # Bitbucket Server serves ssh://host:7999/proj/repo over HTTPS as
    # https://host/scm/proj/repo. Exactly three segments leaves out a context
    # path (/bitbucket/scm/proj/repo), which stays as it is. gitlab.com is a
    # known host where scm is only a group name, so its HTTPS and SSH remotes
    # keep the same tag.
    rest = path.removeprefix("scm/")
    if over_http and host != "gitlab.com" and rest != path and rest.count("/") == 1:
        return rest
    return path


def _azure_repo(host: str, path: str) -> str:
    """Every Azure DevOps remote form as lowercase ``org/project/repo``.

    SSH spells it ``v3/org/project/repo``, HTTPS ``org/project/_git/repo``, and
    legacy visualstudio.com remotes carry the org in the host name and may add
    ``DefaultCollection/`` or use ``_ssh`` for ``_git``. Azure ignores case, so
    the markers and a ``.git`` suffix match in any case. Any other shape keeps
    the release tag.
    """
    segments = path.lower().removesuffix(".git").split("/")
    if host in ("ssh.dev.azure.com", "vs-ssh.visualstudio.com"):
        if len(segments) != 4 or segments[0] != "v3":
            return path.removesuffix(".git")
        return "/".join(segments[1:])
    if host.endswith(".visualstudio.com"):
        segments.insert(0, host.removesuffix(".visualstudio.com"))
    if len(segments) > 1 and segments[1] == "defaultcollection":
        del segments[1]
    if len(segments) != 4 or segments[2] not in ("_git", "_ssh"):
        return path.removesuffix(".git")
    return f"{segments[0]}/{segments[1]}/{segments[3]}"


def _read_git_file(path: str) -> str:
    try:
        with open(path, encoding="utf-8") as handle:
            return handle.read().strip()
    except (OSError, UnicodeError):
        return ""


def _git_branch(start: str) -> str:
    """Checked-out branch, the first 12 chars of a detached HEAD, or ``""``."""
    git_dir = _find_git_dir(start)
    if not git_dir:
        return ""
    head = _read_git_file(os.path.join(git_dir, "HEAD"))
    match = _HEAD_REF.match(head)
    if match:
        return match.group(1).strip()
    return head[:12] if _SHA.match(head) else ""


def _find_git_dir(start: str) -> str:
    current = start
    for _ in range(_MAX_PARENTS):
        resolved = _resolve_git_entry(os.path.join(current, ".git"))
        if resolved:
            return resolved
        parent = os.path.dirname(current)
        if parent == current:
            break
        current = parent
    return ""


def _resolve_git_entry(path: str) -> str:
    """The git directory ``path`` names: itself, or its ``gitdir:`` target.

    A linked worktree and a submodule both have ``.git`` as a file holding a
    ``gitdir:`` pointer, so HEAD lives elsewhere.
    """
    if os.path.isdir(path):
        return path
    content = _read_git_file(path)
    match = _GITDIR_LINE.search(content)
    if not match:
        return ""
    target = match.group(1).strip()
    if not os.path.isabs(target):
        target = os.path.join(os.path.dirname(path), target)
    return os.path.normpath(target)


def _reset_for_tests() -> None:
    global _CACHED
    _CACHED = None
