package agento11y

import (
	"context"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Draft metric compatibility with GenAI PR #574 at
// fb436c99dbe30d445c1fa952d67fdb74ce35dd4d, not adoption of the whole proposal.
// Consumers must select this or the legacy duration, not sum both. Unlike the
// legacy metric, serialization failures are not execution errors, and nonzero
// command exits are errors unless the caller explicitly marks them benign.
const metricToolDuration = "gen_ai.execute_tool.duration"

func newToolDurationHistogram(meter metric.Meter) (metric.Float64Histogram, error) {
	return meter.Float64Histogram(metricToolDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of tool execution."),
		metric.WithExplicitBucketBoundaries(durationBucketsSeconds...),
	)
}

func (c *Client) recordToolMetrics(ctx context.Context, seed ToolExecutionStart, end ToolExecutionEnd, startedAt, completedAt time.Time, execErr error) {
	if c.instruments.toolDuration == nil || startedAt.IsZero() || completedAt.IsZero() {
		return
	}
	duration := completedAt.Sub(startedAt).Seconds()
	if duration < 0 {
		duration = 0
	}
	var attrs []attribute.KeyValue
	for _, kv := range metricIdentityAttributes(seed.RequestProvider, seed.RequestModel, seed.AgentName, seed.AgentVersion) {
		if kv.Value.AsString() != "" {
			attrs = append(attrs, kv)
		}
	}
	attrs = append(attrs, c.clientTagMetricAttributes()...)
	attrs = append(attrs, metricStringAttribute(spanAttrToolName, strings.TrimSpace(seed.ToolName)))
	if toolType := strings.TrimSpace(seed.ToolType); toolType != "" {
		attrs = append(attrs, metricStringAttribute("gen_ai.tool.type", toolType))
	}
	kind := "generic"
	if isToolExecutionKind(seed.ExecutionKind) {
		kind = string(seed.ExecutionKind)
		if name := strings.TrimSpace(end.VerifiedSkillName); name != "" {
			attrs = append(attrs, metricStringAttribute("gen_ai.skill.name", name))
		}
		if name := strings.TrimSpace(end.VerifiedSkillResourceName); seed.ExecutionKind != ToolExecutionKindLoadSkill && name != "" {
			attrs = append(attrs, metricStringAttribute("gen_ai.skill.resource.name", name))
		}
	}
	attrs = append(attrs, attribute.String("agento11y.tool.execution.kind", kind))
	errorType := ""
	if execErr != nil {
		errorType = "tool_execution_error"
	}
	if seed.ExecutionKind == ToolExecutionKindCommand && end.ProcessExitCode != nil {
		code := *end.ProcessExitCode
		attrs = append(attrs, attribute.Int64("process.exit.code", code))
		if errorType == "" && code != 0 && (end.ProcessExitCodeIsError == nil || *end.ProcessExitCodeIsError) {
			errorType = strconv.FormatInt(code, 10)
		}
	}
	if errorType != "" {
		attrs = append(attrs, attribute.String(spanAttrErrorType, errorType))
	}
	c.instruments.toolDuration.Record(metricExemplarContext(ctx), duration, metric.WithAttributes(attrs...))
}
