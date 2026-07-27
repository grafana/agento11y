from __future__ import annotations

import atexit
import os
from typing import Any

from agent import build_agent, run_agent
from agento11y_client import setup_agento11y
from bedrock_agentcore.runtime import BedrockAgentCoreApp
from dotenv import load_dotenv
from telemetry import setup_opentelemetry

load_dotenv(override=False)

telemetry = setup_opentelemetry()
agento11y = setup_agento11y(
    tracer_provider=telemetry.tracer_provider,
    meter_provider=telemetry.meter_provider,
)
agent = build_agent()
app = BedrockAgentCoreApp()


@app.entrypoint
def handler(request: dict[str, Any], context: Any) -> dict[str, Any]:
    prompt = _prompt_from_request(request)
    if not prompt:
        return {
            "status": "error",
            "reply": ("Missing prompt. Send {'prompt': '...'} or {'message': '...'} in the request body."),
        }

    conversation_id = (
        str(getattr(context, "session_id", "") or "")
        or str(request.get("conversation_id") or "")
        or str(request.get("session_id") or "")
        or str(request.get("runtimeSessionId") or "")
    )
    account_id = str(request.get("account_id") or "acct-demo-1042")

    return run_agent(
        agent=agent,
        agento11y=agento11y,
        telemetry=telemetry,
        prompt=prompt,
        conversation_id=conversation_id,
        account_id=account_id,
    )


def _prompt_from_request(request: dict[str, Any]) -> str:
    value = request.get("prompt") or request.get("message") or request.get("input")
    if isinstance(value, str):
        return value.strip()
    return ""


def _shutdown() -> None:
    agento11y.shutdown()
    telemetry.shutdown()


atexit.register(_shutdown)


if __name__ == "__main__":
    app.run(port=int(os.getenv("PORT", "8080")))
