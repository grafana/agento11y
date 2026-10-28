"""Claude Agent SDK agento11y integration tests."""

from __future__ import annotations

import logging
from collections.abc import AsyncIterator
from datetime import timedelta

import pytest
from agento11y import (
    Client,
    ClientConfig,
    ContentCaptureMode,
    GenerationExportConfig,
    HookEvaluateResponse,
    HookInput,
    user_text_message,
)
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse, PartKind
from agento11y_claude_agent import (
    Agento11yClaudeAgentHandler,
    Agento11yClaudeSDKClient,
    agento11y_query,
    create_agento11y_claude_agent_handler,
    with_agento11y_claude_agent_options,
)
from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    TextBlock,
    ThinkingBlock,
    ToolUseBlock,
    UserMessage,
)


class _CapturingExporter:
    def __init__(self) -> None:
        self.requests = []

    def export_generations(self, request):
        self.requests.append(request)
        return ExportGenerationsResponse(
            results=[
                ExportGenerationResult(generation_id=generation.id, accepted=True) for generation in request.generations
            ]
        )

    def shutdown(self) -> None:
        return


def _new_client(exporter: _CapturingExporter) -> Client:
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
            content_capture=ContentCaptureMode.DEFAULT,
        )
    )


class _FakeClaudeSDKClient:
    def __init__(self, options=None) -> None:
        self.options = options
        self.queries = []
        self.permission_modes = []
        self.rewind_ids = []
        self.interrupted = False
        self.disconnected = False
        self.entered = False
        self.exited = False
        self.responses: list[list[object]] = []

    async def __aenter__(self):
        self.entered = True
        return self

    async def __aexit__(self, _exc_type, _exc_val, _exc_tb) -> bool:
        self.exited = True
        return False

    async def query(self, prompt, session_id="default") -> None:
        self.queries.append((prompt, session_id))

    async def receive_response(self) -> AsyncIterator[object]:
        response = self.responses.pop(0)
        for message in response:
            yield message

    async def receive_messages(self) -> AsyncIterator[object]:
        async for message in self.receive_response():
            yield message

    async def set_permission_mode(self, mode) -> None:
        self.permission_modes.append(mode)

    async def rewind_files(self, user_message_id: str) -> None:
        self.rewind_ids.append(user_message_id)

    async def interrupt(self) -> None:
        self.interrupted = True

    async def disconnect(self) -> None:
        self.disconnected = True


def _success_result(session_id: str = "session-42") -> ResultMessage:
    return ResultMessage(
        subtype="success",
        duration_ms=100,
        duration_api_ms=90,
        is_error=False,
        num_turns=1,
        session_id=session_id,
        stop_reason="end_turn",
        total_cost_usd=0.01,
    )


_READ_TOOL = ToolUseBlock(id="toolu_1", name="Read", input={"file_path": "README.md"})


def _part_payload(part) -> str:
    if part.kind == PartKind.THINKING:
        return part.thinking
    if part.kind == PartKind.TOOL_CALL:
        return part.tool_call.name
    return part.text


def _pre_tool_use_input(tool_use_id: str, tool_name: str = "Bash") -> dict[str, object]:
    return {
        "hook_event_name": "PreToolUse",
        "session_id": "session-42",
        "transcript_path": "",
        "cwd": "",
        "tool_name": tool_name,
        "tool_input": {"command": "pwd"},
        "tool_use_id": tool_use_id,
    }


def _post_tool_use_input(tool_use_id: str, tool_response: object) -> dict[str, object]:
    return {
        **_pre_tool_use_input(tool_use_id),
        "hook_event_name": "PostToolUse",
        "tool_response": tool_response,
    }


def _post_tool_use_failure_input(tool_use_id: str, error: str) -> dict[str, object]:
    return {
        **_pre_tool_use_input(tool_use_id),
        "hook_event_name": "PostToolUseFailure",
        "error": error,
    }


class _StubToolRecorder:
    """Tool recorder that stores what it is given, and raises or returns the errors a test configures."""

    def __init__(self, *, end_error: Exception | None = None, final_error: Exception | None = None) -> None:
        self.result: dict[str, object] | None = None
        self.ended = False
        self._end_error = end_error
        self._final_error = final_error
        self._exec_error: Exception | None = None

    def set_result(self, **payload) -> None:
        self.result = payload

    def set_exec_error(self, error) -> None:
        self._exec_error = error

    def end(self) -> None:
        self.ended = True
        if self._end_error is not None:
            raise self._end_error

    def err(self):
        # The real recorder returns the exec error it was given.
        return self._final_error or self._exec_error


class _ToolRecorderClient:
    """Client with a fixed guard decision and optional stub tool recorders.

    Generations come from the real ``client``, and so does every recorder requested
    after ``recorders`` is empty.
    """

    def __init__(
        self,
        client: Client | None = None,
        *,
        recorders: list[_StubToolRecorder] | None = None,
        hook_response: HookEvaluateResponse | None = None,
    ) -> None:
        self._client = client
        self._recorders = list(recorders or [])
        self._hook_response = hook_response or HookEvaluateResponse(action="allow")

    def start_streaming_generation(self, start):
        return self._client.start_streaming_generation(start)

    def start_tool_execution(self, start):
        if self._recorders:
            return self._recorders.pop(0)
        return self._client.start_tool_execution(start)

    def evaluate_hook(self, _request):
        return self._hook_response


