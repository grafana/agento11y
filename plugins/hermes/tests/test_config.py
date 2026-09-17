from __future__ import annotations

import base64
import logging

import pytest

from grafana_agento11y_hermes import _config


def _expected_basic(tenant: str, token: str) -> str:
    creds = base64.b64encode(f"{tenant}:{token}".encode()).decode()
    return f"Basic {creds}"


@pytest.fixture(autouse=True)
def _clear_auth_env(monkeypatch: pytest.MonkeyPatch) -> None:
    for var in (
        "AGENTO11Y_AUTH_MODE",
        "AGENTO11Y_AUTH_TENANT_ID",
        "AGENTO11Y_AUTH_TOKEN",
        "OTEL_EXPORTER_OTLP_ENDPOINT",
        "OTEL_EXPORTER_OTLP_HEADERS",
        "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT",
        "AGENTO11Y_HERMES_ERROR_FLUSH_TIMEOUT",
        "AGENTO11Y_HERMES_SAMPLE_RATE",
        "AGENTO11Y_HERMES_MAX_CHARS",
        "AGENTO11Y_HERMES_OTEL_AUTO",
        "AGENTO11Y_HEADERS",
    ):
        monkeypatch.delenv(var, raising=False)


def test_otel_auth_headers_derived_from_generations_creds(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_AUTH_TENANT_ID", "stack-1")
    monkeypatch.setenv("AGENTO11Y_AUTH_TOKEN", "glc_secret")
    assert _config._otel_auth_headers() == {
        "Authorization": _expected_basic("stack-1", "glc_secret"),
        "X-Scope-OrgID": "stack-1",
    }


def test_otel_auth_headers_empty_without_tenant(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_AUTH_TOKEN", "glc_secret")
    assert _config._otel_auth_headers() == {}


def test_otel_auth_headers_empty_without_token(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_AUTH_TENANT_ID", "stack-1")
    assert _config._otel_auth_headers() == {}


def test_otel_auth_headers_skipped_for_bearer_mode(monkeypatch: pytest.MonkeyPatch) -> None:
    """A bearer token is not a basic password — don't derive basic auth from it."""
    monkeypatch.setenv("AGENTO11Y_AUTH_MODE", "bearer")
    monkeypatch.setenv("AGENTO11Y_AUTH_TENANT_ID", "stack-1")
    monkeypatch.setenv("AGENTO11Y_AUTH_TOKEN", "glc_secret")
    assert _config._otel_auth_headers() == {}


def test_load_populates_otel_auth_headers(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_AUTH_TENANT_ID", "stack-1")
    monkeypatch.setenv("AGENTO11Y_AUTH_TOKEN", "glc_secret")
    cfg = _config.load()
    assert cfg.otel_auth_headers == {
        "Authorization": _expected_basic("stack-1", "glc_secret"),
        "X-Scope-OrgID": "stack-1",
    }


def test_load_otel_auth_headers_empty_when_no_creds() -> None:
    assert _config.load().otel_auth_headers == {}


# --- OTLP endpoint resolution ---


def test_otel_unconfigured_without_any_endpoint() -> None:
    cfg = _config.load()
    assert cfg.otel_configured is False
    assert cfg.otel_endpoint_override == ""


def test_standard_endpoint_needs_no_override(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otlp.example/otlp")
    cfg = _config.load()
    assert cfg.otel_configured is True
    assert cfg.otel_endpoint_override == ""


def test_branded_endpoint_configures_otel(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "https://otlp.example/otlp")
    cfg = _config.load()
    assert cfg.otel_configured is True
    assert cfg.otel_endpoint_override == "https://otlp.example/otlp"


def test_standard_endpoint_wins_over_branded(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "https://branded.example/otlp")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://standard.example/otlp")
    cfg = _config.load()
    assert cfg.otel_endpoint_override == "", "the exporters read the standard env themselves"


def test_blank_branded_endpoint_is_unset(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "   ")
    cfg = _config.load()
    assert cfg.otel_configured is False
    assert cfg.otel_endpoint_override == ""


# --- error flush timeout ---


def test_error_flush_timeout_defaults_to_two_seconds() -> None:
    assert _config.load().error_flush_timeout == 2.0


def test_error_flush_timeout_from_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_HERMES_ERROR_FLUSH_TIMEOUT", "0.5")
    assert _config.load().error_flush_timeout == 0.5


def test_error_flush_timeout_zero_disables(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_HERMES_ERROR_FLUSH_TIMEOUT", "0")
    assert _config.load().error_flush_timeout == 0.0


# --- env parsing ---
#
# An unreadable value has to fall back to the default and say so, rather than
# raising out of load() and taking the whole plugin down at import time.


@pytest.mark.parametrize(
    ("raw", "expected"),
    (("0.5", 0.5), ("1", 1.0), (" 0.25 ", 0.25), ("", 1.0), ("   ", 1.0), ("half", 1.0), ("1,5", 1.0)),
)
def test_a_float_env_falls_back_to_its_default(monkeypatch: pytest.MonkeyPatch, raw: str, expected: float) -> None:
    monkeypatch.setenv("AGENTO11Y_HERMES_SAMPLE_RATE", raw)
    assert _config.load().sample_rate == expected


@pytest.mark.parametrize(
    ("raw", "expected"),
    (("500", 500), (" 500 ", 500), ("", 12000), ("lots", 12000), ("1.5", 12000)),
)
def test_an_int_env_falls_back_to_its_default(monkeypatch: pytest.MonkeyPatch, raw: str, expected: int) -> None:
    monkeypatch.setenv("AGENTO11Y_HERMES_MAX_CHARS", raw)
    assert _config.load().max_chars == expected


def test_an_unreadable_env_value_is_logged(monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture) -> None:
    monkeypatch.setenv("AGENTO11Y_HERMES_MAX_CHARS", "lots")
    with caplog.at_level(logging.WARNING):
        _config.load()
    assert [r for r in caplog.records if "AGENTO11Y_HERMES_MAX_CHARS" in r.getMessage()]


@pytest.mark.parametrize(
    ("raw", "expected"),
    (
        ("1", True),
        ("true", True),
        ("TRUE", True),
        ("yes", True),
        ("on", True),
        ("0", False),
        ("false", False),
        ("no", False),
        ("anything else", False),
        ("", True),
        ("  ", True),
    ),
)
def test_a_bool_env_reads_the_usual_spellings(monkeypatch: pytest.MonkeyPatch, raw: str, expected: bool) -> None:
    monkeypatch.setenv("AGENTO11Y_HERMES_OTEL_AUTO", raw)
    assert _config.load().otel_auto is expected


# --- generations channel presence ---


@pytest.mark.parametrize(
    ("env", "expected"),
    (
        ({}, False),
        ({"AGENTO11Y_AUTH_TOKEN": "glc_secret"}, True),
        ({"AGENTO11Y_AUTH_TOKEN": "  "}, False),
        ({"AGENTO11Y_AUTH_MODE": "basic"}, True),
        ({"AGENTO11Y_AUTH_MODE": "bearer"}, True),
        ({"AGENTO11Y_AUTH_MODE": "none"}, False),
        ({"AGENTO11Y_AUTH_MODE": "NONE"}, False),
    ),
)
def test_the_generations_channel_is_configured_by_token_or_mode(
    monkeypatch: pytest.MonkeyPatch, env: dict[str, str], expected: bool
) -> None:
    for key, value in env.items():
        monkeypatch.setenv(key, value)
    assert _config.load().generations_configured is expected


# --- header parsing ---


@pytest.mark.parametrize(
    ("raw", "expected"),
    (
        ("", {}),
        ("X-Foo=bar", {"X-Foo": "bar"}),
        ("X-Foo=bar,X-Baz=qux", {"X-Foo": "bar", "X-Baz": "qux"}),
        (" X-Foo = bar , X-Baz=qux ", {"X-Foo": "bar", "X-Baz": "qux"}),
        ("Authorization=Basic dGVzdA==", {"Authorization": "Basic dGVzdA=="}),
        ("novalue,X-Foo=bar", {"X-Foo": "bar"}),
        ("=orphan,X-Foo=bar", {"X-Foo": "bar"}),
        (",,", {}),
        ("X-Empty=", {"X-Empty": ""}),
    ),
)
def test_export_headers_parse_like_the_sdk(monkeypatch: pytest.MonkeyPatch, raw: str, expected: dict[str, str]) -> None:
    monkeypatch.setenv("AGENTO11Y_HEADERS", raw)
    assert _config.load().export_headers == expected
