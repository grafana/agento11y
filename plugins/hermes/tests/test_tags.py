"""Opt-in client labels and filesystem-only Git resolution."""

from __future__ import annotations

import os
import pathlib

import pytest

from grafana_agento11y_hermes import _tags


@pytest.fixture(autouse=True)
def isolated_tags(monkeypatch: pytest.MonkeyPatch):
    for key in tuple(os.environ):
        if key.startswith(("AGENTO11Y_", "SIGIL_")):
            monkeypatch.delenv(key)
    _tags._reset_for_tests()
    yield
    _tags._reset_for_tests()


def _repo(root: pathlib.Path, head: str) -> pathlib.Path:
    """A directory holding a ``.git`` directory whose HEAD is ``head``."""
    git_dir = root / ".git"
    git_dir.mkdir(parents=True)
    (git_dir / "HEAD").write_text(head)
    return root


# --- HEAD parsing ---


@pytest.mark.parametrize(
    ("head", "expected"),
    (
        ("ref: refs/heads/main\n", "main"),
        ("ref: refs/heads/feature/nested-name\n", "feature/nested-name"),
        ("ref: refs/heads/main", "main"),
        # Detached HEAD: the short sha stands in for a branch name.
        ("4d1f0c2b9a8e7f60514233aabbccddeeff001122\n", "4d1f0c2b9a8e"),
        # A ref outside refs/heads (a tag checkout) names no branch.
        ("ref: refs/tags/v1.0.0\n", ""),
        ("", ""),
        ("not a head at all\n", ""),
        # Too short to be a sha.
        ("abc123\n", ""),
    ),
)
def test_head_contents_resolve_to_a_branch(tmp_path: pathlib.Path, head: str, expected: str) -> None:
    assert _tags._git_branch(str(_repo(tmp_path, head))) == expected


def test_a_git_dir_without_a_head_file_names_no_branch(tmp_path: pathlib.Path) -> None:
    (tmp_path / ".git").mkdir()
    assert _tags._git_branch(str(tmp_path)) == ""


def test_no_repository_names_no_branch(tmp_path: pathlib.Path) -> None:
    assert _tags._git_branch(str(tmp_path)) == ""


# --- the walk towards the root ---


def test_the_branch_is_found_from_a_subdirectory(tmp_path: pathlib.Path) -> None:
    _repo(tmp_path, "ref: refs/heads/main\n")
    deep = tmp_path / "src" / "pkg"
    deep.mkdir(parents=True)
    assert _tags._git_branch(str(deep)) == "main"


def test_the_walk_stops_after_six_parents(tmp_path: pathlib.Path) -> None:
    """A repository further up than ``_MAX_PARENTS`` is deliberately not found."""
    _repo(tmp_path, "ref: refs/heads/main\n")
    deep = tmp_path.joinpath(*[f"d{i}" for i in range(_tags._MAX_PARENTS)])
    deep.mkdir(parents=True)
    assert _tags._find_git_dir(str(deep)) == ""
    # One level closer is inside the limit.
    assert _tags._find_git_dir(str(deep.parent)) == str(tmp_path / ".git")


def test_the_walk_terminates_at_the_filesystem_root() -> None:
    assert _tags._find_git_dir(os.sep) == ""


# --- worktree and submodule pointers ---


def test_an_absolute_gitdir_pointer_is_followed(tmp_path: pathlib.Path) -> None:
    real = tmp_path / "store" / "worktrees" / "wt1"
    real.mkdir(parents=True)
    (real / "HEAD").write_text("ref: refs/heads/side-branch\n")

    checkout = tmp_path / "checkout"
    checkout.mkdir()
    (checkout / ".git").write_text(f"gitdir: {real}\n")

    assert _tags._git_branch(str(checkout)) == "side-branch"


