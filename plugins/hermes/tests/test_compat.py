"""Legacy env var promotion.

``_compat`` promotes supported aliases before plugin or SDK config is read. These tests drive ``apply_legacy_env`` with an explicit dict,
which also bypasses its once-per-process guard.
"""

from __future__ import annotations

from typing import Any

import pytest

from grafana_agento11y_hermes import _compat


def test_old_name_is_copied_not_moved() -> None:
    """Hermes tool subprocesses inherit the environment, so the old name stays."""
    env = {"SIGIL_AUTH_TOKEN": "glc_secret"}

    promoted = _compat.apply_legacy_env(env)

    assert promoted == ["SIGIL_AUTH_TOKEN"]
    assert env == {"SIGIL_AUTH_TOKEN": "glc_secret", "AGENTO11Y_AUTH_TOKEN": "glc_secret"}


def test_new_name_wins_when_both_are_set() -> None:
    env = {"SIGIL_AUTH_TOKEN": "old", "AGENTO11Y_AUTH_TOKEN": "new"}

    promoted = _compat.apply_legacy_env(env)

    assert promoted == []
    assert env == {"SIGIL_AUTH_TOKEN": "old", "AGENTO11Y_AUTH_TOKEN": "new"}


@pytest.mark.parametrize(
    ("old", "new"),
    [
        # These two aliases rename more than the prefix.
        ("SIGIL_API_ENDPOINT", "AGENTO11Y_ENDPOINT"),
        ("SIGIL_TENANT_ID", "AGENTO11Y_AUTH_TENANT_ID"),
        ("SIGIL_REDACT_INPUT_MESSAGES", "AGENTO11Y_REDACT_INPUT_MESSAGES"),
        ("SIGIL_SERVICE_ACCOUNT_TOKEN", "AGENTO11Y_SERVICE_ACCOUNT_TOKEN"),
        ("SIGIL_DEBUG", "AGENTO11Y_DEBUG"),
        ("SIGIL_HEADERS", "AGENTO11Y_HEADERS"),
        ("SIGIL_HERMES_MAX_CHARS", "AGENTO11Y_HERMES_MAX_CHARS"),
    ],
)
def test_renames_cover_transport_privacy_and_plugin_settings(old: str, new: str) -> None:
    env = {old: "v"}

    _compat.apply_legacy_env(env)

    assert env == {old: "v", new: "v"}


def test_local_rename_table_covers_credentials() -> None:
    """Credentials and privacy aliases do not depend on SDK internals."""
    table = _compat.renames()

    assert table["SIGIL_AUTH_TOKEN"] == "AGENTO11Y_AUTH_TOKEN"
    assert table["SIGIL_CONTENT_CAPTURE_MODE"] == "AGENTO11Y_CONTENT_CAPTURE_MODE"


def test_aliases_survive_an_sdk_without_the_table(monkeypatch: pytest.MonkeyPatch) -> None:
    import builtins

    real_import = builtins.__import__

    # The parameters are spelled out rather than taken as *args, because the
    # shim is checked against the real ``__import__`` signature.
    def guarded(name: str, globals: Any = None, locals: Any = None, fromlist: Any = (), level: int = 0) -> Any:
        if name == "agento11y.config":
            raise ImportError("no config module")
        return real_import(name, globals, locals, fromlist, level)

    monkeypatch.setattr(builtins, "__import__", guarded)

    table = _compat.renames()

    assert table["SIGIL_AUTH_TOKEN"] == "AGENTO11Y_AUTH_TOKEN"
    assert table["SIGIL_HERMES_MAX_CHARS"] == "AGENTO11Y_HERMES_MAX_CHARS"


def test_unrelated_names_are_untouched() -> None:
    env = {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otlp", "HERMES_SIGIL_API_KEY": "stale"}

    promoted = _compat.apply_legacy_env(env)

    assert promoted == []
    assert env == {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otlp", "HERMES_SIGIL_API_KEY": "stale"}


def test_os_environ_path_runs_once(monkeypatch: pytest.MonkeyPatch) -> None:
    _compat._reset_for_tests()
    monkeypatch.setenv("SIGIL_AUTH_TOKEN", "glc_secret")
    monkeypatch.delenv("AGENTO11Y_AUTH_TOKEN", raising=False)

    first = _compat.apply_legacy_env()

    import os

    assert first == ["SIGIL_AUTH_TOKEN"]
    assert os.environ["AGENTO11Y_AUTH_TOKEN"] == "glc_secret"
    assert os.environ["SIGIL_AUTH_TOKEN"] == "glc_secret"

    monkeypatch.setenv("SIGIL_AUTH_TOKEN", "later")
    assert _compat.apply_legacy_env() == []
    assert os.environ["AGENTO11Y_AUTH_TOKEN"] == "glc_secret"

    _compat._reset_for_tests()
