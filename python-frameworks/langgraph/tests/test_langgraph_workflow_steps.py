"""Workflow-step capture with real LangGraph graphs."""

from __future__ import annotations

import asyncio
from datetime import timedelta

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


def test_concurrent_graph_runs_on_one_handler_keep_their_own_conversations() -> None:
    exporter = _CapturingExporter()
    client = Client(
        ClientConfig(
            generation_export=GenerationExportConfig(batch_size=50, flush_interval=timedelta(0)),
            generation_exporter=exporter,
        )
    )
    model = FakeListChatModel(responses=["ok", "ok"])

    async def agent(state, config):
        await asyncio.sleep(0.03)
        return {"messages": [await model.ainvoke(state["messages"], config)]}

    builder = StateGraph(MessagesState)
    builder.add_node("agent", agent)
    builder.add_edge(START, "agent")
    builder.add_edge("agent", END)
    graph = builder.compile()
    handler = Agento11yAsyncLangGraphHandler(client=client, capture_workflow_steps=True)

    def invoke(thread_id: str):
        return graph.ainvoke(
            {"messages": [HumanMessage(thread_id)]},
            {"callbacks": [handler], "configurable": {"thread_id": thread_id}},
        )

    async def run() -> None:
        first = asyncio.create_task(invoke("customer-a"))
        # Start the second run while the first one is inside its node.
        await asyncio.sleep(0.005)
        await asyncio.gather(first, invoke("customer-b"))

    try:
        asyncio.run(run())
        client.flush()
    finally:
        client.shutdown()

    assert len(exporter.workflow_steps) == 2
    for step in exporter.workflow_steps:
        assert step.conversation_id in str(step.input_state)
    assert sorted(generation.conversation_id for generation in exporter.generations) == ["customer-a", "customer-b"]
