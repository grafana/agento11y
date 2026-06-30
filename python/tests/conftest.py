"""Shared test fixtures/helpers for Sigil Python SDK tests."""

from __future__ import annotations

import copy
import os
import sys
from pathlib import Path

_PYTHON_ROOT = Path(__file__).resolve().parents[1]
if str(_PYTHON_ROOT) not in sys.path:
    sys.path.insert(0, str(_PYTHON_ROOT))

import pytest
from sigil_sdk.models import ExportGenerationResult, ExportGenerationsResponse


@pytest.fixture(autouse=True)
def _clear_sigil_env(monkeypatch):
    """Strip ambient SIGIL_* / OTEL_* so Client() doesn't pick up the dev's shell."""
    for key in list(os.environ):
        if key.startswith("SIGIL_") or key.startswith("OTEL_"):
            monkeypatch.delenv(key, raising=False)
    yield


class CapturingGenerationExporter:
    """In-memory generation exporter used by runtime tests."""

    def __init__(self, failures_before_success: int = 0) -> None:
        self.failures_before_success = failures_before_success
        self.requests = []
        self.attempts = 0
        self.shutdown_calls = 0

    def export_generations(self, request):
        self.attempts += 1
        self.requests.append(copy.deepcopy(request))

        if self.failures_before_success > 0:
            self.failures_before_success -= 1
            raise RuntimeError("forced export failure")

        return ExportGenerationsResponse(
            results=[
                ExportGenerationResult(
                    generation_id=generation.id,
                    accepted=True,
                )
                for generation in request.generations
            ]
        )

    def shutdown(self) -> None:
        self.shutdown_calls += 1
