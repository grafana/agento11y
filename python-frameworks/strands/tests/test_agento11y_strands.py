"""Strands hook provider lifecycle tests."""

from __future__ import annotations

import contextvars
import json
from datetime import timedelta
from types import SimpleNamespace

import agento11y_strands
import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.models import (
    ExportGenerationResult,
    ExportGenerationsResponse,
    ExportWorkflowStepResult,
    ExportWorkflowStepsResponse,
    MessageRole,
    PartKind,
)
from agento11y_strands import (
    Agento11yStrandsHookProvider,
    create_agento11y_strands_hook_provider,
    with_agento11y_strands_hooks,
)
from strands.models import Model


class _CapturingExporter:
    def __init__(self) -> None:
        self.requests = []
        self.workflow_requests = []

    def export_generations(self, request):
        self.requests.append(request)
        return ExportGenerationsResponse(
            results=[
                ExportGenerationResult(generation_id=generation.id, accepted=True) for generation in request.generations
            ]
        )

    def export_workflow_steps(self, request):
        self.workflow_requests.append(request)
        return ExportWorkflowStepsResponse(
            results=[ExportWorkflowStepResult(step_id=step.id, accepted=True) for step in request.workflow_steps]
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


class _NestedParamsModel:
    def __init__(self, **params) -> None:
        self._params = params

    def get_config(self):
        return {"model_id": "gpt-4o", "params": self._params}


class _FlatConfigModel:
    def __init__(self, **config) -> None:
        self._config = config

    def get_config(self):
        return {"model_id": "mistral-large-latest", **self._config}


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


def _agent(name: str = "strands-agent"):
    return SimpleNamespace(
        name=name,
        agent_id="agent-42",
        model=_Model(),
        system_prompt="Be brief.",
        messages=[{"role": "user", "content": [{"text": "hello"}]}],
        tool_registry=_ToolRegistry(),
    )


def _record_one_model_call(client, agent, *, message=None) -> None:
    hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
    invocation_state = {"conversation_id": "conv-mode"}
    hooks.before_model_call(SimpleNamespace(agent=agent, invocation_state=invocation_state))
    hooks.after_model_call(
        SimpleNamespace(
            agent=agent,
            invocation_state=invocation_state,
            stop_response=SimpleNamespace(
                message=message if message is not None else {"role": "assistant", "content": [{"text": "ok"}]},
                stop_reason="end_turn",
            ),
            exception=None,
        )
    )
    client.flush()


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


def test_strands_multi_agent_generations_have_parent_ids_and_workflow_links() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        state = {"conversation_id": "conv-lineage"}
        source = object()
        first_agent = _agent("one")
        second_agent = _agent("two")

        hooks.before_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))
        hooks.before_node_call(SimpleNamespace(source=source, node_id="one", invocation_state=state))
        hooks.before_model_call(SimpleNamespace(agent=first_agent, invocation_state=state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=first_agent,
                invocation_state=state,
                stop_response=SimpleNamespace(message={"role": "assistant", "content": [{"text": "one"}]}),
                exception=None,
            )
        )
        hooks.after_node_call(SimpleNamespace(source=source, node_id="one", invocation_state=state))

        hooks.before_node_call(SimpleNamespace(source=source, node_id="two", invocation_state=state))
        hooks.before_model_call(SimpleNamespace(agent=second_agent, invocation_state=state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=second_agent,
                invocation_state=state,
                stop_response=SimpleNamespace(message={"role": "assistant", "content": [{"text": "two"}]}),
                exception=None,
            )
        )
        hooks.after_node_call(SimpleNamespace(source=source, node_id="two", invocation_state=state))
        hooks.after_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))

        client.flush()
        generations = [generation for request in exporter.requests for generation in request.generations]
        assert len(generations) == 2
        assert generations[0].agent_name == "one"
        assert generations[1].agent_name == "two"
        assert generations[0].parent_generation_ids == []
        assert generations[1].parent_generation_ids == [generations[0].id]
        steps = [step for request in exporter.workflow_requests for step in request.workflow_steps]
        assert steps[0].linked_generation_ids == [generations[0].id]
        assert steps[1].linked_generation_ids == [generations[1].id]
        assert steps[1].parent_step_ids == [steps[0].id]
    finally:
        client.shutdown()


