"""Scripted OpenAI-compatible endpoint for the failure and retry paths.

MOCK_SCRIPT is a comma-separated list, one entry per completion request; the
last entry repeats when the list runs out.

  429         rate limited, with Retry-After: hermes retries and re-fires
              pre_api_request under the SAME api_request_id
  500         server error, also retried
  401         auth error, not retried
  empty       200 with an empty SSE body, which hermes treats as a retryable
              provider fault and gives up on after three attempts
  scratchpad  content with an unterminated <REASONING_SCRATCHPAD> and
              finish_reason=length, which reaches hermes' thinking-budget-
              exhausted path
  tool        a call to the read-only ``skills_list`` tool, so hermes executes
              a tool and comes back for a second API call. Pair it as
              ``tool,ok`` to get a two-call session, which is the only way to
              reach the paths that need more than one request in a session.
  ok          a normal assistant reply

Every step honours the request's own ``stream`` flag: a streaming request gets
SSE and a non-streaming one gets a plain body. Hermes always prefers the
streaming path, and reads a non-streamed body as an empty stream.
"""

from __future__ import annotations

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

E2E_DIR = os.environ.get("E2E_DIR", "/tmp/agento11y-hermes-e2e")
SCRIPT = (os.environ.get("MOCK_SCRIPT") or "ok").split(",")
LOG = os.environ.get("MOCK_LOG") or os.path.join(E2E_DIR, "mock.log")

_calls = {"n": 0}


def log(line: str) -> None:
    with open(LOG, "a") as handle:
        handle.write(line + "\n")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002 - stdlib signature
        pass

    def _send(self, code: int, body: dict[str, Any], extra_headers: dict[str, str] | None = None) -> None:
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        for key, value in (extra_headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(raw)

    def _send_stream(self, chunks: list[dict[str, Any]]) -> None:
        payload = "".join("data: " + json.dumps(chunk) + "\n\n" for chunk in chunks) + "data: [DONE]\n\n"
        raw = payload.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _send_sse(self, text: str, finish_reason: str) -> None:
        base = {"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 0, "model": "mock-model"}
        self._send_stream(
            [
                {**base, "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}}]},
                {**base, "choices": [{"index": 0, "delta": {"content": text}}]},
                {
                    **base,
                    "choices": [{"index": 0, "delta": {}, "finish_reason": finish_reason}],
                    "usage": {"prompt_tokens": 11, "completion_tokens": 4, "total_tokens": 15},
                },
            ]
        )

    def _send_sse_tool_call(self, name: str, arguments: str) -> None:
        base = {"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 0, "model": "mock-model"}
        call = {"index": 0, "id": "call_mock_1", "type": "function", "function": {"name": name, "arguments": ""}}
        self._send_stream(
            [
                {
                    **base,
                    "choices": [{"index": 0, "delta": {"role": "assistant", "content": "", "tool_calls": [call]}}],
                },
                {
                    **base,
                    "choices": [
                        {"index": 0, "delta": {"tool_calls": [{"index": 0, "function": {"arguments": arguments}}]}}
                    ],
                },
                {
                    **base,
                    "choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}],
                    "usage": {"prompt_tokens": 11, "completion_tokens": 4, "total_tokens": 15},
                },
            ]
        )

    def _send_tool_call(self, name: str, arguments: str) -> None:
        call = {"id": "call_mock_1", "type": "function", "function": {"name": name, "arguments": arguments}}
        self._send(
            200,
            {
                "id": "chatcmpl-mock",
                "object": "chat.completion",
                "created": 0,
                "model": "mock-model",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": None, "tool_calls": [call]},
                        "finish_reason": "tool_calls",
                    }
                ],
                "usage": {"prompt_tokens": 11, "completion_tokens": 4, "total_tokens": 15},
            },
        )

    def do_GET(self) -> None:  # noqa: N802 - stdlib handler naming
        if self.path.rstrip("/").endswith("/models"):
            self._send(200, {"object": "list", "data": [{"id": "mock-model", "object": "model"}]})
            return
        self._send(404, {"error": {"message": "not found"}})

    def do_POST(self) -> None:  # noqa: N802 - stdlib handler naming
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length)
        try:
            parsed = json.loads(body or b"{}")
        except Exception:
            parsed = {}
        streaming = bool(parsed.get("stream"))

        # Hermes opens with a ``/api/show`` model probe. Only a completion
        # request may advance the script, otherwise every entry is one call
        # later than written and a scripted failure never reaches the turn.
        if "completions" not in self.path:
            log(f"PROBE path={self.path} bytes={len(body)}")
            self._send(200, {"model": "mock-model"})
            return

        step = SCRIPT[min(_calls["n"], len(SCRIPT) - 1)].strip()
        _calls["n"] += 1
        log(f"CALL {_calls['n']} path={self.path} step={step} stream={streaming} bytes={len(body)}")

        if step == "429":
            self._send(429, {"error": {"message": "mock rate limit", "type": "rate_limit_error"}}, {"Retry-After": "1"})
            return
        if step == "500":
            self._send(500, {"error": {"message": "mock server error", "type": "server_error"}})
            return
        if step == "401":
            self._send(401, {"error": {"message": "mock invalid key", "type": "authentication_error"}})
            return
        if step == "empty":
            raw = b"data: [DONE]\n\n"
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
            return

        if step == "tool":
            if streaming:
                self._send_sse_tool_call("skills_list", "{}")
            else:
                self._send_tool_call("skills_list", "{}")
            return

        text, finish = "MOCK-REPLY-OK", "stop"
        if step == "scratchpad":
            text, finish = "<REASONING_SCRATCHPAD>thinking, and never closing the tag", "length"

        if streaming:
            self._send_sse(text, finish)
            return

        self._send(
            200,
            {
                "id": "chatcmpl-mock",
                "object": "chat.completion",
                "created": 0,
                "model": "mock-model",
                "choices": [{"index": 0, "message": {"role": "assistant", "content": text}, "finish_reason": finish}],
                "usage": {"prompt_tokens": 11, "completion_tokens": 4, "total_tokens": 15},
            },
        )


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8799
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
