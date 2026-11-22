from __future__ import annotations

import os
import sys
import unittest
from pathlib import Path
from typing import Any
from unittest.mock import Mock, patch

APP = Path(__file__).resolve().parents[1] / "app" / "AgentCoreOtlpDemo"
sys.path.insert(0, str(APP))

from agent import TOOLS, run_agent  # noqa: E402
from langchain.agents import create_agent  # noqa: E402
from langchain_core.language_models.fake_chat_models import FakeMessagesListChatModel  # noqa: E402
from langchain_core.messages import AIMessage  # noqa: E402
from opentelemetry.sdk.metrics import MeterProvider  # noqa: E402
from opentelemetry.sdk.resources import Resource  # noqa: E402
from opentelemetry.sdk.trace import TracerProvider  # noqa: E402
from opentelemetry.sdk.trace.export import SimpleSpanProcessor  # noqa: E402
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter  # noqa: E402
from telemetry import Telemetry  # noqa: E402


class ToolCallingModel(FakeMessagesListChatModel):
    def bind_tools(self, tools: Any, **kwargs: Any) -> ToolCallingModel:
        return self


class AgentIdentityTests(unittest.TestCase):
    def setUp(self) -> None:
        resource = Resource({"service.name": "distinct-service-name"})
        self.exporter = InMemorySpanExporter()
        traces = TracerProvider(resource=resource)
        traces.add_span_processor(SimpleSpanProcessor(self.exporter))
        metrics = MeterProvider(resource=resource)
        meter = metrics.get_meter("agent-identity-test")
        self.telemetry = Telemetry(
            tracer_provider=traces,
            meter_provider=metrics,
            tracer=traces.get_tracer("agent-identity-test"),
            requests=meter.create_counter("test.requests"),
            duration=meter.create_histogram("test.duration"),
        )
        self.addCleanup(self.telemetry.shutdown)

    def test_all_span_types_keep_agent_identity_across_capture_modes(self) -> None:
        for mode in ["full", "no_tool_content", "metadata_only"]:
            for name in ["logical-agent", " logical-agent ", "", None]:
                with self.subTest(mode=mode, name=name):
                    self.exporter.clear()
                    settings = {"AGENTO11Y_CONTENT_CAPTURE_MODE": mode}
                    if name is not None:
                        settings["AGENT_NAME"] = name
                    with patch.dict(os.environ, settings, clear=True):
                        response = run_agent(
                            agent=self._tool_calling_agent(),
                            telemetry=self.telemetry,
                            prompt="Check synthetic inventory.",
                            conversation_id="identity-conversation",
                        )
                    self.assertEqual(response["status"], "ok")
                    spans = self.exporter.get_finished_spans()
                    self.assertEqual(len(spans), 4)
                    self.assertEqual(sum(span.name.startswith("chat ") for span in spans), 2)
                    self.assertIn("execute_tool lookup_inventory", {span.name for span in spans})
                    for span in spans:
                        attributes = span.attributes or {}
                        expected_name = (name or "").strip() or "agentcore-otlp-demo"
                        self.assertEqual(attributes.get("gen_ai.agent.name"), expected_name)
                        self.assertEqual(attributes.get("gen_ai.conversation.id"), "identity-conversation")
                        self.assertNotEqual(attributes["gen_ai.agent.name"], span.resource.attributes["service.name"])

    def test_failed_invocation_keeps_agent_identity(self) -> None:
        with patch.dict(os.environ, {"AGENT_NAME": "logical-agent"}, clear=True):
            response = run_agent(
                agent=Mock(invoke=Mock(side_effect=RuntimeError("synthetic failure"))),
                telemetry=self.telemetry,
                prompt="Synthetic error case.",
                conversation_id="failed-conversation",
            )
        self.assertEqual(response["status"], "error")
        span = self.exporter.get_finished_spans()[0]
        attributes = span.attributes or {}
        self.assertEqual(attributes.get("gen_ai.agent.name"), "logical-agent")
        self.assertEqual(attributes.get("gen_ai.conversation.id"), "failed-conversation")

    def _tool_calling_agent(self) -> Any:
        model = ToolCallingModel(
            responses=[
                AIMessage(
                    content="",
                    tool_calls=[{"name": "lookup_inventory", "args": {"query": "ENV-100"}, "id": "call-inventory"}],
                ),
                AIMessage(content="Synthetic inventory checked."),
            ]
        )
        return create_agent(model=model, tools=TOOLS)
