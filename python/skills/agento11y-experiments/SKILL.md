---
name: agento11y-experiments
description: >-
  Run any Python LLM agent as an Agent Observability experiment using the public
  agento11y.experiments package: define a test suite, run an existing agent
  through typed trials, bind or record generation I/O, grade outputs, and
  publish scores, including from stored Grafana test suites.
---

# Agent Observability experiments

Use this skill when adding framework-free offline evaluation to a Python project.
The public SDK surface is `agento11y.experiments`; do not use removed v0 runner
APIs.

This is the reference for the run-side API. If you don't yet know which evaluators
you need or have no test cases, start with the `agento11y-eval-starter` skill — it reads
your agent, recommends evaluators, writes a starter suite, and generates a minimal
runner; come here for the deeper patterns (binding existing generations, auditable
LLM judges, cross-process verifiers, pass@k/pass^k).

The normal setup cost for an already instrumented agent should be small:

1. Import `experiments` from `agento11y`.
2. Define a `TestSuite` with `TestCase`s.
3. Wrap the existing agent call in `with exp.trial(case) as trial:`.
4. Bind the generation/conversation ids your normal instrumentation already
   produced, or call `trial.record_io(...)` when the harness owns the call.
5. Emit one final score and any supporting scores.

## Setup

```bash
pip install "agento11y>=0.11.0"
```

Required environment:

```bash
export AGENTO11Y_ENDPOINT=https://agento11y-prod-<region>.grafana.net
export AGENTO11Y_AUTH_TOKEN=<grafana-cloud-ingestion-api-key>

# Optional when the endpoint requires tenant-scoped basic auth.
export AGENTO11Y_AUTH_TENANT_ID=<stack-id>

# Optional UI host for deep links when it differs from AGENTO11Y_ENDPOINT.
export AGENTO11Y_GRAFANA_URL=https://<your-stack>.grafana.net
```

Local-suite experiment ingest uses only the Cloud ingestion API key. Stored
suite push/pull additionally uses `AGENTO11Y_CONTROL_ENDPOINT` and a Grafana
service-account token in `AGENTO11Y_SERVICE_ACCOUNT_TOKEN`.

Experimental OTel eval spans/events are disabled by default. Opt in only when
asked:

```python
with experiments.experiment("nightly", use_experimental_otel=True) as exp:
    ...
```

## Recommended Pattern

```python
from agento11y import experiments

suite = experiments.TestSuite(
    suite_id="smoke",
    name="Smoke",
    version="2026-06-29",
    test_cases=[
        experiments.TestCase(test_case_id="capital-fr", input="Capital of France?", expected="Paris"),
    ],
)
verifier = experiments.Evaluator(evaluator_id="exact_match", version="2026-06-29", kind="deterministic")

with experiments.experiment(
    "PR experiment",
    experiment_id=f"pr-{git_sha}",
    suite=suite,
    planned_trial_count=len(suite.test_cases),
    candidate={"git_sha": git_sha, "model_name": "gpt-4o-mini"},
    tags=["ci"],
) as exp:
    for case in suite.test_cases:
        with exp.trial(case) as trial:
            answer = call_your_agent(case.input)

            # If normal instrumentation already created a conversation/generation,
            # bind those ids instead of recording duplicate I/O.
            # trial.bind_conversation(conversation_id)
            # trial.bind_generation(generation_id, conversation_id=conversation_id)
            trial.record_io(
                input=case.input,
                output=answer,
                model_provider="openai",
                model_name="gpt-4o-mini",
            )

            passed = str(case.expected).lower() in answer.lower()
            trial.final_score(
                1.0 if passed else 0.0,
                passed=passed,
                explanation=f"expected {case.expected!r}, got {answer!r}",
                evaluator=verifier,
            )

print(exp.url)
```

The context manager upserts the run on enter, creates a typed trial per case,
exports buffered scores when each trial exits, and finalizes the run as
`completed` or `failed`.

