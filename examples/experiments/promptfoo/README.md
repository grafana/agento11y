# Promptfoo experiment example

This example runs a deterministic local Promptfoo provider, evaluates two test
cases, and publishes the completed result set to Agent Observability. It needs no
LLM API key. The second answer is deliberately wrong: expect 50% passing and a
nonzero Promptfoo exit code for the failed assertion, even when publishing succeeds.

## Local Docker run

After installing dependencies and building the SDK, run:

```sh
cd examples/experiments/promptfoo
pnpm install --ignore-workspace --frozen-lockfile
AGENTO11Y_LOCAL_DEMO=true PROMPTFOO_DISABLE_TELEMETRY=1 pnpm eval
```

The extension uses explicit localhost:8080 ingest/control clients and a
localhost:3000 report link. It publishes the native vars/assertions as browsable
cases, marks the suite `framework:promptfoo`, and pins all candidates in one
evaluation to the same published checkpoint. No model calls are made.

Pass `testSuitesClient: new TestSuitesClient()` in your extension to opt into
stored suite publication, and set `suitePublicationPolicy` to `subset` to retain
remote-only cases or `replace` to prune them. The example uses `subset`.
Cloud stored-suite publication requires the separate control-plane service
account. Without `testSuitesClient`, the adapter publishes trial snapshots and
suite provenance, not stored suites. Candidate prompt text stays on runs, not in
the shared suite input; repeated trials share one stored native case definition.

## Cloud configuration

```bash
cd examples/experiments/promptfoo
cp .env.example .env
# Fill in the Agent Observability values.
set -a && source .env && set +a
pnpm install --ignore-workspace --frozen-lockfile
pnpm eval
```

The extension publishes Promptfoo's aggregate row score as the single
`primary_verdict`. Named assertion scores such as `exact_match` are diagnostics.
Configs with multiple prompt/provider candidates produce one Agent Observability
experiment per candidate so reports remain directly comparable.
It records each Promptfoo request/response as an anchor generation by default.
Set `recordIO: false` in `agento11y-extension.mjs` when the target already emits
instrumented generations; use result metadata keys
`agento11y.conversation_id` and `agento11y.generation_id` to bind those instead.

The example is outside the root pnpm workspace; `--ignore-workspace` installs
its own locked dependencies. Install the root workspace dependencies first
(`pnpm install --frozen-lockfile` from the repository root) for the SDK build.
The local `link:` dependency runs this checkout. In another project, install the
published `@grafana/agento11y` package and remove the `build:sdk` script.
