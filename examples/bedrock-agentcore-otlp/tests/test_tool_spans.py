from __future__ import annotations

import json
import os
import sys
import unittest
from pathlib import Path
from typing import Any
from unittest.mock import patch
from uuid import uuid4

APP = Path(__file__).resolve().parents[1] / "app" / "AgentCoreOtlpDemo"
sys.path.insert(0, str(APP))

from langchain_core.messages import ToolMessage  # noqa: E402
from opentelemetry.exporter.otlp.proto.common.trace_encoder import encode_spans  # noqa: E402
from opentelemetry.sdk.resources import Resource  # noqa: E402
from opentelemetry.sdk.trace import ReadableSpan, TracerProvider  # noqa: E402
from opentelemetry.sdk.trace.export import SimpleSpanProcessor  # noqa: E402
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter  # noqa: E402
from otel_genai import GenAISpanHandler  # noqa: E402


class ToolSpanTests(unittest.TestCase):
    def setUp(self) -> None:
        self.exporter = InMemorySpanExporter()
        provider = TracerProvider(resource=Resource({}))
        provider.add_span_processor(SimpleSpanProcessor(self.exporter))
        self.tracer = provider.get_tracer("tool-span-test")
        self.addCleanup(provider.shutdown)

    def test_results_are_valid_json_for_supported_shapes(self) -> None:
        cases = [
            ({"ok": True}, {"ok": True}),
            ([1, 2], [1, 2]),
            ('{"ok":true}', {"ok": True}),
            ("plain text", "plain text"),
            (0, 0),
            (False, False),
            (None, None),
            ("", ""),
        ]
        for output, expected in cases:
            with self.subTest(output_type=type(output).__name__):
                attributes = self._record_tool(output).attributes or {}
                result_json = attributes["gen_ai.tool.call.result"]
                arguments_json = attributes["gen_ai.tool.call.arguments"]
                assert isinstance(result_json, str)
                assert isinstance(arguments_json, str)
                self.assertEqual(json.loads(result_json), expected)
                self.assertEqual(json.loads(arguments_json), {"query": "synthetic"})
                self.assertEqual(attributes["gen_ai.tool.call.id"], "call-unit")
                self.assertIs(attributes.get("agento11y.record"), True)

    def test_tool_message_recovers_call_id_at_completion(self) -> None:
        output = ToolMessage(content='{"items":[]}', tool_call_id="call-completed")
        attributes = self._record_tool(output, call_id=None).attributes or {}
        self.assertEqual(attributes["gen_ai.tool.call.id"], "call-completed")
        result_json = attributes["gen_ai.tool.call.result"]
        assert isinstance(result_json, str)
        self.assertEqual(json.loads(result_json), {"items": []})

    def test_reduced_capture_keeps_identity_and_omits_tool_payloads(self) -> None:
        for mode in ["no_tool_content", "metadata_only"]:
            with self.subTest(mode=mode):
                attributes = self._record_tool({"ok": True}, mode=mode).attributes or {}
                self.assertIs(attributes.get("agento11y.record"), True)
                self.assertEqual(attributes["gen_ai.tool.call.id"], "call-unit")
                self.assertNotIn("gen_ai.tool.call.arguments", attributes)
                self.assertNotIn("gen_ai.tool.call.result", attributes)

    def test_large_result_survives_otlp_serialization(self) -> None:
        result = {"payload": "x" * 70_000}
        span = self._record_tool(result)
        request = encode_spans([span])
        wire_span = request.resource_spans[0].scope_spans[0].spans[0]
        attributes = {item.key: item.value.string_value for item in wire_span.attributes}
        self.assertEqual(json.loads(attributes["gen_ai.tool.call.result"]), result)
        self.assertEqual(attributes["gen_ai.tool.call.id"], "call-unit")
        self.assertEqual(wire_span.dropped_attributes_count, 0)

    def _record_tool(self, output: Any, mode: str = "full", call_id: str | None = "call-unit") -> ReadableSpan:
        with patch.dict(os.environ, {"AGENTO11Y_CONTENT_CAPTURE_MODE": mode}, clear=True):
            handler = GenAISpanHandler(
                self.tracer,
                provider="bedrock",
                model="synthetic",
                agent_name="logical-agent",
                conversation_id="tool-conversation",
            )
        run_id = uuid4()
        handler.on_tool_start(
            {"name": "synthetic_tool"},
            '{"query":"synthetic"}',
            run_id=run_id,
            inputs={"query": "synthetic"},
            tool_call_id=call_id,
        )
        handler.on_tool_end(output, run_id=run_id)
        return self.exporter.get_finished_spans()[-1]