@pytest.mark.asyncio
async def test_agento11y_query_records_claude_agent_stream() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def fake_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(
            content=[
                TextBlock("I'll use a tool."),
                ToolUseBlock(id="toolu_1", name="Read", input={"file_path": "README.md"}),
            ],
            model="claude-sonnet-4-5",
            usage={
                "input_tokens": 100,
                "output_tokens": 25,
                "total_tokens": 125,
                "cache_read_input_tokens": 20,
                "cache_write_input_tokens": 10,
            },
            stop_reason="tool_use",
            session_id="session-42",
        )
        yield ResultMessage(
            subtype="success",
            duration_ms=100,
            duration_api_ms=90,
            is_error=False,
            num_turns=1,
            session_id="session-42",
            stop_reason="end_turn",
            total_cost_usd=0.01,
            result="Final answer.",
        )

    try:
        seen = [
            message
            async for message in agento11y_query(
                prompt="Inspect the README.",
                client=client,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5", permission_mode="acceptEdits"),
                conversation_id="conv-42",
                agent_name="claude-agent",
                _query_fn=fake_query,
            )
        ]
        assert len(seen) == 2

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == "conv-42"
        assert generation.agent_name == "claude-agent"
        assert generation.model.provider == "anthropic"
        assert generation.model.name == "claude-sonnet-4-5"
        assert generation.tags["agento11y.framework.name"] == "claude-agent-sdk"
        assert generation.tags["agento11y.framework.source"] == "hooks"
        assert generation.metadata["agento11y.framework.session_id"] == "session-42"
        assert generation.metadata["agento11y.claude_agent.total_cost_usd"] == 0.01
        assert generation.input[0].parts[0].text == "Inspect the README."
        assert generation.output[0].parts[0].text == "I'll use a tool."
        assert generation.output[0].parts[1].kind == PartKind.TOOL_CALL
        assert generation.output[0].parts[1].tool_call.name == "Read"
        assert generation.output[1].parts[0].text == "Final answer."
        assert generation.usage.input_tokens == 100
        assert generation.usage.output_tokens == 25
        assert generation.usage.cache_read_input_tokens == 20
        assert generation.usage.cache_write_input_tokens == 10
        assert generation.stop_reason == "end_turn"
    finally:
        client.shutdown()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("blocks", "expected_parts"),
    [
        pytest.param([TextBlock(""), _READ_TOOL], [(PartKind.TOOL_CALL, "Read")], id="empty-text"),
        pytest.param([TextBlock("   \n "), _READ_TOOL], [(PartKind.TOOL_CALL, "Read")], id="whitespace-text"),
        pytest.param(
            [ThinkingBlock(thinking="", signature="s"), _READ_TOOL],
            [(PartKind.TOOL_CALL, "Read")],
            id="empty-thinking",
        ),
        pytest.param(
            [ThinkingBlock(thinking="   \n ", signature="s"), _READ_TOOL],
            [(PartKind.TOOL_CALL, "Read")],
            id="whitespace-thinking",
        ),
        pytest.param(
            [TextBlock("  I'll use a tool.  "), _READ_TOOL],
            [(PartKind.TEXT, "  I'll use a tool.  "), (PartKind.TOOL_CALL, "Read")],
            id="text",
        ),
        pytest.param(
            [ThinkingBlock(thinking="  I need a tool.  ", signature="s"), _READ_TOOL],
            [(PartKind.THINKING, "  I need a tool.  "), (PartKind.TOOL_CALL, "Read")],
            id="thinking",
        ),
        pytest.param(
            [TextBlock("Reading the README."), ToolUseBlock(id="toolu_1", name="", input={})],
            [(PartKind.TEXT, "Reading the README.")],
            id="empty-tool-name",
        ),
        pytest.param(
            [TextBlock("Reading the README."), ToolUseBlock(id="toolu_1", name="  ", input={})],
            [(PartKind.TEXT, "Reading the README.")],
            id="whitespace-tool-name",
        ),
        pytest.param(
            [TextBlock(""), ToolUseBlock(id="toolu_1", name="", input={})],
            [],
            id="every-block-blank",
        ),
    ],
)
async def test_agento11y_query_skips_blank_blocks(blocks, expected_parts) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def fake_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(
            content=blocks,
            model="claude-sonnet-4-5",
            stop_reason="tool_use",
            session_id="session-42",
        )
        yield _success_result()

    try:
        seen = [
            message
            async for message in agento11y_query(
                prompt="Inspect the README.",
                client=client,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
                conversation_id="conv-42",
                agent_name="claude-agent",
                _query_fn=fake_query,
            )
        ]
        assert len(seen) == 2

        client.flush()
        generations = exporter.requests[0].generations
        assert len(generations) == 1
        output = generations[0].output
        parts = output[0].parts if output else []
        assert [(part.kind, _part_payload(part)) for part in parts] == expected_parts
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_agento11y_query_early_stream_exit_finishes_without_error() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def fake_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("Partial.")], model="claude-sonnet-4-5", session_id="session-42")
        yield AssistantMessage(content=[TextBlock("Unread.")], model="claude-sonnet-4-5", session_id="session-42")

    try:
        stream = agento11y_query(
            prompt="Inspect the README.",
            client=client,
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            conversation_id="conv-42",
            agent_name="claude-agent",
            _query_fn=fake_query,
        )
        async for _message in stream:
            await stream.aclose()
            break

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.output[0].parts[0].text == "Partial."
        assert generation.call_error == ""
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_claude_sdk_client_early_stream_exit_finishes_without_error() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    claude = _FakeClaudeSDKClient()
    claude.responses.append(
        [
            AssistantMessage(content=[TextBlock("Partial.")], model="claude-sonnet-4-5", session_id="session-42"),
            AssistantMessage(content=[TextBlock("Unread.")], model="claude-sonnet-4-5", session_id="session-42"),
        ]
    )

    try:
        async with Agento11yClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            conversation_id="conv-client",
            agent_name="claude-agent",
        ) as agento11y_claude:
            await agento11y_claude.query("Inspect the README.")
            stream = agento11y_claude.receive_response()
            async for _message in stream:
                await stream.aclose()
                break

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.output[0].parts[0].text == "Partial."
        assert generation.call_error == ""
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_handler_start_failure_can_be_retried() -> None:
    class _Recorder:
        def __init__(self) -> None:
            self.generation = None
            self.ended = False

        def set_call_error(self, _error) -> None:
            return

        def set_result(self, generation) -> None:
            self.generation = generation

        def end(self) -> None:
            self.ended = True

        def err(self):
            return None

    class _FlakyClient:
        def __init__(self) -> None:
            self.calls = 0
            self.recorder = _Recorder()

        def start_streaming_generation(self, _start):
            self.calls += 1
            if self.calls == 1:
                raise RuntimeError("start failed")
            return self.recorder

    flaky_client = _FlakyClient()
    handler = Agento11yClaudeAgentHandler(client=flaky_client, conversation_id="conv-42")  # type: ignore[arg-type]
    options = ClaudeAgentOptions(model="claude-sonnet-4-5")

    with pytest.raises(RuntimeError, match="start failed"):
        await handler.start(prompt="First prompt.", options=options)

    await handler.start(prompt="Retry prompt.", options=options)
    handler.record_message(AssistantMessage(content=[TextBlock("Recovered.")], model="claude-sonnet-4-5"))
    handler.record_message(_success_result())

    assert flaky_client.calls == 2
    assert flaky_client.recorder.ended is True
    assert flaky_client.recorder.generation.input[0].parts[0].text == "Retry prompt."
    assert flaky_client.recorder.generation.output[0].parts[0].text == "Recovered."


