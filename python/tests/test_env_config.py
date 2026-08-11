"""Tests for canonical AGENTO11Y_* environment resolution in the Python SDK."""

from __future__ import annotations

import logging
from collections.abc import Callable

import pytest
from agento11y import ApiConfig, Client, ClientConfig, HooksConfig
from agento11y.config import default_config, resolve_config
from agento11y.models import ContentCaptureMode, GenerationStart, ModelRef


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


def _check_hooks_defaults(cfg: ClientConfig) -> None:
    assert cfg.hooks.enabled is False
    assert cfg.hooks.phases == ["preflight"]
    assert cfg.hooks.timeout_seconds == 15.0
    assert cfg.hooks.fail_open is True


def _check_hooks_all_from_env(cfg: ClientConfig) -> None:
    assert cfg.hooks.enabled is True
    assert cfg.hooks.phases == ["preflight", "postflight"]
    assert cfg.hooks.timeout_seconds == 3.0
    assert cfg.hooks.fail_open is False


def _check_hooks_enabled_only(cfg: ClientConfig) -> None:
    assert cfg.hooks.enabled is True
    assert cfg.hooks.phases == ["preflight"]
    assert cfg.hooks.timeout_seconds == 15.0
    assert cfg.hooks.fail_open is True


def _check_hooks_phases_only(cfg: ClientConfig) -> None:
    assert cfg.hooks.enabled is False
    assert cfg.hooks.phases == ["postflight"]


def _check_hooks_timeout_only(cfg: ClientConfig) -> None:
    assert cfg.hooks.timeout_seconds == 2.5
    assert cfg.hooks.enabled is False


def _check_hooks_fail_open_only(cfg: ClientConfig) -> None:
    assert cfg.hooks.fail_open is False
    assert cfg.hooks.enabled is False


def _check_hooks_phase_normalization(cfg: ClientConfig) -> None:
    assert cfg.hooks.phases == ["postflight", "preflight"]


def _check_hooks_invalid_bools_keep_defaults(cfg: ClientConfig) -> None:
    # A typo must not read as false: fail_open stays at its true default.
    assert cfg.hooks.enabled is False
    assert cfg.hooks.fail_open is True


def _check_hooks_invalid_phase_keeps_default(cfg: ClientConfig) -> None:
    assert cfg.hooks.phases == ["preflight"]


def _check_hooks_unknown_phase_keeps_postflight(cfg: ClientConfig) -> None:
    assert cfg.hooks.phases == ["postflight"]


def _check_hooks_max_timeout(cfg: ClientConfig) -> None:
    assert cfg.hooks.timeout_seconds == 119.999


def _check_hooks_invalid_timeout_keeps_default(cfg: ClientConfig) -> None:
    assert cfg.hooks.timeout_seconds == 15.0


def _check_hooks_invalid_sibling_keeps_valid(cfg: ClientConfig) -> None:
    assert cfg.hooks.enabled is True
    assert cfg.hooks.phases == ["preflight", "postflight"]
    assert cfg.hooks.timeout_seconds == 15.0
    assert cfg.hooks.fail_open is True


def _check_legacy_hooks_env_ignored(cfg: ClientConfig) -> None:
    assert cfg.hooks.enabled is False
    assert cfg.hooks.phases == ["preflight"]
    assert cfg.hooks.timeout_seconds == 15.0
    assert cfg.hooks.fail_open is True


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
        pytest.param({}, _check_hooks_defaults, id="hooks default to off with concrete values"),
        pytest.param(
            {
                "AGENTO11Y_HOOKS_ENABLED": "true",
                "AGENTO11Y_HOOKS_PHASES": "preflight,postflight",
                "AGENTO11Y_HOOKS_TIMEOUT_MS": "3000",
                "AGENTO11Y_HOOKS_FAIL_OPEN": "false",
            },
            _check_hooks_all_from_env,
            id="all four hooks variables from env",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_ENABLED": "true"},
            _check_hooks_enabled_only,
            id="hooks enabled alone keeps other defaults",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_PHASES": "postflight"},
            _check_hooks_phases_only,
            id="hooks phases alone",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "2500"},
            _check_hooks_timeout_only,
            id="hooks timeout milliseconds convert to seconds",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_FAIL_OPEN": "off"},
            _check_hooks_fail_open_only,
            id="hooks fail open alone",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_PHASES": " POSTFLIGHT , ,preflight, postflight "},
            _check_hooks_phase_normalization,
            id="hooks phases trim lowercase dedupe and keep order",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_ENABLED": "maybe", "AGENTO11Y_HOOKS_FAIL_OPEN": "ture"},
            _check_hooks_invalid_bools_keep_defaults,
            id="invalid hooks booleans keep defaults",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_PHASES": "preflight,bogus"},
            _check_hooks_invalid_phase_keeps_default,
            id="unknown hook phase is dropped and the rest applies",
        ),
        pytest.param(
            # Rejecting the whole list would fall back to the ["preflight"]
            # default, starting enforcement on a phase the operator did not ask
            # for and skipping the one they did.
            {"AGENTO11Y_HOOKS_PHASES": "postflight,bogus"},
            _check_hooks_unknown_phase_keeps_postflight,
            id="a typo beside postflight does not switch the phase to preflight",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_PHASES": "bogus"},
            _check_hooks_invalid_phase_keeps_default,
            id="a phase list with no usable entry keeps the default",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "0"},
            _check_hooks_invalid_timeout_keeps_default,
            id="zero hook timeout is rejected",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "-1"},
            _check_hooks_invalid_timeout_keeps_default,
            id="negative hook timeout is rejected",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "1.5"},
            _check_hooks_invalid_timeout_keeps_default,
            id="non-integer hook timeout is rejected",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "not-a-number"},
            _check_hooks_invalid_timeout_keeps_default,
            id="unparsable hook timeout is rejected",
        ),
        pytest.param(
            # int() reads PEP 515 underscores; Go and JS do not, so the SDKs
            # would otherwise disagree on what this value means.
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "3_000"},
            _check_hooks_invalid_timeout_keeps_default,
            id="underscore digit grouping in the hook timeout is rejected",
        ),
        pytest.param(
            # int() also reads non-ASCII digits. These are Arabic-Indic 3000.
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "\u0663\u0660\u0660\u0660"},
            _check_hooks_invalid_timeout_keeps_default,
            id="non-ascii digits in the hook timeout are rejected",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "120000"},
            _check_hooks_invalid_timeout_keeps_default,
            id="a hook timeout above the server ceiling is rejected",
        ),
        pytest.param(
            # Unbounded, this reads as 1e17 seconds: no effective timeout, so a
            # hung evaluator would block the agent.
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "99999999999999999999"},
            _check_hooks_invalid_timeout_keeps_default,
            id="an absurd hook timeout is rejected",
        ),
        pytest.param(
            {"AGENTO11Y_HOOKS_TIMEOUT_MS": "119999"},
            _check_hooks_max_timeout,
            id="the largest honoured hook timeout is accepted",
        ),
        pytest.param(
            {
                "AGENTO11Y_HOOKS_ENABLED": "true",
                "AGENTO11Y_HOOKS_PHASES": "preflight,postflight",
                "AGENTO11Y_HOOKS_TIMEOUT_MS": "nope",
            },
            _check_hooks_invalid_sibling_keeps_valid,
            id="invalid hook timeout preserves the other hooks variables",
        ),
        pytest.param(
            {
                "SIGIL_HOOKS_ENABLED": "true",
                "SIGIL_HOOKS_PHASES": "postflight",
                "SIGIL_HOOKS_TIMEOUT_MS": "3000",
                "SIGIL_HOOKS_FAIL_OPEN": "false",
            },
            _check_legacy_hooks_env_ignored,
            id="SIGIL_HOOKS_ vars are ignored",
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
        env={"AGENTO11Y_ENDPOINT": "https://env:4318", "AGENTO11Y_AGENT_NAME": "planner"},
    )
    assert cfg.generation_export.endpoint == "https://explicit:4318"
    assert cfg.agent_name == "planner"


