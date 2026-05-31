# Python LangGraph Dependency DAG POC

This example emits a five-node LangGraph workflow as Sigil generations linked by `parent_generation_ids`.

It uses `FakeListChatModel`, so it exercises the LangGraph callback lifecycle without external model credentials. Replace the fake model with a real in-house LangChain chat model, `ChatOpenAI`, or another provider wrapper for a customer POC.

## Run

```bash
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
python main.py
```

By default, the example uses `SIGIL_EXPORT_PROTOCOL=none` so it can run without a local stack. Point it at a local Sigil stack before running if you want the records to appear in Grafana:

```bash
export SIGIL_EXPORT_PROTOCOL=http
export SIGIL_EXPORT_ENDPOINT=http://localhost:8080
python main.py
```

## What It Proves

Each LangGraph node passes these callback metadata keys into `with_sigil_langgraph_callbacks`:

- `sigil.generation.id`: stable generation ID for the current node.
- `sigil.generation.parent_generation_ids`: generation IDs for upstream nodes whose output this node consumes.
- `langgraph_node`: node name shown in Sigil generation metadata.
- `thread_id`: conversation ID so all node generations land in one conversation.

Sigil's conversation dependency tab can render the resulting workflow because the exported generations form this DAG:

```text
extract_intent -> retrieve_context -> draft_answer -> critique_answer -> final_answer
                                      \------------------------------/
```

This proves the current Sigil data model can represent LangGraph workflow dependencies as a generation DAG. It does not yet provide a native LangGraph state-transition or state-diff UI.
