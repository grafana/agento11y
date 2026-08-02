package agento11y

import (
	"log"
	"regexp"
	"strings"
)

// RedactSecretText replaces recognized secrets in arbitrary text. It is used
// by the experiments package for scores, explanations, metadata, and textual
// artifacts. Email addresses are redacted to match the built-in generation
// sanitizer's default.
func RedactSecretText(value string) string {
	return redactFull(value, true)
}

// RedactSecretValue recursively returns a redacted copy of JSON-like values.
// Unknown scalar types are returned unchanged.
func RedactSecretValue(value any) any {
	switch typed := value.(type) {
	case string:
		return RedactSecretText(typed)
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = RedactSecretValue(typed[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = RedactSecretValue(item)
		}
		return out
	case []string:
		out := make([]string, len(typed))
		for i := range typed {
			out[i] = RedactSecretText(typed[i])
		}
		return out
	default:
		return value
	}
}

// GenerationSanitizer mutates a generation before export. Sanitizers receive
// the fully normalized Generation and return the version to ship. Implementations
// may mutate strings/payloads (e.g. redact secrets) but must preserve message
// and part structure.
//
// If a sanitizer panics during Recorder.End the SDK downgrades the generation's
// content capture mode to ContentCaptureModeMetadataOnly and logs a warning.
type GenerationSanitizer func(Generation) Generation

// SecretRedactionOptions configures the built-in secret-redaction sanitizer.
type SecretRedactionOptions struct {
	// RedactInputMessages, when non-nil, sets whether user messages in
	// Generation.Input are sanitized too. Nil falls back to
	// SIGIL_REDACT_INPUT_MESSAGES and then to false (leave user input as-is).
	// Assistant and tool messages in Input are sanitized regardless because
	// they typically replay tool results and prior model output that share the
	// same secret surface as Generation.Output.
	RedactInputMessages *bool
	// RedactEmailAddresses, when non-nil, sets whether email addresses are
	// redacted. Nil defaults to true (redact). Set to a pointer to false to
	// preserve email addresses verbatim.
	RedactEmailAddresses *bool
}

// secretPattern is one compiled pattern from the generated table in
// redaction_patterns_gen.go. Edit redaction/patterns.json and run
// `mise run generate:redaction` to change the table.
type secretPattern struct {
	id string
	re *regexp.Regexp
}

// tier2Pattern carries a replacement template that keeps the matched key and
// rewrites only the value, so JSON and env syntax survive redaction.
type tier2Pattern struct {
	id          string
	re          *regexp.Regexp
	replacement string
}

// tier1Combined alternates all tier1Patterns into a single regex so each input
// is scanned once instead of once per pattern. Each pattern is wrapped in a
// capturing group; the matched group index identifies which pattern fired.
var tier1Combined = func() *regexp.Regexp {
	parts := make([]string, len(tier1Patterns))
	for i, p := range tier1Patterns {
		parts[i] = "(" + p.re.String() + ")"
	}
	return regexp.MustCompile(strings.Join(parts, "|"))
}()

// redactFull applies tier 1, optional email, and tier 2 patterns. Use for
// tool-call args, tool-result content, system prompts, and any field where
// arbitrary content can be expected (env dumps, shell output).
func redactFull(s string, includeEmail bool) string {
	s = redactLight(s, includeEmail)
	for _, p := range tier2Patterns {
		s = p.re.ReplaceAllString(s, p.replacement)
	}
	return s
}

// redactLight applies tier 1 and optional email patterns only. Use for
// assistant text and reasoning, where the tier 2 heuristics would cause too
// many false positives, and for short metadata strings (titles, error messages).
func redactLight(s string, includeEmail bool) string {
	s = redactTier1String(s)
	if includeEmail {
		s = emailPattern.re.ReplaceAllString(s, "[REDACTED:"+emailPattern.id+"]")
	}
	return s
}

// redactFullBytes is the []byte form of redactFull; it operates on the source
// slice directly so JSON payloads avoid a string round-trip.
func redactFullBytes(src []byte, includeEmail bool) []byte {
	src = redactTier1Bytes(src)
	if includeEmail {
		src = emailPattern.re.ReplaceAll(src, []byte("[REDACTED:"+emailPattern.id+"]"))
	}
	for _, p := range tier2Patterns {
		src = p.re.ReplaceAll(src, []byte(p.replacement))
	}
	return src
}

func redactTier1String(s string) string {
	matches := tier1Combined.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		for g := 1; g <= len(tier1Patterns); g++ {
			if m[2*g] >= 0 {
				b.WriteString("[REDACTED:")
				b.WriteString(tier1Patterns[g-1].id)
				b.WriteByte(']')
				break
			}
		}
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func redactTier1Bytes(src []byte) []byte {
	matches := tier1Combined.FindAllSubmatchIndex(src, -1)
	if len(matches) == 0 {
		return src
	}
	out := make([]byte, 0, len(src))
	last := 0
	for _, m := range matches {
		out = append(out, src[last:m[0]]...)
		for g := 1; g <= len(tier1Patterns); g++ {
			if m[2*g] >= 0 {
				out = append(out, "[REDACTED:"...)
				out = append(out, tier1Patterns[g-1].id...)
				out = append(out, ']')
				break
			}
		}
		last = m[1]
	}
	out = append(out, src[last:]...)
	return out
}

// resolveRedactInputMessages applies precedence explicit > env > false, where
// env reads AGENTO11Y_REDACT_INPUT_MESSAGES with SIGIL_REDACT_INPUT_MESSAGES
// fallback. An unrecognised value is logged with the selected key and ignored.
func resolveRedactInputMessages(lookup envLookup, explicit *bool) bool {
	if explicit != nil {
		return *explicit
	}
	if lookup == nil {
		lookup = defaultLookup
	}
	if v, key, ok := envTrimmed(lookup, envRedactInputMessages); ok {
		if parsed, valid := parseStrictBool(v); valid {
			return parsed
		}
		log.Default().Printf("agento11y: ignoring invalid %s %q", key, v)
	}
	return false
}

// NewSecretRedactionSanitizer returns a GenerationSanitizer that redacts
// known secret formats from generation content. The returned sanitizer is
// safe for concurrent use.
//
// By default it redacts Generation.Output (assistant + tool), Generation.SystemPrompt,
// Generation.ConversationTitle, Generation.CallError, and the assistant /
// tool messages in Generation.Input. User messages in Generation.Input are
// only redacted when input redaction is enabled, resolved as:
// explicit RedactInputMessages > SIGIL_REDACT_INPUT_MESSAGES > false.
// An invalid env value is logged and ignored. Email redaction is on unless
// RedactEmailAddresses points to false.
func NewSecretRedactionSanitizer(opts SecretRedactionOptions) GenerationSanitizer {
	return newSecretRedactionSanitizer(defaultLookup, opts)
}

func newSecretRedactionSanitizer(lookup envLookup, opts SecretRedactionOptions) GenerationSanitizer {
	includeEmail := opts.RedactEmailAddresses == nil || *opts.RedactEmailAddresses
	redactInputs := resolveRedactInputMessages(lookup, opts.RedactInputMessages)

	return func(g Generation) Generation {
		if g.SystemPrompt != "" {
			g.SystemPrompt = redactFull(g.SystemPrompt, includeEmail)
		}
		// ConversationTitle and CallError are short natural-language strings;
		// lightweight redaction (tier 1 + email) avoids mangling them with the
		// tier 2 heuristics.
		if g.ConversationTitle != "" {
			g.ConversationTitle = redactLight(g.ConversationTitle, includeEmail)
		}
		if g.CallError != "" {
			g.CallError = redactLight(g.CallError, includeEmail)
		}

		for i := range g.Input {
			sanitizeMessage(&g.Input[i], inputTextMode(g.Input[i].Role, redactInputs), includeEmail)
		}

		for i := range g.Output {
			sanitizeMessage(&g.Output[i], outputTextMode(g.Output[i].Role), includeEmail)
		}

		return g
	}
}

// textMode is which redaction mode to apply to PartKindText for a given role;
// thinking, tool-call, and tool-result parts use a fixed mode regardless.
type textMode int

const (
	textModeSkip textMode = iota
	textModeLight
	textModeFull
)

// inputTextMode picks the redaction mode for an Input message's text part.
// Historic assistant turns and tool results in Input always get role-aware
// redaction; user text is only redacted when the caller opts in.
func inputTextMode(role Role, redactUserInput bool) textMode {
	switch role {
	case RoleUser:
		if redactUserInput {
			return textModeFull
		}
		return textModeSkip
	case RoleTool:
		return textModeFull
	case RoleAssistant:
		return textModeLight
	default:
		return textModeSkip
	}
}

func outputTextMode(role Role) textMode {
	switch role {
	case RoleAssistant:
		return textModeLight
	case RoleTool:
		return textModeFull
	default:
		return textModeSkip
	}
}

func sanitizeMessage(m *Message, mode textMode, includeEmail bool) {
	if mode == textModeSkip {
		return
	}
	for i := range m.Parts {
		p := &m.Parts[i]
		switch p.Kind {
		case PartKindText:
			if mode == textModeFull {
				p.Text = redactFull(p.Text, includeEmail)
			} else {
				p.Text = redactLight(p.Text, includeEmail)
			}
		case PartKindThinking:
			p.Thinking = redactLight(p.Thinking, includeEmail)
		case PartKindToolCall:
			if p.ToolCall != nil && len(p.ToolCall.InputJSON) > 0 {
				p.ToolCall.InputJSON = redactFullBytes(p.ToolCall.InputJSON, includeEmail)
			}
		case PartKindToolResult:
			if p.ToolResult != nil {
				if p.ToolResult.Content != "" {
					p.ToolResult.Content = redactFull(p.ToolResult.Content, includeEmail)
				}
				if len(p.ToolResult.ContentJSON) > 0 {
					p.ToolResult.ContentJSON = redactFullBytes(p.ToolResult.ContentJSON, includeEmail)
				}
			}
		case PartKindMedia:
			// Media URLs and data URLs are generation content, but this sanitizer
			// only redacts textual and JSON payloads. Metadata-only capture strips
			// media URLs before export when content capture is disabled.
			continue
		}
	}
}