@pytest.mark.asyncio
async def test_claude_sdk_client_start_failure_can_be_retried() -> None:
    class _Recorder:
        def __init__(self) -> None:
            self.generation = None

        def set_call_error(self, _error) -> None:
            return

        def set_result(self, generation) -> None:
            self.generation = generation

        def end(self) -> None:
            return

        def err(self):
            return None

    class _FlakyAgento11yClient:
        def __init__(self) -> None:
            self.calls = 0
            self.recorder = _Recorder()

        def start_streaming_generation(self, _start):
            self.calls += 1
            if self.calls == 1:
                raise RuntimeError("start failed")
            return self.recorder

    agento11y_client = _FlakyAgento11yClient()
    claude = _FakeClaudeSDKClient()
    claude.responses.append(
        [AssistantMessage(content=[TextBlock("Recovered.")], model="claude-sonnet-4-5"), _success_result()]
    )

    agento11y_claude = Agento11yClaudeSDKClient(
        client=agento11y_client,  # type: ignore[arg-type]
        _claude_client=claude,  # type: ignore[arg-type]
        options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
        conversation_id="conv-client",
        agent_name="claude-agent",
    )

    with pytest.raises(RuntimeError, match="start failed"):
        await agento11y_claude.query("First prompt.")

    await agento11y_claude.query("Retry prompt.")
    _ = [message async for message in agento11y_claude.receive_response()]

    assert agento11y_client.calls == 2
    assert claude.queries == [("Retry prompt.", "default")]
    assert agento11y_client.recorder.generation.input[0].parts[0].text == "Retry prompt."
    assert agento11y_client.recorder.generation.output[0].parts[0].text == "Recovered."


