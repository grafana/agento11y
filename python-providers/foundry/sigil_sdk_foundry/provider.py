"""Microsoft Foundry helpers built on the OpenAI-compatible project client."""

from __future__ import annotations

import inspect
from collections.abc import AsyncIterator, Iterator, Mapping
from contextlib import asynccontextmanager, contextmanager
from dataclasses import dataclass, field
from typing import Any

from sigil_sdk_openai import OpenAIOptions, ResponsesStreamSummary
from sigil_sdk_openai import responses as openai_responses

_project_endpoint_metadata_key = "azure.foundry.project_endpoint"


@dataclass(slots=True)
class FoundryOptions:
    """Optional Sigil enrichments for Microsoft Foundry wrappers."""

    provider_name: str = "azure_foundry"
    conversation_id: str = ""
    agent_name: str = ""
    agent_version: str = ""
    project_endpoint: str = ""
    tags: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)
    raw_artifacts: bool = False

    def to_openai_options(self) -> OpenAIOptions:
        """Convert Foundry options to the underlying OpenAI-compatible mapper options."""

        metadata = dict(self.metadata)
        if self.project_endpoint:
            metadata.setdefault(_project_endpoint_metadata_key, self.project_endpoint)

        return OpenAIOptions(
            provider_name=self.provider_name,
            conversation_id=self.conversation_id,
            agent_name=self.agent_name,
            agent_version=self.agent_version,
            tags=dict(self.tags),
            metadata=metadata,
            raw_artifacts=self.raw_artifacts,
        )


def create_project_client(endpoint: str, credential: Any | None = None, **kwargs: Any) -> Any:
    """Create an Azure AIProjectClient for a Microsoft Foundry project endpoint."""

    try:
        from azure.ai.projects import AIProjectClient
        from azure.identity import DefaultAzureCredential
    except ModuleNotFoundError as exc:  # pragma: no cover - dependency is installed for package users
        raise ModuleNotFoundError(
            "sigil-sdk-foundry requires azure-ai-projects>=2.0.0 and azure-identity. "
            "Install it with `pip install sigil-sdk-foundry`."
        ) from exc

    return AIProjectClient(endpoint=endpoint, credential=credential or DefaultAzureCredential(), **kwargs)


@contextmanager
def openai_client_from_project(endpoint: str, credential: Any | None = None, **kwargs: Any) -> Iterator[Any]:
    """Yield the OpenAI-compatible client from a Microsoft Foundry project endpoint."""

    project_client = create_project_client(endpoint, credential=credential, **kwargs)
    openai_client = project_client.get_openai_client()

    if hasattr(openai_client, "__enter__"):
        with openai_client as managed_client:
            yield managed_client
        return

    try:
        yield openai_client
    finally:
        close = getattr(openai_client, "close", None)
        if callable(close):
            close()


@asynccontextmanager
async def _async_openai_client_from_project(
    endpoint: str, credential: Any | None = None, **kwargs: Any
) -> AsyncIterator[Any]:
    project_client = create_project_client(endpoint, credential=credential, **kwargs)
    openai_client = project_client.get_openai_client()

    if hasattr(openai_client, "__aenter__"):
        async with openai_client as managed_client:
            yield managed_client
        return

    try:
        yield openai_client
    finally:
        close = getattr(openai_client, "close", None) or getattr(openai_client, "aclose", None)
        if callable(close):
            result = close()
            if inspect.isawaitable(result):
                await result


def _responses_create(
    client: Any,
    openai_client: Any,
    request: Mapping[str, Any],
    options: FoundryOptions | None = None,
) -> Any:
    opts = (options or FoundryOptions()).to_openai_options()
    request_payload = dict(request)

    return openai_responses.create(
        client,
        request_payload,
        lambda provider_request: openai_client.responses.create(**provider_request),
        opts,
    )


async def _responses_create_async(
    client: Any,
    openai_client: Any,
    request: Mapping[str, Any],
    options: FoundryOptions | None = None,
) -> Any:
    opts = (options or FoundryOptions()).to_openai_options()
    request_payload = dict(request)

    async def call(provider_request: Mapping[str, Any]) -> Any:
        result = openai_client.responses.create(**provider_request)
        if inspect.isawaitable(result):
            return await result
        return result

    return await openai_responses.create_async(client, request_payload, call, opts)


def _responses_stream(
    client: Any,
    openai_client: Any,
    request: Mapping[str, Any],
    options: FoundryOptions | None = None,
) -> ResponsesStreamSummary:
    opts = (options or FoundryOptions()).to_openai_options()
    request_payload = {**dict(request), "stream": True}

    return openai_responses.stream(
        client,
        request_payload,
        lambda provider_request: _collect_responses_stream(openai_client.responses.create(**provider_request)),
        opts,
    )


async def _responses_stream_async(
    client: Any,
    openai_client: Any,
    request: Mapping[str, Any],
    options: FoundryOptions | None = None,
) -> ResponsesStreamSummary:
    opts = (options or FoundryOptions()).to_openai_options()
    request_payload = {**dict(request), "stream": True}

    async def call(provider_request: Mapping[str, Any]) -> ResponsesStreamSummary:
        stream = openai_client.responses.create(**provider_request)
        if inspect.isawaitable(stream):
            stream = await stream
        return await _collect_responses_stream_async(stream)

    return await openai_responses.stream_async(client, request_payload, call, opts)


