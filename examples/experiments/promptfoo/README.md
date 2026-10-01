# Promptfoo experiment example

This example runs a deterministic Promptfoo provider, evaluates two test
cases, and publishes the completed result set to Agent Observability. It needs no
LLM API key. The second answer is deliberately wrong: expect 50% passing and a
nonzero Promptfoo exit code for the failed assertion, even when publishing succeeds.

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
The `link:` dependency runs this checkout. In another project, install the
published `@grafana/agento11y` package and remove the `build:sdk` script.

To publish browsable cases to Grafana Cloud, pass a `TestSuitesClient` with
your control-plane service account and set `suitePublicationPolicy` to `subset`
to retain remote-only cases or `replace` to prune them. The example publishes
trial snapshots and suite provenance by default.