def test_strands_parallel_nodes_keep_model_runs_and_workflow_links_isolated() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        state = {"conversation_id": "conv-parallel"}
        first_agent = _agent("first")
        second_agent = _agent("second")
        source = SimpleNamespace(
            name="parallel-graph",
            nodes={
                "first": SimpleNamespace(executor=first_agent),
                "second": SimpleNamespace(executor=second_agent),
            },
        )

        hooks.before_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))
        hooks.before_node_call(SimpleNamespace(source=source, node_id="first", invocation_state=state))
        hooks.before_node_call(SimpleNamespace(source=source, node_id="second", invocation_state=state))
        hooks.before_model_call(SimpleNamespace(agent=first_agent, invocation_state=state))
        hooks.before_model_call(SimpleNamespace(agent=second_agent, invocation_state=state))

        # Complete the calls in start order. A shared LIFO stack would swap
        # these results, which is possible when graph nodes run concurrently.
        for agent, text in ((first_agent, "first-result"), (second_agent, "second-result")):
            hooks.after_model_call(
                SimpleNamespace(
                    agent=agent,
                    invocation_state=state,
                    stop_response=SimpleNamespace(
                        message={"role": "assistant", "content": [{"text": text}]},
                        stop_reason="end_turn",
                    ),
                    exception=None,
                )
            )

        hooks.after_node_call(SimpleNamespace(source=source, node_id="first", invocation_state=state))
        hooks.after_node_call(SimpleNamespace(source=source, node_id="second", invocation_state=state))
        hooks.after_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))
        client.flush()

        generations = [generation for request in exporter.requests for generation in request.generations]
        output_by_agent = {generation.agent_name: generation.output[0].parts[0].text for generation in generations}
        assert output_by_agent == {"first": "first-result", "second": "second-result"}
        assert all(generation.parent_generation_ids == [] for generation in generations)

        steps = [step for request in exporter.workflow_requests for step in request.workflow_steps]
        generation_id_by_agent = {generation.agent_name: generation.id for generation in generations}
        assert {step.step_name: step.linked_generation_ids for step in steps} == {
            "first": [generation_id_by_agent["first"]],
            "second": [generation_id_by_agent["second"]],
        }
    finally:
        client.shutdown()


def test_strands_nested_multi_agent_inherits_fallback_conversation_and_exports_node_step() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        state = {}
        outer_agent = _agent("outer")
        node_agent = _agent("node")
        source = SimpleNamespace(
            name="nested-graph",
            nodes={"node": SimpleNamespace(executor=node_agent)},
        )

        hooks.before_invocation(SimpleNamespace(agent=outer_agent, invocation_state=state))
        hooks.before_model_call(SimpleNamespace(agent=outer_agent, invocation_state=state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=outer_agent,
                invocation_state=state,
                stop_response=SimpleNamespace(
                    message={"role": "assistant", "content": [{"text": "delegating"}]},
                    stop_reason="end_turn",
                ),
                exception=None,
            )
        )
        hooks.before_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))
        hooks.before_node_call(SimpleNamespace(source=source, node_id="node", invocation_state=state))
        hooks.before_model_call(SimpleNamespace(agent=node_agent, invocation_state=state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=node_agent,
                invocation_state=state,
                stop_response=SimpleNamespace(
                    message={"role": "assistant", "content": [{"text": "done"}]},
                    stop_reason="end_turn",
                ),
                exception=None,
            )
        )
        hooks.after_node_call(SimpleNamespace(source=source, node_id="node", invocation_state=state))
        hooks.after_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))
        hooks.after_invocation(SimpleNamespace(agent=outer_agent, invocation_state=state))
        client.flush()

        generations = [generation for request in exporter.requests for generation in request.generations]
        outer_generation = next(generation for generation in generations if generation.agent_name == "outer")
        node_generation = next(generation for generation in generations if generation.agent_name == "node")
        step = exporter.workflow_requests[0].workflow_steps[0]
        assert step.step_name == "node"
        assert step.linked_generation_ids == [node_generation.id]
        assert step.conversation_id == node_generation.conversation_id == outer_generation.conversation_id
    finally:
        client.shutdown()


