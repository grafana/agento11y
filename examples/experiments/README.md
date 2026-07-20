# Experiments (offline evals)

These examples run an agent over a fixed dataset, grade each answer, and publish
the results to Grafana AI Observability as an **experiment** you can browse and
compare. This is offline evaluation: you control the inputs, run them in a batch,
and score the outputs, rather than scoring live production traffic.

An experiment run has four parts:

1. **Dataset** — a list of items, each with an input and (optionally) an expected
   answer.
2. **Target** — the agent or function under test. It runs once per item and
   records or binds a generation for the experiment.
3. **Scorer** — grading that turns each output into a score. The examples
   use an exact-match scorer; swap in an LLM-as-judge for real grading.
4. **Publish** — the SDK creates the run (`POST /api/v1/experiment-runs:upsert`),
   exports the scores against the same experiment id (`POST /api/v1/scores:export`),
   finalizes the run, and prints a deep link in your stack.

A/B testing is just two runs with different experiment ids or tags over the same dataset.

| Example | Stack | Where |
| --- | --- | --- |
| Framework-free | Python + core `agento11y` | [`python/`](python/) |
| Framework-free | Go + `agento11y/go` | [`go/`](go/) |

For credentials, see the [credentials section in the repo README](../../README.md#grafana-cloud-credentials).
Each example's own README covers the run command and the canned-vs-real-model
behavior.
