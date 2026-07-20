"""Mapping helpers between Python models and generated protobuf types."""

from __future__ import annotations

import hashlib
from datetime import timezone

from google.protobuf import json_format, struct_pb2, timestamp_pb2

from .internal.gen.sigil.v1 import generation_ingest_pb2 as sigil_pb2
from .models import (
    ArtifactKind,
    Generation,
    GenerationMode,
    MessageRole,
    PartKind,
    WorkflowStep,
)


def generation_to_proto(generation: Generation) -> sigil_pb2.Generation:
    """Converts a `Generation` model into protobuf `sigil.v1.Generation`."""

    message = sigil_pb2.Generation(
        id=generation.id,
        conversation_id=generation.conversation_id,
        operation_name=generation.operation_name,
        mode=_map_generation_mode(generation.mode),
        trace_id=generation.trace_id,
        span_id=generation.span_id,
        model=sigil_pb2.ModelRef(
            provider=generation.model.provider,
            name=generation.model.name,
        ),
        response_id=generation.response_id,
        response_model=generation.response_model,
        system_prompt=generation.system_prompt,
        input=[_map_message(msg) for msg in generation.input],
        output=[_map_message(msg) for msg in generation.output],
        tools=[_map_tool(tool) for tool in generation.tools],
        usage=sigil_pb2.TokenUsage(
            input_tokens=generation.usage.input_tokens,
            output_tokens=generation.usage.output_tokens,
            total_tokens=generation.usage.total_tokens,
            cache_read_input_tokens=generation.usage.cache_read_input_tokens,
            cache_write_input_tokens=generation.usage.cache_write_input_tokens,
            reasoning_tokens=generation.usage.reasoning_tokens,
        ),
        stop_reason=generation.stop_reason,
        tags=dict(generation.tags),
        raw_artifacts=[_map_artifact(artifact) for artifact in generation.artifacts],
        call_error=generation.call_error,
        agent_name=generation.agent_name,
        agent_version=generation.agent_version,
        parent_generation_ids=list(generation.parent_generation_ids),
    )

    if generation.started_at is not None:
        started_at = timestamp_pb2.Timestamp()
        started_at.FromDatetime(generation.started_at.astimezone(timezone.utc))
        message.started_at.CopyFrom(started_at)

    if generation.completed_at is not None:
        completed_at = timestamp_pb2.Timestamp()
        completed_at.FromDatetime(generation.completed_at.astimezone(timezone.utc))
        message.completed_at.CopyFrom(completed_at)

    if generation.metadata:
        metadata = struct_pb2.Struct()
        metadata.update(generation.metadata)
        message.metadata.CopyFrom(metadata)

    if generation.max_tokens is not None:
        message.max_tokens = generation.max_tokens
    if generation.temperature is not None:
        message.temperature = generation.temperature
    if generation.top_p is not None:
        message.top_p = generation.top_p
    if generation.tool_choice is not None:
        message.tool_choice = generation.tool_choice
    if generation.thinking_enabled is not None:
        message.thinking_enabled = generation.thinking_enabled

    trimmed_effective_version = generation.effective_version.strip()
    if trimmed_effective_version:
        digest = hashlib.sha256(trimmed_effective_version.encode("utf-8")).hexdigest()
        message.effective_version = "sha256:" + digest

    return message


def generation_to_proto_json(generation: Generation) -> dict[str, object]:
    """Converts a generation into proto-json dictionary with snake_case keys."""

    message = generation_to_proto(generation)
    return json_format.MessageToDict(
        message,
        preserving_proto_field_name=True,
    )


def workflow_step_to_proto_json(step: WorkflowStep) -> dict[str, object]:
    """Converts a WorkflowStep into proto-json dictionary with snake_case keys."""

    message = workflow_step_to_proto(step)
    return json_format.MessageToDict(
        message,
        preserving_proto_field_name=True,
    )


