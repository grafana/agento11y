"""A deliberately tiny LangGraph agent: one node that answers a question.

When ``OPENAI_API_KEY`` is set, the node uses a real ``ChatOpenAI`` model.
Otherwise it falls back to a deterministic fake chat model that returns the
supplied ``canned_answers`` in order.
"""

from __future__ import annotations

import os
from typing import TypedDict

from langchain_core.runnables import RunnableConfig
from langgraph.graph import END, StateGraph


class GraphState(TypedDict):
    question: str
    answer: str


def _build_model(canned_answers: list[str]):
    if os.environ.get("OPENAI_API_KEY"):
        from langchain_openai import ChatOpenAI

        return ChatOpenAI(model="gpt-4o-mini", temperature=0)

    # Deterministic fallback: returns canned answers in order, one per invocation.
    from langchain_core.language_models.fake_chat_models import FakeListChatModel

    return FakeListChatModel(responses=list(canned_answers))


def build_graph(canned_answers: list[str]):
    """Compiles a one-node graph that answers ``state['question']``."""

    llm = _build_model(canned_answers)

    def answer_node(state: GraphState, config: RunnableConfig) -> GraphState:
        response = llm.invoke(state["question"], config=config)
        return {"question": state["question"], "answer": str(response.content).strip()}

    workflow = StateGraph(GraphState)
    workflow.add_node("answer", answer_node)
    workflow.set_entry_point("answer")
    workflow.add_edge("answer", END)
    return workflow.compile()
