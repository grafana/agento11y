# Sigil Python Provider Helper: Microsoft Foundry

`sigil-sdk-foundry` records calls made through the Microsoft Foundry Python SDK's OpenAI-compatible project client. Microsoft Foundry exposes the Responses API through the project endpoint:

```text
https://<resource-name>.services.ai.azure.com/api/projects/<project-name>
```

## Installation

```bash
pip install sigil-sdk sigil-sdk-foundry
```

## Responses example

```python
from sigil_sdk import Client, ClientConfig
from sigil_sdk_foundry import FoundryOptions, responses

sigil = Client(ClientConfig())

response = responses.create_from_project(
    sigil,
    "https://<resource-name>.services.ai.azure.com/api/projects/<project-name>",
    {
        "model": "gpt-5.2",
        "instructions": "Be concise",
        "input": "What is the size of France in square miles?",
    },
    options=FoundryOptions(
        conversation_id="conv-1",
        agent_name="foundry-agent",
        agent_version="1.0.0",
    ),
)

print(response.output_text)
sigil.shutdown()
```

## Reusing an existing Foundry project client

If your app already owns the `AIProjectClient`, get the OpenAI-compatible client and pass it to the wrapper:

```python
from azure.ai.projects import AIProjectClient
from azure.identity import DefaultAzureCredential
from sigil_sdk_foundry import FoundryOptions, responses

project = AIProjectClient(
    endpoint="https://<resource-name>.services.ai.azure.com/api/projects/<project-name>",
    credential=DefaultAzureCredential(),
)

with project.get_openai_client() as openai_client:
    response = responses.create(
        sigil,
        openai_client,
        {"model": "gpt-5.2", "input": "Summarize this deployment in 3 bullets"},
        FoundryOptions(conversation_id="conv-1", agent_name="foundry-agent"),
    )
```

## Public API

- `create_project_client(endpoint, credential=None, **kwargs)`
- `openai_client_from_project(endpoint, credential=None, **kwargs)`
- `responses.create(...)`
- `responses.create_async(...)`
- `responses.stream(...)`
- `responses.stream_async(...)`
- `responses.create_from_project(...)`
- `responses.create_from_project_async(...)`
- `responses.stream_from_project(...)`
- `responses.stream_from_project_async(...)`
- `responses.from_request_response(...)`
- `responses.from_stream(...)`

The wrappers reuse the strict OpenAI Responses mapper from `sigil-sdk-openai`, but default the Sigil model provider to `azure_foundry` and add `azure.foundry.project_endpoint` metadata when the project-endpoint helper is used.
