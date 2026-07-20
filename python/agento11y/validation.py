"""Generation payload validation logic."""

from __future__ import annotations

from .models import (
    ArtifactKind,
    ContentCaptureMode,
    EmbeddingResult,
    EmbeddingStart,
    Generation,
    GenerationMode,
    MessageRole,
    PartKind,
    _metadata_key_content_capture_mode,
)


def _is_content_stripped(generation: Generation) -> bool:
    """Reports whether the generation has been through MetadataOnly stripping."""
    return generation.metadata.get(_metadata_key_content_capture_mode) == ContentCaptureMode.METADATA_ONLY.value


def validate_generation(generation: Generation) -> None:
    """Raises ValueError when a generation payload is invalid."""

    content_stripped = _is_content_stripped(generation)

    if generation.mode is not None and generation.mode not in (GenerationMode.SYNC, GenerationMode.STREAM):
        raise ValueError("generation.mode must be one of SYNC|STREAM")

    if generation.model.provider.strip() == "":
        raise ValueError("generation.model.provider is required")

    if generation.model.name.strip() == "":
        raise ValueError("generation.model.name is required")

    for index, message in enumerate(generation.input):
        _validate_message(
            "generation.input",
            index,
            message.role.value if hasattr(message.role, "value") else str(message.role),
            message.parts,
            content_stripped,
        )

    for index, message in enumerate(generation.output):
        _validate_message(
            "generation.output",
            index,
            message.role.value if hasattr(message.role, "value") else str(message.role),
            message.parts,
            content_stripped,
        )

    for index, tool in enumerate(generation.tools):
        if tool.name.strip() == "":
            raise ValueError(f"generation.tools[{index}].name is required")

    for index, artifact in enumerate(generation.artifacts):
        if artifact.kind not in (
            ArtifactKind.REQUEST,
            ArtifactKind.RESPONSE,
            ArtifactKind.TOOLS,
            ArtifactKind.PROVIDER_EVENT,
        ):
            raise ValueError(f"generation.artifacts[{index}].kind is invalid")
        if artifact.record_id.strip() == "" and len(artifact.payload) == 0:
            raise ValueError(f"generation.artifacts[{index}] must provide payload or record_id")


def validate_embedding_start(start: EmbeddingStart) -> None:
    """Raises ValueError when embedding start payload is invalid."""

    if start.model.provider.strip() == "":
        raise ValueError("embedding.model.provider is required")
    if start.model.name.strip() == "":
        raise ValueError("embedding.model.name is required")
    if start.dimensions is not None and start.dimensions <= 0:
        raise ValueError("embedding.dimensions must be > 0")
    if start.encoding_format != "" and start.encoding_format.strip() == "":
        raise ValueError("embedding.encoding_format must not be blank")


def validate_embedding_result(result: EmbeddingResult) -> None:
    """Raises ValueError when embedding result payload is invalid."""

    if result.input_count < 0:
        raise ValueError("embedding.input_count must be >= 0")
    if result.input_tokens < 0:
        raise ValueError("embedding.input_tokens must be >= 0")
    if result.dimensions is not None and result.dimensions <= 0:
        raise ValueError("embedding.dimensions must be > 0")


def _validate_message(path: str, index: int, role: str, parts: list[object], content_stripped: bool = False) -> None:
    if role not in (MessageRole.USER.value, MessageRole.ASSISTANT.value, MessageRole.TOOL.value):
        raise ValueError(f"{path}[{index}].role must be one of user|assistant|tool")

    if len(parts) == 0:
        raise ValueError(f"{path}[{index}].parts must not be empty")

    for part_index, part in enumerate(parts):
        _validate_part(path, index, part_index, role, part, content_stripped)


def _validate_part(
    path: str, message_index: int, part_index: int, role: str, part: object, content_stripped: bool = False
) -> None:
    kind = part.kind.value if hasattr(part.kind, "value") else str(part.kind)

    if kind not in (
        PartKind.TEXT.value,
        PartKind.THINKING.value,
        PartKind.TOOL_CALL.value,
        PartKind.TOOL_RESULT.value,
    ):
        raise ValueError(f"{path}[{message_index}].parts[{part_index}].kind is invalid")

    field_count = 0
    if getattr(part, "text", "").strip() != "":
        field_count += 1
    if getattr(part, "thinking", "").strip() != "":
        field_count += 1
    if getattr(part, "tool_call", None) is not None:
        field_count += 1
    if getattr(part, "tool_result", None) is not None:
        field_count += 1

    # Stripped text/thinking parts have empty payloads — that's expected.
    stripped_text_or_thinking = content_stripped and kind in (PartKind.TEXT.value, PartKind.THINKING.value)
    if field_count != 1 and not stripped_text_or_thinking:
        raise ValueError(f"{path}[{message_index}].parts[{part_index}] must set exactly one payload field")

    if kind == PartKind.TEXT.value:
        if not content_stripped and getattr(part, "text", "").strip() == "":
            raise ValueError(f"{path}[{message_index}].parts[{part_index}].text is required")
        return

    if kind == PartKind.THINKING.value:
        if role != MessageRole.ASSISTANT.value:
            raise ValueError(f"{path}[{message_index}].parts[{part_index}].thinking only allowed for assistant role")
        if not content_stripped and getattr(part, "thinking", "").strip() == "":
            raise ValueError(f"{path}[{message_index}].parts[{part_index}].thinking is required")
        return

    if kind == PartKind.TOOL_CALL.value:
        if role != MessageRole.ASSISTANT.value:
            raise ValueError(f"{path}[{message_index}].parts[{part_index}].tool_call only allowed for assistant role")
        tool_call = getattr(part, "tool_call", None)
        if tool_call is None or getattr(tool_call, "name", "").strip() == "":
            raise ValueError(f"{path}[{message_index}].parts[{part_index}].tool_call.name is required")
        return

    if role != MessageRole.TOOL.value:
        raise ValueError(f"{path}[{message_index}].parts[{part_index}].tool_result only allowed for tool role")
    if getattr(part, "tool_result", None) is None:
        raise ValueError(f"{path}[{message_index}].parts[{part_index}].tool_result is required")
