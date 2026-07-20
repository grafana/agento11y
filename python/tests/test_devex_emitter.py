from __future__ import annotations

import sys
import time
from importlib import util as importlib_util
from pathlib import Path

MODULE_PATH = Path(__file__).resolve().parents[1] / "scripts" / "devex_emitter.py"
MODULE_SPEC = importlib_util.spec_from_file_location("devex_emitter", MODULE_PATH)
assert MODULE_SPEC is not None and MODULE_SPEC.loader is not None
emitter = importlib_util.module_from_spec(MODULE_SPEC)
sys.modules[MODULE_SPEC.name] = emitter
MODULE_SPEC.loader.exec_module(emitter)


def test_tags_and_metadata_include_required_contract_fields() -> None:
    persona, tags, metadata = emitter.build_tags_metadata("openai", "SYNC", 2, 1)

    assert persona in {"planner", "retriever", "executor"}
    assert tags["agento11y.devex.language"] == "python"
    assert tags["agento11y.devex.provider"] == "openai"
    assert tags["agento11y.devex.source"] == "provider_wrapper"
    assert tags["agento11y.devex.mode"] == "SYNC"

    assert metadata["turn_index"] == 2
    assert metadata["conversation_slot"] == 1
    assert metadata["agent_persona"] == persona
    assert metadata["emitter"] == "sdk-traffic"
    assert isinstance(metadata["provider_shape"], str)


def test_custom_provider_source_uses_core_custom_tag() -> None:
    assert emitter.source_tag_for("mistral") == "core_custom"
    assert emitter.source_tag_for("gemini") == "provider_wrapper"


def test_mode_choice_respects_stream_threshold(monkeypatch) -> None:
    monkeypatch.setattr(emitter.random, "randint", lambda _a, _b: 10)
    assert emitter.choose_mode(30) == "STREAM"

    monkeypatch.setattr(emitter.random, "randint", lambda _a, _b: 35)
    assert emitter.choose_mode(30) == "SYNC"


def test_thread_rotation_resets_turn_after_threshold() -> None:
    state = emitter.SourceState(conversations=1)

    first = emitter.resolve_thread(state, rotate_turns=3, source="openai", slot=0)
    assert first.turn == 0
    first_id = first.conversation_id
    assert first_id

    first.turn = 3
    time.sleep(0.002)
    rotated = emitter.resolve_thread(state, rotate_turns=3, source="openai", slot=0)
    assert rotated.turn == 0
    assert rotated.conversation_id != first_id


def test_anthropic_emit_uses_messages_namespace_wrapper(monkeypatch) -> None:
    calls: dict[str, object] = {}

    class MessagesNamespace:
        def create(self, client, request, provider_call, options):
            calls["client"] = client
            calls["request"] = request
            calls["options"] = options
            calls["response"] = provider_call(request)
            return calls["response"]

    monkeypatch.setattr(emitter, "messages", MessagesNamespace())
    monkeypatch.setattr(emitter, "AnthropicOptions", lambda **kwargs: kwargs)

    context = emitter.EmitContext(
        conversation_id="conv-anthropic",
        turn=7,
        slot=0,
        agent_name="devex-python-anthropic-planner",
        agent_version="devex-1",
        tags={"agento11y.devex.provider": "anthropic"},
        metadata={"provider_shape": "messages"},
    )

    emitter.emit_anthropic_sync(object(), context)

    request = calls["request"]
    assert isinstance(request, dict)
    assert request["model"] == "claude-sonnet-4-5"
    assert request["messages"][0]["role"] == "user"

    response = calls["response"]
    assert isinstance(response, dict)
    assert response["id"] == "py-anthropic-sync-7"
    assert response["stop_reason"] == "end_turn"

    options = calls["options"]
    assert isinstance(options, dict)
    assert options["conversation_id"] == "conv-anthropic"
    assert options["agent_name"] == "devex-python-anthropic-planner"


