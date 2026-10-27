//go:build conformance && tool_execution_kinds

package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	lognoop "go.opentelemetry.io/otel/log/noop"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/grafana/agento11y/go/otelgenai"
	"github.com/grafana/agento11y/go/otelgenai/weavertest"
)

type executionKindTraceClient struct {
	spans []*tracepb.Span
}

func (*executionKindTraceClient) Start(context.Context) error { return nil }
func (*executionKindTraceClient) Stop(context.Context) error  { return nil }
func (c *executionKindTraceClient) UploadTraces(_ context.Context, resources []*tracepb.ResourceSpans) error {
	for _, resource := range resources {
		for _, scope := range resource.ScopeSpans {
			c.spans = append(c.spans, scope.Spans...)
		}
	}
	return nil
}

func TestToolExecutionKindsConformance(t *testing.T) {
	if _, err := exec.LookPath("weaver"); err != nil {
		t.Fatal("weaver is not on PATH; run test:go:tool-execution-kinds-conformance through mise")
	}
	const registryRef = "771e3210f7518694666834f7243573b8018c7a4f"
	if os.Getenv("SEMCONV_GENAI_REF") != registryRef {
		t.Fatal("tool execution kinds require their separate registry pin; run test:go:tool-execution-kinds-conformance through mise")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	assets, err := weavertest.Setup(ctx, registryRef)
	if err != nil {
		t.Fatalf("prepare Weaver inputs: %v", err)
	}
	assertExecutionKindCoverage(t, assets)

	data, err := os.ReadFile("../../../conformance/tool-execution-kinds/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version int      `json:"version"`
		Keys    []string `json:"skill_process_attributes"`
		Cases   []struct {
			ID    string            `json:"id"`
			Start map[string]string `json:"start"`
			End   struct {
				Code *int64 `json:"process_exit_code"`
			} `json:"end"`
			CaptureMode string                     `json:"capture_mode"`
			OTelCapture string                     `json:"otel_capture"`
			Error       string                     `json:"error"`
			Name        string                     `json:"span_name"`
			Attributes  map[string]json.RawMessage `json:"attributes"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 1 || len(fixture.Keys) == 0 || len(fixture.Cases) == 0 {
		t.Fatal("invalid tool-execution-kind fixture")
	}

	var samples []weavertest.Sample
	samplesByID := map[string]weavertest.Sample{}
	for _, tc := range fixture.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			client := &executionKindTraceClient{}
			exporter, err := otlptrace.New(ctx, client)
			if err != nil {
				t.Fatal(err)
			}
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			capture := otelgenai.CaptureNoContent
			if tc.CaptureMode == "full" {
				capture = otelgenai.CaptureSpanOnly
			}
			if tc.OTelCapture != "" {
				capture = otelgenai.CaptureMode(tc.OTelCapture)
			}
			// Only spans reach the newer registry. Legacy metric instruments stay on their existing pin.
			handler := otelgenai.NewHandler(otelgenai.WithTracerProvider(provider),
				otelgenai.WithMeterProvider(metricnoop.NewMeterProvider()),
				otelgenai.WithLoggerProvider(lognoop.NewLoggerProvider()), otelgenai.WithCaptureMode(capture))
			s := tc.Start
			inv := &otelgenai.Invocation{
				Operation: otelgenai.OperationExecuteTool, ToolName: s["tool_name"], ToolType: "function", ToolCallID: "call-1",
				ExecutionKind: otelgenai.ToolExecutionKind(s["execution_kind"]), SkillName: s["skill_name"],
				SkillDescription: s["skill_description"], SkillSourceURI: s["skill_source_uri"],
				SkillResourceName: s["skill_resource_name"], ProcessExecutableName: s["process_executable_name"],
				ProcessExecutablePath: s["process_executable_path"],
			}
			callCtx := handler.Start(ctx, inv)
			inv.ProcessExitCode, inv.ErrorMessage = tc.End.Code, tc.Error
			handler.End(callCtx, inv)
			if len(client.spans) != 1 {
				t.Fatalf("recorded %d spans, want one", len(client.spans))
			}
			span := client.spans[0]
			if span.Name != tc.Name {
				t.Errorf("span name = %q, want %q", span.Name, tc.Name)
			}
			if span.Kind != tracepb.Span_SPAN_KIND_INTERNAL {
				t.Errorf("kind = %v, want INTERNAL", span.Kind)
			}
			if (span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR) != (tc.Error != "") {
				t.Errorf("status = %v, execution error = %q", span.Status, tc.Error)
			}
			sample, err := weavertest.SampleFromSpan(span)
			if err != nil {
				t.Fatal(err)
			}
			attrs := map[string]json.RawMessage{}
			for _, attr := range sample.Span.Attributes {
				attrs[attr.Name] = attr.Value
			}
			toolName := s["tool_name"]
			if s["execution_kind"] == "load_skill" || s["execution_kind"] == "read_skill_resource" || s["execution_kind"] == "command" {
				toolName = strings.TrimSpace(toolName)
			}
			for key, want := range map[string]string{"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": toolName} {
				encoded, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				if string(attrs[key]) != string(encoded) {
					t.Errorf("%s = %s, want %s", key, attrs[key], encoded)
				}
			}
			for _, key := range fixture.Keys {
				got, present := attrs[key]
				want, expected := tc.Attributes[key]
				if present != expected || (present && string(got) != string(want)) {
					t.Errorf("%s = %s (present %v), want %s (present %v)", key, got, present, want, expected)
				}
			}
			if _, ok := attrs["agento11y.skill.name"]; ok {
				t.Error("legacy skill key emitted")
			}
			if _, ok := attrs["gen_ai.agent.name"]; ok {
				t.Error("agent identity fabricated for a tool with no agent name")
			}
			if len(sample.Span.SpanEvents) != 0 {
				t.Error("tool execution emitted events")
			}
			samples = append(samples, sample)
			samplesByID[tc.ID] = sample
		})
	}
	if len(samples) != len(fixture.Cases) {
		t.Fatal("not every fixture produced a sample")
	}
	findings, err := weavertest.LiveCheck(ctx, assets, samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("tool-execution-kind violations: %+v", findings)
	}
	for _, negative := range []struct {
		name    string
		fixture string
		mutate  func(*weavertest.Sample)
		id      string
		target  string
	}{
		{name: "rejects wrong tool name", fixture: "generic", id: "genai_span_name_format", mutate: func(sample *weavertest.Sample) { sample.Span.Name = "execute_tool wrong" }},
		{name: "rejects client kind", fixture: "command", id: "genai_span_kind_unexpected", mutate: func(sample *weavertest.Sample) { sample.Span.Kind = "client" }},
		{name: "rejects string exit code", fixture: "command", id: "type_mismatch", target: "process.exit.code", mutate: func(sample *weavertest.Sample) {
			for i := range sample.Span.Attributes {
				if sample.Span.Attributes[i].Name == "process.exit.code" {
					sample.Span.Attributes[i].Value = json.RawMessage(`"zero"`)
				}
			}
		}},
	} {
		t.Run(negative.name, func(t *testing.T) {
			sample, ok := samplesByID[negative.fixture]
			if !ok {
				t.Fatalf("missing negative-test source %s", negative.fixture)
			}
			sample.Span.Attributes = slices.Clone(sample.Span.Attributes)
			negative.mutate(&sample)
			findings, err := weavertest.LiveCheck(ctx, assets, []weavertest.Sample{sample})
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != 1 || findings[0].ID != negative.id || findings[0].Target != negative.target {
				t.Errorf("findings = %+v, want one %s at %q", findings, negative.id, negative.target)
			}
		})
	}
}

func assertExecutionKindCoverage(t *testing.T, assets weavertest.Assets) {
	t.Helper()
	paths, err := filepathGlobCoverage(assets.AdviceData)
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		Spans          map[string]json.RawMessage `json:"spans"`
		ExecutionKinds map[string]struct {
			Type       string            `json:"type"`
			Attributes map[string]string `json:"attributes"`
		} `json:"tool_execution_kinds"`
	}
	if err := json.Unmarshal(paths, &model); err != nil {
		t.Fatal(err)
	}
	if len(model.ExecutionKinds) != 3 {
		t.Fatalf("coverage contains %d tool execution kinds, want three", len(model.ExecutionKinds))
	}
	for _, executionKind := range []string{"load_skill", "read_skill_resource", "command"} {
		id := "gen_ai.execute_tool." + executionKind + ".internal"
		definition, ok := model.ExecutionKinds[id]
		if !ok || definition.Type != "gen_ai.execute_tool.internal" {
			t.Errorf("missing or incorrect execution kind %s: %+v", id, definition)
		}
		if _, ok := model.Spans[id]; ok {
			t.Errorf("execution kind %s merged into base spans", id)
		}
	}
	command := model.ExecutionKinds["gen_ai.execute_tool.command.internal"]
	for _, key := range []string{"process.executable.name", "process.executable.path", "process.exit.code"} {
		if _, ok := command.Attributes[key]; !ok {
			t.Errorf("command coverage omits %s", key)
		}
	}
	base := string(model.Spans["gen_ai.execute_tool.internal"])
	for _, key := range []string{"gen_ai.skill.", "process."} {
		if strings.Contains(base, key) {
			t.Errorf("base tool coverage includes skill/process attributes: %s", base)
		}
	}
}

func filepathGlobCoverage(glob string) ([]byte, error) {
	files, err := filepath.Glob(glob)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if filepath.Base(file) == "coverage-model.json" {
			return os.ReadFile(file)
		}
	}
	return nil, fmt.Errorf("coverage model missing from %s", glob)
}