def _responses_create_from_project(
    client: Any,
    endpoint: str,
    request: Mapping[str, Any],
    credential: Any | None = None,
    options: FoundryOptions | None = None,
    **kwargs: Any,
) -> Any:
    foundry_options = _options_with_endpoint(options, endpoint)
    with openai_client_from_project(endpoint, credential=credential, **kwargs) as openai_client:
        return _responses_create(client, openai_client, request, foundry_options)


async def _responses_create_from_project_async(
    client: Any,
    endpoint: str,
    request: Mapping[str, Any],
    credential: Any | None = None,
    options: FoundryOptions | None = None,
    **kwargs: Any,
) -> Any:
    foundry_options = _options_with_endpoint(options, endpoint)
    async with _async_openai_client_from_project(endpoint, credential=credential, **kwargs) as openai_client:
        return await _responses_create_async(client, openai_client, request, foundry_options)


def _responses_stream_from_project(
    client: Any,
    endpoint: str,
    request: Mapping[str, Any],
    credential: Any | None = None,
    options: FoundryOptions | None = None,
    **kwargs: Any,
) -> ResponsesStreamSummary:
    foundry_options = _options_with_endpoint(options, endpoint)
    with openai_client_from_project(endpoint, credential=credential, **kwargs) as openai_client:
        return _responses_stream(client, openai_client, request, foundry_options)


async def _responses_stream_from_project_async(
    client: Any,
    endpoint: str,
    request: Mapping[str, Any],
    credential: Any | None = None,
    options: FoundryOptions | None = None,
    **kwargs: Any,
) -> ResponsesStreamSummary:
    foundry_options = _options_with_endpoint(options, endpoint)
    async with _async_openai_client_from_project(endpoint, credential=credential, **kwargs) as openai_client:
        return await _responses_stream_async(client, openai_client, request, foundry_options)


def _responses_from_request_response(
    request: Mapping[str, Any],
    response: Any,
    options: FoundryOptions | None = None,
) -> Any:
    return openai_responses.from_request_response(
        dict(request), response, (options or FoundryOptions()).to_openai_options()
    )


def _responses_from_stream(
    request: Mapping[str, Any],
    summary: ResponsesStreamSummary,
    options: FoundryOptions | None = None,
) -> Any:
    return openai_responses.from_stream(dict(request), summary, (options or FoundryOptions()).to_openai_options())


def _collect_responses_stream(stream: Any) -> ResponsesStreamSummary:
    events: list[Any] = []
    output_text: list[str] = []
    final_response = None

    try:
        for event in stream:
            events.append(event)
            _capture_stream_event(event, output_text)
            final_response = _final_response_from_event(event, final_response)
    finally:
        close = getattr(stream, "close", None)
        if callable(close):
            close()

    return ResponsesStreamSummary(final_response=final_response, events=events, output_text="".join(output_text))


async def _collect_responses_stream_async(stream: Any) -> ResponsesStreamSummary:
    events: list[Any] = []
    output_text: list[str] = []
    final_response = None

    try:
        async for event in stream:
            events.append(event)
            _capture_stream_event(event, output_text)
            final_response = _final_response_from_event(event, final_response)
    finally:
        close = getattr(stream, "close", None) or getattr(stream, "aclose", None)
        if callable(close):
            result = close()
            if inspect.isawaitable(result):
                await result

    return ResponsesStreamSummary(final_response=final_response, events=events, output_text="".join(output_text))


def _capture_stream_event(event: Any, output_text: list[str]) -> None:
    if _read(event, "type") != "response.output_text.delta":
        return
    delta = _read(event, "delta")
    if isinstance(delta, str):
        output_text.append(delta)


def _final_response_from_event(event: Any, current: Any) -> Any:
    response = _read(event, "response")
    if response is None:
        return current

    event_type = _read(event, "type")
    if event_type in {"response.completed", "response.incomplete", "response.failed"}:
        return response
    return current


def _options_with_endpoint(options: FoundryOptions | None, endpoint: str) -> FoundryOptions:
    if options is None:
        return FoundryOptions(project_endpoint=endpoint)
    if options.project_endpoint:
        return options

    return FoundryOptions(
        provider_name=options.provider_name,
        conversation_id=options.conversation_id,
        agent_name=options.agent_name,
        agent_version=options.agent_version,
        project_endpoint=endpoint,
        tags=dict(options.tags),
        metadata=dict(options.metadata),
        raw_artifacts=options.raw_artifacts,
    )


def _read(obj: Any, key: str) -> Any:
    if obj is None:
        return None
    if isinstance(obj, Mapping):
        return obj.get(key)
    return getattr(obj, key, None)


class _ResponsesNamespace:
    """Namespace for Microsoft Foundry Responses API wrappers and mappers."""

    create = staticmethod(_responses_create)
    create_async = staticmethod(_responses_create_async)
    stream = staticmethod(_responses_stream)
    stream_async = staticmethod(_responses_stream_async)
    create_from_project = staticmethod(_responses_create_from_project)
    create_from_project_async = staticmethod(_responses_create_from_project_async)
    stream_from_project = staticmethod(_responses_stream_from_project)
    stream_from_project_async = staticmethod(_responses_stream_from_project_async)
    from_request_response = staticmethod(_responses_from_request_response)
    from_stream = staticmethod(_responses_from_stream)


responses = _ResponsesNamespace()
