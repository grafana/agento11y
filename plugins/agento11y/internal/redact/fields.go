package redact

import "encoding/json"

// Per-field redaction strength.
//
// Redact and RedactLightweight name a mechanism; the helpers below name the
// field a mapper is about to export. Every mapper used to pick its own tier per
// field, and the plugins disagreed: a prompt was tier 1 in two of them and
// tier 1+2 in five, and one sent tool arguments through tier 1 only. Call these
// instead of choosing a tier at the call site.
//
// The split matches the SDKs' generation sanitizer (go/agento11y/redaction.go,
// js/src/redaction.ts and the other three), so a plugin export and an SDK
// export scrub the same field the same way:
//
//   - Assembled or machine-generated content gets tier 1+2. A prompt carries
//     pasted config and command output, a system prompt carries environment
//     dumps, and tool payloads are structured data, so the tier 2 key/value
//     heuristic earns its false positives there.
//   - Prose gets tier 1 only. Model text, reasoning, a conversation title and
//     an error message are sentences, and tier 2 replaces the word after any
//     `key:` or `token:` in them.
//
// Tier 2's cost on a prompt is real: `sort key: name` comes out as
// `sort key: [REDACTED:env-secret-value]`. AGENTO11Y_REDACT_INPUT_MESSAGES=false
// turns prompt redaction off for anyone who would rather keep the text.
//
// The tool-payload helpers go past the SDKs: on a payload that decodes as JSON
// they also redact a value under a secret-looking key, which covers the names
// tier 2's fixed list misses (`authorization`, `cookie`, `client_secret`).

// Prompt redacts a user prompt: tier 1 + tier 2.
func (r *Redactor) Prompt(text string) string { return r.Redact(text) }

// SystemPrompt redacts an exported system prompt: tier 1 + tier 2.
func (r *Redactor) SystemPrompt(text string) string { return r.Redact(text) }

// ToolPayload redacts tool arguments or a tool result held as text:
// tier 1 + tier 2.
func (r *Redactor) ToolPayload(text string) string { return r.Redact(text) }

// ToolPayloadJSON redacts tool arguments or a tool result exported as JSON.
// The value keeps its JSON shape, so the field stays parseable downstream.
func (r *Redactor) ToolPayloadJSON(raw json.RawMessage) json.RawMessage {
	return r.RedactJSON(raw)
}

// ToolPayloadText redacts tool arguments or a tool result for a field that
// takes a string, such as a span attribute. A payload that is a JSON string is
// unwrapped first, so the attribute carries the text rather than its quoted
// encoding.
func (r *Redactor) ToolPayloadText(raw json.RawMessage) string {
	return r.RedactJSONForText(raw)
}

// AssistantText redacts model prose: tier 1 only.
func (r *Redactor) AssistantText(text string) string { return r.RedactLightweight(text) }

// Thinking redacts model reasoning: tier 1 only.
func (r *Redactor) Thinking(text string) string { return r.RedactLightweight(text) }

// Title redacts a conversation title: tier 1 only. A title is usually the
// first prompt, truncated, so it is prose.
func (r *Redactor) Title(text string) string { return r.RedactLightweight(text) }

// ErrorText redacts an error message from the agent or a tool: tier 1 only.
func (r *Redactor) ErrorText(text string) string { return r.RedactLightweight(text) }
