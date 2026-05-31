---
name: sigil-experiments
description: >-
  Run any Python LLM agent as a Sigil offline-evaluation experiment using the
  core sigil-sdk (no framework adapter required): record generations, run over a
  dataset (or A/B two versions), grade locally, and publish scores to Sigil. Use
  when a user wants to evaluate/compare agent runs, gate a PR on agent quality,
  or upload an old eval run to Sigil and is NOT using a supported framework
  adapter (LangGraph, LangChain, etc.) — for those, prefer the framework skill.
---

# Sigil experiments (framework-free)

You are a coding agent adding **experiment tracking** to a Python project using
the core `sigil-sdk` only — no framework adapter. Keep changes minimal and ride
on the existing generation instrumentation (`client.start_generation(...)`). The
flow is generation-first and publishes continuously: create the run, then per
item run the agent (recording generations tagged with the `run_id`), grade, and
export scores under the same `run_id`.

If the project already uses a supported framework (LangGraph, LangChain, OpenAI
Agents, LlamaIndex, Strands, Google ADK, LiteLLM), prefer that framework's
experiments skill — it auto-captures generation ids from the framework callback.
This skill is the generic fallback that works with raw SDK recording.

## Setup

```bash
pip install "sigil-sdk>=0.7.0"
```

Configure the client from env (works in CI):

```python
import os
from sigil_sdk import ApiConfig, AuthConfig, Client, ClientConfig, GenerationExportConfig

endpoint = os.environ.get("SIGIL_ENDPOINT", "http://localhost:8080")
client = Client(
    ClientConfig(
        api=ApiConfig(endpoint=endpoint),
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint=f"{endpoint}/api/v1/generations:export",
            auth=AuthConfig(mode="tenant", tenant_id=os.environ.get("SIGIL_AUTH_TENANT_ID", "fake")),
        ),
    )
)
```

## Pattern 1 — Run a new experiment over a dataset (recommended)

Use `ExperimentRunner`. You supply a **dataset**, a **target** (run your agent,
recording its generation(s) via `run.start_generation(...)`), and one or more
**scorers** (return typed `ScoreOutput`s). The runner creates the run, tags every
generation with the `run_id`, grades, publishes scores, finalizes, and prints a
link.

```python
from sigil_sdk import (
    DatasetItem, ExperimentRun, ExperimentRunner, Generation, GenerationStart,
    ModelRef, ScoreOutput, ScoreValue, TargetResult, assistant_text_message,
    user_text_message,
)

dataset = [
    DatasetItem(id="capital-fr", input="Capital of France?", expected="Paris",
                metadata={"task_id": "capital", "task_category": "trivia"}),
    # ...
]

def target(item, run):
    # IMPORTANT: record the agent's call through run.start_generation(...) so the
    # generation carries the experiment run_id and its id is captured for scoring.
    with run.start_generation(GenerationStart(
        model=ModelRef(provider="openai", name="gpt-4o-mini"),
    )) as rec:
        answer = call_your_agent(item.input)  # your code: returns the model output
        rec.set_result(Generation(
            model=ModelRef(provider="openai", name="gpt-4o-mini"),
            input=[user_text_message(str(item.input))],
            output=[assistant_text_message(answer)],
        ))
    return TargetResult(output=answer)  # generation ids captured automatically

def exact_match(item, result):
    passed = str(item.expected).lower() in str(result.output).lower()
    return [ScoreOutput(
        evaluator_id="my_suite.exact_match", evaluator_version="2026-05-30",
        score_key="exact_match", value=ScoreValue(number=1.0 if passed else 0.0),
        passed=passed, explanation=f"expected '{item.expected}', got '{result.output}'",
    )]

runner = ExperimentRunner(
    client=client,
    run_id=f"pr-{os.environ.get('GIT_SHA', 'local')}",  # stable id => idempotent retries
    name="PR experiment",
    dataset={"id": "my_dataset", "version": "2026-05-30"},
    candidate={"git_sha": os.environ.get("GIT_SHA", "local")},
    tags=["ci"],
    agent_name="my-agent",
)
result = runner.run(dataset, target, [exact_match])
print(result.url)
if result.report and result.report.summary.pass_rate < 0.9:
    raise SystemExit(1)  # gate the PR
```

**A/B testing**: run two `ExperimentRunner`s with different `run_id`/`tags`
(e.g. `candidate={"prompt":"v1"}` vs `"v2"`) over the same dataset and scorers,
then compare the two runs in the Sigil UI.

## Pattern 2 — Lower-level context manager

When you want to drive the loop yourself (custom ordering, multiple calls per
item, streaming), use `experiment(...)`:

