"""Tests for generation sanitization and built-in secret redaction."""

from __future__ import annotations

import logging
from dataclasses import replace
from datetime import timedelta

from agento11y import (
    Client,
    ClientConfig,
    ContentCaptureMode,
    Generation,
    GenerationExportConfig,
    GenerationMode,
    GenerationStart,
    Message,
    MessageRole,
    ModelRef,
    Part,
    PartKind,
    SecretRedactionOptions,
    TokenUsage,
    ToolCall,
    ToolResult,
    create_secret_redaction_sanitizer,
)
from agento11y.redaction import _resolve_redact_input_messages
from conftest import CapturingGenerationExporter
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import StatusCode

_API_KEY = "sk-proj-" + "a" * 48


def _new_client(
    exporter: CapturingGenerationExporter,
    *,
    generation_sanitizer=None,
    logger: logging.Logger | None = None,
    tracer=None,
    content_capture: ContentCaptureMode = ContentCaptureMode.DEFAULT,
) -> Client:
    return Client(
        ClientConfig(
            tracer=tracer,
            generation_export=GenerationExportConfig(
                batch_size=10,
                flush_interval=timedelta(seconds=60),
                queue_size=10,
                max_retries=1,
                initial_backoff=timedelta(milliseconds=1),
                max_backoff=timedelta(milliseconds=1),
            ),
            generation_exporter=exporter,
            content_capture=content_capture,
            generation_sanitizer=generation_sanitizer,
            logger=logger,
        )
    )


def test_secret_redaction_sanitizer_redacts_assistant_and_tool_content_by_default() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(exporter, generation_sanitizer=create_secret_redaction_sanitizer())

    try:
        secret_token = "glc_abcdefghijklmnopqrstuvwxyz1234"
        env_secret = "DATABASE_PASSWORD=hunter2secret123"
        bearer_token = "a" * 30

        rec = client.start_generation(
            GenerationStart(
                model=ModelRef(provider="openai", name="gpt-5"),
            )
        )
        rec.set_result(
            Generation(
                input=[
                    Message(
                        role=MessageRole.USER,
                        parts=[Part(kind=PartKind.TEXT, text=f"user pasted {secret_token}")],
                    )
                ],
                output=[
                    Message(
                        role=MessageRole.ASSISTANT,
                        parts=[
                            Part(kind=PartKind.TEXT, text=f"assistant saw {secret_token}"),
                            Part(kind=PartKind.THINKING, thinking=f"thinking about {secret_token}"),
                            Part(
                                kind=PartKind.TOOL_CALL,
                                tool_call=ToolCall(
                                    name="bash",
                                    id="call-1",
                                    input_json=f'{{"header":"Bearer {bearer_token}","env":"{env_secret}"}}'.encode(),
                                ),
                            ),
                        ],
                    ),
                    Message(
                        role=MessageRole.TOOL,
                        parts=[
                            Part(
                                kind=PartKind.TOOL_RESULT,
                                tool_result=ToolResult(
                                    tool_call_id="call-1",
                                    name="bash",
                                    content=f"output {env_secret}",
                                ),
                            )
                        ],
                    ),
                ],
                usage=TokenUsage(input_tokens=1, output_tokens=1),
            )
        )
        rec.end()

        assert rec.err() is None
        generation = rec.last_generation
        assert generation is not None
        assert "glc_" in generation.input[0].parts[0].text
        assert "glc_" not in generation.output[0].parts[0].text
        assert "[REDACTED:grafana-cloud-token]" in generation.output[0].parts[0].text
        assert "glc_" not in generation.output[0].parts[1].thinking
        assert "hunter2secret123" not in generation.output[0].parts[2].tool_call.input_json.decode("utf-8")
        assert "Bearer " not in generation.output[0].parts[2].tool_call.input_json.decode("utf-8")
        assert "[REDACTED:" in generation.output[0].parts[2].tool_call.input_json.decode("utf-8")
        assert "hunter2secret123" not in generation.output[1].parts[0].tool_result.content
        assert "[REDACTED:env-secret-value]" in generation.output[1].parts[0].tool_result.content
    finally:
        client.shutdown()


