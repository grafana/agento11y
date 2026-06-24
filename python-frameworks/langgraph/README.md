# Sigil Python Framework Module: LangGraph

`sigil-sdk-langgraph` provides callback handlers that map LangGraph lifecycle events into Sigil generation recorder lifecycles.

## Installation

```bash
pip install sigil-sdk sigil-sdk-langgraph
pip install langgraph langchain-openai
```

## Usage

```python
from sigil_sdk import Client
from sigil_sdk_langgraph import with_sigil_langgraph_callbacks

client = Client()
config = with_sigil_langgraph_callbacks(None, client=client, provider_resolver="auto")
```

## End-to-end example (graph invoke + stream)

```python
from typing import TypedDict

from langchain_core.runnables import RunnableConfig
from langchain_openai import ChatOpenAI
from langgraph.graph import END, StateGraph
from sigil_sdk import Client
from sigil_sdk_langgraph import with_sigil_langgraph_callbacks


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

sigil_config = with_sigil_langgraph_callbacks(
    None,
    client=client,
    provider_resolver="auto",
    agent_name="langgraph-example",
    agent_version="1.0.0",
)

# Non-stream graph invocation.
out = graph.invoke(
    {"prompt": "Explain SLO burn rate in one paragraph.", "answer": ""},
    config=sigil_config,
)
print(out["answer"])

# Streamed graph events.
for _event in graph.stream(
    {"prompt": "List three practical alerting tips.", "answer": ""},
    config=sigil_config,
):
    pass

client.shutdown()
```

## Workflow step capture

Enable `capture_workflow_steps=True` to record each graph node as a Sigil workflow step.
This builds a visual DAG in the Sigil UI showing node execution order, duration, input/output state,
and which LLM generations ran inside each node.

Always set `conversation_title` to a short human-readable label — it appears as the conversation
name in the Sigil UI. Without it, the title falls back to an opaque auto-generated ID.

```python
from sigil_sdk import Client
from sigil_sdk_langgraph import SigilLangGraphHandler

client = Client()
handler = SigilLangGraphHandler(
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
    **with_sigil_langgraph_callbacks(None, client=client, provider_resolver="auto"),
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
- `metadata["sigil.framework.run_id"]=<run id>`
- `metadata["sigil.framework.thread_id"]=<thread id>`
- generation span attributes `sigil.framework.run_id` and `sigil.framework.thread_id`

## Experiments (offline evaluation)

Run a LangGraph agent over a dataset as a Sigil experiment, grade locally, and
publish scores you can compare in the Sigil UI. The runner rides on the callback
handler above, so generations emitted during the run are auto-tagged with the
experiment `run_id`.

```python
from sigil_sdk import ScoreValue
from sigil_sdk_langgraph import DatasetItem, ExperimentRunner, ScoreOutput, TargetResult

dataset = [
    DatasetItem(id="capital-fr", input="Capital of France?", expected="Paris",
                metadata={"task_id": "capital", "task_category": "trivia"}),
]

def target(item, run):
    # Pass run.langgraph_config() so generations carry the experiment run_id.
    out = graph.invoke({"prompt": item.input, "answer": ""}, config=run.langgraph_config())
    return TargetResult(output=out["answer"])  # generation ids captured automatically

def exact_match(item, result):
    passed = str(item.expected).lower() in str(result.output).lower()
    return [ScoreOutput(evaluator_id="suite.exact_match", evaluator_version="2026-05-28",
                        score_key="exact_match", value=ScoreValue(number=1.0 if passed else 0.0),
                        passed=passed)]

runner = ExperimentRunner(client=client, run_id="pr-123", name="PR 123",
                          dataset={"id": "smoke", "version": "2026-05-28"}, tags=["ci"])
result = runner.run(dataset, target, [exact_match])
print(result.url)  # deep link to the experiment in Sigil
```

The runner creates the run (`source="external"`), runs + grades each item,
exports scores attributed to the `run_id`, and finalizes the run (`succeeded` on
clean exit, `failed` on exception or Ctrl-C). For ad-hoc loops use
the lower-level `experiment(...)` context manager. A/B testing is two runs with
different `run_id`/`tags`. See the `sigil-langgraph-experiments` skill
(`skills/sigil-langgraph-experiments/SKILL.md`) and the runnable example at
`examples/experiments/python-langgraph/` for grading patterns (including
LLM-as-judge) and uploading older runs.

Upload modes: `continuous` (default, publish per item), `bulk` (publish at the
end), `manual` (publish + finalize only when you call `run.publish()` /
`run.finalize()`).

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
  - `sigil.framework.name=langgraph`
  - `sigil.framework.source=handler`
  - `sigil.framework.language=python`
  - `metadata["sigil.framework.run_id"]=<run id>`
  - `metadata["sigil.framework.thread_id"]=<thread id>` (when present in callback metadata/config)
  - `metadata["sigil.framework.parent_run_id"]` (when available)
  - `metadata["sigil.framework.component_name"]` (serialized component identity)
  - `metadata["sigil.framework.run_type"]` (`llm`, `chat`, `tool`, `chain`, `retriever`)
  - `metadata["sigil.framework.tags"]` (normalized callback tags)
  - `metadata["sigil.framework.retry_attempt"]` (when available)
  - `metadata["sigil.framework.langgraph.node"]` (when callback context exposes node identity)
  - generation span attributes mirror low-cardinality framework metadata keys

Call `client.shutdown()` during teardown to flush buffered telemetry.
