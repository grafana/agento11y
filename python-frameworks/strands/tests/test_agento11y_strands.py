"""Strands hook provider lifecycle tests."""

from __future__ import annotations

from datetime import timedelta
from types import SimpleNamespace

from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.models import ExportGenerationResult, ExportGenerationsResponse, MessageRole, PartKind
from agento11y_strands import (
    Agento11yStrandsHookProvider,
    create_agento11y_strands_hook_provider,
    with_agento11y_strands_hooks,
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
        )
    )


class _Model:
    def get_config(self):
        return {
            "model": "gpt-5",
            "provider": "openai",
            "temperature": 0.2,
            "max_tokens": 256,
        }


class _ToolRegistry:
    def get_all_tools_config(self):
        return {
            "lookup": {
                "toolSpec": {
                    "name": "lookup",
                    "description": "Look up a value.",
                    "inputSchema": {"json": {"type": "object"}},
                }
            }
        }


def _agent():
    return SimpleNamespace(
        name="strands-agent",
        agent_id="agent-42",
        model=_Model(),
        system_prompt="Be brief.",
        messages=[{"role": "user", "content": [{"text": "hello"}]}],
        tool_registry=_ToolRegistry(),
    )


def test_strands_model_lifecycle_exports_generation_with_framework_metadata() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        invocation_state = {"conversation_id": "conv-42", "thread_id": "thread-42"}
        agent = _agent()

        hooks.before_invocation(SimpleNamespace(agent=agent, invocation_state=invocation_state))
        hooks.before_model_call(SimpleNamespace(agent=agent, invocation_state=invocation_state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=agent,
                invocation_state=invocation_state,
                stop_response=SimpleNamespace(
                    message={
                        "role": "assistant",
                        "content": [{"text": "world"}],
                        "metadata": {
                            "usage": {
                                "inputTokens": 10,
                                "outputTokens": 5,
                                "totalTokens": 15,
                            }
                        },
                    },
                    stop_reason="end_turn",
                ),
                exception=None,
            )
        )
        hooks.after_invocation(SimpleNamespace(agent=agent, invocation_state=invocation_state))

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "STREAM"
        assert generation.model.provider == "openai"
        assert generation.model.name == "gpt-5"
        assert generation.tags["agento11y.framework.name"] == "strands"
        assert generation.tags["agento11y.framework.source"] == "hooks"
        assert generation.tags["agento11y.framework.language"] == "python"
        assert generation.conversation_id == "conv-42"
        assert generation.agent_name == "strands-agent"
        assert generation.metadata["agento11y.framework.thread_id"] == "thread-42"
        assert generation.metadata["agento11y.framework.component_name"] == "strands-agent"
        assert generation.metadata["agento11y.framework.run_type"] == "chat"
        assert generation.output[0].parts[0].text == "world"
        assert generation.usage.input_tokens == 10
        assert generation.usage.output_tokens == 5
        assert generation.usage.total_tokens == 15
        assert generation.stop_reason == "end_turn"
        assert generation.system_prompt == "Be brief."
        assert generation.temperature == 0.2
        assert generation.max_tokens == 256
        assert generation.tools[0].name == "lookup"
    finally:
        client.shutdown()