def test_secret_redaction_sanitizer_redacts_email_addresses_by_default() -> None:
    sanitizer = create_secret_redaction_sanitizer()
    sanitized = sanitizer(
        Generation(
            id="gen-1",
            mode=GenerationMode.SYNC,
            operation_name="generateText",
            model=ModelRef(provider="openai", name="gpt-5"),
            output=[
                Message(
                    role=MessageRole.ASSISTANT,
                    parts=[Part(kind=PartKind.TEXT, text="Send me an email to example@example.com")],
                )
            ],
        )
    )

    assert "example@example.com" not in sanitized.output[0].parts[0].text
    assert "[REDACTED:email]" in sanitized.output[0].parts[0].text


def test_secret_redaction_sanitizer_can_leave_email_addresses_enabled_off() -> None:
    sanitizer = create_secret_redaction_sanitizer(SecretRedactionOptions(redact_email_addresses=False))
    sanitized = sanitizer(
        Generation(
            id="gen-1",
            mode=GenerationMode.SYNC,
            operation_name="generateText",
            model=ModelRef(provider="openai", name="gpt-5"),
            output=[
                Message(
                    role=MessageRole.ASSISTANT,
                    parts=[Part(kind=PartKind.TEXT, text="Send me an email to example@example.com")],
                )
            ],
        )
    )

    assert sanitized.output[0].parts[0].text == "Send me an email to example@example.com"


def test_secret_redaction_sanitizer_can_redact_user_input() -> None:
    sanitizer = create_secret_redaction_sanitizer(SecretRedactionOptions(redact_input_messages=True))
    sanitized = sanitizer(
        Generation(
            id="gen-1",
            mode=GenerationMode.SYNC,
            operation_name="generateText",
            model=ModelRef(provider="openai", name="gpt-5"),
            input=[
                Message(
                    role=MessageRole.USER,
                    parts=[
                        Part(
                            kind=PartKind.TEXT,
                            text="key sk-proj-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                        )
                    ],
                )
            ],
        )
    )

    assert "sk-proj-" not in sanitized.input[0].parts[0].text
    assert "[REDACTED:openai-project-key]" in sanitized.input[0].parts[0].text


def test_secret_redaction_sanitizer_redacts_tool_and_assistant_input_when_opted_in() -> None:
    sanitizer = create_secret_redaction_sanitizer(SecretRedactionOptions(redact_input_messages=True))
    secret_token = "glc_abcdefghijklmnopqrstuvwxyz1234"
    env_secret = "DATABASE_PASSWORD=hunter2secret123"
    bearer_token = "a" * 30

    sanitized = sanitizer(
        Generation(
            id="gen-1",
            mode=GenerationMode.SYNC,
            operation_name="generateText",
            model=ModelRef(provider="openai", name="gpt-5"),
            input=[
                Message(
                    role=MessageRole.USER,
                    parts=[Part(kind=PartKind.TEXT, text=f"user pasted {secret_token}")],
                ),
                Message(
                    role=MessageRole.ASSISTANT,
                    parts=[
                        Part(kind=PartKind.TEXT, text=f"assistant response with {secret_token}"),
                        Part(
                            kind=PartKind.TOOL_CALL,
                            tool_call=ToolCall(
                                name="bash",
                                id="call-1",
                                input_json=f'{{"header":"Bearer {bearer_token}"}}'.encode(),
                            ),
                        ),
                    ],
                ),
                Message(
                    role=MessageRole.TOOL,
                    parts=[
                        Part(
                            kind=PartKind.TOOL_RESULT,
                            tool_result=ToolResult(
                                tool_call_id="call-1",
                                name="bash",
                                content=f"output {env_secret}",
                            ),
                        )
                    ],
                ),
            ],
        )
    )

    assert "glc_" not in sanitized.input[0].parts[0].text
    assert "[REDACTED:grafana-cloud-token]" in sanitized.input[0].parts[0].text

    assert "glc_" not in sanitized.input[1].parts[0].text
    assert "[REDACTED:grafana-cloud-token]" in sanitized.input[1].parts[0].text

    assert "Bearer " not in sanitized.input[1].parts[1].tool_call.input_json.decode("utf-8")
    assert "[REDACTED:bearer-token]" in sanitized.input[1].parts[1].tool_call.input_json.decode("utf-8")

    assert "hunter2secret123" not in sanitized.input[2].parts[0].tool_result.content
    assert "[REDACTED:env-secret-value]" in sanitized.input[2].parts[0].tool_result.content


def test_generation_sanitizer_failure_falls_back_to_metadata_only(caplog) -> None:
    exporter = CapturingGenerationExporter()
    logger = logging.getLogger("agento11y.test_redaction")
    client = _new_client(
        exporter,
        generation_sanitizer=lambda _generation: (_ for _ in ()).throw(RuntimeError("boom")),
        logger=logger,
    )

    try:
        with caplog.at_level(logging.WARNING, logger=logger.name):
            rec = client.start_generation(
                GenerationStart(
                    model=ModelRef(provider="openai", name="gpt-5"),
                    conversation_title="Top secret title",
                    system_prompt="system secret",
                )
            )
            rec.set_result(
                Generation(
                    input=[Message(role=MessageRole.USER, parts=[Part(kind=PartKind.TEXT, text="hello")])],
                    output=[Message(role=MessageRole.ASSISTANT, parts=[Part(kind=PartKind.TEXT, text="world")])],
                    usage=TokenUsage(input_tokens=1, output_tokens=1),
                )
            )
            rec.end()

        assert rec.err() is None
        generation = rec.last_generation
        assert generation is not None
        assert generation.metadata["agento11y.sdk.content_capture_mode"] == "metadata_only"
        assert generation.conversation_title == ""
        assert generation.system_prompt == ""
        assert generation.input[0].parts[0].text == ""
        assert generation.output[0].parts[0].text == ""
        assert "agento11y: generation sanitization failed, falling back to metadata_only" in caplog.text
    finally:
        client.shutdown()


def _new_span_capture() -> tuple[InMemorySpanExporter, TracerProvider]:
    span_exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(span_exporter))
    return span_exporter, provider


