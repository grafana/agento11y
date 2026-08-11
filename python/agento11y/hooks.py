"""Synchronous hook evaluation for Agent Observability preflight/postflight rules."""

from __future__ import annotations

import base64
import json
import logging
from dataclasses import dataclass, field
from enum import Enum
from typing import Any
from urllib import error as urllib_error
from urllib import parse as urllib_parse
from urllib import request as urllib_request

from opentelemetry import trace

from .config import HooksConfig
from .context import conversation_id_from_context
from .errors import HookDeniedError, HookTransportError
from .models import Message, MessageRole, Part, PartKind, ToolCall, ToolDefinition, ToolResult

_logger = logging.getLogger("agento11y")

HOOKS_EVALUATE_PATH = "/api/v1/hooks:evaluate"
HOOK_TIMEOUT_HEADER = "X-Agento11y-Hook-Timeout-Ms"
DEFAULT_HOOK_TIMEOUT = 15.0
_MAX_HOOK_RESPONSE_BYTES = 4 << 20

# MessageRole enum values from proto/agento11y/v1/generation_ingest.proto, as an
# emitter that marshals the generated structs with a plain JSON encoder sends
# them. Kept as literals so the hook path does not pull in the protobuf stubs.
_PROTO_ROLE_ASSISTANT = 2
_PROTO_ROLE_TOOL = 3


class HookPhase(str, Enum):
    """Hook evaluation phases."""

    PREFLIGHT = "preflight"
    POSTFLIGHT = "postflight"


class HookAction(str, Enum):
    """Verdict returned by the hook evaluation service."""

    ALLOW = "allow"
    DENY = "deny"


@dataclass(slots=True)
class HookModel:
    """Identifies the upstream model for hook rule matching."""

    provider: str = ""
    name: str = ""


@dataclass(slots=True)
class HookContext:
    """Routing/matching context attached to a hook evaluation request."""

    model: HookModel | None = None
    agent_name: str = ""
    agent_version: str = ""
    tags: dict[str, str] = field(default_factory=dict)
    conversation_id: str = ""
    trace_id: str = ""
    span_id: str = ""


@dataclass(slots=True)
class HookInput:
    """Evaluable payload (request for preflight, request+response for postflight)."""

    messages: list[Message] = field(default_factory=list)
    tools: list[ToolDefinition] = field(default_factory=list)
    system_prompt: str = ""
    output: list[Message] = field(default_factory=list)
    conversation_preview: str = ""


@dataclass(slots=True)
class HookEvaluateRequest:
    """Hook evaluation request body."""

    phase: str
    context: HookContext
    input: HookInput


@dataclass(slots=True)
class HookEvaluation:
    """Per-rule outcome reported by the server."""

    rule_id: str
    evaluator_id: str
    evaluator_kind: str
    passed: bool
    latency_ms: int = 0
    explanation: str = ""
    reason: str = ""


@dataclass(slots=True)
class HookEvaluateResponse:
    """Hook evaluation response body."""

    action: str
    rule_id: str = ""
    reason: str = ""
    transformed_input: HookInput | None = None
    evaluations: list[HookEvaluation] = field(default_factory=list)

    @property
    def is_deny(self) -> bool:
        """Returns True when the server denied the request."""

        return self.action == HookAction.DENY.value


def hook_denied_from_response(response: HookEvaluateResponse | None) -> HookDeniedError | None:
    """Converts a denied evaluation response into a HookDeniedError."""

    if response is None or not response.is_deny:
        return None
    return HookDeniedError(
        reason=response.reason,
        rule_id=response.rule_id,
        evaluations=list(response.evaluations),
    )


