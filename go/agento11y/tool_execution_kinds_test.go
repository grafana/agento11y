package agento11y

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type toolExecutionKindSampler struct {
	parameters []sdktrace.SamplingParameters
}

func (s *toolExecutionKindSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	p.Attributes = append([]attribute.KeyValue(nil), p.Attributes...)
	s.parameters = append(s.parameters, p)
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}

func (*toolExecutionKindSampler) Description() string { return "tool execution kind sampler" }

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
			attrs := toolPrivateSpanAttributes(ToolExecutionStart{
				ExecutionKind: ToolExecutionKindLoadSkill, SkillSourceURI: tc.raw,
			}, ContentCaptureModeFull)
			set := attribute.NewSet(attrs...)
			value, present := set.Value("gen_ai.skill.source.uri")
			if present != (tc.want != "") || value.AsString() != tc.want {
				t.Errorf("source URI = %q (present %v), want %q", value.AsString(), present, tc.want)
			}
		})
	}
}

func TestConformanceToolExecutionKinds(t *testing.T) {
	data, err := os.ReadFile("../../conformance/tool-execution-kinds/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Keys    []string `json:"skill_process_attributes"`
		Private []string `json:"private_attributes"`
		Cases   []struct {
			ID    string            `json:"id"`
			Start map[string]string `json:"start"`
			End   struct {
				Code *int64 `json:"process_exit_code"`
			} `json:"end"`
			CaptureMode    string         `json:"capture_mode"`
			IncludeContent bool           `json:"include_content"`
			Error          string         `json:"error"`
			Name           string         `json:"span_name"`
			Attributes     map[string]any `json:"attributes"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixtures.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			sampler := &toolExecutionKindSampler{}
			recorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler), sdktrace.WithSpanProcessor(recorder))
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			client := NewClient(Config{TracerProvider: tp, MeterProvider: mp, testGenerationExporter: &capturingGenerationExporter{}})
			t.Cleanup(func() {
				_ = client.Shutdown(context.Background())
				_ = tp.Shutdown(context.Background())
				_ = mp.Shutdown(context.Background())
			})
			mode := ContentCaptureModeNoToolContent
			switch tc.CaptureMode {
			case "full":
				mode = ContentCaptureModeFull
			case "metadata_only":
				mode = ContentCaptureModeMetadataOnly
			case "full_with_metadata_spans":
				mode = ContentCaptureModeFullWithMetadataSpans
			}
			s := tc.Start
			seed := ToolExecutionStart{
				ToolName: s["tool_name"], ToolType: "function", ToolCallID: "call-1",
				ExecutionKind: ToolExecutionKind(s["execution_kind"]), SkillName: s["skill_name"],
				SkillDescription: s["skill_description"], SkillSourceURI: s["skill_source_uri"],
				SkillResourceName: s["skill_resource_name"], ProcessExecutableName: s["process_executable_name"],
				ProcessExecutablePath: s["process_executable_path"], ContentCapture: mode, IncludeContent: tc.IncludeContent,
				StartedAt: time.Unix(1, 0),
			}
			_, rec := client.StartToolExecution(context.Background(), seed)
			seed.SkillName = "caller mutation"
			end := ToolExecutionEnd{ProcessExitCode: tc.End.Code, Arguments: "argument", Result: "result", CompletedAt: time.Unix(2, 0)}
			rec.SetResult(end)
			if tc.End.Code != nil {
				*tc.End.Code = 99
			}
			if tc.Error != "" {
				rec.SetExecError(errors.New(tc.Error))
			}
			rec.End()
			rec.End()
			spans := recorder.Ended()
			if len(spans) != 1 {
				t.Fatalf("span count = %d", len(spans))
			}
			span := spans[0]
			if span.Name() != tc.Name || span.SpanKind() != trace.SpanKindInternal {
				t.Fatalf("span = %q %v", span.Name(), span.SpanKind())
			}
			if got := span.Status().Code == codes.Error; got != (tc.Error != "") {
				t.Errorf("error status = %v, execution error = %q", got, tc.Error)
			}
			attrs := map[string]any{}
			for _, kv := range span.Attributes() {
				attrs[string(kv.Key)] = kv.Value.AsInterface()
			}
			if _, ok := attrs["agento11y.skill.name"]; ok {
				t.Fatal("vendor skill key emitted")
			}
			for _, key := range fixtures.Keys {
				got, present := attrs[key]
				want, expected := tc.Attributes[key]
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if present != expected || (present && string(gotJSON) != string(wantJSON)) {
					t.Errorf("%s = %s (present %v), want %s (present %v)", key, gotJSON, present, wantJSON, expected)
				}
			}
			if len(sampler.parameters) != 1 {
				t.Fatalf("sampling calls = %d", len(sampler.parameters))
			}
			p := sampler.parameters[0]
			if p.Name != tc.Name {
				t.Errorf("sampling name = %q, want %q", p.Name, tc.Name)
			}
			sampling := attribute.NewSet(p.Attributes...)
			for _, key := range append(append([]string{}, fixtures.Private...), "process.exit.code") {
				if sampling.HasValue(attribute.Key(key)) {
					t.Errorf("sampler received private/completion field %s", key)
				}
			}
			for _, key := range []string{"gen_ai.operation.name", "gen_ai.tool.name", "gen_ai.tool.type", "gen_ai.skill.name", "gen_ai.skill.resource.name", "process.executable.name"} {
				got, present := sampling.Value(attribute.Key(key))
				want, expected := attrs[key]
				if present != expected || (present && got.AsInterface() != want) {
					t.Errorf("sampling %s = %v (present %v), want %v (present %v)", key, got, present, want, expected)
				}
			}
			wantContent := mode == ContentCaptureModeFull || (mode == ContentCaptureModeNoToolContent && tc.IncludeContent)
			for _, key := range []string{spanAttrToolCallArguments, spanAttrToolCallResult} {
				if _, got := attrs[key]; got != wantContent {
					t.Errorf("%s present = %v, want %v", key, got, wantContent)
				}
			}
			var metrics metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &metrics); err != nil {
				t.Fatal(err)
			}
			points := 0
			for _, scope := range metrics.ScopeMetrics {
				for _, metric := range scope.Metrics {
					if histogram, ok := metric.Data.(metricdata.Histogram[float64]); ok {
						for _, point := range histogram.DataPoints {
							points++
							for _, kv := range point.Attributes.ToSlice() {
								if strings.HasPrefix(string(kv.Key), "gen_ai.skill.") || strings.HasPrefix(string(kv.Key), "process.") {
									t.Errorf("metric contains %s", kv.Key)
								}
							}
						}
					}
				}
			}
			if points != 1 {
				t.Errorf("tool metric points = %d, want 1", points)
			}
		})
	}
}
