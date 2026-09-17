"""Local OTLP/HTTP receiver that decodes and logs span and metric names.

Answers "did the plugin export it" without involving a backend. Needed because
sampling on the receiving end (Adaptive Traces) can drop short internal spans,
so a span missing from a trace store says nothing about the exporter.

Requires the OTel proto package, which the plugin already depends on.
"""

from __future__ import annotations

import gzip
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

from opentelemetry.proto.collector.metrics.v1 import metrics_service_pb2
from opentelemetry.proto.collector.trace.v1 import trace_service_pb2

E2E_DIR = os.environ.get("E2E_DIR", "/tmp/agento11y-hermes-e2e")
LOG = os.environ.get("SINK_LOG") or os.path.join(E2E_DIR, "otlp-sink.log")


def log(line: str) -> None:
    with open(LOG, "a") as handle:
        handle.write(line + "\n")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002 - stdlib signature
        pass

    def do_POST(self) -> None:  # noqa: N802 - stdlib handler naming
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        if self.headers.get("Content-Encoding") == "gzip":
            raw = gzip.decompress(raw)
        try:
            if self.path.endswith("/v1/traces"):
                request = trace_service_pb2.ExportTraceServiceRequest()
                request.ParseFromString(raw)
                for resource_spans in request.resource_spans:
                    resource = {a.key: a.value.string_value for a in resource_spans.resource.attributes}
                    for scope_spans in resource_spans.scope_spans:
                        for span in scope_spans.spans:
                            attrs = sorted(a.key for a in span.attributes)
                            # Ids, because a tool span is expected to sit under
                            # the generation span of the call that asked for it,
                            # and this is the one view no sampling can hide.
                            log(
                                f"SPAN {span.name} | status={span.status.code} "
                                f"| trace={span.trace_id.hex()} span={span.span_id.hex()} "
                                f"parent={span.parent_span_id.hex() or '(root)'} "
                                f"| service={resource.get('service.name')} | attrs={attrs}"
                            )
            elif self.path.endswith("/v1/metrics"):
                request = metrics_service_pb2.ExportMetricsServiceRequest()
                request.ParseFromString(raw)
                for resource_metrics in request.resource_metrics:
                    for scope_metrics in resource_metrics.scope_metrics:
                        for metric in scope_metrics.metrics:
                            log(f"METRIC {metric.name}")
            else:
                log(f"OTHER {self.path} bytes={len(raw)}")
        except Exception as exc:
            log(f"DECODE-ERROR {self.path}: {exc!r}")
        self.send_response(200)
        self.send_header("Content-Type", "application/x-protobuf")
        self.send_header("Content-Length", "0")
        self.end_headers()


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8801
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
