# Getting started with live DeepEval evaluations

Run from the SDK repository root. These examples use the current checkout;
install compatible published artifacts only once they contain the live APIs.

The real-agent walkthrough requires a local Sigil stack with Grafana, Alloy,
Tempo and Prometheus (ports 8080, 3000 and 4318), Docker, `uv`, and an
`ANTHROPIC_API_KEY` in your environment. It makes **paid model calls**.
It uses ten reviewed ledger questions and actual provider responses, not canned
answers. Telemetry goes to the local stack; model requests go to Anthropic.

Set `HARBOR_CHECKOUT` to a compatible Harbor checkout. Use a fresh batch name
on every invocation; finalized runs cannot be overwritten.

## Run the instrumented agent

```sh
uv run --project "$HARBOR_CHECKOUT" \
  --with-editable ./python --with-editable ./python-providers/anthropic \
  --with-editable ./python-frameworks/deepeval --with-editable ./python-frameworks/harbor \
  --with opentelemetry-exporter-otlp-proto-http --with python-dotenv \
  python examples/experiments/real-agent/run.py deepeval \
  --model haiku --batch deepeval-getting-started-1 \
  --results /tmp/deepeval-getting-started-1
```

Repeat with `--model sonnet` and the same batch/results values for the other
candidate. The model labels are resolved in the shared example; inspect its
configuration before running if you need different available model IDs.

The live runner invokes the instrumented agent inside each trial and grades its
output with DeepEval's native exact-match metric. Stable case IDs align both
candidates. It does not wait for a completed native batch before creating the run.

## Inspect the result

Open the printed Grafana URL and sign in. Expect ten planned trials, independently
captured model outputs, and linked conversations. Quality is measured, not
guaranteed: do not assume every answer will pass. Compare actual tokens with
the captured provider responses in the results directory. Cost is calculated
from usage and the example's documented rate card, not a billing invoice.

The [shared walkthrough](../real-agent/README.md) documents dependencies,
measurement scope, and the source files.
Start with [the live entry point](../real-agent/run.py) and [the instrumented agent](../real-agent/agent.py).

## Export completed results instead

The existing [main.py](main.py) and [README](README.md) demonstrate native
batch evaluation followed by publication. Use `publish_deepeval_results` for an
already evaluated result; this does not reconstruct historical live progress.
