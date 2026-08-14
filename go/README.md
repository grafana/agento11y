# Grafana Agent Observability Go SDK

The agento11y Go SDK records LLM generations and tool calls for [Grafana Agent observability](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/). It emits OpenTelemetry spans and metrics through your existing OTel setup and sends normalized generation payloads through the Agent Observability ingest channel.

## Install

```sh
go get github.com/grafana/agento11y/go
```

## Quick start

```go
client := agento11y.NewClient(agento11y.Config{}) // reads AGENTO11Y_* env vars
defer func() { _ = client.Shutdown(context.Background()) }()

ctx, rec := client.StartGeneration(ctx, agento11y.GenerationStart{
	ConversationID: "conv-9b2f",
	AgentName:      "assistant-core",
	AgentVersion:   "1.0.0",
	Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
})
defer rec.End()

resp, err := provider.Call(ctx, req)
if err != nil {
	rec.SetCallError(err)
	return err
}

rec.SetResult(agento11y.Generation{
	Input:  []agento11y.Message{agento11y.UserTextMessage("Hello")},
	Output: []agento11y.Message{agento11y.AssistantTextMessage(resp.Text)},
	Usage:  agento11y.TokenUsage{InputTokens: 120, OutputTokens: 42},
}, nil)
```

See `Configuration` for the explicit-config form and `Recording API` for the full surface.

Framework helpers:

- Google ADK: [`go-frameworks/google-adk`](../go-frameworks/google-adk/README.md)

## Core model

- `Generation` is the canonical entity.
- `Generation.Mode` is explicit: `SYNC` or `STREAM`.
- `OperationName` defaults are mode-aware:
  - `SYNC` -> `generateText`
  - `STREAM` -> `streamText`
- `ModelRef` bundles `provider + model`.
- `ConversationTitle` is an optional human-readable label for the conversation.
- `AgentName` and `AgentVersion` are optional generation/tool identity fields.
- `SystemPrompt` is separate from messages.
- `ToolDefinition.Deferred` records whether a tool is marked as deferred.
- Request controls are optional first-class fields:
  - `MaxTokens`
  - `Temperature`
  - `TopP`
  - `ToolChoice`
  - `ThinkingEnabled`
- `Message` contains typed parts: `text`, `thinking`, `tool_call`, `tool_result`.
- Normalized `tool_result` correlation is provider-safe:
  - Preserve `tool_result.tool_call_id` whenever the upstream provider exposes a stable per-call identifier.
  - When the upstream surface omits a per-call ID, populate `tool_result.name` with the tool/function name as the fallback correlation key.
  - Local validation requires at least one of `tool_result.tool_call_id` or `tool_result.name`.
- `TokenUsage` includes token/cache/reasoning fields.
- Raw provider `Artifacts` are optional debug payloads.

## Recording API (explicit, OTel-like)

- `StartGeneration(ctx, start)` -> `(ctx, *GenerationRecorder)`
- `StartStreamingGeneration(ctx, start)` -> `(ctx, *GenerationRecorder)`
- `StartToolExecution(ctx, start)` -> `(ctx, *ToolExecutionRecorder)`
- `rec.SetResult(...)` / `rec.SetCallError(...)`
- `rec.End()` is defer-safe and idempotent.
- `rec.Err()` reports local validation/enqueue failures only.
- Background export failures are retried and logged.
- Generation spans emit request controls using GenAI keys where standardized:
  - `gen_ai.request.max_tokens`
  - `gen_ai.request.temperature`
  - `gen_ai.request.top_p`
  - `agento11y.gen_ai.request.tool_choice`
  - `agento11y.gen_ai.request.thinking.enabled`
  - `agento11y.gen_ai.request.thinking.budget_tokens` (provider-specific)
  - `gen_ai.response.finish_reasons` is emitted as a string array.
- Generation/tool spans always include SDK identity attributes:
  - `agento11y.sdk.name=sdk-go`
- Normalized generation metadata always includes the same SDK identity key; conflicting caller values are overwritten.
- Context helpers are available for defaults:
  - `WithConversationID(ctx, id)`
  - `WithConversationTitle(ctx, title)`
  - `WithAgentName(ctx, name)`
  - `WithAgentVersion(ctx, version)`

## Configuration

