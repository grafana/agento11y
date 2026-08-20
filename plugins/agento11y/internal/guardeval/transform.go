package guardeval

import (
	"encoding/json"
	"log"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/grafana/agento11y/go/agento11y"
)

type Transform struct {
	patterns []compiledPattern
}

type compiledPattern struct {
	re   *regexp.Regexp
	repl string
}

// ApplyTransform applies the compiled patterns to a deep clone of the input and
// reports whether any substitution occurred. It rewrites text parts,
// tool_call.input_json, tool_result content/content_json, system prompt,
// conversation preview, and tool descriptions/schemas. Thinking blocks are left
// untouched (signed model reasoning). logger may be nil.
//
// The third return value names the JSON fields whose rewrite was dropped
// because it would not have been valid JSON. The caller reports them: a
// silently dropped redaction leaves a secret in a call the rule claims to have
// cleaned.
func ApplyTransform(in agento11y.HookInput, ct *Transform, logger *log.Logger) (agento11y.HookInput, bool, []string) {
	return applyTransform(in, ct, logger, false)
}

// ApplyRelayTransform applies the same patterns to the copy sent to Cloud. It
// rewrites every payload field present on a part, including thinking and media
// content, regardless of the part kind. This copy never returns to a provider.
func ApplyRelayTransform(in agento11y.HookInput, ct *Transform, logger *log.Logger) (agento11y.HookInput, bool, []string) {
	return applyTransform(in, ct, logger, true)
}

func applyTransform(in agento11y.HookInput, ct *Transform, logger *log.Logger, relay bool) (agento11y.HookInput, bool, []string) {
	out := cloneHookInput(in)
	if ct == nil || len(ct.patterns) == 0 {
		return out, false, nil
	}
	changed := false
	var dropped []string
	if s, ch := applyString(out.SystemPrompt, ct); ch {
		out.SystemPrompt = s
		changed = true
	}
	if s, ch := applyString(out.ConversationPreview, ct); ch {
		out.ConversationPreview = s
		changed = true
	}
	for i := range out.Messages {
		ch, drops := transformMessage(&out.Messages[i], ct, logger, relay)
		changed = changed || ch
		dropped = append(dropped, drops...)
	}
	for i := range out.Output {
		ch, drops := transformMessage(&out.Output[i], ct, logger, relay)
		changed = changed || ch
		dropped = append(dropped, drops...)
	}
	for i := range out.Tools {
		td := &out.Tools[i]
		if s, ch := applyString(td.Description, ct); ch {
			td.Description = s
			changed = true
		}
		what := "tools[" + strconv.Itoa(i) + "].input_schema"
		ns, ch, drop := applyRawJSON(td.InputSchema, ct, logger, what)
		if ch {
			td.InputSchema = ns
			changed = true
		}
		if drop {
			dropped = append(dropped, what)
		}
	}
	return out, changed, dropped
}

func transformMessage(m *agento11y.Message, ct *Transform, logger *log.Logger, relay bool) (bool, []string) {
	changed := false
	var dropped []string
	for i := range m.Parts {
		p := &m.Parts[i]
		if relay {
			ch, drops := transformRelayPart(p, ct, logger)
			changed = changed || ch
			dropped = append(dropped, drops...)
			continue
		}
		switch p.Kind {
		case agento11y.PartKindText:
			if p.Text != "" {
				if ns, ch := applyString(p.Text, ct); ch {
					p.Text = ns
					changed = true
				}
			}
		case agento11y.PartKindThinking:
			// Skip: thinking blocks carry model reasoning, not user input, and
			// rewriting them invalidates the provider's signature.
		case agento11y.PartKindMedia:
			// Media fields are outside the transform's supported field set.
		case agento11y.PartKindToolCall:
			if p.ToolCall != nil {
				ns, ch, drop := applyRawJSON(p.ToolCall.InputJSON, ct, logger, "tool_call.input_json")
				if ch {
					p.ToolCall.InputJSON = ns
					changed = true
				}
				if drop {
					dropped = append(dropped, "tool_call.input_json")
				}
			}
		case agento11y.PartKindToolResult:
			if p.ToolResult == nil {
				continue
			}
			if p.ToolResult.Content != "" {
				if ns, ch := applyString(p.ToolResult.Content, ct); ch {
					p.ToolResult.Content = ns
					changed = true
				}
			}
			ns, ch, drop := applyRawJSON(p.ToolResult.ContentJSON, ct, logger, "tool_result.content_json")
			if ch {
				p.ToolResult.ContentJSON = ns
				changed = true
			}
			if drop {
				dropped = append(dropped, "tool_result.content_json")
			}
		}
	}
	return changed, dropped
}

