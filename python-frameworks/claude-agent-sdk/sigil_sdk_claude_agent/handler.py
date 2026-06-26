"""Claude Agent SDK hook handlers for Sigil generation recording."""

from __future__ import annotations

import json
import secrets
from collections.abc import AsyncIterable, AsyncIterator, Callable
from dataclasses import replace
from pathlib import Path
from typing import Any
from uuid import uuid4

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    HookMatcher,
    ResultMessage,
    TextBlock,
    ThinkingBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)
from claude_agent_sdk import (
    query as claude_query,
)
from sigil_sdk import (
    Client,
    Generation,
    GenerationMode,
    GenerationStart,
    HookContext,
    HookEvaluateRequest,
    HookInput,
    HookModel,
    HookPhase,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    ToolCall,
    ToolDefinition,
    ToolExecutionStart,
    ToolResult,
    assistant_text_message,
    hook_denied_from_response,
    user_text_message,
)
from sigil_sdk.usage import map_usage

_framework_name = "claude-agent-sdk"
_framework_source = "hooks"
_framework_language = "python"
_framework_instrumentation_name = "github.com/grafana/sigil/sdks/python-frameworks/claude-agent-sdk"
_metadata_run_id = "sigil.framework.run_id"
_metadata_run_type = "sigil.framework.run_type"
_metadata_session_id = "sigil.framework.session_id"
_metadata_permission_mode = "sigil.claude_agent.permission_mode"
_metadata_cwd = "sigil.claude_agent.cwd"
_metadata_total_cost_usd = "sigil.claude_agent.total_cost_usd"