class _ScriptedModel(Model):
    """Answer with the given turns in order: "tools:<a>,<b>" calls those tools, anything else is text."""

    def __init__(self, *turns: str) -> None:
        self._turns = list(turns)

    def update_config(self, **_config) -> None:
        return

    def get_config(self):
        return {"model_id": "scripted", "provider": "custom"}

    async def structured_output(self, *_args, **_kwargs):
        raise NotImplementedError
        yield

    async def stream(self, *_args, **_kwargs):
        turn = self._turns.pop(0)
        yield {"messageStart": {"role": "assistant"}}
        if turn.startswith("tools:"):
            for name in turn.removeprefix("tools:").split(","):
                yield {"contentBlockStart": {"start": {"toolUse": {"name": name, "toolUseId": f"call-{name}"}}}}
                yield {"contentBlockDelta": {"delta": {"toolUse": {"input": json.dumps({"input": "hi"})}}}}
                yield {"contentBlockStop": {}}
            yield {"messageStop": {"stopReason": "tool_use"}}
            return
        yield {"contentBlockStart": {"start": {}}}
        yield {"contentBlockDelta": {"delta": {"text": turn}}}
        yield {"contentBlockStop": {}}
        yield {"messageStop": {"stopReason": "end_turn"}}


@pytest.fixture(autouse=True)
def _no_tool_caller_between_tests():
    # Some tests end a tool in another context on purpose, which leaves a caller set in this one.
    token = agento11y_strands._tool_callers.set(None)
    yield
    agento11y_strands._tool_callers.reset(token)


def _model_call(hooks, agent, state, text) -> None:
    hooks.before_model_call(SimpleNamespace(agent=agent, invocation_state=state))
    hooks.after_model_call(
        SimpleNamespace(
            agent=agent,
            invocation_state=state,
            stop_response=SimpleNamespace(
                message={"role": "assistant", "content": [{"text": text}]}, stop_reason="end_turn"
            ),
            exception=None,
        )
    )


def _tool_event(agent, state, **fields):
    return SimpleNamespace(
        agent=agent,
        invocation_state=state,
        tool_use={"toolUseId": "call-1", "name": "ask_sub_agent", "input": {}},
        selected_tool=None,
        **fields,
    )


def _tool_ended(agent, state):
    return _tool_event(agent, state, result={"status": "success"}, exception=None)


def _generations_by_text(exporter: _CapturingExporter) -> dict:
    generations = [generation for request in exporter.requests for generation in request.generations]
    return {generation.output[0].parts[0].text: generation for generation in generations}


def _assert_no_tool_call_state_left(hooks) -> None:
    assert hooks._agento11y_handler._graph_root_run_keys == set()
    assert hooks._agento11y_handler._tool_invocation_last_generation_id == {}
    assert hooks._tool_callers_before == {}


def _assert_no_state_left(hooks) -> None:
    # Graph tests use _assert_no_tool_call_state_left: the links of node generations stay in
    # _run_to_graph_key after the node ends.
    assert hooks._agento11y_handler._run_to_graph_key == {}
    _assert_no_tool_call_state_left(hooks)


# The caller's conversation wins over a key of the invoked agent, as it does for the nodes of a graph.
@pytest.mark.parametrize("sub_state", [{}, {"session_id": "session-2"}])
def test_strands_agent_called_from_a_tool_runs_below_the_caller(sub_state: dict) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        outer_state = {"session_id": "session-1"}
        outer_agent = _agent("outer")
        sub_agent = _agent("sub")

        hooks.before_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        _model_call(hooks, outer_agent, outer_state, "delegating")
        hooks.before_tool_call(_tool_event(outer_agent, outer_state))
        # Strands starts the sub-agent as a new invocation with its own state.
        hooks.before_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        _model_call(hooks, sub_agent, sub_state, "done")
        _model_call(hooks, sub_agent, sub_state, "done again")
        hooks.after_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        hooks.after_tool_call(_tool_ended(outer_agent, outer_state))
        _model_call(hooks, outer_agent, outer_state, "final")
        hooks.after_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        client.flush()
    finally:
        client.shutdown()

    generations = _generations_by_text(exporter)
    assert {text: generation.conversation_id for text, generation in generations.items()} == {
        "delegating": "session-1",
        "done": "session-1",
        "done again": "session-1",
        "final": "session-1",
    }
    assert generations["done"].parent_generation_ids == [generations["delegating"].id]
    assert generations["done again"].parent_generation_ids == [generations["done"].id]
    assert generations["final"].parent_generation_ids == [generations["delegating"].id]
    assert exporter.workflow_requests == []
    _assert_no_state_left(hooks)


