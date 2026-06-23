# Sigil framework-free experiment example

A minimal, runnable example of an **offline evaluation experiment** using the
core `sigil-sdk` only — **no framework adapter**. It runs a tiny agent over a
small dataset, grades each answer locally, and publishes the results to Sigil as
an experiment you can browse and compare.

Use this shape when your agent is *not* built on a supported framework
(LangGraph, LangChain, OpenAI Agents, ...). For those, prefer the matching
framework example/skill — it auto-captures generation ids from the framework
callback.

| Piece | What it shows | Where |
| --- | --- | --- |
| `ExperimentRunner` | Loops a dataset, runs the target, grades, publishes scores, finalizes the run | `app/run_experiment.py` |
| `run.start_generation(...)` | Records the agent's call tagged with the experiment `run_id`, capturing the generation id | `app/run_experiment.py` (`target`) |
| User scorer | Local grading returning typed `ScoreOutput`s (swap in LLM-as-judge here) | `app/run_experiment.py` (`exact_match_scorer`) |
| Tiny agent | A plain function that answers a question (real model or offline canned answers) | `app/agent.py` |

## How it works

1. The runner calls `POST /api/v1/eval/experiments` to create an `external` run.
2. For each dataset item it runs the agent inside `run.start_generation(...)`, so
   the generation the agent emits is exported through the normal Sigil path and
   tagged with `experiment.run_id`. The runner captures the produced generation
   id for you.
3. It flushes generations, runs your scorer(s), and exports the scores with the
   same `run_id` (`POST /api/v1/scores:export`).
4. When the dataset is done it finalizes the run (`succeeded`/`failed`/`canceled`)
   and prints a deep link.

A/B testing is just two runs with different `run_id`/`tags` over the same items.

## Prerequisites

- Python 3.11+ and [uv](https://docs.astral.sh/uv/)
- Grafana Cloud AI Observability credentials from your stack's Connection page
- Optional: `OPENAI_API_KEY` (without it, deterministic canned answers are used
  so the example runs fully offline)

## Run it

```bash
uv sync

# Grafana Cloud ingest plane: generations and scores.
export SIGIL_ENDPOINT=https://<your-sigil-api-host>
export SIGIL_AUTH_TENANT_ID=<your-tenant-id>
export SIGIL_AUTH_TOKEN=<your-cloud-access-policy-token>

# Grafana Cloud eval control plane: experiment create/update/finalize/report.
export SIGIL_EVAL_ENDPOINT=https://<your-stack>.grafana.net
export SIGIL_EVAL_PATH_PREFIX=/api/plugins/grafana-sigil-app/resources
export SIGIL_EVAL_AUTH_TOKEN=<your-grafana-service-account-token>

# Optional: stable run id for CI retries / a real model
export RUN_ID=experiment-example-$(git rev-parse --short HEAD 2>/dev/null || echo dev)
# export OPENAI_API_KEY=sk-...

uv run python -m app.run_experiment
```

You should see output like:

```
Experiment 'experiment-example-abc123' finished: 3 score(s) accepted.
pass_rate=1.00 mean_score=1.00
View in Sigil: https://<your-stack>.grafana.net/a/grafana-sigil-app/evaluation/experiments/experiment-example-abc123
```

> The deep link is derived from `SIGIL_EVAL_ENDPOINT`. Set
> `SIGIL_EXPERIMENT_URL_TEMPLATE` only if your UI needs a custom URL shape.

## Adapt it

- **Real agent:** replace `app/agent.py` with your agent and have `target` record
  its call via `run.start_generation(...)`. If you record generations elsewhere
  (e.g. a provider wrapper), call `run.track_generation_id(gen_id)` instead.
- **Real grading:** replace `exact_match_scorer` with your own scorer — including
  an LLM-as-judge that itself records a generation (see the
  `python/skills/sigil-experiments/` skill in this repo).
- **CI gate:** inspect `result.report.summary.pass_rate` and exit non-zero to
  fail a pull request.
