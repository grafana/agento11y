from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

import pytest
from agento11y import Client
from opentelemetry.metrics import NoOpMeterProvider
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import StatusCode

from agento11y_hermes import _client, _config, _hooks

CONTRACT = json.loads((Path(__file__).parent / "fixtures" / "skills-v2026.6.5.json").read_text())


@pytest.mark.parametrize("case", CONTRACT["cases"], ids=lambda case: case["id"])
@pytest.mark.parametrize("selector", [{}, {"file_path": None}, {"file_path": ""}], ids=["absent", "null", "empty"])
@pytest.mark.parametrize("json_result", [False, True], ids=["object", "json"])
@pytest.mark.parametrize("mode", ["full", "no_tool_content", "metadata_only", "full_with_metadata_spans"])
def test_older_selector_free_resource_reads_remain_generic(
    monkeypatch: pytest.MonkeyPatch,
    case: dict[str, Any],
    selector: dict[str, Any],
    json_result: bool,
    mode: str,
) -> None:
    monkeypatch.setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode)
    spans = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(spans))
    plugin_config = _config.PluginConfig(error_flush_timeout=0)
    config = _client._to_client_config(plugin_config)
    config.generation_export.protocol = "none"
    config.tracer = provider.get_tracer("hermes-skill-regression")
    config.meter = NoOpMeterProvider().get_meter("hermes-skill-regression")
    client = Client(config)
    monkeypatch.setattr(_client, "_CLIENT", client)
    monkeypatch.setattr(_client, "_CONFIG", plugin_config)
    args = {**case["args"], **selector}
    result = json.dumps(case["result"]) if json_result else copy.deepcopy(case["result"])
    original_args, original_result = copy.deepcopy((args, result))
    try:
        _hooks.on_pre_api_request(
            api_request_id="request-1", session_id="session-1", model="test-model", conversation_history=[]
        )
        _hooks.on_post_api_request(api_request_id="request-1", assistant_message={"role": "assistant", "content": ""})
        _hooks.on_post_tool_call(
            api_request_id="request-1",
            session_id="session-1",
            tool_name=case["tool_name"],
            tool_call_id="call-1",
            args=args,
            result=result,
            status="success",
            duration_ms=25,
        )
        finished = spans.get_finished_spans()
        assert len(finished) == 2
        generation, tool = finished
        assert tool.name == "execute_tool skill_view"
        attributes = dict(tool.attributes or {})
        assert attributes["gen_ai.tool.name"] == "skill_view"
        assert attributes["gen_ai.tool.call.id"] == "call-1"
        assert not any(key.startswith(("gen_ai.skill.", "agento11y.skill.")) for key in attributes)
        assert tool.context.trace_id == generation.context.trace_id
        assert tool.parent is not None and tool.parent.span_id == generation.context.span_id
        assert attributes["agento11y.generation.parent_generation_ids"] == (
            (generation.attributes or {})["agento11y.generation.id"],
        )
        assert tool.end_time is not None and tool.start_time is not None
        assert tool.end_time - tool.start_time == pytest.approx(25_000_000, abs=1_000, rel=0)
        assert tool.status.status_code == StatusCode.OK
        assert not tool.events
        if mode != "full":
            assert "gen_ai.tool.call.arguments" not in attributes
            assert "gen_ai.tool.call.result" not in attributes
            assert case["result"]["content"] not in repr(attributes)
            assert case["result"]["path"] not in repr(attributes)
        assert args == original_args
        assert result == original_result
    finally:
        client.shutdown()
        provider.shutdown()
