from __future__ import annotations

import builtins
import inspect
import logging
import subprocess
import sys
import textwrap
from unittest.mock import Mock

import pytest
from agento11y import (
    Agento11yError,
    Client,
    ClientConfig,
    ExperimentalFeatureDisabledError,
    ExportFlushError,
    FlushNotVerifiableError,
    GenerationExportConfig,
    OTelDependencyMissingError,
)
from agento11y.experimental import (
    ENV_ENABLE_EXPERIMENTAL_FEATURES,
    FEATURE_OTEL_GENERATION_EXPORT,
    ExperimentalFeature,
)
from opentelemetry.metrics import NoOpMeterProvider
from opentelemetry.trace import NoOpTracerProvider


def _block_genai_imports(monkeypatch: pytest.MonkeyPatch) -> list[str]:
    attempted: list[str] = []
    original_import = builtins.__import__

    def blocked_import(name, *args, **kwargs):
        if name == "opentelemetry.util.genai" or name.startswith("opentelemetry.util.genai."):
            attempted.append(name)
            raise ModuleNotFoundError(f"No module named {name!r}", name=name)
        return original_import(name, *args, **kwargs)

    monkeypatch.setattr(builtins, "__import__", blocked_import)
    return attempted


@pytest.mark.parametrize("protocol", ["none", "http", "grpc"])
def test_native_client_does_not_import_genai(protocol: str) -> None:
    script = textwrap.dedent(
        """
        import builtins
        import sys

        original_import = builtins.__import__
        attempted = []

        def blocked_import(name, *args, **kwargs):
            if name == "opentelemetry.util.genai" or name.startswith("opentelemetry.util.genai."):
                attempted.append(name)
                raise ModuleNotFoundError(name, name=name)
            return original_import(name, *args, **kwargs)

        builtins.__import__ = blocked_import
        from agento11y import Client, ClientConfig, GenerationExportConfig

        client = Client(ClientConfig(generation_export=GenerationExportConfig(protocol=sys.argv[1])))
        client.shutdown()
        assert not attempted, attempted
        assert not any(name.startswith("opentelemetry.util.genai") for name in sys.modules)
        """
    )
    result = subprocess.run(
        [sys.executable, "-c", script, protocol], capture_output=True, text=True, timeout=30, check=False
    )
    assert result.returncode == 0, result.stdout + result.stderr


@pytest.mark.parametrize("from_env", [False, True], ids=["explicit", "environment"])
def test_otel_requires_experimental_opt_in(monkeypatch: pytest.MonkeyPatch, from_env: bool) -> None:
    monkeypatch.delenv(ENV_ENABLE_EXPERIMENTAL_FEATURES, raising=False)
    monkeypatch.setenv("AGENTO11Y_USE_EXPERIMENTAL_OTEL", "true")
    attempted = _block_genai_imports(monkeypatch)
    if from_env:
        monkeypatch.setenv("AGENTO11Y_PROTOCOL", "otel")
        config = None
    else:
        config = ClientConfig(generation_export=GenerationExportConfig(protocol="otel"))

    with pytest.raises(ExperimentalFeatureDisabledError) as caught:
        Client(config)

    assert caught.value.feature == FEATURE_OTEL_GENERATION_EXPORT
    assert caught.value.env_var == ENV_ENABLE_EXPERIMENTAL_FEATURES
    assert attempted == []