def test_strands_node_agent_tool_invokes_an_agent_below_the_node() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        state = {"conversation_id": "graph-1"}
        sub_state = {}
        node_agent = _agent("node")
        sub_agent = _agent("sub")
        source = SimpleNamespace(name="graph", nodes={"node": SimpleNamespace(executor=node_agent)})

        hooks.before_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))
        hooks.before_node_call(SimpleNamespace(source=source, node_id="node", invocation_state=state))
        _model_call(hooks, node_agent, state, "delegating")
        hooks.before_tool_call(_tool_event(node_agent, state))
        hooks.before_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        _model_call(hooks, sub_agent, sub_state, "done")
        hooks.after_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        hooks.after_tool_call(_tool_ended(node_agent, state))
        _model_call(hooks, node_agent, state, "final")
        hooks.after_node_call(SimpleNamespace(source=source, node_id="node", invocation_state=state))
        hooks.after_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))
        client.flush()
    finally:
        client.shutdown()

    generations = _generations_by_text(exporter)
    step = exporter.workflow_requests[0].workflow_steps[0]
    assert step.step_name == "node"
    assert step.linked_generation_ids == [generations[text].id for text in ("delegating", "done", "final")]
    assert {generation.conversation_id for generation in generations.values()} == {"graph-1"}
    assert generations["done"].parent_generation_ids == [generations["delegating"].id]
    assert generations["final"].parent_generation_ids == [generations["delegating"].id]
    _assert_no_tool_call_state_left(hooks)


def test_strands_multi_agent_invoked_from_a_tool_joins_the_caller_conversation() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        outer_state = {}
        swarm_state = {}
        outer_agent = _agent("outer")
        node_agent = _agent("node")
        source = SimpleNamespace(name="swarm", nodes={"node": SimpleNamespace(executor=node_agent)})

        hooks.before_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        _model_call(hooks, outer_agent, outer_state, "delegating")
        hooks.before_tool_call(_tool_event(outer_agent, outer_state))
        # The tool starts the swarm with a state of its own, so the state does not link it to the caller.
        hooks.before_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=swarm_state))
        hooks.before_node_call(SimpleNamespace(source=source, node_id="node", invocation_state=swarm_state))
        # The node agent runs in the tool's context, but it belongs to its node, so it is not
        # recorded as an agent that the tool invoked.
        hooks.before_invocation(SimpleNamespace(agent=node_agent, invocation_state=swarm_state))
        assert hooks._agento11y_handler._tool_invocation_last_generation_id == {}
        _model_call(hooks, node_agent, swarm_state, "done")
        hooks.after_invocation(SimpleNamespace(agent=node_agent, invocation_state=swarm_state))
        hooks.after_node_call(SimpleNamespace(source=source, node_id="node", invocation_state=swarm_state))
        hooks.after_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=swarm_state))
        hooks.after_tool_call(_tool_ended(outer_agent, outer_state))
        hooks.after_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        client.flush()
    finally:
        client.shutdown()

    generations = _generations_by_text(exporter)
    step = exporter.workflow_requests[0].workflow_steps[0]
    assert step.step_name == "node"
    assert step.linked_generation_ids == [generations["done"].id]
    assert step.conversation_id == generations["done"].conversation_id == generations["delegating"].conversation_id
    _assert_no_tool_call_state_left(hooks)


