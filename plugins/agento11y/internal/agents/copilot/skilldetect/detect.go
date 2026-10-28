package skilldetect

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/grafana/agento11y/plugins/agento11y/internal/skillidentity"
)

// Call Detect only for successful, unambiguous Copilot CLI completions.
func Detect(toolName string, input json.RawMessage) (string, bool) {
	if toolName != "skill" || !utf8.Valid(input) {
		return "", false
	}
	input = bytes.TrimSpace(input)
	if len(input) > 0 && input[0] == '"' {
		var wrapped string
		if err := json.Unmarshal(input, &wrapped); err != nil {
			return "", false
		}
		input = json.RawMessage(wrapped)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return "", false
	}
	var name string
	if err := json.Unmarshal(fields["skill"], &name); err != nil {
		return "", false
	}
	return skillidentity.Validate(name)
}