class SigilClaudeAgentHandler:
    """Records Claude Agent SDK streams and tool hooks into Sigil."""

    def __init__(
        self,
        *,
        client: Client,
        conversation_id: str = "",
        agent_name: str = "",
        agent_version: str = "",
        model: str = "",
        provider: str = "anthropic",
        capture_inputs: bool = True,
        capture_outputs: bool = True,
        enable_guards: bool = True,
        extra_tags: dict[str, str] | None = None,
        extra_metadata: dict[str, Any] | None = None,
    ) -> None:
        self._client = client
        self._conversation_id = conversation_id.strip()
        self._agent_name = agent_name.strip()
        self._agent_version = agent_version.strip()
        self._model = model.strip()
        self._provider = provider.strip() or "anthropic"
        self._capture_inputs = capture_inputs
        self._capture_outputs = capture_outputs
        self._enable_guards = enable_guards
        self._extra_tags = dict(extra_tags or {})
        self._extra_metadata = dict(extra_metadata or {})

        self._reset_run_state()

    def _reset_run_state(self) -> None:
        self._run_id = str(uuid4())
        self._recorder: Any | None = None
        self._started = False
        self._finished = False
        self._input_messages: list[Message] = []
        self._output_messages: list[Message] = []
        self._usage: Any = None
        self._response_model = ""
        self._stop_reason = ""
        self._session_id = ""
        self._total_cost_usd: float | None = None
        self._tool_runs: dict[str, Any] = {}
        self._tool_arguments: dict[str, Any] = {}
        self._skip_next_replayed_prompt = ""

    def instrument_options(self, options: ClaudeAgentOptions | None = None) -> ClaudeAgentOptions:
        """Return Claude options with Sigil hook callbacks attached."""

        options = options or ClaudeAgentOptions()
        hooks = {event: list(matchers) for event, matchers in (options.hooks or {}).items()}
        self._append_hook(hooks, "UserPromptSubmit", self._on_user_prompt_submit)
        self._append_hook(hooks, "PreToolUse", self._on_pre_tool_use)
        self._append_hook(hooks, "PostToolUse", self._on_post_tool_use)
        self._append_hook(hooks, "PostToolUseFailure", self._on_post_tool_use_failure)
        self._append_hook(hooks, "Stop", self._on_stop)
        return replace(options, hooks=hooks)

    async def start(self, *, prompt: str | AsyncIterable[dict[str, Any]] | None, options: ClaudeAgentOptions) -> None:
        """Start recording the Claude Agent SDK query if it has not started yet."""

        if self._started and not self._finished:
            return
        if self._finished:
            self._reset_run_state()
        self._started = True
        self._model = self._model or (options.model or "")
        conversation_id = self._resolve_conversation_id(options)
        metadata = dict(self._extra_metadata)
        metadata[_metadata_run_id] = self._run_id
        metadata[_metadata_run_type] = "agent"
        if options.permission_mode:
            metadata[_metadata_permission_mode] = options.permission_mode
        if options.cwd is not None:
            metadata[_metadata_cwd] = str(options.cwd)

        tags = dict(self._extra_tags)
        tags["sigil.framework.name"] = _framework_name
        tags["sigil.framework.source"] = _framework_source
        tags["sigil.framework.language"] = _framework_language

        start = GenerationStart(
            conversation_id=conversation_id,
            agent_name=self._agent_name,
            agent_version=self._agent_version,
            mode=GenerationMode.STREAM,
            model=ModelRef(provider=self._provider, name=self._model or "claude"),
            tags=tags,
            metadata=metadata,
            system_prompt=_system_prompt_text(options.system_prompt),
            tools=_tool_definitions_from_options(options),
        )
        if start.id == "":
            start.id = f"gen_{secrets.token_hex(8)}"
        self._recorder = self._client.start_streaming_generation(start)

        if self._capture_inputs and isinstance(prompt, str):
            prompt_text = prompt.strip()
            if prompt_text != "":
                self._input_messages.append(user_text_message(prompt_text))
                self._skip_next_replayed_prompt = prompt_text

    def record_message(self, message: Any) -> None:
        """Record one Claude Agent SDK stream message."""

        if isinstance(message, UserMessage):
            mapped = _map_user_message(message)
            if mapped is not None and self._capture_inputs:
                if self._skip_next_replayed_prompt and _message_text(mapped) == self._skip_next_replayed_prompt:
                    self._skip_next_replayed_prompt = ""
                    return
                self._input_messages.append(mapped)
            return

        if isinstance(message, AssistantMessage):
            if message.model:
                self._response_model = message.model
                if self._model == "":
                    self._model = message.model
            if message.stop_reason:
                self._stop_reason = message.stop_reason
            if message.session_id:
                self._session_id = message.session_id
            if message.usage:
                self._usage = message.usage
            mapped = _map_assistant_message(message)
            if mapped is not None and self._capture_outputs:
                self._output_messages.append(mapped)
            return

        if isinstance(message, ResultMessage):
            self._session_id = message.session_id or self._session_id
            self._stop_reason = message.stop_reason or self._stop_reason
            self._total_cost_usd = message.total_cost_usd
            if message.usage:
                self._usage = message.usage
            elif message.model_usage:
                self._usage = _merge_model_usage(message.model_usage)
            if (
                self._capture_outputs
                and message.result
                and not _messages_contain_text(self._output_messages, message.result)
            ):
                self._output_messages.append(assistant_text_message(message.result))
            error = RuntimeError(message.stop_reason or "claude agent query failed") if message.is_error else None
            self.finish(error=error)

    def finish(self, error: BaseException | None = None) -> None:
        """End the active Sigil generation, exporting any collected result."""

        if self._finished:
            return
        self._finished = True
        self._end_open_tools()
        if self._recorder is None:
            return

        try:
            if error is not None:
                self._recorder.set_call_error(Exception(str(error)))

            metadata: dict[str, Any] = {}
            if self._session_id:
                metadata[_metadata_session_id] = self._session_id
            if self._total_cost_usd is not None:
                metadata[_metadata_total_cost_usd] = self._total_cost_usd

            self._recorder.set_result(
                Generation(
                    input=self._input_messages if self._capture_inputs else [],
                    output=self._output_messages if self._capture_outputs else [],
                    usage=map_usage(self._usage),
                    response_model=self._response_model,
                    stop_reason=self._stop_reason,
                    metadata=metadata,
                )
            )
        finally:
            self._recorder.end()

        recorder_error = self._recorder.err()
        if recorder_error is not None:
            raise recorder_error

    async def _on_user_prompt_submit(
        self, input_data: HookInput, _tool_use_id: str | None, _context: Any
    ) -> dict[str, Any]:
        if not self._enable_guards:
            return {}
        prompt = _as_string(_read(input_data, "prompt"))
        if prompt == "":
            return {}
        denied = self._evaluate_guard(
            messages=[user_text_message(prompt)],
            conversation_preview=prompt,
        )
        if denied is None:
            return {}
        return {
            "continue_": False,
            "stopReason": denied.reason,
            "systemMessage": str(denied),
            "reason": denied.reason,
        }

    async def _on_pre_tool_use(self, input_data: HookInput, tool_use_id: str | None, _context: Any) -> dict[str, Any]:
        tool_name = _as_string(_read(input_data, "tool_name"))
        tool_input = _read(input_data, "tool_input") or {}
        run_key = _tool_run_key(input_data, tool_use_id)
        recorder = self._client.start_tool_execution(
            ToolExecutionStart(
                tool_name=tool_name or "claude_tool",
                tool_call_id=run_key,
                conversation_id=self._conversation_id,
                agent_name=self._agent_name,
                agent_version=self._agent_version,
                include_content=self._capture_inputs or self._capture_outputs,
            )
        )
        self._tool_runs[run_key] = recorder
        if self._capture_inputs:
            self._tool_arguments[run_key] = tool_input

        if not self._enable_guards:
            return {}
        denied = self._evaluate_guard(
            tools=[ToolDefinition(name=tool_name, type="claude_agent_tool")],
            conversation_preview=f"{tool_name} {_json_string(tool_input)}".strip(),
        )
        if denied is None:
            return {}

        self._finish_tool(run_key, error=denied)
        return {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "deny",
                "permissionDecisionReason": denied.reason,
            }
        }

    async def _on_post_tool_use(self, input_data: HookInput, tool_use_id: str | None, _context: Any) -> dict[str, Any]:
        run_key = _tool_run_key(input_data, tool_use_id)
        self._finish_tool(run_key, result=_read(input_data, "tool_response"))
        return {}

    async def _on_post_tool_use_failure(
        self, input_data: HookInput, tool_use_id: str | None, _context: Any
    ) -> dict[str, Any]:
        run_key = _tool_run_key(input_data, tool_use_id)
        self._finish_tool(run_key, error=RuntimeError(_as_string(_read(input_data, "error")) or "tool failed"))
        return {}

    async def _on_stop(self, _input_data: HookInput, _tool_use_id: str | None, _context: Any) -> dict[str, Any]:
        self._end_open_tools()
        return {}

    def _evaluate_guard(
        self,
        *,
        messages: list[Message] | None = None,
        tools: list[ToolDefinition] | None = None,
        conversation_preview: str = "",
    ) -> Any | None:
        response = self._client.evaluate_hook(
            HookEvaluateRequest(
                phase=HookPhase.PREFLIGHT.value,
                context=HookContext(
                    model=HookModel(provider=self._provider, name=self._model or "claude"),
                    agent_name=self._agent_name,
                    agent_version=self._agent_version,
                    tags={
                        "sigil.framework.name": _framework_name,
                        **self._extra_tags,
                    },
                ),
                input=HookInput(
                    messages=messages or [],
                    tools=tools or [],
                    conversation_preview=conversation_preview,
                ),
            )
        )
        return hook_denied_from_response(response)

    def _append_hook(self, hooks: dict[str, list[HookMatcher]], event: str, callback: Any) -> None:
        matchers = hooks.setdefault(event, [])
        if any(getattr(matcher, "_sigil_claude_agent", False) for matcher in matchers):
            return
        matcher = HookMatcher(hooks=[callback])
        matcher._sigil_claude_agent = True
        matchers.append(matcher)

    def _resolve_conversation_id(self, options: ClaudeAgentOptions) -> str:
        if self._conversation_id:
            return self._conversation_id
        if options.session_id:
            self._conversation_id = options.session_id
            return self._conversation_id
        if options.resume:
            self._conversation_id = options.resume
            return self._conversation_id
        self._conversation_id = f"sigil:framework:{_framework_name}:{self._run_id}"
        return self._conversation_id

    def _finish_tool(self, run_key: str, *, result: Any = None, error: BaseException | None = None) -> None:
        recorder = self._tool_runs.pop(run_key, None)
        arguments = self._tool_arguments.pop(run_key, None)
        if recorder is None:
            return
        try:
            if error is not None:
                recorder.set_exec_error(Exception(str(error)))
            else:
                payload: dict[str, Any] = {}
                if arguments is not None:
                    payload["arguments"] = arguments
                if self._capture_outputs:
                    payload["result"] = result
                recorder.set_result(**payload)
        finally:
            recorder.end()
        recorder_error = recorder.err()
        if recorder_error is not None:
            raise recorder_error

    def _end_open_tools(self) -> None:
        for run_key in list(self._tool_runs):
            self._finish_tool(run_key, error=RuntimeError("tool did not complete before Claude Agent SDK stopped"))


