"""Synchronous hook evaluation for Sigil preflight/postflight rules."""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass, field
from enum import Enum
from typing import Any
from urllib import error as urllib_error
from urllib import parse as urllib_parse
from urllib import request as urllib_request

from .config import HooksConfig
from .errors import HookDeniedError, HookTransportError
from .models import Message, MessageRole, Part, PartKind, ToolDefinition

HOOKS_EVALUATE_PATH = "/api/v1/hooks:evaluate"
HOOK_TIMEOUT_HEADER = "X-Sigil-Hook-Timeout-Ms"
DEFAULT_HOOK_TIMEOUT = 15.0
_MAX_HOOK_RESPONSE_BYTES = 4 << 20


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
    """Sends a hook evaluation request to the Sigil API.

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

    timeout = hooks.timeout_seconds if hooks.timeout_seconds > 0 else DEFAULT_HOOK_TIMEOUT
    base_url = _base_url_from_api_endpoint(api_endpoint, insecure)
    if base_url is None:
        return _fail_open_or_raise(hooks.fail_open, "api endpoint is required")
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
        return _fail_open_or_raise(hooks.fail_open, f"status {exc.code}: {message}")
    except Exception as exc:  # noqa: BLE001
        return _fail_open_or_raise(hooks.fail_open, str(exc))

    if status < 200 or status >= 300:
        decoded = raw.decode("utf-8", errors="replace").strip()
        return _fail_open_or_raise(hooks.fail_open, f"status {status}: {decoded or 'unexpected status'}")
    if len(raw) > _MAX_HOOK_RESPONSE_BYTES:
        return _fail_open_or_raise(hooks.fail_open, "hook response too large")

    text = raw.decode("utf-8", errors="replace").strip()
    if text == "":
        return _fail_open_or_raise(hooks.fail_open, "empty hook response payload")

    try:
        parsed = json.loads(text)
    except Exception as exc:  # noqa: BLE001
        return _fail_open_or_raise(hooks.fail_open, f"invalid JSON response: {exc}")

    return _parse_response(parsed)


def _allow_response() -> HookEvaluateResponse:
    return HookEvaluateResponse(action=HookAction.ALLOW.value)


def _fail_open_or_raise(fail_open: bool, detail: str) -> HookEvaluateResponse:
    if fail_open:
        return _allow_response()
    raise HookTransportError(f"sigil hook evaluation failed: {detail}")


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
    parts: list[dict[str, Any]] = []
    for part in message.parts:
        if part.kind == PartKind.TEXT and part.text:
            parts.append({"text": part.text})
        elif part.kind == PartKind.THINKING and part.thinking:
            parts.append({"thinking": part.thinking})
        elif part.kind == PartKind.TOOL_CALL and part.tool_call is not None:
            payload: dict[str, Any] = {
                "id": part.tool_call.id,
                "name": part.tool_call.name,
            }
            if part.tool_call.input_json:
                payload["input_json"] = base64.b64encode(part.tool_call.input_json).decode("ascii")
            parts.append({"tool_call": payload})
        elif part.kind == PartKind.TOOL_RESULT and part.tool_result is not None:
            tr = part.tool_result
            tr_payload: dict[str, Any] = {
                "is_error": tr.is_error,
                "content": tr.content,
            }
            if tr.tool_call_id:
                tr_payload["tool_call_id"] = tr.tool_call_id
            if tr.name:
                tr_payload["name"] = tr.name
            if tr.content_json:
                tr_payload["content_json"] = base64.b64encode(tr.content_json).decode("ascii")
            parts.append({"tool_result": tr_payload})
        else:
            # Fallback: emit a minimal text part so the payload remains valid JSON.
            if part.text:
                parts.append({"text": part.text})
    out: dict[str, Any] = {"role": _message_role_wire(message.role), "parts": parts}
    if message.name:
        out["name"] = message.name
    return out


def _serialize_tool(tool: ToolDefinition) -> dict[str, Any]:
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
    raw_msgs = data.get("messages")
    if isinstance(raw_msgs, list):
        msgs: list[Message] = []
        for item in raw_msgs:
            m = _parse_wire_message_dict(item)
            if m is not None:
                msgs.append(m)
        if msgs:
            out.messages = msgs
    if out.messages or out.conversation_preview or out.system_prompt:
        return out
    return None


def _parse_wire_message_dict(item: Any) -> Message | None:
    if not isinstance(item, dict):
        return None
    role_val = item.get("role", "user")
    role = MessageRole.USER
    if isinstance(role_val, int):
        if role_val == 2:
            role = MessageRole.ASSISTANT
        elif role_val == 3:
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
        if not isinstance(pr, dict):
            continue
        txt = pr.get("text")
        if isinstance(txt, str) and txt != "":
            parts.append(Part(kind=PartKind.TEXT, text=txt))
            continue
        think = pr.get("thinking")
        if isinstance(think, str) and think != "":
            parts.append(Part(kind=PartKind.THINKING, thinking=think))
    name = item.get("name")
    n = str(name) if isinstance(name, str) else ""
    return Message(role=role, parts=parts, name=n)


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
