"""Tests for generation sanitization and built-in secret redaction."""

from __future__ import annotations

import logging
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


def _new_client(
    exporter: CapturingGenerationExporter,
    *,
    generation_sanitizer=None,
    logger: logging.Logger | None = None,
) -> Client:
    return Client(
        ClientConfig(
            generation_export=GenerationExportConfig(
                batch_size=10,
                flush_interval=timedelta(seconds=60),
                queue_size=10,
                max_retries=1,
                initial_backoff=timedelta(milliseconds=1),
                max_backoff=timedelta(milliseconds=1),
            ),
            generation_exporter=exporter,
            content_capture=ContentCaptureMode.DEFAULT,
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
        assert generation.metadata["sigil.sdk.content_capture_mode"] == "metadata_only"
        assert generation.conversation_title == ""
        assert generation.system_prompt == ""
        assert generation.input[0].parts[0].text == ""
        assert generation.output[0].parts[0].text == ""
        assert "sigil: generation sanitization failed, falling back to metadata_only" in caplog.text
    finally:
        client.shutdown()


def test_resolve_redact_input_messages_defaults_to_false() -> None:
    assert _resolve_redact_input_messages(None, env={}) is False


def test_resolve_redact_input_messages_explicit_wins_over_env() -> None:
    assert _resolve_redact_input_messages(False, env={"SIGIL_REDACT_INPUT_MESSAGES": "true"}) is False
    assert _resolve_redact_input_messages(True, env={"SIGIL_REDACT_INPUT_MESSAGES": "false"}) is True


def test_resolve_redact_input_messages_truthy_tokens() -> None:
    for token in ("1", "true", "TRUE", "yes", "on"):
        got = _resolve_redact_input_messages(None, env={"SIGIL_REDACT_INPUT_MESSAGES": token})
        assert got is True, f"token={token!r}"


def test_resolve_redact_input_messages_falsy_tokens() -> None:
    for token in ("0", "false", "FALSE", "no", "off"):
        got = _resolve_redact_input_messages(None, env={"SIGIL_REDACT_INPUT_MESSAGES": token})
        assert got is False, f"token={token!r}"


def test_resolve_redact_input_messages_blank_falls_back_to_false() -> None:
    assert _resolve_redact_input_messages(None, env={"SIGIL_REDACT_INPUT_MESSAGES": "   "}) is False


def test_resolve_redact_input_messages_invalid_warns_and_falls_back(caplog) -> None:
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        got = _resolve_redact_input_messages(None, env={"SIGIL_REDACT_INPUT_MESSAGES": "maybe"})
    assert got is False
    assert any(
        "SIGIL_REDACT_INPUT_MESSAGES" in record.getMessage() and "maybe" in record.getMessage()
        for record in caplog.records
    )


def test_resolve_redact_input_messages_preferred_key() -> None:
    assert _resolve_redact_input_messages(None, env={"AGENTO11Y_REDACT_INPUT_MESSAGES": "true"}) is True


def test_resolve_redact_input_messages_preferred_wins_over_legacy() -> None:
    env = {"AGENTO11Y_REDACT_INPUT_MESSAGES": "false", "SIGIL_REDACT_INPUT_MESSAGES": "true"}
    assert _resolve_redact_input_messages(None, env=env) is False


def test_resolve_redact_input_messages_blank_preferred_falls_through_to_legacy() -> None:
    env = {"AGENTO11Y_REDACT_INPUT_MESSAGES": "   ", "SIGIL_REDACT_INPUT_MESSAGES": "true"}
    assert _resolve_redact_input_messages(None, env=env) is True


def test_resolve_redact_input_messages_explicit_beats_both_prefixes() -> None:
    env = {"AGENTO11Y_REDACT_INPUT_MESSAGES": "false", "SIGIL_REDACT_INPUT_MESSAGES": "false"}
    assert _resolve_redact_input_messages(True, env=env) is True


def test_resolve_redact_input_messages_invalid_preferred_blocks_valid_legacy(caplog) -> None:
    env = {"AGENTO11Y_REDACT_INPUT_MESSAGES": "maybe", "SIGIL_REDACT_INPUT_MESSAGES": "true"}
    with caplog.at_level(logging.WARNING, logger="agento11y"):
        assert _resolve_redact_input_messages(None, env=env) is False
    assert any("AGENTO11Y_REDACT_INPUT_MESSAGES" in record.getMessage() for record in caplog.records)


def test_create_secret_redaction_sanitizer_reads_preferred_env(monkeypatch) -> None:
    monkeypatch.setenv("AGENTO11Y_REDACT_INPUT_MESSAGES", "true")
    sanitizer = create_secret_redaction_sanitizer()
    generation = Generation(
        id="gen-env-preferred",
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


def test_create_secret_redaction_sanitizer_reads_env(monkeypatch) -> None:
    monkeypatch.setenv("SIGIL_REDACT_INPUT_MESSAGES", "true")
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
