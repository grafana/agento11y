---
name: agento11y-experiments
description: >-
  Run any Python LLM agent as a Sigil experiment using the public
  agento11y.experiments package: define a test suite, run an existing agent
  through typed trials, bind or record generation I/O, grade outputs, and
  publish scores to Grafana Cloud Sigil with one ingestion API key.
---

# Sigil experiments

Use this skill when adding framework-free offline evaluation to a Python project.
The public SDK surface is `agento11y.experiments`; do not use removed v0 runner
APIs.

This is the reference for the run-side API. If you don't yet know which evaluators
you need or have no test cases, start with the `agento11y-eval-starter` skill — it reads
your agent, recommends evaluators, writes a starter suite, and generates a minimal
runner; come here for the deeper patterns (binding existing generations, auditable
LLM judges, cross-process verifiers, pass@k/pass^k).

The normal setup cost for an already instrumented agent should be small:

1. Import `agento11y.experiments` as `sigil`.
2. Define a `TestSuite` with `TestCase`s.
3. Wrap the existing agent call in `with exp.trial(case) as trial:`.
4. Bind the generation/conversation ids your normal instrumentation already
   produced, or call `trial.record_io(...)` when the harness owns the call.
5. Emit one final score and any supporting scores.

## Setup

```bash
pip install "agento11y>=0.9.0"
```

Required environment:

```bash
export AGENTO11Y_ENDPOINT=https://sigil-prod-<region>.grafana.net
export AGENTO11Y_AUTH_TOKEN=<grafana-cloud-ingestion-api-key>

# Optional when the endpoint requires tenant-scoped basic auth.
export AGENTO11Y_AUTH_TENANT_ID=<stack-id>

# Optional UI host for deep links when it differs from AGENTO11Y_ENDPOINT.
export AGENTO11Y_GRAFANA_URL=https://<your-stack>.grafana.net
```

Experiment ingest uses the Cloud ingestion API key. Do not add a separate
control-plane URL or eval API key.

Experimental OTel eval spans/events are disabled by default. Opt in only when
asked:

```python
with sigil.experiment("nightly", use_experimental_otel=True) as exp:
    ...
```

## Recommended Pattern

```python
from agento11y import experiments as sigil

suite = sigil.TestSuite(
    suite_id="smoke",
    name="Smoke",
    version="2026-06-29",
    test_cases=[
        sigil.TestCase(test_case_id="capital-fr", input="Capital of France?", expected="Paris"),
    ],
)
verifier = sigil.Evaluator(evaluator_id="exact_match", version="2026-06-29", kind="deterministic")

with sigil.experiment(
    "PR experiment",
    experiment_id=f"pr-{git_sha}",
    suite=suite,
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

## Scoring

Use `trial.final_score(...)` for the headline result. Add supporting scores with
`trial.check_score(...)`, `trial.rubric_score(...)`, or `trial.score(...)`.

```python
trial.check_score("json_valid", passed=is_valid_json(answer))
trial.rubric_score("helpfulness", 0.82, explanation="Useful but missed one constraint")
```

An LLM judge is just another model call plus a score. If the judge call should be
auditable, instrument it normally or record it as generation I/O before emitting
the score.

```python
verdict = call_judge(case.input, answer)
trial.rubric_score(
    "correctness",
    verdict["score"],
    passed=verdict["score"] >= 0.7,
    explanation=verdict["reason"],
    evaluator=sigil.Evaluator(evaluator_id="judge.correctness", version="2026-06-29", kind="llm_judge"),
)
```

## Cross-Process Evaluation

Use `TrialRef` when a verifier runs in a separate process or container.

```python
ref = trial.ref
env = ref.to_env()
```

In the verifier:

```python
from agento11y import experiments as sigil

client = sigil.Client(
    endpoint=os.environ["AGENTO11Y_ENDPOINT"],
    tenant_id=os.environ.get("AGENTO11Y_AUTH_TENANT_ID", ""),
    ingest_token=os.environ["AGENTO11Y_AUTH_TOKEN"],
)
ref = sigil.TrialRef.from_env()
if ref is None:
    raise RuntimeError("missing Sigil trial environment")
trial = sigil.Trial.from_ref(client, ref)
trial.final_score(0.9, passed=True)
trial.flush()
```

## Gotchas

- Use a stable `experiment_id` for CI retries.
- Prefer binding existing conversation/generation ids when the agent is already
  instrumented; use `record_io(...)` when the experiment harness is the only
  instrumentation around the agent call.
- Keep “offline evaluation” wording for batch eval workflows and UI routes.
- The Grafana UI route is
  `/a/grafana-sigil-app/offline-experiments/experiments/{experiment_id}`.
