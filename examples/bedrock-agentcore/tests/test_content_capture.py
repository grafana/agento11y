from __future__ import annotations

import os
import sys
import unittest
from pathlib import Path
from typing import Any
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "app" / "AgentCoreObservabilityDemo"))

from agent import SYSTEM_PROMPT, TOOLS, run_agent
from agento11y import Client, ClientConfig
from agento11y.models import (
    ExportGenerationResult,
    ExportGenerationsResponse,
    ExportWorkflowStepsResponse,
    Generation,
)
from agento11y_client import setup_agento11y
from content_capture import DemoClient, without_tool_payloads
from langchain.agents import create_agent
from langchain_core.language_models.fake_chat_models import FakeMessagesListChatModel
from langchain_core.messages import AIMessage
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from telemetry import Telemetry

PROMPT = "Check inventory for ENV-100."
REPLY = "Synthetic inventory checked."


class ToolCallingModel(FakeMessagesListChatModel):
    def bind_tools(self, tools: Any, **kwargs: Any) -> ToolCallingModel:
        return self


def tool_calling_model() -> ToolCallingModel:
    return ToolCallingModel(
        responses=[
            AIMessage(
                content="",
                tool_calls=[{"name": "lookup_inventory", "args": {"query": "ENV-100"}, "id": "call-inventory"}],
            ),
            AIMessage(content=REPLY),
        ]
    )


class GenerationSink:
    def __init__(self) -> None:
        self.generations: list[Generation] = []

    def export_generations(self, request: Any) -> ExportGenerationsResponse:
        self.generations.extend(request.generations)
        return ExportGenerationsResponse(
            results=[ExportGenerationResult(generation_id=g.id, accepted=True) for g in request.generations]
        )

    def shutdown(self) -> None:
        pass

    def export_workflow_steps(self, request: Any) -> ExportWorkflowStepsResponse:
        raise AssertionError("This example does not enable workflow step capture.")


class ContentCaptureTests(unittest.TestCase):
    def setUp(self) -> None:
        self.environment = patch.dict(os.environ, {"AGENTO11Y_ANONYMOUS_USAGE_STATS": "disabled"}, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.spans = InMemorySpanExporter()
        self.provider = TracerProvider(resource=Resource({}))
        self.provider.add_span_processor(SimpleSpanProcessor(self.spans))
        self.meter_provider = MeterProvider(resource=Resource({}))
        self.addCleanup(self.provider.shutdown)
        self.addCleanup(self.meter_provider.shutdown)
        meter = self.meter_provider.get_meter("test")
        self.telemetry = Telemetry(
            self.provider,
            self.meter_provider,
            self.provider.get_tracer("test"),
            meter.create_counter("requests"),
            meter.create_histogram("duration"),
        )
        self.sink = GenerationSink()

    def _make_client(self, config: ClientConfig) -> Client:
        config.generation_exporter = self.sink
        return DemoClient(config)

    def _invoke(self, mode: str | None) -> tuple[list[Any], Any]:
        if mode is not None:
            os.environ["AGENTO11Y_CONTENT_CAPTURE_MODE"] = mode
        with patch("agento11y_client.DemoClient", side_effect=self._make_client):
            client = setup_agento11y(tracer_provider=self.provider, meter_provider=self.meter_provider)
        try:
            result = run_agent(
                agent=create_agent(model=tool_calling_model(), tools=TOOLS, system_prompt=SYSTEM_PROMPT),
                agento11y=client,
                telemetry=self.telemetry,
                prompt=PROMPT,
                conversation_id="test-conversation",
            )
            client.flush()
        finally:
            client.shutdown()
        self.assertEqual(result, {"conversation_id": "test-conversation", "reply": REPLY, "status": "ok"})
        self.assertEqual(len(self.sink.generations), 2)
        parts = [part for g in self.sink.generations for m in [*g.input, *g.output] for part in m.parts]
        tool_spans = [
            s for s in self.spans.get_finished_spans() if s.attributes and s.attributes.get("gen_ai.tool.name")
        ]
        self.assertEqual(len(tool_spans), 1)
        assert tool_spans[0].attributes is not None
        self.assertEqual(tool_spans[0].attributes["gen_ai.tool.name"], "lookup_inventory")
        return parts, tool_spans[0]

    def _assert_chat_preserved(self, parts: list[Any]) -> None:
        text = [part.text for part in parts if part.text]
        self.assertIn(PROMPT, text)
        self.assertIn(REPLY, text)
        self.assertTrue(any(SYSTEM_PROMPT.strip() in g.system_prompt for g in self.sink.generations))
        self.assertTrue(any(part.tool_call and part.tool_call.name == "lookup_inventory" for part in parts))
        self.assertTrue(any(part.tool_result and part.tool_result.tool_call_id == "call-inventory" for part in parts))

    def _assert_tool_payloads_omitted(self, parts: list[Any], span: Any) -> None:
        self.assertNotIn("gen_ai.tool.call.arguments", span.attributes)
        self.assertNotIn("gen_ai.tool.call.result", span.attributes)
        for part in parts:
            if part.tool_call:
                self.assertFalse(part.tool_call.input_json)
            if part.tool_result:
                self.assertFalse(part.tool_result.content)
                self.assertFalse(part.tool_result.content_json)

    def test_no_tool_content_keeps_chat_without_tool_payloads(self) -> None:
        parts, span = self._invoke("no_tool_content")
        self._assert_chat_preserved(parts)
        self._assert_tool_payloads_omitted(parts, span)

    def test_default_keeps_chat_without_tool_payloads(self) -> None:
        parts, span = self._invoke(None)
        self._assert_chat_preserved(parts)
        self._assert_tool_payloads_omitted(parts, span)

    def test_full_keeps_tool_payloads(self) -> None:
        parts, span = self._invoke("full")
        self._assert_chat_preserved(parts)
        self.assertIn("ENV-100", span.attributes["gen_ai.tool.call.arguments"])
        self.assertIn("matches", span.attributes["gen_ai.tool.call.result"])
        self.assertTrue(any(part.tool_call and part.tool_call.input_json for part in parts))
        self.assertTrue(any(part.tool_result and part.tool_result.content for part in parts))

    def test_metadata_only_also_omits_chat(self) -> None:
        parts, span = self._invoke("metadata_only")
        self._assert_tool_payloads_omitted(parts, span)
        self.assertFalse(any(part.text for part in parts))
        self.assertFalse(any(g.system_prompt for g in self.sink.generations))

    def test_sanitizer_does_not_mutate_original_generation(self) -> None:
        self._invoke("full")
        original = self.sink.generations[-1]
        original_result = next(p.tool_result for m in original.input for p in m.parts if p.tool_result)
        original_result.content_json = b'{"synthetic": true}'
        sanitized = without_tool_payloads(original)
        sanitized_result = next(p.tool_result for m in sanitized.input for p in m.parts if p.tool_result)
        self.assertTrue(original_result.content)
        self.assertTrue(original_result.content_json)
        self.assertFalse(sanitized_result.content)
        self.assertFalse(sanitized_result.content_json)


if __name__ == "__main__":
    unittest.main()
