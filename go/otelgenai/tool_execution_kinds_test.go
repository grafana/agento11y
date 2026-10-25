package otelgenai_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log/logtest"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/agento11y/go/otelgenai"
)

type toolExecutionKindFixture struct {
	Version                int      `json:"version"`
	SkillProcessAttributes []string `json:"skill_process_attributes"`
	PrivateAttributes      []string `json:"private_attributes"`
	Cases                  []struct {
		ID          string `json:"id"`
		CaptureMode string `json:"capture_mode"`
		OTelCapture string `json:"otel_capture"`
		Start       struct {
			ToolName              string `json:"tool_name"`
			ExecutionKind         string `json:"execution_kind"`
			SkillName             string `json:"skill_name"`
			SkillDescription      string `json:"skill_description"`
			SkillSourceURI        string `json:"skill_source_uri"`
			SkillResourceName     string `json:"skill_resource_name"`
			ProcessExecutableName string `json:"process_executable_name"`
			ProcessExecutablePath string `json:"process_executable_path"`
		} `json:"start"`
		End struct {
			ProcessExitCode *int64 `json:"process_exit_code"`
		} `json:"end"`
		Error      string                     `json:"error"`
		SpanName   string                     `json:"span_name"`
		Attributes map[string]json.RawMessage `json:"attributes"`
	} `json:"cases"`
}

type toolExecutionKindSampler struct {
	calls []sdktrace.SamplingParameters
}

func (s *toolExecutionKindSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	p.Attributes = append([]attribute.KeyValue(nil), p.Attributes...)
	s.calls = append(s.calls, p)
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}

func (*toolExecutionKindSampler) Description() string { return "tool execution kind test sampler" }

func newToolExecutionKindHandler(t *testing.T, opts ...otelgenai.Option) (*otelgenai.Handler, *tracetest.SpanRecorder, *toolExecutionKindSampler) {
	t.Helper()
	sampler := &toolExecutionKindSampler{}
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler), sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	handler := otelgenai.NewHandler(append(opts, otelgenai.WithTracerProvider(provider))...)
	return handler, recorder, sampler
}

func toolExecutionKindAttrs(attrs []attribute.KeyValue) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func assertToolString(t *testing.T, attrs map[string]attribute.Value, key, want string) {
	t.Helper()
	if got, ok := attrs[key]; !ok || got.Type() != attribute.STRING || got.AsString() != want {
		t.Errorf("%s = %v (present %v), want %q", key, got, ok, want)
	}
}

