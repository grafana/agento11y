"""Conversation rating transport tests."""

from __future__ import annotations

import json
import threading
from datetime import timedelta, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
from agento11y import (
    ApiConfig,
    AuthConfig,
    Client,
    ClientConfig,
    ConversationRatingInput,
    ConversationRatingValue,
    GenerationExportConfig,
    RatingConflictError,
    ValidationError,
)
from opentelemetry import trace


def test_submit_conversation_rating_over_http_round_trip() -> None:
    captured: dict[str, object] = {}

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            captured["path"] = self.path
            captured["headers"] = {k.lower(): v for k, v in self.headers.items()}
            captured["payload"] = json.loads(body.decode("utf-8"))

            response = {
                "rating": {
                    "rating_id": "rat-1",
                    "conversation_id": "conv-1",
                    "rating": "CONVERSATION_RATING_VALUE_BAD",
                    "created_at": "2026-02-13T12:00:00Z",
                },
                "summary": {
                    "total_count": 1,
                    "good_count": 0,
                    "bad_count": 1,
                    "latest_rating": "CONVERSATION_RATING_VALUE_BAD",
                    "latest_rated_at": "2026-02-13T12:00:00Z",
                    "has_bad_rating": True,
                },
            }
            encoded = json.dumps(response).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        GenerationExportConfig(
            protocol="grpc",
            endpoint="localhost:4317",
            auth=AuthConfig(mode="tenant", tenant_id="tenant-a"),
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        ),
        api_endpoint=f"http://127.0.0.1:{server.server_address[1]}",
    )

    try:
        response = client.submit_conversation_rating(
            "conv-1",
            ConversationRatingInput(
                rating_id="rat-1",
                rating=ConversationRatingValue.BAD,
                comment="wrong answer",
                metadata={"channel": "assistant"},
            ),
        )

        assert captured["path"] == "/api/v1/conversations/conv-1/ratings"
        headers = captured["headers"]
        assert isinstance(headers, dict)
        assert headers.get("x-scope-orgid") == "tenant-a"
        assert captured["payload"] == {
            "rating_id": "rat-1",
            "rating": "CONVERSATION_RATING_VALUE_BAD",
            "comment": "wrong answer",
            "metadata": {"channel": "assistant"},
        }

        assert response.rating.rating_id == "rat-1"
        assert response.rating.conversation_id == "conv-1"
        assert response.summary.has_bad_rating is True
        assert response.summary.bad_count == 1
        assert response.rating.created_at.tzinfo == timezone.utc
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_submit_conversation_rating_maps_conflict() -> None:
    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            encoded = b"idempotency conflict"
            self.send_response(409)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        GenerationExportConfig(
            protocol="http",
            endpoint=f"http://127.0.0.1:{server.server_address[1]}/api/v1/generations:export",
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        ),
        api_endpoint=f"http://127.0.0.1:{server.server_address[1]}",
    )

    try:
        with pytest.raises(RatingConflictError):
            client.submit_conversation_rating(
                "conv-1",
                ConversationRatingInput(
                    rating_id="rat-1",
                    rating=ConversationRatingValue.GOOD,
                ),
            )
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def test_submit_conversation_rating_validates_input() -> None:
    client = _new_client(
        GenerationExportConfig(
            protocol="http",
            endpoint="http://127.0.0.1:8080/api/v1/generations:export",
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        )
    )
    try:
        with pytest.raises(ValidationError):
            client.submit_conversation_rating(
                "conv-1",
                ConversationRatingInput(
                    rating_id="",
                    rating=ConversationRatingValue.GOOD,
                ),
            )
    finally:
        client.shutdown()


def test_submit_conversation_rating_applies_bearer_auth_header() -> None:
    captured_headers: dict[str, str] = {}

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            captured_headers.update({k.lower(): v for k, v in self.headers.items()})
            response = {
                "rating": {
                    "rating_id": "rat-1",
                    "conversation_id": "conv-1",
                    "rating": "CONVERSATION_RATING_VALUE_GOOD",
                    "created_at": "2026-02-13T12:00:00Z",
                },
                "summary": {
                    "total_count": 1,
                    "good_count": 1,
                    "bad_count": 0,
                    "latest_rating": "CONVERSATION_RATING_VALUE_GOOD",
                    "latest_rated_at": "2026-02-13T12:00:00Z",
                    "has_bad_rating": False,
                },
            }
            encoded = json.dumps(response).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def log_message(self, _format, *_args):  # noqa: A003
            return

    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    client = _new_client(
        GenerationExportConfig(
            protocol="http",
            endpoint=f"127.0.0.1:{server.server_address[1]}/api/v1/generations:export",
            insecure=True,
            auth=AuthConfig(mode="bearer", bearer_token="token-a"),
            batch_size=1,
            flush_interval=timedelta(seconds=1),
            max_retries=1,
            initial_backoff=timedelta(milliseconds=1),
            max_backoff=timedelta(milliseconds=10),
        ),
        api_endpoint=f"127.0.0.1:{server.server_address[1]}",
    )

    try:
        client.submit_conversation_rating(
            "conv-1",
            ConversationRatingInput(
                rating_id="rat-1",
                rating=ConversationRatingValue.GOOD,
            ),
        )
        assert captured_headers.get("authorization") == "Bearer token-a"
    finally:
        client.shutdown()
        server.shutdown()
        server.server_close()


def _new_client(generation_export: GenerationExportConfig, api_endpoint: str = "http://localhost:8080") -> Client:
    return Client(
        ClientConfig(
            generation_export=generation_export,
            api=ApiConfig(endpoint=api_endpoint),
            tracer=trace.get_tracer("sigil-sdk-python-rating-test"),
        )
    )
