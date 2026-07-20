"""HTTP route handlers.

`POST /chat` runs the classifier first, then the agent (unless the
message is off-topic). Both calls share a `conversation_id` so they
group together in Sigil.
"""

from __future__ import annotations

import uuid

from fastapi import APIRouter, Request
from pydantic import BaseModel

from .agent import run_agent
from .classifier import classify_message


OFF_TOPIC_REPLY = (
    "I'm a weather assistant, so I can only help with weather and "
    "forecast questions. Ask me about the weather in a city and I'll "
    "look it up for you."
)


class ChatRequest(BaseModel):
    message: str
    conversation_id: str | None = None


class ChatResponse(BaseModel):
    conversation_id: str
    reply: str
    classification: str
    classifier_raw: str


router = APIRouter()


@router.get("/healthz")
def healthz() -> dict[str, str]:
    return {"status": "ok"}


@router.post("/chat", response_model=ChatResponse)
def chat(req: ChatRequest, request: Request) -> ChatResponse:
    state = request.app.state
    conversation_id = req.conversation_id or f"conv-{uuid.uuid4().hex[:12]}"

    classification = classify_message(
        agento11y=state.agento11y,
        conversation_id=conversation_id,
        user_message=req.message,
        model_name=state.classifier_model,
    )

    if classification.label == "OFF_TOPIC":
        reply = OFF_TOPIC_REPLY
    else:
        reply = run_agent(
            agent=state.agent,
            agento11y=state.agento11y,
            user_message=req.message,
            conversation_id=conversation_id,
        )

    return ChatResponse(
        conversation_id=conversation_id,
        reply=reply,
        classification=classification.label,
        classifier_raw=classification.raw,
    )