@pytest.mark.asyncio
async def test_agento11y_query_reuses_finished_handler_for_new_generation() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    handler = create_agento11y_claude_agent_handler(client=client, conversation_id="conv-42", agent_name="claude-agent")

    async def first_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("First.")], model="claude-sonnet-4-5", session_id="session-1")
        yield _success_result("session-1")

    async def second_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("Second.")], model="claude-sonnet-4-5", session_id="session-2")
        yield _success_result("session-2")

    try:
        _ = [
            message
            async for message in agento11y_query(
                prompt="First prompt.",
                client=client,
                handler=handler,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
                _query_fn=first_query,
            )
        ]
        _ = [
            message
            async for message in agento11y_query(
                prompt="Second prompt.",
                client=client,
                handler=handler,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
                _query_fn=second_query,
            )
        ]

        client.flush()
        generations = exporter.requests[0].generations
        assert [generation.input[0].parts[0].text for generation in generations] == ["First prompt.", "Second prompt."]
        assert [generation.output[0].parts[0].text for generation in generations] == ["First.", "Second."]
        assert [generation.metadata["agento11y.framework.session_id"] for generation in generations] == [
            "session-1",
            "session-2",
        ]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_reused_handler_resolves_conversation_id_per_run() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    handler = create_agento11y_claude_agent_handler(client=client, agent_name="claude-agent")

    async def first_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("First.")], model="claude-sonnet-4-5", session_id="session-1")
        yield _success_result("session-1")

    async def second_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("Second.")], model="claude-sonnet-4-5", session_id="session-2")
        yield _success_result("session-2")

    try:
        _ = [
            message
            async for message in agento11y_query(
                prompt="First prompt.",
                client=client,
                handler=handler,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5", session_id="conversation-one"),
                _query_fn=first_query,
            )
        ]
        _ = [
            message
            async for message in agento11y_query(
                prompt="Second prompt.",
                client=client,
                handler=handler,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5", session_id="conversation-two"),
                _query_fn=second_query,
            )
        ]

        client.flush()
        generations = exporter.requests[0].generations
        assert [generation.conversation_id for generation in generations] == ["conversation-one", "conversation-two"]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_reused_handler_resolves_model_per_run() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    handler = create_agento11y_claude_agent_handler(client=client, conversation_id="conv-42", agent_name="claude-agent")

    async def first_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("First.")], model="claude-model-a", session_id="session-1")
        yield _success_result("session-1")

    async def second_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("Second.")], model="claude-model-b", session_id="session-2")
        yield _success_result("session-2")

    try:
        _ = [
            message
            async for message in agento11y_query(
                prompt="First prompt.",
                client=client,
                handler=handler,
                options=ClaudeAgentOptions(model="claude-model-a"),
                _query_fn=first_query,
            )
        ]
        _ = [
            message
            async for message in agento11y_query(
                prompt="Second prompt.",
                client=client,
                handler=handler,
                options=ClaudeAgentOptions(model="claude-model-b"),
                _query_fn=second_query,
            )
        ]

        client.flush()
        generations = exporter.requests[0].generations
        assert [generation.model.name for generation in generations] == ["claude-model-a", "claude-model-b"]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_agento11y_query_skips_replayed_initial_user_message() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def fake_query(**_kwargs) -> AsyncIterator[object]:
        yield UserMessage(content="Inspect the README.")
        yield AssistantMessage(content=[TextBlock("Done.")], model="claude-sonnet-4-5", session_id="session-42")
        yield _success_result()

    try:
        seen = [
            message
            async for message in agento11y_query(
                prompt="Inspect the README.",
                client=client,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
                conversation_id="conv-42",
                agent_name="claude-agent",
                _query_fn=fake_query,
            )
        ]
        assert len(seen) == 3

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert [part.text for message in generation.input for part in message.parts] == ["Inspect the README."]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_agento11y_claude_sdk_client_records_receive_response_stream() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    claude = _FakeClaudeSDKClient()
    claude.responses.append(
        [
            UserMessage(content="Inspect the README."),
            AssistantMessage(content=[TextBlock("Done.")], model="claude-sonnet-4-5", session_id="session-42"),
            _success_result(),
        ]
    )

    try:
        async with Agento11yClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5", permission_mode="default"),
            conversation_id="conv-client",
            agent_name="claude-agent",
        ) as agento11y_claude:
            await agento11y_claude.query("Inspect the README.", session_id="work")
            seen = [message async for message in agento11y_claude.receive_response()]

        assert claude.entered is True
        assert claude.exited is True
        assert claude.queries == [("Inspect the README.", "work")]
        assert len(seen) == 3

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == "conv-client"
        assert generation.agent_name == "claude-agent"
        assert generation.input[0].parts[0].text == "Inspect the README."
        assert len(generation.input) == 1
        assert generation.output[0].parts[0].text == "Done."
        assert generation.metadata["agento11y.framework.session_id"] == "session-42"
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_agento11y_claude_sdk_client_records_multiple_queries() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    claude = _FakeClaudeSDKClient()
    claude.responses.extend(
        [
            [AssistantMessage(content=[TextBlock("First.")], model="claude-sonnet-4-5"), _success_result("s1")],
            [AssistantMessage(content=[TextBlock("Second.")], model="claude-sonnet-4-5"), _success_result("s2")],
        ]
    )

    try:
        async with Agento11yClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            conversation_id="conv-client",
            agent_name="claude-agent",
        ) as agento11y_claude:
            await agento11y_claude.query("First prompt.")
            _ = [message async for message in agento11y_claude.receive_response()]
            await agento11y_claude.query("Second prompt.")
            _ = [message async for message in agento11y_claude.receive_messages()]

        client.flush()
        generations = exporter.requests[0].generations
        assert [generation.output[0].parts[0].text for generation in generations] == ["First.", "Second."]
        assert [generation.metadata["agento11y.framework.session_id"] for generation in generations] == ["s1", "s2"]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_agento11y_claude_sdk_client_rejects_overlapping_queries() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    claude = _FakeClaudeSDKClient()

    try:
        async with Agento11yClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            conversation_id="conv-client",
            agent_name="claude-agent",
        ) as agento11y_claude:
            await agento11y_claude.query("First prompt.")
            with pytest.raises(RuntimeError, match="previous response stream finishes"):
                await agento11y_claude.query("Second prompt.")
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_agento11y_claude_sdk_client_uses_stable_fallback_conversation() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    claude = _FakeClaudeSDKClient()
    claude.responses.extend(
        [
            [AssistantMessage(content=[TextBlock("First.")], model="claude-sonnet-4-5"), _success_result("s1")],
            [AssistantMessage(content=[TextBlock("Second.")], model="claude-sonnet-4-5"), _success_result("s2")],
        ]
    )

    try:
        async with Agento11yClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            agent_name="claude-agent",
        ) as agento11y_claude:
            await agento11y_claude.query("First prompt.")
            _ = [message async for message in agento11y_claude.receive_response()]
            await agento11y_claude.query("Second prompt.")
            _ = [message async for message in agento11y_claude.receive_response()]

        client.flush()
        generations = exporter.requests[0].generations
        assert len({generation.conversation_id for generation in generations}) == 1
        assert generations[0].conversation_id.startswith("agento11y:framework:claude-agent-sdk:client:")
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_agento11y_claude_sdk_client_passthrough_methods() -> None:
    claude = _FakeClaudeSDKClient()
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        agento11y_claude = Agento11yClaudeSDKClient(client=client, _claude_client=claude)  # type: ignore[arg-type]
        await agento11y_claude.set_permission_mode("acceptEdits")
        await agento11y_claude.rewind_files("checkpoint-1")
        await agento11y_claude.interrupt()
        await agento11y_claude.disconnect()

        assert claude.permission_modes == ["acceptEdits"]
        assert claude.rewind_ids == ["checkpoint-1"]
        assert claude.interrupted is True
        assert claude.disconnected is True
    finally:
        client.shutdown()