def test_strands_agent_called_from_a_tool_drops_its_workflow_step_that_never_ended(caplog) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        outer_state = {}
        sub_state = {}
        graph_state = {}
        outer_agent = _agent("outer")
        sub_agent = _agent("sub")
        node_agent = _agent("node")
        source = SimpleNamespace(name="graph", nodes={"node": SimpleNamespace(executor=node_agent)})

        hooks.before_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        _model_call(hooks, outer_agent, outer_state, "delegating")
        hooks.before_tool_call(_tool_event(outer_agent, outer_state))
        hooks.before_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        _model_call(hooks, sub_agent, sub_state, "sub calls a tool")
        hooks.before_tool_call(_tool_event(sub_agent, sub_state))
        # The sub-agent's tool starts a graph whose node never gets AfterNodeCallEvent.
        hooks.before_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=graph_state))
        hooks.before_node_call(SimpleNamespace(source=source, node_id="node", invocation_state=graph_state))
        assert hooks._agento11y_handler._workflow_step_runs != {}
        hooks.after_tool_call(_tool_ended(sub_agent, sub_state))
        hooks.after_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        hooks.after_tool_call(_tool_ended(outer_agent, outer_state))
        hooks.after_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        client.flush()
    finally:
        client.shutdown()

    # The caller's root cleanup cannot reach the step after the sub-agent ends, so the sub-agent drops it.
    assert hooks._agento11y_handler._workflow_step_runs == {}
    assert "on_chain_end callback never fired" in caplog.text


@pytest.mark.parametrize(
    "case",
    [
        # The tool ends normally while the caller is still running.
        "tool_ended",
        # A background tool ends in another context, and the caller calls the model again.
        "tool_ended_in_another_context",
        # The caller ends in another context, so nothing clears the value in this one.
        "caller_ended_in_another_context",
        # An interrupt stops the tool before AfterToolCallEvent, and the caller ends.
        "tool_never_ended",
    ],
)
def test_strands_agent_started_after_the_tool_is_top_level(case: str) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        outer_state = {}
        next_state = {}
        outer_agent = _agent("outer")
        next_agent = _agent("next")
        outer_ended = SimpleNamespace(agent=outer_agent, invocation_state=outer_state)

        hooks.before_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        _model_call(hooks, outer_agent, outer_state, "delegating")
        hooks.before_tool_call(_tool_event(outer_agent, outer_state))
        if case == "tool_ended":
            hooks.after_tool_call(_tool_ended(outer_agent, outer_state))
        elif case == "tool_ended_in_another_context":
            contextvars.copy_context().run(hooks.after_tool_call, _tool_ended(outer_agent, outer_state))
            _model_call(hooks, outer_agent, outer_state, "after the tool")
        elif case == "caller_ended_in_another_context":
            contextvars.copy_context().run(hooks.after_tool_call, _tool_ended(outer_agent, outer_state))
            contextvars.copy_context().run(hooks.after_invocation, outer_ended)
        else:
            hooks.after_invocation(outer_ended)

        # When the caller has ended, a run below it would look the same in the export, so check the
        # caller directly.
        assert hooks._active_tool_caller() is None
        # Application code starts another agent in the same context.
        hooks.before_invocation(SimpleNamespace(agent=next_agent, invocation_state=next_state))
        _model_call(hooks, next_agent, next_state, "first")
        _model_call(hooks, next_agent, next_state, "second")
        hooks.after_invocation(SimpleNamespace(agent=next_agent, invocation_state=next_state))
        if case in ("tool_ended", "tool_ended_in_another_context"):
            hooks.after_invocation(outer_ended)
        client.flush()
    finally:
        client.shutdown()

    generations = _generations_by_text(exporter)
    # A run below the caller would follow the caller's generation. A top-level run starts its own lineage.
    assert generations["first"].parent_generation_ids == []
    assert generations["second"].parent_generation_ids == [generations["first"].id]
    assert hooks._tool_callers_before == {}