def evaluate_hook(
    *,
    api_endpoint: str,
    insecure: bool,
    extra_headers: dict[str, str],
    hooks: HooksConfig,
    request: HookEvaluateRequest,
) -> HookEvaluateResponse:
    """Sends a hook evaluation request to the Agent Observability API.

    Returns ``HookAction.ALLOW`` without contacting the server when hooks are
    disabled or the request phase is not configured. Honours
    ``HooksConfig.fail_open`` to convert transport failures into allow
    responses (the default).
    """

    if not hooks.enabled:
        return _allow_response()

    phases = hooks.phases or ["preflight"]
    if request.phase not in phases:
        return _allow_response()

    # A caller-built override may leave fields unset; apply the same schema
    # defaults resolve_config applies rather than reading None as "fail closed".
    fail_open = True if hooks.fail_open is None else hooks.fail_open
    timeout = hooks.timeout_seconds if hooks.timeout_seconds and hooks.timeout_seconds > 0 else DEFAULT_HOOK_TIMEOUT
    base_url = _base_url_from_api_endpoint(api_endpoint, insecure)
    if base_url is None:
        return _fail_open_or_raise(fail_open, "api endpoint is required")
    endpoint = base_url.rstrip("/") + HOOKS_EVALUATE_PATH

    timeout_ms = max(1, int(timeout * 1000))
    payload = json.dumps(_serialize_request(request)).encode("utf-8")
    headers = {
        "Content-Type": "application/json",
        HOOK_TIMEOUT_HEADER: str(timeout_ms),
        **(extra_headers or {}),
    }
    http_request = urllib_request.Request(
        endpoint,
        data=payload,
        method="POST",
        headers=headers,
    )

    try:
        with urllib_request.urlopen(http_request, timeout=timeout) as response:
            status = response.getcode()
            raw = response.read(_MAX_HOOK_RESPONSE_BYTES + 1)
    except urllib_error.HTTPError as exc:
        body = ""
        try:
            body = exc.read().decode("utf-8", errors="replace").strip()
        except Exception:  # noqa: BLE001
            body = ""
        message = body if body else f"HTTP {exc.code}"
        return _fail_open_or_raise(fail_open, f"status {exc.code}: {message}")
    except Exception as exc:  # noqa: BLE001
        return _fail_open_or_raise(fail_open, str(exc))

    if status < 200 or status >= 300:
        decoded = raw.decode("utf-8", errors="replace").strip()
        return _fail_open_or_raise(fail_open, f"status {status}: {decoded or 'unexpected status'}")
    if len(raw) > _MAX_HOOK_RESPONSE_BYTES:
        return _fail_open_or_raise(fail_open, "hook response too large")

    text = raw.decode("utf-8", errors="replace").strip()
    if text == "":
        return _fail_open_or_raise(fail_open, "empty hook response payload")

    try:
        parsed = json.loads(text)
    except Exception as exc:  # noqa: BLE001
        return _fail_open_or_raise(fail_open, f"invalid JSON response: {exc}")

    return _parse_response(parsed)


def _allow_response() -> HookEvaluateResponse:
    return HookEvaluateResponse(action=HookAction.ALLOW.value)


def _fail_open_or_raise(fail_open: bool, detail: str) -> HookEvaluateResponse:
    if fail_open:
        # Fail-open turns an evaluator outage into a silent allow, so record it.
        _logger.warning("agento11y: hook evaluation failed, allowing request (fail_open): %s", detail)
        return _allow_response()
    raise HookTransportError(f"agento11y hook evaluation failed: {detail}")


def _serialize_request(request: HookEvaluateRequest) -> dict[str, Any]:
    return {
        "phase": request.phase,
        "context": _serialize_context(request.context),
        "input": _serialize_input(request.input),
    }


def _serialize_context(context: HookContext) -> dict[str, Any]:
    out: dict[str, Any] = {}
    if context.model is not None:
        out["model"] = {
            "provider": context.model.provider,
            "name": context.model.name,
        }
    if context.agent_name:
        out["agent_name"] = context.agent_name
    if context.agent_version:
        out["agent_version"] = context.agent_version
    if context.tags:
        out["tags"] = dict(context.tags)
    conversation_id = context.conversation_id or conversation_id_from_context() or ""
    if conversation_id:
        out["conversation_id"] = conversation_id
    span_context = trace.get_current_span().get_span_context()
    trace_id = context.trace_id
    span_id = context.span_id
    if span_context.is_valid:
        trace_id = trace_id or format(span_context.trace_id, "032x")
        span_id = span_id or format(span_context.span_id, "016x")
    if trace_id:
        out["trace_id"] = trace_id
    if span_id:
        out["span_id"] = span_id
    return out


def _serialize_input(payload: HookInput) -> dict[str, Any]:
    out: dict[str, Any] = {}
    if payload.messages:
        out["messages"] = [_serialize_message(m) for m in payload.messages]
    if payload.tools:
        out["tools"] = [_serialize_tool(t) for t in payload.tools]
    if payload.system_prompt:
        out["system_prompt"] = payload.system_prompt
    if payload.output:
        out["output"] = [_serialize_message(m) for m in payload.output]
    if payload.conversation_preview:
        out["conversation_preview"] = payload.conversation_preview
    return out