class SigilClaudeSDKClient:
    """Wrap ``ClaudeSDKClient`` and record each response stream to Sigil."""

    def __init__(
        self,
        *,
        client: Client,
        options: ClaudeAgentOptions | None = None,
        _claude_client: ClaudeSDKClient | None = None,
        _client_factory: Callable[..., ClaudeSDKClient] = ClaudeSDKClient,
        **handler_kwargs: Any,
    ) -> None:
        self._sigil_client = client
        self._handler_kwargs = dict(handler_kwargs)
        self._active_handler: SigilClaudeAgentHandler | None = None
        self._options = self._instrument_options(options)
        self._claude = _claude_client or _client_factory(options=self._options)

    async def __aenter__(self) -> SigilClaudeSDKClient:
        await self._claude.__aenter__()
        return self

    async def __aexit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> bool:
        self._finish_active(error=exc_val if isinstance(exc_val, BaseException) else None)
        return await self._claude.__aexit__(exc_type, exc_val, exc_tb)

    async def query(self, prompt: str | AsyncIterable[dict[str, Any]], session_id: str = "default") -> None:
        """Start a Claude query and the matching Sigil generation."""

        if self._active_handler is not None:
            self._finish_active()
        handler = create_sigil_claude_agent_handler(client=self._sigil_client, **self._handler_kwargs)
        self._active_handler = handler
        await handler.start(prompt=prompt, options=self._options)
        try:
            await self._claude.query(prompt, session_id=session_id)
        except BaseException as exc:
            self._finish_active(error=exc)
            raise

    async def receive_response(self) -> AsyncIterator[Any]:
        """Yield one Claude response stream while recording messages to Sigil."""

        async for message in self._record_messages(self._claude.receive_response()):
            yield message

    async def receive_messages(self) -> AsyncIterator[Any]:
        """Yield all Claude messages while recording messages to Sigil."""

        async for message in self._record_messages(self._claude.receive_messages()):
            yield message

    async def set_permission_mode(self, mode: Any) -> None:
        await self._claude.set_permission_mode(mode)

    async def rewind_files(self, user_message_id: str) -> None:
        await self._claude.rewind_files(user_message_id)

    async def interrupt(self) -> None:
        self._finish_active(error=RuntimeError("Claude Agent SDK query interrupted"))
        await self._claude.interrupt()

    async def disconnect(self) -> None:
        self._finish_active()
        await self._claude.disconnect()

    @property
    def claude(self) -> ClaudeSDKClient:
        """Return the wrapped Claude SDK client for advanced passthrough usage."""

        return self._claude

    def _instrument_options(self, options: ClaudeAgentOptions | None) -> ClaudeAgentOptions:
        options = options or ClaudeAgentOptions()
        hooks = {event: list(matchers) for event, matchers in (options.hooks or {}).items()}
        self._append_hook(hooks, "UserPromptSubmit", self._on_user_prompt_submit)
        self._append_hook(hooks, "PreToolUse", self._on_pre_tool_use)
        self._append_hook(hooks, "PostToolUse", self._on_post_tool_use)
        self._append_hook(hooks, "PostToolUseFailure", self._on_post_tool_use_failure)
        self._append_hook(hooks, "Stop", self._on_stop)
        return replace(options, hooks=hooks)

    def _append_hook(self, hooks: dict[str, list[HookMatcher]], event: str, callback: Any) -> None:
        matchers = hooks.setdefault(event, [])
        matcher = HookMatcher(hooks=[callback])
        matcher._sigil_claude_client = True
        matchers.append(matcher)

    async def _on_user_prompt_submit(
        self, input_data: HookInput, tool_use_id: str | None, context: Any
    ) -> dict[str, Any]:
        if self._active_handler is None:
            return {}
        return await self._active_handler._on_user_prompt_submit(input_data, tool_use_id, context)

    async def _on_pre_tool_use(self, input_data: HookInput, tool_use_id: str | None, context: Any) -> dict[str, Any]:
        if self._active_handler is None:
            return {}
        return await self._active_handler._on_pre_tool_use(input_data, tool_use_id, context)

    async def _on_post_tool_use(self, input_data: HookInput, tool_use_id: str | None, context: Any) -> dict[str, Any]:
        if self._active_handler is None:
            return {}
        return await self._active_handler._on_post_tool_use(input_data, tool_use_id, context)

    async def _on_post_tool_use_failure(
        self, input_data: HookInput, tool_use_id: str | None, context: Any
    ) -> dict[str, Any]:
        if self._active_handler is None:
            return {}
        return await self._active_handler._on_post_tool_use_failure(input_data, tool_use_id, context)

    async def _on_stop(self, input_data: HookInput, tool_use_id: str | None, context: Any) -> dict[str, Any]:
        if self._active_handler is None:
            return {}
        return await self._active_handler._on_stop(input_data, tool_use_id, context)

    async def _record_messages(self, messages: AsyncIterator[Any]) -> AsyncIterator[Any]:
        try:
            async for message in messages:
                if self._active_handler is not None:
                    self._active_handler.record_message(message)
                yield message
        except BaseException as exc:
            self._finish_active(error=exc)
            raise
        finally:
            self._finish_active()

    def _finish_active(self, error: BaseException | None = None) -> None:
        handler = self._active_handler
        self._active_handler = None
        if handler is not None:
            handler.finish(error=error)


