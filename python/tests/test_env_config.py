"""Tests for canonical AGENTO11Y_* environment resolution in the Python SDK."""

from __future__ import annotations

import logging
from collections.abc import Callable
from datetime import timedelta

import pytest
from agento11y import ApiConfig, Client, ClientConfig, GenerationExportConfig
from agento11y.config import _WARNED_LEGACY_ENV, default_config, resolve_config
from agento11y.models import ContentCaptureMode, GenerationStart, ModelRef

_DEFAULT_EXPORT_TIMEOUT = timedelta(seconds=30)


def test_client_warns_when_metrics_provider_is_not_registered(monkeypatch, caplog: pytest.LogCaptureFixture) -> None:
    class ProxyMeterProvider:
        pass

    monkeypatch.setattr("agento11y.client.metrics.get_meter_provider", ProxyMeterProvider)
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        client = Client(
            ClientConfig(
                generation_export=GenerationExportConfig(protocol="none"),
            )
        )
        client.shutdown()

    assert any("OTel metrics are not configured" in record.getMessage() for record in caplog.records)


def _check_no_env(cfg: ClientConfig) -> None:
    assert cfg.generation_export.endpoint == "localhost:4317"
    assert cfg.generation_export.protocol == "grpc"
    assert cfg.generation_export.insecure is False
    assert cfg.generation_export.auth.mode == "none"
    assert cfg.generation_export.export_timeout == _DEFAULT_EXPORT_TIMEOUT
    assert cfg.generation_export.max_retries == 5
    assert cfg.generation_export.max_backoff == timedelta(seconds=5)
    assert cfg.generation_export.queue_size == 2000
    assert cfg.agent_name == ""
    assert cfg.debug is False
    assert cfg.use_experimental_otel is False


def _check_transport(cfg: ClientConfig) -> None:
    assert cfg.generation_export.endpoint == "https://env:4318"
    assert cfg.generation_export.protocol == "http"
    assert cfg.generation_export.insecure is True
    auth = cfg.generation_export.auth
    assert auth.mode == "basic"
    assert auth.tenant_id == "42"
    assert auth.basic_user == "42"
    assert auth.basic_password == "glc_xxx"


def _check_bearer(cfg: ClientConfig) -> None:
    assert cfg.generation_export.auth.mode == "bearer"
    assert cfg.generation_export.auth.bearer_token == "tok"


def _check_agent_user_tags(cfg: ClientConfig) -> None:
    assert cfg.agent_name == "planner"
    assert cfg.agent_version == "1.2.3"
    assert cfg.user_id == "alice@example.com"
    assert cfg.tags == {"service": "orchestrator", "env": "prod"}
    assert cfg.debug is True


def _check_content_capture_metadata(cfg: ClientConfig) -> None:
    assert cfg.content_capture == ContentCaptureMode.METADATA_ONLY


def _check_content_capture_full_with_metadata_spans(cfg: ClientConfig) -> None:
    assert cfg.content_capture == ContentCaptureMode.FULL_WITH_METADATA_SPANS


def _check_experimental_otel(cfg: ClientConfig) -> None:
    assert cfg.use_experimental_otel is True


def _check_invalid_auth_mode_preserves_valid(cfg: ClientConfig) -> None:
    assert cfg.generation_export.endpoint == "valid.example:4318"
    assert cfg.agent_name == "valid-agent"
    # Auth mode reverted to 'none' since the env value was rejected.
    assert cfg.generation_export.auth.mode == "none"


def _check_stray_tenant_does_not_error(cfg: ClientConfig) -> None:
    assert cfg.generation_export.auth.mode == "none"


def _check_export_timeout(cfg: ClientConfig) -> None:
    assert cfg.generation_export.export_timeout == timedelta(milliseconds=1500)


def _check_export_timeout_min(cfg: ClientConfig) -> None:
    assert cfg.generation_export.export_timeout == timedelta(milliseconds=1)


def _check_export_timeout_max(cfg: ClientConfig) -> None:
    assert cfg.generation_export.export_timeout == timedelta(milliseconds=2147483647)


def _check_generation_export_tuning(cfg: ClientConfig) -> None:
    assert cfg.generation_export.max_retries == 12
    assert cfg.generation_export.max_backoff == timedelta(milliseconds=45000)
    assert cfg.generation_export.queue_size == 5000


