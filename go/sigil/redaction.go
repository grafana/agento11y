package sigil

import (
	"log"
	"regexp"
	"strings"
)

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

type secretPattern struct {
	id string
	re *regexp.Regexp
}

// tier1Patterns covers high-confidence secret formats. Applied by both the
// full and lightweight redactors.
var tier1Patterns = []secretPattern{
	{"grafana-cloud-token", regexp.MustCompile(`\bglc_[A-Za-z0-9_-]{20,}`)},
	{"grafana-service-account-token", regexp.MustCompile(`\bglsa_[A-Za-z0-9_-]{20,}`)},
	{"aws-access-token", regexp.MustCompile(`\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}\b`)},
	{"github-pat", regexp.MustCompile(`\bghp_[A-Za-z0-9_]{36,}`)},
	{"github-oauth", regexp.MustCompile(`\bgho_[A-Za-z0-9_]{36,}`)},
	{"github-app-token", regexp.MustCompile(`\bghs_[A-Za-z0-9_]{36,}`)},
	{"github-fine-grained-pat", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{82}`)},
	{"anthropic-api-key", regexp.MustCompile(`\bsk-ant-api03-[a-zA-Z0-9_-]{93}AA`)},
	{"anthropic-admin-key", regexp.MustCompile(`\bsk-ant-admin01-[a-zA-Z0-9_-]{93}AA`)},
	{"openai-api-key", regexp.MustCompile(`\bsk-[a-zA-Z0-9]{20}T3BlbkFJ[a-zA-Z0-9]{20}`)},
	{"openai-project-key", regexp.MustCompile(`\bsk-proj-[a-zA-Z0-9_-]{40,}`)},
	{"openai-svcacct-key", regexp.MustCompile(`\bsk-svcacct-[a-zA-Z0-9_-]{40,}`)},
	{"gcp-api-key", regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{35}`)},
	{"private-key", regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`)},
	{"connection-string", regexp.MustCompile(`(?:postgres|mysql|mongodb|redis|amqp)://[^\s'"]+@[^\s'"]+`)},
	{"bearer-token", regexp.MustCompile(`[Bb]earer\s+[A-Za-z0-9_.\-~+/]{20,}={0,3}`)},
	{"slack-token", regexp.MustCompile(`\bxox[bporas]-[A-Za-z0-9-]{10,}`)},
	{"stripe-key", regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{20,}`)},
	{"sendgrid-api-key", regexp.MustCompile(`\bSG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}`)},
	{"twilio-api-key", regexp.MustCompile(`\bSK[a-f0-9]{32}`)},
	{"npm-token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}`)},
	{"pypi-token", regexp.MustCompile(`\bpypi-[A-Za-z0-9_-]{50,}`)},
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

// emailPattern is toggleable via SecretRedactionOptions.RedactEmailAddresses.
var emailPattern = secretPattern{
	id: "email",
	re: regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`),
}

// envSecretPattern covers heuristic env-style key/value assignments. Applied
// only by the full redactor (skipped for assistant text/thinking in
// lightweight mode) to avoid mangling natural-language strings.
var envSecretPattern = secretPattern{
	id: "env-secret-value",
	re: regexp.MustCompile(`(?i)((?:PASSWORD|SECRET|TOKEN|KEY|CREDENTIAL|API_KEY|PRIVATE_KEY|ACCESS_KEY)\s*[=:]\s*)([^\s"{}\[\],]+)`),
}

// redactFull applies tier 1, optional email, and env-style patterns. Use for
// tool-call args, tool-result content, system prompts, and any field where
// arbitrary content can be expected (env dumps, shell output).
func redactFull(s string, includeEmail bool) string {
	s = redactTier1String(s)
	if includeEmail {
		s = emailPattern.re.ReplaceAllString(s, "[REDACTED:"+emailPattern.id+"]")
	}
	return envSecretPattern.re.ReplaceAllString(s, "${1}[REDACTED:"+envSecretPattern.id+"]")
}

// redactLight applies tier 1 and optional email patterns only. Use for
// assistant text and reasoning, where env-style heuristics would cause too many
// false positives, and for short metadata strings (titles, error messages).
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
	return envSecretPattern.re.ReplaceAll(src, []byte("${1}[REDACTED:"+envSecretPattern.id+"]"))
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

// resolveRedactInputMessages applies precedence explicit > env > false.
// An unrecognised SIGIL_REDACT_INPUT_MESSAGES value is logged and ignored.
func resolveRedactInputMessages(lookup envLookup, explicit *bool) bool {
	if explicit != nil {
		return *explicit
	}
	if lookup == nil {
		lookup = defaultLookup
	}
	if v, ok := envTrimmed(lookup, envRedactInputMessages); ok {
		if parsed, valid := parseStrictBool(v); valid {
			return parsed
		}
		log.Default().Printf("sigil: ignoring invalid %s %q", envRedactInputMessages, v)
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
		// env-style heuristic.
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

// textMode is which tier to apply to PartKindText for a given role; thinking,
// tool-call, and tool-result parts use a fixed tier regardless.
type textMode int

const (
	textModeSkip textMode = iota
	textModeLight
	textModeFull
)

// inputTextMode picks the tier for an Input message's text part. Historic
// assistant turns and tool results in Input always get role-aware redaction;
// user text is only redacted when the caller opts in.
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
		}
	}
}