@pytest.mark.parametrize(
    "env,key",
    [
        pytest.param({"AGENTO11Y_HOOKS_ENABLED": "maybe"}, "AGENTO11Y_HOOKS_ENABLED", id="enabled"),
        pytest.param({"AGENTO11Y_HOOKS_PHASES": "preflight,bogus"}, "AGENTO11Y_HOOKS_PHASES", id="phases"),
        pytest.param({"AGENTO11Y_HOOKS_PHASES": ","}, "AGENTO11Y_HOOKS_PHASES", id="phases with no entry"),
        pytest.param({"AGENTO11Y_HOOKS_TIMEOUT_MS": "0"}, "AGENTO11Y_HOOKS_TIMEOUT_MS", id="timeout"),
        pytest.param({"AGENTO11Y_HOOKS_FAIL_OPEN": "ture"}, "AGENTO11Y_HOOKS_FAIL_OPEN", id="fail open"),
    ],
)
def test_invalid_hooks_value_warning_names_selected_key(
    env: dict[str, str], key: str, caplog: pytest.LogCaptureFixture
) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        resolve_config(None, env=env)
    assert any(key in record.getMessage() for record in caplog.records)


def test_caller_hooks_config_wins_over_env() -> None:
    explicit = ClientConfig(
        hooks=HooksConfig(enabled=False, phases=["postflight"], timeout_seconds=2.5, fail_open=False)
    )
    cfg = resolve_config(
        explicit,
        env={
            "AGENTO11Y_HOOKS_ENABLED": "true",
            "AGENTO11Y_HOOKS_PHASES": "preflight,postflight",
            "AGENTO11Y_HOOKS_TIMEOUT_MS": "9000",
            "AGENTO11Y_HOOKS_FAIL_OPEN": "true",
        },
    )
    assert cfg.hooks.enabled is False
    assert cfg.hooks.phases == ["postflight"]
    assert cfg.hooks.timeout_seconds == 2.5
    assert cfg.hooks.fail_open is False


def test_env_fills_hooks_fields_the_caller_left_unset() -> None:
    explicit = ClientConfig(hooks=HooksConfig(timeout_seconds=2.5))
    cfg = resolve_config(
        explicit,
        env={"AGENTO11Y_HOOKS_ENABLED": "true", "AGENTO11Y_HOOKS_TIMEOUT_MS": "9000"},
    )
    assert cfg.hooks.enabled is True
    assert cfg.hooks.timeout_seconds == 2.5


def test_invalid_capture_mode_warning_names_selected_key(caplog: pytest.LogCaptureFixture) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        cfg = resolve_config(None, env={"AGENTO11Y_CONTENT_CAPTURE_MODE": "bogus"})
    assert cfg.content_capture == ContentCaptureMode.DEFAULT
    assert any("AGENTO11Y_CONTENT_CAPTURE_MODE" in r.getMessage() for r in caplog.records)


def test_legacy_env_namespace_is_ignored_with_migration_warning(caplog: pytest.LogCaptureFixture) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        cfg = resolve_config(None, env={"SIGIL_ENDPOINT": "https://legacy.example"})
    assert cfg.generation_export.endpoint == "localhost:4317"
    assert any(
        "SIGIL_ENDPOINT is ignored; rename it to AGENTO11Y_ENDPOINT" in record.getMessage() for record in caplog.records
    )


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
