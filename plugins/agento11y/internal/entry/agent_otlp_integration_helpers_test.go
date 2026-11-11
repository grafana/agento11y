package entry

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/stretchr/testify/require"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
)

type agentOTLPCapture struct {
	server     *httptest.Server
	mu         sync.Mutex
	traceReqs  []*coltracepb.ExportTraceServiceRequest
	metricReqs []*colmetricpb.ExportMetricsServiceRequest
	errors     []error
}

func newAgentOTLPCapture(t *testing.T) *agentOTLPCapture {
	t.Helper()
	c := &agentOTLPCapture{}
	c.server = newLoopbackServer(t, http.HandlerFunc(c.serveHTTP))
	return c
}

func (c *agentOTLPCapture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	reject := func(status int, err error) {
		c.mu.Lock()
		c.errors = append(c.errors, fmt.Errorf("%s: %w", r.URL.Path, err))
		c.mu.Unlock()
		http.Error(w, err.Error(), status)
	}
	var message proto.Message
	switch r.URL.Path {
	case "/v1/traces":
		message = &coltracepb.ExportTraceServiceRequest{}
	case "/v1/metrics":
		message = &colmetricpb.ExportMetricsServiceRequest{}
	default:
		reject(http.StatusNotFound, errors.New("unknown OTLP route"))
		return
	}
	if r.Method != http.MethodPost {
		reject(http.StatusMethodNotAllowed, errors.New("OTLP requires POST"))
		return
	}
	var reader io.Reader = r.Body
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			reject(http.StatusBadRequest, fmt.Errorf("decode gzip: %w", err))
			return
		}
		defer gz.Close()
		reader = gz
	default:
		reject(http.StatusUnsupportedMediaType, errors.New("unsupported content encoding"))
		return
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		reject(http.StatusBadRequest, fmt.Errorf("read OTLP body: %w", err))
		return
	}
	if err := proto.Unmarshal(body, message); err != nil {
		reject(http.StatusBadRequest, fmt.Errorf("decode protobuf: %w", err))
		return
	}
	c.mu.Lock()
	switch request := message.(type) {
	case *coltracepb.ExportTraceServiceRequest:
		c.traceReqs = append(c.traceReqs, request)
	case *colmetricpb.ExportMetricsServiceRequest:
		c.metricReqs = append(c.metricReqs, request)
	}
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (c *agentOTLPCapture) snapshot() ([]*coltracepb.ExportTraceServiceRequest, []*colmetricpb.ExportMetricsServiceRequest, []error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	traces := make([]*coltracepb.ExportTraceServiceRequest, len(c.traceReqs))
	for i, request := range c.traceReqs {
		traces[i] = proto.Clone(request).(*coltracepb.ExportTraceServiceRequest)
	}
	metrics := make([]*colmetricpb.ExportMetricsServiceRequest, len(c.metricReqs))
	for i, request := range c.metricReqs {
		metrics[i] = proto.Clone(request).(*colmetricpb.ExportMetricsServiceRequest)
	}
	return traces, metrics, append([]error(nil), c.errors...)
}

// validate checks the companion OTLP export against want and the HTTP
// generations, given in the order of want.generations. It joins generation
// spans by generation, trace, span, and conversation ID, and tool spans by
// call ID to the generation span that issued the call.
// Generation export uses current semantic conventions; HTTP companion telemetry uses legacy names.
func (c *agentOTLPCapture) validate(want agentExpectation, generations []*agento11yv1.Generation, generationExport bool) error {
	traces, metrics, errs := c.snapshot()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("OTLP receiver: %w", err)
	}
	if len(generations) != len(want.generations) {
		return fmt.Errorf("got %d HTTP generations, want %d", len(generations), len(want.generations))
	}
	identity := map[string]string{
		"gen_ai.agent.name": want.agent, "gen_ai.provider.name": want.provider(), "gen_ai.request.model": fakeModelName,
		"agento11y.tag.user": fixtureEmail, "agento11y.tag.repo": fixtureRepo, "agento11y.tag.git.branch": fixtureBranch,
	}
	generationIndex := map[string]int{}
	type owner struct {
		generation int
		call       shellCall
	}
	owners := map[string]owner{}
	for i, w := range want.generations {
		generationIndex[generations[i].Id] = i
		for _, call := range w.calls {
			owners[call.id] = owner{i, call}
		}
	}
	forbidden := want.secrets
	if !want.full() {
		forbidden = slices.Concat(want.forbidden, want.secrets)
	}
	seenGenerations := map[int]bool{}
	seenCalls := map[string]bool{}
	for _, request := range traces {
		if err := agentOTLPCheckPrivacy(request, forbidden, !want.full()); err != nil {
			return fmt.Errorf("trace export: %w", err)
		}
		for _, resource := range request.GetResourceSpans() {
			for _, scope := range resource.GetScopeSpans() {
				for _, span := range scope.GetSpans() {
					if err := agentOTLPWantAttrs(span.GetAttributes(), identity); err != nil {
						return fmt.Errorf("span %q: %w", span.GetName(), err)
					}
					if len(span.GetTraceId()) != 16 || len(span.GetSpanId()) != 8 || bytes.Equal(span.GetTraceId(), make([]byte, 16)) || bytes.Equal(span.GetSpanId(), make([]byte, 8)) {
						return fmt.Errorf("span %q has invalid trace/span ID", span.GetName())
					}
					if span.StartTimeUnixNano == 0 || span.EndTimeUnixNano < span.StartTimeUnixNano {
						return fmt.Errorf("span %q has invalid timestamps", span.Name)
					}
					attrs := agentOTLPAttrs(span.Attributes)
					switch attrs["gen_ai.operation.name"] {
					case "execute_tool":
						id := attrs["gen_ai.tool.call.id"]
						o, ok := owners[id]
						if !ok || seenCalls[id] {
							return fmt.Errorf("unexpected or duplicate tool span %q", id)
						}
						seenCalls[id] = true
						parent := generations[o.generation]
						if err := agentOTLPWantAttrs(span.Attributes, map[string]string{
							"gen_ai.tool.name": "Bash", "gen_ai.tool.type": "function", "gen_ai.conversation.id": parent.ConversationId,
						}); err != nil {
							return fmt.Errorf("tool span %q: %w", id, err)
						}
						if hex.EncodeToString(span.TraceId) != parent.TraceId || hex.EncodeToString(span.ParentSpanId) != parent.SpanId {
							return fmt.Errorf("tool span %q parent %x/%x, want generation %s span %s/%s", id, span.TraceId, span.ParentSpanId, parent.Id, parent.TraceId, parent.SpanId)
						}
						if failed := span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR; (!o.call.failed || want.reportsFailure()) && failed != o.call.failed {
							return fmt.Errorf("tool span %q error status = %t, want %t", id, failed, o.call.failed)
						}
						if want.full() {
							for key, marker := range map[string]string{"gen_ai.tool.call.arguments": o.call.wantInput(), "gen_ai.tool.call.result": o.call.wantResult()} {
								if !strings.Contains(attrs[key], marker) {
									return fmt.Errorf("tool span %q %s = %q lacks %q", id, key, attrs[key], marker)
								}
							}
						}
					case "generateText", "streamText", "chat":
						id := attrs["agento11y.generation.id"]
						i, ok := generationIndex[id]
						if !ok || seenGenerations[i] {
							return fmt.Errorf("unexpected or duplicate generation ID %q", id)
						}
						seenGenerations[i] = true
						g := generations[i]
						if hex.EncodeToString(span.TraceId) != g.TraceId || hex.EncodeToString(span.SpanId) != g.SpanId {
							return fmt.Errorf("generation %q span %x/%x, want HTTP export %s/%s", id, span.TraceId, span.SpanId, g.TraceId, g.SpanId)
						}
						if err := agentOTLPWantAttrs(span.Attributes, map[string]string{"gen_ai.conversation.id": g.ConversationId, "user.id": fixtureEmail}); err != nil {
							return fmt.Errorf("generation %q identity: %w", id, err)
						}
						if err := agentOTLPWantUsage(span.Attributes, want.generations[i].usage, generationExport); err != nil {
							return fmt.Errorf("generation %q: %w", id, err)
						}
					default:
						return fmt.Errorf("unexpected span operation %q", attrs["gen_ai.operation.name"])
					}
				}
			}
		}
	}
	if len(seenGenerations) != len(want.generations) || len(seenCalls) != len(owners) {
		return fmt.Errorf("span counts: generations=%d tools=%d, want %d and %d", len(seenGenerations), len(seenCalls), len(want.generations), len(owners))
	}
	for _, request := range metrics {
		if err := agentOTLPCheckPrivacy(request, forbidden, !want.full()); err != nil {
			return fmt.Errorf("metric export: %w", err)
		}
	}
	return agentOTLPWantTokenMetrics(metrics, want, identity, generationExport)
}

// agentOTLPWantUsage requires exactly the nonzero usage attributes, as
// integers, and the inclusive marker whenever usage was reported.
func agentOTLPWantUsage(attrs []*commonpb.KeyValue, want wantUsage, generationExport bool) error {
	cacheRead, cacheWrite, reasoning := "gen_ai.usage.cache_read_input_tokens", "gen_ai.usage.cache_write_input_tokens", "gen_ai.usage.reasoning_tokens"
	if generationExport {
		cacheRead, cacheWrite, reasoning = "gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.reasoning.output_tokens"
	}
	for key, value := range map[string]int64{
		"gen_ai.usage.input_tokens": want.input, "gen_ai.usage.output_tokens": want.output,
		cacheRead: want.cacheRead, cacheWrite: want.cacheWrite, reasoning: want.reasoning,
	} {
		got, ok, err := agentOTLPIntAttr(attrs, key)
		if err != nil {
			return err
		}
		if ok != (value != 0) || got != value {
			return fmt.Errorf("usage attribute %s = %d (present %t), want %d", key, got, ok, value)
		}
	}
	semantics := ""
	if want != (wantUsage{}) {
		semantics = "inclusive"
	}
	if got := agentOTLPAttrs(attrs)["gen_ai.token.semantics"]; got != semantics {
		return fmt.Errorf("gen_ai.token.semantics = %q, want %q", got, semantics)
	}
	return nil
}