```python
from sigil_sdk import experiment

with experiment(client=client, run_id="run-123", name="manual loop",
                agent_name="my-agent") as run:
    for item in dataset:
        run.reset_capture(conversation_id=f"conv-{item.id}")  # one conversation per item
        with run.start_generation(GenerationStart(model=ModelRef(provider="openai", name="gpt-4o-mini"))) as rec:
            answer = call_your_agent(item.input)
            rec.set_result(Generation(output=[assistant_text_message(answer)]))
        run.add_scores(my_scores(item, answer), item=item,
                       generation_ids=run.produced_generation_ids)
# on exit: succeeded; on exception: failed; on Ctrl-C: canceled (all automatic)
```

If you record generations somewhere the run can't see (e.g. a provider wrapper),
call `run.track_generation_id(gen_id)` so scores still attach automatically.

## LLM-as-judge scorer (optional)

A judge is just a scorer that calls a model. Record the judge's own call as a
Sigil generation so the grade is auditable:

```python
def llm_judge(item, result):
    prompt = f"Question: {item.input}\nAnswer: {result.output}\nScore 0-1 for correctness."
    with client.start_generation(GenerationStart(
        model=ModelRef(provider="openai", name="gpt-4o-mini"),
        agent_name="judge", operation_name="llm-judge",
    )) as rec:
        verdict = call_your_model(prompt)  # -> {"score": 0.8, "reason": "..."}
        rec.set_result(Generation(
            model=ModelRef(provider="openai", name="gpt-4o-mini"),
            input=[user_text_message(prompt)],
            output=[assistant_text_message(verdict["reason"])],
        ))
    return [ScoreOutput(
        evaluator_id="llm_judge.correctness", evaluator_version="2026-05-30",
        score_key="correctness", value=ScoreValue(number=verdict["score"]),
        passed=verdict["score"] >= 0.5, explanation=verdict["reason"],
    )]
```

## Pattern 3 — Upload an OLD experiment (no first-class importer yet)

The first iteration has **no built-in uploader**. Simulate one by replaying
stored transcripts conversation-by-conversation: create the run, then for each
stored conversation export a generation, flush, and export its score.

```python
from sigil_sdk import (CreateExperimentRequest, Generation, GenerationStart, ModelRef,
                       ScoreItem, ScoreSource, ScoreValue, assistant_text_message,
                       stable_id, user_text_message)

run_id = "backfill-2026-05-30"
client.create_experiment(CreateExperimentRequest(
    run_id=run_id, name="Backfilled run", source="external",
    tags=["backfill"], metadata={"imported_from": "old_results.jsonl"}))

accepted = 0
try:
    for row in load_old_results():  # your stored transcripts + grades
        gen_id = stable_id("gen", run_id, row["id"])
        conv_id = stable_id("conv", run_id, row["id"])
        with client.start_generation(GenerationStart(
            id=gen_id, conversation_id=conv_id,
            model=ModelRef(provider=row["provider"], name=row["model"]),
            agent_name=row.get("agent", "agent"),
            tags={"experiment.run_id": run_id},
            metadata={"experiment_run_id": run_id, "task_id": row["task_id"]},
        )) as rec:
            rec.set_result(Generation(
                id=gen_id, conversation_id=conv_id,
                model=ModelRef(provider=row["provider"], name=row["model"]),
                input=[user_text_message(row["prompt"])],
                output=[assistant_text_message(row["answer"])],
            ))
        client.flush()  # so strict score ingest can find the generation
        resp = client.export_scores([ScoreItem(
            score_id=stable_id("score", run_id, row["id"], "reward"),
            generation_id=gen_id, conversation_id=conv_id, run_id=run_id,
            evaluator_id="backfill.reward", evaluator_version="2026-05-30",
            score_key="reward", value=ScoreValue(number=row["score"]),
            passed=row["score"] >= 0.5, metadata={"task_id": row["task_id"]},
            source=ScoreSource(kind="experiment", id=run_id),
        )])
        accepted += resp.accepted_count
    client.complete_experiment(run_id, "succeeded", score_count=accepted)
except Exception as exc:
    client.complete_experiment(run_id, "failed", score_count=accepted, error=str(exc))
    raise
finally:
    client.shutdown()
```

## Rules / gotchas

- Always record the agent's call through `run.start_generation(...)` (or call
  `run.track_generation_id(...)`) — that is what tags generations with the
  `run_id` and lets scores attach to them.
- Use a **stable `run_id`** (e.g. derived from the git SHA) so CI retries are
  idempotent; score IDs are derived deterministically too.
- Always `flush()` before exporting scores (the runner does this for you).
- Score metadata keys read by the Sigil report: `dataset_id`, `dataset_version`,
  `item_id`, `task_id`, `task_category`, `trial_id`. The runner copies these from
  the run spec and `DatasetItem.metadata` automatically.
- Upload modes: `continuous` (default, publish per item), `bulk` (publish at the
  end), `manual` (publish + finalize only when you call `run.publish()` /
  `run.finalize()`). Users can delete experiments, so the default is to publish.
- The run is finalized automatically: `succeeded` on clean exit, `failed` on
  exception, `canceled` on Ctrl-C.