def test_a_relative_gitdir_pointer_resolves_against_the_pointer_file(tmp_path: pathlib.Path) -> None:
    real = tmp_path / "store" / "modules" / "sub"
    real.mkdir(parents=True)
    (real / "HEAD").write_text("ref: refs/heads/submodule-branch\n")

    checkout = tmp_path / "checkout"
    checkout.mkdir()
    (checkout / ".git").write_text("gitdir: ../store/modules/sub\n")

    assert _tags._git_branch(str(checkout)) == "submodule-branch"


def test_a_git_file_without_a_pointer_is_ignored(tmp_path: pathlib.Path) -> None:
    (tmp_path / ".git").write_text("this is not a gitdir pointer\n")
    assert _tags._find_git_dir(str(tmp_path)) == ""


def test_an_unreadable_git_entry_is_ignored(tmp_path: pathlib.Path, monkeypatch: pytest.MonkeyPatch) -> None:
    (tmp_path / ".git").write_text("gitdir: /somewhere\n")

    def deny(*_: object, **__: object) -> object:
        raise PermissionError("nope")

    monkeypatch.setattr("builtins.open", deny)
    assert _tags._resolve_git_entry(str(tmp_path / ".git")) == ""


# --- the resolved tag set ---


def test_tags_carry_only_opted_in_branch(tmp_path: pathlib.Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES", "branch")
    _repo(tmp_path, "ref: refs/heads/main\n")
    monkeypatch.setattr(os, "getcwd", lambda: str(tmp_path))
    assert _tags.builtin_tags() == {
        "entrypoint": "hermes",
        "git.branch": "main",
    }


def test_an_unresolvable_branch_is_omitted_rather_than_sent_empty(
    tmp_path: pathlib.Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "on")
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES", "branch")
    monkeypatch.setattr(os, "getcwd", lambda: str(tmp_path))
    assert "git.branch" not in _tags.builtin_tags()


def test_an_unresolvable_cwd_leaves_only_the_entrypoint(monkeypatch: pytest.MonkeyPatch) -> None:
    """A deleted working directory makes getcwd raise; the tags still resolve."""
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "on")
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES", "repo,branch")

    def deny() -> str:
        raise OSError("cwd is gone")

    monkeypatch.setattr(os, "getcwd", deny)
    assert _tags.builtin_tags() == {"entrypoint": "hermes"}


def test_resolution_is_cached_after_the_first_call(tmp_path: pathlib.Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "on")
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES", "branch")
    calls: list[int] = []

    def counting_getcwd() -> str:
        calls.append(1)
        return str(tmp_path)

    monkeypatch.setattr(os, "getcwd", counting_getcwd)
    _tags.builtin_tags()
    _tags.builtin_tags()
    assert len(calls) == 1, "the .git walk must stay off the per-request path"


def test_default_tags_are_identity_only(tmp_path: pathlib.Path, monkeypatch: pytest.MonkeyPatch) -> None:
    _repo(tmp_path, "ref: refs/heads/main\n")
    monkeypatch.setattr(os, "getcwd", lambda: str(tmp_path))

    client, seed = _tags.client_tags(), _tags.seed_tags()
    assert seed == {}
    assert "cwd" not in client
    assert client == {
        "agento11y.framework.name": "hermes",
        "agento11y.framework.source": "plugin",
        "agento11y.framework.language": "python",
        "entrypoint": "hermes",
    }


def test_the_cached_dict_cannot_be_mutated_by_a_caller(monkeypatch: pytest.MonkeyPatch) -> None:
    tags = _tags.builtin_tags()
    tags["entrypoint"] = "tampered"
    assert _tags.builtin_tags()["entrypoint"] == "hermes"


@pytest.mark.parametrize("switch", ["", "0", "false", "NO", "off", "invalid", "user,repo"])
def test_off_never_resolves(switch, monkeypatch):
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", switch)
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES", "all")
    monkeypatch.setattr(_tags, "_git_branch", lambda _: pytest.fail("git lookup"))
    monkeypatch.setattr(_tags, "_git_repo", lambda _: pytest.fail("repo lookup"))
    monkeypatch.setattr(os, "getcwd", lambda: pytest.fail("cwd lookup"))
    monkeypatch.setattr(_tags.getpass, "getuser", lambda: pytest.fail("user lookup"))
    assert _tags.builtin_tags() == {"entrypoint": "hermes"}


