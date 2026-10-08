from __future__ import annotations

import base64
import json
import math
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any

from .config import ClientConfig
from .errors import OTelDependencyMissingError
from .experimental import FEATURE_OTEL_GENERATION_EXPORT, require_experimental
from .models import (
    ContentCaptureMode,
    Generation,
    GenerationMode,
    GenerationStart,
    Message,
    Part,
    PartKind,
    TokenInputSemantics,
    ToolDefinition,
)
from .proto_mapping import _effective_version_digest

if TYPE_CHECKING:
    from opentelemetry.util.genai.handler import TelemetryHandler
    from opentelemetry.util.genai.invocation import InferenceInvocation
    from opentelemetry.util.types import AttributeValue

INSTRUMENTATION_NAME = "github.com/grafana/sigil/sdks/python"
SCHEMA_URL = "https://opentelemetry.io/schemas/1.37.0"

_PROVIDER_STORED_TO_WIRE = {
    "gemini": "gcp.gemini",
    "mistral": "mistral_ai",
    "moonshotai": "moonshot_ai",
    "vertex": "gcp.vertex_ai",
    "bedrock": "aws.bedrock",
    "azure-openai": "azure.ai.openai",
    "azure-ai-inference": "azure.ai.inference",
    "watsonx": "ibm.watsonx.ai",
    "x-ai": "x_ai",
}
_TITLE = "agento11y.conversation.title"
_CONTENT_METADATA_KEYS = {_TITLE, "sigil.conversation.title", "call_error"}
_THINKING_BUDGET = "agento11y.gen_ai.request.thinking.budget_tokens"


def build_otel_handler(config: ClientConfig) -> TelemetryHandler:
    require_experimental(FEATURE_OTEL_GENERATION_EXPORT)
    try:
        from opentelemetry._logs import NoOpLoggerProvider
        from opentelemetry.util.genai.handler import TelemetryHandler
    except ModuleNotFoundError as exc:
        raise OTelDependencyMissingError(
            'agento11y: AGENTO11Y_PROTOCOL=otel requires "agento11y[otel]"; install the otel extra or select http/grpc'
        ) from exc
    return TelemetryHandler(
        tracer_provider=config.tracer_provider,
        meter_provider=config.meter_provider,
        logger_provider=NoOpLoggerProvider(),
        instrumentation_scope_name=INSTRUMENTATION_NAME,
        instrumentation_scope_version="",
    )


def provider_name(provider: str) -> str:
    return _PROVIDER_STORED_TO_WIRE.get(provider, provider)


def operation_name(operation: str) -> str:
    return "chat" if operation in ("", "generateText", "streamText") else operation


def start_invocation(handler: TelemetryHandler, seed: Generation | GenerationStart) -> InferenceInvocation:
    invocation = handler.inference(
        provider_name(seed.model.provider),
        request_model=seed.model.name,
        operation_name=operation_name(seed.operation_name),
        conversation_id=seed.conversation_id,
    )
    invocation.suspend()
    return invocation