def _check_invalid_export_timeout_preserves_valid(cfg: ClientConfig) -> None:
    # The bad timeout is dropped, the sibling env var in the same batch still applies.
    assert cfg.generation_export.export_timeout == _DEFAULT_EXPORT_TIMEOUT
    assert cfg.generation_export.endpoint == "valid.example:4318"


def _check_legacy_export_timeout_ignored(cfg: ClientConfig) -> None:
    assert cfg.generation_export.export_timeout == _DEFAULT_EXPORT_TIMEOUT


def _check_legacy_transport(cfg: ClientConfig) -> None:
    assert cfg.generation_export.endpoint == "https://legacy:4318"
    assert cfg.generation_export.protocol == "http"
    assert cfg.generation_export.insecure is True
    assert cfg.generation_export.headers["X-A"] == "1"
    assert cfg.generation_export.auth.tenant_id == "42"


def _check_legacy_agent_user_tags(cfg: ClientConfig) -> None:
    assert cfg.user_id == "alice@example.com"
    assert cfg.tags == {"service": "orchestrator"}
    assert cfg.debug is True


def _check_legacy_api_endpoint(cfg: ClientConfig) -> None:
    assert cfg.generation_export.endpoint == "https://legacy-api:4318"


def _check_legacy_tenant_id(cfg: ClientConfig) -> None:
    assert cfg.generation_export.auth.tenant_id == "legacy-42"


def _check_canonical_wins_over_legacy(cfg: ClientConfig) -> None:
    assert cfg.generation_export.endpoint == "https://canonical:4318"
    # AGENTO11Y_TAGS supplies the whole map; SIGIL_TAGS adds nothing to it.
    assert cfg.tags == {"a": "1"}


def _check_blank_canonical_falls_back(cfg: ClientConfig) -> None:
    assert cfg.generation_export.endpoint == "https://legacy:4318"


def _check_invalid_canonical_blocks_valid_legacy(cfg: ClientConfig) -> None:
    # A nonblank canonical value is selected before validation, so a valid
    # legacy value never resurfaces behind it.
    assert cfg.content_capture == ContentCaptureMode.DEFAULT