func agentOTLPIntAttr(attrs []*commonpb.KeyValue, key string) (int64, bool, error) {
	for _, attr := range attrs {
		if attr.GetKey() != key {
			continue
		}
		value, ok := attr.GetValue().GetValue().(*commonpb.AnyValue_IntValue)
		if !ok {
			return 0, true, fmt.Errorf("attribute %s is %T, want an integer", key, attr.GetValue().GetValue())
		}
		return value.IntValue, true, nil
	}
	return 0, false, nil
}

// agentOTLPWantTokenMetrics compares token-usage histograms with the
// nonzero usage buckets of every expected generation. Each hook process
// exports cumulative series, possibly more than once (ForceFlush, then
// Shutdown), so only the latest snapshot of each series lifetime counts. A
// lifetime is the full series identity plus its StartTimeUnixNano.
func agentOTLPWantTokenMetrics(metrics []*colmetricpb.ExportMetricsServiceRequest, want agentExpectation, identity map[string]string, generationExport bool) error {
	type observation struct {
		count uint64
		sum   float64
	}
	wantTotals := map[string]observation{}
	for _, g := range want.generations {
		for tokenType, value := range g.usage.buckets() {
			if generationExport && tokenType == "cache_write" {
				tokenType = "cache_creation"
			}
			total := wantTotals[tokenType]
			total.count++
			total.sum += float64(value)
			wantTotals[tokenType] = total
		}
	}
	type snapshot struct {
		tokenType string
		point     *metricpb.HistogramDataPoint
	}
	series := map[string][]snapshot{}
	for _, request := range metrics {
		for _, resource := range request.GetResourceMetrics() {
			for _, scope := range resource.GetScopeMetrics() {
				for _, metric := range scope.GetMetrics() {
					if metric.GetName() != "gen_ai.client.token.usage" {
						continue
					}
					histogram := metric.GetHistogram()
					if histogram == nil || len(histogram.DataPoints) == 0 {
						return errors.New("token usage requires histogram datapoints")
					}
					if histogram.AggregationTemporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
						return fmt.Errorf("token usage temporality %s, want cumulative", histogram.AggregationTemporality)
					}
					for _, point := range histogram.DataPoints {
						// user.id identifies spans, not metric series. The user tag is a metric dimension.
						if err := agentOTLPWantAttrs(point.GetAttributes(), identity); err != nil {
							return fmt.Errorf("token usage datapoint: %w", err)
						}
						attrs := agentOTLPAttrs(point.Attributes)
						tokenType := attrs["gen_ai.token.type"]
						if _, ok := wantTotals[tokenType]; !ok {
							return fmt.Errorf("unexpected token type %q", tokenType)
						}
						if attrs["gen_ai.token.semantics"] != "inclusive" {
							return fmt.Errorf("%s token usage semantics %q, want inclusive", tokenType, attrs["gen_ai.token.semantics"])
						}
						if point.Count == 0 || point.Sum == nil || point.Flags != 0 {
							return fmt.Errorf("incorrect %s token usage count/sum: count=%d sum=%v", tokenType, point.Count, point.Sum)
						}
						if point.StartTimeUnixNano == 0 || point.TimeUnixNano < point.StartTimeUnixNano {
							return fmt.Errorf("invalid %s token usage timestamps: start=%d time=%d", tokenType, point.StartTimeUnixNano, point.TimeUnixNano)
						}
						if len(point.BucketCounts) > 0 {
							var buckets uint64
							for _, n := range point.BucketCounts {
								buckets += n
							}
							if buckets != point.Count {
								return fmt.Errorf("%s token usage buckets hold %d observations, count %d", tokenType, buckets, point.Count)
							}
						}
						key := strings.Join([]string{
							agentOTLPKey(resource.GetResource().GetAttributes()), scope.GetScope().GetName(), scope.GetScope().GetVersion(),
							metric.GetName(), metric.GetUnit(), agentOTLPKey(point.Attributes), strconv.FormatUint(point.StartTimeUnixNano, 10),
						}, "\x00")
						series[key] = append(series[key], snapshot{tokenType, point})
					}
				}
			}
		}
	}
	got := map[string]observation{}
	for _, snapshots := range series {
		slices.SortStableFunc(snapshots, func(a, b snapshot) int { return cmp.Compare(a.point.TimeUnixNano, b.point.TimeUnixNano) })
		for i := 1; i < len(snapshots); i++ {
			previous, current := snapshots[i-1].point, snapshots[i].point
			if current.Count < previous.Count || current.GetSum() < previous.GetSum() ||
				(current.TimeUnixNano == previous.TimeUnixNano && (current.Count != previous.Count || current.GetSum() != previous.GetSum())) {
				return fmt.Errorf("%s token usage series is not cumulative: count %d sum %v after count %d sum %v",
					snapshots[i].tokenType, current.Count, current.GetSum(), previous.Count, previous.GetSum())
			}
		}
		latest := snapshots[len(snapshots)-1]
		total := got[latest.tokenType]
		total.count += latest.point.Count
		total.sum += latest.point.GetSum()
		got[latest.tokenType] = total
	}
	for _, tokenType := range slices.Sorted(maps.Keys(wantTotals)) {
		if got[tokenType] != wantTotals[tokenType] {
			return fmt.Errorf("%s token usage count/sum %+v, want %+v", tokenType, got[tokenType], wantTotals[tokenType])
		}
	}
	return nil
}

