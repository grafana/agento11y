package skilldetect

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/grafana/agento11y/plugins/agento11y/internal/skillidentity"
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
	return skillidentity.Validate(name)
}
