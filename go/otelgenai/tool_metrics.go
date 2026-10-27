package otelgenai

import (
	"context"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Draft metric compatibility with GenAI PR #574 at
// fb436c99dbe30d445c1fa952d67fdb74ce35dd4d, not adoption of the whole proposal.
// Select this or the legacy client duration; summing both double-counts tools.
// Nonzero command exits affect only this metric, not legacy spans or metrics.
const metricToolDuration = "gen_ai.execute_tool.duration"

const toolExecutionKindKey attribute.Key = "agento11y.tool.execution.kind"

func newToolDurationHistogram(meter metric.Meter) (metric.Float64Histogram, error) {
	return meter.Float64Histogram(metricToolDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of tool execution."),
		metric.WithExplicitBucketBoundaries(durationBucketsSeconds...),
	)
}

func (inv *Invocation) snapshotToolCompletion() {
	if inv.ProcessExitCode != nil {
		inv.ProcessExitCode = new(*inv.ProcessExitCode)
	}
	if inv.ProcessExitCodeIsError != nil {
		inv.ProcessExitCodeIsError = new(*inv.ProcessExitCodeIsError)
	}
}

func reservedToolMetricKey(key attribute.Key) bool {
	switch key {
	case genAISkillNameKey, genAISkillResourceNameKey, toolExecutionKindKey,
		"error.type", processExitCodeKey, genAISkillDescriptionKey,
		genAISkillSourceURIKey, processExecutableNameKey, processExecutablePathKey,
		"gen_ai.tool.name", "gen_ai.tool.type", "gen_ai.tool.description",
		"gen_ai.conversation.id", "gen_ai.tool.call.id", "gen_ai.tool.call.arguments",
		"gen_ai.tool.call.result", "gen_ai.input.messages", "gen_ai.output.messages",
		"gen_ai.system_instructions":
		return true
	}
	return false
}

func toolMetricAttributes(inv *Invocation) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	for _, kv := range inv.MetricAttributes {
		if !reservedToolMetricKey(kv.Key) {
			attrs = append(attrs, kv)
		}
	}
	for _, field := range []struct {
		key   attribute.Key
		value string
	}{
		{"gen_ai.tool.name", inv.ToolName}, {"gen_ai.tool.type", inv.ToolType},
		{"gen_ai.agent.name", inv.AgentName}, {"gen_ai.agent.version", inv.AgentVersion},
		{"gen_ai.provider.name", inv.Provider}, {"gen_ai.request.model", inv.RequestModel},
		{"gen_ai.response.model", inv.ResponseModel},
	} {
		attrs = appendTrimmedAttribute(attrs, field.key, field.value)
	}
	kind := inv.toolExecutionKind()
	if kind == "" {
		attrs = append(attrs, toolExecutionKindKey.String("generic"))
	} else {
		attrs = append(attrs, toolExecutionKindKey.String(string(kind)))
		attrs = appendTrimmedAttribute(attrs, genAISkillNameKey, inv.VerifiedSkillName)
		if kind != ToolExecutionKindLoadSkill {
			attrs = appendTrimmedAttribute(attrs, genAISkillResourceNameKey, inv.VerifiedSkillResourceName)
		}
	}
	errorType := inv.errorType()
	if kind == ToolExecutionKindCommand && inv.ProcessExitCode != nil {
		code := *inv.ProcessExitCode
		attrs = append(attrs, processExitCodeKey.Int64(code))
		if errorType == "" && code != 0 && (inv.ProcessExitCodeIsError == nil || *inv.ProcessExitCodeIsError) {
			errorType = strconv.FormatInt(code, 10)
		}
	}
	if errorType != "" {
		attrs = append(attrs, attribute.String("error.type", errorType))
	}
	return attrs
}

func (i instruments) recordToolDuration(ctx context.Context, inv *Invocation) {
	if i.toolDuration == nil || inv.operation() != OperationExecuteTool || strings.TrimSpace(inv.ToolName) == "" {
		return
	}
	if duration, ok := inv.duration(); ok {
		i.toolDuration.Record(ctx, duration, metric.WithAttributes(toolMetricAttributes(inv)...))
	}
}
