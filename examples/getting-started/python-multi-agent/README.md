# Getting Started — Multi-Agent Dependency Graph (Python)

Shows how to use `parent_generation_ids` to declare dependencies between agents. Two agents run independently, then a third combines their outputs:

```
researcher ──┐
             ├──► synthesizer
critic ──────┘
```

## Setup

```bash
cd examples/getting-started/python-multi-agent
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

Open the Agent Observability plugin in your Grafana Cloud stack. In the conversation detail you should see:

- Three generations, each with its own `agent_name`.
- The **Graph** tab showing `researcher` and `critic` as root nodes, with `synthesizer` depending on both.
