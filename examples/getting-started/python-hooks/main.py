"""Guarded AI Observability getting-started example - Python + OpenAI.

The SDK evaluates a Sigil preflight hook before the provider call. Guard rules
configured in Grafana Cloud can allow the call, deny it, or return transformed
input such as redacted messages.
"""

import os
from urllib.parse import urlparse

from dotenv import load_dotenv
from openai import OpenAI
from opentelemetry import metrics, trace
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from agento11y import (
    ApiConfig,
    AuthConfig,
    Client,
    ClientConfig,
    GenerationExportConfig,
    GenerationStart,
    HookContext,
    HookDeniedError,
    HookEvaluateRequest,
    HookInput,
    HookModel,
    HookPhase,
    HooksConfig,
    Message,
    MessageRole,
    ModelRef,
    PartKind,
    TokenUsage,
    assistant_text_message,
    hook_denied_from_response,
    text_part,
)

load_dotenv()


def sigil_api_endpoint() -> str:
    parsed = urlparse(os.environ["AGENTO11Y_ENDPOINT"])
    return f"{parsed.scheme}://{parsed.netloc}"


def message_text(message: Message) -> str:
    return "\n".join(part.text for part in message.parts if part.kind == PartKind.TEXT)


def openai_messages(system_prompt: str, messages: list[Message]) -> list[dict[str, str]]:
    out = [{"role": "system", "content": system_prompt}]
    for message in messages:
        role = message.role.value if isinstance(message.role, MessageRole) else str(message.role)
        if role in ("user", "assistant", "system"):
            out.append({"role": role, "content": message_text(message)})
    return out


resource = Resource.create({"service.name": "getting-started-python-hooks"})

tp = TracerProvider(resource=resource)
tp.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
trace.set_tracer_provider(tp)

mp = MeterProvider(
    resource=resource,
    metric_readers=[PeriodicExportingMetricReader(OTLPMetricExporter())],
)
metrics.set_meter_provider(mp)

openai_client = OpenAI()
model = "gpt-4.1-mini"

sigil = Client(
    ClientConfig(
        generation_export=GenerationExportConfig(
            protocol="http",
            endpoint=os.environ["AGENTO11Y_ENDPOINT"],
            auth=AuthConfig(
                mode="basic",
                tenant_id=os.environ["AGENTO11Y_AUTH_TENANT_ID"],
                basic_password=os.environ["AGENTO11Y_AUTH_TOKEN"],
            ),
        ),
        api=ApiConfig(endpoint=sigil_api_endpoint()),
        hooks=HooksConfig(enabled=True, phases=[HookPhase.PREFLIGHT.value]),
    )
)

system_prompt = "You are a helpful assistant. Keep answers concise."
prompt = "My name is Jane Doe and my email is jane@example.com. Explain LLM guardrails in one sentence."
input_messages = [Message(role=MessageRole.USER, parts=[text_part(prompt)])]

try:
    hook_response = sigil.evaluate_hook(
        HookEvaluateRequest(
            phase=HookPhase.PREFLIGHT.value,
            context=HookContext(
                agent_name="getting-started-hooks",
                agent_version="1.0.0",
                model=HookModel(provider="openai", name=model),
            ),
            input=HookInput(
                messages=input_messages,
                system_prompt=system_prompt,
                conversation_preview=prompt,
            ),
        )
    )

    denied = hook_denied_from_response(hook_response)
    if denied is not None:
        raise denied

    transformed = hook_response.transformed_input
    if transformed is not None:
        if transformed.messages:
            input_messages = transformed.messages
        if transformed.system_prompt:
            system_prompt = transformed.system_prompt
        print("Sigil hook allowed the call with transformed input.\n")
    else:
        print("Sigil hook allowed the call.\n")

    completion = openai_client.chat.completions.create(
        model=model,
        messages=openai_messages(system_prompt, input_messages),
    )

    response_text = completion.choices[0].message.content or ""
    usage = completion.usage
    print(f"Response: {response_text}\n")

    with sigil.start_generation(
        GenerationStart(
            conversation_id="getting-started-python-hooks",
            agent_name="getting-started-hooks",
            agent_version="1.0.0",
            model=ModelRef(provider="openai", name=model),
            system_prompt=system_prompt,
        )
    ) as rec:
        rec.set_result(
            input=input_messages,
            output=[assistant_text_message(response_text)],
            response_id=completion.id,
            response_model=completion.model,
            stop_reason=completion.choices[0].finish_reason or "",
            usage=TokenUsage(
                input_tokens=usage.prompt_tokens if usage else 0,
                output_tokens=usage.completion_tokens if usage else 0,
            ),
        )
        if rec.err() is not None:
            print("SDK error:", rec.err())

except HookDeniedError as exc:
    print(f"Blocked by Sigil guard rule {exc.rule_id or '<unknown>'}: {exc.reason}")

else:
    print("Done - check the AI Observability plugin in your Grafana Cloud stack.")

finally:
    sigil.shutdown()
    tp.shutdown()
    mp.shutdown()
