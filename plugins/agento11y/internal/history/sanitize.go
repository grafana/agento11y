package history

import (
	"encoding/json"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

// DefaultMaxFieldBytes caps any single text field. Historical sources hold
// large pasted blobs, and a cap keeps one turn from bloating the export. The
// default is generous enough to leave normal turns intact.
const DefaultMaxFieldBytes = 256 * 1024

// Truncation reports what one Truncate call did, so a caller can record it in
// quality metadata.
type Truncation struct {
	Truncated     bool
	OriginalBytes int
	KeptBytes     int
}

// Truncate caps s at max bytes on a UTF-8 boundary. A non-positive max means no
// limit. The returned Truncation always reports the original size.
func Truncate(s string, max int) (string, Truncation) {
	t := Truncation{OriginalBytes: len(s), KeptBytes: len(s)}
	if max <= 0 || len(s) <= max {
		return s, t
	}
	cut := max
	// Back off to a valid UTF-8 boundary; continuation bytes are 0b10xxxxxx.
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	t.Truncated = true
	t.KeptBytes = cut
	return s[:cut], t
}

// Sanitizer redacts and truncates historical turn content before export. It is
// the single redaction point for import: an importer returns raw content (the
// Codex mapper takes RawContent for exactly this reason) so nothing is redacted
// twice or, worse, once by a mapper and never by the framework.
//
// The zero value works: it uses the default cap and the zero-value Redactor.
type Sanitizer struct {
	MaxFieldBytes int // 0 uses DefaultMaxFieldBytes; negative disables truncation
	Redactor      redact.Redactor
}

func (s Sanitizer) maxBytes() int {
	if s.MaxFieldBytes == 0 {
		return DefaultMaxFieldBytes
	}
	return s.MaxFieldBytes
}

func (s Sanitizer) cleanText(text string) (string, Truncation) {
	return Truncate(s.Redactor.Redact(text), s.maxBytes())
}

// Sanitize redacts and truncates every text-bearing field of g.Gen in place and
// records truncation in g.Quality.Truncated. A JSON field is redacted
// key-aware and stays valid JSON.
func (s Sanitizer) Sanitize(g *HistoricalGeneration) {
	max := s.maxBytes()
	truncated := false

	clean := func(text string) string {
		out, t := s.cleanText(text)
		truncated = truncated || t.Truncated
		return out
	}

	// A redacted JSON field that still exceeds the cap is replaced with a valid
	// JSON placeholder rather than a sliced (and so invalid) byte run.
	cleanJSON := func(raw json.RawMessage) json.RawMessage {
		red := s.Redactor.RedactJSON(raw)
		if max > 0 && len(red) > max {
			truncated = true
			return jsonTruncatedPlaceholder
		}
		return red
	}

	g.Gen.SystemPrompt = clean(g.Gen.SystemPrompt)
	// A historical title or call error can carry prompt text, file paths,
	// commands, or secrets, so both go through the same redaction as message
	// content.
	g.Gen.ConversationTitle = clean(g.Gen.ConversationTitle)
	g.Gen.CallError = clean(g.Gen.CallError)
	s.cleanMessages(g.Gen.Input, clean, cleanJSON)
	s.cleanMessages(g.Gen.Output, clean, cleanJSON)

	// Tool definitions can be built from local source data, so their free-text
	// description and JSON schema are sanitized too. Name and Type are tool
	// identifiers used as metadata and are left intact.
	for i := range g.Gen.Tools {
		g.Gen.Tools[i].Description = clean(g.Gen.Tools[i].Description)
		if len(g.Gen.Tools[i].InputSchema) > 0 {
			g.Gen.Tools[i].InputSchema = cleanJSON(g.Gen.Tools[i].InputSchema)
		}
	}

	if truncated {
		g.Quality.Truncated = true
	}
}

// jsonTruncatedPlaceholder is a valid JSON value substituted for a JSON field
// too large to keep, so export never sees malformed JSON.
var jsonTruncatedPlaceholder = json.RawMessage(`"[TRUNCATED]"`)

func (s Sanitizer) cleanMessages(msgs []agento11y.Message, clean func(string) string, cleanJSON func(json.RawMessage) json.RawMessage) {
	for i := range msgs {
		for j := range msgs[i].Parts {
			p := &msgs[i].Parts[j]
			p.Text = clean(p.Text)
			p.Thinking = clean(p.Thinking)
			if p.ToolCall != nil && len(p.ToolCall.InputJSON) > 0 {
				p.ToolCall.InputJSON = cleanJSON(p.ToolCall.InputJSON)
			}
			if p.ToolResult != nil {
				p.ToolResult.Content = clean(p.ToolResult.Content)
				if len(p.ToolResult.ContentJSON) > 0 {
					p.ToolResult.ContentJSON = cleanJSON(p.ToolResult.ContentJSON)
				}
			}
		}
	}
}
