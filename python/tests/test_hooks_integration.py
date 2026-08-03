"""Guard behavior of the Python SDK against a real local HTTP server.

The other hook tests assert on fields of the captured body they already expect,
so an encoding the server cannot read looks identical to a correct one. These
cases run the full transport and compare each captured body with
`conformance/hooks/request-preflight.json`.
"""

from __future__ import annotations

import base64
import concurrent.futures
import logging
import time
from collections.abc import Iterator
from datetime import timedelta

import pytest
from agento11y import (
    ApiConfig,
    AuthConfig,
    Client,
    ClientConfig,
    GenerationExportConfig,
    HooksConfig,
    HookTransportError,
    hook_denied_from_response,
)
from hooks_fixtures import diff_json, load_preflight_request, load_responses, preflight_request
from hooks_server import HOOKS_EVALUATE_PATH, HookServer
from opentelemetry import trace

RESPONSES = load_responses()


@pytest.fixture
def hook_server() -> Iterator[HookServer]:
    server = HookServer()
    try:
        yield server
    finally:
        server.close()


@pytest.mark.parametrize("fail_open", [True, False], ids=["fail-open", "fail-closed"])
def test_allow_proceeds_under_both_policies(hook_server: HookServer, fail_open: bool) -> None:
    hook_server.response = RESPONSES["allow"]
    client = _client(hook_server.url, fail_open=fail_open)
    try:
        response = client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()

    assert response.action == "allow"
    assert hook_denied_from_response(response) is None
    assert response.evaluations[0].rule_id == "pii-detect"
    _assert_preflight_request(hook_server)


@pytest.mark.parametrize("fail_open", [True, False], ids=["fail-open", "fail-closed"])
def test_deny_is_enforced_under_both_policies(hook_server: HookServer, fail_open: bool) -> None:
    hook_server.response = RESPONSES["deny"]
    client = _client(hook_server.url, fail_open=fail_open)
    try:
        response = client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()

    assert response.is_deny
    denied = hook_denied_from_response(response)
    assert denied is not None
    assert denied.rule_id == "block-destructive-bash"
    assert denied.reason == "Bash(*rm*) is not allowed in this environment"
    _assert_preflight_request(hook_server)


@pytest.mark.parametrize("status", [429, 503], ids=["non-500", "5xx"])
def test_http_error_fails_open_with_a_warning(
    hook_server: HookServer, status: int, caplog: pytest.LogCaptureFixture
) -> None:
    hook_server.status = status
    hook_server.raw_body = "upstream unavailable"
    client = _client(hook_server.url, fail_open=True)
    try:
        with caplog.at_level(logging.WARNING, logger="agento11y"):
            response = client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()

    assert response.action == "allow"
    assert _fail_open_warnings(caplog), "a swallowed transport failure must be logged"
    assert str(status) in "\n".join(_fail_open_warnings(caplog))
    _assert_preflight_request(hook_server)


@pytest.mark.parametrize("status", [429, 503], ids=["non-500", "5xx"])
def test_http_error_fails_closed(hook_server: HookServer, status: int) -> None:
    hook_server.status = status
    hook_server.raw_body = "upstream unavailable"
    client = _client(hook_server.url, fail_open=False)
    try:
        with pytest.raises(HookTransportError):
            client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()

    _assert_preflight_request(hook_server)


def test_malformed_body_fails_open_with_a_warning(hook_server: HookServer, caplog: pytest.LogCaptureFixture) -> None:
    hook_server.raw_body = '{"action": "allow"'
    client = _client(hook_server.url, fail_open=True)
    try:
        with caplog.at_level(logging.WARNING, logger="agento11y"):
            response = client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()

    assert response.action == "allow"
    assert _fail_open_warnings(caplog), "a malformed response must be logged when swallowed"
    _assert_preflight_request(hook_server)


def test_malformed_body_fails_closed(hook_server: HookServer) -> None:
    hook_server.raw_body = '{"action": "allow"'
    client = _client(hook_server.url, fail_open=False)
    try:
        with pytest.raises(HookTransportError):
            client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()

    _assert_preflight_request(hook_server)


def test_slow_response_fails_open_before_the_server_answers(
    hook_server: HookServer, caplog: pytest.LogCaptureFixture
) -> None:
    hook_server.delay = 2.0
    client = _client(hook_server.url, fail_open=True, timeout_seconds=0.25)
    started = time.monotonic()
    try:
        with caplog.at_level(logging.WARNING, logger="agento11y"):
            response = client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()
    elapsed = time.monotonic() - started

    assert response.action == "allow"
    assert elapsed < hook_server.delay, f"client waited {elapsed:.2f}s, so it did not enforce its own timeout"
    assert _fail_open_warnings(caplog), "a timed-out evaluation must be logged when swallowed"
    # The client is gone by the time the delayed write runs, so the write error is
    # the case under test rather than a broken server.
    _assert_preflight_request(hook_server, expect_response_written=False)