def _message_role_wire(role: Any) -> str:
    """Maps SDK message roles to JSON string values for the hooks REST API."""

    value = getattr(role, "value", role)
    if value in ("user", "assistant", "tool"):
        return value
    return "user"


def _serialize_message(message: Message) -> dict[str, Any]:
    """Serializes a message for the hooks API.

    Every part carries its ``kind``. The server dispatches on that field, and it
    recovers a missing one for text only. A ``kind``-less thinking, tool call, or
    tool result part therefore reaches rule evaluation as an empty part, and a
    tool-filter guard sees no tool calls in it and allows the request.

    Tool arguments and tool result payloads go out as embedded JSON, not base64.
    The hooks API reads them as raw JSON, so a base64 blob is what argument-level
    rules end up matching against. Responses come back the other way around,
    because those are protobuf JSON. See ``conformance/hooks/README.md``.
    """

    parts: list[dict[str, Any]] = []
    for part in message.parts:
        if part.kind == PartKind.TEXT and part.text:
            parts.append({"kind": PartKind.TEXT.value, "text": part.text})
        elif part.kind == PartKind.THINKING and part.thinking:
            parts.append({"kind": PartKind.THINKING.value, "thinking": part.thinking})
        elif part.kind == PartKind.TOOL_CALL and part.tool_call is not None:
            payload: dict[str, Any] = {"name": part.tool_call.name}
            if part.tool_call.id:
                payload["id"] = part.tool_call.id
            if part.tool_call.input_json:
                payload["input_json"] = _embedded_json(part.tool_call.input_json)
            parts.append({"kind": PartKind.TOOL_CALL.value, "tool_call": payload})
        elif part.kind == PartKind.TOOL_RESULT and part.tool_result is not None:
            tr = part.tool_result
            tr_payload: dict[str, Any] = {}
            if tr.tool_call_id:
                tr_payload["tool_call_id"] = tr.tool_call_id
            if tr.name:
                tr_payload["name"] = tr.name
            if tr.is_error:
                tr_payload["is_error"] = True
            if tr.content:
                tr_payload["content"] = tr.content
            if tr.content_json:
                tr_payload["content_json"] = _embedded_json(tr.content_json)
            parts.append({"kind": PartKind.TOOL_RESULT.value, "tool_result": tr_payload})
        else:
            # Fallback: emit a minimal text part so the payload remains valid JSON.
            if part.text:
                parts.append({"kind": PartKind.TEXT.value, "text": part.text})
    out: dict[str, Any] = {"role": _message_role_wire(message.role), "parts": parts}
    if message.name:
        out["name"] = message.name
    return out


def _embedded_json(raw: bytes) -> Any:
    """Decodes JSON bytes for embedding in a hook request.

    Bytes that do not parse are sent as a JSON string, which keeps the request
    body valid and leaves the text visible to rules instead of dropping it. Go
    and JS do the same.
    """

    try:
        return json.loads(raw)
    except Exception:  # noqa: BLE001
        return raw.decode("utf-8", errors="replace")


def _serialize_tool(tool: ToolDefinition) -> dict[str, Any]:
    """Serializes a tool definition for the hooks API.

    ``input_schema_json`` stays base64 even though tool call arguments do not: the
    server decodes the tools list straight into its protobuf type, whose
    ``input_schema_json`` is a bytes field. A schema under any other key is
    ignored, and embedded JSON under this key fails that decode, which makes the
    server answer 400 for the whole evaluation.
    """

    out: dict[str, Any] = {"name": tool.name}
    if tool.description:
        out["description"] = tool.description
    if tool.type:
        out["type"] = tool.type
    if tool.input_schema_json:
        out["input_schema_json"] = base64.b64encode(tool.input_schema_json).decode("ascii")
    if tool.deferred:
        out["deferred"] = True
    return out