@pytest.mark.parametrize(
    "env,check",
    [
        pytest.param({}, _check_no_env, id="no env uses defaults"),
        pytest.param(
            {
                "AGENTO11Y_ENDPOINT": "https://env:4318",
                "AGENTO11Y_PROTOCOL": "http",
                "AGENTO11Y_INSECURE": "true",
                "AGENTO11Y_HEADERS": "X-A=1,X-B=two",
                "AGENTO11Y_AUTH_MODE": "basic",
                "AGENTO11Y_AUTH_TENANT_ID": "42",
                "AGENTO11Y_AUTH_TOKEN": "glc_xxx",
            },
            _check_transport,
            id="transport from env",
        ),
        pytest.param(
            {"AGENTO11Y_AUTH_MODE": "bearer", "AGENTO11Y_AUTH_TOKEN": "tok"},
            _check_bearer,
            id="bearer auth from env",
        ),
        pytest.param(
            {
                "AGENTO11Y_AGENT_NAME": "planner",
                "AGENTO11Y_AGENT_VERSION": "1.2.3",
                "AGENTO11Y_USER_ID": "alice@example.com",
                "AGENTO11Y_TAGS": "service=orchestrator,env=prod",
                "AGENTO11Y_DEBUG": "true",
            },
            _check_agent_user_tags,
            id="agent user tags debug from env",
        ),
        pytest.param(
            {"AGENTO11Y_CONTENT_CAPTURE_MODE": "metadata_only"},
            _check_content_capture_metadata,
            id="content capture mode from env",
        ),
        pytest.param(
            {"AGENTO11Y_CONTENT_CAPTURE_MODE": "full_with_metadata_spans"},
            _check_content_capture_full_with_metadata_spans,
            id="full_with_metadata_spans content capture mode from env",
        ),
        pytest.param(
            {"AGENTO11Y_USE_EXPERIMENTAL_OTEL": "true"},
            _check_experimental_otel,
            id="experimental otel opt-in from env",
        ),
        pytest.param(
            {
                "AGENTO11Y_AUTH_MODE": "Bearrer",
                "AGENTO11Y_ENDPOINT": "valid.example:4318",
                "AGENTO11Y_AGENT_NAME": "valid-agent",
            },
            _check_invalid_auth_mode_preserves_valid,
            id="invalid auth mode preserves other valid env",
        ),
        pytest.param(
            {"AGENTO11Y_AUTH_TENANT_ID": "42"},
            _check_stray_tenant_does_not_error,
            id="stray AGENTO11Y_AUTH_TENANT_ID does not error",
        ),
        pytest.param(
            {"AGENTO11Y_EXPORT_TIMEOUT_MS": "1500"},
            _check_export_timeout,
            id="export timeout from env",
        ),
        pytest.param(
            {"AGENTO11Y_EXPORT_TIMEOUT_MS": "1"},
            _check_export_timeout_min,
            id="export timeout accepts inclusive minimum",
        ),
        pytest.param(
            {"AGENTO11Y_EXPORT_TIMEOUT_MS": "2147483647"},
            _check_export_timeout_max,
            id="export timeout accepts inclusive maximum",
        ),
        pytest.param(
            {
                "AGENTO11Y_MAX_RETRIES": "12",
                "AGENTO11Y_MAX_BACKOFF_MS": "45000",
                "AGENTO11Y_QUEUE_SIZE": "5000",
            },
            _check_generation_export_tuning,
            id="generation export tuning from env",
        ),
        pytest.param(
            {
                "AGENTO11Y_EXPORT_TIMEOUT_MS": "abc",
                "AGENTO11Y_ENDPOINT": "valid.example:4318",
            },
            _check_invalid_export_timeout_preserves_valid,
            id="invalid export timeout preserves other valid env",
        ),
        pytest.param(
            {"SIGIL_EXPORT_TIMEOUT_MS": "1500"},
            _check_legacy_export_timeout_ignored,
            id="legacy SIGIL export timeout is never read",
        ),
        pytest.param(
            {
                "SIGIL_ENDPOINT": "https://legacy:4318",
                "SIGIL_PROTOCOL": "http",
                "SIGIL_INSECURE": "true",
                "SIGIL_HEADERS": "X-A=1",
                "SIGIL_AUTH_TENANT_ID": "42",
            },
            _check_legacy_transport,
            id="legacy transport names still resolve",
        ),
        pytest.param(
            {
                "SIGIL_USER_ID": "alice@example.com",
                "SIGIL_TAGS": "service=orchestrator",
                "SIGIL_DEBUG": "true",
            },
            _check_legacy_agent_user_tags,
            id="legacy user tags debug still resolve",
        ),
        pytest.param(
            {"SIGIL_API_ENDPOINT": "https://legacy-api:4318"},
            _check_legacy_api_endpoint,
            id="legacy SIGIL_API_ENDPOINT resolves the endpoint",
        ),
        pytest.param(
            {"SIGIL_TENANT_ID": "legacy-42"},
            _check_legacy_tenant_id,
            id="legacy SIGIL_TENANT_ID resolves the tenant",
        ),
        pytest.param(
            {
                "AGENTO11Y_ENDPOINT": "https://canonical:4318",
                "SIGIL_ENDPOINT": "https://legacy:4318",
                "AGENTO11Y_TAGS": "a=1",
                "SIGIL_TAGS": "b=2",
            },
            _check_canonical_wins_over_legacy,
            id="canonical wins over legacy",
        ),
        pytest.param(
            {"AGENTO11Y_ENDPOINT": "   ", "SIGIL_ENDPOINT": "https://legacy:4318"},
            _check_blank_canonical_falls_back,
            id="blank canonical falls back to legacy",
        ),
        pytest.param(
            {
                "AGENTO11Y_CONTENT_CAPTURE_MODE": "bogus",
                "SIGIL_CONTENT_CAPTURE_MODE": "metadata_only",
            },
            _check_invalid_canonical_blocks_valid_legacy,
            id="invalid canonical blocks valid legacy",
        ),
    ],
)
def test_resolve_config_env(env: dict[str, str], check: Callable[[ClientConfig], None]) -> None:
    cfg = resolve_config(None, env=env)
    check(cfg)