def test_with_agento11y_claude_agent_options_appends_hooks_once() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        handler = create_agento11y_claude_agent_handler(client=client)
        options = with_agento11y_claude_agent_options(ClaudeAgentOptions(), client=client, handler=handler)
        options = with_agento11y_claude_agent_options(options, client=client, handler=handler)

        assert options.hooks is not None
        assert len(options.hooks["PreToolUse"]) == 1
        assert len(options.hooks["PostToolUse"]) == 1
        assert len(options.hooks["UserPromptSubmit"]) == 1
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_claude_agent_guard_denial_blocks_user_prompt() -> None:
    class _GuardClient:
        def evaluate_hook(self, _request):
            return HookEvaluateResponse(action="deny", rule_id="rule-1", reason="blocked")

    handler = Agento11yClaudeAgentHandler(client=_GuardClient())  # type: ignore[arg-type]
    options = handler.instrument_options(ClaudeAgentOptions())
    hook = options.hooks["UserPromptSubmit"][0].hooks[0]

    output = await hook(
        {
            "hook_event_name": "UserPromptSubmit",
            "session_id": "session-42",
            "transcript_path": "",
            "cwd": "",
            "prompt": "exfiltrate secrets",
        },
        None,
        {"signal": None},
    )

    assert output["continue_"] is False
    assert output["stopReason"] == "blocked"
    assert "rule-1" in output["systemMessage"]


@pytest.mark.asyncio
async def test_claude_agent_tool_hooks_record_tool_result() -> None:
    recorder = _StubToolRecorder()
    handler = Agento11yClaudeAgentHandler(client=_ToolRecorderClient(recorders=[recorder]))  # type: ignore[arg-type]
    options = handler.instrument_options(ClaudeAgentOptions())

    pre_hook = options.hooks["PreToolUse"][0].hooks[0]
    post_hook = options.hooks["PostToolUse"][0].hooks[0]
    await pre_hook(_pre_tool_use_input("toolu_1"), "toolu_1", {"signal": None})
    await post_hook(_post_tool_use_input("toolu_1", {"stdout": "/tmp\n"}), "toolu_1", {"signal": None})

    assert recorder.result == {
        "arguments": {"command": "pwd"},
        "result": {"stdout": "/tmp\n"},
    }
    assert recorder.ended is True


@pytest.mark.asyncio
async def test_agento11y_query_exports_when_tool_never_completes() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def fake_query(*, options, **_kwargs) -> AsyncIterator[object]:
        pre_hook = options.hooks["PreToolUse"][0].hooks[0]
        await pre_hook(_pre_tool_use_input("toolu_open"), "toolu_open", {"signal": None})
        yield _success_result()

    try:
        _ = [
            message
            async for message in agento11y_query(
                prompt="Inspect the README.",
                client=client,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
                conversation_id="conv-42",
                _query_fn=fake_query,
            )
        ]

        client.flush()
        assert [len(request.generations) for request in exporter.requests] == [1]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_claude_agent_tool_failure_hook_does_not_drop_span(caplog) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        handler = Agento11yClaudeAgentHandler(client=client, conversation_id="conv-42")
        options = handler.instrument_options(ClaudeAgentOptions())
        pre_hook = options.hooks["PreToolUse"][0].hooks[0]
        failure_hook = options.hooks["PostToolUseFailure"][0].hooks[0]

        await pre_hook(_pre_tool_use_input("toolu_1"), "toolu_1", {"signal": None})
        with caplog.at_level(logging.ERROR):
            output = await failure_hook(
                _post_tool_use_failure_input("toolu_1", "disk full"),
                "toolu_1",
                {"signal": None},
            )

        assert output == {}
        assert caplog.records == []
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_claude_agent_tool_hooks_log_recorder_produced_error(caplog) -> None:
    recorder = _StubToolRecorder(final_error=RuntimeError("serialize tool result: unsupported type"))
    handler = Agento11yClaudeAgentHandler(client=_ToolRecorderClient(recorders=[recorder]))  # type: ignore[arg-type]
    options = handler.instrument_options(ClaudeAgentOptions())
    pre_hook = options.hooks["PreToolUse"][0].hooks[0]
    failure_hook = options.hooks["PostToolUseFailure"][0].hooks[0]

    await pre_hook(_pre_tool_use_input("toolu_1"), "toolu_1", {"signal": None})
    with caplog.at_level(logging.ERROR):
        output = await failure_hook(
            _post_tool_use_failure_input("toolu_1", "disk full"),
            "toolu_1",
            {"signal": None},
        )

    assert output == {}
    assert "toolu_1" in caplog.text
    assert "serialize tool result" in caplog.text