# An async tool that awaits invoke_async runs the sub-agent in the tool's own context, so the
# sub-agent's tool hooks change the value that the outer tool still needs.
@pytest.mark.parametrize("sub_tool_end", ["never", "in_another_context"])
def test_strands_sub_agent_tool_that_did_not_end_here_restores_the_outer_caller(sub_tool_end: str) -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        outer_state = {"session_id": "session-1"}
        sub_state = {}
        second_state = {}
        outer_agent = _agent("outer")
        sub_agent = _agent("sub")
        second_agent = _agent("second")

        hooks.before_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        _model_call(hooks, outer_agent, outer_state, "delegating")
        hooks.before_tool_call(_tool_event(outer_agent, outer_state))
        hooks.before_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        _model_call(hooks, sub_agent, sub_state, "sub calls a tool")
        hooks.before_tool_call(_tool_event(sub_agent, sub_state))
        if sub_tool_end == "in_another_context":
            # A background tool ends in its own context, and the sub-agent calls the model again.
            contextvars.copy_context().run(hooks.after_tool_call, _tool_ended(sub_agent, sub_state))
            _model_call(hooks, sub_agent, sub_state, "sub after its tool")
        # Otherwise an interrupt stops the sub-agent's tool before AfterToolCallEvent.
        hooks.after_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        # The outer tool goes on and invokes a second agent in the same context.
        hooks.before_invocation(SimpleNamespace(agent=second_agent, invocation_state=second_state))
        _model_call(hooks, second_agent, second_state, "second")
        hooks.after_invocation(SimpleNamespace(agent=second_agent, invocation_state=second_state))
        hooks.after_tool_call(_tool_ended(outer_agent, outer_state))
        hooks.after_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        client.flush()
    finally:
        client.shutdown()

    generations = _generations_by_text(exporter)
    assert generations["second"].parent_generation_ids == [generations["delegating"].id]
    assert generations["second"].conversation_id == "session-1"
    _assert_no_state_left(hooks)


def test_strands_two_hook_providers_on_one_agent_each_link_the_agent_called_from_a_tool() -> None:
    exporters = [_CapturingExporter(), _CapturingExporter()]
    clients = [_new_client(exporter) for exporter in exporters]
    try:
        providers = [
            create_agento11y_strands_hook_provider(client=client, provider_resolver="auto") for client in clients
        ]
        outer_state = {}
        sub_state = {}
        outer_agent = _agent("outer")
        sub_agents = [_agent("sub-1"), _agent("sub-2")]

        for hooks in providers:
            hooks.before_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
            _model_call(hooks, outer_agent, outer_state, "delegating")
        for hooks in providers:
            hooks.before_tool_call(_tool_event(outer_agent, outer_state))
        # Each sub-agent uses only one of the providers.
        for hooks, sub_agent in zip(providers, sub_agents, strict=True):
            hooks.before_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
            _model_call(hooks, sub_agent, sub_state, "done")
            hooks.after_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        for hooks in reversed(providers):
            hooks.after_tool_call(_tool_ended(outer_agent, outer_state))
        for hooks in providers:
            hooks.after_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        for client in clients:
            client.flush()
    finally:
        for client in clients:
            client.shutdown()

    for exporter in exporters:
        generations = _generations_by_text(exporter)
        assert generations["done"].parent_generation_ids == [generations["delegating"].id]
        assert generations["done"].conversation_id == generations["delegating"].conversation_id


def test_strands_agent_called_from_a_tool_without_workflow_capture_nests_its_span() -> None:
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    spans = InMemorySpanExporter()
    tracer_provider = TracerProvider()
    tracer_provider.add_span_processor(SimpleSpanProcessor(spans))
    exporter = _CapturingExporter()
    client = Client(
        ClientConfig(
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
            tracer_provider=tracer_provider,
        )
    )
    try:
        hooks = create_agento11y_strands_hook_provider(
            client=client, provider_resolver="auto", capture_workflow_steps=False
        )
        outer_state = {"session_id": "session-1"}
        sub_state = {}
        outer_agent = _agent("outer")
        sub_agent = _agent("sub")

        hooks.before_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        _model_call(hooks, outer_agent, outer_state, "delegating")
        hooks.before_tool_call(_tool_event(outer_agent, outer_state))
        hooks.before_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        _model_call(hooks, sub_agent, sub_state, "done")
        hooks.after_invocation(SimpleNamespace(agent=sub_agent, invocation_state=sub_state))
        hooks.after_tool_call(_tool_ended(outer_agent, outer_state))
        hooks.after_invocation(SimpleNamespace(agent=outer_agent, invocation_state=outer_state))
        client.flush()
    finally:
        client.shutdown()

    chain_spans = {span.name: span for span in spans.get_finished_spans() if span.name.startswith("strands.chain")}
    assert chain_spans["strands.chain sub"].parent.span_id == chain_spans["strands.chain outer"].context.span_id
    assert set(_generations_by_text(exporter)) == {"delegating", "done"}


