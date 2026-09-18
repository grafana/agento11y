# Grafana Agent O11y Evals

The default SDK text-evaluation workflow: `input.prompt`, a reviewed
`expected.assistant_response`, and metadata for provenance. `run_evals` validates
all cases before creating a run, invokes your text agent, and publishes one
explicit primary exact-match verdict per trial. Exceptions fail the run;
a wrong answer is a completed trial with a failed verdict.

Exact matching is suitable for short factual outputs, not a universal quality
metric. Use an `EvaluationPlan` for multiple local/stored LLM judges plus
deterministic checks. See the [case and evaluation-plan guide](../../../python/docs/evaluation-plans.md)
and [real-provider mixed-judge example](judged.py). Manual trial APIs and native
DeepEval metrics remain available for advanced workflows.

## Local Docker demonstration

Start the Sigil Docker stack, including its result worker. No provider credentials
or paid model calls are needed. From the SDK root, with the Python package installed:

```sh
PYTHONPATH=python python examples/experiments/grafana/main.py
```

The script uses **only localhost:8080 and localhost:3000**, not environment
endpoints. `local-development` is a dummy credential for the auth-disabled local
server, not a Cloud credential. It publishes a two-case suite and pulls its exact
version, then runs a deliberately broken v1 (50% passing) and corrected v2 (100%)
on that same checkpoint. It records synthetic input/output anchor generations,
not traces of a real LLM. No token/cost values are fabricated. Open the printed
links, then compare the runs in Grafana.

## Your instrumented agent

```python
from agento11y.experiments import TestSuitesClient, run_evals

suite = TestSuitesClient().pull_suite("your-suite", version="v1")
run = run_evals(suite, run_my_agent, candidate={"agent_name": "support", "agent_version": "commit-sha"})
print(run.url)
```

`run_my_agent` receives a prompt string and returns an answer string. Normal SDK
instrumentation inside a trial associates its conversation. `record_io` is off
by default; enable it only to capture input/output anchors for an uninstrumented
target. Do not enable it just to duplicate already-instrumented content.

Use Connection settings for ingest credentials and the separate Grafana
control-plane service account required for suite push/pull. Never store these
in suite JSON, Git, or screenshots.

## Version and source ownership

- Grafana-owned: curate/promote in a draft, review/redact expected answers, publish,
  then pull a pinned checkpoint in the runner.
- Git-owned: review local definitions in PRs, call `push_suite(suite, publish=True)`,
  and run the returned `pushed.suite` (which has the actual published version).
- `suite.to_yaml(path)` exports a pulled suite for review. Push/pull are not
  automatic GitHub synchronization. Pull and reconcile existing draft edits before
  pushing; `prune=True` explicitly deletes remote-only cases and is not the default.
- Batch related edits; publish a checkpoint before a reproducible run. A changed
  agent alone does not need a changed suite. Do not use observed failed responses
  as reference answers without correction.

k6 can use this case convention and the existing lifecycle API from a local
runner. This example does not claim a released k6 library.
