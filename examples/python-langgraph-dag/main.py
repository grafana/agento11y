from __future__ import annotations

import os
from typing import Annotated, TypedDict
from uuid import uuid4

from langchain_core.language_models.fake_chat_models import FakeListChatModel
from langchain_core.messages import HumanMessage
from langgraph.graph import END, START, StateGraph
from sigil_sdk import Client, ClientConfig, GenerationExportConfig
from sigil_sdk_langgraph import with_sigil_langgraph_callbacks


def merge_generation_ids(left: dict[str, str] | None, right: dict[str, str] | None) -> dict[str, str]:
    merged = dict(left or {})
    merged.update(right or {})
    return merged


class GraphState(TypedDict, total=False):
    question: str
    intent: str
    context: str
    draft: str
    critique: str
    answer: str
    generation_ids: Annotated[dict[str, str], merge_generation_ids]


client = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol=os.getenv("SIGIL_EXPORT_PROTOCOL", "none"),
            endpoint=os.getenv("SIGIL_EXPORT_ENDPOINT", "localhost:4317"),
        )
    )
)
conversation_id = f"rad-ai-langgraph-poc-{uuid4().hex[:8]}"

# FakeListChatModel triggers the LangChain/LangGraph callback lifecycle without
# requiring external model credentials. Replace this with a real in-house model
# wrapper or ChatOpenAI/ChatAnthropic in a customer POC.
llm = FakeListChatModel(
    responses=[
        "Intent: explain whether Sigil can visualize LangGraph node dependencies.",
        "Context: Sigil renders dependency DAGs from parent_generation_ids.",
        "Draft: emit stable generation IDs per LangGraph node and link parents.",
        "Critique: call out that this is a generation DAG, not a native LangGraph state diff UI.",
        "Final: Sigil can POC the workflow DAG today; native state-transition UX is future work.",
    ]
)


def generation_id(node: str) -> str:
    return f"{conversation_id}:{node}"


def invoke_node(state: GraphState, *, node: str, prompt: str, parents: list[str]) -> tuple[str, str]:
    current_generation_id = generation_id(node)
    known_generation_ids = state.get("generation_ids", {})
    parent_generation_ids = [known_generation_ids[parent] for parent in parents if parent in known_generation_ids]

    config = with_sigil_langgraph_callbacks(
        {
            "metadata": {
                "thread_id": conversation_id,
                "langgraph_node": node,
                "sigil.generation.id": current_generation_id,
                "sigil.generation.parent_generation_ids": parent_generation_ids,
            },
            "configurable": {"thread_id": conversation_id},
        },
        client=client,
        provider="custom",
        agent_name=node,
        agent_version="poc",
    )
    response = llm.invoke([HumanMessage(content=prompt)], config=config)
    return str(response.content), current_generation_id


def extract_intent(state: GraphState) -> GraphState:
    output, gen_id = invoke_node(
        state,
        node="extract_intent",
        parents=[],
        prompt=f"Extract the customer's intent from: {state['question']}",
    )
    return {"intent": output, "generation_ids": {"extract_intent": gen_id}}


def retrieve_context(state: GraphState) -> GraphState:
    output, gen_id = invoke_node(
        state,
        node="retrieve_context",
        parents=["extract_intent"],
        prompt=f"Find Sigil context for this intent: {state['intent']}",
    )
    return {"context": output, "generation_ids": {"retrieve_context": gen_id}}


def draft_answer(state: GraphState) -> GraphState:
    output, gen_id = invoke_node(
        state,
        node="draft_answer",
        parents=["retrieve_context"],
        prompt=f"Draft an answer using this context: {state['context']}",
    )
    return {"draft": output, "generation_ids": {"draft_answer": gen_id}}


def critique_answer(state: GraphState) -> GraphState:
    output, gen_id = invoke_node(
        state,
        node="critique_answer",
        parents=["draft_answer"],
        prompt=f"Critique this draft for product accuracy: {state['draft']}",
    )
    return {"critique": output, "generation_ids": {"critique_answer": gen_id}}


def final_answer(state: GraphState) -> GraphState:
    output, gen_id = invoke_node(
        state,
        node="final_answer",
        parents=["draft_answer", "critique_answer"],
        prompt=f"Finalize this draft: {state['draft']}\nCritique: {state['critique']}",
    )
    return {"answer": output, "generation_ids": {"final_answer": gen_id}}


workflow = StateGraph(GraphState)
workflow.add_node("extract_intent", extract_intent)
workflow.add_node("retrieve_context", retrieve_context)
workflow.add_node("draft_answer", draft_answer)
workflow.add_node("critique_answer", critique_answer)
workflow.add_node("final_answer", final_answer)
workflow.add_edge(START, "extract_intent")
workflow.add_edge("extract_intent", "retrieve_context")
workflow.add_edge("retrieve_context", "draft_answer")
workflow.add_edge("draft_answer", "critique_answer")
workflow.add_edge("critique_answer", "final_answer")
workflow.add_edge("final_answer", END)
graph = workflow.compile()

try:
    result = graph.invoke(
        {
            "question": "Can Sigil visualize a Rad AI LangGraph workflow?",
            "generation_ids": {},
        }
    )
    client.flush()
finally:
    client.shutdown()

print(f"conversation_id={conversation_id}")
print("generation DAG:")
for node, gen_id in result["generation_ids"].items():
    print(f"- {node}: {gen_id}")
print(f"answer={result['answer']}")