def test_otel_missing_dependency_names_extra(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(ENV_ENABLE_EXPERIMENTAL_FEATURES, "true")
    attempted = _block_genai_imports(monkeypatch)

    with pytest.raises(OTelDependencyMissingError, match=r"agento11y\[otel\]") as caught:
        Client(ClientConfig(generation_export=GenerationExportConfig(protocol="otel")))

    assert attempted
    assert isinstance(caught.value.__cause__, ModuleNotFoundError)


def test_otel_rejects_custom_generation_exporter(monkeypatch: pytest.MonkeyPatch) -> None:
    attempted = _block_genai_imports(monkeypatch)
    exporter = Mock()

    with pytest.raises(ValueError, match="conflicts with generation_exporter"):
        Client(
            ClientConfig(
                generation_export=GenerationExportConfig(protocol="otel"),
                generation_exporter=exporter,
            )
        )

    assert attempted == []
    assert exporter.mock_calls == []


@pytest.mark.parametrize("direct", [False, True], ids=["providers", "direct-instruments"])
def test_native_instruments_take_precedence_over_providers(monkeypatch: pytest.MonkeyPatch, direct: bool) -> None:
    tracer_provider = Mock(spec=NoOpTracerProvider)
    meter_provider = Mock(spec=NoOpMeterProvider)
    tracer_provider.get_tracer.return_value = NoOpTracerProvider().get_tracer("provider")
    meter_provider.get_meter.return_value = NoOpMeterProvider().get_meter("provider")
    tracer = NoOpTracerProvider().get_tracer("direct") if direct else None
    meter = NoOpMeterProvider().get_meter("direct") if direct else None
    monkeypatch.setattr("agento11y.client.trace.get_tracer_provider", Mock(side_effect=AssertionError("global tracer")))
    monkeypatch.setattr("agento11y.client.metrics.get_meter_provider", Mock(side_effect=AssertionError("global meter")))

    client = Client(
        ClientConfig(
            generation_export=GenerationExportConfig(protocol="none"),
            tracer_provider=tracer_provider,
            meter_provider=meter_provider,
            tracer=tracer,
            meter=meter,
        )
    )
    try:
        if direct:
            assert client._tracer is tracer
            assert client._meter is meter
            tracer_provider.get_tracer.assert_not_called()
            meter_provider.get_meter.assert_not_called()
        else:
            tracer_provider.get_tracer.assert_called_once()
            meter_provider.get_meter.assert_called_once()
            assert client._tracer is tracer_provider.get_tracer.return_value
            assert client._meter is meter_provider.get_meter.return_value
    finally:
        client.shutdown()


def test_otel_handler_uses_providers_not_direct_instruments() -> None:
    pytest.importorskip("opentelemetry.util.genai.handler")
    from agento11y.otel_export import build_otel_handler

    tracer_provider = Mock(wraps=NoOpTracerProvider())
    meter_provider = Mock(wraps=NoOpMeterProvider())
    tracer = Mock()
    meter = Mock()
    build_otel_handler(
        ClientConfig(
            generation_export=GenerationExportConfig(protocol="otel"),
            tracer_provider=tracer_provider,
            meter_provider=meter_provider,
            tracer=tracer,
            meter=meter,
        )
    )

    tracer_provider.get_tracer.assert_called_once()
    meter_provider.get_meter.assert_called_once()
    assert tracer.mock_calls == []
    assert meter.mock_calls == []


@pytest.mark.parametrize("arity", [10, 12, 21])
def test_native_config_preserves_positional_arguments(arity: int) -> None:
    names = (
        "generation_export",
        "api",
        "embedding_capture",
        "hooks",
        "content_capture",
        "content_capture_resolver",
        "generation_sanitizer",
        "tracer",
        "meter",
        "logger",
        "now",
        "sleep",
        "generation_exporter",
        "use_experimental_otel",
        "agent_name",
        "agent_version",
        "user_id",
        "tags",
        "ingest_actor",
        "debug",
        "generation_export_endpoint",
    )
    parameters = inspect.signature(ClientConfig).parameters
    assert tuple(name for name, param in parameters.items() if param.kind != param.KEYWORD_ONLY) == names
    defaults = ClientConfig(generation_export=GenerationExportConfig(protocol="none"), logger=logging.getLogger("test"))
    values = [getattr(defaults, name) for name in names[:arity]]
    config = ClientConfig(*values)
    assert config.logger is defaults.logger
    client = Client(config)
    try:
        assert client._config.logger is defaults.logger
        assert client._config.tracer_provider is None
        assert client._config.meter_provider is None
    finally:
        client.shutdown()


def test_otel_feature_name() -> None:
    assert ExperimentalFeature.OTEL_GENERATION_EXPORT.value == FEATURE_OTEL_GENERATION_EXPORT


@pytest.mark.parametrize("error_type", [OTelDependencyMissingError, FlushNotVerifiableError, ExportFlushError])
def test_otel_errors_are_sdk_errors(error_type: type[Exception]) -> None:
    assert isinstance(error_type("export failed"), Agento11yError)