def test_gemini_stream_uses_models_namespace_and_responses_summary(monkeypatch) -> None:
    calls: dict[str, object] = {}

    class StrictGeminiStreamSummary:
        def __init__(self, *, responses, output_text="", final_response=None):
            self.responses = responses
            self.output_text = output_text
            self.final_response = final_response

    class ModelsNamespace:
        def generate_content_stream(self, client, model, contents, config, provider_call, options):
            calls["client"] = client
            calls["model"] = model
            calls["contents"] = contents
            calls["config"] = config
            calls["options"] = options
            calls["summary"] = provider_call(model, contents, config)
            return calls["summary"]

    monkeypatch.setattr(emitter, "GeminiStreamSummary", StrictGeminiStreamSummary)
    monkeypatch.setattr(emitter, "models", ModelsNamespace())
    monkeypatch.setattr(emitter, "GeminiOptions", lambda **kwargs: kwargs)

    context = emitter.EmitContext(
        conversation_id="conv-gemini",
        turn=9,
        slot=1,
        agent_name="devex-python-gemini-retriever",
        agent_version="devex-1",
        tags={"agento11y.devex.provider": "gemini"},
        metadata={"provider_shape": "generate_content"},
    )

    emitter.emit_gemini_stream(object(), context)

    assert calls["model"] == "gemini-2.5-pro"
    summary = calls["summary"]
    assert isinstance(summary, StrictGeminiStreamSummary)
    assert len(summary.responses) == 1
    assert summary.final_response["response_id"] == "py-gemini-stream-9"

    options = calls["options"]
    assert isinstance(options, dict)
    assert options["conversation_id"] == "conv-gemini"


def test_emit_frameworks_invokes_all_framework_handlers_for_provider_sources(monkeypatch) -> None:
    calls: list[tuple[str, str]] = []

    class _FakeHandler:
        def __init__(self, *, client, **kwargs):
            del client
            del kwargs

        def on_chat_model_start(self, _serialized, _messages, *, run_id, **_kwargs):
            calls.append(("start", str(run_id)))

        def on_llm_end(self, _response, *, run_id, **_kwargs):
            calls.append(("end", str(run_id)))

    monkeypatch.setattr(emitter, "Agento11yLangChainHandler", _FakeHandler)
    monkeypatch.setattr(emitter, "Agento11yLangGraphHandler", _FakeHandler)
    monkeypatch.setattr(emitter, "Agento11yOpenAIAgentsHandler", _FakeHandler)
    monkeypatch.setattr(emitter, "Agento11yLlamaIndexHandler", _FakeHandler)
    monkeypatch.setattr(emitter, "Agento11yGoogleAdkHandler", _FakeHandler)

    context = emitter.EmitContext(
        conversation_id="conv-framework",
        turn=3,
        slot=0,
        agent_name="devex-python-openai-planner",
        agent_version="devex-1",
        tags={"agento11y.devex.provider": "openai"},
        metadata={"provider_shape": "framework"},
    )

    emitter.emit_frameworks(object(), "openai", "SYNC", context)
    assert len(calls) == 10


def test_emit_frameworks_skips_non_provider_sources(monkeypatch) -> None:
    monkeypatch.setattr(
        emitter, "Agento11yLangChainHandler", lambda **_kwargs: (_ for _ in ()).throw(RuntimeError("unexpected"))
    )
    monkeypatch.setattr(
        emitter, "Agento11yLangGraphHandler", lambda **_kwargs: (_ for _ in ()).throw(RuntimeError("unexpected"))
    )
    monkeypatch.setattr(
        emitter, "Agento11yOpenAIAgentsHandler", lambda **_kwargs: (_ for _ in ()).throw(RuntimeError("unexpected"))
    )
    monkeypatch.setattr(
        emitter, "Agento11yLlamaIndexHandler", lambda **_kwargs: (_ for _ in ()).throw(RuntimeError("unexpected"))
    )
    monkeypatch.setattr(
        emitter, "Agento11yGoogleAdkHandler", lambda **_kwargs: (_ for _ in ()).throw(RuntimeError("unexpected"))
    )

    context = emitter.EmitContext(
        conversation_id="conv-framework",
        turn=3,
        slot=0,
        agent_name="devex-python-mistral-planner",
        agent_version="devex-1",
        tags={"agento11y.devex.provider": "mistral"},
        metadata={"provider_shape": "framework"},
    )

    emitter.emit_frameworks(object(), "mistral", "SYNC", context)