def _parse_response(payload: Any) -> HookEvaluateResponse:
    if not isinstance(payload, dict):
        return _allow_response()
    action = payload.get("action")
    if action != HookAction.DENY.value:
        action = HookAction.ALLOW.value
    rule_id = _string_field(payload.get("rule_id"))
    reason = _string_field(payload.get("reason"))

    raw_evaluations = payload.get("evaluations")
    evaluations: list[HookEvaluation] = []
    if isinstance(raw_evaluations, list):
        for entry in raw_evaluations:
            if not isinstance(entry, dict):
                continue
            evaluations.append(
                HookEvaluation(
                    rule_id=_string_field(entry.get("rule_id")),
                    evaluator_id=_string_field(entry.get("evaluator_id")),
                    evaluator_kind=_string_field(entry.get("evaluator_kind")),
                    passed=bool(entry.get("passed")),
                    latency_ms=_int_field(entry.get("latency_ms")),
                    explanation=_string_field(entry.get("explanation")),
                    reason=_string_field(entry.get("reason")),
                )
            )
    raw_ti = payload.get("transformed_input")
    transformed_input = _parse_hook_input_wire(raw_ti)

    return HookEvaluateResponse(
        action=action,
        rule_id=rule_id,
        reason=reason,
        transformed_input=transformed_input,
        evaluations=evaluations,
    )


def _parse_hook_input_wire(data: Any) -> HookInput | None:
    """Parses server transformed_input (snake_case JSON) into HookInput."""

    if not isinstance(data, dict):
        return None
    out = HookInput()
    cp = data.get("conversation_preview")
    if isinstance(cp, str) and cp != "":
        out.conversation_preview = cp
    sp = data.get("system_prompt")
    if isinstance(sp, str) and sp != "":
        out.system_prompt = sp
    messages = _parse_wire_messages(data.get("messages"))
    if messages:
        out.messages = messages
    output = _parse_wire_messages(data.get("output"))
    if output:
        out.output = output
    raw_tools = data.get("tools")
    if isinstance(raw_tools, list):
        tools: list[ToolDefinition] = []
        for item in raw_tools:
            tool = _parse_wire_tool_dict(item)
            if tool is not None:
                tools.append(tool)
        if tools:
            out.tools = tools
    if out.messages or out.output or out.tools or out.conversation_preview or out.system_prompt:
        return out
    return None


def _parse_wire_messages(raw: Any) -> list[Message]:
    if not isinstance(raw, list):
        return []
    out: list[Message] = []
    for item in raw:
        message = _parse_wire_message_dict(item)
        if message is not None:
            out.append(message)
    return out


def _parse_wire_tool_dict(item: Any) -> ToolDefinition | None:
    if not isinstance(item, dict):
        return None
    name = _string_field(item.get("name"))
    if name == "":
        return None
    return ToolDefinition(
        name=name,
        description=_string_field(item.get("description")),
        type=_string_field(item.get("type")),
        input_schema_json=_wire_json_bytes(item.get("input_schema_json")),
        deferred=item.get("deferred") is True,
    )


def _parse_wire_message_dict(item: Any) -> Message | None:
    """Parses one wire message from a server's ``transformed_input``.

    Any role but ``assistant`` and ``tool`` becomes ``user``, so a ``system``
    message in a transform is indistinguishable from a user one. Adapters that
    write a transform back into an outgoing request do not read the role at all.
    They send the system prompt as its own field, and they match transformed
    messages to the request by position.
    """
    if not isinstance(item, dict):
        return None
    role_val = item.get("role", "user")
    role = MessageRole.USER
    if isinstance(role_val, int):
        if role_val == _PROTO_ROLE_ASSISTANT:
            role = MessageRole.ASSISTANT
        elif role_val == _PROTO_ROLE_TOOL:
            role = MessageRole.TOOL
    else:
        role_raw = str(role_val).lower()
        if role_raw == "assistant":
            role = MessageRole.ASSISTANT
        elif role_raw == "tool":
            role = MessageRole.TOOL
    parts_raw = item.get("parts")
    if not isinstance(parts_raw, list):
        return Message(role=role, parts=[])
    parts: list[Part] = []
    for pr in parts_raw:
        part = _parse_wire_part_dict(pr)
        if part is not None:
            parts.append(part)
    name = item.get("name")
    n = str(name) if isinstance(name, str) else ""
    return Message(role=role, parts=parts, name=n)


