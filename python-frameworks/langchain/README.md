# Agent Observability Python Framework Module: LangChain

`agento11y-langchain` provides callback handlers that map LangChain lifecycle events into agento11y generation recorder lifecycles.

## Installation

```bash
pip install agento11y agento11y-langchain
pip install langchain-openai
```

## Usage

```python
from agento11y import Client
from agento11y_langchain import with_agento11y_langchain_callbacks

client = Client()
config = with_agento11y_langchain_callbacks(None, client=client, provider_resolver="auto")
```

## End-to-end example (invoke + stream)

```python
from langchain_openai import ChatOpenAI
from agento11y import Client
from agento11y_langchain import Agento11yLangChainHandler, with_agento11y_langchain_callbacks

client = Client()
handler = Agento11yLangChainHandler(
    client=client,
    provider_resolver="auto",
    agent_name="langchain-example",
    agent_version="1.0.0",
)

llm = ChatOpenAI(model="gpt-4o-mini", temperature=0)

# Non-stream call -> SYNC generation mode.
result = llm.invoke(
    "Summarize why retry budgets matter.",
    config=with_agento11y_langchain_callbacks(None, client=client, provider_resolver="auto"),
)
print(result.content)

# Stream call -> STREAM generation mode + TTFT tracking.
for chunk in llm.stream(
    "Give me three short reliability tips.",
    config=with_agento11y_langchain_callbacks(None, client=client, provider_resolver="auto"),
):
    if chunk.content:
        print(chunk.content, end="", flush=True)
print()

# Advanced usage: explicit handler wiring remains supported.
_ = llm.invoke("manual handler wiring", config={"callbacks": [handler]})

client.shutdown()
```

## Conversation grouping

The handler resolves the conversation id per invocation, in this order:

1. `conversation_id` / `session_id` / `group_id` in the callback metadata, invocation params, or `configurable`
2. A `thread_id` in the same places
3. The handler's `conversation_id` constructor argument
4. A synthetic per-run id

Pass `conversation_id` on the constructor when your application owns the conversation identity.
Per-invocation identity still wins, so a handler built once per process cannot override it.

```python
handler = Agento11yLangChainHandler(
    client=client,
    agent_name="my-chain",
    conversation_id=request.conversation_id,
)
```

Without any of these, each run becomes its own conversation.

## Behavior

- Lifecycle mapping:
  - `on_llm_start` / `on_chat_model_start` -> generation recorder
  - System and developer messages passed to `on_chat_model_start` are lifted out of the input
    message list into `system_prompt` (joined with a blank line when there are several), since the
    wire format has no system role. An explicit `invocation_params["system_prompt"]` wins.
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
  - `agento11y.framework.name=langchain`
  - `agento11y.framework.source=handler`
  - `agento11y.framework.language=python`
  - `metadata["agento11y.framework.run_id"]=<run id>`
  - `metadata["agento11y.framework.thread_id"]=<thread id>` (when present in callback metadata/config)
  - `metadata["agento11y.framework.parent_run_id"]` (when available)
  - `metadata["agento11y.framework.component_name"]` (serialized component identity)
  - `metadata["agento11y.framework.run_type"]` (`llm`, `chat`, `tool`, `chain`, `retriever`)
  - `metadata["agento11y.framework.tags"]` (normalized callback tags)
  - `metadata["agento11y.framework.retry_attempt"]` (when available)
  - generation span attributes mirror low-cardinality framework metadata keys

Call `client.shutdown()` during teardown to flush buffered telemetry.
