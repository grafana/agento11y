"""Shared fixtures for the agento11y LiteLLM framework tests."""

from __future__ import annotations

import os

import pytest
from agento11y.config import _WARNED_LEGACY_ENV


@pytest.fixture(autouse=True)
def _clear_agento11y_env(monkeypatch):
    """Strip ambient AGENTO11Y_* / SIGIL_* / OTEL_* so Client() ignores the local shell.

    Mirrors ``python/tests/conftest.py``. Without it, a developer who exports
    ``AGENTO11Y_CONTENT_CAPTURE_MODE`` or ``AGENTO11Y_REDACT_INPUT_MESSAGES``
    sees content-capture assertions fail here even though CI passes.
    """
    for key in list(os.environ):
        if key.startswith(("AGENTO11Y_", "SIGIL_", "OTEL_")):
            monkeypatch.delenv(key, raising=False)
    _WARNED_LEGACY_ENV.clear()
    yield
    _WARNED_LEGACY_ENV.clear()
