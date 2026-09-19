"""Tests for the generation-export User-Agent token."""

from __future__ import annotations

import builtins
from typing import Any

import pytest

from grafana_agento11y_hermes import _version


@pytest.fixture
def sdk_version_import_fails(monkeypatch: pytest.MonkeyPatch) -> None:
    """Make ``from agento11y.version import user_agent`` fail, as an older SDK does."""
    real_import = builtins.__import__

    # The parameters are spelled out rather than taken as *args, because the
    # shim is checked against the real ``__import__`` signature.
    def guarded(name: str, globals: Any = None, locals: Any = None, fromlist: Any = (), level: int = 0) -> Any:
        if name == "agento11y.version":
            raise ImportError("no version module in this SDK")
        return real_import(name, globals, locals, fromlist, level)

    monkeypatch.setattr(builtins, "__import__", guarded)


def test_plugin_user_agent_format() -> None:
    ua = _version.plugin_user_agent()
    plugin, sdk = ua.split(" ", 1)
    assert plugin.startswith("agento11y-plugin-hermes/")
    assert plugin.split("/", 1)[1]  # non-empty version
    assert sdk.startswith("agento11y-sdk-python/")
    assert sdk.split("/", 1)[1]


def test_plugin_user_agent_falls_back_when_metadata_missing(monkeypatch) -> None:
    def boom(name: str) -> str:
        raise _version.PackageNotFoundError(name)

    monkeypatch.setattr(_version, "version", boom)
    ua = _version.plugin_user_agent()
    assert ua.startswith("agento11y-plugin-hermes/dev ")


def test_the_sdk_token_falls_back_to_package_metadata(sdk_version_import_fails: None) -> None:
    """An SDK without ``agento11y.version`` still gets a version-carrying token."""
    sdk = _version._sdk_user_agent()
    assert sdk.startswith("agento11y-sdk-python/")
    assert sdk.split("/", 1)[1] not in ("", "unknown")


def test_the_sdk_token_is_unknown_when_nothing_can_report_a_version(
    sdk_version_import_fails: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    def boom(name: str) -> str:
        raise _version.PackageNotFoundError(name)

    monkeypatch.setattr(_version, "version", boom)
    assert _version._sdk_user_agent() == "agento11y-sdk-python/unknown"
