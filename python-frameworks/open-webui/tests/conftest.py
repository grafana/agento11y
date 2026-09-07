from __future__ import annotations

import os

import pytest
from agento11y.config import _WARNED_LEGACY_ENV


@pytest.fixture(autouse=True)
def _clear_agento11y_env(monkeypatch):
    for key in list(os.environ):
        if key.startswith(("AGENTO11Y_", "SIGIL_", "OTEL_")):
            monkeypatch.delenv(key, raising=False)
    _WARNED_LEGACY_ENV.clear()
    yield
    _WARNED_LEGACY_ENV.clear()