def test_slow_response_fails_closed_before_the_server_answers(hook_server: HookServer) -> None:
    hook_server.delay = 2.0
    client = _client(hook_server.url, fail_open=False, timeout_seconds=0.25)
    started = time.monotonic()
    try:
        with pytest.raises(HookTransportError):
            client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()
    elapsed = time.monotonic() - started

    assert elapsed < hook_server.delay, f"client waited {elapsed:.2f}s, so it did not enforce its own timeout"
    _assert_preflight_request(hook_server, expect_response_written=False)


@pytest.mark.parametrize("fail_open", [True, False], ids=["fail-open", "fail-closed"])
def test_unconfigured_phase_sends_no_request(hook_server: HookServer, fail_open: bool) -> None:
    hook_server.status = 500
    client = _client(hook_server.url, fail_open=fail_open, phases=["postflight"])
    try:
        response = client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()

    assert response.action == "allow"
    assert hook_server.request_count == 0


@pytest.mark.parametrize("fail_open", [True, False], ids=["fail-open", "fail-closed"])
def test_disabled_hooks_send_no_request(hook_server: HookServer, fail_open: bool) -> None:
    hook_server.status = 500
    client = _client(hook_server.url, fail_open=fail_open, enabled=False)
    try:
        response = client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()

    assert response.action == "allow"
    assert hook_server.request_count == 0


def test_configured_auth_reaches_the_server(hook_server: HookServer) -> None:
    client = _client(
        hook_server.url,
        auth=AuthConfig(mode="basic", basic_user="12345", basic_password="glc-token", tenant_id="12345"),
    )
    try:
        client.evaluate_hook(preflight_request())
    finally:
        client.shutdown()

    headers = hook_server.requests[0]["headers"]
    expected = base64.b64encode(b"12345:glc-token").decode()
    assert headers["authorization"] == f"Basic {expected}"
    assert headers["x-scope-orgid"] == "12345"
    assert headers["x-agento11y-hook-timeout-ms"] == "5000"
    _assert_preflight_request(hook_server)


def test_concurrent_evaluations_are_not_serialized(hook_server: HookServer) -> None:
    """A guard on the request path must not funnel every caller through one connection."""

    hook_server.delay = 0.2
    client = _client(hook_server.url, timeout_seconds=5.0)
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            results = list(pool.map(lambda _: client.evaluate_hook(preflight_request()), range(4)))
    finally:
        client.shutdown()

    assert [r.action for r in results] == ["allow"] * 4
    assert hook_server.max_in_flight > 1
    assert hook_server.errors == []
    for payload in hook_server.payloads:
        assert diff_json(payload, load_preflight_request()) == []


def _fail_open_warnings(caplog: pytest.LogCaptureFixture) -> list[str]:
    return [
        record.getMessage()
        for record in caplog.records
        if record.levelno >= logging.WARNING and "allowing request (fail_open)" in record.getMessage()
    ]


def _assert_preflight_request(server: HookServer, *, expect_response_written: bool = True) -> None:
    assert server.request_count == 1, f"expected exactly one hook request, got {server.request_count}"
    if expect_response_written:
        # A server that failed to answer makes the client fail open, and a
        # fail-open assertion passes whether or not the response was ever sent.
        assert server.errors == [], "the test server could not answer: " + "; ".join(server.errors)
    entry = server.requests[0]
    assert entry["path"] == HOOKS_EVALUATE_PATH
    assert entry["headers"]["content-type"] == "application/json"
    diffs = diff_json(entry["payload"], load_preflight_request())
    assert diffs == [], "captured request body does not match the shared fixture:\n" + "\n".join(diffs)


def _client(
    api_endpoint: str,
    *,
    enabled: bool = True,
    fail_open: bool = True,
    phases: list[str] | None = None,
    timeout_seconds: float = 5.0,
    auth: AuthConfig | None = None,
) -> Client:
    hooks = HooksConfig(
        enabled=enabled,
        phases=phases if phases is not None else ["preflight"],
        timeout_seconds=timeout_seconds,
        fail_open=fail_open,
    )
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                protocol="http",
                endpoint=f"{api_endpoint}/api/v1/generations:export",
                auth=auth if auth is not None else AuthConfig(),
                insecure=True,
                batch_size=1,
                flush_interval=timedelta(seconds=60),
                max_retries=1,
                initial_backoff=timedelta(milliseconds=1),
                max_backoff=timedelta(milliseconds=10),
            ),
            api=ApiConfig(endpoint=api_endpoint),
            hooks=hooks,
            tracer=trace.get_tracer("agento11y-sdk-python-hooks-integration"),
        )
    )
