"""Workflow-step capture with real LangGraph graphs."""

from __future__ import annotations

import asyncio
from datetime import timedelta

import pytest
from agento11y import Client, ClientConfig, GenerationExportConfig
from agento11y.models import (
    ExportGenerationResult,
    ExportGenerationsResponse,
    ExportWorkflowStepResult,
    ExportWorkflowStepsResponse,
)
from agento11y_langgraph import Agento11yAsyncLangGraphHandler
from langchain_core.language_models.fake_chat_models import FakeListChatModel
from langchain_core.messages import HumanMessage
from langgraph.graph import END, START, MessagesState, StateGraph


class _CapturingExporter:
    def __init__(self) -> None:
        self.generations = []
        self.workflow_steps = []

    def export_generations(self, request):
        self.generations.extend(request.generations)
        return ExportGenerationsResponse(
            results=[
                ExportGenerationResult(generation_id=generation.id, accepted=True) for generation in request.generations
            ]
        )

    def export_workflow_steps(self, request):
        self.workflow_steps.extend(request.workflow_steps)
        return ExportWorkflowStepsResponse(
            results=[ExportWorkflowStepResult(step_id=step.id, accepted=True) for step in request.workflow_steps]
        )

    def shutdown(self) -> None:
        return


@pytest.mark.parametrize("second_thread_id", ["customer-b", None])
def test_concurrent_graph_runs_on_one_handler_keep_their_own_conversations(second_thread_id: str | None) -> None:
    exporter = _CapturingExporter()
    client = Client(
        ClientConfig(
            generation_export=GenerationExportConfig(batch_size=50, flush_interval=timedelta(0)),
            generation_exporter=exporter,
        )
    )
    model = FakeListChatModel(responses=["ok", "ok"])
    events: dict[str, asyncio.Event] = {}

    async def agent(state, config):
        if state["messages"][0].content == "first":
            # Keep the first run inside its node until the second run has ended.
            events["first_inside"].set()
            await asyncio.wait_for(events["second_done"].wait(), timeout=5)
        return {"messages": [await model.ainvoke(state["messages"], config)]}

    builder = StateGraph(MessagesState)
    builder.add_node("agent", agent)
    builder.add_edge(START, "agent")
    builder.add_edge("agent", END)
    graph = builder.compile()
    handler = Agento11yAsyncLangGraphHandler(client=client, capture_workflow_steps=True)

    def invoke(text: str, thread_id: str | None):
        configurable = {"thread_id": thread_id} if thread_id is not None else {}
        return graph.ainvoke({"messages": [HumanMessage(text)]}, {"callbacks": [handler], "configurable": configurable})

    async def run() -> None:
        events["first_inside"] = asyncio.Event()
        events["second_done"] = asyncio.Event()
        first = asyncio.create_task(invoke("first", "customer-a"))
        await asyncio.wait_for(events["first_inside"].wait(), timeout=5)
        await invoke("second", second_thread_id)
        events["second_done"].set()
        await first

    try:
        asyncio.run(run())
        client.flush()
    finally:
        client.shutdown()

    def run_of(text: str) -> str:
        return "first" if "first" in text else "second"

    step_conversations = {run_of(str(step.input_state)): step.conversation_id for step in exporter.workflow_steps}
    generation_conversations = {
        run_of(generation.input[0].parts[0].text): generation.conversation_id for generation in exporter.generations
    }
    assert len(exporter.workflow_steps) == len(exporter.generations) == 2
    assert step_conversations == generation_conversations
    assert step_conversations["first"] == "customer-a"
    if second_thread_id is not None:
        assert step_conversations["second"] == second_thread_id
    else:
        assert step_conversations["second"].startswith("agento11y:framework:langgraph:")
