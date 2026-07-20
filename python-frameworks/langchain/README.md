# Sigil Python Framework Module: LangChain

`agento11y-langchain` provides callback handlers that map LangChain lifecycle events into Sigil generation recorder lifecycles.

## Installation

```bash
pip install agento11y agento11y-langchain
pip install langchain-openai
```

## Usage

```python
from agento11y import Client
from agento11y_langchain import with_sigil_langchain_callbacks

client = Client()
config = with_sigil_langchain_callbacks(None, client=client, provider_resolver="auto")
```

## End-to-end example (invoke + stream)

```python
from langchain_openai import ChatOpenAI
from agento11y import Client
from agento11y_langchain import SigilLangChainHandler, with_sigil_langchain_callbacks

client = Client()
handler = SigilLangChainHandler(
    client=client,
    provider_resolver="auto",
    agent_name="langchain-example",
    agent_version="1.0.0",
)

llm = ChatOpenAI(model="gpt-4o-mini", temperature=0)

# Non-stream call -> SYNC generation mode.
result = llm.invoke(
    "Summarize why retry budgets matter.",
    config=with_sigil_langchain_callbacks(None, client=client, provider_resolver="auto"),
)
print(result.content)

# Stream call -> STREAM generation mode + TTFT tracking.
for chunk in llm.stream(
    "Give me three short reliability tips.",
    config=with_sigil_langchain_callbacks(None, client=client, provider_resolver="auto"),
):
    if chunk.content:
        print(chunk.content, end="", flush=True)
print()

# Advanced usage: explicit handler wiring remains supported.
_ = llm.invoke("manual handler wiring", config={"callbacks": [handler]})

client.shutdown()
```

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
  - `sigil.framework.name=langchain`
  - `sigil.framework.source=handler`
  - `sigil.framework.language=python`
  - `metadata["sigil.framework.run_id"]=<run id>`
  - `metadata["sigil.framework.thread_id"]=<thread id>` (when present in callback metadata/config)
  - `metadata["sigil.framework.parent_run_id"]` (when available)
  - `metadata["sigil.framework.component_name"]` (serialized component identity)
  - `metadata["sigil.framework.run_type"]` (`llm`, `chat`, `tool`, `chain`, `retriever`)
  - `metadata["sigil.framework.tags"]` (normalized callback tags)
  - `metadata["sigil.framework.retry_attempt"]` (when available)
  - generation span attributes mirror low-cardinality framework metadata keys

Call `client.shutdown()` during teardown to flush buffered telemetry.