def generation_attributes(
    generation: Generation,
    capture_mode: ContentCaptureMode,
    client_tags: Mapping[str, str] | None = None,
) -> dict[str, AttributeValue]:
    """Serialization failures propagate so the caller can reject the entire record
    before updating its invocation.

    The caller must validate and sanitize the generation and apply the resolved capture policy before mapping.
    """
    attrs: dict[str, AttributeValue] = {
        "agento11y.sdk.name": "sdk-python",
        "gen_ai.operation.name": operation_name(generation.operation_name),
        "gen_ai.provider.name": provider_name(generation.model.provider),
        "gen_ai.request.model": generation.model.name,
    }
    if generation.id:
        attrs["agento11y.record"] = "true"
        attrs["agento11y.generation.id"] = generation.id
    for key, value in (
        ("gen_ai.conversation.id", generation.conversation_id),
        ("gen_ai.agent.name", generation.agent_name),
        ("gen_ai.agent.version", generation.agent_version),
        ("gen_ai.response.id", generation.response_id),
        ("gen_ai.response.model", generation.response_model),
        ("user.id", generation.user_id),
    ):
        if value:
            attrs[key] = value
    if generation.mode == GenerationMode.STREAM:
        attrs["gen_ai.request.stream"] = True
    for key, value in (
        ("gen_ai.request.max_tokens", generation.max_tokens),
        ("gen_ai.request.temperature", generation.temperature),
        ("gen_ai.request.top_p", generation.top_p),
        ("agento11y.gen_ai.request.thinking.enabled", generation.thinking_enabled),
    ):
        if value is not None:
            attrs[key] = value
    if generation.tool_choice is not None and (choice := generation.tool_choice.strip()):
        attrs["agento11y.gen_ai.request.tool_choice"] = choice
    if (budget := _thinking_budget(generation.metadata)) is not None:
        attrs[_THINKING_BUDGET] = budget
    if generation.stop_reason:
        attrs["gen_ai.response.finish_reasons"] = (generation.stop_reason,)
    usage = generation.usage
    if any(
        (
            usage.input_tokens,
            usage.output_tokens,
            usage.total_tokens,
            usage.cache_read_input_tokens,
            usage.cache_write_input_tokens,
            usage.reasoning_tokens,
        )
    ):
        attrs["gen_ai.usage.input_tokens"] = usage.input_tokens
        attrs["gen_ai.usage.output_tokens"] = usage.output_tokens
    for key, count in (
        ("gen_ai.usage.cache_read.input_tokens", usage.cache_read_input_tokens),
        ("gen_ai.usage.cache_creation.input_tokens", usage.cache_write_input_tokens),
        ("gen_ai.usage.cache_write.input_tokens", usage.cache_write_input_tokens),
        ("gen_ai.usage.reasoning.output_tokens", usage.reasoning_tokens),
        ("agento11y.gen_ai.usage.total_tokens", usage.total_tokens),
    ):
        if count:
            attrs[key] = count
    if usage.input_semantics == TokenInputSemantics.INCLUSIVE:
        attrs["gen_ai.token.semantics"] = "inclusive"
    if generation.parent_generation_ids:
        attrs["agento11y.generation.parent_generation_ids"] = tuple(generation.parent_generation_ids)
    if digest := _effective_version_digest(generation.effective_version):
        attrs["agento11y.agent.effective_version"] = digest
    if generation.tags:
        attrs["agento11y.generation.tags"] = _json_dumps(generation.tags, sort_keys=True)
    metadata = {key: value for key, value in generation.metadata.items() if key not in _CONTENT_METADATA_KEYS}
    if metadata:
        attrs["agento11y.generation.metadata"] = _json_dumps(metadata, sort_keys=True)
    if capture_mode != ContentCaptureMode.METADATA_ONLY:
        if generation.conversation_title:
            attrs[_TITLE] = generation.conversation_title
        attrs.update(_content_attributes(generation))
        if generation.artifacts:
            attrs["agento11y.generation.raw_artifacts"] = _json_dumps(_encode_artifacts(generation))
    attrs.update(_tag_attributes(client_tags or {}))
    return attrs


def metric_attributes(
    generation: Generation,
    client_tags: Mapping[str, str] | None = None,
    *,
    error_category: str = "",
) -> dict[str, AttributeValue]:
    """The utility supplies operation and model dimensions."""
    attrs: dict[str, AttributeValue] = dict(_tag_attributes(client_tags or {}))
    if error_category:
        attrs["error.category"] = error_category
    if generation.usage.input_semantics == TokenInputSemantics.INCLUSIVE:
        attrs["gen_ai.token.semantics"] = "inclusive"
    if generation.agent_name:
        attrs["gen_ai.agent.name"] = generation.agent_name
    if generation.agent_version:
        attrs["gen_ai.agent.version"] = generation.agent_version
    return attrs


def _thinking_budget(metadata: Mapping[str, Any]) -> int | None:
    value = metadata.get(_THINKING_BUDGET)
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float) and math.isfinite(value) and value.is_integer():
        return int(value)
    if isinstance(value, str):
        try:
            return int(value.strip())
        except ValueError:
            pass
    return None


def _content_attributes(generation: Generation) -> dict[str, str]:
    attrs: dict[str, str] = {}
    if generation.input:
        attrs["gen_ai.input.messages"] = _json_dumps(_encode_messages(generation.input))
    if generation.output:
        attrs["gen_ai.output.messages"] = _json_dumps(_encode_messages(generation.output, generation.stop_reason))
    if generation.system_prompt:
        attrs["gen_ai.system_instructions"] = _json_dumps([{"type": "text", "content": generation.system_prompt}])
    if generation.tools:
        attrs["gen_ai.tool.definitions"] = _json_dumps(_encode_tool_definitions(generation.tools))
    return attrs


