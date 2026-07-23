# Examples

Runnable examples for the Grafana Agent observability SDKs, grouped into three tiers.

## Getting started — [`getting-started/`](getting-started/)

Minimal, self-contained quickstarts that make a real LLM call and record the
generation to Grafana Agent Observability. Pick your language and you should be
running in under five minutes. Covers single-generation quickstarts, hooks and
guards (preflight guard evaluation), and a multi-agent dependency graph. See
[`getting-started/README.md`](getting-started/README.md) for the full list.

## Experiments (offline evals) — [`experiments/`](experiments/)

Offline evaluation examples: run an agent over a fixed dataset, grade each
answer, and publish the results to Grafana Agent Observability as an experiment you
can browse and compare. See [`experiments/README.md`](experiments/README.md) for
what an experiment run is (dataset → target → scorer → publish) and the
per-language examples.

## Reference app — [`python-langchain/`](python-langchain/)

A fuller FastAPI service showing a LangChain agent with framework callbacks and
manual SDK instrumentation side by side. Use this when you want to see the SDK
wired into a real service rather than a single script.

## Credentials

Every example needs your Grafana Cloud credentials (instance ID, API token,
endpoint URL) and, where it calls a provider, an LLM API key. See the
[credentials section in the repo README](../README.md#grafana-cloud-credentials)
for where to find each value.