func TestToolExecutionKindFixtures(t *testing.T) {
	data, err := os.ReadFile("../../conformance/tool-execution-kinds/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture toolExecutionKindFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 1 || len(fixture.Cases) == 0 || len(fixture.SkillProcessAttributes) == 0 || len(fixture.PrivateAttributes) == 0 {
		t.Fatal("invalid fixture header")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			capture := otelgenai.CaptureNoContent
			if tc.CaptureMode == "full" {
				capture = otelgenai.CaptureSpanOnly
			}
			if tc.OTelCapture != "" {
				capture = otelgenai.CaptureMode(tc.OTelCapture)
			}
			meter, collect := newMetricRecorder(t)
			logs := logtest.NewRecorder()
			handler, spans, sampler := newToolExecutionKindHandler(t, meter,
				otelgenai.WithCaptureMode(capture), otelgenai.WithLoggerProvider(logs), otelgenai.WithEmitEvent(true))
			inv := &otelgenai.Invocation{
				Operation: otelgenai.OperationExecuteTool, ToolName: tc.Start.ToolName,
				ExecutionKind: otelgenai.ToolExecutionKind(tc.Start.ExecutionKind),
				SkillName:     tc.Start.SkillName, SkillDescription: tc.Start.SkillDescription,
				SkillSourceURI: tc.Start.SkillSourceURI, SkillResourceName: tc.Start.SkillResourceName,
				ProcessExecutableName: tc.Start.ProcessExecutableName, ProcessExecutablePath: tc.Start.ProcessExecutablePath,
				StartedAt: startedAt, CompletedAt: completedAt,
				// Even a known completion code must never become a creation attribute.
				ProcessExitCode: tc.End.ProcessExitCode,
			}
			ctx := handler.Start(context.Background(), inv)
			inv.ErrorMessage = tc.Error
			handler.End(ctx, inv)
			handler.End(ctx, inv)
			ended := spans.Ended()
			if len(ended) != 1 || len(sampler.calls) != 1 {
				t.Fatalf("spans = %d, sampler calls = %d, want one each", len(ended), len(sampler.calls))
			}
			span, sampled := ended[0], sampler.calls[0]
			if span.Name() != tc.SpanName || sampled.Name != tc.SpanName {
				t.Errorf("finished name = %q, sampling name = %q, want %q", span.Name(), sampled.Name, tc.SpanName)
			}
			if span.SpanKind() != trace.SpanKindInternal || sampled.Kind != trace.SpanKindInternal {
				t.Error("tool span must be INTERNAL at creation and completion")
			}
			attrs, sampling := spanAttrs(span), toolExecutionKindAttrs(sampled.Attributes)
			assertToolString(t, attrs, "gen_ai.operation.name", "execute_tool")
			assertToolString(t, sampling, "gen_ai.operation.name", "execute_tool")
			toolName := tc.Start.ToolName
			if tc.Start.ExecutionKind == "load_skill" || tc.Start.ExecutionKind == "read_skill_resource" || tc.Start.ExecutionKind == "command" {
				toolName = strings.TrimSpace(toolName)
			}
			assertToolString(t, attrs, "gen_ai.tool.name", toolName)
			assertToolString(t, sampling, "gen_ai.tool.name", toolName)
			for _, key := range fixture.SkillProcessAttributes {
				raw, wanted := tc.Attributes[key]
				value, present := attrs[key]
				if present != wanted {
					t.Errorf("%s present = %v, want %v", key, present, wanted)
				}
				if wanted {
					if key == "process.exit.code" {
						var code int64
						if err := json.Unmarshal(raw, &code); err != nil {
							t.Fatal(err)
						}
						if value.Type() != attribute.INT64 || value.AsInt64() != code {
							t.Errorf("exit code = %v, want int64 %d", value, code)
						}
					} else {
						var text string
						if err := json.Unmarshal(raw, &text); err != nil {
							t.Fatal(err)
						}
						assertToolString(t, attrs, key, text)
					}
				}
				structural := key == "gen_ai.skill.name" || key == "gen_ai.skill.resource.name" || key == "process.executable.name"
				if got, ok := sampling[key]; ok != (wanted && structural) || (ok && got != value) {
					t.Errorf("sampler %s = %v (present %v), want published structural identity only", key, got, ok)
				}
			}
			for _, key := range fixture.PrivateAttributes {
				if _, ok := sampling[key]; ok {
					t.Errorf("private %s reached sampler", key)
				}
			}
			if _, ok := attrs["agento11y.skill.name"]; ok {
				t.Error("legacy skill attribute emitted")
			}
			if (span.Status().Code == codes.Error) != (tc.Error != "") {
				t.Errorf("status = %v, error = %q", span.Status(), tc.Error)
			}
			if events, _ := recordedEvents(t, logs); len(events) != 0 || len(span.Events()) != 0 {
				t.Error("tool execution emitted events")
			}
			metrics := collect()
			if len(metrics) == 0 {
				t.Fatal("no tool duration metric")
			}
			for _, m := range metrics {
				for _, point := range metricPointAttributes(t, m) {
					for key := range point {
						if strings.HasPrefix(key, "gen_ai.skill.") || strings.HasPrefix(key, "process.") || key == "agento11y.skill.name" {
							t.Errorf("metric %s has skill/process dimension %s", m.Name, key)
						}
					}
				}
			}
		})
	}
}

func commandInvocation() *otelgenai.Invocation {
	return &otelgenai.Invocation{
		Operation: otelgenai.OperationExecuteTool, ExecutionKind: otelgenai.ToolExecutionKindCommand,
		ToolName: "exec", SkillName: "review", SkillResourceName: "check.py", ProcessExecutableName: "python3",
		SkillDescription: "private", SkillSourceURI: "file:///skills/review", ProcessExecutablePath: "/bin/python3",
		StartedAt: startedAt, CompletedAt: completedAt,
	}
}

func TestToolExecutionKindHookIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*otelgenai.Invocation)
	}{
		{"Operation", func(inv *otelgenai.Invocation) { inv.Operation = otelgenai.OperationChat }},
		{"ExecutionKind", func(inv *otelgenai.Invocation) { inv.ExecutionKind = otelgenai.ToolExecutionKindLoadSkill }},
		{"ToolName", func(inv *otelgenai.Invocation) { inv.ToolName = "other" }},
		{"SkillName", func(inv *otelgenai.Invocation) { inv.SkillName = "other" }},
		{"SkillResourceName", func(inv *otelgenai.Invocation) { inv.SkillResourceName = "other" }},
		{"ProcessExecutableName", func(inv *otelgenai.Invocation) { inv.ProcessExecutableName = "other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testOTelErrors.reset()
			handler, spans, sampler := newToolExecutionKindHandler(t,
				otelgenai.WithCaptureMode(otelgenai.CaptureNoContent),
				otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					tc.mutate(inv)
					return nil
				})),
				otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					want := commandInvocation()
					if inv.Operation != want.Operation || inv.ExecutionKind != want.ExecutionKind || inv.ToolName != want.ToolName || inv.SkillName != want.SkillName || inv.SkillResourceName != want.SkillResourceName || inv.ProcessExecutableName != want.ProcessExecutableName {
						t.Error("later hook received mutated identity")
					}
					return nil
				})))
			inv := commandInvocation()
			ctx := handler.Start(context.Background(), inv)
			handler.End(ctx, inv)
			if got := testOTelErrors.read(); len(got) != 1 || !strings.Contains(got[0].Error(), tc.name) {
				t.Errorf("reported errors = %v, want one %s mutation", got, tc.name)
			}
			if len(spans.Ended()) != 1 || spans.Ended()[0].Name() != sampler.calls[0].Name {
				t.Fatal("hook changed published span identity")
			}
			for key, want := range toolExecutionKindAttrs(sampler.calls[0].Attributes) {
				if got := spanAttrs(spans.Ended()[0])[key]; got != want {
					t.Errorf("published %s = %v, want %v", key, got, want)
				}
			}
		})
	}
}

func TestToolExecutionKindHookReplacementAndPanic(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(map[bool]string{false: "replace", true: "replace_and_panic"}[panics], func(t *testing.T) {
			testOTelErrors.reset()
			logs := logtest.NewRecorder()
			meter, collect := newMetricRecorder(t)
			handler, spans, sampler := newToolExecutionKindHandler(t, meter,
				otelgenai.WithCaptureMode(otelgenai.CaptureSpanAndEvent), otelgenai.WithLoggerProvider(logs), otelgenai.WithEmitEvent(true),
				otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					*inv = otelgenai.Invocation{SkillDescription: "new private", SkillSourceURI: "s3://skills/review", ProcessExecutablePath: "/new/path", ProcessExitCode: new(int64(7)), Attributes: []attribute.KeyValue{attribute.String("unsafe", "raw")}}
					if panics {
						panic("redactor failed")
					}
					return []attribute.KeyValue{attribute.String("hook.extra", "present")}
				})),
				otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, capture otelgenai.CaptureMode) []attribute.KeyValue {
					if inv.Operation != otelgenai.OperationExecuteTool || inv.ExecutionKind != otelgenai.ToolExecutionKindCommand || inv.ToolName != "exec" || inv.SkillName != "review" || inv.SkillResourceName != "check.py" || inv.ProcessExecutableName != "python3" || !inv.Span().IsRecording() {
						t.Error("replacement lost lifecycle or identity before later hook")
					}
					if panics && capture != otelgenai.CaptureNoContent {
						t.Error("later hook retained content capture after panic")
					}
					inv.ExecutionKind = "future"
					return nil
				})))
			inv := commandInvocation()
			ctx := handler.Start(context.Background(), inv)
			handler.End(ctx, inv)
			handler.End(ctx, inv)
			if len(spans.Ended()) != 1 || len(sampler.calls) != 1 {
				t.Fatal("replacement lost single-span lifecycle")
			}
			span := spans.Ended()[0]
			if span.Name() != sampler.calls[0].Name || !span.StartTime().Equal(startedAt) || !span.EndTime().Equal(completedAt) {
				t.Error("replacement changed span identity or timing")
			}
			wantErrors := 7
			if panics {
				wantErrors++
			}
			if got := testOTelErrors.read(); len(got) != wantErrors {
				t.Errorf("errors = %v, want %d", got, wantErrors)
			}
			attrs := spanAttrs(span)
			if code := attrs["process.exit.code"]; code.Type() != attribute.INT64 || code.AsInt64() != 7 {
				t.Error("completion code lost during replacement or panic")
			}
			for _, key := range []string{"gen_ai.skill.description", "gen_ai.skill.source.uri", "process.executable.path", "unsafe", "hook.extra"} {
				if _, ok := attrs[key]; ok == panics {
					t.Errorf("%s present = %v, panicked = %v", key, ok, panics)
				}
			}
			if events, _ := recordedEvents(t, logs); len(events) != 0 {
				t.Error("replacement turned command execution into inference event")
			}
			for _, m := range collect() {
				if count := histogramCount(t, m); count != 1 {
					t.Errorf("metric count = %d, want 1", count)
				}
			}
		})
	}
}