@pytest.mark.asyncio
async def test_claude_agent_guard_denial_returns_deny_decision() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    deny = HookEvaluateResponse(action="deny", rule_id="rule-1", reason="dangerous command")

    try:
        handler = Agento11yClaudeAgentHandler(
            client=_ToolRecorderClient(client, hook_response=deny),  # type: ignore[arg-type]
            enable_guards=True,
        )
        options = handler.instrument_options(ClaudeAgentOptions())
        pre_hook = options.hooks["PreToolUse"][0].hooks[0]

        output = await pre_hook(_pre_tool_use_input("toolu_1"), "toolu_1", {"signal": None})

        assert output["hookSpecificOutput"]["permissionDecision"] == "deny"
        assert output["hookSpecificOutput"]["permissionDecisionReason"] == "dangerous command"
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_claude_agent_guard_denial_survives_recorder_failure(caplog) -> None:
    recorder = _StubToolRecorder(end_error=RuntimeError("denied recorder end failed"))
    deny = HookEvaluateResponse(action="deny", rule_id="rule-1", reason="dangerous command")
    handler = Agento11yClaudeAgentHandler(
        client=_ToolRecorderClient(recorders=[recorder], hook_response=deny),  # type: ignore[arg-type]
    )
    options = handler.instrument_options(ClaudeAgentOptions())
    pre_hook = options.hooks["PreToolUse"][0].hooks[0]

    with caplog.at_level(logging.ERROR):
        output = await pre_hook(_pre_tool_use_input("toolu_1"), "toolu_1", {"signal": None})

    assert output["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert "denied recorder end failed" in caplog.text


@pytest.mark.asyncio
async def test_open_tool_teardown_continues_after_recorder_failure(caplog) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    first = _StubToolRecorder(end_error=RuntimeError("first recorder end failed"))
    second = _StubToolRecorder()

    try:
        handler = Agento11yClaudeAgentHandler(
            client=_ToolRecorderClient(client, recorders=[first, second]),  # type: ignore[arg-type]
            conversation_id="conv-42",
        )
        options = handler.instrument_options(ClaudeAgentOptions(model="claude-sonnet-4-5"))
        await handler.start(prompt="Inspect the README.", options=options)
        pre_hook = options.hooks["PreToolUse"][0].hooks[0]
        await pre_hook(_pre_tool_use_input("toolu_1"), "toolu_1", {"signal": None})
        await pre_hook(_pre_tool_use_input("toolu_2"), "toolu_2", {"signal": None})

        with caplog.at_level(logging.ERROR):
            handler.finish()

        assert second.ended is True
        client.flush()
        assert [len(request.generations) for request in exporter.requests] == [1]
        assert "first recorder end failed" in caplog.text
    finally:
        client.shutdown()


_PROMPT_WITH_EMAIL = "Email my order confirmation to jane.doe@example.com"
_REDACTED_PROMPT = "Email my order confirmation to [REDACTED]"


def _redacting_hook(_request) -> HookEvaluateResponse:
    return HookEvaluateResponse(
        action="allow",
        rule_id="redact_emails",
        transformed_input=HookInput(messages=[user_text_message(_REDACTED_PROMPT)]),
    )


def _user_prompt_submit_input(prompt: str) -> dict[str, object]:
    return {
        "hook_event_name": "UserPromptSubmit",
        "session_id": "session-42",
        "transcript_path": "",
        "cwd": "",
        "prompt": prompt,
    }


def _recorded_input_text(generation) -> list[str]:
    return [part.text for message in generation.input for part in message.parts]


# A UserPromptSubmit hook can't rewrite the prompt, so the model still gets the
# original. The recorded generation must not keep what the redact guard removed.
@pytest.mark.asyncio
async def test_agento11y_query_records_guard_redacted_prompt(monkeypatch) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    monkeypatch.setattr(client, "evaluate_hook", _redacting_hook)

    async def fake_query(*, options, **_kwargs) -> AsyncIterator[object]:
        hook = options.hooks["UserPromptSubmit"][0].hooks[0]
        await hook(_user_prompt_submit_input(_PROMPT_WITH_EMAIL), None, {"signal": None})
        yield UserMessage(content=_PROMPT_WITH_EMAIL)
        yield AssistantMessage(content=[TextBlock("Done.")], model="claude-sonnet-4-5", session_id="session-42")
        yield _success_result()

    try:
        _ = [
            message
            async for message in agento11y_query(
                prompt=_PROMPT_WITH_EMAIL,
                client=client,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
                conversation_id="conv-42",
                _query_fn=fake_query,
            )
        ]

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert _recorded_input_text(generation) == [_REDACTED_PROMPT]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_claude_agent_handler_redacts_prompt_recorded_from_stream(monkeypatch) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    monkeypatch.setattr(client, "evaluate_hook", _redacting_hook)

    async def streamed_prompt() -> AsyncIterator[dict[str, object]]:
        yield {"type": "user", "message": {"role": "user", "content": _PROMPT_WITH_EMAIL}}

    try:
        handler = Agento11yClaudeAgentHandler(client=client, conversation_id="conv-42")
        options = handler.instrument_options(ClaudeAgentOptions(model="claude-sonnet-4-5"))
        # A streamed prompt isn't a string, so start() records nothing and the
        # prompt arrives as a UserMessage after the hook ran.
        await handler.start(prompt=streamed_prompt(), options=options)
        hook = options.hooks["UserPromptSubmit"][0].hooks[0]
        await hook(_user_prompt_submit_input(_PROMPT_WITH_EMAIL), None, {"signal": None})
        handler.record_message(UserMessage(content=_PROMPT_WITH_EMAIL))
        handler.record_message(_success_result())

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert _recorded_input_text(generation) == [_REDACTED_PROMPT]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_claude_agent_guard_denial_records_redacted_prompt(monkeypatch) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    def deny_and_redact(_request) -> HookEvaluateResponse:
        return HookEvaluateResponse(
            action="deny",
            rule_id="redact_then_block",
            reason="blocked",
            transformed_input=HookInput(messages=[user_text_message(_REDACTED_PROMPT)]),
        )

    monkeypatch.setattr(client, "evaluate_hook", deny_and_redact)

    try:
        handler = Agento11yClaudeAgentHandler(client=client, conversation_id="conv-42")
        options = handler.instrument_options(ClaudeAgentOptions(model="claude-sonnet-4-5"))
        await handler.start(prompt=_PROMPT_WITH_EMAIL, options=options)
        hook = options.hooks["UserPromptSubmit"][0].hooks[0]
        output = await hook(_user_prompt_submit_input(_PROMPT_WITH_EMAIL), None, {"signal": None})
        handler.finish()

        assert output["continue_"] is False
        client.flush()
        generation = exporter.requests[0].generations[0]
        assert _recorded_input_text(generation) == [_REDACTED_PROMPT]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_claude_agent_handler_keeps_prompt_without_redaction(monkeypatch) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    monkeypatch.setattr(client, "evaluate_hook", lambda _request: HookEvaluateResponse(action="allow"))

    try:
        handler = Agento11yClaudeAgentHandler(client=client, conversation_id="conv-42")
        options = handler.instrument_options(ClaudeAgentOptions(model="claude-sonnet-4-5"))
        await handler.start(prompt=_PROMPT_WITH_EMAIL, options=options)
        hook = options.hooks["UserPromptSubmit"][0].hooks[0]
        await hook(_user_prompt_submit_input(_PROMPT_WITH_EMAIL), None, {"signal": None})
        handler.finish()

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert _recorded_input_text(generation) == [_PROMPT_WITH_EMAIL]
    finally:
        client.shutdown()


# Claude can replay the prompt split into several text blocks. Each block is
# redacted, and the message keeps every block.
@pytest.mark.asyncio
async def test_claude_agent_handler_redacts_multipart_replayed_prompt(monkeypatch) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    monkeypatch.setattr(client, "evaluate_hook", _redacting_hook)

    async def streamed_prompt() -> AsyncIterator[dict[str, object]]:
        yield {"type": "user", "message": {"role": "user", "content": _PROMPT_WITH_EMAIL}}

    try:
        handler = Agento11yClaudeAgentHandler(client=client, conversation_id="conv-42")
        options = handler.instrument_options(ClaudeAgentOptions(model="claude-sonnet-4-5"))
        await handler.start(prompt=streamed_prompt(), options=options)
        hook = options.hooks["UserPromptSubmit"][0].hooks[0]
        await hook(_user_prompt_submit_input(_PROMPT_WITH_EMAIL), None, {"signal": None})
        handler.record_message(
            UserMessage(content=[TextBlock("Email my order confirmation"), TextBlock("to jane.doe@example.com")])
        )
        handler.record_message(_success_result())

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert _recorded_input_text(generation) == ["Email my order confirmation", "to [REDACTED]"]
    finally:
        client.shutdown()


# A later run of the same session can replay an earlier prompt, and the
# redaction has to follow it. A new session on the same handler starts clean.
@pytest.mark.asyncio
async def test_claude_agent_handler_redacts_prompt_replayed_into_a_later_run(monkeypatch) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    monkeypatch.setattr(client, "evaluate_hook", _redacting_hook)

    async def streamed_prompt() -> AsyncIterator[dict[str, object]]:
        yield {"type": "user", "message": {"role": "user", "content": "Continue."}}

    async def run(options: ClaudeAgentOptions, session_id: str, *, prompt: object) -> None:
        options = handler.instrument_options(options)
        await handler.start(prompt=prompt, options=options)
        if prompt == _PROMPT_WITH_EMAIL:
            hook = options.hooks["UserPromptSubmit"][0].hooks[0]
            await hook(_user_prompt_submit_input(_PROMPT_WITH_EMAIL), None, {"signal": None})
        else:
            handler.record_message(UserMessage(content=_PROMPT_WITH_EMAIL))
        handler.record_message(
            AssistantMessage(content=[TextBlock("Done.")], model="claude-sonnet-4-5", session_id=session_id)
        )
        handler.finish()

    try:
        handler = Agento11yClaudeAgentHandler(client=client)
        await run(ClaudeAgentOptions(model="claude-sonnet-4-5"), "session-42", prompt=_PROMPT_WITH_EMAIL)
        await run(
            ClaudeAgentOptions(model="claude-sonnet-4-5", resume="session-42"), "session-42", prompt=streamed_prompt()
        )
        await run(ClaudeAgentOptions(model="claude-sonnet-4-5"), "session-99", prompt=streamed_prompt())

        client.flush()
        generations = [generation for request in exporter.requests for generation in request.generations]
        assert [_recorded_input_text(generation) for generation in generations] == [
            [_REDACTED_PROMPT],
            [_REDACTED_PROMPT],
            [_PROMPT_WITH_EMAIL],
        ]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_claude_agent_handler_records_prompt_redacted_to_nothing(monkeypatch) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    monkeypatch.setattr(
        client,
        "evaluate_hook",
        lambda _request: HookEvaluateResponse(
            action="allow",
            rule_id="redact_everything",
            transformed_input=HookInput(messages=[user_text_message("")]),
        ),
    )

    try:
        handler = Agento11yClaudeAgentHandler(client=client, conversation_id="conv-42")
        options = handler.instrument_options(ClaudeAgentOptions(model="claude-sonnet-4-5"))
        await handler.start(prompt=_PROMPT_WITH_EMAIL, options=options)
        hook = options.hooks["UserPromptSubmit"][0].hooks[0]
        await hook(_user_prompt_submit_input(_PROMPT_WITH_EMAIL), None, {"signal": None})
        handler.finish()

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert _recorded_input_text(generation) == ["[REDACTED]"]
    finally:
        client.shutdown()


# Agento11yClaudeSDKClient makes a new handler for every query, so the redaction
# has to outlive the handler for a later query that replays the prompt.
@pytest.mark.asyncio
async def test_agento11y_claude_sdk_client_redacts_prompt_replayed_into_a_later_query(monkeypatch) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    monkeypatch.setattr(client, "evaluate_hook", _redacting_hook)
    claude = _FakeClaudeSDKClient()
    claude.responses.extend(
        [
            [
                AssistantMessage(content=[TextBlock("Sent.")], model="claude-sonnet-4-5", session_id="session-42"),
                _success_result(),
            ],
            [
                UserMessage(content=_PROMPT_WITH_EMAIL),
                AssistantMessage(content=[TextBlock("Done.")], model="claude-sonnet-4-5", session_id="session-42"),
                _success_result(),
            ],
        ]
    )

    try:
        async with Agento11yClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            conversation_id="conv-client",
        ) as agento11y_claude:
            await agento11y_claude.query(_PROMPT_WITH_EMAIL)
            hook = agento11y_claude._options.hooks["UserPromptSubmit"][-1].hooks[0]
            await hook(_user_prompt_submit_input(_PROMPT_WITH_EMAIL), None, {"signal": None})
            _ = [message async for message in agento11y_claude.receive_response()]
            await agento11y_claude.query("Continue.")
            _ = [message async for message in agento11y_claude.receive_response()]

        client.flush()
        generations = [generation for request in exporter.requests for generation in request.generations]
        assert [_recorded_input_text(generation) for generation in generations] == [
            [_REDACTED_PROMPT],
            ["Continue.", _REDACTED_PROMPT],
        ]
    finally:
        client.shutdown()


