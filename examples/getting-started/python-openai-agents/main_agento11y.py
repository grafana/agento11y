"""Native agento11y generation export, framework hooks, spans, and metrics."""

from __future__ import annotations

import asyncio

from agent import AGENT_NAME, PROMPTS, create_agent, env, required_env, setup_otel
from agento11y import AuthConfig, Client, ClientConfig, GenerationExportConfig
from agento11y_openai_agents import with_agento11y_openai_agents_hooks
from agents import Runner, set_trace_processors
from dotenv import load_dotenv


async def main() -> None:
    load_dotenv()
    required_env("OPENAI_API_KEY")
    tenant_id = required_env("AGENTO11Y_AUTH_TENANT_ID")
    endpoint = required_env("AGENTO11Y_ENDPOINT")
    token = required_env("AGENTO11Y_AUTH_TOKEN")
    # Remove OpenAI's default hosted tracing exporter. Native hooks below are
    # independent of these tracing processors; do not install the OTel instrumentors.
    set_trace_processors([])
    tracer_provider, meter_provider = setup_otel()
    try:
        # Providers must exist before Client resolves its tracer and meter.
        agento11y = Client(
            ClientConfig(
                generation_export=GenerationExportConfig(
                    protocol=env("AGENTO11Y_PROTOCOL", "http"),
                    endpoint=endpoint,
                    auth=AuthConfig(
                        mode="basic",
                        tenant_id=tenant_id,
                        basic_user=tenant_id,
                        basic_password=token,
                    ),
                ),
                meter=meter_provider.get_meter("agento11y-openai-agents-example"),
            )
        )
        try:
            run_options = with_agento11y_openai_agents_hooks(
                None,
                client=agento11y,
                provider_resolver="auto",
                agent_name=AGENT_NAME,
                agent_version=env("AGENTO11Y_AGENT_VERSION", "dev"),
            )
            agent = create_agent()
            context = {"conversation_id": env("AGENTO11Y_CONVERSATION_ID", "openai-agents-support-demo")}
            history = []
            for prompt in PROMPTS:
                result = await Runner.run(
                    agent,
                    input=[*history, {"role": "user", "content": prompt}],
                    context=context,
                    hooks=run_options["hooks"],
                )
                history = result.to_input_list()
                print(result.final_output)
        finally:
            agento11y.shutdown()
    finally:
        tracer_provider.shutdown()
        meter_provider.shutdown()

    print("Done")


if __name__ == "__main__":
    asyncio.run(main())