func TestToolExecutionKindLateIdentity(t *testing.T) {
	for _, inHook := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_end", true: "in_hook"}[inHook], func(t *testing.T) {
			testOTelErrors.reset()
			complete := func(inv *otelgenai.Invocation) {
				inv.ToolName = " exec "
				inv.SkillName = " review "
				inv.SkillResourceName = " check.py "
				inv.ProcessExecutableName = " python3 "
				inv.SkillDescription = " redacted instructions "
				inv.SkillSourceURI = " https://u:p@example.com/skill?q=secret#private "
				inv.ProcessExecutablePath = " /bin/python3 "
				inv.ProcessExitCode = new(int64(0))
			}
			var opts []otelgenai.Option
			opts = append(opts, otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly))
			if inHook {
				opts = append(opts, otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					complete(inv)
					return nil
				})))
			}
			handler, spans, sampler := newToolExecutionKindHandler(t, opts...)
			inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ExecutionKind: otelgenai.ToolExecutionKindCommand, ToolName: " \t ", SkillName: " ", SkillResourceName: " ", ProcessExecutableName: " "}
			ctx := handler.Start(context.Background(), inv)
			if !inHook {
				complete(inv)
			}
			handler.End(ctx, inv)
			if got := testOTelErrors.read(); len(got) != 0 {
				t.Errorf("late fields caused errors: %v", got)
			}
			if sampler.calls[0].Name != "execute_tool" || len(sampler.calls[0].Attributes) != 1 {
				t.Errorf("unknown identity published at creation: %+v", sampler.calls[0])
			}
			span := spans.Ended()[0]
			if span.Name() != "execute_tool exec review check.py" {
				t.Errorf("late name = %q", span.Name())
			}
			attrs := spanAttrs(span)
			for key, want := range map[string]string{"gen_ai.tool.name": "exec", "gen_ai.skill.name": "review", "gen_ai.skill.resource.name": "check.py", "process.executable.name": "python3", "gen_ai.skill.description": "redacted instructions", "gen_ai.skill.source.uri": "https://example.com/skill", "process.executable.path": "/bin/python3"} {
				assertToolString(t, attrs, key, want)
			}
			if code := attrs["process.exit.code"]; code.Type() != attribute.INT64 || code.AsInt64() != 0 {
				t.Error("known zero completion code omitted")
			}
		})
	}
}