def create_sigil_claude_agent_handler(*, client: Client, **handler_kwargs: Any) -> SigilClaudeAgentHandler:
    """Create a Claude Agent SDK Sigil handler."""

    return SigilClaudeAgentHandler(client=client, **handler_kwargs)


def with_sigil_claude_agent_options(
    options: ClaudeAgentOptions | None,
    *,
    client: Client,
    handler: SigilClaudeAgentHandler | None = None,
    **handler_kwargs: Any,
) -> ClaudeAgentOptions:
    """Return Claude Agent SDK options with Sigil hooks attached."""

    handler = handler or create_sigil_claude_agent_handler(client=client, **handler_kwargs)
    return handler.instrument_options(options)


async def sigil_query(
    *,
    prompt: str | AsyncIterable[dict[str, Any]],
    client: Client,
    options: ClaudeAgentOptions | None = None,
    handler: SigilClaudeAgentHandler | None = None,
    _query_fn: Callable[..., AsyncIterator[Any]] | None = None,
    **handler_kwargs: Any,
) -> AsyncIterator[Any]:
    """Run ``claude_agent_sdk.query`` while recording the stream to Sigil."""

    handler = handler or create_sigil_claude_agent_handler(client=client, **handler_kwargs)
    instrumented_options = handler.instrument_options(options)
    await handler.start(prompt=prompt, options=instrumented_options)
    query_fn = _query_fn or claude_query
    try:
        async for message in query_fn(prompt=prompt, options=instrumented_options):
            handler.record_message(message)
            yield message
    except BaseException as exc:
        handler.finish(error=exc)
        raise
    finally:
        handler.finish()