# When the guard redacts the whole prompt, or Claude splits a value across text
# blocks, no single block holds what was removed. Every block it spans is
# redacted, and blocks it doesn't touch are kept.
@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("transformed", "blocks", "want"),
    [
        pytest.param(
            "",
            ["Email my order confirmation", "to jane.doe@example.com"],
            ["[REDACTED]", "[REDACTED]"],
            id="whole-prompt-redaction-replayed-in-blocks",
        ),
        pytest.param(
            _REDACTED_PROMPT,
            ["Email my order confirmation to jane.doe@", "example.com", "Thanks."],
            ["[REDACTED]", "[REDACTED]", "Thanks."],
            id="value-split-across-blocks",
        ),
    ],
)
async def test_claude_agent_handler_redacts_text_split_across_blocks(monkeypatch, transformed, blocks, want) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    monkeypatch.setattr(
        client,
        "evaluate_hook",
        lambda _request: HookEvaluateResponse(
            action="allow",
            rule_id="redact_emails",
            transformed_input=HookInput(messages=[user_text_message(transformed)]),
        ),
    )

    async def streamed_prompt() -> AsyncIterator[dict[str, object]]:
        yield {"type": "user", "message": {"role": "user", "content": _PROMPT_WITH_EMAIL}}

    try:
        handler = Agento11yClaudeAgentHandler(client=client, conversation_id="conv-42")
        options = handler.instrument_options(ClaudeAgentOptions(model="claude-sonnet-4-5"))
        await handler.start(prompt=streamed_prompt(), options=options)
        hook = options.hooks["UserPromptSubmit"][0].hooks[0]
        await hook(_user_prompt_submit_input(_PROMPT_WITH_EMAIL), None, {"signal": None})
        handler.record_message(UserMessage(content=[TextBlock(block) for block in blocks]))
        handler.record_message(_success_result())

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert _recorded_input_text(generation) == want
    finally:
        client.shutdown()