def _parse_wire_part_dict(pr: Any) -> Part | None:
    """Reconstructs one wire part, keying off ``kind`` and falling back to payload keys.

    Tool call and tool result payloads arrive base64-encoded because the server
    marshals its protobuf bytes fields with encoding/json. A parser that skipped
    those two kinds would drop the transform a guard asked the caller to apply,
    and report nothing.

    The server always sets ``kind``, so the payload-key fallback only covers a
    hand-written or protobuf-JSON body. Go and JS keep the same tolerance.
    """

    if not isinstance(pr, dict):
        return None
    kind = _string_field(pr.get("kind"))
    raw_call = pr.get("tool_call")
    raw_result = pr.get("tool_result")
    if kind == "":
        if isinstance(raw_call, dict):
            kind = PartKind.TOOL_CALL.value
        elif isinstance(raw_result, dict):
            kind = PartKind.TOOL_RESULT.value
        elif isinstance(pr.get("thinking"), str) and pr.get("thinking") != "":
            kind = PartKind.THINKING.value
        elif isinstance(pr.get("text"), str) and pr.get("text") != "":
            kind = PartKind.TEXT.value
        else:
            return None
    if kind == PartKind.TOOL_CALL.value:
        if not isinstance(raw_call, dict):
            return None
        name = _string_field(raw_call.get("name"))
        if name == "":
            return None
        return Part(
            kind=PartKind.TOOL_CALL,
            tool_call=ToolCall(
                name=name,
                id=_string_field(raw_call.get("id")),
                input_json=_wire_json_bytes(raw_call.get("input_json")),
            ),
        )
    if kind == PartKind.TOOL_RESULT.value:
        if not isinstance(raw_result, dict):
            return None
        return Part(
            kind=PartKind.TOOL_RESULT,
            tool_result=ToolResult(
                tool_call_id=_string_field(raw_result.get("tool_call_id")),
                name=_string_field(raw_result.get("name")),
                content=_string_field(raw_result.get("content")),
                content_json=_wire_json_bytes(raw_result.get("content_json")),
                is_error=raw_result.get("is_error") is True,
            ),
        )
    if kind == PartKind.THINKING.value:
        thinking = _string_field(pr.get("thinking"))
        return Part(kind=PartKind.THINKING, thinking=thinking) if thinking else None
    text = _string_field(pr.get("text"))
    return Part(kind=PartKind.TEXT, text=text) if text else None


def _wire_json_bytes(value: Any) -> bytes:
    """Recovers a response-side JSON payload as raw bytes.

    The server base64-encodes protobuf bytes fields, so the common case is a
    base64 string holding a JSON document. The result is always a JSON document:
    base64 that decodes to something else, and a string that is neither base64
    nor JSON, are kept as a JSON string so the text survives. Go and JS apply the
    same rule; see ``conformance/hooks/README.md``.
    """

    if isinstance(value, (dict, list)):
        return json.dumps(value, separators=(",", ":")).encode("utf-8")
    if not isinstance(value, str) or value == "":
        return b""
    decoded = _decode_base64(value)
    if decoded is not None:
        if _is_json_document(decoded):
            return decoded
        return _json_string_bytes(decoded.decode("utf-8", errors="replace"))
    raw = value.encode("utf-8")
    if _is_json_document(raw):
        return raw
    return _json_string_bytes(value)


def _decode_base64(value: str) -> bytes | None:
    try:
        return base64.b64decode(value, validate=True)
    except Exception:  # noqa: BLE001
        return None


def _is_json_document(raw: bytes) -> bool:
    try:
        json.loads(raw)
    except Exception:  # noqa: BLE001
        return False
    return True


def _json_string_bytes(text: str) -> bytes:
    return json.dumps(text).encode("utf-8")


def _string_field(value: Any) -> str:
    if isinstance(value, str):
        return value
    return ""


def _int_field(value: Any) -> int:
    if isinstance(value, bool):
        return 0
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        try:
            return int(value)
        except (OverflowError, ValueError):
            return 0
    if isinstance(value, str):
        try:
            return int(value)
        except ValueError:
            return 0
    return 0


def _base_url_from_api_endpoint(endpoint: str, insecure: bool) -> str | None:
    trimmed = (endpoint or "").strip()
    if trimmed == "":
        return None
    if trimmed.startswith("http://") or trimmed.startswith("https://"):
        parsed = urllib_parse.urlparse(trimmed)
        if not parsed.scheme or not parsed.netloc:
            return None
        return f"{parsed.scheme}://{parsed.netloc}"
    without_scheme = trimmed[7:] if trimmed.startswith("grpc://") else trimmed
    host = without_scheme.split("/", 1)[0].strip()
    if host == "":
        return None
    scheme = "http" if insecure else "https"
    return f"{scheme}://{host}"
