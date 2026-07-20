"""Claude Agent SDK Sigil integration tests."""

from __future__ import annotations

from collections.abc import AsyncIterator
from datetime import timedelta

import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig, HookEvaluateResponse
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse, PartKind
from agento11y_claude_agent import (
    SigilClaudeAgentHandler,
    SigilClaudeSDKClient,
    create_sigil_claude_agent_handler,
    sigil_query,
    with_sigil_claude_agent_options,
)
from claude_agent_sdk import AssistantMessage, ClaudeAgentOptions, ResultMessage, TextBlock, ToolUseBlock, UserMessage


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


@pytest.mark.asyncio
async def test_sigil_query_records_claude_agent_stream() -> None:
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
            async for message in sigil_query(
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
        assert generation.tags["sigil.framework.name"] == "claude-agent-sdk"
        assert generation.tags["sigil.framework.source"] == "hooks"
        assert generation.metadata["sigil.framework.session_id"] == "session-42"
        assert generation.metadata["sigil.claude_agent.total_cost_usd"] == 0.01
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
async def test_sigil_query_early_stream_exit_finishes_without_error() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def fake_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("Partial.")], model="claude-sonnet-4-5", session_id="session-42")
        yield AssistantMessage(content=[TextBlock("Unread.")], model="claude-sonnet-4-5", session_id="session-42")

    try:
        stream = sigil_query(
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
        async with SigilClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            conversation_id="conv-client",
            agent_name="claude-agent",
        ) as sigil_claude:
            await sigil_claude.query("Inspect the README.")
            stream = sigil_claude.receive_response()
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
    handler = SigilClaudeAgentHandler(client=flaky_client, conversation_id="conv-42")  # type: ignore[arg-type]
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

    class _FlakySigilClient:
        def __init__(self) -> None:
            self.calls = 0
            self.recorder = _Recorder()

        def start_streaming_generation(self, _start):
            self.calls += 1
            if self.calls == 1:
                raise RuntimeError("start failed")
            return self.recorder

    sigil_client = _FlakySigilClient()
    claude = _FakeClaudeSDKClient()
    claude.responses.append(
        [AssistantMessage(content=[TextBlock("Recovered.")], model="claude-sonnet-4-5"), _success_result()]
    )

    sigil_claude = SigilClaudeSDKClient(
        client=sigil_client,  # type: ignore[arg-type]
        _claude_client=claude,  # type: ignore[arg-type]
        options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
        conversation_id="conv-client",
        agent_name="claude-agent",
    )

    with pytest.raises(RuntimeError, match="start failed"):
        await sigil_claude.query("First prompt.")

    await sigil_claude.query("Retry prompt.")
    _ = [message async for message in sigil_claude.receive_response()]

    assert sigil_client.calls == 2
    assert claude.queries == [("Retry prompt.", "default")]
    assert sigil_client.recorder.generation.input[0].parts[0].text == "Retry prompt."
    assert sigil_client.recorder.generation.output[0].parts[0].text == "Recovered."


