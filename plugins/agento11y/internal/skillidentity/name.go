package skillidentity

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

func Validate(name string) (string, bool) {
	if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
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