@pytest.mark.parametrize(
    "raw",
    [
        pytest.param("0", id="zero is below the inclusive minimum"),
        pytest.param("-1", id="negative"),
        pytest.param("1.5", id="fractional"),
        pytest.param("abc", id="non-numeric"),
        pytest.param("2147483648", id="above the inclusive maximum"),
    ],
)
def test_invalid_export_timeout_warns_and_keeps_default(raw: str, caplog: pytest.LogCaptureFixture) -> None:
    """Bad AGENTO11Y_EXPORT_TIMEOUT_MS warns, keeps 30s, and lets siblings apply."""
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        cfg = resolve_config(
            None,
            env={"AGENTO11Y_EXPORT_TIMEOUT_MS": raw, "AGENTO11Y_AGENT_NAME": "planner"},
        )
    assert cfg.generation_export.export_timeout == _DEFAULT_EXPORT_TIMEOUT
    assert cfg.agent_name == "planner"
    assert any("AGENTO11Y_EXPORT_TIMEOUT_MS" in record.getMessage() for record in caplog.records)


def test_legacy_export_timeout_is_not_read(caplog: pytest.LogCaptureFixture) -> None:
    """AGENTO11Y_EXPORT_TIMEOUT_MS postdates the rename: SIGIL_EXPORT_TIMEOUT_MS is unused and warns once."""

    with caplog.at_level(logging.WARNING, logger="agento11y"):
        cfg = resolve_config(None, env={"SIGIL_EXPORT_TIMEOUT_MS": "1500"})

    assert cfg.generation_export.export_timeout == _DEFAULT_EXPORT_TIMEOUT
    assert any(
        "SIGIL_EXPORT_TIMEOUT_MS is ignored; rename it to AGENTO11Y_EXPORT_TIMEOUT_MS" in record.getMessage()
        for record in caplog.records
    )


def test_export_timeout_default_is_thirty_seconds() -> None:
    """Both the raw dataclass and the resolved config expose the 30s default."""
    assert GenerationExportConfig().export_timeout == _DEFAULT_EXPORT_TIMEOUT
    assert default_config().generation_export.export_timeout == _DEFAULT_EXPORT_TIMEOUT


def test_explicit_export_timeout_beats_env() -> None:
    explicit = ClientConfig(generation_export=GenerationExportConfig(export_timeout=timedelta(seconds=5)))
    cfg = resolve_config(explicit, env={"AGENTO11Y_EXPORT_TIMEOUT_MS": "1500"})
    assert cfg.generation_export.export_timeout == timedelta(seconds=5)


def test_explicit_generation_export_tuning_beats_env() -> None:
    explicit = ClientConfig(
        generation_export=GenerationExportConfig(
            max_retries=5,
            max_backoff=timedelta(seconds=5),
            queue_size=2000,
        )
    )
    cfg = resolve_config(
        explicit,
        env={
            "AGENTO11Y_MAX_RETRIES": "12",
            "AGENTO11Y_MAX_BACKOFF_MS": "45000",
            "AGENTO11Y_QUEUE_SIZE": "5000",
        },
    )
    assert cfg.generation_export.max_retries == 5
    assert cfg.generation_export.max_backoff == timedelta(seconds=5)
    assert cfg.generation_export.queue_size == 2000


