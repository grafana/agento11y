# Go prompt optimizer

This example hill-climbs Grafana Assistant's production unit-inference prompt.
A reasoning model proposes prompt rewrites, the task model runs each rewrite
against 27 embedded fixtures, and the deterministic production-parity scorer
records every attempt through the Go experiments SDK. The search objective is
`Report().Summary.FinalScoreAvg`, read back from Agent Observability after each
candidate experiment is finalized.

The optimizer itself remains example code. The SDK owns experiment, trial,
generation, score, and report lifecycle.

## Run

From the Go experiments module:

```bash
cd examples/experiments/go
cp .env.example .env
set -a && source .env && set +a
GOWORK=off go run ./prompt-optimization
```

The default task model is `claude-haiku-4-5`; the stronger
`claude-sonnet-5` model proposes four candidates for each of two rounds.
Anthropic's OpenAI-compatible endpoint is the default. Any compatible endpoint
works:

```bash
GOWORK=off go run ./prompt-optimization \
  --llm-base-url http://localhost:11434/v1 \
  --model llama3.2 \
  --reasoning-model llama3.2 \
  --model-provider ollama
```

The provider key fallback is `OPENAI_API_KEY`, then `ANTHROPIC_API_KEY`, then
`not-needed` for local servers. Agent Observability ingest uses
`AGENTO11Y_ENDPOINT`, `AGENTO11Y_AUTH_TENANT_ID`, and
`AGENTO11Y_AUTH_TOKEN`.

Useful flags:

- `--start naive|production` chooses a plausible first draft or the hand-tuned
  production prompt.
- `--rounds`, `--candidates`, and `--run-id` control the search.
- `--suite-version v1` links runs to a separately pushed stored suite version.
  Without it, the in-memory suite uses a digest of the embedded fixtures and
  requires no control-plane credentials.
- `--grafana-url` controls the printed comparison link.
- `--record-content` explicitly disables the SDK's default secret redaction so
  full prompts and fixture inputs remain inspectable. This can export sensitive
  content; leave it off unless the dataset is safe to record.

Every candidate gets a unique experiment ID and a SHA-256 prompt version. Task
calls use temperature 0 and a 32-token cap. Reasoning calls omit temperature
and allow 4096 tokens. Generated candidate JSON may be raw, fenced, or wrapped
in prose; explicit system messages are preferred, with the first message used
as a fallback.

## Test

```bash
cd examples/experiments/go
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

The embedded fixture and production prompt files are exact copies from the
original Python prompt-optimization example.
