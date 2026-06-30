"""Tests for canonical SIGIL_* env-var resolution in the Python SDK."""

from __future__ import annotations

from collections.abc import Callable

import pytest
from sigil_sdk import ApiConfig, Client, ClientConfig
from sigil_sdk.config import default_config, resolve_config
from sigil_sdk.models import ContentCaptureMode, GenerationStart, ModelRef


def _check_no_env(cfg: ClientConfig) -> None:
    assert cfg.generation_export.endpoint == "localhost:4317"
    assert cfg.generation_export.protocol == "grpc"
    assert cfg.generation_export.insecure is False
    assert cfg.generation_export.auth.mode == "none"
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


@pytest.mark.parametrize(
    "env,check",
    [
        pytest.param({}, _check_no_env, id="no env uses defaults"),
        pytest.param(
            {
                "SIGIL_ENDPOINT": "https://env:4318",
                "SIGIL_PROTOCOL": "http",
                "SIGIL_INSECURE": "true",
                "SIGIL_HEADERS": "X-A=1,X-B=two",
                "SIGIL_AUTH_MODE": "basic",
                "SIGIL_AUTH_TENANT_ID": "42",
                "SIGIL_AUTH_TOKEN": "glc_xxx",
            },
            _check_transport,
            id="transport from env",
        ),
        pytest.param(
            {"SIGIL_AUTH_MODE": "bearer", "SIGIL_AUTH_TOKEN": "tok"},
            _check_bearer,
            id="bearer auth from env",
        ),
        pytest.param(
            {
                "SIGIL_AGENT_NAME": "planner",
                "SIGIL_AGENT_VERSION": "1.2.3",
                "SIGIL_USER_ID": "alice@example.com",
                "SIGIL_TAGS": "service=orchestrator,env=prod",
                "SIGIL_DEBUG": "true",
            },
            _check_agent_user_tags,
            id="agent user tags debug from env",
        ),
        pytest.param(
            {"SIGIL_CONTENT_CAPTURE_MODE": "metadata_only"},
            _check_content_capture_metadata,
            id="content capture mode from env",
        ),
        pytest.param(
            {"SIGIL_CONTENT_CAPTURE_MODE": "full_with_metadata_spans"},
            _check_content_capture_full_with_metadata_spans,
            id="full_with_metadata_spans content capture mode from env",
        ),
        pytest.param(
            {"SIGIL_USE_EXPERIMENTAL_OTEL": "true"},
            _check_experimental_otel,
            id="experimental otel opt-in from env",
        ),
        pytest.param(
            {
                "SIGIL_AUTH_MODE": "Bearrer",
                "SIGIL_ENDPOINT": "valid.example:4318",
                "SIGIL_AGENT_NAME": "valid-agent",
            },
            _check_invalid_auth_mode_preserves_valid,
            id="invalid auth mode preserves other valid env",
        ),
        pytest.param(
            {"SIGIL_AUTH_TENANT_ID": "42"},
            _check_stray_tenant_does_not_error,
            id="stray SIGIL_AUTH_TENANT_ID does not error",
        ),
    ],
)
def test_resolve_config_env(env: dict[str, str], check: Callable[[ClientConfig], None]) -> None:
    cfg = resolve_config(None, env=env)
    check(cfg)


def test_explicit_overrides_env() -> None:
    explicit = ClientConfig()
    explicit.generation_export.endpoint = "https://explicit:4318"
    cfg = resolve_config(
        explicit,
        env={"SIGIL_ENDPOINT": "https://env:4318", "SIGIL_AGENT_NAME": "planner"},
    )
    assert cfg.generation_export.endpoint == "https://explicit:4318"
    assert cfg.agent_name == "planner"


def test_sigil_endpoint_also_defaults_api_endpoint() -> None:
    cfg = resolve_config(None, env={"SIGIL_ENDPOINT": "https://sigil.example"})
    assert cfg.generation_export.endpoint == "https://sigil.example"
    assert cfg.api.endpoint == "https://sigil.example"


