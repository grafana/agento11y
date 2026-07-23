# Getting Started - Go Hooks and Guards

Shows how to evaluate agento11y hooks before an LLM call so Grafana Agent Observability guard rules can allow, deny, or transform the request.

Use this pattern when your application owns the provider call and you need guardrails on the critical path. In Agent Observability terminology, the SDK evaluates a **hook**; the policies configured in Grafana Cloud are **guards**.

## Setup

```bash
cd examples/getting-started/go-hooks
cp .env.example .env
# Fill in your credentials in .env - see the SDK README for where to find each value.
go mod tidy
```

Create or enable at least one preflight guard rule in Agent Observability. Good first rules are:

- A transform rule that redacts PII before the provider call.
- A deny rule that blocks prompt-injection attempts or other disallowed input.

## Run

```bash
go run .
```

If the guard allows the request, the example applies any `transformed_input` returned by Agent Observability, calls OpenAI, records the generation, and prints `Done`.

If the guard denies the request, the example catches `HookDeniedError`, prints the rule and reason, and does not call OpenAI.
