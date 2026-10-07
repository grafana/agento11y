"""Requires agento11y and the OTel SDK; a backend checkout is needed only for configured receiver replay.

Set PYTHON_OTLP_PROBE_DIR to keep pairs for the temporary Go overlay. To run it
from pytest as well, set PYTHON_OTLP_RECEIVER_ROOT and
PYTHON_OTLP_RECEIVER_OVERLAY. No network exporter is constructed. The subprocess
gets only HOME/PATH and Go cache locations, with module/toolchain downloads off.
"""

from __future__ import annotations

import copy
import json
import os
import subprocess
from pathlib import Path
from typing import Any

from agento11y.models import Generation
from agento11y.proto_mapping import generation_to_proto
from google.protobuf.json_format import MessageToDict
from google.protobuf.timestamp_pb2 import Timestamp
from opentelemetry.exporter.otlp.proto.common.trace_encoder import encode_spans
from opentelemetry.sdk.trace import ReadableSpan


def otlp_json(span: ReadableSpan) -> dict[str, Any]:
    """Protobuf JSON uses base64 bytes; OTLP/HTTP JSON mandates hex trace/span IDs.
    Attribute strings (including raw JSON content) are never parsed/re-encoded.
    """
    request = encode_spans([span])
    result = MessageToDict(request)
    for resource, encoded_resource in zip(request.resource_spans, result["resourceSpans"], strict=True):
        for scope, encoded_scope in zip(resource.scope_spans, encoded_resource["scopeSpans"], strict=True):
            for wire, encoded in zip(scope.spans, encoded_scope["spans"], strict=True):
                encoded["traceId"] = wire.trace_id.hex()
                encoded["spanId"] = wire.span_id.hex()
                if wire.parent_span_id:
                    encoded["parentSpanId"] = wire.parent_span_id.hex()
                for link, encoded_link in zip(wire.links, encoded.get("links", []), strict=True):
                    encoded_link["traceId"] = link.trace_id.hex()
                    encoded_link["spanId"] = link.span_id.hex()
    return result


def expected_generation(span: ReadableSpan, source: Generation | dict[str, Any]) -> dict[str, Any]:
    """Expected normalized protobuf, not a Python simulation of receiver decode.

    The Client source is recorder.last_generation: normalization merges client
    tags, adds sdk.name/content_capture_mode metadata plus title/user.id mirrors,
    and fills a missing usage.total_tokens with input+output. Fixture dictionaries
    retain original content and do not receive these Client-only additions.
    Explicit transport differences: chat operation, live utility times/context,
    and omitted zero usage (the native mapper always constructs a usage message).
    Reserved call_error metadata is not exported; title is reconstructed by the
    receiver from its dedicated attribute. Effective-version hashing is performed
    by the existing native mapper (shared fixture dictionaries are hashed by the
    caller). No content/bytes/tags/metadata are otherwise discarded.
    """
    if isinstance(source, Generation):
        proto = generation_to_proto(source)
        if not any(
            (
                source.usage.input_tokens,
                source.usage.output_tokens,
                source.usage.total_tokens,
                source.usage.cache_read_input_tokens,
                source.usage.cache_write_input_tokens,
                source.usage.reasoning_tokens,
            )
        ):
            proto.ClearField("usage")
        expected = MessageToDict(proto, preserving_proto_field_name=True)
        metadata = expected.get("metadata", {})
        metadata.pop("call_error", None)
        metadata.pop("sigil.conversation.title", None)
        if not metadata:
            expected.pop("metadata", None)
    else:
        # The Go SYSTEM fixture is not representable by Python's public role
        # enum/native mapper. Keep its original protobuf expectation.
        expected = copy.deepcopy(source)
    context = span.get_span_context()
    assert context.is_valid
    assert span.start_time and span.end_time and span.end_time >= span.start_time
    expected["trace_id"] = f"{context.trace_id:032x}"
    expected["span_id"] = f"{context.span_id:016x}"
    for field, nanos in (("started_at", span.start_time), ("completed_at", span.end_time)):
        timestamp = Timestamp(seconds=nanos // 1_000_000_000, nanos=nanos % 1_000_000_000)
        expected[field] = timestamp.ToJsonString()
    expected["operation_name"] = "chat"
    return expected


def write_pair(directory: Path, name: str, span: ReadableSpan, source: Generation | dict[str, Any]) -> None:
    directory.mkdir(parents=True, exist_ok=True)
    (directory / f"{name}.span.json").write_text(json.dumps(otlp_json(span), ensure_ascii=False))
    (directory / f"{name}.generation.json").write_text(
        json.dumps(expected_generation(span, source), ensure_ascii=False)
    )


def run_receiver(directory: Path, *, rejected: bool = False) -> subprocess.CompletedProcess[str] | None:
    root = os.environ.get("PYTHON_OTLP_RECEIVER_ROOT")
    overlay = os.environ.get("PYTHON_OTLP_RECEIVER_OVERLAY")
    if not root and not overlay:
        return None
    if not root or not overlay:
        raise ValueError("set both PYTHON_OTLP_RECEIVER_ROOT and PYTHON_OTLP_RECEIVER_OVERLAY")
    env = {key: os.environ[key] for key in ("HOME", "PATH", "GOCACHE", "GOMODCACHE", "GOPATH") if key in os.environ}
    env.update(GOPROXY="off", GOSUMDB="off", GOTOOLCHAIN="local")
    pattern = "^TestPythonLocalReceiverFixtures$"
    if rejected:
        pattern = "^TestPythonLocalReceiver(Fixtures|ActualRejected)$"
    return subprocess.run(
        [
            "go",
            "test",
            "-mod=readonly",
            f"-overlay={Path(overlay).resolve()}",
            "./internal/ingest/otlptrace",
            "-run",
            pattern,
            "-count=1",
            "-v",
            "-args",
            f"-probe-dir={directory.resolve()}",
            "-probe-operation=chat",
            "-probe-raw-effective-version=false",
            "-probe-format=otlp-json",
        ],
        cwd=root,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=180,
    )