func transformRelayPart(p *agento11y.Part, ct *Transform, logger *log.Logger) (bool, []string) {
	changed := false
	var dropped []string
	if next, ch := applyString(p.Text, ct); ch {
		p.Text = next
		changed = true
	}
	if next, ch := applyString(p.Thinking, ct); ch {
		p.Thinking = next
		changed = true
	}
	if p.ToolCall != nil {
		next, ch, drop := applyRawJSON(p.ToolCall.InputJSON, ct, logger, "tool_call.input_json")
		if ch {
			p.ToolCall.InputJSON = next
			changed = true
		}
		if drop {
			dropped = append(dropped, "tool_call.input_json")
		}
	}
	if p.ToolResult != nil {
		if next, ch := applyString(p.ToolResult.Content, ct); ch {
			p.ToolResult.Content = next
			changed = true
		}
		next, ch, drop := applyRawJSON(p.ToolResult.ContentJSON, ct, logger, "tool_result.content_json")
		if ch {
			p.ToolResult.ContentJSON = next
			changed = true
		}
		if drop {
			dropped = append(dropped, "tool_result.content_json")
		}
	}
	if p.Media != nil {
		if next, ch := applyString(p.Media.URL, ct); ch {
			p.Media.URL = next
			changed = true
		}
		if next, ch := applyString(p.Media.Name, ct); ch {
			p.Media.Name = next
			changed = true
		}
	}
	return changed, dropped
}

// applyRawJSON keeps the original bytes when a pattern removes JSON syntax or a
// replacement makes the result invalid JSON. Otherwise response marshaling can
// fail and fail-open transport can allow the call. The third return value
// reports a discarded rewrite. logger may be nil.
func applyRawJSON(raw json.RawMessage, ct *Transform, logger *log.Logger, what string) (json.RawMessage, bool, bool) {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return raw, false, false
	}
	ns, changed := applyString(string(raw), ct)
	if !changed {
		return raw, false, false
	}
	if !json.Valid([]byte(ns)) {
		if logger != nil {
			logger.Printf("local guards: redaction of %s dropped: the rewritten value is not valid JSON (check the pattern and replacement)", what)
		}
		return raw, false, true
	}
	return json.RawMessage(ns), true, false
}

func applyString(s string, ct *Transform) (string, bool) {
	if s == "" || ct == nil {
		return s, false
	}
	out := s
	changed := false
	for _, p := range ct.patterns {
		if !p.re.MatchString(out) {
			continue
		}
		// Literal replacement: admin-provided text must not expand $ / captures.
		next := p.re.ReplaceAllLiteralString(out, p.repl)
		if next != out {
			changed = true
			out = next
		}
	}
	return out, changed
}

func cloneHookInput(in agento11y.HookInput) agento11y.HookInput {
	out := agento11y.HookInput{
		SystemPrompt:        in.SystemPrompt,
		ConversationPreview: in.ConversationPreview,
	}
	if in.Messages != nil {
		out.Messages = cloneMessages(in.Messages)
	}
	if in.Output != nil {
		out.Output = cloneMessages(in.Output)
	}
	if in.Tools != nil {
		out.Tools = make([]agento11y.ToolDefinition, len(in.Tools))
		for i, t := range in.Tools {
			out.Tools[i] = t
			out.Tools[i].InputSchema = cloneRaw(t.InputSchema)
		}
	}
	return out
}

func cloneMessages(in []agento11y.Message) []agento11y.Message {
	out := make([]agento11y.Message, len(in))
	for i, m := range in {
		out[i] = agento11y.Message{Role: m.Role, Name: m.Name}
		if m.Parts == nil {
			continue
		}
		out[i].Parts = make([]agento11y.Part, len(m.Parts))
		for j, p := range m.Parts {
			np := agento11y.Part{
				Kind:     p.Kind,
				Text:     p.Text,
				Thinking: p.Thinking,
				Metadata: p.Metadata,
			}
			if p.ToolCall != nil {
				tc := *p.ToolCall
				tc.InputJSON = cloneRaw(p.ToolCall.InputJSON)
				np.ToolCall = &tc
			}
			if p.ToolResult != nil {
				tr := *p.ToolResult
				tr.ContentJSON = cloneRaw(p.ToolResult.ContentJSON)
				np.ToolResult = &tr
			}
			// Relay mode rewrites URL and Name, so the cloned input needs its own
			// media value.
			if p.Media != nil {
				media := *p.Media
				np.Media = &media
			}
			out[i].Parts[j] = np
		}
	}
	return out
}

func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	return append(json.RawMessage(nil), b...)
}
