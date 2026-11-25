from __future__ import annotations

import os
import sys
import unittest
from pathlib import Path
from typing import Any
from unittest.mock import patch
from uuid import uuid4

APP = Path(__file__).resolve().parents[1] / "app" / "AgentCoreOtlpDemo"
sys.path.insert(0, str(APP))

from langchain_core.messages import AIMessage, HumanMessage
from langchain_core.outputs import ChatGeneration, LLMResult
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import ReadableSpan, TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from otel_genai import GenAISpanHandler


class ChatSpanTests(unittest.TestCase):
    def setUp(self) -> None:
        self.exporter = InMemorySpanExporter()
        self.provider = TracerProvider(resource=Resource({}))
        self.provider.add_span_processor(SimpleSpanProcessor(self.exporter))
        self.addCleanup(self.provider.shutdown)

    def test_provider_names_follow_semantic_conventions(self) -> None:
        for provider, expected in [("bedrock", "aws.bedrock"), ("anthropic", "anthropic")]:
            with self.subTest(provider=provider):
                attributes = self._record_chat(provider, {}).attributes or {}
                self.assertEqual(attributes["gen_ai.provider.name"], expected)
                self.assertIs(attributes["agento11y.record"], True)

    def test_finish_reasons_preserve_provider_values_in_all_capture_modes(self) -> None:
        for mode in ["full", "no_tool_content", "metadata_only"]:
            for provider, key in [("bedrock", "stopReason"), ("anthropic", "stop_reason")]:
                for reason in ["tool_use", "end_turn", "max_tokens"]:
                    with self.subTest(mode=mode, provider=provider, reason=reason):
                        span = self._record_chat(provider, {key: reason, "model_id": "synthetic-response"}, mode)
                        attributes = span.attributes or {}
                        self.assertEqual(attributes["gen_ai.response.finish_reasons"], (reason,))
                        self.assertEqual(attributes["gen_ai.response.model"], "synthetic-response")
                        self.assertEqual("gen_ai.output.messages" in attributes, mode != "metadata_only")

    def test_missing_finish_reason_does_not_invent_a_value(self) -> None:
        for metadata in [{}, {"stopReason": ""}, {"stop_reason": None}]:
            with self.subTest(metadata=metadata):
                attributes = self._record_chat("bedrock", metadata).attributes or {}
                self.assertNotIn("gen_ai.response.finish_reasons", attributes)

    def _record_chat(self, provider: str, metadata: dict[str, Any], mode: str = "no_tool_content") -> ReadableSpan:
        with patch.dict(os.environ, {"AGENTO11Y_CONTENT_CAPTURE_MODE": mode}, clear=True):
            handler = GenAISpanHandler(
                self.provider.get_tracer("chat-span-test"),
                provider=provider,
                model="synthetic-request",
                agent_name="logical-agent",
                conversation_id="chat-conversation",
            )
        run_id = uuid4()
        handler.on_chat_model_start({}, [[HumanMessage(content="Synthetic request.")]], run_id=run_id)
        message = AIMessage(content="Synthetic response.", response_metadata=metadata)
        handler.on_llm_end(LLMResult(generations=[[ChatGeneration(message=message)]]), run_id=run_id)
        return self.exporter.get_finished_spans()[-1]