// agentOTLPKey renders attributes independent of their order.
func agentOTLPKey(attrs []*commonpb.KeyValue) string {
	parts := make([]string, len(attrs))
	for i, attr := range attrs {
		value, _ := proto.MarshalOptions{Deterministic: true}.Marshal(attr.GetValue())
		parts[i] = attr.GetKey() + "=" + hex.EncodeToString(value)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// HTTP reconstruction can hide missing attributes and malformed content extensions.
func (c *agentOTLPCapture) validateGenerations(want agentExpectation) ([]*agento11yv1.Generation, error) {
	if want.mode == "default" || want.mode == "" {
		want.mode = "metadata_only"
	}
	traces, _, errs := c.snapshot()
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("OTLP receiver: %w", err)
	}
	var references []*agento11yv1.Generation
	spans := map[string]*tracepb.Span{}
	toolCalls := map[string][]string{}
	spanIDs := map[string]bool{}
	conversation := want.conversation
	for _, request := range traces {
		for _, resource := range request.GetResourceSpans() {
			for _, scope := range resource.GetScopeSpans() {
				for _, span := range scope.GetSpans() {
					attrs := agentOTLPAttrs(span.Attributes)
					switch attrs["gen_ai.operation.name"] {
					case "execute_tool":
						parent := hex.EncodeToString(span.TraceId) + "/" + hex.EncodeToString(span.ParentSpanId)
						toolCalls[parent] = append(toolCalls[parent], attrs["gen_ai.tool.call.id"])
						continue
					case "generateText", "streamText", "chat":
					default:
						continue
					}
					if err := agentOTLPWantAttrs(span.Attributes, map[string]string{"agento11y.record": "true"}); err != nil {
						return nil, fmt.Errorf("generation routing marker: %w", err)
					}
					id := attrs["agento11y.generation.id"]
					if id == "" || spans[id] != nil {
						return nil, fmt.Errorf("missing or duplicate generation ID %q", id)
					}
					traceID, spanID := hex.EncodeToString(span.TraceId), hex.EncodeToString(span.SpanId)
					if !validHexID(traceID, 32) || !validHexID(spanID, 16) || spanIDs[traceID+"/"+spanID] {
						return nil, fmt.Errorf("invalid or duplicate generation trace/span ID %s/%s", traceID, spanID)
					}
					spanIDs[traceID+"/"+spanID] = true
					if span.StartTimeUnixNano == 0 || span.EndTimeUnixNano < span.StartTimeUnixNano {
						return nil, errors.New("generation has invalid timestamps")
					}
					if got := attrs["gen_ai.conversation.id"]; got == "" || (conversation != "" && conversation != got) {
						return nil, fmt.Errorf("missing or inconsistent generation conversation ID %q", got)
					} else {
						conversation = got
					}
					var metadata map[string]any
					if err := json.Unmarshal([]byte(attrs["agento11y.generation.metadata"]), &metadata); err != nil {
						return nil, fmt.Errorf("decode generation metadata: %w", err)
					}
					if metadata["agento11y.sdk.content_capture_mode"] != want.mode || metadata["agento11y.user.id"] != fixtureEmail {
						return nil, errors.New("incorrect generation capture mode or identity metadata")
					}
					var tags map[string]string
					if err := json.Unmarshal([]byte(attrs["agento11y.generation.tags"]), &tags); err != nil {
						return nil, fmt.Errorf("decode generation tags: %w", err)
					}
					for key, value := range map[string]string{"user": fixtureEmail, "repo": fixtureRepo, "git.branch": fixtureBranch} {
						if tags[key] != value {
							return nil, fmt.Errorf("incorrect generation tag %s", key)
						}
					}
					spans[id] = span
					references = append(references, &agento11yv1.Generation{Id: id, TraceId: traceID, SpanId: spanID, ConversationId: conversation})
				}
			}
		}
	}
	if len(references) != len(want.generations) {
		return nil, fmt.Errorf("got %d generation spans, want %d", len(references), len(want.generations))
	}
	order, err := matchGenerations(want.generations, references, func(w wantGeneration, g *agento11yv1.Generation) error {
		span := spans[g.Id]
		// Metadata-only generation spans omit messages. Their child tool spans
		// distinguish generations with equal usage but different tool calls.
		if err := sameIDs("companion tool calls", toolCalls[g.TraceId+"/"+g.SpanId], w.calls); err != nil {
			return err
		}
		if err := agentOTLPWantUsage(span.Attributes, w.usage, true); err != nil {
			return err
		}
		total, present, err := agentOTLPIntAttr(span.Attributes, "agento11y.gen_ai.usage.total_tokens")
		if err != nil {
			return err
		}
		if value := w.usage.input + w.usage.output; total != value || present != (value != 0) {
			return fmt.Errorf("generation total usage = %d (present %t), want %d", total, present, value)
		}
		if want.full() {
			return agentOTLPWantGenerationContent(span, w, want)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ordered := make([]*agento11yv1.Generation, len(order))
	for i, j := range order {
		ordered[i] = references[j]
	}
	if err := c.validate(want, ordered, true); err != nil {
		return nil, err
	}
	return ordered, nil
}

type agentOTLPMessage struct {
	Role  string          `json:"role"`
	Parts []agentOTLPPart `json:"parts"`
}

type agentOTLPPart struct {
	Type         string          `json:"type"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Content      string          `json:"content,omitempty"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
	ArgumentsB64 json.RawMessage `json:"agento11y.arguments_b64,omitempty"`
	ResponseB64  json.RawMessage `json:"agento11y.response_b64,omitempty"`
	IsError      bool            `json:"agento11y.is_error,omitempty"`
}

func agentOTLPJSONContent(raw, encoded json.RawMessage) ([]byte, error) {
	if encoded != nil {
		var text string
		if err := json.Unmarshal(encoded, &text); err != nil {
			return nil, fmt.Errorf("decode base64 field: %w", err)
		}
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, fmt.Errorf("decode base64 content: %w", err)
		}
		raw = decoded
	}
	if !json.Valid(raw) {
		return nil, errors.New("missing or invalid JSON content")
	}
	return raw, nil
}

func agentOTLPWantGenerationContent(span *tracepb.Span, w wantGeneration, want agentExpectation) error {
	attrs := agentOTLPAttrs(span.Attributes)
	var definitions []struct{ Name string }
	if raw := attrs["gen_ai.tool.definitions"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &definitions); err != nil {
			return fmt.Errorf("decode tool definitions: %w", err)
		}
	}
	var names, wantNames []string
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	if len(w.calls) > 0 {
		wantNames = []string{"Bash"}
	}
	if !slices.Equal(names, wantNames) {
		return fmt.Errorf("tool definitions %v, want %v", names, wantNames)
	}
	byID := map[string]shellCall{}
	for _, call := range slices.Concat(w.calls, w.results) {
		byID[call.id] = call
	}
	var calls, results, prompts, texts []string
	for _, key := range []string{"gen_ai.input.messages", "gen_ai.output.messages"} {
		var messages []agentOTLPMessage
		if raw := attrs[key]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &messages); err != nil {
				return fmt.Errorf("decode %s: %w", key, err)
			}
		}
		input := key == "gen_ai.input.messages"
		for _, message := range messages {
			if !input && message.Role != "assistant" {
				return fmt.Errorf("output %s message", message.Role)
			}
			for _, part := range message.Parts {
				switch part.Type {
				case "text":
					if input {
						if message.Role != "user" {
							return fmt.Errorf("input text in %s message", message.Role)
						}
						prompts = append(prompts, part.Content)
					} else {
						texts = append(texts, part.Content)
					}
				case "tool_call":
					if input || part.Name != "Bash" {
						return fmt.Errorf("incorrect generation tool call %q", part.ID)
					}
					calls = append(calls, part.ID)
					raw, err := agentOTLPJSONContent(part.Arguments, part.ArgumentsB64)
					if err != nil {
						return fmt.Errorf("generation tool call %q: %w", part.ID, err)
					}
					if err := checkToolContent("generation tool call "+part.ID, "", raw, byID[part.ID].wantInput(), true); err != nil {
						return err
					}
				case "tool_call_response":
					if !input || message.Role != "tool" {
						return fmt.Errorf("generation tool result in %s message", message.Role)
					}
					results = append(results, part.ID)
					call := byID[part.ID]
					if (!call.failed || want.reportsFailure()) && part.IsError != call.failed {
						return fmt.Errorf("generation tool result %q is_error = %t, want %t", part.ID, part.IsError, call.failed)
					}
					raw, err := agentOTLPJSONContent(part.Response, part.ResponseB64)
					if err != nil {
						return fmt.Errorf("generation tool result %q: %w", part.ID, err)
					}
					if err := checkToolContent("generation tool result "+part.ID, "", raw, call.wantResult(), true); err != nil {
						return err
					}
				case "reasoning":
					if input {
						return errors.New("input reasoning part")
					}
				default:
					return fmt.Errorf("unexpected generation part %q", part.Type)
				}
			}
		}
	}
	if err := sameIDs("tool calls", calls, w.calls); err != nil {
		return err
	}
	if err := sameIDs("tool results", results, w.results); err != nil {
		return err
	}
	if err := checkProse("prompt", prompts, w.prompt, true); err != nil {
		return err
	}
	return checkProse("reply", texts, w.text, true)
}

func agentOTLPAttrs(attrs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[attr.GetKey()] = attr.GetValue().GetStringValue()
	}
	return out
}

func agentOTLPWantAttrs(attrs []*commonpb.KeyValue, want map[string]string) error {
	seen := map[string]bool{}
	for _, attr := range attrs {
		if seen[attr.GetKey()] {
			return fmt.Errorf("duplicate attribute %q", attr.GetKey())
		}
		seen[attr.GetKey()] = true
	}
	got := agentOTLPAttrs(attrs)
	for key, value := range want {
		if got[key] != value {
			return fmt.Errorf("attribute %s = %q, want %q", key, got[key], value)
		}
	}
	return nil
}

func agentOTLPContentKey(key string) bool {
	switch key {
	case "gen_ai.system_instructions", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.tool.definitions",
		"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "gen_ai.tool.description", "gen_ai.embeddings.input_texts",
		"gen_ai.skill.description", "gen_ai.skill.source.uri", "process.executable.path",
		"gen_ai.retrieval.query.text", "gen_ai.retrieval.documents", "agento11y.generation.raw_artifacts",
		"agento11y.conversation.title", "sigil.conversation.title", "call_error", "exception.message", "exception.stacktrace":
		return true
	}
	return false
}

// agentOTLPCheckPrivacy walks every string and bytes field, including
// base64 and nested JSON, for forbidden text. With noContent, content
// attribute keys are also rejected, empty or not.
func agentOTLPCheckPrivacy(message proto.Message, forbidden []string, noContent bool) error {
	var walk func(protoreflect.Message) error
	walk = func(m protoreflect.Message) error {
		if m.Descriptor().FullName() == "opentelemetry.proto.common.v1.KeyValue" {
			key := m.Get(m.Descriptor().Fields().ByName("key")).String()
			if noContent && agentOTLPContentKey(key) {
				return fmt.Errorf("content attribute %q", key)
			}
		}
		var err error
		m.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			check := func(v protoreflect.Value) error {
				switch field.Kind() {
				case protoreflect.MessageKind:
					return walk(v.Message())
				case protoreflect.StringKind:
					return agentOTLPCheckText(v.String(), forbidden, noContent, 0)
				case protoreflect.BytesKind:
					return agentOTLPCheckText(string(v.Bytes()), forbidden, noContent, 0)
				default:
					return nil
				}
			}
			if field.IsList() {
				list := value.List()
				for i := 0; i < list.Len() && err == nil; i++ {
					err = check(list.Get(i))
				}
			} else {
				err = check(value)
			}
			return err == nil
		})
		return err
	}
	return walk(message.ProtoReflect())
}