@pytest.mark.parametrize("switch", ["1", "true", " YES ", "On"])
@pytest.mark.parametrize(
    ("names", "expected"),
    [
        ("", ["user", "repo", "branch"]),
        (" ALL ", ["user", "repo", "branch"]),
        ("USER, user,, BRANCH,unknown", ["user", "branch"]),
        (",,", []),
        ("unknown", []),
        ("repo", ["repo"]),
        ("branch", ["branch"]),
    ],
)
def test_selection_table(switch, names, expected, monkeypatch):
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", switch)
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES", names)
    assert _tags._selected_tags() == expected


@pytest.mark.parametrize("suffix", ["AUTO_CODING_AGENT_TAGS", "AUTO_CODING_AGENT_TAGS_NAMES", "USER_ID", "TAGS"])
@pytest.mark.parametrize(
    ("canonical", "legacy", "expected"),
    [
        (" new ", "old", "new"),
        ("", " old ", "old"),
        ("  ", "old", "old"),
        ("", "", ""),
        ("false", "true", "false"),
    ],
)
def test_alias_precedence(suffix, canonical, legacy, expected, monkeypatch):
    monkeypatch.setenv("AGENTO11Y_" + suffix, canonical)
    monkeypatch.setenv("SIGIL_" + suffix, legacy)
    assert _tags._env(suffix) == expected


def test_legacy_selection_and_user(monkeypatch):
    monkeypatch.setenv("SIGIL_AUTO_CODING_AGENT_TAGS", "true")
    monkeypatch.setenv("SIGIL_AUTO_CODING_AGENT_TAGS_NAMES", "user")
    monkeypatch.setenv("SIGIL_USER_ID", " legacy ")
    assert _tags.builtin_tags() == {"entrypoint": "hermes", "user": "legacy"}


@pytest.mark.parametrize("name", ["user", "repo", "branch"])
@pytest.mark.parametrize("value", ["", "  ", " normal ", " 😀" * 129])
def test_values_trimmed_capped_or_omitted(name, value, monkeypatch):
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES", name)
    monkeypatch.setattr(_tags.getpass, "getuser", lambda: value)
    monkeypatch.setattr(_tags, "_git_repo", lambda _: value)
    monkeypatch.setattr(_tags, "_git_branch", lambda _: value)
    tags = _tags.client_tags()
    key = _tags._TAG_KEYS[name]
    if value.strip():
        assert tags[key] == value.strip()[:128]
    else:
        assert key not in tags


def test_configured_user_wins_without_account_lookup(monkeypatch):
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES", "user")
    monkeypatch.setenv("AGENTO11Y_USER_ID", " configured ")
    monkeypatch.setattr(_tags.getpass, "getuser", lambda: pytest.fail("account lookup"))
    assert _tags.client_tags()["user"] == "configured"


def test_account_failure_keeps_other_tags(monkeypatch):
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")

    def deny():
        raise KeyError("account unavailable")

    monkeypatch.setattr(_tags.getpass, "getuser", deny)
    monkeypatch.setattr(_tags, "_git_repo", lambda _: "owner/repo")
    monkeypatch.setattr(_tags, "_git_branch", lambda _: "main")
    assert _tags.builtin_tags() == {"entrypoint": "hermes", "repo": "owner/repo", "git.branch": "main"}


def test_explicit_tags_skip_resolution_and_sdk_collisions(monkeypatch):
    monkeypatch.setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "true")
    explicit = {
        "user": "x" * 150,
        "repo": "chosen",
        "git.branch": "manual",
        "cwd": "/explicit",
        "entrypoint": "custom",
        "agento11y.framework.name": "custom",
    }
    monkeypatch.setenv("AGENTO11Y_TAGS", ",".join(f"{k}={v}" for k, v in explicit.items()))
    monkeypatch.setattr(os, "getcwd", lambda: pytest.fail("cwd lookup"))
    monkeypatch.setattr(_tags.getpass, "getuser", lambda: pytest.fail("user lookup"))
    assert not (explicit.keys() & _tags.client_tags().keys())
    assert _tags._explicit_tags() == explicit
    assert _tags.seed_tags() == {}


