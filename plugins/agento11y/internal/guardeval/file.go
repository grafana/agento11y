package guardeval

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// The rules file is TOML, hand-written, and read-only to this process. TOML
// buys two things JSON cannot: comments next to the rule they explain, and
// literal strings, so a pattern reads '(?i)\bgit\s+reset\b' rather than the
// double-escaped "(?i)\\bgit\\s+reset\\b" that a regex in JSON becomes.
//
// Nothing here writes the file. Re-encoding TOML from a decoded map would drop
// the comments and reorder the keys, so a save would quietly destroy the two
// things the format was chosen for.
//
// Rules are converted to JSON once, at this boundary, and the rest of the
// package works in JSON: the compile path is shared with rules that arrive over
// the wire, and a raw JSON rule round-trips fields the local evaluator ignores
// instead of dropping them.

// ConfigFile is the rules file that sits next to config.env.
const ConfigFile = "guards.toml"

// rulesKey is the array-of-tables the rules are read from. The file is a
// document rather than a bare list so it has room to grow other sections.
const rulesKey = "rules"

// FilePath is the ruleset that sits next to the given config.env. An empty
// config path yields an empty ruleset path, which the engine reads as no rules.
func FilePath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), ConfigFile)
}

// ParseRules decodes a rules file into raw JSON rule objects. An empty document
// is an empty ruleset, not an error: a file holding only comments is a file the
// user is still writing.
func ParseRules(data []byte) ([]json.RawMessage, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	entry, ok := doc[rulesKey]
	if !ok {
		return nil, nil
	}
	items, ok := entry.([]any)
	if !ok {
		return nil, fmt.Errorf("%q must be a list of rules, written [[%s]]", rulesKey, rulesKey)
	}

	out := make([]json.RawMessage, 0, len(items))
	for i, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a table", rulesKey, i)
		}
		if match, ok := fields["match"].(map[string]any); ok {
			fields["match"] = flattenMatch(match)
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", rulesKey, i, err)
		}
		out = append(out, encoded)
	}
	return out, nil
}

// flattenMatch turns the nested tables TOML produces into the dotted keys the
// matcher reads, so [rules.match.model] name = "..." arrives as "model.name".
// Nesting is what TOML gives you for free: writing the dotted key directly
// would need quoting, since an unquoted dot means a sub-table. A key that
// already holds a dot passes through, so both spellings work.
func flattenMatch(match map[string]any) map[string]any {
	out := make(map[string]any, len(match))
	for key, value := range match {
		nested, ok := value.(map[string]any)
		if !ok {
			out[key] = value
			continue
		}
		for suffix, leaf := range flattenMatch(nested) {
			out[key+"."+suffix] = leaf
		}
	}
	return out
}
