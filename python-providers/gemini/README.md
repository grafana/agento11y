# Sigil Python Provider Helper: Gemini

`agento11y-gemini` provides strict Gemini Models wrappers and mappers for Sigil.

## Installation

```bash
pip install agento11y agento11y-gemini google-genai
```

## Public API

- Wrappers:
  - `models.generate_content(...)`
  - `models.generate_content_async(...)`
  - `models.generate_content_stream(...)`
  - `models.generate_content_stream_async(...)`
  - `models.embed_content(...)`
  - `models.embed_content_async(...)`
- Mappers:
  - `models.from_request_response(...)`
  - `models.from_stream(...)`
  - `models.embedding_from_response(...)`

## Wrapper Mode (Sync)

```python
from google.genai import types as genai_types
from agento11y import Client, ClientConfig
from agento11y_gemini import GeminiOptions, models

client = Client(ClientConfig())

model = "gemini-2.5-pro"
contents = [genai_types.Content(role="user", parts=[genai_types.Part(text="Hello")])]
config = genai_types.GenerateContentConfig(max_output_tokens=256)

response = models.generate_content(
    client,
    model,
    contents,
    config,
    lambda req_model, req_contents, req_config: gemini_client.models.generate_content(
        model=req_model,
        contents=req_contents,
        config=req_config,
    ),
    GeminiOptions(conversation_id="conv-1", agent_name="assistant", agent_version="1.0.0"),
)
```

## Wrapper Mode (Stream)

```python
from agento11y_gemini import GeminiStreamSummary, models

summary = models.generate_content_stream(
    client,
    model,
    contents,
    config,
    lambda req_model, req_contents, req_config: GeminiStreamSummary(
        responses=list(gemini_client.models.generate_content_stream(
            model=req_model,
            contents=req_contents,
            config=req_config,
        ))
    ),
)
```

## Mapper Mode

```python
generation = models.from_request_response(model, contents, config, response)
stream_generation = models.from_stream(model, contents, config, summary)
```

## Embedding example

```python
embedding_response = models.embed_content(
    client,
    "gemini-embedding-001",
    contents,
    None,
    lambda req_model, req_contents, req_config: gemini_client.models.embed_content(
        model=req_model,
        contents=req_contents,
        config=req_config,
    ),
)
```

## Raw Provider Artifacts (Opt-In)

```python
options = GeminiOptions(raw_artifacts=True)
```

Raw artifacts are default OFF and should only be enabled for diagnostics.

## Provider metadata mapping

Gemini-specific fields are mapped as follows:

- `usage.thoughts_token_count` -> normalized `usage.reasoning_tokens`
- `usage.tool_use_prompt_token_count` -> metadata `sigil.gen_ai.usage.tool_use_prompt_tokens`
- `config.thinking_config.thinking_budget` -> metadata `sigil.gen_ai.request.thinking.budget_tokens`
- `config.thinking_config.thinking_level` -> metadata `sigil.gen_ai.request.thinking.level`