@pytest.mark.parametrize(
    ("raw", "expected"),
    [
        ("", {}),
        ("missing,=value,key=, = ,ok = a=b,ok=last", {"ok": "last"}),
        ("user=explicit, repo = owner/repo ", {"user": "explicit", "repo": "owner/repo"}),
    ],
)
def test_explicit_pair_parsing(raw, expected, monkeypatch):
    monkeypatch.setenv("AGENTO11Y_TAGS", raw)
    assert _tags._explicit_tags() == expected


@pytest.mark.parametrize(
    ("remote", "expected"),
    [
        ("", ""),
        ("  ", ""),
        ("git@host:owner/name.git", "owner/name"),
        ("https://user:secret@host/group/sub/name.git?token=secret#fragment", "group/sub/name"),
        ("ssh://git@host:2222/owner/name.git/", "owner/name"),
        ("file:///srv/repos/name.git/", "name"),
        ("/srv/git:odd/name.git", "name"),
        ("../name.git", "name"),
        ("https://host", ""),
        ("https://[broken/name", ""),
        ("g:owner/name", "owner/name"),
        ("host:", ""),
    ],
)
def test_remote_repository_names(remote, expected):
    assert _tags._repo_from_remote(remote) == expected


@pytest.mark.parametrize("config", ["", "broken config", '[remote "other"]\nurl = x'])
def test_repo_falls_back_to_checkout_name(config, tmp_path):
    root = _repo(tmp_path / "checkout", "ref: refs/heads/main")
    (root / ".git" / "config").write_text(config)
    assert _tags._git_repo(str(root)) == "checkout"


def test_repo_without_git_is_omitted(tmp_path):
    assert _tags._git_repo(str(tmp_path)) == ""


@pytest.mark.parametrize("absolute", [True, False])
@pytest.mark.parametrize("remote", [True, False])
def test_worktree_repo_uses_common_directory(absolute, remote, tmp_path):
    root = _repo(tmp_path / "main", "ref: refs/heads/main")
    common = root / ".git"
    worktree_git = common / "worktrees" / "side"
    worktree_git.mkdir(parents=True)
    (worktree_git / "commondir").write_text(str(common) if absolute else "../..")
    (worktree_git / "HEAD").write_text("ref: refs/heads/side")
    checkout = tmp_path / "side"
    checkout.mkdir()
    (checkout / ".git").write_text(f"gitdir: {worktree_git}")
    if remote:
        (common / "config").write_text('[remote "origin"]\nurl = https://secret@host/owner/repo.git\n')
    assert _tags._git_repo(str(checkout)) == ("owner/repo" if remote else "main")
    assert _tags._git_branch(str(checkout)) == "side"


def test_gitdir_name_fallback_for_submodule(tmp_path):
    store = tmp_path / "module.git"
    store.mkdir()
    checkout = tmp_path / "checkout"
    checkout.mkdir()
    (checkout / ".git").write_text(f"gitdir: {store}")
    assert _tags._git_repo(str(checkout)) == "module"


@pytest.mark.parametrize("filename", ["HEAD", "config", "commondir"])
def test_invalid_utf8_is_ignored(filename, tmp_path):
    _repo(tmp_path, "ref: refs/heads/main")
    (tmp_path / ".git" / filename).write_bytes(b"\xff")
    assert _tags._read_git_file(str(tmp_path / ".git" / filename)) == ""
    assert _tags._git_repo(str(tmp_path)) == tmp_path.name


def test_invalid_utf8_git_pointer_is_ignored(tmp_path):
    (tmp_path / ".git").write_bytes(b"\xff")
    assert _tags._find_git_dir(str(tmp_path)) == ""
