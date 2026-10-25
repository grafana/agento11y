package skilldetect

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

// Read paths do not establish a skill-load attempt.
func Detect(toolName string, input json.RawMessage) (string, bool) {
	if toolName != "Skill" || !utf8.Valid(input) {
		return "", false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return "", false
	}
	var name string
	if err := json.Unmarshal(fields["skill"], &name); err != nil {
		return "", false
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", false
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 256 || redact.New().Redact(name) != name {
		return "", false
	}

	if scheme, _, hasScheme := strings.Cut(name, ":"); hasScheme {
		switch strings.ToLower(scheme) {
		case "file", "http", "https", "ftp", "ssh", "git", "npm", "github":
			return "", false
		}
	}

	// Colons separate plugin namespaces. Paths and URLs are not skill names.
	for segment := range strings.SplitSeq(name, ":") {
		for i, ch := range segment {
			if unicode.IsLetter(ch) || unicode.IsDigit(ch) {
				continue
			}
			if i > 0 && (ch == '-' || ch == '_' || ch == '.') {
				continue
			}
			return "", false
		}
		if segment == "" {
			return "", false
		}
	}
	return name, true
}
