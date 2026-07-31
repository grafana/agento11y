"""Programmable local `hooks:evaluate` server for the Python hook tests.

One server whose status, body, and delay are set per test. That covers allow,
deny, transport failure, timeout, and malformed-response cases without a second
server. The server captures each request, so a test can compare the body it
actually sent with the shared fixture.

`plugins/pi/src/testHttp.ts` takes a per-request responder callback instead. The
programmable attributes here suit pytest fixtures better, and the `errors` list
does the same job in both.
"""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

HOOKS_EVALUATE_PATH = "/api/v1/hooks:evaluate"


class HookServer:
    """Local hook-evaluation server with programmable responses and delays."""

    def __init__(self) -> None:
        self.requests: list[dict[str, Any]] = []
        self.response: dict[str, Any] = {"action": "allow", "evaluations": []}
        # raw_body overrides `response` so a test can return a malformed payload.
        self.raw_body: str | None = None
        self.status = 200
        self.delay = 0.0
        self.in_flight = 0
        self.max_in_flight = 0
        # Non-empty when the server could not answer a request. A test that only
        # asserts a fail-open allow passes either way, so the assertion helpers
        # check this list instead of trusting a silent handler.
        self.errors: list[str] = []
        self._lock = threading.Lock()

        server = self

        class _Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"

            def do_POST(self):  # noqa: N802
                length = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(length)
                with server._lock:
                    entry: dict[str, Any] = {
                        "path": self.path,
                        "headers": {k.lower(): v for k, v in self.headers.items()},
                        "raw": body.decode("utf-8", errors="replace"),
                    }
                    try:
                        entry["payload"] = json.loads(body.decode("utf-8"))
                    except Exception:  # noqa: BLE001
                        entry["payload"] = None
                    server.requests.append(entry)
                    server.in_flight += 1
                    server.max_in_flight = max(server.max_in_flight, server.in_flight)
                try:
                    payload = server.raw_body if server.raw_body is not None else json.dumps(server.response)
                    encoded = payload.encode("utf-8")
                    status = server.status
                    delay = server.delay
                except Exception as exc:  # noqa: BLE001
                    with server._lock:
                        server.errors.append(f"could not render the response: {exc}")
                        server.in_flight -= 1
                    return
                try:
                    if delay:
                        time.sleep(delay)
                    self.send_response(status)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(encoded)))
                    self.end_headers()
                    self.wfile.write(encoded)
                except Exception as exc:  # noqa: BLE001
                    # A client that aborted on timeout leaves a closed socket
                    # here. Record it: the timeout cases expect it, and every
                    # other case asserts this list is empty.
                    with server._lock:
                        server.errors.append(f"could not write the response: {exc}")
                finally:
                    with server._lock:
                        server.in_flight -= 1

            def log_message(self, _format, *_args):  # noqa: A003
                return

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        self._server.daemon_threads = True
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self._server.server_address[1]}"

    @property
    def payloads(self) -> list[dict[str, Any]]:
        return [entry["payload"] for entry in self.requests]

    @property
    def request_count(self) -> int:
        return len(self.requests)

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
