# Getting Started — Multi-Agent Dependency Graph (Claude Agent SDK)

The same dependency graph as [`../python-multi-agent`](../python-multi-agent), but
each agent is a [Claude Agent SDK](https://docs.anthropic.com/en/docs/claude-code/sdk)
`query()` run instead of a raw OpenAI call:

```
researcher ──┐
             ├──► synthesizer
critic ──────┘
```

The Claude Agent SDK has **no Sigil framework adapter**, so this example shows
**manual instrumentation with the core SDK**: it drains each `query()` message
stream to collect the assistant text and token usage, then records one
generation per agent with `sigil.start_generation` and links them with
`parent_generation_ids`. The Anthropic `cache_creation_input_tokens` count is
mapped onto Sigil's `cache_write_input_tokens` field.

## Setup

```bash
cd examples/getting-started/python-claude-agent-sdk-multi-agent
cp .env.example .env
# Fill in your credentials in .env — see the SDK README for where to find each value.
```

```bash
pip install -r requirements.txt
```

The Claude Agent SDK requires Python ≥ 3.10 and authenticates the same way as
Claude Code. For headless runs, set `ANTHROPIC_API_KEY` in `.env`.

## Run

```bash
python main.py
```

Open the AI Observability plugin in your Grafana Cloud stack. In the conversation
detail you should see:

- Three generations, each with its own `agent_name`, provider `anthropic`, and
  Claude token usage (including cache read/write when present).
- The **Graph** tab showing `researcher` and `critic` as root nodes, with
  `synthesizer` depending on both.