def _raised_call_error() -> RuntimeError:
    """Returns a raised error whose cause also carries the key, as a wrapped provider error does."""
    try:
        try:
            raise ValueError(f"upstream rejected {_API_KEY}")
        except ValueError as cause:
            raise RuntimeError(f"API error: invalid key {_API_KEY}") from cause
    except RuntimeError as error:
        return error


def _span_text(span) -> str:
    return repr((dict(span.attributes), span.status.description, [dict(event.attributes) for event in span.events]))


def test_secret_redaction_sanitizer_also_redacts_the_title_and_call_error_copies_in_metadata() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(exporter, generation_sanitizer=create_secret_redaction_sanitizer())

    try:
        rec = client.start_generation(
            GenerationStart(
                model=ModelRef(provider="openai", name="gpt-5"),
                conversation_title=f"Discussion about {_API_KEY}",
            )
        )
        rec.set_call_error(RuntimeError(f"API error: invalid key {_API_KEY}"))
        rec.end()
        client.flush()

        generation = exporter.requests[0].generations[0]
        assert "[REDACTED:openai-project-key]" in generation.conversation_title
        assert "[REDACTED:openai-project-key]" in generation.call_error
        assert generation.metadata["agento11y.conversation.title"] == generation.conversation_title
        assert generation.metadata["call_error"] == generation.call_error
        assert _API_KEY not in repr(generation)
    finally:
        client.shutdown()


def test_generation_sanitizer_that_clears_the_title_and_call_error_drops_their_metadata_copies() -> None:
    exporter = CapturingGenerationExporter()
    client = _new_client(
        exporter,
        generation_sanitizer=lambda generation: replace(generation, conversation_title="", call_error=""),
    )

    try:
        rec = client.start_generation(
            GenerationStart(model=ModelRef(provider="openai", name="gpt-5"), conversation_title="Private title")
        )
        rec.set_call_error(RuntimeError("private error"))
        rec.end()
        client.flush()

        generation = exporter.requests[0].generations[0]
        assert generation.conversation_title == ""
        assert generation.call_error == ""
        assert "agento11y.conversation.title" not in generation.metadata
        assert "call_error" not in generation.metadata
    finally:
        client.shutdown()