def test_strands_tool_use_turn_exports_tool_call_output_and_agent_name() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        invocation_state = {"conversation_id": "conv-tool-use"}
        agent = _agent()

        hooks.before_invocation(SimpleNamespace(agent=agent, invocation_state=invocation_state))
        hooks.before_model_call(SimpleNamespace(agent=agent, invocation_state=invocation_state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=agent,
                invocation_state=invocation_state,
                stop_response=SimpleNamespace(
                    message={
                        "role": "assistant",
                        "content": [
                            {
                                "toolUse": {
                                    "toolUseId": "toolu_1",
                                    "name": "lookup",
                                    "input": {"query": "hello"},
                                }
                            }
                        ],
                        "metadata": {
                            "usage": {
                                "inputTokens": 10,
                                "outputTokens": 5,
                                "totalTokens": 15,
                            }
                        },
                    },
                    stop_reason="tool_use",
                ),
                exception=None,
            )
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.agent_name == "strands-agent"
        assert generation.stop_reason == "tool_use"
        assert len(generation.output) == 1
        tool_call_part = generation.output[0].parts[0]
        assert tool_call_part.kind == PartKind.TOOL_CALL
        assert tool_call_part.tool_call.id == "toolu_1"
        assert tool_call_part.tool_call.name == "lookup"
        assert tool_call_part.tool_call.input_json == b'{"query": "hello"}'
    finally:
        client.shutdown()


def test_strands_followup_generation_input_includes_tool_history() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        invocation_state = {"conversation_id": "conv-tool-result"}
        agent = _agent()
        agent.messages = [
            {"role": "user", "content": [{"text": "look this up"}]},
            {
                "role": "assistant",
                "content": [
                    {
                        "toolUse": {
                            "toolUseId": "toolu_1",
                            "name": "lookup",
                            "input": {"query": "hello"},
                        }
                    }
                ],
            },
            {
                "role": "user",
                "content": [
                    {
                        "toolResult": {
                            "toolUseId": "toolu_1",
                            "status": "success",
                            "content": [{"text": "world"}],
                        }
                    }
                ],
            },
        ]

        hooks.before_model_call(SimpleNamespace(agent=agent, invocation_state=invocation_state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=agent,
                invocation_state=invocation_state,
                stop_response=SimpleNamespace(
                    message={"role": "assistant", "content": [{"text": "done"}]},
                    stop_reason="end_turn",
                ),
                exception=None,
            )
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert [message.role for message in generation.input] == [
            MessageRole.USER,
            MessageRole.ASSISTANT,
            MessageRole.TOOL,
        ]
        assert generation.input[1].parts[0].kind == PartKind.TOOL_CALL
        assert generation.input[1].parts[0].tool_call.name == "lookup"
        assert generation.input[2].parts[0].kind == PartKind.TOOL_RESULT
        assert generation.input[2].parts[0].tool_result.tool_call_id == "toolu_1"
        assert generation.input[2].parts[0].tool_result.content == "world"
    finally:
        client.shutdown()


def test_strands_model_error_ends_generation_without_exporting_result() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        hooks = create_agento11y_strands_hook_provider(client=client)
        invocation_state = {"conversation_id": "conv-error"}
        agent = _agent()

        hooks.before_model_call(SimpleNamespace(agent=agent, invocation_state=invocation_state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=agent, invocation_state=invocation_state, stop_response=None, exception=RuntimeError("nope")
            )
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.conversation_id == "conv-error"
        assert generation.call_error == "nope"
        assert generation.output == []
    finally:
        client.shutdown()


def test_with_agento11y_strands_hooks_adds_provider_to_config_once() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        config = with_agento11y_strands_hooks({"hooks": []}, client=client)
        config = with_agento11y_strands_hooks(config, client=client)

        hooks = config["hooks"]
        assert len(hooks) == 1
        assert isinstance(hooks[0], Agento11yStrandsHookProvider)
    finally:
        client.shutdown()


def test_strands_cache_token_usage_preserved() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        invocation_state = {"conversation_id": "conv-cache"}
        agent = _agent()

        hooks.before_model_call(SimpleNamespace(agent=agent, invocation_state=invocation_state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=agent,
                invocation_state=invocation_state,
                stop_response=SimpleNamespace(
                    message={
                        "role": "assistant",
                        "content": [{"text": "cached response"}],
                        "metadata": {
                            "usage": {
                                "inputTokens": 100,
                                "outputTokens": 20,
                                "totalTokens": 120,
                                "cacheReadInputTokens": 80,
                                "cacheWriteInputTokens": 10,
                            }
                        },
                    },
                    stop_reason="end_turn",
                ),
                exception=None,
            )
        )

        client.flush()
        generation = exporter.requests[0].generations[0]
        assert generation.usage.input_tokens == 100
        assert generation.usage.output_tokens == 20
        assert generation.usage.total_tokens == 120
        assert generation.usage.cache_read_input_tokens == 80
        assert generation.usage.cache_write_input_tokens == 10
    finally:
        client.shutdown()
