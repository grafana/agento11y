# Agent Observability for Open WebUI

The `agento11y-open-webui` package adds an administrator-installed
[Open WebUI](https://openwebui.com/) Filter. It exports one snapshot after each
supported assistant response without changing the request or response body.

The Filter records response-level data. It does not intercept each provider call
inside an agent or tool loop.

## Requirements

- Open WebUI 0.11.3 or later
- Python 3.10 or later
- A Grafana Cloud Agent Observability API URL, instance ID, and access token

Refer to
[Set up Agent Observability in Grafana Cloud](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/get-started/grafana-cloud/)
to get the connection values.

## Install the package

Install the package in the Python environment that runs the Open WebUI backend.
For the official container, build a derived image so the package remains
installed after a restart or upgrade:

```dockerfile
FROM ghcr.io/open-webui/open-webui:v0.11.3

ARG AGENTO11Y_VERSION
RUN test -n "${AGENTO11Y_VERSION}" && pip install --no-cache-dir \
    "agento11y==${AGENTO11Y_VERSION}" \
    "agento11y-open-webui==${AGENTO11Y_VERSION}"
```

Pass a released SDK version as `AGENTO11Y_VERSION` when you build the image.
This pins the core SDK and Filter package to the same version.

## Configure Agent Observability

Set these variables on the Open WebUI backend before it starts:

```dotenv
AGENTO11Y_ENDPOINT=<AGENT_OBSERVABILITY_API_URL>
AGENTO11Y_PROTOCOL=http
AGENTO11Y_AUTH_MODE=basic
AGENTO11Y_AUTH_TENANT_ID=<INSTANCE_ID>
AGENTO11Y_AUTH_TOKEN=<ACCESS_TOKEN>
```

Grafana Cloud requires `AGENTO11Y_PROTOCOL=http` and
`AGENTO11Y_AUTH_MODE=basic`. Set `AGENTO11Y_AGENT_NAME` to change the exported
agent name. The default for this Filter is `open-webui`.

## Install the Filter

In Open WebUI, open **Admin panel > Functions** and create a Function with this
body:

```python
"""
title: Grafana Agent Observability
required_open_webui_version: 0.11.3
"""

from agento11y_open_webui import Filter
```

Save the Function as a Filter. Activate it and make it global. The Function
body imports the package from the backend image and does not install dependencies
at runtime.

## Enable token reporting

For each streamed model, edit its capabilities in Open WebUI and enable
**Usage**. Open WebUI then requests usage from compatible providers with
`stream_options.include_usage`. Token data remains unavailable unless the provider
reports normalized `input_tokens` and `output_tokens`. The Filter uses
`total_tokens` when present and otherwise sums the input and output counts. It
ignores `prompt_tokens`, `completion_tokens`, and cache or reasoning counts in
`input_tokens_details` and `output_tokens_details`.

## Configure OpenTelemetry

Generation export and OpenTelemetry (OTel) export are separate paths. Generation
snapshots can reach Agent Observability when OTel is disabled, but trace- and
metric-backed views remain empty.

Open WebUI can register the global OTel providers that the Python SDK uses for
generation spans and `gen_ai.client.token.usage` metrics. Enable Open WebUI's
OTel setup, traces, and metrics:

```dotenv
ENABLE_OTEL=true
ENABLE_OTEL_TRACES=true
ENABLE_OTEL_METRICS=true
OTEL_EXPORTER_OTLP_ENDPOINT=<OTLP_ENDPOINT>
OTEL_METRICS_EXPORTER_OTLP_ENDPOINT=<OTLP_ENDPOINT>
OTEL_BASIC_AUTH_USERNAME=<OTLP_INSTANCE_ID>
OTEL_BASIC_AUTH_PASSWORD=<ACCESS_TOKEN>
```

Use the OTLP endpoint and instance ID from your Grafana Cloud OpenTelemetry
connection. Refer to
[Open WebUI OpenTelemetry configuration](https://docs.openwebui.com/reference/monitoring/otel/)
for transport, TLS, and signal-specific settings. Open WebUI also emits service
request, database, Redis, and system telemetry. Those signals do not replace the
Filter's generation snapshots.

## Content capture and privacy

When capture is not configured, the Filter uses `metadata_only` instead of the
core SDK's content-enabled default. It exports no user or assistant message text
in this mode.

To export the selected user and assistant text, set a content-enabled mode before
starting Open WebUI. For example:

```dotenv
AGENTO11Y_CONTENT_CAPTURE_MODE=full
```

Captured text passes through the SDK sanitizer for known secret formats, with
input redaction enabled. Redaction is best effort and does not guarantee removal
of every sensitive value.

The Filter exports message content when it is a string. For block content, it
exports text only from top-level `text`, `input_text`, and `output_text` blocks.
It omits system prompts, thinking blocks, tool inputs and results, attachments,
titles, provider errors, and arbitrary host metadata. The opaque Open WebUI user
ID, chat ID, selected message ID, and selected model ID remain present in
`metadata_only` mode.

## Snapshot behavior

Each matched `inlet` and `outlet` pair can produce one generation:

- The Open WebUI chat ID is the conversation ID. A direct API call without a chat
  ID uses the generation ID that the Filter created for the snapshot as its
  conversation ID.
- The selected assistant is the message whose ID matches the outlet body ID.
- The input is the last user message before that assistant.
- The exported model is `custom/open-webui-response-summary`. The selected Open
  WebUI model ID remains metadata, so the record does not claim provider
  attribution or model pricing.
- Each invocation gets a new generation ID, including Continue and tool-approval
  resumes.
- Continue and approval-resume snapshots omit token counts to avoid counting
  retained usage twice.
- Streaming requests use streaming generation mode. The Filter does not report
  time to first token.

A standalone outlet callback does not create a snapshot. Repeating an outlet
callback does not create another snapshot. Responses without a matching outlet
are not exported. These include some
failures, cancellations, tool-only responses, and approval pauses.

The Filter attempts export at most once within one Open WebUI request lifecycle.
It does not deduplicate across processes or workers.

## Direct API requests

In Open WebUI 0.11.3, streaming and non-streaming
`POST /api/chat/completions` requests run inline outlet Filters when
`ENABLE_API_OUTLET_FILTERS=true`. This setting defaults to `true` in that
release. Keep it enabled to export direct API response snapshots.

The legacy `POST /api/chat/completed` callback does not share the original inlet
state, so this Filter ignores it. Routes that bypass the standard chat-completion
Filter lifecycle are not supported.

## Delivery and failures

The SDK queues exports in memory and sends them in the background. A queued
snapshot does not confirm delivery. An abrupt process exit can lose queued data,
and queues do not transfer between Open WebUI workers.

Snapshot mapping, recorder setup, finalization, and queueing errors do not reject
or modify the chat response. The backend logs
`agento11y Open WebUI export failed: <category>` for these errors without
including response payloads or exception text. The core SDK reports background
delivery failures separately and can include exporter error text.

If snapshots do not appear:

1. Confirm that both packages are installed in the running backend image.
1. Confirm that the Function is active for the selected model.
1. Confirm that the `AGENTO11Y_*` variables are visible to the backend process.
1. For streamed models, confirm that **Usage** is enabled and that the provider
   returns usage.
1. For direct API calls, confirm that `ENABLE_API_OUTLET_FILTERS=true`.
