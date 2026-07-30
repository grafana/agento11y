"""gRPC transport for generation export."""

from __future__ import annotations

from urllib.parse import urlparse

import grpc

from ..internal.gen.agento11y.v1 import generation_ingest_pb2 as agento11y_pb2
from ..internal.gen.agento11y.v1 import generation_ingest_pb2_grpc as agento11y_pb2_grpc
from ..models import (
    ExportGenerationResult,
    ExportGenerationsRequest,
    ExportGenerationsResponse,
    ExportWorkflowStepResult,
    ExportWorkflowStepsRequest,
    ExportWorkflowStepsResponse,
)
from ..proto_mapping import generation_to_proto, workflow_step_to_proto
from ..version import user_agent


class GRPCGenerationExporter:
    """Sends generation and workflow step batches to the Agent Observability gRPC services."""

    def __init__(self, endpoint: str, headers: dict[str, str] | None = None, insecure: bool = False) -> None:
        host, implicit_insecure = _parse_endpoint(endpoint)
        # gRPC reserves the user-agent metadata key, so the User-Agent travels
        # via the channel option rather than per-call metadata. grpc appends its
        # own token after this value.
        user_agent_value = user_agent()
        metadata: list[tuple[str, str]] = []
        for key, value in (headers or {}).items():
            if key.lower() == "user-agent":
                if value.strip():
                    user_agent_value = value
                continue
            metadata.append((key.lower(), value))
        self._headers = metadata
        options = [("grpc.primary_user_agent", user_agent_value)]
        self._channel = (
            grpc.insecure_channel(host, options=options)
            if (insecure or implicit_insecure)
            else grpc.secure_channel(host, grpc.ssl_channel_credentials(), options=options)
        )
        self._stub = agento11y_pb2_grpc.GenerationIngestServiceStub(self._channel)
        self._workflow_step_stub = agento11y_pb2_grpc.WorkflowStepIngestServiceStub(self._channel)

    def export_generations(self, request: ExportGenerationsRequest) -> ExportGenerationsResponse:
        grpc_request = agento11y_pb2.ExportGenerationsRequest(
            generations=[generation_to_proto(generation) for generation in request.generations]
        )
        response = self._stub.ExportGenerations(grpc_request, timeout=10, metadata=self._headers)
        return ExportGenerationsResponse(
            results=[
                ExportGenerationResult(
                    generation_id=result.generation_id,
                    accepted=result.accepted,
                    error=result.error,
                )
                for result in response.results
            ]
        )

    def export_workflow_steps(self, request: ExportWorkflowStepsRequest) -> ExportWorkflowStepsResponse:
        grpc_request = agento11y_pb2.ExportWorkflowStepsRequest(
            workflow_steps=[workflow_step_to_proto(step) for step in request.workflow_steps]
        )
        response = self._workflow_step_stub.ExportWorkflowSteps(grpc_request, timeout=10, metadata=self._headers)
        return ExportWorkflowStepsResponse(
            results=[
                ExportWorkflowStepResult(
                    step_id=result.step_id,
                    accepted=result.accepted,
                    error=result.error,
                )
                for result in response.results
            ]
        )

    def shutdown(self) -> None:
        self._channel.close()


def _parse_endpoint(endpoint: str) -> tuple[str, bool]:
    trimmed = endpoint.strip()
    if not trimmed:
        raise ValueError("endpoint is required")

    if "://" not in trimmed:
        return trimmed, False

    parsed = urlparse(trimmed)
    if parsed.netloc == "":
        raise ValueError("endpoint host is required")

    return parsed.netloc, parsed.scheme == "http"