@pytest.mark.parametrize(
    "key,raw,field,default,minimum",
    [
        pytest.param("AGENTO11Y_MAX_RETRIES", "0", "max_retries", 5, 1, id="zero retries"),
        pytest.param("AGENTO11Y_MAX_RETRIES", "-1", "max_retries", 5, 1, id="negative retries"),
        pytest.param("AGENTO11Y_MAX_RETRIES", "1.5", "max_retries", 5, 1, id="fractional retries"),
        pytest.param("AGENTO11Y_MAX_RETRIES", "abc", "max_retries", 5, 1, id="non-numeric retries"),
        pytest.param("AGENTO11Y_MAX_RETRIES", "2147483648", "max_retries", 5, 1, id="retries above max"),
        pytest.param("AGENTO11Y_MAX_BACKOFF_MS", "0", "max_backoff", timedelta(seconds=5), 1, id="zero backoff"),
        pytest.param("AGENTO11Y_MAX_BACKOFF_MS", "-1", "max_backoff", timedelta(seconds=5), 1, id="negative backoff"),
        pytest.param(
            "AGENTO11Y_MAX_BACKOFF_MS", "1.5", "max_backoff", timedelta(seconds=5), 1, id="fractional backoff"
        ),
        pytest.param(
            "AGENTO11Y_MAX_BACKOFF_MS", "abc", "max_backoff", timedelta(seconds=5), 1, id="non-numeric backoff"
        ),
        pytest.param(
            "AGENTO11Y_MAX_BACKOFF_MS", "2147483648", "max_backoff", timedelta(seconds=5), 1, id="backoff above max"
        ),
        pytest.param("AGENTO11Y_QUEUE_SIZE", "0", "queue_size", 2000, 1, id="zero queue size"),
        pytest.param("AGENTO11Y_QUEUE_SIZE", "-1", "queue_size", 2000, 1, id="negative queue size"),
        pytest.param("AGENTO11Y_QUEUE_SIZE", "1.5", "queue_size", 2000, 1, id="fractional queue size"),
        pytest.param("AGENTO11Y_QUEUE_SIZE", "abc", "queue_size", 2000, 1, id="non-numeric queue size"),
        pytest.param("AGENTO11Y_QUEUE_SIZE", "2147483648", "queue_size", 2000, 1, id="queue size above max"),
    ],
)
def test_invalid_generation_export_tuning_warns_and_keeps_default(
    key: str,
    raw: str,
    field: str,
    default: int | timedelta,
    minimum: int,
    caplog: pytest.LogCaptureFixture,
) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        cfg = resolve_config(None, env={key: raw, "AGENTO11Y_AGENT_NAME": "planner"})

    assert getattr(cfg.generation_export, field) == default
    assert cfg.agent_name == "planner"
    assert any(
        key in record.getMessage() and f"from {minimum} through 2147483647" in record.getMessage()
        for record in caplog.records
    )


@pytest.mark.parametrize(
    "caller_timeout",
    [
        pytest.param(timedelta(0), id="zero"),
        pytest.param(timedelta(seconds=-3), id="negative"),
    ],
)
def test_invalid_caller_export_timeout_is_clamped(caller_timeout: timedelta) -> None:
    """Non-positive caller values are clamped like the neighbouring interval fields."""
    explicit = ClientConfig(generation_export=GenerationExportConfig(export_timeout=caller_timeout))
    cfg = resolve_config(explicit, env={})
    assert cfg.generation_export.export_timeout == _DEFAULT_EXPORT_TIMEOUT


def test_explicit_overrides_env() -> None:
    explicit = ClientConfig()
    explicit.generation_export.endpoint = "https://explicit:4318"
    cfg = resolve_config(
        explicit,
        env={"AGENTO11Y_ENDPOINT": "https://env:4318", "AGENTO11Y_AGENT_NAME": "planner"},
    )
    assert cfg.generation_export.endpoint == "https://explicit:4318"
    assert cfg.agent_name == "planner"


@pytest.mark.parametrize(
    "env,expected_key",
    [
        pytest.param({"AGENTO11Y_CONTENT_CAPTURE_MODE": "bogus"}, "AGENTO11Y_CONTENT_CAPTURE_MODE", id="canonical"),
        pytest.param({"SIGIL_CONTENT_CAPTURE_MODE": "bogus"}, "SIGIL_CONTENT_CAPTURE_MODE", id="legacy"),
    ],
)
def test_invalid_capture_mode_warning_names_selected_key(
    env: dict[str, str], expected_key: str, caplog: pytest.LogCaptureFixture
) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        cfg = resolve_config(None, env=env)
    assert cfg.content_capture == ContentCaptureMode.DEFAULT
    assert any(f"ignoring invalid {expected_key}" in r.getMessage() for r in caplog.records)


def test_legacy_env_namespace_resolves_with_deprecation_warning(caplog: pytest.LogCaptureFixture) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        cfg = resolve_config(None, env={"SIGIL_ENDPOINT": "https://legacy.example"})
    assert cfg.generation_export.endpoint == "https://legacy.example"
    assert any(
        "SIGIL_ENDPOINT is deprecated; rename it to AGENTO11Y_ENDPOINT" in record.getMessage()
        for record in caplog.records
    )


def test_legacy_env_warning_fires_once_per_process(caplog: pytest.LogCaptureFixture) -> None:
    env = {"SIGIL_ENDPOINT": "https://legacy.example"}
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        resolve_config(None, env=env)
        resolve_config(None, env=env)
    warnings = [r for r in caplog.records if "SIGIL_ENDPOINT" in r.getMessage()]
    assert len(warnings) == 1

    _WARNED_LEGACY_ENV.clear()
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        resolve_config(None, env=env)
    assert len([r for r in caplog.records if "SIGIL_ENDPOINT" in r.getMessage()]) == 2


