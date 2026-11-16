"""First-token timing follows content, including non-text model deltas."""

import asyncio
import inspect
from uuid import uuid4

import pytest
from agento11y.client import GenerationRecorder
from agento11y_langchain import Agento11yAsyncLangChainHandler, Agento11yLangChainHandler
from langchain_core.messages import AIMessageChunk, HumanMessage
from langchain_core.outputs import ChatGenerationChunk
from test_langchain_handler import _CapturingExporter, _new_client


@pytest.mark.parametrize("handler_type", [Agento11yLangChainHandler, Agento11yAsyncLangChainHandler])
@pytest.mark.parametrize("capture_outputs", [False, True])
@pytest.mark.parametrize(
    "token,message",
    [
        ("hello", AIMessageChunk(content="hello")),
        ([{"type": "thinking", "thinking": "Let me think."}], AIMessageChunk(content=[])),
        ("", AIMessageChunk(content=[{"type": "thinking", "thinking": "Let me think."}])),
        ([{"type": "tool_use", "id": "call_add", "name": "add", "input": {}}], AIMessageChunk(content=[])),
        ("", AIMessageChunk(content=[], tool_call_chunks=[{"id": "call_add", "name": "add", "args": "", "index": 0}])),
        ("", AIMessageChunk(content=[], tool_call_chunks=[{"id": None, "name": None, "args": '{"a":', "index": 0}])),
    ],
)
def test_first_content_marks_time_once(monkeypatch, handler_type, capture_outputs, token, message):
    exporter = _CapturingExporter()
    client = _new_client(exporter)
    handler = handler_type(client=client, capture_outputs=capture_outputs)
    times = []
    original = GenerationRecorder.set_first_token_at

    def record(recorder, timestamp):
        times.append(timestamp)
        original(recorder, timestamp)

    monkeypatch.setattr(GenerationRecorder, "set_first_token_at", record)

    async def emit(method, *args, **kwargs):
        result = getattr(handler, method)(*args, **kwargs)
        if inspect.isawaitable(result):
            await result

    async def run():
        run_id = uuid4()
        await emit(
            "on_chat_model_start",
            {"name": "ChatAnthropic"},
            [[HumanMessage(content="add")]],
            run_id=run_id,
            invocation_params={"model": "claude-haiku-4-5", "streaming": True},
        )
        for empty in [
            AIMessageChunk(content=""),
            AIMessageChunk(content=[]),
            AIMessageChunk(content=[{"type": "text", "text": ""}]),
            AIMessageChunk(content=[{"type": "thinking", "thinking": ""}]),
            AIMessageChunk(content=[], tool_call_chunks=[{"name": None, "id": None, "args": "", "index": 0}]),
        ]:
            await emit("on_llm_new_token", empty.content, chunk=ChatGenerationChunk(message=empty), run_id=run_id)
        assert times == []
        await emit("on_llm_new_token", token, chunk=ChatGenerationChunk(message=message), run_id=run_id)
        assert len(times) == 1
        await emit("on_llm_new_token", "later text", run_id=run_id)
        assert len(times) == 1
        await emit("on_llm_error", RuntimeError("stream interrupted"), run_id=run_id)

    try:
        asyncio.run(run())
    finally:
        client.shutdown()