def test_secret_redaction_sanitizer_also_redacts_the_title_and_call_error_on_the_span() -> None:
    span_exporter, provider = _new_span_capture()
    client = _new_client(
        CapturingGenerationExporter(),
        generation_sanitizer=create_secret_redaction_sanitizer(),
        tracer=provider.get_tracer("agento11y-test"),
    )

    try:
        rec = client.start_generation(
            GenerationStart(
                model=ModelRef(provider="openai", name="gpt-5"),
                conversation_title=f"Discussion about {_API_KEY}",
            )
        )
        rec.set_call_error(_raised_call_error())
        rec.end()

        span = span_exporter.get_finished_spans()[0]
        assert "[REDACTED:openai-project-key]" in span.attributes["agento11y.conversation.title"]
        assert span.status.status_code == StatusCode.ERROR
        assert "[REDACTED:openai-project-key]" in span.status.description
        assert len(span.events) == 1
        event = span.events[0]
        assert event.attributes["exception.type"] == "RuntimeError"
        assert "[REDACTED:openai-project-key]" in event.attributes["exception.message"]
        assert "_raised_call_error" in event.attributes["exception.stacktrace"]
        assert _API_KEY not in _span_text(span)
    finally:
        client.shutdown()
        provider.shutdown()


def test_without_a_sanitizer_the_span_keeps_the_raw_call_error_and_full_stacktrace() -> None:
    span_exporter, provider = _new_span_capture()
    client = _new_client(CapturingGenerationExporter(), tracer=provider.get_tracer("agento11y-test"))

    try:
        rec = client.start_generation(GenerationStart(model=ModelRef(provider="openai", name="gpt-5")))
        error = _raised_call_error()
        rec.set_call_error(error)
        rec.end()

        span = span_exporter.get_finished_spans()[0]
        assert span.status.description == str(error)
        assert len(span.events) == 1
        event = span.events[0]
        assert event.attributes["exception.message"] == str(error)
        assert event.attributes["exception.stacktrace"].startswith("Traceback")
        assert "upstream rejected" in event.attributes["exception.stacktrace"]
    finally:
        client.shutdown()
        provider.shutdown()


def test_generation_sanitizer_that_clears_the_title_and_call_error_leaves_neither_on_the_span() -> None:
    span_exporter, provider = _new_span_capture()
    client = _new_client(
        CapturingGenerationExporter(),
        generation_sanitizer=lambda generation: replace(generation, conversation_title="", call_error=""),
        tracer=provider.get_tracer("agento11y-test"),
    )

    try:
        rec = client.start_generation(
            GenerationStart(model=ModelRef(provider="openai", name="gpt-5"), conversation_title="Private title")
        )
        rec.set_call_error(RuntimeError("private error"))
        rec.end()

        span = span_exporter.get_finished_spans()[0]
        assert span.attributes["agento11y.conversation.title"] == ""
        assert span.status.status_code == StatusCode.ERROR
        assert span.status.description
        assert "Private title" not in _span_text(span)
        assert "private error" not in _span_text(span)
    finally:
        client.shutdown()
        provider.shutdown()


def test_generation_sanitizer_that_clears_the_title_adds_no_title_to_a_full_with_metadata_spans_span() -> None:
    span_exporter, provider = _new_span_capture()
    client = _new_client(
        CapturingGenerationExporter(),
        generation_sanitizer=lambda generation: replace(generation, conversation_title=""),
        tracer=provider.get_tracer("agento11y-test"),
        content_capture=ContentCaptureMode.FULL_WITH_METADATA_SPANS,
    )

    try:
        rec = client.start_generation(
            GenerationStart(model=ModelRef(provider="openai", name="gpt-5"), conversation_title="Private title")
        )
        rec.end()

        span = span_exporter.get_finished_spans()[0]
        assert "agento11y.conversation.title" not in span.attributes
    finally:
        client.shutdown()
        provider.shutdown()