func TestToolExecutionKindRawOverrides(t *testing.T) {
	testOTelErrors.reset()
	handler, spans, sampler := newToolExecutionKindHandler(t,
		otelgenai.WithCaptureMode(otelgenai.CaptureNoContent),
		otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, _ *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
			return []attribute.KeyValue{attribute.String("gen_ai.skill.name", "hook skill"), attribute.String("process.executable.name", "hook executable")}
		})))
	inv := commandInvocation()
	inv.Attributes = []attribute.KeyValue{
		attribute.String("gen_ai.tool.name", "raw tool"), attribute.String("gen_ai.skill.name", "raw skill"), attribute.String("gen_ai.skill.resource.name", "raw resource"),
	}
	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)
	attrs, sampling := spanAttrs(spans.Ended()[0]), toolExecutionKindAttrs(sampler.calls[0].Attributes)
	for key, want := range map[string]string{"gen_ai.tool.name": "raw tool", "gen_ai.skill.name": "hook skill", "gen_ai.skill.resource.name": "raw resource", "process.executable.name": "hook executable"} {
		assertToolString(t, attrs, key, want)
	}
	for key, want := range map[string]string{"gen_ai.tool.name": "exec", "gen_ai.skill.name": "review", "gen_ai.skill.resource.name": "check.py", "process.executable.name": "python3"} {
		assertToolString(t, sampling, key, want)
	}
	if spans.Ended()[0].Name() != sampler.calls[0].Name || inv.SkillName != "review" || len(testOTelErrors.read()) != 0 {
		t.Error("raw overrides changed typed identity or reported typed mutation")
	}
}

func TestToolExecutionKindGenericHooksUnchanged(t *testing.T) {
	for _, executionKind := range []otelgenai.ToolExecutionKind{"", "future"} {
		t.Run(string(executionKind), func(t *testing.T) {
			testOTelErrors.reset()
			logs := logtest.NewRecorder()
			handler, spans, sampler := newToolExecutionKindHandler(t,
				otelgenai.WithLoggerProvider(logs), otelgenai.WithEmitEvent(true),
				otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					*inv = otelgenai.Invocation{RequestModel: "new model"}
					return nil
				})))
			inv := commandInvocation()
			inv.ExecutionKind = executionKind
			inv.ToolName = " lookup "
			ctx := handler.Start(context.Background(), inv)
			handler.End(ctx, inv)
			if sampler.calls[0].Name != "execute_tool  lookup " || spans.Ended()[0].Name() != "chat new model" {
				t.Error("generic naming or hook behavior changed")
			}
			if got := testOTelErrors.read(); len(got) != 0 {
				t.Errorf("generic hook caused identity errors: %v", got)
			}
			if events, _ := recordedEvents(t, logs); len(events) != 1 {
				t.Error("generic hook no longer changes event eligibility")
			}
		})
	}
}

func TestToolExecutionKindNonToolApplicability(t *testing.T) {
	handler, spans, sampler := newToolExecutionKindHandler(t, otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly))
	inv := commandInvocation()
	inv.Operation = otelgenai.OperationChat
	inv.ToolName = " tool "
	inv.RequestModel = "model"
	inv.ProcessExitCode = new(int64(1))
	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)
	attrs := spanAttrs(spans.Ended()[0])
	if spans.Ended()[0].Name() != "chat model" {
		t.Error("execution kind affected non-tool span name")
	}
	assertToolString(t, attrs, "gen_ai.tool.name", " tool ")
	for _, values := range []map[string]attribute.Value{attrs, toolExecutionKindAttrs(sampler.calls[0].Attributes)} {
		for key := range values {
			if strings.HasPrefix(key, "gen_ai.skill.") || strings.HasPrefix(key, "process.") {
				t.Errorf("non-tool invocation emitted %s", key)
			}
		}
	}
}

func TestToolExecutionKindPrivateCaptureModes(t *testing.T) {
	for _, mode := range []otelgenai.CaptureMode{otelgenai.CaptureNoContent, otelgenai.CaptureEventOnly, otelgenai.CaptureSpanOnly, otelgenai.CaptureSpanAndEvent} {
		t.Run(string(mode), func(t *testing.T) {
			handler, spans, sampler := newToolExecutionKindHandler(t, otelgenai.WithCaptureMode(mode))
			inv := commandInvocation()
			ctx := handler.Start(context.Background(), inv)
			handler.End(ctx, inv)
			for _, key := range []string{"gen_ai.skill.description", "gen_ai.skill.source.uri", "process.executable.path"} {
				if _, ok := spanAttrs(spans.Ended()[0])[key]; ok != mode.SpanContent() {
					t.Errorf("%s present = %v for %s", key, ok, mode)
				}
				if _, ok := toolExecutionKindAttrs(sampler.calls[0].Attributes)[key]; ok {
					t.Errorf("private %s reached sampler", key)
				}
			}
		})
	}
}

