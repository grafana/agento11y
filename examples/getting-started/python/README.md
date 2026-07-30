# Getting Started — Python

Makes an OpenAI chat completion and records the generation to Grafana Cloud Agent Observability.

## Setup

```bash
cd examples/getting-started/python
cp .env.example .env
# Fill in your credentials in .env — see the SDK README for where to find each value.
```

```bash
pip install -r requirements.txt
```

## Run

```bash
python main.py
```

You should see the LLM response printed, followed by `Done`. Open the Agent Observability plugin in your Grafana Cloud stack to see the recorded generation, and check your Grafana Cloud Traces and Metrics datasources for SDK-emitted spans and metrics.
