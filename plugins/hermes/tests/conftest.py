"""Fake SDK clients avoid network calls; state resets prevent test-order leaks."""

from __future__ import annotations

import itertools
from collections.abc import Callable, Iterator
from typing import Any

import pytest
from opentelemetry import trace

from grafana_agento11y_hermes import _client, _hooks, _state, _tags


class FakeSpan:
    """Stand-in for the OTel span the SDK recorders expose.

    Hands out a valid ``SpanContext`` so the parenting path in
    ``on_post_tool_call`` behaves as it does against the real SDK, and records
    attribute writes.
    """

    _next_id = itertools.count(1)

    def __init__(self) -> None:
        ident = next(FakeSpan._next_id)
        self.attributes: dict[str, Any] = {}
        self._context = trace.SpanContext(
            trace_id=ident,
            span_id=ident,
            is_remote=False,
            trace_flags=trace.TraceFlags(trace.TraceFlags.SAMPLED),
        )

    def get_span_context(self) -> trace.SpanContext:
        return self._context

    def set_attribute(self, key: str, value: Any) -> None:
        self.attributes[key] = value


class SdkExploded(RuntimeError):
    """What an injected SDK failure raises. Distinct so a test can name it."""


class FakeRecorder:
    """Records lifecycle calls for assertions.

    ``raises`` names the methods that blow up instead of recording, which is how
    ``test_fail_open`` drives every SDK failure the plugin has to survive.
    """

    def __init__(self, raises: frozenset[str] = frozenset()) -> None:
        self.span = FakeSpan()
        self.entered = False
        self.exited = False
        self.raises = raises
        self.set_result_calls: list[dict[str, Any]] = []
        self.set_call_error_calls: list[Exception] = []
        self.set_exec_error_calls: list[Exception] = []
        # Method names in the order they were called, for tests that assert
        # ordering rather than just occurrence.
        self.calls: list[str] = []

    def _maybe_raise(self, name: str) -> None:
        if name in self.raises:
            raise SdkExploded(f"{name} exploded")

    def __enter__(self) -> FakeRecorder:
        self.entered = True
        self._maybe_raise("__enter__")
        return self

    def __exit__(self, exc_type, exc, tb) -> bool:
        self.exited = True
        self._maybe_raise("__exit__")
        return False

    def set_result(self, *args: Any, **kwargs: Any) -> None:
        self.calls.append("set_result")
        self.set_result_calls.append(dict(kwargs))
        self._maybe_raise("set_result")

    def set_call_error(self, error: Exception) -> None:
        self.calls.append("set_call_error")
        self.set_call_error_calls.append(error)
        self._maybe_raise("set_call_error")

    def set_exec_error(self, error: Exception) -> None:
        self.calls.append("set_exec_error")
        self.set_exec_error_calls.append(error)
        self._maybe_raise("set_exec_error")


class FakeClient:
    """In-memory stand-in for ``agento11y.Client``.

    ``raises`` names the client and recorder methods that fail. Recorder names
    are handed to every recorder this client hands out.
    """

    def __init__(self, *args: Any, raises: frozenset[str] = frozenset(), **kwargs: Any) -> None:
        self.start_generation_calls: list[Any] = []
        self.start_tool_execution_calls: list[Any] = []
        self.flush_calls = 0
        self.shutdown_calls = 0
        self.raises = raises
        self.init_args = args
        self.init_kwargs = kwargs
        self._next_gen_recorder: FakeRecorder | None = None
        self._next_tool_recorder: FakeRecorder | None = None

    def _maybe_raise(self, name: str) -> None:
        if name in self.raises:
            raise SdkExploded(f"{name} exploded")

    def start_generation(self, start: Any) -> FakeRecorder:
        self.start_generation_calls.append(start)
        self._maybe_raise("start_generation")
        rec = FakeRecorder(self.raises)
        self._next_gen_recorder = rec
        return rec

    def start_tool_execution(self, start: Any) -> FakeRecorder:
        self.start_tool_execution_calls.append(start)
        self._maybe_raise("start_tool_execution")
        rec = FakeRecorder(self.raises)
        self._next_tool_recorder = rec
        return rec

    def flush(self) -> None:
        self.flush_calls += 1
        self._maybe_raise("flush")

    def shutdown(self) -> None:
        self.shutdown_calls += 1


@pytest.fixture(autouse=True)
def reset_module_state() -> Iterator[None]:
    _client._reset_for_tests()
    _state.reset_for_tests()
    _hooks._reset_for_tests()
    _tags._reset_for_tests()
    yield
    _client._reset_for_tests()
    _state.reset_for_tests()
    _hooks._reset_for_tests()
    _tags._reset_for_tests()


@pytest.fixture
def env_creds(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTO11Y_ENDPOINT", "http://localhost/api/v1/generations:export")
    monkeypatch.setenv("AGENTO11Y_PROTOCOL", "http")
    monkeypatch.setenv("AGENTO11Y_AUTH_MODE", "basic")
    monkeypatch.setenv("AGENTO11Y_AUTH_TENANT_ID", "stack-1")
    monkeypatch.setenv("AGENTO11Y_AUTH_TOKEN", "glc_secret")
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost/otlp")


@pytest.fixture
def patch_client(monkeypatch: pytest.MonkeyPatch, env_creds: None) -> FakeClient:
    import agento11y

    instances: list[FakeClient] = []

    def factory(*args: Any, **kwargs: Any) -> FakeClient:
        instance = FakeClient(*args, **kwargs)
        instances.append(instance)
        return instance

    monkeypatch.setattr(agento11y, "Client", factory)
    # Skip the real OTel auto-setup — tests for that path are isolated.
    from grafana_agento11y_hermes import _otel

    monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)

    client = _client._get_client()
    assert isinstance(client, FakeClient), "fake client should have been constructed"
    return client


@pytest.fixture
def failing_client(monkeypatch: pytest.MonkeyPatch, env_creds: None) -> Callable[..., FakeClient]:
    """Build the singleton with named client and recorder methods raising ``SdkExploded``."""
    import agento11y

    from grafana_agento11y_hermes import _otel

    def build(*names: str) -> FakeClient:
        raises = frozenset(names)

        def factory(*args: Any, **kwargs: Any) -> FakeClient:
            return FakeClient(*args, raises=raises, **kwargs)

        monkeypatch.setattr(agento11y, "Client", factory)
        monkeypatch.setattr(_otel, "setup_if_needed", lambda cfg: True)
        client = _client._get_client()
        assert isinstance(client, FakeClient), "fake client should have been constructed"
        return client

    return build


class FakeContext:
    """Stand-in for the hermes plugin context object passed to ``register``."""

    def __init__(self) -> None:
        self.hooks: dict[str, Any] = {}

    def register_hook(self, name: str, handler: Any) -> None:
        self.hooks[name] = handler


@pytest.fixture
def ctx() -> FakeContext:
    return FakeContext()