def _encode_messages(messages: list[Message], finish_reason: str | None = None) -> list[dict[str, Any]]:
    encoded: list[dict[str, Any]] = []
    for message in messages:
        item: dict[str, Any] = {"role": message.role.value}
        if message.name:
            item["name"] = message.name
        item["parts"] = [mapped for part in message.parts if (mapped := _encode_part(part)) is not None]
        if finish_reason is not None:
            item["finish_reason"] = finish_reason
        encoded.append(item)
    return encoded


def _encode_part(part: Part) -> dict[str, Any] | None:
    if part.kind == PartKind.TEXT:
        item: dict[str, Any] = {"type": "text", "content": part.text}
    elif part.kind == PartKind.THINKING:
        item = {"type": "reasoning", "content": part.thinking}
    elif part.kind == PartKind.TOOL_CALL:
        if part.tool_call is None:
            return None
        arguments, encoded = _encode_json_bytes(part.tool_call.input_json)
        item = {"type": "tool_call", "id": part.tool_call.id, "name": part.tool_call.name}
        if arguments is not None:
            item["arguments"] = arguments
        if encoded:
            item["agento11y.arguments_b64"] = encoded
    elif part.kind == PartKind.TOOL_RESULT:
        if part.tool_result is None:
            return None
        result = part.tool_result
        response, encoded = _encode_tool_response(result.content, result.content_json)
        item = {"type": "tool_call_response", "id": result.tool_call_id, "response": response}
        if result.name:
            item["agento11y.tool_name"] = result.name
        if result.is_error:
            item["agento11y.is_error"] = True
        if encoded:
            item["agento11y.response_b64"] = encoded
    else:
        return None
    if part.metadata.provider_type:
        item["agento11y.provider_type"] = part.metadata.provider_type
    return item


def _encode_tool_definitions(tools: list[ToolDefinition]) -> list[dict[str, Any]]:
    encoded: list[dict[str, Any]] = []
    for tool in tools:
        item: dict[str, Any] = {"type": tool.type or "function", "name": tool.name}
        if tool.description:
            item["description"] = tool.description
        parameters, parameters_b64 = _encode_json_bytes(tool.input_schema_json)
        if parameters is not None:
            item["parameters"] = parameters
        if tool.deferred:
            item["agento11y.deferred"] = True
        if parameters_b64:
            item["agento11y.input_schema_b64"] = parameters_b64
        encoded.append(item)
    return encoded


def _encode_tool_response(content: str, content_json: bytes) -> tuple[Any, str]:
    if not content_json:
        return content, ""
    embedded, encoded = _encode_json_bytes(content_json)
    if content == "" and embedded is not None and not isinstance(embedded, str):
        return embedded, ""
    return content, encoded or base64.b64encode(content_json).decode("ascii")


def _encode_json_bytes(raw: bytes) -> tuple[Any | None, str]:
    if not raw:
        return None, ""
    try:
        value = json.loads(raw.decode("utf-8"), parse_constant=_reject_json_constant)
        # Sigil keeps the embedded JSON bytes. Parsing and dumping must not rewrite them.
        if value is not None and _json_dumps(value).encode("utf-8") == raw:
            return value, ""
    except (UnicodeError, ValueError, OverflowError, RecursionError):
        pass
    return None, base64.b64encode(raw).decode("ascii")


def _reject_json_constant(value: str) -> None:
    raise ValueError(f"invalid JSON constant {value}")


def _encode_artifacts(generation: Generation) -> list[dict[str, str]]:
    encoded: list[dict[str, str]] = []
    for artifact in generation.artifacts:
        item = {
            key: value
            for key, value in (
                ("kind", artifact.kind.value),
                ("name", artifact.name),
                ("content_type", artifact.content_type),
                ("record_id", artifact.record_id),
                ("uri", artifact.uri),
            )
            if value
        }
        if artifact.payload:
            item["payload_b64"] = base64.b64encode(artifact.payload).decode("ascii")
        encoded.append(item)
    return encoded


def _tag_attributes(tags: Mapping[str, str]) -> dict[str, str]:
    pairs = sorted((key.strip(), (value or "").strip()) for key, value in tags.items() if key.strip())
    return {f"agento11y.tag.{key}": value for key, value in pairs}


def _json_dumps(value: Any, *, sort_keys: bool = False) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), sort_keys=sort_keys, allow_nan=False)


__all__ = [
    "build_otel_handler",
    "generation_attributes",
    "metric_attributes",
    "operation_name",
    "provider_name",
    "start_invocation",
]
