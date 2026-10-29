package agento11y

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
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
			CaptureMode            string            `json:"capture_mode"`
			IncludeContent         bool              `json:"include_content"`
			Error                  string            `json:"error"`
			Name                   string            `json:"span_name"`
			Attributes             map[string]any    `json:"attributes"`
			ClientMetricAttributes map[string]string `json:"client_metric_attributes"`
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
			seed.ExecutionKind = ToolExecutionKindCommand
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
			if tc.ClientMetricAttributes == nil {
				t.Fatal("fixture missing client_metric_attributes")
			}
			want := toolMetricBaseline(strings.TrimSpace(s["tool_name"]))
			if tc.Error != "" {
				want[spanAttrErrorType] = "tool_execution_error"
				want[spanAttrErrorCategory] = "sdk_error"
			}
			maps.Copy(want, tc.ClientMetricAttributes)
			duration := collectToolDuration(t, reader)
			if len(duration.DataPoints) != 1 {
				t.Fatalf("tool metric points = %d, want 1", len(duration.DataPoints))
			}
			point := duration.DataPoints[0]
			assertToolMetricAttributes(t, point.Attributes, want)
			assertToolDuration(t, point, 1, 1)
		})
	}
}

func toolMetricBaseline(tool string) map[string]string {
	return map[string]string{
		"gen_ai.operation.name": "execute_tool",
		"gen_ai.tool.name":      tool,
		"gen_ai.provider.name":  "",
		"gen_ai.request.model":  "",
		"gen_ai.agent.name":     "",
		"error.type":            "",
		"error.category":        "",
	}
}

func assertToolMetricAttributes(t *testing.T, attrs attribute.Set, want map[string]string) {
	t.Helper()
	got := make(map[string]string, attrs.Len())
	for _, kv := range attrs.ToSlice() {
		if kv.Value.Type() != attribute.STRING {
			t.Errorf("metric attribute %s has type %v, want string", kv.Key, kv.Value.Type())
		}
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if !maps.Equal(got, want) {
		t.Errorf("metric attributes = %v, want %v", got, want)
	}
}

func collectToolDuration(t *testing.T, reader *sdkmetric.ManualReader) metricdata.Histogram[float64] {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	var metrics []metricdata.Metrics
	for _, scope := range collected.ScopeMetrics {
		metrics = append(metrics, scope.Metrics...)
	}
	if len(metrics) != 1 || metrics[0].Name != metricOperationDuration || metrics[0].Unit != "s" {
		t.Fatalf("tool instrument inventory = %v, want only %s (s)", metrics, metricOperationDuration)
	}
	histogram, ok := metrics[0].Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("duration data type = %T", metrics[0].Data)
	}
	return histogram
}

func assertToolDuration(t *testing.T, point metricdata.HistogramDataPoint[float64], count uint64, seconds float64) {
	t.Helper()
	if point.Count != count || point.Sum != float64(count)*seconds {
		t.Errorf("duration count/sum = %d/%v, want %d/%v", point.Count, point.Sum, count, float64(count)*seconds)
	}
	bounds := []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	if !slices.Equal(point.Bounds, bounds) {
		t.Errorf("duration bounds = %v, want %v", point.Bounds, bounds)
	}
	buckets := make([]uint64, len(bounds)+1)
	bucket := slices.IndexFunc(bounds, func(bound float64) bool { return seconds <= bound })
	if bucket < 0 {
		bucket = len(bounds)
	}
	buckets[bucket] = count
	if !slices.Equal(point.BucketCounts, buckets) {
		t.Errorf("duration buckets = %v, want %v", point.BucketCounts, buckets)
	}
}

func TestToolExecutionMetricsWithoutRecordingSpans(t *testing.T) {
	for _, tracing := range []string{"noop", "unsampled"} {
		for _, mode := range []ContentCaptureMode{
			ContentCaptureModeDefault, ContentCaptureModeFull, ContentCaptureModeNoToolContent,
			ContentCaptureModeMetadataOnly, ContentCaptureModeFullWithMetadataSpans,
		} {
			for _, kind := range []ToolExecutionKind{ToolExecutionKindLoadSkill, ToolExecutionKindReadSkillResource, ToolExecutionKindCommand} {
				t.Run(tracing+"/"+mode.String()+"/"+string(kind), func(t *testing.T) {
					var tp trace.TracerProvider = noop.NewTracerProvider()
					if tracing == "unsampled" {
						provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
						tp = provider
						t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
					}
					reader := sdkmetric.NewManualReader()
					mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
					client := NewClient(Config{
						TracerProvider: tp, MeterProvider: mp, Tags: map[string]string{"team": "sdk"},
						testGenerationExporter: &capturingGenerationExporter{},
					})
					t.Cleanup(func() {
						_ = client.Shutdown(context.Background())
						_ = mp.Shutdown(context.Background())
					})
					start := ToolExecutionStart{
						ToolName: " host-tool ", ExecutionKind: kind, SkillName: " code-review ",
						RequestProvider: " provider ", RequestModel: " model ", AgentName: " agent ", AgentVersion: " v1 ",
						ContentCapture: mode, StartedAt: time.Unix(1, 0),
						SkillDescription: "private description", SkillSourceURI: "file:///private/skill",
						SkillResourceName: "private-resource", ProcessExecutableName: "private-executable",
						ProcessExecutablePath: "/private/executable", ToolDescription: "private tool",
						ConversationID: "private-conversation", ToolCallID: "private-call",
					}
					ctx, rec := client.StartToolExecution(context.Background(), start)
					if trace.SpanFromContext(ctx).IsRecording() {
						t.Fatal("expected a non-recording span")
					}
					start.SkillName = "mutated-skill"
					start.ExecutionKind = "mutated-kind"
					code := int64(37)
					rec.SetResult(ToolExecutionEnd{
						Arguments: "private-arguments", Result: "private-result", ProcessExitCode: &code,
						CompletedAt: time.Unix(2, 0),
					})
					rec.End()
					rec.End()
					if err := rec.Err(); err != nil {
						t.Fatalf("nonzero exit without execution error: %v", err)
					}
					want := toolMetricBaseline("host-tool")
					maps.Copy(want, map[string]string{
						"gen_ai.provider.name": "provider", "gen_ai.request.model": "model", "gen_ai.agent.name": "agent",
						"gen_ai.agent.version": "v1", "agento11y.tag.team": "sdk",
						"gen_ai.skill.name": "code-review", "agento11y.tool.execution.kind": string(kind),
					})
					histogram := collectToolDuration(t, reader)
					if len(histogram.DataPoints) != 1 {
						t.Fatalf("duration points = %d, want 1", len(histogram.DataPoints))
					}
					assertToolMetricAttributes(t, histogram.DataPoints[0].Attributes, want)
					assertToolDuration(t, histogram.DataPoints[0], 1, 1)
				})
			}
		}
	}
}

