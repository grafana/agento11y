"""Standard OpenTelemetry instrumentation; no agento11y SDK imports."""

from __future__ import annotations

import asyncio
import os

from agent import AGENT_NAME, PROMPTS, create_agent, env, required_env, setup_otel
from agents import RunConfig, Runner
from dotenv import load_dotenv
from opentelemetry.instrumentation.genai.openai import OpenAIInstrumentor
from opentelemetry.instrumentation.genai.openai_agents import OpenAIAgentsInstrumentor
from opentelemetry.sdk.trace import SpanProcessor


class AgentLabels(SpanProcessor):
    """Label chat/tool spans for this single agent; invoke_agent already has a name."""

    def on_start(self, span, parent_context=None) -> None:
        if span.instrumentation_scope.name.startswith("opentelemetry.instrumentation.genai."):
            span.set_attribute("gen_ai.agent.name", AGENT_NAME)


async def main() -> None:
    load_dotenv()
    required_env("OPENAI_API_KEY")
    # Opt in only this variant, preserving any existing resource attributes.
    attributes = os.getenv("OTEL_RESOURCE_ATTRIBUTES", "").strip()
    os.environ["OTEL_RESOURCE_ATTRIBUTES"] = (
        f"{attributes},agento11y.record=true" if attributes else "agento11y.record=true"
    )
    os.environ.setdefault("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "span_only")
    agent = create_agent()
    tracer_provider, meter_provider = setup_otel(AgentLabels())
    try:
        # This replaces OpenAI's hosted exporter with the OTel processor.
        # agents.set_trace_processors([]) is another way to clear defaults, but
        # calling it AFTER instrument() would remove the OTel processor too.
        OpenAIAgentsInstrumentor().instrument(disable_openai_trace_export=True)
        # The Agents instrumentor emits workflow/agent/tool spans; this second
        # instrumentor supplies chat spans and token metrics for Responses API calls.
        OpenAIInstrumentor().instrument()

        conversation_id = env("AGENTO11Y_CONVERSATION_ID", "openai-agents-support-demo")
        run_config = RunConfig(workflow_name="Order support", group_id=conversation_id)
        history = []
        for prompt in PROMPTS:
            result = await Runner.run(
                agent,
                input=[*history, {"role": "user", "content": prompt}],
                run_config=run_config,
            )
            history = result.to_input_list()
            print(result.final_output)
    finally:
        tracer_provider.shutdown()
        meter_provider.shutdown()

    print("Done")


if __name__ == "__main__":
    asyncio.run(main())