@pytest.mark.parametrize("executor", ["concurrent", "sequential"])
@pytest.mark.parametrize("sub_agent_tool", ["sync", "async", "as_tool"])
def test_strands_real_agents_called_from_tools_follow_the_caller(executor: str, sub_agent_tool: str) -> None:
    from strands import Agent, tool
    from strands.tools.executors import ConcurrentToolExecutor, SequentialToolExecutor

    def make_tool(name: str, sub_agent):
        if sub_agent_tool == "as_tool":
            # Strands runs the sub-agent's stream_async inside the tool.
            return sub_agent.as_tool(name=name)
        if sub_agent_tool == "async":

            async def ask(input: str) -> str:
                """Ask a sub-agent."""
                return str(await sub_agent.invoke_async(input))

        else:

            def ask(input: str) -> str:
                """Ask a sub-agent."""
                # Strands runs a sync tool in a worker thread, and the sub-agent call starts another one.
                return str(sub_agent(input))

        return tool(name=name)(ask)

    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        sub_a = Agent(name="sub_a", model=_ScriptedModel("a-done"), hooks=[hooks], callback_handler=None)
        sub_b = Agent(name="sub_b", model=_ScriptedModel("b-done"), hooks=[hooks], callback_handler=None)
        outer_agent = Agent(
            name="outer",
            model=_ScriptedModel("tools:ask_a,ask_b", "final"),
            tools=[make_tool("ask_a", sub_a), make_tool("ask_b", sub_b)],
            tool_executor=ConcurrentToolExecutor() if executor == "concurrent" else SequentialToolExecutor(),
            hooks=[hooks],
            callback_handler=None,
        )
        outer_agent("hello", invocation_state={"session_id": "session-1"})
        client.flush()
    finally:
        client.shutdown()

    generations = [generation for request in exporter.requests for generation in request.generations]
    by_agent = {name: [g for g in generations if g.agent_name == name] for name in ("outer", "sub_a", "sub_b")}
    tool_call, final = by_agent["outer"]
    assert {generation.conversation_id for generation in generations} == {"session-1"}
    # Both sub-agents follow the generation that called their tools, not each other.
    assert by_agent["sub_a"][0].parent_generation_ids == [tool_call.id]
    assert by_agent["sub_b"][0].parent_generation_ids == [tool_call.id]
    assert final.parent_generation_ids == [tool_call.id]
    _assert_no_state_left(hooks)


def test_strands_real_graph_called_from_a_tool_joins_the_caller_conversation() -> None:
    from strands import Agent, tool
    from strands.multiagent import GraphBuilder

    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        first = Agent(name="first", model=_ScriptedModel("first-done"), hooks=[hooks], callback_handler=None)
        second = Agent(name="second", model=_ScriptedModel("second-done"), hooks=[hooks], callback_handler=None)
        builder = GraphBuilder()
        builder.set_hook_providers([hooks])
        builder.add_node(first, "first")
        builder.add_node(second, "second")
        builder.add_edge("first", "second")
        graph = builder.build()

        @tool
        def run_graph(input: str) -> str:
            """Run a graph."""
            return str(graph(input))

        outer_agent = Agent(
            name="outer",
            model=_ScriptedModel("tools:run_graph", "final"),
            tools=[run_graph],
            hooks=[hooks],
            callback_handler=None,
        )
        outer_agent("hello", invocation_state={"session_id": "session-1"})
        client.flush()
    finally:
        client.shutdown()

    generations = [generation for request in exporter.requests for generation in request.generations]
    by_agent = {name: [g for g in generations if g.agent_name == name] for name in ("outer", "first", "second")}
    steps = [step for request in exporter.workflow_requests for step in request.workflow_steps]
    assert {generation.conversation_id for generation in generations} == {"session-1"}
    assert {step.step_name: step.conversation_id for step in steps} == {"first": "session-1", "second": "session-1"}
    # The graph is its own root, so its first node does not follow the caller's generation.
    assert by_agent["first"][0].parent_generation_ids == []
    assert by_agent["second"][0].parent_generation_ids == [by_agent["first"][0].id]
    _assert_no_tool_call_state_left(hooks)