def test_explicit_api_endpoint_overrides_sigil_endpoint() -> None:
    explicit = ClientConfig(api=ApiConfig(endpoint="https://api.example"))
    cfg = resolve_config(explicit, env={"SIGIL_ENDPOINT": "https://ingest.example"})
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
            "SIGIL_AUTH_MODE": "basic",
            "SIGIL_AUTH_TENANT_ID": "42",
            "SIGIL_AUTH_TOKEN": "envpass",
        },
    )
    assert cfg.generation_export.auth.mode == "bearer"
    assert cfg.generation_export.auth.bearer_token == "callertok"
    # Authorization header carries the caller's bearer token, not env's password.
    assert cfg.generation_export.headers["Authorization"] == "Bearer callertok"


def test_caller_tags_merge_with_env_tags() -> None:
    """Env tags layer under caller tags; caller wins on key collision."""
    explicit = ClientConfig(tags={"team": "ai", "env": "staging"})
    cfg = resolve_config(explicit, env={"SIGIL_TAGS": "service=orch,env=prod"})
    assert cfg.tags == {"service": "orch", "team": "ai", "env": "staging"}


def test_env_token_fills_caller_bearer_mode() -> None:
    """SIGIL_AUTH_TOKEN must fill caller-supplied bearer mode."""
    explicit = ClientConfig()
    explicit.generation_export.auth.mode = "bearer"
    cfg = resolve_config(
        explicit,
        env={"SIGIL_AUTH_TOKEN": "envtok"},
    )
    assert cfg.generation_export.auth.mode == "bearer"
    assert cfg.generation_export.auth.bearer_token == "envtok"


def test_resolve_config_does_not_mutate_caller() -> None:
    """resolve_config must not mutate the caller's ClientConfig."""
    cfg_in = ClientConfig()
    assert cfg_in.generation_export.endpoint is None
    assert cfg_in.user_id is None

    _ = resolve_config(cfg_in, env={"SIGIL_ENDPOINT": "first.example:4317", "SIGIL_USER_ID": "alice"})

    # Original instance is untouched.
    assert cfg_in.generation_export.endpoint is None
    assert cfg_in.user_id is None

    # And subsequent resolves see fresh env, not state from the first call.
    out2 = resolve_config(cfg_in, env={"SIGIL_ENDPOINT": "second.example:4317", "SIGIL_USER_ID": "bob"})
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
            {"SIGIL_AUTH_MODE": "basic", "SIGIL_AUTH_TENANT_ID": "42"},
            "basic_password",
            id="basic mode requires password",
        ),
        pytest.param(
            {"SIGIL_AUTH_MODE": "basic"},
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
    via_class = ClientConfig.from_env(env={"SIGIL_AGENT_NAME": "planner", "SIGIL_PROTOCOL": "none"})
    via_resolve = resolve_config(None, env={"SIGIL_AGENT_NAME": "planner", "SIGIL_PROTOCOL": "none"})
    assert via_class.agent_name == via_resolve.agent_name
    assert via_class.generation_export.protocol == via_resolve.generation_export.protocol


def test_client_reads_env_automatically(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SIGIL_PROTOCOL", "none")
    monkeypatch.setenv("SIGIL_AGENT_NAME", "from-env")
    monkeypatch.setenv("SIGIL_USER_ID", "alice")
    monkeypatch.setenv("SIGIL_TAGS", "team=ai")

    client = Client()
    try:
        rec = client.start_generation(GenerationStart(model=ModelRef(provider="openai", name="gpt-5")))
        assert rec.seed.agent_name == "from-env"
        assert rec.seed.user_id == "alice"
        assert rec.seed.tags == {"team": "ai"}
    finally:
        client.shutdown()


def test_client_per_call_overrides_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SIGIL_PROTOCOL", "none")
    monkeypatch.setenv("SIGIL_AGENT_NAME", "planner")
    monkeypatch.setenv("SIGIL_TAGS", "env=prod")

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
