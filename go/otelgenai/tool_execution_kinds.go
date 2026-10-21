package otelgenai

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// These additions follow merged GenAI #498 and are still development conventions.
const (
	genAISkillNameKey         attribute.Key = "gen_ai.skill.name"
	genAISkillDescriptionKey  attribute.Key = "gen_ai.skill.description"
	genAISkillSourceURIKey    attribute.Key = "gen_ai.skill.source.uri"
	genAISkillResourceNameKey attribute.Key = "gen_ai.skill.resource.name"
	processExecutableNameKey  attribute.Key = "process.executable.name"
	processExecutablePathKey  attribute.Key = "process.executable.path"
	processExitCodeKey        attribute.Key = "process.exit.code"
)

func (inv *Invocation) toolExecutionKind() ToolExecutionKind {
	if inv.operation() == OperationExecuteTool {
		switch inv.ExecutionKind {
		case ToolExecutionKindLoadSkill, ToolExecutionKindReadSkillResource, ToolExecutionKindCommand:
			return inv.ExecutionKind
		}
	}
	return ""
}

func (inv *Invocation) toolName() string {
	if inv.toolExecutionKind() != "" {
		return strings.TrimSpace(inv.ToolName)
	}
	return inv.ToolName
}

func (inv *Invocation) toolExecutionKindSpanName() string {
	parts := []string{string(OperationExecuteTool)}
	appendNonblank := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	appendNonblank(inv.ToolName)
	switch inv.toolExecutionKind() {
	case ToolExecutionKindLoadSkill:
		appendNonblank(inv.SkillName)
	case ToolExecutionKindReadSkillResource, ToolExecutionKindCommand:
		if strings.TrimSpace(inv.SkillResourceName) != "" {
			appendNonblank(inv.SkillName)
			appendNonblank(inv.SkillResourceName)
		} else if inv.toolExecutionKind() == ToolExecutionKindCommand {
			appendNonblank(inv.ProcessExecutableName)
		}
	}
	return strings.Join(parts, " ")
}

func appendTrimmedAttribute(attrs []attribute.KeyValue, key attribute.Key, value string) []attribute.KeyValue {
	if value = strings.TrimSpace(value); value != "" {
		attrs = append(attrs, key.String(value))
	}
	return attrs
}

func (inv *Invocation) toolIdentityAttributes() []attribute.KeyValue {
	executionKind := inv.toolExecutionKind()
	if executionKind == "" {
		return nil
	}
	attrs := appendTrimmedAttribute(nil, genAISkillNameKey, inv.SkillName)
	if executionKind != ToolExecutionKindLoadSkill {
		attrs = appendTrimmedAttribute(attrs, genAISkillResourceNameKey, inv.SkillResourceName)
	}
	if executionKind == ToolExecutionKindCommand {
		attrs = appendTrimmedAttribute(attrs, processExecutableNameKey, inv.ProcessExecutableName)
	}
	return attrs
}

func (inv *Invocation) toolPrivateAttributes() []attribute.KeyValue {
	executionKind := inv.toolExecutionKind()
	if executionKind == "" {
		return nil
	}
	attrs := appendTrimmedAttribute(nil, genAISkillDescriptionKey, inv.SkillDescription)
	if uri := scrubSkillSourceURI(inv.SkillSourceURI); uri != "" {
		attrs = append(attrs, genAISkillSourceURIKey.String(uri))
	}
	if executionKind == ToolExecutionKindCommand {
		attrs = appendTrimmedAttribute(attrs, processExecutablePathKey, inv.ProcessExecutablePath)
	}
	return attrs
}

func scrubSkillSourceURI(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return ""
	}
	// net/url does not validate escapes in the query, even though it is discarded below.
	if _, err := url.PathUnescape(raw); err != nil {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return ""
	}
	// A source must be an absolute location, not just a scheme or relative path.
	if strings.EqualFold(u.Scheme, "file") {
		if !strings.HasPrefix(u.Path, "/") {
			return ""
		}
		u.Scheme = "file"
		u.OmitHost = false
	} else if u.Host == "" {
		return ""
	}
	if u.Host != "" {
		if u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
			return ""
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 0 || n > 65535 {
				return ""
			}
		}
	} else if u.User != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

type toolIdentity struct {
	operation     Operation
	executionKind ToolExecutionKind
	tool          string
	skill         string
	resource      string
	executable    string
}

func snapshotToolIdentity(inv *Invocation) *toolIdentity {
	executionKind := inv.toolExecutionKind()
	if executionKind == "" {
		return nil
	}
	identity := &toolIdentity{
		operation: inv.Operation, executionKind: executionKind,
		tool: strings.TrimSpace(inv.ToolName), skill: strings.TrimSpace(inv.SkillName),
	}
	if executionKind != ToolExecutionKindLoadSkill {
		identity.resource = strings.TrimSpace(inv.SkillResourceName)
	}
	if executionKind == ToolExecutionKindCommand {
		identity.executable = strings.TrimSpace(inv.ProcessExecutableName)
	}
	return identity
}

func (identity *toolIdentity) restore(inv *Invocation) {
	if identity == nil {
		return
	}
	report := func(field string) {
		otel.Handle(fmt.Errorf("otelgenai: cannot change tool execution field %s published at Start", field))
	}
	if inv.Operation != identity.operation {
		report("Operation")
		inv.Operation = identity.operation
	}
	if inv.ExecutionKind != identity.executionKind {
		report("ExecutionKind")
		inv.ExecutionKind = identity.executionKind
	}
	preserve := func(field string, target *string, published string) {
		if published != "" {
			if strings.TrimSpace(*target) != published {
				report(field)
			}
			*target = published
		}
	}
	preserve("ToolName", &inv.ToolName, identity.tool)
	preserve("SkillName", &inv.SkillName, identity.skill)
	preserve("SkillResourceName", &inv.SkillResourceName, identity.resource)
	preserve("ProcessExecutableName", &inv.ProcessExecutableName, identity.executable)
}
