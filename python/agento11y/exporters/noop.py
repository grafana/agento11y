"""No-op generation exporter implementation."""

from __future__ import annotations

from ..models import (
    ExportGenerationResult,
    ExportGenerationsResponse,
    ExportWorkflowStepResult,
    ExportWorkflowStepsResponse,
)


class NoopGenerationExporter:
    """Generation exporter that accepts batches without sending transport calls."""

    def export_generations(self, request):
        return ExportGenerationsResponse(
            results=[
                ExportGenerationResult(
                    generation_id=generation.id,
                    accepted=True,
                )
                for generation in request.generations
            ]
        )

    def export_workflow_steps(self, request):
        return ExportWorkflowStepsResponse(
            results=[ExportWorkflowStepResult(step_id=step.id, accepted=True) for step in request.workflow_steps]
        )

    def shutdown(self) -> None:
        return