Set `planned_trial_count` to the runner's exact post-filter, post-attempt trial
count. Do not derive it from the stored suite when the runner filters cases or
runs multiple attempts. Normal context-manager finalization omits
`score_count`, allowing Agent Observability to use its authoritative stored score count.
Each `(test_case_id, attempt)` pair must be unique within a run; increment
`attempt` for retries.

## Stored Suites

Stored suites use a Grafana service-account token in addition to the ingestion
credential:

```bash
export AGENTO11Y_CONTROL_ENDPOINT=https://<stack>.grafana.net/a/grafana-sigil-app
export AGENTO11Y_SERVICE_ACCOUNT_TOKEN=<grafana-service-account-token>
```

Pull and run the latest published version while preserving exact suite
provenance:

```python
from agento11y import experiments

with experiments.experiment_from_suite(
    "dashboard-regression",
    version="latest_published",
    experiment_id=f"pr-{git_sha}",
) as exp:
    for case in exp.suite.cases:
        with exp.trial(case) as trial:
            answer = call_your_agent(case.input)
            trial.final_score(answer == case.expected)
```

Use `TestSuite.from_yaml(...)` and `TestSuitesClient.push_suite(...)` to manage
portable source-controlled suites. Pushes are additive by default; pass
`prune=True` to delete remote-only draft cases and `publish=True` to publish the
resulting version.

## Scoring

Use `trial.final_score(...)` for the headline result. Add supporting scores with
`trial.check_score(...)`, `trial.rubric_score(...)`, or `trial.score(...)`.

```python
trial.check_score("json_valid", passed=is_valid_json(answer))
trial.rubric_score("helpfulness", 0.82, explanation="Useful but missed one constraint")
```

Locally configured judges do not require a platform evaluator. ``LLMJudge``
executes an injected model callable, and ``trial.evaluate_output`` publishes the grader
transcript and links it to the score automatically.

```python
judge = experiments.LLMJudge(
    evaluator_id="judge.correctness",
    invoke=judge_model.invoke,
    model_provider="anthropic",
    model_name="claude-sonnet-4-5",
    prompt_template="Input: {input}\nExpected: {expected}\nOutput: {output}\nReturn a JSON score.",
)
trial.evaluate_output(judge, input=case.input, expected=case.expected, output=answer)

regex = experiments.RegexJudge(evaluator_id="regex.answer", pattern=r"Paris", score_key="contains_answer")
trial.evaluate_output(regex, input=case.input, output=answer)
```

## Cross-Process Evaluation

Use `TrialRef` when a verifier runs in a separate process or container.

```python
ref = trial.ref
env = ref.to_env()
```

In the verifier:

```python
from agento11y import experiments

client = experiments.Client(
    endpoint=os.environ["AGENTO11Y_ENDPOINT"],
    tenant_id=os.environ.get("AGENTO11Y_AUTH_TENANT_ID", ""),
    ingest_token=os.environ["AGENTO11Y_AUTH_TOKEN"],
)
ref = experiments.TrialRef.from_env()
if ref is None:
    raise RuntimeError("missing experiment trial environment")
trial = experiments.Trial.from_ref(client, ref)
trial.final_score(0.9, passed=True)
trial.close()
```

For all v1 environment and worker-lifecycle changes, see the
[Experiments v2 migration guide](https://github.com/grafana/agento11y/blob/main/docs/experiments-python-v2-migration.md).

## Gotchas

- Use a stable `experiment_id` for CI retries.
- Prefer binding existing conversation/generation ids when the agent is already
  instrumented; use `record_io(...)` when the experiment harness is the only
  instrumentation around the agent call.
- The Grafana UI route is
  `/a/grafana-sigil-app/experiments/runs/{experiment_id}`.
- Catch evaluator failures outside each `with exp.trial(...)` block when one
  malformed judge response should fail only that trial and the run should continue.