@pytest.mark.asyncio
async def test_sigil_query_reuses_finished_handler_for_new_generation() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    handler = create_sigil_claude_agent_handler(client=client, conversation_id="conv-42", agent_name="claude-agent")

    async def first_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("First.")], model="claude-sonnet-4-5", session_id="session-1")
        yield _success_result("session-1")

    async def second_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("Second.")], model="claude-sonnet-4-5", session_id="session-2")
        yield _success_result("session-2")

    try:
        _ = [
            message
            async for message in sigil_query(
                prompt="First prompt.",
                client=client,
                handler=handler,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
                _query_fn=first_query,
            )
        ]
        _ = [
            message
            async for message in sigil_query(
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
        assert [generation.metadata["sigil.framework.session_id"] for generation in generations] == [
            "session-1",
            "session-2",
        ]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_reused_handler_resolves_conversation_id_per_run() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    handler = create_sigil_claude_agent_handler(client=client, agent_name="claude-agent")

    async def first_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("First.")], model="claude-sonnet-4-5", session_id="session-1")
        yield _success_result("session-1")

    async def second_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("Second.")], model="claude-sonnet-4-5", session_id="session-2")
        yield _success_result("session-2")

    try:
        _ = [
            message
            async for message in sigil_query(
                prompt="First prompt.",
                client=client,
                handler=handler,
                options=ClaudeAgentOptions(model="claude-sonnet-4-5", session_id="conversation-one"),
                _query_fn=first_query,
            )
        ]
        _ = [
            message
            async for message in sigil_query(
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
    handler = create_sigil_claude_agent_handler(client=client, conversation_id="conv-42", agent_name="claude-agent")

    async def first_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("First.")], model="claude-model-a", session_id="session-1")
        yield _success_result("session-1")

    async def second_query(**_kwargs) -> AsyncIterator[object]:
        yield AssistantMessage(content=[TextBlock("Second.")], model="claude-model-b", session_id="session-2")
        yield _success_result("session-2")

    try:
        _ = [
            message
            async for message in sigil_query(
                prompt="First prompt.",
                client=client,
                handler=handler,
                options=ClaudeAgentOptions(model="claude-model-a"),
                _query_fn=first_query,
            )
        ]
        _ = [
            message
            async for message in sigil_query(
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
async def test_sigil_query_skips_replayed_initial_user_message() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    async def fake_query(**_kwargs) -> AsyncIterator[object]:
        yield UserMessage(content="Inspect the README.")
        yield AssistantMessage(content=[TextBlock("Done.")], model="claude-sonnet-4-5", session_id="session-42")
        yield _success_result()

    try:
        seen = [
            message
            async for message in sigil_query(
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
async def test_sigil_claude_sdk_client_records_receive_response_stream() -> None:
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
        async with SigilClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5", permission_mode="default"),
            conversation_id="conv-client",
            agent_name="claude-agent",
        ) as sigil_claude:
            await sigil_claude.query("Inspect the README.", session_id="work")
            seen = [message async for message in sigil_claude.receive_response()]

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
        assert generation.metadata["sigil.framework.session_id"] == "session-42"
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_sigil_claude_sdk_client_records_multiple_queries() -> None:
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
        async with SigilClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            conversation_id="conv-client",
            agent_name="claude-agent",
        ) as sigil_claude:
            await sigil_claude.query("First prompt.")
            _ = [message async for message in sigil_claude.receive_response()]
            await sigil_claude.query("Second prompt.")
            _ = [message async for message in sigil_claude.receive_messages()]

        client.flush()
        generations = exporter.requests[0].generations
        assert [generation.output[0].parts[0].text for generation in generations] == ["First.", "Second."]
        assert [generation.metadata["sigil.framework.session_id"] for generation in generations] == ["s1", "s2"]
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_sigil_claude_sdk_client_rejects_overlapping_queries() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    claude = _FakeClaudeSDKClient()

    try:
        async with SigilClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            conversation_id="conv-client",
            agent_name="claude-agent",
        ) as sigil_claude:
            await sigil_claude.query("First prompt.")
            with pytest.raises(RuntimeError, match="previous response stream finishes"):
                await sigil_claude.query("Second prompt.")
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_sigil_claude_sdk_client_uses_stable_fallback_conversation() -> None:
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
        async with SigilClaudeSDKClient(
            client=client,
            _claude_client=claude,  # type: ignore[arg-type]
            options=ClaudeAgentOptions(model="claude-sonnet-4-5"),
            agent_name="claude-agent",
        ) as sigil_claude:
            await sigil_claude.query("First prompt.")
            _ = [message async for message in sigil_claude.receive_response()]
            await sigil_claude.query("Second prompt.")
            _ = [message async for message in sigil_claude.receive_response()]

        client.flush()
        generations = exporter.requests[0].generations
        assert len({generation.conversation_id for generation in generations}) == 1
        assert generations[0].conversation_id.startswith("sigil:framework:claude-agent-sdk:client:")
    finally:
        client.shutdown()


@pytest.mark.asyncio
async def test_sigil_claude_sdk_client_passthrough_methods() -> None:
    claude = _FakeClaudeSDKClient()
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        sigil_claude = SigilClaudeSDKClient(client=client, _claude_client=claude)  # type: ignore[arg-type]
        await sigil_claude.set_permission_mode("acceptEdits")
        await sigil_claude.rewind_files("checkpoint-1")
        await sigil_claude.interrupt()
        await sigil_claude.disconnect()

        assert claude.permission_modes == ["acceptEdits"]
        assert claude.rewind_ids == ["checkpoint-1"]
        assert claude.interrupted is True
        assert claude.disconnected is True
    finally:
        client.shutdown()


def test_with_sigil_claude_agent_options_appends_hooks_once() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        handler = create_sigil_claude_agent_handler(client=client)
        options = with_sigil_claude_agent_options(ClaudeAgentOptions(), client=client, handler=handler)
        options = with_sigil_claude_agent_options(options, client=client, handler=handler)

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

    handler = SigilClaudeAgentHandler(client=_GuardClient())  # type: ignore[arg-type]
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
    class _ToolRecorder:
        def __init__(self) -> None:
            self.result = None
            self.ended = False

        def set_result(self, **payload) -> None:
            self.result = payload

        def set_exec_error(self, error) -> None:
            self.result = {"error": str(error)}

        def end(self) -> None:
            self.ended = True

        def err(self):
            return None

    class _ToolClient:
        def __init__(self) -> None:
            self.recorder = _ToolRecorder()

        def start_tool_execution(self, _start):
            return self.recorder

        def evaluate_hook(self, _request):
            return HookEvaluateResponse(action="allow")

    tool_client = _ToolClient()
    handler = SigilClaudeAgentHandler(client=tool_client)  # type: ignore[arg-type]
    options = handler.instrument_options(ClaudeAgentOptions())

    pre_hook = options.hooks["PreToolUse"][0].hooks[0]
    post_hook = options.hooks["PostToolUse"][0].hooks[0]
    await pre_hook(
        {
            "hook_event_name": "PreToolUse",
            "session_id": "session-42",
            "transcript_path": "",
            "cwd": "",
            "tool_name": "Bash",
            "tool_input": {"command": "pwd"},
            "tool_use_id": "toolu_1",
        },
        "toolu_1",
        {"signal": None},
    )
    await post_hook(
        {
            "hook_event_name": "PostToolUse",
            "session_id": "session-42",
            "transcript_path": "",
            "cwd": "",
            "tool_name": "Bash",
            "tool_input": {"command": "pwd"},
            "tool_response": {"stdout": "/tmp\n"},
            "tool_use_id": "toolu_1",
        },
        "toolu_1",
        {"signal": None},
    )

    assert tool_client.recorder.result == {
        "arguments": {"command": "pwd"},
        "result": {"stdout": "/tmp\n"},
    }
    assert tool_client.recorder.ended is True