func agentOTLPCheckText(text string, forbidden []string, noContent bool, depth int) error {
	for _, f := range forbidden {
		if strings.Contains(text, f) {
			return fmt.Errorf("leaked ordinary content %q", f)
		}
	}
	if depth >= 8 {
		return errors.New("cannot inspect deeply encoded content")
	}
	if raw, err := base64.StdEncoding.DecodeString(text); err == nil && len(raw) > 0 {
		if err := agentOTLPCheckText(string(raw), forbidden, noContent, depth+1); err != nil {
			return err
		}
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) != nil {
		return nil
	}
	var walk func(any) error
	walk = func(value any) error {
		switch v := value.(type) {
		case string:
			return agentOTLPCheckText(v, forbidden, noContent, depth+1)
		case []any:
			for _, item := range v {
				if err := walk(item); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, item := range v {
				if noContent && agentOTLPContentKey(key) {
					return fmt.Errorf("content payload key %q", key)
				}
				if err := agentOTLPCheckText(key, forbidden, noContent, depth+1); err != nil {
					return err
				}
				if err := walk(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(decoded)
}

func TestAgentOTLPCaptureReceiver(t *testing.T) {
	for _, signal := range []string{"traces", "metrics"} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/gzip=%t", signal, compressed), func(t *testing.T) {
				f := agentOTLPFixture(fixtureExpectations()["claude-code/metadata_only/baseline"])
				traces, metrics := f.traces, f.metrics[0]
				var message proto.Message = traces
				if signal == "metrics" {
					message = metrics
				}
				body, err := proto.Marshal(message)
				require.NoError(t, err)
				encoding := ""
				if compressed {
					body, encoding = agentOTLPGzip(t, body), "gzip"
				}
				capture := newAgentOTLPCapture(t)
				agentOTLPPost(t, capture, "/v1/"+signal, encoding, body, http.StatusOK)
				gotTraces, gotMetrics, errs := capture.snapshot()
				require.Empty(t, errs)
				if signal == "traces" {
					require.Empty(t, gotMetrics)
					require.Len(t, gotTraces, 1)
					require.True(t, proto.Equal(traces, gotTraces[0]))
					gotTraces[0].ResourceSpans = nil
				} else {
					require.Empty(t, gotTraces)
					require.Len(t, gotMetrics, 1)
					require.True(t, proto.Equal(metrics, gotMetrics[0]))
					gotMetrics[0].ResourceMetrics = nil
				}
				gotTraces, gotMetrics, _ = capture.snapshot()
				if signal == "traces" {
					require.True(t, proto.Equal(traces, gotTraces[0]), "snapshot must not alias stored protobufs")
				} else {
					require.True(t, proto.Equal(metrics, gotMetrics[0]), "snapshot must not alias stored protobufs")
				}
			})
		}
	}
}

func TestAgentOTLPCaptureRejectsRequests(t *testing.T) {
	for _, route := range []string{"/v1/traces", "/v1/metrics"} {
		for _, tc := range []struct {
			name, encoding string
			body           []byte
			status         int
		}{
			{name: "malformed protobuf", body: []byte{0xff}, status: http.StatusBadRequest},
			{name: "malformed gzip", encoding: "gzip", body: []byte("invalid"), status: http.StatusBadRequest},
			{name: "gzipped malformed protobuf", encoding: "gzip", body: agentOTLPGzip(t, []byte{0xff}), status: http.StatusBadRequest},
			{name: "truncated gzip", encoding: "gzip", body: agentOTLPGzip(t, nil)[:12], status: http.StatusBadRequest},
			{name: "unsupported encoding", encoding: "br", status: http.StatusUnsupportedMediaType},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				capture := newAgentOTLPCapture(t)
				agentOTLPPost(t, capture, route, tc.encoding, tc.body, tc.status)
				traces, metrics, errs := capture.snapshot()
				require.Empty(t, traces)
				require.Empty(t, metrics)
				require.Len(t, errs, 1)
				require.ErrorContains(t, capture.validate(agentExpectation{}, nil, false), "OTLP receiver")
			})
		}
	}
	for _, route := range []string{"/wrong", "/v1/logs", "/api/v1/generations:export"} {
		t.Run("unknown route "+route, func(t *testing.T) {
			capture := newAgentOTLPCapture(t)
			agentOTLPPost(t, capture, route, "", nil, http.StatusNotFound)
			traces, metrics, errs := capture.snapshot()
			require.Empty(t, traces)
			require.Empty(t, metrics)
			require.Len(t, errs, 1)
			require.ErrorContains(t, errs[0], "unknown OTLP route")
		})
	}
}

func agentOTLPGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, err := writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func agentOTLPPost(t *testing.T, capture *agentOTLPCapture, path, encoding string, body []byte, status int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, capture.server.URL+path, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/x-protobuf")
	request.Header.Set("Content-Encoding", encoding)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.Equal(t, status, response.StatusCode)
}

func TestAgentOTLPCaptureConcurrentSnapshots(t *testing.T) {
	capture := newAgentOTLPCapture(t)
	f := agentOTLPFixture(fixtureExpectations()["codex/metadata_only/baseline"])
	traces, metrics := f.traces, f.metrics[0]
	traceBody, err := proto.Marshal(traces)
	require.NoError(t, err)
	metricBody, err := proto.Marshal(metrics)
	require.NoError(t, err)
	const requests = 8
	results := make(chan error, requests)
	client := &http.Client{Timeout: 5 * time.Second}
	for i := range requests {
		go func() {
			path, body := "/v1/traces", traceBody
			if i%2 != 0 {
				path, body = "/v1/metrics", metricBody
			}
			response, err := client.Post(capture.server.URL+path, "application/x-protobuf", bytes.NewReader(body))
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusOK {
					err = fmt.Errorf("HTTP status %d", response.StatusCode)
				}
			}
			traces, metrics, _ := capture.snapshot()
			for _, request := range traces {
				request.ResourceSpans = nil
			}
			for _, request := range metrics {
				request.ResourceMetrics = nil
			}
			results <- err
		}()
	}
	for range requests {
		require.NoError(t, <-results)
	}
	gotTraces, gotMetrics, errs := capture.snapshot()
	require.Empty(t, errs)
	require.Len(t, gotTraces, requests/2)
	require.Len(t, gotMetrics, requests/2)
	for _, request := range gotTraces {
		require.True(t, proto.Equal(traces, request))
	}
	for _, request := range gotMetrics {
		require.True(t, proto.Equal(metrics, request))
	}
}

func TestAgentOTLPCheckPrivacy(t *testing.T) {
	value := func(text string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: text}}
	}
	metadataForbidden := []string{"Run echo", fakeToolOutput, "Done.", baselineSecret}
	for _, tc := range []struct {
		name    string
		message proto.Message
		full    bool
		wantErr string
	}{
		{name: "safe metadata", message: &commonpb.KeyValue{Key: "agento11y.generation.metadata", Value: value(`{"agento11y.sdk.content_capture_mode":"metadata_only"}`)}},
		{name: "empty content payload", message: &commonpb.KeyValue{Key: "gen_ai.input.messages", Value: value("")}, wantErr: "content attribute"},
		{name: "event", message: &tracepb.Span_Event{Name: "custom", Attributes: []*commonpb.KeyValue{{Key: "custom", Value: value(fakeToolOutput)}}}, wantErr: "ordinary content"},
		{name: "status", message: &tracepb.Status{Message: "Done."}, wantErr: "ordinary content"},
		{name: "scope", message: &commonpb.InstrumentationScope{Name: "Run echo"}, wantErr: "ordinary content"},
		{name: "metric exemplar", message: &metricpb.Exemplar{FilteredAttributes: []*commonpb.KeyValue{{Key: "custom", Value: value(fakeToolOutput)}}}, wantErr: "ordinary content"},
		{name: "nested JSON", message: &commonpb.KeyValue{Key: "custom", Value: value(`{"nested":{"agento11y.conversation.title":"ordinary title"}}`)}, wantErr: "content payload key"},
		{name: "base64", message: &commonpb.KeyValue{Key: "custom", Value: value(base64.StdEncoding.EncodeToString([]byte(fakeToolOutput)))}, wantErr: "ordinary content"},
		{name: "JSON base64", message: &commonpb.KeyValue{Key: "custom", Value: value(`{"payload":"` + base64.StdEncoding.EncodeToString([]byte("Done.")) + `"}`)}, wantErr: "ordinary content"},
		{name: "bytes", message: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte(baselineSecret)}}, wantErr: "ordinary content"},
		{name: "nested attributes", message: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{{Key: "gen_ai.tool.call.result", Value: value("ordinary result")}}}}}, wantErr: "content attribute"},
		{name: "full content attribute", full: true, message: &commonpb.KeyValue{Key: "gen_ai.tool.call.result", Value: value("hello-from-fake [REDACTED:grafana-cloud-token]")}},
		{name: "full content payload key", full: true, message: &commonpb.KeyValue{Key: "custom", Value: value(`{"gen_ai.input.messages":"Run echo"}`)}},
		{name: "full raw secret", full: true, message: &commonpb.KeyValue{Key: "gen_ai.tool.call.arguments", Value: value(`{"command":"` + secretArgumentToken + `"}`)}, wantErr: "ordinary content"},
		{name: "full escaped JSON secret", full: true, message: &commonpb.KeyValue{Key: "custom", Value: value(`{"payload":"` + strings.ReplaceAll(secretOutputToken, "_", `\u005f`) + `"}`)}, wantErr: "ordinary content"},
		{name: "full base64 bytes secret", full: true, message: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte(base64.StdEncoding.EncodeToString([]byte(secretReplyToken)))}}, wantErr: "ordinary content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forbidden := metadataForbidden
			if tc.full {
				forbidden = agentCaseSecrets
			}
			err := agentOTLPCheckPrivacy(tc.message, forbidden, !tc.full)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

type agentOTLPFixtureData struct {
	traces      *coltracepb.ExportTraceServiceRequest
	metrics     []*colmetricpb.ExportMetricsServiceRequest
	generations []*agento11yv1.Generation
}

func agentOTLPString(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func agentOTLPInt(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}}
}

