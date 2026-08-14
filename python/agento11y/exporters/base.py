"""Exporter protocol used by the generation runtime."""

from __future__ import annotations

from datetime import timedelta
from typing import Protocol

from ..models import (
    ExportGenerationsRequest,
    ExportGenerationsResponse,
    ExportWorkflowStepsRequest,
    ExportWorkflowStepsResponse,
)

# Per-request deadline applied to every generation / workflow step export call
# when the caller does not configure ``GenerationExportConfig.export_timeout``.
# Kept here (rather than in config) so exporters constructed directly, without a
# resolved ClientConfig, still get the same bound.
DEFAULT_EXPORT_TIMEOUT_SECONDS = 30.0


def resolve_timeout_seconds(timeout: float | timedelta | None) -> float:
    """Normalizes an exporter timeout to positive seconds.

    Accepts ``timedelta`` (the config field type) or a raw number of seconds
    (what urllib and grpc want). ``None`` and non-positive values fall back to
    :data:`DEFAULT_EXPORT_TIMEOUT_SECONDS` so a misconfigured exporter still has
    a deadline instead of blocking a flush thread forever.
    """

    if timeout is None:
        return DEFAULT_EXPORT_TIMEOUT_SECONDS
    seconds = timeout.total_seconds() if isinstance(timeout, timedelta) else float(timeout)
    if seconds <= 0:
        return DEFAULT_EXPORT_TIMEOUT_SECONDS
    return seconds


class GenerationExporter(Protocol):
    """Exporter protocol for generation ingest transports."""

    def export_generations(self, request: ExportGenerationsRequest) -> ExportGenerationsResponse:
        """Exports one generation batch."""

    def export_workflow_steps(self, request: ExportWorkflowStepsRequest) -> ExportWorkflowStepsResponse:
        """Exports one workflow step batch."""

    def shutdown(self) -> None:
        """Closes transport resources."""