The snippet below configures the SDK explicitly. As an alternative, set `AGENTO11Y_*` environment variables and pass an empty `agento11y.Config{}` — refer to the [Grafana Cloud setup guide](https://grafana.com/docs/grafana-cloud/machine-learning/agent-observability/get-started/grafana-cloud/) for the variable names.

```go
client := agento11y.NewClient(agento11y.Config{})
defer func() { _ = client.Shutdown(context.Background()) }()
```

For explicit configuration with custom auth or batch tuning:

```go
cfg := agento11y.DefaultConfig()

// Optional: inject tracer/meter explicitly.
// If unset, the SDK uses otel.Tracer(...) and otel.Meter(...).
// cfg.Tracer = myTracer
// cfg.Meter = myMeter

// Generation export to Grafana Cloud.
cfg.GenerationExport.Protocol = agento11y.GenerationExportProtocolHTTP
cfg.GenerationExport.Endpoint = "https://agento11y-prod-<region>.grafana.net"
cfg.GenerationExport.Auth = agento11y.AuthConfig{
	Mode:          agento11y.ExportAuthModeBasic,
	TenantID:      os.Getenv("AGENTO11Y_AUTH_TENANT_ID"),
	BasicPassword: os.Getenv("AGENTO11Y_AUTH_TOKEN"),
}
cfg.GenerationExport.BatchSize = 100
cfg.GenerationExport.FlushInterval = time.Second
cfg.GenerationExport.QueueSize = 2000
cfg.GenerationExport.MaxRetries = 5
cfg.GenerationExport.InitialBackoff = 100 * time.Millisecond
cfg.GenerationExport.MaxBackoff = 5 * time.Second
cfg.GenerationExport.ExportTimeout = 30 * time.Second
cfg.GenerationExport.GRPCMaxSendMessageBytes = 16 << 20
cfg.GenerationExport.GRPCMaxReceiveMessageBytes = 16 << 20
cfg.GenerationExport.PayloadMaxBytes = 16 << 20

// Agent Observability API base used by helpers like SubmitConversationRating.
cfg.API.Endpoint = "https://agento11y-prod-<region>.grafana.net"

client := agento11y.NewClient(cfg)
defer func() {
	_ = client.Shutdown(context.Background())
}()
```

`GenerationExport.ExportTimeout` bounds each HTTP or gRPC generation and workflow-step request. It defaults to 30 seconds. Set `AGENTO11Y_EXPORT_TIMEOUT_MS` to a base-10 integer from `1` through `2147483647` to override the default. A positive caller value wins over the environment variable.

`GenerationExport.HTTPTimeout` remains an HTTP-only override. A positive value wins over `ExportTimeout` on HTTP requests. The experiments client uses this field for `ClientOptions.RetryTimeout`.

Configure OTEL exporters (traces/metrics) in your application OTEL SDK setup.

Quick OTEL setup pattern before creating the agento11y client:

```go
tp := sdktrace.NewTracerProvider()
otel.SetTracerProvider(tp)

mp := sdkmetric.NewMeterProvider()
otel.SetMeterProvider(mp)
```

The providers above have no exporters attached, so nothing leaves the process. See
[OpenTelemetry Setup](../docs/concepts/otel-setup.md) for the full wiring, including the OTLP
exporters, and why analytics stays empty when this step is skipped.

### Instrumentation-only mode (no generation send)

Use `GenerationExportProtocolNone` to keep generation and tool instrumentation active while disabling generation transport:

```go
cfg := agento11y.DefaultConfig()
cfg.GenerationExport.Protocol = agento11y.GenerationExportProtocolNone

client := agento11y.NewClient(cfg)
defer func() { _ = client.Shutdown(context.Background()) }()
```

## Generation export auth modes

Auth is configured for generation export.

- `none`
- `tenant` (requires `TenantID`, injects `X-Scope-OrgID`)
- `bearer` (requires `BearerToken`, injects `Authorization: Bearer <token>`)
- `basic` (requires `BasicPassword` + `BasicUser` or `TenantID`, injects `Authorization: Basic <base64(user:password)>`; also injects `X-Scope-OrgID` when `TenantID` is set — for multi-tenant deployments only, not needed for Grafana Cloud)

Invalid combinations fail fast during `NewClient(...)`.

```go
cfg.GenerationExport.Auth = agento11y.AuthConfig{
	Mode:        agento11y.ExportAuthModeBearer,
	BearerToken: "token-from-secret-manager",
}
```

Explicit transport headers remain the highest-precedence escape hatch. If `Headers` already contains `Authorization` or `X-Scope-OrgID`, the SDK does not overwrite them.

### Grafana Cloud auth (basic)

For Grafana Cloud, use `basic` auth mode. The username is your Grafana Cloud instance/tenant ID and the password is your Grafana Cloud API key:

```go
cfg.GenerationExport.Auth = agento11y.AuthConfig{
	Mode:          agento11y.ExportAuthModeBasic,
	TenantID:      os.Getenv("AGENTO11Y_AUTH_TENANT_ID"),
	BasicPassword: os.Getenv("AGENTO11Y_AUTH_TOKEN"),
}
```

If your deployment requires a distinct username (different from the tenant ID), set `BasicUser` explicitly:

```go
cfg.GenerationExport.Auth = agento11y.AuthConfig{
	Mode:          agento11y.ExportAuthModeBasic,
	TenantID:      os.Getenv("AGENTO11Y_AUTH_TENANT_ID"),
	BasicUser:     os.Getenv("AGENTO11Y_AUTH_TENANT_ID"),
	BasicPassword: os.Getenv("AGENTO11Y_AUTH_TOKEN"),
}
```

## Hooks and Guards

Use hooks when you want Agent Observability guard rules to run before an LLM call. The SDK evaluates the hook on your request path; guard rules configured in Grafana Cloud decide whether to allow, deny, or transform the input.

Hooks are disabled by default. Enable them on the client and call `EvaluateHook(...)` before the provider request:

```go
cfg := agento11y.DefaultConfig()
cfg.Hooks.Enabled = true
cfg.Hooks.Phases = []agento11y.HookPhase{agento11y.HookPhasePreflight}

client := agento11y.NewClient(cfg)

messages := []agento11y.Message{
	agento11y.UserTextMessage("Summarize this customer note..."),
}
response, err := client.EvaluateHook(ctx, agento11y.HookEvaluateRequest{
	Phase: agento11y.HookPhasePreflight,
	Context: agento11y.HookContext{
		AgentName:    "support-agent",
		AgentVersion: "1.0.0",
		Model:        &agento11y.HookModel{Provider: "openai", Name: "gpt-5"},
		ConversationID: "support-case-42",
	},
	Input: agento11y.HookInput{
		Messages:            messages,
		SystemPrompt:        "You are a helpful support agent.",
		ConversationPreview: "Summarize this customer note...",
	},
})
if err != nil {
	return err
}
if err := agento11y.HookDeniedFromResponse(response); err != nil {
	return err
}
if response.TransformedInput != nil && len(response.TransformedInput.Messages) > 0 {
	messages = response.TransformedInput.Messages
}
```

`HooksConfig` defaults to `Phases: []HookPhase{HookPhasePreflight}`, `Timeout: 15s`, and fail-open behavior. With fail-open enabled, hook transport errors resolve to allow so an unavailable evaluator does not block production traffic. Set `FailOpen` to `agento11y.BoolPtr(false)` for strict paths that should fail closed.

Set `HookContext.ConversationID` to the same ID used by `StartGeneration(...)`. The SDK also reads `WithConversationID(...)` and the active OpenTelemetry span when explicit correlation fields are omitted. This lets Agent Observability retain denied preflight attempts even though no LLM generation is created.

If you use transformed input, pass the transformed messages/system prompt to the provider and record those same values in `StartGeneration(...)`. For a runnable example, see [`../examples/getting-started/go-hooks/`](../examples/getting-started/go-hooks/).

## Experimental: export generations as OTel spans

> **Experimental.** Set both `AGENTO11Y_PROTOCOL=otel` and `AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true`. With only the protocol set, `NewClient` logs `agento11y otel generation export unavailable: ...` and delivers no generations, while the metadata span and the SDK's own four metrics keep working as they do on the HTTP protocol.

In otel mode each finished generation leaves the process as one GenAI semantic-convention span on your application's OTel pipeline, and the `generations:export` endpoint is not called. The span is a `CLIENT` span named `chat <model>`. It carries `gen_ai.*` attributes from semconv v1.41.0, plus the `agento11y.*` extension attributes the backend decodes back into a generation. The SDK records `gen_ai.client.operation.duration`, `gen_ai.client.token.usage`, and `gen_ai.client.operation.time_to_first_chunk` instead of its own four metrics. Direct `otelgenai` callers can record `gen_ai.client.operation.time_per_output_chunk` with `Handler.RecordChunk`.

Pass the providers, not just a tracer and a meter:

```go
client := agento11y.NewClient(agento11y.Config{
	GenerationExport: agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolOTel},
	TracerProvider:   tracerProvider,
	MeterProvider:    meterProvider,
})
```

`Flush` force-flushes `Config.TracerProvider`, and without it `Flush` returns `ErrFlushNotVerifiable` rather than a success it cannot back. The generation-export handler gets its tracer and spec meter from `Config.TracerProvider` and `Config.MeterProvider`, or the corresponding global providers. `Config.Tracer` and `Config.Meter` do not affect generation export; they continue to configure the SDK's tool-execution and embedding telemetry in every mode. Only an explicit tracer provider gives `Flush` a delivery boundary it can verify.

What changes when the mode is on:

- Delivery is your span processor's. The SDK installs no processor, keeps no export queue of its own, retries nothing, and gets no per-generation result. A batching processor drops spans silently when its queue overflows, and an unsampled trace never reaches it.
- `Flush` force-flushes the tracer provider instead of draining the export queue, so it still blocks until the spans leave the process and still reports the exporter's error.
- `ExportGeneration` returns `ErrSynchronousExportUnsupported`: a span processor decides on its own when to export, so there is no delivery result to wait for.
- Workflow steps have no span encoding. `EnqueueWorkflowStep` drops them and returns `ErrWorkflowStepEnqueueFailed`.
- The metrics keep their `agento11y.tag.*` dimensions, the error category, and the token-semantics marker, and add `gen_ai.response.model`. `gen_ai.client.operation.time_to_first_chunk` replaces `gen_ai.client.time_to_first_token`, and `gen_ai.client.tool_calls_per_operation` is not emitted at all, so panels keyed on either name stay empty for otel-mode traffic.
- `gen_ai.operation.name` is `chat` where the SDK's own defaults are `generateText` and `streamText`, on the span and on the metrics. A caller's own operation name rides through unchanged. The streaming half carries the span attribute `gen_ai.request.stream=true` and the sync half omits it, so filter for its presence rather than for `false`. `gen_ai.request.stream` is never a metric dimension.
- A generation the SDK's own validator refuses is not delivered, as on every other protocol. Its span still closes, carrying the error but neither the generation id nor the message content.

Content capture follows `AGENTO11Y_CONTENT_CAPTURE_MODE`, per call as well as per client. otel mode does not read the conventions' `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`: the span is the export, so a traces-side variable must not decide what the export contains. `metadata_only` emits the span without any content attribute. `full_with_metadata_spans` exists to keep content off the OTel destination, and in otel mode there is no other destination, so it exports no content at all; `NewClient` logs that.

[`go/otelgenai`](otelgenai/) produces the span. It is a GenAI-semconv instrumentation util with no agento11y dependency, so used on its own it emits plain `gen_ai` spans any OTel backend can read. A completion hook in [`agento11y/otelhook`](agento11y/otelhook/) adds the `agento11y.*` attributes.

Direct `otelgenai` callers can also emit one `gen_ai.client.inference.operation.details` log record for each completed `chat`, `text_completion`, `generate_content`, or caller-defined operation. `embeddings`, `execute_tool`, `invoke_agent`, `retrieval`, `fetch_response`, `invoke_workflow`, `create_agent`, and `plan` do not emit these records.

The records need an OTel log provider. The handler uses the global provider by default, which is a no-op until the application installs one, and under `EVENT_ONLY` that leaves the content nowhere: off the span, and in no exported event. `EVENT_ONLY` and `SPAN_AND_EVENT` are values of `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`, and both enable the records by default. `OTEL_INSTRUMENTATION_GENAI_EMIT_EVENT=true` or `false` overrides that default, and `otelgenai.WithEmitEvent` overrides the environment. Set to `true` under `NO_CONTENT` or `SPAN_ONLY`, it emits records without message content. The agento11y SDK's otel mode disables the records, because it configures no log destination.

## Wiring custom env vars

The SDK only auto-loads `AGENTO11Y_*` env vars (`AGENTO11Y_ENDPOINT`, `AGENTO11Y_PROTOCOL`, `AGENTO11Y_AUTH_MODE`, `AGENTO11Y_AUTH_TOKEN`, etc.) when you call `agento11y.NewClient(agento11y.Config{})`. For any other env var (for example one your secret manager exposes under a different name), read it in your app and pass the value into the config:

```go
genToken := strings.TrimSpace(os.Getenv("MY_APP_AGENTO11Y_TOKEN"))
if genToken != "" {
	cfg.GenerationExport.Auth = agento11y.AuthConfig{
		Mode:        agento11y.ExportAuthModeBearer,
		BearerToken: genToken,
	}
}
```

Common topology:

- Grafana Cloud: generation `basic` mode with instance ID and API key.
- Self-hosted direct to the ingest API: generation `tenant` mode.
- Traces/metrics via OTEL Collector/Alloy: configure exporters in your app OTEL SDK setup.
- Enterprise proxy: generation `bearer` mode to proxy; proxy authenticates and forwards tenant header upstream.

## Offline experiments

Use `github.com/grafana/agento11y/go/agento11y/experiments` when an existing
benchmark, CI job, notebook, or agent harness owns execution and Agent
Observability should track the run. The package publishes typed trials,
generations, scores, evaluations, usage/cost, and artifacts; it does not
schedule work.

Suite-free publishing needs `AGENTO11Y_ENDPOINT`, `AGENTO11Y_AUTH_TOKEN`, and
optional `AGENTO11Y_AUTH_TENANT_ID`:

```go
client, err := experiments.NewClientFromEnv()
if err != nil {
	return err
}
defer client.Shutdown(context.Background())

planned := len(cases) * attempts // optional; never inferred from suite size
run, err := experiments.WithExperiment(ctx, client, experiments.ExperimentOptions{
	ExperimentID:      stableResumeID,
	Name:              "nightly",
	PlannedTrialCount: &planned,
	Candidate: &experiments.Candidate{
		AgentName: "support-agent", ModelName: "gpt-5", GitSHA: gitSHA,
	},
}, func(ctx context.Context, run *experiments.Experiment) error {
	for _, testCase := range cases {
		for attempt := 1; attempt <= attempts; attempt++ {
			err := run.WithTrial(ctx, testCase, func(ctx context.Context, trial *experiments.Trial) error {
				output := runAgent(testCase.Input)
				trial.RecordIO(experiments.RecordIOOptions{Input: testCase.Input, Output: output})
				if _, err := trial.CheckScore("json_valid", validJSON(output), experiments.ScoreOptions{}); err != nil {
					return err
				}
				didPass := passed(output)
				if _, err := trial.FinalScore(score(output), experiments.ScoreOptions{Passed: &didPass}); err != nil {
					return err
				}
				_, err := trial.Flush(ctx) // publish this scored attempt immediately
				return err
			}, experiments.TrialOptions{Attempt: attempt})
			if err != nil {
				return err
			}
		}
	}
	return nil
})
```

Keep `ExperimentID`, case ID, and attempt stable when resuming. The SDK derives
stable trial/generation/conversation IDs and occurrence-aware score IDs from
them. Reusing the same case/attempt twice in one run is rejected; increment the
attempt for genuinely new work. Normal finalization omits `score_count`, which
is appropriate for distributed runners. Supply `FinalizeOptions.ScoreCount`
only when the count is an intentional server-side assertion.

Portable suites accept `id`/`test_case_id` and `cases`/`test_cases` YAML aliases:

```go
suite, err := experiments.LoadSuite("evals/smoke.yaml")
suites, err := experiments.NewTestSuitesClient(experiments.TestSuitesClientOptions{})
pushed, err := suites.PushSuite(ctx, *suite, experiments.PushSuiteOptions{
	Prune: true, Publish: true, Changelog: "nightly sync",
})
```

Stored-suite operations additionally use `AGENTO11Y_CONTROL_ENDPOINT` (or
`AGENTO11Y_GRAFANA_URL`) and `AGENTO11Y_SERVICE_ACCOUNT_TOKEN`. Run ingest
continues to use the ingest credential. `NewExperimentFromSuite` and
`WithExperimentFromSuite` resolve exact, `latest`, `latest_published`, or
`draft` versions before starting, so the selected version is durable.

### Grading with an evaluator stored in your tenant

> **Experimental.** Set `AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true` to use
> this. Without it, `Trial.Evaluate`, `Client.TriggerTrialEvaluation`, and
> `Client.GetTrialEvaluation` return `agento11y.ErrExperimentalFeatureDisabled`
> without sending a request. Experimental features can change or be removed in
> any release.

`Trial.Evaluate` grades the conversation Agent Observability already stored,
using an evaluator defined in your tenant, instead of a score the runner
computes. `Trial.EvaluateOutput` is the in-process judge; `Trial.Evaluate` is
the stored one.

```go
if err := run.WithTrial(ctx, testCase, func(ctx context.Context, trial *experiments.Trial) error {
	conversationID := runAgent(ctx, testCase.Input) // already instrumented
	trial.BindConversation(conversationID)

	// Blocks until the worker reaches a terminal status.
	evaluation, err := trial.Evaluate(ctx, "helpfulness", experiments.EvaluateOptions{
		EvaluatorVersion: "v3", // optional; empty pins the latest active version
	})
	if err != nil {
		var failed *agento11y.TrialEvaluationFailedError
		if errors.As(err, &failed) {
			log.Printf("evaluation %s failed: %s", failed.EvaluationID, failed.Detail)
		}
		return err
	}
	log.Printf("stored evaluator finished: %s", evaluation.Status)
	return nil // no FinalScore: the backend owns the verdict
}); err != nil {
	return err
}
```

The call persists the trial's conversation binding and exports the anchor
generation before queueing the evaluation, because the backend rejects an
evaluation for a trial with no stored conversation. A trial graded this way
closes as `completed` without a local `FinalScore`; an error returned from the
trial callback afterwards still closes it as `errored`.

`EvaluateOptions.Timeout` (default 300s) bounds the wait, and `ctx` cancellation
ends it too. Worker failure returns `*agento11y.TrialEvaluationFailedError`, an
exceeded deadline returns `*agento11y.TrialEvaluationTimeoutError`, and both
carry the evaluation ID; `errors.Is` matches `agento11y.ErrTrialEvaluationFailed`
and `agento11y.ErrTrialEvaluationTimeout`. The poll interval doubles from
`EvaluateOptions.PollInterval` (default 500ms) up to 5s, so a long wait does not
keep reading status at the floor rate. The evaluation is keyed by trial,
conversation, evaluator, and resolved evaluator version, so triggering the same
combination returns the existing evaluation instead of running it twice, and
requeues it once it has failed.

Leave `ScoreCount` unset when a run uses cloud evaluation. Agent Observability
checks an asserted count against every stored score, including the ones the
stored evaluator wrote, so a locally derived count conflicts. Both
`experiments.Experiment` and the root-package `ExperimentRun` drop a count of
their own once one of their trials queued a cloud evaluation. A trial built
directly with `experiments.NewTrial` (or the root `agento11y.NewTrial`) has no
run to mark, so a runner that finalizes such a run has to leave the count unset
itself.

The score is attached to the conversation and the trial with no generation ID, so
read it from a trial's `Scores` in `Experiment.Report`, not from a per-generation
lookup. It carries the evaluator's own score key, and only a score under the
`final` key feeds the report's pass rate, so `Summary.PassRate` stays nil for a
run graded only in the cloud.

A wait that ends in a timeout, a cancelled context, or a transport error leaves
the evaluation running server-side. Finalizing the run as `completed` while an
evaluation is still queued returns `agento11y.ErrExperimentConflict`; call
`Evaluate` again to wait for the same evaluation, then finalize. `Evaluate` also
blocks the next trial, so to grade several at once trigger with
`Client.TriggerTrialEvaluation` and poll `Client.GetTrialEvaluation` yourself,
keeping both inside the experiment.

Local `LLMJudge` and `RegexJudge` helpers require no platform evaluator.
`Trial.RecordEvaluation` also accepts framework-owned evaluations without
reinterpreting their transcript. If an evaluation includes a grader generation,
the SDK publishes and links it before its score. Secret redaction is enabled by
default for generations, scores, explanations, metadata, and text-like
artifacts. Experimental trial spans and `gen_ai.evaluation.result` events are
opt-in with `AGENTO11Y_USE_EXPERIMENTAL_OTEL=true`.

See the runnable [Go streaming example](../examples/experiments/go/) and the
[stored-evaluator example](../examples/experiments/go/cloud-evaluator/).

## Content Capture Mode

`ContentCaptureMode` controls what content the SDK includes in exported generation payloads and OTel span attributes. See [Content Capture Modes](../docs/concepts/content-capture-modes.md) for the canonical mode matrix and defaults; the snippets below show how to wire it up in Go.

Client-level default:

```go
cfg := agento11y.DefaultConfig()
cfg.ContentCapture = agento11y.ContentCaptureModeMetadataOnly

client := agento11y.NewClient(cfg)
defer func() { _ = client.Shutdown(context.Background()) }()
```

The core SDK client treats `ContentCaptureModeDefault` as `ContentCaptureModeNoToolContent`: generation content is captured but tool-execution arguments and results stay out of spans.

Per-generation override:

```go
ctx, rec := client.StartGeneration(ctx, agento11y.GenerationStart{
    Model:          agento11y.ModelRef{Provider: "openai", Name: "gpt-5"},
    ContentCapture: agento11y.ContentCaptureModeFull,
})
defer rec.End()
```

Per-tool-execution override (here `Full` opts into capturing tool arguments and results in the span):

```go
ctx, tool := client.StartToolExecution(ctx, agento11y.ToolExecutionStart{
    ToolName:       "search",
    ContentCapture: agento11y.ContentCaptureModeFull,
})
defer tool.End()
```

Tool executions also inherit the parent generation's resolved mode via context, so explicit overrides are rarely needed inside an instrumented generation block.

Dynamic resolution via `ContentCaptureResolver`:

```go
cfg.ContentCaptureResolver = func(ctx context.Context, metadata map[string]any) agento11y.ContentCaptureMode {
    if metadata["tenant"] == "healthcare" {
        return agento11y.ContentCaptureModeMetadataOnly
    }
    return agento11y.ContentCaptureModeDefault // defer to Config.ContentCapture
}
```

Resolver panics are recovered and treated as `ContentCaptureModeMetadataOnly` (fail-closed).

Resolution precedence for generations (highest to lowest):

1. Per-generation `ContentCapture`
2. `ContentCaptureResolver` return value
3. `Config.ContentCapture` (defaults to `ContentCaptureModeNoToolContent`)

Resolution precedence for tool executions (highest to lowest):

1. Per-tool `ContentCapture`
2. Parent generation's resolved mode, propagated through `context.Context`
3. `ContentCaptureResolver` return value
4. `Config.ContentCapture` (defaults to `ContentCaptureModeNoToolContent`)

User-provided `Metadata` and `Tags` are not stripped by any capture mode. SDK-internal metadata keys that carry content (e.g. `call_error`, `agento11y.conversation.title`) are stripped along with the matching content. See [Tags and Metadata](../docs/concepts/tags-and-metadata.md) for where client tags, per-generation tags, metadata, and `user_id` each show up (export vs spans vs metrics).

## Pre-Ingest Redaction

Use `GenerationSanitizer` when you want to redact substrings from normalized generations before validation, span sync, and export.

```go
redactEmails := true
redactInputs := false
cfg := agento11y.DefaultConfig()
cfg.GenerationSanitizer = agento11y.NewSecretRedactionSanitizer(agento11y.SecretRedactionOptions{
    RedactInputMessages:  &redactInputs, // nil falls back to AGENTO11Y_REDACT_INPUT_MESSAGES, then false
    RedactEmailAddresses: &redactEmails, // nil defaults to true; point to false to preserve
})

client := agento11y.NewClient(cfg)
```

The built-in sanitizer:

- redacts high-confidence secret formats in assistant text and thinking
- redacts secret formats plus key/value secrets in system prompts, tool call inputs, and tool results
- redacts email addresses by default
- redacts `Generation.ConversationTitle` and `Generation.CallError`
- redacts historic assistant turns and tool messages in `Generation.Input`
- leaves user messages in `Generation.Input` unchanged unless input redaction is enabled

To preserve email addresses, opt out explicitly:

```go
preserveEmails := false
cfg.GenerationSanitizer = agento11y.NewSecretRedactionSanitizer(agento11y.SecretRedactionOptions{
    RedactEmailAddresses: &preserveEmails,
})
```

If a sanitizer panics during `Recorder.End`, the SDK falls back to `ContentCaptureModeMetadataOnly` for that generation and logs a warning via `Config.Logger`, so a partially redacted payload is never shipped.

### Configuring redaction via environment variables

`NewSecretRedactionSanitizer` reads `AGENTO11Y_REDACT_INPUT_MESSAGES` (accepts `1/0`, `true/false`, `yes/no`, `on/off`) when `RedactInputMessages` is left nil. Precedence is explicit option > env var > `false`. An unrecognised env value is logged via the standard logger and ignored, so a typo falls back to the next layer instead of silently flipping redaction.

```go
// Leave RedactInputMessages nil so AGENTO11Y_REDACT_INPUT_MESSAGES decides.
cfg.GenerationSanitizer = agento11y.NewSecretRedactionSanitizer(agento11y.SecretRedactionOptions{})
```

## Conversation Ratings

Use the SDK helper to submit user-facing ratings:

```go
rating, err := client.SubmitConversationRating(ctx, "conv-123", agento11y.ConversationRatingInput{
	RatingID: "rat-123",
	Rating:   agento11y.ConversationRatingValueBad,
	Comment:  "Answer ignored user context",
	Metadata: map[string]any{
		"channel": "assistant-ui",
	},
	Source: "sdk-go",
})
if err != nil {
	panic(err)
}

fmt.Printf("rating=%s has_bad=%v\n", rating.Rating.Rating, rating.Summary.HasBadRating)
```

`SubmitConversationRating` sends requests to `cfg.API.Endpoint`, which should be the Grafana Cloud Agent Observability API URL from Agent Observability configuration, and uses the same generation-export auth headers that your client config already resolves.

## Lifecycle requirement

- Always call `client.Shutdown(ctx)` before process exit.
- `Shutdown` flushes pending generation batches and closes generation exporters.
  Under the experimental otel protocol that queue is always empty, so `Shutdown`
  also force-flushes `Config.TracerProvider`; shutting the provider itself down
  stays yours.
- Optional `client.Flush(ctx)` is available for explicit flush points.

## SDK metrics

The SDK emits four OTel histograms automatically through your configured OTel meter provider:

- `gen_ai.client.operation.duration`
- `gen_ai.client.token.usage`
- `gen_ai.client.time_to_first_token`
- `gen_ai.client.tool_calls_per_operation`

## Streaming example

```go
ctx, rec := client.StartStreamingGeneration(ctx, agento11y.GenerationStart{
	ConversationID: "conv-stream",
	AgentName:      "assistant-core",
	AgentVersion:   "1.0.0",
	Model:          agento11y.ModelRef{Provider: "openai", Name: "gpt-5"},
})
defer rec.End()

// accumulate stream output...
rec.SetResult(agento11y.Generation{
	Input:  []agento11y.Message{agento11y.UserTextMessage("Say hello")},
	Output: []agento11y.Message{agento11y.AssistantTextMessage(stitchedOutput)},
}, nil)
```

## Embedding observability

Use `StartEmbedding` for embedding API calls. Embedding recording emits OTel spans and SDK metrics only, and does not enqueue generation export payloads.

```go
ctx, rec := client.StartEmbedding(ctx, agento11y.EmbeddingStart{
	AgentName:    "retrieval-worker",
	AgentVersion: "1.0.0",
	Model:        agento11y.ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
})
defer rec.End()

resp, err := provider.Embeddings.New(ctx, req)
if err != nil {
	rec.SetCallError(err)
	return err
}

rec.SetResult(agento11y.EmbeddingResult{
	InputCount:    len(req.Input),
	InputTokens:   resp.Usage.PromptTokens,
	InputTexts:    req.Input, // captured only when EmbeddingCapture.CaptureInput=true
	ResponseModel: resp.Model,
})
if err := rec.Err(); err != nil {
	return err
}
```

Input text capture is opt-in and should stay off in production unless you need short-term debugging:

```go
cfg.EmbeddingCapture = agento11y.EmbeddingCaptureConfig{
	CaptureInput:  true,
	MaxInputItems: 20,
	MaxTextLength: 1024,
}
```

`CaptureInput` can expose PII/document content in spans. Keep it disabled by default and enable only for scoped diagnostics.

TraceQL examples:

- `traces{gen_ai.operation.name="embeddings"}`
- `traces{gen_ai.operation.name="embeddings" && gen_ai.request.model="text-embedding-3-small"}`
- `traces{gen_ai.operation.name="embeddings" && error.type!=""}`

## Provider wrappers

Provider modules are documented wrapper-first for ergonomics and include explicit-flow alternatives.

Current Go provider helpers:

- `go-providers/openai` (OpenAI Chat Completions + Responses wrappers and mappers)
- `go-providers/anthropic` (Anthropic Messages wrappers and mappers; embeddings currently unsupported by the upstream SDK/API surface)
- `go-providers/gemini`

## Raw artifact policy

- Default: raw artifacts OFF in provider wrappers.
- Opt-in only for debug workflows (`WithRawArtifacts()` in provider helper packages).
- Normalized generation fields remain always on.

## Conformance harness

The Go SDK ships a local no-Docker conformance harness for the current cross-SDK baseline.

- Default local command: `mise run sdk:conformance`
- Direct Go command: `cd go && GOWORK=off go test ./agento11y -run '^TestConformance' -count=1`
- Current baseline coverage: sync roundtrip, conversation title resolution, user ID resolution, agent name/version resolution, streaming mode + TTFT, tool execution, embeddings, validation/error handling, rating submission, and shutdown flush semantics across exported generation payloads, OTLP spans, OTLP metrics, and local rating HTTP capture