// agentOTLPFixture builds the companion OTLP export for want. Every
// generation has its own metric lifetime, flushed twice.
func agentOTLPFixture(want agentExpectation) agentOTLPFixtureData {
	generations := generationExportFixture(want).Generations
	identity := func() []*commonpb.KeyValue {
		return []*commonpb.KeyValue{
			agentOTLPString("gen_ai.agent.name", want.agent), agentOTLPString("gen_ai.provider.name", want.provider()), agentOTLPString("gen_ai.request.model", fakeModelName),
			agentOTLPString("agento11y.tag.user", fixtureEmail), agentOTLPString("agento11y.tag.repo", fixtureRepo), agentOTLPString("agento11y.tag.git.branch", fixtureBranch),
		}
	}
	var spans []*tracepb.Span
	var metrics []*colmetricpb.ExportMetricsServiceRequest
	for i, w := range want.generations {
		g := generations[i]
		traceID, _ := hex.DecodeString(g.TraceId)
		spanID, _ := hex.DecodeString(g.SpanId)
		attrs := append(identity(), agentOTLPString("gen_ai.operation.name", "generateText"), agentOTLPString("agento11y.generation.id", g.Id),
			agentOTLPString("gen_ai.conversation.id", g.ConversationId), agentOTLPString("user.id", fixtureEmail))
		for _, usage := range []struct {
			key   string
			value int64
		}{
			{"gen_ai.usage.input_tokens", w.usage.input}, {"gen_ai.usage.output_tokens", w.usage.output},
			{"gen_ai.usage.cache_read_input_tokens", w.usage.cacheRead}, {"gen_ai.usage.cache_write_input_tokens", w.usage.cacheWrite},
			{"gen_ai.usage.reasoning_tokens", w.usage.reasoning},
		} {
			if usage.value != 0 {
				attrs = append(attrs, agentOTLPInt(usage.key, usage.value))
			}
		}
		if w.usage != (wantUsage{}) {
			attrs = append(attrs, agentOTLPString("gen_ai.token.semantics", "inclusive"))
		}
		spans = append(spans, &tracepb.Span{Name: "generateText " + fakeModelName, TraceId: traceID, SpanId: spanID, StartTimeUnixNano: 100, EndTimeUnixNano: 200, Attributes: attrs})
		for j, call := range w.calls {
			attrs := append(identity(), agentOTLPString("gen_ai.operation.name", "execute_tool"), agentOTLPString("gen_ai.tool.name", "Bash"),
				agentOTLPString("gen_ai.tool.call.id", call.id), agentOTLPString("gen_ai.tool.type", "function"), agentOTLPString("gen_ai.conversation.id", g.ConversationId))
			if want.full() {
				attrs = append(attrs, agentOTLPString("gen_ai.tool.call.arguments", `{"command":"`+call.wantInput()+`"}`), agentOTLPString("gen_ai.tool.call.result", call.wantResult()))
			}
			status := &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
			if call.failed && want.reportsFailure() {
				status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
			}
			spans = append(spans, &tracepb.Span{
				Name: "execute_tool Bash", TraceId: traceID, SpanId: []byte{0x80, 0, 0, 0, 0, 0, byte(i + 1), byte(j + 1)}, ParentSpanId: spanID,
				StartTimeUnixNano: 120, EndTimeUnixNano: 180, Attributes: attrs, Status: status,
			})
		}
		var points []*metricpb.HistogramDataPoint
		buckets := w.usage.buckets()
		for _, tokenType := range slices.Sorted(maps.Keys(buckets)) {
			sum := float64(buckets[tokenType])
			points = append(points, &metricpb.HistogramDataPoint{
				Attributes:        append(identity(), agentOTLPString("gen_ai.operation.name", "generateText"), agentOTLPString("gen_ai.token.type", tokenType), agentOTLPString("gen_ai.token.semantics", "inclusive")),
				StartTimeUnixNano: uint64(1000 * (i + 1)), Count: 1, Sum: &sum, BucketCounts: []uint64{0, 1}, ExplicitBounds: []float64{1},
			})
		}
		for flush := range 2 {
			flushed := make([]*metricpb.HistogramDataPoint, len(points))
			for k, point := range points {
				flushed[k] = proto.Clone(point).(*metricpb.HistogramDataPoint)
				flushed[k].TimeUnixNano = flushed[k].StartTimeUnixNano + uint64(flush+1)
			}
			metrics = append(metrics, &colmetricpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{agentOTLPString("service.name", "agento11y")}},
				ScopeMetrics: []*metricpb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "github.com/grafana/agento11y/go/agento11y"}, Metrics: []*metricpb.Metric{
					{Name: "gen_ai.client.operation.duration", Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE}}},
					{Name: "gen_ai.client.token.usage", Unit: "{token}", Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{
						AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: flushed,
					}}},
				}}},
			}}})
		}
	}
	traces := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}}
	return agentOTLPFixtureData{traces: traces, metrics: metrics, generations: generations}
}

func (f agentOTLPFixtureData) spans() []*tracepb.Span {
	return f.traces.ResourceSpans[0].ScopeSpans[0].Spans
}

func (f agentOTLPFixtureData) span(key, value string) *tracepb.Span {
	for _, span := range f.spans() {
		if agentOTLPAttrs(span.Attributes)[key] == value {
			return span
		}
	}
	panic("fixture has no span with " + key + "=" + value)
}

func (f agentOTLPFixtureData) points() []*metricpb.HistogramDataPoint {
	var points []*metricpb.HistogramDataPoint
	for _, request := range f.metrics {
		points = append(points, f.tokenHistogram(request).DataPoints...)
	}
	return points
}

func (agentOTLPFixtureData) tokenHistogram(request *colmetricpb.ExportMetricsServiceRequest) *metricpb.Histogram {
	return request.ResourceMetrics[0].ScopeMetrics[0].Metrics[1].GetHistogram()
}

// setAgentOTLPAttr replaces key, appending it when absent. A nil value removes it.
func setAgentOTLPAttr(attrs *[]*commonpb.KeyValue, key string, value *commonpb.AnyValue) {
	*attrs = slices.DeleteFunc(*attrs, func(attr *commonpb.KeyValue) bool { return attr.Key == key })
	if value != nil {
		*attrs = append(*attrs, &commonpb.KeyValue{Key: key, Value: value})
	}
}

