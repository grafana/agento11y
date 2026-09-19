"""Sentinel exceptions for the SDK's call-error channel.

``GenerationRecorder.set_call_error`` takes an ``Exception``. A string reaches
``span.record_exception`` inside ``end()`` and raises ``TypeError`` under
``ContentCaptureMode.FULL``, leaking the span.
Synthesizing an exception from a message is what the first-party plugins do.

The SDK picks ``error.category`` from the exception: it reads ``status_code``
off it (429 to ``rate_limit``, 401/403 to ``auth_error``, 5xx to
``server_error``) and otherwise scans ``str(error)``. Both sentinels carry
``status_code`` and keep a short message so the scan finds nothing to
misread.
"""

from __future__ import annotations


class ProviderCallError(Exception):
    """An LLM API call hermes reported as failed."""

    def __init__(self, error_type: str = "", status_code: int | None = None) -> None:
        super().__init__(error_type or "api_request_error")
        self.status_code = status_code


class SupersededAttempt(ProviderCallError):
    """An attempt a retry displaced before its ``post_api_request`` fired.

    Hermes assigns ``api_request_id`` above its retry loop, so a second
    ``pre_api_request`` for an id means the first attempt was abandoned. It was
    a real provider call, so it is exported, but marked rather than reported as
    a successful generation with no output.
    """

    def __init__(self) -> None:
        super().__init__("superseded_by_retry")