def test_unused_legacy_env_name_does_not_warn(caplog: pytest.LogCaptureFixture) -> None:
    """The warning fires at the point of use, not on every legacy name present."""
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        cfg = resolve_config(
            None,
            env={"AGENTO11Y_ENDPOINT": "https://canonical.example", "SIGIL_ENDPOINT": "https://legacy.example"},
        )
    assert cfg.generation_export.endpoint == "https://canonical.example"
    assert not [r for r in caplog.records if "SIGIL_ENDPOINT" in r.getMessage()]


def test_canonical_only_legacy_name_stays_silent_when_canonical_is_set(caplog: pytest.LogCaptureFixture) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        cfg = resolve_config(
            None,
            env={"AGENTO11Y_EXPORT_TIMEOUT_MS": "5000", "SIGIL_EXPORT_TIMEOUT_MS": "9000"},
        )
    assert cfg.generation_export.export_timeout == timedelta(milliseconds=5000)
    assert not [r for r in caplog.records if "SIGIL_EXPORT_TIMEOUT_MS" in r.getMessage()]


def test_canonical_only_legacy_warning_fires_once_per_process(caplog: pytest.LogCaptureFixture) -> None:
    env = {"SIGIL_EXPORT_TIMEOUT_MS": "5000"}
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        resolve_config(None, env=env)
        resolve_config(None, env=env)
    assert len([r for r in caplog.records if "SIGIL_EXPORT_TIMEOUT_MS" in r.getMessage()]) == 1


def test_agento11y_endpoint_also_defaults_api_endpoint() -> None:
    cfg = resolve_config(None, env={"AGENTO11Y_ENDPOINT": "https://sigil.example"})
    assert cfg.generation_export.endpoint == "https://sigil.example"
    assert cfg.api.endpoint == "https://sigil.example"


def test_explicit_api_endpoint_overrides_agento11y_endpoint() -> None:
    explicit = ClientConfig(api=ApiConfig(endpoint="https://api.example"))
    cfg = resolve_config(explicit, env={"AGENTO11Y_ENDPOINT": "https://ingest.example"})
    assert cfg.generation_export.endpoint == "https://ingest.example"
    assert cfg.api.endpoint == "https://api.example"


def test_caller_bearer_mode_wins_over_env_basic_mode() -> None:
    """Caller mode wins; env mode-incompatible credentials are silently ignored."""
    explicit = ClientConfig()
    explicit.generation_export.auth.mode = "bearer"
    explicit.generation_export.auth.bearer_token = "callertok"
    cfg = resolve_config(
        explicit,
        env={
            "AGENTO11Y_AUTH_MODE": "basic",
            "AGENTO11Y_AUTH_TENANT_ID": "42",
            "AGENTO11Y_AUTH_TOKEN": "envpass",
        },
    )
    assert cfg.generation_export.auth.mode == "bearer"
    assert cfg.generation_export.auth.bearer_token == "callertok"
    # Authorization header carries the caller's bearer token, not env's password.
    assert cfg.generation_export.headers["Authorization"] == "Bearer callertok"


def test_caller_tags_merge_with_env_tags() -> None:
    """Env tags layer under caller tags; caller wins on key collision."""
    explicit = ClientConfig(tags={"team": "ai", "env": "staging"})
    cfg = resolve_config(explicit, env={"AGENTO11Y_TAGS": "service=orch,env=prod"})
    assert cfg.tags == {"service": "orch", "team": "ai", "env": "staging"}


def test_caller_tags_win_over_preferred_env_tags() -> None:
    explicit = ClientConfig(tags={"env": "staging"})
    cfg = resolve_config(explicit, env={"AGENTO11Y_TAGS": "service=orch,env=prod"})
    assert cfg.tags == {"service": "orch", "env": "staging"}


def test_env_token_fills_caller_bearer_mode() -> None:
    """AGENTO11Y_AUTH_TOKEN must fill caller-supplied bearer mode."""
    explicit = ClientConfig()
    explicit.generation_export.auth.mode = "bearer"
    cfg = resolve_config(
        explicit,
        env={"AGENTO11Y_AUTH_TOKEN": "envtok"},
    )
    assert cfg.generation_export.auth.mode == "bearer"
    assert cfg.generation_export.auth.bearer_token == "envtok"


