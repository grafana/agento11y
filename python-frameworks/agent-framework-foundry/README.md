# Sigil Python Framework Integration: Microsoft Agent Framework + Foundry

`sigil-sdk-agent-framework-foundry` instruments Microsoft Agent Framework agents that use Microsoft Foundry project endpoints.

It covers:

- `Agent(client=FoundryChatClient(...))`
- `FoundryChatClient.get_response(...)`
- `FoundryAgent(...)` for Prompt/Hosted agents managed by Foundry Agent Service
- Agent Framework function/tool middleware

## Installation

```bash
pip install sigil-sdk sigil-sdk-agent-framework-foundry
```

## Ephemeral Foundry agent

```python
from agent_framework import Agent
from agent_framework.foundry import FoundryChatClient
from azure.identity import AzureCliCredential
from sigil_sdk import Client
from sigil_sdk_agent_framework_foundry import create_sigil_foundry_middleware

sigil = Client()

agent = Agent(
    client=FoundryChatClient(
        project_endpoint="https://<resource-name>.services.ai.azure.com/api/projects/<project-name>",
        model="gpt-5.2",
        credential=AzureCliCredential(),
    ),
    name="foundry-demo-agent",
    instructions="You are concise.",
    middleware=create_sigil_foundry_middleware(
        client=sigil,
        conversation_id="conv-1",
        agent_version="1.0.0",
    ),
)

response = await agent.run("Give one reason to instrument AI agents.")
print(response.text)
```

## Service-managed Foundry agent

```python
from agent_framework.foundry import FoundryAgent
from azure.identity import AzureCliCredential
from sigil_sdk_agent_framework_foundry import create_sigil_foundry_middleware

agent = FoundryAgent(
    project_endpoint="https://<resource-name>.services.ai.azure.com/api/projects/<project-name>",
    agent_name="my-hosted-agent",
    credential=AzureCliCredential(),
    middleware=create_sigil_foundry_middleware(
        client=sigil,
        conversation_id="conv-1",
        agent_version="1.0.0",
    ),
)

response = await agent.run("Summarize the latest deployment status.")
```

## Existing objects

```python
from sigil_sdk_agent_framework_foundry import instrument_foundry_agent

instrument_foundry_agent(agent, client=sigil, conversation_id="conv-1")
```

For direct `FoundryChatClient.get_response(...)` calls, either pass the chat/function middleware in the `middleware=` argument or call `instrument_foundry_chat_client(...)`.