func TestToolExecutionSkillMetricsPreserveErrors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		execErr      error
		arguments    any
		result       any
		wantError    string
		wantCategory string
	}{
		{name: "success"},
		{name: "explicit", execErr: context.DeadlineExceeded, wantError: "context deadline exceeded", wantCategory: "timeout"},
		{name: "arguments", arguments: func() {}, wantError: "serialize tool arguments: json: unsupported type: func()", wantCategory: "sdk_error"},
		{name: "result", result: func() {}, wantError: "serialize tool result: json: unsupported type: func()", wantCategory: "sdk_error"},
		{name: "combined", execErr: context.DeadlineExceeded, result: func() {}, wantError: "context deadline exceeded\nserialize tool result: json: unsupported type: func()", wantCategory: "timeout"},
	} {
		for _, named := range []bool{false, true} {
			name := tc.name + "/generic"
			if named {
				name = tc.name + "/skill"
			}
			t.Run(name, func(t *testing.T) {
				spans, tp, reader, mp := newExemplarTestHarness(t, exemplar.AlwaysOffFilter)
				client := NewClient(Config{TracerProvider: tp, MeterProvider: mp, testGenerationExporter: &capturingGenerationExporter{}})
				t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
				start := ToolExecutionStart{ToolName: "load", ContentCapture: ContentCaptureModeFull, StartedAt: time.Unix(1, 0)}
				want := toolMetricBaseline("load")
				if named {
					start.ExecutionKind = ToolExecutionKindLoadSkill
					start.SkillName = " review "
					want["gen_ai.skill.name"] = "review"
					want["agento11y.tool.execution.kind"] = "load_skill"
				}
				_, rec := client.StartToolExecution(context.Background(), start)
				rec.SetResult(ToolExecutionEnd{Arguments: tc.arguments, Result: tc.result, CompletedAt: time.Unix(2, 0)})
				rec.SetExecError(tc.execErr)
				rec.End()
				rec.End()
				err := rec.Err()
				if tc.wantError == "" {
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
				} else {
					if err == nil || err.Error() != tc.wantError {
						t.Fatalf("recorder error = %v, want %q", err, tc.wantError)
					}
					if tc.execErr != nil && !errors.Is(err, tc.execErr) {
						t.Errorf("recorder lost execution error: %v", err)
					}
					want[spanAttrErrorType] = "tool_execution_error"
					want[spanAttrErrorCategory] = tc.wantCategory
				}
				ended := spans.Ended()
				if len(ended) != 1 {
					t.Fatalf("ended spans = %d, want 1", len(ended))
				}
				status := ended[0].Status()
				wantCode := codes.Ok
				if tc.wantError != "" {
					wantCode = codes.Error
				}
				if status.Code != wantCode || status.Description != tc.wantError {
					t.Errorf("span status = %v, want %v/%q", status, wantCode, tc.wantError)
				}
				attrs := attribute.NewSet(ended[0].Attributes()...)
				for _, key := range []string{spanAttrErrorType, spanAttrErrorCategory} {
					got, present := attrs.Value(attribute.Key(key))
					if present != (tc.wantError != "") || got.AsString() != want[key] {
						t.Errorf("span %s = %q (present %v), want %q", key, got.AsString(), present, want[key])
					}
				}
				histogram := collectToolDuration(t, reader)
				if len(histogram.DataPoints) != 1 {
					t.Fatalf("duration points = %d, want 1", len(histogram.DataPoints))
				}
				assertToolMetricAttributes(t, histogram.DataPoints[0].Attributes, want)
				assertToolDuration(t, histogram.DataPoints[0], 1, 1)
			})
		}
	}
}