def test_resolve_config_does_not_mutate_caller() -> None:
    """resolve_config must not mutate the caller's ClientConfig."""
    cfg_in = ClientConfig()
    assert cfg_in.generation_export.endpoint is None
    assert cfg_in.user_id is None

    _ = resolve_config(cfg_in, env={"AGENTO11Y_ENDPOINT": "first.example:4317", "AGENTO11Y_USER_ID": "alice"})

    # Original instance is untouched.
    assert cfg_in.generation_export.endpoint is None
    assert cfg_in.user_id is None

    # And subsequent resolves see fresh env, not state from the first call.
    out2 = resolve_config(cfg_in, env={"AGENTO11Y_ENDPOINT": "second.example:4317", "AGENTO11Y_USER_ID": "bob"})
    assert out2.generation_export.endpoint == "second.example:4317"
    assert out2.user_id == "bob"


def test_default_config_returns_concrete_values() -> None:
    """default_config() returns concrete schema defaults, not None sentinels."""
    cfg = default_config()
    assert cfg.generation_export.endpoint == "localhost:4317"
    assert cfg.generation_export.protocol == "grpc"
    assert cfg.generation_export.insecure is False
    assert cfg.generation_export.headers == {}
    assert cfg.generation_export.auth.mode == "none"
    assert cfg.user_id == ""


@pytest.mark.parametrize(
    "env,exc_match",
    [
        pytest.param(
            {"AGENTO11Y_AUTH_MODE": "basic", "AGENTO11Y_AUTH_TENANT_ID": "42"},
            "basic_password",
            id="basic mode requires password",
        ),
        pytest.param(
            {"AGENTO11Y_AUTH_MODE": "basic"},
            "basic_password",
            id="basic mode requires password (no tenant)",
        ),
    ],
)
def test_resolve_config_missing_required_field_raises(env: dict[str, str], exc_match: str) -> None:
    """Missing-required-field auth configs still raise (caller-fixable error)."""
    with pytest.raises(ValueError, match=exc_match):
        resolve_config(None, env=env)


def test_from_env_classmethod_matches_resolve() -> None:
    via_class = ClientConfig.from_env(env={"AGENTO11Y_AGENT_NAME": "planner", "AGENTO11Y_PROTOCOL": "none"})
    via_resolve = resolve_config(None, env={"AGENTO11Y_AGENT_NAME": "planner", "AGENTO11Y_PROTOCOL": "none"})
    assert via_class.agent_name == via_resolve.agent_name
    assert via_class.generation_export.protocol == via_resolve.generation_export.protocol


def test_from_env_classmethod_matches_resolve_with_preferred_keys() -> None:
    env = {"AGENTO11Y_AGENT_NAME": "planner", "AGENTO11Y_PROTOCOL": "none"}
    via_class = ClientConfig.from_env(env=env)
    via_resolve = resolve_config(None, env=env)
    assert via_class.agent_name == via_resolve.agent_name == "planner"
    assert via_class.generation_export.protocol == via_resolve.generation_export.protocol == "none"


def test_client_reads_env_automatically(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_PROTOCOL", "none")
    monkeypatch.setenv("AGENTO11Y_AGENT_NAME", "from-env")
    monkeypatch.setenv("AGENTO11Y_USER_ID", "alice")
    monkeypatch.setenv("AGENTO11Y_TAGS", "team=ai")

    client = Client()
    try:
        rec = client.start_generation(GenerationStart(model=ModelRef(provider="openai", name="gpt-5")))
        assert rec.seed.agent_name == "from-env"
        assert rec.seed.user_id == "alice"
        assert rec.seed.tags == {"team": "ai"}
    finally:
        client.shutdown()


def test_client_per_call_overrides_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_PROTOCOL", "none")
    monkeypatch.setenv("AGENTO11Y_AGENT_NAME", "planner")
    monkeypatch.setenv("AGENTO11Y_TAGS", "env=prod")

    client = Client()
    try:
        rec = client.start_generation(
            GenerationStart(
                model=ModelRef(provider="openai", name="gpt-5"),
                agent_name="reviewer",
                tags={"env": "staging", "task": "summarize"},
            ),
        )
        assert rec.seed.agent_name == "reviewer"
        assert rec.seed.tags == {"env": "staging", "task": "summarize"}
    finally:
        client.shutdown()
