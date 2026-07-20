"""OpenAI strict wrapper namespaces for Sigil Python SDK."""

from .provider import (
    ChatCompletionsStreamSummary,
    OpenAIOptions,
    ResponsesStreamSummary,
    chat,
    embeddings,
    responses,
)

__all__ = [
    "ChatCompletionsStreamSummary",
    "OpenAIOptions",
    "ResponsesStreamSummary",
    "chat",
    "embeddings",
    "responses",
]