def workflow_step_to_proto(step: WorkflowStep) -> sigil_pb2.WorkflowStep:
    """Converts a WorkflowStep model into protobuf sigil.v1.WorkflowStep."""

    message = sigil_pb2.WorkflowStep(
        id=step.id,
        conversation_id=step.conversation_id,
        step_name=step.step_name,
        framework=step.framework,
        agent_name=step.agent_name,
        agent_version=step.agent_version,
        error=step.error,
        tags=dict(step.tags),
        linked_generation_ids=list(step.linked_generation_ids),
        parent_step_ids=list(step.parent_step_ids),
        trace_id=step.trace_id,
        span_id=step.span_id,
    )

    if step.started_at is not None:
        started_at = timestamp_pb2.Timestamp()
        started_at.FromDatetime(step.started_at.astimezone(timezone.utc))
        message.started_at.CopyFrom(started_at)

    if step.completed_at is not None:
        completed_at = timestamp_pb2.Timestamp()
        completed_at.FromDatetime(step.completed_at.astimezone(timezone.utc))
        message.completed_at.CopyFrom(completed_at)

    if step.input_state:
        input_state = struct_pb2.Struct()
        input_state.update(step.input_state)
        message.input_state.CopyFrom(input_state)

    if step.output_state:
        output_state = struct_pb2.Struct()
        output_state.update(step.output_state)
        message.output_state.CopyFrom(output_state)

    if step.metadata:
        metadata = struct_pb2.Struct()
        metadata.update(step.metadata)
        message.metadata.CopyFrom(metadata)

    return message


def _map_generation_mode(mode: GenerationMode | None) -> int:
    if mode == GenerationMode.STREAM:
        return sigil_pb2.GENERATION_MODE_STREAM
    if mode == GenerationMode.SYNC:
        return sigil_pb2.GENERATION_MODE_SYNC
    return sigil_pb2.GENERATION_MODE_UNSPECIFIED


def _map_message(message: object) -> sigil_pb2.Message:
    role_value = message.role.value if hasattr(message.role, "value") else str(message.role)
    parts = [_map_part(part) for part in message.parts]
    return sigil_pb2.Message(
        role=_map_message_role(role_value),
        name=message.name,
        parts=parts,
    )


def _map_message_role(role: str) -> int:
    if role == MessageRole.USER.value:
        return sigil_pb2.MESSAGE_ROLE_USER
    if role == MessageRole.ASSISTANT.value:
        return sigil_pb2.MESSAGE_ROLE_ASSISTANT
    if role == MessageRole.TOOL.value:
        return sigil_pb2.MESSAGE_ROLE_TOOL
    return sigil_pb2.MESSAGE_ROLE_UNSPECIFIED


def _map_part(part: object) -> sigil_pb2.Part:
    metadata = None
    provider_type = getattr(part.metadata, "provider_type", "") if getattr(part, "metadata", None) is not None else ""
    if provider_type:
        metadata = sigil_pb2.PartMetadata(provider_type=provider_type)

    kind_value = part.kind.value if hasattr(part.kind, "value") else str(part.kind)
    if kind_value == PartKind.TEXT.value:
        return sigil_pb2.Part(metadata=metadata, text=part.text)
    if kind_value == PartKind.THINKING.value:
        return sigil_pb2.Part(metadata=metadata, thinking=part.thinking)
    if kind_value == PartKind.TOOL_CALL.value:
        return sigil_pb2.Part(
            metadata=metadata,
            tool_call=sigil_pb2.ToolCall(
                id=part.tool_call.id,
                name=part.tool_call.name,
                input_json=bytes(part.tool_call.input_json),
            ),
        )
    if kind_value == PartKind.TOOL_RESULT.value:
        return sigil_pb2.Part(
            metadata=metadata,
            tool_result=sigil_pb2.ToolResult(
                tool_call_id=part.tool_result.tool_call_id,
                name=part.tool_result.name,
                content=part.tool_result.content,
                content_json=bytes(part.tool_result.content_json),
                is_error=part.tool_result.is_error,
            ),
        )
    return sigil_pb2.Part(metadata=metadata)


def _map_tool(tool: object) -> sigil_pb2.ToolDefinition:
    return sigil_pb2.ToolDefinition(
        name=tool.name,
        description=tool.description,
        type=tool.type,
        input_schema_json=bytes(tool.input_schema_json),
        deferred=bool(tool.deferred),
    )


def _map_artifact(artifact: object) -> sigil_pb2.Artifact:
    return sigil_pb2.Artifact(
        kind=_map_artifact_kind(artifact.kind),
        name=artifact.name,
        content_type=artifact.content_type,
        payload=bytes(artifact.payload),
        record_id=artifact.record_id,
        uri=artifact.uri,
    )


def _map_artifact_kind(kind: ArtifactKind) -> int:
    if kind == ArtifactKind.REQUEST:
        return sigil_pb2.ARTIFACT_KIND_REQUEST
    if kind == ArtifactKind.RESPONSE:
        return sigil_pb2.ARTIFACT_KIND_RESPONSE
    if kind == ArtifactKind.TOOLS:
        return sigil_pb2.ARTIFACT_KIND_TOOLS
    if kind == ArtifactKind.PROVIDER_EVENT:
        return sigil_pb2.ARTIFACT_KIND_PROVIDER_EVENT
    return sigil_pb2.ARTIFACT_KIND_UNSPECIFIED