func agentOTLPStringValue(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func agentOTLPGenerationFixture(want agentExpectation) agentOTLPFixtureData {
	f := agentOTLPFixture(want)
	for _, g := range f.generations {
		span := f.span("agento11y.generation.id", g.Id)
		metadata, _ := protojson.Marshal(g.Metadata)
		tags, _ := json.Marshal(g.Tags)
		for _, attr := range span.Attributes {
			switch attr.Key {
			case "gen_ai.usage.cache_read_input_tokens":
				attr.Key = "gen_ai.usage.cache_read.input_tokens"
			case "gen_ai.usage.cache_write_input_tokens":
				attr.Key = "gen_ai.usage.cache_creation.input_tokens"
			case "gen_ai.usage.reasoning_tokens":
				attr.Key = "gen_ai.usage.reasoning.output_tokens"
			}
		}
		span.Attributes = append(span.Attributes,
			agentOTLPString("agento11y.record", "true"),
			agentOTLPString("agento11y.generation.metadata", string(metadata)),
			agentOTLPString("agento11y.generation.tags", string(tags)))
		if total := g.GetUsage().GetTotalTokens(); total != 0 {
			span.Attributes = append(span.Attributes, agentOTLPInt("agento11y.gen_ai.usage.total_tokens", total))
		}
		if !want.full() {
			continue
		}
		for key, messages := range map[string][]*agento11yv1.Message{"gen_ai.input.messages": g.Input, "gen_ai.output.messages": g.Output} {
			var wire []agentOTLPMessage
			for _, message := range messages {
				role := map[agento11yv1.MessageRole]string{
					agento11yv1.MessageRole_MESSAGE_ROLE_USER: "user", agento11yv1.MessageRole_MESSAGE_ROLE_ASSISTANT: "assistant", agento11yv1.MessageRole_MESSAGE_ROLE_TOOL: "tool",
				}[message.Role]
				m := agentOTLPMessage{Role: role}
				for _, part := range message.Parts {
					p := agentOTLPPart{}
					switch payload := part.Payload.(type) {
					case *agento11yv1.Part_Text:
						p.Type, p.Content = "text", payload.Text
					case *agento11yv1.Part_ToolCall:
						p.Type, p.ID, p.Name, p.Arguments = "tool_call", payload.ToolCall.Id, payload.ToolCall.Name, payload.ToolCall.InputJson
					case *agento11yv1.Part_ToolResult:
						result := payload.ToolResult
						p.Type, p.ID, p.IsError = "tool_call_response", result.ToolCallId, result.IsError
						p.Response = result.ContentJson
						if len(p.Response) == 0 {
							p.Response, _ = json.Marshal(result.Content)
						}
					}
					m.Parts = append(m.Parts, p)
				}
				wire = append(wire, m)
			}
			if len(wire) > 0 {
				raw, _ := json.Marshal(wire)
				span.Attributes = append(span.Attributes, agentOTLPString(key, string(raw)))
			}
		}
		if len(g.Tools) > 0 {
			span.Attributes = append(span.Attributes, agentOTLPString("gen_ai.tool.definitions", `[{"type":"function","name":"Bash"}]`))
		}
	}
	for _, point := range f.points() {
		if agentOTLPAttrs(point.Attributes)["gen_ai.token.type"] == "cache_write" {
			setAgentOTLPAttr(&point.Attributes, "gen_ai.token.type", agentOTLPStringValue("cache_creation"))
		}
	}
	return f
}

func TestAgentOTLPGenerationValidation(t *testing.T) {
	expectations := fixtureExpectations()
	for _, name := range slices.Sorted(maps.Keys(expectations)) {
		t.Run("valid/"+name, func(t *testing.T) {
			want := expectations[name]
			f := agentOTLPGenerationFixture(want)
			slices.Reverse(f.spans())
			capture := &agentOTLPCapture{traceReqs: []*coltracepb.ExportTraceServiceRequest{f.traces}, metricReqs: f.metrics}
			got, err := capture.validateGenerations(want)
			require.NoError(t, err)
			require.Len(t, got, len(f.generations))
			for i, g := range got {
				require.Equal(t, f.generations[i].Id, g.Id)
				require.Equal(t, f.generations[i].TraceId, g.TraceId)
				require.Equal(t, f.generations[i].SpanId, g.SpanId)
				require.Equal(t, f.generations[i].ConversationId, g.ConversationId)
			}
			if want.mode == "metadata_only" {
				want.mode = "default"
				defaultRefs, err := capture.validateGenerations(want)
				require.NoError(t, err)
				require.Equal(t, got, defaultRefs)
			}
		})
	}
	for _, agent := range []string{"claude-code", "codex"} {
		for _, mode := range []string{"full", "metadata_only", "default"} {
			for _, tc := range []struct {
				name, key, value, wantErr string
				fullOnly                  bool
			}{
				{name: "routing marker", key: "agento11y.record", wantErr: "routing marker"},
				{name: "usage", key: "gen_ai.usage.input_tokens", value: "wrong", wantErr: "want an integer"},
				{name: "cache read usage", key: "gen_ai.usage.cache_read.input_tokens", value: "1", wantErr: "usage attribute gen_ai.usage.cache_read.input_tokens"},
				{name: "cache write usage", key: "gen_ai.usage.cache_creation.input_tokens", value: "1", wantErr: "usage attribute gen_ai.usage.cache_creation.input_tokens"},
				{name: "reasoning usage", key: "gen_ai.usage.reasoning.output_tokens", value: "1", wantErr: "usage attribute gen_ai.usage.reasoning.output_tokens"},
				{name: "total usage", key: "agento11y.gen_ai.usage.total_tokens", wantErr: "total usage"},
				{name: "capture mode", key: "agento11y.generation.metadata", value: `{}`, wantErr: "capture mode"},
				{name: "tags", key: "agento11y.generation.tags", value: `{}`, wantErr: "generation tag"},
				{name: "conversation", key: "gen_ai.conversation.id", wantErr: "conversation ID"},
				{name: "secret", key: "custom", value: secretReplyToken, wantErr: "ordinary content"},
				{name: "malformed messages", key: "gen_ai.input.messages", value: "[", wantErr: "decode gen_ai.input.messages", fullOnly: true},
				{name: "tool result", key: "gen_ai.input.messages", value: `[{"role":"tool","parts":[{"type":"tool_call_response","id":"call_fake","response":"wrong"}]}]`, wantErr: "generation tool result", fullOnly: true},
				{name: "invalid base64", key: "gen_ai.input.messages", value: `[{"role":"tool","parts":[{"type":"tool_call_response","id":"call_fake","response":"hello-from-fake","agento11y.response_b64":"%"}]}]`, wantErr: "base64", fullOnly: true},
				{name: "wrong encoded result", key: "gen_ai.input.messages", value: `[{"role":"tool","parts":[{"type":"tool_call_response","id":"call_fake","response":"hello-from-fake","agento11y.response_b64":"Indyb25nIg=="}]}]`, wantErr: "generation tool result", fullOnly: true},
				{name: "duplicate generation", wantErr: "duplicate generation ID"},
				{name: "duplicate span ID", wantErr: "duplicate generation trace/span ID"},
				{name: "missing timestamps", wantErr: "timestamps"},
				{name: "reversed timestamps", wantErr: "timestamps"},
				{name: "tool parent", wantErr: "companion tool calls"},
				{name: "missing metrics", wantErr: "token usage count/sum"},
			} {
				if tc.fullOnly && mode != "full" {
					continue
				}
				t.Run(agent+"/"+mode+"/"+tc.name, func(t *testing.T) {
					captureMode := mode
					if mode == "default" {
						captureMode = "metadata_only"
					}
					want := expectations[agent+"/"+captureMode+"/baseline"]
					want.conversation = "fixture-session"
					f := agentOTLPGenerationFixture(want)
					span := f.span("agento11y.generation.id", f.generations[0].Id)
					if tc.key != "" {
						var value *commonpb.AnyValue
						if tc.value != "" {
							value = agentOTLPStringValue(tc.value)
							if tc.value == "1" {
								value = agentOTLPInt(tc.key, 1).Value
							}
						}
						if tc.fullOnly {
							span = f.span("agento11y.generation.id", f.generations[len(f.generations)-1].Id)
						}
						setAgentOTLPAttr(&span.Attributes, tc.key, value)
					} else {
						switch tc.name {
						case "duplicate generation":
							scope := f.traces.ResourceSpans[0].ScopeSpans[0]
							scope.Spans = append(scope.Spans, proto.Clone(span).(*tracepb.Span))
						case "duplicate span ID":
							other := proto.Clone(span).(*tracepb.Span)
							setAgentOTLPAttr(&other.Attributes, "agento11y.generation.id", agentOTLPStringValue("other"))
							scope := f.traces.ResourceSpans[0].ScopeSpans[0]
							scope.Spans = append(scope.Spans, other)
						case "missing timestamps":
							span.StartTimeUnixNano = 0
						case "reversed timestamps":
							span.EndTimeUnixNano = 1
						case "tool parent":
							f.span("gen_ai.tool.call.id", fakeToolCallID).ParentSpanId = nil
						case "missing metrics":
							f.metrics = nil
						}
					}
					want.mode = mode
					capture := &agentOTLPCapture{traceReqs: []*coltracepb.ExportTraceServiceRequest{f.traces}, metricReqs: f.metrics}
					_, err := capture.validateGenerations(want)
					require.ErrorContains(t, err, tc.wantErr)
				})
			}
		}
	}
}

func TestAgentOTLPGenerationValidationWire(t *testing.T) {
	for _, tc := range []struct {
		name, contentJSON string
		isError, wantErr  bool
	}{
		{name: "object result", contentJSON: `{"output":"hello-from-fake\n"}`},
		{name: "string result", contentJSON: `"hello-from-fake\n"`},
		{name: "formatted object result", contentJSON: `{ "output": "hello-from-fake\n" }`},
		{name: "wrong result", contentJSON: `"wrong"`, wantErr: true},
		{name: "invalid JSON result", contentJSON: fakeToolOutput, wantErr: true},
		{name: "missing result", wantErr: true},
		{name: "failed result", contentJSON: `"hello-from-fake"`, isError: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAgentEnvironment(t)
			envconfig.PinAliasEnvBlank(t)
			capture := newAgentOTLPCapture(t)
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", capture.server.URL)
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
			providers, err := otel.Setup(t.Context(), "fixture-session")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, providers.Shutdown(context.Background())) })
			client, err := emit.NewClient(emit.ClientOptions{
				Providers: providers, ContentCapture: agento11y.ContentCaptureModeFull, UserID: fixtureEmail,
				Tags: map[string]string{"user": fixtureEmail, "repo": fixtureRepo, "git.branch": fixtureBranch},
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Shutdown(context.Background())) })
			call := shellCall{id: fakeToolCallID, marker: fakeToolOutput}
			want := agentExpectation{agent: "codex", mode: "full", conversation: "fixture-session", generations: []wantGeneration{{
				prompt: "wire-prompt-marker", text: "Done.", calls: []shellCall{call}, results: []shellCall{call},
				usage: wantUsage{input: 20, output: 10, cacheRead: 3, cacheWrite: 2, reasoning: 4},
			}}}
			require.NoError(t, emit.Record(t.Context(), client, agento11y.GenerationStart{
				ID: "fixture-generation", ConversationID: "fixture-session", AgentName: "codex",
				Model: agento11y.ModelRef{Provider: "openai", Name: fakeModelName}, StartedAt: time.Now().Add(-time.Second),
			}, agento11y.Generation{
				Input: []agento11y.Message{
					agento11y.UserTextMessage("wire-prompt-marker"),
					{Role: agento11y.RoleTool, Parts: []agento11y.Part{agento11y.ToolResultPart(agento11y.ToolResult{
						ToolCallID: fakeToolCallID, ContentJSON: []byte(tc.contentJSON), IsError: tc.isError,
					})}},
				},
				Output: []agento11y.Message{
					{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{agento11y.ToolCallPart(agento11y.ToolCall{
						ID: fakeToolCallID, Name: "Bash", InputJSON: []byte(`{"cmd":"echo hello-from-fake"}`),
					})}},
					agento11y.AssistantTextMessage("Done."),
				},
				Tools: []agento11y.ToolDefinition{{Name: "Bash", Type: "function"}},
				Usage: agento11y.TokenUsage{
					InputTokens: 20, OutputTokens: 10, TotalTokens: 30, CacheReadInputTokens: 3, CacheWriteInputTokens: 2, ReasoningTokens: 4,
					InputSemantics: agento11y.TokenInputSemanticsInclusive,
				},
				CompletedAt: time.Now(),
			}, nil, func(ctx context.Context) {
				_, recorder := client.StartToolExecution(ctx, agento11y.ToolExecutionStart{
					ToolName: "Bash", ToolCallID: fakeToolCallID, ToolType: "function", ConversationID: "fixture-session",
					AgentName: "codex", RequestModel: fakeModelName, RequestProvider: "openai", StartedAt: time.Now(),
				})
				recorder.SetResult(agento11y.ToolExecutionEnd{Arguments: map[string]string{"cmd": "echo " + fakeToolOutput}, Result: fakeToolOutput, CompletedAt: time.Now()})
				recorder.End()
				require.NoError(t, recorder.Err())
			}))
			require.NoError(t, client.Flush(t.Context()))
			require.NoError(t, providers.ForceFlush())
			_, err = capture.validateGenerations(want)
			if tc.wantErr {
				require.ErrorContains(t, err, "generation tool result")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAgentOTLPValidate(t *testing.T) {
	expectations := fixtureExpectations()
	for _, name := range slices.Sorted(maps.Keys(expectations)) {
		t.Run("valid/"+name, func(t *testing.T) {
			want := expectations[name]
			f := agentOTLPFixture(want)
			capture := &agentOTLPCapture{traceReqs: []*coltracepb.ExportTraceServiceRequest{f.traces}, metricReqs: f.metrics}
			require.NoError(t, capture.validate(want, f.generations, false))
		})
	}

	type fixture = agentOTLPFixtureData
	generationSpan := func(f fixture, i int) *tracepb.Span { return f.span("agento11y.generation.id", f.generations[i].Id) }
	toolSpan := func(f fixture, id string) *tracepb.Span { return f.span("gen_ai.tool.call.id", id) }
	pointOf := func(f fixture, tokenType string) *metricpb.HistogramDataPoint {
		for _, point := range f.points() {
			if agentOTLPAttrs(point.Attributes)["gen_ai.token.type"] == tokenType {
				return point
			}
		}
		panic("fixture has no " + tokenType + " point")
	}
	dropPoints := func(f *fixture, tokenType string) {
		for _, request := range f.metrics {
			histogram := f.tokenHistogram(request)
			histogram.DataPoints = slices.DeleteFunc(histogram.DataPoints, func(point *metricpb.HistogramDataPoint) bool {
				return agentOTLPAttrs(point.Attributes)["gen_ai.token.type"] == tokenType
			})
		}
	}
	for _, tc := range []struct {
		name    string
		cases   []string
		mutate  func(*fixture, agentExpectation)
		wantErr string // empty: the mutation is valid
	}{
		{name: "single flush", cases: []string{"claude-code/metadata_only/multi_tool", "codex/full/resume"}, mutate: func(f *fixture, want agentExpectation) {
			f.metrics = slices.DeleteFunc(f.metrics, func(r *colmetricpb.ExportMetricsServiceRequest) bool {
				return f.tokenHistogram(r).DataPoints[0].TimeUnixNano%2 == 0
			})
		}},
		{name: "reordered attributes in later flush", cases: []string{"claude-code/metadata_only/nonzero_usage", "codex/metadata_only/nonzero_usage"}, mutate: func(f *fixture, want agentExpectation) {
			for _, point := range f.tokenHistogram(f.metrics[1]).DataPoints {
				slices.Reverse(point.Attributes)
			}
		}},
		{name: "one lifetime for every generation", cases: []string{"claude-code/metadata_only/resume"}, mutate: func(f *fixture, want agentExpectation) {
			merged := map[string]*metricpb.HistogramDataPoint{}
			for _, point := range f.points() {
				if point.TimeUnixNano != point.StartTimeUnixNano+2 {
					continue
				}
				tokenType := agentOTLPAttrs(point.Attributes)["gen_ai.token.type"]
				if m, ok := merged[tokenType]; ok {
					m.Count += point.Count
					*m.Sum += point.GetSum()
					m.BucketCounts[1] += point.BucketCounts[1]
					continue
				}
				m := proto.Clone(point).(*metricpb.HistogramDataPoint)
				m.StartTimeUnixNano, m.TimeUnixNano = 1, 9
				merged[tokenType] = m
			}
			request := f.metrics[0]
			f.tokenHistogram(request).DataPoints = slices.Collect(maps.Values(merged))
			f.metrics = []*colmetricpb.ExportMetricsServiceRequest{request}
		}},
		{name: "Codex failed command status", cases: []string{"codex/full/failed_command"}, mutate: func(f *fixture, want agentExpectation) {
			toolSpan(*f, "call_fail").Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
		}},
		{name: "missing timestamps", mutate: func(f *fixture, want agentExpectation) {
			generationSpan(*f, 0).StartTimeUnixNano = 0
		}, wantErr: "timestamps"},
		{name: "reversed timestamps", mutate: func(f *fixture, want agentExpectation) {
			generationSpan(*f, 0).EndTimeUnixNano = 1
		}, wantErr: "timestamps"},
		{name: "duplicate generation", mutate: func(f *fixture, want agentExpectation) {
			scope := f.traces.ResourceSpans[0].ScopeSpans[0]
			scope.Spans = append(scope.Spans, proto.Clone(generationSpan(*f, 0)).(*tracepb.Span))
		}, wantErr: "duplicate generation ID"},
		{name: "missing generation", mutate: func(f *fixture, want agentExpectation) {
			scope, drop := f.traces.ResourceSpans[0].ScopeSpans[0], generationSpan(*f, 0)
			scope.Spans = slices.DeleteFunc(scope.Spans, func(span *tracepb.Span) bool { return span == drop })
		}, wantErr: "span counts"},
		{name: "extra generation for terminal failure", cases: []string{"claude-code/metadata_only/model_error_400", "codex/full/model_error_400"}, mutate: func(f *fixture, want agentExpectation) {
			other := agentOTLPFixture(expectations[want.agent+"/full/text_only"])
			f.traces.ResourceSpans[0].ScopeSpans[0].Spans = other.spans()
		}, wantErr: "generation ID"},
		{name: "token usage for terminal failure", cases: []string{"claude-code/metadata_only/model_error_400", "codex/metadata_only/model_error_400"}, mutate: func(f *fixture, want agentExpectation) {
			f.metrics = agentOTLPFixture(expectations[want.agent+"/metadata_only/text_only"]).metrics
		}, wantErr: "unexpected token type"},
		{name: "wrong last span tag", mutate: func(f *fixture, want agentExpectation) {
			spans := f.spans()
			setAgentOTLPAttr(&spans[len(spans)-1].Attributes, "agento11y.tag.repo", agentOTLPStringValue("wrong"))
		}, wantErr: "agento11y.tag.repo"},
		{name: "duplicate attribute", mutate: func(f *fixture, want agentExpectation) {
			span := generationSpan(*f, 0)
			span.Attributes = append(span.Attributes, span.Attributes[0])
		}, wantErr: "duplicate attribute"},
		{name: "wrong identity", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&generationSpan(*f, 0).Attributes, "user.id", agentOTLPStringValue("wrong"))
		}, wantErr: "identity"},
		{name: "model tool name", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&toolSpan(*f, fakeToolCallID).Attributes, "gen_ai.tool.name", agentOTLPStringValue("exec_command"))
		}, wantErr: "gen_ai.tool.name"},
		{name: "wrong tool call ID", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&toolSpan(*f, fakeToolCallID).Attributes, "gen_ai.tool.call.id", agentOTLPStringValue("wrong"))
		}, wantErr: "tool span \"wrong\""},
		{name: "duplicate tool span", cases: []string{"claude-code/full/multi_tool", "codex/metadata_only/multi_tool"}, mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&toolSpan(*f, "call_multi_b").Attributes, "gen_ai.tool.call.id", agentOTLPStringValue("call_multi_a"))
		}, wantErr: "duplicate tool span"},
		{name: "missing tool", cases: []string{"claude-code/metadata_only/multi_tool", "codex/full/multi_tool"}, mutate: func(f *fixture, want agentExpectation) {
			scope, drop := f.traces.ResourceSpans[0].ScopeSpans[0], toolSpan(*f, "call_multi_c")
			scope.Spans = slices.DeleteFunc(scope.Spans, func(span *tracepb.Span) bool { return span == drop })
		}, wantErr: "span counts"},
		{name: "missing generation ID", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&generationSpan(*f, 0).Attributes, "agento11y.generation.id", nil)
		}, wantErr: "generation ID"},
		{name: "generation span not exported over HTTP", mutate: func(f *fixture, want agentExpectation) {
			generationSpan(*f, 0).SpanId = bytes.Repeat([]byte{9}, 8)
		}, wantErr: "want HTTP export"},
		{name: "generation trace not exported over HTTP", mutate: func(f *fixture, want agentExpectation) {
			span := generationSpan(*f, 0)
			span.TraceId = bytes.Repeat([]byte{9}, 16)
		}, wantErr: "want HTTP export"},
		{name: "tool parent from another generation", cases: []string{"claude-code/metadata_only/multi_tool", "claude-code/full/resume", "codex/metadata_only/resume"}, mutate: func(f *fixture, want agentExpectation) {
			last := want.generations[len(want.generations)-1]
			if len(last.calls) == 0 {
				last = want.generations[len(want.generations)-2]
			}
			toolSpan(*f, last.calls[len(last.calls)-1].id).ParentSpanId = generationSpan(*f, 0).SpanId
		}, wantErr: "parent"},
		{name: "unparented tool", mutate: func(f *fixture, want agentExpectation) { toolSpan(*f, fakeToolCallID).ParentSpanId = nil }, wantErr: "parent"},
		{name: "tool in another trace", mutate: func(f *fixture, want agentExpectation) {
			toolSpan(*f, fakeToolCallID).TraceId = bytes.Repeat([]byte{9}, 16)
		}, wantErr: "parent"},
		{name: "tool conversation", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&toolSpan(*f, fakeToolCallID).Attributes, "gen_ai.conversation.id", agentOTLPStringValue("another-session"))
		}, wantErr: "gen_ai.conversation.id"},
		{name: "generation conversation", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&generationSpan(*f, 0).Attributes, "gen_ai.conversation.id", nil)
		}, wantErr: "gen_ai.conversation.id"},
		{name: "string usage", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&generationSpan(*f, 0).Attributes, "gen_ai.usage.input_tokens", agentOTLPStringValue("10"))
		}, wantErr: "want an integer"},
		{name: "wrong usage", cases: []string{"claude-code/metadata_only/nonzero_usage", "codex/full/nonzero_usage"}, mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&generationSpan(*f, 0).Attributes, "gen_ai.usage.output_tokens", agentOTLPInt("gen_ai.usage.output_tokens", 1).Value)
		}, wantErr: "usage attribute gen_ai.usage.output_tokens"},
		{name: "missing cache read", cases: []string{"claude-code/metadata_only/nonzero_usage", "codex/metadata_only/nonzero_usage"}, mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&generationSpan(*f, 0).Attributes, "gen_ai.usage.cache_read_input_tokens", nil)
		}, wantErr: "cache_read_input_tokens"},
		{name: "zero usage attribute", cases: []string{"claude-code/metadata_only/nonzero_usage"}, mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&generationSpan(*f, 0).Attributes, "gen_ai.usage.reasoning_tokens", agentOTLPInt("", 0).Value)
		}, wantErr: "reasoning_tokens"},
		{name: "missing semantics", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&generationSpan(*f, 0).Attributes, "gen_ai.token.semantics", nil)
		}, wantErr: "gen_ai.token.semantics"},
		{name: "failed tool span OK", cases: []string{"claude-code/full/failed_command", "claude-code/metadata_only/failed_command"}, mutate: func(f *fixture, want agentExpectation) {
			toolSpan(*f, "call_fail").Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
		}, wantErr: "error status = false, want true"},
		{name: "successful tool span error", cases: []string{"claude-code/full/failed_command", "codex/full/failed_command"}, mutate: func(f *fixture, want agentExpectation) {
			toolSpan(*f, "call_recover").Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
		}, wantErr: "error status = true, want false"},
		{name: "missing argument redaction marker", cases: []string{"claude-code/full/secrets", "codex/full/secrets"}, mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&toolSpan(*f, "call_secret").Attributes, "gen_ai.tool.call.arguments", agentOTLPStringValue(`{"command":"secret-argument-marker"}`))
		}, wantErr: "lacks"},
		{name: "missing result", cases: []string{"claude-code/full/baseline", "codex/full/multi_tool"}, mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&f.spans()[1].Attributes, "gen_ai.tool.call.result", nil)
		}, wantErr: "gen_ai.tool.call.result"},
		{name: "raw argument secret", cases: []string{"claude-code/full/secrets", "codex/full/secrets"}, mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&toolSpan(*f, "call_secret").Attributes, "gen_ai.tool.call.arguments", agentOTLPStringValue(`{"command":"secret-argument-marker [REDACTED:grafana-cloud-token] `+secretArgumentToken+`"}`))
		}, wantErr: "ordinary content"},
		{name: "nested JSON output secret", cases: []string{"claude-code/full/secrets", "codex/full/secrets"}, mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&toolSpan(*f, "call_secret").Attributes, "gen_ai.tool.call.result", agentOTLPStringValue(`secret-output-marker [REDACTED:grafana-service-account-token]`))
			span := toolSpan(*f, "call_secret")
			span.Events = []*tracepb.Span_Event{{Name: "custom", Attributes: []*commonpb.KeyValue{agentOTLPString("payload", `{"nested":["`+strings.ReplaceAll(secretOutputToken, "_", `\u005f`)+`"]}`)}}}
		}, wantErr: "ordinary content"},
		{name: "base64 reply secret", cases: []string{"claude-code/full/secrets", "codex/full/secrets"}, mutate: func(f *fixture, want agentExpectation) {
			generationSpan(*f, 0).Attributes = append(generationSpan(*f, 0).Attributes, &commonpb.KeyValue{Key: "custom", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte(base64.StdEncoding.EncodeToString([]byte(secretReplyToken)))}}})
		}, wantErr: "ordinary content"},
		{name: "metric secret", cases: []string{"claude-code/full/secrets"}, mutate: func(f *fixture, want agentExpectation) {
			f.metrics[0].ResourceMetrics[0].Resource.Attributes = append(f.metrics[0].ResourceMetrics[0].Resource.Attributes, agentOTLPString("custom", secretReplyToken))
		}, wantErr: "metric export"},
		{name: "metadata tool content", mutate: func(f *fixture, want agentExpectation) {
			toolSpan(*f, fakeToolCallID).Attributes = append(toolSpan(*f, fakeToolCallID).Attributes, &commonpb.KeyValue{Key: "gen_ai.tool.call.arguments"})
		}, wantErr: "content attribute"},
		{name: "metadata marker", cases: []string{"claude-code/metadata_only/secrets", "codex/metadata_only/multi_tool"}, mutate: func(f *fixture, want agentExpectation) {
			generationSpan(*f, 0).Events = []*tracepb.Span_Event{{Name: "secret-reply-marker multi-output-b"}}
		}, wantErr: "ordinary content"},
		{name: "resource leak", mutate: func(f *fixture, want agentExpectation) {
			f.traces.ResourceSpans[0].Resource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{agentOTLPString("custom", fakeToolOutput)}}
		}, wantErr: "ordinary content"},
		{name: "missing metrics", mutate: func(f *fixture, want agentExpectation) { f.metrics = nil }, wantErr: "token usage count/sum"},
		{name: "wrong last datapoint tag", mutate: func(f *fixture, want agentExpectation) {
			points := f.points()
			setAgentOTLPAttr(&points[len(points)-1].Attributes, "agento11y.tag.git.branch", agentOTLPStringValue("wrong"))
		}, wantErr: "agento11y.tag.git.branch"},
		{name: "wrong datapoint agent", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&pointOf(*f, "output").Attributes, "gen_ai.agent.name", agentOTLPStringValue("wrong"))
		}, wantErr: "gen_ai.agent.name"},
		{name: "wrong datapoint model", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&pointOf(*f, "output").Attributes, "gen_ai.request.model", agentOTLPStringValue("wrong"))
		}, wantErr: "gen_ai.request.model"},
		{name: "missing datapoint semantics", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&pointOf(*f, "input").Attributes, "gen_ai.token.semantics", nil)
		}, wantErr: "semantics"},
		{name: "zero datapoint count", mutate: func(f *fixture, want agentExpectation) { pointOf(*f, "output").Count = 0 }, wantErr: "count/sum"},
		{name: "missing datapoint start", mutate: func(f *fixture, want agentExpectation) { pointOf(*f, "output").StartTimeUnixNano = 0 }, wantErr: "output token usage timestamps"},
		{name: "datapoint before its start", mutate: func(f *fixture, want agentExpectation) {
			point := pointOf(*f, "input")
			point.TimeUnixNano = point.StartTimeUnixNano - 1
		}, wantErr: "input token usage timestamps"},
		{name: "unexpected token type", mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&pointOf(*f, "output").Attributes, "gen_ai.token.type", agentOTLPStringValue("wrong"))
		}, wantErr: "token type"},
		{name: "reasoning bucket for Claude", cases: []string{"claude-code/metadata_only/nonzero_usage"}, mutate: func(f *fixture, want agentExpectation) {
			setAgentOTLPAttr(&pointOf(*f, "cache_write").Attributes, "gen_ai.token.type", agentOTLPStringValue("reasoning"))
		}, wantErr: "unexpected token type \"reasoning\""},
		{name: "missing output observations", mutate: func(f *fixture, want agentExpectation) { dropPoints(f, "output") }, wantErr: "output token usage"},
		{name: "missing cache read bucket", cases: []string{"claude-code/metadata_only/nonzero_usage", "codex/full/resume"}, mutate: func(f *fixture, want agentExpectation) { dropPoints(f, "cache_read") }, wantErr: "cache_read token usage"},
		{name: "missing reasoning bucket", cases: []string{"codex/metadata_only/nonzero_usage"}, mutate: func(f *fixture, want agentExpectation) { dropPoints(f, "reasoning") }, wantErr: "reasoning token usage"},
		{name: "wrong token sum", mutate: func(f *fixture, want agentExpectation) {
			for _, point := range f.points() {
				if agentOTLPAttrs(point.Attributes)["gen_ai.token.type"] == "output" {
					*point.Sum++
				}
			}
		}, wantErr: "count/sum"},
		{name: "swapped generation values", cases: []string{"claude-code/metadata_only/nonzero_usage"}, mutate: func(f *fixture, want agentExpectation) {
			// Both lifetimes claim the first generation's output, so the sum is 10, not 14.
			for _, point := range f.points() {
				if agentOTLPAttrs(point.Attributes)["gen_ai.token.type"] == "output" {
					*point.Sum = 5
				}
			}
		}, wantErr: "output token usage count/sum"},
		{name: "bucket counts disagree", mutate: func(f *fixture, want agentExpectation) { pointOf(*f, "input").BucketCounts = []uint64{0, 2} }, wantErr: "buckets hold"},
		{name: "not a histogram", mutate: func(f *fixture, want agentExpectation) {
			f.metrics[0].ResourceMetrics[0].ScopeMetrics[0].Metrics[1].Data = &metricpb.Metric_Sum{Sum: &metricpb.Sum{}}
		}, wantErr: "histogram datapoints"},
		{name: "delta temporality", mutate: func(f *fixture, want agentExpectation) {
			f.tokenHistogram(f.metrics[1]).AggregationTemporality = metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
		}, wantErr: "temporality"},
		{name: "unspecified temporality", mutate: func(f *fixture, want agentExpectation) {
			f.tokenHistogram(f.metrics[0]).AggregationTemporality = metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_UNSPECIFIED
		}, wantErr: "temporality"},
		{name: "decreasing cumulative snapshot", mutate: func(f *fixture, want agentExpectation) {
			for _, point := range f.tokenHistogram(f.metrics[0]).DataPoints {
				point.Count, point.BucketCounts = 2, []uint64{0, 2}
			}
		}, wantErr: "not cumulative"},
		{name: "conflicting snapshots at one time", mutate: func(f *fixture, want agentExpectation) {
			for _, point := range f.tokenHistogram(f.metrics[1]).DataPoints {
				point.TimeUnixNano--
				*point.Sum++
			}
		}, wantErr: "not cumulative"},
	} {
		cases := tc.cases
		if cases == nil {
			cases = []string{"claude-code/metadata_only/baseline", "codex/metadata_only/baseline"}
		}
		for _, key := range cases {
			t.Run(tc.name+"/"+key, func(t *testing.T) {
				want, ok := expectations[key]
				require.True(t, ok, key)
				f := agentOTLPFixture(want)
				tc.mutate(&f, want)
				capture := &agentOTLPCapture{traceReqs: []*coltracepb.ExportTraceServiceRequest{f.traces}, metricReqs: f.metrics}
				err := capture.validate(want, f.generations, false)
				if tc.wantErr == "" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tc.wantErr)
				}
			})
		}
	}
}