def _map_user_message(message: UserMessage) -> Message | None:
    return _message_from_content(MessageRole.USER, message.content)


def _map_assistant_message(message: AssistantMessage) -> Message | None:
    return _message_from_content(MessageRole.ASSISTANT, message.content)


def _message_from_content(default_role: MessageRole, content: Any) -> Message | None:
    if isinstance(content, str):
        text = content.strip()
        if text == "":
            return None
        return Message(role=default_role, parts=[Part(kind=PartKind.TEXT, text=text)])

    parts: list[Part] = []
    role = default_role
    for block in content if isinstance(content, list) else []:
        part = _part_from_block(block)
        if part is None:
            continue
        if part.kind == PartKind.TOOL_RESULT:
            role = MessageRole.TOOL
        parts.append(part)
    if not parts:
        return None
    return Message(role=role, parts=parts)


def _message_text(message: Message) -> str:
    if len(message.parts) != 1:
        return ""
    part = message.parts[0]
    if part.kind != PartKind.TEXT:
        return ""
    return part.text.strip()


def _messages_contain_text(messages: list[Message], text: str) -> bool:
    target = text.strip()
    if target == "":
        return True
    return any(_message_text(message) == target for message in messages)


def _part_from_block(block: Any) -> Part | None:
    if isinstance(block, TextBlock):
        return Part(kind=PartKind.TEXT, text=block.text)
    if isinstance(block, ThinkingBlock):
        return Part(kind=PartKind.THINKING, thinking=block.thinking)
    if isinstance(block, ToolUseBlock) or _has_attrs(block, "id", "name", "input"):
        return Part(
            kind=PartKind.TOOL_CALL,
            tool_call=ToolCall(
                id=_as_string(_read(block, "id")),
                name=_as_string(_read(block, "name")),
                input_json=_json_bytes(_read(block, "input")),
            ),
        )
    if isinstance(block, ToolResultBlock) or _has_attrs(block, "tool_use_id", "content"):
        content = _read(block, "content")
        return Part(
            kind=PartKind.TOOL_RESULT,
            tool_result=ToolResult(
                tool_call_id=_as_string(_read(block, "tool_use_id")),
                content=content if isinstance(content, str) else "",
                content_json=b"" if isinstance(content, str) else _json_bytes(content),
                is_error=bool(_read(block, "is_error")),
            ),
        )
    return None


