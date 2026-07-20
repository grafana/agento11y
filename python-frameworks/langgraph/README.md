# Sigil Python Framework Module: LangGraph

`agento11y-langgraph` provides callback handlers that map LangGraph lifecycle events into Sigil generation recorder lifecycles.

## Installation

```bash
pip install agento11y agento11y-langgraph
pip install langgraph langchain-openai
```

## Usage

```python
from agento11y import Client
from agento11y_langgraph import with_agento11y_langgraph_callbacks

client = Client()
config = with_agento11y_langgraph_callbacks(None, client=client, provider_resolver="auto")
```

## End-to-end example (graph invoke + stream)

```python
from typing import TypedDict

from langchain_core.runnables import RunnableConfig
from langchain_openai import ChatOpenAI
from langgraph.graph import END, StateGraph
from agento11y import Client
from agento11y_langgraph import with_agento11y_langgraph_callbacks


class GraphState(TypedDict):
    prompt: str
    answer: str


client = Client()
llm = ChatOpenAI(model="gpt-4o-mini", temperature=0)


def run_model(state: GraphState, config: RunnableConfig) -> GraphState:
    response = llm.invoke(
        state["prompt"],
        config=config,
    )
    return {"prompt": state["prompt"], "answer": str(response.content).strip()}


workflow = StateGraph(GraphState)
workflow.add_node("model", run_model)
workflow.set_entry_point("model")
workflow.add_edge("model", END)
graph = workflow.compile()

agento11y_config = with_agento11y_langgraph_callbacks(
    None,
    client=client,
    provider_resolver="auto",
    agent_name="langgraph-example",
    agent_version="1.0.0",
)

# Non-stream graph invocation.
out = graph.invoke(
    {"prompt": "Explain SLO burn rate in one paragraph.", "answer": ""},
    config=agento11y_config,
)
print(out["answer"])

# Streamed graph events.
for _event in graph.stream(
    {"prompt": "List three practical alerting tips.", "answer": ""},
    config=agento11y_config,
):
    pass

client.shutdown()
```

## Workflow step capture

Enable `capture_workflow_steps=True` to record each graph node as a Sigil workflow step.
This enables the **Workflow** tab in the conversation detail view, showing node execution order,
duration, input/output state, and which LLM generations ran inside each node. The **Dependencies**
tab remains available for the generation-level DAG built from `parent_generation_ids`.

Always set `conversation_title` to a short human-readable label — it appears as the conversation
name in the Sigil UI. Without it, the title falls back to an opaque auto-generated ID.

```python
from agento11y import Client
from agento11y_langgraph import Agento11yLangGraphHandler

client = Client()
handler = Agento11yLangGraphHandler(
    client=client,
    agent_name="my-pipeline",
    conversation_title="My Pipeline Run",
    capture_workflow_steps=True,
)

# Reuse the `graph` from the end-to-end example above. The node must pass its
# received `config` into `llm.invoke(...)` so generations link to the workflow step.
result = graph.invoke(
    {"prompt": "Explain why my dashboard is slow.", "answer": ""},
    config={"callbacks": [handler]},
)
client.shutdown()
```

The handler automatically:
- Detects graph root and direct-child nodes
- Creates a workflow step per node with `input_state`, `output_state`, and timestamps
- Links LLM generation IDs to their parent step via `linked_generation_ids`
- Tracks sequential `parent_step_ids` so the DAG edges are correct

## Persistent thread example (LangGraph checkpointer)

```python
from langgraph.checkpoint.memory import MemorySaver

checkpointer = MemorySaver()
graph = workflow.compile(checkpointer=checkpointer)

thread_config = {
    **with_agento11y_langgraph_callbacks(None, client=client, provider_resolver="auto"),
    "configurable": {"thread_id": "customer-42"},
}

graph.invoke({"prompt": "Remember that my timezone is UTC+1.", "answer": ""}, config=thread_config)
graph.invoke({"prompt": "What timezone did I just give you?", "answer": ""}, config=thread_config)

# Advanced usage: explicit handler wiring remains supported.
_ = graph.invoke(
    {"prompt": "manual handler wiring", "answer": ""},
    config={"callbacks": [handler]},
)
```

When `thread_id` is present, the handler records:

- `conversation_id=<thread_id>`
- `metadata["agento11y.framework.run_id"]=<run id>`
- `metadata["agento11y.framework.thread_id"]=<thread id>`
- generation span attributes `agento11y.framework.run_id` and `agento11y.framework.thread_id`

## Behavior

- Lifecycle mapping:
  - `on_llm_start` / `on_chat_model_start` -> generation recorder
  - `on_tool_start` / `on_tool_end` / `on_tool_error` -> `start_tool_execution`
  - `on_chain_start` / `on_chain_end` / `on_chain_error` -> framework chain spans
  - `on_retriever_start` / `on_retriever_end` / `on_retriever_error` -> framework retriever spans
  - `on_llm_new_token` -> first-token timestamp for stream mode
- Mode mapping: non-stream -> `SYNC`, stream -> `STREAM`.
- Provider resolver parity:
  - explicit provider metadata when available
  - model-name inference (`gpt-`/`o1`/`o3`/`o4` -> `openai`, `claude-` -> `anthropic`, `gemini-` -> `gemini`)
  - fallback -> `custom`
- Framework tags/metadata are always set:
  - `agento11y.framework.name=langgraph`
  - `agento11y.framework.source=handler`
  - `agento11y.framework.language=python`
  - `metadata["agento11y.framework.run_id"]=<run id>`
  - `metadata["agento11y.framework.thread_id"]=<thread id>` (when present in callback metadata/config)
  - `metadata["agento11y.framework.parent_run_id"]` (when available)
  - `metadata["agento11y.framework.component_name"]` (serialized component identity)
  - `metadata["agento11y.framework.run_type"]` (`llm`, `chat`, `tool`, `chain`, `retriever`)
  - `metadata["agento11y.framework.tags"]` (normalized callback tags)
  - `metadata["agento11y.framework.retry_attempt"]` (when available)
  - `metadata["agento11y.framework.langgraph.node"]` (when callback context exposes node identity)
  - generation span attributes mirror low-cardinality framework metadata keys

Call `client.shutdown()` during teardown to flush buffered telemetry.
