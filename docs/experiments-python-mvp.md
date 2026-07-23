# Experiments Python SDK MVP

Status: release candidate design, July 2026

## Executive summary

The Experiments SDK is a tracking integration for work executed by another
system. Agent Observability records runs, trials, scores, usage, artifacts, and links to
the telemetry that explains a result. It does not own the experiment loop, a
universal transcript representation, or an evaluator runtime.

This boundary lets Harbor, LangGraph, CI jobs, notebooks, and custom benchmark
runners keep their native execution and transcript models. They integrate by
publishing stable identifiers and results rather than converting their runtime
state into an Agent Observability-owned format.

## Decisions that define the public contract

### Tracking is the stable core

The stable primitives are:

- An experiment run with optional suite and candidate provenance.
- A test-case trial identified by case and attempt.
- Links to existing conversations, generations, traces, spans, and artifacts.
- Scores with evaluator identity, verdict, explanation, and optional grader
  references.
- Explicit lifecycle, planned trial count, usage, and cost recording.

An external runner can use these primitives without stored test suites or
Agent Observability-managed evaluators.

### Frameworks own transcripts

Agent Observability does not define a canonical transcript or retrieve a conversation to
execute a local judge. A framework adapter may render its native trajectory for
an evaluator, then submit an `EvaluationResult` through
`trial.record_evaluation(...)`.

`trial.evaluate_output(...)` is deliberately narrower. It grades only the
`input`, `output`, and `expected` values supplied by the caller. Its name makes
that limitation part of the API instead of implying full-conversation grading.

### Correlation is the interoperability layer

Conversation, generation, trace, span, trial, case, run, and evaluator IDs are
the durable integration surface. They can be produced in different processes
and transported through `TrialRef` without sharing Python object models.

### Stored suites are optional control-plane enrichment

Local `TestSuite` values work with ingestion credentials alone. Pulling,
pushing, or publishing a stored suite additionally requires the Grafana control
endpoint and service-account credential. `experiment_from_suite(...)` keeps
that credential split explicit and snapshots the selected suite version into
the run and its trials.

### Usage follows the generation that incurred it

Candidate and grader model calls are published as generations with model and
token usage. Scores link to grader generations for provenance. Agent Observability derives
missing trial usage and cost only from the trial's candidate conversation;
grader usage is not folded into that fallback because doing so can double-count
judge work. Runners that need combined candidate-and-judge accounting must
record that trial usage or cost explicitly.

### The runner owns the execution plan

`planned_trial_count` is an optional immutable count supplied when the run is
created. It is the runner's post-filter, post-attempt plan; Agent Observability does not
infer it from a stored suite because suite size does not describe selection,
retries, or multiple attempts.

Finalization closes result-bearing writes and starts asynchronous result
projection. The SDK therefore flushes scores and terminal trial state before
finalizing. Normal finalization omits `score_count` so Agent Observability uses its
authoritative stored count; callers can pass an explicit count as an assertion.

### OpenTelemetry remains an adapter

The SDK's internal run/trial API is not defined by a draft semantic convention.
Experimental OTel emission is opt-in and isolated so finalized `test.*` and
`gen_ai.evaluation.*` conventions can be adopted without changing runner-facing
tracking APIs.

## MVP release surface

- Create and finalize external experiment runs with an optional planned trial
  count.
- Record multiple attempts per test case.
- Attach local cases or an exact stored-suite version.
- Record candidate agent/model identity.
- Bind trials to conversations, generations, traces, and spans.
- Publish numeric, boolean, or string scores with evaluator provenance.
- Link and publish grader generations.
- Record artifacts, duration, tokens, and cost.
- Push, pull, version, and publish portable YAML test suites.
- Use canonical `AGENTO11Y_*` configuration only.
- Use local output evaluators as optional convenience helpers.

## Deferred beyond the MVP

- Agent Observability as an experiment scheduler or runner.
- A universal conversation or trajectory schema.
- Fetching platform-managed evaluator definitions into the SDK.
- Automatic transcript reconstruction from stored telemetry.
- First-class adapters for every benchmark framework. Harbor and other adapters
  should remain thin reporters over their native result models.
- A stable OTel experiment implementation before the upstream conventions are
  finalized.
- Cross-language parity for local evaluator conveniences. Tracking contract
  parity remains the priority.
- Advanced evaluator orchestration such as concurrency, retries, ensembles,
  rubric pipelines, and model routing.

These deferred items can be added through adapters and optional helpers without
changing the tracking primitives above.