func TestToolExecutionKindStartSnapshot(t *testing.T) {
	testOTelErrors.reset()
	handler, spans, sampler := newToolExecutionKindHandler(t)
	inv := commandInvocation()
	inv.ToolName, inv.SkillName = " exec ", " review "
	ctx := handler.Start(context.Background(), inv)
	inv.ExecutionKind = otelgenai.ToolExecutionKindLoadSkill
	handler.End(ctx, inv)
	if got := testOTelErrors.read(); len(got) != 1 || !strings.Contains(got[0].Error(), "ExecutionKind") {
		t.Errorf("errors = %v, want one change since Start", got)
	}
	if spans.Ended()[0].Name() != sampler.calls[0].Name || inv.ExecutionKind != otelgenai.ToolExecutionKindCommand {
		t.Error("identity was snapshotted at End rather than Start")
	}
}

func TestToolExecutionKindSourceURI(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{" file:///opt/skills/review?token=secret#fragment ", "file:///opt/skills/review"},
		{"https://user:secret@example.com:443/a%20b?token=secret#fragment", "https://example.com:443/a%20b"}, // trufflehog:ignore
		{"s3://user:secret@bucket/path?version=secret#fragment", "s3://bucket/path"},
		{"https://example.com/path?", "https://example.com/path"},
		{"https://example.com", "https://example.com"},
		{"https://[::1]:443/a%2Fb?q=%20#frag", "https://[::1]:443/a%2Fb"},
		{"gs://bucket/a%20b?version=secret#fragment", "gs://bucket/a%20b"},
		{"file:/opt/skills/review/SKILL.md", "file:///opt/skills/review/SKILL.md"},
		{"file:///", "file:///"},
		{"file://host/path", "file://host/path"},
		{"file:/opt/a%20b?token=secret#fragment", "file:///opt/a%20b"},
		{"", ""}, {"  ", ""}, {"\u2003", ""}, {"../skills/review", ""}, {"/skills/review", ""},
		{"//example.com/skill", ""}, {"https://[broken", ""}, {"https://example.com/%zz", ""},
		{"https:///path", ""}, {"https:/path", ""}, {"file:relative", ""}, {"file:", ""},
		{"file://host", ""}, {"file:?query", ""}, {"s3:///path", ""},
		{"https://example.com/path?token=%zz", ""}, {"https://example.com/path#%zz", ""},
		{"https://example.com/path?token=%", ""}, {"https://example.com/path#%0", ""},
		{"https://user%zz@example.com/path", ""},
		{"https://example.com/a b", ""}, {"https://example.com/path?token=a b", ""},
		{"https://example.com/path#a\u2003b", ""}, {"https://example.com/a\tb", ""},
		{"https://example.com/path?token=a\u0085b", ""}, {"https://example.com/path#a\u007fb", ""},
		{"https://example.com:invalid/path", ""}, {"https://example.com:65536/path", ""},
		{"https://example.com:/path", ""}, {"https://:443/path", ""},
		{"https://[not-an-ip]/path", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			handler, spans, sampler := newToolExecutionKindHandler(t, otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly))
			inv := commandInvocation()
			inv.SkillSourceURI = tc.raw
			ctx := handler.Start(context.Background(), inv)
			handler.End(ctx, inv)
			attrs := spanAttrs(spans.Ended()[0])
			if tc.want == "" {
				if _, ok := attrs["gen_ai.skill.source.uri"]; ok {
					t.Error("invalid source URI emitted")
				}
			} else {
				assertToolString(t, attrs, "gen_ai.skill.source.uri", tc.want)
			}
			if _, ok := toolExecutionKindAttrs(sampler.calls[0].Attributes)["gen_ai.skill.source.uri"]; ok {
				t.Error("source URI reached sampler")
			}
		})
	}
}

func TestToolExecutionKindRepeatedMutationReportsPerAttempt(t *testing.T) {
	testOTelErrors.reset()
	hook := otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
		inv.ExecutionKind = "future"
		return nil
	}))
	handler, _, _ := newToolExecutionKindHandler(t, hook, hook)
	inv := commandInvocation()
	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)
	errs := testOTelErrors.read()
	if len(errs) != 2 || errs[0].Error() != errs[1].Error() {
		t.Errorf("errors = %v, want one report per attempted execution-kind change", errs)
	}
}