def test_strands_graph_fallback_conversation_id_is_shared_by_steps_and_generations() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    try:
        hooks = create_agento11y_strands_hook_provider(client=client, provider_resolver="auto")
        state = {}
        agent = _agent("node")
        source = SimpleNamespace(name="graph", nodes={"node": SimpleNamespace(executor=agent)})

        hooks.before_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))
        hooks.before_node_call(SimpleNamespace(source=source, node_id="node", invocation_state=state))
        hooks.before_model_call(SimpleNamespace(agent=agent, invocation_state=state))
        hooks.after_model_call(
            SimpleNamespace(
                agent=agent,
                invocation_state=state,
                stop_response=SimpleNamespace(
                    message={"role": "assistant", "content": [{"text": "done"}]},
                    stop_reason="end_turn",
                ),
                exception=None,
            )
        )
        hooks.after_node_call(SimpleNamespace(source=source, node_id="node", invocation_state=state))
        hooks.after_multi_agent_invocation(SimpleNamespace(source=source, invocation_state=state))
        client.flush()

        generation = exporter.requests[0].generations[0]
        step = exporter.workflow_requests[0].workflow_steps[0]
        assert generation.conversation_id == step.conversation_id
    finally:
        client.shutdown()


def test_strands_bedrock_model_id_resolves_to_vendor_provider() -> None:
    from agento11y_strands.handler import _resolve_provider_name

    assert _resolve_provider_name("", "auto", "us.anthropic.claude-sonnet-4-20250514-v1:0", {}) == "anthropic"
    assert (
        _resolve_provider_name(
            "",
            "auto",
            "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.meta.llama3-3-70b-instruct-v1:0",
            {},
        )
        == "meta"
    )
    assert (
        _resolve_provider_name("", "auto", "us.anthropic.claude-sonnet-4-v1:0", {"provider": "bedrock"}) == "anthropic"
    )


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


def test_strands_nested_params_config_reports_sync_mode_and_sampling_params() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        agent = _agent()
        agent.model = _NestedParamsModel(stream=False, temperature=0.2, max_tokens=256, top_p=0.9)
        _record_one_model_call(client, agent)

        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "SYNC"
        assert generation.temperature == 0.2
        assert generation.max_tokens == 256
        assert generation.top_p == 0.9
    finally:
        client.shutdown()


def test_strands_flat_config_stream_flag_reports_sync_mode() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        agent = _agent()
        agent.model = _FlatConfigModel(stream=False, temperature=0.3)
        _record_one_model_call(client, agent)

        generation = exporter.requests[0].generations[0]
        assert generation.mode.value == "SYNC"
        assert generation.temperature == 0.3
    finally:
        client.shutdown()


def test_strands_bedrock_streaming_flag_reports_sync_mode() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        agent = _agent()
        agent.model = _FlatConfigModel(streaming=False)
        _record_one_model_call(client, agent)

        assert exporter.requests[0].generations[0].mode.value == "SYNC"
    finally:
        client.shutdown()


def test_strands_streaming_is_the_default_when_the_config_omits_the_flag() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        agent = _agent()
        agent.model = _NestedParamsModel(temperature=0.5)
        _record_one_model_call(client, agent)

        assert exporter.requests[0].generations[0].mode.value == "STREAM"
    finally:
        client.shutdown()


def test_strands_model_call_without_message_metadata_records_no_usage() -> None:
    exporter = _CapturingExporter()
    client = _new_client(exporter)

    try:
        agent = _agent()
        _record_one_model_call(client, agent, message={"role": "assistant", "content": [{"text": "ok"}]})

        generation = exporter.requests[0].generations[0]
        assert generation.stop_reason == "end_turn"
        assert generation.usage is None or generation.usage.total_tokens == 0
    finally:
        client.shutdown()


def test_strands_cache_token_usage_is_preserved() -> None:
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