def test_generation_sanitizer_leaves_no_title_on_the_span_when_the_generation_had_none() -> None:
    span_exporter, provider = _new_span_capture()
    client = _new_client(
        CapturingGenerationExporter(),
        generation_sanitizer=create_secret_redaction_sanitizer(),
        tracer=provider.get_tracer("agento11y-test"),
    )

    try:
        rec = client.start_generation(GenerationStart(model=ModelRef(provider="openai", name="gpt-5")))
        rec.end()

        span = span_exporter.get_finished_spans()[0]
        assert "agento11y.conversation.title" not in span.attributes
    finally:
        client.shutdown()
        provider.shutdown()


def test_resolve_redact_input_messages_defaults_to_false() -> None:
    assert _resolve_redact_input_messages(None, env={}) is False


def test_resolve_redact_input_messages_explicit_wins_over_env() -> None:
    assert _resolve_redact_input_messages(False, env={"AGENTO11Y_REDACT_INPUT_MESSAGES": "true"}) is False
    assert _resolve_redact_input_messages(True, env={"AGENTO11Y_REDACT_INPUT_MESSAGES": "false"}) is True


def test_resolve_redact_input_messages_truthy_tokens() -> None:
    for token in ("1", "true", "TRUE", "yes", "on"):
        got = _resolve_redact_input_messages(None, env={"AGENTO11Y_REDACT_INPUT_MESSAGES": token})
        assert got is True, f"token={token!r}"


def test_resolve_redact_input_messages_falsy_tokens() -> None:
    for token in ("0", "false", "FALSE", "no", "off"):
        got = _resolve_redact_input_messages(None, env={"AGENTO11Y_REDACT_INPUT_MESSAGES": token})
        assert got is False, f"token={token!r}"


def test_resolve_redact_input_messages_blank_falls_back_to_false() -> None:
    assert _resolve_redact_input_messages(None, env={"AGENTO11Y_REDACT_INPUT_MESSAGES": "   "}) is False


def test_resolve_redact_input_messages_invalid_warns_and_falls_back(caplog) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        got = _resolve_redact_input_messages(None, env={"AGENTO11Y_REDACT_INPUT_MESSAGES": "maybe"})
    assert got is False
    assert any(
        "AGENTO11Y_REDACT_INPUT_MESSAGES" in record.getMessage() and "maybe" in record.getMessage()
        for record in caplog.records
    )


def test_resolve_redact_input_messages_reads_legacy_namespace(caplog) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        assert _resolve_redact_input_messages(None, env={"SIGIL_REDACT_INPUT_MESSAGES": "true"}) is True
    assert any("SIGIL_REDACT_INPUT_MESSAGES is deprecated" in record.getMessage() for record in caplog.records)


def test_resolve_redact_input_messages_canonical_wins_over_legacy() -> None:
    env = {"AGENTO11Y_REDACT_INPUT_MESSAGES": "false", "SIGIL_REDACT_INPUT_MESSAGES": "true"}
    assert _resolve_redact_input_messages(None, env=env) is False


def test_resolve_redact_input_messages_invalid_warns_with_legacy_key(caplog) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        assert _resolve_redact_input_messages(None, env={"SIGIL_REDACT_INPUT_MESSAGES": "maybe"}) is False
    assert any("ignoring invalid SIGIL_REDACT_INPUT_MESSAGES" in record.getMessage() for record in caplog.records)


def test_create_secret_redaction_sanitizer_reads_env(monkeypatch) -> None:
    monkeypatch.setenv("AGENTO11Y_REDACT_INPUT_MESSAGES", "true")
    sanitizer = create_secret_redaction_sanitizer()
    generation = Generation(
        id="gen-env",
        mode=GenerationMode.SYNC,
        operation_name="generateText",
        model=ModelRef(provider="openai", name="gpt-5"),
        input=[
            Message(
                role=MessageRole.USER,
                parts=[Part(kind=PartKind.TEXT, text="token glc_abcdefghijklmnopqrstuvwxyz0123")],
            )
        ],
    )
    sanitized = sanitizer(generation)
    assert "glc_" not in sanitized.input[0].parts[0].text
    assert "[REDACTED:grafana-cloud-token]" in sanitized.input[0].parts[0].text
