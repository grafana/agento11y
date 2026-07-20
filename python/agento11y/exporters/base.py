"""Exporter protocol used by the generation runtime."""

from __future__ import annotations

from typing import Protocol

from ..models import (
    ExportGenerationsRequest,
    ExportGenerationsResponse,
    ExportWorkflowStepsRequest,
    ExportWorkflowStepsResponse,
)


class GenerationExporter(Protocol):
    """Exporter protocol for generation ingest transports."""

    def export_generations(self, request: ExportGenerationsRequest) -> ExportGenerationsResponse:
        """Exports one generation batch."""

    def export_workflow_steps(self, request: ExportWorkflowStepsRequest) -> ExportWorkflowStepsResponse:
        """Exports one workflow step batch."""

    def shutdown(self) -> None:
        """Closes transport resources."""