def _tool_definitions_from_options(options: ClaudeAgentOptions) -> list[ToolDefinition]:
    names: list[str] = []
    raw_tools = options.tools
    if isinstance(raw_tools, list):
        names.extend(str(tool) for tool in raw_tools)
    names.extend(options.allowed_tools or [])
    output: list[ToolDefinition] = []
    seen: set[str] = set()
    for name in names:
        clean = name.strip()
        if clean == "" or clean in seen:
            continue
        seen.add(clean)
        output.append(ToolDefinition(name=clean, type="claude_agent_tool"))
    return output


def _system_prompt_text(system_prompt: Any) -> str:
    if isinstance(system_prompt, str):
        return system_prompt
    if isinstance(system_prompt, dict):
        if system_prompt.get("type") == "preset":
            return _as_string(system_prompt.get("append"))
        if system_prompt.get("type") == "file":
            path = _as_string(system_prompt.get("path"))
            if path:
                return f"file:{path}"
    return ""


def _merge_model_usage(model_usage: dict[str, Any]) -> dict[str, int]:
    merged: dict[str, int] = {}
    for usage in model_usage.values():
        if not isinstance(usage, dict):
            continue
        for key, value in usage.items():
            int_value = _as_int(value)
            if int_value:
                merged[key] = merged.get(key, 0) + int_value
    return merged


def _tool_run_key(input_data: Any, tool_use_id: str | None) -> str:
    return (
        _as_string(tool_use_id)
        or _as_string(_read(input_data, "tool_use_id"))
        or f"{_as_string(_read(input_data, 'tool_name'))}:{secrets.token_hex(8)}"
    )


def _has_attrs(value: Any, *attrs: str) -> bool:
    return all(hasattr(value, attr) for attr in attrs)


def _read(value: Any, key: str) -> Any:
    if value is None:
        return None
    if isinstance(value, dict):
        return value.get(key)
    return getattr(value, key, None)


def _as_string(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, Path):
        return str(value)
    return str(value).strip()


def _as_int(value: Any) -> int:
    if value is None or isinstance(value, bool):
        return 0
    try:
        return int(value)
    except (TypeError, ValueError):
        return 0


def _json_string(value: Any) -> str:
    try:
        return json.dumps(value, default=str, sort_keys=True)
    except (TypeError, ValueError):
        return str(value)


def _json_bytes(value: Any) -> bytes:
    if value is None:
        return b""
    if isinstance(value, bytes):
        return value
    return _json_string(value).encode("utf-8")
