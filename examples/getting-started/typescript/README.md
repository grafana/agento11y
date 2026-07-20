# Getting Started — TypeScript

Makes an OpenAI chat completion and records the generation to Grafana Cloud AI Observability.

## Setup

```bash
cd examples/getting-started/typescript
cp .env.example .env
# Fill in your credentials in .env — see the SDK README for where to find each value.
```

```bash
npm install
```

> The `@grafana/agento11y` package is installed from the local monorepo via a `file:` reference. If you're working outside the monorepo, replace it with the published package once available.

## Run

```bash
npx tsx main.ts
```

You should see the LLM response printed, followed by `Done`. Open the AI Observability plugin in your Grafana Cloud stack to see the recorded generation, and check your Grafana Cloud Traces and Metrics datasources for SDK-emitted spans and metrics.
